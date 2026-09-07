package tlsconfig

import (
	"crypto/tls"
	"errors"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls"
)

// IdentityPolicy is shared by public-product control, mesh and snapshot paths.
// Empty settings select the documented operator defaults, never extra identities.
type IdentityPolicy struct {
	TrustDomain   string
	CoordinatorID string
	UpstreamID    string
}

func NewIdentityPolicy(domain, coordinator, upstream string) (IdentityPolicy, error) {
	if domain == "" {
		domain = DefaultTrustDomain
	}
	td, err := spiffeid.TrustDomainFromString(domain)
	if err != nil || domain != strings.TrimSpace(domain) || td.Name() != domain {
		return IdentityPolicy{}, errors.New("invalid trust domain")
	}
	if coordinator == "" {
		coordinator = "spiffe://" + domain + "/service/relay-coordinator"
	}
	if upstream == "" {
		upstream = "spiffe://" + domain + "/service/coordinator"
	}
	for _, raw := range []string{coordinator, upstream} {
		id, err := spiffeid.FromString(raw)
		if err != nil || id.TrustDomain() != td || id.Path() == "" || strings.HasPrefix(id.Path(), "/relay/") {
			return IdentityPolicy{}, errors.New("service identity must be a distinct service in the configured trust domain")
		}
	}
	if coordinator == upstream {
		return IdentityPolicy{}, errors.New("upstream and Relay Coordinator identities must differ")
	}
	return IdentityPolicy{domain, coordinator, upstream}, nil
}

func (p IdentityPolicy) Effective() IdentityPolicy {
	if p.TrustDomain == "" {
		p, _ = NewIdentityPolicy("", "", "")
	}
	return p
}

func (p IdentityPolicy) RelayIdentity(relayID string) (spiffeid.ID, error) {
	p = p.Effective()
	if relayID == "" || relayID != strings.TrimSpace(relayID) || strings.ContainsAny(relayID, "/?#%") {
		return spiffeid.ID{}, errors.New("invalid relay id")
	}
	return spiffeid.FromString("spiffe://" + p.TrustDomain + "/relay/" + relayID)
}

// PeerID must only be called with a connection authenticated by the configured
// TLS verifier (SPIFFE callback or standard mutual TLS), never an unverified socket.
func PeerID(state tls.ConnectionState) (spiffeid.ID, error) {
	return spiffetls.PeerIDFromConnectionState(state)
}
