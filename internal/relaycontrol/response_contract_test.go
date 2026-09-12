package relaycontrol

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/endless-net/relay/internal/relay"
	protocol "github.com/endless-net/relay/relayapi/v1"
	rpc "github.com/endless-net/relay/relayapi/v1/relayapigrpc"
	"google.golang.org/grpc"
)

type responseControl struct {
	rpc.RelayControlClient
	acquire *rpc.AcquireSessionResponse
	route   *rpc.AuthorizePeerResponse
	renew   *rpc.RenewSessionResponse
	release *rpc.ReleaseSessionResponse
}

func (s responseControl) AcquireSession(context.Context, *rpc.AcquireSessionRequest, ...grpc.CallOption) (*rpc.AcquireSessionResponse, error) {
	return s.acquire, nil
}
func (s responseControl) AuthorizePeer(context.Context, *rpc.AuthorizePeerRequest, ...grpc.CallOption) (*rpc.AuthorizePeerResponse, error) {
	return s.route, nil
}
func (s responseControl) RenewSession(context.Context, *rpc.RenewSessionRequest, ...grpc.CallOption) (*rpc.RenewSessionResponse, error) {
	return s.renew, nil
}
func (s responseControl) ReleaseSession(context.Context, *rpc.ReleaseSessionRequest, ...grpc.CallOption) (*rpc.ReleaseSessionResponse, error) {
	return s.release, nil
}

func TestControlRejectsNilAndInvalidSessionResponses(t *testing.T) {
	c := &Client{RelayID: "relay", BootID: "boot", Control: responseControl{}}
	credential := protocol.Credential{NetworkID: "n", NodeID: "a"}
	lease := relay.SessionLease{NetworkID: "n", NodeID: "a", Epoch: 1, Credential: credential}
	if _, err := c.AcquireSession(t.Context(), credential); err == nil {
		t.Fatal("nil acquire accepted")
	}
	if _, err := c.AuthorizePeer(t.Context(), credential, 1, "n", "b"); err == nil {
		t.Fatal("nil route accepted")
	}
	if err := c.RenewSession(t.Context(), lease); err == nil {
		t.Fatal("nil renewal accepted")
	}
	if err := c.ReleaseSession(t.Context(), lease); err == nil {
		t.Fatal("nil release accepted")
	}
	for _, epoch := range []int64{-1, 0, 1} {
		c.Control = responseControl{acquire: &rpc.AcquireSessionResponse{Epoch: epoch}}
		got, err := c.AcquireSession(t.Context(), credential)
		if (err == nil) != (epoch > 0) {
			t.Fatalf("epoch %d: %v", epoch, err)
		}
		if err == nil && (got.Epoch != epoch || got.NetworkID != "n" || got.NodeID != "a" || got.RelayID != "relay" || got.BootID != "boot") {
			t.Fatal("lease identity changed")
		}
	}
	for _, route := range []*rpc.AuthorizePeerResponse{
		{}, {DestinationRelayId: "r", DestinationBootId: "b", DestinationEpoch: 0},
		{DestinationRelayId: " r", DestinationBootId: "b", DestinationEpoch: 1},
		{DestinationRelayId: "r", DestinationBootId: " b", DestinationEpoch: 1},
	} {
		c.Control = responseControl{route: route}
		if _, err := c.AuthorizePeer(t.Context(), credential, 1, "n", "b"); err == nil {
			t.Fatal("invalid route accepted")
		}
	}
	c.Control = responseControl{route: &rpc.AuthorizePeerResponse{DestinationRelayId: "r", DestinationBootId: "b", DestinationEpoch: 1}, renew: &rpc.RenewSessionResponse{}, release: &rpc.ReleaseSessionResponse{}}
	if _, err := c.AuthorizePeer(t.Context(), credential, 1, "n", "b"); err != nil {
		t.Fatal(err)
	}
	if err := c.RenewSession(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	if err := c.ReleaseSession(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorStateRejectedAtomicallyAndTrustCopied(t *testing.T) {
	bundle, err := protocol.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	bundle.Keys[0].NotAfter = &until
	updates := 0
	c := &Client{Peers: peerUpdaterFunc(func([]*rpc.RelayInstance) { updates++ })}
	expiry := time.Now().Add(time.Minute)
	if err := c.acceptCoordinatorState(nil, expiry.UnixNano(), rpc.TrustBundleFromProtocol(bundle)); err != nil {
		t.Fatal(err)
	}
	for _, peers := range [][]*rpc.RelayInstance{{nil}, {{RelayId: "r", BootId: "b"}}, {{RelayId: "r", BootId: " b", MeshAddr: "r:443"}}} {
		if err := c.acceptCoordinatorState(peers, expiry.Add(time.Minute).UnixNano(), rpc.TrustBundleFromProtocol(bundle)); err == nil {
			t.Fatal("invalid peer accepted")
		}
		if updates != 1 || !c.currentLeaseExpiry().Equal(expiry) {
			t.Fatal("rejected state changed lease or peers")
		}
	}
	got, err := c.TrustBundle()
	if err != nil {
		t.Fatal(err)
	}
	got.Keys[0].PublicKey = "mutated"
	*got.Keys[0].NotAfter = time.Time{}
	again, err := c.TrustBundle()
	if err != nil || again.Keys[0].NotAfter.IsZero() {
		t.Fatal("trust output aliases client state", err)
	}
}
