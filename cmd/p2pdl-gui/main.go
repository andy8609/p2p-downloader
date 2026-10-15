// Command p2pdl-gui 是 p2pdl 的 Windows 桌面壳（Wails v2 + WebView2）。
//
// 架构：本进程只负责界面与引擎进程的生命周期管理，实际的下载工作由独立的
// p2pdl-engine 进程完成（通过本地控制面通信）。因此：
//   - 界面卡顿或崩溃都不会中断下载；
//   - 引擎被外部杀掉时，界面能立刻感知并提供「重启引擎」。
//
// 前端为纯静态资源（HTML/CSS/JS），不依赖 npm 或任何构建步骤，
// 保证「一条命令构建」这一验收要求。
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"p2pdl/internal/gui"
)

//go:embed all:frontend
var assets embed.FS

func main() {
	var (
		dataDir      = flag.String("data-dir", "", "下载目录（默认取设置文件，其次 ~/Downloads/p2pdl）")
		metaDir      = flag.String("meta-dir", "", "元数据目录（默认 <下载目录>/.p2pdl）")
		engineBinary = flag.String("engine", "", "引擎可执行文件路径（默认自动查找同目录）")
		port         = flag.Int("port", 0, "控制面端口，0 表示自动分配")
		settingsPath = flag.String("settings", "", "设置文件路径（默认 %APPDATA%/p2pdl/settings.json）")
	)
	flag.Parse()

	// 界面依赖 WebView2；缺失时给出明确指引，而不是留下一个打不开的白窗口。
	if ok, detail := gui.CheckWebView2(); !ok {
		if !gui.PromptAndGuideWebView2() {
			fmt.Fprintln(os.Stderr, "缺少 WebView2 运行时，已退出："+detail)
			os.Exit(2)
		}
		log.Printf("用户选择继续；WebView2 检测结果: %s", detail)
	} else {
		log.Printf("WebView2 Runtime 可用：%s", detail)
	}

	service := gui.NewService(gui.ServiceOptions{
		DataDir:      *dataDir,
		MetaDir:      *metaDir,
		EngineBinary: *engineBinary,
		Port:         *port,
		SettingsPath: *settingsPath,
		Logf:         func(format string, args ...any) { log.Printf(format, args...) },
		// 文件对话框由宿主注入：internal/gui 因此不依赖 Wails，可以被单元测试完整覆盖。
		PickFile: func(ctx context.Context) (string, error) {
			return runtime.OpenFileDialog(ctx, runtime.OpenDialogOptions{
				Title: "选择种子文件",
				Filters: []runtime.FileFilter{
					{DisplayName: "种子文件 (*.torrent)", Pattern: "*.torrent"},
					{DisplayName: "所有文件 (*.*)", Pattern: "*.*"},
				},
			})
		},
		PickDirectory: func(ctx context.Context) (string, error) {
			return runtime.OpenDirectoryDialog(ctx, runtime.OpenDialogOptions{
				Title: "选择下载目录",
			})
		},
		// 关闭确认：引擎是独立进程，关掉界面并不会停止下载——
		// 正因为如此，更要在用户关窗时把这件事说清楚。
		ConfirmClose: func(ctx context.Context, active int) (gui.CloseChoice, error) {
			const (
				keep   = "保留后台下载"
				stop   = "停止并退出"
				cancel = "取消"
			)
			choice, err := runtime.MessageDialog(ctx, runtime.MessageDialogOptions{
				Type:    runtime.QuestionDialog,
				Title:   "仍有任务在下载",
				Message: fmt.Sprintf("当前有 %d 个任务未完成。\n\n保留后台下载：关闭界面，下载继续进行（可再次打开界面查看）。\n停止并退出：结束下载进程后退出。", active),
				Buttons: []string{keep, stop, cancel},
				DefaultButton: keep,
				CancelButton:  cancel,
			})
			if err != nil {
				return gui.CloseCancel, err
			}
			switch choice {
			case keep:
				return gui.CloseKeepEngineRunning, nil
			case stop:
				return gui.CloseStopEngine, nil
			default:
				return gui.CloseCancel, nil
			}
		},
	})

	err := wails.Run(&options.App{
		Title:         "p2pdl 下载器",
		Width:         1120,
		Height:        740,
		MinWidth:      900,
		MinHeight:     580,
		DisableResize: false,
		Frameless:     false,
		AssetServer:   &assetserver.Options{Assets: assets},
		OnStartup:     service.Startup,
		OnShutdown:    service.Shutdown,
		OnBeforeClose: service.OnBeforeClose,
		Bind:          []any{service},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动界面失败: %v\n", err)
		os.Exit(1)
	}
}
