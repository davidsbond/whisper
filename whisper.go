package whisper

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

type (
	Node struct {
		id       uint64
		port     int
		address  string
		logger   *slog.Logger
		store    PeerStore
		key      *ecdh.PrivateKey
		curve    ecdh.Curve
		metadata []byte
		events   chan Event
	}

	PeerStore interface {
		FindPeer(ctx context.Context, id uint64) (Peer, error)
		SavePeer(ctx context.Context, peer Peer) error
		ListPeers(ctx context.Context) ([]Peer, error)
	}
)

const (
	ProtocolVersionUnknown uint8 = iota
	ProtocolVersion1
)

const (
	publicKeySize = 32
	ipv4Size      = 4
	ipv6Size      = 16
)

var (
	ErrPeerNotFound = errors.New("peer not found")
)

func New(id uint64, options ...NodeOption) *Node {
	config := defaultNodeConfig()
	for _, option := range options {
		option(config)
	}

	return &Node{
		id:      id,
		port:    config.port,
		logger:  config.logger.With("node_id", id),
		curve:   config.curve,
		store:   config.store,
		address: config.address,
		events:  make(chan Event),
	}
}

func (n *Node) Events() <-chan Event {
	return n.events
}

func (n *Node) SetMetadata(ctx context.Context, metadata []byte) error {
	self, err := n.store.FindPeer(ctx, n.id)
	if err != nil {
		return fmt.Errorf("failed to find local peer record: %w", err)
	}

	self.Metadata = metadata
	self.Delta = time.Now().Unix()

	if err = n.store.SavePeer(ctx, self); err != nil {
		return fmt.Errorf("failed to save local peer record: %w", err)
	}

	return nil
}

// Join starts a new Whisper node, informing an existing peer of its existence and thus adding it to gossip membership.
func (n *Node) Join(ctx context.Context, addr string) error {
	if err := n.bootstrap(ctx); err != nil {
		return err
	}

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return n.start(ctx)
	})

	group.Go(func() error {
		return n.sendJoinRequest(ctx, addr)
	})

	return group.Wait()
}

func (n *Node) sendJoinRequest(ctx context.Context, addr string) error {
	tcp, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to resolve TCP address %q: %w", addr, err)
	}

	conn, err := net.DialTCP("tcp", nil, tcp)
	if err != nil {
		return fmt.Errorf("failed to dial TCP address %q: %w", addr, err)
	}

	defer conn.Close()

	local, err := n.store.FindPeer(ctx, n.id)
	switch {
	case errors.Is(err, ErrPeerNotFound):
		local = Peer{
			ID:        n.id,
			Address:   netip.AddrPort{},
			PublicKey: n.key.PublicKey(),
			Delta:     time.Now().Unix(),
			Metadata:  n.metadata,
		}
	case err != nil:
		return fmt.Errorf("failed to find local peer record: %w", err)
	}

	data, err := local.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal local peer record: %w", err)
	}

	if _, err = conn.Write(data); err != nil {
		return fmt.Errorf("failed to write local peer record: %w", err)
	}

	local.Status = PeerStatusJoined
	local.Delta = time.Now().Unix()
	if err = n.store.SavePeer(ctx, local); err != nil {
		return fmt.Errorf("failed to save local peer record: %w", err)
	}

	buf := make([]byte, 1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			read, err := conn.Read(buf)
			switch {
			case errors.Is(err, io.EOF):
				return nil
			case err != nil:
				return fmt.Errorf("failed to read peer message: %w", err)
			}

			var peer Peer
			if err = peer.Unmarshal(n.curve, buf[:read]); err != nil {
				return fmt.Errorf("failed to unmarshal peer record: %w", err)
			}

			if err = n.store.SavePeer(ctx, peer); err != nil {
				return fmt.Errorf("failed to save peer record: %w", err)
			}

			n.logger.With("peer", peer.ID).Info("discovered new peer")
			n.events <- Event{
				Type: EventTypePeerDiscovered,
				Peer: peer,
			}
		}
	}
}

func (n *Node) Start(ctx context.Context) error {
	if err := n.bootstrap(ctx); err != nil {
		return err
	}

	local, err := n.store.FindPeer(ctx, n.id)
	if err != nil {
		return fmt.Errorf("failed to find local peer record: %w", err)
	}

	local.Status = PeerStatusJoined
	local.Delta = time.Now().Unix()
	if err = n.store.SavePeer(ctx, local); err != nil {
		return fmt.Errorf("failed to save local peer record: %w", err)
	}

	return n.start(ctx)
}

// Start a new Whisper node, to be used when there are no peers to connect to for building a fresh gossip cluster.
func (n *Node) start(ctx context.Context) error {
	n.logger.Info("starting whisper node")
	defer close(n.events)

	tcp, err := net.ListenTCP("tcp", &net.TCPAddr{Port: n.port})
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", n.port, err)
	}

	udp, err := net.ListenUDP("udp", &net.UDPAddr{Port: n.port})
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", n.port, err)
	}

	n.logger.With("port", n.port).Info("listening for TCP/UDP connections")

	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		return n.handleTCP(ctx, tcp)
	})

	group.Go(func() error {
		return n.handleUDP(ctx, udp)
	})

	group.Go(func() error {
		return n.gossip(ctx)
	})

	group.Go(func() error {
		<-ctx.Done()
		return tcp.Close()
	})

	group.Go(func() error {
		<-ctx.Done()
		return udp.Close()
	})

	return group.Wait()
}

