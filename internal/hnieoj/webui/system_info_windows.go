//go:build windows

package webui

import (
	"syscall"
	"unsafe"
)

func diskInfo(path string) (uint64, uint64) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetDiskFreeSpaceExW")
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0
	}
	var freeAvailable, totalBytes, totalFree uint64
	r1, _, _ := proc.Call(
		uintptr(unsafe.Pointer(ptr)),
		uintptr(unsafe.Pointer(&freeAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r1 == 0 {
		return 0, 0
	}
	return totalBytes, totalFree
}
