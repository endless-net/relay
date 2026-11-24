package store

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"time"

	"github.com/endless-net/relay/internal/tlsconfig"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func transportFreezer(transport tlsconfig.DatabaseTransport) func(context.Context, *pgx.ConnConfig) error {
	return func(_ context.Context, c *pgx.ConnConfig) error {
		return freezeIdentity(c, transport)
	}
}

// freezeIdentity binds each database connection to the SVID used in its handshake.
// The pool fetches fresh Workload API material on the next connection.
func freezeIdentity(c *pgx.ConnConfig, transport tlsconfig.DatabaseTransport) error {
	cfg := c.TLSConfig.Clone()
	certificate, e := cfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	if e != nil || certificate == nil || len(certificate.Certificate) == 0 {
		return errRegistryTrust
	}
	leaf, e := x509.ParseCertificate(certificate.Certificate[0])
	if e != nil || !time.Now().Add(3*time.Second).Before(leaf.NotAfter) {
		return errRegistryTrust
	}
	cfg.Certificates = []tls.Certificate{*certificate}
	var clientCertificateRequested bool
	cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		clientCertificateRequested = true
		return certificate, nil
	}
	if cfg.ServerName != "" {
		if transport.VerifyLease == nil {
			return errRegistryTrust
		}
		c.AfterNetConnect = func(ctx context.Context, _ *pgconn.Config, underlying net.Conn) (net.Conn, error) {
			secured, ok := underlying.(*tls.Conn)
			if !ok {
				return underlying, errRegistryTrust
			}
			clientCertificateRequested = false
			if err := secured.HandshakeContext(ctx); err != nil {
				return underlying, err
			}
			if !clientCertificateRequested {
				return underlying, errRegistryTrust
			}
			return underlying, verifyDatabaseLease(secured, *certificate, transport)
		}
	}
	if verify := cfg.VerifyPeerCertificate; verify != nil {
		cfg.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
			if err := verify(raw, chains); err != nil {
				return errRegistryTrust
			}
			return nil
		}
	}
	if verify := cfg.VerifyConnection; verify != nil {
		cfg.VerifyConnection = func(state tls.ConnectionState) error {
			if err := verify(state); err != nil {
				return errRegistryTrust
			}
			return nil
		}
	}
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
		return errRegistryTrust
	}
	state := conn.ConnectionState()
	if _, e := tlsconfig.PeerID(state); e != nil {
		return errRegistryTrust
	}
	until := state.PeerCertificates[0].NotAfter.Add(-time.Second)
	_, e := tlsconfig.ExpireConnection(conn.NetConn(), until)
	return e
}

func verifyDatabaseLease(conn *tls.Conn, local tls.Certificate, transport tlsconfig.DatabaseTransport) error {
	expiry, err := transport.VerifyLease(local, conn.ConnectionState())
	if err != nil || !time.Now().Add(3*time.Second).Before(expiry) {
		return errRegistryTrust
	}
	_, err = tlsconfig.ExpireConnection(conn.NetConn(), expiry.Add(-time.Second))
	return err
}

func reusableIdentity(c *pgx.Conn, transport tlsconfig.DatabaseTransport) bool {
	if c.IsClosed() {
		return false
	}
	cfg := c.Config()
	if cfg.TLSConfig == nil || len(cfg.TLSConfig.Certificates) == 0 {
		return false
	}
	certificate := cfg.TLSConfig.Certificates[0]
	conn, ok := c.PgConn().Conn().(*tls.Conn)
	if !ok {
		return false
	}
	if transport.VerifyLease != nil {
		return verifyDatabaseLease(conn, certificate, transport) == nil
	}
	if len(certificate.Certificate) == 0 {
		return false
	}
	own, e := x509.ParseCertificate(certificate.Certificate[0])
	if e != nil || !time.Now().Add(3*time.Second).Before(own.NotAfter) {
		return false
	}
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return false
	}
	return time.Now().Add(3 * time.Second).Before(state.PeerCertificates[0].NotAfter)
}
