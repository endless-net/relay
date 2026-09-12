package main

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSmokeTLSVerification(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "test-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	addr := strings.TrimPrefix(server.URL, "https://")
	if err := checkRelayTLS(ctx, addr, "example.com", ca); err != nil {
		t.Fatal("trusted TLS endpoint rejected", err)
	}
	if err := checkRelayTLS(ctx, addr, "wrong.example", ca); err == nil {
		t.Fatal("wrong TLS name accepted")
	}
	if err := checkRelayTLS(ctx, addr, "example.com", ""); err == nil {
		t.Fatal("untrusted test CA accepted")
	}
	if err := checkRelayTLS(ctx, addr, "example.com", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing CA accepted")
	}
	old := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	old.TLS = &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
	old.StartTLS()
	t.Cleanup(old.Close)
	if err := checkRelayTLS(ctx, strings.TrimPrefix(old.URL, "https://"), "example.com", ca); err == nil {
		t.Fatal("TLS 1.2 endpoint accepted")
	}
}
