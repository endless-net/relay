//go:build product

package product

import (
	"bytes"
	"encoding/json"
	"net"
	"testing"
	"time"

	wire "github.com/endless-net/relay/internal/wire"
)

func TestRuntimeIPv6PublicTLSAndMesh(t *testing.T) {
	f := newFixtureWithPublicListener(t, "", "[::1]:0")
	a, _ := f.client(t, 0, 0)
	defer func() { _ = a.Close() }()
	b, reader := f.client(t, 1, 1)
	defer func() { _ = b.Close() }()
	for _, conn := range []net.Conn{a, b} {
		host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
		if err != nil || net.ParseIP(host) == nil || net.ParseIP(host).To4() != nil {
			t.Fatal("public connection did not use IPv6")
		}
	}
	payload := bytes.Repeat([]byte{0xa5}, 1200)
	if err := json.NewEncoder(a).Encode(wire.ClientFrame{Type: wire.MessageClientFrame, ProtocolVersion: 1, PeerNetworkID: "fixture", PeerID: "node-1", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	var frame wire.ServerFrame
	if err := reader.Decode(&frame); err != nil || !bytes.Equal(frame.Payload, payload) {
		t.Fatal("IPv6 public TLS mesh delivery failed", err)
	}
	t.Log("both public clients used IPv6 TLS; opaque payload crossed the authenticated mesh unchanged")
}
