//go:build product

package product

import (
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRuntimeDirectDNSRegistryRotatesSVID(t *testing.T) {
	f := newFixtureWithDatabaseProfile(t, "", "127.0.0.1:0", true)
	_, port, err := net.SplitHostPort(f.registryAddress)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(t.Context(), fmt.Sprintf("host=%s port=%s user=relaytest dbname=relay009 sslmode=disable", filepath.Join(f.dir, "pgsocket"), port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(t.Context()) }()

	identity := "spiffe://endlessnet.ru/service/relay-coordinator"
	short := f.pki.issue(t, identity, "relaytest", 12*time.Second)
	f.workloads["rc"].update(t, identity, short, f.pki.ca.Raw)
	if _, err := admin.Exec(t.Context(), "SELECT pg_terminate_backend(pid) FROM pg_stat_ssl WHERE ssl"); err != nil {
		t.Fatal(err)
	}
	countConnections := func(serial string) int {
		t.Helper()
		var count int
		if err := admin.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_ssl WHERE ssl AND client_serial::text=$1", serial).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	shortSerial := short.Leaf.SerialNumber.String()
	for until := time.Now().Add(5 * time.Second); countConnections(shortSerial) == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(until) {
			t.Fatal("SQL pool did not use the short-lived SVID")
		}
	}

	fresh := f.pki.issue(t, identity, "relaytest", time.Hour)
	f.workloads["rc"].update(t, identity, fresh, f.pki.ca.Raw)
	freshSerial := fresh.Leaf.SerialNumber.String()
	until := short.Leaf.NotAfter.Add(-2 * time.Second)
	for countConnections(shortSerial) != 0 || countConnections(freshSerial) == 0 {
		if time.Now().After(until) {
			t.Fatal("SQL pool did not drain the older SVID before its hard cutoff")
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitReady(t, f.ctx, f.coordinatorDiagnostics)
	assertAlive(t, f.coordinator)
	for i, address := range f.diagnostics {
		waitReady(t, f.ctx, address)
		assertAlive(t, f.processes[i])
	}
}
