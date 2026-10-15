// Package version 集中管理版本信息与构建元数据。
//
// 版本值可通过构建期 ldflags 注入，例如：
//
//	go build -ldflags "-X p2pdl/internal/version.Version=1.2.3"
package version

import (
	"fmt"
	"runtime"
)

// 以下变量由构建脚本通过 -ldflags -X 覆盖，默认值用于本地 go run。
var (
	// Version 是语义化版本号。
	Version = "0.0.0-dev"
	// Commit 是构建时的 Git 提交短哈希。
	Commit = "unknown"
	// BuildTime 是构建时间（RFC3339 或构建脚本给定格式）。
	BuildTime = "unknown"
)

// GoVersion 返回编译当前二进制的 Go 工具链版本。
func GoVersion() string {
	return runtime.Version()
}

// Platform 返回 "os/arch" 形式的目标平台标识。
func Platform() string {
	return fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
}

// Full 返回单行完整版本描述，用于 CLI/GUI 的「关于」与启动日志。
func Full() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s, %s)",
		Version, Commit, BuildTime, GoVersion(), Platform())
}
