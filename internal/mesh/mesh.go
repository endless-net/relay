package mesh

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	"github.com/endless-net/relay/internal/relay"
	"github.com/endless-net/relay/internal/tlsconfig"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

type DeliverFunc func(networkID, fromNodeID, destinationNetworkID, toNodeID string, destinationEpoch int64, payload []byte) error

type Manager struct {
	relayv1.UnimplementedRelayMeshServer

	IdentityPolicy tlsconfig.IdentityPolicy
	RelayID        string
	BootID         string
	TLSConfig      *tls.Config
	Deliver        DeliverFunc
	QueueSize      int

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.RWMutex
	peers  map[string]*peerConnection
}

type peerConnection struct {
	ctx      context.Context
	instance *relayv1.RelayInstance
	send     chan *relayv1.MeshMessage
	cancel   context.CancelFunc
	ready    atomic.Bool
}

func NewManager(ctx context.Context, relayID, bootID string, tlsConfig *tls.Config, deliver DeliverFunc) *Manager {
	managerCtx, cancel := context.WithCancel(ctx)
	return &Manager{RelayID: relayID, BootID: bootID, TLSConfig: tlsConfig, Deliver: deliver, QueueSize: 256, ctx: managerCtx, cancel: cancel, peers: map[string]*peerConnection{}}
}

func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, connection := range m.peers {
		connection.cancel()
	}
	m.peers = map[string]*peerConnection{}
}

func (m *Manager) UpdatePeers(peers []*relayv1.RelayInstance) {
	desired := make(map[string]*relayv1.RelayInstance, len(peers))
	for _, instance := range peers {
		if instance == nil || instance.GetRelayId() == "" || instance.GetRelayId() == m.RelayID || instance.GetMeshAddr() == "" {
			continue
		}
		copy := &relayv1.RelayInstance{
			RelayId:  instance.GetRelayId(),
			BootId:   instance.GetBootId(),
			MeshAddr: instance.GetMeshAddr(),
		}
		desired[copy.RelayId] = copy
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for relayID, existing := range m.peers {
		wanted, ok := desired[relayID]
		if ok && wanted.GetBootId() == existing.instance.GetBootId() && wanted.GetMeshAddr() == existing.instance.GetMeshAddr() {
			delete(desired, relayID)
			continue
		}
		existing.cancel()
		delete(m.peers, relayID)
	}
	for relayID, instance := range desired {
		peerCtx, cancel := context.WithCancel(m.ctx)
		connection := &peerConnection{ctx: peerCtx, instance: instance, send: make(chan *relayv1.MeshMessage, m.queueSize()), cancel: cancel}
		m.peers[relayID] = connection
		go m.runPeer(peerCtx, connection)
	}
}

func (m *Manager) Forward(_ context.Context, route relay.PeerRoute, networkID, fromNodeID, destinationNetworkID, toNodeID string, payload []byte) error {
	if route.RelayID == "" || route.RelayID == m.RelayID {
		return errors.New("relay mesh route is not remote")
	}
	m.mu.RLock()
	connection := m.peers[route.RelayID]
	m.mu.RUnlock()
	if connection == nil || connection.instance.GetBootId() != route.BootID {
		return relay.ErrDestinationFenced
	}
	if !connection.ready.Load() {
		return errors.New("relay mesh peer is unavailable")
	}
	message := &relayv1.MeshMessage{ProtocolVersion: relayv1.MeshProtocolVersion, Body: &relayv1.MeshMessage_Frame{Frame: &relayv1.MeshFrame{RelayId: m.RelayID, BootId: m.BootID, NetworkId: networkID, DestinationNetworkId: destinationNetworkID, FromNodeId: fromNodeID, ToNodeId: toNodeID, DestinationEpoch: route.Epoch, Payload: append([]byte(nil), payload...)}}}
	if err := validateMeshMessage(message); err != nil {
		return err
	}
	select {
	case connection.send <- message:
		return nil
	default:
		return errors.New("relay mesh outbound queue is full")
	}
}

func (m *Manager) Connect(stream relayv1.RelayMesh_ConnectServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := validateMeshMessage(first); err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil || hello.GetRelayId() == "" || hello.GetBootId() == "" {
		return errors.New("relay mesh hello is required")
	}
	if err := requireRelayIdentityWithPolicy(stream.Context(), hello.GetRelayId(), m.IdentityPolicy); err != nil {
		return err
	}
	m.mu.RLock()
	incoming := m.peers[hello.GetRelayId()]
	m.mu.RUnlock()
	if incoming == nil || incoming.instance.GetBootId() != hello.GetBootId() {
		return relay.ErrDestinationFenced
	}
	if err := stream.Send(meshHello(m.RelayID, m.BootID)); err != nil {
		return err
	}
	for {
		message, err := receiveCurrentPeer(incoming.ctx, stream)
		if err != nil {
			return err
		}
		if err := validateMeshMessage(message); err != nil {
			return err
		}
		if incoming.ctx.Err() != nil {
			return relay.ErrDestinationFenced
		}
		switch body := message.GetBody().(type) {
		case *relayv1.MeshMessage_Frame:
			frame := body.Frame
			if frame.GetRelayId() != hello.GetRelayId() || frame.GetBootId() != hello.GetBootId() || m.Deliver == nil {
				return errors.New("invalid relay mesh frame source")
			}
			_ = m.Deliver(frame.GetNetworkId(), frame.GetFromNodeId(), frame.GetDestinationNetworkId(), frame.GetToNodeId(), frame.GetDestinationEpoch(), frame.GetPayload())
		case *relayv1.MeshMessage_Ping:
			if err := stream.Send(&relayv1.MeshMessage{ProtocolVersion: relayv1.MeshProtocolVersion, Body: &relayv1.MeshMessage_Pong{Pong: &relayv1.MeshPong{}}}); err != nil {
				return err
			}
		default:
			return errors.New("unsupported relay mesh message")
		}
	}
}

func (m *Manager) runPeer(ctx context.Context, connection *peerConnection) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := m.connectPeer(ctx, connection)
		if ctx.Err() != nil {
			return
		}
		_ = err
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func (m *Manager) connectPeer(ctx context.Context, connection *peerConnection) error {
	connection.ready.Store(false)
	defer connection.ready.Store(false)
	if m.TLSConfig == nil {
		return errors.New("relay mesh mTLS configuration is required")
	}
	tlsConfig := m.TLSConfig.Clone()
	previousVerify := tlsConfig.VerifyConnection
	expectedID, err := m.IdentityPolicy.RelayIdentity(connection.instance.GetRelayId())
	if err != nil {
		return err
	}
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if previousVerify != nil {
			if err := previousVerify(state); err != nil {
				return err
			}
		}
		id, err := tlsconfig.PeerID(state)
		if err != nil || id != expectedID {
			return errors.New("relay mesh server certificate identity mismatch")
		}
		return nil
	}
	grpcConnection, err := grpc.NewClient(connection.instance.GetMeshAddr(), grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return err
	}
	defer grpcConnection.Close()
	stream, err := relayv1.NewRelayMeshClient(grpcConnection).Connect(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(meshHello(m.RelayID, m.BootID)); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if err := validateMeshMessage(first); err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil || hello.GetRelayId() != connection.instance.GetRelayId() || hello.GetBootId() != connection.instance.GetBootId() {
		return relay.ErrDestinationFenced
	}
	connection.ready.Store(true)
	receiveErr := make(chan error, 1)
	go func() {
		for {
			message, err := stream.Recv()
			if err != nil {
				receiveErr <- err
				return
			}
			if err := validateMeshMessage(message); err != nil {
				receiveErr <- err
				return
			}
			if message.GetPong() == nil {
				receiveErr <- errors.New("invalid relay mesh response")
				return
			}
		}
	}()
	ping := time.NewTicker(5 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-receiveErr:
			return err
		case message := <-connection.send:
			if err := stream.Send(message); err != nil {
				return err
			}
		case <-ping.C:
			if err := stream.Send(&relayv1.MeshMessage{ProtocolVersion: relayv1.MeshProtocolVersion, Body: &relayv1.MeshMessage_Ping{Ping: &relayv1.MeshPing{}}}); err != nil {
				return err
			}
		}
	}
}

