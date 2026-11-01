package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestControlTracePreservesParentAndRedactsFailure(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdk.NewTracerProvider(sdk.WithSpanProcessor(recorder), sdk.WithSampler(sdk.AlwaysSample()))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	r := &Runtime{telemetry: &telemetry{tracer: provider.Tracer("fixture"), queue: make(chan event, 1)}}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"))
	_, err := r.unary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/fixture/RenewSession"}, func(context.Context, any) (any, error) { return nil, errors.New("private-dsn-credential") })
	if err == nil {
		t.Fatal("handler failure hidden")
	}
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Parent().SpanID().String() != "0123456789abcdef" || spans[0].Status().Code != codes.Error {
		t.Fatal("trace parent/failure missing")
	}
	if strings.Contains(spans[0].Status().Description, "private") || len(spans[0].Events()) != 0 {
		t.Fatal("confidential error recorded")
	}
}
