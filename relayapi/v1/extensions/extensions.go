// Package extensions defines validated planning contracts for Relay capabilities
// that are not yet implemented by the runtime. These objects MUST NOT be sent on
// relay-v1-tls: activation requires a separately approved transport contract.
package extensions

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Feature names describe support, not authority. Advertising support never grants access.
type Feature string

const (
	Lifecycle       Feature = "lifecycle"
	Presence        Feature = "presence"
	RegionalRouting Feature = "regional_routing"
	Discovery       Feature = "discovery"
	PeerRelay       Feature = "peer_relay"
	Diagnostics     Feature = "diagnostics"
	TrafficPolicy   Feature = "traffic_policy"
	OfflineResume   Feature = "offline_resume"
	WebSocket       Feature = "websocket"
)

func ValidateFeatures(features []Feature) error {
	seen := map[Feature]bool{}
	for _, f := range features {
		switch f {
		case Lifecycle, Presence, RegionalRouting, Discovery, PeerRelay, Diagnostics, TrafficPolicy, OfflineResume, WebSocket:
		default:
			return fmt.Errorf("unknown feature %q", f)
		}
		if seen[f] {
			return errors.New("duplicate feature")
		}
		seen[f] = true
	}
	return nil
}

type Peer struct {
	NetworkID string `json:"network_id"`
	NodeID    string `json:"node_id"`
}

func (p Peer) Validate() error {
	if !canonical(p.NetworkID) || !canonical(p.NodeID) {
		return errors.New("network and node are required")
	}
	return nil
}

// Flow is directional. Reverse initiation requires a distinct authorization.
type Flow struct {
	Source      Peer `json:"source"`
	Destination Peer `json:"destination"`
}

func (f Flow) Validate() error {
	if err := f.Source.Validate(); err != nil {
		return err
	}
	if err := f.Destination.Validate(); err != nil {
		return err
	}
	if f.Source == f.Destination {
		return errors.New("self flow is invalid")
	}
	return nil
}

// LifecycleNotice gives advisory reconnect timing; it cannot extend any lease.
// A client samples jitter in [ReconnectAfterMS, ReconnectAfterMS+JitterMS].
type LifecycleNotice struct {
	RelayID              string    `json:"relay_id"`
	Reason               string    `json:"reason"` // draining, restarting, rebalancing
	Deadline             time.Time `json:"deadline"`
	ReconnectAfterMS     int64     `json:"reconnect_after_ms"`
	JitterMS             int64     `json:"jitter_ms"`
	RetryBudgetMS        int64     `json:"retry_budget_ms"`
	PreferredEndpointIDs []string  `json:"preferred_endpoint_ids"`
}

func (n LifecycleNotice) Validate(now time.Time) error {
	if !canonical(n.RelayID) {
		return errors.New("relay id is required")
	}
	if n.Reason != "draining" && n.Reason != "restarting" && n.Reason != "rebalancing" {
		return errors.New("invalid lifecycle reason")
	}
	if !n.Deadline.After(now) || n.Deadline.Sub(now) > 24*time.Hour {
		return errors.New("invalid lifecycle deadline")
	}
	if n.ReconnectAfterMS < 0 || n.JitterMS < 0 || n.RetryBudgetMS <= 0 || n.RetryBudgetMS > 300000 {
		return errors.New("invalid reconnect timing")
	}
	if n.ReconnectAfterMS > n.RetryBudgetMS || n.JitterMS > n.RetryBudgetMS-n.ReconnectAfterMS {
		return errors.New("reconnect delay exceeds retry budget")
	}
	return unique(n.PreferredEndpointIDs)
}

// PresenceNotice is emitted only after authorizing the flow. Policy denial must
// not disclose whether the destination exists. Sequence is scoped to a subscription.
type PresenceNotice struct {
	Flow           Flow      `json:"flow"`
	SubscriptionID string    `json:"subscription_id"`
	Sequence       uint64    `json:"sequence"`
	State          string    `json:"state"` // present, unavailable, migrating
	ValidUntil     time.Time `json:"valid_until"`
}

