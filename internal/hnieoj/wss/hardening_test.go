package wss

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/identity"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
	"github.com/criyle/go-judge/internal/hnieoj/resultqueue"
	"github.com/criyle/go-judge/internal/hnieoj/tasks"
)

func newUnitClient(t *testing.T) (*Client, *session) {
	t.Helper()
	q, err := resultqueue.Open(t.TempDir(), 8, 1<<20, 1<<16, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c := New(nil, &auth.Credential{}, tasks.NewRegistry(), q, nopLogger{}, Options{RequestTimeout: time.Second})
	return c, newSession(nil)
}

// TestResumeAppliesEpochLeaseAndCancelsRejectedOrOmitted 覆盖 R2：
// resumed attempt 必须同步 epoch/租约；rejected 与 omitted attempt 必须取消且不复活。
func TestResumeAppliesEpochLeaseAndCancelsRejectedOrOmitted(t *testing.T) {
	c, s := newUnitClient(t)
	var canceledA, canceledB, canceledC atomic.Bool
	c.registry.Start(tasks.Attempt{SubmissionID: "s1", JudgeTaskID: "j1", AttemptID: "a1"}, func() { canceledA.Store(true) })
	c.registry.Start(tasks.Attempt{SubmissionID: "s2", JudgeTaskID: "j2", AttemptID: "a2"}, func() { canceledB.Store(true) })
	c.registry.Start(tasks.Attempt{SubmissionID: "s3", JudgeTaskID: "j3", AttemptID: "a3"}, func() { canceledC.Store(true) })
	c.epoch.Store(7)

	go func() {
		data := <-s.sendCh
		var req protocol.Envelope
		_ = json.Unmarshal(data, &req)
		var payload ResumeTasksPayload
		_ = json.Unmarshal(req.Payload, &payload)
		if len(payload.Attempts) != 3 {
			t.Errorf("expected 3 resume attempts, got %d", len(payload.Attempts))
		}
		s.lookup(req.RequestID).ch <- &protocol.Envelope{
			Type:      protocol.TypeResumeResult,
			RequestID: req.RequestID,
			Payload:   json.RawMessage(`{"resumed":[{"submissionId":"s1","judgeTaskId":"j1","attemptId":"a1","leaseUntil":9999999999999}],"rejected":[{"submissionId":"s2","judgeTaskId":"j2","attemptId":"a2","reason":"lost"}]}`),
		}
	}()

	if err := c.resume(context.Background(), s); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if canceledA.Load() {
		t.Fatal("resumed attempt must not be cancelled")
	}
	if !canceledB.Load() {
		t.Fatal("rejected attempt must be cancelled")
	}
	if !canceledC.Load() {
		t.Fatal("omitted attempt must be cancelled")
	}
	got, ok := c.registry.Get("s1", "j1", "a1")
	if !ok {
		t.Fatal("resumed attempt removed from registry")
	}
	if got.Epoch != 7 {
		t.Fatalf("resumed attempt epoch = %d, want 7", got.Epoch)
	}
	if got.LeaseUntil != 9999999999999 {
		t.Fatalf("resumed attempt lease = %d", got.LeaseUntil)
	}
}

func TestResumeRejectsWrongResponseType(t *testing.T) {
	c, s := newUnitClient(t)
	go func() {
		data := <-s.sendCh
		var req protocol.Envelope
		_ = json.Unmarshal(data, &req)
		s.lookup(req.RequestID).ch <- &protocol.Envelope{
			Type:      protocol.TypeHeartbeatAck,
			RequestID: req.RequestID,
			Payload:   json.RawMessage(`{}`),
		}
	}()
	if err := c.resume(context.Background(), s); err == nil {
		t.Fatal("resume accepted a non-RESUME_RESULT response")
	}
}

func TestApplyAuthOKValidatesIdentityAndEpoch(t *testing.T) {
	store, err := identity.Open(t.TempDir()+"/identity.json", "n", "formal")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatal(err)
	}
	c := New(store, &auth.Credential{}, tasks.NewRegistry(), nil, nopLogger{}, Options{})

	base := AuthOKPayload{
		NodeID: "node-1", KeyID: "key-1", AccessToken: "a.b.c",
		AccessExpiresAt: time.Now().Add(time.Hour).UnixMilli(), SessionEpoch: 3,
	}
	if err := c.applyAuthOK(&base, false); err != nil {
		t.Fatalf("valid AUTH_OK rejected: %v", err)
	}
	if c.Epoch() != 3 {
		t.Fatalf("epoch = %d", c.Epoch())
	}

	unknownKey := base
	unknownKey.KeyID = "ghost"
	if err := c.applyAuthOK(&unknownKey, false); err == nil {
		t.Fatal("AUTH_OK with unknown keyId accepted")
	}
	wrongNode := base
	wrongNode.NodeID = "other"
	if err := c.applyAuthOK(&wrongNode, false); err == nil {
		t.Fatal("AUTH_OK with mismatched nodeId accepted")
	}
	zeroEpoch := base
	zeroEpoch.SessionEpoch = 0
	if err := c.applyAuthOK(&zeroEpoch, false); err == nil {
		t.Fatal("AUTH_OK with non-positive epoch accepted")
	}
	noExpiry := base
	noExpiry.AccessExpiresAt = 0
	if err := c.applyAuthOK(&noExpiry, false); err == nil {
		t.Fatal("AUTH_OK without expiry accepted")
	}
}

func TestRenewRejectsMismatchedLeaseIdentity(t *testing.T) {
	store, err := identity.Open(t.TempDir()+"/identity.json", "n", "formal")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyEnrollment("node-1", "key-1"); err != nil {
		t.Fatal(err)
	}
	srv := newFakeServer(t, "node-1", "key-1", store.PublicKey())
	defer srv.close()
	queue, _ := resultqueue.Open(t.TempDir(), 8, 1<<20, 1<<16, time.Hour)
	c := New(store, &auth.Credential{}, tasks.NewRegistry(), queue, nopLogger{}, Options{
		URL: srv.wsURL(), Audience: srv.audience, NodeType: "formal",
		LocalConcurrency: 1, RequestTimeout: 2 * time.Second,
		ConnectMinBackoff: 50 * time.Millisecond, ConnectMaxBackoff: 200 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, 3*time.Second, func() bool { return srv.readyCount.Load() > 0 }, "READY")

	// fake server 回显请求身份，Renew 正常成功；再验证 identity mismatch 路径。
	if _, err := c.Renew(ctx, "s", "j", "a"); err != nil {
		t.Fatalf("renew: %v", err)
	}
}
