package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSnapshotRejectsUnknownTrailingAndInvalidContracts(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		wantErr bool
	}{
		{name: "empty snapshot", content: `{"version":1,"endpoints":[]}`},
		{name: "unknown field", content: `{"version":1,"endpoints":[],"legacy":true}`, wantErr: true},
		{name: "trailing value", content: `{"version":1,"endpoints":[]} {}`, wantErr: true},
		{name: "invalid version", content: `{"version":0,"endpoints":[]}`, wantErr: true},
		{name: "non-canonical endpoint", content: `{"version":1,"endpoints":[{"id":" relay","addr":"relay:9443","protocol":"relay-v1-tls","priority":0}]}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "endpoints.json")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadSnapshot(path)
			if (err != nil) != test.wantErr {
				t.Fatalf("loadSnapshot error = %v, want error %t", err, test.wantErr)
			}
		})
	}
}

func TestUpstreamURLRequiresHTTPSOrigin(t *testing.T) {
	for _, raw := range []string{"https://upstream:9447", "https://auth.operator.example/", "https://[::1]:9447"} {
		if err := validateUpstreamURL(raw); err != nil {
			t.Errorf("valid origin %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://upstream:9447", "upstream:9447", "https://", " https://upstream", "https://user:fixture@upstream", "https://upstream?query=1", "https://upstream?", "https://upstream#fragment", "https://upstream/prefix", "https://upstream:bad"} {
		if err := validateUpstreamURL(raw); err == nil {
			t.Errorf("accepted invalid origin %q", raw)
		}
	}
}
