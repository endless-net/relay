package mesh

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/endless-net/relay/internal/budget"
	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"

	relayv1 "github.com/endless-net/relay/internal/grpcwire"
	"github.com/endless-net/relay/internal/relay"
	"github.com/endless-net/relay/internal/tlsconfig"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

type DeliverFunc func(networkID, fromNodeID, destinationNetworkID, toNodeID string, destinationEpoch int64, authorizedUntil time.Time, guard func() bool, payload []byte) error

type Manager struct {
	events [8]atomic.Uint64
	relayv1.UnimplementedRelayMeshServer

	IdentityPolicy tlsconfig.IdentityPolicy
	RelayID        string
	BootID         string
	TLSConfig      *tls.Config
	Deliver        DeliverFunc
	QueueSize      int
	QueueBudget    *budget.Bytes

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.RWMutex
	peers  map[string]*peerConnection
}

type meshItem struct {
	message *relayv1.MeshMessage
	route   relay.PeerRoute
	bytes   int64
}
type peerConnection struct {
	established     atomic.Bool
	handshakeBudget time.Duration
	queueMu         sync.Mutex
	closed          atomic.Bool
	incoming        atomic.Bool
	bytes           atomic.Int64
	ctx             context.Context
	instance        *relayv1.RelayInstance
	send            chan meshItem
	cancel          context.CancelFunc
	ready           atomic.Bool
}

func NewManager(ctx context.Context, relayID, bootID string, tlsConfig *tls.Config, deliver DeliverFunc) *Manager {
	managerCtx, cancel := context.WithCancel(ctx)
	return &Manager{RelayID: relayID, BootID: bootID, TLSConfig: tlsConfig, Deliver: deliver, QueueSize: 256, QueueBudget: &budget.Bytes{Limit: 64 << 20}, ctx: managerCtx, cancel: cancel, peers: map[string]*peerConnection{}}
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
	if len(peers) > 64 {
		m.cancel()
		return
	}
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
		if ok && wanted.GetBootId() == existing.instance.GetBootId() && wanted.GetMeshAddr() == existing.instance.GetMeshAddr() && !existing.closed.Load() {
			delete(desired, relayID)
			continue
		}
		existing.cancel()
		delete(m.peers, relayID)
	}
	for relayID, instance := range desired {
		peerCtx, cancel := context.WithCancel(m.ctx)
		connection := &peerConnection{ctx: peerCtx, instance: instance, send: make(chan meshItem, m.queueSize()), cancel: cancel}
		m.peers[relayID] = connection
		go m.runPeer(peerCtx, connection)
	}
}

func (m *Manager) Forward(_ context.Context, route relay.PeerRoute, networkID, fromNodeID, destinationNetworkID, toNodeID string, payload []byte) (result error) {
	defer func() { m.forwardFailure(result) }()
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
	connection.queueMu.Lock()
	defer connection.queueMu.Unlock()
	if connection.closed.Load() || connection.ctx.Err() != nil {
		return relay.ErrDestinationFenced
	}
	cost := int64(len(payload)*2 + 512)
	if connection.bytes.Add(cost) > 4<<20 {
		connection.bytes.Add(-cost)
		return errors.New("mesh_queue_bytes")
	}
	if !m.QueueBudget.Acquire(cost) {
		connection.bytes.Add(-cost)
		return errors.New("mesh_global_bytes")
	}
	accepted := false
	defer func() {
		if !accepted {
			connection.bytes.Add(-cost)
			m.QueueBudget.Release(cost)
		}
	}()
	until := route.AuthorizationDeadline()
	if until.IsZero() || !time.Now().Before(until) {
		return errExpiredGrant
	}
	message := &relayv1.MeshMessage{ProtocolVersion: relayv1.MeshProtocolVersion, Body: &relayv1.MeshMessage_Frame{Frame: &relayv1.MeshFrame{RelayId: m.RelayID, BootId: m.BootID, NetworkId: networkID, DestinationNetworkId: destinationNetworkID, FromNodeId: fromNodeID, ToNodeId: toNodeID, DestinationEpoch: route.Epoch, AuthorizationExpiresUnixNano: until.UnixNano(), Payload: append([]byte(nil), payload...)}}}
	if err := validateMeshMessage(message); err != nil {
		return err
	}
	select {
	case connection.send <- meshItem{message, route, cost}:
		accepted = true
		return nil
	default:
		return errors.New("relay mesh outbound queue is full")
	}
}

