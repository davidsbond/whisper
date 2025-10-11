package store

import (
	"context"
	"maps"
	"slices"
	"sync"

	whisperv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/v1"
)

type (
	InMemoryStore struct {
		mux   sync.RWMutex
		peers map[uint64]*whisperv1.Peer
	}
)

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		peers: make(map[uint64]*whisperv1.Peer),
	}
}

func (s *InMemoryStore) FindPeer(ctx context.Context, id uint64) (*whisperv1.Peer, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()

	peer, ok := s.peers[id]
	if !ok {
		return nil, ErrPeerNotFound
	}

	return peer, ctx.Err()
}

func (s *InMemoryStore) SavePeer(ctx context.Context, peer *whisperv1.Peer) error {
	s.mux.Lock()
	defer s.mux.Unlock()

	existing, ok := s.peers[peer.GetId()]
	if !ok {
		s.peers[peer.GetId()] = peer
		return ctx.Err()
	}

	if peer.GetDelta() < existing.GetDelta() {
		return ctx.Err()
	}

	s.peers[peer.GetId()] = peer
	return ctx.Err()
}

func (s *InMemoryStore) ListPeers(ctx context.Context) ([]*whisperv1.Peer, error) {
	s.mux.RLock()
	defer s.mux.RUnlock()

	peers := slices.Collect(maps.Values(s.peers))

	slices.SortFunc(peers, func(a, b *whisperv1.Peer) int {
		if a.GetId() == b.GetId() {
			return 0
		}

		if a.GetDelta() < b.GetDelta() {
			return -1
		}

		return 1
	})

	return peers, ctx.Err()
}
