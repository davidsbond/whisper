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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/hkdf"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
	whisperv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/v1"
	"github.com/davidsbond/whisper/internal/service"
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

		// Used for encryption
		key   *ecdh.PrivateKey
		curve ecdh.Curve

		// Advertised to other peers
		address  string
		metadata proto.Message

		// Used for networking
		port int
	}
)

// New returns a new whisper node with the specified id. See the Option type for available configuration values.
func New(id uint64, options ...Option) *Node {
	cfg := defaultConfig()
	for _, opt := range options {
		opt(cfg)
	}

	return &Node{
		id:          id,
		key:         cfg.key,
		curve:       cfg.curve,
		address:     cfg.address,
		metadata:    cfg.metadata,
		logger:      cfg.logger,
		store:       cfg.store,
		port:        cfg.port,
		joinAddress: cfg.joinAddress,
	}
}

// Run a whisper node.
func (n *Node) Run(ctx context.Context) error {
	if err := n.bootstrap(ctx); err != nil {
		return fmt.Errorf("failed to bootstrap peer: %w", err)
	}

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return n.listenTCP(ctx)
	})

	group.Go(func() error {
		return n.listenUDP(ctx)
	})

	group.Go(func() error {
		return n.gossip(ctx)
	})

	group.Go(func() error {
		<-ctx.Done()
		return n.leave()
	})

	return group.Wait()
}

func (n *Node) bootstrap(ctx context.Context) error {
	self := peer.Peer{
		ID:      n.id,
		Address: n.address,
		Delta:   time.Now().Unix(),
		Status:  peer.StatusJoining,
	}

	if n.joinAddress == "" {
		self.Status = peer.StatusJoined
	}

	if n.metadata != nil {
		metadata, err := anypb.New(n.metadata)
		if err != nil {
			return fmt.Errorf("invalid metadata: %w", err)
		}

		self.Metadata = metadata
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
	client, closer, err := peer.Dial(n.joinAddress)
	if err != nil {
		return fmt.Errorf("failed to dial peer: %w", err)
	}

	defer closer()

	request := &whispersvcv1.JoinRequest{
		Peer: &whisperv1.Peer{
			Id:        self.ID,
			Address:   self.Address,
			PublicKey: self.PublicKey.Bytes(),
			Delta:     self.Delta,
			Status:    whisperv1.PeerStatus(self.Status),
		},
	}

	if self.Metadata != nil {
		request.Peer.Metadata, err = anypb.New(self.Metadata)
		if err != nil {
			return fmt.Errorf("failed to marshal metadata: %w", err)
		}
	}

	response, err := client.Join(ctx, request)
	if err != nil {
		return fmt.Errorf("failed to send join request: %w", err)
	}

	for _, protoPeer := range response.GetPeers() {
		publicKey, err := n.curve.NewPublicKey(protoPeer.GetPublicKey())
		if err != nil {
			return fmt.Errorf("failed to parse public key for peer %q: %w", protoPeer.GetId(), err)
		}

		p := peer.Peer{
			ID:        protoPeer.GetId(),
			Address:   protoPeer.GetAddress(),
			Delta:     protoPeer.GetDelta(),
			Status:    peer.Status(protoPeer.GetStatus()),
			PublicKey: publicKey,
		}

		if protoPeer.GetMetadata() != nil {
			p.Metadata, err = protoPeer.GetMetadata().UnmarshalNew()
			if err != nil {
				return fmt.Errorf("failed to unmarshal metadata for peer %q: %w", protoPeer.GetId(), err)
			}
		}

		if err = n.store.SavePeer(ctx, p); err != nil {
			return fmt.Errorf("failed to save peer record: %w", err)
		}
	}

	return nil
}

func (n *Node) leave() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	selected, err := n.selectPeer(ctx)
	if err != nil {
		return fmt.Errorf("failed to select peer: %w", err)
	}

	if selected.IsEmpty() {
		return nil
	}

	client, closer, err := peer.Dial(selected.Address)
	if err != nil {
		return fmt.Errorf("failed to dial peer: %w", err)
	}

	defer closer()
	request := &whispersvcv1.LeaveRequest{Id: n.id}
	if _, err = client.Leave(ctx, request); err != nil {
		return fmt.Errorf("failed to send leave request: %w", err)
	}

	return nil
}

