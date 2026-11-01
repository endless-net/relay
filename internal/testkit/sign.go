// Package testkit contains synthetic credential fixtures, never used by runtime commands.
package testkit

import (
	"crypto/ed25519"
	"time"

	wire "github.com/endless-net/relay/internal/wire"
)

func Sign(key ed25519.PrivateKey, network, node string, expiry time.Time) (*wire.Credential, error) {
	pub := key.Public().(ed25519.PublicKey)
	raw, e := wire.SigningPayloadForPublicKey(pub, network, node, expiry)
	if e != nil {
		return nil, e
	}
	return wire.CredentialFromSignature(pub, network, node, expiry, ed25519.Sign(key, raw))
}
