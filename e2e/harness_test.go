//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	spiffetls "github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
)

const (
	testNetworkID             = "e2e-network"
	relayCoordinatorHTTPSPort = 7078
	defaultPostgres           = "postgres:17-alpine@sha256:742f40ea20b9ff2ff31db5458d127452988a2164df9e17441e191f3b72252193"
)

var suite *harness

type harness struct {
	repoRoot     string
	composeFile  string
	composeBin   string
	composeArgs  []string
	project      string
	fixturesDir  string
	artifactsDir string
	keep         bool

	relayImage       string
	coordinatorImage string
	mockImage        string
	postgresImage    string
	bootIDs          map[string]string
	builtImages      []string

	signingKey   ed25519.PrivateKey
	publicRoots  *x509.CertPool
	serviceRoots *x509.CertPool
	certificates map[string]tls.Certificate
}

func TestMain(m *testing.M) {
	h, err := newHarness()
	if err != nil {
		fmt.Fprintln(os.Stderr, "prepare multi-relay E2E harness:", err)
		os.Exit(1)
	}
	suite = h
	code := 1
	if err := h.start(); err != nil {
		fmt.Fprintln(os.Stderr, "start multi-relay E2E harness:", err)
		h.captureDiagnostics()
	} else {
		code = m.Run()
		h.captureDiagnostics()
	}
	if h.keep {
		fmt.Fprintf(os.Stderr, "E2E_KEEP enabled; compose project %s and fixtures %s are preserved\n", h.project, h.fixturesDir)
	} else if err := h.down(); err != nil {
		fmt.Fprintln(os.Stderr, "tear down multi-relay E2E harness:", err)
		code = 1
	}
	if !h.keep {
		if err := h.removeBuiltImages(); err != nil {
			fmt.Fprintln(os.Stderr, "remove locally built E2E images:", err)
			code = 1
		}
		if err := os.RemoveAll(h.fixturesDir); err != nil {
			fmt.Fprintln(os.Stderr, "remove ephemeral E2E PKI:", err)
			code = 1
		}
	}
	if code == 0 && os.Getenv("E2E_ARTIFACT_DIR") == "" && !h.keep {
		_ = os.RemoveAll(h.artifactsDir)
	}
	os.Exit(code)
}

func newHarness() (*harness, error) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, errors.New("resolve E2E source path")
	}
	repoRoot := filepath.Dir(filepath.Dir(sourceFile))
	composeBin, composeArgs, err := resolveComposeCommand()
	if err != nil {
		return nil, err
	}
	fixturesDir, err := os.MkdirTemp("", "endlessnet-relay-e2e-fixtures-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(fixturesDir, 0o755); err != nil {
		_ = os.RemoveAll(fixturesDir)
		return nil, err
	}
	artifactsDir := strings.TrimSpace(os.Getenv("E2E_ARTIFACT_DIR"))
	if artifactsDir == "" {
		artifactsDir, err = os.MkdirTemp("", "endlessnet-relay-e2e-artifacts-")
	} else {
		artifactsDir, err = filepath.Abs(artifactsDir)
		if err == nil {
			err = os.MkdirAll(artifactsDir, 0o755)
		}
	}
	if err != nil {
		_ = os.RemoveAll(fixturesDir)
		return nil, err
	}
	randomSuffix, err := randomHex(6)
	if err != nil {
		_ = os.RemoveAll(fixturesDir)
		return nil, err
	}
	h := &harness{
		repoRoot:      repoRoot,
		composeFile:   filepath.Join(repoRoot, "e2e", "compose.yml"),
		composeBin:    composeBin,
		composeArgs:   composeArgs,
		project:       "endlessnet-relay-e2e-" + randomSuffix,
		fixturesDir:   fixturesDir,
		artifactsDir:  artifactsDir,
		keep:          os.Getenv("E2E_KEEP") == "1",
		postgresImage: envOr("E2E_POSTGRES_IMAGE", defaultPostgres),
		bootIDs: map[string]string{
			"relay-a": "boot-a-1",
			"relay-b": "boot-b-1",
			"relay-c": "boot-c-1",
		},
		certificates: make(map[string]tls.Certificate),
	}
	if err := h.generateFixtures(); err != nil {
		_ = os.RemoveAll(fixturesDir)
		return nil, err
	}
	return h, nil
}

