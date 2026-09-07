package relaycoordinator

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	"github.com/endless-net/relay/internal/store"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type allowAuthorizer struct {
	bundle    protocolv1.SigningTrustBundle
	bundleErr error
}

func (allowAuthorizer) AuthorizeCredential(context.Context, protocolv1.Credential) error {
	return nil
}

func (allowAuthorizer) AuthorizePeer(context.Context, protocolv1.Credential, string) error {
	return nil
}

func (a allowAuthorizer) RelayTrustBundle(context.Context) (protocolv1.SigningTrustBundle, error) {
	return a.bundle, a.bundleErr
}

func TestInstanceResponsesRequireValidTrustBundle(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := protocolv1.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"register", "heartbeat"} {
		for _, tc := range []struct {
			name       string
			authorizer allowAuthorizer
			code       codes.Code
		}{
			{"valid", allowAuthorizer{bundle: bundle}, codes.OK},
			{"invalid", allowAuthorizer{}, codes.Unavailable},
			{"upstream failure", allowAuthorizer{bundle: bundle, bundleErr: errors.New("private upstream detail")}, codes.Unavailable},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				ctx := relayContext(t, "relay-a")
				storage := store.NewMemory()
				if _, _, err := storage.RegisterInstance(ctx, store.Instance{RelayID: "relay-a", BootID: "boot-a", MeshAddr: "relay-a:9444"}, time.Minute); err != nil {
					t.Fatal(err)
				}
				server := &Server{Store: storage, Authorizer: tc.authorizer}
				var received *relayv1.SigningTrustBundle
				var callErr error
				if method == "register" {
					response, err := server.RegisterInstance(ctx, &relayv1.RegisterInstanceRequest{RelayId: "relay-a", BootId: "boot-a", MeshAddr: "relay-a:9444"})
					received, callErr = response.GetRelayTrustBundle(), err
				} else {
					response, err := server.HeartbeatInstance(ctx, &relayv1.HeartbeatInstanceRequest{RelayId: "relay-a", BootId: "boot-a"})
					received, callErr = response.GetRelayTrustBundle(), err
				}
				if status.Code(callErr) != tc.code {
					t.Fatalf("response error = %v, want %v", callErr, tc.code)
				}
				if tc.code != codes.OK {
					if received != nil || status.Convert(callErr).Message() != "relay trust bundle unavailable" {
						t.Fatalf("unexpected failure response: %v, %v", received, callErr)
					}
					return
				}
				converted, err := relayv1.TrustBundleToProtocol(received)
				if err != nil || len(converted.Keys) != len(bundle.Keys) || converted.Keys[0] != bundle.Keys[0] {
					t.Fatalf("trust bundle was not preserved: %v, %v", converted, err)
				}
			})
		}
	}
}

func TestSessionMethodsRejectInvalidIdentityBeforeStoreAccess(t *testing.T) {
	for _, method := range []string{"renew", "release"} {
		for _, tc := range []struct {
			name, boot, network, node string
			epoch                     int64
		}{
			{"missing boot", "", "network", "node", 1},
			{"padded boot", " boot", "network", "node", 1},
			{"missing network", "boot", "", "node", 1},
			{"padded network", "boot", "network ", "node", 1},
			{"missing node", "boot", "network", "", 1},
			{"padded node", "boot", "network", " node", 1},
			{"zero epoch", "boot", "network", "node", 0},
			{"negative epoch", "boot", "network", "node", -1},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				server := &Server{Store: store.NewMemory(), Authorizer: allowAuthorizer{}}
				ctx := relayContext(t, "relay-a")
				var err error
				if method == "renew" {
					_, err = server.RenewSession(ctx, &relayv1.RenewSessionRequest{RelayId: "relay-a", BootId: tc.boot, NetworkId: tc.network, NodeId: tc.node, Epoch: tc.epoch})
				} else {
					_, err = server.ReleaseSession(ctx, &relayv1.ReleaseSessionRequest{RelayId: "relay-a", BootId: tc.boot, NetworkId: tc.network, NodeId: tc.node, Epoch: tc.epoch})
				}
				if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != "session identity and epoch are required" {
					t.Fatalf("unexpected validation error: %v", err)
				}
			})
		}
	}
}

