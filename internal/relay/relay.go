package relay

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/endless-net/relay/internal/budget"
	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"

	protocolv1 "github.com/endless-net/relay/internal/wire"
)

const (
	MaxFramePayloadBytes       = protocolv1.MaxFramePayloadBytes
	DefaultOutboundQueueFrames = 32
	DefaultAuthTimeout         = 5 * time.Second
	MinHeartbeatInterval       = time.Duration(protocolv1.MinHeartbeatIntervalMS) * time.Millisecond
	MaxHeartbeatInterval       = time.Duration(protocolv1.MaxHeartbeatIntervalMS) * time.Millisecond
	maxLineBytes               = MaxFramePayloadBytes*2 + 16*1024
	outboundWriteTimeout       = 2 * time.Second
)

var errRelayOutboundQueueFull = errors.New("relay outbound queue is full")
var errRelayLineTooLarge = errors.New("relay line is too large")
var errStaleQueuedFrame = errors.New("stale_queued_frame")

type Server struct {
	workers             sync.WaitGroup
	listener            net.Listener
	listening           atomic.Bool
	fenced              atomic.Bool
	Addr                string
	TrustBundleProvider func() (protocolv1.SigningTrustBundle, error)
	TLSConfig           *tls.Config
	Metrics             *Metrics
	RelayID             string
	Control             ControlPlane
	Mesh                MeshForwarder
	// OutboundQueueFrames bounds per-peer frames waiting for socket writes.
	// Zero uses DefaultOutboundQueueFrames.
	OutboundQueueFrames int
	// BandwidthLimitBytesPerSecond bounds inbound payload bytes per session.
	// Zero disables bandwidth limiting.
	BandwidthLimitBytesPerSecond int
	// AuthTimeout bounds unauthenticated connections and control-plane calls.
	// Zero uses DefaultAuthTimeout.
	AuthTimeout time.Duration
	// Admission is shared by all relay listeners in one process so global and
	// per-source limits apply across plaintext and TLS endpoints together.
	Admission *AdmissionController

	mu          sync.Mutex
	sessions    map[string]map[string]*session
	admissionMu sync.Mutex
	QueueBudget *budget.Bytes
	payloadRate *budget.Rate
	frameRate   *budget.Rate
}

type ServerConfig struct {
	Addr                         string
	TrustBundleProvider          func() (protocolv1.SigningTrustBundle, error)
	TLSConfig                    *tls.Config
	Metrics                      *Metrics
	RelayID                      string
	Control                      ControlPlane
	Mesh                         MeshForwarder
	OutboundQueueFrames          int
	BandwidthLimitBytesPerSecond int
	AuthTimeout                  time.Duration
	Admission                    *AdmissionController
}

func NewServer(config ServerConfig) (*Server, error) {
	server := &Server{
		Addr:                         config.Addr,
		TrustBundleProvider:          config.TrustBundleProvider,
		TLSConfig:                    config.TLSConfig,
		Metrics:                      config.Metrics,
		RelayID:                      config.RelayID,
		Control:                      config.Control,
		Mesh:                         config.Mesh,
		OutboundQueueFrames:          config.OutboundQueueFrames,
		BandwidthLimitBytesPerSecond: config.BandwidthLimitBytesPerSecond,
		AuthTimeout:                  config.AuthTimeout,
		Admission:                    config.Admission,
	}
	if err := server.validateConfiguration(); err != nil {
		return nil, err
	}
	return server, nil
}

func (s *Server) Fence() {
	s.fenced.Store(true)
	s.metrics().setDraining(true)
	s.mu.Lock()
	listener := s.listener
	s.mu.Unlock()
	if listener != nil {
		_ = listener.Close()
	}
	s.closeSessions()
}
func (s *Server) Ready() bool { return s.listening.Load() && !s.fenced.Load() }

