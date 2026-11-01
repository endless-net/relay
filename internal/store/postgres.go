package store

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"io/fs"
	"time"

	db "github.com/endless-net/relay/internal/store/storedb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Postgres struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func OpenPostgres(ctx context.Context, dsn string, transport *tls.Config) (*Postgres, error) {
	c, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		return nil, errors.New("registry_config")
	}
	if e = secureConfig(c.ConnConfig, transport); e != nil {
		return nil, e
	}
	c.ConnConfig.Tracer = queryTracer{}
	c.BeforeConnect = freezeIdentity
	c.AfterConnect = bindDatabasePeer
	c.PrepareConn = func(_ context.Context, conn *pgx.Conn) (bool, error) { return reusableIdentity(conn), nil }
	c.AfterRelease = reusableIdentity
	c.MaxConns = 16
	c.MinConns = 0
	c.ConnConfig.ConnectTimeout = 2 * time.Second
	p, e := pgxpool.NewWithConfig(ctx, c)
	if e != nil {
		return nil, errors.New("registry_open")
	}
	x := &Postgres{pool: p, q: db.New(p)}
	if e = x.Ping(ctx); e != nil {
		p.Close()
		return nil, e
	}
	return x, nil
}

// Migrate is an explicit domain operation, separate from runtime startup.
func Migrate(ctx context.Context, dsn string, transport *tls.Config) error {
	c, e := pgx.ParseConfig(dsn)
	if e != nil {
		return errors.New("registry_config")
	}
	if e = secureConfig(c, transport); e != nil {
		return e
	}
	c.ConnectTimeout = 2 * time.Second
	pool := stdlib.OpenDB(*c, stdlib.OptionBeforeConnect(freezeIdentity), stdlib.OptionAfterConnect(bindDatabasePeer))
	defer func() { _ = pool.Close() }()
	pool.SetMaxOpenConns(1)

	provider, e := goose.NewProvider(goose.DialectPostgres, pool, migrationsSubFS())
	if e != nil {
		return e
	}
	_, e = provider.Up(ctx)
	return e
}
func migrationsSubFS() fs.FS { s, _ := fs.Sub(migrations, "migrations"); return s }
func (p *Postgres) Close()   { p.pool.Close() }
func (p *Postgres) Ping(ctx context.Context) error {
	ctx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	started := time.Now()
	observed, e := p.q.Clock(ctx)
	if e != nil || observed.Before(started.Add(-time.Second)) || observed.After(time.Now().Add(time.Second)) {
		return errors.New("registry_clock_untrusted")
	}
	if e := p.pool.Ping(ctx); e != nil {
		return errors.New("registry_unavailable")
	}
	return nil
}
func (p *Postgres) RegisterInstance(ctx context.Context, i Instance, ttl time.Duration) ([]Instance, time.Time, error) {
	if e := validateInstance(i); e != nil {
		return nil, time.Time{}, e
	}
	ctx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	now := time.Now()
	expires := now.Add(ttl)
	tx, e := p.pool.Begin(ctx)
	if e != nil {
		return nil, time.Time{}, e
	}
	defer rollback(tx)()
	q := p.q.WithTx(tx)
	if e = q.LockRegistry(ctx); e != nil {
		return nil, time.Time{}, e
	}
	current, readErr := q.LockInstance(ctx, i.RelayID)
	if readErr != nil && !errors.Is(readErr, pgx.ErrNoRows) {
		return nil, time.Time{}, readErr
	}
	if readErr == nil && current.BootID == i.BootID && current.MeshAddr != i.MeshAddr {
		return nil, time.Time{}, ErrFenced
	}
	if readErr != nil || current.BootID != i.BootID {
		if _, e = q.KnownBoot(ctx, db.KnownBootParams{RelayID: i.RelayID, BootID: i.BootID}); e == nil {
			return nil, time.Time{}, ErrFenced
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return nil, time.Time{}, e
		}
		count, e := q.InstanceCount(ctx)
		if e != nil {
			return nil, time.Time{}, e
		}
		if readErr != nil && count >= 64 {
			return nil, time.Time{}, errors.New("registry_capacity")
		}
	}
	if _, e = q.Register(ctx, db.RegisterParams{RelayID: i.RelayID, BootID: i.BootID, MeshAddr: i.MeshAddr, Expires: expires, NowAt: now}); e != nil {
		return nil, time.Time{}, storeError(e, ErrFenced)
	}
	if e = q.RecordBoot(ctx, db.RecordBootParams{RelayID: i.RelayID, BootID: i.BootID}); e != nil {
		return nil, time.Time{}, e
	}
	if e = tx.Commit(ctx); e != nil {
		return nil, time.Time{}, e
	}

	peers, e := p.activePeers(ctx, i.RelayID, now)
	return peers, expires, e
}
func (p *Postgres) HeartbeatInstance(ctx context.Context, id, boot string, ttl time.Duration) ([]Instance, time.Time, error) {
	ctx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	now := time.Now()
	expires := now.Add(ttl)
	_, e := p.q.Heartbeat(ctx, db.HeartbeatParams{RelayID: id, BootID: boot, Expires: expires, NowAt: now})
	if e != nil {
		return nil, time.Time{}, storeError(e, ErrFenced)
	}
	peers, e := p.activePeers(ctx, id, now)
	return peers, expires, e
}
func (p *Postgres) AcquireSession(ctx context.Context, s Session, ttl time.Duration) (Session, error) {
	if e := validateSession(s); e != nil {
		return Session{}, e
	}
	ctx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	tx, e := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if e != nil {
		return Session{}, e
	}
	defer func() {
		clean, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = tx.Rollback(clean)
	}()
	q := p.q.WithTx(tx)
	i, e := q.LockInstance(ctx, s.RelayID)
	now := time.Now()
	if e != nil {
		return Session{}, storeError(e, ErrFenced)
	}
	if i.BootID != s.BootID || !now.Before(i.LeaseExpiresAt) {
		return Session{}, ErrFenced
	}
	count, e := q.ActiveSessionCount(ctx, db.ActiveSessionCountParams{RelayID: s.RelayID, NowAt: now})
	if e != nil {
		return Session{}, e
	}
	if count >= 1000 {
		return Session{}, errors.New("session_capacity")
	}
	row, e := q.Acquire(ctx, db.AcquireParams{NetworkID: s.NetworkID, NodeID: s.NodeID, RelayID: s.RelayID, BootID: s.BootID, Expires: now.Add(ttl), NowAt: now})
	if e != nil {
		return Session{}, storeError(e, ErrSessionFenced)
	}
	if e = tx.Commit(ctx); e != nil {
		return Session{}, e
	}
	return sessionRow(row), nil
}
func (p *Postgres) RenewSession(ctx context.Context, s Session, ttl time.Duration) (Session, error) {
	ctx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	now := time.Now()
	row, e := p.q.Renew(ctx, db.RenewParams{NetworkID: s.NetworkID, NodeID: s.NodeID, RelayID: s.RelayID, BootID: s.BootID, Epoch: s.Epoch, NowAt: now, Expires: now.Add(ttl)})
	return sessionRow(row), storeError(e, ErrSessionFenced)
}
func (p *Postgres) ReleaseSession(ctx context.Context, s Session) error {
	ctx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	return p.q.Release(ctx, db.ReleaseParams{NetworkID: s.NetworkID, NodeID: s.NodeID, RelayID: s.RelayID, BootID: s.BootID, Epoch: s.Epoch})
}
func (p *Postgres) ResolveSession(ctx context.Context, network, node string, now time.Time) (Session, error) {
	ctx, c := context.WithTimeout(ctx, 2*time.Second)
	defer c()
	row, e := p.q.Resolve(ctx, db.ResolveParams{NetworkID: network, NodeID: node, NowAt: now})
	return sessionRow(row), storeError(e, ErrSessionMissing)
}
func (p *Postgres) activePeers(ctx context.Context, id string, now time.Time) ([]Instance, error) {
	rows, e := p.q.Peers(ctx, db.PeersParams{RelayID: id, NowAt: now})
	out := make([]Instance, 0, len(rows))
	for _, r := range rows {
		out = append(out, Instance{r.RelayID, r.BootID, r.MeshAddr, r.LeaseExpiresAt})
	}
	return out, e
}
func sessionRow(r db.NodeSessionLease) Session {
	return Session{r.NetworkID, r.NodeID, r.RelayID, r.BootID, r.Epoch, r.LeaseExpiresAt}
}
func storeError(e, fenced error) error {
	if errors.Is(e, pgx.ErrNoRows) {
		return fenced
	}
	return e
}

func rollback(tx pgx.Tx) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(ctx)
	}
}

func secureConfig(c *pgx.ConnConfig, transport *tls.Config) error {
	if transport == nil || transport.MinVersion != tls.VersionTLS13 || transport.GetClientCertificate == nil || (transport.VerifyPeerCertificate == nil && transport.VerifyConnection == nil) {
		return errors.New("registry_spiffe_tls_required")
	}
	c.TLSConfig = transport.Clone()
	c.Fallbacks = nil
	return nil
}
