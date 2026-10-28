package store

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"time"

	"github.com/endless-net/relay/internal/tlsconfig"
	"github.com/jackc/pgx/v5"
)

// freezeIdentity binds each database connection to the SVID used in its handshake.
// The pool fetches fresh Workload API material on the next connection.
func freezeIdentity(_ context.Context, c *pgx.ConnConfig) error {
	cfg := c.TLSConfig.Clone()
	certificate, e := cfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if e != nil || certificate == nil || len(certificate.Certificate) == 0 {
		return errors.New("registry_identity_unavailable")
	}
	leaf, e := x509.ParseCertificate(certificate.Certificate[0])
	if e != nil || !time.Now().Add(3*time.Second).Before(leaf.NotAfter) {
		return errors.New("registry_identity_expired")
	}
	cfg.Certificates = []tls.Certificate{*certificate}
	cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return certificate, nil }
	c.TLSConfig = cfg
	dial := c.DialFunc
	until := leaf.NotAfter.Add(-time.Second)
	c.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, e := dial(ctx, network, address)
		if e != nil {
			return nil, e
		}
		return tlsconfig.ExpireConnection(conn, until)
	}
	return nil
}

func bindDatabasePeer(_ context.Context, c *pgx.Conn) error {
	conn, ok := c.PgConn().Conn().(*tls.Conn)
	if !ok {
		return errors.New("registry_tls_missing")
	}
	state := conn.ConnectionState()
	if _, e := tlsconfig.PeerID(state); e != nil {
		return errors.New("registry_peer_untrusted")
	}
	_, e := tlsconfig.ExpireConnection(conn.NetConn(), state.PeerCertificates[0].NotAfter.Add(-time.Second))
	return e
}

func reusableIdentity(c *pgx.Conn) bool {
	if c.IsClosed() {
		return false
	}
	cfg := c.Config()
	if cfg.TLSConfig == nil || len(cfg.TLSConfig.Certificates) == 0 {
		return false
	}
	certificate := cfg.TLSConfig.Certificates[0]
	if len(certificate.Certificate) == 0 {
		return false
	}
	own, e := x509.ParseCertificate(certificate.Certificate[0])
	if e != nil || !time.Now().Add(3*time.Second).Before(own.NotAfter) {
		return false
	}
	conn, ok := c.PgConn().Conn().(*tls.Conn)
	if !ok {
		return false
	}
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return false
	}
	return time.Now().Add(3 * time.Second).Before(state.PeerCertificates[0].NotAfter)
}
