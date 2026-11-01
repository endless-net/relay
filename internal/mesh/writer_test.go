package mesh

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientWriterInterruptsFlowControlledSendAtGrantExpiry(t *testing.T) {
	unblock := make(chan struct{})
	var once sync.Once
	w := newClientWriter(t.Context(), func() { once.Do(func() { close(unblock) }) })
	defer w.close()
	started := time.Now()
	err := w.send(started.Add(30*time.Millisecond), func() error { <-unblock; return errors.New("transport_closed") })
	if err == nil || time.Since(started) > time.Second {
		t.Fatal("grant did not interrupt blocked send")
	}
}

func TestClientWriterCancellationAndWatchdogReuse(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	unblock := make(chan struct{})
	var once sync.Once
	w := newClientWriter(ctx, func() { once.Do(func() { close(unblock) }) })
	defer w.close()
	for range 100 {
		if err := w.send(time.Time{}, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	select {
	case <-unblock:
	case <-time.After(time.Second):
		t.Fatal("stream cancellation did not close transport")
	}
	if err := w.send(time.Time{}, func() error { t.Fatal("cancelled send invoked"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestMeshMetricLabelsNeverIncludeRawErrors(t *testing.T) {
	m := &Manager{}
	m.forwardFailure(errors.New("private-token-network-node"))
	if strings.Contains(m.Render(), "private") || len(strings.Split(strings.TrimSpace(m.Render()), "\n")) != len(eventNames) {
		t.Fatal("unbounded or confidential labels")
	}
}

func BenchmarkMeshWriteWatchdog(b *testing.B) {
	b.Run("shared_timer", func(b *testing.B) {
		w := newClientWriter(b.Context(), func() {})
		defer w.close()
		b.ReportAllocs()
		for b.Loop() {
			if err := w.send(time.Time{}, func() error { return nil }); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("per_call_goroutine", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := sendBounded(b.Context(), func() {}, func() error { return nil }); err != nil {
				b.Fatal(err)
			}
		}
	})
}
