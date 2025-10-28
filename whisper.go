//go:generate go tool buf format -w
//go:generate go tool buf generate

// Package whisper provides a gRPC-based gossip protocol. Each peer within the network advertises itself
// to others, periodically sending updates about its current view of the network to others via UDP. All peer data sent
// via UDP is encrypted using Diffie-Hellman. Once two peers become aware of each other's public keys, they can derive
// secrets and share information.
//
// A peer optionally joins an existing network on start. This is managed via TCP. Upon acceptance, the peer they
// joined via will provide its current state of the gossip network so that the new peer can derive secrets for
// all others.
package whisper

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davidsbond/x/slicepool"
	"github.com/davidsbond/x/syncmap"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
	whisperv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/v1"
	"github.com/davidsbond/whisper/internal/service"
	"github.com/davidsbond/whisper/pkg/event"
	"github.com/davidsbond/whisper/pkg/peer"
	"github.com/davidsbond/whisper/pkg/store"
)

type (
	// The Node type represents a single whisper node. Nodes should be created via the New function.
	Node struct {
		id          uint64
		joinAddress string
		logger      *slog.Logger
		store       PeerStore

		// Knobs for gossipping
		gossipInterval time.Duration
		checkInterval  time.Duration
		reapInterval   time.Duration

		// Used for encryption
		key     *ecdh.PrivateKey
		curve   ecdh.Curve
		ciphers *syncmap.Map[string, cipher.AEAD]

		// Advertised to other peers
		address  string
		metadata proto.Message

		// Used for networking
		port      int
		bytes     *slicepool.Pool[byte]
		clientTLS *tls.Config
		serverTLS *tls.Config

		// Used to signal the node is up and running
		ready        chan struct{}
		listeningUDP atomic.Bool
		listeningTCP atomic.Bool
		gossiping    atomic.Bool

		// Used for state events
		events chan event.Event
	}
)

const (
	udpSize = 65535
)

// New returns a new whisper node with the specified id. See the Option type for available configuration values. Once
// a Node has been created you must call Node.Events method and handle its output. Otherwise, blocking will occur when
// the first state change happens after calling Node.Run.
func New(id uint64, options ...Option) *Node {
	cfg := defaultConfig()
	for _, opt := range options {
		opt(cfg)
	}

	return &Node{
		id:             id,
		joinAddress:    cfg.joinAddress,
		logger:         cfg.logger,
		store:          cfg.store,
		gossipInterval: cfg.gossipInterval,
		checkInterval:  cfg.checkInterval,
		reapInterval:   cfg.reapInterval,
		key:            cfg.key,
		curve:          cfg.curve,
		ciphers:        syncmap.New[string, cipher.AEAD](),
		address:        cfg.address,
		metadata:       cfg.metadata,
		port:           cfg.port,
		bytes:          slicepool.New[byte](udpSize),
		clientTLS:      cfg.clientTLS,
		serverTLS:      cfg.serverTLS,
		ready:          make(chan struct{}, 1),
		events:         make(chan event.Event, 1),
	}
}

// Run a whisper node.
func (n *Node) Run(ctx context.Context) error {
	if err := n.bootstrap(ctx); err != nil {
		return fmt.Errorf("failed to bootstrap peer: %w", err)
	}

	// We use a cancellable context here so that we can shut everything down once n.leave() has returned. This will
	// increase the likelihood of our "leaving" state being propagated as well as status checks working until we're
	// truly out of the network.
	nCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return n.listenTCP(nCtx)
	})

	group.Go(func() error {
		return n.listenUDP(nCtx)
	})

	group.Go(func() error {
		return n.gossip(nCtx)
	})

	group.Go(func() error {
		<-ctx.Done()
		defer cancel()
		defer close(n.ready)
		defer close(n.events)

		return n.leave()
	})

	err := group.Wait()
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}

	return err
}

