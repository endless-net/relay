package relaycontrol

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	pb "github.com/endless-net/relay/internal/grpcwire"
	wire "github.com/endless-net/relay/internal/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type pairControl struct {
	pb.RelayControlClient
	key   string
	calls int
	deny  bool
}

func (s *pairControl) AuthorizePeer(_ context.Context, r *pb.AuthorizePeerRequest, _ ...grpc.CallOption) (*pb.AuthorizePeerResponse, error) {
	s.calls++
	if s.deny || r.Credential.KeyId != s.key {
		return nil, status.Error(codes.PermissionDenied, "denied")
	}
	return &pb.AuthorizePeerResponse{DestinationRelayId: "b", DestinationBootId: "bb", DestinationEpoch: 1}, nil
}

func TestCacheBindsWholeCredentialAndInvalidatesDeny(t *testing.T) {
	bundle, e := wire.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	if e != nil {
		t.Fatal(e)
	}
	backend := &pairControl{key: bundle.ActiveKeyID}
	c := &Client{Control: backend, Peers: peerUpdaterFunc(func([]*pb.RelayInstance) {})}
	now := time.Now()
	if e = c.acceptCoordinatorState(nil, now.Add(30*time.Second).UnixNano(), pb.TrustBundleFromProtocol(bundle), now); e != nil {
		t.Fatal(e)
	}
	credential := wire.Credential{Algorithm: wire.CredentialAlgorithm, KeyID: bundle.ActiveKeyID, NetworkID: "n", NodeID: "a", Signature: "fixture", ExpiresAt: now.Add(time.Minute)}
	route, e := c.AuthorizePeer(t.Context(), credential, 1, "n", "b")
	if e != nil || !route.Valid() {
		t.Fatal("initial grant", e)
	}
	if _, e = c.AuthorizePeer(t.Context(), credential, 1, "n", "b"); e != nil || backend.calls != 1 {
		t.Fatal("cache miss")
	}
	tampered := credential
	tampered.KeyID = "foreign"
	if _, e = c.AuthorizePeer(t.Context(), tampered, 1, "n", "b"); e == nil || backend.calls != 2 {
		t.Fatal("credential fields not bound")
	}
	backend.deny = true
	for key, item := range c.routes {
		item.started = time.Now().Add(-6 * time.Second)
		c.routes[key] = item
	}
	if _, e = c.AuthorizePeer(t.Context(), credential, 1, "n", "b"); e == nil {
		t.Fatal("deny ignored")
	}
	if route.Valid() {
		t.Fatal("queued stale grant remained valid")
	}
}

func TestControlClockSkewRejectedBeforeStateChange(t *testing.T) {
	c := &Client{}
	now := time.Now()
	if e := c.acceptCoordinatorState(nil, now.Add(32*time.Second).UnixNano(), nil, now); e == nil {
		t.Fatal("clock skew accepted")
	}
	if c.Ready() {
		t.Fatal("untrusted clock enabled ready")
	}
}
