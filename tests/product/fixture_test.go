//go:build product

// Package product exercises real runtime binaries against isolated synthetic authorities.
package product

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	upstream "github.com/endless-net/coordinator/api/relay/v1/upstreamv1connect"
	pb "github.com/endless-net/relay/internal/grpcwire"
	"github.com/endless-net/relay/internal/testkit"
	wire "github.com/endless-net/relay/internal/wire"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	workload "github.com/spiffe/go-spiffe/v2/proto/spiffe/workload"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	spiffetls "github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc"
)

type pki struct {
	ca    *x509.Certificate
	key   *ecdsa.PrivateKey
	roots *x509.CertPool
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated-relay-fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(2 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	raw, e := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	ca, e = x509.ParseCertificate(raw)
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &pki{ca, key, roots}
}
func (p *pki) issue(t *testing.T, id, cn string, lifetime time.Duration) tls.Certificate {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if e != nil {
		t.Fatal(e)
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(lifetime), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}}
	if id != "" {
		u, e := url.Parse(id)
		if e != nil {
			t.Fatal(e)
		}
		cert.URIs = []*url.URL{u}
	}
	raw, e := x509.CreateCertificate(rand.Reader, cert, p.ca, &key.PublicKey, p.key)
	if e != nil {
		t.Fatal(e)
	}
	leaf, e := x509.ParseCertificate(raw)
	if e != nil {
		t.Fatal(e)
	}
	return tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: key, Leaf: leaf}
}
func writeCert(t *testing.T, dir, name string, cert tls.Certificate) (string, string) {
	t.Helper()
	key, e := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	certPath, keyPath := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if e = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); e != nil {
		t.Fatal(e)
	}
	return certPath, keyPath
}

type workAPI struct {
	workload.UnimplementedSpiffeWorkloadAPIServer
	mu       sync.Mutex
	current  *workload.X509SVID
	watchers map[chan struct{}]bool
}

func (a *workAPI) FetchX509SVID(_ *workload.X509SVIDRequest, stream grpc.ServerStreamingServer[workload.X509SVIDResponse]) error {
	notify := make(chan struct{}, 1)
	a.mu.Lock()
	a.watchers[notify] = true
	current := a.current
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.watchers, notify); a.mu.Unlock() }()
	for {
		if e := stream.Send(&workload.X509SVIDResponse{Svids: []*workload.X509SVID{current}}); e != nil {
			return e
		}
		select {
		case <-stream.Context().Done():
			return nil
		case <-notify:
			a.mu.Lock()
			current = a.current
			a.mu.Unlock()
		}
	}
}
func (a *workAPI) update(t *testing.T, id string, cert tls.Certificate, root []byte) {
	t.Helper()
	key, e := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if e != nil {
		t.Fatal(e)
	}
	a.mu.Lock()
	a.current = &workload.X509SVID{SpiffeId: id, X509Svid: cert.Certificate[0], X509SvidKey: key, Bundle: root}
	for ch := range a.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	a.mu.Unlock()
}

type authority struct {
	upstream.UnimplementedRelayUpstreamServiceHandler
	bundle      wire.SigningTrustBundle
	public      string
	revoked     atomic.Bool
	unavailable atomic.Bool
}

