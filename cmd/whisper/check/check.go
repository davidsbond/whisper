package check

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
	"github.com/davidsbond/whisper/pkg/peer"
)

func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Check a whisper node is available via another whisper node.",
		Example: `
  # Check the status of node "2" via 192.168.0.1:8000
  whisper check 2 192.168.0.1:8000`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse id: %w", err)
			}

			address := args[1]
			client, closer, err := peer.Dial(address)
			if err != nil {
				return fmt.Errorf("failed to connect to %q: %w", address, err)
			}

			defer closer()
			_, err = client.Check(cmd.Context(), &whispersvcv1.CheckRequest{Id: id})
			switch status.Code(err) {
			case codes.OK:
				fmt.Println("target peer is accessible at this address")
			case codes.FailedPrecondition:
				fmt.Println("target peer is marked as left or gone at this address")
			case codes.NotFound:
				fmt.Println("no record of target peer at this address")
			default:
				return fmt.Errorf("failed to check peer %q via %q: %w", id, address, err)
			}

			return nil
		},
	}
}
