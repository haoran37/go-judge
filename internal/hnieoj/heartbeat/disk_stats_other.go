//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd

package heartbeat

func diskUsage(string) (int64, int64) {
	return 0, 0
}
