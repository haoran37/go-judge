package tasks

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestRegistryTracksAndCancelsAttempt(t *testing.T) {
	r := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !r.Start(Attempt{SubmissionID: "s", JudgeTaskID: "j", AttemptID: "a", LeaseUntil: 1}, cancel) {
		t.Fatal("first start should succeed")
	}
	if r.Start(Attempt{SubmissionID: "s", JudgeTaskID: "j", AttemptID: "a"}, func() {}) {
		t.Fatal("duplicate start must be rejected")
	}
	if !r.Has("s", "j", "a") {
		t.Fatal("attempt should be present")
	}
	if !r.UpdateLease("s", "j", "a", 42) {
		t.Fatal("update lease failed")
	}
	snapshot := r.Snapshot()
	if len(snapshot) != 1 || snapshot[0].LeaseUntil != 42 {
		t.Fatalf("snapshot %+v", snapshot)
	}
	if !r.Cancel("s", "j", "a") {
		t.Fatal("cancel failed")
	}
	if ctx.Err() == nil {
		t.Fatal("cancel should cancel the context")
	}
	r.Complete("s", "j", "a")
	if r.Len() != 0 {
		t.Fatalf("len after complete = %d", r.Len())
	}
	// 完成后的重复分配仍应被识别，避免重复执行。
	if !r.Has("s", "j", "a") {
		t.Fatal("completed attempt should be remembered")
	}
}

func TestCancelAllCancelsEveryAttempt(t *testing.T) {
	r := NewRegistry()
	var count atomic.Int32
	for _, id := range []string{"a", "b", "c"} {
		ctx, cancel := context.WithCancel(context.Background())
		_ = ctx
		r.Start(Attempt{SubmissionID: "s", JudgeTaskID: "j", AttemptID: id}, func() { count.Add(1); cancel() })
	}
	r.CancelAll()
	if count.Load() != 3 {
		t.Fatalf("cancelled %d, want 3", count.Load())
	}
}

func TestCancelOnlyMatchingAttempt(t *testing.T) {
	r := NewRegistry()
	var otherCancelled atomic.Bool
	_, otherCancel := context.WithCancel(context.Background())
	_ = otherCancel
	r.Start(Attempt{SubmissionID: "s", JudgeTaskID: "j", AttemptID: "a"}, func() { otherCancelled.Store(true) })
	if r.Cancel("s", "j", "wrong") {
		t.Fatal("cancelling unknown attempt must not match")
	}
	if otherCancelled.Load() {
		t.Fatal("wrong attempt was cancelled")
	}
}