func resolveComposeCommand() (string, []string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, dockerErr := exec.CommandContext(ctx, "docker", "compose", "version").CombinedOutput()
	if dockerErr == nil {
		return "docker", []string{"compose"}, nil
	}
	standalone, standaloneErr := exec.LookPath("docker-compose")
	if standaloneErr == nil {
		return standalone, nil, nil
	}
	return "", nil, fmt.Errorf(
		"resolve Docker Compose: docker compose: %w (%s); docker-compose: %v",
		dockerErr,
		strings.TrimSpace(string(output)),
		standaloneErr,
	)
}

func (h *harness) start() error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := h.prepareImages(ctx); err != nil {
		return err
	}
	if err := h.startSPIRE(ctx); err != nil {
		return err
	}
	if _, err := h.compose(ctx, "up", "-d", "postgres", "upstream", "fault-proxy"); err != nil {
		return err
	}
	if err := h.waitFor(ctx, "PostgreSQL readiness", func() error {
		_, err := h.compose(context.Background(), "exec", "-T", "postgres", "pg_isready", "-U", "relay", "-d", "relay")
		return err
	}); err != nil {
		return err
	}
	if err := h.waitForHTTPS(ctx, "upstream", 9447, "/healthz", "upstream", h.certificates["relay-coordinator"], http.StatusOK); err != nil {
		return err
	}
	if _, err := h.compose(ctx, "up", "-d", "relay-coordinator"); err != nil {
		return err
	}
	if err := h.waitForHTTPS(ctx, "relay-coordinator", relayCoordinatorHTTPSPort, "/readyz", "relay-coordinator", h.certificates["e2e-client"], http.StatusOK); err != nil {
		return err
	}
	if _, err := h.compose(ctx, "up", "-d", "relay-a", "relay-b", "relay-c"); err != nil {
		return err
	}
	for _, relayID := range []string{"relay-a", "relay-b", "relay-c"} {
		if err := h.waitForRelay(ctx, relayID); err != nil {
			return err
		}
	}
	return h.waitFor(ctx, "three active relay registrations", func() error {
		output, err := h.postgresQuery(context.Background(), "SELECT count(*) FROM relay_instances WHERE lease_expires_at > now()")
		if err != nil {
			return err
		}
		if strings.TrimSpace(output) != "3" {
			return fmt.Errorf("active relay count is %q", strings.TrimSpace(output))
		}
		return nil
	})
}

func (h *harness) prepareImages(ctx context.Context) error {
	h.relayImage = strings.TrimSpace(os.Getenv("E2E_RELAY_IMAGE"))
	if h.relayImage == "" {
		h.relayImage = h.project + "-relay:local"
		if _, err := h.docker(ctx, "build", "--tag", h.relayImage, "--file", "Dockerfile.relay", "."); err != nil {
			return err
		}
		h.builtImages = append(h.builtImages, h.relayImage)
	}
	h.coordinatorImage = strings.TrimSpace(os.Getenv("E2E_COORDINATOR_IMAGE"))
	if h.coordinatorImage == "" {
		h.coordinatorImage = h.project + "-coordinator:local"
		if _, err := h.docker(ctx, "build", "--tag", h.coordinatorImage, "--file", "Dockerfile.coordinator", "."); err != nil {
			return err
		}
		h.builtImages = append(h.builtImages, h.coordinatorImage)
	}
	h.mockImage = strings.TrimSpace(os.Getenv("E2E_UPSTREAM_IMAGE"))
	if h.mockImage != "" {
		return nil
	}
	h.mockImage = h.project + "-mock:local"
	_, err := h.docker(ctx, "build", "--tag", h.mockImage, "--file", "e2e/Dockerfile.mock", ".")
	if err == nil {
		h.builtImages = append(h.builtImages, h.mockImage)
	}
	return err
}

func (h *harness) removeBuiltImages() error {
	if len(h.builtImages) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	args := append([]string{"image", "rm"}, h.builtImages...)
	_, err := h.docker(ctx, args...)
	return err
}

