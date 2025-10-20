package store

import (
	"context"
	"slices"

	"github.com/davidsbond/whisper/internal/syncmap"
	"github.com/davidsbond/whisper/pkg/peer"
)

type (
	// The InMemoryStore type provides a concurrent store for peer data.
	InMemoryStore struct {
		peers *syncmap.Map[uint64, peer.Peer]
	}
)

// NewInMemoryStore returns a new instance of the InMemoryStore type.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		peers: syncmap.New[uint64, peer.Peer](),
	}
}

// FindPeer attempts to return the specified peer. Returns ErrPeerNotFound if the peer does not exist in the store.
func (s *InMemoryStore) FindPeer(ctx context.Context, id uint64) (peer.Peer, error) {
	p, ok := s.peers.Get(id)
	if !ok {
		return peer.Peer{}, ErrPeerNotFound
	}

	return p, ctx.Err()
}

// SavePeer inserts or updates the given peer into the store. If the provided peer already exists and has a lower delta
// than the one already in the store, the update is not performed.
func (s *InMemoryStore) SavePeer(ctx context.Context, peer peer.Peer) error {
	existing, ok := s.peers.Get(peer.ID)
	if !ok {
		s.peers.Put(peer.ID, peer)
		return ctx.Err()
	}

	if peer.Delta < existing.Delta {
		return ctx.Err()
	}

	s.peers.Put(peer.ID, peer)
	return ctx.Err()
}

// ListPeers returns all peers in the store ordered by their id.
func (s *InMemoryStore) ListPeers(ctx context.Context) ([]peer.Peer, error) {
	peers := s.peers.Values()

	slices.SortFunc(peers, func(a, b peer.Peer) int {
		if a.ID < b.ID {
			return -1
		}

		return 1
	})

	return peers, ctx.Err()
}

// RemovePeer removes a specified peer from the store. Returns ErrPeerNotFound if a peer with a matching id does not
// exist.
func (s *InMemoryStore) RemovePeer(ctx context.Context, id uint64) error {
	if _, ok := s.peers.Get(id); !ok {
		return ErrPeerNotFound
	}

	s.peers.Remove(id)
	return ctx.Err()
}
