package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	upstream "github.com/endless-net/coordinator/api/relay/v1/upstreamv1connect"
	"github.com/endless-net/relay/internal/authz"
	"github.com/endless-net/relay/internal/budget"
	pb "github.com/endless-net/relay/internal/grpcwire"
	"github.com/endless-net/relay/internal/mesh"
	"github.com/endless-net/relay/internal/relay"
	"github.com/endless-net/relay/internal/relaycontrol"
	"github.com/endless-net/relay/internal/relaycoordinator"
	"github.com/endless-net/relay/internal/store"
	"github.com/endless-net/relay/internal/tlsconfig"
	"go.uber.org/fx"
	"google.golang.org/grpc"
)

type Runtime struct {
	c          Config
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	ready      atomic.Bool
	failed     atomic.Bool
	http       *http.Server
	listeners  []net.Listener
	grpc       *grpc.Server
	workload   *tlsconfig.WorkloadRuntime
	db         *store.Postgres
	control    *relaycontrol.Client
	client     *grpc.ClientConn
	mesh       *mesh.Manager
	data       *relay.Server
	httpClient *http.Client
	stats      *relay.Metrics
	telemetry  *telemetry
	fatal      func()
}

func App(c Config) *fx.App {
	return fx.New(fx.NopLogger, fx.StartTimeout(5*time.Second), fx.StopTimeout(10*time.Second), fx.Invoke(func(lc fx.Lifecycle, stop fx.Shutdowner) {
		r := &Runtime{c: c, fatal: func() { _ = stop.Shutdown(fx.ExitCode(1)) }}
		lc.Append(fx.Hook{OnStart: r.Start, OnStop: r.Stop})
	}))
}

