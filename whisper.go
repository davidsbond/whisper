//go:generate go tool buf format -w
//go:generate go tool buf generate

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
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
	whisperv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/v1"
	"github.com/davidsbond/whisper/internal/service"
	"github.com/davidsbond/whisper/internal/store"
)

type (
	peerStore interface {
		FindPeer(ctx context.Context, id uint64) (*whisperv1.Peer, error)
		SavePeer(ctx context.Context, peer *whisperv1.Peer) error
		ListPeers(ctx context.Context) ([]*whisperv1.Peer, error)
	}
)

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
	self := &whisperv1.Peer{
		Id:      cfg.id,
		Address: cfg.address,
		Delta:   time.Now().Unix(),
		Status:  whisperv1.PeerStatus_PEER_STATUS_JOINING,
	}

	if cfg.joinAddress == "" {
		self.Status = whisperv1.PeerStatus_PEER_STATUS_JOINED
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

		self.PublicKey = key.PublicKey().Bytes()
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

func join(ctx context.Context, cfg *config, self *whisperv1.Peer) error {
	client, closer, err := dialPeer(cfg.joinAddress)
	if err != nil {
		return fmt.Errorf("failed to dial peer: %w", err)
	}

	defer closer()
	request := &whispersvcv1.JoinRequest{Peer: self}
	response, err := client.Join(ctx, request)
	if err != nil {
		return fmt.Errorf("failed to send join request: %w", err)
	}

	for _, peer := range response.GetPeers() {
		if err = cfg.store.SavePeer(ctx, peer); err != nil {
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

	var selected *whisperv1.Peer
	for _, peer := range peers {
		if peer.Id == cfg.id {
			continue
		}

		if peer.Status == whisperv1.PeerStatus_PEER_STATUS_JOINED {
			selected = peer
			break
		}
	}

	if selected == nil {
		return errors.New("failed to find an active peer to inform for leaving gossip network")
	}

	client, closer, err := dialPeer(selected.GetAddress())
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

	server := grpc.NewServer()
	service.New(cfg.store, cfg.curve).Register(server)

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

	logger = logger.With("peer_id", remote.GetId())

	local, err := cfg.store.FindPeer(ctx, remote.GetId())
	switch {
	case errors.Is(err, store.ErrPeerNotFound):
		break
	case err != nil:
		logger.With("error", err).Error("failed to lookup peer")
		return
	}

	// The inbound peer data is newer than ours, so we should update our local state.
	if remote.GetDelta() > local.GetDelta() {
		logger.Debug("got new peer data")

		if err = cfg.store.SavePeer(ctx, remote); err != nil {
			logger.With("error", err).Error("failed to save peer")
		}

		return
	}

	// We're already up-to-date for this peer, so we'll exit early here.
	if remote.GetDelta() == local.GetDelta() {
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

func sendPeerMessage(cfg *config, target, local *whisperv1.Peer) error {
	udp, err := net.ResolveUDPAddr("udp", target.GetAddress())
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

func decryptPeer(cfg *config, source *whisperv1.Peer, message *whisperv1.PeerMessage) (*whisperv1.Peer, error) {
	publicKey, err := cfg.curve.NewPublicKey(source.GetPublicKey())
	if err != nil {
		return nil, fmt.Errorf("failed to parse public key: %w", err)
	}

	secret, err := cfg.key.ECDH(publicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to derive shared secret: %w", err)
	}

	block, err := aes.NewCipher(secret)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES block: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	nonce := message.GetNonce()
	ciphertext := message.GetCiphertext()

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt peer: %w", err)
	}

	peer := &whisperv1.Peer{}
	if err = proto.Unmarshal(plaintext, peer); err != nil {
		return nil, fmt.Errorf("failed to unmarshal peer: %w", err)
	}

	return peer, nil
}

func encryptPeer(cfg *config, target, peer *whisperv1.Peer) (nonce, ciphertext []byte, err error) {
	publicKey, err := cfg.curve.NewPublicKey(target.GetPublicKey())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse public key: %w", err)
	}

	secret, err := cfg.key.ECDH(publicKey)
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

	plaintext, err := proto.Marshal(peer)
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
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			peers, err := cfg.store.ListPeers(ctx)
			if err != nil {
				cfg.logger.With("error", err).Error("failed to list peers")
				continue
			}

			if len(peers) == 0 {
				continue
			}

			idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(peers))))
			if err != nil {
				cfg.logger.With("error", err).Error("failed to select peer")
				continue
			}

			target := peers[idx.Int64()]
			if target.GetId() == cfg.id || target.Status != whisperv1.PeerStatus_PEER_STATUS_JOINED {
				continue
			}

			logger := cfg.logger.With("target_id", target.GetId())

			for _, peer := range peers {
				if err = sendPeerMessage(cfg, target, peer); err != nil {
					logger.With("error", err, "peer_id", peer.GetId()).Error("failed to send peer message")
				}
			}
		}
	}
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