func (a *authority) credential(raw *pb.Credential) error {
	if a.unavailable.Load() {
		return connect.NewError(connect.CodeUnavailable, errors.New("fixture_authority_unavailable"))
	}
	c, e := pb.CredentialToProtocol(raw)
	if e != nil || a.revoked.Load() || c.NetworkID != "fixture" || wire.Verify(c, a.public, time.Now()) != nil {
		return connect.NewError(connect.CodePermissionDenied, errors.New("fixture_deny"))
	}
	return nil
}
func (a *authority) AuthorizeCredential(_ context.Context, r *connect.Request[pb.AuthorizeCredentialRequest]) (*connect.Response[pb.AuthorizeCredentialResponse], error) {
	if e := a.credential(r.Msg.Credential); e != nil {
		return nil, e
	}
	return connect.NewResponse(&pb.AuthorizeCredentialResponse{}), nil
}
func (a *authority) AuthorizePeerPair(_ context.Context, r *connect.Request[pb.AuthorizePeerPairRequest]) (*connect.Response[pb.AuthorizePeerPairResponse], error) {
	if e := a.credential(r.Msg.Credential); e != nil {
		return nil, e
	}
	var index int
	if _, e := fmt.Sscanf(r.Msg.Credential.NodeId, "node-%d", &index); e != nil || index%2 != 0 || r.Msg.PeerId != fmt.Sprintf("node-%d", index+1) || r.Msg.PeerNetworkId != "fixture" {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("fixture_pair_deny"))
	}
	return connect.NewResponse(&pb.AuthorizePeerPairResponse{}), nil
}
func (a *authority) GetTrustBundle(context.Context, *connect.Request[pb.GetTrustBundleRequest]) (*connect.Response[pb.GetTrustBundleResponse], error) {
	if a.unavailable.Load() {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("fixture_authority_unavailable"))
	}
	return connect.NewResponse(&pb.GetTrustBundleResponse{RelayTrustBundle: pb.TrustBundleFromProtocol(a.bundle)}), nil
}

type child struct {
	command *exec.Cmd
	done    chan error
	exited  chan struct{}
	once    sync.Once
	log     string
}

func startChild(t *testing.T, ctx context.Context, name string, args, env []string, dir string) *child {
	t.Helper()
	log := filepath.Join(dir, filepath.Base(name)+fmt.Sprint(time.Now().UnixNano())+".log")
	sink, e := os.OpenFile(log, os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	cmd.Stdout = sink
	cmd.Stderr = sink
	if e = cmd.Start(); e != nil {
		_ = sink.Close()
		t.Fatal(e)
	}
	c := &child{command: cmd, done: make(chan error, 1), exited: make(chan struct{}), log: log}
	go func() { c.done <- cmd.Wait(); _ = sink.Close(); close(c.exited) }()
	t.Cleanup(func() { c.stop(t) })
	return c
}
func (c *child) stop(t *testing.T) {
	t.Helper()
	c.once.Do(func() {
		_ = c.command.Process.Signal(syscall.SIGTERM)
		select {
		case <-c.done:
		case <-time.After(12 * time.Second):
			_ = c.command.Process.Kill()
			<-c.done
			t.Error("child shutdown exceeded deadline")
		}
	})
}
func envFor(values map[string]string) []string {
	all := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		all[key] = value
	}
	for key, value := range values {
		all[key] = value
	}
	out := make([]string, 0, len(all))
	for key, value := range all {
		out = append(out, key+"="+value)
	}
	return out
}
func freeAddress(t *testing.T) string {
	return freeAddressAt(t, "127.0.0.1:0")
}

func freeAddressAt(t *testing.T, address string) string {
	t.Helper()
	l, e := (&net.ListenConfig{}).Listen(t.Context(), "tcp", address)
	if e != nil {
		t.Fatal(e)
	}
	boundAddress := l.Addr().String()
	_ = l.Close()
	return boundAddress
}

type fixture struct {
	dir                    string
	pki                    *pki
	private                ed25519.PrivateKey
	authority              *authority
	public                 [2]string
	diagnostics            [2]string
	processes              [2]*child
	coordinator            *child
	coordinatorDiagnostics string
	registry               *child
	registryAddress        string
	workloads              map[string]*workAPI
	ctx                    context.Context
	cancel                 context.CancelFunc
}

func newFixture(t *testing.T) *fixture {
	return newFixtureWithTelemetry(t, "")
}

func newFixtureWithTelemetry(t *testing.T, telemetryEndpoint string) *fixture {
	return newFixtureWithPublicListener(t, telemetryEndpoint, "127.0.0.1:0")
}

