// Package worker 实现有界槽位任务领取：每个槽位一次只领取一个任务，
// 空队列有上限的指数退避加抖动，SIGTERM 后停止领取但在有界窗口内排空在途任务。
package worker

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/auth"
	"github.com/criyle/go-judge/internal/hnieoj/gateway"
	"github.com/criyle/go-judge/internal/hnieoj/logging"
	"github.com/criyle/go-judge/internal/hnieoj/model"
)

const defaultRenewAfterMillis = 20 * time.Second

// Claimer 每次至多返回一个任务；无任务时返回 (nil, nil)。
type Claimer interface {
	Claim(ctx context.Context) (*gateway.Claim, error)
}

// Renewer 续租当前任务所有权。
type Renewer interface {
	Renew(ctx context.Context, submissionID, judgeTaskID, attemptID string) (int64, error)
}

// Handler 处理一个任务；ctx 在租约丢失或排空超时时被取消。
type Handler func(ctx context.Context, task model.Task) error

type Options struct {
	Slots             int
	EmptyMinBackoff   time.Duration
	EmptyMaxBackoff   time.Duration
	DrainTimeout      time.Duration
	RenewRetryBackoff time.Duration
}

type Pool struct {
	slots             int
	claimer           Claimer
	renewer           Renewer
	handler           Handler
	logger            logging.Logger
	emptyMin          time.Duration
	emptyMax          time.Duration
	drainTimeout      time.Duration
	renewRetryBackoff time.Duration

	draining atomic.Bool
	active   atomic.Int64
	randMu   sync.Mutex
	rand     *rand.Rand
}

func New(claimer Claimer, renewer Renewer, handler Handler, logger logging.Logger, opts Options) *Pool {
	if opts.Slots <= 0 {
		opts.Slots = 1
	}
	if opts.EmptyMinBackoff <= 0 {
		opts.EmptyMinBackoff = 200 * time.Millisecond
	}
	if opts.EmptyMaxBackoff < opts.EmptyMinBackoff {
		opts.EmptyMaxBackoff = 5 * time.Second
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = 5 * time.Minute
	}
	if opts.RenewRetryBackoff <= 0 {
		opts.RenewRetryBackoff = 2 * time.Second
	}
	return &Pool{
		slots:             opts.Slots,
		claimer:           claimer,
		renewer:           renewer,
		handler:           handler,
		logger:            logger,
		emptyMin:          opts.EmptyMinBackoff,
		emptyMax:          opts.EmptyMaxBackoff,
		drainTimeout:      opts.DrainTimeout,
		renewRetryBackoff: opts.RenewRetryBackoff,
		rand:              rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Active 返回当前在途任务计数，供心跳上报。
func (p *Pool) Active() *atomic.Int64 {
	return &p.active
}

func (p *Pool) Draining() bool {
	return p.draining.Load()
}

// Run 启动有界槽位并阻塞，直到 ctx 取消且排空窗口结束（或超时取消在途任务）。
func (p *Pool) Run(ctx context.Context) error {
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	claimCtx, cancelClaim := context.WithCancel(runCtx)
	defer cancelClaim()

	var wg sync.WaitGroup
	for i := 0; i < p.slots; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.worker(runCtx, claimCtx)
		}()
	}

	<-ctx.Done()
	// 停止新领取并中断空闲 claim 请求，避免 shutdown 被无任务的请求挂住。
	p.draining.Store(true)
	cancelClaim()

	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(p.drainTimeout):
		// 排空超时：取消在途任务，服务器会在租约到期后回收。
		p.logger.Warn("drain timeout exceeded, cancelling in-flight tasks", logging.String("drainTimeout", p.drainTimeout.String()))
		cancelRun()
		<-drained
	}
	return ctx.Err()
}

func (p *Pool) worker(runCtx, claimCtx context.Context) {
	backoff := p.emptyMin
	for {
		if claimCtx.Err() != nil || p.draining.Load() {
			return
		}
		claim, err := p.claimer.Claim(claimCtx)
		if err != nil {
			if claimCtx.Err() != nil || p.draining.Load() {
				return
			}
			if auth.IsDenied(err) {
				p.logger.Warn("task claim denied, stopping worker", logging.Error(err))
				return
			}
			p.logger.Warn("task claim failed", logging.Error(err))
			if !p.sleepWithJitter(claimCtx, &backoff) {
				return
			}
			continue
		}
		if claim == nil {
			if !p.sleepWithJitter(claimCtx, &backoff) {
				return
			}
			continue
		}
		backoff = p.emptyMin
		p.runTask(runCtx, claim)
	}
}

func (p *Pool) sleepWithJitter(ctx context.Context, backoff *time.Duration) bool {
	delay := p.jitter(*backoff)
	if *backoff < p.emptyMax {
		*backoff *= 2
		if *backoff > p.emptyMax {
			*backoff = p.emptyMax
		}
	}
	return sleepContext(ctx, delay)
}

func (p *Pool) jitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	p.randMu.Lock()
	// 在 [base/2, base] 区间抖动，避免所有槽位同时轮询。
	delta := base / 2
	if delta <= 0 {
		p.randMu.Unlock()
		return base
	}
	n := p.rand.Int63n(int64(delta) + 1)
	p.randMu.Unlock()
	return base - delta + time.Duration(n)
}

