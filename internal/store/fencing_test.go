package store

import (
	"context"
	"testing"
	"time"
)

func TestRegressionReleasedEpochCannotBeReused(t *testing.T) {
	checkReleasedEpoch(t, NewMemory())
}
func checkReleasedEpoch(t *testing.T, s Store) {
	ctx := context.Background()
	if _, _, err := s.RegisterInstance(ctx, Instance{RelayID: "r", BootID: "b", MeshAddr: "r:9444"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	identity := Session{NetworkID: "n", NodeID: "node", RelayID: "r", BootID: "b"}
	first, err := s.AcquireSession(ctx, identity, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseSession(ctx, first); err != nil {
		t.Fatal(err)
	}
	second, err := s.AcquireSession(ctx, identity, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.Epoch <= first.Epoch {
		t.Errorf("epoch reused: first=%d second=%d", first.Epoch, second.Epoch)
	}
	if err = s.ReleaseSession(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveSession(ctx, "n", "node", time.Now()); err != nil {
		t.Errorf("old release deleted new session: %v", err)
	}
}
func TestRegressionResolveRejectsReplacedInstanceBoot(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	if _, _, err := s.RegisterInstance(ctx, Instance{RelayID: "r", BootID: "old", MeshAddr: "r:9444"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireSession(ctx, Session{NetworkID: "n", NodeID: "node", RelayID: "r", BootID: "old"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegisterInstance(ctx, Instance{RelayID: "r", BootID: "new", MeshAddr: "r:9444"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ResolveSession(ctx, "n", "node", time.Now()); err == nil {
		t.Fatalf("fenced boot still resolves: %s", got.BootID)
	}
}

func TestExpiredSessionCannotRenew(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	_, _, err := s.RegisterInstance(ctx, Instance{RelayID: "r", BootID: "b", MeshAddr: "r:9444"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.AcquireSession(ctx, Session{NetworkID: "n", NodeID: "node", RelayID: "r", BootID: "b"}, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RenewSession(ctx, old, time.Minute); err == nil {
		t.Fatal("expired session renewed")
	}
}
