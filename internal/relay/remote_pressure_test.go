package relay

import (
	"net"
	"testing"
	"time"

	protocolv1 "github.com/endless-net/relay/internal/wire"
)

func TestRemoteQueueOverflowDropsOnlyNewFrame(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()
	sess := &session{networkID: "n", nodeID: "b", conn: a, lease: SessionLease{Epoch: 1}, sendCh: make(chan protocolv1.ServerFrame, 1), done: make(chan struct{})}
	s := &Server{}
	s.addSession(sess)
	if err := s.DeliverRemote("n", "a", "n", "b", 1, time.Now().Add(time.Second), nil, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeliverRemote("n", "a", "n", "b", 1, time.Now().Add(time.Second), nil, []byte("second")); err == nil {
		t.Fatal("queue should reject")
	}
	if sess.closed.Load() {
		t.Fatal("queue overflow closed an otherwise valid session")
	}
}
