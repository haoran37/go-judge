package testdata

import (
	"os"
	"path/filepath"
	"sort"
	"time"
)

const lastUsedFileName = "last-used"

type cacheEntry struct {
	path     string
	lastUsed time.Time
	size     int64
}

func cleanupCache(cacheRoot string, maxCacheBytes int64, maxUnusedDuration time.Duration) (int, int64, error) {
	problemRoot := filepath.Join(cacheRoot, "problems")
	entries, err := collectCacheEntries(problemRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}

	now := time.Now()
	removed := 0
	var freed int64
	kept := make([]cacheEntry, 0, len(entries))
	for _, entry := range entries {
		if maxUnusedDuration > 0 && now.Sub(entry.lastUsed) > maxUnusedDuration {
			if err := os.RemoveAll(entry.path); err != nil {
				return removed, freed, err
			}
			removed++
			freed += entry.size
			continue
		}
		kept = append(kept, entry)
	}

	if maxCacheBytes <= 0 {
		return removed, freed, nil
	}
	var total int64
	for _, entry := range kept {
		total += entry.size
	}
	if total <= maxCacheBytes {
		return removed, freed, nil
	}
	sort.Slice(kept, func(i, j int) bool {
		return kept[i].lastUsed.Before(kept[j].lastUsed)
	})
	for _, entry := range kept {
		if total <= maxCacheBytes {
			break
		}
		if err := os.RemoveAll(entry.path); err != nil {
			return removed, freed, err
		}
		removed++
		freed += entry.size
		total -= entry.size
	}
	return removed, freed, nil
}

func collectCacheEntries(problemRoot string) ([]cacheEntry, error) {
	entries, err := os.ReadDir(problemRoot)
	if err != nil {
		return nil, err
	}
	result := make([]cacheEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(problemRoot, entry.Name())
		result = append(result, cacheEntry{
			path:     path,
			lastUsed: resolveLastUsed(path),
			size:     directorySize(path),
		})
	}
	return result, nil
}

func resolveLastUsed(problemRoot string) time.Time {
	for _, name := range []string{lastUsedFileName, "data-version"} {
		info, err := os.Stat(filepath.Join(problemRoot, name))
		if err == nil {
			return info.ModTime()
		}
	}
	info, err := os.Stat(problemRoot)
	if err != nil {
		return time.Now()
	}
	return info.ModTime()
}

func touchLastUsed(problemRoot string) {
	path := filepath.Join(problemRoot, lastUsedFileName)
	now := time.Now()
	if _, err := os.Stat(path); err == nil {
		_ = os.Chtimes(path, now, now)
		return
	}
	_ = os.WriteFile(path, []byte(now.Format(time.RFC3339)), 0o644)
}

func directorySize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}
