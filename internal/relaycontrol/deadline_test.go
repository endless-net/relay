package relaycontrol

import (
	"encoding/base64"
	"testing"
	"time"

	wire "github.com/endless-net/relay/internal/wire"
)

func TestAuthorizationDeadlineIncludesEveryCurrentAuthorityBound(t *testing.T) {
	now := time.Now()
	for _, name := range []string{"trust-freshness", "instance", "credential", "key"} {
		t.Run(name, func(t *testing.T) {
			bundle, err := wire.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
			if err != nil {
				t.Fatal(err)
			}
			credential := wire.Credential{KeyID: bundle.ActiveKeyID, ExpiresAt: now.Add(time.Minute)}
			client := &Client{trustAt: now, leaseExpires: now.Add(30 * time.Second), trustBundle: bundle}
			limit := now.Add(500 * time.Millisecond)
			switch name {
			case "trust-freshness":
				client.trustAt = limit.Add(-10 * time.Second)
			case "instance":
				client.leaseExpires = limit
			case "credential":
				credential.ExpiresAt = limit.Add(time.Second)
			case "key":
				expiry := limit.Add(time.Second)
				client.trustBundle.Keys[0].NotAfter = &expiry
			}
			first := client.AuthorizationDeadline(credential)
			if !first.Equal(limit) {
				t.Fatal("authority deadline omitted bound", name, first, limit)
			}
			if again := client.AuthorizationDeadline(credential); !again.Equal(first) {
				t.Fatal("read restarted authority lifetime")
			}
		})
	}
}
