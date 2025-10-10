package whisper

import (
	"crypto/ecdh"
	"log/slog"
)

type (
	NodeOption func(*nodeConfig)

	nodeConfig struct {
		port     int
		logger   *slog.Logger
		store    PeerStore
		curve    ecdh.Curve
		key      *ecdh.PrivateKey
		address  string
		metadata []byte
	}
)

func defaultNodeConfig() *nodeConfig {
	return &nodeConfig{
		port:    8000,
		logger:  slog.Default().WithGroup("whisper"),
		store:   NewInMemoryPeerStore(),
		curve:   ecdh.X25519(),
		address: "0.0.0.0:8000",
	}
}

func WithPort(port int) NodeOption {
	return func(config *nodeConfig) {
		config.port = port
	}
}

func WithAddress(address string) NodeOption {
	return func(config *nodeConfig) {
		config.address = address
	}
}

func WithLogger(logger *slog.Logger) NodeOption {
	return func(config *nodeConfig) {
		config.logger = logger
	}
}