// Ready is used to determine if the whisper node is up and running. This method blocks until the node reports it is
// ready or the given context is cancelled.
func (n *Node) Ready(ctx context.Context) error {
	n.reportReadiness()

	select {
	case <-n.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Events returns a channel from which peer events can be handled. See the event.Event type for more details. This method
// should only be called once.
func (n *Node) Events() <-chan event.Event {
	return n.events
}

func (n *Node) reportReadiness() {
	if n.gossiping.Load() && n.listeningUDP.Load() && n.listeningTCP.Load() {
		n.ready <- struct{}{}
	}
}

// ID returns the node's identifier.
func (n *Node) ID() uint64 {
	return n.id
}

// Address returns the address the node is advertising to peers.
func (n *Node) Address() string {
	return n.address
}

// Peers returns all peers within the gossip network known at the time it is called.
func (n *Node) Peers(ctx context.Context) ([]peer.Peer, error) {
	return n.store.ListPeers(ctx)
}

// SetMetadata updates the metadata on the Node. This causes a delta update which will be propagated out to the
// gossip network.
func (n *Node) SetMetadata(ctx context.Context, message proto.Message) error {
	self, err := n.store.FindPeer(ctx, n.id)
	if err != nil {
		return fmt.Errorf("failed to lookup local peer record: %w", err)
	}

	self.Metadata = message
	self.Delta = time.Now().UnixNano()

	if err = n.store.SavePeer(ctx, self); err != nil {
		return fmt.Errorf("failed to save local peer record: %w", err)
	}

	n.metadata = message
	return nil
}

func (n *Node) bootstrap(ctx context.Context) error {
	self := peer.Peer{
		ID:       n.id,
		Address:  n.address,
		Delta:    time.Now().UnixNano(),
		Status:   peer.StatusJoining,
		Metadata: n.metadata,
	}

	if n.joinAddress == "" {
		self.Status = peer.StatusJoined
	}

	if n.key == nil {
		key, err := n.curve.GenerateKey(rand.Reader)
		if err != nil {
			return fmt.Errorf("failed to generate key: %w", err)
		}

		self.PublicKey = key.PublicKey()
		n.key = key
	}

	if err := n.store.SavePeer(ctx, self); err != nil {
		return fmt.Errorf("failed to save local peer record: %w", err)
	}

	if n.joinAddress != "" {
		if err := n.join(ctx, self); err != nil {
			return fmt.Errorf("failed to join gossip network: %w", err)
		}
	}

	return nil
}

func (n *Node) join(ctx context.Context, self peer.Peer) error {
	client, closer, err := peer.Dial(n.joinAddress, n.clientTLS)
	if err != nil {
		return fmt.Errorf("failed to dial peer: %w", err)
	}

	defer closer()

	p, err := self.ToProto()
	if err != nil {
		return fmt.Errorf("failed to convert local peer state to proto: %w", err)
	}

	request := &whispersvcv1.JoinRequest{Peer: p}
	response, err := client.Join(ctx, request)
	if err != nil {
		return fmt.Errorf("failed to send join request: %w", err)
	}

	for _, protoPeer := range response.GetPeers() {
		p, err := peer.FromProto(protoPeer, n.curve)
		if err != nil {
			return fmt.Errorf("failed to parse peer: %w", err)
		}

		if err = n.store.SavePeer(ctx, p); err != nil {
			return fmt.Errorf("failed to save peer record: %w", err)
		}

		if err = n.writeEvent(ctx, event.TypeDiscovered, p); err != nil {
			return fmt.Errorf("failed to write event: %w", err)
		}
	}

	if err = n.setStatus(ctx, peer.StatusJoined); err != nil {
		return fmt.Errorf("failed to set joined status: %w", err)
	}

	return nil
}

func (n *Node) leave() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := n.setStatus(ctx, peer.StatusLeaving); err != nil {
		return fmt.Errorf("failed to set leaving status: %w", err)
	}

	selected, err := n.selectPeer(ctx)
	if err != nil {
		return fmt.Errorf("failed to select peer: %w", err)
	}

	if selected.IsEmpty() {
		return nil
	}

	client, closer, err := peer.Dial(selected.Address, n.clientTLS)
	if err != nil {
		return fmt.Errorf("failed to dial peer: %w", err)
	}

	defer closer()
	request := &whispersvcv1.LeaveRequest{Id: n.id}
	if _, err = client.Leave(ctx, request); err != nil {
		return fmt.Errorf("failed to send leave request: %w", err)
	}

	if err = n.setStatus(ctx, peer.StatusLeft); err != nil {
		return fmt.Errorf("failed to set left status: %w", err)
	}

	return nil
}

func (n *Node) setStatus(ctx context.Context, status peer.Status) error {
	self, err := n.store.FindPeer(ctx, n.id)
	if err != nil {
		return fmt.Errorf("failed to lookup local peer record: %w", err)
	}

	self.Status = status
	self.Delta = time.Now().UnixNano()

	if err = n.store.SavePeer(ctx, self); err != nil {
		return fmt.Errorf("failed to save local peer record: %w", err)
	}

	return nil
}

func (n *Node) listenTCP(ctx context.Context) error {
	tcp, err := net.ListenTCP("tcp", &net.TCPAddr{Port: n.port})
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", n.port, err)
	}

	var options []grpc.ServerOption
	if n.serverTLS != nil {
		options = append(options, grpc.Creds(credentials.NewTLS(n.serverTLS)))
	}

	server := grpc.NewServer(options...)
	service.New(n.id, n.store, n.curve, n.logger, n.clientTLS, n.events).Register(server)

	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		n.listeningTCP.Store(true)
		n.reportReadiness()

		return server.Serve(tcp)
	})

	group.Go(func() error {
		<-ctx.Done()

		n.listeningTCP.Store(true)
		server.GracefulStop()
		return nil
	})

	return group.Wait()
}

