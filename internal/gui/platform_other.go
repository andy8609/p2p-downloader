//go:build !windows

package gui

import (
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
)

// hideWindowAttr 在非 Windows 平台上无需隐藏子进程窗口。
func hideWindowAttr() *syscall.SysProcAttr { return nil }

// CheckWebView2 只对 Windows 有意义，其他平台直接视为可用。
func CheckWebView2() (bool, string) {
	return true, "非 Windows 平台不适用"
}

// MessageBox 在非 Windows 平台上退化为日志输出。
func MessageBox(title, text string, _ uint32) int {
	fmt.Printf("[%s] %s\n", title, text)
	return 0
}

// OpenURL 在非 Windows 平台上使用系统默认程序打开链接。
func OpenURL(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// PromptAndGuideWebView2 在非 Windows 平台上无需引导。
func PromptAndGuideWebView2() bool { return true }

// NotifyUser 在非 Windows 平台上退化为日志输出。
func NotifyUser(title, text string) { fmt.Printf("[%s] %s\n", title, text) }
