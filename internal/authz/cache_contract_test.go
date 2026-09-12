package authz

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	protocol "github.com/endless-net/relay/relayapi/v1"
)

type trustUpstream struct {
	stubAuthorizer
	load func() (protocol.SigningTrustBundle, error)
}

func (s *trustUpstream) RelayTrustBundle(context.Context) (protocol.SigningTrustBundle, error) {
	s.calls++
	return s.load()
}

func trustFixture(t *testing.T) protocol.SigningTrustBundle {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := protocol.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTrustCacheFreshStaleBoundariesAndRecovery(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	start := now
	bundle := trustFixture(t)
	var failure error
	u := &trustUpstream{load: func() (protocol.SigningTrustBundle, error) { return bundle, failure }}
	c := NewCache(u)
	c.Now = func() time.Time { return now }
	get := func(wantErr bool, calls int) {
		t.Helper()
		b, err := c.RelayTrustBundle(t.Context())
		if (err != nil) != wantErr || u.calls != calls {
			t.Fatalf("error=%v calls=%d want error=%v calls=%d", err, u.calls, wantErr, calls)
		}
		if !wantErr && b.ActiveKeyID != bundle.ActiveKeyID {
			t.Fatal("wrong trust")
		}
	}
	get(false, 1)
	failure = errors.New("offline")
	now = start.Add(5*time.Second - time.Nanosecond)
	get(false, 1)
	now = start.Add(5 * time.Second)
	get(false, 2)
	now = start.Add(30*time.Second - time.Nanosecond)
	get(false, 3)
	now = start.Add(30 * time.Second)
	get(true, 4)
	failure = nil
	bundle = trustFixture(t)
	get(false, 5)
}

func TestTrustCacheRechecksStaleDeadlineAfterLoad(t *testing.T) {
	now := time.Now()
	bundle := trustFixture(t)
	u := &trustUpstream{load: func() (protocol.SigningTrustBundle, error) { return bundle, nil }}
	c := NewCache(u)
	c.Now = func() time.Time { return now }
	if _, err := c.RelayTrustBundle(t.Context()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Second)
	u.load = func() (protocol.SigningTrustBundle, error) {
		now = now.Add(30 * time.Second)
		return protocol.SigningTrustBundle{}, errors.New("slow outage")
	}
	if _, err := c.RelayTrustBundle(t.Context()); err == nil {
		t.Fatal("expired stale trust returned after slow upstream")
	}
}

func TestTrustCacheOwnsInputAndOutput(t *testing.T) {
	bundle := trustFixture(t)
	until := time.Now().Add(time.Hour)
	bundle.Keys[0].NotAfter = &until
	u := &trustUpstream{load: func() (protocol.SigningTrustBundle, error) { return bundle, nil }}
	c := NewCache(u)
	got, err := c.RelayTrustBundle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got.Keys[0].PublicKey = "mutated"
	*got.Keys[0].NotAfter = time.Time{}
	bundle.Keys[0].Algorithm = "mutated"
	again, err := c.RelayTrustBundle(t.Context())
	if err != nil || again.Validate() != nil || again.Keys[0].NotAfter.IsZero() {
		t.Fatal("caller or upstream mutated cached trust", err)
	}
}

func TestCacheReplacementDoesNotEvictUnrelatedEntry(t *testing.T) {
	c := NewCache(&stubAuthorizer{})
	c.MaxEntries = 2
	now := time.Now()
	c.put("older", entry{allowed: true, storedAt: now})
	c.put("newer", entry{allowed: true, storedAt: now.Add(time.Second)})
	c.put("newer", entry{allowed: false, storedAt: now.Add(2 * time.Second)})
	if len(c.entries) != 2 || !c.entries["older"].allowed || c.entries["newer"].allowed {
		t.Fatal("replacement evicted unrelated entry")
	}
	c.put("third", entry{storedAt: now.Add(3 * time.Second)})
	if _, ok := c.entries["older"]; ok || len(c.entries) != 2 {
		t.Fatal("oldest entry was not evicted")
	}
}

func TestUnconfiguredCacheFailsClosed(t *testing.T) {
	c := &Cache{}
	if err := c.AuthorizeCredential(t.Context(), protocol.Credential{ExpiresAt: time.Now().Add(time.Hour)}); err == nil {
		t.Fatal("missing upstream accepted")
	}
	if _, err := c.RelayTrustBundle(t.Context()); err == nil {
		t.Fatal("missing upstream trust accepted")
	}
}
