package store

import (
	"context"
	"crypto/tls"
	"testing"

	"github.com/endless-net/relay/internal/tlsconfig"
)

func TestRegistryRejectsPlaintextAndStaticTLS(t *testing.T) {
	for _, config := range []*tls.Config{nil, {MinVersion: tls.VersionTLS13}} {
		transport := tlsconfig.DatabaseTransport{TLSConfig: config}
		if _, e := OpenPostgres(context.Background(), "postgres://unused/fixture", transport); e == nil {
			t.Fatal("unverified database transport accepted")
		}
		if e := Migrate(context.Background(), "postgres://unused/fixture", transport); e == nil {
			t.Fatal("unverified migration transport accepted")
		}
	}
}
