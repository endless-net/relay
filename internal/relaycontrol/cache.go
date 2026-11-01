package relaycontrol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/endless-net/relay/internal/relay"
	wire "github.com/endless-net/relay/internal/wire"
)

type routeEntry struct {
	route   relay.PeerRoute
	started time.Time
}

func (c *Client) AuthorizePeer(ctx context.Context, credential wire.Credential, epoch int64, network, node string) (relay.PeerRoute, error) {
	if e := ctx.Err(); e != nil {
		return relay.PeerRoute{}, e
	}
	raw, _ := json.Marshal(struct {
		Credential    wire.Credential
		Epoch         int64
		Network, Node string
	}{credential, epoch, network, node})
	hash := sha256.Sum256(raw)
	key := hex.EncodeToString(hash[:])
	started := time.Now()
	if !started.Add(time.Second).Before(credential.ExpiresAt) {
		return relay.PeerRoute{}, errors.New("credential_expired")
	}
	c.cacheMu.Lock()
	entry, found := c.routes[key]
	c.cacheMu.Unlock()
	if found && started.Sub(entry.started) < 5*time.Second && entry.route.Valid() {
		return entry.route, nil
	}
	rpcCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	route, err := c.authorizePeerRPC(rpcCtx, credential, epoch, network, node)
	if rpcCtx.Err() != nil {
		return relay.PeerRoute{}, rpcCtx.Err()
	}
	if err != nil {
		if found && time.Since(entry.started) < 10*time.Second && errors.Is(err, relay.ErrControlUnavailable) && entry.route.Valid() {
			return entry.route, nil
		}
		c.cacheMu.Lock()
		delete(c.routes, key)
		c.cacheMu.Unlock()
		return relay.PeerRoute{}, err
	}
	route.ValidUntil = started.Add(10 * time.Second)
	if limit := credential.ExpiresAt.Add(-time.Second); limit.Before(route.ValidUntil) {
		route.ValidUntil = limit
	}
	bundle, trustErr := c.TrustBundle()
	if trustErr != nil {
		return relay.PeerRoute{}, trustErr
	}
	trustKey, trustErr := bundle.Resolve(credential.KeyID, time.Now().Add(time.Second))
	if trustErr != nil {
		return relay.PeerRoute{}, trustErr
	}
	if limit := trustKey.NotAfter; limit != nil && limit.Add(-time.Second).Before(route.ValidUntil) {
		route.ValidUntil = limit.Add(-time.Second)
	}
	route.CurrentDeadline = func() time.Time {
		c.cacheMu.Lock()
		e, ok := c.routes[key]
		c.cacheMu.Unlock()
		if !ok || !e.started.Equal(started) || !time.Now().Before(e.route.ValidUntil) {
			return time.Time{}
		}
		until := c.AuthorizationDeadline(credential)
		if until.IsZero() {
			return time.Time{}
		}
		if route.ValidUntil.Before(until) {
			until = route.ValidUntil
		}
		return until
	}
	route.Valid = func() bool { until := route.CurrentDeadline(); return !until.IsZero() && time.Now().Before(until) }

	c.cacheMu.Lock()
	if c.routes == nil {
		c.routes = map[string]routeEntry{}
	}
	if len(c.routes) >= 10000 {
		for k, e := range c.routes {
			if time.Since(e.started) >= 10*time.Second {
				delete(c.routes, k)
			}
		}
	}
	if _, exists := c.routes[key]; !exists && len(c.routes) >= 10000 {
		c.cacheMu.Unlock()
		return relay.PeerRoute{}, errors.New("route_cache_full")
	}
	c.routes[key] = routeEntry{route, started}
	c.cacheMu.Unlock()
	return route, nil
}
