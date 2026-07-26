package protocolv1

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCredentialRejectsTamperExpiryAndLegacyAlgorithms(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := Sign(privateKey, "network", "node", time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	publicKeyText := encodePublicKey(publicKey)
	if err := Verify(*credential, publicKeyText, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	tampered := *credential
	tampered.NodeID = "other-node"
	if err := Verify(tampered, publicKeyText, time.Now().UTC()); err == nil {
		t.Fatal("tampered credential verified")
	}
	legacy := *credential
	legacy.Algorithm = "ed25519-relay-credential-v2"
	if err := Verify(legacy, publicKeyText, time.Now().UTC()); err == nil {
		t.Fatal("legacy credential algorithm verified")
	}
	expired, err := Sign(privateKey, "network", "node", time.Now().UTC().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(*expired, publicKeyText, time.Now().UTC()); err == nil {
		t.Fatal("expired credential verified")
	}
}

func TestCredentialRoundTripAndStrictJSON(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := Sign(privateKey, "network-1", "node-1", time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	bundle, err := NewSigningTrustBundle(encodePublicKey(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := bundle.Resolve(credential.KeyID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(*credential, trusted.PublicKey, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw[:len(raw)-1], []byte(`,"unknown":true}`)...)
	var decoded Credential
	if err := json.Unmarshal(raw, &decoded); err == nil {
		t.Fatal("expected unknown credential field to be rejected")
	}
	canonical, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "public_key") {
		t.Fatal("credential embedded a public key")
	}
	if err := decoded.UnmarshalJSON(append(canonical, []byte(`{}`)...)); err == nil {
		t.Fatal("credential accepted trailing JSON")
	}
}

func TestPublicProtocolRequiresExplicitVersionAndCanonicalIdentifiers(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Sign(privateKey, " network", "node", time.Now().Add(time.Minute)); err == nil {
		t.Fatal("credential signer accepted a non-canonical network id")
	}
	hello := ClientHello{}
	if err := hello.Validate(); err == nil {
		t.Fatal("client hello accepted missing type and version")
	}
	hello.Type = MessageClientHello
	hello.ProtocolVersion = Version
	hello.HeartbeatIntervalMS = MinHeartbeatIntervalMS - 1
	if err := hello.Validate(); err == nil {
		t.Fatal("client hello accepted an invalid heartbeat")
	}
	frame := ClientFrame{Type: MessageClientFrame, ProtocolVersion: Version, PeerID: " peer", Payload: []byte("payload")}
	if err := frame.Validate(); err == nil {
		t.Fatal("client frame accepted a non-canonical peer id")
	}
	raw, err := json.Marshal(ClientHello{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"type":"client_hello"`) || strings.Contains(string(raw), `"protocol_version":1`) {
		t.Fatalf("marshal inserted protocol defaults: %s", raw)
	}
}

func encodePublicKey(publicKey ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(publicKey)
}
