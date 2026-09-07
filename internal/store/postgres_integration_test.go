//go:build integration

package store

import (
	"context"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"math"
	"os"
	"testing"
	"time"
)

func TestPostgresReleasedEpochCannotBeReused(t *testing.T) {
	dsn := os.Getenv("E2E_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("E2E_POSTGRES_DSN required")
	}
	s, err := OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkReleasedEpoch(t, s)
	checkConcurrentEpochs(t, s)
}

func TestPostgresPersistenceExpiryOverflowAndSnapshots(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("E2E_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("E2E_POSTGRES_DSN required")
	}
	s, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	identity := Session{NetworkID: "persistence", NodeID: "node", RelayID: "persist", BootID: "old"}
	if _, _, err = s.RegisterInstance(ctx, Instance{RelayID: identity.RelayID, BootID: identity.BootID, MeshAddr: "persist:9444"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	first, err := s.AcquireSession(ctx, identity, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseSession(ctx, first); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AcquireSession(ctx, identity, time.Minute)
	if err != nil || second.Epoch <= first.Epoch {
		t.Fatalf("restart epoch: %d %v", second.Epoch, err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE node_session_leases SET lease_expires_at=now()-interval '1 second' WHERE network_id='persistence'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RenewSession(ctx, second, time.Minute); err == nil {
		t.Fatal("expired session renewed")
	}
	third, err := s.AcquireSession(ctx, identity, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE relay_instances SET lease_expires_at=now()-interval '1 second' WHERE relay_id='persist'`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveSession(ctx, identity.NetworkID, identity.NodeID, time.Now()); err == nil {
		t.Fatal("expired instance resolved")
	}
	if _, _, err = s.RegisterInstance(ctx, Instance{RelayID: identity.RelayID, BootID: "new", MeshAddr: "persist:9444"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveSession(ctx, identity.NetworkID, identity.NodeID, time.Now()); err == nil {
		t.Fatal("old boot resolved")
	}
	if _, err = s.RenewSession(ctx, third, time.Minute); err == nil {
		t.Fatal("old boot renewed")
	}
	if _, err = s.pool.Exec(ctx, `UPDATE node_session_leases SET epoch=$1 WHERE network_id='persistence'`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	identity.BootID = "new"
	if _, err = s.AcquireSession(ctx, identity, time.Minute); err == nil {
		t.Fatal("epoch overflow accepted")
	}
	snapshot := protocolv1.EndpointSnapshot{Version: 7, Endpoints: []protocolv1.Endpoint{{ID: "one", Addr: "one:9443", Protocol: "relay-v1-tls"}}}
	if err = s.ReplaceEndpoints(ctx, snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	rollback := snapshot
	rollback.Version = 6
	if err = s.ReplaceEndpoints(ctx, rollback, time.Now()); err == nil {
		t.Fatal("snapshot rollback accepted")
	}
	altered := protocolv1.EndpointSnapshot{Version: 7, Endpoints: []protocolv1.Endpoint{{ID: "two", Addr: "two:9443", Protocol: "relay-v1-tls"}}}
	if err = s.ReplaceEndpoints(ctx, altered, time.Now()); err == nil {
		t.Fatal("same-version change accepted")
	}
	malformed := protocolv1.EndpointSnapshot{Version: 8, Endpoints: []protocolv1.Endpoint{{ID: "bad", Addr: "bad:9443", Protocol: "unknown"}}}
	if err = s.ReplaceEndpoints(ctx, malformed, time.Now()); err == nil {
		t.Fatal("malformed snapshot accepted")
	}
	empty := protocolv1.EndpointSnapshot{Version: 8}
	if err = s.ReplaceEndpoints(ctx, empty, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.EndpointSnapshot(ctx)
	if err != nil || got.Version != 8 || len(got.Endpoints) != 0 {
		t.Fatal("empty snapshot did not persist")
	}
}
