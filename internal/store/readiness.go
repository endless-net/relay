package store

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	errRegistryClock       = errors.New("registry_clock_untrusted")
	errRegistryTrust       = errors.New("registry_trust_untrusted")
	errRegistryTransport   = errors.New("registry_transport_unavailable")
	errRegistryUnavailable = errors.New("registry_unavailable")
)

func registryClockWithinWindow(started, observed, finished time.Time) bool {
	return !observed.Before(started.Add(-time.Second)) && !observed.After(finished.Add(time.Second))
}

func checkRegistry(ctx context.Context, probe func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result := registryProbeError(probe(ctx))
	if result == errRegistryTransport && ctx.Err() == nil {
		result = registryProbeError(probe(ctx))
	}
	return result
}

// Only connection failures qualify for replay of the read-only probe.
func registryProbeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errRegistryClock) {
		return errRegistryClock
	}
	var tlsAlert tls.AlertError
	if errors.Is(err, errRegistryTrust) || errors.As(err, &tlsAlert) {
		return errRegistryTrust
	}
	var sqlError *pgconn.PgError
	if errors.As(err, &sqlError) {
		if sqlError.Code == "57P01" || sqlError.Code == "57P02" || sqlError.Code == "57P03" {
			return errRegistryTransport
		}
		if strings.HasPrefix(sqlError.Code, "28") {
			return errRegistryTrust
		}
		return errRegistryUnavailable
	}
	var networkError net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, pgconn.ErrConnClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) || (errors.As(err, &networkError) && networkError.Timeout()) {
		return errRegistryTransport
	}
	return errRegistryUnavailable
}
