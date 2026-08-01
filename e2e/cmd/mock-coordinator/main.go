//go:build e2e

package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	protocolv1 "github.com/endless-net/relay/protocol/v1"
)

const (
	coordinatorURI = "spiffe://endlessnet.ru/service/relay-coordinator"
	maxBodyBytes   = 1 << 20
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
	config config
	nodes  map[string]struct{}
	pairs  map[string]struct{}
}

func main() {
	addr := flag.String("addr", ":9447", "HTTPS listen address")
	certFile := flag.String("tls-cert-file", "", "server certificate")
	keyFile := flag.String("tls-key-file", "", "server private key")
	caFile := flag.String("service-ca-file", "", "service CA bundle")
	configFile := flag.String("config-file", "", "strict mock configuration")
	flag.Parse()

	cfg, err := loadConfig(*configFile)
	if err != nil {
		log.Fatal(err)
	}
	tlsConfig, err := mutualTLS(*certFile, *keyFile, *caFile)
	if err != nil {
		log.Fatal(err)
	}
	s := newServer(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
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

func mutualTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(strings.TrimSpace(certFile), strings.TrimSpace(keyFile))
	if err != nil {
		return nil, err
	}
	rawCA, err := os.ReadFile(strings.TrimSpace(caFile))
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rawCA) {
		return nil, errors.New("service CA contains no certificates")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		ClientCAs:    roots,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
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
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return false
	}
	leaf := r.TLS.VerifiedChains[0][0]
	return len(leaf.URIs) == 1 && leaf.URIs[0] != nil && leaf.URIs[0].String() == coordinatorURI
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
