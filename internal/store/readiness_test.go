package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRegistryClockToleranceIncludesProbeLatency(t *testing.T) {
	started := time.Unix(100, 0)
	finished := started.Add(150 * time.Millisecond)
	for _, tt := range []struct {
		name     string
		observed time.Time
		want     bool
	}{
		{name: "inside probe", observed: started.Add(time.Millisecond), want: true},
		{name: "accepted lower bound", observed: started.Add(-time.Second), want: true},
		{name: "accepted upper bound", observed: finished.Add(time.Second), want: true},
		{name: "server behind", observed: started.Add(-time.Second - time.Nanosecond)},
		{name: "server ahead", observed: finished.Add(time.Second + time.Nanosecond)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := registryClockWithinWindow(started, tt.observed, finished); got != tt.want {
				t.Fatalf("clock within tolerance = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestRegistryReadOnlyProbeRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		first    error
		cancel   bool
		want     error
		attempts int
	}{
		{name: "eof", first: fmt.Errorf("wrapped: %w", io.EOF), attempts: 2},
		{name: "reset", first: syscall.ECONNRESET, attempts: 2},
		{name: "server shutdown", first: &pgconn.PgError{Code: "57P01"}, attempts: 2},
		{name: "server crash", first: &pgconn.PgError{Code: "57P02"}, attempts: 2},
		{name: "server starting or stopping", first: &pgconn.PgError{Code: "57P03"}, attempts: 2},
		{name: "clock", first: errRegistryClock, want: errRegistryClock, attempts: 1},
		{name: "tls", first: errRegistryTrust, want: errRegistryTrust, attempts: 1},
		{name: "sql auth", first: &pgconn.PgError{Code: "28000"}, want: errRegistryTrust, attempts: 1},
		{name: "sql error", first: &pgconn.PgError{Code: "42P01"}, want: errRegistryUnavailable, attempts: 1},
		{name: "cancelled", first: io.EOF, cancel: true, want: errRegistryTransport, attempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			var first context.Context
			var calls int
			got := checkRegistry(parent, func(ctx context.Context) error {
				calls++
				if _, ok := ctx.Deadline(); !ok {
					t.Error("missing probe deadline")
				}
				if calls == 1 {
					first = ctx
					if tc.cancel {
						cancel()
					}
					return tc.first
				}
				if ctx != first {
					t.Error("retry did not share the initial budget")
				}
				return nil
			})
			if !errors.Is(got, tc.want) || calls != tc.attempts {
				t.Errorf("probe = %v in %d attempts, want %v in %d", got, calls, tc.want, tc.attempts)
			}
		})
	}
}

func TestRegistryProbeDoesNotLoop(t *testing.T) {
	calls := 0
	err := checkRegistry(t.Context(), func(context.Context) error {
		calls++
		return io.EOF
	})
	if !errors.Is(err, errRegistryTransport) || calls != 2 {
		t.Fatalf("probe = %v in %d attempts", err, calls)
	}
}
