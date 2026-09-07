//go:build e2e

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/endless-net/relay/internal/tlsconfig"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
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

type authorizationRequest struct {
	Action     string                `json:"action"`
	Credential protocolv1.Credential `json:"credential"`
	PeerID     string                `json:"peer_id,omitempty"`
}

type server struct {
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
	mux.HandleFunc("/internal/coordinator/relay-control/v1/trust-bundle", s.trustBundle)
	mux.HandleFunc("/internal/coordinator/relay-control/v1/authorize", s.authorize)
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
	}
	listener, err := tls.Listen("tcp", *addr, tlsConfig)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("mock coordinator listening on %s", *addr)
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

func (s *server) trustBundle(w http.ResponseWriter, r *http.Request) {
	if !s.before(w, r) {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r.Method != http.MethodGet || !s.authorizedService(r) {
		http.Error(w, "service authentication required", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		RelayTrustBundle protocolv1.SigningTrustBundle `json:"relay_trust_bundle"`
	}{RelayTrustBundle: s.config.TrustBundle})
}

func (s *server) authorize(w http.ResponseWriter, r *http.Request) {
	if !s.before(w, r) {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r.Method != http.MethodPost || !s.authorizedService(r) {
		http.Error(w, "service authentication required", http.StatusUnauthorized)
		return
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	var request authorizationRequest
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid authorization request", http.StatusBadRequest)
		return
	}
	if err := requireEOF(decoder); err != nil {
		http.Error(w, "invalid authorization request", http.StatusBadRequest)
		return
	}
	if !s.authorizeRequest(request) {
		http.Error(w, "relay authorization denied", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) authorizeRequest(request authorizationRequest) bool {
	credential := request.Credential
	if credential.NetworkID != s.config.NetworkID {
		return false
	}
	if _, ok := s.nodes[credential.NodeID]; !ok {
		return false
	}
	key, err := s.config.TrustBundle.Resolve(credential.KeyID, time.Now().UTC())
	if err != nil || protocolv1.Verify(credential, key.PublicKey, time.Now().UTC()) != nil {
		return false
	}
	switch request.Action {
	case "credential":
		if strings.TrimSpace(request.PeerID) != "" {
			return false
		}
		return true
	case "peer":
		if _, ok := s.pairs[pairKey(credential.NodeID, request.PeerID)]; !ok {
			return false
		}
		return true
	default:
		return false
	}
}

func (s *server) authorizedService(r *http.Request) bool {
	if r.TLS == nil {
		return false
	}
	id, err := tlsconfig.PeerID(*r.TLS)
	return err == nil && id.String() == s.policy.CoordinatorID
}
func (s *server) before(w http.ResponseWriter, r *http.Request) bool {
	s.mu.RLock()
	mode, delay := s.mode, s.delay
	s.mu.RUnlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return false
		}
	}
	switch mode {
	case "unavailable":
		http.Error(w, "test outage", 503)
		return false
	case "deny":
		http.Error(w, "test denial", 403)
		return false
	case "hang":
		<-r.Context().Done()
		return false
	case "invalid":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"unexpected":true}`)
		return false
	}
	return true
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
