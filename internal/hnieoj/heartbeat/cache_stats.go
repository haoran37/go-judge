package heartbeat

import (
	"os"
	"path/filepath"
)

type CacheStats struct {
	CacheUsedBytes    int64
	CacheProblemCount int
	DiskTotalBytes    int64
	DiskFreeBytes     int64
}

func collectCacheStats(cacheRoot string) CacheStats {
	stats := CacheStats{}
	if cacheRoot == "" {
		return stats
	}
	stats.CacheUsedBytes = directorySize(cacheRoot)
	stats.CacheProblemCount = cachedProblemCount(filepath.Join(cacheRoot, "problems"))
	stats.DiskTotalBytes, stats.DiskFreeBytes = diskUsage(cacheRoot)
	return stats
}

func directorySize(root string) int64 {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
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
	if err != nil {
		return 0
	}
	return total
}

func cachedProblemCount(problemRoot string) int {
	entries, err := os.ReadDir(problemRoot)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count
}
