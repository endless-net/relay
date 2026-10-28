//go:build integration && load

package relay

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/endless-net/relay/internal/budget"
	wire "github.com/endless-net/relay/internal/wire"
)

// TestRelayCapacity is an opt-in product fixture, not real Client/control-plane E2E.
// Duration defaults to the accepted 30-minute window; short runs are diagnostic only.
func TestRelayCapacity(t *testing.T) {
	duration := 30 * time.Minute
	if raw := os.Getenv("RELAY_LOAD_DURATION"); raw != "" {
		parsed, e := time.ParseDuration(raw)
		if e != nil || parsed <= 0 {
			t.Fatal("invalid load duration")
		}
		duration = parsed
	}
	count := 1000
	if raw := os.Getenv("RELAY_LOAD_SESSIONS"); raw != "" {
		parsed, e := strconv.Atoi(raw)
		if e != nil || parsed < 2 || parsed > 1000 || parsed%2 != 0 {
			t.Fatal("invalid session count")
		}
		count = parsed
	}
	server, roots, key, _, cancel, done := startRelayForTest(t, 5*time.Second)
	defer stopRelayForTest(t, cancel, done)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	type client struct {
		conn net.Conn
		mu   sync.Mutex
	}
	clients := make([]*client, 0, count)
	var workers sync.WaitGroup
	var received, sent, failures atomic.Int64
	var stopping atomic.Bool
	var histogram [1001]atomic.Int64
	cleanup := func() {
		stopping.Store(true)
		stop()
		for _, c := range clients {
			_ = c.conn.Close()
		}
		workers.Wait()
	}
	defer cleanup()
	for i := 0; i < count; i++ {
		connection, reader := authenticateRelayForTest(t, server.Addr, roots, key, "capacity", fmt.Sprintf("node-%d", i), 10000)
		c := &client{conn: connection}
		clients = append(clients, c)
		workers.Go(func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					c.mu.Lock()
					e := json.NewEncoder(c.conn).Encode(wire.Heartbeat{Type: wire.MessageHeartbeat, ProtocolVersion: 1, Time: time.Now().UTC().Format(time.RFC3339Nano)})
					c.mu.Unlock()
					if e != nil {
						if !stopping.Load() {
							failures.Add(1)
						}
						return
					}
				}
			}
		})
		workers.Go(func() {
			decoder := json.NewDecoder(reader)
			for {
				var raw json.RawMessage
				if e := decoder.Decode(&raw); e != nil {
					if !stopping.Load() {
						failures.Add(1)
					}
					return
				}
				var kind struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(raw, &kind) != nil {
					failures.Add(1)
					return
				}
				if kind.Type == wire.MessageError {
					failures.Add(1)
					continue
				}
				if kind.Type != wire.MessageServerFrame {
					continue
				}
				var frame wire.ServerFrame
				if e := json.Unmarshal(raw, &frame); e != nil || len(frame.Payload) != 1200 {
					failures.Add(1)
					continue
				}
				stamp := int64(binary.BigEndian.Uint64(frame.Payload[:8]))
				elapsed := time.Since(time.Unix(0, stamp)).Milliseconds()
				bucket := max(int64(0), min(int64(1000), elapsed))
				histogram[bucket].Add(1)
				received.Add(1)
			}
		})
		// The accepted per-IP handshake rate is 10/s; ramp below it rather than bypassing admission.
		time.Sleep(125 * time.Millisecond)
		if (i+1)%100 == 0 {
			t.Logf("ramp sessions=%d", i+1)
		}
	}
	t.Log("connection warmup 30s")
	time.Sleep(30 * time.Second)
	t.Logf("steady begin sessions=%d duration=%s GOMAXPROCS=%d", count, duration, runtime.GOMAXPROCS(0))
	limiter := budget.NewRate(12500000, 125000)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	report := time.NewTicker(time.Minute)
	defer report.Stop()
	started := time.Now()
	end := time.NewTimer(duration)
	defer end.Stop()
	sample := time.NewTicker(time.Second)
	defer sample.Stop()
	var maxRSS int64
	index := 0
	rss := func() int64 {
		raw, e := os.ReadFile("/proc/self/statm")
		if e != nil {
			return 0
		}
		fields := strings.Fields(string(raw))
		if len(fields) < 2 {
			return 0
		}
		pages, _ := strconv.ParseInt(fields[1], 10, 64)
		return pages * int64(os.Getpagesize())
	}
loop:
	for {
		select {
		case <-end.C:
			break loop
		case <-sample.C:
			maxRSS = max(maxRSS, rss())
		case <-report.C:
			maxRSS = max(maxRSS, rss())
			t.Logf("elapsed=%s sent=%d received=%d failures=%d RSS=%d", time.Since(started).Round(time.Second), sent.Load(), received.Load(), failures.Load(), maxRSS)
		case <-ticker.C:
			for limiter.Allow(1200) {
				i := (index % (count / 2)) * 2
				index++
				payload := make([]byte, 1200)
				binary.BigEndian.PutUint64(payload, uint64(time.Now().UnixNano()))
				frame := wire.ClientFrame{Type: wire.MessageClientFrame, ProtocolVersion: 1, PeerID: fmt.Sprintf("node-%d", i+1), PeerNetworkID: "capacity", Payload: payload}
				c := clients[i]
				c.mu.Lock()
				e := json.NewEncoder(c.conn).Encode(frame)
				c.mu.Unlock()
				if e != nil {
					t.Fatal("load write failed")
				}
				sent.Add(1)
			}
		}
	}
	elapsed := time.Since(started)
	drain := time.Now().Add(5 * time.Second)
	for received.Load() < sent.Load() && time.Now().Before(drain) {
		time.Sleep(10 * time.Millisecond)
	}
	maxRSS = max(maxRSS, rss())
	rate := float64(sent.Load()*1200*8) / elapsed.Seconds() / 1e6
	var cumulative int64
	p95 := 1000
	for ms := range histogram {
		cumulative += histogram[ms].Load()
		if cumulative*100 >= received.Load()*95 {
			p95 = ms
			break
		}
	}
	t.Logf("RESULT sessions=%d duration=%s sent=%d received=%d payload_Mbit_s=%.3f p95_ms=%d RSS=%d failures=%d", count, elapsed, sent.Load(), received.Load(), rate, p95, maxRSS, failures.Load())
	if failures.Load() != 0 || received.Load() != sent.Load() {
		t.Fatal("loss or session failure")
	}
	if rate < 99 {
		t.Fatal("payload throughput below target")
	}
	if p95 > 50 {
		t.Fatal("latency target missed")
	}
	if maxRSS > 1<<30 {
		t.Fatal("RSS target exceeded")
	}
}