func (n *Node) listenUDP(ctx context.Context) error {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{Port: n.port})
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", n.port, err)
	}

	n.listeningUDP.Store(true)
	n.reportReadiness()

	var (
		netErr net.Error
		group  sync.WaitGroup
	)

	defer group.Wait()

	for {
		select {
		case <-ctx.Done():
			n.listeningUDP.Store(false)
			return udp.Close()
		default:
			if err = udp.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				n.logger.With("error", err).ErrorContext(ctx, "failed to set read deadline")
				continue
			}

			buf := n.bytes.Get()

			length, _, err := udp.ReadFromUDP(*buf)
			switch {
			case errors.As(err, &netErr) && netErr.Timeout():
				n.bytes.Put(buf)
				continue
			case err != nil:
				n.bytes.Put(buf)
				n.logger.With("error", err).ErrorContext(ctx, "failed to read UDP packet")
				continue
			}

			payload := *buf
			group.Add(1)
			go func(payload []byte) {
				defer group.Done()
				defer n.bytes.Put(buf)
				n.handlePeerMessage(ctx, payload)
			}(payload[:length])
		}
	}
}

func (n *Node) handlePeerMessage(ctx context.Context, payload []byte) {
	message := &whisperv1.PeerMessage{}
	if err := proto.Unmarshal(payload, message); err != nil {
		n.logger.With("error", err).ErrorContext(ctx, "failed to unmarshal peer message")
		return
	}

	logger := n.logger.With("from", message.GetSourceId())

	source, err := n.store.FindPeer(ctx, message.GetSourceId())
	switch {
	case errors.Is(err, store.ErrPeerNotFound):
		return
	case err != nil:
		logger.With("error", err).ErrorContext(ctx, "failed to lookup peer")
		return
	}

	remote, err := n.decryptPeer(source, message)
	if err != nil {
		logger.With("error", err).ErrorContext(ctx, "failed to decrypt peer")
		return
	}

	logger = logger.With("peer", remote.ID)

	local, err := n.store.FindPeer(ctx, remote.ID)
	switch {
	case errors.Is(err, store.ErrPeerNotFound):
		break
	case err != nil:
		logger.With("error", err).ErrorContext(ctx, "failed to lookup peer")
		return
	}

	// The inbound peer data is newer than ours, so we should update our local state.
	if remote.Delta > local.Delta {
		// A peer may have decided that the remote peer has failed when in fact it has left gracefully and the
		// sender has yet to receive the update. We guard against this here by checking if our local state
		// is StatusLeft and the remote state is StatusGone. If so, persist the StatusLeft status and update
		// the delta of the peer.
		if remote.Status == peer.StatusGone && (local.Status == peer.StatusLeft) {
			remote.Status = local.Status
			remote.Delta = time.Now().UnixNano()
		}

		if err = n.store.SavePeer(ctx, remote); err != nil {
			logger.With("error", err).ErrorContext(ctx, "failed to save peer")
			return
		}

		eventType := event.TypeUpdated
		switch {
		case remote.Status == peer.StatusLeft && local.Status != peer.StatusLeft:
			eventType = event.TypeLeft
		case remote.Status == peer.StatusGone && local.Status != peer.StatusGone:
			eventType = event.TypeGone
		case local.IsEmpty():
			eventType = event.TypeDiscovered
		}

		if err = n.writeEvent(ctx, eventType, remote); err != nil {
			logger.With("error", err).ErrorContext(ctx, "failed to write event")
			return
		}
	}

	// In this case, the peer sending the data is out-of-date. So we'll send our local state for this peer
	// to them.
	if remote.Delta < local.Delta {
		if err = n.sendPeerMessage(source, local); err != nil {
			logger.With("error", err).ErrorContext(ctx, "failed to send peer message")
			return
		}
	}
}

