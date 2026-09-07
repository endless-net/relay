package authz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	protocolv1 "github.com/endless-net/relay/protocol/v1"
)

var ErrDenied = errors.New("relay authorization denied")

type Authorizer interface {
	AuthorizeCredential(context.Context, protocolv1.Credential) error
	AuthorizePeer(context.Context, protocolv1.Credential, string) error
	RelayTrustBundle(context.Context) (protocolv1.SigningTrustBundle, error)
}

type Cache struct {
	Upstream    Authorizer
	FreshTTL    time.Duration
	StaleTTL    time.Duration
	NegativeTTL time.Duration
	MaxEntries  int
	Now         func() time.Time

	mu      sync.Mutex
	entries map[string]entry
	bundle  bundleEntry
}

type entry struct {
	allowed  bool
	storedAt time.Time
}

type bundleEntry struct {
	bundle   protocolv1.SigningTrustBundle
	storedAt time.Time
}

func NewCache(upstream Authorizer) *Cache {
	return &Cache{Upstream: upstream, FreshTTL: 5 * time.Second, StaleTTL: 30 * time.Second, NegativeTTL: time.Second, MaxEntries: 10000, Now: time.Now, entries: map[string]entry{}}
}

func (c *Cache) AuthorizeCredential(ctx context.Context, credential protocolv1.Credential) error {
	return c.cached(cacheKey("credential", credential, ""), credential.ExpiresAt, func() error {
		return c.Upstream.AuthorizeCredential(ctx, credential)
	})
}

func (c *Cache) AuthorizePeer(ctx context.Context, credential protocolv1.Credential, peerID string) error {
	return c.cached(cacheKey("peer", credential, peerID), credential.ExpiresAt, func() error {
		return c.Upstream.AuthorizePeer(ctx, credential, peerID)
	})
}

func (c *Cache) cached(key string, expires time.Time, load func() error) error {
	if c.Upstream == nil {
		return errors.New("relay authorization upstream is required")
	}
	now := c.now()
	if !now.Before(expires) {
		return ErrDenied
	}
	c.mu.Lock()
	cached, found := c.entries[key]
	c.mu.Unlock()
	if found {
		ttl := c.FreshTTL
		if !cached.allowed {
			ttl = c.NegativeTTL
		}
		if now.Sub(cached.storedAt) < ttl {
			if !cached.allowed {
				return ErrDenied
			}
			return nil
		}
	}
	err := load()
	now = c.now()
	if !now.Before(expires) {
		return ErrDenied
	}
	if err == nil || errors.Is(err, ErrDenied) {
		c.put(key, entry{allowed: err == nil, storedAt: now})
		return err
	}
	if found && cached.allowed && now.Sub(cached.storedAt) < c.StaleTTL {
		return nil
	}
	return err
}

func (c *Cache) RelayTrustBundle(ctx context.Context) (protocolv1.SigningTrustBundle, error) {
	if c.Upstream == nil {
		return protocolv1.SigningTrustBundle{}, errors.New("relay authorization upstream is required")
	}
	now := c.now()
	c.mu.Lock()
	cached := c.bundle
	c.mu.Unlock()
	if !cached.storedAt.IsZero() && now.Sub(cached.storedAt) < c.FreshTTL {
		return cached.bundle, nil
	}
	bundle, err := c.Upstream.RelayTrustBundle(ctx)
	if err == nil {
		c.mu.Lock()
		c.bundle = bundleEntry{bundle: bundle, storedAt: now}
		c.mu.Unlock()
		return bundle, nil
	}
	if !cached.storedAt.IsZero() && c.now().Sub(cached.storedAt) < c.StaleTTL {
		return cached.bundle, nil
	}
	return protocolv1.SigningTrustBundle{}, err
}

func (c *Cache) put(key string, value entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]entry{}
	}
	if c.MaxEntries > 0 && len(c.entries) >= c.MaxEntries {
		var oldestKey string
		var oldest time.Time
		for candidate, item := range c.entries {
			if oldestKey == "" || item.storedAt.Before(oldest) {
				oldestKey, oldest = candidate, item.storedAt
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = value
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func cacheKey(action string, credential protocolv1.Credential, peerID string) string {
	raw, _ := json.Marshal(struct {
		Action     string                `json:"action"`
		Credential protocolv1.Credential `json:"credential"`
		PeerID     string                `json:"peer_id"`
	}{action, credential, peerID})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
