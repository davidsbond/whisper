package peer_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/davidsbond/whisper/pkg/peer"
)

func TestPeer_IsEmpty(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name     string
		Peer     peer.Peer
		Expected bool
	}{
		{
			Name:     "unspecified",
			Expected: true,
			Peer: peer.Peer{
				Status: peer.StatusUnspecified,
			},
		},
		{
			Name:     "joining",
			Expected: false,
			Peer: peer.Peer{
				Status: peer.StatusJoining,
			},
		},
		{
			Name:     "joined",
			Expected: false,
			Peer: peer.Peer{
				Status: peer.StatusJoined,
			},
		},
		{
			Name:     "left",
			Expected: false,
			Peer: peer.Peer{
				Status: peer.StatusLeft,
			},
		},
		{
			Name:     "gone",
			Expected: false,
			Peer: peer.Peer{
				Status: peer.StatusGone,
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			assert.EqualValues(t, tc.Expected, tc.Peer.IsEmpty())
		})
	}
}
