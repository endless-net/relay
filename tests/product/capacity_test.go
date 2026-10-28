//go:build product && load

package product

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/endless-net/relay/internal/budget"
	wire "github.com/endless-net/relay/internal/wire"
)

// TestRuntimeMeshCapacity keeps 1000 sessions on each real dataplane process.
// Network/Signing authorities are synthetic; production SC-006 is not claimed.
func TestRuntimeMeshCapacity(t *testing.T) {
	duration := 30 * time.Minute
	if raw := os.Getenv("RELAY_PRODUCT_DURATION"); raw != "" {
		parsed, e := time.ParseDuration(raw)
		if e != nil || parsed <= 0 {
			t.Fatal("invalid duration")
		}
		duration = parsed
	}
	count := 1000
	if raw := os.Getenv("RELAY_PRODUCT_SESSIONS"); raw != "" {
		parsed, e := strconv.Atoi(raw)
		if e != nil || parsed < 32 || parsed > 1000 {
			t.Fatal("invalid sessions")
		}
		count = parsed
	}
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type client struct {
		conn net.Conn
		mu   sync.Mutex
	}
	clients := make([]*client, 0, count*2)
	var workers sync.WaitGroup
	var stopping atomic.Bool
	var measuring atomic.Bool
	var sent, received, failures atomic.Int64
	var histogram [1001]atomic.Int64
	defer func() {
		stopping.Store(true)
		cancel()
		for _, c := range clients {
			_ = c.conn.Close()
		}
		workers.Wait()
	}()
	for i := 0; i < count; i++ {
		for role := range 2 {
			connection, reader := f.client(t, role, i*2+role)
			c := &client{conn: connection}
			clients = append(clients, c)
			expected := fmt.Sprintf("node-%d", i*2)
			workers.Go(func() {
				heartbeat := time.NewTicker(10 * time.Second)
				defer heartbeat.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-heartbeat.C:
						c.mu.Lock()
						e := json.NewEncoder(c.conn).Encode(wire.Heartbeat{Type: wire.MessageHeartbeat, ProtocolVersion: 1, Time: time.Now().UTC().Format(time.RFC3339Nano)})
						c.mu.Unlock()
						if e != nil {
							if !stopping.Load() {
								if failures.Add(1) == 1 {
									t.Logf("first heartbeat failure: %v", e)
								}
							}
							return
						}
					}
				}
			})
			workers.Go(func() {
				for {
					var raw json.RawMessage
					if e := reader.Decode(&raw); e != nil {
						if !stopping.Load() && measuring.Load() {
							if failures.Add(1) == 1 {
								t.Logf("first read failure: %v", e)
							}
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
						if measuring.Load() {
							if failures.Add(1) == 1 {
								t.Logf("first runtime rejection: %s", raw)
							}
						}
						continue
					}
					if kind.Type != wire.MessageServerFrame {
						continue
					}
					var frame wire.ServerFrame
					if e := json.Unmarshal(raw, &frame); e != nil || len(frame.Payload) != 1200 || frame.FromNodeID != expected || frame.FromNetworkID != "fixture" {
						failures.Add(1)
						continue
					}
					if frame.Payload[8] == 0 {
						continue
					}
					elapsed := time.Since(time.Unix(0, int64(binary.BigEndian.Uint64(frame.Payload[:8])))).Milliseconds()
					histogram[max(int64(0), min(int64(1000), elapsed))].Add(1)
					received.Add(1)
				}
			})
		}
		time.Sleep(125 * time.Millisecond)
		if (i+1)%100 == 0 {
			t.Logf("ramp sessions_per_dataplane=%d", i+1)
		}
	}
	t.Log("traffic warmup30s")
	limiter := budget.NewRate(12500000, 125000)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	sample := time.NewTicker(time.Second)
	defer sample.Stop()
	report := time.NewTicker(time.Minute)
	defer report.Stop()
	end := time.NewTimer(duration + 30*time.Second)
	defer end.Stop()
	warmStarted := time.Now()
	started := warmStarted.Add(30 * time.Second)
	var peak [2]int64
	index := 0
	rss := func(pid int) int64 {
		raw, e := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
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
	t.Logf("steady begin sessions_per_dataplane=%d duration=%s", count, duration)
loop:
	for {
		select {
		case <-end.C:
			break loop
		case <-sample.C:
			if !measuring.Load() && !time.Now().Before(started) {
				measuring.Store(true)
				t.Log("measured window started")
			}
			for role := range 2 {
				peak[role] = max(peak[role], rss(f.processes[role].command.Process.Pid))
			}
			if failures.Load() != 0 {
				for _, address := range f.diagnostics {
					client := &http.Client{Timeout: time.Second}
					request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/metrics", nil)
					response, err := client.Do(request)
					if err == nil {
						raw, _ := io.ReadAll(io.LimitReader(response.Body, 65536))
						_ = response.Body.Close()
						t.Logf("runtime metrics: %s", raw)
					}
				}
				t.Fatal("runtime error during capacity")
			}
		case <-report.C:
			if !measuring.Load() {
				continue
			}
			t.Logf("elapsed=%s sent=%d received=%d failures=%d RSS_A=%d RSS_B=%d", time.Since(started).Round(time.Second), sent.Load(), received.Load(), failures.Load(), peak[0], peak[1])
		case <-ticker.C:
			if !measuring.Load() && !time.Now().Before(started) {
				measuring.Store(true)
			}
			for limiter.Allow(1200) {
				i := index % count
				index++
				payload := make([]byte, 1200)
				binary.BigEndian.PutUint64(payload, uint64(time.Now().UnixNano()))
				measured := measuring.Load()
				if measured {
					payload[8] = 1
				}
				frame := wire.ClientFrame{Type: wire.MessageClientFrame, ProtocolVersion: 1, PeerNetworkID: "fixture", PeerID: fmt.Sprintf("node-%d", i*2+1), Payload: payload}
				c := clients[i*2]
				c.mu.Lock()
				e := json.NewEncoder(c.conn).Encode(frame)
				c.mu.Unlock()
				if e != nil {
					t.Fatal("load write failed")
				}
				if measured {
					sent.Add(1)
				}
			}
		}
	}
	elapsed := time.Since(started)
	drain := time.Now().Add(5 * time.Second)
	for received.Load() < sent.Load() && time.Now().Before(drain) {
		time.Sleep(10 * time.Millisecond)
	}
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
	t.Logf("RESULT sessions_per_dataplane=%d duration=%s sent=%d received=%d payload_Mbit_s=%.3f p95_ms=%d RSS_A=%d RSS_B=%d failures=%d", count, elapsed, sent.Load(), received.Load(), rate, p95, peak[0], peak[1], failures.Load())
	if failures.Load() != 0 || sent.Load() != received.Load() {
		t.Fatal("loss or session failure")
	}
	if rate < 99 {
		t.Fatal("throughput target missed")
	}
	if p95 > 50 {
		t.Fatal("latency target missed")
	}
	if peak[0] > 1<<30 || peak[1] > 1<<30 {
		t.Fatal("RSS target missed")
	}
}
