//go:build product

package product

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	pb "github.com/endless-net/relay/internal/grpcwire"
	"github.com/endless-net/relay/internal/relaycontrol"
	"github.com/endless-net/relay/internal/tlsconfig"
	wire "github.com/endless-net/relay/internal/wire"
	"google.golang.org/grpc/metadata"
)

func TestRuntimeUnavailableExporterPreservesReadinessAndMesh(t *testing.T) {
	f := newFixtureWithTelemetry(t, "https://"+freeAddress(t))
	identity, err := tlsconfig.NewWorkloadRuntime(t.Context(), "unix://"+filepath.Join(f.dir, "a.sock"), "spiffe://endlessnet.ru/service/relay/a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = identity.Close() }()
	transport, err := identity.ClientTLSConfig("spiffe://endlessnet.ru/service/relay-coordinator")
	if err != nil {
		t.Fatal(err)
	}
	// Resolve the exact control address from this fixture's own process env.
	var address string
	for _, entry := range f.processes[0].command.Env {
		if value, found := strings.CutPrefix(entry, "ENDLESSNET_RELAY_COORDINATOR_ADDRESS="); found {
			address = value
		}
	}
	connection, err := relaycontrol.Dial(address, transport)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	// A sampled parent makes exporter failure deterministic despite 1% root
	// sampling. Empty boot/endpoint is rejected without changing live ownership.
	ctx := metadata.NewOutgoingContext(t.Context(), metadata.Pairs("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"))
	if _, err = pb.NewRelayControlClient(connection).RegisterInstance(ctx, &pb.RegisterInstanceRequest{RelayId: "a"}); err == nil {
		t.Fatal("invalid probe unexpectedly changed registry")
	}
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(6 * time.Second)
	failed := false
	for time.Now().Before(deadline) {
		request, e := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+f.coordinatorDiagnostics+"/metrics", nil)
		if e != nil {
			t.Fatal(e)
		}
		response, e := client.Do(request)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := io.ReadAll(io.LimitReader(response.Body, 65536))
		_ = response.Body.Close()
		if e != nil {
			t.Fatal(e)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if value, found := strings.CutPrefix(line, "endlessnet_relay_export_failed_total "); found {
				count, _ := strconv.ParseUint(value, 10, 64)
				failed = count > 0
			}
		}
		if failed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !failed {
		t.Fatal("exporter failure was not observed")
	}
	waitReady(t, f.ctx, f.coordinatorDiagnostics)
	for _, diagnostics := range f.diagnostics {
		waitReady(t, f.ctx, diagnostics)
	}
	a, _ := f.client(t, 0, 0)
	defer func() { _ = a.Close() }()
	b, reader := f.client(t, 1, 1)
	defer func() { _ = b.Close() }()
	if err = json.NewEncoder(a).Encode(wire.ClientFrame{Type: wire.MessageClientFrame, ProtocolVersion: 1, PeerNetworkID: "fixture", PeerID: "node-1", Payload: []byte("exporter-is-optional")}); err != nil {
		t.Fatal(err)
	}
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	var frame wire.ServerFrame
	if err = reader.Decode(&frame); err != nil || string(frame.Payload) != "exporter-is-optional" {
		t.Fatal("optional exporter broke mesh", err)
	}
	t.Log("unavailable HTTPS exporter counted; both roles stayed ready and fresh mesh authorization/delivery succeeded")
}
