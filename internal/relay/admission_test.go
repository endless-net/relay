package relay

import (
	"strings"
	"testing"
)

type testAddr string

func (a testAddr) Network() string { return "tcp" }
func (a testAddr) String() string  { return string(a) }

func TestAdmissionControllerBoundsGlobalAndPerSourceConnections(t *testing.T) {
	metrics := NewMetrics()
	controller, err := NewAdmissionController(AdmissionLimits{
		MaxConnections:          2,
		MaxConcurrentAuth:       1,
		MaxConnectionsPerSource: 1,
	}, metrics)
	if err != nil {
		t.Fatal(err)
	}

	releaseA, ok := controller.tryAcquireConnection(testAddr("192.0.2.10:1000"))
	if !ok {
		t.Fatal("first source connection was rejected")
	}
	if release, ok := controller.tryAcquireConnection(testAddr("192.0.2.10:1001")); ok {
		release()
		t.Fatal("connection above the per-source limit was admitted")
	}
	releaseB, ok := controller.tryAcquireConnection(testAddr("192.0.2.11:1000"))
	if !ok {
		t.Fatal("second source connection was rejected")
	}
	if release, ok := controller.tryAcquireConnection(testAddr("192.0.2.12:1000")); ok {
		release()
		t.Fatal("connection above the global limit was admitted")
	}

	rendered := metrics.Render()
	for _, want := range []string{
		"endlessnet_relay_connections_active 2",
		"endlessnet_relay_connections_total 2",
		`endlessnet_relay_connection_rejections_total{reason="source_limit"} 1`,
		`endlessnet_relay_connection_rejections_total{reason="global_limit"} 1`,
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("relay admission metrics missing %q:\n%s", want, rendered)
		}
	}

	releaseA()
	releaseA()
	releaseB()
	if len(controller.sources) != 0 {
		t.Fatalf("released source limiter entries = %#v, want empty", controller.sources)
	}
	if rendered := metrics.Render(); !strings.Contains(rendered, "endlessnet_relay_connections_active 0") {
		t.Fatalf("relay active connection metric after release:\n%s", rendered)
	}
}

func TestAdmissionControllerLoadShedsConcurrentAuthentication(t *testing.T) {
	metrics := NewMetrics()
	controller, err := NewAdmissionController(AdmissionLimits{
		MaxConnections:          2,
		MaxConcurrentAuth:       1,
		MaxConnectionsPerSource: 2,
	}, metrics)
	if err != nil {
		t.Fatal(err)
	}
	release, ok := controller.tryAcquireAuth()
	if !ok {
		t.Fatal("first authentication was rejected")
	}
	if secondRelease, ok := controller.tryAcquireAuth(); ok {
		secondRelease()
		t.Fatal("authentication above concurrency limit was admitted")
	}
	if rendered := metrics.Render(); !strings.Contains(rendered, `endlessnet_relay_connection_rejections_total{reason="auth_limit"} 1`) ||
		!strings.Contains(rendered, "endlessnet_relay_auth_active 1") {
		t.Fatalf("relay auth admission metrics:\n%s", rendered)
	}
	release()
	thirdRelease, ok := controller.tryAcquireAuth()
	if !ok {
		t.Fatal("authentication was not admitted after capacity release")
	}
	thirdRelease()
}

func TestRelaySourceNormalizesIPAndBoundsFallbackKey(t *testing.T) {
	if got := relaySource(testAddr("[2001:db8::1]:443")); got != "2001:db8::1" {
		t.Fatalf("IPv6 relay source = %q", got)
	}
	if got := relaySource(testAddr("192.0.2.1:443")); got != "192.0.2.1" {
		t.Fatalf("IPv4 relay source = %q", got)
	}
	if got := relaySource(testAddr(strings.Repeat("x", 300))); len(got) != 256 {
		t.Fatalf("fallback relay source length = %d, want 256", len(got))
	}
}
