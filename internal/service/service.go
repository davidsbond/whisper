// Package service provides the gRPC service implementation for a whisper peer.
package service

import (
	"context"
	"crypto/ecdh"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	whispersvcv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/service/v1"
	whisperv1 "github.com/davidsbond/whisper/internal/generated/proto/whisper/v1"
	"github.com/davidsbond/whisper/pkg/peer"
	"github.com/davidsbond/whisper/pkg/store"
)

type (
	// The Service type is a whispersvcv1.WhisperServiceServer implementation that is used for TCP-based communications
	// between peers in the gossip network. Primarily this is used for peers joining and leaving the network.
	Service struct {
		whispersvcv1.UnimplementedWhisperServiceServer

		id     uint64
		peers  PeerStore
		curve  ecdh.Curve
		logger *slog.Logger
		tls    *tls.Config
	}

	// The PeerStore interface describes types that persist the current state of all peers within the gossip network.
	PeerStore interface {
		// FindPeer should return a peer.Peer whose identifier matches the one provided. It should return
		// store.ErrPeerNotFound if a matching peer does not exist.
		FindPeer(ctx context.Context, id uint64) (peer.Peer, error)
		// SavePeer should persist the provided peer.Peer.
		SavePeer(ctx context.Context, peer peer.Peer) error
		// ListPeers should return all peers in the store.
		ListPeers(ctx context.Context) ([]peer.Peer, error)
	}
)

// New returns a new instance of the Service type that will persist peer data using the provided PeerStore implementation.
func New(id uint64, peers PeerStore, curve ecdh.Curve, logger *slog.Logger, tls *tls.Config) *Service {
	return &Service{
		id:     id,
		peers:  peers,
		curve:  curve,
		logger: logger,
		tls:    tls,
	}
}

// Register the gRPC service implementation.
func (svc *Service) Register(s grpc.ServiceRegistrar) {
	whispersvcv1.RegisterWhisperServiceServer(s, svc)
}

// Join handles an inbound gRPC request from a peer wishing to join the gossip network. Validation is performed on the
// request to ensure all fields are valid and checks are made to ensure that if a matching peer identifier is already
// present is must be a peer that has already left or "gone missing".
//
// On success, the joining peer is provided the current state of the network as known to this peer.
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
	case existing.Status != peer.StatusLeft && existing.Status != peer.StatusGone:
		// Do not allow a joining peer to take the identifier of a peer that hasn't left or gone missing. Otherwise, a peer
		// could duplicate an existing identifier and take over.
		return nil, status.Errorf(codes.AlreadyExists, "peer %q is already an active peer in the network", r.GetPeer().GetId())
	}

	p, err := peer.FromProto(r.GetPeer(), svc.curve)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to parse peer %q: %v", r.GetPeer().GetId(), err)
	}

	if err = svc.peers.SavePeer(ctx, p); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save peer: %v", err)
	}

	svc.logger.With("peer", r.GetPeer().GetId()).DebugContext(ctx, "received join request from peer")

	peers, err := svc.peers.ListPeers(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list peers: %v", err)
	}

	response := &whispersvcv1.JoinResponse{Peers: make([]*whisperv1.Peer, len(peers))}
	for i, p := range peers {
		response.Peers[i] = &whisperv1.Peer{
			Id:        p.ID,
			Address:   p.Address,
			PublicKey: p.PublicKey.Bytes(),
			Delta:     p.Delta,
			Status:    whisperv1.PeerStatus(p.Status),
		}

		if p.Metadata != nil {
			response.Peers[i].Metadata, err = anypb.New(p.Metadata)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to marshal metadata for peer %d: %v", p.ID, err)
			}
		}
	}

	return response, nil
}

func (svc *Service) validateJoinRequest(r *whispersvcv1.JoinRequest) error {
	if r.GetPeer() == nil {
		return errors.New("no peer specified")
	}

	p := r.GetPeer()
	if p.GetAddress() == "" {
		return errors.New("no peer address specified")
	}

	if _, err := netip.ParseAddrPort(p.GetAddress()); err != nil {
		return fmt.Errorf("invalid peer address %q: %w", p.GetAddress(), err)
	}

	if len(p.GetPublicKey()) == 0 {
		return errors.New("no peer public key specified")
	}

	if p.GetStatus() != whisperv1.PeerStatus_PEER_STATUS_JOINING {
		return fmt.Errorf("invalid peer status: %v", p.GetStatus())
	}

	return nil
}