func (h *harness) environment() []string {
	env := append([]string(nil), os.Environ()...)
	env = append(env,
		"E2E_RELAY_IMAGE="+h.relayImage,
		"E2E_COORDINATOR_IMAGE="+h.coordinatorImage,
		"E2E_POSTGRES_IMAGE="+h.postgresImage,
		"E2E_INTERNAL_MOCK_IMAGE="+h.mockImage,
		"E2E_INTERNAL_FIXTURES_DIR="+filepath.ToSlash(h.fixturesDir),
		"E2E_INTERNAL_RELAY_A_BOOT_ID="+h.bootIDs["relay-a"],
		"E2E_INTERNAL_RELAY_B_BOOT_ID="+h.bootIDs["relay-b"],
		"E2E_INTERNAL_RELAY_C_BOOT_ID="+h.bootIDs["relay-c"],
	)
	return env
}

func (h *harness) docker(ctx context.Context, args ...string) (string, error) {
	return h.command(ctx, "docker", args...)
}

func (h *harness) command(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.repoRoot
	cmd.Env = h.environment()
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if err != nil {
		return output.String(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, output.String())
	}
	return output.String(), nil
}

func (h *harness) compose(ctx context.Context, args ...string) (string, error) {
	prefix := append([]string(nil), h.composeArgs...)
	prefix = append(prefix, "--project-name", h.project, "--file", h.composeFile)
	return h.command(ctx, h.composeBin, append(prefix, args...)...)
}

func (h *harness) down() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := h.compose(ctx, "down", "--volumes", "--remove-orphans", "--timeout", "10")
	return err
}

func (h *harness) stopService(service string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if service == "relay-coordinator" {
		// The Coordinator outage assertion starts immediately after this
		// call. Avoid spending most of its timeout on a graceful Docker stop
		// while the Coordinator can still serve control-plane requests.
		_, err := h.compose(ctx, "kill", "-s", "SIGKILL", service)
		return err
	}
	_, err := h.compose(ctx, "stop", "--timeout", "5", service)
	return err
}

func (h *harness) startService(service string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := h.compose(ctx, "up", "-d", "--no-deps", service)
	return err
}

func (h *harness) recreateRelay(relayID, bootID string) error {
	if _, ok := h.bootIDs[relayID]; !ok {
		return fmt.Errorf("unknown relay %q", relayID)
	}
	h.bootIDs[relayID] = bootID
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := h.compose(ctx, "up", "-d", "--no-deps", "--force-recreate", relayID); err != nil {
		return err
	}
	return h.waitForRelay(ctx, relayID)
}

func (h *harness) waitForRelay(ctx context.Context, relayID string) error {
	if err := h.waitForHTTP(ctx, relayID, 9090, "/healthz", http.StatusOK); err != nil {
		return err
	}
	return h.waitFor(ctx, relayID+" public TLS", func() error {
		address, err := h.port(context.Background(), relayID, 9443)
		if err != nil {
			return err
		}
		connection, err := tls.Dial("tcp", address, &tls.Config{RootCAs: h.publicRoots, ServerName: relayID, MinVersion: tls.VersionTLS13})
		if err != nil {
			return err
		}
		return connection.Close()
	})
}

func (h *harness) waitForHTTP(ctx context.Context, service string, containerPort int, path string, status int) error {
	return h.waitFor(ctx, service+" "+path, func() error {
		address, err := h.port(context.Background(), service, containerPort)
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+address+path, nil)
		if err != nil {
			return err
		}
		response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode != status {
			return fmt.Errorf("status is %s", response.Status)
		}
		return nil
	})
}

func (h *harness) waitForHTTPS(ctx context.Context, service string, containerPort int, path, serverName string, certificate tls.Certificate, status int) error {
	return h.waitFor(ctx, service+" "+path, func() error {
		address, err := h.port(context.Background(), service, containerPort)
		if err != nil {
			return err
		}
		client := h.mutualHTTPClient(certificate, serverName)
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+address+path, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode != status {
			return fmt.Errorf("status is %s", response.Status)
		}
		return nil
	})
}

func (h *harness) waitFor(ctx context.Context, description string, check func() error) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		if err := check(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for %s: %w (last error: %v)", description, ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func (h *harness) port(ctx context.Context, service string, containerPort int) (string, error) {
	output, err := h.compose(ctx, "port", service, strconv.Itoa(containerPort))
	if err != nil {
		return "", err
	}
	address := strings.TrimSpace(output)
	if address == "" {
		return "", fmt.Errorf("%s port %d is not published", service, containerPort)
	}
	return address, nil
}

