package relaycontrol

import (
	"context"
	"encoding/base64"
	relayv1 "github.com/endless-net/relay/api/relay/v1"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"google.golang.org/grpc"
	"testing"
	"time"
)

type auditHungControl struct {
	relayv1.RelayControlClient
	bundle  *relayv1.SigningTrustBundle
	entered chan struct{}
}

func (a *auditHungControl) RegisterInstance(context.Context, *relayv1.RegisterInstanceRequest, ...grpc.CallOption) (*relayv1.RegisterInstanceResponse, error) {
	return &relayv1.RegisterInstanceResponse{LeaseExpiresUnixNano: time.Now().Add(40 * time.Millisecond).UnixNano(), RelayTrustBundle: a.bundle}, nil
}
func (a *auditHungControl) HeartbeatInstance(ctx context.Context, _ *relayv1.HeartbeatInstanceRequest, _ ...grpc.CallOption) (*relayv1.HeartbeatInstanceResponse, error) {
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
	c := &Client{RelayID: "r", BootID: "b", MeshAddr: "r:9444", Control: stub, Peers: peerUpdaterFunc(func([]*relayv1.RelayInstance) {}), HeartbeatInterval: time.Millisecond, FencingGrace: time.Millisecond}
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
