package heartbeat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
)

func TestSendHeartbeatPayload(t *testing.T) {
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
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	var running atomic.Int64
	running.Store(2)
	cfg := config.Config{
		Node:      config.NodeConfig{Name: "node-1", Type: "formal", MaxConcurrency: 4},
		HnieOJ:    config.HnieOJConfig{BaseURL: server.URL},
		Testdata:  config.TestdataConfig{CacheRoot: t.TempDir(), StatsInterval: time.Minute},
		Heartbeat: config.HeartbeatConfig{Endpoint: "/heartbeat"},
	}
	cred := &auth.Credential{HeaderName: "X-Judge-Token", HeaderValue: "formal-token", NodeID: "node-id"}
	httpClient := server.Client()
	httpClient.Timeout = time.Second
	client := New(cfg, cred, httpClient, logging.NopLogger{}, &running)

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
}
