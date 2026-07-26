package relaycoordinator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls"
	relayv1 "github.com/unng-lab/endlessnet-relay/api/relay/v1"
	"github.com/unng-lab/endlessnet-relay/internal/authz"
	"github.com/unng-lab/endlessnet-relay/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	DefaultInstanceTTL = 15 * time.Second
	DefaultSessionTTL  = 15 * time.Second
)

type Server struct {
	relayv1.UnimplementedRelayControlServer

	Store       store.Store
	Authorizer  authz.Authorizer
	InstanceTTL time.Duration
	SessionTTL  time.Duration
}

func (s *Server) RegisterInstance(ctx context.Context, request *relayv1.RegisterInstanceRequest) (*relayv1.RegisterInstanceResponse, error) {
	if err := s.authorizeRelayIdentity(ctx, request.GetRelayId()); err != nil {
		return nil, err
	}
	if !canonicalRequired(request.GetBootId()) || !canonicalRequired(request.GetMeshAddr()) {
		return nil, status.Error(codes.InvalidArgument, "boot_id and mesh_addr are required")
	}
	peers, expires, err := s.Store.RegisterInstance(ctx, store.Instance{RelayID: request.GetRelayId(), BootID: request.GetBootId(), MeshAddr: request.GetMeshAddr()}, s.instanceTTL())
	if err != nil {
		return nil, mapStoreError(err)
	}
	bundle, err := s.Authorizer.RelayTrustBundle(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "relay trust bundle unavailable")
	}
	if err := bundle.Validate(); err != nil {
		return nil, status.Error(codes.Unavailable, "relay trust bundle unavailable")
	}
	return &relayv1.RegisterInstanceResponse{Peers: protoInstances(peers), LeaseExpiresUnixNano: expires.UnixNano(), RelayTrustBundle: relayv1.TrustBundleFromProtocol(bundle)}, nil
}

func (s *Server) HeartbeatInstance(ctx context.Context, request *relayv1.HeartbeatInstanceRequest) (*relayv1.HeartbeatInstanceResponse, error) {
	if err := s.authorizeRelayIdentity(ctx, request.GetRelayId()); err != nil {
		return nil, err
	}
	if !canonicalRequired(request.GetBootId()) {
		return nil, status.Error(codes.InvalidArgument, "boot_id is required")
	}
	peers, expires, err := s.Store.HeartbeatInstance(ctx, request.GetRelayId(), request.GetBootId(), s.instanceTTL())
	if err != nil {
		return nil, mapStoreError(err)
	}
	bundle, err := s.Authorizer.RelayTrustBundle(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "relay trust bundle unavailable")
	}
	if err := bundle.Validate(); err != nil {
		return nil, status.Error(codes.Unavailable, "relay trust bundle unavailable")
	}
	return &relayv1.HeartbeatInstanceResponse{Peers: protoInstances(peers), LeaseExpiresUnixNano: expires.UnixNano(), RelayTrustBundle: relayv1.TrustBundleFromProtocol(bundle)}, nil
}

func (s *Server) AcquireSession(ctx context.Context, request *relayv1.AcquireSessionRequest) (*relayv1.AcquireSessionResponse, error) {
	if err := s.authorizeRelayIdentity(ctx, request.GetRelayId()); err != nil {
		return nil, err
	}
	if !canonicalRequired(request.GetBootId()) {
		return nil, status.Error(codes.InvalidArgument, "boot_id is required")
	}
	credential, err := relayv1.CredentialToProtocol(request.GetCredential())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid relay credential")
	}
	if err := s.Authorizer.AuthorizeCredential(ctx, credential); err != nil {
		return nil, mapAuthorizationError(err)
	}
	lease, err := s.Store.AcquireSession(ctx, store.Session{NetworkID: credential.NetworkID, NodeID: credential.NodeID, RelayID: request.GetRelayId(), BootID: request.GetBootId()}, s.sessionTTL())
	if err != nil {
		return nil, mapStoreError(err)
	}
	return &relayv1.AcquireSessionResponse{Epoch: lease.Epoch}, nil
}

func (s *Server) RenewSession(ctx context.Context, request *relayv1.RenewSessionRequest) (*relayv1.RenewSessionResponse, error) {
	if err := s.authorizeRelayIdentity(ctx, request.GetRelayId()); err != nil {
		return nil, err
	}
	if !canonicalRequired(request.GetBootId()) || !canonicalRequired(request.GetNetworkId()) || !canonicalRequired(request.GetNodeId()) || request.GetEpoch() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "session identity and epoch are required")
	}
	credential, err := relayv1.CredentialToProtocol(request.GetCredential())
	if err != nil || credential.NetworkID != request.GetNetworkId() || credential.NodeID != request.GetNodeId() {
		return nil, status.Error(codes.InvalidArgument, "invalid relay credential")
	}
	if err := s.Authorizer.AuthorizeCredential(ctx, credential); err != nil {
		return nil, mapAuthorizationError(err)
	}
	_, err = s.Store.RenewSession(ctx, store.Session{NetworkID: request.GetNetworkId(), NodeID: request.GetNodeId(), RelayID: request.GetRelayId(), BootID: request.GetBootId(), Epoch: request.GetEpoch()}, s.sessionTTL())
	if err != nil {
		return nil, mapStoreError(err)
	}
	return &relayv1.RenewSessionResponse{}, nil
}

