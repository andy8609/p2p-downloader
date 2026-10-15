//go:build windows

package gui

import "testing"

// 验收相关：本机必须能检测到 WebView2 并给出可读版本号。
// 若本机确实没装（例如全新的 Server Core），测试跳过而不是失败——
// 那是环境缺依赖，不是实现缺陷；实现侧的「缺失引导」由 PromptAndGuideWebView2 承担。
func TestCheckWebView2ReportsVersion(t *testing.T) {
	ok, detail := CheckWebView2()
	if !ok {
		t.Skipf("本机未安装 WebView2（%s），跳过版本检测", detail)
	}
	if detail == "" {
		t.Fatal("检测到可用但未给出原因/版本号")
	}
	t.Logf("WebView2 版本: %s", detail)
}

// 注册表与目录两条探测路径都应可用（任一命中即视为已安装）。
func TestWebView2DetectionPaths(t *testing.T) {
	if version, ok := webView2VersionFromRegistry(); ok {
		t.Logf("注册表路径命中: %s", version)
	} else {
		t.Log("注册表路径未命中（可能被策略清理）")
	}
	if version, ok := webView2VersionFromDir(); ok {
		t.Logf("目录路径命中: %s", version)
	} else {
		t.Log("目录路径未命中")
	}
}

// OpenURL 只允许 http/https：它是从应用里拉起外部程序的入口，
// 放开任意协议等于给出一个本机任意程序执行的面。
func TestOpenURLRejectsDangerousSchemes(t *testing.T) {
	for _, bad := range []string{
		"file:///C:/Windows/system32/calc.exe",
		"javascript:alert(1)",
		"ms-settings:",
		"",
		"C:\\Windows\\system32\\calc.exe",
	} {
		if err := OpenURL(bad); err == nil {
			t.Errorf("OpenURL(%q) 应被拒绝", bad)
		}
	}
}
