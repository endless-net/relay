package mesh

import (
	"errors"
	"fmt"
	"strings"

	"github.com/endless-net/relay/internal/relay"
)

var eventNames = [...]string{"queue_full", "queue_bytes", "global_bytes", "peer_not_ready", "fenced", "stream_reset", "incoming_reset", "other"}

func (m *Manager) forwardFailure(err error) {
	if err == nil {
		return
	}
	index := 7
	switch err.Error() {
	case "relay mesh outbound queue is full":
		index = 0
	case "mesh_queue_bytes":
		index = 1
	case "mesh_global_bytes":
		index = 2
	case "relay mesh peer is unavailable":
		index = 3
	default:
		if errors.Is(err, relay.ErrDestinationFenced) {
			index = 4
		}
	}
	m.events[index].Add(1)
}

// Render never uses caller-controlled identities or errors as metric labels.
func (m *Manager) Render() string {
	var out strings.Builder
	for index, name := range eventNames {
		_, _ = fmt.Fprintf(&out, "endlessnet_relay_mesh_events_total{reason=%q} %d\n", name, m.events[index].Load())
	}
	return out.String()
}
