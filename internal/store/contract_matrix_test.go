package store

import (
	"errors"
	"testing"
	"time"

	protocol "github.com/endless-net/relay/relayapi/v1"
)

func TestMemoryStoreContractMatrix(t *testing.T) { checkStoreContractMatrix(t, NewMemory()) }

// Shared with the integration-tagged real PostgreSQL test. Names are isolated
// from the other persistence tests; the global endpoint snapshot is not changed.
func checkStoreContractMatrix(t *testing.T, s Store) {
	t.Helper()
	ctx := t.Context()
	prefix := "matrix-" + t.Name()
	instance := Instance{RelayID: prefix, BootID: "boot", MeshAddr: "relay:9444"}
	if _, _, err := s.RegisterInstance(ctx, instance, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.HeartbeatInstance(ctx, prefix, "wrong", time.Minute); !errors.Is(err, ErrFenced) {
		t.Fatal("wrong boot heartbeat accepted", err)
	}
	if _, _, err := s.HeartbeatInstance(ctx, prefix, "boot", time.Minute); err != nil {
		t.Fatal(err)
	}
	identity := Session{RelayID: prefix, BootID: "boot", NetworkID: prefix + "-a", NodeID: "same-node"}
	a, err := s.AcquireSession(ctx, identity, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	identity.NetworkID = prefix + "-b"
	b, err := s.AcquireSession(ctx, identity, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewSession(ctx, a, time.Minute); err != nil {
		t.Fatal(err)
	}
	wrong := a
	wrong.Epoch++
	if _, err := s.RenewSession(ctx, wrong, time.Minute); !errors.Is(err, ErrSessionFenced) {
		t.Fatal("wrong epoch renewed", err)
	}
	if err := s.ReleaseSession(ctx, wrong); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ResolveSession(ctx, a.NetworkID, a.NodeID, time.Now()); err != nil || got.Epoch != a.Epoch {
		t.Fatal("stale release removed active lease", err)
	}
	if err := s.ReleaseSession(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveSession(ctx, a.NetworkID, a.NodeID, time.Now()); !errors.Is(err, ErrSessionMissing) {
		t.Fatal("released session resolved", err)
	}
	if got, err := s.ResolveSession(ctx, b.NetworkID, b.NodeID, time.Now()); err != nil || got.NetworkID != b.NetworkID {
		t.Fatal("release crossed network boundary", err)
	}
	if _, err := s.ResolveSession(ctx, b.NetworkID, b.NodeID, b.LeaseExpires); !errors.Is(err, ErrSessionMissing) {
		t.Fatal("exclusive lease expiry accepted", err)
	}
	instance.BootID = "replacement"
	if _, _, err := s.RegisterInstance(ctx, instance, time.Minute); err != nil {
		t.Fatal(err)
	}
	// Stores may report either the instance or its session as fenced; neither
	// can authorize renewal after the owner boot changed.
	if _, err := s.RenewSession(ctx, b, time.Minute); !errors.Is(err, ErrSessionFenced) && !errors.Is(err, ErrFenced) {
		t.Fatal("old owner renewed after replacement", err)
	}
}

func TestMemorySnapshotOrderingOwnershipAndRollback(t *testing.T) {
	s := NewMemory()
	snapshot := protocol.EndpointSnapshot{Version: 2, Endpoints: []protocol.Endpoint{{ID: "z", Addr: "z:443", Protocol: protocol.EndpointProtocolTLS}, {ID: "a", Addr: "a:443", Protocol: protocol.EndpointProtocolTLS}}}
	if err := s.ReplaceEndpoints(t.Context(), snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	snapshot.Endpoints[0].Addr = "mutated"
	got, err := s.EndpointSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoints[0].ID != "a" || got.Endpoints[1].Addr != "z:443" {
		t.Fatal("snapshot not canonical or input aliased")
	}
	got.Endpoints[0].Addr = "mutated"
	again, err := s.EndpointSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if again.Endpoints[0].Addr != "a:443" {
		t.Fatal("snapshot output aliased")
	}
	if err := s.ReplaceEndpoints(t.Context(), protocol.EndpointSnapshot{Version: 1}, time.Now()); err == nil {
		t.Fatal("snapshot rollback accepted")
	}
	again.Endpoints[0], again.Endpoints[1] = again.Endpoints[1], again.Endpoints[0]
	if err := s.ReplaceEndpoints(t.Context(), again, time.Now()); err != nil {
		t.Fatal("same contents in different order rejected", err)
	}
}
