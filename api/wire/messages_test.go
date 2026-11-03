package wire_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/endless-net/relay/api/wire"
)

func TestPublishedMessageFixtures(t *testing.T) {
	raw, err := os.ReadFile("../fixtures/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Instance string
		Valid          bool
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		kind, _, _ := strings.Cut(c.Name, ".")
		if !strings.Contains(" ClientHello ClientFrame ServerFrame Ready Error Heartbeat ", " "+kind+" ") {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			fixture, err := os.ReadFile("../" + c.Instance)
			if err != nil {
				t.Fatal(err)
			}
			message, err := wire.DecodeMessage(fixture)
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%v: %v", c.Valid, err)
			}
			if c.Valid {
				encoded, err := wire.MarshalMessage(message)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := wire.NewDecoder(bytes.NewReader(encoded)).Read(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPublishedWireByteVectors(t *testing.T) {
	raw, err := os.ReadFile("../wire-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		Bytes []struct {
			Name string `json:"name"`
			Wire string `json:"wire_hex"`
			OK   bool   `json:"valid"`
		} `json:"byte_vectors"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}
	for _, c := range source.Bytes {
		t.Run(c.Name, func(t *testing.T) {
			line, err := hex.DecodeString(c.Wire)
			if err != nil {
				t.Fatal(err)
			}
			_, err = wire.NewDecoder(bytes.NewReader(line)).Read()
			if (err == nil) != c.OK {
				t.Fatalf("valid=%v: %v", c.OK, err)
			}
		})
	}
	if len(source.Bytes) < 16 {
		t.Fatal("published byte vectors not exercised")
	}
}

func TestStrictFieldNamesDuplicatesAndMissingZeroValues(t *testing.T) {
	for name, raw := range map[string]string{
		"duplicate":           `{"type":"ready","protocol_version":1,"ready":true,"ready":true}`,
		"escaped duplicate":   `{"type":"ready","protocol_version":1,"ready":true,"\u0072eady":true}`,
		"case alias":          `{"type":"ready","protocol_version":1,"ready":true,"READY":false}`,
		"missing empty error": `{"type":"error","protocol_version":1}`,
		"null":                `{"type":"error","protocol_version":1,"error":null}`,
		"wrong version":       `{"type":"ready","protocol_version":2,"ready":true}`,
		"future kind":         `{"type":"unknown","protocol_version":1}`,
		"array":               `[]`,
		"two values":          `{"type":"ready","protocol_version":1,"ready":true}{}`,
		"base64 unused bits":  `{"type":"client_frame","protocol_version":1,"peer_id":"p","peer_network_id":"n","payload":"Zh=="}`,
		"base64 embedded LF":  `{"type":"client_frame","protocol_version":1,"peer_id":"p","peer_network_id":"n","payload":"Zg==\n"}`,
		"invalid calendar":    `{"type":"heartbeat","protocol_version":1,"time":"2030-02-31T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := wire.DecodeMessage([]byte(raw)); !errors.Is(err, wire.ErrMessage) {
				t.Fatal("invalid message accepted")
			}
		})
	}
	fixture, err := os.ReadFile("../fixtures/ClientHello.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, insertion := range []string{`"node_id":"other",`, `"NODE_ID":"other",`, `"unknown":1,`} {
		modified := bytes.Replace(fixture, []byte(`"credential": {`), []byte(`"credential": {`+insertion), 1)
		if bytes.Equal(modified, fixture) {
			var compact bytes.Buffer
			if err := json.Compact(&compact, fixture); err != nil {
				t.Fatal(err)
			}
			modified = bytes.Replace(compact.Bytes(), []byte(`"credential":{`), []byte(`"credential":{`+insertion), 1)
		}
		if _, err := wire.DecodeMessage(modified); !errors.Is(err, wire.ErrMessage) {
			t.Fatal("invalid nested credential accepted")
		}
	}
}

func TestDecoderBoundariesAndBufferedMessages(t *testing.T) {
	ready := &wire.Ready{Type: "ready", ProtocolVersion: 1, Ready: true}
	line, err := wire.MarshalMessage(ready)
	if err != nil {
		t.Fatal(err)
	}
	d := wire.NewDecoder(bytes.NewReader(append(bytes.Clone(line), line...)))
	for range 2 {
		if _, err := d.Read(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Read(); !errors.Is(err, io.EOF) {
		t.Fatal("buffered messages lost")
	}
	for _, raw := range [][]byte{line[:len(line)-1], append(bytes.Repeat([]byte{' '}, wire.MaxLineBytes), line...), []byte("invalid\n")} {
		d = wire.NewDecoder(bytes.NewReader(raw))
		for range 2 {
			if _, err := d.Read(); !errors.Is(err, wire.ErrMessage) {
				t.Fatal("framing error did not poison decoder")
			}
		}
	}
	for _, message := range []wire.Message{nil, (*wire.Ready)(nil), &wire.Ready{Type: "ready", ProtocolVersion: 1}} {
		if _, err := wire.MarshalMessage(message); !errors.Is(err, wire.ErrMessage) {
			t.Fatal("invalid typed message accepted")
		}
	}
	for _, size := range []int{wire.MaxFramePayloadBytes, wire.MaxFramePayloadBytes + 1, wire.MaxFramePayloadBytes + 2} {
		_, err := wire.MarshalMessage(&wire.ClientFrame{Type: "client_frame", ProtocolVersion: 1, PeerID: "p", PeerNetworkID: "n", Payload: make([]byte, size)})
		if (err == nil) != (size == wire.MaxFramePayloadBytes) {
			t.Fatal("decoded payload bound changed")
		}
	}
}

func TestCredentialSigningBytes(t *testing.T) {
	key := "ed25519:" + strings.Repeat("A", 43)
	expires := time.Date(2030, 1, 1, 3, 1, 0, 123456789, time.FixedZone("fixture", 3*3600))
	payload, err := wire.CredentialSigningPayload(key, "network", "node", expires)
	want := `{"schema":3,"key_id":"` + key + `","network_id":"network","node_id":"node","expires_at":"2030-01-01T00:01:00.123456789Z"}`
	if err != nil || string(payload) != want {
		t.Fatal("canonical credential payload changed")
	}
	for _, test := range []struct {
		key, network, node string
		expires            time.Time
	}{{"wrong", "network", "node", expires}, {key, " network", "node", expires}, {key, "network", "", expires}, {key, "network", "node", time.Time{}}} {
		if _, err := wire.CredentialSigningPayload(test.key, test.network, test.node, test.expires); !errors.Is(err, wire.ErrMessage) {
			t.Fatal("invalid signing input accepted")
		}
	}
}

func TestLineLengthIncludesDelimiter(t *testing.T) {
	prefix := `{"type":"error","protocol_version":1,"error":"`
	suffix := "\"}\n"
	padding := wire.MaxLineBytes - len(prefix) - len(suffix)
	line := prefix + strings.Repeat("x", padding) + suffix
	if _, err := wire.NewDecoder(strings.NewReader(line)).Read(); err != nil {
		t.Fatal("exact maximum line rejected", err)
	}
	if _, err := wire.NewDecoder(strings.NewReader(prefix + strings.Repeat("x", padding+1) + suffix)).Read(); !errors.Is(err, wire.ErrMessage) {
		t.Fatal("oversized line accepted")
	}
	message := &wire.Error{Type: wire.MessageError, ProtocolVersion: wire.Version, Error: strings.Repeat("x", padding)}
	encoded, err := wire.MarshalMessage(message)
	if err != nil || len(encoded) != wire.MaxLineBytes {
		t.Fatal("exact maximum typed line rejected", err)
	}
	message.Error += "x"
	if _, err := wire.MarshalMessage(message); !errors.Is(err, wire.ErrMessage) {
		t.Fatal("oversized typed line accepted")
	}
}
