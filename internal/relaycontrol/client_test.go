package relaycontrol

import (
	"testing"
	"time"

	relayv1 "github.com/unng-lab/endlessnet-relay/api/relay/v1"
	protocolv1 "github.com/unng-lab/endlessnet-relay/protocol/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestEveryUnaryResponseRejectsUnknownProtobufFields(t *testing.T) {
	responses := []proto.Message{
		&relayv1.RegisterInstanceResponse{},
		&relayv1.HeartbeatInstanceResponse{},
		&relayv1.AcquireSessionResponse{},
		&relayv1.RenewSessionResponse{},
		&relayv1.ReleaseSessionResponse{},
		&relayv1.AuthorizePeerResponse{},
	}
	unknown := protowire.AppendTag(nil, 99, protowire.VarintType)
	unknown = protowire.AppendVarint(unknown, 1)
	for _, response := range responses {
		response.ProtoReflect().SetUnknown(unknown)
		if err := rejectResponse(response); err == nil {
			t.Fatalf("unknown protobuf response field was accepted in %T", response)
		}
	}
}

func TestAcceptCoordinatorStateValidatesEntireResponseContract(t *testing.T) {
	bundle := protocolv1.SigningTrustBundle{Version: protocolv1.SigningTrustBundleVersion, ActiveKeyID: "missing"}
	client := &Client{Peers: peerUpdaterFunc(func([]*relayv1.RelayInstance) {})}
	if err := client.acceptCoordinatorState(nil, time.Now().Add(time.Minute).UnixNano(), relayv1.TrustBundleFromProtocol(bundle)); err == nil {
		t.Fatal("invalid trust bundle was accepted")
	}
	if err := client.acceptCoordinatorState([]*relayv1.RelayInstance{{RelayId: " relay", BootId: "boot", MeshAddr: "relay:9444"}}, time.Now().Add(time.Minute).UnixNano(), &relayv1.SigningTrustBundle{}); err == nil {
		t.Fatal("non-canonical peer identity was accepted")
	}
	if err := client.acceptCoordinatorState(nil, time.Now().Add(-time.Minute).UnixNano(), &relayv1.SigningTrustBundle{}); err == nil {
		t.Fatal("expired instance lease was accepted")
	}
}

type peerUpdaterFunc func([]*relayv1.RelayInstance)

func (f peerUpdaterFunc) UpdatePeers(peers []*relayv1.RelayInstance) { f(peers) }
