package tlsconfig

import (
	"crypto/tls"
	"errors"
	"testing"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

type rotatingSource struct {
	svid *x509svid.SVID
	err  error
}

func (s *rotatingSource) GetX509SVID() (*x509svid.SVID, error) { return s.svid, s.err }
func (s *rotatingSource) GetX509BundleForTrustDomain(domain spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return x509bundle.New(domain), s.err
}

func TestWorkloadSourceRevalidatesIdentityOnEveryRotation(t *testing.T) {
	id := spiffeid.RequireFromString("spiffe://operator.example/relay/a")
	source := &rotatingSource{svid: &x509svid.SVID{ID: id}}
	runtime, err := newWorkloadRuntimeWithSource(source, id.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.source.GetX509SVID(); err != nil {
		t.Fatal(err)
	}
	source.svid = &x509svid.SVID{ID: spiffeid.RequireFromString("spiffe://operator.example/relay/b")}
	if _, err := runtime.source.GetX509SVID(); err == nil {
		t.Fatal("rotated to another identity")
	}
	source.svid = nil
	if _, err := runtime.source.GetX509SVID(); err == nil {
		t.Fatal("missing SVID accepted")
	}
	source.err = errors.New("source offline")
	if _, err := runtime.source.GetX509SVID(); !errors.Is(err, source.err) {
		t.Fatal("source error hidden", err)
	}
	if _, err := runtime.source.GetX509BundleForTrustDomain(id.TrustDomain()); !errors.Is(err, source.err) {
		t.Fatal("bundle error hidden", err)
	}
	source.err = nil
	source.svid = &x509svid.SVID{ID: id}
	if _, err := runtime.source.GetX509SVID(); err != nil {
		t.Fatal("source did not recover", err)
	}
	config, err := runtime.ClientTLSConfigForTrustDomain()
	if err != nil || config.MinVersion != tls.VersionTLS13 || config.GetClientCertificate == nil {
		t.Fatal("invalid mesh TLS config", err)
	}
	closed := false
	runtime.closeWorkload = func() error { closed = true; return nil }
	if err := runtime.Close(); err != nil || !closed {
		t.Fatal("workload source not closed", err)
	}
	var empty *WorkloadRuntime
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := empty.ClientTLSConfigForTrustDomain(); err == nil {
		t.Fatal("nil runtime accepted")
	}
}