func (m *Manager) Connect(stream relayv1.RelayMesh_ConnectServer) (result error) {
	defer func() {
		if result != nil && m.ctx.Err() == nil {
			m.events[6].Add(1)
		}
	}()
	handshakeCtx, handshakeCancel := context.WithTimeout(stream.Context(), 5*time.Second)
	first, err := receiveCurrentPeer(handshakeCtx, stream)
	handshakeCancel()
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
	if !incoming.incoming.CompareAndSwap(false, true) {
		return errors.New("duplicate_mesh_stream")
	}
	defer incoming.incoming.Store(false)
	if err := sendBounded(stream.Context(), func() {}, func() error { return stream.Send(meshHello(m.RelayID, m.BootID)) }); err != nil {
		return err
	}
	for {
		idleCtx, idleCancel := context.WithTimeout(incoming.ctx, 30*time.Second)
		message, err := receiveCurrentPeer(idleCtx, stream)
		idleCancel()
		if err != nil {
			return err
		}
		if err := validateMeshMessage(message); err != nil {
			return err
		}
		if identityErr := requireRelayIdentityWithPolicy(stream.Context(), hello.GetRelayId(), m.IdentityPolicy); identityErr != nil {
			return identityErr
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
			until, err := receiveGrantDeadline(frame.GetAuthorizationExpiresUnixNano(), time.Now())
			if errors.Is(err, errExpiredGrant) {
				m.events[4].Add(1)
				continue
			}
			if err != nil {
				return err
			}
			verified, _ := peer.FromContext(stream.Context())
			info := verified.AuthInfo.(credentials.TLSInfo)
			peerUntil := info.State.PeerCertificates[0].NotAfter.Add(-time.Second)
			if peerUntil.Before(until) {
				until = peerUntil
			}
			guard := func() bool { return incoming.ctx.Err() == nil && stream.Context().Err() == nil }
			_ = m.Deliver(frame.GetNetworkId(), frame.GetFromNodeId(), frame.GetDestinationNetworkId(), frame.GetToNodeId(), frame.GetDestinationEpoch(), until, guard, frame.GetPayload())
		case *relayv1.MeshMessage_Ping:
			if err := sendBounded(stream.Context(), func() {}, func() error {
				return stream.Send(&relayv1.MeshMessage{ProtocolVersion: relayv1.MeshProtocolVersion, Body: &relayv1.MeshMessage_Pong{Pong: &relayv1.MeshPong{}}})
			}); err != nil {
				return err
			}
		default:
			return errors.New("unsupported relay mesh message")
		}
	}
}

