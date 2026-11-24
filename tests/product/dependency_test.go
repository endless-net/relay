//go:build product

package product

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/endless-net/relay/internal/testkit"
	wire "github.com/endless-net/relay/internal/wire"
)

func waitExited(t *testing.T, process *child, deadline time.Time) {
	t.Helper()
	timer := time.NewTimer(max(time.Duration(0), time.Until(deadline)))
	defer timer.Stop()
	select {
	case <-process.exited:
	case <-timer.C:
		t.Fatal("dependency failure did not stop the expected fixture process")
	}
}

func assertSafeFailure(t *testing.T, process *child, reason string) {
	t.Helper()
	raw, err := os.ReadFile(process.log)
	if err != nil || !strings.Contains(string(raw), `"msg":"`+reason+`"`) {
		t.Fatal("missing bounded dependency failure reason", reason)
	}
}

func assertNotReady(t *testing.T, address string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err == nil {
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode == 200 {
			t.Fatal("failed role reports ready")
		}
	}
}

func TestRuntimeRegistryFailureAndRecovery(t *testing.T) {
	assertRuntimeRegistryRecovery(t, newFixture(t))
}

func TestRuntimeDirectDNSRegistryFailureAndRecovery(t *testing.T) {
	assertRuntimeRegistryRecovery(t, newFixtureWithDatabaseProfile(t, "", "127.0.0.1:0", true))
}

