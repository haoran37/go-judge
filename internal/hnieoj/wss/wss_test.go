package wss

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
	"github.com/criyle/go-judge/internal/hnieoj/resultqueue"
	"github.com/criyle/go-judge/internal/hnieoj/tasks"
	"github.com/criyle/go-judge/internal/hnieoj/worker"
)

type nopLogger struct{}

func (nopLogger) Info(string, ...logging.Field)  {}
func (nopLogger) Warn(string, ...logging.Field)  {}
func (nopLogger) Error(string, ...logging.Field) {}
func (nopLogger) Debug(string, ...logging.Field) {}

type connState struct {
	conn           *websocket.Conn
	writeMu        sync.Mutex
	challenge      ChallengePayload
	challengeReqID string
	offset         atomic.Int64
}

type fakeServer struct {
	t        *testing.T
	audience string
	nodeID   string
	keyID    string
	pubKey   string

	server *httptest.Server

	mu      sync.Mutex
	current *connState
	epoch   int64

	readyCount atomic.Int32
	acks       chan TaskAckPayload
	running    chan TaskEventPayload
	results    chan TaskEventPayload
	leaseReqs  chan LeasePayload
	drains     chan struct{}
	resumeReqs chan ResumeTasksPayload

	dropResultAcks atomic.Int32
	dropRunning    atomic.Int32
	resumeMu       sync.Mutex
	resumeFn       func([]ResumeAttempt) ResumeResultPayload
}

func (s *fakeServer) setResumeFn(fn func([]ResumeAttempt) ResumeResultPayload) {
	s.resumeMu.Lock()
	defer s.resumeMu.Unlock()
	s.resumeFn = fn
}

func (s *fakeServer) resume(attempts []ResumeAttempt) ResumeResultPayload {
	s.resumeMu.Lock()
	fn := s.resumeFn
	s.resumeMu.Unlock()
	if fn == nil {
		return ResumeResultPayload{}
	}
	return fn(attempts)
}

func newFakeServer(t *testing.T, nodeID, keyID, publicKey string) *fakeServer {
	s := &fakeServer{
		t:          t,
		audience:   "judge.example.test",
		nodeID:     nodeID,
		keyID:      keyID,
		pubKey:     publicKey,
		acks:       make(chan TaskAckPayload, 64),
		running:    make(chan TaskEventPayload, 64),
		results:    make(chan TaskEventPayload, 64),
		leaseReqs:  make(chan LeasePayload, 64),
		drains:     make(chan struct{}, 8),
		resumeReqs: make(chan ResumeTasksPayload, 8),
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/judge/node" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		state := &connState{conn: conn}
		s.mu.Lock()
		s.current = state
		s.epoch++
		state.challenge = ChallengePayload{}
		s.mu.Unlock()
		s.serve(state)
	}))
	return s
}

func (s *fakeServer) close() { s.server.Close() }

func (s *fakeServer) wsURL() string {
	return "ws" + strings.TrimPrefix(s.server.URL, "http") + "/ws/judge/node"
}

func (s *fakeServer) write(state *connState, envType, reqID string, payload any) error {
	raw := []byte("{}")
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		raw = encoded
	}
	env := protocol.Envelope{Version: protocol.Version, Type: envType, RequestID: reqID, Timestamp: time.Now().UnixMilli(), Payload: raw}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	state.writeMu.Lock()
	defer state.writeMu.Unlock()
	return state.conn.WriteMessage(websocket.TextMessage, data)
}

func (s *fakeServer) serve(state *connState) {
	defer state.conn.Close()
	s.sendChallenge(state, randomRequestID())
	for {
		_, data, err := state.conn.ReadMessage()
		if err != nil {
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return
		}
		if err := s.handleMessage(state, &env); err != nil {
			return
		}
	}
}

