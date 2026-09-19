// Package resultqueue 是终态结果的本地有界持久队列：
// 先落盘（原子 + fsync、0600/current-user ACL），收到服务端 TASK_RESULT_ACK 后删除。
// 队列满或磁盘写失败时拒绝新结果并显式失败，绝不丢结果或无界等待。
package resultqueue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/criyle/go-judge/internal/hnieoj/securefile"
)

// Errors 供调用方区分队列满与磁盘故障。
var (
	ErrFull     = errors.New("result queue is full")
	ErrTooLarge = errors.New("result record exceeds size limit")
)

// Record 是一条待确认的终态结果。
type Record struct {
	ID           string          `json:"id"`
	SubmissionID string          `json:"submissionId"`
	JudgeTaskID  string          `json:"judgeTaskId"`
	AttemptID    string          `json:"attemptId"`
	Event        json.RawMessage `json:"event"`
	EnqueuedAt   int64           `json:"enqueuedAt"`
	Attempts     int             `json:"attempts"`
}

// Queue 是线程安全的持久结果队列。
type Queue struct {
	dir        string
	maxRecords int
	maxBytes   int64
	maxRecord  int64
	ttl        time.Duration

	mu      sync.Mutex
	records map[string]Record
	// reservations 为已接纳但尚未产生终态的 attempt 预留容量（record 数 + 字节预算），
	// 保证并发完成时每条已接纳任务都能落盘，不会挤爆有界队列。
	reservations map[string]struct{}
	bytes        int64
	// degraded 表示发生过磁盘写入失败：停止接受新结果与新的任务分配。
	degraded bool
}

// Open 打开（必要时创建）结果队列目录并加载已有记录。
func Open(dir string, maxRecords int, maxBytes int64, maxRecord int64, ttl time.Duration) (*Queue, error) {
	if maxRecords <= 0 {
		maxRecords = 256
	}
	if maxBytes <= 0 {
		maxBytes = 64 * 1024 * 1024
	}
	if maxRecord <= 0 {
		maxRecord = 4 * 1024 * 1024
	}
	q := &Queue{
		dir:          dir,
		maxRecords:   maxRecords,
		maxBytes:     maxBytes,
		maxRecord:    maxRecord,
		ttl:          ttl,
		records:      map[string]Record{},
		reservations: map[string]struct{}{},
	}
	if err := securefile.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := q.load(); err != nil {
		return nil, err
	}
	return q, nil
}

// maxRecordFileOverhead 是记录 JSON 包装（id/submissionId 等）允许的最大额外字节。
const maxRecordFileOverhead = 64 * 1024

func (q *Queue) load() error {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return err
	}
	// 先按目录条目统计数量/单文件大小，再决定是否读取，避免启动时无界读盘。
	// 单文件上限与运行时 Enqueue 的持久化字节检查一致：任何写入成功的记录都能重新打开。
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > q.maxRecord+maxRecordFileOverhead {
			return fmt.Errorf("result record %s exceeds size limit on load", entry.Name())
		}
		count++
		if count > q.maxRecords {
			return fmt.Errorf("result queue has %d records on load, exceeds max %d", count, q.maxRecords)
		}
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(q.dir, entry.Name()))
		if err != nil {
			return err
		}
		var rec Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			// 损坏记录不能静默丢弃：保留文件并报错，交由人工处理。
			return fmt.Errorf("corrupt result record %s: %w", entry.Name(), err)
		}
		if int64(len(rec.Event)) > q.maxRecord {
			return fmt.Errorf("result record %s event exceeds size limit", entry.Name())
		}
		q.records[rec.ID] = rec
		// 运行时字节预算针对 event 载荷；load 必须用同一口径累计，
		// 否则带 metadata 的文件会在重启时被判超限而打不开。
		q.bytes += int64(len(rec.Event))
		if q.bytes > q.maxBytes {
			return fmt.Errorf("result queue on load exceeds max bytes %d", q.maxBytes)
		}
	}
	return nil
}

