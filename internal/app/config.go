package app

import (
	"errors"
	"flag"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	DatabaseIdentity        string `env:"ENDLESSNET_RELAY_DATABASE_IDENTITY"`
	Role                    string
	Listen                  string `env:"ENDLESSNET_RELAY_LISTEN" envDefault:":9443"`
	MeshListen              string `env:"ENDLESSNET_RELAY_MESH_LISTEN" envDefault:":9444"`
	MeshAddress             string `env:"ENDLESSNET_RELAY_MESH_ADDRESS"`
	ControlListen           string `env:"ENDLESSNET_RELAY_CONTROL_LISTEN" envDefault:":9445"`
	HTTP                    string `env:"ENDLESSNET_RELAY_HTTP_LISTEN" envDefault:"127.0.0.1:9190"`
	ID                      string `env:"ENDLESSNET_RELAY_ID"`
	Coordinator             string `env:"ENDLESSNET_RELAY_COORDINATOR_ADDRESS"`
	Upstream                string `env:"ENDLESSNET_RELAY_UPSTREAM_URL"`
	WorkloadAPI             string `env:"ENDLESSNET_RELAY_WORKLOAD_API" envDefault:"unix:///run/spire/sockets/agent.sock"`
	Certificate             string `env:"ENDLESSNET_RELAY_TLS_CERT"`
	Key                     string `env:"ENDLESSNET_RELAY_TLS_KEY"`
	DSN                     string `env:"ENDLESSNET_RELAY_DATABASE_URL"`
	OTel                    string `env:"ENDLESSNET_RELAY_OTEL_ENDPOINT"`
	Check, Version, Migrate bool
}

func Parse(role string, args []string) (Config, error) {
	c := Config{Role: role}
	if e := env.Parse(&c); e != nil {
		return c, errors.New("config_environment")
	}
	f := flag.NewFlagSet(role, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.BoolVar(&c.Check, "check-config", false, "")
	f.BoolVar(&c.Version, "version", false, "")
	f.BoolVar(&c.Migrate, "migrate", false, "")
	if e := f.Parse(args); e != nil || f.NArg() != 0 {
		return c, errors.New("config_flags")
	}
	if c.Version {
		return c, nil
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.Role != "relay" && c.Role != "relay-coordinator" {
		return errors.New("config_role")
	}
	host, port, e := net.SplitHostPort(c.HTTP)
	if e != nil || port == "0" || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("config_diagnostics")
	}
	if c.OTel != "" {
		u, e := url.Parse(c.OTel)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("config_otel")
		}
	}
	if c.Role == "relay-coordinator" {
		id, e := spiffeid.FromString(c.DatabaseIdentity)
		if e != nil || id.TrustDomain().Name() != "endlessnet.ru" || !strings.HasPrefix(id.Path(), "/service/") {
			return errors.New("config_database_identity")
		}
	}
	if c.Migrate {
		if c.Role != "relay-coordinator" || c.DSN == "" {
			return errors.New("config_migration")
		}
		return nil
	}
	if !strings.HasPrefix(c.WorkloadAPI, "unix:///") {
		return errors.New("config_workload_api")
	}
	addresses := []string{c.HTTP}
	if c.Role == "relay" {
		if c.ID == "" || c.ID != strings.TrimSpace(c.ID) || strings.ContainsAny(c.ID, " /?#%\x00\r\n\t") || c.MeshAddress == "" || c.Coordinator == "" || c.Certificate == "" || c.Key == "" {
			return errors.New("config_dataplane")
		}
		addresses = append(addresses, c.Listen, c.MeshListen)
		if !numericPort(c.MeshAddress) {
			return errors.New("config_mesh_address")
		}
		if !numericPort(c.Coordinator) {
			return errors.New("config_coordinator_address")
		}
	} else {
		u, e := url.Parse(c.Upstream)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || c.DSN == "" {
			return errors.New("config_upstream")
		}
		addresses = append(addresses, c.ControlListen)
	}
	seen := map[string]bool{}
	for _, a := range addresses {
		_, port, e := net.SplitHostPort(a)
		if e != nil || port == "" || !numericPort(a) || seen[a] {
			return errors.New("config_listener")
		}
		seen[a] = true
	}
	return nil
}

func numericPort(address string) bool {
	_, port, e := net.SplitHostPort(address)
	if e != nil {
		return false
	}
	number, e := strconv.Atoi(port)
	return e == nil && number > 0 && number <= 65535
}
