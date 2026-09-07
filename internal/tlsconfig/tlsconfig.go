package tlsconfig

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	spiffetlsconfig "github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

const (
	DefaultTrustDomain  = "endlessnet.ru"
	DefaultWorkloadAPI  = "unix:///run/spire/sockets/agent.sock"
	ProviderSPIFFE      = "spiffe"
	ProviderDevelopment = "development_file"
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

func MutualServer(certFile, keyFile, caFile string) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(strings.TrimSpace(certFile), strings.TrimSpace(keyFile))
	if err != nil {
		return nil, fmt.Errorf("load service server certificate: %w", err)
	}
	roots, err := certPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13}, nil
}

func MutualClient(certFile, keyFile, caFile, serverName, expectedServerURI, expectedClientURI string) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(strings.TrimSpace(certFile), strings.TrimSpace(keyFile))
	if err != nil {
		return nil, fmt.Errorf("load service client certificate: %w", err)
	}
	if len(certificate.Certificate) == 0 {
		return nil, errors.New("service client certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse service client certificate: %w", err)
	}
	if expectedClientURI != "" && !hasSingleURI(leaf, expectedClientURI) {
		return nil, fmt.Errorf("service client certificate does not contain identity %q", expectedClientURI)
	}
	certificate.Leaf = leaf
	roots, err := certPool(caFile)
	if err != nil {
		return nil, err
	}
	config := &tls.Config{Certificates: []tls.Certificate{certificate}, RootCAs: roots, ServerName: strings.TrimSpace(serverName), MinVersion: tls.VersionTLS13}
	if expectedServerURI != "" {
		config.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 || !hasSingleURI(state.VerifiedChains[0][0], expectedServerURI) {
				return fmt.Errorf("service server certificate does not contain identity %q", expectedServerURI)
			}
			return nil
		}
	}
	return config, nil
}

func certPool(path string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return nil, fmt.Errorf("load service CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, errors.New("service CA file contains no certificates")
	}
	return pool, nil
}

func hasSingleURI(certificate *x509.Certificate, expected string) bool {
	return certificate != nil && len(certificate.URIs) == 1 && certificate.URIs[0] != nil && certificate.URIs[0].String() == expected
}
