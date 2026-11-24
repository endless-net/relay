package store

import (
	"crypto/tls"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestDirectPostgresRejectsAlternativeTargetsAndCredentials(t *testing.T) {
	for _, dsn := range []string{
		"postgres://registry@127.0.0.1/relay",
		"postgres://registry@pg.test,other.test/relay",
		"postgres://registry:password@pg.test/relay",
		"postgres://registry@pg.test/relay?host=other.test",
		"postgres://registry@pg.test/relay?sslmode=disable",
		"postgres://registry@pg.test/relay?service=other",
	} {
		if _, err := DirectPostgresHost(dsn); err == nil {
			t.Errorf("accepted incompatible DSN %q", dsn)
		}
	}
}

func TestDirectPostgresDoesNotSourceAmbientCredentials(t *testing.T) {
	t.Setenv("PGSERVICE", "")
	t.Setenv("PGHOST", "other.test")
	t.Setenv("PGPORT", "9999")
	t.Setenv("PGPASSWORD", "test-ambient-password")
	t.Setenv("PGSSLKEY", "/unavailable/path")
	t.Setenv("PGOPTIONS", "-c role=other")
	transport := &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, ServerName: "pg.test",
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return nil, nil },
		VerifyConnection:     func(tls.ConnectionState) error { return nil },
	}
	input, err := databaseDSN("postgres://registry@pg.test/relay?sslmode=verify-full", transport)
	if err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := secureConfig(config, transport); err != nil {
		t.Fatal(err)
	}
	if config.Host != "pg.test" || config.Port != 5432 || config.User != "registry" ||
		config.Password != "" || config.RequireAuth != "none" || config.TLSConfig.ServerName != "pg.test" ||
		len(config.Fallbacks) != 0 || len(config.RuntimeParams) != 0 {
		t.Fatal("ambient configuration altered native SQL mTLS")
	}
}
