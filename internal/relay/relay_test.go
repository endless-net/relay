package relay

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	protocolv1 "github.com/unng-lab/endlessnet-relay/protocol/v1"
)

type testControl struct {
	mu            sync.Mutex
	nextEpoch     int64
	sessions      map[string]SessionLease
	acquireHook   func(context.Context, protocolv1.Credential) (SessionLease, error)
	renewHook     func(context.Context, SessionLease) error
	authorizeHook func(context.Context, protocolv1.Credential, int64, string) (PeerRoute, error)
}

func (c *testControl) AcquireSession(ctx context.Context, credential protocolv1.Credential) (SessionLease, error) {
	if c.acquireHook != nil {
		return c.acquireHook(ctx, credential)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextEpoch++
	lease := SessionLease{NetworkID: credential.NetworkID, NodeID: credential.NodeID, RelayID: "relay-test", BootID: "boot-test", Epoch: c.nextEpoch, Credential: credential}
	if c.sessions == nil {
		c.sessions = make(map[string]SessionLease)
	}
	c.sessions[credential.NodeID] = lease
	return lease, nil
}

func (c *testControl) RenewSession(ctx context.Context, lease SessionLease) error {
	if c.renewHook != nil {
		return c.renewHook(ctx, lease)
	}
	return nil
}

func (c *testControl) ReleaseSession(context.Context, SessionLease) error { return nil }

func (c *testControl) AuthorizePeer(ctx context.Context, credential protocolv1.Credential, sourceEpoch int64, peerID string) (PeerRoute, error) {
	if c.authorizeHook != nil {
		return c.authorizeHook(ctx, credential, sourceEpoch, peerID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	lease, ok := c.sessions[peerID]
	if !ok {
		return PeerRoute{}, errors.New("peer is unavailable")
	}
	return PeerRoute{RelayID: lease.RelayID, BootID: lease.BootID, Epoch: lease.Epoch}, nil
}

type testMesh struct{}

func (*testMesh) Forward(context.Context, PeerRoute, string, string, string, []byte) error {
	return nil
}

func TestNewServerRequiresProductionDependencies(t *testing.T) {
	certificate, _ := tlsCertificateForTest(t)
	valid := ServerConfig{
		Addr:                "127.0.0.1:1",
		TLSConfig:           &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13},
		RelayID:             "relay-test",
		Control:             &testControl{},
		Mesh:                &testMesh{},
		TrustBundleProvider: func() (protocolv1.SigningTrustBundle, error) { return protocolv1.SigningTrustBundle{}, nil },
	}
	for _, test := range []struct {
		name   string
		mutate func(*ServerConfig)
	}{
		{name: "TLS", mutate: func(config *ServerConfig) { config.TLSConfig = nil }},
		{name: "TLS 1.2", mutate: func(config *ServerConfig) { config.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12} }},
		{name: "control", mutate: func(config *ServerConfig) { config.Control = nil }},
		{name: "mesh", mutate: func(config *ServerConfig) { config.Mesh = nil }},
		{name: "trust provider", mutate: func(config *ServerConfig) { config.TrustBundleProvider = nil }},
		{name: "negative timeout", mutate: func(config *ServerConfig) { config.AuthTimeout = -time.Second }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := NewServer(config); err == nil {
				t.Fatal("invalid production configuration was accepted")
			}
		})
	}
	if _, err := NewServer(valid); err != nil {
		t.Fatalf("valid production configuration: %v", err)
	}
}

func TestDecodeRelayLineRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	for _, line := range [][]byte{
		[]byte(`{"type":"client_frame","protocol_version":1,"peer_id":"node","payload":"YQ==","legacy":true}`),
		[]byte(`{"type":"client_frame","protocol_version":1,"peer_id":"node","payload":"YQ=="} {}`),
	} {
		var frame protocolv1.ClientFrame
		if err := decodeRelayLine(line, &frame); err == nil {
			t.Fatalf("invalid relay line was accepted: %s", line)
		}
	}
}

