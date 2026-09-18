package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/gateway"
	"github.com/criyle/go-judge/internal/hnieoj/heartbeat"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/worker"
)

func lifecycleJWT(expMillis int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"tokenId":"t","nodeId":"n"}`, expMillis)))
	return header + "." + payload + ".signature"
}

func lifecycleEnvelope(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "msg": "success", "data": data})
}

type lifecycleClaimer struct {
	mu    sync.Mutex
	claim *gateway.Claim
	used  bool
}

func (c *lifecycleClaimer) Claim(ctx context.Context) (*gateway.Claim, error) {
	c.mu.Lock()
	if !c.used {
		c.used = true
		claim := c.claim
		c.mu.Unlock()
		return claim, nil
	}
	c.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

type lifecycleRenewer struct{}

func (lifecycleRenewer) Renew(ctx context.Context, _, _, _ string) (int64, error) {
	return time.Now().Add(time.Hour).UnixMilli(), nil
}

// TestCoordinateShutdownDrainsAndKeepsAuxAlive 覆盖真实关闭编排：
// SIGTERM 后停止领取、在途任务排空期间凭证续期与心跳仍然存活，退出前显式发送
// draining 心跳，排空完成后取消辅助协程。
func TestCoordinateShutdownDrainsAndKeepsAuxAlive(t *testing.T) {
	var renewCalls atomic.Int64
	heartbeats := make(chan heartbeat.Payload, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/judge/nodes/token/renew":
			renewCalls.Add(1)
			lifecycleEnvelope(w, map[string]any{
				"token": lifecycleJWT(time.Now().Add(400 * time.Millisecond).UnixMilli()), "tokenType": "Bearer",
				"nodeId": "n", "tokenId": "t",
			})
		case "/judge/nodes/heartbeat":
			var payload heartbeat.Payload
			_ = json.NewDecoder(r.Body).Decode(&payload)
			select {
			case heartbeats <- payload:
			default:
			}
			lifecycleEnvelope(w, nil)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := config.Config{
		Node: config.NodeConfig{Name: "lifecycle-node", Type: "formal", MaxConcurrency: 1, SupportedJudgeModes: []string{"default"}},
		HnieOJ: config.HnieOJConfig{
			BaseURL:    server.URL,
			Credential: config.Credential{Token: lifecycleJWT(time.Now().Add(400 * time.Millisecond).UnixMilli())},
			Renew:      config.RenewConfig{SafetyMargin: 50 * time.Millisecond, RetryBackoff: 10 * time.Millisecond},
		},
		Testdata:  config.TestdataConfig{CacheRoot: t.TempDir(), StatsInterval: time.Minute},
		Heartbeat: config.HeartbeatConfig{Enabled: true, Endpoint: "/judge/nodes/heartbeat", Interval: time.Hour},
	}
	manager, err := auth.Load(context.Background(), cfg, server.Client(), logging.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	auxCtx, cancelAux := context.WithCancel(context.Background())
	defer cancelAux()
	manager.StartRenewal(auxCtx)

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	handler := func(ctx context.Context, _ model.Task) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	claimer := &lifecycleClaimer{claim: &gateway.Claim{
		Task:             model.Task{SubmissionID: "sub-1", JudgeTaskID: "task-1"},
		AttemptID:        "attempt-1",
		LeaseUntil:       time.Now().Add(time.Hour).UnixMilli(),
		RenewAfterMillis: 3_600_000,
	}}
	pool := worker.New(claimer, lifecycleRenewer{}, handler, logging.NopLogger{}, worker.Options{
		Slots: 1, EmptyMinBackoff: time.Hour, EmptyMaxBackoff: time.Hour, DrainTimeout: 2 * time.Second,
	})

	draining := &atomic.Bool{}
	heartbeatClient := heartbeat.New(cfg, manager.Credential(), server.Client(), logging.NopLogger{}, pool.Active(), draining)
	heartbeatClient.Start(auxCtx)

	signalCtx, cancelSignal := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- coordinateShutdown(signalCtx, cancelAux, heartbeatClient, draining, pool, true, logging.NopLogger{})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight task never started")
	}
	renewBefore := renewCalls.Load()
	cancelSignal() // SIGTERM 语义：停止领取并开始排空。

	// 排空期间必须发出 draining=true 的最终心跳，且仍能看到在途任务。
	final := waitForDrainingHeartbeat(t, heartbeats, 2*time.Second)
	if final.RunningTasks != 1 {
		t.Fatalf("final draining heartbeat runningTasks = %d, want 1", final.RunningTasks)
	}
	// 排空期间凭证续期仍须继续。
	waitForRenewDuringDrain(t, &renewCalls, renewBefore, 2*time.Second)

	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not complete after in-flight task finished")
	}
	if !draining.Load() {
		t.Fatal("draining flag should be published on shutdown")
	}
}

func waitForDrainingHeartbeat(t *testing.T, heartbeats <-chan heartbeat.Payload, timeout time.Duration) heartbeat.Payload {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case payload := <-heartbeats:
			if payload.Draining {
				return payload
			}
		case <-deadline:
			t.Fatal("no draining heartbeat observed before timeout")
		}
	}
}

func waitForRenewDuringDrain(t *testing.T, calls *atomic.Int64, before int64, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for calls.Load() <= before {
		select {
		case <-deadline:
			t.Fatal("credential renewal stopped during in-flight drain")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}
