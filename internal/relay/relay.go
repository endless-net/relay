package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	protocolv1 "github.com/endless-net/relay/protocol/v1"
)

const (
	MaxFramePayloadBytes       = protocolv1.MaxFramePayloadBytes
	DefaultOutboundQueueFrames = 64
	DefaultAuthTimeout         = 5 * time.Second
	MinHeartbeatInterval       = time.Duration(protocolv1.MinHeartbeatIntervalMS) * time.Millisecond
	MaxHeartbeatInterval       = time.Duration(protocolv1.MaxHeartbeatIntervalMS) * time.Millisecond
	maxLineBytes               = MaxFramePayloadBytes*2 + 16*1024
	outboundWriteTimeout       = 5 * time.Second
)

var errRelayOutboundQueueFull = errors.New("relay outbound queue is full")
var errRelayLineTooLarge = errors.New("relay line is too large")

type Server struct {
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
	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	tlsConfig := s.TLSConfig.Clone()
	listener = tls.NewListener(listener, tlsConfig)
	defer listener.Close()
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	s.listening.Store(true)
	defer s.listening.Store(false)
	if s.fenced.Load() {
		return errors.New("relay is fenced")
	}
	defer s.closeSessions()
	go func() {
		<-ctx.Done()
		s.metrics().setDraining(true)
		_ = listener.Close()
		s.closeSessions()
	}()
	slog.Info("starting endlessnet relay server", "addr", listener.Addr().String(), "tls", true)
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
		go func() {
			defer release()
			s.handleConn(ctx, conn)
		}()
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

func (s *Server) authorizePeer(ctx context.Context, credential protocolv1.Credential, sourceEpoch int64, peerID string) (PeerRoute, error) {
	authorizationCtx := ctx
	var cancel context.CancelFunc
	if timeout := s.authTimeout(); timeout > 0 {
		authorizationCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return s.Control.AuthorizePeer(authorizationCtx, credential, sourceEpoch, peerID)
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
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
			writeError(writer, "invalid relay auth")
		} else if isNetTimeout(err) {
			s.metrics().recordAuthFailure()
			writeError(writer, "relay auth timed out")
		}
		return
	}
	var auth protocolv1.ClientHello
	if err := decodeRelayLine(authLine, &auth); err != nil {
		s.metrics().recordAuthFailure()
		writeError(writer, "invalid relay auth")
		return
	}
	if err := auth.Validate(); err != nil {
		s.metrics().recordAuthFailure()
		writeError(writer, "unsupported relay protocol")
		return
	}
	trustBundle, err := s.relaySigningTrustBundle()
	if err != nil {
		s.metrics().recordAuthFailure()
		writeError(writer, "invalid relay credential")
		return
	}
	trustedKey, err := trustBundle.Resolve(auth.Credential.KeyID, time.Now().UTC())
	if err != nil || protocolv1.Verify(auth.Credential, trustedKey.PublicKey, time.Now().UTC()) != nil {
		s.metrics().recordAuthFailure()
		writeError(writer, "invalid relay credential")
		return
	}
	controlCtx, cancelControl := context.WithTimeout(ctx, s.authTimeout())
	lease, err := s.Control.AcquireSession(controlCtx, auth.Credential)
	cancelControl()
	if err != nil {
		s.metrics().recordAuthFailure()
		writeError(writer, "relay coordinator rejected session")
		return
	}
	authRelease()
	sess := s.newSession(ctx, conn, writer, auth.Credential, heartbeatIntervalFromAuth(auth.HeartbeatIntervalMS))
	sess.lease = lease
	s.addSession(sess)
	if s.fenced.Load() {
		sess.close()
	}
	s.metrics().recordSessionStart()
	defer func() {
		s.removeSession(sess)
		if sess.lease.Epoch > 0 {
			releaseCtx, cancel := context.WithTimeout(context.Background(), s.authTimeout())
			_ = s.Control.ReleaseSession(releaseCtx, sess.lease)
			cancel()
		}
		sess.close()
		s.metrics().recordSessionEnd()
	}()
	if err := sess.writeReady(); err != nil {
		return
	}
	sess.startWriter()
	sess.startHeartbeat()
	sess.startLeaseRenewal(s.Control, 5*time.Second, s.authTimeout())
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line, err := readLine(reader)
		if err != nil {
			if errors.Is(err, errRelayLineTooLarge) {
				s.metrics().recordDrop("invalid")
				sess.writeError("relay frame is too large")
			}
			return
		}
		var frame protocolv1.ClientFrame
		if err := decodeRelayLine(line, &frame); err != nil {
			s.metrics().recordDrop("invalid")
			sess.writeError("invalid relay frame")
			return
		}
		if err := frame.Validate(); err != nil {
			s.metrics().recordDrop("invalid")
			sess.writeError(err.Error())
			return
		}
		peerID := frame.PeerID
		if peerID == sess.nodeID {
			s.metrics().recordDrop("invalid")
			sess.writeError("relay peer_id cannot target the sending node")
			return
		}
		if !sess.allowInboundPayload(len(frame.Payload)) {
			s.metrics().recordDrop("bandwidth")
			sess.writeError("relay bandwidth limit exceeded")
			return
		}
		route, err := s.authorizePeer(sess.ctx, sess.credential, sess.lease.Epoch, peerID)
		if err != nil {
			s.metrics().recordDrop("acl")
			writeErr := sess.writeError("relay peer is not allowed")
			if errors.Is(err, ErrControlUnavailable) || writeErr != nil {
				sess.revoke()
				return
			}
			continue
		}
		s.metrics().recordInboundFrame(len(frame.Payload))
		if route.RelayID != s.RelayID {
			if s.Mesh.Forward(ctx, route, sess.networkID, sess.nodeID, peerID, frame.Payload) != nil {
				s.metrics().recordDrop("mesh")
				sess.writeError("relay peer forwarding failed")
			}
			continue
		}
		peer := s.peerSession(sess.networkID, peerID)
		if peer == nil {
			s.metrics().recordDrop("no_peer")
			sess.writeError("relay peer is not connected")
			continue
		}
		if peer.lease.Epoch != route.Epoch {
			s.metrics().recordDrop("fenced")
			sess.writeError("relay peer session is fenced")
			continue
		}
		if err := peer.enqueueFrame(newServerFrame(sess.nodeID, frame.Payload)); err != nil {
			s.metrics().recordDrop("slow_consumer")
			peer.close()
			sess.writeError("relay peer write failed")
			continue
		}
		s.metrics().recordOutboundFrame(len(frame.Payload))
	}
}

