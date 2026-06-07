//go:build linux || darwin || freebsd || netbsd || openbsd

package heartbeat

import "syscall"

func diskUsage(path string) (int64, int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	blockSize := int64(stat.Bsize)
	total := int64(stat.Blocks) * blockSize
	free := int64(stat.Bavail) * blockSize
	return total, free
}
