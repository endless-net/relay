//go:build product

package product

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

func TestRuntimeRegistryFailureAndExplicitRestart(t *testing.T) {
	f := newFixture(t)
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
	waitExited(t, f.coordinator, failedAt.Add(6*time.Second))
	assertSafeFailure(t, f.coordinator, "registry_unavailable")
	assertNotReady(t, f.coordinatorDiagnostics)
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
	t.Logf("registry failure stopped Coordinator; existing session closed in %s", time.Since(failedAt))
	if delay := time.Until(failedAt.Add(16 * time.Second)); delay > 0 {
		time.Sleep(delay)
	}
	recoveredAt := time.Now()
	oldDB := f.registry
	f.registry = startChild(t, f.ctx, oldDB.command.Path, oldDB.command.Args[1:], oldDB.command.Env, f.dir)
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
	oldCoordinator := f.coordinator
	f.coordinator = startChild(t, f.ctx, oldCoordinator.command.Path, nil, oldCoordinator.command.Env, f.dir)
	waitReady(t, f.ctx, f.coordinatorDiagnostics)
	for _, address := range f.diagnostics {
		waitReady(t, f.ctx, address)
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
		t.Logf("after explicit registry/Coordinator restart recovered in %s", elapsed)
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
