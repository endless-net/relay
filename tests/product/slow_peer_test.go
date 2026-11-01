//go:build product

package product

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/endless-net/relay/internal/wire"
)

func TestRuntimeSlowPeerDoesNotBlockUnrelatedPair(t *testing.T) {
	f := newFixture(t)
	sender, senderReader := f.client(t, 0, 0)
	defer func() { _ = sender.Close() }()
	slow, _ := f.client(t, 0, 1)
	defer func() { _ = slow.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var rejected atomic.Int64
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			var message struct {
				Type  string `json:"type"`
				Error string `json:"error"`
			}
			if senderReader.Decode(&message) != nil {
				return
			}
			if message.Type == wire.MessageError && message.Error == "peer_queue_full" {
				rejected.Add(1)
			}
			if ctx.Err() != nil {
				return
			}
		}
	}()
	defer func() { cancel(); _ = sender.Close(); <-readerDone }()
	// The receiver stops reading after hello. This exceeds TCP buffering while
	// offering at most ~87 Mbit/s decoded payload, below the100Mbit/s cap.
	frame := wire.ClientFrame{Type: wire.MessageClientFrame, ProtocolVersion: 1, PeerNetworkID: "fixture", PeerID: "node-1", Payload: make([]byte, 65536)}
	for range 100 {
		_ = sender.SetWriteDeadline(time.Now().Add(time.Second))
		if err := json.NewEncoder(sender).Encode(frame); err != nil {
			t.Fatal("slow destination blocked source session", err)
		}
		time.Sleep(6 * time.Millisecond)
	}
	deadline := time.Now().Add(3 * time.Second)
	for rejected.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rejected.Load() == 0 {
		t.Fatal("slow peer did not exercise bounded queue rejection")
	}
	client := &http.Client{Timeout: time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+f.diagnostics[0]+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 65536))
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "endlessnet_relay_drops_total{reason=\"slow_consumer\"} 0\n") {
		t.Fatal("slow consumer rejection not observed in metrics")
	}
	healthyA, _ := f.client(t, 0, 2)
	defer func() { _ = healthyA.Close() }()
	healthyB, reader := f.client(t, 0, 3)
	defer func() { _ = healthyB.Close() }()
	if err = json.NewEncoder(healthyA).Encode(wire.ClientFrame{Type: wire.MessageClientFrame, ProtocolVersion: 1, PeerNetworkID: "fixture", PeerID: "node-3", Payload: []byte("independent-pair")}); err != nil {
		t.Fatal(err)
	}
	_ = healthyB.SetReadDeadline(time.Now().Add(time.Second))
	var delivered wire.ServerFrame
	if err = reader.Decode(&delivered); err != nil || string(delivered.Payload) != "independent-pair" {
		t.Fatal("slow consumer blocked unrelated pair", err)
	}
	waitReady(t, f.ctx, f.diagnostics[0])
	t.Logf("slow receiver caused %d bounded queue rejections; unrelated fresh pair and readiness remained available", rejected.Load())
}
