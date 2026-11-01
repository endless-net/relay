//go:build integration

package relay

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	protocolv1 "github.com/endless-net/relay/internal/wire"
)

func TestCrossNetworkDeliveryPreservesIdentityAndDeniesSubstitution(t *testing.T) {
	server, roots, key, control, cancel, done := startRelayForTest(t, time.Second)
	defer stopRelayForTest(t, cancel, done)
	target, targetReader := authenticateRelayForTest(t, server.Addr, roots, key, "destination", "same-node", 0)
	defer func() { _ = target.Close() }()
	source, sourceReader := authenticateRelayForTest(t, server.Addr, roots, key, "source", "same-node", 0)
	defer func() { _ = source.Close() }()
	control.mu.Lock()
	lease := control.sessions["destination/same-node"]
	control.mu.Unlock()
	control.authorizeHook = func(_ context.Context, credential protocolv1.Credential, epoch int64, networkID, nodeID string) (PeerRoute, error) {
		if credential.NetworkID != "source" || credential.NodeID != "same-node" || epoch <= 0 || networkID != "destination" || nodeID != "same-node" {
			return PeerRoute{}, errors.New("pair denied")
		}
		return PeerRoute{RelayID: lease.RelayID, BootID: lease.BootID, Epoch: lease.Epoch}, nil
	}
	send := func(networkID string) {
		t.Helper()
		if err := json.NewEncoder(source).Encode(protocolv1.ClientFrame{Type: protocolv1.MessageClientFrame, ProtocolVersion: protocolv1.Version, PeerNetworkID: networkID, PeerID: "same-node", Payload: []byte("scoped")}); err != nil {
			t.Fatal(err)
		}
	}
	send("other-network")
	_ = source.SetReadDeadline(time.Now().Add(time.Second))
	var denial protocolv1.Error
	if err := json.NewDecoder(sourceReader).Decode(&denial); err != nil || denial.Type != protocolv1.MessageError {
		t.Fatalf("substituted network: response=%+v error=%v", denial, err)
	}
	send("destination")
	_ = target.SetReadDeadline(time.Now().Add(time.Second))
	var frame protocolv1.ServerFrame
	if err := json.NewDecoder(targetReader).Decode(&frame); err != nil {
		t.Fatal(err)
	}
	if frame.FromNetworkID != "source" || frame.FromNodeID != "same-node" || string(frame.Payload) != "scoped" {
		t.Fatalf("wrong sender identity: %+v", frame)
	}
	if err := server.DeliverRemote("remote-source", "same-node", "destination", "same-node", lease.Epoch+1, time.Now().Add(time.Second), nil, []byte("stale")); !errors.Is(err, ErrDestinationFenced) {
		t.Fatalf("stale remote destination epoch: %v", err)
	}
	if err := server.DeliverRemote("remote-source", "same-node", "", "same-node", lease.Epoch, time.Now().Add(time.Second), nil, []byte("missing")); err == nil {
		t.Fatal("remote delivery accepted a missing network")
	}
	if err := server.DeliverRemote("remote-source", "same-node", "destination", "same-node", lease.Epoch, time.Now().Add(time.Second), nil, []byte("remote")); err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(targetReader).Decode(&frame); err != nil || frame.FromNetworkID != "remote-source" || string(frame.Payload) != "remote" {
		t.Fatalf("remote identity: frame=%+v error=%v", frame, err)
	}
}