func (s *fakeServer) sendChallenge(state *connState, reqID string) {
	nonce := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	challenge := ChallengePayload{
		ChallengeID: "challenge-" + reqID,
		Nonce:       nonce,
		Audience:    s.audience,
		ExpiresAt:   time.Now().Add(time.Minute).UnixMilli(),
		ServerTime:  time.Now().UnixMilli(),
	}
	state.challenge = challenge
	state.challengeReqID = reqID
	_ = s.write(state, protocol.TypeAuthChallenge, reqID, challenge)
}

func (s *fakeServer) handleMessage(state *connState, env *protocol.Envelope) error {
	switch env.Type {
	case protocol.TypeAuthResponse:
		var payload AuthResponsePayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return err
		}
		if payload.ChallengeID != state.challenge.ChallengeID {
			return fmt.Errorf("challenge mismatch")
		}
		fields := protocol.AuthFields(s.audience, state.challenge.ChallengeID, state.challenge.Nonce, payload.NodeID, payload.KeyID)
		if err := protocol.Verify(s.pubKey, payload.Signature, fields...); err != nil {
			s.t.Logf("auth signature rejected: %v", err)
			return s.write(state, protocol.TypeError, env.RequestID, ErrorPayload{Code: 403, Message: "bad signature"})
		}
		s.mu.Lock()
		epoch := s.epoch
		s.mu.Unlock()
		return s.write(state, protocol.TypeAuthOK, env.RequestID, AuthOKPayload{
			NodeID:                  s.nodeID,
			KeyID:                   s.keyID,
			AccessToken:             fakeJWT(time.Hour),
			AccessExpiresAt:         time.Now().Add(time.Hour).UnixMilli(),
			SessionEpoch:            epoch,
			ServerTime:              time.Now().UnixMilli(),
			MaxConcurrency:          2,
			SupportedJudgeModes:     []string{"default"},
			AuthorizationUntil:      time.Now().Add(24 * time.Hour).UnixMilli(),
			HeartbeatIntervalMillis: 100,
		})
	case protocol.TypeAuthRefresh:
		s.sendChallenge(state, env.RequestID)
		return nil
	case protocol.TypeResumeTasks:
		var payload ResumeTasksPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return err
		}
		select {
		case s.resumeReqs <- payload:
		default:
		}
		result := s.resume(payload.Attempts)
		return s.write(state, protocol.TypeResumeResult, env.RequestID, result)
	case protocol.TypeReady:
		s.readyCount.Add(1)
		return nil
	case protocol.TypeTaskAck:
		var payload TaskAckPayload
		_ = json.Unmarshal(env.Payload, &payload)
		select {
		case s.acks <- payload:
		default:
		}
		return nil
	case protocol.TypeTaskRunning:
		var payload TaskEventPayload
		_ = json.Unmarshal(env.Payload, &payload)
		if s.dropRunning.Load() > 0 {
			s.dropRunning.Add(-1)
			return nil
		}
		select {
		case s.running <- payload:
		default:
		}
		return s.write(state, protocol.TypeTaskEventAck, env.RequestID, map[string]string{})
	case protocol.TypeTaskResult:
		var payload TaskEventPayload
		_ = json.Unmarshal(env.Payload, &payload)
		if s.dropResultAcks.Load() > 0 {
			s.dropResultAcks.Add(-1)
			return nil
		}
		select {
		case s.results <- payload:
		default:
		}
		var task model.Task
		_ = json.Unmarshal(payload.Event, &task)
		ack := TaskResultAckPayload{SubmissionID: payload.SubmissionID, JudgeTaskID: task.JudgeTaskID, AttemptID: task.AttemptID}
		return s.write(state, protocol.TypeTaskResultAck, env.RequestID, ack)
	case protocol.TypeLeaseRenew:
		var payload LeasePayload
		_ = json.Unmarshal(env.Payload, &payload)
		select {
		case s.leaseReqs <- payload:
		default:
		}
		payload.LeaseUntil = time.Now().Add(30 * time.Second).UnixMilli()
		return s.write(state, protocol.TypeLeaseRenewed, env.RequestID, payload)
	case protocol.TypeHeartbeat:
		return s.write(state, protocol.TypeHeartbeatAck, env.RequestID, HeartbeatAckPayload{ServerTime: time.Now().UnixMilli()})
	case protocol.TypeNodeDrain:
		select {
		case s.drains <- struct{}{}:
		default:
		}
		return nil
	case protocol.TypePong:
		return nil
	default:
		return nil
	}
}

