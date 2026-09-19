package heartbeat

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/criyle/go-judge/internal/hnieoj/config"
)

func TestPayloadIncludesDrainingAndMetrics(t *testing.T) {
	cfg := *config.Default()
	cfg.Node.Name = "node-a"
	cfg.Node.Type = "temp"
	cfg.Node.MaxConcurrency = 3
	cfg.Node.SupportedJudgeModes = []string{"default", "spj"}
	var running atomic.Int64
	running.Store(2)
	builder := NewBuilder(cfg, &running, func() string { return "node-42" })

	raw, err := builder.Payload(true)
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	var payload Payload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.NodeID != "node-42" || payload.NodeName != "node-a" || payload.NodeType != "temp" {
		t.Fatalf("identity fields wrong: %+v", payload)
	}
	if !payload.Draining {
		t.Fatal("draining flag missing")
	}
	if payload.RunningTasks != 2 || payload.MaxConcurrency != 3 {
		t.Fatalf("metrics wrong: %+v", payload)
	}
	if payload.Version == "" {
		t.Fatal("version missing")
	}
}