func (n *Node) bootstrap(ctx context.Context) error {
	if n.key == nil {
		key, err := n.curve.GenerateKey(rand.Reader)
		if err != nil {
			return fmt.Errorf("failed to generate private key: %w", err)
		}

		n.logger.With("public_key", base64.StdEncoding.EncodeToString(key.PublicKey().Bytes())).Info("generated key")
		n.key = key
	}

	addr, err := netip.ParseAddrPort(n.address)
	if err != nil {
		return fmt.Errorf("failed to parse local address %q: %w", n.address, err)
	}

	// Save a peer record for the local node
	local := Peer{
		ID:        n.id,
		Address:   addr,
		PublicKey: n.key.PublicKey(),
		Delta:     time.Now().Unix(),
		Metadata:  n.metadata,
		Status:    PeerStatusJoining,
	}

	if err = n.store.SavePeer(ctx, local); err != nil {
		return fmt.Errorf("failed to save local peer: %w", err)
	}

	return nil
}

// handleTCP handles individual TCP connections. Whisper only uses TCP to handle join requests from other peers, as TLS
// is used to establish trust between peers initially. Once a peer has successfully joined, all further communication
// is done using UDP and derived keys for encryption.
//
// A successful join request should be responded to with this peer's entire current state.
func (n *Node) handleTCP(ctx context.Context, listener *net.TCPListener) error {
	var group sync.WaitGroup
	defer group.Wait()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			conn, err := listener.AcceptTCP()
			switch {
			case errors.Is(err, net.ErrClosed):
				return ctx.Err()
			case err != nil:
				n.logger.With("error", err).Error("failed to accept TCP connection")
				continue
			}

			group.Add(1)
			go n.handleJoin(ctx, &group, conn)
		}
	}
}

func (n *Node) handleJoin(ctx context.Context, group *sync.WaitGroup, conn *net.TCPConn) {
	defer group.Done()
	defer conn.Close()

	buf := make([]byte, 1024)
	length, err := conn.Read(buf)
	switch {
	case errors.Is(err, io.EOF):
		return
	case err != nil:
		n.logger.With("error", err).Error("failed to read from TCP connection")
		return
	}

	var remote Peer
	if err = remote.Unmarshal(n.curve, buf[:length]); err != nil {
		n.logger.With("error", err).Error("failed to unmarshal peer")
		return
	}

	if err = n.store.SavePeer(ctx, remote); err != nil {
		n.logger.With("error", err, "peer", remote.ID).Error("failed to save peer")
		return
	}

	n.logger.With("peer", remote.ID).Info("peer joined")
	n.events <- Event{
		Type: EventTypePeerDiscovered,
		Peer: remote,
	}

	peers, err := n.store.ListPeers(ctx)
	if err != nil {
		n.logger.With("error", err).Error("failed to list peers")
		return
	}

	for _, peer := range peers {
		if ctx.Err() != nil {
			return
		}

		data, err := peer.Marshal()
		if err != nil {
			n.logger.With("error", err).Error("failed to marshal peer")
			continue
		}

		if _, err = conn.Write(data); err != nil {
			n.logger.With("error", err).Error("failed to write peer to TCP connection")
			continue
		}
	}
}

// handleUDP handles individual UDP packets. These packets come from other peers to keep this node updated with the
// latest state of all other peers.
func (n *Node) handleUDP(ctx context.Context, conn *net.UDPConn) error {
	const udpSize = 65535

	var group sync.WaitGroup
	defer group.Wait()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			buf := make([]byte, udpSize)
			length, _, err := conn.ReadFromUDP(buf)
			switch {
			case errors.Is(err, net.ErrClosed):
				return ctx.Err()
			case err != nil:
				n.logger.With("error", err).Error("failed to read UDP packet")
				continue
			}

			payload := buf[:length]
			group.Add(1)
			go n.handlePeerMessage(ctx, &group, payload)
		}
	}
}

