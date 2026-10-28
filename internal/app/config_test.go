package app

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{Role: "relay", Listen: "127.0.0.1:9443", MeshListen: "127.0.0.1:9444", HTTP: "127.0.0.1:9190", ID: "r", MeshAddress: "127.0.0.1:9444", Coordinator: "127.0.0.1:9445", Certificate: "server.pem", Key: "server.key", WorkloadAPI: "unix:///tmp/workload.sock"}
}

func TestConfigRejectsUnsafeBoundaries(t *testing.T) {
	for _, edit := range []func(*Config){func(c *Config) { c.HTTP = "0.0.0.0:9190" }, func(c *Config) { c.ID = "a/b" }, func(c *Config) { c.WorkloadAPI = "http://localhost" }, func(c *Config) { c.OTel = "http://telemetry" }, func(c *Config) { c.Key = "" }, func(c *Config) { c.Listen = c.MeshListen }, func(c *Config) { c.Role = "unknown" }} {
		c := validConfig()
		edit(&c)
		if c.Validate() == nil {
			t.Fatal("unsafe config accepted")
		}
	}
	c := validConfig()
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	c.Role = "relay-coordinator"
	c.ControlListen = "127.0.0.1:9445"
	c.Upstream = "https://coordinator:443"
	c.DSN = "postgres://fixture"
	c.DatabaseIdentity = "spiffe://endlessnet.ru/service/relay-store"
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
}

func TestFxFailedMandatoryStartupIsBounded(t *testing.T) {
	c := validConfig()
	c.HTTP = "0.0.0.0:9190"
	a := App(c)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := a.Start(ctx); e == nil {
		t.Fatal("invalid mandatory config started")
	}
	_ = a.Stop(ctx)
}

func TestLogBoundaryDropsSecretsAndBoundsQueue(t *testing.T) {
	telemetry := &telemetry{queue: make(chan event, 1)}
	h := safeHandler{t: telemetry}
	record := slog.NewRecord(time.Now(), slog.LevelError, "credential-private-token", 0)
	record.AddAttrs(slog.String("payload", "private"))
	if e := h.Handle(context.Background(), record); e != nil {
		t.Fatal(e)
	}
	item := <-telemetry.queue
	if strings.Contains(item.name, "private") || strings.Contains(item.name, "credential") {
		t.Fatal("secret in fixed event")
	}
	telemetry.log("rpc", "")
	telemetry.log("rpc", "")
	if telemetry.dropped.Load() != 1 {
		t.Fatal("unbounded log queue")
	}
}
