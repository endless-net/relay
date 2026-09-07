//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestProductUpstreamContract(t *testing.T) {
	address, err := suite.port(context.Background(), "upstream", 9447)
	if err != nil {
		t.Fatal(err)
	}
	dial := func(identity string) *grpc.ClientConn {
		t.Helper()
		connection, err := grpc.NewClient("passthrough:///"+address, grpc.WithTransportCredentials(credentials.NewTLS(suite.internalClientTLS(suite.certificates[identity], "upstream"))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { connection.Close() })
		return connection
	}
	connection := dial("relay-coordinator")
	client := relayv1.NewRelayUpstreamServiceClient(connection)
	signed, err := protocolv1.Sign(suite.signingKey, testNetworkID, "node-a", time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := protocolv1.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(suite.signingKey.Public().(ed25519.PublicKey)))
	if err != nil {
		t.Fatal(err)
	}
	cfg := upstreamConfig(bundle)
	setUpstream(t, map[string]any{"config": cfg})
	t.Cleanup(func() { setUpstream(t, map[string]any{"config": cfg}) })

	t.Run("typed_authorization_and_trust", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := client.AuthorizeCredential(ctx, &relayv1.AuthorizeCredentialRequest{Credential: relayv1.CredentialFromProtocol(*signed)}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.AuthorizePeerPair(ctx, &relayv1.AuthorizePeerPairRequest{Credential: relayv1.CredentialFromProtocol(*signed), PeerId: "node-b"}); err != nil {
			t.Fatal(err)
		}
		response, err := client.GetTrustBundle(ctx, &relayv1.GetTrustBundleRequest{})
		if err != nil {
			t.Fatal(err)
		}
		actual, err := relayv1.TrustBundleToProtocol(response.GetRelayTrustBundle())
		if err != nil || actual.ActiveKeyID != bundle.ActiveKeyID {
			t.Fatal("typed trust differs")
		}
	})
	t.Run("exact_caller_identity", func(t *testing.T) {
		for _, identity := range []string{"relay-a", "e2e-client"} {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, err := relayv1.NewRelayUpstreamServiceClient(dial(identity)).GetTrustBundle(ctx, &relayv1.GetTrustBundleRequest{})
			cancel()
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("wrong caller %s: %v", identity, err)
			}
		}
	})
	t.Run("strict_requests_and_service_version", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		credential := relayv1.CredentialFromProtocol(*signed)
		raw := protowire.AppendTag(nil, 99, protowire.VarintType)
		credential.ProtoReflect().SetUnknown(protowire.AppendVarint(raw, 1))
		_, err := client.AuthorizeCredential(ctx, &relayv1.AuthorizeCredentialRequest{Credential: credential})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatal("unknown nested field accepted")
		}
		_, err = client.AuthorizeCredential(ctx, &relayv1.AuthorizeCredentialRequest{})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatal("missing credential accepted")
		}
		oversized := relayv1.CredentialFromProtocol(*signed)
		oversized.Signature = string(bytes.Repeat([]byte("x"), 2<<20))
		_, err = client.AuthorizeCredential(ctx, &relayv1.AuthorizeCredentialRequest{Credential: oversized})
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatal("oversized upstream request was not rejected")
		}
		err = connection.Invoke(ctx, "/endlessnet.relay.v99.RelayUpstreamService/GetTrustBundle", &relayv1.GetTrustBundleRequest{}, &relayv1.GetTrustBundleResponse{})
		if status.Code(err) != codes.Unimplemented {
			t.Fatal("unsupported service version accepted")
		}
	})
	t.Run("inactive_destination_denied", func(t *testing.T) {
		inactive := upstreamConfig(bundle)
		inactive["nodes"] = []string{"node-a", "node-c", "node-d"}
		setUpstream(t, map[string]any{"config": inactive})
		defer setUpstream(t, map[string]any{"config": cfg})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := client.AuthorizePeerPair(ctx, &relayv1.AuthorizePeerPairRequest{Credential: relayv1.CredentialFromProtocol(*signed), PeerId: "node-b"})
		if status.Code(err) != codes.PermissionDenied {
			t.Fatal("inactive destination authorized despite remaining pair")
		}
	})
	t.Run("deadline_and_recovery", func(t *testing.T) {
		setUpstream(t, map[string]any{"mode": "hang"})
		defer setUpstream(t, map[string]any{})
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := client.GetTrustBundle(ctx, &relayv1.GetTrustBundleRequest{})
		cancel()
		if status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("deadline: %v", err)
		}
		setUpstream(t, map[string]any{})
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := client.GetTrustBundle(ctx, &relayv1.GetTrustBundleRequest{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("json_routes_absent", func(t *testing.T) {
		httpClient := suite.mutualHTTPClient(suite.certificates["relay-coordinator"], "upstream")
		for _, test := range []struct{ method, path string }{{http.MethodPost, "/internal/coordinator/relay-control/v1/authorize"}, {http.MethodGet, "/internal/coordinator/relay-control/v1/trust-bundle"}} {
			request, err := http.NewRequest(test.method, "https://"+address+test.path, bytes.NewBufferString("{}"))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			response, err := httpClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("removed JSON route returned %d", response.StatusCode)
			}
		}
	})
}
