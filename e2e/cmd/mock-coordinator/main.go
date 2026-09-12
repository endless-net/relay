//go:build e2e

package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/endless-net/relay/internal/testserver"
	"github.com/endless-net/relay/internal/tlsconfig"
)

func main() {
	addr := flag.String("addr", ":9447", "HTTPS listen address")
	configFile := flag.String("config-file", "", "strict mock configuration")
	workloadAPI := flag.String("workload-api-addr", "", "ephemeral SPIRE Workload API")
	domain := flag.String("trust-domain", tlsconfig.DefaultTrustDomain, "test trust domain")
	flag.Parse()
	policy, err := tlsconfig.NewIdentityPolicy(*domain, "", "")
	if err != nil {
		log.Fatal(err)
	}

	cfg, err := testserver.LoadConfig(*configFile)
	if err != nil {
		log.Fatal(err)
	}
	runtime, err := tlsconfig.NewWorkloadRuntime(context.Background(), *workloadAPI, policy.UpstreamID)
	if err != nil {
		log.Fatal(err)
	}
	defer runtime.Close()
	tlsConfig, err := runtime.ServerTLSConfig()
	if err != nil {
		log.Fatal(err)
	}
	s := testserver.New(cfg, policy)
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           s.Handler(),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 5 * time.Second,
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("mock coordinator listening on %s", *addr)
	if err := httpServer.ServeTLS(listener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
