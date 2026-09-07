//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

type relayEvent struct {
	frame     *protocolv1.ServerFrame
	relayErr  *protocolv1.Error
	heartbeat *protocolv1.Heartbeat
	closed    error
}

type relayClient struct {
	relayID string
	nodeID  string
	conn    *tls.Conn
	writer  *bufio.Writer
	events  chan relayEvent
	closed  chan struct{}
	writeMu sync.Mutex
	closeMu sync.Once
}

func TestMultiRelayInfrastructure(t *testing.T) {
	clients := make(map[string]*relayClient)
	defer func() {
		for _, client := range clients {
			client.close()
		}
	}()

	if !t.Run("startup_and_registry", func(t *testing.T) {
		testStartupAndRegistry(t)
	}) {
		t.FailNow()
	}
	if !t.Run("full_mesh_routing", func(t *testing.T) {
		for nodeID, relayID := range map[string]string{"node-a": "relay-a", "node-b": "relay-b", "node-c": "relay-c"} {
			client, err := suite.dialRelayClient(relayID, nodeID)
			if err != nil {
				t.Fatalf("connect %s to %s: %v", nodeID, relayID, err)
			}
			clients[nodeID] = client
		}
		for _, route := range [][2]string{{"node-a", "node-b"}, {"node-b", "node-a"}, {"node-b", "node-c"}, {"node-c", "node-b"}, {"node-c", "node-a"}, {"node-a", "node-c"}} {
			waitForTransfer(t, clients[route[0]], clients[route[1]], 20*time.Second)
		}
		for _, route := range [][2]string{{"node-a", "node-b"}, {"node-b", "node-a"}, {"node-b", "node-c"}, {"node-c", "node-b"}, {"node-c", "node-a"}, {"node-a", "node-c"}} {
			payload := []byte("matrix-" + route[0] + "-to-" + route[1])
			assertTransfer(t, clients[route[0]], clients[route[1]], payload)
		}
		assertMetricsDoNotContain(t, "matrix-node")
	}) {
		t.FailNow()
	}
	if !t.Run("security_contracts", func(t *testing.T) {
		testSecurityContracts(t, clients)
	}) {
		t.FailNow()
	}
	if !t.Run("session_migration_and_fencing", func(t *testing.T) {
		oldClient := clients["node-b"]
		newClient, err := suite.dialRelayClient("relay-c", "node-b")
		if err != nil {
			t.Fatalf("migrate node-b to relay-c: %v", err)
		}
		clients["node-b"] = newClient
		payload := []byte("migration-session")
		waitForTransfer(t, clients["node-a"], newClient, 20*time.Second)
		assertTransfer(t, clients["node-a"], newClient, payload)
		assertNoPayload(t, oldClient, payload, time.Second)
		waitForClientClose(t, oldClient, 8*time.Second)
		state, err := suite.postgresQuery(context.Background(), "SELECT relay_id || ':' || epoch FROM node_session_leases WHERE network_id='e2e-network' AND node_id='node-b'")
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(state) != "relay-c:2" {
			t.Fatalf("migrated session state = %q, want relay-c:2", strings.TrimSpace(state))
		}
		assertTransfer(t, newClient, clients["node-a"], []byte("migration-reverse-route"))
	}) {
		t.FailNow()
	}
	if !t.Run("relay_failure_and_recovery", func(t *testing.T) {
		nodeD, err := suite.dialRelayClient("relay-b", "node-d")
		if err != nil {
			t.Fatalf("connect node-d to relay-b: %v", err)
		}
		clients["node-d"] = nodeD
		waitForTransfer(t, clients["node-a"], nodeD, 20*time.Second)
		if err := suite.stopService("relay-b"); err != nil {
			t.Fatal(err)
		}
		waitForClientClose(t, nodeD, 5*time.Second)
		assertTransfer(t, clients["node-a"], clients["node-c"], []byte("remaining-relays-still-route"))
		assertEventuallyRejected(t, clients["node-a"], "node-d", []byte("offline-relay-payload"), "relay peer is not allowed", 12*time.Second)
		if err := suite.recreateRelay("relay-b", "boot-b-2"); err != nil {
			t.Fatal(err)
		}
		reconnected, err := suite.dialRelayClient("relay-b", "node-d")
		if err != nil {
			t.Fatalf("reconnect node-d to relay-b: %v", err)
		}
		clients["node-d"] = reconnected
		waitForTransfer(t, clients["node-a"], reconnected, 20*time.Second)
		assertTransfer(t, reconnected, clients["node-c"], []byte("recovered-relay-route"))
		bootID, err := suite.postgresQuery(context.Background(), "SELECT boot_id FROM relay_instances WHERE relay_id='relay-b'")
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(bootID) != "boot-b-2" {
			t.Fatalf("relay-b boot id = %q, want boot-b-2", strings.TrimSpace(bootID))
		}
	}) {
		t.FailNow()
	}
	if !t.Run("coordinator_restart_and_recovery", func(t *testing.T) {
		outageStarted := time.Now()
		if err := suite.stopService("relay-coordinator"); err != nil {
			t.Fatal(err)
		}
		assertRejected(t, clients["node-a"], "node-c", []byte("coordinator-outage-payload"), 12*time.Second)
		assertNoPayload(t, clients["node-c"], []byte("coordinator-outage-payload"), 500*time.Millisecond)
		if err := suite.startService("relay-coordinator"); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(outageStarted); elapsed >= 15*time.Second {
			t.Fatalf("coordinator outage lasted %s, exceeding instance lease", elapsed)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := suite.waitForHTTPS(ctx, "relay-coordinator", relayCoordinatorHTTPSPort, "/readyz", "relay-coordinator", suite.certificates["e2e-client"], http.StatusOK); err != nil {
			t.Fatal(err)
		}
		for _, relayID := range []string{"relay-a", "relay-c"} {
			if err := suite.waitForRelay(ctx, relayID); err != nil {
				t.Fatal(err)
			}
		}
		clients["node-a"].close()
		clients["node-c"].close()
		clients["node-a"] = dialRelayClientEventually(t, "relay-a", "node-a", 15*time.Second)
		clients["node-c"] = dialRelayClientEventually(t, "relay-c", "node-c", 15*time.Second)
		bootState, err := suite.postgresQuery(context.Background(), "SELECT string_agg(relay_id || ':' || boot_id, ',' ORDER BY relay_id) FROM relay_instances")
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(bootState) != "relay-a:boot-a-1,relay-b:boot-b-2,relay-c:boot-c-1" {
			t.Fatalf("relay boot state after Coordinator restart = %q", strings.TrimSpace(bootState))
		}
		waitForTransfer(t, clients["node-a"], clients["node-c"], 20*time.Second)
		assertTransfer(t, clients["node-c"], clients["node-a"], []byte("coordinator-recovered-without-relay-restart"))
	}) {
		t.FailNow()
	}
}

func dialRelayClientEventually(t *testing.T, relayID, nodeID string, timeout time.Duration) *relayClient {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		client, err := suite.dialRelayClient(relayID, nodeID)
		if err == nil {
			return client
		}
		lastErr = err
		waitPoll(deadline)
	}
	t.Fatalf("connect %s to %s after recovery: %v", nodeID, relayID, lastErr)
	return nil
}

