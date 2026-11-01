package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/endless-net/relay/internal/budget"
	wire "github.com/endless-net/relay/internal/wire"
)

func TestQueueByteCapAndGlobalRelease(t *testing.T) {
	pool := &budget.Bytes{Limit: 4096}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &session{ctx: ctx, pool: pool, sendCh: make(chan wire.ServerFrame, 32), done: make(chan struct{})}
	frame := wire.ServerFrame{Payload: make([]byte, 1000)}
	if e := s.enqueueFrame(frame); e != nil {
		t.Fatal(e)
	}
	if e := s.enqueueFrame(frame); e == nil {
		t.Fatal("global byte cap bypassed")
	}
	queued := <-s.sendCh
	s.releaseFrame(queued)
	if pool.Used() != 0 || s.queueBytes.Load() != 0 {
		t.Fatal("retained reservation")
	}
}

func TestExpiredFrameNeverReachesSocket(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &session{ctx: ctx, conn: a, writer: bufio.NewWriter(a), pool: &budget.Bytes{Limit: 4096}, sendCh: make(chan wire.ServerFrame, 1), done: make(chan struct{})}
	expired := time.Now().Add(-time.Second)
	if e := s.writeFrame(wire.ServerFrame{Payload: []byte("forbidden"), ValidUntil: expired}); e == nil {
		t.Fatal("expired frame accepted")
	}
	_ = b.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	one := make([]byte, 1)
	if n, _ := b.Read(one); n != 0 {
		t.Fatal("expired frame delivered")
	}
}

func TestConcurrentQueueCloseReleasesBudget(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{ctx: ctx, cancel: cancel, conn: a, writer: bufio.NewWriter(a), pool: &budget.Bytes{Limit: 4096}, sendCh: make(chan wire.ServerFrame, 32), done: make(chan struct{})}
	s.startWriter()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_ = s.enqueueFrame(wire.ServerFrame{Payload: []byte("a"), Guard: func() bool { return false }})
		})
	}
	s.close()
	wg.Wait()
	<-s.done
	if s.pool.Used() != 0 {
		t.Fatal("closed queue retained memory budget")
	}
}

func TestTrustKeyExpiryBoundsAnAlreadyStartedWrite(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	end := time.Now().Add(1200 * time.Millisecond)
	// Resolve validates key material, even though this socket fixture does not sign.
	bundle, err := wire.NewSigningTrustBundle("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	bundle.Keys[0].NotAfter = &end
	s := &session{conn: a, writer: bufio.NewWriter(a), credential: wire.Credential{KeyID: bundle.ActiveKeyID}, trust: func() (wire.SigningTrustBundle, error) { return bundle, nil }}
	started := time.Now()
	if err = s.writeFrame(wire.ServerFrame{Payload: []byte("blocked")}); err == nil {
		t.Fatal("slow write survived trust expiry")
	}
	if time.Since(started) > 700*time.Millisecond {
		t.Fatal("write ignored conservative trust deadline")
	}
}

func TestRemoteQueueHasBoundedResidence(t *testing.T) {
	sess := &session{lease: SessionLease{Epoch: 7}, pool: &budget.Bytes{Limit: 4096}, sendCh: make(chan wire.ServerFrame, 1), done: make(chan struct{})}
	srv := &Server{sessions: map[string]map[string]*session{"n": {"b": sess}}}
	before := time.Now()
	if err := srv.DeliverRemote("n", "a", "n", "b", 7, time.Now().Add(time.Second), nil, []byte("opaque")); err != nil {
		t.Fatal(err)
	}
	frame := <-sess.sendCh
	defer sess.releaseFrame(frame)
	if frame.ValidUntil.Before(before) || frame.ValidUntil.After(time.Now().Add(2*time.Second)) {
		t.Fatal("remote queue has no bounded residence")
	}
}

func TestRemoteSourceGrantExpiresWhileDestinationRemainsValid(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	sess := &session{conn: a, writer: bufio.NewWriter(a), lease: SessionLease{Epoch: 7}, pool: &budget.Bytes{Limit: 4096}, sendCh: make(chan wire.ServerFrame, 1), done: make(chan struct{})}
	destinationUntil := time.Now().Add(time.Minute)
	sess.validUntil.Store(&destinationUntil)
	srv := &Server{sessions: map[string]map[string]*session{"n": {"b": sess}}}
	if err := srv.DeliverRemote("n", "a", "n", "b", 7, time.Now().Add(20*time.Millisecond), func() bool { return true }, []byte("expired-source")); err != nil {
		t.Fatal(err)
	}
	frame := <-sess.sendCh
	defer sess.releaseFrame(frame)
	time.Sleep(30 * time.Millisecond)
	if !sess.usable() {
		t.Fatal("destination fixture should remain valid")
	}
	if err := sess.writeFrame(frame); err == nil {
		t.Fatal("source lifetime was restarted at destination")
	}
	_ = b.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	if count, _ := b.Read(make([]byte, 1)); count != 0 {
		t.Fatal("expired source bytes emitted")
	}
}

func TestRevokedQueuedPairDoesNotCloseValidDestination(t *testing.T) {
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	sess := &session{ctx: ctx, cancel: cancel, conn: a, writer: bufio.NewWriter(a), pool: &budget.Bytes{Limit: 4096}, sendCh: make(chan wire.ServerFrame, 2), done: make(chan struct{}), metrics: NewMetrics()}
	defer func() { sess.close(); _ = b.Close(); <-sess.done }()
	var allowed atomic.Bool
	allowed.Store(true)
	var calls atomic.Int64
	atWrite := make(chan struct{})
	guard := func() bool {
		if calls.Add(1) == 2 {
			close(atWrite)
		}
		return allowed.Load()
	}
	sess.writeMu.Lock()
	sess.startWriter()
	if err := sess.enqueueFrame(wire.ServerFrame{Payload: []byte("forbidden"), Guard: guard}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-atWrite:
	case <-time.After(time.Second):
		sess.writeMu.Unlock()
		t.Fatal("writer did not reach guarded write")
	}
	allowed.Store(false)
	sess.writeMu.Unlock()
	if err := sess.enqueueFrame(wire.ServerFrame{Payload: []byte("permitted")}); err != nil {
		t.Fatal(err)
	}
	_ = b.SetReadDeadline(time.Now().Add(time.Second))
	var frame wire.ServerFrame
	if err := json.NewDecoder(b).Decode(&frame); err != nil || string(frame.Payload) != "permitted" {
		t.Fatal("pair denial closed valid destination", err)
	}
}
