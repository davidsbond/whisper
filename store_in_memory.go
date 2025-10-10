package whisper

import (
	"context"
	"maps"
	"slices"
	"sync"
)

type (
	InMemoryPeerStore struct {
		mux   sync.RWMutex
		peers map[uint64]Peer
	}
)

func NewInMemoryPeerStore() *InMemoryPeerStore {
	return &InMemoryPeerStore{
		peers: make(map[uint64]Peer),
	}
}

func (s *InMemoryPeerStore) FindPeer(ctx context.Context, id uint64) (Peer, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()

	if p, ok := s.peers[id]; ok {
		return p, ctx.Err()
	}

	return Peer{}, ErrPeerNotFound
}

func (s *InMemoryPeerStore) SavePeer(ctx context.Context, peer Peer) error {
	s.mux.Lock()
	defer s.mux.Unlock()

	record, ok := s.peers[peer.ID]
	switch {
	case !ok:
		break
	case record.Delta > peer.Delta:
		return ctx.Err()
	}

	s.peers[peer.ID] = peer
	return ctx.Err()
}

func (s *InMemoryPeerStore) ListPeers(ctx context.Context) ([]Peer, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()

	peers := slices.Collect(maps.Values(s.peers))

	slices.SortFunc(peers, func(a, b Peer) int {
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
