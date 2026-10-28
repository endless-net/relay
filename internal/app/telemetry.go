package app

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type event struct{ name, trace string }
type telemetry struct {
	done         chan struct{}
	sink         *os.File
	tracer       trace.Tracer
	provider     *sdk.TracerProvider
	queue        chan event
	cancel       context.CancelFunc
	dropped      atomic.Uint64
	exportFailed atomic.Uint64
}

func newTelemetry(ctx context.Context, endpoint string) (*telemetry, error) {
	opts := []sdk.TracerProviderOption{sdk.WithSampler(sdk.ParentBased(sdk.TraceIDRatioBased(0.01))), sdk.WithResource(resource.NewSchemaless(attribute.String("service.name", "relay")))}
	if endpoint != "" {
		exporter, e := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint), otlptracehttp.WithTimeout(time.Second))
		if e != nil {
			return nil, e
		}
		opts = append(opts, sdk.WithBatcher(exporter, sdk.WithMaxQueueSize(256), sdk.WithMaxExportBatchSize(64), sdk.WithBatchTimeout(time.Second), sdk.WithExportTimeout(time.Second)))
	}
	p := sdk.NewTracerProvider(opts...)
	otel.SetTracerProvider(p)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	run, cancel := context.WithCancel(context.Background())
	t := &telemetry{tracer: p.Tracer("relay/runtime009"), provider: p, queue: make(chan event, 256), done: make(chan struct{}), cancel: cancel}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(error) { t.exportFailed.Add(1) }))
	sink, e := os.OpenFile("/dev/stderr", os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		_ = p.Shutdown(ctx)
		return nil, e
	}
	t.sink = sink
	logger := slog.New(slog.NewJSONHandler(sink, nil)).With("service", "relay", "component", "runtime")
	slog.SetDefault(slog.New(safeHandler{t: t}))
	go func() {
		defer close(t.done)
		for {
			select {
			case <-run.Done():
				return
			case e := <-t.queue:
				_ = sink.SetWriteDeadline(time.Now().Add(time.Second))
				logger.InfoContext(run, e.name, "trace_id", e.trace)
			}
		}
	}()
	return t, nil
}
func (t *telemetry) log(name, id string) {
	if t == nil {
		return
	}
	switch name {
	case "started", "stopped", "fatal", "rpc", "registry_unavailable", "trust_unavailable", "workload_expired", "listener_failed", "control_failed", "dataplane_failed":
	default:
		name = "runtime"
	}
	select {
	case t.queue <- event{name, id}:
	default:
		t.dropped.Add(1)
	}
}
func (t *telemetry) close(ctx context.Context) error {
	t.cancel()
	if t.done != nil {
		select {
		case <-t.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return t.provider.Shutdown(ctx)
}

type safeHandler struct{ t *telemetry }

func (h safeHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h safeHandler) Handle(ctx context.Context, r slog.Record) error {
	h.t.log(r.Message, trace.SpanContextFromContext(ctx).TraceID().String())
	return nil
}
func (h safeHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h safeHandler) WithGroup(string) slog.Handler      { return h }
