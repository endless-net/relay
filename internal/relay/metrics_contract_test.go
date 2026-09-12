package relay

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestMetricsCountersAreConcurrentAndBoundedLabels(t *testing.T) {
	m := NewMetrics()
	var workers sync.WaitGroup
	for range 20 {
		workers.Go(func() {
			for range 10 {
				m.recordConnectionStart()
				m.recordAuthStart()
				m.recordInboundFrame(3)
				m.recordOutboundFrame(5)
				m.recordAuthEnd()
				m.recordConnectionEnd()
			}
		})
	}
	workers.Wait()
	if m.connectionsActive.Load() != 0 || m.authActive.Load() != 0 || m.framesInbound.Load() != 200 || m.bytesInbound.Load() != 600 || m.bytesOutbound.Load() != 1000 {
		t.Fatal("concurrent counters diverged")
	}
	for _, reason := range []string{"invalid", "no_peer", "write_failed", "slow_consumer", "bandwidth", "acl", "mesh", "fenced"} {
		m.recordDrop(reason)
	}
	// Unrecognized/internal strings must not become unbounded or secret labels.
	m.recordDrop("private-node-identifier")
	rendered := m.Render()
	for _, reason := range []string{"no_peer", "write_failed", "slow_consumer", "bandwidth", "acl", "mesh", "fenced"} {
		if !strings.Contains(rendered, fmt.Sprintf("endlessnet_relay_drops_total{reason=%q} 1", reason)) {
			t.Fatalf("missing drop metric %s", reason)
		}
	}
	if !strings.Contains(rendered, `endlessnet_relay_drops_total{reason="invalid"} 2`) || strings.Contains(rendered, "private-node-identifier") {
		t.Fatal("unknown drop reason leaked or was not counted")
	}
	m.setDraining(true)
	if !strings.Contains(m.Render(), "endlessnet_relay_draining 1") {
		t.Fatal("draining not exposed")
	}
	m.setDraining(false)
	if !strings.Contains(m.Render(), "endlessnet_relay_draining 0") {
		t.Fatal("draining did not clear")
	}
}
