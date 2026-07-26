package tlsconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

type staticWorkloadSource struct {
	svid *x509svid.SVID
}

func (s staticWorkloadSource) GetX509SVID() (*x509svid.SVID, error) { return s.svid, nil }

func (staticWorkloadSource) GetX509BundleForTrustDomain(spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return x509bundle.New(spiffeid.RequireTrustDomainFromString(DefaultTrustDomain)), nil
}

func TestWorkloadRuntimeRequiresExactOwnIdentityAndPeerTrustDomain(t *testing.T) {
	identity := spiffeid.RequireFromString("spiffe://endlessnet.ru/relay/relay-a")
	runtime, err := newWorkloadRuntimeWithSource(staticWorkloadSource{svid: &x509svid.SVID{ID: identity}}, identity.String())
	if err != nil {
		t.Fatal(err)
	}
	serverConfig, err := runtime.ServerTLSConfig()
	if err != nil || serverConfig.MinVersion != tls.VersionTLS13 || serverConfig.GetCertificate == nil {
		t.Fatalf("SPIFFE server TLS config = %#v, %v", serverConfig, err)
	}
	clientConfig, err := runtime.ClientTLSConfig("spiffe://endlessnet.ru/service/relay-coordinator")
	if err != nil || clientConfig.MinVersion != tls.VersionTLS13 || clientConfig.GetClientCertificate == nil {
		t.Fatalf("SPIFFE client TLS config = %#v, %v", clientConfig, err)
	}
	if _, err := runtime.ClientTLSConfig("spiffe://other.example/service/relay-coordinator"); err == nil {
		t.Fatal("peer from another trust domain was accepted")
	}
	if _, err := newWorkloadRuntimeWithSource(staticWorkloadSource{svid: &x509svid.SVID{ID: identity}}, "spiffe://endlessnet.ru/relay/relay-b"); err == nil {
		t.Fatal("unexpected own workload identity was accepted")
	}
}

func TestTLSConfigsRequireTLS13AndExactWorkloadIdentity(t *testing.T) {
	ca, caKey := tlsTestCA(t)
	identity := "spiffe://endlessnet.ru/relay/relay-a"
	certificate, key, leaf := tlsTestCertificate(t, ca, caKey, identity)
	directory := t.TempDir()
	caFile := filepath.Join(directory, "ca.pem")
	certFile := filepath.Join(directory, "cert.pem")
	keyFile := filepath.Join(directory, "key.pem")
	writeTLSFile(t, caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw}))
	writeTLSFile(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}))
	rawKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeTLSFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: rawKey}))

	publicConfig, err := PublicServer(certFile, keyFile)
	if err != nil || publicConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("public TLS config = %#v, %v", publicConfig, err)
	}
	serverConfig, err := MutualServer(certFile, keyFile, caFile)
	if err != nil || serverConfig.MinVersion != tls.VersionTLS13 || serverConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("mutual server TLS config = %#v, %v", serverConfig, err)
	}
	clientConfig, err := MutualClient(certFile, keyFile, caFile, "coordinator", "spiffe://endlessnet.ru/service/coordinator", identity)
	if err != nil || clientConfig.MinVersion != tls.VersionTLS13 || clientConfig.VerifyConnection == nil {
		t.Fatalf("mutual client TLS config = %#v, %v", clientConfig, err)
	}
	if _, err := MutualClient(certFile, keyFile, caFile, "coordinator", "", "spiffe://endlessnet.ru/relay/other"); err == nil {
		t.Fatal("client certificate with the wrong workload identity was accepted")
	}
	serverIdentity, err := url.Parse("spiffe://endlessnet.ru/service/coordinator")
	if err != nil {
		t.Fatal(err)
	}
	remote := *leaf
	remote.URIs = []*url.URL{serverIdentity}
	if err := clientConfig.VerifyConnection(tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{&remote, ca}}}); err != nil {
		t.Fatalf("matching server workload identity: %v", err)
	}
	remote.URIs = leaf.URIs
	if err := clientConfig.VerifyConnection(tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{&remote, ca}}}); err == nil {
		t.Fatal("wrong server workload identity was accepted")
	}
}

func tlsTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

func tlsTestCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, identity string) ([]byte, *ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "relay-a"}, DNSNames: []string{"coordinator"}, URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw, key, certificate
}

func writeTLSFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}
