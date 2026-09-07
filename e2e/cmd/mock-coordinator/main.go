//go:build e2e

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	"github.com/endless-net/relay/internal/tlsconfig"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	maxBodyBytes = 1 << 20
)

type config struct {
	NetworkID   string                        `json:"network_id"`
	Nodes       []string                      `json:"nodes"`
	PeerPairs   []peerPair                    `json:"peer_pairs"`
	TrustBundle protocolv1.SigningTrustBundle `json:"relay_trust_bundle"`
}

type peerPair struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type server struct {
	relayv1.UnimplementedRelayUpstreamServiceServer
	mu     sync.RWMutex
	policy tlsconfig.IdentityPolicy
	mode   string
	delay  time.Duration
	config config
	nodes  map[string]struct{}
	pairs  map[string]struct{}
}

func main() {
	addr := flag.String("addr", ":9447", "HTTPS listen address")
	configFile := flag.String("config-file", "", "strict mock configuration")
	workloadAPI := flag.String("workload-api-addr", "", "ephemeral SPIRE Workload API")
	domain := flag.String("trust-domain", tlsconfig.DefaultTrustDomain, "test trust domain")
	flag.Parse()
	policy, err := tlsconfig.NewIdentityPolicy(*domain, "", "")
	if err != nil {
		log.Fatal(err)
	}

	cfg, err := loadConfig(*configFile)
	if err != nil {
		log.Fatal(err)
	}
	runtime, err := tlsconfig.NewWorkloadRuntime(context.Background(), *workloadAPI, policy.UpstreamID)
	if err != nil {
		log.Fatal(err)
	}
	defer runtime.Close()
	tlsConfig, err := runtime.ServerTLSConfig()
	if err != nil {
		log.Fatal(err)
	}
	s := newServer(cfg)
	s.policy = policy
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/test/control", s.control)
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(relayv1.RejectUnknownUnaryServerInterceptor), grpc.MaxRecvMsgSize(maxBodyBytes), grpc.MaxSendMsgSize(maxBodyBytes))
	relayv1.RegisterRelayUpstreamServiceServer(grpcServer, s)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("mock coordinator listening on %s", *addr)
	if err := httpServer.ServeTLS(listener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func loadConfig(path string) (config, error) {
	var cfg config
	raw, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return cfg, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, err
	}
	if err := requireEOF(decoder); err != nil {
		return cfg, err
	}
	if strings.TrimSpace(cfg.NetworkID) == "" || len(cfg.Nodes) == 0 {
		return cfg, errors.New("mock coordinator configuration is incomplete")
	}
	if err := cfg.TrustBundle.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid trust bundle: %w", err)
	}
	return cfg, nil
}

func newServer(cfg config) *server {
	s := &server{config: cfg, nodes: make(map[string]struct{}, len(cfg.Nodes)), pairs: make(map[string]struct{}, len(cfg.PeerPairs))}
	for _, node := range cfg.Nodes {
		s.nodes[strings.TrimSpace(node)] = struct{}{}
	}
	for _, pair := range cfg.PeerPairs {
		s.pairs[pairKey(pair.From, pair.To)] = struct{}{}
	}
	return s
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !s.authorizedService(r) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{\"status\":\"ok\"}\n")
}

func (s *server) GetTrustBundle(ctx context.Context, request *relayv1.GetTrustBundleRequest) (*relayv1.GetTrustBundleResponse, error) {
	if err := s.beforeRPC(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	bundle := relayv1.TrustBundleFromProtocol(s.config.TrustBundle)
	if s.mode == "invalid" {
		bundle.Version = 99
	}
	return &relayv1.GetTrustBundleResponse{RelayTrustBundle: bundle}, nil
}

func (s *server) AuthorizeCredential(ctx context.Context, request *relayv1.AuthorizeCredentialRequest) (*relayv1.AuthorizeCredentialResponse, error) {
	if err := s.beforeRPC(ctx); err != nil {
		return nil, err
	}
	credential, err := relayv1.CredentialToProtocol(request.GetCredential())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid credential")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.authorizeCredential(credential) {
		return nil, status.Error(codes.PermissionDenied, "credential denied")
	}
	return &relayv1.AuthorizeCredentialResponse{}, nil
}

func (s *server) AuthorizePeerPair(ctx context.Context, request *relayv1.AuthorizePeerPairRequest) (*relayv1.AuthorizePeerPairResponse, error) {
	if err := s.beforeRPC(ctx); err != nil {
		return nil, err
	}
	credential, err := relayv1.CredentialToProtocol(request.GetCredential())
	if err != nil || request.GetPeerId() == "" || request.GetPeerId() != strings.TrimSpace(request.GetPeerId()) {
		return nil, status.Error(codes.InvalidArgument, "invalid peer authorization")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, activeTarget := s.nodes[request.GetPeerId()]
	_, allowedPair := s.pairs[pairKey(credential.NodeID, request.GetPeerId())]
	if !s.authorizeCredential(credential) || !activeTarget || !allowedPair {
		return nil, status.Error(codes.PermissionDenied, "peer pair denied")
	}
	return &relayv1.AuthorizePeerPairResponse{}, nil
}

// Called under s.mu; signature, expiry and domain policy remain independent of
// the Relay Coordinator's client adapter and authorization cache.
func (s *server) authorizeCredential(credential protocolv1.Credential) bool {
	if credential.NetworkID != s.config.NetworkID {
		return false
	}
	if _, ok := s.nodes[credential.NodeID]; !ok {
		return false
	}
	now := time.Now().UTC()
	key, err := s.config.TrustBundle.Resolve(credential.KeyID, now)
	return err == nil && protocolv1.Verify(credential, key.PublicKey, now) == nil
}

func (s *server) beforeRPC(ctx context.Context) error {
	remote, ok := peer.FromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "workload identity required")
	}
	tlsInfo, ok := remote.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return status.Error(codes.Unauthenticated, "TLS required")
	}
	id, err := tlsconfig.PeerID(tlsInfo.State)
	if err != nil || id.String() != s.policy.CoordinatorID {
		return status.Error(codes.PermissionDenied, "caller identity denied")
	}
	s.mu.RLock()
	mode, delay := s.mode, s.delay
	s.mu.RUnlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
	switch mode {
	case "unavailable":
		return status.Error(codes.Unavailable, "test outage")
	case "deny":
		return status.Error(codes.PermissionDenied, "test denial")
	case "hang":
		<-ctx.Done()
		return status.FromContextError(ctx.Err()).Err()
	}
	return nil
}

func (s *server) authorizedService(r *http.Request) bool {
	if r.TLS == nil {
		return false
	}
	id, err := tlsconfig.PeerID(*r.TLS)
	return err == nil && id.String() == s.policy.CoordinatorID
}
func (s *server) control(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil {
		http.Error(w, "forbidden", 403)
		return
	}
	id, err := tlsconfig.PeerID(*r.TLS)
	if err != nil || id.String() != s.policy.UpstreamID || r.Method != http.MethodPut {
		http.Error(w, "forbidden", 403)
		return
	}
	var input struct {
		Mode    string  `json:"mode"`
		DelayMS int     `json:"delay_ms"`
		Config  *config `json:"config,omitempty"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid", 400)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = input.Mode
	s.delay = time.Duration(input.DelayMS) * time.Millisecond
	if input.Config != nil {
		replacement := newServer(*input.Config)
		s.config = replacement.config
		s.nodes = replacement.nodes
		s.pairs = replacement.pairs
	}
	w.WriteHeader(204)
}

func pairKey(from, to string) string {
	return strings.TrimSpace(from) + "\x00" + strings.TrimSpace(to)
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}
