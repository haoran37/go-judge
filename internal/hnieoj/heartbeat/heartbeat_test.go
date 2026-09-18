package heartbeat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
)

func TestSendHeartbeatPayloadIncludingDraining(t *testing.T) {
	type requestInfo struct {
		auth    string
		payload Payload
		err     error
	}
	requests := make(chan requestInfo, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload Payload
		err := json.NewDecoder(r.Body).Decode(&payload)
		requests <- requestInfo{
			auth:    r.Header.Get("X-Judge-Token"),
			payload: payload,
			err:     err,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "msg": "success", "data": nil})
	}))
	defer server.Close()

	var running atomic.Int64
	running.Store(2)
	var draining atomic.Bool
	draining.Store(true)
	cfg := config.Config{
		Node:      config.NodeConfig{Name: "node-1", Type: "formal", MaxConcurrency: 4, SupportedJudgeModes: []string{"default", "spj"}},
		HnieOJ:    config.HnieOJConfig{BaseURL: server.URL},
		Testdata:  config.TestdataConfig{CacheRoot: t.TempDir(), StatsInterval: time.Minute},
		Heartbeat: config.HeartbeatConfig{Endpoint: "/heartbeat"},
	}
	cred := &auth.Credential{HeaderName: "X-Judge-Token", HeaderValue: "formal-token", NodeID: "node-id"}
	httpClient := server.Client()
	httpClient.Timeout = time.Second
	client := New(cfg, cred, httpClient, logging.NopLogger{}, &running, &draining)

	if err := client.Send(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := <-requests
	if got.auth != "formal-token" {
		t.Fatalf("auth header = %q", got.auth)
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.payload.NodeID != "node-id" || got.payload.NodeName != "node-1" || got.payload.RunningTasks != 2 || got.payload.MaxConcurrency != 4 {
		t.Fatalf("unexpected payload: %+v", got.payload)
	}
	if !got.payload.Draining {
		t.Fatalf("expected draining to be reported: %+v", got.payload)
	}
	if len(got.payload.SupportedJudgeModes) != 2 || got.payload.SupportedJudgeModes[1] != "spj" {
		t.Fatalf("supported judge modes = %#v", got.payload.SupportedJudgeModes)
	}
}

func TestSendHeartbeatFailsOnHTTP200BusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 403, "msg": "revoked", "data": nil})
	}))
	defer server.Close()

	cfg := config.Config{
		Node:      config.NodeConfig{Name: "node-1", Type: "formal", MaxConcurrency: 1, SupportedJudgeModes: []string{"default"}},
		HnieOJ:    config.HnieOJConfig{BaseURL: server.URL},
		Testdata:  config.TestdataConfig{CacheRoot: t.TempDir(), StatsInterval: time.Minute},
		Heartbeat: config.HeartbeatConfig{Endpoint: "/heartbeat"},
	}
	cred := &auth.Credential{HeaderName: "Authorization", HeaderValue: "Bearer token", NodeID: "node-id"}
	client := New(cfg, cred, server.Client(), logging.NopLogger{}, &atomic.Int64{}, &atomic.Bool{})
	if err := client.Send(context.Background()); err == nil {
		t.Fatal("expected HTTP200 code!=200 to fail")
	}
}

// TestSendHeartbeatConcurrentWithCredentialRenewal 覆盖心跳读取凭证与后台续期
// 原子替换凭证并发进行时不得产生数据竞争（配合 -race 运行）。
func TestSendHeartbeatConcurrentWithCredentialRenewal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200,"msg":"success","data":null}`))
	}))
	defer server.Close()

	cfg := config.Config{
		Node:      config.NodeConfig{Name: "node-1", Type: "formal", MaxConcurrency: 1, SupportedJudgeModes: []string{"default"}},
		HnieOJ:    config.HnieOJConfig{BaseURL: server.URL},
		Testdata:  config.TestdataConfig{CacheRoot: t.TempDir(), StatsInterval: time.Minute},
		Heartbeat: config.HeartbeatConfig{Endpoint: "/heartbeat"},
	}
	cred := &auth.Credential{NodeID: "node-id", TokenID: "token-id"}
	client := New(cfg, cred, server.Client(), logging.NopLogger{}, &atomic.Int64{}, &atomic.Bool{})

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				cred.Replace(&auth.Credential{NodeID: "node-id", TokenID: "token-id"})
			}
		}
	}()
	for i := 0; i < 30; i++ {
		if err := client.Send(context.Background()); err != nil {
			t.Error(err)
		}
	}
	close(done)
	wg.Wait()
}
