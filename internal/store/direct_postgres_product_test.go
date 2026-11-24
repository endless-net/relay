//go:build product

package store

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "github.com/endless-net/relay/internal/store/storedb"
	"github.com/endless-net/relay/internal/tlsconfig"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func openDirectPostgresFixture(t *testing.T) (*Postgres, context.Context) {
	t.Helper()
	workloadAPI := os.Getenv("RELAY_POSTGRES_TEST_WORKLOAD_API")
	dsn := os.Getenv("RELAY_POSTGRES_TEST_DSN")
	peer := os.Getenv("RELAY_POSTGRES_TEST_PEER")
	if workloadAPI == "" || dsn == "" || peer == "" {
		t.Skip("requires an isolated, freshly migrated PostgreSQL registry and SPIRE Workload API")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	workload, err := tlsconfig.NewWorkloadRuntime(ctx, workloadAPI, "spiffe://endlessnet.ru/service/relay-coordinator")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workload.Close() })
	host, err := DirectPostgresHost(dsn)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := workload.DatabaseTLSConfig(peer, host)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenPostgres(ctx, dsn, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.Close)
	return registry, ctx
}

func TestDirectPostgresReadinessRequiresRegistryPrivileges(t *testing.T) {
	registry, ctx := openDirectPostgresFixture(t)
	const restore = "GRANT UPDATE ON node_session_leases TO CURRENT_USER"
	if _, err := registry.pool.Exec(ctx, "REVOKE UPDATE ON node_session_leases FROM CURRENT_USER"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := registry.pool.Exec(cleanup, restore); err != nil {
			t.Error(err)
		}
	})
	if err := registry.Ping(ctx); !errors.Is(err, errRegistryUnavailable) {
		t.Fatalf("registry readiness with missing UPDATE privilege: %v", err)
	}
	if _, err := registry.pool.Exec(ctx, restore); err != nil {
		t.Fatal(err)
	}
	if err := registry.Ping(ctx); err != nil {
		t.Fatalf("registry readiness did not recover after restoring UPDATE: %v", err)
	}
}

