package peer

import (
	"crypto/ecdh"

	"google.golang.org/protobuf/proto"
)

type (
	Peer struct {
		ID        uint64
		Address   string
		Delta     int64
		Status    Status
		PublicKey *ecdh.PublicKey
		Metadata  proto.Message
	}

	Status int
)

func (p Peer) IsEmpty() bool {
	return p.Status == StatusUnspecified
}

const (
	StatusUnspecified Status = iota
	StatusJoining
	StatusJoined
	StatusLeft
	StatusGone
)
