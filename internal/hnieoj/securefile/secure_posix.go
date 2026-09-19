//go:build !windows

package securefile

import (
	"fmt"
	"os"
)

// hardenPath 在 POSIX 上设置目录/文件权限并断言 group/other 无权限。
func hardenPath(path string, mode os.FileMode) error {
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("insecure permissions %04o on %s", perm, path)
	}
	return nil
}

// SyncDir 在 POSIX 上 fsync 目录，确保 rename 结果落盘。
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync directory %s: %w", dir, err)
	}
	return nil
}
