package relay

import (
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"net"
	"testing"
)

func TestRegressionRemoteSlowConsumerMustClose(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	sess := &session{networkID: "n", nodeID: "b", conn: a, lease: SessionLease{Epoch: 1}, sendCh: make(chan protocolv1.ServerFrame, 1), done: make(chan struct{})}
	s := &Server{}
	s.addSession(sess)
	if err := s.DeliverRemote("n", "a", "n", "b", 1, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverRemote("n", "a", "n", "b", 1, []byte("second")); err == nil {
		t.Fatal("queue should reject")
	}
	if !sess.closed.Load() {
		t.Fatal("mesh destination remained connected after outbound queue overflow")
	}
}
