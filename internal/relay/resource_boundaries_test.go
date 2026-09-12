package relay

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	protocol "github.com/endless-net/relay/relayapi/v1"
)

func TestBandwidthExactLimitAndNewWindow(t *testing.T) {
	s := &session{bandwidthLimitBytesPerSec: 10}
	if !s.allowInboundPayload(4) || !s.allowInboundPayload(6) {
		t.Fatal("exact budget rejected")
	}
	if s.allowInboundPayload(1) || s.allowInboundPayload(11) {
		t.Fatal("over budget accepted")
	}
	s.bandwidthWindowStart = time.Now().Add(-time.Second)
	if !s.allowInboundPayload(10) {
		t.Fatal("new window did not replenish")
	}
}

func TestQueueCapacityClosureAndSessionReplacement(t *testing.T) {
	server := &Server{}
	makeSession := func() *session {
		left, right := net.Pipe()
		t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		return &session{networkID: "network", nodeID: "node", conn: left, ctx: ctx, cancel: cancel, done: make(chan struct{}), sendCh: make(chan protocol.ServerFrame, 1)}
	}
	old := makeSession()
	server.addSession(old)
	frame := newServerFrame("network", "sender", []byte{1})
	if err := old.enqueueFrame(frame); err != nil {
		t.Fatal(err)
	}
	if err := old.enqueueFrame(frame); !errors.Is(err, errRelayOutboundQueueFull) {
		t.Fatal("full queue accepted", err)
	}
	current := makeSession()
	server.addSession(current)
	if !old.closed.Load() {
		t.Fatal("old session not closed")
	}
	server.removeSession(old)
	if server.peerSession("network", "node") != current {
		t.Fatal("late cleanup removed replacement")
	}
	if err := old.enqueueFrame(frame); err == nil {
		t.Fatal("closed queue accepted frame")
	}
	server.removeSession(current)
	if server.peerSession("network", "node") != nil || len(server.sessions) != 0 {
		t.Fatal("session map not cleaned")
	}
}
