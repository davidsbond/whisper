package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/davidsbond/whisper/cmd/whisper/check"
	"github.com/davidsbond/whisper/cmd/whisper/start"
	"github.com/davidsbond/whisper/cmd/whisper/status"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	cmd := &cobra.Command{
		Use:   "whisper",
		Short: "A gRPC-based gossip protocol.",
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}

	cmd.AddCommand(
		check.Command(),
		status.Command(),
		start.Command(),
	)

	if info, ok := debug.ReadBuildInfo(); ok {
		cmd.Version = info.Main.Version
	}

	if err := cmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
