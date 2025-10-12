//go:generate go tool buf format -w
//go:generate go tool buf generate

// Package whisper provides a zero-trust, gRPC-based gossip protocol. Each peer within the network advertises itself
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
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
	whisperv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/v1"
	"github.com/davidsbond/whisper/internal/service"
	"github.com/davidsbond/whisper/pkg/peer"
	"github.com/davidsbond/whisper/pkg/store"
)

// Run a whisper node.
func Run(ctx context.Context, options ...Option) error {
	cfg := defaultConfig()
	for _, opt := range options {
		opt(cfg)
	}

	if err := bootstrap(ctx, cfg); err != nil {
		return fmt.Errorf("failed to bootstrap peer: %w", err)
	}

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return listenTCP(ctx, cfg)
	})

	group.Go(func() error {
		return listenUDP(ctx, cfg)
	})

	group.Go(func() error {
		return gossip(ctx, cfg)
	})

	group.Go(func() error {
		<-ctx.Done()
		return leave(cfg)
	})

	return group.Wait()
}

func bootstrap(ctx context.Context, cfg *config) error {
	self := peer.Peer{
		ID:      cfg.id,
		Address: cfg.address,
		Delta:   time.Now().Unix(),
		Status:  peer.StatusJoining,
	}

	if cfg.joinAddress == "" {
		self.Status = peer.StatusJoined
	}

	if cfg.metadata != nil {
		metadata, err := anypb.New(cfg.metadata)
		if err != nil {
			return fmt.Errorf("invalid metadata: %w", err)
		}

		self.Metadata = metadata
	}

	if cfg.key == nil {
		key, err := cfg.curve.GenerateKey(rand.Reader)
		if err != nil {
			return fmt.Errorf("failed to generate key: %w", err)
		}

		cfg.logger.
			With("public_key", base64.StdEncoding.EncodeToString(key.PublicKey().Bytes())).
			Debug("generated new private key")

		self.PublicKey = key.PublicKey()
		cfg.key = key
	}

	if err := cfg.store.SavePeer(ctx, self); err != nil {
		return fmt.Errorf("failed to save local peer record: %w", err)
	}

	if cfg.joinAddress != "" {
		if err := join(ctx, cfg, self); err != nil {
			return fmt.Errorf("failed to join gossip network: %w", err)
		}
	}

	return nil
}

func join(ctx context.Context, cfg *config, self peer.Peer) error {
	cfg.logger.With("address", cfg.address).Info("joining gossip network")

	client, closer, err := dialPeer(cfg.joinAddress)
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
		publicKey, err := cfg.curve.NewPublicKey(protoPeer.GetPublicKey())
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

		if err = cfg.store.SavePeer(ctx, p); err != nil {
			return fmt.Errorf("failed to save peer record: %w", err)
		}
	}

	return nil
}

func leave(cfg *config) error {
	cfg.logger.Debug("leaving gossip network")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	peers, err := cfg.store.ListPeers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list peers: %w", err)
	}

	var selected peer.Peer
	for _, p := range peers {
		if p.ID == cfg.id {
			continue
		}

		if p.Status == peer.StatusJoined {
			selected = p
			break
		}
	}

	if selected.IsEmpty() {
		return errors.New("failed to find an active peer to inform for leaving gossip network")
	}

	client, closer, err := dialPeer(selected.Address)
	if err != nil {
		return fmt.Errorf("failed to dial peer: %w", err)
	}

	defer closer()
	request := &whispersvcv1.LeaveRequest{Id: cfg.id}
	if _, err = client.Leave(ctx, request); err != nil {
		return fmt.Errorf("failed to send leave request: %w", err)
	}

	return nil
}

