package relaycontrol

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	"github.com/endless-net/relay/internal/relay"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type PeerUpdater interface {
	UpdatePeers([]*relayv1.RelayInstance)
}

type Client struct {
	RelayID  string
	BootID   string
	MeshAddr string
	Control  relayv1.RelayControlClient
	Peers    PeerUpdater

	HeartbeatInterval time.Duration
	FencingGrace      time.Duration

	mu           sync.RWMutex
	trustBundle  protocolv1.SigningTrustBundle
	leaseExpires time.Time
}

func Dial(address string, tlsConfig *tls.Config) (*grpc.ClientConn, error) {
	if strings.TrimSpace(address) == "" {
		return nil, errors.New("relay Coordinator address is required")
	}
	if tlsConfig == nil {
		return nil, errors.New("relay Coordinator mTLS configuration is required")
	}
	return grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
}

func (c *Client) Run(ctx context.Context) error {
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- c.run(workerCtx) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-result:
			return err
		case <-ticker.C:
			expiry := c.currentLeaseExpiry()
			if !expiry.IsZero() && !time.Now().Before(expiry.Add(c.fencingGrace())) {
				return errors.New("relay instance lease expired")
			}
		}
	}
}
func (c *Client) Ready() bool { return time.Now().Before(c.currentLeaseExpiry()) }
func (c *Client) run(ctx context.Context) error {
	if c.Control == nil || c.Peers == nil || !canonicalRequired(c.RelayID) || !canonicalRequired(c.BootID) || !canonicalRequired(c.MeshAddr) {
		return errors.New("relay control client is not configured")
	}
	registerCtx, cancelRegister := context.WithTimeout(ctx, 5*time.Second)
	response, err := c.Control.RegisterInstance(registerCtx, &relayv1.RegisterInstanceRequest{RelayId: c.RelayID, BootId: c.BootID, MeshAddr: c.MeshAddr})
	cancelRegister()
	if err != nil {
		return err
	}
	if err := rejectResponse(response); err != nil {
		return err
	}
	if err := c.acceptCoordinatorState(response.GetPeers(), response.GetLeaseExpiresUnixNano(), response.GetRelayTrustBundle()); err != nil {
		return err
	}
	ticker := time.NewTicker(c.heartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			heartbeatCtx, cancelHeartbeat := context.WithTimeout(ctx, 5*time.Second)
			response, err := c.Control.HeartbeatInstance(heartbeatCtx, &relayv1.HeartbeatInstanceRequest{RelayId: c.RelayID, BootId: c.BootID})
			cancelHeartbeat()
			if err == nil {
				err = rejectResponse(response)
			}
			if err == nil {
				err = c.acceptCoordinatorState(response.GetPeers(), response.GetLeaseExpiresUnixNano(), response.GetRelayTrustBundle())
			}
			if err != nil && time.Now().UTC().After(c.currentLeaseExpiry().Add(c.fencingGrace())) {
				return err
			}
		}
	}
}

func (c *Client) TrustBundle() (protocolv1.SigningTrustBundle, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.trustBundle.Validate(); err != nil {
		return protocolv1.SigningTrustBundle{}, errors.New("relay trust bundle is unavailable")
	}
	bundle := c.trustBundle
	bundle.Keys = append([]protocolv1.SigningTrustKey(nil), c.trustBundle.Keys...)
	return bundle, nil
}

func (c *Client) AcquireSession(ctx context.Context, credential protocolv1.Credential) (relay.SessionLease, error) {
	response, err := c.Control.AcquireSession(ctx, &relayv1.AcquireSessionRequest{RelayId: c.RelayID, BootId: c.BootID, Credential: relayv1.CredentialFromProtocol(credential)})
	if err != nil {
		return relay.SessionLease{}, err
	}
	if err := rejectResponse(response); err != nil {
		return relay.SessionLease{}, err
	}
	if response.GetEpoch() <= 0 {
		return relay.SessionLease{}, errors.New("relay Coordinator returned an invalid session epoch")
	}
	return relay.SessionLease{NetworkID: credential.NetworkID, NodeID: credential.NodeID, RelayID: c.RelayID, BootID: c.BootID, Epoch: response.GetEpoch(), Credential: credential}, nil
}