func TestRelayRequiresExplicitVersionAndForwardsOverTLS13(t *testing.T) {
	server, roots, privateKey, control, cancel, done := startRelayForTest(t, 500*time.Millisecond)
	defer stopRelayForTest(t, cancel, done)

	unversioned := dialTLSForTest(t, server.Addr, roots)
	credential := signCredentialForTest(t, privateKey, "network", "legacy")
	if err := json.NewEncoder(unversioned).Encode(protocolv1.ClientHello{Credential: credential}); err != nil {
		t.Fatal(err)
	}
	var protocolError protocolv1.Error
	if err := json.NewDecoder(unversioned).Decode(&protocolError); err != nil {
		t.Fatal(err)
	}
	_ = unversioned.Close()
	if protocolError.Type != protocolv1.MessageError || protocolError.ProtocolVersion != protocolv1.Version {
		t.Fatalf("protocol error = %#v", protocolError)
	}

	peerB, readerB := authenticateRelayForTest(t, server.Addr, roots, privateKey, "network", "node-b", 0)
	defer peerB.Close()
	peerA, _ := authenticateRelayForTest(t, server.Addr, roots, privateKey, "network", "node-a", 0)
	defer peerA.Close()
	payload := []byte("opaque relay payload")
	if err := json.NewEncoder(peerA).Encode(protocolv1.ClientFrame{Type: protocolv1.MessageClientFrame, ProtocolVersion: protocolv1.Version, PeerID: "node-b", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	var frame protocolv1.ServerFrame
	if err := json.NewDecoder(readerB).Decode(&frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != protocolv1.MessageServerFrame || frame.ProtocolVersion != protocolv1.Version || frame.FromNodeID != "node-a" || string(frame.Payload) != string(payload) {
		t.Fatalf("forwarded frame = %#v", frame)
	}
	control.mu.Lock()
	epoch := control.sessions["node-b"].Epoch
	control.mu.Unlock()
	if err := server.DeliverRemote("network", "node-a", "node-b", epoch+1, payload); !errors.Is(err, ErrDestinationFenced) {
		t.Fatalf("fenced delivery error = %v", err)
	}
}

func TestRelayEmitsRequestedHeartbeat(t *testing.T) {
	server, roots, privateKey, _, cancel, done := startRelayForTest(t, time.Second)
	defer stopRelayForTest(t, cancel, done)
	connection, reader := authenticateRelayForTest(t, server.Addr, roots, privateKey, "network", "node", protocolv1.MinHeartbeatIntervalMS)
	defer connection.Close()
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var heartbeat protocolv1.Heartbeat
	if err := json.NewDecoder(reader).Decode(&heartbeat); err != nil {
		t.Fatal(err)
	}
	if heartbeat.Type != protocolv1.MessageHeartbeat || heartbeat.ProtocolVersion != protocolv1.Version || heartbeat.Time == "" {
		t.Fatalf("heartbeat = %#v", heartbeat)
	}
}

func TestRelayAcquireSessionUsesAuthTimeout(t *testing.T) {
	server, roots, privateKey, control, cancel, done := startRelayForTest(t, 50*time.Millisecond)
	defer stopRelayForTest(t, cancel, done)
	control.acquireHook = func(ctx context.Context, _ protocolv1.Credential) (SessionLease, error) {
		<-ctx.Done()
		return SessionLease{}, ctx.Err()
	}
	connection := dialTLSForTest(t, server.Addr, roots)
	defer connection.Close()
	credential := signCredentialForTest(t, privateKey, "network", "node")
	started := time.Now()
	if err := json.NewEncoder(connection).Encode(protocolv1.ClientHello{Type: protocolv1.MessageClientHello, ProtocolVersion: protocolv1.Version, Credential: credential}); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var response protocolv1.Error
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("AcquireSession took %s", elapsed)
	}
}

func TestSessionRenewalFailureClosesSession(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := &session{conn: serverSide, ctx: ctx, cancel: cancel, done: make(chan struct{}), metrics: &Metrics{}}
	control := &testControl{renewHook: func(context.Context, SessionLease) error { return errors.New("revoked") }}
	sess.startLeaseRenewal(control, 10*time.Millisecond, 50*time.Millisecond)
	deadline := time.Now().Add(time.Second)
	for !sess.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !sess.closed.Load() {
		t.Fatal("revoked session remained open")
	}
}

func TestControlUnavailableDuringPeerAuthorizationClosesSession(t *testing.T) {
	server, roots, privateKey, control, cancel, done := startRelayForTest(t, 50*time.Millisecond)
	defer stopRelayForTest(t, cancel, done)
	peerB, _ := authenticateRelayForTest(t, server.Addr, roots, privateKey, "network", "node-b", 0)
	defer peerB.Close()
	peerA, readerA := authenticateRelayForTest(t, server.Addr, roots, privateKey, "network", "node-a", 0)
	defer peerA.Close()

	control.authorizeHook = func(context.Context, protocolv1.Credential, int64, string) (PeerRoute, error) {
		return PeerRoute{}, fmt.Errorf("%w: test outage", ErrControlUnavailable)
	}
	if err := json.NewEncoder(peerA).Encode(protocolv1.ClientFrame{Type: protocolv1.MessageClientFrame, ProtocolVersion: protocolv1.Version, PeerID: "node-b", Payload: []byte("fail-closed")}); err != nil {
		t.Fatal(err)
	}
	if err := peerA.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	decoderA := json.NewDecoder(readerA)
	var relayError protocolv1.Error
	if err := decoderA.Decode(&relayError); err != nil {
		t.Fatal(err)
	}
	if relayError.Type != protocolv1.MessageError || relayError.Error == "" {
		t.Fatalf("relay error = %#v", relayError)
	}
	var trailing json.RawMessage
	if err := decoderA.Decode(&trailing); err == nil {
		t.Fatal("session remained open after relay control outage")
	}
}

func TestPeerDenialDoesNotCloseSession(t *testing.T) {
	server, roots, privateKey, control, cancel, done := startRelayForTest(t, 500*time.Millisecond)
	defer stopRelayForTest(t, cancel, done)
	peerB, readerB := authenticateRelayForTest(t, server.Addr, roots, privateKey, "network", "node-b", 0)
	defer peerB.Close()
	peerA, readerA := authenticateRelayForTest(t, server.Addr, roots, privateKey, "network", "node-a", 0)
	defer peerA.Close()

	control.mu.Lock()
	allowed := control.sessions["node-b"]
	control.mu.Unlock()
	var denied atomic.Bool
	denied.Store(true)
	control.authorizeHook = func(context.Context, protocolv1.Credential, int64, string) (PeerRoute, error) {
		if denied.Load() {
			return PeerRoute{}, errors.New("peer denied")
		}
		return PeerRoute{RelayID: allowed.RelayID, BootID: allowed.BootID, Epoch: allowed.Epoch}, nil
	}
	if err := json.NewEncoder(peerA).Encode(protocolv1.ClientFrame{Type: protocolv1.MessageClientFrame, ProtocolVersion: protocolv1.Version, PeerID: "node-b", Payload: []byte("denied")}); err != nil {
		t.Fatal(err)
	}
	var relayError protocolv1.Error
	if err := json.NewDecoder(readerA).Decode(&relayError); err != nil {
		t.Fatal(err)
	}
	denied.Store(false)
	payload := []byte("allowed-after-denial")
	if err := json.NewEncoder(peerA).Encode(protocolv1.ClientFrame{Type: protocolv1.MessageClientFrame, ProtocolVersion: protocolv1.Version, PeerID: "node-b", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	var frame protocolv1.ServerFrame
	if err := json.NewDecoder(readerB).Decode(&frame); err != nil {
		t.Fatal(err)
	}
	if frame.FromNodeID != "node-a" || string(frame.Payload) != string(payload) {
		t.Fatalf("forwarded frame = %#v", frame)
	}
}

func startRelayForTest(t *testing.T, authTimeout time.Duration) (*Server, *x509.CertPool, ed25519.PrivateKey, *testControl, context.CancelFunc, <-chan error) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := protocolv1.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	certificate, roots := tlsCertificateForTest(t)
	control := &testControl{}
	server, err := NewServer(ServerConfig{
		Addr:        freeTCPAddr(t),
		TLSConfig:   &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13},
		RelayID:     "relay-test",
		Control:     control,
		Mesh:        &testMesh{},
		AuthTimeout: authTimeout,
		TrustBundleProvider: func() (protocolv1.SigningTrustBundle, error) {
			return bundle, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe(ctx) }()
	return server, roots, privateKey, control, cancel, done
}

func stopRelayForTest(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relay shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay shutdown timed out")
	}
}

func authenticateRelayForTest(t *testing.T, addr string, roots *x509.CertPool, privateKey ed25519.PrivateKey, networkID, nodeID string, heartbeatMS int) (net.Conn, *bufio.Reader) {
	t.Helper()
	connection := dialTLSForTest(t, addr, roots)
	hello := protocolv1.ClientHello{Type: protocolv1.MessageClientHello, ProtocolVersion: protocolv1.Version, Credential: signCredentialForTest(t, privateKey, networkID, nodeID), HeartbeatIntervalMS: heartbeatMS}
	if err := json.NewEncoder(connection).Encode(hello); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	var ready protocolv1.Ready
	if err := json.NewDecoder(reader).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	if ready.Type != protocolv1.MessageReady || ready.ProtocolVersion != protocolv1.Version || !ready.Ready {
		t.Fatalf("relay ready = %#v", ready)
	}
	return connection, reader
}

func signCredentialForTest(t *testing.T, privateKey ed25519.PrivateKey, networkID, nodeID string) protocolv1.Credential {
	t.Helper()
	credential, err := protocolv1.Sign(privateKey, networkID, nodeID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return *credential
}

func dialTLSForTest(t *testing.T, addr string, roots *x509.CertPool) net.Conn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
		connection, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13})
		if err == nil {
			if connection.ConnectionState().Version != tls.VersionTLS13 {
				t.Fatalf("relay TLS version = %#x", connection.ConnectionState().Version)
			}
			return connection
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("dial relay %s: %v", addr, lastErr)
	return nil
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func tlsCertificateForTest(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey, Leaf: parsed}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return certificate, roots
}
