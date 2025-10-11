package whisper

import (
	"crypto/ecdh"
	"log/slog"

	"google.golang.org/protobuf/proto"

	"github.com/davidsbond/whisper/internal/store"
)

type (
	Option func(*config)

	config struct {
		id          uint64
		address     string
		port        int
		key         *ecdh.PrivateKey
		metadata    proto.Message
		curve       ecdh.Curve
		joinAddress string
		logger      *slog.Logger
		store       peerStore
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
	}
}

func WithID(id uint64) Option {
	return func(c *config) {
		c.id = id
	}
}

func WithAddress(address string) Option {
	return func(c *config) {
		c.address = address
	}
}

func WithPort(port int) Option {
	return func(c *config) {
		c.port = port
	}
}

func WithKey(key *ecdh.PrivateKey) Option {
	return func(c *config) {
		c.key = key
	}
}

func WithMetadata(metadata proto.Message) Option {
	return func(c *config) {
		c.metadata = metadata
	}
}

func WithCurve(curve ecdh.Curve) Option {
	return func(c *config) {
		c.curve = curve
	}
}

func WithLogger(logger *slog.Logger) Option {
	return func(c *config) {
		c.logger = logger
	}
}

func WithStore(store peerStore) Option {
	return func(c *config) {
		c.store = store
	}
}

func WithJoinAddress(joinAddress string) Option {
	return func(c *config) {
		c.joinAddress = joinAddress
	}
}