type session struct {
	controlDeadline           func(protocolv1.Credential) time.Time
	frameRate                 *budget.Rate
	queueBytes                atomic.Int64
	queueMu                   sync.Mutex
	pool                      *budget.Bytes
	validUntil                atomic.Pointer[time.Time]
	trust                     func() (protocolv1.SigningTrustBundle, error)
	networkID                 string
	nodeID                    string
	credential                protocolv1.Credential
	lease                     SessionLease
	conn                      net.Conn
	writer                    *bufio.Writer
	sendCh                    chan protocolv1.ServerFrame
	ctx                       context.Context
	cancel                    context.CancelFunc
	done                      chan struct{}
	metrics                   *Metrics
	heartbeatInterval         time.Duration
	bandwidthLimitBytesPerSec int
	bandwidthWindowStart      time.Time
	bandwidthWindowBytes      int
	writeMu                   sync.Mutex
	closeOnce                 sync.Once
	closed                    atomic.Bool
	revoked                   atomic.Bool
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := s.validateConfiguration(); err != nil {
		return err
	}
	if _, err := s.relaySigningTrustBundle(); err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.Addr)
	if err != nil {
		return err
	}
	tlsConfig := s.TLSConfig.Clone()
	listener = tls.NewListener(listener, tlsConfig)
	defer func() { _ = listener.Close() }()
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	s.listening.Store(true)
	defer s.listening.Store(false)
	if s.fenced.Load() {
		return errors.New("relay is fenced")
	}
	defer s.workers.Wait()
	defer s.closeSessions()
	go func() {
		<-ctx.Done()
		s.metrics().setDraining(true)
		_ = listener.Close()
		s.closeSessions()
	}()
	slog.Info("starting relay server", "addr", listener.Addr().String(), "tls", true)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		release, admitted := s.admissionController().tryAcquireConnection(conn.RemoteAddr())
		if !admitted {
			_ = conn.Close()
			continue
		}
		s.workers.Go(func() {
			defer release()
			s.handleConn(ctx, conn)
		})
	}
}

func (s *Server) validateConfiguration() error {
	if strings.TrimSpace(s.Addr) == "" || s.Addr != strings.TrimSpace(s.Addr) {
		return errors.New("relay addr is required")
	}
	if s.TLSConfig == nil || s.TLSConfig.MinVersion != tls.VersionTLS13 || (s.TLSConfig.MaxVersion != 0 && s.TLSConfig.MaxVersion != tls.VersionTLS13) {
		return errors.New("relay TLS 1.3 configuration is required")
	}
	if s.RelayID == "" || s.RelayID != strings.TrimSpace(s.RelayID) {
		return errors.New("relay id is required")
	}
	if s.Control == nil || s.Mesh == nil || s.TrustBundleProvider == nil {
		return errors.New("relay control, mesh, and trust bundle provider are required")
	}
	if s.AuthTimeout < 0 {
		return errors.New("relay auth timeout must not be negative")
	}
	return nil
}