// handlePeerMessage handles a single inbound UDP datagram that contains information on a peer in the gossip cluster.
// Should this peer find that an inbound packet contains data older than what is stored locally, this peer will attempt
// to update the calling peer with the newer data.
//
// All UDP packets contain encrypted peer data using derived keys. Each packet indicates its source peer as a header,
// which this peer will use to look up its public key and generate a derived key.
func (n *Node) handlePeerMessage(ctx context.Context, group *sync.WaitGroup, payload []byte) {
	defer group.Done()

	if len(payload) < 9 {
		n.logger.Warn("received invalid peer message")
		return
	}

	version := payload[0]
	if version != ProtocolVersion1 {
		n.logger.With("version", version).Warn("received peer message with unsupported protocol version")
		return
	}

	sourceID := binary.BigEndian.Uint64(payload[1:9])
	source, err := n.store.FindPeer(ctx, sourceID)
	switch {
	case errors.Is(err, ErrPeerNotFound):
		n.logger.With("peer", sourceID).Warn("received message from unknown peer")
		return
	case err != nil:
		n.logger.With("peer", sourceID, "error", err).Error("failed to find peer")
		return
	}

	// Once we've validated the peer message's version and source, we send the remainder of the packet (nonce + ciphertext)
	// to be decrypted.
	remote, err := n.decryptPeer(source.PublicKey, payload[9:])
	if err != nil {
		n.logger.With("peer", sourceID, "error", err).Error("failed to decrypt peer message")
		return
	}

	local, err := n.store.FindPeer(ctx, remote.ID)
	switch {
	case errors.Is(err, ErrPeerNotFound):
		n.logger.With("peer", remote.ID, "address", remote.Address).Info("discovered new peer")
		if err = n.store.SavePeer(ctx, remote); err != nil {
			n.logger.With("peer", remote.ID, "error", err).Error("failed to save peer")
		}

		n.events <- Event{
			Type: EventTypePeerDiscovered,
			Peer: remote,
		}

		return
	case err != nil:
		n.logger.With("peer", remote.ID, "error", err).Error("failed to find peer")
		return
	}

	// If the remote peer is newer than what we have locally, save it. From here we need not take any further action.
	if remote.Delta > local.Delta {
		if err = n.store.SavePeer(ctx, remote); err != nil {
			n.logger.With("peer", remote.ID, "error", err).Error("failed to save peer")
		}

		n.events <- Event{
			Type: EventTypePeerUpdated,
			Peer: remote,
		}

		return
	}

	if remote.Delta == local.Delta {
		return
	}

	// If our local data is newer, we'll send our local state to the peer that messaged us.
	if err = n.sendPeerMessage(source, local); err != nil {
		n.logger.With("peer", remote.ID, "error", err).Error("failed to send peer message")
	}
}

// decryptPeer generates a shared secret using the node's local private key and the provided public key. It uses
// this key to decrypt the provided payload and parses its contents into a Peer type.
func (n *Node) decryptPeer(publicKey *ecdh.PublicKey, payload []byte) (Peer, error) {
	secret, err := n.key.ECDH(publicKey)
	if err != nil {
		return Peer{}, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	block, err := aes.NewCipher(secret)
	if err != nil {
		return Peer{}, fmt.Errorf("failed to create AES block: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Peer{}, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(payload) < nonceSize+gcm.Overhead() {
		return Peer{}, io.ErrUnexpectedEOF
	}

	nonce := payload[:nonceSize]
	ciphertext := payload[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return Peer{}, fmt.Errorf("failed to decrypt peer: %w", err)
	}

	if len(plaintext) < 8+8+publicKeySize+1+2 {
		return Peer{}, io.ErrUnexpectedEOF
	}

	var peer Peer
	if err = peer.Unmarshal(n.curve, plaintext); err != nil {
		return Peer{}, fmt.Errorf("failed to unmarshal peer: %w", err)
	}

	return peer, nil
}

func (n *Node) sendPeerMessage(target Peer, peer Peer) error {
	udp, err := net.ResolveUDPAddr("udp", target.Address.String())
	if err != nil {
		return fmt.Errorf("failed to resolve UDP address: %w", err)
	}

	conn, err := net.DialUDP("udp", nil, udp)
	if err != nil {
		return fmt.Errorf("failed to dial UDP: %w", err)
	}

	defer conn.Close()
	nonce, ciphertext, err := n.encryptPeer(target, peer)
	if err != nil {
		return fmt.Errorf("failed to encrypt peer: %w", err)
	}

	id := make([]byte, 8)
	binary.BigEndian.PutUint64(id, n.id)

	buf := make([]byte, 0, 1+8+len(nonce)+len(ciphertext))
	buf = append(buf, ProtocolVersion1)
	buf = append(buf, id...)
	buf = append(buf, nonce...)
	buf = append(buf, ciphertext...)

	if _, err = conn.Write(buf); err != nil {
		return fmt.Errorf("failed to write peer message: %w", err)
	}

	return nil
}

func (n *Node) encryptPeer(target Peer, peer Peer) (nonce, ciphertext []byte, err error) {
	secret, err := n.key.ECDH(target.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	block, err := aes.NewCipher(secret)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create AES block: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	plaintext, err := peer.Marshal()
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
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			peers, err := n.store.ListPeers(ctx)
			if err != nil {
				n.logger.With("error", err).Error("failed to list peers")
				continue
			}

			if len(peers) == 0 {
				n.logger.Warn("no peers to update")
				continue
			}

			idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(peers))))
			if err != nil {
				n.logger.With("error", err).Error("failed to select peer")
				continue
			}

			target := peers[idx.Int64()]
			if target.ID == n.id {
				continue
			}

			logger := n.logger.With("peer", target.ID)
			logger.Info("sending peer updates")
			for _, peer := range peers {
				if peer.ID == target.ID {
					continue
				}

				if err = n.sendPeerMessage(target, peer); err != nil {
					logger.With("error", err).Error("failed to send peer message")
				}
			}
		}
	}
}