func (h *harness) postgresQuery(ctx context.Context, query string) (string, error) {
	return h.compose(ctx, "exec", "-T", "postgres", "psql", "-U", "relay", "-d", "relay", "-Atc", query)
}

func (h *harness) internalClientTLS(certificate tls.Certificate, serverName string) *tls.Config {
	path := "/relay/" + serverName
	if serverName == "relay-coordinator" {
		path = "/service/relay-coordinator"
	}
	if serverName == "upstream" {
		path = "/service/coordinator"
	}
	roots, err := os.ReadFile(filepath.Join(h.fixturesDir, "service-ca.crt"))
	if err != nil {
		panic(err)
	}
	td := spiffeid.RequireTrustDomainFromString(h.trustDomain())
	bundle, err := x509bundle.Parse(td, roots)
	if err != nil {
		panic(err)
	}
	cfg := spiffetls.TLSClientConfig(bundle, spiffetls.AuthorizeID(spiffeid.RequireFromString("spiffe://"+h.trustDomain()+path)))
	cfg.Certificates = []tls.Certificate{certificate}
	cfg.MinVersion = tls.VersionTLS13
	return cfg
}
func (h *harness) mutualHTTPClient(certificate tls.Certificate, serverName string) *http.Client {
	return &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: h.internalClientTLS(certificate, serverName)}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (h *harness) captureDiagnostics() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if output, err := h.compose(ctx, "ps", "--all"); err == nil {
		_ = os.WriteFile(filepath.Join(h.artifactsDir, "compose-ps.txt"), []byte(redactedDiagnostic(output)), 0o644)
	}
	if output, err := h.compose(ctx, "logs", "--no-color", "--timestamps"); err == nil {
		_ = os.WriteFile(filepath.Join(h.artifactsDir, "compose.log"), []byte(redactedDiagnostic(output)), 0o644)
	}
	for _, relayID := range []string{"relay-a", "relay-b", "relay-c"} {
		address, err := h.port(ctx, relayID, 9090)
		if err != nil {
			continue
		}
		response, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + address + "/metrics")
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		_ = os.WriteFile(filepath.Join(h.artifactsDir, relayID+"-metrics.txt"), raw, 0o644)
	}
	fmt.Fprintln(os.Stderr, "E2E diagnostics:", h.artifactsDir)
}

