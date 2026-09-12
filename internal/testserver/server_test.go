package testserver_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/endless-net/relay/internal/authz"
	"github.com/endless-net/relay/internal/testserver"
	"github.com/endless-net/relay/internal/tlsconfig"
	protocol "github.com/endless-net/relay/relayapi/v1"
	rpc "github.com/endless-net/relay/relayapi/v1/relayapigrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// This test uses real HTTP/2, TLS, signatures and the production upstream
// adapter. Full runtime, PostgreSQL and Workload API coverage lives in e2e.
func TestUpstreamTestServer(t *testing.T) {
	policy, err := tlsconfig.NewIdentityPolicy("operator.example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := protocol.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	cfg := testserver.Config{NetworkID: "network", Nodes: []string{"alice", "bob"}, PeerPairs: []testserver.PeerPair{{From: "alice", To: "bob"}}, TrustBundle: bundle}
	server := testserver.New(cfg, policy)
	// Mutating caller-owned configuration must not change the running peer.
	cfg.Nodes[0] = "removed"
	roots, issue := certificates(t)
	serverCert := issue(2, policy.UpstreamID)
	host := server.Start(t, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots})
	clientTLS := func(identity string) *tls.Config {
		return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{issue(3, identity)}, RootCAs: roots, VerifyConnection: func(state tls.ConnectionState) error {
			id, err := tlsconfig.PeerID(state)
			if err != nil || id.String() != policy.UpstreamID {
				return errors.New("unexpected upstream identity")
			}
			return nil
		}}
	}
	connect := func(identity string) rpc.RelayUpstreamServiceClient {
		t.Helper()
		conn, err := grpc.NewClient(strings.TrimPrefix(host.URL, "https://"), grpc.WithTransportCredentials(credentials.NewTLS(clientTLS(identity))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return rpc.NewRelayUpstreamServiceClient(conn)
	}
	client := connect(policy.CoordinatorID)
	adapter := authz.GRPCAuthorizer{Client: client}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	credential, err := protocol.Sign(key, "network", "alice", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	control := func(identity, body string, want int) {
		t.Helper()
		transport := &http.Transport{TLSClientConfig: clientTLS(identity), ForceAttemptHTTP2: true}
		defer transport.CloseIdleConnections()
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, host.URL+"/test/control", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response, err := (&http.Client{Transport: transport, Timeout: time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		if response.StatusCode != want {
			t.Fatalf("control status %d, want %d", response.StatusCode, want)
		}
	}
	t.Run("trust_and_signed_credential", func(t *testing.T) {
		got, err := adapter.RelayTrustBundle(ctx)
		if err != nil || got.ActiveKeyID != bundle.ActiveKeyID {
			t.Fatal("trust mismatch", err)
		}
		if err := adapter.AuthorizeCredential(ctx, *credential); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("directed_acl_and_network_isolation", func(t *testing.T) {
		if err := adapter.AuthorizePeer(ctx, *credential, "network", "bob"); err != nil {
			t.Fatal(err)
		}
		if err := adapter.AuthorizePeer(ctx, *credential, "other", "bob"); !errors.Is(err, authz.ErrDenied) {
			t.Fatal("cross-network access was not denied", err)
		}
		bob, err := protocol.Sign(key, "network", "bob", time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if err := adapter.AuthorizePeer(ctx, *bob, "network", "alice"); !errors.Is(err, authz.ErrDenied) {
			t.Fatal("reverse access was not denied", err)
		}
	})
	t.Run("invalid_signature_and_expiry", func(t *testing.T) {
		invalid := *credential
		invalid.NodeID = "bob"
		if err := adapter.AuthorizeCredential(ctx, invalid); !errors.Is(err, authz.ErrDenied) {
			t.Fatal("tampered credential was not denied", err)
		}
		expired, err := protocol.Sign(key, "network", "alice", time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if err := adapter.AuthorizeCredential(ctx, *expired); !errors.Is(err, authz.ErrDenied) {
			t.Fatal("expired credential was not denied", err)
		}
	})
	t.Run("unknown_fields_and_wrong_identity", func(t *testing.T) {
		request := &rpc.GetTrustBundleRequest{}
		request.ProtoReflect().SetUnknown([]byte{0x78, 1})
		if _, err := client.GetTrustBundle(ctx, request); status.Code(err) != codes.InvalidArgument {
			t.Fatal("unknown field accepted", err)
		}
		if _, err := connect("spiffe://operator.example/service/other").GetTrustBundle(ctx, &rpc.GetTrustBundleRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Fatal("wrong identity accepted", err)
		}
		control(policy.CoordinatorID, `{}`, http.StatusForbidden)
	})
	t.Run("outage_deadline_and_recovery", func(t *testing.T) {
		control(policy.UpstreamID, `{"mode":"unavailable"}`, http.StatusNoContent)
		if err := adapter.AuthorizeCredential(ctx, *credential); status.Code(err) != codes.Unavailable {
			t.Fatal("outage not observed", err)
		}
		control(policy.UpstreamID, `{"mode":"hang"}`, http.StatusNoContent)
		short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
		defer stop()
		if err := adapter.AuthorizeCredential(short, *credential); status.Code(err) != codes.DeadlineExceeded {
			t.Fatal("deadline not observed", err)
		}
		control(policy.UpstreamID, `{}`, http.StatusNoContent)
		if err := adapter.AuthorizeCredential(ctx, *credential); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("strict_control_and_revocation", func(t *testing.T) {
		control(policy.UpstreamID, `{} {}`, http.StatusBadRequest)
		control(policy.UpstreamID, `{"delay_ms":-1}`, http.StatusBadRequest)
		control(policy.UpstreamID, `{"mode":"deny"}`, http.StatusNoContent)
		if err := adapter.AuthorizeCredential(ctx, *credential); !errors.Is(err, authz.ErrDenied) {
			t.Fatal("revocation not observed", err)
		}
		control(policy.UpstreamID, `{}`, http.StatusNoContent)
		cfg.Nodes = []string{"bob"}
		raw, err := json.Marshal(map[string]any{"config": cfg})
		if err != nil {
			t.Fatal(err)
		}
		control(policy.UpstreamID, string(raw), http.StatusNoContent)
		if err := adapter.AuthorizeCredential(ctx, *credential); !errors.Is(err, authz.ErrDenied) {
			t.Fatal("removed node was not denied", err)
		}
		cfg.Nodes = []string{"alice", "bob"}
		raw, err = json.Marshal(map[string]any{"config": cfg})
		if err != nil {
			t.Fatal(err)
		}
		control(policy.UpstreamID, string(raw), http.StatusNoContent)
		if err := adapter.AuthorizeCredential(ctx, *credential); err != nil {
			t.Fatal(err)
		}
	})
}

// Fixture certificates stay in memory. SPIRE and SVID rotation are not mocked
// in the full product suite; these static identities only speed up peer tests.
func certificates(t *testing.T) (*x509.CertPool, func(int64, string) tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	raw, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return roots, func(serial int64, identity string) tls.Certificate {
		t.Helper()
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(identity)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, URIs: []*url.URL{uri}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der, raw}, PrivateKey: leafKey}
	}
}
