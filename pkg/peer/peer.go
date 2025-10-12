// Package peer provides types for working with whisper peers.
package peer

import (
	"crypto/ecdh"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
)

type (
	// The Peer type represents a single member of the gossip network.
	Peer struct {
		// The peer's unique identifier.
		ID uint64
		// The address (ip:port) that the peer is available at for TCP/UDP traffic.
		Address string
		// An integer representing how new the state of the peer is (higher number, newer state).
		Delta int64
		// The current status of the peer.
		Status Status
		// The peer's public key to use for deriving shared secrets for UDP packets.
		PublicKey *ecdh.PublicKey
		// Arbitrary metadata advertised by the peer.
		Metadata proto.Message
	}

	// The Status type describes different statuses a Peer can have.
	Status int
)

// IsEmpty returns true if the peer has an unspecified status. In all valid scenarios a peer should have a non-zero
// status set.
func (p Peer) IsEmpty() bool {
	return p.Status == StatusUnspecified
}

const (
	StatusUnspecified Status = iota
	// StatusJoining describes a peer currently attempting to join the gossip network.
	StatusJoining
	// StatusJoined describes a peer that is actively participating in the gossip network.
	StatusJoined
	// StatusLeft describes a peer that has left the gossip network.
	StatusLeft
	// StatusGone describes a peer that may have failed and is no longer accessible within the gossip network.
	StatusGone
)

// Dial the peer at the given address. Returns the client, a closer function and a possible error. The closer function
// must be called when you are done with the client.
func Dial(address string) (whispersvcv1.WhisperServiceClient, func(), error) {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create grpc client: %w", err)
	}

	client := whispersvcv1.NewWhisperServiceClient(conn)
	return client, func() {
		conn.Close()
	}, nil
}
