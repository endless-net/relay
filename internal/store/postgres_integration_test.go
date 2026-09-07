//go:build integration

package store

import (
	"context"
	"os"
	"testing"
)

func TestPostgresReleasedEpochCannotBeReused(t *testing.T) {
	dsn := os.Getenv("E2E_POSTGRES_DSN")
	if dsn == "" {
		t.Fatal("E2E_POSTGRES_DSN required")
	}
	s, err := OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checkReleasedEpoch(t, s)
}