func (n *Node) sendPeerMessage(target, local peer.Peer) error {
	udp, err := net.ResolveUDPAddr("udp", target.Address)
	if err != nil {
		return fmt.Errorf("failed to resolve UDP address: %w", err)
	}

	conn, err := net.DialUDP("udp", nil, udp)
	if err != nil {
		return fmt.Errorf("failed to dial UDP: %w", err)
	}

	defer conn.Close()
	nonce, ciphertext, err := n.encryptPeer(target, local)
	if err != nil {
		return fmt.Errorf("failed to encrypt peer: %w", err)
	}

	message := &whisperv1.PeerMessage{
		SourceId:   n.id,
		Nonce:      nonce,
		Ciphertext: ciphertext,
	}

	buf, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal peer message: %w", err)
	}

	if _, err = conn.Write(buf); err != nil {
		return fmt.Errorf("failed to write peer message: %w", err)
	}

	return nil
}

func (n *Node) decryptPeer(source peer.Peer, message *whisperv1.PeerMessage) (peer.Peer, error) {
	gcm, err := n.getGCM(source)
	if err != nil {
		return peer.Peer{}, fmt.Errorf("failed to get GCM: %w", err)
	}

	nonce := message.GetNonce()
	ciphertext := message.GetCiphertext()

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return peer.Peer{}, fmt.Errorf("failed to decrypt peer: %w", err)
	}

	protoPeer := &whisperv1.Peer{}
	if err = proto.Unmarshal(plaintext, protoPeer); err != nil {
		return peer.Peer{}, fmt.Errorf("failed to unmarshal peer: %w", err)
	}

	p, err := peer.FromProto(protoPeer, n.curve)
	if err != nil {
		return peer.Peer{}, fmt.Errorf("failed to parse peer: %w", err)
	}

	return p, nil
}

func (n *Node) encryptPeer(target, p peer.Peer) (nonce, ciphertext []byte, err error) {
	gcm, err := n.getGCM(target)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get GCM: %w", err)
	}

	protoPeer, err := p.ToProto()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to convert peer: %w", err)
	}

	plaintext, err := proto.Marshal(protoPeer)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal peer: %w", err)
	}

	nonce = make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return nonce, ciphertext, nil
}

