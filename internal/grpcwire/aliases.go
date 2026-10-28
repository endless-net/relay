package grpcwire

import (
	up "github.com/endless-net/coordinator/api/relay/v1"
	pb "github.com/endless-net/relay/api/relay/v1"
)

type Credential = pb.Credential
type SigningTrustKey = pb.SigningTrustKey
type SigningTrustBundle = pb.SigningTrustBundle
type RelayInstance = pb.RelayInstance
type RegisterInstanceRequest = pb.RegisterInstanceRequest
type RegisterInstanceResponse = pb.RegisterInstanceResponse
type HeartbeatInstanceRequest = pb.HeartbeatInstanceRequest
type HeartbeatInstanceResponse = pb.HeartbeatInstanceResponse
type AcquireSessionRequest = pb.AcquireSessionRequest
type AcquireSessionResponse = pb.AcquireSessionResponse
type RenewSessionRequest = pb.RenewSessionRequest
type RenewSessionResponse = pb.RenewSessionResponse
type ReleaseSessionRequest = pb.ReleaseSessionRequest
type ReleaseSessionResponse = pb.ReleaseSessionResponse
type AuthorizePeerRequest = pb.AuthorizePeerRequest
type AuthorizePeerResponse = pb.AuthorizePeerResponse
type MeshMessage = pb.MeshMessage
type MeshMessage_Hello = pb.MeshMessage_Hello
type MeshMessage_Frame = pb.MeshMessage_Frame
type MeshMessage_Ping = pb.MeshMessage_Ping
type MeshMessage_Pong = pb.MeshMessage_Pong
type MeshHello = pb.MeshHello
type MeshFrame = pb.MeshFrame
type MeshPing = pb.MeshPing
type MeshPong = pb.MeshPong
type RelayControlClient = pb.RelayControlClient
type RelayControlServer = pb.RelayControlServer
type UnimplementedRelayControlServer = pb.UnimplementedRelayControlServer
type UnsafeRelayControlServer = pb.UnsafeRelayControlServer
type RelayMeshClient = pb.RelayMeshClient
type RelayMeshServer = pb.RelayMeshServer
type UnimplementedRelayMeshServer = pb.UnimplementedRelayMeshServer
type UnsafeRelayMeshServer = pb.UnsafeRelayMeshServer

var NewRelayControlClient = pb.NewRelayControlClient
var RegisterRelayControlServer = pb.RegisterRelayControlServer
var NewRelayMeshClient = pb.NewRelayMeshClient
var RegisterRelayMeshServer = pb.RegisterRelayMeshServer

type AuthorizeCredentialRequest = up.AuthorizeCredentialRequest
type AuthorizeCredentialResponse = up.AuthorizeCredentialResponse
type AuthorizePeerPairRequest = up.AuthorizePeerPairRequest
type AuthorizePeerPairResponse = up.AuthorizePeerPairResponse
type GetTrustBundleRequest = up.GetTrustBundleRequest
type GetTrustBundleResponse = up.GetTrustBundleResponse

type RelayMesh_ConnectServer = pb.RelayMesh_ConnectServer
type RelayMesh_ConnectClient = pb.RelayMesh_ConnectClient