func (n PresenceNotice) Validate(now time.Time) error {
	if err := n.Flow.Validate(); err != nil {
		return err
	}
	if !canonical(n.SubscriptionID) || n.Sequence == 0 || !n.ValidUntil.After(now) {
		return errors.New("invalid presence scope or validity")
	}
	if n.State != "present" && n.State != "unavailable" && n.State != "migrating" {
		return errors.New("invalid presence state")
	}
	return nil
}

// RegionPolicy references authenticated endpoint IDs from an accepted snapshot.
// Home is a preference. Allowed regions are a hard boundary, even during failure.
type RegionPolicy struct {
	Home            string   `json:"home"`
	Allowed         []string `json:"allowed"`
	Fallback        []string `json:"fallback"`
	EndpointIDs     []string `json:"endpoint_ids"`
	CrossRegionMesh bool     `json:"cross_region_mesh"`
}

func (p RegionPolicy) Validate() error {
	if err := unique(p.Allowed); err != nil {
		return err
	}
	if err := unique(p.Fallback); err != nil {
		return err
	}
	if err := unique(p.EndpointIDs); err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, r := range p.Allowed {
		allowed[r] = true
	}
	if !allowed[p.Home] || len(p.EndpointIDs) == 0 {
		return errors.New("home region and endpoints must be allowed")
	}
	for _, r := range p.Fallback {
		if !allowed[r] || r == p.Home {
			return errors.New("invalid fallback region")
		}
	}
	return nil
}

// BootstrapEndpoint separates dial addresses from TLS identity. Literal addresses
// support DNS failure and IPv4/IPv6; TLS verification remains mandatory.
type BootstrapEndpoint struct {
	ID        string           `json:"id"`
	TLSName   string           `json:"tls_name"`
	Addresses []netip.AddrPort `json:"addresses"`
	Transport string           `json:"transport"` // tls, wss; support is not implied by port 443
}

func (e BootstrapEndpoint) Validate() error {
	if !canonical(e.ID) || !canonical(e.TLSName) || strings.ContainsAny(e.TLSName, "/: ") {
		return errors.New("invalid endpoint identity")
	}
	if e.Transport != "tls" && e.Transport != "wss" {
		return errors.New("unsupported transport")
	}
	return addresses(e.Addresses)
}

// DiscoveryEnvelope carries an end-to-end authenticated encrypted message.
// Relays enforce directed authorization, size and rate limits but do not decrypt.
// Receivers bind the nonce, expiry, flow and operation to the authenticated payload
// and reject replay. A discovered address never grants route permission.
type DiscoveryEnvelope struct {
	Flow       Flow      `json:"flow"`
	Operation  string    `json:"operation"` // candidates, probe_request, probe_response
	Nonce      string    `json:"nonce"`
	ExpiresAt  time.Time `json:"expires_at"`
	Ciphertext []byte    `json:"ciphertext"`
}

func (e DiscoveryEnvelope) Validate(now time.Time) error {
	if err := e.Flow.Validate(); err != nil {
		return err
	}
	if e.Operation != "candidates" && e.Operation != "probe_request" && e.Operation != "probe_response" {
		return errors.New("invalid discovery operation")
	}
	if !canonical(e.Nonce) || len(e.Nonce) > 128 || !e.ExpiresAt.After(now) || e.ExpiresAt.Sub(now) > time.Minute {
		return errors.New("invalid discovery nonce or expiry")
	}
	if len(e.Ciphertext) == 0 || len(e.Ciphertext) > 4096 {
		return errors.New("invalid discovery payload size")
	}
	return nil
}

// PeerRelayOffer is an advertisement, never a grant. Authority must approve the
// relay node, flow and transport before returning a PeerRelayReservation.
type PeerRelayOffer struct {
	Relay     Peer             `json:"relay"`
	OfferID   string           `json:"offer_id"`
	Addresses []netip.AddrPort `json:"addresses"`
	ExpiresAt time.Time        `json:"expires_at"`
}

func (o PeerRelayOffer) Validate(now time.Time) error {
	if err := o.Relay.Validate(); err != nil {
		return err
	}
	if !canonical(o.OfferID) || !o.ExpiresAt.After(now) {
		return errors.New("invalid peer relay offer")
	}
	return addresses(o.Addresses)
}