func (p *Pool) runTask(runCtx context.Context, claim *gateway.Claim) {
	task := claim.Task
	task.AttemptID = claim.AttemptID
	lease := &leaseState{untilMillis: claim.LeaseUntil}
	// 领取时租约已过期：所有权不可信，不启动处理，也不允许迟到的续期响应复活它。
	if lease.expired(time.Now()) {
		p.logger.Warn("claim lease already expired, skipping task",
			logging.String("submissionId", task.SubmissionID), logging.String("judgeTaskId", task.JudgeTaskID))
		return
	}

	taskCtx, taskCancel := context.WithCancel(runCtx)
	defer taskCancel()

	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		p.renewLoop(taskCtx, taskCancel, claim, lease)
	}()

	p.active.Add(1)
	err := p.handler(taskCtx, task)
	p.active.Add(-1)

	// 任务结束立即取消续期请求与协程，避免请求活过任务或租约。
	taskCancel()
	<-renewDone

	if lease.lost() {
		p.logger.Warn("task lease lost, stopped without final confirmation",
			logging.String("submissionId", task.SubmissionID), logging.String("judgeTaskId", task.JudgeTaskID))
		return
	}
	if err != nil {
		p.logger.Warn("task processing failed",
			logging.String("submissionId", task.SubmissionID), logging.String("judgeTaskId", task.JudgeTaskID), logging.Error(err))
	}
}

func (p *Pool) renewLoop(taskCtx context.Context, cancel context.CancelFunc, claim *gateway.Claim, lease *leaseState) {
	period := time.Duration(claim.RenewAfterMillis) * time.Millisecond
	if period <= 0 {
		period = defaultRenewAfterMillis
	}
	for {
		if taskCtx.Err() != nil {
			return
		}
		if wait := p.nextRenewDelay(lease, period); wait > 0 {
			if !sleepContext(taskCtx, wait) {
				return
			}
		}
		if taskCtx.Err() != nil {
			return
		}
		if !p.renewWithRetry(taskCtx, claim, lease) {
			if taskCtx.Err() != nil {
				return
			}
			lease.markLost()
			cancel()
			return
		}
	}
}

func (p *Pool) nextRenewDelay(lease *leaseState, period time.Duration) time.Duration {
	delay := period
	if deadline := lease.deadline(); !deadline.IsZero() {
		safety := p.renewRetryBackoff
		if half := period / 2; safety > half {
			safety = half
		}
		limit := time.Until(deadline) - safety
		if limit < delay {
			delay = limit
		}
	}
	if delay < 0 {
		return 0
	}
	return delay
}

// renewWithRetry 只在本地已知租约有效期内重试网络失败；请求上下文绑定当前租约截止时间，
// 任务完成或租约到期都会立即取消请求。被拒绝或租约耗尽立即返回 false。
func (p *Pool) renewWithRetry(taskCtx context.Context, claim *gateway.Claim, lease *leaseState) bool {
	for {
		if taskCtx.Err() != nil {
			return false
		}
		deadline := lease.deadline()
		if deadline.IsZero() || !time.Now().Before(deadline) {
			return false
		}
		reqCtx, cancelReq := context.WithDeadline(taskCtx, deadline)
		until, err := p.renewer.Renew(reqCtx, claim.Task.SubmissionID, claim.Task.JudgeTaskID, claim.AttemptID)
		cancelReq()
		if err == nil {
			// 迟到的成功响应不得复活已经过期的租约。
			if until <= 0 || !time.Now().Before(deadline) {
				p.logger.Warn("task lease renewal returned after lease deadline",
					logging.String("submissionId", claim.Task.SubmissionID))
				return false
			}
			lease.update(until)
			p.logger.Info("task lease renewed",
				logging.String("submissionId", claim.Task.SubmissionID), logging.Int64("leaseUntil", until))
			return true
		}
		if auth.IsDenied(err) {
			p.logger.Warn("task lease renewal denied",
				logging.String("submissionId", claim.Task.SubmissionID), logging.Error(err))
			return false
		}
		remaining := time.Until(lease.deadline())
		if remaining <= 0 {
			p.logger.Warn("task lease expired before renewal succeeded",
				logging.String("submissionId", claim.Task.SubmissionID), logging.Error(err))
			return false
		}
		wait := p.renewRetryBackoff
		if wait > remaining {
			wait = remaining
		}
		if !sleepContext(taskCtx, wait) {
			return false
		}
	}
}

type leaseState struct {
	mu          sync.Mutex
	untilMillis int64
	lostFlag    bool
}

func (l *leaseState) update(untilMillis int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.untilMillis = untilMillis
}

func (l *leaseState) deadline() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.untilMillis <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(l.untilMillis)
}

func (l *leaseState) expired(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.untilMillis <= 0 {
		return true
	}
	return !now.Before(time.UnixMilli(l.untilMillis))
}

func (l *leaseState) markLost() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lostFlag = true
}

func (l *leaseState) lost() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lostFlag
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
