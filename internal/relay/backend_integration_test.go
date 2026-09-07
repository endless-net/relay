package relay_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	"github.com/endless-net/relay/internal/authz"
	"github.com/endless-net/relay/internal/mesh"
	"github.com/endless-net/relay/internal/relay"
	"github.com/endless-net/relay/internal/relaycontrol"
	"github.com/endless-net/relay/internal/relaycoordinator"
	"github.com/endless-net/relay/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// External acceptance processes supply a real compatible upstream over stdin.
// All runtime components and session storage are production implementations.
// Certificates are ephemeral test fixtures, not Workload API evidence.
func TestExternalBackendDataplane(t *testing.T) {
	if os.Getenv("RELAY_EXTERNAL_BACKEND_TEST") != "1" {
		t.Skip("requires an isolated external upstream and PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	decoder := json.NewDecoder(os.Stdin)
	decoder.DisallowUnknownFields()
	var input struct {
		UpstreamAddress string
		UpstreamCAPEM   []byte
	}
	if err := decoder.Decode(&input); err != nil {
		t.Fatal("invalid upstream handoff", err)
	}
	host, _, err := net.SplitHostPort(input.UpstreamAddress)
	if err != nil || host != "127.0.0.1" {
		t.Fatal("upstream must be an isolated loopback endpoint")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(input.UpstreamCAPEM) {
		t.Fatal("missing upstream test trust")
	}
	upstream, err := grpc.NewClient(input.UpstreamAddress, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots})))
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	storage := backendPostgres(t, ctx)
	authorizer := authz.NewCache(authz.GRPCAuthorizer{Client: relayv1.NewRelayUpstreamServiceClient(upstream)})
	ca, serverCertificate, relayCertificate := backendCertificates(t)
	localRoots := x509.NewCertPool()
	if !localRoots.AppendCertsFromPEM(ca) {
		t.Fatal("invalid runtime test trust")
	}
	serverTLS := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCertificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: localRoots}
	clientTLS := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{relayCertificate}, RootCAs: localRoots}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	controlServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.UnaryInterceptor(relayv1.RejectUnknownUnaryServerInterceptor))
	relayv1.RegisterRelayControlServer(controlServer, &relaycoordinator.Server{Store: storage, Authorizer: authorizer})
	defer controlServer.Stop()
	go func() { _ = controlServer.Serve(listener) }()
	controlConnection, err := relaycontrol.Dial(listener.Addr().String(), clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer controlConnection.Close()
	var dataplane *relay.Server
	manager := mesh.NewManager(ctx, "backend-relay", "backend-boot", clientTLS, func(network, from, destinationNetwork, to string, epoch int64, payload []byte) error {
		return dataplane.DeliverRemote(network, from, destinationNetwork, to, epoch, payload)
	})
	defer manager.Close()
	control := &relaycontrol.Client{RelayID: "backend-relay", BootID: "backend-boot", MeshAddr: listener.Addr().String(), Control: relayv1.NewRelayControlClient(controlConnection), Peers: manager}
	controlDone := make(chan error, 1)
	go func() { controlDone <- control.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-controlDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("control did not stop")
		}
	}()
	for !control.Ready() {
		select {
		case err := <-controlDone:
			controlDone <- err
			t.Fatal("control startup failed", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	dataplane, err = relay.NewServer(relay.ServerConfig{Addr: address, RelayID: "backend-relay", TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCertificate}}, TrustBundleProvider: control.TrustBundle, Metrics: relay.NewMetrics(), Control: control, Mesh: manager})
	if err != nil {
		t.Fatal(err)
	}
	dataplaneDone := make(chan error, 1)
	go func() { dataplaneDone <- dataplane.ListenAndServe(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-dataplaneDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("dataplane did not stop")
		}
	}()
	for !dataplane.Ready() {
		select {
		case err := <-dataplaneDone:
			dataplaneDone <- err
			t.Fatal("dataplane startup failed", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	ready, err := json.Marshal(struct {
		Address string
		CAPEM   []byte
	}{address, ca})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("ENDLESSNET_RELAY_READY %s\n", ready)
	var stop struct{}
	if err := decoder.Decode(&stop); !errors.Is(err, io.EOF) {
		t.Fatal("parent must close stdin after acceptance", err)
	}
}

func backendPostgres(t *testing.T, ctx context.Context) *store.Postgres {
	t.Helper()
	dsn := os.Getenv("RELAY_BACKEND_DATABASE_URL")
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatal("isolated PostgreSQL required")
	}
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("relay_backend_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	storage, err := store.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.Close)
	return storage
}

func backendCertificates(t *testing.T) ([]byte, tls.Certificate, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, identity string) tls.Certificate {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(identity)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, URIs: []*url.URL{uri}}
		raw, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{raw, der}, PrivateKey: leafKey}
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), issue(2, "spiffe://endlessnet.ru/service/relay-coordinator"), issue(3, "spiffe://endlessnet.ru/relay/backend-relay")
}
