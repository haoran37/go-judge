// Package tasks 维护节点在途任务注册表：供重连后 RESUME_TASKS、TASK_CANCEL 取消、
// 以及防止同一 attempt 被重复执行。注册表只保存任务标识与取消函数，不保存任务内容。
package tasks

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Attempt 描述一个在途 attempt。
type Attempt struct {
	SubmissionID string `json:"submissionId"`
	JudgeTaskID  string `json:"judgeTaskId"`
	AttemptID    string `json:"attemptId"`
	LeaseUntil   int64  `json:"leaseUntil"`
	StartedAt    int64  `json:"startedAt"`
	// Epoch 是注册该 attempt 时的 WSS sessionEpoch，旧 epoch 的迟到回复不得复活任务。
	Epoch int64 `json:"-"`
}

// Key 构造 attempt 唯一键。
func Key(submissionID, judgeTaskID, attemptID string) string {
	return submissionID + "\x00" + judgeTaskID + "\x00" + attemptID
}

type entry struct {
	attempt Attempt
	cancel  context.CancelFunc
	// onLease 由执行器注册，RESUME/LEASE_RENEW 更新租约时同步到执行器内部 deadline。
	onLease func(int64)
}

// Registry 是并发安全的在途任务注册表。已完成的 attempt 会短暂保留，
// 使重复 TASK_ASSIGN 只被 ACK 而不会重复执行。
type Registry struct {
	mu      sync.Mutex
	entries map[string]*entry
	done    map[string]time.Time
	doneTTL time.Duration
	// doneObserver 在 attempt 被 Complete（执行器确认结束）后回调，供持有者释放按
	// attempt 预留的资源（例如持久结果队列容量）；回调在锁外执行，避免与队列锁互等。
	// Cancel/CancelAll 只取消 context，不回调，防止执行器仍在写终态时提前释放容量。
	doneObserver func(submissionID, judgeTaskID, attemptID string)
}

// DefaultDoneTTL 是已完成 attempt 的去重记忆窗口。
const DefaultDoneTTL = 30 * time.Minute

func NewRegistry() *Registry {
	return &Registry{entries: map[string]*entry{}, done: map[string]time.Time{}, doneTTL: DefaultDoneTTL}
}

// Has 判断 attempt 是否在途或最近已完成（重复分配必须只 ACK）。
func (r *Registry) Has(submissionID, judgeTaskID, attemptID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneDoneLocked(time.Now())
	key := Key(submissionID, judgeTaskID, attemptID)
	if _, ok := r.entries[key]; ok {
		return true
	}
	_, ok := r.done[key]
	return ok
}

// Start 注册一个 attempt 并保存其取消函数；返回 false 表示同 attempt 已在执行（不得重复执行）。
func (r *Registry) Start(attempt Attempt, cancel context.CancelFunc) bool {
	if attempt.StartedAt == 0 {
		attempt.StartedAt = time.Now().UnixMilli()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := Key(attempt.SubmissionID, attempt.JudgeTaskID, attempt.AttemptID)
	if _, exists := r.entries[key]; exists {
		return false
	}
	r.entries[key] = &entry{attempt: attempt, cancel: cancel}
	return true
}

// UpdateLease 更新 attempt 的租约截止，并同步给执行器内部 deadline。
func (r *Registry) UpdateLease(submissionID, judgeTaskID, attemptID string, leaseUntil int64) bool {
	r.mu.Lock()
	e, ok := r.entries[Key(submissionID, judgeTaskID, attemptID)]
	if !ok {
		r.mu.Unlock()
		return false
	}
	e.attempt.LeaseUntil = leaseUntil
	onLease := e.onLease
	r.mu.Unlock()
	if onLease != nil {
		onLease(leaseUntil)
	}
	return true
}

// Resume 把服务端批准的同一个 attempt 续到当前会话：更新租约、epoch，
// 并同步执行器 deadline，保证 registry 与执行器不再使用分离的旧 leaseState。
func (r *Registry) Resume(submissionID, judgeTaskID, attemptID string, leaseUntil, epoch int64) bool {
	r.mu.Lock()
	e, ok := r.entries[Key(submissionID, judgeTaskID, attemptID)]
	if !ok {
		r.mu.Unlock()
		return false
	}
	e.attempt.LeaseUntil = leaseUntil
	e.attempt.Epoch = epoch
	onLease := e.onLease
	r.mu.Unlock()
	if onLease != nil {
		onLease(leaseUntil)
	}
	return true
}

// SetLeaseObserver 允许执行器登记租约更新回调；attempt 不在途时返回 false。
func (r *Registry) SetLeaseObserver(submissionID, judgeTaskID, attemptID string, fn func(int64)) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[Key(submissionID, judgeTaskID, attemptID)]
	if !ok {
		return false
	}
	e.onLease = fn
	return true
}