func (s *Server) authorizePeer(ctx context.Context, credential protocolv1.Credential, sourceEpoch int64, peerNetworkID, peerID string) (PeerRoute, error) {
	authorizationCtx := ctx
	var cancel context.CancelFunc
	if timeout := s.authTimeout(); timeout > 0 {
		authorizationCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return s.Control.AuthorizePeer(authorizationCtx, credential, sourceEpoch, peerNetworkID, peerID)
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, maxLineBytes)
	writer := bufio.NewWriter(conn)
	authRelease, admitted := s.admissionController().tryAcquireAuth()
	if !admitted {
		// Load-shed without writing: on a TLS listener a response would perform
		// another attacker-controlled handshake and defeat admission backpressure.
		return
	}
	defer authRelease()
	authTimeout := s.authTimeout()
	if authTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(authTimeout))
	}
	authLine, err := readLine(reader)
	if authTimeout > 0 {
		_ = conn.SetReadDeadline(time.Time{})
	}
	if err != nil {
		if errors.Is(err, errRelayLineTooLarge) {
			s.metrics().recordAuthFailure()
			writeError(writer, "invalid_auth")
		} else if isNetTimeout(err) {
			s.metrics().recordAuthFailure()
			writeError(writer, "auth_timeout")
		}
		return
	}
	var auth protocolv1.ClientHello
	if err := decodeRelayLine(authLine, &auth); err != nil {
		s.metrics().recordAuthFailure()
		writeError(writer, "invalid_auth")
		return
	}
	if err := auth.Validate(); err != nil {
		s.metrics().recordAuthFailure()
		writeError(writer, "unsupported_protocol")
		return
	}
	trustBundle, err := s.relaySigningTrustBundle()
	if err != nil {
		s.metrics().recordAuthFailure()
		writeError(writer, "invalid_credential")
		return
	}
	trustedKey, err := trustBundle.Resolve(auth.Credential.KeyID, time.Now().UTC())
	if err != nil || protocolv1.Verify(auth.Credential, trustedKey.PublicKey, time.Now().UTC()) != nil {
		s.metrics().recordAuthFailure()
		writeError(writer, "invalid_credential")
		return
	}
	grantStarted := time.Now()
	controlCtx, cancelControl := context.WithTimeout(ctx, min(s.authTimeout(), 2*time.Second))
	lease, err := s.Control.AcquireSession(controlCtx, auth.Credential)
	cancelControl()
	if err != nil {
		s.metrics().recordAuthFailure()
		writeError(writer, "session_rejected")
		return
	}
	authRelease()
	sess := s.newSession(ctx, conn, writer, auth.Credential, heartbeatIntervalFromAuth(auth.HeartbeatIntervalMS))
	sess.lease = lease
	firstDeadline := grantStarted.Add(10 * time.Second)
	sess.validUntil.Store(&firstDeadline)
	_ = conn.SetWriteDeadline(time.Time{})
	s.addSession(sess)
	if s.fenced.Load() {
		sess.close()
	}
	s.metrics().recordSessionStart()
	sess.startWriter()
	defer func() {
		s.removeSession(sess)
		if sess.lease.Epoch > 0 {
			releaseCtx, cancel := context.WithTimeout(context.Background(), s.authTimeout())
			_ = s.Control.ReleaseSession(releaseCtx, sess.lease)
			cancel()
		}
		sess.close()
		<-sess.done
		s.metrics().recordSessionEnd()
	}()
	if err := sess.writeReady(); err != nil {
		return
	}
	sess.startHeartbeat()
	sess.startLeaseRenewal(s.Control, 5*time.Second, s.authTimeout())
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		if _, err := reader.Peek(1); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := readLine(reader)
		if err != nil {
			if errors.Is(err, errRelayLineTooLarge) {
				s.metrics().recordDrop("invalid")
				_ = sess.writeError("frame_too_large")
			}
			return
		}
		if !s.allowMessage() || (sess.frameRate != nil && !sess.frameRate.Allow(1)) {
			s.metrics().recordDrop("bandwidth")
			return
		}
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &kind); err != nil {
			return
		}
		if kind.Type == protocolv1.MessageHeartbeat {
			var heartbeat protocolv1.Heartbeat
			if e := decodeRelayLine(line, &heartbeat); e != nil {
				return
			}
			if heartbeat.ProtocolVersion != 1 {
				return
			}
			if _, e := time.Parse(time.RFC3339Nano, heartbeat.Time); e != nil {
				return
			}
			continue
		}
		var frame protocolv1.ClientFrame
		if err := decodeRelayLine(line, &frame); err != nil {
			s.metrics().recordDrop("invalid")
			_ = sess.writeError("invalid_frame")
			return
		}
		if err := frame.Validate(); err != nil {
			s.metrics().recordDrop("invalid")
			_ = sess.writeError("invalid_frame")
			return
		}
		peerID := frame.PeerID
		if frame.PeerNetworkID == sess.networkID && peerID == sess.nodeID {
			s.metrics().recordDrop("invalid")
			_ = sess.writeError("self_target")
			return
		}
		if !sess.usable() || !s.allowPayload(len(frame.Payload)) || !sess.allowInboundPayload(len(frame.Payload)) {
			s.metrics().recordDrop("bandwidth")
			_ = sess.writeError("bandwidth_limit")
			return
		}
		route, err := s.authorizePeer(sess.ctx, sess.credential, sess.lease.Epoch, frame.PeerNetworkID, peerID)
		if err != nil {
			s.metrics().recordDrop("acl")
			writeErr := sess.writeError("pair_denied")
			if writeErr != nil {
				sess.revoke()
				return
			}
			continue
		}
		s.metrics().recordInboundFrame(len(frame.Payload))
		originalDeadline := route.CurrentDeadline
		originalUntil := route.ValidUntil
		route.CurrentDeadline = func() time.Time {
			until := sess.authorizationDeadline()
			other := originalUntil
			if originalDeadline != nil {
				other = originalDeadline()
			}
			if until.IsZero() || (originalDeadline != nil && other.IsZero()) {
				return time.Time{}
			}
			if !other.IsZero() && other.Before(until) {
				until = other
			}
			return until
		}
		originalValid := route.Valid
		route.Valid = func() bool { return sess.usable() && (originalValid == nil || originalValid()) }
		if route.RelayID != s.RelayID {
			if s.Mesh.Forward(ctx, route, sess.networkID, sess.nodeID, frame.PeerNetworkID, peerID, frame.Payload) != nil {
				s.metrics().recordDrop("mesh")
				_ = sess.writeError("mesh_unavailable")
			}
			continue
		}
		peer := s.peerSession(frame.PeerNetworkID, peerID)
		if peer == nil {
			s.metrics().recordDrop("no_peer")
			_ = sess.writeError("peer_unavailable")
			continue
		}
		if peer.lease.Epoch != route.Epoch || peer.lease.BootID != route.BootID {
			s.metrics().recordDrop("fenced")
			_ = sess.writeError("peer_fenced")
			continue
		}
		out := newServerFrame(sess.networkID, sess.nodeID, frame.Payload)
		out.ValidUntil = route.AuthorizationDeadline()
		out.CurrentDeadline = route.AuthorizationDeadline
		out.Guard = func() bool { return sess.usable() && (route.Valid == nil || route.Valid()) }
		if err := peer.enqueueFrame(out); err != nil {
			s.metrics().recordDrop("slow_consumer")
			_ = sess.writeError("peer_queue_full")
			continue
		}
		s.metrics().recordOutboundFrame(len(frame.Payload))
	}
}

