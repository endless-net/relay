//go:build product

package product

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	wire "github.com/endless-net/relay/internal/wire"
)

func TestRuntimePublicBoundsAndStrictFailures(t *testing.T) {
	f := newFixture(t)
	a, _ := f.client(t, 0, 0)
	defer func() { _ = a.Close() }()
	b, reader := f.client(t, 1, 1)
	defer func() { _ = b.Close() }()
	for _, size := range []int{1, 65536} {
		payload := bytes.Repeat([]byte{0xff}, size)
		if err := json.NewEncoder(a).Encode(wire.ClientFrame{Type: wire.MessageClientFrame, ProtocolVersion: 1, PeerNetworkID: "fixture", PeerID: "node-1", Payload: payload}); err != nil {
			t.Fatal(err)
		}
		_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
		var frame wire.ServerFrame
		if err := reader.Decode(&frame); err != nil || !bytes.Equal(frame.Payload, payload) {
			t.Fatal("boundary payload changed or rejected", size, err)
		}
	}
	body := func(peer int, version int, payload, extra string) string {
		return fmt.Sprintf(`{"type":"client_frame","protocol_version":%d,"peer_network_id":"fixture","peer_id":"node-%d","payload":%s%s}`, version, peer, payload, extra)
	}
	cases := []struct {
		name  string
		build func(int) string
	}{
		{"duplicate-key", func(peer int) string { return body(peer, 1, `"YQ=="`, `,"type":"client_frame"`) }},
		{"unknown-field", func(peer int) string { return body(peer, 1, `"YQ=="`, `,"experimental":true`) }},
		{"unknown-kind", func(peer int) string {
			return strings.Replace(body(peer, 1, `"YQ=="`, ""), "client_frame", "future_kind", 1)
		}},
		{"unknown-version", func(peer int) string { return body(peer, 2, `"YQ=="`, "") }},
		{"noncanonical-base64", func(peer int) string { return body(peer, 1, `"YR=="`, "") }},
		{"invalid-utf8", func(peer int) string { return body(peer, 1, string([]byte{'"', 0xff, '"'}), "") }},
		{"payload-over-limit", func(peer int) string {
			return body(peer, 1, `"`+base64.StdEncoding.EncodeToString(make([]byte, 65537))+`"`, "")
		}},
		{"line-over-limit", func(peer int) string { return body(peer, 1, `"YQ=="`, `,"padding":"`+strings.Repeat("a", 147456)+`"`) }},
	}
	for index, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			source, sourceReader := f.client(t, 0, index*2+2)
			defer func() { _ = source.Close() }()
			destination, destinationReader := f.client(t, 1, index*2+3)
			defer func() { _ = destination.Close() }()
			if _, err := source.Write([]byte(item.build(index*2+3) + "\n")); err != nil {
				t.Fatal(err)
			}
			_ = source.SetReadDeadline(time.Now().Add(3 * time.Second))
			var failure wire.Error
			if err := sourceReader.Decode(&failure); err != nil || failure.Type != wire.MessageError {
				t.Fatal("malformed input did not produce bounded rejection", err)
			}
			if failure.Error != "invalid_frame" && failure.Error != "frame_too_large" {
				t.Fatal("unexpected public reason code", failure.Error)
			}
			var more json.RawMessage
			if err := sourceReader.Decode(&more); err == nil {
				t.Fatal("malformed session remained open")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("malformed session was not closed before deadline")
			}
			_ = destination.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			var delivered json.RawMessage
			if err := destinationReader.Decode(&delivered); err == nil {
				t.Fatal("malformed input reached destination")
			} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
				t.Fatal("healthy destination unexpectedly closed", err)
			}
		})
	}
	t.Log("real TLS+mesh passed payload1/65536; strict failures closed source with fixed code and no destination delivery")
}
