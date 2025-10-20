package start

import (
	"crypto/ecdh"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/davidsbond/whisper"
	"github.com/davidsbond/whisper/internal/tlsutil"
)

func Command() *cobra.Command {
	var (
		port        int
		address     string
		joinAddress string
		keyFile     string
		tlsCert     string
		tlsKey      string
		tlsCA       string
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

			curve := ecdh.X25519()

			var key *ecdh.PrivateKey
			if keyFile != "" {
				keyData, err := os.ReadFile(keyFile)
				if err != nil {
					return fmt.Errorf("failed to read key file %q: %w", keyFile, err)
				}

				key, err = curve.NewPrivateKey(keyData)
				if err != nil {
					return fmt.Errorf("failed to parse key file %q: %w", keyFile, err)
				}
			}

			var tlsConfig *tls.Config
			if tlsCert != "" && tlsKey != "" && tlsCA != "" {
				tlsConfig, err = tlsutil.LoadConfig(tlsCert, tlsKey, tlsCA)
				if err != nil {
					return fmt.Errorf("failed to load tls config: %w", err)
				}
			}

			node := whisper.New(id,
				whisper.WithPort(port),
				whisper.WithAddress(address),
				whisper.WithJoinAddress(joinAddress),
				whisper.WithLogger(slog.Default().With("local_id", id)),
				whisper.WithCurve(curve),
				whisper.WithKey(key),
				whisper.WithTLS(tlsConfig),
			)

			return node.Run(cmd.Context())
		},
	}

	flags := cmd.Flags()
	flags.IntVar(&port, "port", 8000, "port to use for TCP/UDP")
	flags.StringVar(&address, "address", "0.0.0.0:8000", "address to advertise to other peers")
	flags.StringVar(&joinAddress, "join", "", "address to use for joining the gossip network")
	flags.StringVar(&keyFile, "key", "", "path to private ECDH key file for gossip over UDP")
	flags.StringVar(&tlsCert, "tls-cert", "", "path to TLS certificate")
	flags.StringVar(&tlsKey, "tls-key", "", "path to TLS key")
	flags.StringVar(&tlsCA, "tls-ca", "", "path to TLS CA")

	return cmd
}
