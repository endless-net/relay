package testserver

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/endless-net/relay/internal/tlsconfig"
	protocol "github.com/endless-net/relay/relayapi/v1"
)

func TestLoadConfigIsStrictAndMissingConfigFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream.json")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("missing config accepted")
	}
	bundle, err := protocol.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	valid := Config{NetworkID: "n", Nodes: []string{"a"}, TrustBundle: bundle}
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		raw   []byte
		valid bool
	}{
		{"valid", raw, true}, {"unknown", []byte(`{"secret":true}`), false}, {"empty", []byte(`{}`), false}, {"trailing", append(append([]byte(nil), raw...), []byte(` {}`)...), false}, {"invalid trust", []byte(`{"network_id":"n","nodes":["a"]}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, tc.raw, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestTestserverPlainHTTPDoesNotBypassIdentity(t *testing.T) {
	policy, err := tlsconfig.NewIdentityPolicy("operator.example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{}, policy)
	for _, path := range []string{"/healthz", "/test/control"} {
		req := httptest.NewRequest(http.MethodPut, path, nil)
		response := httptest.NewRecorder()
		s.Handler().ServeHTTP(response, req)
		if response.Code >= 200 && response.Code < 300 {
			t.Fatal("unauthenticated harness access allowed", path)
		}
	}
}