func decodeRelayLine(line []byte, target any) error { return protocolv1.DecodeStrict(line, target) }

func (s *Server) relaySigningTrustBundle() (protocolv1.SigningTrustBundle, error) {
	bundle, err := s.TrustBundleProvider()
	if err != nil {
		return protocolv1.SigningTrustBundle{}, err
	}
	if err := bundle.Validate(); err != nil {
		return protocolv1.SigningTrustBundle{}, fmt.Errorf("invalid relay signing trust bundle: %w", err)
	}
	return bundle, nil
}

func (s *Server) newSession(ctx context.Context, conn net.Conn, writer *bufio.Writer, credential protocolv1.Credential, heartbeatInterval time.Duration) *session {
	sessionCtx, cancel := context.WithCancel(ctx)
	s.admissionMu.Lock()
	if s.QueueBudget == nil {
		s.QueueBudget = &budget.Bytes{Limit: 64 << 20}
	}
	pool := s.QueueBudget
	s.admissionMu.Unlock()
	return &session{
		frameRate: budget.NewRate(1000, 100), pool: pool, trust: s.relaySigningTrustBundle, controlDeadline: s.Control.AuthorizationDeadline,
		networkID:                 credential.NetworkID,
		nodeID:                    credential.NodeID,
		credential:                credential,
		conn:                      conn,
		writer:                    writer,
		sendCh:                    make(chan protocolv1.ServerFrame, s.outboundQueueFrames()),
		ctx:                       sessionCtx,
		cancel:                    cancel,
		done:                      make(chan struct{}),
		metrics:                   s.metrics(),
		heartbeatInterval:         heartbeatInterval,
		bandwidthLimitBytesPerSec: s.bandwidthLimitBytesPerSecond(),
	}
}

func (s *Server) outboundQueueFrames() int {
	if s.OutboundQueueFrames > 0 {
		return s.OutboundQueueFrames
	}
	return DefaultOutboundQueueFrames
}

func (s *Server) bandwidthLimitBytesPerSecond() int {
	if s.BandwidthLimitBytesPerSecond > 0 {
		return s.BandwidthLimitBytesPerSecond
	}
	return 0
}

func heartbeatIntervalFromAuth(intervalMS int) time.Duration {
	if intervalMS == 0 {
		return 0
	}
	return time.Duration(intervalMS) * time.Millisecond
}

func (s *Server) authTimeout() time.Duration {
	if s.AuthTimeout > 0 {
		return s.AuthTimeout
	}
	return DefaultAuthTimeout
}

func (s *Server) addSession(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]map[string]*session{}
	}
	networkSessions := s.sessions[sess.networkID]
	if networkSessions == nil {
		networkSessions = map[string]*session{}
		s.sessions[sess.networkID] = networkSessions
	}
	if previous := networkSessions[sess.nodeID]; previous != nil && previous != sess {
		previous.close()
	}
	networkSessions[sess.nodeID] = sess
}