func TestDirectPostgresDoesNotReplayAcquireAfterLostCommitReply(t *testing.T) {
	observer, ctx := openDirectPostgresFixture(t)
	const ttl = time.Minute
	instance := Instance{RelayID: "commit-reply-relay", BootID: "commit-reply-boot", MeshAddr: "relay.test:9444"}
	if _, _, err := observer.RegisterInstance(ctx, instance, ttl); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := observer.pool.QueryRow(ctx, `SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM relay_epoch_seq`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	target, fault := postgresWithLostReply(t, observer, "commit")
	requested := Session{NetworkID: "commit-reply-net", NodeID: "commit-reply-node", RelayID: instance.RelayID, BootID: instance.BootID}
	fault.enabled.Store(true)
	lease, err := target.AcquireSession(ctx, requested, ttl)
	if registryProbeError(err) != errRegistryTransport || lease != (Session{}) || fault.attempts.Load() != 1 || !fault.discarded.Load() {
		t.Fatalf("acquire returned lease %v, error %v, commit attempts %d, dropped reply %t", lease, err, fault.attempts.Load(), fault.discarded.Load())
	}
	if err := target.Ping(ctx); err != nil {
		t.Fatalf("pool did not recover: %v", err)
	}
	stored, err := target.ResolveSession(ctx, requested.NetworkID, requested.NodeID, time.Now())
	if err != nil || stored.BootID != requested.BootID || stored.Epoch != before+1 {
		t.Fatalf("committed lease %v, want epoch %d, error %v", stored, before+1, err)
	}
	if _, err := target.AcquireSession(ctx, requested, ttl); !errors.Is(err, ErrSessionFenced) {
		t.Fatalf("existing lease did not fence a new acquire: %v", err)
	}
	if err := target.ReleaseSession(ctx, stored); err != nil {
		t.Fatal(err)
	}
	replacement := Instance{RelayID: "commit-reply-replacement", BootID: "replacement-boot", MeshAddr: "replacement.test:9444"}
	if _, _, err := target.RegisterInstance(ctx, replacement, ttl); err != nil {
		t.Fatal(err)
	}
	requested.RelayID, requested.BootID = replacement.RelayID, replacement.BootID
	current, err := target.AcquireSession(ctx, requested, ttl)
	if err != nil || current.BootID != replacement.BootID || current.Epoch <= stored.Epoch {
		t.Fatalf("replacement lease %v did not supersede %v: %v", current, stored, err)
	}
	if _, err := target.RenewSession(ctx, stored, ttl); !errors.Is(err, ErrSessionFenced) {
		t.Fatalf("obsolete boot and epoch renewed a reassigned lease: %v", err)
	}
}

func TestDirectPostgresDoesNotReplayHeartbeatAfterLostReply(t *testing.T) {
	observer, ctx := openDirectPostgresFixture(t)
	instance := Instance{RelayID: "heartbeat-reply-relay", BootID: "heartbeat-reply-boot", MeshAddr: "relay.test:9444"}
	if _, _, err := observer.RegisterInstance(ctx, instance, time.Minute); err != nil {
		t.Fatal(err)
	}
	target, fault := postgresWithLostReply(t, observer, "-- name: Heartbeat :one\n")
	// Prime pgx's statement cache so the injected read failure drops the
	// UPDATE result rather than the preceding statement description.
	_, before, err := target.HeartbeatInstance(ctx, instance.RelayID, instance.BootID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fault.enabled.Store(true)
	_, expiry, err := target.HeartbeatInstance(ctx, instance.RelayID, instance.BootID, 2*time.Minute)
	if registryProbeError(err) != errRegistryTransport || !expiry.IsZero() || fault.attempts.Load() != 1 || !fault.discarded.Load() {
		t.Fatalf("heartbeat returned expiry %v, error %v, attempts %d, dropped reply %t", expiry, err, fault.attempts.Load(), fault.discarded.Load())
	}
	if err := target.Ping(ctx); err != nil {
		t.Fatalf("pool did not recover: %v", err)
	}
	var stored time.Time
	err = target.pool.QueryRow(ctx, `SELECT lease_expires_at FROM relay_instances WHERE relay_id=$1`, instance.RelayID).Scan(&stored)
	if err != nil || !stored.After(before.Add(59*time.Second)) {
		t.Fatalf("heartbeat result was not committed: persisted expiry %v, original expiry %v, error %v", stored, before, err)
	}
}

func postgresWithLostReply(t *testing.T, observer *Postgres, sqlPrefix string) (*Postgres, *lostReplyFault) {
	t.Helper()
	// Copy the shipped pool configuration and interrupt the reply at the
	// TCP stream. TLS and SQL still run through their regular paths.
	fault := &lostReplyFault{sqlPrefix: sqlPrefix}
	config := observer.pool.Config()
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &lostReplyConn{Conn: conn, fault: fault}, nil
	}
	config.ConnConfig.Tracer = fault
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Postgres{pool: pool, q: db.New(pool)}, fault
}

type lostReplyFault struct {
	sqlPrefix string
	attempts  atomic.Int32
	enabled   atomic.Bool
	armed     atomic.Bool
	discarded atomic.Bool
}

func (f *lostReplyFault) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if f.enabled.Load() && strings.HasPrefix(query.SQL, f.sqlPrefix) && f.attempts.Add(1) == 1 {
		f.armed.Store(true)
	}
	return ctx
}

func (*lostReplyFault) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type lostReplyConn struct {
	net.Conn
	fault *lostReplyFault
}

func (c *lostReplyConn) Read(buffer []byte) (int, error) {
	n, err := c.Conn.Read(buffer)
	if n > 0 && c.fault.armed.Swap(false) {
		c.fault.discarded.Store(true)
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}
