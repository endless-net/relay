package authz

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	coordinatorrelay "github.com/endless-net/coordinator/api/relay/v1"
	pb "github.com/endless-net/relay/internal/grpcwire"
	wire "github.com/endless-net/relay/internal/wire"
	"google.golang.org/protobuf/encoding/protowire"
)

type upstreamStub struct {
	credential func(context.Context, *connect.Request[coordinatorrelay.AuthorizeCredentialRequest]) (*connect.Response[coordinatorrelay.AuthorizeCredentialResponse], error)
	pair       func(context.Context, *connect.Request[coordinatorrelay.AuthorizePeerPairRequest]) (*connect.Response[coordinatorrelay.AuthorizePeerPairResponse], error)
	trust      func(context.Context, *connect.Request[coordinatorrelay.GetTrustBundleRequest]) (*connect.Response[coordinatorrelay.GetTrustBundleResponse], error)
}

func (s upstreamStub) AuthorizeCredential(ctx context.Context, r *connect.Request[coordinatorrelay.AuthorizeCredentialRequest]) (*connect.Response[coordinatorrelay.AuthorizeCredentialResponse], error) {
	return s.credential(ctx, r)
}
func (s upstreamStub) AuthorizePeerPair(ctx context.Context, r *connect.Request[coordinatorrelay.AuthorizePeerPairRequest]) (*connect.Response[coordinatorrelay.AuthorizePeerPairResponse], error) {
	return s.pair(ctx, r)
}
func (s upstreamStub) GetTrustBundle(ctx context.Context, r *connect.Request[coordinatorrelay.GetTrustBundleRequest]) (*connect.Response[coordinatorrelay.GetTrustBundleResponse], error) {
	return s.trust(ctx, r)
}

func TestCanonicalUpstreamCarriesContextAndDeadline(t *testing.T) {
	credential := wire.Credential{Algorithm: wire.CredentialAlgorithm, KeyID: "key", NetworkID: "source", NodeID: "node", ExpiresAt: time.Now().Add(time.Minute), Signature: "fixture"}
	called := false
	a := GRPCAuthorizer{Client: upstreamStub{pair: func(ctx context.Context, r *connect.Request[coordinatorrelay.AuthorizePeerPairRequest]) (*connect.Response[coordinatorrelay.AuthorizePeerPairResponse], error) {
		called = true
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Fatal("missing bounded deadline")
		}
		if r.Msg.PeerNetworkId != "destination" || r.Msg.PeerId != "peer" || r.Msg.Credential.NetworkId != "source" {
			t.Fatal("context lost")
		}
		return connect.NewResponse(&coordinatorrelay.AuthorizePeerPairResponse{}), nil
	}}}
	if e := a.AuthorizePeer(t.Context(), credential, "destination", "peer"); e != nil || !called {
		t.Fatal(e)
	}
	called = false
	if e := a.AuthorizePeer(t.Context(), credential, " destination", "peer"); e == nil || called {
		t.Fatal("noncanonical scope reached authority")
	}
}

func TestDenyAndUnknownResponsesDoNotGrant(t *testing.T) {
	a := GRPCAuthorizer{Client: upstreamStub{credential: func(context.Context, *connect.Request[coordinatorrelay.AuthorizeCredentialRequest]) (*connect.Response[coordinatorrelay.AuthorizeCredentialResponse], error) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("deny"))
	}}}
	if !errors.Is(a.AuthorizeCredential(t.Context(), wire.Credential{}), ErrDenied) {
		t.Fatal("deny changed to allow")
	}
	a.Client = upstreamStub{credential: func(context.Context, *connect.Request[coordinatorrelay.AuthorizeCredentialRequest]) (*connect.Response[coordinatorrelay.AuthorizeCredentialResponse], error) {
		r := &coordinatorrelay.AuthorizeCredentialResponse{}
		r.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
		return connect.NewResponse(r), nil
	}}
	if a.AuthorizeCredential(t.Context(), wire.Credential{}) == nil {
		t.Fatal("unknown response accepted")
	}
	if (GRPCAuthorizer{}).AuthorizeCredential(t.Context(), wire.Credential{}) == nil {
		t.Fatal("missing authority granted")
	}
}

func TestTrustProjectionUsesRelayOwnedTypes(t *testing.T) {
	bundle, e := wire.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	if e != nil {
		t.Fatal(e)
	}
	a := GRPCAuthorizer{Client: upstreamStub{trust: func(context.Context, *connect.Request[coordinatorrelay.GetTrustBundleRequest]) (*connect.Response[coordinatorrelay.GetTrustBundleResponse], error) {
		return connect.NewResponse(&coordinatorrelay.GetTrustBundleResponse{RelayTrustBundle: pb.TrustBundleFromProtocol(bundle)}), nil
	}}}
	got, e := a.RelayTrustBundle(t.Context())
	if e != nil || got.ActiveKeyID != bundle.ActiveKeyID {
		t.Fatal("projection mismatch", e)
	}
}