func (s *Server) ReleaseSession(ctx context.Context, request *relayv1.ReleaseSessionRequest) (*relayv1.ReleaseSessionResponse, error) {
	if err := s.authorizeRelayIdentity(ctx, request.GetRelayId()); err != nil {
		return nil, err
	}
	if !canonicalRequired(request.GetBootId()) || !canonicalRequired(request.GetNetworkId()) || !canonicalRequired(request.GetNodeId()) || request.GetEpoch() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "session identity and epoch are required")
	}
	err := s.Store.ReleaseSession(ctx, store.Session{NetworkID: request.GetNetworkId(), NodeID: request.GetNodeId(), RelayID: request.GetRelayId(), BootID: request.GetBootId(), Epoch: request.GetEpoch()})
	if err != nil {
		return nil, mapStoreError(err)
	}
	return &relayv1.ReleaseSessionResponse{}, nil
}

func (s *Server) AuthorizePeer(ctx context.Context, request *relayv1.AuthorizePeerRequest) (*relayv1.AuthorizePeerResponse, error) {
	if err := s.authorizeRelayIdentity(ctx, request.GetRelayId()); err != nil {
		return nil, err
	}
	if !canonicalRequired(request.GetBootId()) || !canonicalRequired(request.GetPeerId()) || request.GetSourceEpoch() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "source session and peer identity are required")
	}
	credential, err := relayv1.CredentialToProtocol(request.GetCredential())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid relay credential")
	}
	source, err := s.Store.ResolveSession(ctx, credential.NetworkID, credential.NodeID, time.Now().UTC())
	if err != nil || source.RelayID != request.GetRelayId() || source.BootID != request.GetBootId() || source.Epoch != request.GetSourceEpoch() {
		return nil, status.Error(codes.FailedPrecondition, "source relay session is fenced")
	}
	if err := s.Authorizer.AuthorizePeer(ctx, credential, request.GetPeerId()); err != nil {
		return nil, mapAuthorizationError(err)
	}
	destination, err := s.Store.ResolveSession(ctx, credential.NetworkID, request.GetPeerId(), time.Now().UTC())
	if err != nil {
		return nil, status.Error(codes.NotFound, "relay peer is not connected")
	}
	return &relayv1.AuthorizePeerResponse{DestinationRelayId: destination.RelayID, DestinationBootId: destination.BootID, DestinationEpoch: destination.Epoch}, nil
}

func (s *Server) EndpointHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/internal/relay-control/v1/endpoints" {
			http.NotFound(w, r)
			return
		}
		if !isExactHTTPPeer(r, "spiffe://endlessnet.ru/service/coordinator") {
			http.Error(w, "verified coordinator identity required", http.StatusForbidden)
			return
		}
		snapshot, err := s.Store.EndpointSnapshot(r.Context())
		if err != nil {
			http.Error(w, "relay endpoint snapshot unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := snapshot.Validate(); err != nil {
			http.Error(w, "relay endpoint snapshot unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot)
	})
}

func (s *Server) authorizeRelayIdentity(ctx context.Context, requestedRelayID string) error {
	if s.Store == nil || s.Authorizer == nil {
		return status.Error(codes.FailedPrecondition, "relay coordinator is not configured")
	}
	if !canonicalRequired(requestedRelayID) {
		return status.Error(codes.InvalidArgument, "relay_id is required")
	}
	relayID, err := relayIDFromPeer(ctx)
	if err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}
	if relayID != requestedRelayID {
		return status.Error(codes.PermissionDenied, "relay certificate identity does not match relay_id")
	}
	return nil
}

func relayIDFromPeer(ctx context.Context) (string, error) {
	peerInfo, ok := peer.FromContext(ctx)
	if !ok || peerInfo.AuthInfo == nil {
		return "", errors.New("verified relay certificate is required")
	}
	tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", errors.New("verified relay certificate is required")
	}
	peerID, err := spiffetls.PeerIDFromConnectionState(tlsInfo.State)
	if err != nil {
		return "", errors.New("verified relay certificate is required")
	}
	const prefix = "spiffe://endlessnet.ru/relay/"
	raw := peerID.String()
	if !strings.HasPrefix(raw, prefix) || !canonicalRequired(strings.TrimPrefix(raw, prefix)) {
		return "", errors.New("relay certificate URI SAN is invalid")
	}
	return strings.TrimPrefix(raw, prefix), nil
}

func protoInstances(instances []store.Instance) []*relayv1.RelayInstance {
	result := make([]*relayv1.RelayInstance, 0, len(instances))
	for _, instance := range instances {
		result = append(result, &relayv1.RelayInstance{RelayId: instance.RelayID, BootId: instance.BootID, MeshAddr: instance.MeshAddr})
	}
	return result
}

func mapStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrFenced), errors.Is(err, store.ErrSessionFenced):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, store.ErrSessionMissing):
		return status.Error(codes.NotFound, err.Error())
	default:
		return status.Error(codes.Unavailable, "relay coordinator store unavailable")
	}
}

func mapAuthorizationError(err error) error {
	if errors.Is(err, authz.ErrDenied) {
		return status.Error(codes.PermissionDenied, "relay authorization denied")
	}
	return status.Error(codes.Unavailable, "relay authorization unavailable")
}

func isExactHTTPPeer(request *http.Request, expected string) bool {
	if request == nil || request.TLS == nil {
		return false
	}
	peerID, err := spiffetls.PeerIDFromConnectionState(*request.TLS)
	if err != nil {
		return false
	}
	expectedID, err := spiffeid.FromString(expected)
	return err == nil && peerID == expectedID
}

func (s *Server) instanceTTL() time.Duration {
	if s.InstanceTTL > 0 {
		return s.InstanceTTL
	}
	return DefaultInstanceTTL
}

func (s *Server) sessionTTL() time.Duration {
	if s.SessionTTL > 0 {
		return s.SessionTTL
	}
	return DefaultSessionTTL
}

func canonicalRequired(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}
