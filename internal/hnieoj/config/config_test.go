package config

import (
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

func TestMergeRemoteConfig(t *testing.T) {
	body := []byte(`
node:
  maxConcurrency: 3
  supportedJudgeModes: ["default", "spj"]
rabbitmq:
  prefetch: 3
  maxRetries: 5
  retryBackoff: "15s"
testdata:
  maxCacheBytes: 1024
  maxUnusedDuration: "24h"
  cleanupInterval: "30m"
  statsInterval: "2m"
heartbeat:
  enabled: true
  interval: "20s"
`)
	var overlay remoteConfigOverlay
	if err := yaml.Unmarshal(body, &overlay); err != nil {
		t.Fatalf("unmarshal remote config: %v", err)
	}
	cfg := defaultConfig()
	mergeRemoteConfig(cfg, overlay)

	if cfg.Node.MaxConcurrency != 3 {
		t.Fatalf("unexpected maxConcurrency: %d", cfg.Node.MaxConcurrency)
	}
	if cfg.RabbitMQ.Prefetch != 3 || cfg.RabbitMQ.MaxRetries != 5 || cfg.RabbitMQ.RetryBackoff != 15*time.Second {
		t.Fatalf("unexpected rabbitmq config: %+v", cfg.RabbitMQ)
	}
	if cfg.Testdata.MaxCacheBytes != 1024 || cfg.Testdata.MaxUnusedDuration != 24*time.Hour ||
		cfg.Testdata.CleanupInterval != 30*time.Minute || cfg.Testdata.StatsInterval != 2*time.Minute {
		t.Fatalf("unexpected testdata config: %+v", cfg.Testdata)
	}
	if !cfg.Heartbeat.Enabled || cfg.Heartbeat.Interval != 20*time.Second {
		t.Fatalf("unexpected heartbeat config: %+v", cfg.Heartbeat)
	}
	if len(cfg.Node.SupportedJudgeModes) != 2 || cfg.Node.SupportedJudgeModes[1] != "spj" {
		t.Fatalf("unexpected supported judge modes: %#v", cfg.Node.SupportedJudgeModes)
	}
}

func TestNormalizeJudgeModes(t *testing.T) {
	got, err := normalizeJudgeModes([]string{" default ", "SPJ", "spj", "interactive"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"default", "spj", "interactive"}
	if len(got) != len(want) {
		t.Fatalf("modes = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("modes = %#v, want %#v", got, want)
		}
	}
	if _, err := normalizeJudgeModes([]string{"unsafe"}); err == nil {
		t.Fatal("expected unsupported mode error")
	}
}
