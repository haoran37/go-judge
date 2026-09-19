package wss

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
	"github.com/criyle/go-judge/internal/hnieoj/resultqueue"
	"github.com/criyle/go-judge/internal/hnieoj/tasks"
)

// 两个持久候选 key 都被服务端权威拒绝时，Run 必须停止且不自动 re-enroll；
// 每个候选使用独立物理连接，pending 私钥不得因拒绝被删除。
func TestFocusedAllKeyCandidatesRejectedStopsWithoutReenroll(t *testing.T) {
	store, err := identity.Open(t.TempDir()+"/identity.json", "n", "formal")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginRotation("rotation-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRotationPrepared("rotation-1", "key-2", "nonce-1", time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		defer conn.Close()
		challenge := ChallengePayload{
			ChallengeID: "challenge", Nonce: "0123456789abcdef0123456789abcdef",
			Audience: "aud", ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), ServerTime: time.Now().UnixMilli(),
		}
		raw, _ := json.Marshal(challenge)
		env := protocol.Envelope{Version: protocol.Version, Type: protocol.TypeAuthChallenge, RequestID: "chal", Timestamp: time.Now().UnixMilli(), Payload: raw}
		data, _ := json.Marshal(env)
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req protocol.Envelope
			if err := json.Unmarshal(msg, &req); err != nil {
				return
			}
			if req.Type != protocol.TypeAuthResponse {
				continue
			}
			payload, _ := json.Marshal(ErrorPayload{Code: 403, Message: "key not active"})
			errEnv := protocol.Envelope{Version: protocol.Version, Type: protocol.TypeError, RequestID: req.RequestID, Timestamp: time.Now().UnixMilli(), Payload: payload}
			out, _ := json.Marshal(errEnv)
			if err := conn.WriteMessage(websocket.TextMessage, out); err != nil {
				return
			}
		}
	}))
	defer ts.Close()

	queue, _ := resultqueue.Open(t.TempDir(), 8, 1<<20, 1<<16, time.Hour)
	client := New(store, &auth.Credential{}, tasks.NewRegistry(), queue, nopLogger{}, Options{
		URL:      "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/judge/node",
		Audience: "aud", NodeType: "formal", LocalConcurrency: 1,
		RequestTimeout: time.Second, ConnectMinBackoff: 10 * time.Millisecond, ConnectMaxBackoff: 20 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil despite both persisted key candidates being rejected")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after both persisted key candidates were rejected")
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("physical connections = %d, want exactly 2 (current + pending)", got)
	}
	if store.Pending() == nil {
		t.Fatal("pending candidate erased on authoritative rejection")
	}
}