func (m *Manager) runPeer(ctx context.Context, connection *peerConnection) {
	defer func() {
		connection.queueMu.Lock()
		defer connection.queueMu.Unlock()
		connection.closed.Store(true)
		for {
			select {
			case item := <-connection.send:
				connection.bytes.Add(-item.bytes)
				m.QueueBudget.Release(item.bytes)
			default:
				return
			}
		}
	}()
	round := time.Now()
	attempts := 0
	for ctx.Err() == nil {
		remaining := 5*time.Second - time.Since(round)
		if attempts >= 3 || remaining <= 0 {
			return
		}
		connection.handshakeBudget = remaining
		connection.established.Store(false)
		if err := m.connectPeer(ctx, connection); err != nil && ctx.Err() == nil {
			m.events[5].Add(1)
		}
		if ctx.Err() != nil {
			return
		}
		if connection.established.Load() {
			round = time.Now()
			attempts = 0
		} else {
			attempts++
		}
		delay := 100 * time.Millisecond
		if attempts >= 2 {
			delay = 250 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
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
	var peerExpiry atomic.Pointer[time.Time]
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
		expiry := state.PeerCertificates[0].NotAfter.Add(-time.Second)
		peerExpiry.Store(&expiry)
		return nil
	}
	grpcConnection, err := grpc.NewClient(connection.instance.GetMeshAddr(), grpc.WithTransportCredentials(tlsconfig.GRPCCredentials(tlsConfig)))
	if err != nil {
		return err
	}
	defer func() { _ = grpcConnection.Close() }()
	writer := newClientWriter(ctx, func() { _ = grpcConnection.Close() })
	defer writer.close()
	handshakeBudget := connection.handshakeBudget
	if handshakeBudget <= 0 {
		handshakeBudget = 5 * time.Second
	}
	handshakeTimer := time.AfterFunc(handshakeBudget, func() { _ = grpcConnection.Close() })
	defer handshakeTimer.Stop()
	stream, err := relayv1.NewRelayMeshClient(grpcConnection).Connect(ctx)
	if err != nil {
		return err
	}
	if err := writer.send(time.Time{}, func() error { return stream.Send(meshHello(m.RelayID, m.BootID)) }); err != nil {
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
	handshakeTimer.Stop()
	connection.established.Store(true)
	connection.ready.Store(true)
	activity := make(chan struct{}, 1)
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
			select {
			case activity <- struct{}{}:
			default:
			}
			if message.GetPong() == nil {
				receiveErr <- errors.New("invalid relay mesh response")
				return
			}
		}
	}()
	idle := time.NewTimer(30 * time.Second)
	defer idle.Stop()
	ping := time.NewTicker(10 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-receiveErr:
			return err
		case <-idle.C:
			return errors.New("mesh_idle_timeout")
		case <-activity:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(30 * time.Second)
		case item := <-connection.send:
			if expiry := peerExpiry.Load(); expiry != nil && !time.Now().Before(*expiry) {
				connection.bytes.Add(-item.bytes)
				m.QueueBudget.Release(item.bytes)
				return errors.New("mesh_peer_expired")
			}
			until := item.route.AuthorizationDeadline()
			valid := !until.IsZero() && time.Now().Before(until)
			if item.route.Valid != nil {
				valid = valid && item.route.Valid()
			}
			if !valid {
				connection.bytes.Add(-item.bytes)
				m.QueueBudget.Release(item.bytes)
				continue
			}
			if captured := time.Unix(0, item.message.GetFrame().GetAuthorizationExpiresUnixNano()); captured.Before(until) {
				until = captured
			}
			item.message.GetFrame().AuthorizationExpiresUnixNano = until.UnixNano()
			err := writer.send(until, func() error { return stream.Send(item.message) })
			connection.bytes.Add(-item.bytes)
			m.QueueBudget.Release(item.bytes)
			if err != nil {
				return err
			}
		case <-ping.C:
			if expiry := peerExpiry.Load(); expiry != nil && !time.Now().Before(*expiry) {
				return errors.New("mesh_peer_expired")
			}
			if err := writer.send(time.Time{}, func() error {
				return stream.Send(&relayv1.MeshMessage{ProtocolVersion: relayv1.MeshProtocolVersion, Body: &relayv1.MeshMessage_Ping{Ping: &relayv1.MeshPing{}}})
			}); err != nil {
				return err
			}
		}
	}
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
		if !canonicalRequired(frame.GetRelayId()) || !canonicalRequired(frame.GetBootId()) || !canonicalRequired(frame.GetNetworkId()) || !canonicalRequired(frame.GetDestinationNetworkId()) || !canonicalRequired(frame.GetFromNodeId()) || !canonicalRequired(frame.GetToNodeId()) || frame.GetDestinationEpoch() <= 0 || frame.GetAuthorizationExpiresUnixNano() <= 0 || len(frame.GetPayload()) == 0 || len(frame.GetPayload()) > relay.MaxFramePayloadBytes {
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

func sendBounded(ctx context.Context, abort func(), send func() error) (resultErr error) {
	ctx, span := otel.Tracer("relay/mesh").Start(ctx, "relay.mesh.send")
	defer func() {
		if resultErr != nil {
			span.SetStatus(otelcodes.Error, "mesh_send_failed")
		}
		span.End()
	}()
	result := make(chan error, 1)
	go func() { result <- send() }()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case e := <-result:
		return e
	case <-ctx.Done():
		abort()
		return ctx.Err()
	case <-timer.C:
		abort()
		return errors.New("mesh_write_deadline")
	}
}