func assertRuntimeRegistryRecovery(t *testing.T, f *fixture) {
	t.Helper()
	a, _ := f.client(t, 0, 0)
	defer func() { _ = a.Close() }()
	b, reader := f.client(t, 1, 1)
	defer func() { _ = b.Close() }()
	failedAt := time.Now()
	// SIGINT is PostgreSQL's clean fast shutdown, restricted to this fixture's
	// private child and data directory; it disconnects existing pool clients.
	if err := f.registry.command.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	waitExited(t, f.registry, failedAt.Add(5*time.Second))
	waitRegistryProbe(t, f, "/readyz", http.StatusServiceUnavailable)
	waitRegistryProbe(t, f, "/healthz", http.StatusOK)
	assertAlive(t, f.coordinator)
	assertRegistryColdStartFails(t, f)
	assertRegistryRejectsNewSession(t, f)
	_ = b.SetReadDeadline(failedAt.Add(15 * time.Second))
	for {
		var message json.RawMessage
		err := reader.Decode(&message)
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("session survived registry outage cutoff")
			}
			break
		}
	}
	t.Logf("registry failure kept Coordinator alive; existing session closed in %s", time.Since(failedAt))
	if delay := time.Until(failedAt.Add(16 * time.Second)); delay > 0 {
		time.Sleep(delay)
	}
	recoveredAt := time.Now()
	oldDB := f.registry
	f.registry = startChild(t, f.ctx, oldDB.command.Path, oldDB.command.Args[1:], oldDB.command.Env, f.dir)
	// Fast shutdown releases the live Coordinator pool before fixture cleanup.
	t.Cleanup(func() { _ = f.registry.command.Process.Signal(syscall.SIGINT) })
	host, port, _ := net.SplitHostPort(f.registryAddress)
	for {
		probe := exec.CommandContext(f.ctx, filepath.Join(filepath.Dir(oldDB.command.Path), "pg_isready"), "-h", host, "-p", port, "-U", "relaytest", "-d", "relay009")
		probe.Env = oldDB.command.Env
		if probe.Run() == nil {
			break
		}
		if time.Since(recoveredAt) > 5*time.Second {
			t.Fatal("fixture registry did not recover")
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitReady(t, f.ctx, f.coordinatorDiagnostics)
	assertAlive(t, f.coordinator)
	for i, address := range f.diagnostics {
		waitReady(t, f.ctx, address)
		assertAlive(t, f.processes[i])
	}
	freshA, _ := f.client(t, 0, 0)
	defer func() { _ = freshA.Close() }()
	freshB, freshReader := f.client(t, 1, 1)
	defer func() { _ = freshB.Close() }()
	if err := json.NewEncoder(freshA).Encode(wire.ClientFrame{Type: wire.MessageClientFrame, ProtocolVersion: 1, PeerNetworkID: "fixture", PeerID: "node-1", Payload: []byte("durable-registry-recovered")}); err != nil {
		t.Fatal(err)
	}
	_ = freshB.SetReadDeadline(recoveredAt.Add(15 * time.Second))
	var restored wire.ServerFrame
	if err := freshReader.Decode(&restored); err != nil || string(restored.Payload) != "durable-registry-recovered" {
		t.Fatal("mesh did not recover with durable registry", err)
	}
	if elapsed := time.Since(recoveredAt); elapsed > 15*time.Second {
		t.Fatal("registry recovery exceeded15s", elapsed)
	} else {
		t.Logf("after registry restart, the same Coordinator process recovered in %s", elapsed)
	}
}

func assertRegistryColdStartFails(t *testing.T, f *fixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 7*time.Second)
	defer cancel()
	probeAddress := freeAddress(t)
	cmd := exec.CommandContext(ctx, f.coordinator.command.Path)
	cmd.Dir = f.dir
	for _, entry := range f.coordinator.command.Env {
		if !strings.HasPrefix(entry, "ENDLESSNET_RELAY_CONTROL_LISTEN=") && !strings.HasPrefix(entry, "ENDLESSNET_RELAY_HTTP_LISTEN=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "ENDLESSNET_RELAY_CONTROL_LISTEN="+freeAddress(t), "ENDLESSNET_RELAY_HTTP_LISTEN="+probeAddress)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "startup_failed") {
		t.Fatalf("cold start without registry: %v, output %s", err, output)
	}
	assertNotReady(t, probeAddress)
}

func assertAlive(t *testing.T, process *child) {
	t.Helper()
	select {
	case <-process.exited:
		t.Fatal("fixture process exited")
	default:
	}
}

func waitRegistryProbe(t *testing.T, f *fixture, path string, statusCode int) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+f.coordinatorDiagnostics+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if response, err := client.Do(request); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == statusCode {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("Coordinator %s did not report %d", path, statusCode)
}

func assertRegistryRejectsNewSession(t *testing.T, f *fixture) {
	t.Helper()
	conn, err := (&tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 2 * time.Second},
		Config:    &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.pki.roots, ServerName: "localhost"},
	}).DialContext(t.Context(), "tcp", f.public[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	credential, err := testkit.Sign(f.private, "fixture", "node-4", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err := json.NewEncoder(conn).Encode(wire.ClientHello{Type: wire.MessageClientHello, ProtocolVersion: 1, Credential: *credential}); err != nil {
		t.Fatal(err)
	}
	var rejected wire.Error
	if err := json.NewDecoder(conn).Decode(&rejected); err != nil || rejected.Type != wire.MessageError {
		t.Fatal("new session was not rejected during registry outage", err)
	}
}

func TestRuntimeExpiredLocalWorkloadStopsDataplane(t *testing.T) {
	f := newFixture(t)
	a, reader := f.client(t, 0, 0)
	defer func() { _ = a.Close() }()
	short := f.pki.issue(t, "spiffe://endlessnet.ru/service/relay/a", "relay-a", 3*time.Second)
	f.workloads["a"].update(t, "spiffe://endlessnet.ru/service/relay/a", short, f.pki.ca.Raw)
	deadline := time.Now().Add(4 * time.Second)
	_ = a.SetReadDeadline(deadline)
	for {
		var message json.RawMessage
		err := reader.Decode(&message)
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("public session survived unusable workload identity")
			}
			break
		}
	}
	waitExited(t, f.processes[0], deadline)
	assertSafeFailure(t, f.processes[0], "workload_expired")
	assertNotReady(t, f.diagnostics[0])
	t.Log("expired current Workload API SVID removed readiness, closed public session and stopped dataplane")
}
