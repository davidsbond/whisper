package service

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
	whisperv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/v1"
	"github.com/davidsbond/whisper/internal/store"
)

type (
	Service struct {
		whispersvcv1.UnimplementedWhisperServiceServer

		peers PeerStore
		curve ecdh.Curve
	}

	PeerStore interface {
		FindPeer(ctx context.Context, id uint64) (*whisperv1.Peer, error)
		SavePeer(ctx context.Context, peer *whisperv1.Peer) error
		ListPeers(ctx context.Context) ([]*whisperv1.Peer, error)
	}
)

func New(peers PeerStore, curve ecdh.Curve) *Service {
	return &Service{
		peers: peers,
		curve: curve,
	}
}

func (svc *Service) Register(s grpc.ServiceRegistrar) {
	whispersvcv1.RegisterWhisperServiceServer(s, svc)
}

func (svc *Service) Join(ctx context.Context, r *whispersvcv1.JoinRequest) (*whispersvcv1.JoinResponse, error) {
	if err := svc.validateJoinRequest(r); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	existing, err := svc.peers.FindPeer(ctx, r.GetPeer().GetId())
	switch {
	case errors.Is(err, store.ErrPeerNotFound):
		break
	case err != nil:
		return nil, status.Errorf(codes.Internal, "failed to lookup peer %q: %v", r.GetPeer().GetId(), err)
	case existing.Status != whisperv1.PeerStatus_PEER_STATUS_LEFT && existing.Status != whisperv1.PeerStatus_PEER_STATUS_GONE:
		// Do not allow a joining peer to take the identifier of a peer that hasn't left or gone missing. Otherwise, a peer
		// could duplicate an existing identifier and take over.
		return nil, status.Errorf(codes.AlreadyExists, "peer %q is already an active peer in the network", r.GetPeer().GetId())
	}

	peer := r.GetPeer()
	peer.Status = whisperv1.PeerStatus_PEER_STATUS_JOINED
	peer.Delta = time.Now().Unix()

	if err = svc.peers.SavePeer(ctx, peer); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save peer: %v", err)
	}

	peers, err := svc.peers.ListPeers(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list peers: %v", err)
	}

	return &whispersvcv1.JoinResponse{Peers: peers}, nil
}

func (svc *Service) Leave(ctx context.Context, r *whispersvcv1.LeaveRequest) (*whispersvcv1.LeaveResponse, error) {
	peer, err := svc.peers.FindPeer(ctx, r.GetId())
	switch {
	case errors.Is(err, store.ErrPeerNotFound):
		return nil, status.Errorf(codes.NotFound, "peer %q not found", r.GetId())
	case err != nil:
		return nil, status.Errorf(codes.Internal, "failed to find peer %q: %v", r.GetId(), err)
	}

	peer.Status = whisperv1.PeerStatus_PEER_STATUS_LEFT
	peer.Delta = time.Now().Unix()

	if err = svc.peers.SavePeer(ctx, peer); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save peer %q: %v", r.GetId(), err)
	}

	return &whispersvcv1.LeaveResponse{}, nil
}

func (svc *Service) validateJoinRequest(r *whispersvcv1.JoinRequest) error {
	if r.GetPeer() == nil {
		return errors.New("no peer specified")
	}

	peer := r.GetPeer()
	if peer.GetAddress() == "" {
		return errors.New("no peer address specified")
	}

	if _, err := netip.ParseAddrPort(peer.GetAddress()); err != nil {
		return fmt.Errorf("invalid peer address %q: %w", peer.GetAddress(), err)
	}

	if len(peer.GetPublicKey()) == 0 {
		return errors.New("no peer public key specified")
	}

	if _, err := svc.curve.NewPublicKey(peer.GetPublicKey()); err != nil {
		return fmt.Errorf("invalid peer public key: %w", err)
	}

	if peer.GetStatus() != whisperv1.PeerStatus_PEER_STATUS_JOINING {
		return fmt.Errorf("invalid peer status: %v", peer.GetStatus())
	}

	return nil
}
