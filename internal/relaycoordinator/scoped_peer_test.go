package relaycoordinator

import (
	"context"
	"testing"
	"time"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	"github.com/endless-net/relay/internal/authz"
	"github.com/endless-net/relay/internal/store"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type scopedAuthorizer struct{ allowAuthorizer }

func (scopedAuthorizer) AuthorizePeer(_ context.Context, source protocolv1.Credential, network, node string) error {
	if source.NetworkID != "source-network" || source.NodeID != "source" || network != "destination-network" || node != "same-node" {
		return authz.ErrDenied
	}
	return nil
}

func TestAuthorizePeerResolvesExplicitDestinationNetwork(t *testing.T) {
	ctx := relayContext(t, "relay-a")
	storage := store.NewMemory()
	for _, relay := range []string{"relay-a", "relay-b"} {
		if _, _, err := storage.RegisterInstance(ctx, store.Instance{RelayID: relay, BootID: "boot", MeshAddr: relay + ":9444"}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	source, err := storage.AcquireSession(ctx, store.Session{NetworkID: "source-network", NodeID: "source", RelayID: "relay-a", BootID: "boot"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct{ network, relay string }{{"source-network", "relay-a"}, {"destination-network", "relay-b"}} {
		if _, err := storage.AcquireSession(ctx, store.Session{NetworkID: pair.network, NodeID: "same-node", RelayID: pair.relay, BootID: "boot"}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{Store: storage, Authorizer: scopedAuthorizer{}}
	credential := protocolv1.Credential{Algorithm: protocolv1.CredentialAlgorithm, KeyID: "fixture", NetworkID: source.NetworkID, NodeID: source.NodeID, ExpiresAt: time.Now().Add(time.Minute), Signature: "fixture"}
	request := &relayv1.AuthorizePeerRequest{RelayId: "relay-a", BootId: "boot", SourceEpoch: source.Epoch, Credential: relayv1.CredentialFromProtocol(credential), PeerNetworkId: "destination-network", PeerId: "same-node"}
	result, err := server.AuthorizePeer(ctx, request)
	if err != nil || result.GetDestinationRelayId() != "relay-b" || result.GetDestinationEpoch() <= 0 {
		t.Fatalf("wrong scoped destination: %v, %v", result, err)
	}
	request.PeerNetworkId = "source-network"
	if _, err := server.AuthorizePeer(ctx, request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("network substitution: %v", err)
	}
	request.PeerNetworkId = ""
	if _, err := server.AuthorizePeer(ctx, request); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing destination network: %v", err)
	}
	request.PeerNetworkId = "destination-network"
	request.SourceEpoch++
	if _, err := server.AuthorizePeer(ctx, request); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("source epoch substitution: %v", err)
	}
}
