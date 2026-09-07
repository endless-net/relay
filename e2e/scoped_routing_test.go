//go:build e2e

package e2e

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"

	protocolv1 "github.com/endless-net/relay/protocol/v1"
)

func TestProductCrossNetworkRouting(t *testing.T) {
	bundle, err := protocolv1.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(suite.signingKey.Public().(ed25519.PublicKey)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { setUpstream(t, map[string]any{"config": upstreamConfig(bundle)}) })
	for _, destinationRelay := range []string{"relay-a", "relay-b"} {
		t.Run(destinationRelay, func(t *testing.T) {
			cfg := upstreamConfig(bundle)
			cfg["networks"] = map[string][]string{"sharing-source": {"same-node"}, "sharing-target": {"same-node"}, "sharing-other": {"same-node"}}
			cfg["scoped_pairs"] = []map[string]string{{"from_network": "sharing-source", "from": "same-node", "to_network": "sharing-target", "to": "same-node"}}
			setUpstream(t, map[string]any{"config": cfg})
			dial := func(relayID, networkID string) *relayClient {
				t.Helper()
				client, err := suite.dialScopedRelayClientReading(relayID, networkID, "same-node", true)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(client.close)
				return client
			}
			source := dial("relay-a", "sharing-source")
			target := dial(destinationRelay, "sharing-target")
			other := dial(destinationRelay, "sharing-other")
			waitForTransfer(t, source, target, 20*time.Second)
			assertTransfer(t, source, target, []byte("cross-network-allowed"))
			deny := func(from, to *relayClient, payload string) {
				t.Helper()
				if err := from.sendScoped(to.networkID, to.nodeID, []byte(payload)); err != nil {
					t.Fatal(err)
				}
				assertNoPayload(t, to, []byte(payload), time.Second)
			}
			deny(source, other, "network-substitution")
			deny(target, source, "reverse-pair")
			cfg["scoped_pairs"] = []map[string]string{}
			setUpstream(t, map[string]any{"config": cfg})
			// Wait beyond the documented positive-cache freshness before denial.
			time.Sleep(6 * time.Second)
			deny(source, target, "withdrawn-pair")
		})
	}
}
