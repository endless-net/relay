package relaycontrol

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	relayapi "github.com/endless-net/relay/api/relay/v1"
	relayv1 "github.com/endless-net/relay/internal/grpcwire"
	protocolv1 "github.com/endless-net/relay/internal/wire"
	"google.golang.org/grpc"
)

type auditHungControl struct {
	relayapi.RelayControlClient

	bundle  *relayapi.SigningTrustBundle
	entered chan struct{}
}

func (a *auditHungControl) RegisterInstance(context.Context, *relayapi.RegisterInstanceRequest, ...grpc.CallOption) (*relayapi.RegisterInstanceResponse, error) {
	return &relayapi.RegisterInstanceResponse{LeaseExpiresUnixNano: time.Now().Add(40 * time.Millisecond).UnixNano(), RelayTrustBundle: a.bundle}, nil
}
func (a *auditHungControl) HeartbeatInstance(ctx context.Context, _ *relayapi.HeartbeatInstanceRequest, _ ...grpc.CallOption) (*relayapi.HeartbeatInstanceResponse, error) {
	close(a.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestRegressionHungHeartbeatMustFence(t *testing.T) {
	bundle, err := protocolv1.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	stub := &auditHungControl{bundle: relayv1.TrustBundleFromProtocol(bundle), entered: make(chan struct{})}
	c := &Client{RelayID: "r", BootID: "b", MeshAddr: "r:9444", Control: stub, Peers: peerUpdaterFunc(func([]*relayapi.RelayInstance) {}), InstanceTTL: 40 * time.Millisecond, HeartbeatInterval: time.Millisecond, FencingGrace: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case <-stub.entered:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not start")
	}
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Error("Run blocked in heartbeat past instance lease plus grace")
	}
	cancel()
}
