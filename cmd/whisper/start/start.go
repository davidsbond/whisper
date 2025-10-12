package start

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/davidsbond/whisper"
)

func Command() *cobra.Command {
	var (
		port        int
		address     string
		joinAddress string
		debug       bool
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start a whisper node.",
		Example: `
  # Standalone node
  whisper start 1

  # Join an existing network
  whisper start 2 --address 192.168.0.1:8000 --join 192.168.0.2:8000`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse id: %w", err)
			}

			logger := slog.Default()
			if debug {
				logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
					Level: slog.LevelDebug,
				}))
			}

			return whisper.Run(cmd.Context(),
				whisper.WithID(id),
				whisper.WithPort(port),
				whisper.WithAddress(address),
				whisper.WithJoinAddress(joinAddress),
				whisper.WithLogger(logger.With("local_id", id)),
			)
		},
	}

	flags := cmd.Flags()
	flags.IntVarP(&port, "port", "p", 8000, "port to use for TCP/UDP")
	flags.StringVarP(&address, "address", "a", "0.0.0.0:8000", "address to advertise to other peers")
	flags.StringVarP(&joinAddress, "join", "j", "", "address to use for joining the gossip network")
	flags.BoolVar(&debug, "debug", false, "enable debug logging")

	return cmd
}
