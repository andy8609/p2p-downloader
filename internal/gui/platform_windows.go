//go:build windows

package gui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
	advapi32        = syscall.NewLazyDLL("advapi32.dll")
)

// WebView2 运行时在注册表中的固定产品 ID（EdgeUpdate 写入）。
const webView2ClientGUID = "{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"

// webView2DownloadURL 是微软官方的运行时下载页（Evergreen Bootstrapper）。
const webView2DownloadURL = "https://go.microsoft.com/fwlink/p/?LinkId=2124703"

// MessageBox 常用标志。
const (
	mbOK          = 0x00000000
	mbIconError   = 0x00000010
	mbIconWarning = 0x00000030
	mbYesNo       = 0x00000004
	mbIDYes       = 6
)

// hideWindowAttr 让 GUI 启动引擎子进程时不弹出控制台黑窗。
//
// 引擎本身仍是控制台程序（用户可手动运行它查看日志），
// 由桌面壳启动时再隐藏窗口，兼顾两种使用方式。
func hideWindowAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

// CheckWebView2 检测 WebView2 Runtime 是否可用，返回 (是否可用, 版本或原因)。
//
// 检测两处（任一命中即视为可用）：
//  1. EdgeUpdate 注册表项——安装器写入的标准位置，含版本号；
//  2. 安装目录下的版本子目录——覆盖注册表项被清理但运行时装着的机器。
func CheckWebView2() (bool, string) {
	if version, ok := webView2VersionFromRegistry(); ok {
		return true, version
	}
	if version, ok := webView2VersionFromDir(); ok {
		return true, version
	}
	return false, "未安装 Microsoft Edge WebView2 Runtime"
}

func webView2VersionFromRegistry() (string, bool) {
	const (
		hkeyLocalMachine = 0x80000002
		keyRead          = 0x20019
	)
	paths := []string{
		`SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\` + webView2ClientGUID,
		`SOFTWARE\Microsoft\EdgeUpdate\Clients\` + webView2ClientGUID,
	}
	procRegOpenKeyEx := advapi32.NewProc("RegOpenKeyExW")
	procRegQueryValueEx := advapi32.NewProc("RegQueryValueExW")
	procRegCloseKey := advapi32.NewProc("RegCloseKey")

	for _, path := range paths {
		pathPtr, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			continue
		}
		var hkey uintptr
		ret, _, _ := procRegOpenKeyEx.Call(
			uintptr(hkeyLocalMachine),
			uintptr(unsafe.Pointer(pathPtr)),
			0,
			uintptr(keyRead),
			uintptr(unsafe.Pointer(&hkey)),
		)
		if ret != 0 {
			continue
		}

		namePtr, _ := syscall.UTF16PtrFromString("pv")
		var valueType, size uint32 = 0, 512
		buf := make([]uint16, size/2)
		ret, _, _ = procRegQueryValueEx.Call(
			hkey,
			uintptr(unsafe.Pointer(namePtr)),
			0,
			uintptr(unsafe.Pointer(&valueType)),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&size)),
		)
		procRegCloseKey.Call(hkey)
		if ret != 0 {
			continue
		}
		if version := strings.TrimSpace(syscall.UTF16ToString(buf)); version != "" {
			return version, true
		}
	}
	return "", false
}

func webView2VersionFromDir() (string, bool) {
	dirs := []string{
		`C:\Program Files (x86)\Microsoft\EdgeWebView\Application`,
		`C:\Program Files\Microsoft\EdgeWebView\Application`,
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			// 目录名即版本号，例如 153.0.4234.48
			if name := entry.Name(); name != "" && name[0] >= '0' && name[0] <= '9' {
				return name, true
			}
		}
	}
	return "", false
}

// MessageBox 弹出原生消息框，返回用户点击的按钮 ID。
func MessageBox(title, text string, flags uint32) int {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	textPtr, _ := syscall.UTF16PtrFromString(text)
	ret, _, _ := procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(flags),
	)
	return int(ret)
}

// OpenURL 用系统默认浏览器打开链接。
func OpenURL(url string) error {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("只允许打开 http/https 链接: %q", url)
	}
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// PromptAndGuideWebView2 在缺少 WebView2 时给出明确提示并引导安装。
// 返回 true 表示用户选择继续（例如刚装完想重试），false 表示退出。
func PromptAndGuideWebView2() bool {
	text := "本程序需要 Microsoft Edge WebView2 Runtime 才能显示界面，当前系统未检测到它。\n\n" +
		"点击「是」将打开微软官方下载页；安装完成后重新运行本程序即可。\n" +
		"点击「否」退出。"
	if choice := MessageBox("缺少 WebView2 运行时", text, mbYesNo|mbIconWarning); choice != mbIDYes {
		return false
	}
	if err := OpenURL(webView2DownloadURL); err != nil {
		MessageBox("无法打开下载页",
			"自动打开浏览器失败，请手动访问：\n"+webView2DownloadURL, mbOK|mbIconError)
		return false
	}
	return true
}

// NotifyUser 弹出错误提示（用于启动期不可恢复的错误）。
func NotifyUser(title, text string) {
	MessageBox(title, text, mbOK|mbIconError)
}
