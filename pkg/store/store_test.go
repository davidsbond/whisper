package store_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/davidsbond/whisper/pkg/peer"
	"github.com/davidsbond/whisper/pkg/store"
)

func TestInMemoryStore(t *testing.T) {
	t.Parallel()

	mem := store.NewInMemoryStore()

	t.Run("stores peer", func(t *testing.T) {
		expected := peer.Peer{
			ID:       1,
			Address:  "0.0.0.0:1234",
			Delta:    100,
			Status:   peer.StatusJoining,
			Metadata: timestamppb.Now(),
		}

		require.NoError(t, mem.SavePeer(t.Context(), expected))

		actual, err := mem.FindPeer(t.Context(), expected.ID)
		require.NoError(t, err)
		assert.EqualValues(t, expected, actual)
	})

	t.Run("updates peer with newer delta", func(t *testing.T) {
		expected := peer.Peer{
			ID:       1,
			Address:  "0.0.0.0:1234",
			Delta:    200,
			Status:   peer.StatusJoining,
			Metadata: timestamppb.Now(),
		}

		require.NoError(t, mem.SavePeer(t.Context(), expected))

		actual, err := mem.FindPeer(t.Context(), expected.ID)
		require.NoError(t, err)
		assert.EqualValues(t, expected, actual)
	})

	t.Run("ignores peer with older delta", func(t *testing.T) {
		expected := peer.Peer{
			ID:       1,
			Address:  "0.0.0.0:1234",
			Delta:    50,
			Status:   peer.StatusJoining,
			Metadata: timestamppb.Now(),
		}

		require.NoError(t, mem.SavePeer(t.Context(), expected))

		actual, err := mem.FindPeer(t.Context(), expected.ID)
		require.NoError(t, err)
		assert.NotEqualValues(t, expected, actual)
	})

	t.Run("lists peers", func(t *testing.T) {
		actual, err := mem.ListPeers(t.Context())
		require.NoError(t, err)
		assert.Len(t, actual, 1)
	})

	t.Run("error for unknown peer", func(t *testing.T) {
		_, err := mem.FindPeer(t.Context(), 1337)
		require.Error(t, err)
	})

	t.Run("lists peers in id order", func(t *testing.T) {
		err := mem.SavePeer(t.Context(), peer.Peer{
			ID:       3,
			Delta:    100,
			Status:   peer.StatusJoining,
			Metadata: timestamppb.Now(),
		})
		require.NoError(t, err)

		err = mem.SavePeer(t.Context(), peer.Peer{
			ID:       2,
			Delta:    100,
			Status:   peer.StatusJoining,
			Metadata: timestamppb.Now(),
		})
		require.NoError(t, err)

		peers, err := mem.ListPeers(t.Context())
		require.NoError(t, err)
		require.Len(t, peers, 3)
		assert.True(t, slices.IsSortedFunc(peers, func(a, b peer.Peer) int {
			if a.ID == b.ID {
				return 0
			}

			if a.ID < b.ID {
				return -1
			}

			return 1
		}))
	})
}