func (n *Node) listenTCP(ctx context.Context) error {
	tcp, err := net.ListenTCP("tcp", &net.TCPAddr{Port: n.port})
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", n.port, err)
	}

	server := grpc.NewServer()
	service.New(n.id, n.store, n.curve).Register(server)

	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		return server.Serve(tcp)
	})

	group.Go(func() error {
		<-ctx.Done()
		server.GracefulStop()
		return tcp.Close()
	})

	return group.Wait()
}

func (n *Node) listenUDP(ctx context.Context) error {
	const udpSize = 65535

	udp, err := net.ListenUDP("udp", &net.UDPAddr{Port: n.port})
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", n.port, err)
	}

	var (
		netErr net.Error
		group  sync.WaitGroup
	)

	defer group.Wait()
	buf := make([]byte, udpSize)

	for {
		select {
		case <-ctx.Done():
			return udp.Close()
		default:
			if err = udp.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				n.logger.With("error", err).Error("failed to set read deadline")
				continue
			}

			length, _, err := udp.ReadFromUDP(buf)
			switch {
			case errors.As(err, &netErr) && netErr.Timeout():
				continue
			case err != nil:
				n.logger.With("error", err).Error("failed to read UDP packet")
				continue
			}

			payload := make([]byte, length)
			copy(payload, buf[:length])

			group.Add(1)
			go n.handlePeerMessage(ctx, &group, payload)
		}
	}
}

