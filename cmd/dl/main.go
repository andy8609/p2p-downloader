// Command dl 是 p2pdl 的命令行客户端。
//
//	dl serve            前台运行引擎与控制面
//	dl add <来源>       新建任务（--watch 实时打印进度）
//	dl list / watch     查看任务
//	dl pause / resume / recheck / rm
//	dl status / limit   引擎状态与限速
//
// 它通过本地控制面与引擎通信，因此可以在引擎运行于另一进程时独立重启与调试。
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

	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
