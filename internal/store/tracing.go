package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Never attach SQL text, bind arguments, DSNs or domain identities to spans.
type queryTracer struct{}

func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	ctx, _ = otel.Tracer("relay/registry").Start(ctx, "relay.registry.query")
	return ctx
}

func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, result pgx.TraceQueryEndData) {
	span := trace.SpanFromContext(ctx)
	if result.Err != nil {
		span.SetStatus(codes.Error, "registry_query_failed")
	}
	span.End()
}
