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
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

type DeliverFunc func(networkID, fromNodeID, toNodeID string, destinationEpoch int64, payload []byte) error

type Manager struct {
	relayv1.UnimplementedRelayMeshServer

	RelayID   string
	BootID    string
	TLSConfig *tls.Config
	Deliver   DeliverFunc
	QueueSize int

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.RWMutex
	peers  map[string]*peerConnection
}

type peerConnection struct {
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
		connection := &peerConnection{instance: instance, send: make(chan *relayv1.MeshMessage, m.queueSize()), cancel: cancel}
		m.peers[relayID] = connection
		go m.runPeer(peerCtx, connection)
	}
}

func (m *Manager) Forward(_ context.Context, route relay.PeerRoute, networkID, fromNodeID, toNodeID string, payload []byte) error {
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
	message := &relayv1.MeshMessage{ProtocolVersion: relayv1.MeshProtocolVersion, Body: &relayv1.MeshMessage_Frame{Frame: &relayv1.MeshFrame{RelayId: m.RelayID, BootId: m.BootID, NetworkId: networkID, FromNodeId: fromNodeID, ToNodeId: toNodeID, DestinationEpoch: route.Epoch, Payload: append([]byte(nil), payload...)}}}
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
	if err := requireRelayIdentity(stream.Context(), hello.GetRelayId()); err != nil {
		return err
	}
	if err := stream.Send(meshHello(m.RelayID, m.BootID)); err != nil {
		return err
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := validateMeshMessage(message); err != nil {
			return err
		}
		switch body := message.GetBody().(type) {
		case *relayv1.MeshMessage_Frame:
			frame := body.Frame
			if frame.GetRelayId() != hello.GetRelayId() || frame.GetBootId() != hello.GetBootId() || m.Deliver == nil {
				return errors.New("invalid relay mesh frame source")
			}
			_ = m.Deliver(frame.GetNetworkId(), frame.GetFromNodeId(), frame.GetToNodeId(), frame.GetDestinationEpoch(), frame.GetPayload())
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
	expectedURI := "spiffe://endlessnet.ru/relay/" + connection.instance.GetRelayId()
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if previousVerify != nil {
			if err := previousVerify(state); err != nil {
				return err
			}
		}
		if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
			return errors.New("verified relay mesh certificate is required")
		}
		certificate := state.VerifiedChains[0][0]
		if len(certificate.URIs) != 1 || certificate.URIs[0] == nil || certificate.URIs[0].String() != expectedURI {
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
	peerInfo, ok := peer.FromContext(ctx)
	if !ok || peerInfo.AuthInfo == nil {
		return errors.New("verified relay mesh certificate is required")
	}
	tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return errors.New("verified relay mesh certificate is required")
	}
	certificate := tlsInfo.State.VerifiedChains[0][0]
	want := "spiffe://endlessnet.ru/relay/" + relayID
	if len(certificate.URIs) != 1 || certificate.URIs[0] == nil || certificate.URIs[0].String() != want {
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
		if !canonicalRequired(frame.GetRelayId()) || !canonicalRequired(frame.GetBootId()) || !canonicalRequired(frame.GetNetworkId()) || !canonicalRequired(frame.GetFromNodeId()) || !canonicalRequired(frame.GetToNodeId()) || frame.GetDestinationEpoch() <= 0 || len(frame.GetPayload()) == 0 || len(frame.GetPayload()) > relay.MaxFramePayloadBytes {
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
