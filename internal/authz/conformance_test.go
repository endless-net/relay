package authz

import (
	"context"
	"encoding/json"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"
)

// These fixtures are authored independently of authorizationRequest and the
// E2E upstream decoder. Their signature is deliberately not a usable credential.
func TestPublishedUpstreamRequestFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/upstream-requests.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []map[string]any
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture["action"].(string), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/internal/coordinator/relay-control/v1/authorize" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("transport contract differs")
				}
				var got map[string]any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				if !reflect.DeepEqual(got, fixture) {
					t.Error("request differs from published fixture")
				}
				w.WriteHeader(204)
			}))
			defer server.Close()
			a := HTTPAuthorizer{BaseURL: server.URL, HTTPClient: server.Client()}
			credential := protocolv1.Credential{Algorithm: "ed25519-relay-credential-v3", KeyID: "fixture-key", NetworkID: "fixture-network", NodeID: "fixture-source", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Signature: "non-credential-fixture"}
			var err error
			if fixture["action"] == "peer" {
				err = a.AuthorizePeer(context.Background(), credential, "fixture-target")
			} else {
				err = a.AuthorizeCredential(context.Background(), credential)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
