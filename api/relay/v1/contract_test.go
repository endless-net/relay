package relayv1

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	protocolv1 "github.com/unng-lab/endlessnet-relay/protocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestCredentialAndTrustBundleTypedRoundTrip(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := protocolv1.Sign(privateKey, "network", "node", time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	convertedCredential, err := CredentialToProtocol(CredentialFromProtocol(*credential))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(CredentialFromProtocol(*credential), CredentialFromProtocol(convertedCredential)) {
		t.Fatalf("credential round trip = %#v", convertedCredential)
	}
	bundle, err := protocolv1.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	convertedBundle, err := TrustBundleToProtocol(TrustBundleFromProtocol(bundle))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(TrustBundleFromProtocol(bundle), TrustBundleFromProtocol(convertedBundle)) {
		t.Fatalf("trust bundle round trip = %#v", convertedBundle)
	}
}

func TestRejectUnknownFieldsRecursesIntoNestedMessages(t *testing.T) {
	unknown := protowire.AppendTag(nil, 99, protowire.VarintType)
	unknown = protowire.AppendVarint(unknown, 1)
	for _, message := range []proto.Message{
		&AcquireSessionRequest{RelayId: "relay", BootId: "boot", Credential: &Credential{unknownFields: unknown}},
		&RegisterInstanceResponse{Peers: []*RelayInstance{{unknownFields: unknown}}},
		&MeshMessage{ProtocolVersion: MeshProtocolVersion, Body: &MeshMessage_Frame{Frame: &MeshFrame{unknownFields: unknown}}},
	} {
		if err := RejectUnknownFields(message); err == nil {
			t.Fatalf("nested unknown protobuf field was accepted in %T", message)
		}
	}
}

func TestUnknownFieldServerInterceptorsRejectUnaryAndStreamMessages(t *testing.T) {
	request := &RegisterInstanceRequest{}
	request.unknownFields = protowire.AppendTag(nil, 99, protowire.VarintType)
	request.unknownFields = protowire.AppendVarint(request.unknownFields, 1)
	called := false
	_, err := RejectUnknownUnaryServerInterceptor(context.Background(), request, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) {
		called = true
		return nil, nil
	})
	if status.Code(err) != codes.InvalidArgument || called {
		t.Fatalf("unary interceptor error = %v, called = %t", err, called)
	}

	stream := &unknownTestServerStream{unknown: request.unknownFields}
	wrapped := &rejectUnknownServerStream{ServerStream: stream}
	if err := wrapped.RecvMsg(&MeshMessage{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("stream interceptor error = %v", err)
	}
}

type unknownTestServerStream struct {
	unknown []byte
}

func (s *unknownTestServerStream) SetHeader(metadata.MD) error  { return nil }
func (s *unknownTestServerStream) SendHeader(metadata.MD) error { return nil }
func (s *unknownTestServerStream) SetTrailer(metadata.MD)       {}
func (s *unknownTestServerStream) Context() context.Context     { return context.Background() }
func (s *unknownTestServerStream) SendMsg(any) error            { return errors.New("not implemented") }
func (s *unknownTestServerStream) RecvMsg(target any) error {
	message, ok := target.(proto.Message)
	if !ok {
		return errors.New("protobuf message is required")
	}
	message.ProtoReflect().SetUnknown(append([]byte(nil), s.unknown...))
	return nil
}
