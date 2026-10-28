package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
)

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