// Epoch 返回指定 attempt 注册时的 sessionEpoch。
func (r *Registry) Epoch(submissionID, judgeTaskID, attemptID string) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[Key(submissionID, judgeTaskID, attemptID)]
	if !ok {
		return 0, false
	}
	return e.attempt.Epoch, true
}

// Get 返回在途 attempt 的最新快照。
func (r *Registry) Get(submissionID, judgeTaskID, attemptID string) (Attempt, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[Key(submissionID, judgeTaskID, attemptID)]
	if !ok {
		return Attempt{}, false
	}
	return e.attempt, true
}

// SetDoneObserver 注册 attempt 被 Complete 后的回调；传 nil 取消注册。
// Cancel/CancelAll 不触发该回调，释放由执行器结束时的 Complete 负责。
func (r *Registry) SetDoneObserver(fn func(submissionID, judgeTaskID, attemptID string)) {
	r.mu.Lock()
	r.doneObserver = fn
	r.mu.Unlock()
}

// Cancel 取消匹配 attempt 的 context；仅匹配 attempt，绝不误伤其他任务。
// 这里刻意不触发 doneObserver：取消只代表 context 被取消，执行器可能仍在写终态结果；
// 提前释放预留会让其他 attempt 借走容量。真正离开在途表的 Complete 才回调释放。
func (r *Registry) Cancel(submissionID, judgeTaskID, attemptID string) bool {
	r.mu.Lock()
	e, ok := r.entries[Key(submissionID, judgeTaskID, attemptID)]
	r.mu.Unlock()
	if !ok {
		return false
	}
	if e.cancel != nil {
		e.cancel()
	}
	return true
}

// Complete 移除已结束的 attempt，并在 TTL 内记住它以防止重复分配被再次执行。
func (r *Registry) Complete(submissionID, judgeTaskID, attemptID string) {
	r.mu.Lock()
	key := Key(submissionID, judgeTaskID, attemptID)
	delete(r.entries, key)
	r.done[key] = time.Now()
	r.pruneDoneLocked(time.Now())
	observer := r.doneObserver
	r.mu.Unlock()
	if observer != nil {
		observer(submissionID, judgeTaskID, attemptID)
	}
}

func (r *Registry) pruneDoneLocked(now time.Time) {
	if r.doneTTL <= 0 {
		r.doneTTL = DefaultDoneTTL
	}
	for key, at := range r.done {
		if now.Sub(at) > r.doneTTL {
			delete(r.done, key)
		}
	}
}

// Snapshot 返回在途 attempt 列表（按标识排序，便于测试与 RESUME）。
func (r *Registry) Snapshot() []Attempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Attempt, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.attempt)
	}
	sort.Slice(out, func(i, j int) bool {
		return Key(out[i].SubmissionID, out[i].JudgeTaskID, out[i].AttemptID) <
			Key(out[j].SubmissionID, out[j].JudgeTaskID, out[j].AttemptID)
	})
	return out
}

// Len 返回在途数量。
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// CancelAll 取消所有在途任务（身份撤销/服务端拒绝时使用）。
// 与 Cancel 一样不触发 doneObserver：释放由每个执行器结束时的 Complete 负责，
// 避免取消瞬间就把仍可能写终态结果的预留容量让给其他 attempt。
func (r *Registry) CancelAll() {
	r.mu.Lock()
	type pending struct {
		cancel context.CancelFunc
	}
	items := make([]pending, 0, len(r.entries))
	for _, e := range r.entries {
		items = append(items, pending{cancel: e.cancel})
	}
	r.mu.Unlock()
	for _, item := range items {
		if item.cancel != nil {
			item.cancel()
		}
	}
}
