// Package relaytest exposes a production-path Relay fixture for external
// client contract tests. It does not expose the internal Relay runtime.
package relaytest

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"sync"

	"github.com/endless-net/relay/internal/relay"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
)

type Config struct {
	Addr        string
	RelayID     string
	TLSConfig   *tls.Config
	TrustBundle protocolv1.SigningTrustBundle
}

type Server struct {
	runtime *relay.Server
}

func NewServer(config Config) (*Server, error) {
	if config.RelayID == "" || config.RelayID != strings.TrimSpace(config.RelayID) {
		return nil, errors.New("relay test id is required")
	}
	if err := config.TrustBundle.Validate(); err != nil {
		return nil, err
	}
	control := newControl(config.RelayID)
	runtime, err := relay.NewServer(relay.ServerConfig{
		Addr:                config.Addr,
		RelayID:             config.RelayID,
		TLSConfig:           config.TLSConfig,
		Metrics:             relay.NewMetrics(),
		Control:             control,
		Mesh:                localMesh{},
		TrustBundleProvider: func() (protocolv1.SigningTrustBundle, error) { return config.TrustBundle, nil },
	})
	if err != nil {
		return nil, err
	}
	return &Server{runtime: runtime}, nil
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	if s == nil || s.runtime == nil {
		return errors.New("relay test server is not configured")
	}
	return s.runtime.ListenAndServe(ctx)
}

type localMesh struct{}

func (localMesh) Forward(context.Context, relay.PeerRoute, string, string, string, []byte) error {
	return errors.New("relay test fixture does not provide remote mesh forwarding")
}

type control struct {
	relayID  string
	bootID   string
	mu       sync.Mutex
	epoch    int64
	sessions map[string]relay.SessionLease
}

func newControl(relayID string) *control {
	return &control{relayID: relayID, bootID: "relaytest-boot", sessions: make(map[string]relay.SessionLease)}
}

func (c *control) AcquireSession(_ context.Context, credential protocolv1.Credential) (relay.SessionLease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	lease := relay.SessionLease{NetworkID: credential.NetworkID, NodeID: credential.NodeID, RelayID: c.relayID, BootID: c.bootID, Epoch: c.epoch, Credential: credential}
	c.sessions[sessionKey(credential.NetworkID, credential.NodeID)] = lease
	return lease, nil
}

func (c *control) RenewSession(_ context.Context, lease relay.SessionLease) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.sessions[sessionKey(lease.NetworkID, lease.NodeID)]
	if !ok || current.Epoch != lease.Epoch {
		return errors.New("relay test session is fenced")
	}
	return nil
}

func (c *control) ReleaseSession(_ context.Context, lease relay.SessionLease) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := sessionKey(lease.NetworkID, lease.NodeID)
	if current, ok := c.sessions[key]; ok && current.Epoch == lease.Epoch {
		delete(c.sessions, key)
	}
	return nil
}

func (c *control) AuthorizePeer(_ context.Context, credential protocolv1.Credential, _ int64, peerID string) (relay.PeerRoute, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	peer, ok := c.sessions[sessionKey(credential.NetworkID, peerID)]
	if !ok {
		return relay.PeerRoute{}, errors.New("relay test peer is not connected")
	}
	return relay.PeerRoute{RelayID: c.relayID, BootID: c.bootID, Epoch: peer.Epoch}, nil
}

func sessionKey(networkID, nodeID string) string {
	return networkID + "\x00" + nodeID
}
