package authz

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// This in-memory transport tests the generated wire contract. Product E2E tests
// the production client and service over real SPIFFE mTLS and network sockets.
func testUpstream(t *testing.T, call func(context.Context, any, *grpc.UnaryServerInfo) (any, error)) relayv1.RelayUpstreamServiceClient {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(relayv1.RejectUnknownUnaryServerInterceptor,
		func(ctx context.Context, request any, info *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (any, error) {
			return call(ctx, request, info)
		}))
	relayv1.RegisterRelayUpstreamServiceServer(server, &relayv1.UnimplementedRelayUpstreamServiceServer{})
	go func() { _ = server.Serve(listener) }()
	connection, err := grpc.NewClient("passthrough:///unit-upstream",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1<<20), grpc.MaxCallSendMsgSize(1<<20)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close(); server.Stop(); listener.Close() })
	return relayv1.NewRelayUpstreamServiceClient(connection)
}

// Fixtures are independent textproto specifications with deliberately unusable
// signatures. Comparing decoded requests catches method, field and time-unit drift.
func TestPublishedUpstreamRequestFixtures(t *testing.T) {
	credential := protocolv1.Credential{Algorithm: "ed25519-relay-credential-v3", KeyID: "fixture-key", NetworkID: "fixture-network", NodeID: "fixture-source", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Signature: "non-credential-fixture"}
	for _, pair := range []bool{false, true} {
		name := "authorize-credential"
		var expected proto.Message = &relayv1.AuthorizeCredentialRequest{}
		method := "/endlessnet.relay.v1.RelayUpstreamService/AuthorizeCredential"
		var response proto.Message = &relayv1.AuthorizeCredentialResponse{}
		if pair {
			name = "authorize-peer-pair"
			expected = &relayv1.AuthorizePeerPairRequest{}
			method = "/endlessnet.relay.v1.RelayUpstreamService/AuthorizePeerPair"
			response = &relayv1.AuthorizePeerPairResponse{}
		}
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/" + name + ".textproto")
			if err != nil {
				t.Fatal(err)
			}
			if err = prototext.Unmarshal(raw, expected); err != nil {
				t.Fatal(err)
			}
			client := testUpstream(t, func(_ context.Context, request any, info *grpc.UnaryServerInfo) (any, error) {
				if info.FullMethod != method || !proto.Equal(expected, request.(proto.Message)) {
					t.Error("request differs from independent protobuf fixture")
				}
				return response, nil
			})
			a := GRPCAuthorizer{Client: client}
			if pair {
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

func TestGRPCAuthorizationStatusAndCacheSemantics(t *testing.T) {
	for _, code := range []codes.Code{codes.OK, codes.PermissionDenied, codes.Unauthenticated, codes.Unavailable, codes.DeadlineExceeded, codes.Unimplemented, codes.InvalidArgument, codes.DataLoss} {
		t.Run(code.String(), func(t *testing.T) {
			var calls atomic.Int32
			client := testUpstream(t, func(context.Context, any, *grpc.UnaryServerInfo) (any, error) {
				if calls.Add(1) == 1 || code == codes.OK {
					return &relayv1.AuthorizeCredentialResponse{}, nil
				}
				return nil, status.Error(code, "fixture status")
			})
			now := time.Now()
			cache := NewCache(GRPCAuthorizer{Client: client})
			cache.Now = func() time.Time { return now }
			credential := protocolv1.Credential{ExpiresAt: now.Add(time.Hour)}
			if err := cache.AuthorizeCredential(context.Background(), credential); err != nil {
				t.Fatal(err)
			}
			now = now.Add(6 * time.Second)
			err := cache.AuthorizeCredential(context.Background(), credential)
			denied := code == codes.PermissionDenied || code == codes.Unauthenticated
			if denied {
				if !errors.Is(err, ErrDenied) {
					t.Fatal("explicit gRPC denial used stale allow")
				}
			} else if err != nil {
				t.Fatalf("bounded stale result: %v", err)
			}
			now = now.Add(31 * time.Second)
			err = cache.AuthorizeCredential(context.Background(), credential)
			if code != codes.OK && err == nil {
				t.Fatal("upstream error authorized beyond stale deadline")
			}
		})
	}
}

func setUnknown(message proto.Message) {
	raw := protowire.AppendTag(nil, 99, protowire.VarintType)
	message.ProtoReflect().SetUnknown(protowire.AppendVarint(raw, 1))
}

func TestGRPCRejectsUnknownAuthorizationResponseFields(t *testing.T) {
	for _, pair := range []bool{false, true} {
		t.Run(map[bool]string{false: "credential", true: "pair"}[pair], func(t *testing.T) {
			var response proto.Message = &relayv1.AuthorizeCredentialResponse{}
			if pair {
				response = &relayv1.AuthorizePeerPairResponse{}
			}
			setUnknown(response)
			client := testUpstream(t, func(context.Context, any, *grpc.UnaryServerInfo) (any, error) { return response, nil })
			a := GRPCAuthorizer{Client: client}
			var err error
			if pair {
				err = a.AuthorizePeer(context.Background(), protocolv1.Credential{}, "peer")
			} else {
				err = a.AuthorizeCredential(context.Background(), protocolv1.Credential{})
			}
			if err == nil {
				t.Fatal("unknown response fields granted authorization")
			}
		})
	}
}

func TestGRPCTrustBundleValidation(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := protocolv1.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(public))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "missing", "version", "unknown_response", "unknown_key", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			response := &relayv1.GetTrustBundleResponse{RelayTrustBundle: relayv1.TrustBundleFromProtocol(bundle)}
			switch mode {
			case "missing":
				response.RelayTrustBundle = nil
			case "version":
				response.RelayTrustBundle.Version = 99
			case "unknown_response":
				setUnknown(response)
			case "unknown_key":
				setUnknown(response.RelayTrustBundle.Keys[0])
			case "oversized":
				response.RelayTrustBundle.Keys[0].PublicKey = string(make([]byte, 2<<20))
			}
			client := testUpstream(t, func(_ context.Context, request any, info *grpc.UnaryServerInfo) (any, error) {
				if info.FullMethod != "/endlessnet.relay.v1.RelayUpstreamService/GetTrustBundle" {
					t.Error("unexpected trust method")
				}
				return response, nil
			})
			_, err := (GRPCAuthorizer{Client: client}).RelayTrustBundle(context.Background())
			if mode == "oversized" && status.Code(err) != codes.ResourceExhausted {
				t.Fatalf("response size limit was not enforced by gRPC: %v", err)
			}
			if (err == nil) != (mode == "valid") {
				t.Fatalf("trust validation outcome: %v", err)
			}
		})
	}
}

func TestGRPCRejectsUnknownNestedRequestBeforeHandler(t *testing.T) {
	var calls atomic.Int32
	client := testUpstream(t, func(context.Context, any, *grpc.UnaryServerInfo) (any, error) {
		calls.Add(1)
		return &relayv1.AuthorizeCredentialResponse{}, nil
	})
	credential := &relayv1.Credential{}
	setUnknown(credential)
	_, err := client.AuthorizeCredential(context.Background(), &relayv1.AuthorizeCredentialRequest{Credential: credential})
	if status.Code(err) != codes.InvalidArgument || calls.Load() != 0 {
		t.Fatal("unknown nested request reached handler")
	}
}

func TestGRPCPropagatesCallerDeadline(t *testing.T) {
	client := testUpstream(t, func(ctx context.Context, _ any, _ *grpc.UnaryServerInfo) (any, error) {
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := (GRPCAuthorizer{Client: client}).AuthorizeCredential(ctx, protocolv1.Credential{})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("deadline result: %v", err)
	}
}
