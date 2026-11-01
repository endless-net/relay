package relay

import (
	"context"
	"errors"
	"time"

	protocolv1 "github.com/endless-net/relay/internal/wire"
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
	ValidUntil      time.Time
	CurrentDeadline func() time.Time
	Valid           func() bool
	RelayID         string
	BootID          string
	Epoch           int64
}

type ControlPlane interface {
	AuthorizationDeadline(protocolv1.Credential) time.Time
	AcquireSession(context.Context, protocolv1.Credential) (SessionLease, error)
	RenewSession(context.Context, SessionLease) error
	ReleaseSession(context.Context, SessionLease) error
	AuthorizePeer(context.Context, protocolv1.Credential, int64, string, string) (PeerRoute, error)
}

// AuthorizationDeadline cannot extend a route's originally captured lifetime.
func (r PeerRoute) AuthorizationDeadline() time.Time {
	until := r.ValidUntil
	if r.CurrentDeadline != nil {
		current := r.CurrentDeadline()
		if current.IsZero() {
			return time.Time{}
		}
		if until.IsZero() || current.Before(until) {
			until = current
		}
	}
	return until
}

type MeshForwarder interface {
	Forward(context.Context, PeerRoute, string, string, string, string, []byte) error
}

var (
	ErrControlUnavailable = errors.New("relay control plane is unavailable")
	ErrDestinationFenced  = errors.New("relay mesh destination is fenced")
)
