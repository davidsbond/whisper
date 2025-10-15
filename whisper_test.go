package whisper_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/davidsbond/whisper"
	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
	whisperv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/v1"
	"github.com/davidsbond/whisper/pkg/peer"
	"github.com/davidsbond/whisper/pkg/store"
)

type (
	WhisperTestSuite struct {
		suite.Suite

		ctx    context.Context
		cancel context.CancelFunc
		logger *slog.Logger
	}
)

func TestWhisper(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip()
		return
	}

	suite.Run(t, new(WhisperTestSuite))
}

func (s *WhisperTestSuite) SetupSuite() {
	t := s.T()

	s.ctx, s.cancel = signal.NotifyContext(t.Context(), os.Interrupt, os.Kill)
	s.logger = slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

func (s *WhisperTestSuite) TearDownSuite() {
	s.cancel()
}

func (s *WhisperTestSuite) TestSingleNode() {
	t := s.T()

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	nodes, _ := createNodes(t, s.logger, 1)
	runNodes(t, ctx, nodes)

	client := dialPeer(t, nodes[0].Address())
	t.Run("advertises self", func(t *testing.T) {
		response, err := client.Status(ctx, &whispersvcv1.StatusRequest{})
		require.NoError(t, err)
		require.NotNil(t, response)
		require.NotNil(t, response.GetSelf())
		require.Len(t, response.GetPeers(), 0)

		self := response.GetSelf()
		assert.EqualValues(t, whisperv1.PeerStatus_PEER_STATUS_JOINED, self.GetStatus())
		assert.EqualValues(t, nodes[0].ID(), self.GetId())
		assert.NotEmpty(t, self.GetPublicKey())
		assert.EqualValues(t, nodes[0].Address(), self.GetAddress())
		assert.NotZero(t, self.GetDelta())
		assert.Nil(t, self.GetMetadata())
	})
}

func (s *WhisperTestSuite) TestMultiNode() {
	t := s.T()

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	nodes, _ := createNodes(t, s.logger, 5)
	closers := runNodes(t, ctx, nodes)

	<-time.After(time.Second * 10)

	t.Run("state is synchronised", func(t *testing.T) {
		for _, node := range nodes {
			t.Run(fmt.Sprintf("on node %d", node.ID()), func(t *testing.T) {
				client := dialPeer(t, node.Address())

				response, err := client.Status(ctx, &whispersvcv1.StatusRequest{})
				require.NoError(t, err)
				require.Len(t, response.GetPeers(), len(nodes)-1)
			})
		}
	})

	t.Run("liveness can be checked", func(t *testing.T) {
		for _, node := range nodes {
			t.Run(fmt.Sprintf("from node %d", node.ID()), func(t *testing.T) {
				client := dialPeer(t, node.Address())

				for _, target := range nodes {
					if target.ID() == node.ID() {
						continue
					}

					t.Run(fmt.Sprintf("to node %d", target.ID()), func(t *testing.T) {
						_, err := client.Check(ctx, &whispersvcv1.CheckRequest{Id: target.ID()})
						require.NoError(t, err)
					})
				}
			})
		}
	})

	t.Run("metadata updates are propagated", func(t *testing.T) {
		for _, target := range nodes {
			expected := timestamppb.Now()

			t.Run(fmt.Sprintf("from node %d", target.ID()), func(t *testing.T) {
				require.NoError(t, target.SetMetadata(ctx, expected))

				<-time.After(time.Second * 10)
				for _, node := range nodes {
					if node.ID() == target.ID() {
						continue
					}

					t.Run(fmt.Sprintf("to node %d", node.ID()), func(t *testing.T) {
						client := dialPeer(t, node.Address())

						response, err := client.Status(ctx, &whispersvcv1.StatusRequest{})
						require.NoError(t, err)

						for _, p := range response.GetPeers() {
							if p.GetId() != target.ID() {
								continue
							}

							actual, err := p.GetMetadata().UnmarshalNew()
							require.NoError(t, err)
							assert.True(t, proto.Equal(expected, actual))
						}
					})
				}
			})
		}
	})

	t.Run("graceful shutdown", func(t *testing.T) {
		for i, node := range nodes {
			t.Run(fmt.Sprintf("on node %d", node.ID()), func(t *testing.T) {
				closers[i]()
				<-time.After(time.Second * 10)
				for _, target := range nodes {
					if target.ID() <= node.ID() {
						continue
					}

					t.Run(fmt.Sprintf("detected by node %d", target.ID()), func(t *testing.T) {
						client := dialPeer(t, target.Address())

						response, err := client.Status(ctx, &whispersvcv1.StatusRequest{})
						require.NoError(t, err)

						for _, p := range response.GetPeers() {
							if p.GetId() != node.ID() {
								continue
							}

							assert.EqualValues(t, whisperv1.PeerStatus_PEER_STATUS_LEFT, p.GetStatus())
							break
						}
					})
				}
			})
		}
	})
}

func (s *WhisperTestSuite) TestFailureDetection() {
	t := s.T()

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	curve := ecdh.X25519()
	nodes, stores := createNodes(t, s.logger, 2)
	runNodes(t, ctx, nodes)

	deadPeerKey, err := curve.GenerateKey(rand.Reader)
	require.NoError(t, err)

	deadPeer, err := peer.FromProto(&whisperv1.Peer{
		Id:        1000,
		Address:   "0.0.0.0:9000",
		PublicKey: deadPeerKey.PublicKey().Bytes(),
		Delta:     time.Now().UnixNano(),
		Status:    whisperv1.PeerStatus_PEER_STATUS_JOINED,
	}, curve)
	require.NoError(t, err)

	for _, st := range stores {
		require.NoError(t, st.SavePeer(ctx, deadPeer))
	}

	<-time.After(time.Second * 10)

	t.Run("node is marked as gone", func(t *testing.T) {
		for _, node := range nodes {
			t.Run(fmt.Sprintf("on node %d", node.ID()), func(t *testing.T) {
				client := dialPeer(t, node.Address())
				response, err := client.Status(ctx, &whispersvcv1.StatusRequest{})
				require.NoError(t, err)

				for _, p := range response.GetPeers() {
					if p.GetId() != deadPeer.ID {
						continue
					}

					assert.EqualValues(t, whisperv1.PeerStatus_PEER_STATUS_GONE, p.GetStatus())
					break
				}
			})
		}
	})
}

func dialPeer(t *testing.T, address string) whispersvcv1.WhisperServiceClient {
	t.Helper()

	client, closer, err := peer.Dial(address)
	require.NoError(t, err)

	t.Cleanup(closer)
	return client
}

func createNodes(t *testing.T, logger *slog.Logger, count int) ([]*whisper.Node, []whisper.PeerStore) {
	t.Helper()

	nodes := make([]*whisper.Node, count)
	stores := make([]whisper.PeerStore, count)

	for i := range count {
		id := i + 1
		port := 8000 + i

		st := store.NewInMemoryStore()

		options := []whisper.Option{
			whisper.WithLogger(logger.With("local", id)),
			whisper.WithPort(port),
			whisper.WithAddress(fmt.Sprintf("0.0.0.0:%d", port)),
			whisper.WithStore(st),
			whisper.WithCheckInterval(time.Second),
			whisper.WithGossipInterval(time.Second),
		}

		if i > 0 {
			options = append(options, whisper.WithJoinAddress(fmt.Sprintf("0.0.0.0:%d", port-1)))
		}

		nodes[i] = whisper.New(uint64(id), options...)
		stores[i] = st
	}

	return nodes, stores
}

func runNodes(t *testing.T, ctx context.Context, nodes []*whisper.Node) []context.CancelFunc {
	t.Helper()

	closers := make([]context.CancelFunc, len(nodes))
	group, gCtx := errgroup.WithContext(ctx)
	for i, node := range nodes {
		if i > 0 {
			<-time.After(time.Second * 5)
		}

		group.Go(func() error {
			nCtx, nCancel := context.WithCancel(gCtx)
			defer nCancel()

			closers[i] = nCancel
			return node.Run(nCtx)
		})

		require.NoError(t, node.Ready(ctx))
	}

	t.Cleanup(func() {
		assert.NoError(t, group.Wait())
	})

	return closers
}
