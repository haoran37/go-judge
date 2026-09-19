package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/criyle/go-judge/internal/hnieoj/config"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
	"github.com/criyle/go-judge/internal/hnieoj/wss"
)

type lifecycleServer struct {
	t        *testing.T
	pubKey   string
	audience string
	server   *httptest.Server

	mu       sync.Mutex
	conn     *websocket.Conn
	writeMu  sync.Mutex
	assigned atomic.Bool
	ready    chan struct{}
	drained  chan struct{}
}

func newLifecycleServer(t *testing.T, pubKey string) *lifecycleServer {
	s := &lifecycleServer{
		t: t, pubKey: pubKey, audience: "judge.example.test",
		ready: make(chan struct{}, 4), drained: make(chan struct{}, 4),
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.serve(conn)
	}))
	return s
}

func (s *lifecycleServer) wsURL() string {
	return "ws" + strings.TrimPrefix(s.server.URL, "http") + "/ws/judge/node"
}

func (s *lifecycleServer) write(envType, reqID string, payload any) {
	raw, _ := json.Marshal(payload)
	env := protocol.Envelope{Version: protocol.Version, Type: envType, RequestID: reqID, Timestamp: time.Now().UnixMilli(), Payload: raw}
	data, _ := json.Marshal(env)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.conn != nil {
		_ = s.conn.WriteMessage(websocket.TextMessage, data)
	}
}

func (s *lifecycleServer) serve(conn *websocket.Conn) {
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	defer conn.Close()

	challengeID := "challenge-1"
	nonce := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	challengeReqID := "challenge-req-1"
	s.write(protocol.TypeAuthChallenge, challengeReqID, wss.ChallengePayload{
		ChallengeID: challengeID, Nonce: nonce, Audience: s.audience,
		ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), ServerTime: time.Now().UnixMilli(),
	})
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
			var payload wss.AuthResponsePayload
			_ = json.Unmarshal(env.Payload, &payload)
			fields := protocol.AuthFields(s.audience, challengeID, nonce, payload.NodeID, payload.KeyID)
			if err := protocol.Verify(s.pubKey, payload.Signature, fields...); err != nil {
				s.t.Logf("auth verify: %v", err)
				s.write(protocol.TypeError, env.RequestID, wss.ErrorPayload{Code: 403, Message: "bad signature"})
				return
			}
			s.write(protocol.TypeAuthOK, env.RequestID, wss.AuthOKPayload{
				NodeID: "node-1", KeyID: "key-1", AccessToken: "a.b.c",
				AccessExpiresAt: time.Now().Add(time.Hour).UnixMilli(), SessionEpoch: 1,
				ServerTime: time.Now().UnixMilli(), MaxConcurrency: 1,
				SupportedJudgeModes: []string{"default"}, HeartbeatIntervalMillis: 200,
			})
		case protocol.TypeAuthRefresh:
			s.write(protocol.TypeAuthChallenge, env.RequestID, wss.ChallengePayload{
				ChallengeID: challengeID, Nonce: nonce, Audience: s.audience,
				ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), ServerTime: time.Now().UnixMilli(),
			})
		case protocol.TypeResumeTasks:
			s.write(protocol.TypeResumeResult, env.RequestID, wss.ResumeResultPayload{})
		case protocol.TypeReady:
			select {
			case s.ready <- struct{}{}:
			default:
			}
			if s.assigned.CompareAndSwap(false, true) {
				s.write(protocol.TypeTaskAssign, "assign-1", wss.TaskAssignPayload{
					Task:      model.Task{SubmissionID: "s1", JudgeTaskID: "j1", JudgeMode: "default"},
					AttemptID: "a1", LeaseUntil: time.Now().Add(time.Minute).UnixMilli(), RenewAfterMillis: 200,
				})
			}
		case protocol.TypeTaskAck:
		case protocol.TypeTaskRunning:
			s.write(protocol.TypeTaskEventAck, env.RequestID, map[string]string{})
		case protocol.TypeTaskResult:
			_ = json.Unmarshal(env.Payload, new(wss.TaskEventPayload))
			s.write(protocol.TypeTaskResultAck, env.RequestID, wss.TaskResultAckPayload{SubmissionID: "s1", JudgeTaskID: "j1", AttemptID: "a1"})
		case protocol.TypeLeaseRenew:
			var payload wss.LeasePayload
			_ = json.Unmarshal(env.Payload, &payload)
			payload.LeaseUntil = time.Now().Add(time.Minute).UnixMilli()
			s.write(protocol.TypeLeaseRenewed, env.RequestID, payload)
		case protocol.TypeHeartbeat:
			s.write(protocol.TypeHeartbeatAck, env.RequestID, wss.HeartbeatAckPayload{ServerTime: time.Now().UnixMilli()})
		case protocol.TypeNodeDrain:
			select {
			case s.drained <- struct{}{}:
			default:
			}
		}
	}
}

