package extensions

import (
	"net/netip"
	"testing"
	"time"

	protocol "github.com/endless-net/relay/relayapi/v1"
)

func TestContracts(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	flow := Flow{Source: Peer{"net-a", "a"}, Destination: Peer{"net-b", "b"}}
	addr := []netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:443"), netip.MustParseAddrPort("[2001:db8::1]:443")}
	for name, err := range map[string]error{
		"features":    ValidateFeatures([]Feature{Lifecycle, Presence, RegionalRouting, Discovery, PeerRelay, Diagnostics, TrafficPolicy, OfflineResume, WebSocket}),
		"lifecycle":   (LifecycleNotice{"relay-a", "restarting", now.Add(time.Minute), 100, 100, 1000, []string{"relay-b"}}).Validate(now),
		"presence":    (PresenceNotice{flow, "sub", 1, "present", now.Add(time.Minute)}).Validate(now),
		"regions":     (RegionPolicy{"a", []string{"a", "b"}, []string{"b"}, []string{"r1"}, false}).Validate(),
		"bootstrap":   (BootstrapEndpoint{"r1", "relay.example", addr, "tls"}).Validate(),
		"discovery":   (DiscoveryEnvelope{flow, "candidates", "nonce", now.Add(time.Second), []byte{1}}).Validate(now),
		"offer":       (PeerRelayOffer{Peer{"net-a", "r"}, "offer", addr, now.Add(time.Minute)}).Validate(now),
		"reservation": (PeerRelayReservation{"offer", "res", Peer{"net-a", "r"}, flow, now.Add(time.Minute), []byte{1}}).Validate(now),
		"budget":      (TrafficBudget{1024, 2048, 4, 10}).Validate(),
		"resume":      (ResumePolicy{true, now.Add(time.Minute)}).Validate(now, now.Add(time.Hour)),
		"diagnostic":  (Diagnostic{"relay", "application", "unavailable", 20}).Validate(),
	} {
		t.Run(name, func(t *testing.T) {
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRejectInvalidAuthorityAndBounds(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	flow := Flow{Source: Peer{"n", "a"}, Destination: Peer{"n", "b"}}
	for name, err := range map[string]error{
		"unknown feature":      ValidateFeatures([]Feature{"unknown"}),
		"duplicate feature":    ValidateFeatures([]Feature{Discovery, Discovery}),
		"missing network":      (Peer{"", "a"}).Validate(),
		"self flow":            (Flow{flow.Source, flow.Source}).Validate(),
		"unknown lifecycle":    (LifecycleNotice{RelayID: "r", Reason: "allow"}).Validate(now),
		"reconnect overflow":   (LifecycleNotice{RelayID: "r", Reason: "draining", Deadline: now.Add(time.Minute), ReconnectAfterMS: 1<<63 - 1, JitterMS: 1, RetryBudgetMS: 1000}).Validate(now),
		"presence expired":     (PresenceNotice{flow, "sub", 1, "present", now}).Validate(now),
		"presence unknown":     (PresenceNotice{flow, "sub", 1, "secret", now.Add(time.Minute)}).Validate(now),
		"residency bypass":     (RegionPolicy{"a", []string{"a"}, []string{"b"}, []string{"r"}, false}).Validate(),
		"duplicate region":     (RegionPolicy{"a", []string{"a", "a"}, nil, []string{"r"}, false}).Validate(),
		"plaintext":            (BootstrapEndpoint{"r", "relay.example", []netip.AddrPort{netip.MustParseAddrPort("192.0.2.1:443")}, "http"}).Validate(),
		"unspecified address":  (BootstrapEndpoint{"r", "relay.example", []netip.AddrPort{netip.MustParseAddrPort("0.0.0.0:443")}, "tls"}).Validate(),
		"discovery expired":    (DiscoveryEnvelope{flow, "candidates", "nonce", now, []byte{1}}).Validate(now),
		"discovery oversized":  (DiscoveryEnvelope{flow, "candidates", "nonce", now.Add(time.Second), make([]byte, 4097)}).Validate(now),
		"discovery unknown":    (DiscoveryEnvelope{flow, "allow", "nonce", now.Add(time.Second), []byte{1}}).Validate(now),
		"offer expired":        (PeerRelayOffer{Relay: flow.Source, OfferID: "o", ExpiresAt: now}).Validate(now),
		"missing proof":        (PeerRelayReservation{OfferID: "o", ReservationID: "r", Relay: flow.Source, Flow: flow, ExpiresAt: now.Add(time.Minute)}).Validate(now),
		"implicit unlimited":   (TrafficBudget{}).Validate(),
		"extended credential":  (ResumePolicy{true, now.Add(time.Hour)}).Validate(now, now.Add(time.Minute)),
		"unbounded diagnostic": (Diagnostic{"relay", "application", "some private server message", 1}).Validate(),
	} {
		t.Run(name, func(t *testing.T) {
			if err == nil {
				t.Fatal("invalid contract accepted")
			}
		})
	}
}

func TestStrictExtensionDecoding(t *testing.T) {
	var d Diagnostic
	if err := protocol.DecodeStrict([]byte(`{"path":"relay","stage":"tls","outcome":"ok","rtt_ms":1,"secret":"x"}`), &d); err == nil {
		t.Fatal("unknown field accepted")
	}
	if err := protocol.DecodeStrict([]byte(`{} {}`), &d); err == nil {
		t.Fatal("trailing message accepted")
	}
}

func FuzzDiscoveryValidation(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"operation":"candidates"}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var e DiscoveryEnvelope
		if protocol.DecodeStrict(raw, &e) == nil {
			_ = e.Validate(time.Unix(1, 0))
		}
	})
}
