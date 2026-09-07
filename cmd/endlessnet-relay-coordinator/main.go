package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	relayv1 "github.com/endless-net/relay/api/relay/v1"
	"github.com/endless-net/relay/internal/authz"
	"github.com/endless-net/relay/internal/relaycoordinator"
	"github.com/endless-net/relay/internal/store"
	"github.com/endless-net/relay/internal/tlsconfig"
	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	grpcAddr := flag.String("grpc-addr", env("ENDLESSNET_RELAY_COORDINATOR_GRPC_ADDR", ":9445"), "Relay Coordinator gRPC listen address")
	httpAddr := flag.String("http-addr", env("ENDLESSNET_RELAY_COORDINATOR_HTTP_ADDR", ":7078"), "Relay Coordinator HTTPS listen address")
	metricsAddr := flag.String("metrics-addr", env("ENDLESSNET_RELAY_COORDINATOR_METRICS_ADDR", "127.0.0.1:9191"), "loopback health listen address")
	dsn := flag.String("postgres-dsn", os.Getenv("ENDLESSNET_RELAY_POSTGRES_DSN"), "dedicated Relay Coordinator PostgreSQL DSN")
	mainCoordinatorURL := flag.String("coordinator-url", os.Getenv("ENDLESSNET_COORDINATOR_URL"), "main EndlessNet Coordinator URL")
	endpointsFile := flag.String("endpoints-file", os.Getenv("ENDLESSNET_RELAY_ENDPOINTS_FILE"), "versioned platform endpoint snapshot JSON")
	serviceTLSProvider := flag.String("service-tls-provider", env("ENDLESSNET_SERVICE_TLS_PROVIDER", tlsconfig.ProviderSPIFFE), "internal identity provider")
	workloadAPIAddr := flag.String("workload-api-addr", env("ENDLESSNET_SERVICE_TLS_WORKLOAD_API_ADDR", tlsconfig.DefaultWorkloadAPI), "SPIFFE Workload API address")
	serviceCA := flag.String("service-ca-file", os.Getenv("ENDLESSNET_SERVICE_CA_FILE"), "service CA bundle")
	serviceCert := flag.String("service-cert-file", os.Getenv("ENDLESSNET_SERVICE_CERT_FILE"), "Relay Coordinator service certificate")
	serviceKey := flag.String("service-key-file", os.Getenv("ENDLESSNET_SERVICE_KEY_FILE"), "Relay Coordinator service private key")
	mainServerName := flag.String("coordinator-server-name", os.Getenv("ENDLESSNET_COORDINATOR_SERVER_NAME"), "main Coordinator TLS server name")
	trustDomain := flag.String("trust-domain", env("ENDLESSNET_RELAY_TRUST_DOMAIN", tlsconfig.DefaultTrustDomain), "operator SPIFFE trust domain")
	coordinatorIdentity := flag.String("coordinator-identity", os.Getenv("ENDLESSNET_RELAY_COORDINATOR_IDENTITY"), "exact Relay Coordinator SPIFFE identity; default derived from trust domain")
	upstreamIdentity := flag.String("upstream-identity", os.Getenv("ENDLESSNET_RELAY_UPSTREAM_IDENTITY"), "exact upstream SPIFFE identity; default derived from trust domain")
	flag.Parse()
	policy, policyErr := tlsconfig.NewIdentityPolicy(*trustDomain, *coordinatorIdentity, *upstreamIdentity)
	if policyErr != nil {
		fatal(policyErr)
	}

	if strings.TrimSpace(*dsn) == "" || strings.TrimSpace(*mainCoordinatorURL) == "" || strings.TrimSpace(*endpointsFile) == "" {
		fatal(errors.New("postgres-dsn, coordinator-url, and endpoints-file are required"))
	}
	if err := validateUpstreamURL(*mainCoordinatorURL); err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var serverTLS, clientTLS *tls.Config
	var err error
	switch strings.TrimSpace(*serviceTLSProvider) {
	case tlsconfig.ProviderSPIFFE:
		runtime, runtimeErr := tlsconfig.NewWorkloadRuntime(ctx, *workloadAPIAddr, policy.CoordinatorID)
		if runtimeErr != nil {
			fatal(runtimeErr)
		}
		defer runtime.Close()
		serverTLS, err = runtime.ServerTLSConfig()
		if err == nil {
			clientTLS, err = runtime.ClientTLSConfig(policy.UpstreamID)
		}
	case tlsconfig.ProviderDevelopment:
		serverTLS, err = tlsconfig.MutualServer(*serviceCert, *serviceKey, *serviceCA)
		if err == nil {
			clientTLS, err = tlsconfig.MutualClient(*serviceCert, *serviceKey, *serviceCA, *mainServerName, policy.UpstreamID, policy.CoordinatorID)
		}
	default:
		err = errors.New("unsupported internal identity provider")
	}
	if err != nil {
		fatal(err)
	}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: clientTLS}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	storage, err := store.OpenPostgres(ctx, *dsn)
	if err != nil {
		fatal(err)
	}
	defer storage.Close()
	snapshot, err := loadSnapshot(*endpointsFile)
	if err != nil {
		fatal(err)
	}
	if err := storage.ReplaceEndpoints(ctx, snapshot, time.Now().UTC()); err != nil {
		fatal(err)
	}
	upstream := authz.HTTPAuthorizer{BaseURL: *mainCoordinatorURL, HTTPClient: httpClient}
	coordinator := &relaycoordinator.Server{IdentityPolicy: policy, Store: storage, Authorizer: authz.NewCache(upstream)}

	grpcListener, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		fatal(err)
	}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.UnaryInterceptor(relayv1.RejectUnknownUnaryServerInterceptor), grpc.StreamInterceptor(relayv1.RejectUnknownStreamServerInterceptor))
	relayv1.RegisterRelayControlServer(grpcServer, coordinator)
	httpListener, err := tls.Listen("tcp", *httpAddr, serverTLS.Clone())
	if err != nil {
		fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/internal/relay-control/v1/endpoints", coordinator.EndpointHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}` + "\n")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		snapshot, err := storage.EndpointSnapshot(r.Context())
		if err != nil || snapshot.Validate() != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}` + "\n"))
	})
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, TLSConfig: serverTLS.Clone()}
	errCh := make(chan error, 3)
	go func() { errCh <- grpcServer.Serve(grpcListener) }()
	go func() { errCh <- httpServer.Serve(httpListener) }()
	go func() { errCh <- serveStatus(ctx, *metricsAddr, storage) }()
	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, grpc.ErrServerStopped) {
			runErr = err
		}
	}
	stop()
	stopped := make(chan struct{})
	go func() { grpcServer.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		grpcServer.Stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if runErr != nil {
		fatal(runErr)
	}
}

func serveStatus(ctx context.Context, addr string, storage store.Store) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, request *http.Request) {
		snapshot, err := storage.EndpointSnapshot(request.Context())
		if err != nil || snapshot.Validate() != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}` + "\n"))
	})
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	return server.ListenAndServe()
}

func loadSnapshot(path string) (protocolv1.EndpointSnapshot, error) {
	raw, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return protocolv1.EndpointSnapshot{}, err
	}
	var snapshot protocolv1.EndpointSnapshot
	if err := protocolv1.DecodeStrict(raw, &snapshot); err != nil {
		return protocolv1.EndpointSnapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return protocolv1.EndpointSnapshot{}, err
	}
	return snapshot, nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func fatal(err error) {
	slog.Error("relay Coordinator stopped", "error", err)
	os.Exit(1)
}

// Validate before opening storage or listeners: a configured HTTP URL must never
// bypass the SPIFFE TLS transport when credentials leave this process.
func validateUpstreamURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || raw != strings.TrimSpace(raw) || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("coordinator-url must be an HTTPS origin without credentials, query, fragment or path prefix")
	}
	return nil
}
