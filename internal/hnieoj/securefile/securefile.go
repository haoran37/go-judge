// Package securefile 提供敏感文件的原子写入与平台加固：
// POSIX 0700/0600 + fsync 目录；Windows 用 current-user-only DACL。
// 敏感数据在写入前就限制访问（临时文件先加固），而不是写完才补救。
package securefile

import (
	"errors"
	"os"
	"path/filepath"
)

// MkdirAll 创建目录并加固权限。
func MkdirAll(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	return hardenPath(dir, mode)
}

// HardenExisting 对已存在的敏感目录/文件重新加固：
// 目录 0700、文件 0600（Windows current-user ACL）。文件不存在时只加固目录。
func HardenExisting(path string, mode os.FileMode) error {
	if err := MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return hardenPath(path, mode)
}

// WriteFileAtomic 原子写入敏感文件：同目录临时文件、写入前加固、fsync 后 rename，再 fsync 目录。
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".secure-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	// 在写入任何敏感字节之前先限制访问，避免短暂可读窗口。
	if err := hardenPath(tmpName, mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if err := hardenPath(path, mode); err != nil {
		return err
	}
	return SyncDir(dir)
}
