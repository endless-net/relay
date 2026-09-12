package mesh

import (
	"context"
	"testing"

	"github.com/endless-net/relay/internal/relay"
	rpc "github.com/endless-net/relay/relayapi/v1/relayapigrpc"
)

func TestMeshQueueBoundsAndCopiesPayload(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	peer := &peerConnection{ctx: ctx, cancel: cancel, instance: &rpc.RelayInstance{RelayId: "remote", BootId: "boot"}, send: make(chan *rpc.MeshMessage, 1)}
	peer.ready.Store(true)
	m := NewManager(ctx, "local", "local-boot", nil, nil)
	t.Cleanup(m.Close)
	m.peers["remote"] = peer
	route := relay.PeerRoute{RelayID: "remote", BootID: "boot", Epoch: 1}
	payload := []byte{1, 2, 3}
	if err := m.Forward(ctx, route, "a", "source", "b", "target", payload); err != nil {
		t.Fatal(err)
	}
	payload[0] = 9
	if err := m.Forward(ctx, route, "a", "source", "b", "target", payload); err == nil {
		t.Fatal("full mesh queue accepted frame")
	}
	message := <-peer.send
	frame := message.GetFrame()
	if frame.GetPayload()[0] != 1 || frame.GetNetworkId() != "a" || frame.GetDestinationNetworkId() != "b" || frame.GetFromNodeId() != "source" || frame.GetToNodeId() != "target" || frame.GetDestinationEpoch() != 1 {
		t.Fatal("queued frame changed identity, epoch or payload")
	}
	for _, invalid := range []relay.PeerRoute{{}, {RelayID: "local"}, {RelayID: "remote", BootID: "stale", Epoch: 1}, {RelayID: "remote", BootID: "boot", Epoch: 0}} {
		if err := m.Forward(ctx, invalid, "a", "source", "b", "target", payload); err == nil {
			t.Fatal("invalid mesh route accepted")
		}
	}
}

func TestPeerSnapshotPreservesUnchangedConnectionAndCancelsRemoved(t *testing.T) {
	m := NewManager(t.Context(), "local", "local-boot", nil, nil)
	t.Cleanup(m.Close)
	ctx, cancel := context.WithCancel(t.Context())
	existing := &peerConnection{ctx: ctx, cancel: cancel, instance: &rpc.RelayInstance{RelayId: "remote", BootId: "boot", MeshAddr: "remote:443"}}
	m.peers["remote"] = existing
	m.UpdatePeers([]*rpc.RelayInstance{nil, {RelayId: "local", BootId: "own", MeshAddr: "local:443"}, {RelayId: "remote", BootId: "boot", MeshAddr: "remote:443"}})
	if len(m.peers) != 1 || m.peers["remote"] != existing {
		t.Fatal("unchanged connection was replaced")
	}
	select {
	case <-ctx.Done():
		t.Fatal("unchanged connection cancelled")
	default:
	}
	m.UpdatePeers(nil)
	if len(m.peers) != 0 {
		t.Fatal("removed peer retained")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("removed peer not cancelled")
	}
}
