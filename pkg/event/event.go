package event

import (
	"github.com/davidsbond/whisper/pkg/peer"
)

type (
	// The Event type represents a single change in peer state.
	Event struct {
		// The type of event that has occurred.
		Type Type
		// The peer that has changed.
		Peer peer.Peer
	}

	// Type is used to denote the kind of state change that has occurred to a single peer.
	Type int
)

const (
	TypeUnspecified Type = iota
	// TypeDiscovered denotes that a Peer has been newly discovered by the local peer.
	TypeDiscovered
	// TypeUpdated denotes that a Peer's delta has changed, possibly due to a metadata update.
	TypeUpdated
	// TypeLeft denotes that a Peer has left the gossip network.
	TypeLeft
	// TypeGone denotes that a Peer has failed or is not accessible from other peers within the gossip network.
	TypeGone
	// TypeRemoved denotes that a Peer has been completely removed from storage.
	TypeRemoved
)

func (e Type) String() string {
	switch e {
	case TypeDiscovered:
		return "DISCOVERED"
	case TypeUpdated:
		return "UPDATED"
	case TypeLeft:
		return "LEFT"
	case TypeGone:
		return "GONE"
	case TypeRemoved:
		return "REMOVED"
	default:
		return "UNSPECIFIED"
	}
}
