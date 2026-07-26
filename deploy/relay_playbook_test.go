package deploy_test

import (
	"os"
	"strings"
	"testing"
)

func TestRelayToolVersionCommands(t *testing.T) {
	playbook, err := os.ReadFile("relay.yml")
	if err != nil {
		t.Fatal(err)
	}

	contents := string(playbook)
	for _, command := range []string{
		"argv: [certbot, --version]",
		"argv: [openssl, version]",
	} {
		if !strings.Contains(contents, command) {
			t.Errorf("relay.yml does not contain %q", command)
		}
	}

	if strings.Contains(contents, "argv: [openssl, --version]") {
		t.Error("relay.yml uses unsupported openssl --version syntax")
	}
}

func TestRelayInstanceConfigUsesHostWireGuardAddress(t *testing.T) {
	playbook, err := os.ReadFile("relay.yml")
	if err != nil {
		t.Fatal(err)
	}

	contents := string(playbook)
	for _, setting := range []string{
		"ENDLESSNET_RELAY_MESH_ADDR={{ wg_address }}:9444",
		"ENDLESSNET_RELAY_MESH_LISTEN_ADDR={{ wg_address }}:9444",
	} {
		if !strings.Contains(contents, setting) {
			t.Errorf("host-specific Relay configuration does not contain %q", setting)
		}
	}
}
