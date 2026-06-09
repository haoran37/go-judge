package testdata

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type CacheInfo struct {
	ProblemID int64     `json:"problemId"`
	Version   int64     `json:"version"`
	SizeBytes int64     `json:"sizeBytes"`
	LastUsed  time.Time `json:"lastUsed"`
}

type CleanupResult struct {
	RemovedProblems int   `json:"removedProblems"`
	FreedBytes      int64 `json:"freedBytes"`
}

func ListCache(cacheRoot string) ([]CacheInfo, error) {
	entries, err := collectCacheEntries(filepath.Join(cacheRoot, "problems"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]CacheInfo, 0, len(entries))
	for _, entry := range entries {
		problemID, err := strconv.ParseInt(filepath.Base(entry.path), 10, 64)
		if err != nil {
			continue
		}
		out = append(out, CacheInfo{
			ProblemID: problemID,
			Version:   readVersion(filepath.Join(entry.path, "data-version")),
			SizeBytes: entry.size,
			LastUsed:  entry.lastUsed,
		})
	}
	return out, nil
}

func DeleteCachedProblem(cacheRoot string, problemID int64) error {
	if problemID <= 0 {
		return fmt.Errorf("invalid problem id %d", problemID)
	}
	problemRoot := filepath.Join(cacheRoot, "problems", strconv.FormatInt(problemID, 10))
	return os.RemoveAll(problemRoot)
}

func Cleanup(cacheRoot string, maxCacheBytes int64, maxUnusedDuration time.Duration) (CleanupResult, error) {
	removed, freed, err := cleanupCache(cacheRoot, maxCacheBytes, maxUnusedDuration)
	return CleanupResult{RemovedProblems: removed, FreedBytes: freed}, err
}