// PeerRelayReservation binds a UDP relay allocation to a directed flow. The
// issuer-specific proof MUST be verified before use; Validate only checks shape.
// Reservation, offer, credential and policy expiry all bound access. Payload stays
// WireGuard encrypted. Reassignment never inherits an old reservation.
type PeerRelayReservation struct {
	OfferID        string    `json:"offer_id"`
	ReservationID  string    `json:"reservation_id"`
	Relay          Peer      `json:"relay"`
	Flow           Flow      `json:"flow"`
	ExpiresAt      time.Time `json:"expires_at"`
	AuthorityProof []byte    `json:"authority_proof"`
}

func (r PeerRelayReservation) Validate(now time.Time) error {
	if err := r.Flow.Validate(); err != nil {
		return err
	}
	if err := r.Relay.Validate(); err != nil {
		return err
	}
	if !canonical(r.OfferID) || !canonical(r.ReservationID) || !r.ExpiresAt.After(now) || len(r.AuthorityProof) == 0 || len(r.AuthorityProof) > 4096 {
		return errors.New("invalid peer relay reservation")
	}
	return nil
}

// TrafficBudget separates process protection from commercial entitlement.
// Zero is invalid; unlimited traffic requires an explicit policy outside this type.
type TrafficBudget struct {
	BytesPerSecond        int64 `json:"bytes_per_second"`
	BurstBytes            int64 `json:"burst_bytes"`
	MaxSessions           int64 `json:"max_sessions"`
	MaxDiscoveryPerSecond int64 `json:"max_discovery_per_second"`
}

func (b TrafficBudget) Validate() error {
	if b.BytesPerSecond <= 0 || b.BurstBytes <= 0 || b.MaxSessions <= 0 || b.MaxDiscoveryPerSecond <= 0 {
		return errors.New("traffic limits must be positive")
	}
	return nil
}

// ResumePolicy is a future local storage policy. It is NOT a new credential.
// Effective expiry is the earliest bound; no outage or restart extends authority.
type ResumePolicy struct {
	AllowStoredCredential bool      `json:"allow_stored_credential"`
	ValidUntil            time.Time `json:"valid_until"`
}

func (p ResumePolicy) Validate(now, credentialExpiry time.Time) error {
	if !p.ValidUntil.After(now) || p.ValidUntil.After(credentialExpiry) {
		return errors.New("resume policy exceeds credential validity")
	}
	return nil
}

// Diagnostic reports use bounded enums rather than arbitrary server error text.
// Peer identities and credentials must not become metric labels or be exported.
type Diagnostic struct {
	Path            string `json:"path"`    // direct, relay, peer_relay, unavailable
	Stage           string `json:"stage"`   // dns, tls, credential, authorization, session, mesh, application, captive_portal
	Outcome         string `json:"outcome"` // ok, denied, unavailable, expired, saturated, unsupported
	RTTMilliseconds int64  `json:"rtt_ms"`
}

func (d Diagnostic) Validate() error {
	if !member(d.Path, "direct", "relay", "peer_relay", "unavailable") || !member(d.Stage, "dns", "tls", "credential", "authorization", "session", "mesh", "application", "captive_portal") || !member(d.Outcome, "ok", "denied", "unavailable", "expired", "saturated", "unsupported") || d.RTTMilliseconds < 0 {
		return errors.New("invalid diagnostic")
	}
	return nil
}

func canonical(s string) bool { return s != "" && s == strings.TrimSpace(s) }
func member(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func unique(values []string) error {
	seen := map[string]bool{}
	for _, v := range values {
		if !canonical(v) || seen[v] {
			return errors.New("invalid or duplicate identifier")
		}
		seen[v] = true
	}
	return nil
}
func addresses(values []netip.AddrPort) error {
	if len(values) == 0 || len(values) > 16 {
		return errors.New("invalid address count")
	}
	seen := map[netip.AddrPort]bool{}
	for _, a := range values {
		if !a.IsValid() || a.Port() == 0 || a.Addr().IsUnspecified() || a.Addr().IsMulticast() || a.Addr().Zone() != "" || seen[a] {
			return errors.New("invalid or duplicate address")
		}
		seen[a] = true
	}
	return nil
}
