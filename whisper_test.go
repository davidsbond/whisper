package whisper_test

import (
	"context"
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

	"github.com/davidsbond/whisper"
	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
	whisperv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/v1"
	"github.com/davidsbond/whisper/pkg/peer"
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
		AddSource: true,
		Level:     slog.LevelDebug,
	}))
}

func (s *WhisperTestSuite) TearDownSuite() {
	s.cancel()
}

func (s *WhisperTestSuite) TestSingleNode() {
	t := s.T()

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	node := whisper.New(1, whisper.WithLogger(s.logger))

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return node.Run(ctx)
	})

	s.Require().NoError(node.Ready(ctx))

	client, closer, err := peer.Dial(node.Address())
	s.Require().NoError(err)

	defer closer()

	t.Run("advertises self", func(t *testing.T) {
		response, err := client.Status(ctx, &whispersvcv1.StatusRequest{})
		require.NoError(t, err)
		require.NotNil(t, response)
		require.NotNil(t, response.GetSelf())
		require.Len(t, response.GetPeers(), 0)

		self := response.GetSelf()
		assert.EqualValues(t, whisperv1.PeerStatus_PEER_STATUS_JOINED, self.GetStatus())
		assert.EqualValues(t, node.ID(), self.GetId())
		assert.NotEmpty(t, self.GetPublicKey())
		assert.EqualValues(t, node.Address(), self.GetAddress())
		assert.NotZero(t, self.GetDelta())
		assert.Nil(t, self.GetMetadata())
	})
}

func (s *WhisperTestSuite) TestMultiNode() {
	t := s.T()

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	count := uint64(3)
	nodes := make([]*whisper.Node, count)

	t.Logf("testing with %d nodes", count)
	for i := range count {
		id := i + 1
		port := int(8000 + i)

		options := []whisper.Option{
			whisper.WithLogger(s.logger),
			whisper.WithPort(port),
			whisper.WithAddress(fmt.Sprintf("0.0.0.0:%d", port)),
		}

		if i > 0 {
			options = append(options, whisper.WithJoinAddress("0.0.0.0:8000"))
		}

		nodes[i] = whisper.New(id, options...)
	}

	closers := make([]context.CancelFunc, len(nodes))
	group, gCtx := errgroup.WithContext(ctx)
	for i, node := range nodes {
		if i > 0 {
			<-time.After(time.Second * 5)
		}

		t.Logf("starting node %d", node.ID())
		group.Go(func() error {
			nCtx, nCancel := context.WithCancel(gCtx)
			defer nCancel()

			closers[i] = nCancel
			return node.Run(nCtx)
		})

		s.Require().NoError(node.Ready(ctx))
	}

	t.Log("all nodes started & ready, allowing time for state to synchronise")
	<-time.After(time.Minute / 2)

	for _, node := range nodes {
		t.Run(fmt.Sprintf("node %d has full state", node.ID()), func(t *testing.T) {
			client, closer, err := peer.Dial(node.Address())
			require.NoError(t, err)
			defer closer()

			response, err := client.Status(ctx, &whispersvcv1.StatusRequest{})
			require.NoError(t, err)
			require.Len(t, response.GetPeers(), len(nodes)-1)
		})

		t.Run(fmt.Sprintf("node %d can check peers", node.ID()), func(t *testing.T) {
			client, closer, err := peer.Dial(node.Address())
			require.NoError(t, err)
			defer closer()

			for _, target := range nodes {
				if target.ID() == node.ID() {
					continue
				}

				_, err = client.Check(ctx, &whispersvcv1.CheckRequest{Id: target.ID()})
				require.NoError(t, err)
			}
		})
	}

	for i, node := range nodes {
		t.Run(fmt.Sprintf("node %d can leave gracefully", node.ID()), func(t *testing.T) {
			t.Logf("shutting down node %d", node.ID())

			closers[i]()
			<-time.After(time.Second * 10)
			for _, target := range nodes {
				if target.ID() <= node.ID() {
					continue
				}

				client, closer, err := peer.Dial(target.Address())
				require.NoError(t, err)

				response, err := client.Status(ctx, &whispersvcv1.StatusRequest{})
				require.NoError(t, err)

				for _, p := range response.GetPeers() {
					if p.GetId() != node.ID() {
						continue
					}

					assert.EqualValues(t, whisperv1.PeerStatus_PEER_STATUS_LEFT, p.GetStatus())
					break
				}

				closer()
			}
		})
	}

	s.Require().NoError(group.Wait())
}