func (c *Client) RenewSession(ctx context.Context, lease relay.SessionLease) error {
	response, err := c.Control.RenewSession(ctx, &relayv1.RenewSessionRequest{RelayId: c.RelayID, BootId: c.BootID, NetworkId: lease.NetworkID, NodeId: lease.NodeID, Epoch: lease.Epoch, Credential: relayv1.CredentialFromProtocol(lease.Credential)})
	if err != nil {
		return err
	}
	return rejectResponse(response)
}

func (c *Client) ReleaseSession(ctx context.Context, lease relay.SessionLease) error {
	response, err := c.Control.ReleaseSession(ctx, &relayv1.ReleaseSessionRequest{RelayId: c.RelayID, BootId: c.BootID, NetworkId: lease.NetworkID, NodeId: lease.NodeID, Epoch: lease.Epoch})
	if err != nil {
		return err
	}
	return rejectResponse(response)
}

func (c *Client) AuthorizePeer(ctx context.Context, credential protocolv1.Credential, sourceEpoch int64, peerID string) (relay.PeerRoute, error) {
	response, err := c.Control.AuthorizePeer(ctx, &relayv1.AuthorizePeerRequest{RelayId: c.RelayID, BootId: c.BootID, Credential: relayv1.CredentialFromProtocol(credential), PeerId: peerID, PeerNetworkId: credential.NetworkID, SourceEpoch: sourceEpoch})
	if err != nil {
		return relay.PeerRoute{}, mapPeerAuthorizationError(err)
	}
	if err := rejectResponse(response); err != nil {
		return relay.PeerRoute{}, err
	}
	if !canonicalRequired(response.GetDestinationRelayId()) || !canonicalRequired(response.GetDestinationBootId()) || response.GetDestinationEpoch() <= 0 {
		return relay.PeerRoute{}, errors.New("relay Coordinator returned an invalid destination route")
	}
	return relay.PeerRoute{RelayID: response.GetDestinationRelayId(), BootID: response.GetDestinationBootId(), Epoch: response.GetDestinationEpoch()}, nil
}

func mapPeerAuthorizationError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", relay.ErrControlUnavailable, err)
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded:
		return fmt.Errorf("%w: %v", relay.ErrControlUnavailable, err)
	default:
		return err
	}
}

func (c *Client) acceptCoordinatorState(peers []*relayv1.RelayInstance, expiresUnixNano int64, rawBundle *relayv1.SigningTrustBundle) error {
	expires := time.Unix(0, expiresUnixNano).UTC()
	if expiresUnixNano <= 0 || !expires.After(time.Now().UTC()) {
		return errors.New("relay Coordinator returned an invalid instance lease")
	}
	for _, peer := range peers {
		if peer == nil || !canonicalRequired(peer.GetRelayId()) || !canonicalRequired(peer.GetBootId()) || !canonicalRequired(peer.GetMeshAddr()) {
			return errors.New("relay Coordinator returned an invalid peer")
		}
	}
	bundle, err := relayv1.TrustBundleToProtocol(rawBundle)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.trustBundle = bundle
	c.leaseExpires = expires
	c.mu.Unlock()
	c.Peers.UpdatePeers(peers)
	return nil
}

func rejectResponse(response proto.Message) error {
	if response == nil {
		return errors.New("relay Coordinator returned an empty protobuf response")
	}
	return relayv1.RejectUnknownFields(response)
}

func (c *Client) currentLeaseExpiry() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.leaseExpires
}

func (c *Client) heartbeatInterval() time.Duration {
	if c.HeartbeatInterval > 0 {
		return c.HeartbeatInterval
	}
	return 5 * time.Second
}

func (c *Client) fencingGrace() time.Duration {
	if c.FencingGrace > 0 {
		return c.FencingGrace
	}
	return 5 * time.Second
}

func canonicalRequired(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}
