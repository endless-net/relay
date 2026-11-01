package mesh

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// clientWriter is used serially by one outgoing stream. Closing its owning
// grpc.ClientConn interrupts flow-controlled Send, so no goroutine/channel or
// new watchdog is needed for each data frame. Incoming server streams use
// sendBounded instead: returning their handler is what cancels a blocked Send.
type clientWriter struct {
	ctx        context.Context
	abort      func()
	timer      *time.Timer
	stopCancel func() bool
}

func newClientWriter(ctx context.Context, abort func()) *clientWriter {
	w := &clientWriter{ctx: ctx, abort: abort}
	w.timer = time.AfterFunc(time.Hour, abort)
	w.timer.Stop()
	w.stopCancel = context.AfterFunc(ctx, abort)
	return w
}

func (w *clientWriter) close() { w.timer.Stop(); w.stopCancel() }

func (w *clientWriter) send(until time.Time, send func() error) (result error) {
	_, span := otel.Tracer("relay/mesh").Start(w.ctx, "relay.mesh.send")
	defer func() {
		if result != nil {
			span.SetStatus(codes.Error, "mesh_send_failed")
		}
		span.End()
	}()
	if err := w.ctx.Err(); err != nil {
		return err
	}
	remaining := 2 * time.Second
	if !until.IsZero() {
		remaining = min(remaining, time.Until(until))
	}
	if deadline, ok := w.ctx.Deadline(); ok {
		remaining = min(remaining, time.Until(deadline))
	}
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	w.timer.Reset(remaining)
	result = send()
	if !w.timer.Stop() {
		w.abort()
		return errors.New("mesh_write_deadline")
	}
	return result
}
