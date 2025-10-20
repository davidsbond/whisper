// Package peer provides types for working with whisper peers.
package peer

import (
	"crypto/ecdh"
	"crypto/sha256"
	"crypto/tls"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
	whisperv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/v1"
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

		publicKeyHash string
	}

	// The Status type describes different statuses a Peer can have.
	Status int
)

// FromProto converts the provided protobuf representation of a peer into a Peer type. It also includes pre-hashing
// the public key for caching GCMs in the gossip process.
func FromProto(in *whisperv1.Peer, curve ecdh.Curve) (Peer, error) {
	publicKey, err := curve.NewPublicKey(in.GetPublicKey())
	if err != nil {
		return Peer{}, fmt.Errorf("failed to parse public key: %w", err)
	}

	hash := sha256.Sum256(in.GetPublicKey())
	peer := Peer{
		ID:            in.GetId(),
		Address:       in.GetAddress(),
		Delta:         in.GetDelta(),
		Status:        Status(in.GetStatus()),
		PublicKey:     publicKey,
		publicKeyHash: string(hash[:]),
	}

	if in.GetMetadata() != nil {
		peer.Metadata, err = in.GetMetadata().UnmarshalNew()
		if err != nil {
			return Peer{}, fmt.Errorf("invalid peer metadata: %w", err)
		}
	}

	return peer, nil
}

// ToProto converts the provided Peer into its protobuf representation.
func ToProto(peer Peer) (*whisperv1.Peer, error) {
	p := &whisperv1.Peer{
		Id:        peer.ID,
		Address:   peer.Address,
		PublicKey: peer.PublicKey.Bytes(),
		Delta:     peer.Delta,
		Status:    whisperv1.PeerStatus(peer.Status),
	}

	if peer.Metadata != nil {
		metadata, err := anypb.New(peer.Metadata)
		if err != nil {
			return nil, fmt.Errorf("invalid peer metadata: %w", err)
		}

		p.Metadata = metadata
	}

	return p, nil
}

// IsEmpty returns true if the peer has an unspecified status. In all valid scenarios a peer should have a non-zero
// status set.
func (p Peer) IsEmpty() bool {
	return p.Status == StatusUnspecified
}

func (p Peer) Hash() string {
	return p.publicKeyHash
}

const (
	StatusUnspecified Status = iota
	// StatusJoining describes a peer currently attempting to join the gossip network.
	StatusJoining
	// StatusJoined describes a peer that is actively participating in the gossip network.
	StatusJoined
	// StatusLeaving describes a peer currently attempting to leave the gossip network.
	StatusLeaving
	// StatusLeft describes a peer that has left the gossip network.
	StatusLeft
	// StatusGone describes a peer that may have failed and is no longer accessible within the gossip network.
	StatusGone
)

// Dial the peer at the given address. Returns the client, a closer function and a possible error. The closer function
// must be called when you are done with the client.
func Dial(address string, tls *tls.Config) (whispersvcv1.WhisperServiceClient, func(), error) {
	credential := insecure.NewCredentials()
	if tls != nil {
		credential = credentials.NewTLS(tls)
	}

	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credential))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create grpc client: %w", err)
	}

	client := whispersvcv1.NewWhisperServiceClient(conn)
	return client, func() {
		conn.Close()
	}, nil
}
