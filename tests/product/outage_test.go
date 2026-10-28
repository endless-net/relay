//go:build product

package product

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/endless-net/relay/internal/testkit"
	wire "github.com/endless-net/relay/internal/wire"
)

// This fixture explicitly restarts its single Coordinator process. It does not
// assert an automatic failover/restart policy of an external production platform.
func TestRuntimeAuthorityOutageAndExplicitCoordinatorRestart(t *testing.T) {
	f := newFixture(t)
	a, _ := f.client(t, 0, 0)
	defer func() { _ = a.Close() }()
	b, destination := f.client(t, 1, 1)
	defer func() { _ = b.Close() }()
	c, source := f.client(t, 0, 2)
	defer func() { _ = c.Close() }()
	d, unusedDestination := f.client(t, 1, 3)
	defer func() { _ = d.Close() }()
	frame := wire.ClientFrame{Type: wire.MessageClientFrame, ProtocolVersion: 1, PeerNetworkID: "fixture", PeerID: "node-1", Payload: []byte("previously-authorized")}
	if err := json.NewEncoder(a).Encode(frame); err != nil {
		t.Fatal(err)
	}
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	var prime wire.ServerFrame
	if err := destination.Decode(&prime); err != nil || string(prime.Payload) != "previously-authorized" {
		t.Fatal("failed to prime permitted pair", err)
	}
	failedAt := time.Now()
	f.authority.unavailable.Store(true)

	// node-2 -> node-3 is permitted by policy but has never had an allow grant.
	if err := json.NewEncoder(c).Encode(wire.ClientFrame{Type: wire.MessageClientFrame, ProtocolVersion: 1, PeerNetworkID: "fixture", PeerID: "node-3", Payload: []byte("new-pair-must-fail")}); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var denied wire.Error
	if err := source.Decode(&denied); err != nil || denied.Type != wire.MessageError {
		t.Fatal("new pair admitted during authority outage", err)
	}
	_ = d.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	var unexpected wire.ServerFrame
	if err := unusedDestination.Decode(&unexpected); err == nil {
		t.Fatal("new pair delivered data during authority outage")
	}

	connection, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 2 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.pki.roots, ServerName: "localhost"}}).DialContext(t.Context(), "tcp", f.public[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	credential, err := testkit.Sign(f.private, "fixture", "node-4", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	if err = json.NewEncoder(connection).Encode(wire.ClientHello{Type: wire.MessageClientHello, ProtocolVersion: 1, Credential: *credential}); err != nil {
		t.Fatal(err)
	}
	var rejected wire.Error
	if err = json.NewDecoder(connection).Decode(&rejected); err != nil || rejected.Type != wire.MessageError {
		t.Fatal("new session admitted during authority outage", err)
	}

	// Keep offering traffic on the previously granted pair until the session ends.
	stop, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop.Done():
				return
			case <-ticker.C:
				_ = a.SetWriteDeadline(failedAt.Add(15 * time.Second))
				if json.NewEncoder(a).Encode(frame) != nil {
					return
				}
			}
		}
	}()
	defer func() { cancel(); _ = a.Close(); <-done }()
	_ = b.SetReadDeadline(failedAt.Add(15 * time.Second))
	var lastPayload time.Duration
	for {
		var message struct {
			Type    string `json:"type"`
			Payload []byte `json:"payload"`
		}
		err = destination.Decode(&message)
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("existing session outlived15s outage cutoff")
			}
			break
		}
		if message.Type == wire.MessageServerFrame {
			lastPayload = time.Since(failedAt)
			if lastPayload > 10500*time.Millisecond {
				t.Fatal("payload outlived10s offline grant", lastPayload)
			}
		}
	}
	cancel()
	_ = a.Close()
	<-done
	t.Logf("authority unavailable: new session/pair rejected; last payload=%s, session closed=%s", lastPayload, time.Since(failedAt))

	// Let every old session lease expire, then restore authority and explicitly
	// activate the same Coordinator binary against the durable registry.
	if delay := time.Until(failedAt.Add(16 * time.Second)); delay > 0 {
		time.Sleep(delay)
	}
	f.authority.unavailable.Store(false)
	recoveredAt := time.Now()
	old := f.coordinator
	f.coordinator = startChild(t, f.ctx, old.command.Path, nil, old.command.Env, f.dir)
	waitReady(t, f.ctx, f.coordinatorDiagnostics)
	for _, address := range f.diagnostics {
		waitReady(t, f.ctx, address)
	}
	freshA, _ := f.client(t, 0, 0)
	defer func() { _ = freshA.Close() }()
	freshB, freshReader := f.client(t, 1, 1)
	defer func() { _ = freshB.Close() }()
	if err = json.NewEncoder(freshA).Encode(frame); err != nil {
		t.Fatal(err)
	}
	_ = freshB.SetReadDeadline(recoveredAt.Add(15 * time.Second))
	var restored wire.ServerFrame
	if err = freshReader.Decode(&restored); err != nil || string(restored.Payload) != "previously-authorized" {
		t.Fatal("mesh did not recover after explicit restart", err)
	}
	if elapsed := time.Since(recoveredAt); elapsed > 15*time.Second {
		t.Fatal("recovery exceeded15s", elapsed)
	} else {
		t.Logf("after explicit Coordinator restart, sessions+mesh recovered in %s", elapsed)
	}
}
