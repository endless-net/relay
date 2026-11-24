package store

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"math/rand/v2"
	"time"

	"github.com/endless-net/relay/internal/tlsconfig"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type registryCertificateLease struct {
	localEnd, peerEnd, chainEnd, refreshFrom time.Time
}

func registryLease(conn *pgx.Conn, transport tlsconfig.DatabaseTransport) (registryCertificateLease, bool) {
	tlsConn, ok := conn.PgConn().Conn().(*tls.Conn)
	if !ok {
		return registryCertificateLease{}, false
	}
	local := conn.Config().TLSConfig.Certificates[0]
	state := tlsConn.ConnectionState()
	expiry, err := transport.VerifyLease(local, state)
	if err != nil || len(local.Certificate) == 0 || len(state.PeerCertificates) == 0 {
		return registryCertificateLease{}, false
	}
	leaf, err := x509.ParseCertificate(local.Certificate[0])
	if err != nil {
		return registryCertificateLease{}, false
	}
	peer := state.PeerCertificates[0]
	refreshFrom := leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) / 2)
	if peerRefresh := peer.NotBefore.Add(peer.NotAfter.Sub(peer.NotBefore) / 2); peerRefresh.Before(refreshFrom) {
		refreshFrom = peerRefresh
	}
	return registryCertificateLease{leaf.NotAfter, peer.NotAfter, expiry, refreshFrom}, true
}

func (current registryCertificateLease) replaces(old registryCertificateLease) bool {
	localNewer := current.localEnd.Compare(old.localEnd)
	peerNewer := current.peerEnd.Compare(old.peerEnd)
	return !current.chainEnd.Before(old.chainEnd) && localNewer >= 0 && peerNewer >= 0 && localNewer+peerNewer > 0
}

func (p *Postgres) refreshRegistryPool(ctx context.Context, config *pgxpool.Config, transport tlsconfig.DatabaseTransport) {
	for ctx.Err() == nil {
		interval := 2 * time.Second
		if p.refreshRegistryConnection(ctx, config, transport) {
			interval = 250 * time.Millisecond
		}
		timer := time.NewTimer(interval + rand.N(interval))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

func (p *Postgres) refreshRegistryConnection(ctx context.Context, config *pgxpool.Config, transport tlsconfig.DatabaseTransport) bool {
	var oldest registryCertificateLease
	for _, idle := range p.pool.AcquireAllIdle(ctx) {
		lease, ok := registryLease(idle.Conn(), transport)
		if ok && !time.Now().Before(lease.refreshFrom) && (oldest.chainEnd.IsZero() || lease.chainEnd.Before(oldest.chainEnd)) {
			oldest = lease
		}
		idle.Release()
	}
	if oldest.chainEnd.IsZero() {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, config.ConnConfig.ConnectTimeout)
	defer cancel()
	connConfig := config.ConnConfig.Copy()
	if err := config.BeforeConnect(probeCtx, connConfig); err != nil {
		return false
	}
	probe, err := pgx.ConnectConfig(probeCtx, connConfig)
	if err != nil {
		return false
	}
	current, ok := registryLease(probe, transport)
	_ = probe.Close(probeCtx)
	if !ok || !current.replaces(oldest) || ctx.Err() != nil {
		return false
	}

	var discarded *pgx.Conn
	for _, idle := range p.pool.AcquireAllIdle(ctx) {
		lease, ok := registryLease(idle.Conn(), transport)
		if discarded == nil && ok && !time.Now().Before(lease.refreshFrom) && current.replaces(lease) {
			discarded = idle.Hijack()
			continue
		}
		idle.Release()
	}
	if discarded == nil {
		return false
	}
	_ = discarded.Close(probeCtx)
	return true
}
