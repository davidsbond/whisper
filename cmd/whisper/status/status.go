package status

import (
	"fmt"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
	"github.com/davidsbond/whisper/pkg/peer"
)

func Command() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Query the status of a whisper node.",
		Example: `
  # Get the status of a whisper node.
  whisper status 192.168.0.1:8000`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			address := args[0]
			client, closer, err := peer.Dial(address)
			if err != nil {
				return fmt.Errorf("failed to connect to %q: %w", address, err)
			}

			defer closer()
			status, err := client.Status(cmd.Context(), &whispersvcv1.StatusRequest{})
			if err != nil {
				return fmt.Errorf("failed to get node status: %w", err)
			}

			output, err := protojson.Marshal(status)
			if err != nil {
				return fmt.Errorf("failed to marshal status: %w", err)
			}

			fmt.Println(string(output))
			return nil
		},
	}
}
