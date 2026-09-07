package authz

import (
	"context"
	"errors"
	"time"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCAuthorizer consumes only the published protobuf upstream service. The
// caller supplies a connection authenticated with the exact upstream SPIFFE ID.
type GRPCAuthorizer struct {
	Client relayv1.RelayUpstreamServiceClient
}

func (a GRPCAuthorizer) AuthorizeCredential(ctx context.Context, credential protocolv1.Credential) error {
	if a.Client == nil {
		return errors.New("upstream gRPC client is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := a.Client.AuthorizeCredential(ctx, &relayv1.AuthorizeCredentialRequest{Credential: relayv1.CredentialFromProtocol(credential)})
	if err != nil {
		return authorizationError(err)
	}
	if response == nil {
		return errors.New("upstream authorization response is required")
	}
	return relayv1.RejectUnknownFields(response)
}

func (a GRPCAuthorizer) AuthorizePeer(ctx context.Context, credential protocolv1.Credential, peerNetworkID, peerID string) error {
	if !validPeerScope(peerNetworkID, peerID) {
		return errors.New("canonical destination network and peer are required")
	}
	if a.Client == nil {
		return errors.New("upstream gRPC client is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := a.Client.AuthorizePeerPair(ctx, &relayv1.AuthorizePeerPairRequest{Credential: relayv1.CredentialFromProtocol(credential), PeerId: peerID, PeerNetworkId: peerNetworkID})
	if err != nil {
		return authorizationError(err)
	}
	if response == nil {
		return errors.New("upstream authorization response is required")
	}
	return relayv1.RejectUnknownFields(response)
}

func (a GRPCAuthorizer) RelayTrustBundle(ctx context.Context) (protocolv1.SigningTrustBundle, error) {
	if a.Client == nil {
		return protocolv1.SigningTrustBundle{}, errors.New("upstream gRPC client is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := a.Client.GetTrustBundle(ctx, &relayv1.GetTrustBundleRequest{})
	if err != nil {
		return protocolv1.SigningTrustBundle{}, err
	}
	if response == nil {
		return protocolv1.SigningTrustBundle{}, errors.New("upstream trust response is required")
	}
	if err := relayv1.RejectUnknownFields(response); err != nil {
		return protocolv1.SigningTrustBundle{}, err
	}
	return relayv1.TrustBundleToProtocol(response.GetRelayTrustBundle())
}

func authorizationError(err error) error {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated:
		return ErrDenied
	default:
		return err
	}
}
