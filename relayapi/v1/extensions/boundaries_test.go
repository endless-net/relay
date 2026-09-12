package extensions

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestExtensionAuthorityAndAddressBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	flow := Flow{Source: Peer{"n", "a"}, Destination: Peer{"n", "b"}}
	addr := netip.MustParseAddrPort("192.0.2.1:443")
	for name, values := range map[string][]netip.AddrPort{
		"empty": nil, "too many": make([]netip.AddrPort, 17), "invalid": {{}}, "zero port": {netip.MustParseAddrPort("192.0.2.1:0")}, "multicast": {netip.MustParseAddrPort("224.0.0.1:443")}, "zoned": {netip.MustParseAddrPort("[fe80::1%eth0]:443")}, "duplicate": {addr, addr},
	} {
		t.Run(name, func(t *testing.T) {
			if err := (BootstrapEndpoint{ID: "r", TLSName: "relay.example", Transport: "tls", Addresses: values}).Validate(); err == nil {
				t.Fatal("invalid addresses accepted")
			}
		})
	}
	for name, err := range map[string]error{
		"source":                    (Flow{Destination: flow.Destination}).Validate(),
		"destination":               (Flow{Source: flow.Source}).Validate(),
		"lifecycle identity":        (LifecycleNotice{Reason: "draining", Deadline: now.Add(time.Second), RetryBudgetMS: 1}).Validate(now),
		"lifecycle deadline":        (LifecycleNotice{RelayID: "r", Reason: "draining", Deadline: now.Add(24*time.Hour + time.Nanosecond), RetryBudgetMS: 1}).Validate(now),
		"lifecycle timing":          (LifecycleNotice{RelayID: "r", Reason: "draining", Deadline: now.Add(time.Second), RetryBudgetMS: 300001}).Validate(now),
		"lifecycle negative jitter": (LifecycleNotice{RelayID: "r", Reason: "draining", Deadline: now.Add(time.Second), RetryBudgetMS: 1, JitterMS: -1}).Validate(now),
		"presence flow":             (PresenceNotice{SubscriptionID: "s", Sequence: 1, State: "present", ValidUntil: now.Add(time.Second)}).Validate(now),
		"presence sequence":         (PresenceNotice{Flow: flow, SubscriptionID: "s", State: "present", ValidUntil: now.Add(time.Second)}).Validate(now),
		"fallback duplicate":        (RegionPolicy{Home: "a", Allowed: []string{"a", "b"}, Fallback: []string{"b", "b"}, EndpointIDs: []string{"r"}}).Validate(),
		"endpoint duplicate":        (RegionPolicy{Home: "a", Allowed: []string{"a"}, EndpointIDs: []string{"r", "r"}}).Validate(),
		"home missing":              (RegionPolicy{Home: "a", Allowed: []string{"b"}, EndpointIDs: []string{"r"}}).Validate(),
		"tls identity":              (BootstrapEndpoint{ID: "r", TLSName: "https://relay.example", Transport: "tls", Addresses: []netip.AddrPort{addr}}).Validate(),
		"discovery flow":            (DiscoveryEnvelope{Operation: "candidates", Nonce: "n", ExpiresAt: now.Add(time.Second), Ciphertext: []byte{1}}).Validate(now),
		"offer identity":            (PeerRelayOffer{OfferID: "o", ExpiresAt: now.Add(time.Second), Addresses: []netip.AddrPort{addr}}).Validate(now),
		"reservation flow":          (PeerRelayReservation{Relay: flow.Source, OfferID: "o", ReservationID: "r", ExpiresAt: now.Add(time.Second), AuthorityProof: []byte{1}}).Validate(now),
		"reservation relay":         (PeerRelayReservation{Flow: flow, OfferID: "o", ReservationID: "r", ExpiresAt: now.Add(time.Second), AuthorityProof: []byte{1}}).Validate(now),
	} {
		t.Run(name, func(t *testing.T) {
			if err == nil {
				t.Fatal("invalid extension accepted")
			}
		})
	}
	for _, size := range []int{0, 1, 4096, 4097} {
		d := DiscoveryEnvelope{Flow: flow, Operation: "probe_response", Nonce: strings.Repeat("a", 128), ExpiresAt: now.Add(time.Minute), Ciphertext: make([]byte, size)}
		valid := size > 0 && size <= 4096
		if err := d.Validate(now); (err == nil) != valid {
			t.Fatalf("discovery size=%d: %v", size, err)
		}
		r := PeerRelayReservation{Flow: flow, Relay: Peer{"n", "r"}, OfferID: "o", ReservationID: "r", ExpiresAt: now.Add(time.Minute), AuthorityProof: make([]byte, size)}
		if err := r.Validate(now); (err == nil) != valid {
			t.Fatalf("proof size=%d: %v", size, err)
		}
	}
	for _, nonce := range []string{"", strings.Repeat("a", 129), " padded"} {
		if err := (DiscoveryEnvelope{Flow: flow, Operation: "probe_request", Nonce: nonce, ExpiresAt: now.Add(time.Second), Ciphertext: []byte{1}}).Validate(now); err == nil {
			t.Fatal("invalid nonce accepted")
		}
	}
	if err := (LifecycleNotice{RelayID: "r", Reason: "rebalancing", Deadline: now.Add(24 * time.Hour), ReconnectAfterMS: 299999, JitterMS: 1, RetryBudgetMS: 300000}).Validate(now); err != nil {
		t.Fatal("exact lifecycle maximum rejected", err)
	}
	if err := (ResumePolicy{ValidUntil: now.Add(time.Minute)}).Validate(now, now.Add(time.Minute)); err != nil {
		t.Fatal("equal credential bound rejected", err)
	}
}
