package relay

import (
	"errors"
	"net"
	"strings"
	"sync"
)

const (
	DefaultMaxConnections          = 4096
	DefaultMaxConcurrentAuth       = 128
	DefaultMaxConnectionsPerSource = 32
)

type AdmissionLimits struct {
	MaxConnections          int
	MaxConcurrentAuth       int
	MaxConnectionsPerSource int
}

// AdmissionController is shared by every listener in a relay process. It
// applies load shedding before a connection goroutine is created and bounds
// expensive authentication work independently from established sessions.
type AdmissionController struct {
	connections chan struct{}
	auth        chan struct{}
	perSource   int
	metrics     *Metrics

	sourcesMu sync.Mutex
	sources   map[string]int
}

func NewAdmissionController(limits AdmissionLimits, metrics *Metrics) (*AdmissionController, error) {
	if limits.MaxConnections <= 0 {
		return nil, errors.New("relay max connections must be positive")
	}
	if limits.MaxConcurrentAuth <= 0 {
		return nil, errors.New("relay max concurrent auth must be positive")
	}
	if limits.MaxConcurrentAuth > limits.MaxConnections {
		return nil, errors.New("relay max concurrent auth must not exceed max connections")
	}
	if limits.MaxConnectionsPerSource <= 0 {
		return nil, errors.New("relay max connections per source must be positive")
	}
	if limits.MaxConnectionsPerSource > limits.MaxConnections {
		return nil, errors.New("relay max connections per source must not exceed max connections")
	}
	return &AdmissionController{
		connections: make(chan struct{}, limits.MaxConnections),
		auth:        make(chan struct{}, limits.MaxConcurrentAuth),
		perSource:   limits.MaxConnectionsPerSource,
		metrics:     metrics,
		sources:     make(map[string]int),
	}, nil
}

func newDefaultAdmissionController(metrics *Metrics) *AdmissionController {
	controller, err := NewAdmissionController(AdmissionLimits{
		MaxConnections:          DefaultMaxConnections,
		MaxConcurrentAuth:       DefaultMaxConcurrentAuth,
		MaxConnectionsPerSource: DefaultMaxConnectionsPerSource,
	}, metrics)
	if err != nil {
		panic(err)
	}
	return controller
}

func (a *AdmissionController) tryAcquireConnection(remote net.Addr) (func(), bool) {
	if a == nil {
		return func() {}, true
	}
	select {
	case a.connections <- struct{}{}:
	default:
		a.metrics.recordConnectionRejected("global_limit")
		return nil, false
	}

	source := relaySource(remote)
	a.sourcesMu.Lock()
	if a.sources[source] >= a.perSource {
		a.sourcesMu.Unlock()
		<-a.connections
		a.metrics.recordConnectionRejected("source_limit")
		return nil, false
	}
	a.sources[source]++
	a.sourcesMu.Unlock()
	a.metrics.recordConnectionStart()

	var once sync.Once
	return func() {
		once.Do(func() {
			a.sourcesMu.Lock()
			remaining := a.sources[source] - 1
			if remaining <= 0 {
				delete(a.sources, source)
			} else {
				a.sources[source] = remaining
			}
			a.sourcesMu.Unlock()
			<-a.connections
			a.metrics.recordConnectionEnd()
		})
	}, true
}

func (a *AdmissionController) tryAcquireAuth() (func(), bool) {
	if a == nil {
		return func() {}, true
	}
	select {
	case a.auth <- struct{}{}:
		a.metrics.recordAuthStart()
	default:
		a.metrics.recordConnectionRejected("auth_limit")
		return nil, false
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-a.auth
			a.metrics.recordAuthEnd()
		})
	}, true
}

func relaySource(addr net.Addr) string {
	if addr == nil {
		return "unknown"
	}
	raw := strings.TrimSpace(addr.String())
	host, _, err := net.SplitHostPort(raw)
	if err == nil {
		host = strings.TrimSpace(strings.TrimSuffix(host, "."))
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
		if host != "" {
			return strings.ToLower(host)
		}
	}
	if len(raw) > 256 {
		return raw[:256]
	}
	if raw == "" {
		return "unknown"
	}
	return raw
}
