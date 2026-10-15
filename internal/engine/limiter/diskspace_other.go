//go:build !windows

package limiter

import (
	"fmt"
	"syscall"
)

// freeSpace 通过 statfs 获取可用空间。
func freeSpace(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("limiter: Statfs 失败（%s）: %w", path, err)
	}
	// Bavail 是「非特权用户可用块数」，比 Bfree 更贴近实际可写额度。
	return int64(st.Bavail) * int64(st.Bsize), nil
}
