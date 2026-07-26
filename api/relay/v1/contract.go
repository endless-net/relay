package relayv1

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	protocolv1 "github.com/unng-lab/endlessnet-relay/protocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const MeshProtocolVersion int32 = 1

func RejectUnknownUnaryServerInterceptor(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if message, ok := request.(proto.Message); ok {
		if err := RejectUnknownFields(message); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	return handler(ctx, request)
}

func RejectUnknownStreamServerInterceptor(server any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return handler(server, &rejectUnknownServerStream{ServerStream: stream})
}

type rejectUnknownServerStream struct {
	grpc.ServerStream
}

func (s *rejectUnknownServerStream) RecvMsg(target any) error {
	if err := s.ServerStream.RecvMsg(target); err != nil {
		return err
	}
	message, ok := target.(proto.Message)
	if !ok {
		return status.Error(codes.InvalidArgument, "protobuf message is required")
	}
	if err := RejectUnknownFields(message); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

func CredentialFromProtocol(credential protocolv1.Credential) *Credential {
	return &Credential{
		Algorithm:         credential.Algorithm,
		KeyId:             credential.KeyID,
		NetworkId:         credential.NetworkID,
		NodeId:            credential.NodeID,
		ExpiresAtUnixNano: credential.ExpiresAt.UTC().UnixNano(),
		Signature:         credential.Signature,
	}
}

func CredentialToProtocol(credential *Credential) (protocolv1.Credential, error) {
	if credential == nil || !canonicalRequired(credential.GetAlgorithm()) || !canonicalRequired(credential.GetKeyId()) || !canonicalRequired(credential.GetNetworkId()) || !canonicalRequired(credential.GetNodeId()) || !canonicalRequired(credential.GetSignature()) || credential.GetExpiresAtUnixNano() <= 0 {
		return protocolv1.Credential{}, errors.New("invalid relay credential")
	}
	return protocolv1.Credential{
		Algorithm: credential.GetAlgorithm(),
		KeyID:     credential.GetKeyId(),
		NetworkID: credential.GetNetworkId(),
		NodeID:    credential.GetNodeId(),
		ExpiresAt: time.Unix(0, credential.GetExpiresAtUnixNano()).UTC(),
		Signature: credential.GetSignature(),
	}, nil
}

func TrustBundleFromProtocol(bundle protocolv1.SigningTrustBundle) *SigningTrustBundle {
	converted := &SigningTrustBundle{Version: int32(bundle.Version), ActiveKeyId: bundle.ActiveKeyID, Keys: make([]*SigningTrustKey, 0, len(bundle.Keys))}
	for _, key := range bundle.Keys {
		convertedKey := &SigningTrustKey{KeyId: key.KeyID, Algorithm: key.Algorithm, PublicKey: key.PublicKey}
		if key.NotBefore != nil {
			convertedKey.NotBeforeUnixNano = key.NotBefore.UTC().UnixNano()
		}
		if key.NotAfter != nil {
			convertedKey.NotAfterUnixNano = key.NotAfter.UTC().UnixNano()
		}
		converted.Keys = append(converted.Keys, convertedKey)
	}
	return converted
}

func TrustBundleToProtocol(bundle *SigningTrustBundle) (protocolv1.SigningTrustBundle, error) {
	if bundle == nil {
		return protocolv1.SigningTrustBundle{}, errors.New("relay trust bundle is required")
	}
	converted := protocolv1.SigningTrustBundle{Version: int(bundle.GetVersion()), ActiveKeyID: bundle.GetActiveKeyId(), Keys: make([]protocolv1.SigningTrustKey, 0, len(bundle.GetKeys()))}
	for _, key := range bundle.GetKeys() {
		if key == nil {
			return protocolv1.SigningTrustBundle{}, errors.New("relay trust bundle contains an empty key")
		}
		convertedKey := protocolv1.SigningTrustKey{KeyID: key.GetKeyId(), Algorithm: key.GetAlgorithm(), PublicKey: key.GetPublicKey()}
		if key.GetNotBeforeUnixNano() != 0 {
			value := time.Unix(0, key.GetNotBeforeUnixNano()).UTC()
			convertedKey.NotBefore = &value
		}
		if key.GetNotAfterUnixNano() != 0 {
			value := time.Unix(0, key.GetNotAfterUnixNano()).UTC()
			convertedKey.NotAfter = &value
		}
		converted.Keys = append(converted.Keys, convertedKey)
	}
	if err := converted.Validate(); err != nil {
		return protocolv1.SigningTrustBundle{}, err
	}
	return converted, nil
}

func RejectUnknownFields(message proto.Message) error {
	if message == nil {
		return errors.New("protobuf message is required")
	}
	return rejectUnknownMessage(message.ProtoReflect(), string(message.ProtoReflect().Descriptor().FullName()))
}

func rejectUnknownMessage(message protoreflect.Message, path string) error {
	if len(message.GetUnknown()) != 0 {
		return fmt.Errorf("%s contains unknown protobuf fields", path)
	}
	var result error
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.IsMap() && field.MapValue().Kind() == protoreflect.MessageKind {
			value.Map().Range(func(key protoreflect.MapKey, item protoreflect.Value) bool {
				result = rejectUnknownMessage(item.Message(), fmt.Sprintf("%s.%s[%v]", path, field.Name(), key.Interface()))
				return result == nil
			})
		} else if field.IsList() && field.Kind() == protoreflect.MessageKind {
			list := value.List()
			for index := 0; index < list.Len() && result == nil; index++ {
				result = rejectUnknownMessage(list.Get(index).Message(), fmt.Sprintf("%s.%s[%d]", path, field.Name(), index))
			}
		} else if field.Kind() == protoreflect.MessageKind {
			result = rejectUnknownMessage(value.Message(), path+"."+string(field.Name()))
		}
		return result == nil
	})
	return result
}

func canonicalRequired(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}
