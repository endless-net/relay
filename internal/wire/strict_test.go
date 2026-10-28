package wire

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestStrictRejectsAmbiguity(t *testing.T) {
	if err := CheckJSON([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}); err == nil {
		t.Fatal("invalid UTF-8 silently replaced")
	}
	for _, raw := range []string{`{"type":"client_frame","type":"client_frame"}`, `{"TYPE":"client_frame"}`, `{"credential":{"node_id":"a","node_id":"b"}}`, `{} {}`, `{"payload":"YQ==\n"}`, `{"payload":"YR=="}`} {
		var frame ClientFrame
		if e := DecodeStrict([]byte(raw), &frame); e == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	var frame ClientFrame
	if e := DecodeStrict([]byte(`{"type":"client_frame","protocol_version":1,"peer_id":"p","peer_network_id":"n","payload":"YQ=="}`), &frame); e != nil {
		t.Fatal(e)
	}
}

func TestCredentialCanonicalSignatureAndExpiry(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	pub := key.Public().(ed25519.PublicKey)
	now := time.Unix(1800000000, 0).UTC()
	expiry := now.Add(time.Minute)
	payload, e := SigningPayloadForPublicKey(pub, "n", "node", expiry)
	if e != nil {
		t.Fatal(e)
	}
	var shape map[string]any
	if e = json.Unmarshal(payload, &shape); e != nil || shape["schema"] != float64(3) {
		t.Fatal("canonical schema")
	}
	c, e := CredentialFromSignature(pub, "n", "node", expiry, ed25519.Sign(key, payload))
	if e != nil {
		t.Fatal(e)
	}
	trusted := base64.RawURLEncoding.EncodeToString(pub)
	if e = Verify(*c, trusted, now); e != nil {
		t.Fatal(e)
	}
	c.NetworkID = "foreign"
	if Verify(*c, trusted, now) == nil {
		t.Fatal("foreign context accepted")
	}
	c.NetworkID = "n"
	if Verify(*c, trusted, expiry) == nil {
		t.Fatal("expiry accepted")
	}
}
