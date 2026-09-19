package resultqueue

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// focusedEventSize 生成总长度恰好 n 字节的 event JSON（至少 8 字节）。
func focusedEventSize(n int) json.RawMessage {
	if n < 8 {
		n = 8
	}
	return json.RawMessage(`{"x":"` + strings.Repeat("a", n-8) + `"}`)
}

// 剩余字节非 0 但不足 maxRecord 时，Reserve 必须拒绝。
func TestFocusedReserveRejectsWhenRemainingBelowMaxRecord(t *testing.T) {
	q, err := Open(t.TempDir(), 10, 1024, 700, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(Record{ID: "existing", Event: focusedEventSize(508)}); err != nil {
		t.Fatal(err)
	}
	if err := q.Reserve("next"); !errors.Is(err, ErrFull) {
		t.Fatalf("reserve error = %v, want ErrFull", err)
	}
	if q.Reserved() != 0 {
		t.Fatalf("rejected reservation leaked: %d", q.Reserved())
	}
}

// 两个未预留大记录合计超总字节时，第二条必须拒绝。
func TestFocusedUnreservedEnqueueRejectsOverTotal(t *testing.T) {
	q, err := Open(t.TempDir(), 10, 1024, 700, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(Record{ID: "first", Event: focusedEventSize(608)}); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(Record{ID: "second", Event: focusedEventSize(608)}); !errors.Is(err, ErrFull) {
		t.Fatalf("second enqueue error = %v, want ErrFull", err)
	}
}

// 预留转换为真实记录大小后释放自己的最坏预算，别的 attempt 才能借到剩余容量。
func TestFocusedReservedConversionReleasesOwnBudget(t *testing.T) {
	q, err := Open(t.TempDir(), 10, 764, 700, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Reserve("a"); err != nil {
		t.Fatalf("reserve a: %v", err)
	}
	// a 仍占满最坏预算，第二个预留放不下。
	if err := q.Reserve("b"); !errors.Is(err, ErrFull) {
		t.Fatalf("reserve b error = %v, want ErrFull", err)
	}
	if err := q.Enqueue(Record{ID: "a", Event: focusedEventSize(24)}); err != nil {
		t.Fatalf("reserved enqueue a: %v", err)
	}
	if err := q.Reserve("b"); err != nil {
		t.Fatalf("reserve b after own budget converted: %v", err)
	}
}

// 写入成功的近上限队列必须能重新打开：load 与运行时使用同一 event 字节口径。
func TestFocusedReopenAfterNearLimitWrites(t *testing.T) {
	dir := t.TempDir()
	const maxRecord = 700
	const maxBytes = 3 * maxRecord
	q, err := Open(dir, 10, maxBytes, maxRecord, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("r%d", i)
		if err := q.Reserve(id); err != nil {
			t.Fatalf("reserve %s: %v", id, err)
		}
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("r%d", i)
		if err := q.Enqueue(Record{ID: id, SubmissionID: "s" + id, JudgeTaskID: "j", AttemptID: "a", Event: focusedEventSize(maxRecord)}); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
	}
	reopened, err := Open(dir, 10, maxBytes, maxRecord, time.Hour)
	if err != nil {
		t.Fatalf("reopen near-limit queue: %v", err)
	}
	if reopened.Len() != 3 {
		t.Fatalf("reopened len = %d, want 3", reopened.Len())
	}
}

// Release 幂等，同一 attempt 的预留只占一次；释放绝不删除已落盘结果。
func TestFocusedReleaseIdempotentAndReservedOnce(t *testing.T) {
	q, err := Open(t.TempDir(), 2, 1<<20, 1<<16, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Reserve("a"); err != nil {
		t.Fatal(err)
	}
	if err := q.Reserve("a"); err != nil {
		t.Fatal(err)
	}
	if q.Reserved() != 1 {
		t.Fatalf("reserved = %d, want 1", q.Reserved())
	}
	q.Release("a")
	q.Release("a")
	if q.Reserved() != 0 {
		t.Fatalf("reserved = %d, want 0", q.Reserved())
	}
	if err := q.Enqueue(Record{ID: "a", Event: focusedEventSize(16)}); err != nil {
		t.Fatal(err)
	}
	q.Release("a")
	if q.Len() != 1 {
		t.Fatalf("release removed persisted record: len=%d", q.Len())
	}
}

// 近上限并发完成不会超：已预留的全部能落盘，额外接纳被拒绝，且可重开。
func TestFocusedConcurrentReservedCompletionStaysWithinBudget(t *testing.T) {
	const maxRecord = 400
	const slots = 4
	maxBytes := int64(slots * maxRecord)
	dir := t.TempDir()
	q, err := Open(dir, slots, maxBytes, maxRecord, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < slots; i++ {
		if err := q.Reserve(fmt.Sprintf("r%d", i)); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, slots)
	for i := 0; i < slots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- q.Enqueue(Record{ID: fmt.Sprintf("r%d", i), Event: focusedEventSize(maxRecord)})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("reserved enqueue failed: %v", err)
		}
	}
	if q.Len() != slots {
		t.Fatalf("len = %d, want %d", q.Len(), slots)
	}
	if err := q.Reserve("extra"); !errors.Is(err, ErrFull) {
		t.Fatalf("extra reserve = %v, want ErrFull", err)
	}
	if _, err := Open(dir, slots, maxBytes, maxRecord, 0); err != nil {
		t.Fatalf("reopen: %v", err)
	}
}
