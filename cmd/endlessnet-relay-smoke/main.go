package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/unng-lab/endlessnet-relay/internal/tlsconfig"
)

func main() {
	relayAddr := flag.String("relay-addr", "", "public Relay TLS address")
	relayServerName := flag.String("relay-server-name", "", "public Relay TLS server name")
	publicCAFile := flag.String("public-ca-file", "", "optional public Relay CA bundle")
	coordinatorHealthURL := flag.String("coordinator-health-url", "", "Relay Coordinator health URL")
	serviceCAFile := flag.String("service-ca-file", "", "service CA bundle")
	serviceCertFile := flag.String("service-cert-file", "", "service client certificate")
	serviceKeyFile := flag.String("service-key-file", "", "service client private key")
	coordinatorServerName := flag.String("coordinator-server-name", "", "Relay Coordinator TLS server name")
	timeout := flag.Duration("timeout", 10*time.Second, "smoke timeout")
	flag.Parse()

	if strings.TrimSpace(*relayAddr) == "" || strings.TrimSpace(*relayServerName) == "" || strings.TrimSpace(*coordinatorHealthURL) == "" {
		fail(errors.New("relay-addr, relay-server-name, and coordinator-health-url are required"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := checkRelayTLS(ctx, *relayAddr, *relayServerName, *publicCAFile); err != nil {
		fail(err)
	}
	clientTLS, err := tlsconfig.MutualClient(*serviceCertFile, *serviceKeyFile, *serviceCAFile, *coordinatorServerName, "spiffe://endlessnet.ru/service/relay-coordinator", "")
	if err != nil {
		fail(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}, Timeout: *timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *coordinatorHealthURL, nil)
	if err != nil {
		fail(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		fail(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("relay Coordinator health returned %s", resp.Status))
	}
	fmt.Println("relay smoke passed")
}

func checkRelayTLS(ctx context.Context, addr, serverName, caFile string) error {
	config := &tls.Config{ServerName: strings.TrimSpace(serverName), MinVersion: tls.VersionTLS13}
	if strings.TrimSpace(caFile) != "" {
		raw, err := os.ReadFile(strings.TrimSpace(caFile))
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(raw) {
			return errors.New("public Relay CA file contains no certificates")
		}
		config.RootCAs = roots
	}
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: config}
	connection, err := dialer.DialContext(ctx, "tcp", strings.TrimSpace(addr))
	if err != nil {
		return err
	}
	return connection.Close()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
