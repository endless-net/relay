package testserver

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

// Start creates a real HTTP/2 TLS endpoint with automatic test cleanup.
// Certificates are supplied by the test; this does not replace SPIRE E2E.
func (s *Server) Start(t testing.TB, config *tls.Config) *httptest.Server {
	t.Helper()
	if config == nil || config.MinVersion < tls.VersionTLS13 || (config.ClientAuth != tls.RequireAndVerifyClientCert && config.VerifyPeerCertificate == nil && config.VerifyConnection == nil) {
		t.Fatal("test upstream requires TLS 1.3 and authenticated client certificates")
	}
	host := httptest.NewUnstartedServer(s.Handler())
	host.EnableHTTP2 = true
	host.TLS = config.Clone()
	host.StartTLS()
	t.Cleanup(host.Close)
	return host
}
