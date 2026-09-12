package relaytest

import (
	"crypto/tls"
	"encoding/base64"
	"testing"

	"github.com/endless-net/relay/internal/relay"
	protocol "github.com/endless-net/relay/relayapi/v1"
)

func TestFixtureConfigurationAndUnsupportedMesh(t *testing.T) {
	bundle, err := protocol.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	valid := Config{Addr: "127.0.0.1:0", RelayID: "r", TrustBundle: bundle, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}}
	if _, err := NewServer(valid); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", " r"} {
		cfg := valid
		cfg.RelayID = id
		if _, err := NewServer(cfg); err == nil {
			t.Fatal("invalid relay ID accepted")
		}
	}
	cfg := valid
	cfg.TrustBundle = protocol.SigningTrustBundle{}
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("invalid trust accepted")
	}
	cfg = valid
	cfg.TLSConfig = nil
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("missing TLS accepted")
	}
	var absent *Server
	if err := absent.ListenAndServe(t.Context()); err == nil {
		t.Fatal("nil fixture accepted")
	}
	if err := (&Server{}).ListenAndServe(t.Context()); err == nil {
		t.Fatal("unconfigured fixture accepted")
	}
	if err := (localMesh{}).Forward(t.Context(), relay.PeerRoute{}, "a", "b", "c", "d", nil); err == nil {
		t.Fatal("fixture pretended to support mesh")
	}
}

func TestFixtureScopedSessionReplacementAndRelease(t *testing.T) {
	c := newControl("r")
	ctx := t.Context()
	credential := protocol.Credential{NetworkID: "a", NodeID: "node"}
	old, err := c.AcquireSession(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	current, err := c.AcquireSession(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	if current.Epoch <= old.Epoch {
		t.Fatal("replacement did not advance epoch")
	}
	if err := c.RenewSession(ctx, old); err == nil {
		t.Fatal("stale fixture lease renewed")
	}
	if err := c.ReleaseSession(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := c.RenewSession(ctx, current); err != nil {
		t.Fatal("late release removed current lease", err)
	}
	if _, err := c.AuthorizePeer(ctx, credential, 1, "b", "node"); err == nil {
		t.Fatal("fixture crossed destination network")
	}
	if route, err := c.AuthorizePeer(ctx, credential, 1, "a", "node"); err != nil || route.Epoch != current.Epoch {
		t.Fatal("fixture route mismatch", err)
	}
	if err := c.ReleaseSession(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := c.RenewSession(ctx, current); err == nil {
		t.Fatal("released fixture lease renewed")
	}
}
