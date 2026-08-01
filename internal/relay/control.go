package relay

import (
	"context"
	"errors"

	protocolv1 "github.com/endless-net/relay/protocol/v1"
)

type SessionLease struct {
	NetworkID  string
	NodeID     string
	RelayID    string
	BootID     string
	Epoch      int64
	Credential protocolv1.Credential
}

type PeerRoute struct {
	RelayID string
	BootID  string
	Epoch   int64
}

type ControlPlane interface {
	AcquireSession(context.Context, protocolv1.Credential) (SessionLease, error)
	RenewSession(context.Context, SessionLease) error
	ReleaseSession(context.Context, SessionLease) error
	AuthorizePeer(context.Context, protocolv1.Credential, int64, string) (PeerRoute, error)
}

type MeshForwarder interface {
	Forward(context.Context, PeerRoute, string, string, string, []byte) error
}

var (
	ErrControlUnavailable = errors.New("relay control plane is unavailable")
	ErrDestinationFenced  = errors.New("relay mesh destination is fenced")
)