func TestSessionReconnectFencesOldRelay(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := protocolv1.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Store: store.NewMemory(), Authorizer: allowAuthorizer{bundle: bundle}}
	for _, relayID := range []string{"relay-a", "relay-b"} {
		ctx := relayContext(t, relayID)
		if _, err := server.RegisterInstance(ctx, &relayv1.RegisterInstanceRequest{RelayId: relayID, BootId: "boot-" + relayID, MeshAddr: relayID + ":9444"}); err != nil {
			t.Fatal(err)
		}
	}
	credential, err := protocolv1.Sign(privateKey, "network", "node", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	protoCredential := relayv1.CredentialFromProtocol(*credential)
	first, err := server.AcquireSession(relayContext(t, "relay-a"), &relayv1.AcquireSessionRequest{RelayId: "relay-a", BootId: "boot-relay-a", Credential: protoCredential})
	if err != nil {
		t.Fatal(err)
	}
	second, err := server.AcquireSession(relayContext(t, "relay-b"), &relayv1.AcquireSessionRequest{RelayId: "relay-b", BootId: "boot-relay-b", Credential: protoCredential})
	if err != nil {
		t.Fatal(err)
	}
	if second.Epoch <= first.Epoch {
		t.Fatalf("new epoch = %d, old epoch = %d", second.Epoch, first.Epoch)
	}
	if _, err := server.RenewSession(relayContext(t, "relay-a"), &relayv1.RenewSessionRequest{RelayId: "relay-a", BootId: "boot-relay-a", NetworkId: "network", NodeId: "node", Epoch: first.Epoch, Credential: protoCredential}); err == nil {
		t.Fatal("old relay renewed a fenced session")
	}
}

func TestEndpointHandlerRequiresExactCoordinatorIdentityAndRejectsTokenOnly(t *testing.T) {
	storage := store.NewMemory()
	if err := storage.ReplaceEndpoints(context.Background(), protocolv1.EndpointSnapshot{Version: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	handler := (&Server{Store: storage}).EndpointHandler()

	tokenOnly := httptest.NewRequest(http.MethodGet, "/internal/relay-control/v1/endpoints", nil)
	tokenOnly.Header.Set("X-EndlessNet-Service-Token", "retired-token")
	tokenOnlyResponse := httptest.NewRecorder()
	handler.ServeHTTP(tokenOnlyResponse, tokenOnly)
	if tokenOnlyResponse.Code != http.StatusForbidden {
		t.Fatalf("token-only endpoint status = %d", tokenOnlyResponse.Code)
	}

	wrongPeer := endpointRequest(t, "spiffe://endlessnet.ru/relay/relay-a")
	wrongPeerResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongPeerResponse, wrongPeer)
	if wrongPeerResponse.Code != http.StatusForbidden {
		t.Fatalf("wrong SPIFFE peer endpoint status = %d", wrongPeerResponse.Code)
	}

	coordinator := endpointRequest(t, "spiffe://endlessnet.ru/service/coordinator")
	coordinatorResponse := httptest.NewRecorder()
	handler.ServeHTTP(coordinatorResponse, coordinator)
	if coordinatorResponse.Code != http.StatusOK {
		t.Fatalf("coordinator endpoint status = %d", coordinatorResponse.Code)
	}
}

func endpointRequest(t *testing.T, identity string) *http.Request {
	t.Helper()
	uri, err := url.Parse(identity)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/internal/relay-control/v1/endpoints", nil)
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{uri}}}}
	return request
}

func relayContext(t *testing.T, relayID string) context.Context {
	t.Helper()
	identity, err := url.Parse("spiffe://endlessnet.ru/relay/" + relayID)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{URIs: []*url.URL{identity}}
	info := credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}}}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: info})
}

func TestControlRejectsRelayIdentityOutsideSharedPolicy(t *testing.T) {
	if _, err := relayIDFromPeer(relayContext(t, "nested/relay")); err == nil {
		t.Fatal("control accepted identity rejected by mesh policy")
	}
}
