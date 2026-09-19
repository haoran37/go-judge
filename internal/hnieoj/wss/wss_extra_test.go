package wss

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
	"github.com/criyle/go-judge/internal/hnieoj/resultqueue"
	"github.com/criyle/go-judge/internal/hnieoj/tasks"
	"github.com/criyle/go-judge/internal/hnieoj/worker"
)

func (s *fakeServer) sendCancel(attemptID, reason string) {
	s.mu.Lock()
	state := s.current
	s.mu.Unlock()
	if state == nil {
		return
	}
	_ = s.write(state, protocol.TypeTaskCancel, randomRequestID(), TaskCancelPayload{
		Tasks:  []TaskCancelItem{{SubmissionID: "s-cancel", JudgeTaskID: "j-cancel", AttemptID: attemptID}},
		Reason: reason,
	})
}

func TestWSSTaskCancelCancelsMatchingAttempt(t *testing.T) {
	store, err := identity.Open(t.TempDir()+"/identity.json", "n", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	_ = store.ApplyEnrollment("node-1", "key-1")
	srv := newFakeServer(t, "node-1", "key-1", store.PublicKey())
	defer srv.close()

	queue, _ := resultqueue.Open(t.TempDir(), 16, 1<<20, 1<<20, 0)
	registry := tasks.NewRegistry()
	client := New(store, &auth.Credential{}, registry, queue, nopLogger{}, Options{
		URL: srv.wsURL(), Audience: srv.audience, NodeType: "formal",
		LocalConcurrency: 1, RequestTimeout: 2 * time.Second,
		ConnectMinBackoff: 50 * time.Millisecond, ConnectMaxBackoff: 200 * time.Millisecond,
		ResultRetryBackoff: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)
	waitFor(t, 3*time.Second, func() bool { return srv.readyCount.Load() > 0 }, "READY")

	var cancelled atomic.Bool
	started := make(chan struct{})
	handler := func(ctx context.Context, task model.Task) error {
		close(started)
		<-ctx.Done()
		cancelled.Store(true)
		return ctx.Err()
	}
	poolCtx, poolCancel := context.WithCancel(context.Background())
	defer poolCancel()
	pool := worker.New(client, client, handler, nopLogger{}, worker.Options{
		Slots: 1, DrainTimeout: 5 * time.Second, RenewRetryBackoff: 50 * time.Millisecond, Registry: registry,
	})
	go pool.Run(poolCtx)

	srv.sendAssign(model.Task{SubmissionID: "s-cancel", JudgeTaskID: "j-cancel", JudgeMode: "default"}, "cancel-attempt", time.Minute)
	<-started
	srv.sendCancel("cancel-attempt", "admin revoked")
	waitFor(t, 3*time.Second, func() bool { return cancelled.Load() }, "TASK_CANCEL to cancel matching attempt")
}

func TestWSSRejectsUntrustedTLSCertificate(t *testing.T) {
	store, _ := identity.Open(t.TempDir()+"/identity.json", "n", "formal")
	_ = store.ApplyEnrollment("node-1", "key-1")
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()

	client := New(store, &auth.Credential{}, tasks.NewRegistry(), nil, nopLogger{}, Options{
		URL:      "wss" + strings.TrimPrefix(ts.URL, "https") + "/ws/judge/node",
		Audience: "judge.example.test", NodeType: "formal",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := client.dial(ctx)
	if err == nil {
		t.Fatal("self-signed certificate must be rejected")
	}
	if !isTLSError(err) {
		t.Fatalf("expected TLS verification error, got %v", err)
	}
}

func TestWSSNewSessionDoesNotAcceptOldRequestID(t *testing.T) {
	store, err := identity.Open(t.TempDir()+"/identity.json", "n", "formal")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	_ = store.ApplyEnrollment("node-1", "key-1")
	srv := newFakeServer(t, "node-1", "key-1", store.PublicKey())
	defer srv.close()

	queue, _ := resultqueue.Open(t.TempDir(), 16, 1<<20, 1<<20, 0)
	registry := tasks.NewRegistry()
	client := New(store, &auth.Credential{}, registry, queue, nopLogger{}, Options{
		URL: srv.wsURL(), Audience: srv.audience, NodeType: "formal",
		LocalConcurrency: 1, RequestTimeout: 2 * time.Second,
		ConnectMinBackoff: 50 * time.Millisecond, ConnectMaxBackoff: 200 * time.Millisecond,
		ResultRetryBackoff: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go client.Run(ctx)
	waitFor(t, 3*time.Second, func() bool { return srv.readyCount.Load() > 0 }, "first READY")
	first := client.currentSession()
	if first == nil {
		t.Fatal("no first session")
	}

	srv.disconnect()
	waitFor(t, 5*time.Second, func() bool { return srv.readyCount.Load() > 1 }, "second READY after reconnect")
	second := client.currentSession()
	if second == nil || second == first {
		t.Fatal("expected a fresh session after reconnect")
	}
	select {
	case <-first.done:
	default:
		t.Fatal("old session should be closed")
	}

	// 在新会话上发送旧 requestId 的“迟到回复”，必须被忽略且不影响新请求。
	srv.mu.Lock()
	state := srv.current
	srv.mu.Unlock()
	if err := srv.write(state, protocol.TypeTaskEventAck, "stale-request-id", map[string]string{}); err != nil {
		t.Fatalf("write stale reply: %v", err)
	}
	leaseUntil, err := client.Renew(ctx, "s", "j", "a")
	if err != nil {
		t.Fatalf("renew after stale reply: %v", err)
	}
	if leaseUntil <= 0 {
		t.Fatalf("leaseUntil = %d", leaseUntil)
	}
}
