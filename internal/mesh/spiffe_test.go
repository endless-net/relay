package mesh

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	relayv1 "github.com/endless-net/relay/api/relay/v1"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	spiffetls "github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"net"
	"testing"
	"time"
)

func TestRegressionSPIFFEMeshConnect(t *testing.T) {
	ca, key := testCA(t)
	td := spiffeid.RequireTrustDomainFromString("endlessnet.ru")
	bundle := x509bundle.FromX509Authorities(td, []*x509.Certificate{ca})
	svid := func(id string) *x509svid.SVID {
		cert := testRelayCertificate(t, ca, key, id)
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return &x509svid.SVID{ID: spiffeid.RequireFromString("spiffe://endlessnet.ru/relay/" + id), Certificates: []*x509.Certificate{leaf}, PrivateKey: cert.PrivateKey.(crypto.Signer)}
	}
	a, b := svid("a"), svid("b")
	clientTLS := spiffetls.MTLSClientConfig(a, bundle, spiffetls.AuthorizeMemberOf(td))
	clientTLS.MinVersion = tls.VersionTLS13
	serverTLS := spiffetls.MTLSServerConfig(b, bundle, spiffetls.AuthorizeMemberOf(td))
	serverTLS.MinVersion = tls.VersionTLS13
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	m := NewManager(context.Background(), "b", "boot-b", clientTLS, nil)
	defer m.Close()
	relayv1.RegisterRelayMeshServer(srv, m)
	m.UpdatePeers([]*relayv1.RelayInstance{{RelayId: "a", BootId: "boot-a", MeshAddr: "127.0.0.1:1"}})
	go srv.Serve(listener)
	defer srv.Stop()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", listener.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("baseline SPIFFE TLS failed: %v", err)
	}
	t.Logf("SPIFFE TLS succeeds; VerifiedChains=%d", len(conn.ConnectionState().VerifiedChains))
	conn.Close()
	sender := NewManager(context.Background(), "a", "boot-a", clientTLS, nil)
	defer sender.Close()
	peerConn := &peerConnection{instance: &relayv1.RelayInstance{RelayId: "b", BootId: "boot-b", MeshAddr: listener.Addr().String()}, send: make(chan *relayv1.MeshMessage, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sender.connectPeer(ctx, peerConn) }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for !peerConn.ready.Load() {
		select {
		case err := <-done:
			t.Fatalf("mesh rejects valid SPIFFE transport: %v", err)
		case <-deadline.C:
			t.Fatal("mesh not ready")
		case <-time.After(time.Millisecond):
		}
	}
}
