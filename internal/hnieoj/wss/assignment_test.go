package wss

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/model"
	"github.com/criyle/go-judge/internal/hnieoj/protocol"
	"github.com/criyle/go-judge/internal/hnieoj/tasks"
)

func assignEnvelope(t *testing.T, submissionID, judgeTaskID, attemptID string) *protocol.Envelope {
	t.Helper()
	raw, err := json.Marshal(TaskAssignPayload{
		Task:       model.Task{SubmissionID: submissionID, JudgeTaskID: judgeTaskID},
		AttemptID:  attemptID,
		LeaseUntil: time.Now().Add(time.Minute).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.Envelope{Type: protocol.TypeTaskAssign, RequestID: "assignment-" + attemptID, Payload: raw}
}

// TestAssignmentDuplicateAtCapacityAcknowledged 覆盖同一 attempt 的重复分配：
// 即便已到批准额度，也必须幂等 ACK，绝不重复入队或断开连接。
func TestAssignmentDuplicateAtCapacityAcknowledged(t *testing.T) {
	c, s := newUnitClient(t)
	s.markReady()
	env := assignEnvelope(t, "s", "j", "a")
	c.handleAssign(s, env)
	<-s.sendCh // 首次 ACK

	c.handleAssign(s, env)
	select {
	case <-s.done:
		t.Fatal("duplicate assignment at capacity disconnected")
	default:
	}
	select {
	case data := <-s.sendCh:
		var ack protocol.Envelope
		if err := json.Unmarshal(data, &ack); err != nil {
			t.Fatal(err)
		}
		if ack.Type != protocol.TypeTaskAck {
			t.Fatalf("duplicate reply type = %s, want TASK_ACK", ack.Type)
		}
	default:
		t.Fatal("duplicate assignment not acknowledged")
	}
	if len(c.assignCh) != 1 {
		t.Fatalf("duplicate queued for execution: %d", len(c.assignCh))
	}
}

// TestAssignmentRejectedWhenResultQueueFull 覆盖结果队列满时不得接纳新任务：
// 既不注册也不入队，并以可重试 ERROR 明确回绝，而不是静默丢弃。
func TestAssignmentRejectedWhenResultQueueFull(t *testing.T) {
	c, s := newUnitClient(t)
	s.markReady()
	for i := 0; i < 8; i++ {
		if err := c.queueRecord("s", "j", string(rune('a'+i)), json.RawMessage(`{"eventType":"JUDGE_FINISHED"}`)); err != nil {
			t.Fatalf("seed result %d: %v", i, err)
		}
	}
	c.handleAssign(s, assignEnvelope(t, "new", "j", "a"))
	if len(c.assignCh) != 0 || c.registry.Len() != 0 {
		t.Fatal("new work admitted despite full durable result queue")
	}
	select {
	case data := <-s.sendCh:
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatal(err)
		}
		if env.Type != protocol.TypeError {
			t.Fatalf("rejection type = %s, want ERROR", env.Type)
		}
		var payload ErrorPayload
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if !payload.Retryable {
			t.Fatal("queue-full rejection must be retryable")
		}
	default:
		t.Fatal("no rejection response for full result queue")
	}
}

// TestAdmissionReservesAndCompletionReleasesResultCapacity 保证接纳时预留、
// attempt 结束（未产生终态）时释放，容量不会被长期占用。
func TestAdmissionReservesAndCompletionReleasesResultCapacity(t *testing.T) {
	c, s := newUnitClient(t)
	s.markReady()
	c.handleAssign(s, assignEnvelope(t, "s", "j", "a"))
	<-s.sendCh
	if got := c.queue.Reserved(); got != 1 {
		t.Fatalf("reserved = %d, want 1", got)
	}
	c.registry.Complete("s", "j", "a")
	if got := c.queue.Reserved(); got != 0 {
		t.Fatalf("reservation not released on completion: %d", got)
	}
}

func loadResumeResult(t *testing.T, s *session, payload string) {
	t.Helper()
	go func() {
		data := <-s.sendCh
		var request protocol.Envelope
		_ = json.Unmarshal(data, &request)
		s.lookup(request.RequestID).ch <- &protocol.Envelope{
			Type:      protocol.TypeResumeResult,
			RequestID: request.RequestID,
			Payload:   json.RawMessage(payload),
		}
	}()
}

// TestResumeZeroLeaseCancelsInsteadOfFabricating 覆盖非法 resume：
// 服务端未给正租约时绝不能本地合成延长，必须立即取消且不改写租约。
func TestResumeZeroLeaseCancelsInsteadOfFabricating(t *testing.T) {
	c, s := newUnitClient(t)
	var canceled atomic.Bool
	originalLease := time.Now().Add(time.Minute).UnixMilli()
	c.registry.Start(tasks.Attempt{SubmissionID: "s", JudgeTaskID: "j", AttemptID: "a", LeaseUntil: originalLease}, func() { canceled.Store(true) })
	loadResumeResult(t, s, `{"resumed":[{"submissionId":"s","judgeTaskId":"j","attemptId":"a","leaseUntil":0}],"rejected":[]}`)
	if err := c.resume(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !canceled.Load() {
		t.Fatal("zero-lease resume fabricated a local extension instead of cancelling")
	}
	got, ok := c.registry.Get("s", "j", "a")
	if !ok {
		t.Fatal("attempt removed from registry")
	}
	if got.LeaseUntil != originalLease {
		t.Fatalf("lease rewritten to %d, want unchanged %d", got.LeaseUntil, originalLease)
	}
}

// TestResumeRejectedWinsOverResumed 覆盖同一 attempt 同时出现在 resumed/rejected：
// 拒绝是权威结论，绝不能复活已取消的执行。
func TestResumeRejectedWinsOverResumed(t *testing.T) {
	c, s := newUnitClient(t)
	var canceled atomic.Bool
	c.registry.Start(tasks.Attempt{SubmissionID: "s", JudgeTaskID: "j", AttemptID: "a", LeaseUntil: time.Now().Add(time.Minute).UnixMilli()}, func() { canceled.Store(true) })
	loadResumeResult(t, s, `{"resumed":[{"submissionId":"s","judgeTaskId":"j","attemptId":"a","leaseUntil":9999999999999}],"rejected":[{"submissionId":"s","judgeTaskId":"j","attemptId":"a","reason":"cancelled"}]}`)
	if err := c.resume(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !canceled.Load() {
		t.Fatal("conflicting rejected attempt was revived by resumed entry")
	}
}
