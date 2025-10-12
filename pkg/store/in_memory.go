package store

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/davidsbond/whisper/pkg/peer"
)

type (
	// The InMemoryStore type provides a concurrent store for peer data.
	InMemoryStore struct {
		mux   sync.RWMutex
		peers map[uint64]peer.Peer
	}
)

// NewInMemoryStore returns a new instance of the InMemoryStore type.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		peers: make(map[uint64]peer.Peer),
	}
}

// FindPeer attempts to return the specified peer. Returns ErrPeerNotFound if the peer does not exist in the store.
func (s *InMemoryStore) FindPeer(ctx context.Context, id uint64) (peer.Peer, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()

	p, ok := s.peers[id]
	if !ok {
		return peer.Peer{}, ErrPeerNotFound
	}

	return p, ctx.Err()
}

// SavePeer inserts or updates the given peer into the store. If the provided peer already exists and has a lower delta
// than the one already in the store, the update is not performed.
func (s *InMemoryStore) SavePeer(ctx context.Context, peer peer.Peer) error {
	s.mux.Lock()
	defer s.mux.Unlock()

	existing, ok := s.peers[peer.ID]
	if !ok {
		s.peers[peer.ID] = peer
		return ctx.Err()
	}

	if peer.Delta < existing.Delta {
		return ctx.Err()
	}

	s.peers[peer.ID] = peer
	return ctx.Err()
}

// ListPeers returns all peers in the store ordered by their id.
func (s *InMemoryStore) ListPeers(ctx context.Context) ([]peer.Peer, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()

	peers := slices.Collect(maps.Values(s.peers))

	slices.SortFunc(peers, func(a, b peer.Peer) int {
		if a.ID < b.ID {
			return -1
		}

		return 1
	})

	return peers, ctx.Err()
}
