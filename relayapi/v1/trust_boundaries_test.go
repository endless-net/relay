package protocolv1

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"
)

func TestTrustBundleValidationAndValidityBoundaries(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	base, err := NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	end := now.Add(time.Hour)
	for _, tc := range []struct {
		name   string
		mutate func(*SigningTrustBundle)
	}{
		{"version", func(b *SigningTrustBundle) { b.Version = 0 }},
		{"empty", func(b *SigningTrustBundle) { b.Keys = nil }},
		{"missing active", func(b *SigningTrustBundle) { b.ActiveKeyID = "missing" }},
		{"padded active", func(b *SigningTrustBundle) { b.ActiveKeyID = " " + b.ActiveKeyID }},
		{"duplicate", func(b *SigningTrustBundle) { b.Keys = append(b.Keys, b.Keys[0]) }},
		{"algorithm", func(b *SigningTrustBundle) { b.Keys[0].Algorithm = "rsa" }},
		{"key id", func(b *SigningTrustBundle) { b.Keys[0].KeyID = "wrong" }},
		{"public key", func(b *SigningTrustBundle) { b.Keys[0].PublicKey = "invalid" }},
		{"empty window", func(b *SigningTrustBundle) { b.Keys[0].NotBefore = &now; b.Keys[0].NotAfter = &now }},
		{"reversed window", func(b *SigningTrustBundle) { b.Keys[0].NotBefore = &end; b.Keys[0].NotAfter = &now }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := base
			b.Keys = append([]SigningTrustKey(nil), base.Keys...)
			tc.mutate(&b)
			if b.Validate() == nil {
				t.Fatal("invalid trust accepted")
			}
			if _, err := b.Resolve(base.ActiveKeyID, now); err == nil {
				t.Fatal("invalid trust resolved")
			}
		})
	}
	base.Keys[0].NotBefore = &now
	base.Keys[0].NotAfter = &end
	for _, tc := range []struct {
		name  string
		at    time.Time
		valid bool
	}{
		{"before", now.Add(-time.Nanosecond), false}, {"inclusive start", now, true}, {"before expiry", end.Add(-time.Nanosecond), true}, {"exclusive end", end, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := base.Resolve(base.ActiveKeyID, tc.at)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	if _, err := base.Resolve("missing", now); err == nil {
		t.Fatal("unknown key resolved")
	}
}

func TestEndpointSnapshotValidationMatrix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot EndpointSnapshot
		valid    bool
	}{
		{"empty", EndpointSnapshot{Version: 1}, true},
		{"zero version", EndpointSnapshot{}, false},
		{"valid", EndpointSnapshot{Version: 1, Endpoints: []Endpoint{{ID: "r", Addr: "relay:443", Protocol: EndpointProtocolTLS}}}, true},
		{"duplicate", EndpointSnapshot{Version: 1, Endpoints: []Endpoint{{ID: "r", Addr: "relay:443", Protocol: EndpointProtocolTLS}, {ID: "r", Addr: "other:443", Protocol: EndpointProtocolTLS}}}, false},
		{"padded id", EndpointSnapshot{Version: 1, Endpoints: []Endpoint{{ID: " r", Addr: "relay:443", Protocol: EndpointProtocolTLS}}}, false},
		{"empty address", EndpointSnapshot{Version: 1, Endpoints: []Endpoint{{ID: "r", Protocol: EndpointProtocolTLS}}}, false},
		{"protocol", EndpointSnapshot{Version: 1, Endpoints: []Endpoint{{ID: "r", Addr: "relay:443", Protocol: "http"}}}, false},
		{"region", EndpointSnapshot{Version: 1, Endpoints: []Endpoint{{ID: "r", Addr: "relay:443", Protocol: EndpointProtocolTLS, Region: " region"}}}, false},
		{"priority", EndpointSnapshot{Version: 1, Endpoints: []Endpoint{{ID: "r", Addr: "relay:443", Protocol: EndpointProtocolTLS, Priority: -1}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.snapshot.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestPublicFrameAndHeartbeatBoundaries(t *testing.T) {
	for _, size := range []int{0, 1, MaxFramePayloadBytes, MaxFramePayloadBytes + 1} {
		frame := ClientFrame{Type: MessageClientFrame, ProtocolVersion: Version, PeerID: "b", PeerNetworkID: "network", Payload: make([]byte, size)}
		if err := frame.Validate(); (err == nil) != (size > 0 && size <= MaxFramePayloadBytes) {
			t.Fatalf("payload size %d: %v", size, err)
		}
	}
	for _, interval := range []int{-1, 0, MinHeartbeatIntervalMS - 1, MinHeartbeatIntervalMS, MaxHeartbeatIntervalMS, MaxHeartbeatIntervalMS + 1} {
		valid := interval == 0 || (interval >= MinHeartbeatIntervalMS && interval <= MaxHeartbeatIntervalMS)
		if err := (ClientHello{Type: MessageClientHello, ProtocolVersion: Version, HeartbeatIntervalMS: interval}).Validate(); (err == nil) != valid {
			t.Fatalf("heartbeat %d: %v", interval, err)
		}
	}
}

func FuzzStrictPublicFrame(f *testing.F) {
	f.Add([]byte(`{"type":"frame","protocol_version":1,"peer_id":"b","peer_network_id":"n","payload":"AQ=="}`))
	f.Add([]byte(`{} {}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var frame ClientFrame
		if DecodeStrict(raw, &frame) == nil {
			_ = frame.Validate()
		}
	})
}
