package relay

import (
	"fmt"
	"strings"
	"sync/atomic"
)

type Metrics struct {
	connectionsActive atomic.Int64
	connectionsTotal  atomic.Uint64
	connectionsGlobal atomic.Uint64
	connectionsSource atomic.Uint64
	connectionsAuth   atomic.Uint64
	authActive        atomic.Int64
	sessionsActive    atomic.Int64
	sessionsTotal     atomic.Uint64
	sessionsRevoked   atomic.Uint64
	heartbeatsTotal   atomic.Uint64
	authFailuresTotal atomic.Uint64
	framesInbound     atomic.Uint64
	framesOutbound    atomic.Uint64
	bytesInbound      atomic.Uint64
	bytesOutbound     atomic.Uint64
	dropsInvalid      atomic.Uint64
	dropsNoPeer       atomic.Uint64
	dropsWriteFailed  atomic.Uint64
	dropsSlowConsumer atomic.Uint64
	dropsBandwidth    atomic.Uint64
	dropsACL          atomic.Uint64
	dropsMesh         atomic.Uint64
	dropsFenced       atomic.Uint64
	slowConsumers     atomic.Uint64
	draining          atomic.Bool
}

func NewMetrics() *Metrics {
	return &Metrics{}
}

func (m *Metrics) Render() string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	writeMetricHelp(&b, "endlessnet_relay_connections_active", "Currently admitted relay connections, including authentication in progress.")
	writeMetricType(&b, "endlessnet_relay_connections_active", "gauge")
	fmt.Fprintf(&b, "endlessnet_relay_connections_active %d\n", m.connectionsActive.Load())
	writeMetricHelp(&b, "endlessnet_relay_connections_total", "Relay connections admitted before authentication.")
	writeMetricType(&b, "endlessnet_relay_connections_total", "counter")
	fmt.Fprintf(&b, "endlessnet_relay_connections_total %d\n", m.connectionsTotal.Load())
	writeMetricHelp(&b, "endlessnet_relay_connection_rejections_total", "Relay connections rejected by bounded admission reason.")
	writeMetricType(&b, "endlessnet_relay_connection_rejections_total", "counter")
	fmt.Fprintf(&b, "endlessnet_relay_connection_rejections_total{reason=\"global_limit\"} %d\n", m.connectionsGlobal.Load())
	fmt.Fprintf(&b, "endlessnet_relay_connection_rejections_total{reason=\"source_limit\"} %d\n", m.connectionsSource.Load())
	fmt.Fprintf(&b, "endlessnet_relay_connection_rejections_total{reason=\"auth_limit\"} %d\n", m.connectionsAuth.Load())
	writeMetricHelp(&b, "endlessnet_relay_auth_active", "Relay authentication operations currently in progress.")
	writeMetricType(&b, "endlessnet_relay_auth_active", "gauge")
	fmt.Fprintf(&b, "endlessnet_relay_auth_active %d\n", m.authActive.Load())
	writeMetricHelp(&b, "endlessnet_relay_sessions_active", "Currently active relay sessions.")
	writeMetricType(&b, "endlessnet_relay_sessions_active", "gauge")
	fmt.Fprintf(&b, "endlessnet_relay_sessions_active %d\n", m.sessionsActive.Load())
	writeMetricHelp(&b, "endlessnet_relay_sessions_total", "Relay sessions accepted after credential verification.")
	writeMetricType(&b, "endlessnet_relay_sessions_total", "counter")
	fmt.Fprintf(&b, "endlessnet_relay_sessions_total %d\n", m.sessionsTotal.Load())
	writeMetricHelp(&b, "endlessnet_relay_sessions_revoked_total", "Relay sessions closed after active credential revalidation failed.")
	writeMetricType(&b, "endlessnet_relay_sessions_revoked_total", "counter")
	fmt.Fprintf(&b, "endlessnet_relay_sessions_revoked_total %d\n", m.sessionsRevoked.Load())
	writeMetricHelp(&b, "endlessnet_relay_heartbeats_total", "Relay heartbeat messages written to active sessions.")
	writeMetricType(&b, "endlessnet_relay_heartbeats_total", "counter")
	fmt.Fprintf(&b, "endlessnet_relay_heartbeats_total %d\n", m.heartbeatsTotal.Load())
	writeMetricHelp(&b, "endlessnet_relay_auth_failures_total", "Relay authentication failures.")
	writeMetricType(&b, "endlessnet_relay_auth_failures_total", "counter")
	fmt.Fprintf(&b, "endlessnet_relay_auth_failures_total %d\n", m.authFailuresTotal.Load())
	writeMetricHelp(&b, "endlessnet_relay_frames_total", "Relay frames processed by direction.")
	writeMetricType(&b, "endlessnet_relay_frames_total", "counter")
	fmt.Fprintf(&b, "endlessnet_relay_frames_total{direction=\"inbound\"} %d\n", m.framesInbound.Load())
	fmt.Fprintf(&b, "endlessnet_relay_frames_total{direction=\"outbound\"} %d\n", m.framesOutbound.Load())
	writeMetricHelp(&b, "endlessnet_relay_bytes_total", "Relay payload bytes processed by direction.")
	writeMetricType(&b, "endlessnet_relay_bytes_total", "counter")
	fmt.Fprintf(&b, "endlessnet_relay_bytes_total{direction=\"inbound\"} %d\n", m.bytesInbound.Load())
	fmt.Fprintf(&b, "endlessnet_relay_bytes_total{direction=\"outbound\"} %d\n", m.bytesOutbound.Load())
	writeMetricHelp(&b, "endlessnet_relay_drops_total", "Relay frame drops by reason.")
	writeMetricType(&b, "endlessnet_relay_drops_total", "counter")
	fmt.Fprintf(&b, "endlessnet_relay_drops_total{reason=\"invalid\"} %d\n", m.dropsInvalid.Load())
	fmt.Fprintf(&b, "endlessnet_relay_drops_total{reason=\"no_peer\"} %d\n", m.dropsNoPeer.Load())
	fmt.Fprintf(&b, "endlessnet_relay_drops_total{reason=\"write_failed\"} %d\n", m.dropsWriteFailed.Load())
	fmt.Fprintf(&b, "endlessnet_relay_drops_total{reason=\"slow_consumer\"} %d\n", m.dropsSlowConsumer.Load())
	fmt.Fprintf(&b, "endlessnet_relay_drops_total{reason=\"bandwidth\"} %d\n", m.dropsBandwidth.Load())
	fmt.Fprintf(&b, "endlessnet_relay_drops_total{reason=\"acl\"} %d\n", m.dropsACL.Load())
	fmt.Fprintf(&b, "endlessnet_relay_drops_total{reason=\"mesh\"} %d\n", m.dropsMesh.Load())
	fmt.Fprintf(&b, "endlessnet_relay_drops_total{reason=\"fenced\"} %d\n", m.dropsFenced.Load())
	writeMetricHelp(&b, "endlessnet_relay_slow_consumers_total", "Relay peers disconnected because their outbound frame queue could not keep up.")
	writeMetricType(&b, "endlessnet_relay_slow_consumers_total", "counter")
	fmt.Fprintf(&b, "endlessnet_relay_slow_consumers_total %d\n", m.slowConsumers.Load())
	writeMetricHelp(&b, "endlessnet_relay_draining", "Whether the relay server is draining sessions due to shutdown.")
	writeMetricType(&b, "endlessnet_relay_draining", "gauge")
	if m.draining.Load() {
		fmt.Fprintln(&b, "endlessnet_relay_draining 1")
	} else {
		fmt.Fprintln(&b, "endlessnet_relay_draining 0")
	}
	return b.String()
}