func testStartupAndRegistry(t *testing.T) {
	t.Helper()
	tables, err := suite.postgresQuery(context.Background(), "SELECT count(*) FROM pg_class WHERE relname IN ('relay_instances','node_session_leases','platform_relay_endpoints')")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(tables) != "3" {
		t.Fatalf("migrated table count = %q, want 3", strings.TrimSpace(tables))
	}
	active, err := suite.postgresQuery(context.Background(), "SELECT count(*) FROM relay_instances WHERE lease_expires_at > now()")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(active) != "3" {
		t.Fatalf("active relay count = %q, want 3", strings.TrimSpace(active))
	}
	address, err := suite.port(context.Background(), "relay-coordinator", relayCoordinatorHTTPSPort)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, "https://"+address+"/internal/relay-control/v1/endpoints", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := suite.mutualHTTPClient(suite.certificates["e2e-client"], "relay-coordinator").Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("endpoint snapshot returned %s: %s", response.Status, strings.TrimSpace(string(raw)))
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var snapshot protocolv1.EndpointSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 1 || len(snapshot.Endpoints) != 3 {
		t.Fatalf("endpoint snapshot = %#v", snapshot)
	}
	for _, relayID := range []string{"relay-a", "relay-b", "relay-c"} {
		metrics := fetchMetrics(t, relayID)
		if !strings.Contains(metrics, "endlessnet_relay_sessions_active 0") {
			t.Fatalf("%s metrics did not expose session gauge", relayID)
		}
	}
}