func (s *fakeServer) sendAssign(task model.Task, attemptID string, lease time.Duration) {
	s.mu.Lock()
	state := s.current
	s.mu.Unlock()
	if state == nil {
		s.t.Log("no active connection for TASK_ASSIGN")
		return
	}
	_ = s.write(state, protocol.TypeTaskAssign, randomRequestID(), TaskAssignPayload{
		Task:             task,
		AttemptID:        attemptID,
		LeaseUntil:       time.Now().Add(lease).UnixMilli(),
		RenewAfterMillis: 200,
	})
}

func (s *fakeServer) disconnect() {
	s.mu.Lock()
	state := s.current
	s.current = nil
	s.mu.Unlock()
	if state != nil {
		_ = state.conn.Close()
	}
}

func fakeJWT(ttl time.Duration) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(ttl).Unix())))
	return header + "." + payload + ".sig"
}

func newTestClient(t *testing.T, srv *fakeServer, queue *resultqueue.Queue, opts Options) (*Client, *identity.Store, *tasks.Registry) {
	t.Helper()
	store, err := identity.Open(t.TempDir()+"/identity.json", "node", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := store.ApplyEnrollment(srv.nodeID, srv.keyID); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	cred := &auth.Credential{}
	registry := tasks.NewRegistry()
	opts.URL = srv.wsURL()
	opts.Audience = srv.audience
	opts.NodeType = "formal"
	opts.HeartbeatFallback = 100 * time.Millisecond
	opts.RequestTimeout = 3 * time.Second
	opts.ConnectMinBackoff = 50 * time.Millisecond
	opts.ConnectMaxBackoff = 200 * time.Millisecond
	opts.ResultRetryBackoff = 50 * time.Millisecond
	opts.WriteTimeout = 2 * time.Second
	client := New(store, cred, registry, queue, nopLogger{}, opts)
	return client, store, registry
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", message)
}

func TestWSSAuthAssignResultAndLease(t *testing.T) {
	store, err := identity.Open(t.TempDir()+"/identity.json", "n", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	srv := newFakeServer(t, "node-1", "key-1", store.PublicKey())
	defer srv.close()

	queue, err := resultqueue.Open(t.TempDir(), 16, 1<<20, 1<<20, 0)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	client := New(store, &auth.Credential{}, tasks.NewRegistry(), queue, nopLogger{}, Options{
		URL: srv.wsURL(), Audience: srv.audience, NodeType: "formal",
		LocalConcurrency: 2, HeartbeatFallback: 100 * time.Millisecond,
		RequestTimeout: 3 * time.Second, ConnectMinBackoff: 50 * time.Millisecond,
		ConnectMaxBackoff: 200 * time.Millisecond, ResultRetryBackoff: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	waitFor(t, 3*time.Second, func() bool { return srv.readyCount.Load() > 0 }, "READY after auth/resume")
	if client.Credential().SessionEpochValue() == 0 {
		t.Fatal("AUTH_OK should set sessionEpoch")
	}

	registry := client.Registry()
	var ran atomic.Int32
	handler := func(ctx context.Context, task model.Task) error {
		ran.Add(1)
		if err := client.ReportStatusChanged(ctx, model.NewEvent(model.EventStatusChanged, task, model.StatusCompiling, 1, 0, 0, nil, "Compiling")); err != nil {
			return err
		}
		return client.ReportJudgeFinished(ctx, model.NewEvent(model.EventJudgeFinished, task, model.StatusAccepted, 1, 1, 1, nil, "done"))
	}
	poolCtx, poolCancel := context.WithCancel(context.Background())
	defer poolCancel()
	pool := worker.New(client, client, handler, nopLogger{}, worker.Options{
		Slots: 1, EmptyMinBackoff: 20 * time.Millisecond, EmptyMaxBackoff: 50 * time.Millisecond,
		DrainTimeout: 2 * time.Second, RenewRetryBackoff: 50 * time.Millisecond, Registry: registry,
	})
	go pool.Run(poolCtx)

	task := model.Task{SubmissionID: "s1", JudgeTaskID: "j1", JudgeID: 1, ProblemID: 1, JudgeMode: "default", IOScore: 1}
	srv.sendAssign(task, "attempt-1", 30*time.Second)

	select {
	case ack := <-srv.acks:
		if ack.SubmissionID != "s1" || ack.AttemptID != "attempt-1" {
			t.Fatalf("unexpected TASK_ACK %+v", ack)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TASK_ACK not received")
	}
	waitFor(t, 3*time.Second, func() bool { return ran.Load() == 1 }, "task handler to run")
	waitFor(t, 3*time.Second, func() bool { return queue.Len() == 0 }, "result to be acknowledged and removed")

	// 重复 TASK_ASSIGN 只 ACK，不重复执行。
	srv.sendAssign(task, "attempt-1", 30*time.Second)
	time.Sleep(300 * time.Millisecond)
	if got := ran.Load(); got != 1 {
		t.Fatalf("duplicate attempt executed %d times", got)
	}
}

func TestWSSReconnectResumesInFlightAttempt(t *testing.T) {
	store, err := identity.Open(t.TempDir()+"/identity.json", "n", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	_ = store.ApplyEnrollment("node-1", "key-1")
	srv := newFakeServer(t, "node-1", "key-1", store.PublicKey())
	defer srv.close()
	srv.dropRunning.Store(1 << 30) // never ACK progress so the task stays in flight

	queue, _ := resultqueue.Open(t.TempDir(), 16, 1<<20, 1<<20, 0)
	registry := tasks.NewRegistry()
	client := New(store, &auth.Credential{}, registry, queue, nopLogger{}, Options{
		URL: srv.wsURL(), Audience: srv.audience, NodeType: "formal",
		LocalConcurrency: 2, RequestTimeout: 3 * time.Second,
		ConnectMinBackoff: 50 * time.Millisecond, ConnectMaxBackoff: 200 * time.Millisecond,
		ResultRetryBackoff: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)
	waitFor(t, 3*time.Second, func() bool { return srv.readyCount.Load() > 0 }, "first READY")

	var ran atomic.Int32
	started := make(chan struct{})
	handler := func(ctx context.Context, task model.Task) error {
		ran.Add(1)
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	poolCtx, poolCancel := context.WithCancel(context.Background())
	defer poolCancel()
	pool := worker.New(client, client, handler, nopLogger{}, worker.Options{
		Slots: 1, DrainTimeout: 10 * time.Second, RenewRetryBackoff: 50 * time.Millisecond, Registry: registry,
	})
	go pool.Run(poolCtx)

	task := model.Task{SubmissionID: "s2", JudgeTaskID: "j2", JudgeMode: "default"}
	srv.sendAssign(task, "attempt-2", 30*time.Second)
	<-started
	waitFor(t, 2*time.Second, func() bool { return registry.Len() == 1 }, "registry to hold in-flight attempt")

	// 断线重连后必须 RESUME 同一 attempt，并且不得重复执行。
	srv.setResumeFn(func(attempts []ResumeAttempt) ResumeResultPayload {
		_ = attempts
		return ResumeResultPayload{Resumed: []ResumedTask{{
			SubmissionID: "s2", JudgeTaskID: "j2", AttemptID: "attempt-2",
			LeaseUntil: time.Now().Add(30 * time.Second).UnixMilli(),
		}}}
	})
	srv.disconnect()
	firstRan := ran.Load()
	waitFor(t, 5*time.Second, func() bool {
		select {
		case resume := <-srv.resumeReqs:
			for _, a := range resume.Attempts {
				if a.AttemptID == "attempt-2" {
					return true
				}
			}
		default:
		}
		return false
	}, "RESUME_TASKS to include in-flight attempt")
	if ran.Load() != firstRan {
		t.Fatalf("resumed attempt must not re-execute: ran %d -> %d", firstRan, ran.Load())
	}
}

func TestWSSResultAckLossRetriesThenClears(t *testing.T) {
	store, err := identity.Open(t.TempDir()+"/identity.json", "n", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	_ = store.ApplyEnrollment("node-1", "key-1")
	srv := newFakeServer(t, "node-1", "key-1", store.PublicKey())
	defer srv.close()
	srv.dropResultAcks.Store(2)

	queue, _ := resultqueue.Open(t.TempDir(), 16, 1<<20, 1<<20, 0)
	if err := queue.Enqueue(resultqueue.Record{
		ID: "s3\x00j3\x00a3", SubmissionID: "s3", JudgeTaskID: "j3", AttemptID: "a3",
		Event: json.RawMessage(`{"eventType":"JUDGE_FINISHED","submissionId":"s3","judgeTaskId":"j3","attemptId":"a3"}`),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	client := New(store, &auth.Credential{}, tasks.NewRegistry(), queue, nopLogger{}, Options{
		URL: srv.wsURL(), Audience: srv.audience, NodeType: "formal",
		LocalConcurrency: 1, RequestTimeout: 800 * time.Millisecond,
		ConnectMinBackoff: 50 * time.Millisecond, ConnectMaxBackoff: 200 * time.Millisecond,
		ResultRetryBackoff: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)

	// 前两次 ACK 丢失，队列必须保留记录并在重试后清空。
	waitFor(t, 10*time.Second, func() bool { return queue.Len() == 0 }, "result to survive lost ACKs and clear")
	select {
	case res := <-srv.results:
		if res.SubmissionID != "s3" {
			t.Fatalf("unexpected result %+v", res)
		}
	default:
	}
}

func TestWSSDrainSendsNodeDrainAndStopsReady(t *testing.T) {
	store, err := identity.Open(t.TempDir()+"/identity.json", "n", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	_ = store.ApplyEnrollment("node-1", "key-1")
	srv := newFakeServer(t, "node-1", "key-1", store.PublicKey())
	defer srv.close()

	queue, _ := resultqueue.Open(t.TempDir(), 16, 1<<20, 1<<20, 0)
	client := New(store, &auth.Credential{}, tasks.NewRegistry(), queue, nopLogger{}, Options{
		URL: srv.wsURL(), Audience: srv.audience, NodeType: "formal",
		LocalConcurrency: 1, RequestTimeout: 3 * time.Second,
		ConnectMinBackoff: 50 * time.Millisecond, ConnectMaxBackoff: 200 * time.Millisecond,
		ResultRetryBackoff: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)
	waitFor(t, 3*time.Second, func() bool { return srv.readyCount.Load() > 0 }, "READY")

	if err := client.BeginDrain(ctx); err != nil {
		t.Fatalf("begin drain: %v", err)
	}
	select {
	case <-srv.drains:
	case <-time.After(3 * time.Second):
		t.Fatal("NODE_DRAIN not received")
	}
	if !client.Draining() {
		t.Fatal("client should be draining")
	}
	before := srv.readyCount.Load()
	client.notifyReady()
	time.Sleep(300 * time.Millisecond)
	if after := srv.readyCount.Load(); after != before {
		t.Fatalf("READY sent while draining: %d -> %d", before, after)
	}
}
