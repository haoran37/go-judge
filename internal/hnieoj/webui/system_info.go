package webui

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type SystemInfoDTO struct {
	CPUCore           int    `json:"cpuCore"`
	GoRoutines        int    `json:"goRoutines"`
	ProcessAllocBytes uint64 `json:"processAllocBytes"`
	ProcessSysBytes   uint64 `json:"processSysBytes"`
	MemoryTotalBytes  uint64 `json:"memoryTotalBytes"`
	MemoryFreeBytes   uint64 `json:"memoryFreeBytes"`
	DiskTotalBytes    uint64 `json:"diskTotalBytes"`
	DiskFreeBytes     uint64 `json:"diskFreeBytes"`
}

func collectSystemInfo(cacheRoot string) SystemInfoDTO {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	total, free := linuxMemoryInfo()
	diskTotal, diskFree := diskInfo(resolveDiskPath(cacheRoot))
	return SystemInfoDTO{
		CPUCore:           runtime.NumCPU(),
		GoRoutines:        runtime.NumGoroutine(),
		ProcessAllocBytes: mem.Alloc,
		ProcessSysBytes:   mem.Sys,
		MemoryTotalBytes:  total,
		MemoryFreeBytes:   free,
		DiskTotalBytes:    diskTotal,
		DiskFreeBytes:     diskFree,
	}
}

func resolveDiskPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "."
	}
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

func linuxMemoryInfo() (uint64, uint64) {
	body, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var total, available uint64
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			total = value * 1024
		case "MemAvailable":
			available = value * 1024
		}
	}
	return total, available
}
