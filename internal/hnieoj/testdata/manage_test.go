package testdata

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestListCacheReportsProblems(t *testing.T) {
	cacheRoot := t.TempDir()
	problemRoot := filepath.Join(cacheRoot, "problems", "1001")
	writeFile(t, filepath.Join(problemRoot, "testdata", "1.in"), "input")
	writeFile(t, filepath.Join(problemRoot, "data-version"), "7")
	touchLastUsed(problemRoot)

	items, err := ListCache(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d, want 1", len(items))
	}
	if items[0].ProblemID != 1001 || items[0].Version != 7 || items[0].SizeBytes <= 0 || items[0].LastUsed.IsZero() {
		t.Fatalf("unexpected cache info: %+v", items[0])
	}
}

func TestDeleteCachedProblemRemovesOnlyProblem(t *testing.T) {
	cacheRoot := t.TempDir()
	writeFile(t, filepath.Join(cacheRoot, "problems", "1001", "testdata", "1.in"), "a")
	writeFile(t, filepath.Join(cacheRoot, "problems", "1002", "testdata", "1.in"), "b")

	if err := DeleteCachedProblem(cacheRoot, 1001); err != nil {
		t.Fatal(err)
	}
	items, err := ListCache(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ProblemID != 1002 {
		t.Fatalf("unexpected remaining cache: %+v", items)
	}
}

func TestCleanupUsesExistingPolicy(t *testing.T) {
	cacheRoot := t.TempDir()
	oldProblemRoot := filepath.Join(cacheRoot, "problems", "1001")
	writeFile(t, filepath.Join(oldProblemRoot, "testdata", "1.in"), "old")
	oldTime := time.Now().Add(-48 * time.Hour)
	lastUsedPath := filepath.Join(oldProblemRoot, lastUsedFileName)
	writeFile(t, lastUsedPath, oldTime.Format(time.RFC3339))
	if err := os.Chtimes(lastUsedPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	result, err := Cleanup(cacheRoot, 0, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedProblems != 1 || result.FreedBytes <= 0 {
		t.Fatalf("unexpected cleanup result: %+v", result)
	}
}
