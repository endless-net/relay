//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (h *harness) trustDomain() string { return envOr("E2E_TRUST_DOMAIN", "endlessnet.ru") }

func (h *harness) writeSPIREConfig() error {
	server := fmt.Sprintf(`server {
 bind_address = "0.0.0.0"
 bind_port = "8081"
 socket_path = "/tmp/spire-server.sock"
 trust_domain = %q
 data_dir = "/tmp/spire-server"
 default_x509_svid_ttl = "60s"
 ca_ttl = "1h"
 log_level = "WARN"
}
plugins {
 DataStore "sql" { plugin_data {
 database_type = "sqlite3"
 connection_string = "/tmp/spire-server/store.sqlite3"
 }
 }
 KeyManager "memory" { plugin_data {} }
 NodeAttestor "join_token" { plugin_data {} }
 UpstreamAuthority "disk" { plugin_data {
 cert_file_path = "/fixtures/service-ca.crt"
 key_file_path = "/fixtures/service-ca.key"
 }
 }
}
`, h.trustDomain())
	if err := os.WriteFile(filepath.Join(h.fixturesDir, "spire-server.conf"), []byte(server), 0644); err != nil {
		return err
	}
	for _, name := range []string{"upstream", "relay-coordinator", "relay-a", "relay-b", "relay-c"} {
		config := fmt.Sprintf(`agent {
 trust_domain = %q
 data_dir = "/tmp/spire-agent"
 server_address = "spire-server"
 server_port = "8081"
 trust_bundle_path = "/fixtures/service-ca.crt"
 socket_path = "/workload/agent.sock"
 join_token_file = "/fixtures/agent-%s.token"
 log_level = "WARN"
}
plugins {
 KeyManager "memory" { plugin_data {} }
 NodeAttestor "join_token" { plugin_data {} }
 WorkloadAttestor "unix" { plugin_data {} }
}
`, h.trustDomain(), name)
		if err := os.WriteFile(filepath.Join(h.fixturesDir, "agent-"+name+".conf"), []byte(config), 0644); err != nil {
			return err
		}
	}
	return nil
}

func (h *harness) startSPIRE(ctx context.Context) error {
	if _, err := h.compose(ctx, "up", "-d", "spire-server"); err != nil {
		return err
	}
	command := func(args ...string) (string, error) {
		prefix := []string{"exec", "-T", "spire-server", "/opt/spire/bin/spire-server"}
		args = append(args, "-socketPath", "/tmp/spire-server.sock")
		return h.compose(ctx, append(prefix, args...)...)
	}
	if err := h.waitFor(ctx, "SPIRE server", func() error { _, err := command("entry", "show"); return err }); err != nil {
		return err
	}
	for _, name := range []string{"upstream", "relay-coordinator", "relay-a", "relay-b", "relay-c"} {
		agentID := "spiffe://" + h.trustDomain() + "/spire/agent/" + name
		raw, err := command("token", "generate", "-spiffeID", agentID, "-output", "json")
		if err != nil {
			return fmt.Errorf("generate ephemeral agent token: command failed")
		}
		var token struct {
			Value string `json:"value"`
		}
		if err = json.Unmarshal([]byte(raw), &token); err != nil {
			return fmt.Errorf("decode ephemeral token response: %w", err)
		}
		if token.Value == "" {
			return fmt.Errorf("SPIRE returned empty join token")
		}
		if err = os.WriteFile(filepath.Join(h.fixturesDir, "agent-"+name+".token"), []byte(token.Value), 0600); err != nil {
			return err
		}
		path := "/relay/" + name
		if name == "upstream" {
			path = "/service/coordinator"
		}
		if name == "relay-coordinator" {
			path = "/service/relay-coordinator"
		}
		if _, err = command("entry", "create", "-parentID", agentID, "-spiffeID", "spiffe://"+h.trustDomain()+path, "-selector", "unix:uid:65532", "-ttl", "60"); err != nil {
			return err
		}
		if _, err = h.compose(ctx, "up", "-d", "agent-"+name); err != nil {
			return err
		}
	}
	return nil
}

// A fixture's private key is never included in diagnostics or error messages.
func redactedDiagnostic(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.Contains(strings.ToLower(line), "token") || strings.Contains(line, "PRIVATE KEY") {
			lines[i] = "[redacted sensitive diagnostic]"
		}
	}
	return strings.Join(lines, "\n")
}