func (n *Node) handlePeerMessage(ctx context.Context, group *sync.WaitGroup, payload []byte) {
	defer group.Done()

	message := &whisperv1.PeerMessage{}
	if err := proto.Unmarshal(payload, message); err != nil {
		n.logger.With("error", err).Error("failed to unmarshal peer message")
		return
	}

	logger := n.logger.With("source_id", message.GetSourceId())

	source, err := n.store.FindPeer(ctx, message.GetSourceId())
	switch {
	case errors.Is(err, store.ErrPeerNotFound):
		return
	case err != nil:
		logger.With("error", err).Error("failed to lookup peer")
		return
	}

	remote, err := n.decryptPeer(source, message)
	if err != nil {
		logger.With("error", err).Error("failed to decrypt peer")
		return
	}

	logger = logger.With("peer_id", remote.ID)

	local, err := n.store.FindPeer(ctx, remote.ID)
	switch {
	case errors.Is(err, store.ErrPeerNotFound):
		break
	case err != nil:
		logger.With("error", err).Error("failed to lookup peer")
		return
	}

	// The inbound peer data is newer than ours, so we should update our local state.
	if remote.Delta > local.Delta {
		if err = n.store.SavePeer(ctx, remote); err != nil {
			logger.With("error", err).Error("failed to save peer")
		}

		return
	}

	// We're already up-to-date for this peer, so we'll exit early here.
	if remote.Delta == local.Delta {
		return
	}

	// In this case, the peer sending the data is out-of-date. So we'll send our local state for this peer
	// to them.
	if err = n.sendPeerMessage(source, local); err != nil {
		logger.With("error", err).Error("failed to send peer message")
		return
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
	secret, err := n.key.ECDH(source.PublicKey)
	if err != nil {
		return peer.Peer{}, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	reader := hkdf.New(sha256.New, secret, nil, []byte("whisper"))
	key := make([]byte, 32)
	if _, err = io.ReadFull(reader, key); err != nil {
		return peer.Peer{}, fmt.Errorf("failed to derive AES key: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return peer.Peer{}, fmt.Errorf("failed to create AES block: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return peer.Peer{}, fmt.Errorf("failed to create GCM: %w", err)
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

	publicKey, err := n.curve.NewPublicKey(protoPeer.GetPublicKey())
	if err != nil {
		return peer.Peer{}, fmt.Errorf("failed to parse public key: %w", err)
	}

	p := peer.Peer{
		ID:        protoPeer.GetId(),
		Address:   protoPeer.GetAddress(),
		Delta:     protoPeer.GetDelta(),
		Status:    peer.Status(protoPeer.GetStatus()),
		PublicKey: publicKey,
	}

	if protoPeer.Metadata != nil {
		p.Metadata, err = protoPeer.Metadata.UnmarshalNew()
		if err != nil {
			return peer.Peer{}, fmt.Errorf("failed to unmarshal peer metadata: %w", err)
		}
	}

	return p, nil
}

func (n *Node) encryptPeer(target, peer peer.Peer) (nonce, ciphertext []byte, err error) {
	secret, err := n.key.ECDH(target.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	reader := hkdf.New(sha256.New, secret, nil, []byte("whisper"))
	key := make([]byte, 32)
	if _, err = io.ReadFull(reader, key); err != nil {
		return nil, nil, fmt.Errorf("failed to derive AES key: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create AES block: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	protoPeer := &whisperv1.Peer{
		Id:        peer.ID,
		Address:   peer.Address,
		PublicKey: peer.PublicKey.Bytes(),
		Delta:     peer.Delta,
		Status:    whisperv1.PeerStatus(peer.Status),
	}

	if peer.Metadata != nil {
		protoPeer.Metadata, err = anypb.New(peer.Metadata)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal peer metadata: %w", err)
		}
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

func (n *Node) gossip(ctx context.Context) error {
	stateTicker := time.NewTicker(time.Second)
	defer stateTicker.Stop()

	checkTicker := time.NewTicker(time.Minute)
	defer checkTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stateTicker.C:
			if err := n.shareState(ctx); err != nil {
				n.logger.With("error", err).Error("failed to share state")
			}
		case <-checkTicker.C:
			if err := n.checkPeer(ctx); err != nil {
				n.logger.With("error", err).Error("failed to check peer")
			}
		}
	}
}

func (n *Node) shareState(ctx context.Context) error {
	target, err := n.selectPeer(ctx)
	if err != nil {
		return fmt.Errorf("failed to select peer: %w", err)
	}

	if target.IsEmpty() {
		return nil
	}

	peers, err := n.store.ListPeers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list peers: %w", err)
	}

	for _, p := range peers {
		if p.ID == target.ID {
			// Don't tell peers about themselves, each peer owns its own state except in the scenario where
			// one is leaving. But if it's leaving, it won't care for more updates.
			continue
		}

		if err = n.sendPeerMessage(target, p); err != nil {
			return fmt.Errorf("failed to send peer message to peer %q: %w", target.ID, err)
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

	client, closer, err := peer.Dial(target.Address)
	if err != nil {
		if err = n.checkPeerViaPeer(ctx, target); err != nil {
			return fmt.Errorf("failed to check peer %q via peer: %w", target.ID, err)
		}
	}

	defer closer()
	if _, err = client.Status(ctx, &whispersvcv1.StatusRequest{}); err != nil {
		if err = n.checkPeerViaPeer(ctx, target); err != nil {
			return fmt.Errorf("failed to check peer %q via peer: %w", target.ID, err)
		}
	}

	return nil
}

func (n *Node) checkPeerViaPeer(ctx context.Context, target peer.Peer) error {
	var selected peer.Peer

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			checker, err := n.selectPeer(ctx)
			if err != nil {
				return fmt.Errorf("failed to select peer: %w", err)
			}

			if checker.ID == target.ID {
				continue
			}

			selected = checker
		}

		break
	}

	client, closer, err := peer.Dial(selected.Address)
	if err != nil {
		return fmt.Errorf("failed to dial peer: %w", err)
	}

	defer closer()
	_, err = client.Check(ctx, &whispersvcv1.CheckRequest{Id: target.ID})
	switch status.Code(err) {
	case codes.OK, codes.FailedPrecondition:
		// We'll get a FailedPrecondition if the target peer has already left or marked as failed within the
		// selected peer's state.
		return nil
	case codes.NotFound:
		// The peer we called doesn't have the target peer in their state, try another peer
		return n.checkPeerViaPeer(ctx, target)
	default:
		if err = n.markPeerGone(ctx, target); err != nil {
			return fmt.Errorf("failed to mark peer %q as gone: %w", target.ID, err)
		}
	}

	return nil
}

func (n *Node) markPeerGone(ctx context.Context, target peer.Peer) error {
	target.Status = peer.StatusGone
	target.Delta = time.Now().Unix()

	if err := n.store.SavePeer(ctx, target); err != nil {
		return fmt.Errorf("failed to save peer: %w", err)
	}

	return nil
}

func (n *Node) selectPeer(ctx context.Context) (peer.Peer, error) {
	peers, err := n.store.ListPeers(ctx)
	if err != nil {
		return peer.Peer{}, err
	}

	var availablePeers int
	for _, p := range peers {
		if p.ID == n.id || p.Status != peer.StatusJoined {
			continue
		}

		availablePeers++
	}

	// If we only have one peer, we're a standalone node.
	if availablePeers == 0 {
		return peer.Peer{}, nil
	}

	idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(peers))))
	if err != nil {
		return peer.Peer{}, err
	}

	target := peers[idx.Int64()]

	// We never want to select ourselves or a peer with an inactive status.
	if target.ID == n.id || target.Status != peer.StatusJoined {
		return n.selectPeer(ctx)
	}

	return target, nil
}
