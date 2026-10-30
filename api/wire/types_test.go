package wire_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/endless-net/relay/api/wire"
)

func TestProducerJSONFixtures(t *testing.T) {
	for _, test := range []struct {
		name   string
		target any
	}{
		{"Endpoint", &wire.Endpoint{}}, {"Credential", &wire.Credential{}},
	} {
		raw, err := os.ReadFile("../fixtures/" + test.name + ".valid.json")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, test.target); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(test.target)
		if err != nil {
			t.Fatal(err)
		}
		var original, actual any
		if err := json.Unmarshal(raw, &original); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &actual); err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(original)
		b, _ := json.Marshal(actual)
		if string(a) != string(b) {
			t.Fatal("producer JSON shape changed")
		}
	}
}

func TestCredentialTimePrecisionAndSnapshotRevision(t *testing.T) {
	original := wire.Credential{Algorithm: "ed25519-relay-credential-v3", ExpiresAt: time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.UTC)}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded wire.Credential
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.ExpiresAt.Equal(original.ExpiresAt) {
		t.Fatal("expiry precision lost")
	}
	snapshot := wire.EndpointSnapshot{Version: 9007199254740993, Endpoints: []wire.Endpoint{{ID: "relay", Addr: "relay.invalid:443", Protocol: "relay-v1-tls", Priority: 2}}}
	raw, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var result wire.EndpointSnapshot
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Version != snapshot.Version || len(result.Endpoints) != 1 || result.Endpoints[0] != snapshot.Endpoints[0] {
		t.Fatal("typed endpoint snapshot changed")
	}
}
