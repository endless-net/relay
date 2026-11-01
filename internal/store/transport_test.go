package store

import (
	"context"
	"crypto/tls"
	"testing"
)

func TestRegistryRejectsPlaintextAndStaticTLS(t *testing.T) {
	for _, transport := range []*tls.Config{nil, {MinVersion: tls.VersionTLS13}} {
		if _, e := OpenPostgres(context.Background(), "postgres://unused/fixture", transport); e == nil {
			t.Fatal("unverified database transport accepted")
		}
		if e := Migrate(context.Background(), "postgres://unused/fixture", transport); e == nil {
			t.Fatal("unverified migration transport accepted")
		}
	}
}
