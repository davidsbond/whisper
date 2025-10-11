package whisper_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sync/errgroup"

	"github.com/davidsbond/whisper"
)

func TestWhisper_Run(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelDebug,
	}))

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return whisper.Run(ctx,
			whisper.WithID(1),
			whisper.WithLogger(logger),
		)
	})

	group.Go(func() error {
		<-time.After(time.Second * 5)
		return whisper.Run(ctx,
			whisper.WithID(2),
			whisper.WithLogger(logger),
			whisper.WithPort(8001),
			whisper.WithAddress("0.0.0.0:8001"),
			whisper.WithJoinAddress("0.0.0.0:8000"),
		)
	})

	group.Go(func() error {
		<-time.After(time.Second * 10)

		// Make this peer leave after 1 minute.
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute/2)
		defer cancel()

		err := whisper.Run(ctx,
			whisper.WithID(3),
			whisper.WithLogger(logger),
			whisper.WithPort(8002),
			whisper.WithAddress("0.0.0.0:8002"),
			whisper.WithJoinAddress("0.0.0.0:8001"),
		)

		if errors.Is(err, context.DeadlineExceeded) {
			return nil
		}

		return err
	})

	assert.NoError(t, group.Wait())
}
