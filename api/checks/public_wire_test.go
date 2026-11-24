package checks

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaPath = "schemas/public-wire.schema.json"
const schemaID = "urn:endlessnet:relay:public-wire:v1"
const definitions = "#/$defs/"

type closedLoader struct{}

func (closedLoader) Load(uri string) (any, error) {
	return nil, fmt.Errorf("schema resource is not registered locally: %s", uri)
}

type fixture struct {
	Name  string
	Valid *bool
}

type byteVector struct {
	fixture
	Definition string
	Wire       string  `json:"wire_hex"`
	PayloadHex *string `json:"opaque_payload_hex"`
	PayloadSHA *string `json:"opaque_payload_sha256"`
}

func TestPublicWire(t *testing.T) {
	root := os.DirFS("..")
	data := readFixture(t, root, schemaPath)
	document, err := strictJSON(data, false)
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		ID    string `json:"$id"`
		Draft string `json:"$schema"`
	}
	if err := json.Unmarshal(data, &identity); err != nil ||
		identity.ID != schemaID || identity.Draft != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("unexpected schema identity: %+v (%v)", identity, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(closedLoader{})
	compiler.AssertFormat()
	compiler.RegisterFormat(&jsonschema.Format{Name: "date-time", Validate: func(value any) error {
		if timestamp, ok := value.(string); ok {
			_, err := parseTimestamp(timestamp)
			return err
		}
		return nil
	}})
	if err := compiler.AddResource(schemaID, document); err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.Compile(schemaID); err != nil {
		t.Fatal(err)
	}

	var cases []struct {
		fixture
		Schema   string
		Instance string
	}
	readManifest(t, root, "fixtures/cases.json", &cases)
	var vectors struct {
		Bytes     []byteVector `json:"byte_vectors"`
		Semantics []struct {
			fixture
			Definition string
			Value      json.RawMessage
			At         string
		} `json:"semantic_vectors"`
	}
	readManifest(t, root, "wire-vectors.json", &vectors)
	if len(cases) == 0 || len(vectors.Bytes) == 0 || len(vectors.Semantics) == 0 {
		t.Fatal("missing schema, byte or semantic cases")
	}
	t.Logf("%d schema fixtures, %d byte vectors, %d semantic vectors", len(cases), len(vectors.Bytes), len(vectors.Semantics))

	for _, c := range cases {
		t.Run("schema/"+c.Name, func(t *testing.T) {
			definition, ok := strings.CutPrefix(c.Schema, schemaPath+definitions)
			if !ok {
				t.Fatalf("schema is outside the local registry: %s", c.Schema)
			}
			schema := compileDefinition(t, compiler, definition)
			value, err := strictJSON(readFixture(t, root, c.Instance), false)
			if err != nil {
				t.Fatal(err)
			}
			checkOutcome(t, c.fixture, schema.Validate(value))
		})
	}
	for _, v := range vectors.Bytes {
		t.Run("bytes/"+v.Name, func(t *testing.T) {
			schema := compileDefinition(t, compiler, v.Definition)
			raw, err := hex.DecodeString(v.Wire)
			if err != nil || v.Wire == "" {
				t.Fatalf("invalid hex fixture: %v", err)
			}
			checkOutcome(t, v.fixture, v.validate(schema, raw))
		})
	}
	for _, v := range vectors.Semantics {
		t.Run("semantic/"+v.Name, func(t *testing.T) {
			switch v.Definition {
			case "SigningTrustBundle", "Credential", "EndpointSnapshot":
			default:
				t.Fatalf("unknown semantic fixture kind: %s", v.Definition)
			}
			schema := compileDefinition(t, compiler, v.Definition)
			value, err := strictJSON(v.Value, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(value); err != nil {
				checkOutcome(t, v.fixture, err)
				return
			}
			checkOutcome(t, v.fixture, validateSemantics(v.Definition, v.Value, v.At))
		})
	}
}

func compileDefinition(t *testing.T, compiler *jsonschema.Compiler, definition string) *jsonschema.Schema {
	t.Helper()
	if definition == "" || strings.ContainsAny(definition, "/#~%") {
		t.Fatalf("invalid fixture definition: %q", definition)
	}
	schema, err := compiler.Compile(schemaID + definitions + definition)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func readFixture(t *testing.T, root fs.FS, path string) []byte {
	t.Helper()
	data, err := fs.ReadFile(root, filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readManifest(t *testing.T, root fs.FS, path string, value any) {
	t.Helper()
	data := readFixture(t, root, path)
	if _, err := strictJSON(data, false); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}

func checkOutcome(t *testing.T, fixture fixture, err error) {
	t.Helper()
	if fixture.Name == "" || fixture.Valid == nil {
		t.Fatal("fixture requires a name and expected outcome")
	}
	if (err == nil) != *fixture.Valid {
		t.Fatalf("expected valid=%t, got %v", *fixture.Valid, err)
	}
}

func strictJSON(data []byte, integersOnly bool) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readValue(decoder, integersOnly)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after JSON value")
	}
	return value, nil
}

func readValue(decoder *json.Decoder, integersOnly bool) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := make(map[string]any)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name := key.(string)
			if _, exists := object[name]; exists {
				return nil, fmt.Errorf("duplicate JSON key: %s", name)
			}
			value, err := readValue(decoder, integersOnly)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		_, err = decoder.Token()
		return object, err
	case json.Delim('['):
		array := make([]any, 0)
		for decoder.More() {
			value, err := readValue(decoder, integersOnly)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		_, err = decoder.Token()
		return array, err
	}
	if number, ok := token.(json.Number); ok && integersOnly && strings.ContainsAny(string(number), ".eE") {
		return nil, errors.New("non-integer number in wire input")
	}
	return token, nil
}

func (v byteVector) validate(schema *jsonschema.Schema, raw []byte) error {
	if len(raw) > 147456 || bytes.Count(raw, []byte{'\n'}) != 1 || raw[len(raw)-1] != '\n' {
		return errors.New("invalid line framing or bound")
	}
	value, err := strictJSON(raw[:len(raw)-1], true)
	if err != nil {
		return err
	}
	if err := schema.Validate(value); err != nil {
		return err
	}
	if v.PayloadHex == nil && v.PayloadSHA == nil {
		return nil
	}
	frame, ok := value.(map[string]any)
	if !ok {
		return errors.New("expected a frame object")
	}
	encoded, ok := frame["payload"].(string)
	if !ok {
		return errors.New("missing opaque payload")
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(payload) < 1 || len(payload) > 65536 {
		return errors.New("invalid opaque payload or bound")
	}
	if v.PayloadHex != nil && hex.EncodeToString(payload) != *v.PayloadHex {
		return errors.New("opaque payload differs")
	}
	if v.PayloadSHA != nil && fmt.Sprintf("%x", sha256.Sum256(payload)) != *v.PayloadSHA {
		return errors.New("opaque payload digest differs")
	}
	return nil
}

func parseTimestamp(value string) (time.Time, error) {
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	if err == nil && timestamp.Year() == 0 {
		return time.Time{}, errors.New("invalid calendar year")
	}
	return timestamp, err
}

func validateSemantics(definition string, data []byte, at string) error {
	switch definition {
	case "SigningTrustBundle":
		var bundle struct {
			ActiveKeyID string `json:"active_key_id"`
			Keys        []struct {
				ID        string     `json:"key_id"`
				Public    string     `json:"public_key"`
				NotBefore *time.Time `json:"not_before"`
				NotAfter  *time.Time `json:"not_after"`
			}
		}
		if err := json.Unmarshal(data, &bundle); err != nil {
			return err
		}
		seen := make(map[string]bool)
		for _, key := range bundle.Keys {
			if seen[key.ID] {
				return errors.New("duplicate trust key")
			}
			seen[key.ID] = true
			public, err := base64.RawURLEncoding.DecodeString(key.Public)
			if err != nil || len(public) != 32 {
				return errors.New("invalid trust public key")
			}
			digest := sha256.Sum256(public)
			if key.ID != "ed25519:"+base64.RawURLEncoding.EncodeToString(digest[:]) {
				return errors.New("trust key identity mismatch")
			}
			if key.NotBefore != nil && key.NotAfter != nil && !key.NotAfter.After(*key.NotBefore) {
				return errors.New("invalid trust validity window")
			}
		}
		if !seen[bundle.ActiveKeyID] {
			return errors.New("missing active trust key")
		}
	case "Credential":
		var credential struct {
			Signature string
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(data, &credential); err != nil {
			return err
		}
		// Only the shape of the synthetic signature is checked.
		signature, err := base64.RawURLEncoding.DecodeString(credential.Signature)
		if err != nil || len(signature) != 64 {
			return errors.New("invalid signature bytes")
		}
		clock, err := parseTimestamp(at)
		if err != nil || !clock.Before(credential.ExpiresAt) {
			return errors.New("credential expiry reached or invalid fixture clock")
		}
	case "EndpointSnapshot":
		var snapshot struct{ Endpoints []struct{ ID string } }
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return err
		}
		seen := make(map[string]bool)
		for _, endpoint := range snapshot.Endpoints {
			if seen[endpoint.ID] {
				return errors.New("duplicate endpoint identity")
			}
			seen[endpoint.ID] = true
		}
	}
	return nil
}
