package store

import (
	"context"
	"errors"
	"testing"
	"time"

	protocolv1 "github.com/unng-lab/endlessnet-relay/protocol/v1"
)

func TestMemorySessionEpochFencesPreviousOwner(t *testing.T) {
	ctx := context.Background()
	storage := NewMemory()
	for _, instance := range []Instance{{RelayID: "relay-a", BootID: "boot-a", MeshAddr: "relay-a:9444"}, {RelayID: "relay-b", BootID: "boot-b", MeshAddr: "relay-b:9444"}} {
		if _, _, err := storage.RegisterInstance(ctx, instance, 15*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	first, err := storage.AcquireSession(ctx, Session{NetworkID: "network", NodeID: "node", RelayID: "relay-a", BootID: "boot-a"}, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, err := storage.AcquireSession(ctx, Session{NetworkID: "network", NodeID: "node", RelayID: "relay-b", BootID: "boot-b"}, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.Epoch != first.Epoch+1 {
		t.Fatalf("second epoch = %d, want %d", second.Epoch, first.Epoch+1)
	}
	if _, err := storage.RenewSession(ctx, first, 15*time.Second); !errors.Is(err, ErrSessionFenced) {
		t.Fatalf("old owner renewal error = %v", err)
	}
	resolved, err := storage.ResolveSession(ctx, "network", "node", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.RelayID != "relay-b" || resolved.Epoch != second.Epoch {
		t.Fatalf("resolved session = %#v", resolved)
	}
}

func TestMemoryPersistsEmptyEndpointSnapshotVersion(t *testing.T) {
	storage := NewMemory()
	snapshot := protocolv1.EndpointSnapshot{Version: 7, Endpoints: []protocolv1.Endpoint{}}
	if err := storage.ReplaceEndpoints(context.Background(), snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	loaded, err := storage.EndpointSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != 7 || len(loaded.Endpoints) != 0 {
		t.Fatalf("empty endpoint snapshot = %#v", loaded)
	}
	if err := storage.ReplaceEndpoints(context.Background(), snapshot, time.Now()); err != nil {
		t.Fatalf("identical snapshot no-op: %v", err)
	}
	changed := protocolv1.EndpointSnapshot{Version: 7, Endpoints: []protocolv1.Endpoint{{ID: "relay", Addr: "relay:9443", Protocol: protocolv1.EndpointProtocolTLS}}}
	if err := storage.ReplaceEndpoints(context.Background(), changed, time.Now()); err == nil {
		t.Fatal("same-version endpoint content change was accepted")
	}
}

func TestMemoryBootChangeFencesOldInstance(t *testing.T) {
	ctx := context.Background()
	storage := NewMemory()
	if _, _, err := storage.RegisterInstance(ctx, Instance{RelayID: "relay-a", BootID: "old", MeshAddr: "relay-a:9444"}, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.RegisterInstance(ctx, Instance{RelayID: "relay-a", BootID: "new", MeshAddr: "relay-a:9444"}, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.HeartbeatInstance(ctx, "relay-a", "old", 15*time.Second); !errors.Is(err, ErrFenced) {
		t.Fatalf("old boot heartbeat error = %v", err)
	}
}
