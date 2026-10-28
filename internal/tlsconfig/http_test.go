//go:build integration

package tlsconfig

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTP2ConnectionExpiresWithItsOriginalClientSVID(t *testing.T) {
	ca, caKey := tlsTestCA(t)
	raw, key, leaf := tlsTestCertificate(t, ca, caKey, "spiffe://endlessnet.ru/service/coordinator")
	serverCert := tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: key}
	leaf.NotAfter = time.Now().Add(4 * time.Second)
	leaf.SerialNumber = big.NewInt(3)
	short, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[tls.Certificate]
	current.Store(&tls.Certificate{Certificate: [][]byte{short}, PrivateKey: key})
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Error("HTTP/2 not negotiated")
		}
		_, _ = io.WriteString(w, r.TLS.PeerCertificates[0].SerialNumber.String())
	}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()
	cfg := &tls.Config{RootCAs: roots, ServerName: "coordinator", MinVersion: tls.VersionTLS13, GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return current.Load(), nil }}
	transport := &http.Transport{TLSClientConfig: cfg, DialTLSContext: HTTPDialTLS(cfg), ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	request := func() string {
		t.Helper()
		response, e := client.Get(srv.URL)
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = response.Body.Close() }()
		body, e := io.ReadAll(response.Body)
		if e != nil {
			t.Fatal(e)
		}
		return string(body)
	}
	if request() != "3" {
		t.Fatal("short client certificate not used")
	}
	current.Store(&serverCert)
	// Rotation must not extend the authentication of an existing HTTP/2 pool entry.
	time.Sleep(time.Until(leaf.NotAfter) + 100*time.Millisecond)
	if request() != "2" {
		t.Fatal("expired handshake identity survived rotation")
	}
}