func newFixtureWithPublicListener(t *testing.T, telemetryEndpoint, publicListen string) *fixture {
	t.Helper()
	pgRoot := os.Getenv("RELAY_POSTGRES_ROOT")
	if !filepath.IsAbs(pgRoot) || !strings.HasPrefix(pgRoot, "/tmp/relay009-pg.") {
		t.Fatal("RELAY_POSTGRES_ROOT must name the extracted isolated test package root")
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &fixture{workloads: map[string]*workAPI{}, dir: t.TempDir(), pki: newPKI(t), ctx: ctx, cancel: cancel}
	t.Cleanup(cancel)
	t.Cleanup(func() {
		if t.Failed() {
			paths, _ := filepath.Glob(filepath.Join(f.dir, "*.log"))
			for _, path := range paths {
				raw, _ := os.ReadFile(path)
				if len(raw) > 2000 {
					raw = raw[len(raw)-2000:]
				}
				t.Logf("fixture log %s: %s", filepath.Base(path), raw)
			}
		}
	})
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	binaries := map[string]string{}
	for _, role := range []string{"relay", "relay-coordinator"} {
		binaryPath := filepath.Join(f.dir, role)
		cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", binaryPath, "./cmd/"+role)
		cmd.Dir = repo
		cmd.Env = envFor(map[string]string{"GOWORK": "off", "GOFLAGS": "-mod=readonly"})
		if output, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("build failed: %v %s", e, output)
		}
		raw, e := os.ReadFile(binaryPath)
		if e != nil {
			t.Fatal(e)
		}
		t.Logf("binary=%s sha256=%x", role, sha256.Sum256(raw))
		binaries[role] = binaryPath
	}
	api := func(name, id string, cert tls.Certificate) *workAPI {
		path := filepath.Join(f.dir, name+".sock")
		listener, e := (&net.ListenConfig{}).Listen(ctx, "unix", path)
		if e != nil {
			t.Fatal(e)
		}
		a := &workAPI{watchers: map[chan struct{}]bool{}}
		f.workloads[name] = a
		a.update(t, id, cert, f.pki.ca.Raw)
		server := grpc.NewServer()
		workload.RegisterSpiffeWorkloadAPIServer(server, a)
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(server.Stop)
		return a
	}
	rcID := "spiffe://endlessnet.ru/service/relay-coordinator"
	rcCert := f.pki.issue(t, rcID, "relaytest", time.Hour)
	api("rc", rcID, rcCert)
	// Rotate A's short SVID while keeping its gRPC transports open.
	aID := "spiffe://endlessnet.ru/service/relay/a"
	short := f.pki.issue(t, aID, "relay-a", 6*time.Second)
	aAPI := api("a", aID, short)
	bID := "spiffe://endlessnet.ru/service/relay/b"
	api("b", bID, f.pki.issue(t, bID, "relay-b", time.Hour))
	rootPath := filepath.Join(f.dir, "ca.crt")
	if e = os.WriteFile(rootPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.pki.ca.Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	pgCert, pgKey := writeCert(t, f.dir, "postgres", f.pki.issue(t, "spiffe://endlessnet.ru/service/relay-store", "postgres", time.Hour))
	pgBin := filepath.Join(pgRoot, "usr/lib/postgresql/18/bin")
	pgData := filepath.Join(f.dir, "pgdata")
	pgSocket := filepath.Join(f.dir, "pgsocket")
	if e = os.Mkdir(pgSocket, 0700); e != nil {
		t.Fatal(e)
	}
	pgEnv := envFor(map[string]string{"LD_LIBRARY_PATH": filepath.Join(pgRoot, "usr/lib/x86_64-linux-gnu")})
	init := exec.CommandContext(ctx, filepath.Join(pgBin, "initdb"), "-D", pgData, "-L", filepath.Join(pgRoot, "usr/share/postgresql/18"), "--no-locale", "-A", "trust", "-U", "relaytest")
	init.Env = pgEnv
	if output, e := init.CombinedOutput(); e != nil {
		t.Fatalf("initdb: %v %s", e, output)
	}
	if e = os.WriteFile(filepath.Join(pgData, "pg_hba.conf"), []byte("local all all trust\nhostssl all relaytest 127.0.0.1/32 cert\nhost all all 127.0.0.1/32 reject\n"), 0600); e != nil {
		t.Fatal(e)
	}
	pgAddress := freeAddress(t)
	f.registryAddress = pgAddress
	_, pgPort, _ := net.SplitHostPort(pgAddress)
	f.registry = startChild(t, ctx, filepath.Join(pgBin, "postgres"), []string{"-D", pgData, "-h", "127.0.0.1", "-p", pgPort, "-k", pgSocket, "-c", "ssl=on", "-c", "ssl_cert_file=" + pgCert, "-c", "ssl_key_file=" + pgKey, "-c", "ssl_ca_file=" + rootPath}, pgEnv, f.dir)
	var pool *pgxpool.Pool
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pool, e = pgxpool.New(ctx, "host="+pgSocket+" port="+pgPort+" user=relaytest dbname=postgres sslmode=disable")
		if e == nil && pool.Ping(ctx) == nil {
			break
		}
		if pool != nil {
			pool.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pool == nil {
		t.Fatal("fixture database did not start")
	}
	if _, e = pool.Exec(ctx, "CREATE DATABASE relay009"); e != nil {
		t.Fatal("fixture database create failed")
	}
	pool.Close()
	pub, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	f.private = private
	bundle, e := wire.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(pub))
	if e != nil {
		t.Fatal(e)
	}
	f.authority = &authority{bundle: bundle, public: base64.RawURLEncoding.EncodeToString(pub)}
	upstreamID := "spiffe://endlessnet.ru/service/coordinator"
	upCert := f.pki.issue(t, upstreamID, "fixture-authority", time.Hour)
	td := spiffeid.RequireTrustDomainFromString("endlessnet.ru")
	trust := x509bundle.FromX509Authorities(td, []*x509.Certificate{f.pki.ca})
	upSVID := &x509svid.SVID{ID: spiffeid.RequireFromString(upstreamID), Certificates: []*x509.Certificate{upCert.Leaf}, PrivateKey: upCert.PrivateKey.(*ecdsa.PrivateKey)}
	_, handler := upstream.NewRelayUpstreamServiceHandler(f.authority)
	backend := httptest.NewUnstartedServer(handler)
	backend.EnableHTTP2 = true
	backend.TLS = spiffetls.MTLSServerConfig(upSVID, trust, spiffetls.AuthorizeID(spiffeid.RequireFromString(rcID)))
	backend.TLS.MinVersion = tls.VersionTLS13
	backend.TLS.Certificates = []tls.Certificate{upCert}
	backend.StartTLS()
	t.Cleanup(backend.Close)
	controlAddress := freeAddress(t)
	rcHTTP := freeAddress(t)
	f.coordinatorDiagnostics = rcHTTP
	dsn := "postgres://relaytest@" + pgAddress + "/relay009?sslmode=require"
	rcEnv := map[string]string{"GOMAXPROCS": "2", "ENDLESSNET_RELAY_WORKLOAD_API": "unix://" + filepath.Join(f.dir, "rc.sock"), "ENDLESSNET_RELAY_DATABASE_URL": dsn, "ENDLESSNET_RELAY_DATABASE_IDENTITY": "spiffe://endlessnet.ru/service/relay-store", "ENDLESSNET_RELAY_UPSTREAM_URL": backend.URL, "ENDLESSNET_RELAY_CONTROL_LISTEN": controlAddress, "ENDLESSNET_RELAY_HTTP_LISTEN": rcHTTP, "ENDLESSNET_RELAY_OTEL_ENDPOINT": telemetryEndpoint}
	migrate := exec.CommandContext(ctx, binaries["relay-coordinator"], "--migrate")
	migrate.Env = envFor(rcEnv)
	if output, e := migrate.CombinedOutput(); e != nil {
		t.Fatalf("runtime migrate failed %v %s", e, output)
	}
	f.coordinator = startChild(t, ctx, binaries["relay-coordinator"], nil, envFor(rcEnv), f.dir)
	waitReady(t, ctx, rcHTTP)
	pubCert, pubKey := writeCert(t, f.dir, "public", f.pki.issue(t, "", "localhost", time.Hour))
	for i, id := range []string{"a", "b"} {
		f.public[i] = freeAddressAt(t, publicListen)
		meshAddress, httpAddress := freeAddress(t), freeAddress(t)
		f.diagnostics[i] = httpAddress
		values := map[string]string{"GOMAXPROCS": "2", "ENDLESSNET_RELAY_ID": id, "ENDLESSNET_RELAY_LISTEN": f.public[i], "ENDLESSNET_RELAY_MESH_LISTEN": meshAddress, "ENDLESSNET_RELAY_MESH_ADDRESS": meshAddress, "ENDLESSNET_RELAY_COORDINATOR_ADDRESS": controlAddress, "ENDLESSNET_RELAY_HTTP_LISTEN": httpAddress, "ENDLESSNET_RELAY_WORKLOAD_API": "unix://" + filepath.Join(f.dir, id+".sock"), "ENDLESSNET_RELAY_TLS_CERT": pubCert, "ENDLESSNET_RELAY_TLS_KEY": pubKey, "ENDLESSNET_RELAY_OTEL_ENDPOINT": telemetryEndpoint}
		f.processes[i] = startChild(t, ctx, binaries["relay"], nil, envFor(values), f.dir)
		waitReady(t, ctx, httpAddress)
		if i == 0 {
			fresh := f.pki.issue(t, aID, "relay-a", time.Hour)
			aAPI.update(t, aID, fresh, f.pki.ca.Raw)
		}
	}
	time.Sleep(7 * time.Second)
	return f
}

func waitReady(t *testing.T, ctx context.Context, address string) {
	t.Helper()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		request, e := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/readyz", nil)
		if e != nil {
			t.Fatal(e)
		}
		response, e := client.Do(request)
		if e == nil {
			_ = response.Body.Close()
			if response.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("runtime readiness deadline exceeded")
}
func (f *fixture) client(t *testing.T, relay, index int) (net.Conn, *json.Decoder) {
	t.Helper()
	conn, e := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 2 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.pki.roots, ServerName: "localhost"}}).DialContext(t.Context(), "tcp", f.public[relay])
	if e != nil {
		t.Fatal(e)
	}
	credential, e := testkit.Sign(f.private, "fixture", fmt.Sprintf("node-%d", index), time.Now().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	if e = json.NewEncoder(conn).Encode(wire.ClientHello{Type: wire.MessageClientHello, ProtocolVersion: 1, Credential: *credential, HeartbeatIntervalMS: 10000}); e != nil {
		_ = conn.Close()
		t.Fatal(e)
	}
	decoder := json.NewDecoder(bufio.NewReader(conn))
	var ready wire.Ready
	if e = decoder.Decode(&ready); e != nil || !ready.Ready {
		_ = conn.Close()
		t.Fatal("runtime rejected fixture credential")
	}
	return conn, decoder
}

