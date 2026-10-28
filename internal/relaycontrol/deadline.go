package relaycontrol

import (
	"time"

	wire "github.com/endless-net/relay/internal/wire"
)

// AuthorizationDeadline captures current trust freshness, key/credential
// validity and the instance lease without starting a new freshness window.
func (c *Client) AuthorizationDeadline(credential wire.Credential) time.Time {
	c.mu.RLock()
	trustAt, until, bundle := c.trustAt, c.leaseExpires, c.trustBundle
	c.mu.RUnlock()
	if trustAt.IsZero() || until.IsZero() || credential.ExpiresAt.IsZero() {
		return time.Time{}
	}
	if limit := trustAt.Add(10 * time.Second); limit.Before(until) {
		until = limit
	}
	if limit := credential.ExpiresAt.Add(-time.Second); limit.Before(until) {
		until = limit
	}
	key, err := bundle.Resolve(credential.KeyID, time.Now().Add(time.Second))
	if err != nil {
		return time.Time{}
	}
	if key.NotAfter != nil && key.NotAfter.Add(-time.Second).Before(until) {
		until = key.NotAfter.Add(-time.Second)
	}
	if !time.Now().Before(until) {
		return time.Time{}
	}
	return until
}
