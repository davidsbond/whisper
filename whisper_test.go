package whisper_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/davidsbond/whisper"
)

func TestWhisper_Run(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	n1 := whisper.New(1, whisper.WithLogger(logger.With("local_id", 1)))
	n2 := whisper.New(2,
		whisper.WithLogger(logger.With("local_id", 2)),
		whisper.WithPort(8001),
		whisper.WithAddress("0.0.0.0:8001"),
		whisper.WithJoinAddress("0.0.0.0:8000"),
	)
	n3 := whisper.New(3,
		whisper.WithLogger(logger.With("local_id", 3)),
		whisper.WithPort(8002),
		whisper.WithAddress("0.0.0.0:8002"),
		whisper.WithJoinAddress("0.0.0.0:8001"),
	)

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		t.Logf("starting node 1")
		return n1.Run(ctx)
	})

	group.Go(func() error {
		<-time.After(time.Second * 5)

		t.Logf("starting node 2")
		return n2.Run(ctx)
	})

	group.Go(func() error {
		<-time.After(time.Second * 10)

		// Make this peer leave after 30 seconds.
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute/2)
		defer cancel()

		group, ctx := errgroup.WithContext(ctx)
		group.Go(func() error {
			t.Logf("starting node 3")
			return n3.Run(ctx)
		})

		group.Go(func() error {
			<-ctx.Done()
			t.Logf("stopping node 3")
			return ctx.Err()
		})

		return group.Wait()
	})

	err := group.Wait()
	if errors.Is(err, context.DeadlineExceeded) {
		return
	}

	require.NoError(t, err)
}
