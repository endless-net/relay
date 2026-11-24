package tlsconfig

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	spiffetlsconfig "github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

const (
	DefaultTrustDomain = "endlessnet.ru"
	DefaultWorkloadAPI = "unix:///run/spire/sockets/agent.sock"
)

type x509Source interface {
	x509svid.Source
	x509bundle.Source
}

type WorkloadRuntime struct {
	source        x509Source
	trustDomain   spiffeid.TrustDomain
	closeWorkload func() error
}

func NewWorkloadRuntime(ctx context.Context, workloadAPIAddr, expectedIdentity string) (*WorkloadRuntime, error) {
	address := strings.TrimSpace(workloadAPIAddr)
	if address == "" {
		address = DefaultWorkloadAPI
	}
	source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(address)))
	if err != nil {
		return nil, fmt.Errorf("connect to SPIFFE Workload API: %w", err)
	}
	runtime, err := newWorkloadRuntimeWithSource(source, expectedIdentity)
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	runtime.closeWorkload = source.Close
	return runtime, nil
}

func newWorkloadRuntimeWithSource(source x509Source, expectedIdentity string) (*WorkloadRuntime, error) {
	if source == nil {
		return nil, errors.New("SPIFFE X.509 source is required")
	}
	expected, err := spiffeid.FromString(strings.TrimSpace(expectedIdentity))
	if err != nil {
		return nil, fmt.Errorf("parse expected SPIFFE workload identity: %w", err)
	}
	trustDomain := expected.TrustDomain()
	svid, err := source.GetX509SVID()
	if err != nil {
		return nil, fmt.Errorf("fetch SPIFFE X.509-SVID: %w", err)
	}
	if svid == nil || svid.ID != expected {
		actual := "<missing>"
		if svid != nil {
			actual = svid.ID.String()
		}
		return nil, fmt.Errorf("SPIFFE workload identity %q does not match expected %q", actual, expected.String())
	}
	return &WorkloadRuntime{source: expectedIdentitySource{source: source, expected: expected}, trustDomain: trustDomain}, nil
}

type expectedIdentitySource struct {
	source   x509Source
	expected spiffeid.ID
}

func (s expectedIdentitySource) GetX509SVID() (*x509svid.SVID, error) {
	svid, err := s.source.GetX509SVID()
	if err != nil {
		return nil, err
	}
	if svid == nil || svid.ID != s.expected {
		return nil, errors.New("SPIFFE workload identity changed unexpectedly")
	}
	return svid, nil
}

func (s expectedIdentitySource) GetX509BundleForTrustDomain(trustDomain spiffeid.TrustDomain) (*x509bundle.Bundle, error) {
	return s.source.GetX509BundleForTrustDomain(trustDomain)
}

func (r *WorkloadRuntime) Close() error {
	if r == nil || r.closeWorkload == nil {
		return nil
	}
	return r.closeWorkload()
}

func (r *WorkloadRuntime) ServerTLSConfig() (*tls.Config, error) {
	if r == nil || r.source == nil {
		return nil, errors.New("SPIFFE workload runtime is required")
	}
	config := spiffetlsconfig.MTLSServerConfig(r.source, r.source, spiffetlsconfig.AuthorizeMemberOf(r.trustDomain))
	config.MinVersion = tls.VersionTLS13
	return config, nil
}

func (r *WorkloadRuntime) ClientTLSConfig(expectedPeer string) (*tls.Config, error) {
	if r == nil || r.source == nil {
		return nil, errors.New("SPIFFE workload runtime is required")
	}
	peerID, err := spiffeid.FromString(strings.TrimSpace(expectedPeer))
	if err != nil {
		return nil, fmt.Errorf("parse expected SPIFFE peer identity: %w", err)
	}
	if peerID.TrustDomain() != r.trustDomain {
		return nil, errors.New("SPIFFE peer belongs to an unexpected trust domain")
	}
	config := spiffetlsconfig.MTLSClientConfig(r.source, r.source, spiffetlsconfig.AuthorizeID(peerID))
	config.MinVersion = tls.VersionTLS13
	return config, nil
}

type DatabaseTransport struct {
	TLSConfig   *tls.Config
	VerifyLease func(tls.Certificate, tls.ConnectionState) (time.Time, error)
}

// DatabaseTLSConfig binds the SPIFFE-authenticated PostgreSQL server to its DNS name.
func (r *WorkloadRuntime) DatabaseTLSConfig(expectedPeer, serverName string) (DatabaseTransport, error) {
	config, err := r.ClientTLSConfig(expectedPeer)
	if err != nil {
		return DatabaseTransport{}, err
	}
	config.ServerName = serverName
	config.MaxVersion = tls.VersionTLS13
	config.SessionTicketsDisabled = true
	verifyPeer := func(certs []*x509.Certificate) ([][]*x509.Certificate, error) {
		identity, _, err := x509svid.Verify(certs, r.source)
		if err != nil || identity.String() != expectedPeer {
			return nil, errors.New("database_peer_untrusted")
		}
		bundle, err := r.source.GetX509BundleForTrustDomain(r.trustDomain)
		if err != nil || bundle == nil {
			return nil, errors.New("database_peer_untrusted")
		}
		roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
		for _, certificate := range bundle.X509Authorities() {
			roots.AddCert(certificate)
		}
		for _, certificate := range certs[1:] {
			intermediates.AddCert(certificate)
		}
		chains, err := certs[0].Verify(x509.VerifyOptions{
			DNSName: serverName, Roots: roots, Intermediates: intermediates,
		})
		if err != nil {
			return nil, errors.New("database_dns_untrusted")
		}
		return chains, nil
	}
	transport := DatabaseTransport{TLSConfig: config}
	transport.VerifyLease = func(local tls.Certificate, state tls.ConnectionState) (time.Time, error) {
		certs := make([]*x509.Certificate, 0, len(local.Certificate))
		for _, der := range local.Certificate {
			certificate, err := x509.ParseCertificate(der)
			if err != nil {
				return time.Time{}, err
			}
			certs = append(certs, certificate)
		}
		_, localChains, err := x509svid.Verify(certs, r.source)
		if err != nil {
			return time.Time{}, err
		}
		peerChains, err := verifyPeer(state.PeerCertificates)
		if err != nil {
			return time.Time{}, err
		}
		expiry := certs[0].NotAfter
		for _, chain := range append(localChains, peerChains...) {
			for _, certificate := range chain {
				if certificate.NotAfter.Before(expiry) {
					expiry = certificate.NotAfter
				}
			}
		}
		return expiry, nil
	}
	return transport, nil
}

func (r *WorkloadRuntime) ClientTLSConfigForTrustDomain() (*tls.Config, error) {
	if r == nil || r.source == nil {
		return nil, errors.New("SPIFFE workload runtime is required")
	}
	config := spiffetlsconfig.MTLSClientConfig(r.source, r.source, spiffetlsconfig.AuthorizeMemberOf(r.trustDomain))
	config.MinVersion = tls.VersionTLS13
	return config, nil
}

func PublicServer(certFile, keyFile string) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(strings.TrimSpace(certFile), strings.TrimSpace(keyFile))
	if err != nil {
		return nil, fmt.Errorf("load public relay certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13}, nil
}

// Healthy checks the currently rotated local SVID; old stream peer expiry is checked separately.
func (r *WorkloadRuntime) Healthy() bool {
	if r == nil || r.source == nil {
		return false
	}
	s, e := r.source.GetX509SVID()
	if e != nil || s == nil || len(s.Certificates) == 0 {
		return false
	}
	return svidWithinValidityWindow(s.Certificates[0], time.Now())
}