func TestRuntimeMeshAndRevocation(t *testing.T) {
	f := newFixture(t)
	a, _ := f.client(t, 0, 0)
	defer func() { _ = a.Close() }()
	b, reader := f.client(t, 1, 1)
	defer func() { _ = b.Close() }()
	payload := []byte("opaque runtime mesh proof")
	if e := json.NewEncoder(a).Encode(wire.ClientFrame{Type: wire.MessageClientFrame, ProtocolVersion: 1, PeerID: "node-1", PeerNetworkID: "fixture", Payload: payload}); e != nil {
		t.Fatal(e)
	}
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	var frame wire.ServerFrame
	if e := reader.Decode(&frame); e != nil || !bytes.Equal(frame.Payload, payload) || frame.FromNodeID != "node-0" {
		t.Fatalf("mesh delivery: %v", e)
	}
	// A's original SVID expired during setup; success above proves automatic fresh handshakes.
	t.Log("real binaries: public TLS, PostgreSQL SPIFFE TLS, control, mesh and rotated Workload API PASS")
	f.authority.revoked.Store(true)
	started := time.Now()
	_ = b.SetReadDeadline(started.Add(15 * time.Second))
	for {
		var raw json.RawMessage
		e := reader.Decode(&raw)
		if e != nil {
			if timeout, ok := e.(net.Error); ok && timeout.Timeout() {
				t.Fatal("revocation exceeded15s")
			}
			break
		}
	}
	t.Logf("existing session terminated after revoke in %s", time.Since(started))
}
