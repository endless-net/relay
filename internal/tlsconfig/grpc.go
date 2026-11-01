package tlsconfig

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
)

// GRPCCredentials closes established transports before either SVID expires.
// A new handshake fetches the currently rotated Workload API material.
func GRPCCredentials(config *tls.Config) credentials.TransportCredentials {
	return &leaseCredentials{config: config.Clone()}
}

type leaseCredentials struct{ config *tls.Config }

func (c *leaseCredentials) Info() credentials.ProtocolInfo {
	return credentials.NewTLS(c.config).Info()
}
func (c *leaseCredentials) Clone() credentials.TransportCredentials { return GRPCCredentials(c.config) }
func (c *leaseCredentials) OverrideServerName(name string) error {
	c.config.ServerName = name
	return nil
}

func (c *leaseCredentials) frozen(client bool) (*tls.Config, time.Time, error) {
	cfg := c.config.Clone()
	var cert *tls.Certificate
	var e error
	if client && cfg.GetClientCertificate != nil {
		cert, e = cfg.GetClientCertificate(&tls.CertificateRequestInfo{})
	} else if !client && cfg.GetCertificate != nil {
		cert, e = cfg.GetCertificate(&tls.ClientHelloInfo{})
	} else if len(cfg.Certificates) > 0 {
		cert = &cfg.Certificates[0]
	}
	if e != nil || cert == nil || len(cert.Certificate) == 0 {
		return nil, time.Time{}, errors.New("workload_certificate_unavailable")
	}
	leaf, e := x509.ParseCertificate(cert.Certificate[0])
	if e != nil {
		return nil, time.Time{}, errors.New("workload_certificate_invalid")
	}
	expiry := leaf.NotAfter.Add(-time.Second)
	if !time.Now().Before(expiry) {
		return nil, time.Time{}, errors.New("workload_certificate_expired")
	}
	if client {
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert, nil }
	} else {
		cfg.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert, nil }
	}
	return cfg, expiry, nil
}

func (c *leaseCredentials) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	cfg, expiry, e := c.frozen(true)
	if e != nil {
		return nil, nil, e
	}
	conn, info, e := credentials.NewTLS(cfg).ClientHandshake(ctx, authority, raw)
	if e != nil {
		return nil, nil, e
	}
	return bindExpiry(conn, info, expiry)
}
func (c *leaseCredentials) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	cfg, expiry, e := c.frozen(false)
	if e != nil {
		return nil, nil, e
	}
	conn, info, e := credentials.NewTLS(cfg).ServerHandshake(raw)
	if e != nil {
		return nil, nil, e
	}
	return bindExpiry(conn, info, expiry)
}

type expiringConn struct {
	expiry time.Time
	closed bool
	net.Conn
	timer *time.Timer
	mu    sync.Mutex
	once  sync.Once
	err   error
}

func (c *expiringConn) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		if c.timer != nil {
			c.timer.Stop()
		}
		c.mu.Unlock()
		c.err = c.Conn.Close()
	})
	return c.err
}
func bindExpiry(conn net.Conn, info credentials.AuthInfo, expiry time.Time) (net.Conn, credentials.AuthInfo, error) {
	verified, ok := info.(credentials.TLSInfo)
	if !ok || len(verified.State.PeerCertificates) == 0 {
		_ = conn.Close()
		return nil, nil, errors.New("workload_peer_missing")
	}
	peerEnd := verified.State.PeerCertificates[0].NotAfter.Add(-time.Second)
	if peerEnd.Before(expiry) {
		expiry = peerEnd
	}
	if !time.Now().Before(expiry) {
		_ = conn.Close()
		return nil, nil, errors.New("workload_peer_expired")
	}
	bounded, e := ExpireConnection(conn, expiry)
	if e != nil {
		return nil, nil, e
	}
	return bounded, info, nil
}

// ExpireConnection can only shorten a live connection's authenticated lifetime.
func ExpireConnection(conn net.Conn, expiry time.Time) (net.Conn, error) {
	c, ok := conn.(*expiringConn)
	if !ok {
		c = &expiringConn{Conn: conn}
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("connection_closed")
	}
	if c.expiry.IsZero() || expiry.Before(c.expiry) {
		if c.timer != nil {
			c.timer.Stop()
		}
		c.expiry = expiry
		c.timer = time.AfterFunc(max(time.Duration(0), time.Until(expiry)), func() { _ = c.Close() })
	}
	c.mu.Unlock()
	return c, nil
}
