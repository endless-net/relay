package tlsconfig

import (
	"net"
	"sync"
	"testing"
	"time"
)

func TestConnectionExpiryCanOnlyShortenAndCloseIsConcurrentSafe(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	conn, e := ExpireConnection(a, time.Now().Add(time.Minute))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = ExpireConnection(conn, time.Now().Add(20*time.Millisecond)); e != nil {
		t.Fatal(e)
	}
	if _, e = ExpireConnection(conn, time.Now().Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	_ = b.SetReadDeadline(time.Now().Add(time.Second))
	if _, e = b.Read(make([]byte, 1)); e == nil {
		t.Fatal("expired connection remained usable")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { _ = conn.Close() })
	}
	wg.Wait()
}