func (s *Server) removeSession(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	networkSessions := s.sessions[sess.networkID]
	if networkSessions == nil || networkSessions[sess.nodeID] != sess {
		return
	}
	delete(networkSessions, sess.nodeID)
	if len(networkSessions) == 0 {
		delete(s.sessions, sess.networkID)
	}
}

func (s *Server) peerSession(networkID, peerID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	networkSessions := s.sessions[networkID]
	if networkSessions == nil {
		return nil
	}
	return networkSessions[peerID]
}

func (s *Server) closeSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, networkSessions := range s.sessions {
		for _, sess := range networkSessions {
			sess.close()
		}
	}
}

func (s *Server) metrics() *Metrics {
	if s.Metrics != nil {
		return s.Metrics
	}
	return nil
}

func (s *Server) admissionController() *AdmissionController {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.Admission == nil {
		s.Admission = newDefaultAdmissionController(s.Metrics)
	}
	return s.Admission
}

func (s *session) writeReady() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := json.NewEncoder(s.writer).Encode(protocolv1.Ready{Type: protocolv1.MessageReady, ProtocolVersion: protocolv1.Version, Ready: true}); err != nil {
		return err
	}
	return s.writer.Flush()
}

func (s *session) writeError(message string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(outboundWriteTimeout)); err != nil {
		return err
	}
	defer func() {
		_ = s.conn.SetWriteDeadline(time.Time{})
	}()
	if err := json.NewEncoder(s.writer).Encode(protocolv1.Error{Type: protocolv1.MessageError, ProtocolVersion: protocolv1.Version, Error: message}); err != nil {
		return err
	}
	return s.writer.Flush()
}

func (s *session) startWriter() {
	go func() {
		defer close(s.done)
		defer func() {
			s.queueMu.Lock()
			defer s.queueMu.Unlock()
			for {
				select {
				case f := <-s.sendCh:
					s.releaseFrame(f)
				default:
					return
				}
			}
		}()
		for {
			select {
			case <-s.ctx.Done():
				return
			case frame := <-s.sendCh:
				if (!frame.ValidUntil.IsZero() && !time.Now().Before(frame.ValidUntil)) || (frame.Guard != nil && !frame.Guard()) {
					s.releaseFrame(frame)
					s.metrics.recordDrop("fenced")
					continue
				}
				err := s.writeFrame(frame)
				s.releaseFrame(frame)
				if err != nil {
					if errors.Is(err, errStaleQueuedFrame) && s.usable() {
						s.metrics.recordDrop("fenced")
						continue
					}
					if !s.closed.Load() {
						s.metrics.recordDrop("write_failed")
					}
					s.close()
					return
				}
			}
		}
	}()
}

func (s *session) startHeartbeat() {
	if s.heartbeatInterval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(s.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-s.done:
				return
			case <-ticker.C:
				if err := s.writeHeartbeat(); err != nil {
					if !s.closed.Load() {
						s.metrics.recordDrop("write_failed")
					}
					s.close()
					return
				}
			}
		}
	}()
}

func (s *session) writeHeartbeat() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(outboundWriteTimeout)); err != nil {
		return err
	}
	defer func() {
		_ = s.conn.SetWriteDeadline(time.Time{})
	}()
	if err := json.NewEncoder(s.writer).Encode(protocolv1.Heartbeat{Type: protocolv1.MessageHeartbeat, ProtocolVersion: protocolv1.Version, Time: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
		return err
	}
	if err := s.writer.Flush(); err != nil {
		return err
	}
	s.metrics.recordHeartbeat()
	return nil
}

func (s *session) startLeaseRenewal(control ControlPlane, interval, timeout time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-s.done:
				return
			case <-ticker.C:
				renewCtx := s.ctx
				var cancel context.CancelFunc
				if timeout > 0 {
					renewCtx, cancel = context.WithTimeout(s.ctx, timeout)
				}
				started := time.Now()
				err := control.RenewSession(renewCtx, s.lease)
				if cancel != nil {
					cancel()
				}
				if err == nil {
					nextDeadline := started.Add(10 * time.Second)
					s.validUntil.Store(&nextDeadline)
				}
				if err != nil {
					if errors.Is(err, ErrControlUnavailable) && s.usable() {
						continue
					}
					s.revoke()
					return
				}
			}
		}
	}()
}