func decodeRelayLine(line []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("relay line contains multiple JSON values")
		}
		return err
	}
	return nil
}

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
	return &session{
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
		for {
			select {
			case <-s.ctx.Done():
				return
			case frame := <-s.sendCh:
				if err := s.writeFrame(frame); err != nil {
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
				err := control.RenewSession(renewCtx, s.lease)
				if cancel != nil {
					cancel()
				}
				if err != nil {
					s.revoke()
					return
				}
			}
		}
	}()
}

func (s *session) enqueueFrame(frame protocolv1.ServerFrame) error {
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

func (s *session) writeFrame(frame protocolv1.ServerFrame) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(outboundWriteTimeout)); err != nil {
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

func newServerFrame(fromNodeID string, payload []byte) protocolv1.ServerFrame {
	return protocolv1.ServerFrame{Type: protocolv1.MessageServerFrame, ProtocolVersion: protocolv1.Version, FromNodeID: fromNodeID, Payload: payload}
}

func (s *Server) DeliverRemote(networkID, fromNodeID, toNodeID string, destinationEpoch int64, payload []byte) error {
	if networkID == "" || networkID != strings.TrimSpace(networkID) || fromNodeID == "" || fromNodeID != strings.TrimSpace(fromNodeID) || toNodeID == "" || toNodeID != strings.TrimSpace(toNodeID) || destinationEpoch <= 0 || len(payload) == 0 || len(payload) > MaxFramePayloadBytes {
		s.metrics().recordDrop("invalid")
		return errors.New("invalid relay mesh payload")
	}
	peer := s.peerSession(networkID, toNodeID)
	if peer == nil {
		s.metrics().recordDrop("no_peer")
		return errors.New("relay mesh destination is not connected")
	}
	if peer.lease.Epoch != destinationEpoch {
		s.metrics().recordDrop("fenced")
		return ErrDestinationFenced
	}
	if err := peer.enqueueFrame(newServerFrame(fromNodeID, payload)); err != nil {
		s.metrics().recordDrop("slow_consumer")
		peer.close()
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
