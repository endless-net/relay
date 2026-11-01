package store

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func TestRegressionReleasedEpochCannotBeReused(t *testing.T) {
	checkReleasedEpoch(t, NewMemory())
}

func TestRetiredBootCannotRegisterAfterNewBootExpires(t *testing.T) {
	s := NewMemory()
	ctx := t.Context()
	if _, _, e := s.RegisterInstance(ctx, Instance{RelayID: "retired", BootID: "old", MeshAddr: "r:9444"}, time.Minute); e != nil {
		t.Fatal(e)
	}
	prior := s.instances["retired"]
	prior.LeaseExpires = time.Now().Add(-time.Second)
	s.instances["retired"] = prior
	if _, _, e := s.RegisterInstance(ctx, Instance{RelayID: "retired", BootID: "new", MeshAddr: "r:9444"}, time.Minute); e != nil {
		t.Fatal(e)
	}
	prior = s.instances["retired"]
	prior.LeaseExpires = time.Now().Add(-time.Second)
	s.instances["retired"] = prior
	if _, _, e := s.RegisterInstance(ctx, Instance{RelayID: "retired", BootID: "old", MeshAddr: "r:9444"}, time.Minute); !errors.Is(e, ErrFenced) {
		t.Fatal("retired boot resurrected", e)
	}
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
	prior := s.instances["r"]
	prior.LeaseExpires = time.Now().Add(-time.Second)
	s.instances["r"] = prior
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

func TestConcurrentAcquireEpochs(t *testing.T) { checkConcurrentEpochs(t, NewMemory()) }
func checkConcurrentEpochs(t *testing.T, s Store) {
	ctx := context.Background()
	for _, id := range []string{"concurrent-a", "concurrent-b"} {
		if _, _, err := s.RegisterInstance(ctx, Instance{RelayID: id, BootID: "boot", MeshAddr: id + ":9444"}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	const count = 20
	epochs := make(chan int64, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "concurrent-a"
			if i%2 == 1 {
				id = "concurrent-b"
			}
			session, err := s.AcquireSession(ctx, Session{NetworkID: "concurrent", NodeID: "node", RelayID: id, BootID: "boot"}, time.Minute)
			if err != nil {
				if !errors.Is(err, ErrSessionFenced) {
					t.Errorf("concurrent acquire: %v", err)
				}
				return
			}
			epochs <- session.Epoch
		}(i)
	}
	wg.Wait()
	close(epochs)
	seen := map[int64]bool{}
	for epoch := range epochs {
		if seen[epoch] {
			t.Errorf("reused epoch %d", epoch)
		}
		seen[epoch] = true
	}
	if len(seen) != 1 {
		t.Fatalf("got %d unique epochs, want %d", len(seen), 1)
	}
}

func TestMemoryEpochOverflowAndInstanceExpiry(t *testing.T) {
	s := NewMemory()
	ctx := context.Background()
	_, _, err := s.RegisterInstance(ctx, Instance{RelayID: "r", BootID: "b", MeshAddr: "r:9444"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.AcquireSession(ctx, Session{NetworkID: "n", NodeID: "node", RelayID: "r", BootID: "b"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveSession(ctx, "n", "node", time.Now().Add(2*time.Minute)); err == nil {
		t.Fatal("expired instance resolved")
	}
	session.Epoch = math.MaxInt64
	for key := range s.sessions {
		s.sessions[key] = session
	}
	if _, err = s.AcquireSession(ctx, session, time.Minute); err == nil {
		t.Fatal("overflow accepted")
	}
}