func (n *Node) getGCM(p peer.Peer) (cipher.AEAD, error) {
	hash := p.Hash()
	if gcm, ok := n.ciphers.Get(hash); ok {
		return gcm, nil
	}

	shared, err := n.key.ECDH(p.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	var key [32]byte
	hkdfReader := hkdf.New(sha256.New, shared, nil, []byte("whisper"))
	if _, err := io.ReadFull(hkdfReader, key[:]); err != nil {
		return nil, fmt.Errorf("failed to derive AES key: %w", err)
	}

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("failed to create AES block: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	n.ciphers.Put(hash, gcm)
	return gcm, nil
}

func (n *Node) gossip(ctx context.Context) error {
	stateTicker := time.NewTicker(n.gossipInterval)
	defer stateTicker.Stop()

	checkTicker := time.NewTicker(n.checkInterval)
	defer checkTicker.Stop()

	reapTicker := time.NewTicker(n.reapInterval)
	defer reapTicker.Stop()

	n.gossiping.Store(true)
	n.reportReadiness()

	for {
		select {
		case <-ctx.Done():
			n.gossiping.Store(false)
			return ctx.Err()
		case <-stateTicker.C:
			if err := n.shareState(ctx); err != nil {
				n.logger.With("error", err).ErrorContext(ctx, "failed to share state")
			}
		case <-checkTicker.C:
			if err := n.checkPeer(ctx); err != nil {
				n.logger.With("error", err).ErrorContext(ctx, "failed to check peer")
			}
		case <-reapTicker.C:
			if err := n.reap(ctx); err != nil {
				n.logger.With("error", err).ErrorContext(ctx, "failed to reap peers")
			}
		}
	}
}

func (n *Node) shareState(ctx context.Context) error {
	targets, err := n.selectPeersExcept(ctx, 3, n.id)
	if err != nil {
		return fmt.Errorf("failed to select peers: %w", err)
	}

	peers, err := n.store.ListPeers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list peers: %w", err)
	}

	var group errgroup.Group
	for _, target := range targets {
		group.Go(func() error {
			return n.sendPeerMessages(ctx, target, peers)
		})
	}

	return group.Wait()
}

func (n *Node) sendPeerMessages(ctx context.Context, target peer.Peer, peers []peer.Peer) error {
	for _, p := range peers {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if p.ID == target.ID {
			// Don't tell peers about themselves, each peer owns its own state except in the scenario where
			// one is leaving. But if it's leaving, it won't care for more updates.
			continue
		}

		if err := n.sendPeerMessage(target, p); err != nil {
			return fmt.Errorf("failed to send peer message to peer %d: %w", target.ID, err)
		}
	}

	return nil
}

func (n *Node) checkPeer(ctx context.Context) error {
	target, err := n.selectPeer(ctx)
	if err != nil {
		return fmt.Errorf("failed to select peer: %w", err)
	}

	if target.IsEmpty() {
		return nil
	}

	client, closer, err := peer.Dial(target.Address, n.clientTLS)
	if err != nil {
		if err = n.checkPeerViaPeers(ctx, target); err != nil {
			return fmt.Errorf("failed to check peer %d via peer: %w", target.ID, err)
		}
	}

	defer closer()
	if _, err = client.Status(ctx, &whispersvcv1.StatusRequest{}); err != nil {
		if err = n.checkPeerViaPeers(ctx, target); err != nil {
			return fmt.Errorf("failed to check peer %d via peer: %w", target.ID, err)
		}
	}

	return nil
}

func (n *Node) checkPeerViaPeers(ctx context.Context, target peer.Peer) error {
	checkVia, err := n.selectPeersExcept(ctx, 3, target.ID)
	if err != nil {
		return fmt.Errorf("failed to select peers: %w", err)
	}

	logger := n.logger.With("peer", target.ID)

	if len(checkVia) == 0 {
		logger.WarnContext(ctx, "no peers available to check peer liveness")
		return nil
	}

	for _, via := range checkVia {
		reached, err := n.checkPeerViaPeer(ctx, target, via)
		if err != nil {
			return fmt.Errorf("failed to check peer %d via peer %d: %w", target.ID, via.ID, err)
		}

		// At least one of our selected peers was able to reach the target peer. Or they had an entry for that peer
		// already in a gone/left state. So we don't update our local state and await an update from another peer.
		if reached {
			return nil
		}
	}

	logger.
		With("via_count", len(checkVia)).
		WarnContext(ctx, "peer was unavailable via other peers, marking as gone")

	if err = n.markPeerGone(ctx, target); err != nil {
		return fmt.Errorf("failed to mark peer %d as gone: %w", target.ID, err)
	}

	if err = n.writeEvent(ctx, event.TypeGone, target); err != nil {
		return fmt.Errorf("failed to write event: %w", err)
	}

	return nil
}

func (n *Node) checkPeerViaPeer(ctx context.Context, target peer.Peer, via peer.Peer) (bool, error) {
	n.logger.
		With("peer", target.ID, "via", via.ID).
		WarnContext(ctx, "failed to check peer liveness directly, checking via another peer")

	client, closer, err := peer.Dial(via.Address, n.clientTLS)
	if err != nil {
		return false, fmt.Errorf("failed to dial peer: %w", err)
	}

	defer closer()
	_, err = client.Check(ctx, &whispersvcv1.CheckRequest{Id: target.ID})
	switch status.Code(err) {
	// These four error codes indicate no action is required on our part for this peer:
	// * OK						The target peer is reachable from the checking peer.
	// * FailedPrecondition		The target peer is already marked as left/gone in the checking peer's state.
	// * Unavailable			We failed to reach the checking peer, so we don't have a definite answer.
	// * NotFound				The checking peer has no record of the target peer, another indefinite answer.
	case codes.OK, codes.FailedPrecondition, codes.Unavailable, codes.NotFound:
		return true, nil
	case codes.Internal:
		// We get an internal if the peer we've dialed cannot communicate with the specified peer.
		return false, nil
	default:
		// All other status codes are unexpected. We report that.
		return false, fmt.Errorf("unexpected error checking peer %d: %w", target.ID, err)
	}
}

func (n *Node) markPeerGone(ctx context.Context, target peer.Peer) error {
	target.Status = peer.StatusGone
	target.Delta = time.Now().UnixNano()

	if err := n.store.SavePeer(ctx, target); err != nil {
		return fmt.Errorf("failed to save peer: %w", err)
	}

	return nil
}

func (n *Node) selectPeersExcept(ctx context.Context, count int, exclude uint64) ([]peer.Peer, error) {
	peers, err := n.store.ListPeers(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list peers: %w", err)
	}

	availablePeers := make([]peer.Peer, 0)
	for _, p := range peers {
		// We only want active peers that do not match our own id or the excluded id.
		if p.ID == n.id || p.Status != peer.StatusJoined || p.ID == exclude {
			continue
		}

		availablePeers = append(availablePeers, p)
	}

	// If we have less available peers than the desired count, just return them all.
	if len(availablePeers) <= count {
		return availablePeers, nil
	}

	// Otherwise, shuffle them randomly and return up to "count".
	mathrand.Shuffle(len(availablePeers), func(i, j int) {
		availablePeers[i], availablePeers[j] = availablePeers[j], availablePeers[i]
	})

	return availablePeers[:count], nil
}

func (n *Node) selectPeer(ctx context.Context) (peer.Peer, error) {
	peers, err := n.store.ListPeers(ctx)
	if err != nil {
		return peer.Peer{}, fmt.Errorf("failed to list peers: %w", err)
	}

	availablePeers := make([]peer.Peer, 0)
	for _, p := range peers {
		if p.ID == n.id || p.Status != peer.StatusJoined {
			continue
		}

		availablePeers = append(availablePeers, p)
	}

	if len(availablePeers) == 0 {
		return peer.Peer{}, nil
	}

	idx := mathrand.IntN(len(availablePeers))
	target := availablePeers[idx]

	return target, nil
}

func (n *Node) reap(ctx context.Context) error {
	peers, err := n.store.ListPeers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list peers: %w", err)
	}

	for _, p := range peers {
		if p.Status != peer.StatusGone && p.Status != peer.StatusLeft {
			continue
		}

		ts := time.Unix(0, p.Delta)
		if time.Since(ts) < n.reapInterval {
			continue
		}

		err = n.store.RemovePeer(ctx, p.ID)
		switch {
		case errors.Is(err, store.ErrPeerNotFound):
			continue
		case err != nil:
			return fmt.Errorf("failed to remove peer %d: %w", p.ID, err)
		}

		n.ciphers.Remove(p.Hash())
		if err = n.writeEvent(ctx, event.TypeRemoved, p); err != nil {
			return fmt.Errorf("failed to write event: %w", err)
		}
	}

	return nil
}

func (n *Node) writeEvent(ctx context.Context, t event.Type, p peer.Peer) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case n.events <- event.Event{Type: t, Peer: p}:
		return nil
	}
}
