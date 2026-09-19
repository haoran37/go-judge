package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

type fakeClaimer struct {
	ch chan *model.Assignment
}

func (f *fakeClaimer) Claim(ctx context.Context) (*model.Assignment, error) {
	select {
	case a := <-f.ch:
		return a, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type fakeRenewer struct {
	mu    sync.Mutex
	lease int64
	err   error
	calls atomic.Int32
	delay time.Duration
}

func (f *fakeRenewer) Renew(ctx context.Context, _, _, _ string) (int64, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	return f.lease, nil
}

func assignment(submissionID string, lease time.Duration) *model.Assignment {
	return &model.Assignment{
		Task:             model.Task{SubmissionID: submissionID, JudgeTaskID: "j-" + submissionID},
		AttemptID:        "a-" + submissionID,
		LeaseUntil:       time.Now().Add(lease).UnixMilli(),
		RenewAfterMillis: 20,
	}
}

func TestPoolBoundsConcurrencyToSlots(t *testing.T) {
	claimer := &fakeClaimer{ch: make(chan *model.Assignment, 8)}
	renewer := &fakeRenewer{lease: time.Now().Add(time.Minute).UnixMilli()}
	var active, maxActive, handled atomic.Int32
	handler := func(ctx context.Context, task model.Task) error {
		current := active.Add(1)
		for {
			old := maxActive.Load()
			if current <= old || maxActive.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		active.Add(-1)
		handled.Add(1)
		return nil
	}
	pool := New(claimer, renewer, handler, logging.NopLogger{}, Options{Slots: 2, EmptyMinBackoff: time.Millisecond, EmptyMaxBackoff: 5 * time.Millisecond, DrainTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = pool.Run(ctx); close(done) }()
	for i := 0; i < 4; i++ {
		claimer.ch <- assignment(string(rune('a'+i)), time.Minute)
	}
	deadline := time.Now().Add(3 * time.Second)
	for handled.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if handled.Load() != 4 {
		t.Fatalf("handled %d, want 4", handled.Load())
	}
	if maxActive.Load() > 2 {
		t.Fatalf("max concurrency %d exceeded slots", maxActive.Load())
	}
}

func TestPoolGracefullyDrainsInFlightTask(t *testing.T) {
	claimer := &fakeClaimer{ch: make(chan *model.Assignment, 1)}
	renewer := &fakeRenewer{lease: time.Now().Add(time.Minute).UnixMilli()}
	started := make(chan struct{})
	release := make(chan struct{})
	handler := func(ctx context.Context, task model.Task) error {
		close(started)
		<-release
		return nil
	}
	pool := New(claimer, renewer, handler, logging.NopLogger{}, Options{Slots: 1, DrainTimeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = pool.Run(ctx); close(done) }()
	claimer.ch <- assignment("drain", time.Minute)
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("drain returned before in-flight task finished")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not finish after task released")
	}
}

func TestPoolCancelsTaskWhenLeaseDenied(t *testing.T) {
	claimer := &fakeClaimer{ch: make(chan *model.Assignment, 1)}
	renewer := &fakeRenewer{err: &auth.DeniedError{StatusCode: 403}}
	var cancelled atomic.Bool
	handler := func(ctx context.Context, task model.Task) error {
		<-ctx.Done()
		cancelled.Store(true)
		return ctx.Err()
	}
	pool := New(claimer, renewer, handler, logging.NopLogger{}, Options{
		Slots: 1, RenewRetryBackoff: 10 * time.Millisecond, DrainTimeout: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = pool.Run(ctx); close(done) }()
	claimer.ch <- assignment("denied", 30*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for !cancelled.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !cancelled.Load() {
		t.Fatal("task context was not cancelled after lease denial")
	}
	cancel()
	<-done
}

func TestPoolSkipsAlreadyExpiredClaim(t *testing.T) {
	claimer := &fakeClaimer{ch: make(chan *model.Assignment, 1)}
	renewer := &fakeRenewer{}
	var called atomic.Bool
	handler := func(ctx context.Context, task model.Task) error {
		called.Store(true)
		return nil
	}
	pool := New(claimer, renewer, handler, logging.NopLogger{}, Options{Slots: 1, DrainTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = pool.Run(ctx) }()
	expired := &model.Assignment{
		Task:       model.Task{SubmissionID: "expired", JudgeTaskID: "j"},
		AttemptID:  "a",
		LeaseUntil: time.Now().Add(-time.Second).UnixMilli(),
	}
	claimer.ch <- expired
	time.Sleep(200 * time.Millisecond)
	if called.Load() {
		t.Fatal("expired claim must not be executed")
	}
}