// Enqueue 原子写入一条结果；重复 ID 视为同一终态重发（幂等）。
func (q *Queue) Enqueue(rec Record) error {
	if rec.ID == "" {
		return errors.New("result record id is required")
	}
	if rec.EnqueuedAt == 0 {
		rec.EnqueuedAt = time.Now().UnixMilli()
	}
	size := int64(len(rec.Event))
	if size > q.maxRecord {
		return ErrTooLarge
	}
	// 先按实际持久化字节编码：写入成功的文件必须满足 load 的单文件上限，
	// 否则重启会因 metadata 超限打不开队列。
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if int64(len(raw)) > q.maxRecord+maxRecordFileOverhead {
		return ErrTooLarge
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.degraded {
		return errors.New("result queue is degraded after a storage failure")
	}
	if existing, ok := q.records[rec.ID]; ok {
		// 同 ID 必须同内容；不同内容拒绝，避免静默替换终态。
		if string(existing.Event) != string(rec.Event) {
			return errors.New("result record with same id has conflicting content")
		}
		return nil
	}
	// 已预留容量的 attempt 把自己的预算转换成真实记录大小；未预留的记录按增量一起校验，
	// 两者都在同一临界区内原子判定，绝不挤占其他 attempt 的预算。
	_, reserved := q.reservations[rec.ID]
	if reserved {
		if q.admissionExceedsLocked(1, -1, size) {
			return ErrFull
		}
	} else if q.admissionExceedsLocked(1, 0, size) {
		return ErrFull
	}
	if err := q.writeRawLocked(rec, raw); err != nil {
		// 磁盘写失败必须显式降级：停止接受新结果与新任务，绝不静默丢弃。
		q.degraded = true
		return err
	}
	delete(q.reservations, rec.ID)
	q.records[rec.ID] = rec
	q.bytes += size
	return nil
}

// Reserve 为一个已接纳的 attempt 预留终态结果容量（一条记录 + maxRecord 字节预算）。
// 预留先验证 used + 所有既有预留 + 本次最坏预算 <= maxBytes，记录数同理；
// 队列满/降级时返回 ErrFull，调用方必须在启动任务前拒绝该次分配。
// 同一 id 幂等；已存在同 id 终态记录时视为已占用容量。
func (q *Queue) Reserve(id string) error {
	if id == "" {
		return errors.New("result reservation id is required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.degraded {
		return errors.New("result queue is degraded after a storage failure")
	}
	if _, ok := q.records[id]; ok {
		return nil
	}
	if _, ok := q.reservations[id]; ok {
		return nil
	}
	if q.admissionExceedsLocked(0, 1, 0) {
		return ErrFull
	}
	if q.reservations == nil {
		q.reservations = map[string]struct{}{}
	}
	q.reservations[id] = struct{}{}
	return nil
}

// Release 释放一个 attempt 的预留容量：任务被取消/未产生终态时调用。
// 预留已转换为终态记录或不存在时为 no-op，绝不删除已落盘结果；
// 重复调用幂等，同一 attempt 的预留只会被释放一次。
func (q *Queue) Release(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.reservations, id)
}

// Reserved 返回当前未转换的预留数量（测试与诊断用）。
func (q *Queue) Reserved() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.reservations)
}

// Peek 返回按入队时间排序的记录副本。
func (q *Queue) Peek() []Record {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Record, 0, len(q.records))
	for _, rec := range q.records {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EnqueuedAt != out[j].EnqueuedAt {
			return out[i].EnqueuedAt < out[j].EnqueuedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// MarkAttempt 记录一次投递尝试次数。
func (q *Queue) MarkAttempt(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	rec, ok := q.records[id]
	if !ok {
		return
	}
	rec.Attempts++
	q.records[id] = rec
}

// Ack 删除已确认的记录。
func (q *Queue) Ack(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	rec, ok := q.records[id]
	if !ok {
		return nil
	}
	if err := os.Remove(q.filePath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	delete(q.records, id)
	q.bytes -= int64(len(rec.Event))
	if q.bytes < 0 {
		q.bytes = 0
	}
	if err := securefile.SyncDir(q.dir); err != nil {
		return err
	}
	return nil
}
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.records)
}

// Full 表示队列是否已无法接受新结果（记录数/字节预算耗尽、有预留或已因磁盘故障降级）。
func (q *Queue) Full() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.fullLocked()
}

// fullLocked 报告当前是否已达到容量上限（无法再接纳新的 attempt/记录）。
// 每条预留按 maxRecord 字节预算计入，使已接纳任务的结果始终能落盘；
// degraded 则完全停止接纳。
func (q *Queue) fullLocked() bool {
	reserved := int64(len(q.reservations))
	return q.degraded ||
		int64(len(q.records))+reserved >= int64(q.maxRecords) ||
		q.bytes+reserved*q.maxRecord >= q.maxBytes
}

// admissionExceedsLocked 判断在现有用量上追加 extraRecords 条真实记录、
// 净增 extraReservations 条预留与 extraBytes 字节后是否会突破容量。
// 每条预留（含 extraReservations 带来的净增）占 maxRecord 字节预算；
// extraBytes 只用于已在预留预算之外的真实记录大小，调用方必须持有 q.mu。
func (q *Queue) admissionExceedsLocked(extraRecords, extraReservations int, extraBytes int64) bool {
	if q.degraded {
		return true
	}
	reserved := int64(len(q.reservations)) + int64(extraReservations)
	if reserved < 0 {
		reserved = 0
	}
	projectedRecords := int64(len(q.records)) + int64(len(q.reservations)) + int64(extraRecords) + int64(extraReservations)
	projectedBytes := q.bytes + reserved*q.maxRecord + extraBytes
	return projectedRecords > int64(q.maxRecords) || projectedBytes > q.maxBytes
}

// Degraded 表示发生过磁盘写失败，已停止接受新结果。
func (q *Queue) Degraded() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.degraded
}

// Prune 清理超过 TTL 的记录；终态结果过期属于异常，返回被清理数量由调用方告警。
func (q *Queue) Prune(now time.Time) (int, error) {
	if q.ttl <= 0 {
		return 0, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	removed := 0
	for id, rec := range q.records {
		if now.UnixMilli()-rec.EnqueuedAt <= q.ttl.Milliseconds() {
			continue
		}
		if err := os.Remove(q.filePath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, err
		}
		delete(q.records, id)
		q.bytes -= int64(len(rec.Event))
		removed++
	}
	if q.bytes < 0 {
		q.bytes = 0
	}
	if removed > 0 {
		return removed, securefile.SyncDir(q.dir)
	}
	return removed, nil
}

func (q *Queue) writeRawLocked(rec Record, raw []byte) error {
	// 统一走 securefile：写入前限制 ACL/继承，原子 + fsync rename。
	return securefile.WriteFileAtomic(q.filePath(rec.ID), raw, 0o600)
}

func (q *Queue) filePath(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(q.dir, hex.EncodeToString(sum[:])+".json")
}