func (h *harness) generateFixtures() error {
	publicCA, publicKey, err := createCA("EndlessNet E2E public CA")
	if err != nil {
		return err
	}
	serviceCA, serviceKey, err := createCA("EndlessNet E2E service CA")
	if err != nil {
		return err
	}
	if err := writeCertificate(filepath.Join(h.fixturesDir, "public-ca.crt"), publicCA.Raw); err != nil {
		return err
	}
	if err := writeCertificate(filepath.Join(h.fixturesDir, "service-ca.crt"), serviceCA.Raw); err != nil {
		return err
	}
	h.publicRoots = x509.NewCertPool()
	h.publicRoots.AddCert(publicCA)
	h.serviceRoots = x509.NewCertPool()
	h.serviceRoots.AddCert(serviceCA)
	keyRaw, err := x509.MarshalECPrivateKey(serviceKey)
	if err != nil {
		return err
	}
	if err = writePEM(filepath.Join(h.fixturesDir, "service-ca.key"), "EC PRIVATE KEY", keyRaw); err != nil {
		return err
	}
	if err = h.writeSPIREConfig(); err != nil {
		return err
	}

	for _, relayID := range []string{"relay-a", "relay-b", "relay-c"} {
		if _, err := h.issueAndWrite(publicCA, publicKey, relayID+"-public", []string{relayID, "localhost"}, "", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}); err != nil {
			return err
		}
		certificate, err := h.issueAndWrite(serviceCA, serviceKey, relayID, []string{relayID, "localhost"}, "spiffe://"+h.trustDomain()+"/relay/"+relayID, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth})
		if err != nil {
			return err
		}
		h.certificates[relayID] = certificate
	}
	coordinatorCertificate, err := h.issueAndWrite(serviceCA, serviceKey, "relay-coordinator", []string{"relay-coordinator", "localhost"}, "spiffe://"+h.trustDomain()+"/service/relay-coordinator", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth})
	if err != nil {
		return err
	}
	h.certificates["relay-coordinator"] = coordinatorCertificate
	upstreamCertificate, err := h.issueAndWrite(serviceCA, serviceKey, "upstream", []string{"upstream", "localhost"}, "spiffe://"+h.trustDomain()+"/service/coordinator", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err != nil {
		return err
	}
	h.certificates["upstream"] = upstreamCertificate
	e2eCertificate, err := h.issueAndWrite(serviceCA, serviceKey, "e2e-client", []string{"e2e-client"}, "spiffe://"+h.trustDomain()+"/service/coordinator", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	if err != nil {
		return err
	}
	h.certificates["e2e-client"] = e2eCertificate

	publicSigningKey, privateSigningKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	h.signingKey = privateSigningKey
	bundle, err := protocolv1.NewSigningTrustBundle(base64.RawURLEncoding.EncodeToString(publicSigningKey))
	if err != nil {
		return err
	}
	nodes := []string{"node-a", "node-b", "node-c", "node-d"}
	type pair struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	pairs := make([]pair, 0, len(nodes)*(len(nodes)-1))
	for _, from := range nodes {
		for _, to := range nodes {
			if from != to {
				pairs = append(pairs, pair{From: from, To: to})
			}
		}
	}
	mockConfig := struct {
		NetworkID   string                        `json:"network_id"`
		Nodes       []string                      `json:"nodes"`
		PeerPairs   []pair                        `json:"peer_pairs"`
		TrustBundle protocolv1.SigningTrustBundle `json:"relay_trust_bundle"`
	}{testNetworkID, nodes, pairs, bundle}
	if err := writeJSON(filepath.Join(h.fixturesDir, "mock-config.json"), mockConfig); err != nil {
		return err
	}
	snapshot := protocolv1.EndpointSnapshot{Version: 1, Endpoints: []protocolv1.Endpoint{
		{ID: "relay-a", Addr: "relay-a:9443", Protocol: "relay-v1-tls", Region: "region-a", Priority: 10},
		{ID: "relay-b", Addr: "relay-b:9443", Protocol: "relay-v1-tls", Region: "region-b", Priority: 20},
		{ID: "relay-c", Addr: "relay-c:9443", Protocol: "relay-v1-tls", Region: "region-c", Priority: 30},
	}}
	return writeJSON(filepath.Join(h.fixturesDir, "endpoints.json"), snapshot)
}

func (h *harness) issueAndWrite(ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string, dnsNames []string, identity string, usages []x509.ExtKeyUsage) (tls.Certificate, error) {
	leaf, key, err := issueCertificate(ca, caKey, name, dnsNames, identity, usages)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPath := filepath.Join(h.fixturesDir, name+".crt")
	keyPath := filepath.Join(h.fixturesDir, name+".key")
	if err := writeCertificate(certPath, leaf.Raw); err != nil {
		return tls.Certificate{}, err
	}
	keyRaw, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyRaw); err != nil {
		return tls.Certificate{}, err
	}
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	certificate.Leaf = leaf
	return certificate, nil
}

func createCA(commonName string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	certificate, err := x509.ParseCertificate(raw)
	return certificate, key, err
}

func issueCertificate(ca *x509.Certificate, caKey *ecdsa.PrivateKey, commonName string, dnsNames []string, identity string, usages []x509.ExtKeyUsage) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().UTC().Add(-5 * time.Minute),
		NotAfter:     time.Now().UTC().Add(2 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  usages,
	}
	if identity != "" {
		parsed, err := url.Parse(identity)
		if err != nil {
			return nil, nil, err
		}
		template.URIs = []*url.URL{parsed}
	}
	raw, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	certificate, err := x509.ParseCertificate(raw)
	return certificate, key, err
}

func writeCertificate(path string, raw []byte) error {
	return writePEM(path, "CERTIFICATE", raw)
}

func writePEM(path, blockType string, raw []byte) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: raw}), 0o444)
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o444)
}

func randomHex(bytesCount int) (string, error) {
	raw := make([]byte, bytesCount)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	const alphabet = "0123456789abcdef"
	encoded := make([]byte, len(raw)*2)
	for i, value := range raw {
		encoded[i*2] = alphabet[value>>4]
		encoded[i*2+1] = alphabet[value&15]
	}
	return string(encoded), nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
