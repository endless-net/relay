package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/endless-net/relay/internal/relay"
	"github.com/endless-net/relay/internal/relaycontrol"
)

func TestMetricsReadinessLifecycle(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var ready atomic.Bool
	done := make(chan error, 1)
	go func() { done <- serveMetrics(ctx, addr, relay.NewMetrics(), ready.Load) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("metrics server did not stop")
		}
	})
	client := &http.Client{Timeout: time.Second}
	check := func(path string, want int, text string) {
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
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != want || !strings.Contains(string(body), text) {
				t.Fatalf("%s: status=%d error=%v", path, response.StatusCode, err)
			}
			return
		}
	}
	check("/healthz", 200, "ok")
	check("/readyz", 503, "not ready")
	ready.Store(true)
	check("/readyz", 200, "ready")
	check("/metrics", 200, "endlessnet_relay_connections")
	ready.Store(false)
	check("/readyz", 503, "not ready")
	check("/healthz", 200, "ok")
}

func TestTrustBootstrapCancellationAndTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitForTrustBundle(ctx, &relaycontrol.Client{}, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	if err := waitForTrustBundle(t.Context(), &relaycontrol.Client{}, time.Millisecond); err == nil {
		t.Fatal("missing trust accepted")
	}
}

func TestBootIDsAndEnvironmentDefaults(t *testing.T) {
	a, b := randomID(), randomID()
	if len(a) != 32 || len(b) != 32 || a == b {
		t.Fatal("boot IDs are not distinct 128-bit identifiers")
	}
	t.Setenv("RELAY_TEST_CONFIG", "  ")
	if env("RELAY_TEST_CONFIG", "fallback") != "fallback" {
		t.Fatal("blank environment did not use default")
	}
	t.Setenv("RELAY_TEST_CONFIG", " value ")
	if env("RELAY_TEST_CONFIG", "fallback") != "value" {
		t.Fatal("environment override not canonicalized")
	}
}