func listenTCP(ctx context.Context, cfg *config) error {
	tcp, err := net.ListenTCP("tcp", &net.TCPAddr{Port: cfg.port})
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", cfg.port, err)
	}

	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(cfg.interceptors...),
	)

	service.New(cfg.id, cfg.store, cfg.curve).Register(server)

	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		cfg.logger.With("port", cfg.port).Debug("serving gRPC")

		return server.Serve(tcp)
	})

	group.Go(func() error {
		<-ctx.Done()

		cfg.logger.Debug("shutting down gRPC server")
		server.GracefulStop()
		return tcp.Close()
	})

	return group.Wait()
}

func listenUDP(ctx context.Context, cfg *config) error {
	const udpSize = 65535

	udp, err := net.ListenUDP("udp", &net.UDPAddr{Port: cfg.port})
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", cfg.port, err)
	}

	cfg.logger.With("port", cfg.port).Debug("serving UDP")

	var group sync.WaitGroup
	defer group.Wait()

	for {
		select {
		case <-ctx.Done():
			cfg.logger.Debug("shutting down UDP server")
			return udp.Close()
		default:
			buf := make([]byte, udpSize)
			length, _, err := udp.ReadFromUDP(buf)
			if err != nil {
				cfg.logger.With("error", err).Error("failed to read UDP packet")
				continue
			}

			payload := buf[:length]
			group.Add(1)
			go handlePeerMessage(ctx, &group, cfg, payload)
		}
	}
}