// Leave handles an inbound gRPC request from a peer wishing to leave the gossip network. This peer must have a record
// of the calling peer for the call to succeed. On success, this peer updates its local state for the calling peer
// with a "left" status which will be propagated out to the rest of the network.
//
// On success, the calling peer can gracefully shut down.
func (svc *Service) Leave(ctx context.Context, r *whispersvcv1.LeaveRequest) (*whispersvcv1.LeaveResponse, error) {
	p, err := svc.peers.FindPeer(ctx, r.GetId())
	switch {
	case errors.Is(err, store.ErrPeerNotFound):
		return nil, status.Errorf(codes.NotFound, "peer %q not found", r.GetId())
	case err != nil:
		return nil, status.Errorf(codes.Internal, "failed to find peer %q: %v", r.GetId(), err)
	}

	p.Status = peer.StatusLeft
	p.Delta = time.Now().UnixNano()

	if err = svc.peers.SavePeer(ctx, p); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save peer %q: %v", r.GetId(), err)
	}

	svc.logger.With("peer", r.GetId()).DebugContext(ctx, "peer is leaving gossip network")

	return &whispersvcv1.LeaveResponse{}, nil
}

// Status handles an inbound gRPC request querying this peer's current view of the gossip network.
func (svc *Service) Status(ctx context.Context, _ *whispersvcv1.StatusRequest) (*whispersvcv1.StatusResponse, error) {
	peers, err := svc.peers.ListPeers(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list peers: %v", err)
	}

	response := &whispersvcv1.StatusResponse{
		Peers: make([]*whisperv1.Peer, 0, len(peers)-1),
	}

	for _, p := range peers {
		record := &whisperv1.Peer{
			Id:        p.ID,
			Address:   p.Address,
			PublicKey: p.PublicKey.Bytes(),
			Delta:     p.Delta,
			Status:    whisperv1.PeerStatus(p.Status),
		}

		if p.Metadata != nil {
			record.Metadata, err = anypb.New(p.Metadata)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to marshal metadata for peer %d: %v", p.ID, err)
			}
		}

		if p.ID == svc.id {
			response.Self = record
			continue
		}

		response.Peers = append(response.Peers, record)
	}

	return response, nil
}

// Check is called by a peer when it detects a possible failure of another peer within the gossip network. This call
// will attempt to reach out to the specified peer and report if it is accessible. This is used to verify a peer is
// not available from more than one peer.
//
// Verification is performed by calling the Status endpoint of the desired peer.
func (svc *Service) Check(ctx context.Context, r *whispersvcv1.CheckRequest) (*whispersvcv1.CheckResponse, error) {
	target, err := svc.peers.FindPeer(ctx, r.GetId())
	switch {
	case errors.Is(err, store.ErrPeerNotFound):
		return nil, status.Errorf(codes.NotFound, "peer %q not found", r.GetId())
	case err != nil:
		return nil, status.Errorf(codes.FailedPrecondition, "failed to lookup peer %q: %v", r.GetId(), err)
	}

	if target.Status == peer.StatusLeft || target.Status == peer.StatusGone {
		// If the specified peer has left, or we already know it has failed, it could be the state on the caller has
		// not yet been updated to reflect this, so we tell the caller that no further action is required.
		return nil, status.Errorf(codes.FailedPrecondition, "peer %q is in status: %v", r.GetId(), target.Status)
	}

	client, closer, err := peer.Dial(target.Address, svc.tls)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to dial peer %q: %v", target.ID, err)
	}

	defer closer()
	if _, err = client.Status(ctx, &whispersvcv1.StatusRequest{}); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to dial peer %q: %v", target.ID, err)
	}

	return &whispersvcv1.CheckResponse{}, nil
}
