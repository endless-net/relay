package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/endless-net/relay/internal/store"
	protocol "github.com/endless-net/relay/relayapi/v1"
)

type statusStore struct {
	store.Store
	state atomic.Int32
}

func (s *statusStore) EndpointSnapshot(context.Context) (protocol.EndpointSnapshot, error) {
	switch s.state.Load() {
	case 1:
		return protocol.EndpointSnapshot{Version: 1}, nil
	case 2:
		return protocol.EndpointSnapshot{}, errors.New("storage unavailable")
	default:
		return protocol.EndpointSnapshot{}, nil
	}
}

func TestCoordinatorReadinessTracksSnapshotAndStorageRecovery(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	s := &statusStore{}
	done := make(chan error, 1)
	go func() { done <- serveStatus(ctx, addr, s) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("status server did not stop")
		}
	})
	client := &http.Client{Timeout: time.Second}
	check := func(path string, want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			response, err := client.Get("http://" + addr + path)
			if err != nil {
				if time.Now().After(deadline) {
					t.Fatal(err)
				}
				time.Sleep(5 * time.Millisecond)
				continue
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != want {
				t.Fatalf("%s: status=%d want=%d", path, response.StatusCode, want)
			}
			return
		}
	}
	check("/readyz", 503)
	check("/healthz", 200)
	s.state.Store(1)
	check("/readyz", 200)
	s.state.Store(2)
	check("/readyz", 503)
	check("/healthz", 200)
	s.state.Store(1)
	check("/readyz", 200)
}
