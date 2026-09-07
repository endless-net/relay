//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
				if strings.Contains(raw, id+"\n") {
					return fmt.Errorf("instance still running")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestProductResources(t *testing.T) {
	t.Run("local_and_remote_backpressure", func(t *testing.T) {
		for _, local := range []bool{true, false} {
			a, b := productClients(t, local)
			// A large burst exercises bounded socket/queue pressure without retaining payloads.
			for i := 0; i < 32; i++ {
				assertTransfer(t, a, b, bytes.Repeat([]byte{byte(i)}, protocolv1.MaxFramePayloadBytes))
			}
			a.close()
			b.close()
		}
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
