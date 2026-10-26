package relayv1_test

import (
	"bytes"
	"testing"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	"google.golang.org/protobuf/proto"
)

func TestRegistryMeshInventoryExcludesUpstreamAuthority(t *testing.T) {
	file := relayv1.File_relay_v1_relay_proto
	if file.Services().Len() != 2 {
		t.Fatalf("unexpected service authority: %d", file.Services().Len())
	}
	control := file.Services().ByName("RelayControl")
	expected := []string{"RegisterInstance", "HeartbeatInstance", "AcquireSession", "RenewSession", "ReleaseSession", "AuthorizePeer"}
	if control.Methods().Len() != len(expected) {
		t.Fatal("control inventory changed")
	}
	for i, name := range expected {
		if string(control.Methods().Get(i).Name()) != name {
			t.Fatalf("method %d changed", i)
		}
	}
	mesh := file.Services().ByName("RelayMesh").Methods().ByName("Connect")
	if !mesh.IsStreamingClient() || !mesh.IsStreamingServer() {
		t.Fatal("mesh must remain bidi")
	}
	if file.Services().ByName("RelayUpstreamService") != nil {
		t.Fatal("foreign authority became Relay producer")
	}
}

func TestDirectedPairAndFencingRoundTrip(t *testing.T) {
	request := &relayv1.AuthorizePeerRequest{RelayId: "relay-fixture", BootId: "boot-fixture", Credential: &relayv1.Credential{Algorithm: "ed25519-relay-credential-v3", KeyId: "key-fixture", NetworkId: "source-network", NodeId: "source-node", ExpiresAtUnixNano: 123456789, Signature: "non-secret-test-signature"}, PeerId: "destination-node", SourceEpoch: 17, PeerNetworkId: "destination-network"}
	wire, err := proto.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded relayv1.AuthorizePeerRequest
	if err := proto.Unmarshal(wire, &decoded); err != nil || !proto.Equal(request, &decoded) {
		t.Fatalf("directed identities lost: %v", err)
	}
	if decoded.GetPeerNetworkId() == decoded.GetCredential().GetNetworkId() {
		t.Fatal("source/destination collapsed")
	}
	frame := &relayv1.MeshMessage{ProtocolVersion: 1, Body: &relayv1.MeshMessage_Frame{Frame: &relayv1.MeshFrame{NetworkId: "source-network", DestinationNetworkId: "destination-network", DestinationEpoch: 17, Payload: []byte{0, 255, 1}}}}
	wire, err = proto.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var decodedFrame relayv1.MeshMessage
	if err := proto.Unmarshal(wire, &decodedFrame); err != nil || !proto.Equal(frame, &decodedFrame) {
		t.Fatalf("opaque payload/fencing lost: %v", err)
	}
}

func TestMalformedAndFutureWire(t *testing.T) {
	for _, bad := range [][]byte{{0x0a, 0x80}, {0xff}, {0x0a, 0x02, 0x61}} {
		if err := proto.Unmarshal(bad, &relayv1.Credential{}); err == nil {
			t.Fatalf("malformed credential accepted: %x", bad)
		}
	}
	future := []byte{0x98, 0x06, 0x01}
	var request relayv1.AuthorizePeerRequest
	if err := proto.Unmarshal(future, &request); err != nil {
		t.Fatal(err)
	}
	wire, err := proto.Marshal(&request)
	if err != nil || !bytes.Equal(wire, future) {
		t.Fatalf("unknown bytes lost: %x %v", wire, err)
	}
	// This does not claim that the historical strict runtime boundary accepts the request.
}
