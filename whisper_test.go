package whisper_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/davidsbond/whisper"
	"golang.org/x/sync/errgroup"
)

func TestThing(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute*2)
	defer cancel()

	n1 := whisper.New(1, whisper.WithPort(8000), whisper.WithAddress("0.0.0.0:8000"), whisper.WithLogger(testLogger(t)))
	n2 := whisper.New(2, whisper.WithPort(8001), whisper.WithAddress("0.0.0.0:8001"), whisper.WithLogger(testLogger(t)))
	n3 := whisper.New(3, whisper.WithPort(8002), whisper.WithAddress("0.0.0.0:8002"), whisper.WithLogger(testLogger(t)))

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return n1.Start(ctx)
	})
	group.Go(func() error {
		for event := range n1.Events() {
			t.Logf("1: got event (%v) for peer %d", event.Type, event.Peer.ID)
		}

		return nil
	})

	group.Go(func() error {
		<-time.After(time.Second * 5)
		return n2.Join(ctx, "0.0.0.0:8000")
	})
	group.Go(func() error {
		for event := range n2.Events() {
			t.Logf("2: got event (%v) for peer %d", event.Type, event.Peer.ID)
		}

		return nil
	})

	group.Go(func() error {
		<-time.After(time.Second * 10)
		return n3.Join(ctx, "0.0.0.0:8000")
	})
	group.Go(func() error {
		for event := range n3.Events() {
			t.Logf("3: got event (%v) for peer %d", event.Type, event.Peer.ID)
		}

		return nil
	})

	<-time.After(time.Second * 30)
	n1.SetMetadata(t.Context(), []byte("hello world"))
	n2.SetMetadata(t.Context(), []byte("hello world"))
	n3.SetMetadata(t.Context(), []byte("hello world"))

	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelDebug,
	}))
}
