//go:build windows

package e2e

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	psapi                    = syscall.NewLazyDLL("psapi.dll")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
	kernel32dll              = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess          = kernel32dll.NewProc("OpenProcess")
	procCloseHandle          = kernel32dll.NewProc("CloseHandle")
)

// processMemoryCounters 对应 PROCESS_MEMORY_COUNTERS（只保留需要的字段）。
type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// processWorkingSet 返回指定进程的当前工作集（字节）。
//
// 用它来验证「大文件下载的内存占用」比在同进程内读 runtime.MemStats 更有说服力：
// 它测量的是操作系统看到的真实占用，包含 Go 运行时之外的映射与栈。
func processWorkingSet(pid int) (int64, error) {
	const processQueryLimitedInformation = 0x1000
	handle, _, callErr := procOpenProcess.Call(
		uintptr(processQueryLimitedInformation),
		0,
		uintptr(pid),
	)
	if handle == 0 {
		return 0, fmt.Errorf("OpenProcess 失败: %v", callErr)
	}
	defer procCloseHandle.Call(handle)

	var counters processMemoryCounters
	counters.Cb = uint32(unsafe.Sizeof(counters))
	ret, _, callErr := procGetProcessMemoryInfo.Call(
		handle,
		uintptr(unsafe.Pointer(&counters)),
		uintptr(counters.Cb),
	)
	if ret == 0 {
		return 0, fmt.Errorf("GetProcessMemoryInfo 失败: %v", callErr)
	}
	return int64(counters.WorkingSetSize), nil
}
