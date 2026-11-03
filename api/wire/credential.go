package wire

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// CredentialSigningPayload returns the existing v3 canonical signing bytes.
// The caller must authorize issuance, resolve the purpose-specific trusted key,
// bound the interval and use Signing. This helper never signs or grants access.
func CredentialSigningPayload(keyID, networkID, nodeID string, expiresAt time.Time) ([]byte, error) {
	key, ok := strings.CutPrefix(keyID, "ed25519:")
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(key)
	if !ok || err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != key || !canonicalID(networkID) || !canonicalID(nodeID) || len(networkID)+len(nodeID) > MaxLineBytes/6 || expiresAt.IsZero() || expiresAt.Year() < 0 || expiresAt.Year() > 9999 {
		return nil, ErrMessage
	}
	payload := struct {
		Schema    int       `json:"schema"`
		KeyID     string    `json:"key_id"`
		NetworkID string    `json:"network_id"`
		NodeID    string    `json:"node_id"`
		ExpiresAt time.Time `json:"expires_at"`
	}{3, keyID, networkID, nodeID, expiresAt.UTC()}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > MaxLineBytes {
		return nil, ErrMessage
	}
	return raw, nil
}
