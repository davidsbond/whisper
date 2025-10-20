package whisper_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/signal"
	"strconv"
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

func (s *WhisperTestSuite) SetupTest() {
	t := s.T()

	s.ctx, s.cancel = signal.NotifyContext(t.Context(), os.Interrupt, os.Kill)
	s.logger = slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})).With("test", t.Name())
}

func (s *WhisperTestSuite) TeardownTest() {
	s.cancel()
}

func (s *WhisperTestSuite) TestSingleNode() {
	t := s.T()

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	ca, key := testCA(t)
	nodes, _ := createNodes(t, s.logger, ca, key, 1)
	runNodes(t, ctx, nodes)
	testTLS := testTLSConfig(t, ca, key, "test")

	client := dialPeer(t, testTLS, nodes[0].Address())

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

	ca, key := testCA(t)
	nodes, _ := createNodes(t, s.logger, ca, key, 5)
	closers := runNodes(t, ctx, nodes)
	testTLS := testTLSConfig(t, ca, key, "test")

	<-time.After(time.Second * 10)

	t.Run("state is synchronised", func(t *testing.T) {
		for _, node := range nodes {
			t.Run(fmt.Sprintf("on node %d", node.ID()), func(t *testing.T) {
				peers, err := node.Peers(ctx)
				require.NoError(t, err)
				require.Len(t, peers, len(nodes))
			})
		}
	})

	t.Run("liveness can be checked", func(t *testing.T) {
		for _, node := range nodes {
			t.Run(fmt.Sprintf("from node %d", node.ID()), func(t *testing.T) {
				client := dialPeer(t, testTLS, node.Address())

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
						peers, err := node.Peers(ctx)
						require.NoError(t, err)

						for _, p := range peers {
							if p.ID != target.ID() {
								continue
							}

							assert.True(t, proto.Equal(expected, p.Metadata))
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
						peers, err := target.Peers(ctx)
						require.NoError(t, err)

						for _, p := range peers {
							if p.ID != node.ID() {
								continue
							}

							assert.EqualValues(t, peer.StatusLeft, p.Status)
							break
						}

						<-time.After(time.Second * 10)
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
	ca, key := testCA(t)
	nodes, stores := createNodes(t, s.logger, ca, key, 2)
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

	<-time.After(time.Minute / 2)

	t.Run("node is marked as gone", func(t *testing.T) {
		for _, node := range nodes {
			t.Run(fmt.Sprintf("on node %d", node.ID()), func(t *testing.T) {
				peers, err := node.Peers(ctx)
				require.NoError(t, err)

				for _, p := range peers {
					if p.ID != deadPeer.ID {
						continue
					}

					assert.EqualValues(t, peer.StatusGone, p.Status)
					break
				}
			})
		}
	})
}

func dialPeer(t *testing.T, cfg *tls.Config, address string) whispersvcv1.WhisperServiceClient {
	t.Helper()

	client, closer, err := peer.Dial(address, cfg)
	require.NoError(t, err)

	t.Cleanup(closer)
	return client
}

func createNodes(t *testing.T, logger *slog.Logger, caCert *x509.Certificate, caKey *rsa.PrivateKey, count int) ([]*whisper.Node, []whisper.PeerStore) {
	t.Helper()

	nodes := make([]*whisper.Node, count)
	stores := make([]whisper.PeerStore, count)

	for i := range count {
		id := i + 1
		port := 8000 + i
		tlsConfig := testTLSConfig(t, caCert, caKey, strconv.Itoa(i))
		st := store.NewInMemoryStore()

		options := []whisper.Option{
			whisper.WithLogger(logger.With("local", id)),
			whisper.WithPort(port),
			whisper.WithAddress(fmt.Sprintf("0.0.0.0:%d", port)),
			whisper.WithStore(st),
			whisper.WithCheckInterval(time.Second * 10),
			whisper.WithGossipInterval(time.Second),
			whisper.WithTLS(tlsConfig),
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
			<-time.After(time.Second * 2)
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

func testCA(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	caCert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "Whisper Root CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, caCert, caCert, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	caCert, err = x509.ParseCertificate(certDER)
	require.NoError(t, err)

	return caCert, caKey
}

func testCert(t *testing.T, caCert *x509.Certificate, caKey *rsa.PrivateKey, name string) (tls.Certificate, *x509.Certificate) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	certTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName: name,
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
		DNSNames:              []string{name, "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("0.0.0.0")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, certTmpl, caCert, &key.PublicKey, caKey)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(certDER)
	require.NoError(t, err)

	tlsCert := tls.Certificate{
		Certificate: [][]byte{certDER, caCert.Raw},
		PrivateKey:  key,
	}

	return tlsCert, cert
}

func testTLSConfig(t *testing.T, caCert *x509.Certificate, caKey *rsa.PrivateKey, name string) *tls.Config {
	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)

	cert, _ := testCert(t, caCert, caKey, name)

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		RootCAs:      caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}

	return cfg
}
