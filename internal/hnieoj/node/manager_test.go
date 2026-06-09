package node

import (
	"context"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
)

func TestStartRuntimeContextOutlivesCallerContext(t *testing.T) {
	manager := NewManager(logging.NopLogger{})
	var runtimeCtx context.Context
	manager.startSandbox = func(ctx context.Context) error {
		runtimeCtx = ctx
		return nil
	}
	cfg := config.Default()
	cfg.Node.Type = "temp"
	cfg.HnieOJ.BaseURL = "http://127.0.0.1:1"
	cfg.HnieOJ.TempToken.JWT = "jwt"
	cfg.HnieOJ.TempToken.TokenType = "Bearer"
	cfg.HnieOJ.TempToken.ProofType = "ed25519"
	cfg.HnieOJ.TempToken.ExpireTime = time.Now().Add(time.Hour).Format(time.RFC3339)
	cfg.Heartbeat.Enabled = false
	cfg.Reporter.Mode = "log"
	cfg.RabbitMQ.RetryBackoff = time.Millisecond
	cfg.Testdata.CacheRoot = t.TempDir()
	manager.SetConfig(*cfg)

	callerCtx, cancel := context.WithCancel(context.Background())
	if err := manager.Start(callerCtx); err != nil {
		t.Fatal(err)
	}
	cancel()

	select {
	case <-runtimeCtx.Done():
		t.Fatalf("runtime context was canceled with caller context: %v", runtimeCtx.Err())
	case <-time.After(20 * time.Millisecond):
	}

	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtimeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("runtime context was not canceled by Stop")
	}
}
