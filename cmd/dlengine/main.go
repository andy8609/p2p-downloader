// Command dlengine 是纯粹的下载引擎进程入口。
//
// 它等价于 `dl serve`，但不包含 CLI 的命令分发——桌面壳（p2pdl-gui）会启动它，
// 并把它的标准输出/错误转发到界面日志。拆成独立进程的好处是：
// 界面崩溃或卡顿都不会中断下载；用户也可以直接命令行运行它做无界面下载。
//
// 用法：
//
//	dlengine --data-dir D:\downloads --meta-dir D:\downloads\.p2pdl --port 0
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"p2pdl/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 复用 CLI 的 serve 实现，保证 GUI 与命令行启动的引擎行为完全一致。
	args := append([]string{"serve"}, os.Args[1:]...)
	os.Exit(cli.Run(ctx, args, os.Stdout, os.Stderr))
}
