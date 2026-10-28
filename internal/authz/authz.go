package authz

import (
	"context"
	"errors"
	"strings"

	wire "github.com/endless-net/relay/internal/wire"
)

var ErrDenied = errors.New("relay authorization denied")

// Authorizer is a current-authority boundary. Runtime implementations do not layer another allow cache here.
type Authorizer interface {
	AuthorizeCredential(context.Context, wire.Credential) error
	AuthorizePeer(context.Context, wire.Credential, string, string) error
	RelayTrustBundle(context.Context) (wire.SigningTrustBundle, error)
}

func validPeerScope(network, node string) bool {
	return network != "" && node != "" && network == strings.TrimSpace(network) && node == strings.TrimSpace(node)
}