func testSecurityContracts(t *testing.T, clients map[string]*relayClient) {
	t.Helper()
	address, err := suite.port(context.Background(), "relay-a", 9443)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := tls.Dial("tcp", address, &tls.Config{RootCAs: suite.publicRoots, ServerName: "relay-a", MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
	if err == nil {
		_ = connection.Close()
		t.Fatal("relay-a accepted TLS 1.2")
	}

	assertRawHelloRejected(t, "relay-a", "node-a", func(message map[string]any) { message["unexpected"] = true }, "invalid relay auth")
	assertRawHelloRejected(t, "relay-a", "node-a", func(message map[string]any) { message["protocol_version"] = 999 }, "unsupported relay protocol")

	coordinatorAddress, err := suite.port(context.Background(), "relay-coordinator", 9445)
	if err != nil {
		t.Fatal(err)
	}
	coordinatorTLS := suite.internalClientTLS(suite.certificates["relay-a"], "relay-coordinator")
	grpcConnection, err := grpc.NewClient(coordinatorAddress, grpc.WithTransportCredentials(credentials.NewTLS(coordinatorTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer grpcConnection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_, err = relayv1.NewRelayControlClient(grpcConnection).RegisterInstance(ctx, &relayv1.RegisterInstanceRequest{RelayId: "relay-b", BootId: "forged", MeshAddr: "forged:9444"})
	cancel()
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("forged relay control identity returned %v, want PermissionDenied", err)
	}

	meshAddress, err := suite.port(context.Background(), "relay-b", 9444)
	if err != nil {
		t.Fatal(err)
	}
	meshTLS := suite.internalClientTLS(suite.certificates["relay-a"], "relay-b")
	meshConnection, err := grpc.NewClient(meshAddress, grpc.WithTransportCredentials(credentials.NewTLS(meshTLS)))
	if err != nil {
		t.Fatal(err)
	}
	defer meshConnection.Close()
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	stream, err := relayv1.NewRelayMeshClient(meshConnection).Connect(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := stream.Send(&relayv1.MeshMessage{ProtocolVersion: relayv1.MeshProtocolVersion, Body: &relayv1.MeshMessage_Hello{Hello: &relayv1.MeshHello{RelayId: "relay-b", BootId: "boot-b-1"}}}); err != nil {
		cancel()
		t.Fatal(err)
	}
	_, err = stream.Recv()
	cancel()
	if err == nil {
		t.Fatal("relay-a certificate was accepted as relay-b mesh identity")
	}

	deniedPayload := []byte("acl-denied-payload")
	drainEvents(clients["node-a"])
	if err := clients["node-a"].send("node-denied", deniedPayload); err != nil {
		t.Fatal(err)
	}
	event := waitEvent(t, clients["node-a"], 3*time.Second)
	if event.relayErr == nil || event.relayErr.Error != "relay peer is not allowed" {
		t.Fatalf("ACL denial event = %#v", event)
	}
	for _, client := range clients {
		assertNoPayload(t, client, deniedPayload, 150*time.Millisecond)
	}
}

func (h *harness) dialRelayClient(relayID, nodeID string) (*relayClient, error) {
	return h.dialRelayClientReading(relayID, nodeID, true)
}
func (h *harness) dialRelayClientReading(relayID, nodeID string, read bool) (*relayClient, error) {
	address, err := h.port(context.Background(), relayID, 9443)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialer := &tls.Dialer{Config: &tls.Config{RootCAs: h.publicRoots, ServerName: relayID, MinVersion: tls.VersionTLS13}}
	rawConnection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	connection := rawConnection.(*tls.Conn)
	credential, err := protocolv1.Sign(h.signingKey, testNetworkID, nodeID, time.Now().UTC().Add(10*time.Minute))
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	writer := bufio.NewWriter(connection)
	if err := json.NewEncoder(writer).Encode(protocolv1.ClientHello{Type: protocolv1.MessageClientHello, ProtocolVersion: protocolv1.Version, Credential: *credential}); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if err := writer.Flush(); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		_ = connection.Close()
		return nil, err
	}
	decoder := json.NewDecoder(bufio.NewReader(connection))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		_ = connection.Close()
		return nil, err
	}
	var ready protocolv1.Ready
	if err := decodeStrict(raw, &ready); err != nil {
		_ = connection.Close()
		return nil, err
	}
	if ready.Type != protocolv1.MessageReady || ready.ProtocolVersion != protocolv1.Version || !ready.Ready {
		_ = connection.Close()
		return nil, fmt.Errorf("relay ready message = %#v", ready)
	}
	client := &relayClient{relayID: relayID, nodeID: nodeID, conn: connection, writer: writer, events: make(chan relayEvent, 128), closed: make(chan struct{})}
	if read {
		go client.readLoop(decoder)
	}
	return client, nil
}

func (c *relayClient) readLoop(decoder *json.Decoder) {
	defer c.closeMu.Do(func() { close(c.closed) })
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			c.publish(relayEvent{closed: err})
			return
		}
		var envelope struct {
			Type            string `json:"type"`
			ProtocolVersion int    `json:"protocol_version"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			c.publish(relayEvent{closed: err})
			return
		}
		if envelope.ProtocolVersion != protocolv1.Version {
			c.publish(relayEvent{closed: fmt.Errorf("server protocol version %d", envelope.ProtocolVersion)})
			return
		}
		switch envelope.Type {
		case protocolv1.MessageServerFrame:
			var frame protocolv1.ServerFrame
			if err := decodeStrict(raw, &frame); err != nil {
				c.publish(relayEvent{closed: err})
				return
			}
			c.publish(relayEvent{frame: &frame})
		case protocolv1.MessageError:
			var relayErr protocolv1.Error
			if err := decodeStrict(raw, &relayErr); err != nil {
				c.publish(relayEvent{closed: err})
				return
			}
			c.publish(relayEvent{relayErr: &relayErr})
		case protocolv1.MessageHeartbeat:
			var heartbeat protocolv1.Heartbeat
			if err := decodeStrict(raw, &heartbeat); err != nil {
				c.publish(relayEvent{closed: err})
				return
			}
			c.publish(relayEvent{heartbeat: &heartbeat})
		default:
			c.publish(relayEvent{closed: fmt.Errorf("unsupported server message %q", envelope.Type)})
			return
		}
	}
}

func (c *relayClient) publish(event relayEvent) {
	select {
	case c.events <- event:
	default:
		_ = c.conn.Close()
	}
}

func (c *relayClient) send(peerID string, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	if err := json.NewEncoder(c.writer).Encode(protocolv1.ClientFrame{Type: protocolv1.MessageClientFrame, ProtocolVersion: protocolv1.Version, PeerID: peerID, Payload: payload}); err != nil {
		return err
	}
	return c.writer.Flush()
}

func (c *relayClient) close() {
	if c != nil && c.conn != nil {
		_ = c.conn.Close()
	}
}

func waitForTransfer(t *testing.T, from, to *relayClient, timeout time.Duration) {
	t.Helper()
	drainEvents(from)
	drainEvents(to)
	deadline := time.Now().Add(timeout)
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		payload := []byte(fmt.Sprintf("probe-%s-%s-%d", from.nodeID, to.nodeID, attempt))
		if err := from.send(to.nodeID, payload); err != nil {
			waitPoll(deadline)
			continue
		}
		attemptDeadline := time.NewTimer(750 * time.Millisecond)
		delivered := false
		for !delivered {
			select {
			case event := <-to.events:
				if event.frame != nil && event.frame.FromNodeID == from.nodeID && bytes.Equal(event.frame.Payload, payload) {
					delivered = true
				}
			case <-from.events:
			case <-attemptDeadline.C:
				goto retry
			}
		}
		if !attemptDeadline.Stop() {
			select {
			case <-attemptDeadline.C:
			default:
			}
		}
		return
	retry:
		waitPoll(deadline)
	}
	t.Fatalf("route %s/%s -> %s/%s did not converge within %s", from.relayID, from.nodeID, to.relayID, to.nodeID, timeout)
}

func assertTransfer(t *testing.T, from, to *relayClient, payload []byte) {
	t.Helper()
	drainEvents(from)
	drainEvents(to)
	if err := from.send(to.nodeID, payload); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-to.events:
			if event.closed != nil {
				t.Fatalf("destination %s closed: %v", to.nodeID, event.closed)
			}
			if event.frame == nil {
				continue
			}
			if event.frame.ProtocolVersion != protocolv1.Version || event.frame.FromNodeID != from.nodeID || !bytes.Equal(event.frame.Payload, payload) {
				t.Fatal("delivered frame source or payload differs")
			}
			assertNoPayload(t, to, payload, 200*time.Millisecond)
			return
		case event := <-from.events:
			if event.relayErr != nil {
				t.Fatalf("source %s returned relay error: %s", from.nodeID, event.relayErr.Error)
			}
			if event.closed != nil {
				t.Fatalf("source %s closed: %v", from.nodeID, event.closed)
			}
		case <-timer.C:
			t.Fatalf("payload was not delivered from %s to %s", from.nodeID, to.nodeID)
		}
	}
}

func assertEventuallyRejected(t *testing.T, from *relayClient, peerID string, payload []byte, expected string, timeout time.Duration) {
	t.Helper()
	drainEvents(from)
	deadline := time.Now().Add(timeout)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		attemptPayload := append(append([]byte(nil), payload...), byte(attempt))
		if err := from.send(peerID, attemptPayload); err != nil {
			if errors.Is(err, net.ErrClosed) {
				t.Fatalf("source %s closed while waiting for rejection", from.nodeID)
			}
			continue
		}
		timer := time.NewTimer(750 * time.Millisecond)
		select {
		case event := <-from.events:
			if event.relayErr != nil && event.relayErr.Error == expected {
				timer.Stop()
				return
			}
			if event.closed != nil {
				timer.Stop()
				t.Fatalf("source %s closed: %v", from.nodeID, event.closed)
			}
		case <-timer.C:
		}
		waitPoll(deadline)
	}
	t.Fatalf("source %s did not return %q for peer %s", from.nodeID, expected, peerID)
}

func assertRejected(t *testing.T, from *relayClient, peerID string, payload []byte, timeout time.Duration) {
	t.Helper()
	drainEvents(from)
	select {
	case <-from.closed:
		return
	default:
	}
	if err := from.send(peerID, payload); err != nil {
		select {
		case <-from.closed:
			return
		default:
			t.Fatal(err)
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case event := <-from.events:
			if event.relayErr != nil && strings.TrimSpace(event.relayErr.Error) != "" {
				return
			}
			if event.closed != nil {
				return
			}
		case <-from.closed:
			return
		case <-timer.C:
			t.Fatalf("source %s did not reject peer %s within %s", from.nodeID, peerID, timeout)
		}
	}
}

func assertNoPayload(t *testing.T, client *relayClient, payload []byte, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case event := <-client.events:
			if event.frame != nil && bytes.Equal(event.frame.Payload, payload) {
				t.Fatalf("client %s received forbidden or duplicate payload", client.nodeID)
			}
			if event.closed != nil {
				return
			}
		case <-timer.C:
			return
		}
	}
}

func waitForClientClose(t *testing.T, client *relayClient, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-client.closed:
		return
	case <-timer.C:
		t.Fatalf("client %s on %s did not close within %s", client.nodeID, client.relayID, timeout)
	}
}

func waitEvent(t *testing.T, client *relayClient, timeout time.Duration) relayEvent {
	t.Helper()
	select {
	case event := <-client.events:
		return event
	case <-time.After(timeout):
		t.Fatalf("client %s produced no event within %s", client.nodeID, timeout)
		return relayEvent{}
	}
}

func drainEvents(client *relayClient) {
	for {
		select {
		case <-client.events:
		default:
			return
		}
	}
}

func waitPoll(deadline time.Time) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return
	}
	duration := 100 * time.Millisecond
	if remaining < duration {
		duration = remaining
	}
	timer := time.NewTimer(duration)
	<-timer.C
}

func assertRawHelloRejected(t *testing.T, relayID, nodeID string, mutate func(map[string]any), expected string) {
	t.Helper()
	address, err := suite.port(context.Background(), relayID, 9443)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := tls.Dial("tcp", address, &tls.Config{RootCAs: suite.publicRoots, ServerName: relayID, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	credential, err := protocolv1.Sign(suite.signingKey, testNetworkID, nodeID, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	message := map[string]any{"type": protocolv1.MessageClientHello, "protocol_version": protocolv1.Version, "credential": credential}
	mutate(message)
	if err := json.NewEncoder(connection).Encode(message); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	var relayErr protocolv1.Error
	if err := json.NewDecoder(connection).Decode(&relayErr); err != nil {
		t.Fatal(err)
	}
	if relayErr.Type != protocolv1.MessageError || relayErr.ProtocolVersion != protocolv1.Version || relayErr.Error != expected {
		t.Fatalf("rejection = %#v, want %q", relayErr, expected)
	}
}

func fetchMetrics(t *testing.T, relayID string) string {
	t.Helper()
	address, err := suite.port(context.Background(), relayID, 9090)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + address + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s metrics returned %s", relayID, response.Status)
	}
	return string(raw)
}

func assertMetricsDoNotContain(t *testing.T, value string) {
	t.Helper()
	for _, relayID := range []string{"relay-a", "relay-b", "relay-c"} {
		if metrics := fetchMetrics(t, relayID); strings.Contains(metrics, value) {
			t.Fatalf("%s metrics leaked application payload", relayID)
		}
	}
}

func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}
