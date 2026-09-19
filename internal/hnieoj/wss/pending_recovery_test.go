package wss

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// pendingKeyServer 模拟“服务端已激活 new key、confirm 回复丢失、进程在 old grace 结束后重启”：
// 旧 key 认证被拒，只有待恢复的 pending 新 key 能通过 AUTH_REFRESH 重新认证。
type pendingKeyServer struct {
	t            *testing.T
	activePub    string
	pendingPub   string
	audience     string
	activeKeyID  string
	pendingKeyID string

	mu        sync.Mutex
	conn      *websocket.Conn
	writeMu   sync.Mutex
	challenge ChallengePayload
	ready     chan struct{}
	// refreshAttempts 记录未认证连接上收到的 AUTH_REFRESH 次数。真实 Java
	// JudgeNodeWebSocketHandler.handleAuthRefresh 会拒绝未认证连接，这里必须一致，
	// 否则测试会错误地放行同 socket refresh 恢复路径。
	refreshAttempts atomic.Int32
}

func (s *pendingKeyServer) write(envType, reqID string, payload any) {
	raw, _ := json.Marshal(payload)
	env := protocol.Envelope{Version: protocol.Version, Type: envType, RequestID: reqID, Timestamp: time.Now().UnixMilli(), Payload: raw}
	data, _ := json.Marshal(env)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.conn != nil {
		_ = s.conn.WriteMessage(websocket.TextMessage, data)
	}
}

func (s *pendingKeyServer) sendChallenge(reqID, id string) {
	challenge := ChallengePayload{
		ChallengeID: id,
		Nonce:       "0123456789abcdef0123456789abcdef",
		Audience:    s.audience,
		ExpiresAt:   time.Now().Add(time.Minute).UnixMilli(),
		ServerTime:  time.Now().UnixMilli(),
	}
	s.mu.Lock()
	s.challenge = challenge
	s.mu.Unlock()
	s.write(protocol.TypeAuthChallenge, reqID, challenge)
}

func (s *pendingKeyServer) serve(conn *websocket.Conn) {
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	defer conn.Close()
	s.sendChallenge("chal-1", "challenge-1")
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return
		}
		switch env.Type {
		case protocol.TypeAuthResponse:
			var payload AuthResponsePayload
			_ = json.Unmarshal(env.Payload, &payload)
			s.mu.Lock()
			challenge := s.challenge
			s.mu.Unlock()
			fields := protocol.AuthFields(s.audience, challenge.ChallengeID, challenge.Nonce, payload.NodeID, payload.KeyID)
			if payload.KeyID == s.pendingKeyID && protocol.Verify(s.pendingPub, payload.Signature, fields...) == nil {
				s.write(protocol.TypeAuthOK, env.RequestID, AuthOKPayload{
					NodeID: "node-1", KeyID: s.pendingKeyID, AccessToken: fakeJWT(time.Hour),
					AccessExpiresAt: time.Now().Add(time.Hour).UnixMilli(), SessionEpoch: 1,
					ServerTime: time.Now().UnixMilli(), MaxConcurrency: 1, HeartbeatIntervalMillis: 1000,
				})
				continue
			}
			// 旧 key（或任何其他 key）一律拒绝，模拟 old key 超出 grace。
			s.write(protocol.TypeError, env.RequestID, ErrorPayload{Code: 403, Message: "key not active"})
		case protocol.TypeAuthRefresh:
			// 与真实 Java 一致：未认证连接上的 AUTH_REFRESH 一律拒绝。
			s.refreshAttempts.Add(1)
			s.write(protocol.TypeError, env.RequestID, ErrorPayload{Code: 401, Message: "connection not authenticated"})
		case protocol.TypeResumeTasks:
			s.write(protocol.TypeResumeResult, env.RequestID, ResumeResultPayload{})
		case protocol.TypeReady:
			select {
			case s.ready <- struct{}{}:
			default:
			}
		case protocol.TypeHeartbeat:
			s.write(protocol.TypeHeartbeatAck, env.RequestID, HeartbeatAckPayload{ServerTime: time.Now().UnixMilli()})
		}
	}
}

func TestPendingRotationKeyRecoversWSSAuth(t *testing.T) {
	store, err := identity.Open(t.TempDir()+"/identity.json", "n", "formal")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatal(err)
	}
	pending, err := store.BeginRotation("rotation-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRotationPrepared("rotation-1", "key-2", "nonce-1", time.Now().Add(15*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	srv := &pendingKeyServer{
		t: t, activePub: store.PublicKey(), pendingPub: pending.NewPublicKey,
		audience: "judge.example.test", activeKeyID: "key-1", pendingKeyID: "key-2",
		ready: make(chan struct{}, 4),
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		srv.serve(conn)
	}))
	defer ts.Close()

	queue, _ := resultqueue.Open(t.TempDir(), 8, 1<<20, 1<<16, time.Hour)
	client := New(store, &auth.Credential{}, tasks.NewRegistry(), queue, nopLogger{}, Options{
		URL:      "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/judge/node",
		Audience: "judge.example.test", NodeType: "formal", LocalConcurrency: 1,
		RequestTimeout: 2 * time.Second, ConnectMinBackoff: 20 * time.Millisecond,
		ConnectMaxBackoff: 100 * time.Millisecond, HeartbeatFallback: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	select {
	case <-srv.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not authenticate with pending rotation key")
	}
	if got := client.Credential().KeyIDValue(); got != "key-2" {
		t.Fatalf("credential key = %q, want pending key-2", got)
	}
	if store.Pending() == nil {
		t.Fatal("pending rotation must not be erased during WSS recovery probe")
	}
	// 恢复绝不能依赖未认证 socket 上的 AUTH_REFRESH：服务端已拒绝该路径。
	if got := srv.refreshAttempts.Load(); got != 0 {
		t.Fatalf("recovery used unauthenticated AUTH_REFRESH %d times; must use a fresh connection", got)
	}
}
