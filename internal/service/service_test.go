package service_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
	whisperv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/v1"
	"github.com/davidsbond/whisper/internal/service"
	"github.com/davidsbond/whisper/pkg/event"
	"github.com/davidsbond/whisper/pkg/peer"
	"github.com/davidsbond/whisper/pkg/store"
)

func TestService_Join(t *testing.T) {
	t.Parallel()

	key := testKey(t)
	logger := testLogger(t)
	curve := ecdh.X25519()

	tt := []struct {
		Name         string
		ID           uint64
		Request      *whispersvcv1.JoinRequest
		Expected     *whispersvcv1.JoinResponse
		ExpectsError bool
		ExpectedCode codes.Code
		Setup        func(s service.PeerStore)
	}{
		{
			Name:         "no peer",
			ID:           1,
			Request:      &whispersvcv1.JoinRequest{},
			ExpectsError: true,
			ExpectedCode: codes.InvalidArgument,
		},
		{
			Name: "no peer address",
			ID:   1,
			Request: &whispersvcv1.JoinRequest{
				Peer: &whisperv1.Peer{
					Address: "",
				},
			},
			ExpectsError: true,
			ExpectedCode: codes.InvalidArgument,
		},
		{
			Name: "invalid peer address",
			ID:   1,
			Request: &whispersvcv1.JoinRequest{
				Peer: &whisperv1.Peer{
					Address: "not-an-ip-port-combo",
				},
			},
			ExpectsError: true,
			ExpectedCode: codes.InvalidArgument,
		},
		{
			Name: "no peer public key",
			ID:   1,
			Request: &whispersvcv1.JoinRequest{
				Peer: &whisperv1.Peer{
					Address: "0.0.0.0:8000",
				},
			},
			ExpectsError: true,
			ExpectedCode: codes.InvalidArgument,
		},
		{
			Name: "invalid peer status",
			ID:   1,
			Request: &whispersvcv1.JoinRequest{
				Peer: &whisperv1.Peer{
					Address:   "0.0.0.0:8000",
					PublicKey: []byte("public-key"),
					Status:    whisperv1.PeerStatus_PEER_STATUS_GONE,
				},
			},
			ExpectsError: true,
			ExpectedCode: codes.InvalidArgument,
		},
		{
			Name: "joined peer already exists",
			ID:   1,
			Request: &whispersvcv1.JoinRequest{
				Peer: &whisperv1.Peer{
					Id:        2,
					Address:   "0.0.0.0:8000",
					PublicKey: key.PublicKey().Bytes(),
					Status:    whisperv1.PeerStatus_PEER_STATUS_JOINING,
				},
			},
			ExpectsError: true,
			ExpectedCode: codes.AlreadyExists,
			Setup: func(s service.PeerStore) {
				require.NoError(t, s.SavePeer(t.Context(), peer.Peer{
					ID:     2,
					Status: peer.StatusJoined,
				}))
			},
		},
		{
			Name: "public key cannot be parsed",
			ID:   1,
			Request: &whispersvcv1.JoinRequest{
				Peer: &whisperv1.Peer{
					Id:        2,
					Address:   "0.0.0.0:8000",
					PublicKey: []byte("here-but-not-valid"),
					Status:    whisperv1.PeerStatus_PEER_STATUS_JOINING,
				},
			},
			ExpectsError: true,
			ExpectedCode: codes.InvalidArgument,
		},
		{
			Name: "success",
			ID:   1,
			Setup: func(s service.PeerStore) {
				require.NoError(t, s.SavePeer(t.Context(), peer.Peer{
					ID:        1,
					Address:   "0.0.0.0:8001",
					Delta:     1000,
					Status:    peer.StatusJoined,
					PublicKey: key.PublicKey(),
					Metadata:  durationpb.New(time.Hour),
				}))
			},
			Request: &whispersvcv1.JoinRequest{
				Peer: &whisperv1.Peer{
					Id:        2,
					Address:   "0.0.0.0:8000",
					PublicKey: key.PublicKey().Bytes(),
					Delta:     100,
					Status:    whisperv1.PeerStatus_PEER_STATUS_JOINING,
					Metadata:  mustAny(t, durationpb.New(time.Hour)),
				},
			},
			Expected: &whispersvcv1.JoinResponse{
				Peers: []*whisperv1.Peer{
					{
						Id:        1,
						Address:   "0.0.0.0:8001",
						Delta:     1000,
						Status:    whisperv1.PeerStatus_PEER_STATUS_JOINED,
						PublicKey: key.PublicKey().Bytes(),
						Metadata:  mustAny(t, durationpb.New(time.Hour)),
					},
					{
						Id:        2,
						Address:   "0.0.0.0:8000",
						PublicKey: key.PublicKey().Bytes(),
						Delta:     100,
						Status:    whisperv1.PeerStatus_PEER_STATUS_JOINING,
						Metadata:  mustAny(t, durationpb.New(time.Hour)),
					},
				},
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			s := store.NewInMemoryStore()
			if tc.Setup != nil {
				tc.Setup(s)
			}

			events := make(chan event.Event, 1)

			response, err := service.New(tc.ID, s, curve, logger, nil, events).Join(t.Context(), tc.Request)
			if tc.ExpectsError {
				require.Error(t, err)
				assert.Nil(t, response)
				assert.EqualValues(t, tc.ExpectedCode, status.Code(err))
				return
			}

			require.NoError(t, err)
			require.Len(t, response.GetPeers(), len(tc.Expected.GetPeers()))

			for i, expected := range tc.Expected.GetPeers() {
				actual := response.GetPeers()[i]

				assert.EqualValues(t, expected.GetId(), actual.GetId())
				assert.EqualValues(t, expected.GetAddress(), actual.GetAddress())
				assert.EqualValues(t, expected.GetMetadata(), actual.GetMetadata())
				assert.EqualValues(t, expected.GetPublicKey(), actual.GetPublicKey())
			}

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			select {
			case evt := <-events:
				assert.EqualValues(t, tc.Request.GetPeer().GetId(), evt.Peer.ID)
			case <-ctx.Done():
				assert.Fail(t, "timed out waiting for event")
			}
		})
	}
}

func testKey(t *testing.T) *ecdh.PrivateKey {
	t.Helper()

	curve := ecdh.X25519()
	key, err := curve.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return key
}

func mustAny(t *testing.T, v proto.Message) *anypb.Any {
	t.Helper()

	a, err := anypb.New(v)
	require.NoError(t, err)
	return a
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		AddSource: testing.Verbose(),
		Level:     slog.LevelDebug,
	}))
}
