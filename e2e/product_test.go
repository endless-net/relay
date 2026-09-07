//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func setUpstream(t *testing.T, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	address, err := suite.port(context.Background(), "upstream", 9447)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, "https://"+address+"/test/control", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := suite.mutualHTTPClient(suite.certificates["e2e-client"], "upstream").Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("upstream control status %d", resp.StatusCode)
	}
}

func productClients(t *testing.T, local bool) (*relayClient, *relayClient) {
	t.Helper()
	target := "relay-b"
	if local {
		target = "relay-a"
	}
	a := dialRelayClientEventually(t, "relay-a", "node-a", 15*time.Second)
	b := dialRelayClientEventually(t, target, "node-b", 15*time.Second)
	t.Cleanup(a.close)
	t.Cleanup(b.close)
	waitForTransfer(t, a, b, 20*time.Second)
	return a, b
}

func TestProductProtocol(t *testing.T) {
	t.Run("strict_protocol_and_isolation", func(t *testing.T) {
		a, b := productClients(t, true)
		testSecurityContracts(t, map[string]*relayClient{"node-a": a, "node-b": b})
		assertTransfer(t, a, b, bytes.Repeat([]byte{0x83}, protocolv1.MaxFramePayloadBytes))
		assertTransfer(t, b, a, []byte("reverse-local"))
	})
	t.Run("malformed_and_oversized_input", func(t *testing.T) {
		address, err := suite.port(context.Background(), "relay-a", 9443)
		if err != nil {
			t.Fatal(err)
		}
		plaintext, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		plaintext.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = io.WriteString(plaintext, "{\"type\":\"client_hello\"}\n")
		response := make([]byte, 64)
		n, _ := plaintext.Read(response)
		plaintext.Close()
		if bytes.Contains(response[:n], []byte("ready")) {
			t.Fatal("plaintext accepted")
		}
		for _, raw := range []string{"{invalid\n", strings.Repeat("x", 200000) + "\n"} {
			conn, err := tls.Dial("tcp", address, &tls.Config{RootCAs: suite.publicRoots, ServerName: "relay-a", MinVersion: tls.VersionTLS13})
			if err != nil {
				t.Fatal(err)
			}
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			_, _ = io.WriteString(conn, raw)
			var reply map[string]any
			err = json.NewDecoder(conn).Decode(&reply)
			conn.Close()
			if err == nil && reply["type"] == protocolv1.MessageReady {
				t.Fatal("malformed input accepted")
			}
		}
		a, b := productClients(t, true)
		if err := a.send("node-b", bytes.Repeat([]byte{1}, protocolv1.MaxFramePayloadBytes+1)); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			event := waitEvent(t, a, time.Until(deadline))
			if event.relayErr != nil {
				return
			}
			if event.closed != nil {
				return
			}
			if event.frame != nil {
				t.Fatal("oversized payload returned")
			}
		}
		_ = b
		t.Fatal("oversized payload not rejected")
	})
	t.Run("credential_rejections", func(t *testing.T) {
		for _, kind := range []string{"signature", "expired", "unknown_key", "inactive", "network"} {
			t.Run(kind, func(t *testing.T) {
				expected := "invalid relay credential"
				assertRawHelloRejected(t, "relay-a", "node-a", func(m map[string]any) {
					credential := m["credential"].(*protocolv1.Credential)
					switch kind {
					case "signature":
						credential.Signature = strings.Repeat("A", len(credential.Signature))
					case "expired":
						credential.ExpiresAt = time.Now().Add(-time.Second)
					case "unknown_key":
						_, key, err := ed25519.GenerateKey(rand.Reader)
						if err != nil {
							t.Fatal(err)
						}
						c, err := protocolv1.Sign(key, testNetworkID, "node-a", time.Now().Add(time.Minute))
						if err != nil {
							t.Fatal(err)
						}
						m["credential"] = c
					case "inactive", "network":
						network, node := testNetworkID, "inactive-node"
						if kind == "network" {
							network, node = "other-network", "node-a"
						}
						c, err := protocolv1.Sign(suite.signingKey, network, node, time.Now().Add(time.Minute))
						if err != nil {
							t.Fatal(err)
						}
						m["credential"] = c
					}
				}, func() string {
					if kind == "inactive" || kind == "network" {
						return "relay coordinator rejected session"
					}
					return expected
				}())
			})
		}
	})
	t.Run("upstream_stale_window_and_recovery", func(t *testing.T) {
		a, b := productClients(t, false)
		setUpstream(t, map[string]any{"mode": "unavailable"})
		t.Cleanup(func() { setUpstream(t, map[string]any{"mode": ""}) })
		assertTransfer(t, a, b, []byte("fresh-cached-pair"))
		waitForClientClose(t, a, 45*time.Second)
		setUpstream(t, map[string]any{"mode": ""})
		for _, id := range []string{"relay-a", "relay-b", "relay-c"} {
			if err := suite.recreateRelay(id, "recovered-"+id); err != nil {
				t.Fatal(err)
			}
		}
		c, d := productClients(t, false)
		assertTransfer(t, c, d, []byte("recovered-upstream"))
	})
}