func requireRelayIdentity(ctx context.Context, relayID string) error {
	return requireRelayIdentityWithPolicy(ctx, relayID, tlsconfig.IdentityPolicy{})
}
func requireRelayIdentityWithPolicy(ctx context.Context, relayID string, policy tlsconfig.IdentityPolicy) error {
	peerInfo, ok := peer.FromContext(ctx)
	if !ok || peerInfo.AuthInfo == nil {
		return errors.New("verified relay mesh certificate is required")
	}
	tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return errors.New("verified relay mesh certificate is required")
	}
	id, err := tlsconfig.PeerID(tlsInfo.State)
	expected, expectedErr := policy.RelayIdentity(relayID)
	if err != nil || expectedErr != nil || id != expected {
		return errors.New("relay mesh certificate identity mismatch")
	}

	return nil
}

func (m *Manager) queueSize() int {
	if m.QueueSize > 0 {
		return m.QueueSize
	}
	return 256
}

func meshHello(relayID, bootID string) *relayv1.MeshMessage {
	return &relayv1.MeshMessage{ProtocolVersion: relayv1.MeshProtocolVersion, Body: &relayv1.MeshMessage_Hello{Hello: &relayv1.MeshHello{RelayId: relayID, BootId: bootID}}}
}

func validateMeshMessage(message *relayv1.MeshMessage) error {
	if message == nil {
		return errors.New("relay mesh message is required")
	}
	if err := relayv1.RejectUnknownFields(message); err != nil {
		return err
	}
	if message.GetProtocolVersion() != relayv1.MeshProtocolVersion || message.GetBody() == nil {
		return errors.New("unsupported relay mesh protocol")
	}
	if frame := message.GetFrame(); frame != nil {
		if !canonicalRequired(frame.GetRelayId()) || !canonicalRequired(frame.GetBootId()) || !canonicalRequired(frame.GetNetworkId()) || !canonicalRequired(frame.GetDestinationNetworkId()) || !canonicalRequired(frame.GetFromNodeId()) || !canonicalRequired(frame.GetToNodeId()) || frame.GetDestinationEpoch() <= 0 || len(frame.GetPayload()) == 0 || len(frame.GetPayload()) > relay.MaxFramePayloadBytes {
			return errors.New("invalid relay mesh frame")
		}
	}
	if hello := message.GetHello(); hello != nil && (!canonicalRequired(hello.GetRelayId()) || !canonicalRequired(hello.GetBootId())) {
		return errors.New("invalid relay mesh hello")
	}
	return nil
}

func canonicalRequired(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

func receiveCurrentPeer(ctx context.Context, stream relayv1.RelayMesh_ConnectServer) (*relayv1.MeshMessage, error) {
	type received struct {
		message *relayv1.MeshMessage
		err     error
	}
	result := make(chan received, 1)
	go func() { message, err := stream.Recv(); result <- received{message, err} }()
	select {
	case <-ctx.Done():
		return nil, relay.ErrDestinationFenced
	case <-stream.Context().Done():
		return nil, stream.Context().Err()
	case value := <-result:
		return value.message, value.err
	}
}