func (s *lifecycleServer) revoke() {
	s.mu.Lock()
	connected := s.conn != nil
	s.mu.Unlock()
	if connected {
		s.write(protocol.TypeNodeState, "state-1", wss.NodeStatePayload{Status: "REVOKED"})
	}
}

func TestManagerNodeStateRevokedCancelsInFlight(t *testing.T) {
	dir := t.TempDir()
	identityPath := filepath.Join(dir, "identity.json")
	store, err := identity.Open(identityPath, "node-a", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	srv := newLifecycleServer(t, store.PublicKey())
	defer srv.server.Close()

	cfg := *config.Default()
	cfg.Node.Name = "node-a"
	cfg.Node.Type = "formal"
	cfg.Node.MaxConcurrency = 1
	cfg.Node.SupportedJudgeModes = []string{"default"}
	cfg.HnieOJ.BaseURL = srv.server.URL
	cfg.HnieOJ.WSSURL = srv.wsURL()
	cfg.HnieOJ.Audience = "judge.example.test"
	cfg.Identity.File = identityPath
	cfg.Identity.StateDir = dir
	cfg.WSS.ResultQueueDir = filepath.Join(dir, "results")
	cfg.WSS.HeartbeatInterval = 200 * time.Millisecond
	cfg.Rotation.Enabled = false
	cfg.Worker.DrainTimeout = 5 * time.Second
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	manager := NewManager(logging.NopLogger{})
	manager.SetConfig(cfg)
	manager.startSandbox = func(context.Context) error { return nil }
	started := make(chan struct{})
	cancelled := make(chan struct{})
	manager.processTask = func(ctx context.Context, task model.Task) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}
	startCtx, cancelStart := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStart()
	if err := manager.Start(startCtx); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("injected task did not start")
	}
	srv.revoke()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("NODE_STATE REVOKED did not cancel in-flight task")
	}
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStop()
	if err := manager.Stop(stopCtx); err != nil {
		t.Fatalf("stop after revoke: %v", err)
	}
}

func TestManagerStopDrainsInFlightTaskAndKeepsSessionAlive(t *testing.T) {
	dir := t.TempDir()
	identityPath := filepath.Join(dir, "identity.json")
	store, err := identity.Open(identityPath, "node-a", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	srv := newLifecycleServer(t, store.PublicKey())
	defer srv.server.Close()

	cfg := *config.Default()
	cfg.Node.Name = "node-a"
	cfg.Node.Type = "formal"
	cfg.Node.MaxConcurrency = 1
	cfg.Node.SupportedJudgeModes = []string{"default"}
	cfg.HnieOJ.BaseURL = srv.server.URL
	cfg.HnieOJ.WSSURL = srv.wsURL()
	cfg.HnieOJ.Audience = "judge.example.test"
	cfg.Identity.File = identityPath
	cfg.Identity.StateDir = dir
	cfg.WSS.ResultQueueDir = filepath.Join(dir, "results")
	cfg.WSS.HeartbeatInterval = 200 * time.Millisecond
	cfg.Rotation.Enabled = false
	cfg.Worker.DrainTimeout = 5 * time.Second
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	manager := NewManager(logging.NopLogger{})
	manager.SetConfig(cfg)
	manager.startSandbox = func(context.Context) error { return nil }
	started := make(chan struct{})
	release := make(chan struct{})
	manager.processTask = func(ctx context.Context, task model.Task) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return manager.wssClient.ReportJudgeFinished(ctx, model.NewEvent(model.EventJudgeFinished, task, model.StatusAccepted, 1, 1, 1, nil, "done"))
	}

	startCtx, cancelStart := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStart()
	if err := manager.Start(startCtx); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("injected task did not start")
	}

	// Stop 必须等待在途任务排空，并在排空期间发出 NODE_DRAIN。
	stopDone := make(chan error, 1)
	go func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopDone <- manager.Stop(stopCtx)
	}()
	select {
	case <-srv.drained:
	case <-time.After(5 * time.Second):
		t.Fatal("NODE_DRAIN not sent during stop")
	}
	select {
	case err := <-stopDone:
		t.Fatalf("stop returned before in-flight task drained: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not finish after task released")
	}
	if state := manager.Status().State; state != StateStopped {
		t.Fatalf("state after stop = %s", state)
	}
}