func (r *Runtime) Start(ctx context.Context) (result error) {
	if e := r.c.Validate(); e != nil {
		return e
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.stats = relay.NewMetrics()
	defer func() {
		if result != nil {
			cleanup, c := context.WithTimeout(context.Background(), 2*time.Second)
			defer c()
			_ = r.Stop(cleanup)
		}
	}()
	var e error
	r.telemetry, e = newTelemetry(ctx, r.c.OTel)
	if e != nil {
		return errors.New("telemetry_start")
	}
	identity := "spiffe://endlessnet.ru/service/relay-coordinator"
	if r.c.Role == "relay" {
		identity = "spiffe://endlessnet.ru/service/relay/" + r.c.ID
	}
	r.workload, e = tlsconfig.NewWorkloadRuntime(ctx, r.c.WorkloadAPI, identity)
	if e != nil {
		return errors.New("workload_unavailable")
	}
	serverTLS, e := r.workload.ServerTLSConfig()
	if e != nil {
		return errors.New("workload_tls")
	}
	if r.c.Role == "relay-coordinator" {
		databaseTLS, tlsErr := r.workload.ClientTLSConfig(r.c.DatabaseIdentity)
		if tlsErr != nil {
			return errors.New("database_tls")
		}
		r.db, e = store.OpenPostgres(ctx, r.c.DSN, databaseTLS)
		if e != nil {
			return errors.New("registry_unavailable")
		}
		upstreamTLS, e := r.workload.ClientTLSConfig("spiffe://endlessnet.ru/service/coordinator")
		if e != nil {
			return errors.New("upstream_tls")
		}
		transport := &http.Transport{TLSClientConfig: upstreamTLS, DialTLSContext: tlsconfig.HTTPDialTLS(upstreamTLS), ForceAttemptHTTP2: true, MaxConnsPerHost: 128, MaxIdleConnsPerHost: 16, IdleConnTimeout: 10 * time.Second, TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: 2 * time.Second}
		r.httpClient = &http.Client{Transport: traceTransport{transport}, Timeout: 2 * time.Second}
		a := authz.GRPCAuthorizer{Client: upstream.NewRelayUpstreamServiceClient(r.httpClient, r.c.Upstream, connect.WithGRPC(), connect.WithReadMaxBytes(147456))}
		if _, e = a.RelayTrustBundle(ctx); e != nil {
			return errors.New("trust_unavailable")
		}
		r.grpc = grpc.NewServer(grpc.Creds(tlsconfig.GRPCCredentials(serverTLS)), grpc.ConnectionTimeout(5*time.Second), grpc.MaxRecvMsgSize(147456), grpc.MaxConcurrentStreams(128), grpc.ChainUnaryInterceptor(r.unary, pb.RejectUnknownUnaryServerInterceptor))
		pb.RegisterRelayControlServer(r.grpc, &relaycoordinator.Server{Store: r.db, Authorizer: a, InstanceTTL: 30 * time.Second, SessionTTL: 15 * time.Second})
		listener, e := r.listen(ctx, r.c.ControlListen, 64)
		if e != nil {
			return e
		}
		r.worker("listener_failed", func() error { return r.grpc.Serve(listener) })
		r.worker("fatal", func() error {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-r.ctx.Done():
					return nil
				case <-ticker.C:
					if e := r.db.Ping(r.ctx); e != nil {
						return errors.New("registry_unavailable")
					}
					if _, e := a.RelayTrustBundle(r.ctx); e != nil {
						return errors.New("trust_unavailable")
					}
				}
			}
		})
	} else {
		targetTLS, e := r.workload.ClientTLSConfig("spiffe://endlessnet.ru/service/relay-coordinator")
		if e != nil {
			return errors.New("control_tls")
		}
		r.client, e = relaycontrol.Dial(r.c.Coordinator, targetTLS)
		if e != nil {
			return errors.New("control_dial")
		}
		raw := make([]byte, 16)
		if _, e := rand.Read(raw); e != nil {
			return errors.New("boot_id")
		}
		boot := hex.EncodeToString(raw)
		peerTLS, e := r.workload.ClientTLSConfigForTrustDomain()
		if e != nil {
			return errors.New("mesh_tls")
		}
		publicTLS, e := tlsconfig.PublicServer(r.c.Certificate, r.c.Key)
		if e != nil {
			return errors.New("public_tls")
		}
		r.control = &relaycontrol.Client{RelayID: r.c.ID, BootID: boot, MeshAddr: r.c.MeshAddress, Control: pb.NewRelayControlClient(r.client)}
		r.data = &relay.Server{Addr: r.c.Listen, TLSConfig: publicTLS, RelayID: r.c.ID, TrustBundleProvider: r.control.TrustBundle, Control: r.control, Metrics: r.stats, AuthTimeout: 5 * time.Second, QueueBudget: &budget.Bytes{Limit: 64 << 20}}
		r.mesh = mesh.NewManager(r.ctx, r.c.ID, boot, peerTLS, r.data.DeliverRemote)
		r.mesh.QueueBudget = r.data.QueueBudget
		r.data.Mesh = r.mesh
		r.control.Peers = r.mesh
		r.grpc = grpc.NewServer(grpc.Creds(tlsconfig.GRPCCredentials(serverTLS)), grpc.ConnectionTimeout(5*time.Second), grpc.MaxRecvMsgSize(147456), grpc.MaxConcurrentStreams(1), grpc.StreamInterceptor(pb.RejectUnknownStreamServerInterceptor))
		pb.RegisterRelayMeshServer(r.grpc, r.mesh)
		listener, e := r.listen(ctx, r.c.MeshListen, 64)
		if e != nil {
			return e
		}
		r.worker("listener_failed", func() error { return r.grpc.Serve(listener) })
		r.worker("control_failed", func() error { return r.control.Run(r.ctx) })
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for !r.control.Ready() {
			select {
			case <-ctx.Done():
				return errors.New("control_start_timeout")
			case <-ticker.C:
				if r.failed.Load() {
					return errors.New("control_start")
				}
			}
		}
		r.worker("dataplane_failed", func() error { return r.data.ListenAndServe(r.ctx) })
		for !r.data.Ready() {
			select {
			case <-ctx.Done():
				return errors.New("dataplane_start_timeout")
			case <-ticker.C:
				if r.failed.Load() {
					return errors.New("dataplane_start")
				}
			}
		}
	}
	r.worker("fatal", func() error {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.ctx.Done():
				return nil
			case <-ticker.C:
				if !r.workload.Healthy() {
					return errors.New("workload_expired")
				}
			}
		}
	})
	listener, e := r.listen(ctx, r.c.HTTP, 64)
	if e != nil {
		return e
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if r.failed.Load() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !r.Ready() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		if r.mesh != nil {
			_, _ = w.Write([]byte(r.mesh.Render()))
		}
		_, _ = w.Write([]byte(r.stats.Render() + fmt.Sprintf("endlessnet_relay_telemetry_dropped_total %d\nendlessnet_relay_export_failed_total %d\n", r.telemetry.dropped.Load(), r.telemetry.exportFailed.Load())))
	})
	r.http = &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
	r.worker("listener_failed", func() error { return r.http.Serve(listener) })
	r.ready.Store(true)
	r.telemetry.log("started", "")
	return nil
}

