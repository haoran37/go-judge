package worker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/gateway"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

type generatingClaimer struct {
	seq atomic.Int64
}

func (g *generatingClaimer) Claim(ctx context.Context) (*gateway.Claim, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	n := g.seq.Add(1)
	return &gateway.Claim{
		Task:             model.Task{SubmissionID: fmt.Sprintf("sub-%d", n), JudgeTaskID: "task"},
		AttemptID:        fmt.Sprintf("attempt-%d", n),
		LeaseUntil:       time.Now().Add(time.Hour).UnixMilli(),
		RenewAfterMillis: 3_600_000,
	}, nil
}

type sliceClaimer struct {
	mu     sync.Mutex
	claims []*gateway.Claim
	idx    int
}

func (s *sliceClaimer) Claim(ctx context.Context) (*gateway.Claim, error) {
	s.mu.Lock()
	if s.idx < len(s.claims) {
		claim := s.claims[s.idx]
		s.idx++
		s.mu.Unlock()
		return claim, nil
	}
	s.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

type fakeRenewer struct {
	mu    sync.Mutex
	calls int
	deny  bool
}

func (r *fakeRenewer) Renew(ctx context.Context, submissionID, judgeTaskID, attemptID string) (int64, error) {
	r.mu.Lock()
	r.calls++
	deny := r.deny
	r.mu.Unlock()
	if deny {
		return 0, &auth.DeniedError{StatusCode: 403}
	}
	return time.Now().Add(time.Hour).UnixMilli(), nil
}

func (r *fakeRenewer) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestPoolBoundsConcurrencyToSlots(t *testing.T) {
	var mu sync.Mutex
	active, peak := 0, 0
	handler := func(ctx context.Context, task model.Task) error {
		mu.Lock()
		active++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		select {
		case <-time.After(30 * time.Millisecond):
		case <-ctx.Done():
		}
		mu.Lock()
		active--
		mu.Unlock()
		return nil
	}
	pool := New(&generatingClaimer{}, &fakeRenewer{}, handler, logging.NopLogger{}, Options{
		Slots: 3, EmptyMinBackoff: time.Millisecond, EmptyMaxBackoff: 5 * time.Millisecond, DrainTimeout: 2 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()
	time.Sleep(250 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if peak != 3 {
		t.Fatalf("peak concurrency = %d, want exactly 3 (bounded by slots)", peak)
	}
}

func TestPoolGracefullyDrainsInFlightTask(t *testing.T) {
	claim := &gateway.Claim{
		Task:             model.Task{SubmissionID: "sub-1", JudgeTaskID: "task-1"},
		AttemptID:        "attempt-1",
		LeaseUntil:       time.Now().Add(time.Hour).UnixMilli(),
		RenewAfterMillis: 3_600_000,
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var canceled atomic.Bool
	handler := func(ctx context.Context, task model.Task) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			canceled.Store(true)
			return ctx.Err()
		}
	}
	pool := New(&sliceClaimer{claims: []*gateway.Claim{claim}}, &fakeRenewer{}, handler, logging.NopLogger{}, Options{
		Slots: 1, EmptyMinBackoff: time.Hour, EmptyMaxBackoff: time.Hour, DrainTimeout: 2 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	<-started
	cancel() // SIGTERM 语义：停止领取，开始排空
	time.Sleep(50 * time.Millisecond)
	if canceled.Load() {
		t.Fatal("in-flight task must not be canceled during graceful drain")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not finish draining")
	}
	if canceled.Load() {
		t.Fatal("task was canceled unexpectedly")
	}
	if !pool.Draining() {
		t.Fatal("pool should be draining after shutdown")
	}
}

func TestPoolIdleClaimCannotHangShutdown(t *testing.T) {
	pool := New(&sliceClaimer{}, &fakeRenewer{}, func(context.Context, model.Task) error {
		return nil
	}, logging.NopLogger{}, Options{
		Slots: 1, EmptyMinBackoff: time.Hour, EmptyMaxBackoff: time.Hour, DrainTimeout: 2 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle claim request hung shutdown")
	}
}

func TestPoolCancelsTaskWhenLeaseDenied(t *testing.T) {
	claim := &gateway.Claim{
		Task:             model.Task{SubmissionID: "sub-1", JudgeTaskID: "task-1"},
		AttemptID:        "attempt-1",
		LeaseUntil:       time.Now().Add(500 * time.Millisecond).UnixMilli(),
		RenewAfterMillis: 10,
	}
	handlerCanceled := make(chan struct{})
	handler := func(ctx context.Context, task model.Task) error {
		<-ctx.Done()
		close(handlerCanceled)
		return ctx.Err()
	}
	pool := New(&sliceClaimer{claims: []*gateway.Claim{claim}}, &fakeRenewer{deny: true}, handler, logging.NopLogger{}, Options{
		Slots: 1, EmptyMinBackoff: time.Hour, EmptyMaxBackoff: time.Hour, DrainTimeout: 2 * time.Second, RenewRetryBackoff: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	select {
	case <-handlerCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("denied renewal should cancel task context when ownership lost")
	}
	cancel()
	<-done
}

// blockingLeaseGateway 的 Claim 返回短租约，Renew 一直阻塞直到请求上下文被取消。
type blockingLeaseGateway struct {
	claimed atomic.Bool
}

func (g *blockingLeaseGateway) Claim(ctx context.Context) (*gateway.Claim, error) {
	if g.claimed.Swap(true) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &gateway.Claim{
		Task:             model.Task{SubmissionID: "sub-1", JudgeTaskID: "task-1"},
		AttemptID:        "attempt-1",
		LeaseUntil:       time.Now().Add(100 * time.Millisecond).UnixMilli(),
		RenewAfterMillis: 10,
	}, nil
}

func (g *blockingLeaseGateway) Renew(ctx context.Context, _, _, _ string) (int64, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestPoolRenewalRequestCannotOutliveLease(t *testing.T) {
	gatewayClient := &blockingLeaseGateway{}
	handlerCanceled := make(chan struct{})
	handler := func(ctx context.Context, _ model.Task) error {
		<-ctx.Done()
		close(handlerCanceled)
		return ctx.Err()
	}
	pool := New(gatewayClient, gatewayClient, handler, logging.NopLogger{}, Options{
		Slots: 1, EmptyMinBackoff: time.Hour, EmptyMaxBackoff: time.Hour, DrainTimeout: 100 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	select {
	case <-handlerCanceled:
	case <-time.After(350 * time.Millisecond):
		t.Fatal("blocked renewal kept processor alive past the lease deadline")
	}
}

func TestPoolSkipsAlreadyExpiredClaim(t *testing.T) {
	claim := &gateway.Claim{
		Task:             model.Task{SubmissionID: "sub-1", JudgeTaskID: "task-1"},
		AttemptID:        "attempt-1",
		LeaseUntil:       time.Now().Add(-time.Second).UnixMilli(),
		RenewAfterMillis: 10,
	}
	var handled atomic.Bool
	handler := func(context.Context, model.Task) error {
		handled.Store(true)
		return nil
	}
	pool := New(&sliceClaimer{claims: []*gateway.Claim{claim}}, &fakeRenewer{}, handler, logging.NopLogger{}, Options{
		Slots: 1, EmptyMinBackoff: time.Hour, EmptyMaxBackoff: time.Hour, DrainTimeout: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if handled.Load() {
		t.Fatal("already-expired claim must not be handed to the processor")
	}
}

// cancelObservingRenewer 记录续期请求何时观察到上下文取消。
type cancelObservingRenewer struct {
	started    chan struct{}
	canceled   chan struct{}
	startOnce  sync.Once
	cancelOnce sync.Once
}

func (r *cancelObservingRenewer) Renew(ctx context.Context, _, _, _ string) (int64, error) {
	r.startOnce.Do(func() { close(r.started) })
	<-ctx.Done()
	r.cancelOnce.Do(func() { close(r.canceled) })
	return 0, ctx.Err()
}

func TestPoolCompletionCancelsInflightRenewal(t *testing.T) {
	claim := &gateway.Claim{
		Task:             model.Task{SubmissionID: "sub-1", JudgeTaskID: "task-1"},
		AttemptID:        "attempt-1",
		LeaseUntil:       time.Now().Add(time.Hour).UnixMilli(),
		RenewAfterMillis: 1,
	}
	renewer := &cancelObservingRenewer{started: make(chan struct{}), canceled: make(chan struct{})}
	handler := func(ctx context.Context, _ model.Task) error {
		// 等续期请求真正在途后再结束任务。
		select {
		case <-renewer.started:
		case <-time.After(time.Second):
			return context.DeadlineExceeded
		}
		return nil
	}
	pool := New(&sliceClaimer{claims: []*gateway.Claim{claim}}, renewer, handler, logging.NopLogger{}, Options{
		Slots: 1, EmptyMinBackoff: time.Hour, EmptyMaxBackoff: time.Hour, DrainTimeout: 5 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	select {
	case <-renewer.canceled:
	case <-time.After(time.Second):
		t.Fatal("task completion did not promptly cancel the in-flight renewal request")
	}
	cancel()
	<-done
}
