package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	relayv1 "github.com/unng-lab/endlessnet-relay/api/relay/v1"
	"github.com/unng-lab/endlessnet-relay/internal/mesh"
	"github.com/unng-lab/endlessnet-relay/internal/relay"
	"github.com/unng-lab/endlessnet-relay/internal/relaycontrol"
	"github.com/unng-lab/endlessnet-relay/internal/tlsconfig"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	publicAddr := flag.String("addr", env("ENDLESSNET_RELAY_ADDR", ":9443"), "public relay TLS listen address")
	publicCert := flag.String("tls-cert-file", os.Getenv("ENDLESSNET_RELAY_TLS_CERT_FILE"), "public relay TLS certificate")
	publicKey := flag.String("tls-key-file", os.Getenv("ENDLESSNET_RELAY_TLS_KEY_FILE"), "public relay TLS private key")
	metricsAddr := flag.String("metrics-addr", env("ENDLESSNET_RELAY_METRICS_ADDR", "127.0.0.1:9190"), "health and metrics listen address")
	relayID := flag.String("relay-id", os.Getenv("ENDLESSNET_RELAY_ID"), "stable relay instance id")
	bootID := flag.String("boot-id", os.Getenv("ENDLESSNET_RELAY_BOOT_ID"), "unique process boot id; generated when empty")
	meshAddr := flag.String("mesh-addr", os.Getenv("ENDLESSNET_RELAY_MESH_ADDR"), "advertised relay mesh address")
	meshListenAddr := flag.String("mesh-listen-addr", env("ENDLESSNET_RELAY_MESH_LISTEN_ADDR", ":9444"), "relay mesh listen address")
	coordinatorAddr := flag.String("relay-coordinator-addr", os.Getenv("ENDLESSNET_RELAY_COORDINATOR_ADDR"), "Relay Coordinator gRPC address")
	serviceTLSProvider := flag.String("service-tls-provider", env("ENDLESSNET_SERVICE_TLS_PROVIDER", tlsconfig.ProviderSPIFFE), "internal identity provider")
	workloadAPIAddr := flag.String("workload-api-addr", env("ENDLESSNET_SERVICE_TLS_WORKLOAD_API_ADDR", tlsconfig.DefaultWorkloadAPI), "SPIFFE Workload API address")
	serviceCA := flag.String("service-ca-file", os.Getenv("ENDLESSNET_SERVICE_CA_FILE"), "service CA bundle")
	serviceCert := flag.String("service-cert-file", os.Getenv("ENDLESSNET_SERVICE_CERT_FILE"), "relay service certificate")
	serviceKey := flag.String("service-key-file", os.Getenv("ENDLESSNET_SERVICE_KEY_FILE"), "relay service private key")
	coordinatorServerName := flag.String("relay-coordinator-server-name", os.Getenv("ENDLESSNET_RELAY_COORDINATOR_SERVER_NAME"), "Relay Coordinator TLS server name")
	bandwidthLimit := flag.Int("bandwidth-limit-bytes-per-second", 0, "per-session bandwidth limit; zero disables")
	maxConnections := flag.Int("max-connections", relay.DefaultMaxConnections, "maximum concurrent client connections")
	maxConcurrentAuth := flag.Int("max-concurrent-auth", relay.DefaultMaxConcurrentAuth, "maximum concurrent authentications")
	maxConnectionsPerSource := flag.Int("max-connections-per-source", relay.DefaultMaxConnectionsPerSource, "maximum concurrent connections per source")
	flag.Parse()

	if strings.TrimSpace(*relayID) == "" || strings.TrimSpace(*meshAddr) == "" || strings.TrimSpace(*coordinatorAddr) == "" {
		fatal(errors.New("relay-id, mesh-addr, and relay-coordinator-addr are required"))
	}
	if *bootID == "" {
		*bootID = randomID()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	publicTLS, err := tlsconfig.PublicServer(*publicCert, *publicKey)
	if err != nil {
		fatal(err)
	}
	relayIdentity := "spiffe://endlessnet.ru/relay/" + *relayID
	var clientTLS, meshServerTLS, meshClientTLS *tls.Config
	switch strings.TrimSpace(*serviceTLSProvider) {
	case tlsconfig.ProviderSPIFFE:
		runtime, runtimeErr := tlsconfig.NewWorkloadRuntime(ctx, *workloadAPIAddr, relayIdentity)
		if runtimeErr != nil {
			fatal(runtimeErr)
		}
		defer runtime.Close()
		clientTLS, err = runtime.ClientTLSConfig("spiffe://endlessnet.ru/service/relay-coordinator")
		if err == nil {
			meshServerTLS, err = runtime.ServerTLSConfig()
		}
		if err == nil {
			meshClientTLS, err = runtime.ClientTLSConfigForTrustDomain()
		}
	case tlsconfig.ProviderDevelopment:
		clientTLS, err = tlsconfig.MutualClient(*serviceCert, *serviceKey, *serviceCA, *coordinatorServerName, "spiffe://endlessnet.ru/service/relay-coordinator", relayIdentity)
		if err == nil {
			meshServerTLS, err = tlsconfig.MutualServer(*serviceCert, *serviceKey, *serviceCA)
		}
		if err == nil {
			meshClientTLS, err = tlsconfig.MutualClient(*serviceCert, *serviceKey, *serviceCA, "", "", relayIdentity)
		}
	default:
		err = fmt.Errorf("unsupported internal identity provider %q", *serviceTLSProvider)
	}
	if err != nil {
		fatal(err)
	}
	coordinatorConnection, err := relaycontrol.Dial(*coordinatorAddr, clientTLS)
	if err != nil {
		fatal(err)
	}
	defer coordinatorConnection.Close()

	metrics := relay.NewMetrics()
	admission, err := relay.NewAdmissionController(relay.AdmissionLimits{MaxConnections: *maxConnections, MaxConcurrentAuth: *maxConcurrentAuth, MaxConnectionsPerSource: *maxConnectionsPerSource}, metrics)
	if err != nil {
		fatal(err)
	}
	var server *relay.Server
	meshManager := mesh.NewManager(ctx, *relayID, *bootID, meshClientTLS, func(networkID, fromNodeID, toNodeID string, destinationEpoch int64, payload []byte) error {
		return server.DeliverRemote(networkID, fromNodeID, toNodeID, destinationEpoch, payload)
	})
	defer meshManager.Close()
	controlClient := &relaycontrol.Client{RelayID: *relayID, BootID: *bootID, MeshAddr: *meshAddr, Control: relayv1.NewRelayControlClient(coordinatorConnection), Peers: meshManager}
	server, err = relay.NewServer(relay.ServerConfig{Addr: *publicAddr, TLSConfig: publicTLS, Metrics: metrics, RelayID: *relayID, BandwidthLimitBytesPerSecond: *bandwidthLimit, AuthTimeout: relay.DefaultAuthTimeout, Admission: admission, Control: controlClient, Mesh: meshManager, TrustBundleProvider: controlClient.TrustBundle})
	if err != nil {
		fatal(err)
	}

	errCh := make(chan error, 4)
	go func() { errCh <- controlClient.Run(ctx) }()
	if err := waitForTrustBundle(ctx, controlClient, 10*time.Second); err != nil {
		fatal(err)
	}
	meshListener, err := net.Listen("tcp", *meshListenAddr)
	if err != nil {
		fatal(err)
	}
	meshGRPC := grpc.NewServer(grpc.Creds(credentials.NewTLS(meshServerTLS)), grpc.UnaryInterceptor(relayv1.RejectUnknownUnaryServerInterceptor), grpc.StreamInterceptor(relayv1.RejectUnknownStreamServerInterceptor))
	relayv1.RegisterRelayMeshServer(meshGRPC, meshManager)
	go func() { errCh <- meshGRPC.Serve(meshListener) }()
	go func() { errCh <- server.ListenAndServe(ctx) }()
	go func() {
		errCh <- serveMetrics(ctx, *metricsAddr, metrics, func() bool {
			bundle, bundleErr := controlClient.TrustBundle()
			return bundleErr == nil && bundle.Validate() == nil
		})
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, grpc.ErrServerStopped) {
			runErr = err
		}
	}
	stop()
	server.Fence()
	meshGRPC.GracefulStop()
	if runErr != nil {
		fatal(runErr)
	}
}

func waitForTrustBundle(ctx context.Context, client *relaycontrol.Client, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := client.TrustBundle(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("relay Coordinator did not provide a trust bundle")
		case <-ticker.C:
		}
	}
}

func serveMetrics(ctx context.Context, addr string, metrics *relay.Metrics, ready func() bool) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}` + "\n")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready == nil || !ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}` + "\n"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(metrics.Render())) })
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	return server.ListenAndServe()
}

func randomID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		fatal(fmt.Errorf("generate relay boot id: %w", err))
	}
	return hex.EncodeToString(raw)
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func fatal(err error) {
	slog.Error("relay stopped", "error", err)
	os.Exit(1)
}