func (m *Metrics) recordConnectionStart() {
	if m == nil {
		return
	}
	m.connectionsActive.Add(1)
	m.connectionsTotal.Add(1)
}

func (m *Metrics) recordConnectionEnd() {
	if m != nil {
		m.connectionsActive.Add(-1)
	}
}

func (m *Metrics) recordConnectionRejected(reason string) {
	if m == nil {
		return
	}
	switch reason {
	case "source_limit":
		m.connectionsSource.Add(1)
	case "auth_limit":
		m.connectionsAuth.Add(1)
	default:
		m.connectionsGlobal.Add(1)
	}
}

func (m *Metrics) recordAuthStart() {
	if m != nil {
		m.authActive.Add(1)
	}
}

func (m *Metrics) recordAuthEnd() {
	if m != nil {
		m.authActive.Add(-1)
	}
}

func (m *Metrics) recordSessionStart() {
	if m == nil {
		return
	}
	m.sessionsActive.Add(1)
	m.sessionsTotal.Add(1)
}

func (m *Metrics) recordSessionEnd() {
	if m == nil {
		return
	}
	m.sessionsActive.Add(-1)
}

func (m *Metrics) recordSessionRevoked() {
	if m != nil {
		m.sessionsRevoked.Add(1)
	}
}

func (m *Metrics) recordHeartbeat() {
	if m != nil {
		m.heartbeatsTotal.Add(1)
	}
}

func (m *Metrics) recordAuthFailure() {
	if m != nil {
		m.authFailuresTotal.Add(1)
	}
}

func (m *Metrics) recordInboundFrame(size int) {
	if m == nil {
		return
	}
	m.framesInbound.Add(1)
	m.bytesInbound.Add(uint64(size))
}

func (m *Metrics) recordOutboundFrame(size int) {
	if m == nil {
		return
	}
	m.framesOutbound.Add(1)
	m.bytesOutbound.Add(uint64(size))
}

func (m *Metrics) recordDrop(reason string) {
	if m == nil {
		return
	}
	switch reason {
	case "no_peer":
		m.dropsNoPeer.Add(1)
	case "slow_consumer":
		m.dropsSlowConsumer.Add(1)
		m.slowConsumers.Add(1)
	case "bandwidth":
		m.dropsBandwidth.Add(1)
	case "acl":
		m.dropsACL.Add(1)
	case "write_failed":
		m.dropsWriteFailed.Add(1)
	case "mesh":
		m.dropsMesh.Add(1)
	case "fenced":
		m.dropsFenced.Add(1)
	default:
		m.dropsInvalid.Add(1)
	}
}

func (m *Metrics) setDraining(value bool) {
	if m != nil {
		m.draining.Store(value)
	}
}

func writeMetricHelp(b *strings.Builder, name, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
}

func writeMetricType(b *strings.Builder, name, metricType string) {
	fmt.Fprintf(b, "# TYPE %s %s\n", name, metricType)
}