func handlePeerMessage(ctx context.Context, group *sync.WaitGroup, cfg *config, payload []byte) {
	defer group.Done()

	message := &whisperv1.PeerMessage{}
	if err := proto.Unmarshal(payload, message); err != nil {
		cfg.logger.With("error", err).Error("failed to unmarshal peer message")
		return
	}

	logger := cfg.logger.With("source_id", message.GetSourceId())

	source, err := cfg.store.FindPeer(ctx, message.GetSourceId())
	switch {
	case errors.Is(err, store.ErrPeerNotFound):
		logger.Warn("received message from unknown peer")
		return
	case err != nil:
		logger.With("error", err).Error("failed to lookup peer")
		return
	}

	remote, err := decryptPeer(cfg, source, message)
	if err != nil {
		logger.With("error", err).Error("failed to decrypt peer")
		return
	}

	logger = logger.With("peer_id", remote.ID)

	local, err := cfg.store.FindPeer(ctx, remote.ID)
	switch {
	case errors.Is(err, store.ErrPeerNotFound):
		break
	case err != nil:
		logger.With("error", err).Error("failed to lookup peer")
		return
	}

	// The inbound peer data is newer than ours, so we should update our local state.
	if remote.Delta > local.Delta {
		logger.Debug("got new peer data")

		if err = cfg.store.SavePeer(ctx, remote); err != nil {
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
	logger.Debug("got old peer data, updating source")
	if err = sendPeerMessage(cfg, source, local); err != nil {
		logger.With("error", err).Error("failed to send peer message")
		return
	}
}

func sendPeerMessage(cfg *config, target, local peer.Peer) error {
	udp, err := net.ResolveUDPAddr("udp", target.Address)
	if err != nil {
		return fmt.Errorf("failed to resolve UDP address: %w", err)
	}

	conn, err := net.DialUDP("udp", nil, udp)
	if err != nil {
		return fmt.Errorf("failed to dial UDP: %w", err)
	}

	defer conn.Close()
	nonce, ciphertext, err := encryptPeer(cfg, target, local)
	if err != nil {
		return fmt.Errorf("failed to encrypt peer: %w", err)
	}

	message := &whisperv1.PeerMessage{
		SourceId:   cfg.id,
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

func decryptPeer(cfg *config, source peer.Peer, message *whisperv1.PeerMessage) (peer.Peer, error) {
	secret, err := cfg.key.ECDH(source.PublicKey)
	if err != nil {
		return peer.Peer{}, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	block, err := aes.NewCipher(secret)
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

	publicKey, err := cfg.curve.NewPublicKey(protoPeer.GetPublicKey())
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

func encryptPeer(cfg *config, target, peer peer.Peer) (nonce, ciphertext []byte, err error) {
	secret, err := cfg.key.ECDH(target.PublicKey)
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

func gossip(ctx context.Context, cfg *config) error {
	stateTicker := time.NewTicker(time.Second)
	defer stateTicker.Stop()

	checkTicker := time.NewTicker(time.Minute)
	defer checkTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stateTicker.C:
			if err := shareState(ctx, cfg); err != nil {
				cfg.logger.With("error", err).Error("failed to share state")
			}
		case <-checkTicker.C:
			if err := checkPeer(ctx, cfg); err != nil {
				cfg.logger.With("error", err).Error("failed to check peer")
			}
		}
	}
}

func shareState(ctx context.Context, cfg *config) error {
	target, err := selectPeer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to select peer: %w", err)
	}

	if target.IsEmpty() {
		return nil
	}

	peers, err := cfg.store.ListPeers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list peers: %w", err)
	}

	for _, p := range peers {
		if p.ID == target.ID {
			// Don't tell peers about themselves, each peer owns its own state except in the scenario where
			// one is leaving. But if it's leaving, it won't care for more updates.
			continue
		}

		if err = sendPeerMessage(cfg, target, p); err != nil {
			return fmt.Errorf("failed to send peer message to peer %q: %w", target.ID, err)
		}
	}

	return nil
}

func checkPeer(ctx context.Context, cfg *config) error {
	target, err := selectPeer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to select peer: %w", err)
	}

	if target.IsEmpty() {
		return nil
	}

	client, closer, err := dialPeer(target.Address)
	if err != nil {
		if err = checkPeerViaPeer(ctx, cfg, target); err != nil {
			return fmt.Errorf("failed to check peer %q via peer: %w", target.ID, err)
		}
	}

	defer closer()
	if _, err = client.Status(ctx, &whispersvcv1.StatusRequest{}); err != nil {
		if err = checkPeerViaPeer(ctx, cfg, target); err != nil {
			return fmt.Errorf("failed to check peer %q via peer: %w", target.ID, err)
		}
	}

	return nil
}

func checkPeerViaPeer(ctx context.Context, cfg *config, target peer.Peer) error {
	var selected peer.Peer

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			checker, err := selectPeer(ctx, cfg)
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

	cfg.logger.With("target_id", target.ID, "checking_id", selected.ID).Info("checking peer liveness via peer")

	client, closer, err := dialPeer(selected.Address)
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
		return checkPeerViaPeer(ctx, cfg, target)
	default:
		if err = markPeerGone(ctx, cfg, target); err != nil {
			return fmt.Errorf("failed to mark peer %q as gone: %w", target.ID, err)
		}
	}

	return nil
}

func markPeerGone(ctx context.Context, cfg *config, target peer.Peer) error {
	target.Status = peer.StatusGone
	target.Delta = time.Now().Unix()

	if err := cfg.store.SavePeer(ctx, target); err != nil {
		return fmt.Errorf("failed to save peer: %w", err)
	}

	return nil
}

func selectPeer(ctx context.Context, cfg *config) (peer.Peer, error) {
	peers, err := cfg.store.ListPeers(ctx)
	if err != nil {
		return peer.Peer{}, err
	}

	// If we only have one peer, we're a standalone node.
	if len(peers) <= 1 {
		return peer.Peer{}, nil
	}

	idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(peers))))
	if err != nil {
		return peer.Peer{}, err
	}

	target := peers[idx.Int64()]

	// We never want to select ourselves or a peer with an inactive status.
	if target.ID == cfg.id || target.Status != peer.StatusJoined {
		return selectPeer(ctx, cfg)
	}

	return target, nil
}

func dialPeer(address string) (whispersvcv1.WhisperServiceClient, func(), error) {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create grpc client: %w", err)
	}

	client := whispersvcv1.NewWhisperServiceClient(conn)
	return client, func() {
		conn.Close()
	}, nil
}
