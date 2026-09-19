package resultqueue

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func record(id string, size int) Record {
	event := `{"id":"` + id + `","pad":"` + strings.Repeat("x", size) + `"}`
	return Record{ID: id, SubmissionID: "s" + id, JudgeTaskID: "j", AttemptID: "a", Event: json.RawMessage(event)}
}

func TestEnqueueAckAndReload(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(dir, 8, 1<<20, 1<<20, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := q.Enqueue(record("one", 16)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := q.Enqueue(record("two", 16)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if q.Len() != 2 {
		t.Fatalf("len = %d", q.Len())
	}
	// 重启后结果必须仍在队列中。
	reopened, err := Open(dir, 8, 1<<20, 1<<20, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Len() != 2 {
		t.Fatalf("reopened len = %d", reopened.Len())
	}
	if err := reopened.Ack("one"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if reopened.Len() != 1 {
		t.Fatalf("after ack len = %d", reopened.Len())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file after ack, got %d", len(entries))
	}
}

func TestEnqueueRejectsConflictingContentAndOversize(t *testing.T) {
	q, err := Open(t.TempDir(), 8, 1<<20, 256, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := q.Enqueue(record("same", 8)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// 同 ID 同内容幂等。
	if err := q.Enqueue(record("same", 8)); err != nil {
		t.Fatalf("idempotent enqueue: %v", err)
	}
	conflict := Record{ID: "same", SubmissionID: "s", JudgeTaskID: "j", AttemptID: "a", Event: json.RawMessage("different")}
	if err := q.Enqueue(conflict); err == nil {
		t.Fatal("conflicting content accepted")
	}
	if err := q.Enqueue(record("too-big", 512)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestQueueFullStopsNewResults(t *testing.T) {
	q, err := Open(t.TempDir(), 1, 1<<20, 1<<20, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := q.Enqueue(record("a", 8)); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if !q.Full() {
		t.Fatal("queue should report full")
	}
	if err := q.Enqueue(record("b", 8)); !errors.Is(err, ErrFull) {
		t.Fatalf("expected ErrFull, got %v", err)
	}
}

func TestPruneRemovesExpiredRecords(t *testing.T) {
	q, err := Open(t.TempDir(), 8, 1<<20, 1<<20, time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	rec := record("old", 8)
	rec.EnqueuedAt = time.Now().Add(-2 * time.Hour).UnixMilli()
	if err := q.Enqueue(rec); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	removed, err := q.Prune(time.Now())
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 || q.Len() != 0 {
		t.Fatalf("prune removed=%d len=%d", removed, q.Len())
	}
}

func TestStorageFailureDegradesQueueAndStopsAccepting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "results")
	q, err := Open(dir, 8, 1<<20, 1<<20, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// 用同名文件替换目录，强制真实磁盘写入失败。
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove dir: %v", err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatalf("replace dir: %v", err)
	}
	if err := q.Enqueue(record("fail", 8)); err == nil {
		t.Fatal("expected storage failure to surface")
	}
	if !q.Degraded() {
		t.Fatal("queue not marked degraded after storage failure")
	}
	if !q.Full() {
		t.Fatal("degraded queue must report full so new tasks stop")
	}
	if err := q.Enqueue(record("after", 8)); err == nil {
		t.Fatal("degraded queue accepted a new result")
	}
}

func TestReservationKeepsAdmittedResultsWritableUnderConcurrentCompletion(t *testing.T) {
	q, err := Open(t.TempDir(), 2, 1<<20, 1<<16, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// 两个在途 attempt 先预留容量；第三个因 record 预算耗尽被拒绝。
	for _, id := range []string{"a", "b"} {
		if err := q.Reserve(id); err != nil {
			t.Fatalf("reserve %s: %v", id, err)
		}
	}
	if err := q.Reserve("c"); !errors.Is(err, ErrFull) {
		t.Fatalf("over-capacity reservation error = %v", err)
	}
	// 并发完成：已预留的两条结果都必须能落盘，不会互相挤占。
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			errs <- q.Enqueue(record(id, 8))
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("reserved enqueue failed: %v", err)
		}
	}
	if q.Len() != 2 {
		t.Fatalf("len = %d, want 2", q.Len())
	}
	if q.Reserved() != 0 {
		t.Fatalf("reservations not converted: %d", q.Reserved())
	}
	// 预留已全部转换为记录，容量仍满，新的接纳必须被拒绝。
	if err := q.Reserve("d"); !errors.Is(err, ErrFull) {
		t.Fatalf("expected ErrFull after conversion, got %v", err)
	}
}

func TestReleaseFreesReservationWithoutTouchingRecords(t *testing.T) {
	q, err := Open(t.TempDir(), 1, 1<<20, 1<<16, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := q.Reserve("a"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if !q.Full() {
		t.Fatal("reserved capacity must report full")
	}
	q.Release("a")
	if q.Full() {
		t.Fatal("released reservation must free capacity")
	}
	if err := q.Enqueue(record("a", 8)); err != nil {
		t.Fatalf("enqueue after release: %v", err)
	}
	// Release 不得删除已落盘记录。
	q.Release("a")
	if q.Len() != 1 {
		t.Fatalf("release removed a persisted record: len=%d", q.Len())
	}
}

func TestFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits only; Windows uses current-user ACL")
	}
	dir := filepath.Join(t.TempDir(), "results")
	q, err := Open(dir, 8, 1<<20, 1<<20, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := q.Enqueue(record("perm", 8)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("entries %d", len(entries))
	}
	fi, err := os.Stat(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("record perm = %04o, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if di.Mode().Perm()&0o077 != 0 {
		t.Fatalf("queue dir perm = %04o", di.Mode().Perm())
	}
}
