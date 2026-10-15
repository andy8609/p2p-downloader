//go:build windows

package limiter

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceEx = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// freeSpace 通过 GetDiskFreeSpaceExW 获取可用空间。
// 第一个参数（可用空间）才是任务真正能用的额度：配额限制下它可能远小于总剩余。
func freeSpace(path string) (int64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("limiter: 路径转换失败: %w", err)
	}

	var freeToCaller, total, totalFree uint64
	ret, _, callErr := procGetDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if ret == 0 {
		return 0, fmt.Errorf("limiter: GetDiskFreeSpaceEx 失败（%s）: %v", path, callErr)
	}
	if freeToCaller > 1<<62 {
		// 极端异常值（例如不支持的超大卷）不参与决策，避免误判为「空间充足」。
		return 0, fmt.Errorf("limiter: 可用空间数值异常（%d）", freeToCaller)
	}
	return int64(freeToCaller), nil
}
