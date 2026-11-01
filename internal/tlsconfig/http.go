package tlsconfig

import (
	"context"
	"crypto/tls"
	"net"
	"time"
)

// HTTPDialTLS freezes the certificate actually used by a pooled HTTP transport.
// The outer connection remains *tls.Conn so net/http can negotiate HTTP/2.
func HTTPDialTLS(config *tls.Config) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		cfg, expiry, err := (&leaseCredentials{config: config}).frozen(true)
		if err != nil {
			return nil, err
		}
		cfg.NextProtos = []string{"h2", "http/1.1"}
		if cfg.ServerName == "" {
			cfg.ServerName, _, _ = net.SplitHostPort(address)
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		raw, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		bounded, err := ExpireConnection(raw, expiry)
		if err != nil {
			_ = raw.Close()
			return nil, err
		}
		conn := tls.Client(bounded, cfg)
		if err = conn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		state := conn.ConnectionState()
		if _, err = PeerID(state); err != nil {
			_ = conn.Close()
			return nil, err
		}
		if _, err = ExpireConnection(bounded, state.PeerCertificates[0].NotAfter.Add(-time.Second)); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}