func (r *Runtime) Ready() bool {
	if !r.ready.Load() || r.failed.Load() || r.workload == nil || !r.workload.Healthy() {
		return false
	}
	if r.control != nil {
		if !r.control.Ready() {
			return false
		}
		if _, e := r.control.TrustBundle(); e != nil {
			return false
		}
	}
	return true
}
func (r *Runtime) worker(reason string, f func() error) {
	r.wg.Go(func() {
		if e := f(); e != nil && r.ctx.Err() == nil {
			r.failed.Store(true)
			r.ready.Store(false)
			if r.data != nil {
				r.data.Fence()
			}
			switch e.Error() {
			case "registry_unavailable", "trust_unavailable", "workload_expired":
				reason = e.Error()
			}
			r.telemetry.log(reason, "")
			r.cancel()
			if r.fatal != nil {
				r.fatal()
			}
		}
	})
}
func (r *Runtime) Stop(ctx context.Context) error {
	r.ready.Store(false)
	if r.cancel != nil {
		r.cancel()
	}
	if r.data != nil {
		r.data.Fence()
	}
	if r.mesh != nil {
		r.mesh.Close()
	}
	if r.grpc != nil {
		r.grpc.Stop()
	}
	if r.http != nil {
		_ = r.http.Close()
	}
	for _, l := range r.listeners {
		_ = l.Close()
	}
	if r.client != nil {
		_ = r.client.Close()
	}
	if r.httpClient != nil {
		r.httpClient.CloseIdleConnections()
	}
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return errors.New("shutdown_timeout")
	}
	if r.db != nil {
		r.db.Close()
	}
	if r.workload != nil {
		_ = r.workload.Close()
	}
	if r.telemetry != nil {
		r.telemetry.log("stopped", "")
		return r.telemetry.close(ctx)
	}
	return nil
}
func Run(role string, args []string) int {
	c, e := Parse(role, args)
	if e != nil {
		_, _ = os.Stderr.WriteString("configuration_invalid\n")
		return 2
	}
	if c.Version {
		_, _ = os.Stdout.WriteString("relay runtime009\n")
		return 0
	}
	if c.Check {
		_, _ = os.Stdout.WriteString("configuration_valid\n")
		return 0
	}
	if c.Migrate {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		runtime, e := tlsconfig.NewWorkloadRuntime(ctx, c.WorkloadAPI, "spiffe://endlessnet.ru/service/relay-coordinator")
		if e != nil {
			_, _ = os.Stderr.WriteString("migration_identity_failed\n")
			return 1
		}
		defer func() { _ = runtime.Close() }()
		transport, e := runtime.ClientTLSConfig(c.DatabaseIdentity)
		if e != nil {
			return 1
		}
		if store.Migrate(ctx, c.DSN, transport) != nil {
			_, _ = os.Stderr.WriteString("migration_failed\n")
			return 1
		}
		return 0
	}
	a := App(c)
	start, cancelStart := context.WithTimeout(context.Background(), 5*time.Second)
	e = a.Start(start)
	cancelStart()
	if e != nil {
		_, _ = os.Stderr.WriteString("startup_failed\n")
		clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.Stop(clean)
		return 1
	}
	signal := <-a.Wait()
	stop, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStop()
	if e = a.Stop(stop); e != nil {
		_, _ = os.Stderr.WriteString("shutdown_failed\n")
		return 1
	}
	return signal.ExitCode
}