func TestProductSPIFFE(t *testing.T) {
	address, err := suite.port(context.Background(), "relay-b", 9444)
	if err != nil {
		t.Fatal(err)
	}
	serial := func() string {
		conn, err := tls.Dial("tcp", address, suite.internalClientTLS(suite.certificates["relay-a"], "relay-b"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		state := conn.ConnectionState()
		if len(state.VerifiedChains) != 0 {
			t.Fatal("test must exercise SPIFFE custom verification")
		}
		return state.PeerCertificates[0].SerialNumber.String()
	}
	old := serial()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if serial() != old {
			return
		}
		waitPoll(deadline)
	}
	t.Fatal("SPIRE did not rotate workload SVID within 90 seconds")
}

func controlClient(t *testing.T) relayv1.RelayControlClient {
	t.Helper()
	address, err := suite.port(context.Background(), "relay-coordinator", 9445)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(suite.internalClientTLS(suite.certificates["relay-a"], "relay-coordinator"))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return relayv1.NewRelayControlClient(conn)
}

func TestProductFencing(t *testing.T) {
	t.Run("postgres_release_reacquire", func(t *testing.T) {
		client := controlClient(t)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		signed, err := protocolv1.Sign(suite.signingKey, testNetworkID, "node-a", time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		credential := relayv1.CredentialFromProtocol(*signed)
		acquire := func() int64 {
			response, err := client.AcquireSession(ctx, &relayv1.AcquireSessionRequest{RelayId: "relay-a", BootId: suite.bootIDs["relay-a"], Credential: credential})
			if err != nil {
				t.Fatal(err)
			}
			return response.Epoch
		}
		first := acquire()
		request := &relayv1.ReleaseSessionRequest{RelayId: "relay-a", BootId: suite.bootIDs["relay-a"], NetworkId: testNetworkID, NodeId: "node-a", Epoch: first}
		if _, err = client.ReleaseSession(ctx, request); err != nil {
			t.Fatal(err)
		}
		second := acquire()
		if second <= first {
			t.Fatal("epoch reused after release")
		}
		if _, err = client.ReleaseSession(ctx, request); err != nil {
			t.Fatal(err)
		}
		if _, err = client.RenewSession(ctx, &relayv1.RenewSessionRequest{RelayId: "relay-a", BootId: suite.bootIDs["relay-a"], NetworkId: testNetworkID, NodeId: "node-a", Epoch: second, Credential: credential}); err != nil {
			t.Fatalf("old release affected new owner: %v", err)
		}
		if _, err = suite.postgresQuery(ctx, "UPDATE node_session_leases SET lease_expires_at=now()-interval '1 second' WHERE node_id='node-a'"); err != nil {
			t.Fatal(err)
		}
		if _, err = client.RenewSession(ctx, &relayv1.RenewSessionRequest{RelayId: "relay-a", BootId: suite.bootIDs["relay-a"], NetworkId: testNetworkID, NodeId: "node-a", Epoch: second, Credential: credential}); err == nil {
			t.Fatal("expired session renewed")
		}
	})
	t.Run("postgres_outage_recovery", func(t *testing.T) {
		a, b := productClients(t, false)
		if _, err := suite.compose(context.Background(), "pause", "postgres"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = suite.compose(context.Background(), "unpause", "postgres") })
		waitForClientClose(t, a, 15*time.Second)
		waitForClientClose(t, b, 15*time.Second)
		if _, err := suite.compose(context.Background(), "unpause", "postgres"); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"relay-a", "relay-b", "relay-c"} {
			if err := suite.recreateRelay(id, "database-recovery-"+id); err != nil {
				t.Fatal(err)
			}
		}
		c, d := productClients(t, false)
		assertTransfer(t, c, d, []byte("database-recovery"))
	})
	t.Run("stalled_control_fences_process", func(t *testing.T) {
		a, b := productClients(t, false)
		if _, err := suite.compose(context.Background(), "pause", "relay-coordinator"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = suite.compose(context.Background(), "unpause", "relay-coordinator") })
		waitForClientClose(t, a, 15*time.Second)
		waitForClientClose(t, b, 15*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if err := suite.waitFor(ctx, "instance process fencing", func() error {
			raw, err := suite.compose(ctx, "ps", "--status", "running", "--services")
			if err != nil {
				return err
			}
			for _, id := range []string{"relay-a", "relay-b", "relay-c"} {
				if hasService(raw, id) {
					return fmt.Errorf("instance still running")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := suite.compose(context.Background(), "unpause", "relay-coordinator"); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"relay-a", "relay-b", "relay-c"} {
			if err := suite.recreateRelay(id, "long-outage-"+id); err != nil {
				t.Fatal(err)
			}
		}
		recoveredA, recoveredB := productClients(t, false)
		assertTransfer(t, recoveredA, recoveredB, []byte("long-outage-recovery"))

	})
}

func TestProductResources(t *testing.T) {
	for _, local := range []bool{true, false} {
		t.Run(fmt.Sprintf("slow_consumer_local_%v", local), func(t *testing.T) {
			target := "relay-b"
			if local {
				target = "relay-a"
			}
			a := dialRelayClientEventually(t, "relay-a", "node-a", 15*time.Second)
			defer a.close()
			b, err := suite.dialRelayClientReading(target, "node-b", false)
			if err != nil {
				t.Fatal(err)
			}
			defer b.close()
			if tcp, ok := b.conn.NetConn().(*net.TCPConn); ok {
				if err := tcp.SetReadBuffer(1024); err != nil {
					t.Fatal(err)
				}
			}
			before := metricValue(t, target, "endlessnet_relay_slow_consumers_total")
			deadline := time.Now().Add(20 * time.Second)
			payload := bytes.Repeat([]byte{0x81}, protocolv1.MaxFramePayloadBytes)
			for time.Now().Before(deadline) {
				for i := 0; i < 32; i++ {
					if err := a.send("node-b", payload); err != nil {
						t.Fatal(err)
					}
					drainEvents(a)
				}
				if metricValue(t, target, "endlessnet_relay_slow_consumers_total") > before {
					b.close()
					recovered := dialRelayClientEventually(t, target, "node-b", 15*time.Second)
					defer recovered.close()
					waitForTransfer(t, a, recovered, 20*time.Second)
					return
				}
			}
			t.Fatal("stalled destination was not closed and counted")
		})
	}
	t.Run("mesh_partition_overflow_and_reconnect", func(t *testing.T) {
		a, b := productClients(t, false)
		c := dialRelayClientEventually(t, "relay-c", "node-c", 15*time.Second)
		defer c.close()
		waitForTransfer(t, a, c, 20*time.Second)
		before := metricValue(t, "relay-a", `endlessnet_relay_drops_total{reason="mesh"}`)
		proxyMode(t, "relay-b", "stall")
		t.Cleanup(func() { proxyMode(t, "relay-b", "") })
		deadline := time.Now().Add(20 * time.Second)
		overflow := false
		payload := bytes.Repeat([]byte{3}, protocolv1.MaxFramePayloadBytes)
		for time.Now().Before(deadline) {
			for i := 0; i < 32; i++ {
				if err := a.send("node-b", payload); err != nil {
					t.Fatal(err)
				}
				drainEvents(a)
				drainEvents(b)
			}
			if metricValue(t, "relay-a", `endlessnet_relay_drops_total{reason="mesh"}`) > before {
				overflow = true
				break
			}
		}
		if !overflow {
			t.Fatal("stalled mesh did not reach bounded outbound capacity")
		}
		assertTransfer(t, a, c, []byte("other-peer-remains-available"))
		proxyMode(t, "relay-b", "disconnect")
		proxyMode(t, "relay-b", "")
		// Backpressure can close the destination when buffered frames are released.
		b.close()
		b = dialRelayClientEventually(t, "relay-b", "node-b", 15*time.Second)
		defer b.close()
		waitForTransfer(t, a, b, 30*time.Second)
	})
	for _, limits := range []struct {
		name                       string
		global, auth, source, held int
	}{
		{"global_limit", 4, 4, 4, 4}, {"source_limit", 8, 8, 2, 2}, {"auth_limit", 8, 2, 8, 2},
	} {
		t.Run(limits.name, func(t *testing.T) {
			t.Setenv("E2E_MAX_CONNECTIONS", strconv.Itoa(limits.global))
			t.Setenv("E2E_MAX_AUTH", strconv.Itoa(limits.auth))
			t.Setenv("E2E_MAX_SOURCE", strconv.Itoa(limits.source))
			if err := suite.recreateRelay("relay-c", "limit-"+limits.name); err != nil {
				t.Fatal(err)
			}
			address, err := suite.port(context.Background(), "relay-c", 9443)
			if err != nil {
				t.Fatal(err)
			}
			connections := []net.Conn{}
			defer func() {
				for _, c := range connections {
					c.Close()
				}
			}()
			for i := 0; i < limits.held; i++ {
				c, err := net.DialTimeout("tcp", address, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				connections = append(connections, c)
			}
			deadline := time.Now().Add(3 * time.Second)
			for metricValue(t, "relay-c", "endlessnet_relay_auth_active") < float64(limits.held) {
				if !time.Now().Before(deadline) {
					t.Fatal("handshakes not admitted")
				}
				waitPoll(deadline)
			}
			c, err := net.DialTimeout("tcp", address, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			name := `endlessnet_relay_connection_rejections_total{reason="` + limits.name + `"}`
			for metricValue(t, "relay-c", name) == 0 {
				if !time.Now().Before(deadline) {
					t.Fatal("limit not enforced")
				}
				waitPoll(deadline)
			}
			deadline = time.Now().Add(12 * time.Second)
			for metricValue(t, "relay-c", "endlessnet_relay_auth_active") != 0 {
				if !time.Now().Before(deadline) {
					t.Fatal("stalled handshake exceeded deadline")
				}
				waitPoll(deadline)
			}
			recovered := dialRelayClientEventually(t, "relay-c", "node-c", 10*time.Second)
			recovered.close()
		})
	}
	t.Run("bandwidth_limit_and_recovery", func(t *testing.T) {
		t.Setenv("E2E_BANDWIDTH", "1024")
		if err := suite.recreateRelay("relay-c", "bandwidth"); err != nil {
			t.Fatal(err)
		}
		a := dialRelayClientEventually(t, "relay-c", "node-c", 15*time.Second)
		defer a.close()
		b := dialRelayClientEventually(t, "relay-c", "node-d", 15*time.Second)
		defer b.close()
		assertTransfer(t, a, b, []byte("within-limit"))
		if err := a.send("node-d", bytes.Repeat([]byte{1}, 1025)); err != nil {
			t.Fatal(err)
		}
		event := waitEvent(t, a, 3*time.Second)
		if event.relayErr == nil || event.relayErr.Error != "relay bandwidth limit exceeded" {
			t.Fatal("bandwidth overflow not rejected")
		}
		waitForClientClose(t, a, 3*time.Second)
		recovered := dialRelayClientEventually(t, "relay-c", "node-c", 10*time.Second)
		defer recovered.close()
		assertTransfer(t, recovered, b, []byte("still-available"))
	})
	t.Run("bounded_sigterm", func(t *testing.T) {
		a, b := productClients(t, false)
		start := time.Now()
		if err := suite.stopService("relay-b"); err != nil {
			t.Fatal(err)
		}
		waitForClientClose(t, b, 5*time.Second)
		if time.Since(start) > 10*time.Second {
			t.Fatal("shutdown exceeded bound")
		}
		if err := suite.recreateRelay("relay-b", "resource-restart"); err != nil {
			t.Fatal(err)
		}
		b = dialRelayClientEventually(t, "relay-b", "node-b", 15*time.Second)
		defer b.close()
		waitForTransfer(t, a, b, 20*time.Second)
	})
}

func TestProductExtended(t *testing.T) {
	if os.Getenv("E2E_EXTENDED") != "1" {
		t.Fatal("extended run requires E2E_EXTENDED=1")
	}
	deadline := time.Now().Add(30 * time.Minute)
	for cycle := 0; time.Now().Before(deadline); cycle++ {
		a := dialRelayClientEventually(t, "relay-a", "node-a", 15*time.Second)
		b := dialRelayClientEventually(t, "relay-b", "node-b", 15*time.Second)
		waitForTransfer(t, a, b, 20*time.Second)
		assertTransfer(t, b, a, []byte("soak"))
		a.close()
		b.close()
		if err := suite.recreateRelay("relay-b", "soak-"+strconv.Itoa(cycle)); err != nil {
			t.Fatal(err)
		}
		if cycle%10 == 0 {
			if err := suite.stopService("relay-coordinator"); err != nil {
				t.Fatal(err)
			}
			if err := suite.startService("relay-coordinator"); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("completed recovery cycle %d", cycle)
	}
}

func metricValue(t *testing.T, relayID, name string) float64 {
	t.Helper()
	for _, line := range strings.Split(fetchMetrics(t, relayID), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == name {
			v, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("missing metric %s", name)
	return 0
}

func hasService(output, service string) bool {
	for _, name := range strings.Fields(output) {
		if name == service {
			return true
		}
	}
	return false
}

func upstreamConfig(bundle protocolv1.SigningTrustBundle) map[string]any {
	nodes := []string{"node-a", "node-b", "node-c", "node-d"}
	pairs := []map[string]string{}
	for _, a := range nodes {
		for _, b := range nodes {
			if a != b {
				pairs = append(pairs, map[string]string{"from": a, "to": b})
			}
		}
	}
	return map[string]any{"network_id": testNetworkID, "nodes": nodes, "peer_pairs": pairs, "relay_trust_bundle": bundle}
}
func TestProductTrustRotation(t *testing.T) {
	oldKey := suite.signingKey
	oldBundle, err := protocolv1.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(oldKey.Public().(ed25519.PublicKey)))
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newBundle, err := protocolv1.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { suite.signingKey = oldKey; setUpstream(t, map[string]any{"config": upstreamConfig(oldBundle)}) })
	overlap := newBundle
	overlap.Keys = append(overlap.Keys, oldBundle.Keys...)
	setUpstream(t, map[string]any{"config": upstreamConfig(overlap)})
	suite.signingKey = key
	client := dialRelayClientEventually(t, "relay-a", "node-a", 20*time.Second)
	client.close()
	suite.signingKey = oldKey
	client = dialRelayClientEventually(t, "relay-a", "node-a", 20*time.Second)
	client.close()
	setUpstream(t, map[string]any{"config": upstreamConfig(newBundle)})
	deadline := time.Now().Add(20 * time.Second)
	retired := false
	for time.Now().Before(deadline) {
		client, err := suite.dialRelayClient("relay-a", "node-a")
		if err != nil {
			retired = true
			break
		}
		client.close()
		waitPoll(deadline)
	}
	if !retired {
		t.Fatal("retired key remained accepted")
	}
	suite.signingKey = key
	a, b := productClients(t, false)
	assertTransfer(t, a, b, []byte("rotation"))
}
func TestProductSnapshotIdentity(t *testing.T) {
	address, err := suite.port(context.Background(), "relay-coordinator", 7078)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"e2e-client", "relay-a", "relay-coordinator"} {
		resp, err := suite.mutualHTTPClient(suite.certificates[name], "relay-coordinator").Get("https://" + address + "/internal/relay-control/v1/endpoints")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if name == "e2e-client" && resp.StatusCode != 200 {
			t.Fatalf("exact caller rejected: %d", resp.StatusCode)
		}
		if name != "e2e-client" && resp.StatusCode == 200 {
			t.Fatalf("wrong snapshot caller accepted: %s", name)
		}
	}
}

func proxyMode(t *testing.T, target, mode string) {
	t.Helper()
	address, err := suite.port(context.Background(), "fault-proxy", 9440)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"target": target, "mode": mode})
	req, err := http.NewRequest(http.MethodPut, "http://"+address+"/control", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("proxy control: %d", resp.StatusCode)
	}
}

func TestProductSPIREOutage(t *testing.T) {
	a, b := productClients(t, false)
	before := metricValue(t, "relay-a", "endlessnet_relay_heartbeats_total")
	if _, err := suite.compose(context.Background(), "pause", "spire-server"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = suite.compose(context.Background(), "unpause", "spire-server") })
	deadline := time.Now().Add(12 * time.Second)
	for metricValue(t, "relay-a", "endlessnet_relay_heartbeats_total") <= before {
		assertTransfer(t, a, b, []byte("cached-svid-during-spire-loss"))
		if !time.Now().Before(deadline) {
			t.Fatal("relay stopped heartbeat during SPIRE loss")
		}
		waitPoll(deadline)
	}
	if _, err := suite.compose(context.Background(), "unpause", "spire-server"); err != nil {
		t.Fatal(err)
	}
	assertTransfer(t, b, a, []byte("spire-recovered"))
}

func TestProductMeshFencing(t *testing.T) {
	a, b := productClients(t, false)
	address, err := suite.port(context.Background(), "relay-b", 9444)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []string{"relay-a", "wrong-domain"} {
		conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(suite.internalClientTLS(suite.certificates[identity], "relay-b"))))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		stream, err := relayv1.NewRelayMeshClient(conn).Connect(ctx)
		if err == nil {
			err = stream.Send(&relayv1.MeshMessage{ProtocolVersion: 1, Body: &relayv1.MeshMessage_Hello{Hello: &relayv1.MeshHello{RelayId: "relay-a", BootId: "obsolete-boot"}}})
		}
		if err == nil {
			_, err = stream.Recv()
		}
		cancel()
		conn.Close()
		if err == nil {
			t.Fatalf("stale boot or wrong domain accepted: %s", identity)
		}
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(suite.internalClientTLS(suite.certificates["relay-a"], "relay-b"))))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := relayv1.NewRelayMeshClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hello := &relayv1.MeshMessage{ProtocolVersion: 1, Body: &relayv1.MeshMessage_Hello{Hello: &relayv1.MeshHello{RelayId: "relay-a", BootId: suite.bootIDs["relay-a"]}}}
	if err = stream.Send(hello); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != nil {
		t.Fatal(err)
	}
	state, err := suite.postgresQuery(ctx, "SELECT epoch FROM node_session_leases WHERE network_id='e2e-network' AND node_id='node-b'")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(state), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	b.close()
	b = dialRelayClientEventually(t, "relay-b", "node-b", 15*time.Second)
	defer b.close()
	payload := []byte("stale-epoch-frame")
	frame := &relayv1.MeshMessage{ProtocolVersion: 1, Body: &relayv1.MeshMessage_Frame{Frame: &relayv1.MeshFrame{RelayId: "relay-a", BootId: suite.bootIDs["relay-a"], NetworkId: testNetworkID, FromNodeId: "node-a", ToNodeId: "node-b", DestinationEpoch: epoch, Payload: payload}}}
	if err = stream.Send(frame); err != nil {
		t.Fatal(err)
	}
	assertNoPayload(t, b, payload, time.Second)
	waitForTransfer(t, a, b, 20*time.Second)
	if err = suite.recreateRelay("relay-a", "mesh-new-boot"); err != nil {
		t.Fatal(err)
	}
	// Registry propagation must close even an idle inbound stream of the old boot.
	if _, err = stream.Recv(); err == nil || ctx.Err() != nil {
		t.Fatal("old boot stream survived snapshot update")
	}
	recovered := dialRelayClientEventually(t, "relay-a", "node-a", 15*time.Second)
	defer recovered.close()
	waitForTransfer(t, recovered, b, 20*time.Second)
}
func TestProductNoTrustBootstrap(t *testing.T) {
	for _, mode := range []string{"invalid", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			setUpstream(t, map[string]any{"mode": mode})
			t.Cleanup(func() { setUpstream(t, map[string]any{"mode": ""}) })
			if _, err := suite.compose(context.Background(), "up", "-d", "--no-deps", "--force-recreate", "relay-coordinator"); err != nil {
				t.Fatal(err)
			}
			if _, err := suite.compose(context.Background(), "up", "-d", "--no-deps", "--force-recreate", "relay-a"); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(12 * time.Second)
			for {
				raw, err := suite.compose(context.Background(), "ps", "--status", "exited", "--services", "relay-a")
				if err != nil {
					t.Fatal(err)
				}
				if hasService(raw, "relay-a") {
					break
				}
				if !time.Now().Before(deadline) {
					t.Fatal("Relay started without usable trust")
				}
				waitPoll(deadline)
			}
			setUpstream(t, map[string]any{"mode": ""})
			for _, id := range []string{"relay-a", "relay-b", "relay-c"} {
				if err := suite.recreateRelay(id, mode+"-trust-recovery-"+id); err != nil {
					t.Fatal(err)
				}
			}
			a, b := productClients(t, false)
			assertTransfer(t, a, b, []byte("trust-bootstrap-recovery"))
		})
	}
}

