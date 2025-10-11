package store

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/davidsbond/whisper/pkg/peer"
)

type (
	InMemoryStore struct {
		mux   sync.RWMutex
		peers map[uint64]peer.Peer
	}
)

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		peers: make(map[uint64]peer.Peer),
	}
}

func (s *InMemoryStore) FindPeer(ctx context.Context, id uint64) (peer.Peer, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()

	p, ok := s.peers[id]
	if !ok {
		return peer.Peer{}, ErrPeerNotFound
	}

	return p, ctx.Err()
}

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

func (s *InMemoryStore) ListPeers(ctx context.Context) ([]peer.Peer, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()

	peers := slices.Collect(maps.Values(s.peers))

	slices.SortFunc(peers, func(a, b peer.Peer) int {
		if a.ID == b.ID {
			return 0
		}

		if a.ID < b.ID {
			return -1
		}

		return 1
	})

	return peers, ctx.Err()
}