func (s *session) enqueueFrame(frame protocolv1.ServerFrame) error {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	cost := int64(len(frame.Payload)*3 + 512)
	if s.pool == nil {
		s.pool = &budget.Bytes{Limit: 64 << 20}
	}
	if s.queueBytes.Load()+cost > 256<<10 || !s.pool.Acquire(cost) {
		return errRelayOutboundQueueFull
	}
	s.queueBytes.Add(cost)
	frame.ReservedBytes = cost
	accepted := false
	defer func() {
		if !accepted {
			s.releaseFrame(frame)
		}
	}()
	if s.closed.Load() {
		return errors.New("relay peer is closed")
	}
	select {
	case <-s.done:
		return errors.New("relay peer is closed")
	default:
	}
	select {
	case s.sendCh <- frame:
		accepted = true
		return nil
	case <-s.done:
		return errors.New("relay peer is closed")
	default:
		return errRelayOutboundQueueFull
	}
}

func (s *session) allowInboundPayload(size int) bool {
	if s.bandwidthLimitBytesPerSec <= 0 {
		return true
	}
	now := time.Now()
	if s.bandwidthWindowStart.IsZero() || now.Sub(s.bandwidthWindowStart) >= time.Second {
		s.bandwidthWindowStart = now
		s.bandwidthWindowBytes = 0
	}
	if size > s.bandwidthLimitBytesPerSec || s.bandwidthWindowBytes+size > s.bandwidthLimitBytesPerSec {
		return false
	}
	s.bandwidthWindowBytes += size
	return true
}

func (s *session) close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if s.cancel != nil {
			s.cancel()
		}
		_ = s.conn.Close()
	})
}

func (s *session) revoke() {
	if s.revoked.CompareAndSwap(false, true) {
		s.metrics.recordSessionRevoked()
	}
	s.close()
}

func (s *session) writeFrame(frame protocolv1.ServerFrame) (result error) {
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	_, span := otel.Tracer("relay/dataplane").Start(ctx, "relay.dataplane.write")
	defer func() {
		if result != nil {
			span.SetStatus(otelcodes.Error, "frame_write_failed")
		}
		span.End()
	}()
	if !s.usable() || (!frame.ValidUntil.IsZero() && !time.Now().Before(frame.ValidUntil)) || (frame.Guard != nil && !frame.Guard()) {
		return errStaleQueuedFrame
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.usable() || (frame.Guard != nil && !frame.Guard()) {
		return errStaleQueuedFrame
	}
	deadline := time.Now().Add(outboundWriteTimeout)
	if !frame.ValidUntil.IsZero() && frame.ValidUntil.Before(deadline) {
		deadline = frame.ValidUntil
	}
	until := s.authorizationDeadline()
	if until.IsZero() && s.controlDeadline != nil {
		return errStaleQueuedFrame
	}
	if !until.IsZero() && until.Before(deadline) {
		deadline = until
	}
	if frame.CurrentDeadline != nil {
		until := frame.CurrentDeadline()
		if until.IsZero() {
			return errStaleQueuedFrame
		}
		if until.Before(deadline) {
			deadline = until
		}
	}
	if !s.credential.ExpiresAt.IsZero() && s.credential.ExpiresAt.Add(-time.Second).Before(deadline) {
		deadline = s.credential.ExpiresAt.Add(-time.Second)
	}
	if s.trust != nil {
		bundle, err := s.trust()
		if err != nil {
			return errors.New("trust_unavailable")
		}
		key, err := bundle.Resolve(s.credential.KeyID, time.Now().Add(time.Second))
		if err != nil {
			return errors.New("trust_expired")
		}
		if limit := key.NotAfter; limit != nil && limit.Add(-time.Second).Before(deadline) {
			deadline = limit.Add(-time.Second)
		}
	}
	if err := s.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	defer func() {
		_ = s.conn.SetWriteDeadline(time.Time{})
	}()
	if err := json.NewEncoder(s.writer).Encode(frame); err != nil {
		return err
	}
	return s.writer.Flush()
}

func readLine(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, errRelayLineTooLarge
		}
		return nil, err
	}
	if len(line) > maxLineBytes {
		return nil, errRelayLineTooLarge
	}
	return line, nil
}

func newServerFrame(fromNetworkID, fromNodeID string, payload []byte) protocolv1.ServerFrame {
	return protocolv1.ServerFrame{Type: protocolv1.MessageServerFrame, ProtocolVersion: protocolv1.Version, FromNodeID: fromNodeID, FromNetworkID: fromNetworkID, Payload: payload}
}

