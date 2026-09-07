package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	protocolv1 "github.com/endless-net/relay/protocol/v1"
)

type stubAuthorizer struct {
	err    error
	calls  int
	bundle protocolv1.SigningTrustBundle
}

func (s *stubAuthorizer) AuthorizeCredential(context.Context, protocolv1.Credential) error {
	s.calls++
	return s.err
}

func (s *stubAuthorizer) AuthorizePeer(context.Context, protocolv1.Credential, string, string) error {
	s.calls++
	return s.err
}

func (s *stubAuthorizer) RelayTrustBundle(context.Context) (protocolv1.SigningTrustBundle, error) {
	return s.bundle, s.err
}

func TestCacheUsesPositiveStaleWindowOnlyOnUpstreamFailure(t *testing.T) {
	now := time.Now().UTC()
	upstream := &stubAuthorizer{}
	cache := NewCache(upstream)
	cache.Now = func() time.Time { return now }
	credential := protocolv1.Credential{NetworkID: "network", NodeID: "node", ExpiresAt: now.Add(time.Hour)}
	if err := cache.AuthorizeCredential(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Second)
	upstream.err = errors.New("unavailable")
	if err := cache.AuthorizeCredential(context.Background(), credential); err != nil {
		t.Fatalf("stale authorization failed: %v", err)
	}
	now = now.Add(25 * time.Second)
	if err := cache.AuthorizeCredential(context.Background(), credential); err == nil {
		t.Fatal("authorization survived beyond stale window")
	}
}

func TestPeerCacheSeparatesDestinationNetworks(t *testing.T) {
	upstream := &stubAuthorizer{}
	cache := NewCache(upstream)
	credential := protocolv1.Credential{NetworkID: "source-network", NodeID: "source", ExpiresAt: time.Now().Add(time.Hour)}
	if err := cache.AuthorizePeer(t.Context(), credential, "allowed-network", "same-node"); err != nil {
		t.Fatal(err)
	}
	upstream.err = ErrDenied
	if err := cache.AuthorizePeer(t.Context(), credential, "other-network", "same-node"); !errors.Is(err, ErrDenied) {
		t.Fatal("another network reused positive decision", err)
	}
	if err := cache.AuthorizePeer(t.Context(), credential, "allowed-network", "same-node"); err != nil {
		t.Fatal("negative decision polluted another network", err)
	}
	if upstream.calls != 2 {
		t.Fatalf("upstream calls=%d", upstream.calls)
	}
	for _, network := range []string{"", " allowed-network", "allowed-network "} {
		if err := cache.AuthorizePeer(t.Context(), credential, network, "same-node"); err == nil {
			t.Fatal("noncanonical network accepted")
		}
	}
	if upstream.calls != 2 {
		t.Fatal("invalid network reached upstream")
	}
}

func TestCacheDoesNotUseNegativeStaleWindow(t *testing.T) {
	now := time.Now().UTC()
	upstream := &stubAuthorizer{err: ErrDenied}
	cache := NewCache(upstream)
	cache.Now = func() time.Time { return now }
	credential := protocolv1.Credential{NetworkID: "network", NodeID: "node", ExpiresAt: now.Add(time.Hour)}
	if err := cache.AuthorizeCredential(context.Background(), credential); !errors.Is(err, ErrDenied) {
		t.Fatalf("negative authorization error = %v", err)
	}
	now = now.Add(2 * time.Second)
	upstream.err = errors.New("unavailable")
	if err := cache.AuthorizeCredential(context.Background(), credential); err == nil {
		t.Fatal("negative decision gained a stale window")
	}
}

func TestCacheRechecksDeadlineAfterUpstream(t *testing.T) {
	for _, expiry := range []bool{false, true} {
		now := time.Now()
		cache := NewCache(&stubAuthorizer{})
		cache.Now = func() time.Time { return now }
		expires := now.Add(time.Hour)
		if expiry {
			expires = now.Add(10 * time.Second)
		}
		if err := cache.cached("key", expires, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		now = now.Add(6 * time.Second)
		err := cache.cached("key", expires, func() error { now = now.Add(31 * time.Second); return errors.New("delayed outage") })
		if err == nil {
			t.Fatalf("delayed response extended deadline, expiry case %v", expiry)
		}
	}
}
func TestSuccessfulUpstreamCannotAuthorizeExpiredCredential(t *testing.T) {
	now := time.Now()
	cache := NewCache(&stubAuthorizer{})
	cache.Now = func() time.Time { return now }
	expires := now.Add(time.Second)
	err := cache.cached("key", expires, func() error { now = expires; return nil })
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("expiry after success: %v", err)
	}
}
