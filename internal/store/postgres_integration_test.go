//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	db "github.com/endless-net/relay/internal/store/storedb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// This suite requires an explicitly isolated, empty PostgreSQL database.
func TestPostgresDurableOwnership(t *testing.T) {
	dsn := os.Getenv("RELAY_TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("RELAY_TEST_DATABASE_URL required for isolated database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if e := migrateFixture(ctx, dsn); e != nil {
		t.Fatal("migration failed")
	}
	s, e := openFixture(ctx, dsn)
	if e != nil {
		t.Fatal("open failed")
	}
	defer s.Close()
	if _, _, e = s.RegisterInstance(ctx, Instance{"r", "boot", "r:9444", time.Time{}}, 30*time.Second); e != nil {
		t.Fatal(e)
	}
	identity := Session{NetworkID: "n", NodeID: "node", RelayID: "r", BootID: "boot"}
	first, e := s.AcquireSession(ctx, identity, 15*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.AcquireSession(ctx, identity, 15*time.Second); !errors.Is(e, ErrSessionFenced) {
		t.Fatalf("live takeover %v", e)
	}
	if e = s.ReleaseSession(ctx, first); e != nil {
		t.Fatal(e)
	}
	s.Close()
	restarted, e := openFixture(ctx, dsn)
	if e != nil {
		t.Fatal("restart failed")
	}
	defer restarted.Close()
	second, e := restarted.AcquireSession(ctx, identity, 15*time.Second)
	if e != nil || second.Epoch <= first.Epoch {
		t.Fatal("epoch lost on restart")
	}
	if e = restarted.ReleaseSession(ctx, first); e != nil {
		t.Fatal(e)
	}
	resolved, e := restarted.ResolveSession(ctx, "n", "node", time.Now())
	if e != nil || resolved.Epoch != second.Epoch {
		t.Fatal("stale release removed current owner")
	}
	if _, _, e = restarted.RegisterInstance(ctx, Instance{"r", "other", "r:9444", time.Time{}}, 30*time.Second); !errors.Is(e, ErrFenced) {
		t.Fatal("live boot takeover")
	}
	if _, _, e = restarted.RegisterInstance(ctx, Instance{RelayID: "retired", BootID: "old", MeshAddr: "r:9444"}, time.Millisecond); e != nil {
		t.Fatal(e)
	}
	time.Sleep(5 * time.Millisecond)
	if _, _, e = restarted.RegisterInstance(ctx, Instance{RelayID: "retired", BootID: "new", MeshAddr: "r:9444"}, time.Millisecond); e != nil {
		t.Fatal(e)
	}
	time.Sleep(5 * time.Millisecond)
	if _, _, e = restarted.RegisterInstance(ctx, Instance{RelayID: "retired", BootID: "old", MeshAddr: "r:9444"}, time.Minute); !errors.Is(e, ErrFenced) {
		t.Fatal("retired boot reused", e)
	}
	var won atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, err := restarted.AcquireSession(ctx, Session{NetworkID: "concurrent", NodeID: "node", RelayID: "r", BootID: "boot"}, 15*time.Second)
			if err == nil {
				won.Add(1)
			} else if !errors.Is(err, ErrSessionFenced) {
				t.Errorf("concurrent acquire %v", err)
			}
		})
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("owners=%d", won.Load())
	}
}

// Plain Unix-socket transport exists only in this build-tagged isolated test fixture.
func openFixture(ctx context.Context, dsn string) (*Postgres, error) {
	c, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		return nil, e
	}
	if !strings.HasPrefix(c.ConnConfig.Host, "/tmp/relay009-pg.") {
		return nil, errors.New("isolated_socket_required")
	}
	c.MaxConns = 16
	p, e := pgxpool.NewWithConfig(ctx, c)
	if e != nil {
		return nil, e
	}
	return &Postgres{pool: p, q: db.New(p)}, nil
}
func migrateFixture(ctx context.Context, dsn string) error {
	c, e := pgx.ParseConfig(dsn)
	if e != nil {
		return e
	}
	if !strings.HasPrefix(c.Host, "/tmp/relay009-pg.") {
		return errors.New("isolated_socket_required")
	}
	p := stdlib.OpenDB(*c)
	defer func() { _ = p.Close() }()
	provider, e := goose.NewProvider(goose.DialectPostgres, p, migrationsSubFS())
	if e != nil {
		return e
	}
	_, e = provider.Up(ctx)
	return e
}