func (s *Server) DeliverRemote(networkID, fromNodeID, destinationNetworkID, toNodeID string, destinationEpoch int64, authorizedUntil time.Time, guard func() bool, payload []byte) error {
	if destinationNetworkID == "" || destinationNetworkID != strings.TrimSpace(destinationNetworkID) || networkID == "" || networkID != strings.TrimSpace(networkID) || fromNodeID == "" || fromNodeID != strings.TrimSpace(fromNodeID) || toNodeID == "" || toNodeID != strings.TrimSpace(toNodeID) || destinationEpoch <= 0 || len(payload) == 0 || len(payload) > MaxFramePayloadBytes {
		s.metrics().recordDrop("invalid")
		return errors.New("invalid relay mesh payload")
	}
	if authorizedUntil.IsZero() || !time.Now().Before(authorizedUntil) || (guard != nil && !guard()) {
		return errStaleQueuedFrame
	}
	if !s.allowMessage() || !s.allowPayload(len(payload)) {
		return errors.New("mesh_payload_rate")
	}
	peer := s.peerSession(destinationNetworkID, toNodeID)
	if peer == nil {
		s.metrics().recordDrop("no_peer")
		return errors.New("relay mesh destination is not connected")
	}
	if peer.lease.Epoch != destinationEpoch {
		s.metrics().recordDrop("fenced")
		return ErrDestinationFenced
	}
	frame := newServerFrame(networkID, fromNodeID, payload)
	// Preserve the sender grant and also cap destination queue residence.
	frame.ValidUntil = time.Now().Add(outboundWriteTimeout)
	if authorizedUntil.Before(frame.ValidUntil) {
		frame.ValidUntil = authorizedUntil
	}
	frame.Guard = guard
	if err := peer.enqueueFrame(frame); err != nil {
		s.metrics().recordDrop("slow_consumer")
		return err
	}
	s.metrics().recordOutboundFrame(len(payload))
	return nil
}

func isNetTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func writeError(writer *bufio.Writer, message string) {
	_ = json.NewEncoder(writer).Encode(protocolv1.Error{Type: protocolv1.MessageError, ProtocolVersion: protocolv1.Version, Error: message})
	_ = writer.Flush()
}

func (s *session) releaseFrame(f protocolv1.ServerFrame) {
	s.queueBytes.Add(-f.ReservedBytes)
	s.pool.Release(f.ReservedBytes)
}
func (s *session) usable() bool {
	if s.closed.Load() {
		return false
	}
	if until := s.validUntil.Load(); until != nil && !time.Now().Before(*until) {
		return false
	}
	if s.controlDeadline != nil {
		until := s.controlDeadline(s.credential)
		if until.IsZero() || !time.Now().Before(until) {
			return false
		}
	}
	if !s.credential.ExpiresAt.IsZero() && !time.Now().Add(time.Second).Before(s.credential.ExpiresAt) {
		return false
	}
	if s.trust != nil {
		b, e := s.trust()
		if e != nil {
			return false
		}
		_, e = b.Resolve(s.credential.KeyID, time.Now().Add(time.Second))
		if e != nil {
			return false
		}
	}
	return true
}
func (s *Server) allowPayload(n int) bool {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.payloadRate == nil {
		s.payloadRate = budget.NewRate(12500000, 1250000)
		if s.frameRate == nil {
			s.frameRate = budget.NewRate(20000, 2000)
		}
	}
	return s.payloadRate.Allow(n)
}

func (s *Server) allowMessage() bool {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.frameRate == nil {
		s.frameRate = budget.NewRate(20000, 2000)
	}
	return s.frameRate.Allow(1)
}

func (s *session) authorizationDeadline() time.Time {
	var until time.Time
	if grant := s.validUntil.Load(); grant != nil {
		until = *grant
	}
	if s.controlDeadline != nil {
		current := s.controlDeadline(s.credential)
		if current.IsZero() {
			return time.Time{}
		}
		if until.IsZero() || current.Before(until) {
			until = current
		}
	}
	if !s.credential.ExpiresAt.IsZero() {
		limit := s.credential.ExpiresAt.Add(-time.Second)
		if until.IsZero() || limit.Before(until) {
			until = limit
		}
	}
	return until
}
