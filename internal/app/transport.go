package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/endless-net/relay/internal/tlsconfig"
	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type limitedListener struct {
	net.Listener
	slots chan struct{}
}
type trackedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *trackedConn) Close() error { e := c.Conn.Close(); c.once.Do(c.release); return e }
func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		c, e := l.Listener.Accept()
		if e != nil {
			return nil, e
		}
		select {
		case l.slots <- struct{}{}:
			return &trackedConn{Conn: c, release: func() { <-l.slots }}, nil
		default:
			_ = c.Close()
		}
	}
}
func (r *Runtime) listen(ctx context.Context, address string, limit int) (net.Listener, error) {
	l, e := (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	if e != nil {
		return nil, errors.New("listener_bind")
	}
	bounded := &limitedListener{Listener: l, slots: make(chan struct{}, limit)}
	r.listeners = append(r.listeners, bounded)
	return bounded, nil
}

var rpcPeerSlots = make(chan struct{}, 64)
var rpcLifeSlots = make(chan struct{}, 64)

func (r *Runtime) unary(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	rpcSlots := rpcLifeSlots
	if strings.HasSuffix(info.FullMethod, "/AuthorizePeer") {
		rpcSlots = rpcPeerSlots
	}
	select {
	case rpcSlots <- struct{}{}:
		defer func() { <-rpcSlots }()
	default:
		return nil, status.Error(codes.ResourceExhausted, "rpc_capacity")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	incoming, _ := metadata.FromIncomingContext(ctx)
	carrier := propagation.MapCarrier{}
	for _, key := range []string{"traceparent", "tracestate"} {
		values := incoming.Get(key)
		if len(values) == 1 && len(values[0]) <= 512 {
			carrier[key] = values[0]
		}
	}
	ctx = propagation.TraceContext{}.Extract(ctx, carrier)
	ctx, span := r.telemetry.tracer.Start(ctx, "relay.control")
	defer span.End()
	result, e := handler(ctx, request)
	if e != nil {
		span.SetStatus(otelcodes.Error, "control_failed")
	}
	r.telemetry.log("rpc", trace.SpanContextFromContext(ctx).TraceID().String())
	return result, e
}

type traceTransport struct{ base http.RoundTripper }

func (t traceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx, span := otel.Tracer("relay/upstream").Start(request.Context(), "relay.upstream")
	defer span.End()
	request = request.Clone(ctx)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(request.Header))
	response, e := t.base.RoundTrip(request)
	if e != nil {
		span.SetStatus(otelcodes.Error, "upstream_unavailable")
		return nil, e
	}
	if response.StatusCode >= 400 {
		span.SetStatus(otelcodes.Error, "upstream_rejected")
	}
	if response.TLS == nil {
		_ = response.Body.Close()
		return nil, errors.New("upstream_tls_required")
	}
	if _, e := tlsconfig.PeerID(*response.TLS); e != nil {
		_ = response.Body.Close()
		return nil, errors.New("upstream_peer_expired")
	}
	return response, nil
}
