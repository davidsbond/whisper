package whisper

import (
	"context"
	"crypto/ecdh"
	"crypto/tls"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/davidsbond/whisper/pkg/peer"
	"github.com/davidsbond/whisper/pkg/store"
)

type (
	// The Option type is a function that modifies the configuration of the whisper node.
	Option func(*config)

	config struct {
		address        string
		port           int
		key            *ecdh.PrivateKey
		metadata       proto.Message
		curve          ecdh.Curve
		joinAddress    string
		logger         *slog.Logger
		store          PeerStore
		gossipInterval time.Duration
		checkInterval  time.Duration
		reapInterval   time.Duration
		clientTLS      *tls.Config
		serverTLS      *tls.Config
	}

	// The PeerStore interface describes types that can persist peer data.
	PeerStore interface {
		// FindPeer should return the peer.Peer whose identifier matches the one provided. It should return
		// store.ErrPeerNotFound if a matching peer does not exist.
		FindPeer(ctx context.Context, id uint64) (peer.Peer, error)
		// SavePeer should persist the given peer.Peer.
		SavePeer(ctx context.Context, peer peer.Peer) error
		// ListPeers should return all peers within the store.
		ListPeers(ctx context.Context) ([]peer.Peer, error)
		// RemovePeer should remove a peer from the store. It should return store.ErrPeerNotFound if a matching
		// peer does not exist.
		RemovePeer(ctx context.Context, id uint64) error
	}
)

func defaultConfig() *config {
	return &config{
		// We'll serve TCP/UDP on port 8000 and inform other peers we do so.
		address: "0.0.0.0:8000",
		port:    8000,
		// A key will be generated if one is not set.
		key: nil,
		// No default metadata.
		metadata: nil,
		// We'll use X25519 for key generation/parsing if unspecified.
		curve: ecdh.X25519(),
		// By default, the whisper node is standalone.
		joinAddress: "",
		logger:      slog.Default(),
		// By default, all peer data is stored in-memory
		store: store.NewInMemoryStore(),
		// Default durations for gossiping
		gossipInterval: 5 * time.Second,
		checkInterval:  time.Second * 10,
		reapInterval:   time.Hour,
	}
}

// WithAddress modifies the address that the whisper node will advertise to its peers for TCP/UDP communication. It
// must be an ip:port combination.
func WithAddress(address string) Option {
	return func(c *config) {
		c.address = address
	}
}

// WithPort modifies the port that the whisper node will use for handling TCP and UDP traffic.
func WithPort(port int) Option {
	return func(c *config) {
		c.port = port
	}
}

// WithKey modifies the private key the whisper node will use to encrypt UDP packets. By default, each whisper node
// will generate a new key on startup.
func WithKey(key *ecdh.PrivateKey) Option {
	return func(c *config) {
		c.key = key
	}
}

// WithMetadata specifies an arbitrary proto.Message implementation that can be set as metadata. This metadata is
// shared with peers in the gossip network. Important to note that you will get errors if not all peers have the
// proto.Message's concrete implementation within their compiled binary.
func WithMetadata(metadata proto.Message) Option {
	return func(c *config) {
		c.metadata = metadata
	}
}

// WithCurve modifies the ecdh.Curve implementation to use for handling public and private keys.
func WithCurve(curve ecdh.Curve) Option {
	return func(c *config) {
		c.curve = curve
	}
}

// WithLogger modifies the logger used by the whisper node. Logs aim to be reasonable and most are at the DEBUG
// level.
func WithLogger(logger *slog.Logger) Option {
	return func(c *config) {
		c.logger = logger
	}
}

// WithStore modifies the PeerStore implementation used by the whisper node to persist peer data. By default, an
// in-memory store is used, leading to a blank state on subsequent startups.
func WithStore(store PeerStore) Option {
	return func(c *config) {
		c.store = store
	}
}

// WithJoinAddress specifies a peer that should be contacted on startup to advertise this whisper node to the network
// and perform an initial sync of available peers.
func WithJoinAddress(joinAddress string) Option {
	return func(c *config) {
		c.joinAddress = joinAddress
	}
}

// WithGossipInterval modifies the interval at which the peer's local state will be shared with a randomly selected
// peer. Defaults to 5 seconds.
func WithGossipInterval(interval time.Duration) Option {
	return func(c *config) {
		c.gossipInterval = interval
	}
}

// WithCheckInterval modifies the interval at which the peer will confirm the liveness of a randomly selected active
// peer. Defaults to 10 seconds.
func WithCheckInterval(interval time.Duration) Option {
	return func(c *config) {
		c.checkInterval = interval
	}
}

// WithReapInterval modifies the interval at which peers will be removed from the store if they have been in the "gone"
// state for longer than the specified interval. Defaults to 1 hour.
func WithReapInterval(interval time.Duration) Option {
	return func(c *config) {
		c.reapInterval = interval
	}
}

// WithTLS modifies the TLS configuration to be used by the node for serving and dialing TCP connections.
func WithTLS(cfg *tls.Config) Option {
	return func(c *config) {
		if cfg == nil {
			return
		}

		serverCfg := cfg.Clone()

		serverCfg.ClientAuth = tls.RequireAndVerifyClientCert
		serverCfg.RootCAs = nil // Server doesn't use RootCAs

		clientCfg := cfg.Clone()
		clientCfg.ClientCAs = nil // Client doesn't use ClientCAs

		c.serverTLS = serverCfg
		c.clientTLS = clientCfg
	}
}
