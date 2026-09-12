package relay

import (
	"testing"
	"time"
)

func TestFenceClosesAuthenticatedSessionAndStopsReadiness(t *testing.T) {
	s, roots, key, _, cancel, done := startRelayForTest(t, time.Second)
	defer stopRelayForTest(t, cancel, done)
	conn, reader := authenticateRelayForTest(t, s.Addr, roots, key, "network", "node", 0)
	defer conn.Close()
	if !s.Ready() {
		t.Fatal("listening server not ready")
	}
	s.Fence()
	s.Fence() // fencing is idempotent under concurrent shutdown paths.
	if s.Ready() {
		t.Fatal("fenced server remains ready")
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("session remains open after fencing")
	}
}
