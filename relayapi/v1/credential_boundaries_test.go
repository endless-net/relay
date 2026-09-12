package protocolv1

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"
)

func TestCredentialCodecInvalidInputMatrix(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Minute)
	credential, err := Sign(key, "n", "a", expiry)
	if err != nil {
		t.Fatal(err)
	}
	public := base64.RawURLEncoding.EncodeToString(pub)
	if _, err := Sign(nil, "n", "a", expiry); err == nil {
		t.Fatal("invalid private key accepted")
	}
	if _, err := Sign(key, " n", "a", expiry); err == nil {
		t.Fatal("padded signing identity accepted")
	}
	if _, err := SigningPayload("", "n", "a", expiry); err == nil {
		t.Fatal("missing signing key ID accepted")
	}
	if _, err := SigningPayloadForPublicKey(nil, "n", "a", expiry); err == nil {
		t.Fatal("missing public key accepted")
	}
	if _, err := CredentialFromSignature(nil, "n", "a", expiry, make([]byte, 64)); err == nil {
		t.Fatal("missing external signer public key accepted")
	}
	if _, err := CredentialFromSignature(pub, "n", "a", expiry, nil); err == nil {
		t.Fatal("missing external signature accepted")
	}
	if _, err := CredentialFromSignature(pub, "", "a", expiry, make([]byte, 64)); err == nil {
		t.Fatal("missing external signer identity accepted")
	}
	for _, mutate := range []func(*Credential){
		func(c *Credential) { c.NetworkID = "" }, func(c *Credential) { c.KeyID = "" }, func(c *Credential) { c.KeyID = "wrong" }, func(c *Credential) { c.Signature = "invalid" },
	} {
		c := *credential
		mutate(&c)
		if Verify(c, public, time.Now()) == nil {
			t.Fatal("invalid credential verified")
		}
	}
	if Verify(*credential, "bad public key", time.Now()) == nil {
		t.Fatal("invalid trust key verified")
	}
	if _, err := NewSigningTrustBundle("bad public key"); err == nil {
		t.Fatal("invalid public key made trust bundle")
	}
	for _, raw := range [][]byte{[]byte(`{`), []byte(`{} garbage`), []byte(`{"expires_at":false}`)} {
		var c Credential
		if c.UnmarshalJSON(raw) == nil {
			t.Fatal("malformed credential decoded")
		}
		if DecodeStrict(raw, &c) == nil {
			t.Fatal("malformed credential decoded through strict codec")
		}
	}
}
