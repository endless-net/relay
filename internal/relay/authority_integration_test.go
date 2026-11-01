//go:build integration

package relay

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/endless-net/relay/internal/wire"
)

func TestPairAuthorityOutageDoesNotRevokeValidSession(t *testing.T) {
	server, roots, key, control, cancel, done := startRelayForTest(t, 5*time.Second)
	defer stopRelayForTest(t, cancel, done)
	var requests atomic.Int64
	control.authorizeHook = func(context.Context, wire.Credential, int64, string, string) (PeerRoute, error) {
		if requests.Add(1) == 1 {
			return PeerRoute{}, ErrControlUnavailable
		}
		return PeerRoute{RelayID: "relay-test", BootID: "boot-test", Epoch: 2}, nil
	}
	a, source := authenticateRelayForTest(t, server.Addr, roots, key, "n", "a", 0)
	defer func() { _ = a.Close() }()
	b, destination := authenticateRelayForTest(t, server.Addr, roots, key, "n", "b", 0)
	defer func() { _ = b.Close() }()
	frame := wire.ClientFrame{Type: wire.MessageClientFrame, ProtocolVersion: 1, PeerID: "b", PeerNetworkID: "n", Payload: []byte("opaque")}
	if e := json.NewEncoder(a).Encode(frame); e != nil {
		t.Fatal(e)
	}
	var failure wire.Error
	if e := json.NewDecoder(source).Decode(&failure); e != nil || failure.Type != wire.MessageError {
		t.Fatal("expected bounded pair rejection", e)
	}
	if e := json.NewEncoder(a).Encode(frame); e != nil {
		t.Fatal("valid session was closed", e)
	}
	_ = b.SetReadDeadline(time.Now().Add(time.Second))
	var received wire.ServerFrame
	if e := json.NewDecoder(destination).Decode(&received); e != nil || string(received.Payload) != "opaque" {
		t.Fatal("session did not recover", e)
	}
}