func TestProductSnapshotPersistence(t *testing.T) {
	path := filepath.Join(suite.fixturesDir, "endpoints.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var initial protocolv1.EndpointSnapshot
	if err = json.Unmarshal(raw, &initial); err != nil {
		t.Fatal(err)
	}
	restart := func(raw []byte, wantReady bool) {
		if err := os.WriteFile(path, raw, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := suite.compose(context.Background(), "up", "-d", "--no-deps", "--force-recreate", "relay-coordinator"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if wantReady {
			if err := suite.waitForHTTPS(ctx, "relay-coordinator", 7078, "/readyz", "relay-coordinator", suite.certificates["e2e-client"], 200); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err := suite.waitFor(ctx, "rejection of invalid snapshot", func() error {
			raw, err := suite.compose(ctx, "ps", "--status", "exited", "--services", "relay-coordinator")
			if err != nil {
				return err
			}
			if !hasService(raw, "relay-coordinator") {
				return fmt.Errorf("process has not rejected snapshot")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	marshal := func(v protocolv1.EndpointSnapshot) []byte {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	changed := initial
	changed.Endpoints = append([]protocolv1.Endpoint(nil), initial.Endpoints...)
	changed.Endpoints[0].Addr = "changed:9443"
	restart(marshal(changed), false)
	newer := initial
	newer.Version = initial.Version + 1
	restart(marshal(newer), true)
	restart(raw, false)
	restart([]byte("{invalid"), false)
	empty := protocolv1.EndpointSnapshot{Version: newer.Version + 1, Endpoints: []protocolv1.Endpoint{}}
	restart(marshal(empty), true)
	restart(marshal(empty), true)
	address, err := suite.port(context.Background(), "relay-coordinator", 7078)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := suite.mutualHTTPClient(suite.certificates["e2e-client"], "relay-coordinator").Get("https://" + address + "/internal/relay-control/v1/endpoints")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got protocolv1.EndpointSnapshot
	if err = json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Version != empty.Version || len(got.Endpoints) != 0 {
		t.Fatal("empty snapshot did not persist across restart")
	}
}
