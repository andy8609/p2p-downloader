// Package cli 实现 dl 命令行客户端。
//
// 命令被实现为可测试的 Run 函数（而不是直接写 os.Exit），
// 因此集成测试可以在同一进程内启动内嵌引擎并驱动全部命令。
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"p2pdl/internal/app"
	"p2pdl/internal/client"
	"p2pdl/internal/config"
	"p2pdl/internal/control"
	"p2pdl/internal/engine"
	"p2pdl/internal/engine/limiter"
	"p2pdl/internal/version"
)

// 退出码语义：调用方（脚本）据此判断结果。
const (
	// ExitOK 表示命令成功。
	ExitOK = 0
	// ExitError 表示运行期错误（网络、状态机拒绝、任务失败等）。
	ExitError = 1
	// ExitUsage 表示命令行用法错误。
	ExitUsage = 2
)

// globalOptions 是所有子命令共享的参数。
type globalOptions struct {
	dataDir  string
	metaDir  string
	addr     string
	token    string
	settings string
	jsonOut  bool
}

// Run 执行一次 CLI 调用并返回退出码。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts := globalOptions{}
	args, err := parseInterspersed(args, &opts)
	if err != nil {
		fmt.Fprintln(stderr, "参数错误: "+err.Error())
		return ExitUsage
	}
	if len(args) == 0 {
		printUsage(stderr)
		return ExitUsage
	}

	command, rest := args[0], args[1:]
	switch command {
	case "serve", "engine":
		return runServe(ctx, opts, rest, stdout, stderr)
	case "add":
		return runAdd(ctx, opts, rest, stdout, stderr)
	case "list", "ls":
		return runList(ctx, opts, rest, stdout, stderr)
	case "pause":
		return runTaskAction(ctx, opts, rest, stdout, stderr, "pause")
	case "resume":
		return runTaskAction(ctx, opts, rest, stdout, stderr, "resume")
	case "recheck":
		return runTaskAction(ctx, opts, rest, stdout, stderr, "recheck")
	case "rm", "remove":
		return runRemove(ctx, opts, rest, stdout, stderr)
	case "watch":
		return runWatch(ctx, opts, rest, stdout, stderr)
	case "status":
		return runStatus(ctx, opts, rest, stdout, stderr)
	case "limit":
		return runLimit(ctx, opts, rest, stdout, stderr)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, "dl "+version.Full())
		return ExitOK
	case "help", "--help", "-h":
		printUsage(stdout)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "未知命令 %q\n\n", command)
		printUsage(stderr)
		return ExitUsage
	}
}

// parseInterspersed 提取全局参数，同时允许它们出现在子命令前后。
func parseInterspersed(args []string, opts *globalOptions) ([]string, error) {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		takeValue := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s 需要一个取值", arg)
			}
			i++
			return args[i], nil
		}
		switch {
		case arg == "--data-dir" || arg == "-data-dir":
			v, err := takeValue()
			if err != nil {
				return nil, err
			}
			opts.dataDir = v
		case strings.HasPrefix(arg, "--data-dir="):
			opts.dataDir = strings.TrimPrefix(arg, "--data-dir=")
		case arg == "--meta-dir" || arg == "-meta-dir":
			v, err := takeValue()
			if err != nil {
				return nil, err
			}
			opts.metaDir = v
		case strings.HasPrefix(arg, "--meta-dir="):
			opts.metaDir = strings.TrimPrefix(arg, "--meta-dir=")
		case arg == "--addr" || arg == "-addr":
			v, err := takeValue()
			if err != nil {
				return nil, err
			}
			opts.addr = v
		case strings.HasPrefix(arg, "--addr="):
			opts.addr = strings.TrimPrefix(arg, "--addr=")
		case arg == "--settings" || arg == "-settings":
			v, err := takeValue()
			if err != nil {
				return nil, err
			}
			opts.settings = v
		case strings.HasPrefix(arg, "--settings="):
			opts.settings = strings.TrimPrefix(arg, "--settings=")
		case arg == "--token" || arg == "-token":
			v, err := takeValue()
			if err != nil {
				return nil, err
			}
			opts.token = v
		case strings.HasPrefix(arg, "--token="):
			opts.token = strings.TrimPrefix(arg, "--token=")
		case arg == "--json":
			opts.jsonOut = true
		default:
			out = append(out, arg)
		}
	}
	return out, nil
}

// defaultDataDir 返回默认下载目录。
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "p2pdl-downloads"
	}
	return filepath.Join(home, "Downloads", "p2pdl")
}

// resolveDataDir 补齐下载目录默认值，并转为绝对路径。
//
// 转绝对路径是必要的：引擎会把端点文件里的路径与下载路径交给其他进程使用，
// 相对路径在不同工作目录下会解析成不同位置。
func (o *globalOptions) resolveDataDir() string {
	if o.dataDir != "" {
		return absolutePath(o.dataDir)
	}
	return defaultDataDir()
}

func absolutePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// resolveMetaDir 补齐元数据目录默认值（与 app.Options.normalize 的规则一致）。
func (o *globalOptions) resolveMetaDir() string {
	if o.metaDir != "" {
		return absolutePath(o.metaDir)
	}
	return filepath.Join(o.resolveDataDir(), ".p2pdl")
}

// connect 发现并连接正在运行的引擎。
func (o *globalOptions) connect(ctx context.Context) (*client.Client, error) {
	if o.addr != "" && o.token != "" {
		c := client.New(o.addr, o.token)
		if err := c.Health(ctx); err != nil {
			return nil, err
		}
		return c, nil
	}
	return client.Discover(o.resolveMetaDir())
}

func settingsFileOrDefault(path string) string {
	if path == "" {
		return config.DefaultPath()
	}
	return path
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `dl - p2pdl 命令行客户端

用法:
  dl <命令> [参数] [全局选项]

命令:
  serve                 前台运行下载引擎与控制面（不启动 GUI）
  add <URL|磁力链>      新建任务（--torrent 可直接指定种子文件）
  list                  列出全部任务
  watch [任务ID]        实时打印进度（缺省监视全部任务）
  pause <任务ID>        暂停任务
  resume <任务ID>       恢复任务
  recheck <任务ID>      重新校验（发现损坏时只重下受损部分）
  rm <任务ID>           删除任务（--files 同时删除文件）
  status                引擎状态与统计
  limit <速率>          修改全局限速，例如 1MB / 500k / 0（0 表示不限）
  version               打印版本信息
  help                  打印本帮助

全局选项:
  --data-dir <目录>     下载目录（默认 ~/Downloads/p2pdl）
  --meta-dir <目录>     元数据目录（默认 <下载目录>/.p2pdl）
  --addr <地址>         直接指定控制面地址（配合 --token 跳过自动发现）
  --token <令牌>        访问令牌
  --settings <文件>     设置文件（默认 %APPDATA%/p2pdl/settings.json）
  --json                以 JSON 输出（便于脚本处理）

退出码:
  0  成功
  1  运行期错误（任务失败、状态机拒绝、连接失败等）
  2  用法错误
`)
}


// reorderArgs 把位置参数移到末尾，使 `dl add <URL> --name x` 这类写法能正确解析。
//
// 背景：标准库 flag 在遇到第一个非 flag 参数后就会停止解析，
// 于是 `add <url> --name x` 中的 --name 会被当成位置参数忽略。
// 用户习惯把来源写在前面，因此在解析前重排一次。
// valueFlags 需列出「需要取一个值」的 flag 名，否则其取值会被误判为位置参数。
func reorderArgs(args []string, valueFlags map[string]bool) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue // --key=value 自带取值
		}
		if valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func runServe(ctx context.Context, opts globalOptions, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", 0, "控制面监听端口（0 表示自动分配）")
	maxTasks := fs.Int("max-tasks", 0, "同时运行的任务数上限（0 表示用设置文件的值）")
	concurrency := fs.Int("concurrency", 0, "单任务并发连接数（0 表示用设置文件的值）")
	rateLimit := fs.String("limit", "", "全局限速，例如 1MB（空表示用设置文件的值）")
	buildManifest := fs.Bool("manifest", false, "校验通过后生成分块清单（便于后续定点修复）")
	settingsPath := fs.String("settings", "", "设置文件路径")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"port": true, "max-tasks": true, "concurrency": true, "limit": true, "settings": true,
	})); err != nil {
		return ExitUsage
	}

	// 配置优先级：命令行参数 > 设置文件 > 内置默认值。
	// 引擎与桌面壳读同一个设置文件，避免两处配置各说各话。
	settingsFile := *settingsPath
	if settingsFile == "" {
		settingsFile = opts.settings
	}
	settings, settingsErr := config.Load(settingsFile)
	if settingsErr != nil {
		fmt.Fprintf(stderr, "读取设置失败（已使用默认值）: %v\n", settingsErr)
	}
	if opts.dataDir != "" {
		settings.DataDir = opts.dataDir
	}
	if opts.metaDir != "" {
		settings.MetaDir = opts.metaDir
	}
	if *port != 0 {
		settings.Port = *port
	}
	if *maxTasks != 0 {
		settings.MaxConcurrentTasks = *maxTasks
	}
	if *concurrency != 0 {
		settings.Concurrency = *concurrency
	}
	if *rateLimit != "" {
		parsed, err := client.ParseSize(*rateLimit)
		if err != nil {
			fmt.Fprintln(stderr, "限速参数错误: "+err.Error())
			return ExitUsage
		}
		settings.GlobalRateLimit = parsed
	}
	if *buildManifest {
		settings.BuildManifest = true
	}
	settings.Normalize()
	if err := settings.Validate(); err != nil {
		fmt.Fprintln(stderr, "设置校验失败: "+err.Error())
		return ExitUsage
	}

	a, err := app.New(app.Options{
		DataDir:                settings.DataDir,
		MetaDir:                settings.MetaDir,
		ChunkSize:              settings.ChunkSize,
		MaxConcurrentTasks:     settings.MaxConcurrentTasks,
		Concurrency:            settings.Concurrency,
		GlobalRateLimit:        settings.GlobalRateLimit,
		MaxMemory:              settings.MaxMemory,
		BuildManifestOnSuccess: settings.BuildManifest,
		PauseOnRestore:         !settings.AutoResume,
		Logf:                   func(format string, args ...any) { fmt.Fprintf(stderr, "[engine] "+format+"\n", args...) },
	})
	if err != nil {
		fmt.Fprintln(stderr, "启动失败: "+err.Error())
		return ExitError
	}

	if err := a.Start(ctx); err != nil {
		fmt.Fprintln(stderr, "恢复任务失败: "+err.Error())
		return ExitError
	}

	srv, err := control.New(control.Options{
		App:  a,
		Addr: fmt.Sprintf("127.0.0.1:%d", settings.Port),
		Logf: func(format string, args ...any) { fmt.Fprintf(stderr, "[control] "+format+"\n", args...) },
	})
	if err != nil {
		fmt.Fprintln(stderr, "创建控制面失败: "+err.Error())
		return ExitError
	}
	addr, err := srv.Start()
	if err != nil {
		fmt.Fprintln(stderr, "启动控制面失败: "+err.Error())
		return ExitError
	}

	metaDir := settings.MetaDir
	if _, err := client.WriteEndpoint(metaDir, addr, srv.TokenPath()); err != nil {
		fmt.Fprintln(stderr, "写入端点文件失败: "+err.Error())
		return ExitError
	}

	fmt.Fprintf(stdout, "引擎已启动\n")
	fmt.Fprintf(stdout, "  设置文件: %s\n", settingsFileOrDefault(settingsFile))
	fmt.Fprintf(stdout, "  控制面: %s\n", "http://"+addr)
	fmt.Fprintf(stdout, "  下载目录: %s\n", a.Options().DataDir)
	fmt.Fprintf(stdout, "  令牌文件: %s\n", srv.TokenPath())
	fmt.Fprintf(stdout, "按 Ctrl+C 退出\n")

	<-ctx.Done()
	fmt.Fprintln(stdout, "正在停止…")

	if err := srv.Stop(); err != nil {
		fmt.Fprintln(stderr, "停止控制面出错: "+err.Error())
	}
	if err := a.Stop(); err != nil {
		fmt.Fprintln(stderr, "停止引擎出错: "+err.Error())
	}
	client.RemoveEndpoint(metaDir)
	fmt.Fprintln(stdout, "已停止")
	return ExitOK
}

// ---------------------------------------------------------------------------
// add
// ---------------------------------------------------------------------------

func runAdd(ctx context.Context, opts globalOptions, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	torrent := fs.String("torrent", "", "种子文件路径（与 URL/磁力链二选一）")
	name := fs.String("name", "", "覆盖文件名")
	dest := fs.String("dest", "", "覆盖保存路径")
	hash := fs.String("hash", "", "期望摘要（十六进制），下载后会校验")
	algo := fs.String("algo", "sha256", "摘要算法（sha256/md5）")
	paused := fs.Bool("paused", false, "创建后保持暂停")
	watch := fs.Bool("watch", true, "等待并实时打印进度（--watch=false 立即返回）")
	timeout := fs.Duration("timeout", 0, "等待超时，0 表示不限制")
	peers := fs.String("peers", "", "BT 静态 peer，逗号分隔，例如 10.0.0.5:6881")
	header := fs.String("header", "", "附加请求头，格式 K:V，多个用逗号分隔")
	if err := fs.Parse(reorderArgs(args, map[string]bool{
		"torrent": true, "name": true, "dest": true, "hash": true, "algo": true,
		"timeout": true, "peers": true, "header": true,
	})); err != nil {
		return ExitUsage
	}

	var source string
	if fs.NArg() > 0 {
		source = fs.Arg(0)
	}
	if source == "" && *torrent == "" {
		fmt.Fprintln(stderr, "用法: dl add <URL|磁力链> [--torrent 种子文件]")
		return ExitUsage
	}

	c, err := opts.connect(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return ExitError
	}

	req := client.AddRequest{
		Source:        source,
		TorrentPath:   *torrent,
		Name:          *name,
		DestPath:      *dest,
		ExpectedHash:  strings.TrimSpace(*hash),
		HashAlgorithm: *algo,
		Paused:        *paused,
	}
	if *peers != "" {
		for _, p := range strings.Split(*peers, ",") {
			if p = strings.TrimSpace(p); p != "" {
				req.Peers = append(req.Peers, p)
			}
		}
	}
	if *header != "" {
		req.Header = map[string]string{}
		for _, pair := range strings.Split(*header, ",") {
			k, v, ok := strings.Cut(pair, ":")
			if !ok {
				fmt.Fprintf(stderr, "请求头格式错误: %q（应为 K:V）\n", pair)
				return ExitUsage
			}
			req.Header[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}

	task, err := c.AddTask(ctx, req)
	if err != nil {
		fmt.Fprintln(stderr, "创建任务失败: "+err.Error())
		return ExitError
	}

	if opts.jsonOut {
		printJSON(stdout, task)
	} else {
		fmt.Fprintf(stdout, "已创建任务 %s\n", task.ID)
		fmt.Fprintf(stdout, "  名称: %s\n", task.DisplayName)
		fmt.Fprintf(stdout, "  类型: %s\n", task.Kind)
		fmt.Fprintf(stdout, "  大小: %s\n", humanSize(task.Total))
		fmt.Fprintf(stdout, "  状态: %s\n", task.State)
		fmt.Fprintf(stdout, "  保存到: %s\n", task.DestPath)
	}

	if !*watch || *paused {
		return ExitOK
	}

	final, err := watchProgress(ctx, c, task.ID, *timeout, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "等待任务失败: "+err.Error())
		return ExitError
	}
	if final.State != engine.StateCompleted {
		fmt.Fprintf(stderr, "任务未成功完成: %s", final.State)
		if final.LastError != "" {
			fmt.Fprintf(stderr, "（%s）", final.LastError)
		}
		fmt.Fprintln(stderr)
		return ExitError
	}
	fmt.Fprintf(stdout, "\n完成: %s（%s）\n", final.DisplayName, humanSize(final.Total))
	return ExitOK
}

// ---------------------------------------------------------------------------
// list / status
// ---------------------------------------------------------------------------

func runList(ctx context.Context, opts globalOptions, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderArgs(args, nil)); err != nil {
		return ExitUsage
	}

	c, err := opts.connect(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return ExitError
	}
	tasks, err := c.ListTasks(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "获取任务列表失败: "+err.Error())
		return ExitError
	}
	if opts.jsonOut {
		printJSON(stdout, tasks)
		return ExitOK
	}
	if len(tasks) == 0 {
		fmt.Fprintln(stdout, "没有任务")
		return ExitOK
	}

	fmt.Fprintf(stdout, "%-28s %-12s %-10s %9s %12s  %s\n",
		"ID", "类型", "状态", "进度", "速度", "名称")
	for _, t := range tasks {
		fmt.Fprintf(stdout, "%-28s %-12s %-10s %9s %12s  %s\n",
			truncate(t.ID, 28), t.Kind, t.State,
			fmt.Sprintf("%.1f%%", t.Progress*100),
			limiter.FormatRate(t.Rate),
			truncate(t.DisplayName, 40))
		if t.LastError != "" {
			fmt.Fprintf(stdout, "%30s  └─ %s\n", "", truncate(t.LastError, 80))
		}
	}
	return ExitOK
}

func runStatus(ctx context.Context, opts globalOptions, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderArgs(args, nil)); err != nil {
		return ExitUsage
	}

	c, err := opts.connect(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return ExitError
	}
	stats, err := c.Stats(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "获取状态失败: "+err.Error())
		return ExitError
	}
	ver, _ := c.Version(ctx)

	if opts.jsonOut {
		printJSON(stdout, map[string]any{"stats": stats, "version": ver})
		return ExitOK
	}

	fmt.Fprintf(stdout, "控制面: %s\n", c.BaseURL())
	if v, ok := ver["version"].(string); ok {
		fmt.Fprintf(stdout, "引擎版本: %s\n", v)
	}
	tasks, _ := stats["tasks"].(map[string]any)
	fmt.Fprintf(stdout, "任务: 共 %v（运行 %v / 等待 %v / 暂停 %v / 完成 %v / 失败 %v）\n",
		tasks["total"], tasks["running"], tasks["pending"], tasks["paused"], tasks["completed"], tasks["failed"])
	fmt.Fprintf(stdout, "并发上限: %v\n", tasks["maxConcurrent"])
	fmt.Fprintf(stdout, "下载目录: %v\n", stats["dataDir"])
	limit, _ := stats["rateLimit"].(float64)
	if limit <= 0 {
		fmt.Fprintln(stdout, "全局限速: 不限")
	} else {
		fmt.Fprintf(stdout, "全局限速: %s\n", limiter.FormatRate(limit))
	}
	return ExitOK
}

func runLimit(ctx context.Context, opts globalOptions, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("limit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderArgs(args, nil)); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "用法: dl limit <速率>，例如 1MB / 500k / 0")
		return ExitUsage
	}
	rate, err := client.ParseSize(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "速率解析失败: "+err.Error())
		return ExitUsage
	}

	c, err := opts.connect(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return ExitError
	}
	out, err := c.SetGlobalRateLimit(ctx, rate)
	if err != nil {
		fmt.Fprintln(stderr, "设置限速失败: "+err.Error())
		return ExitError
	}
	if opts.jsonOut {
		printJSON(stdout, out)
		return ExitOK
	}
	if rate <= 0 {
		fmt.Fprintln(stdout, "已关闭全局限速")
	} else {
		fmt.Fprintf(stdout, "全局限速已设为 %s\n", limiter.FormatRate(float64(rate)))
	}
	return ExitOK
}

// ---------------------------------------------------------------------------
// 任务操作
// ---------------------------------------------------------------------------

func runTaskAction(ctx context.Context, opts globalOptions, args []string, stdout, stderr io.Writer, action string) int {
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderArgs(args, nil)); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "用法: dl %s <任务ID>\n", action)
		return ExitUsage
	}
	id := fs.Arg(0)

	c, err := opts.connect(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return ExitError
	}

	var view engine.TaskView
	switch action {
	case "pause":
		view, err = c.PauseTask(ctx, id)
	case "resume":
		view, err = c.ResumeTask(ctx, id)
	case "recheck":
		view, err = c.RecheckTask(ctx, id)
	default:
		err = fmt.Errorf("未知操作 %q", action)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s 失败: %s\n", action, err.Error())
		return ExitError
	}
	if opts.jsonOut {
		printJSON(stdout, view)
		return ExitOK
	}
	fmt.Fprintf(stdout, "任务 %s 现在状态: %s\n", view.ID, view.State)
	return ExitOK
}

func runRemove(ctx context.Context, opts globalOptions, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	files := fs.Bool("files", false, "同时删除已下载的数据文件")
	if err := fs.Parse(reorderArgs(args, map[string]bool{"timeout": true})); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "用法: dl rm <任务ID> [--files]")
		return ExitUsage
	}
	id := fs.Arg(0)

	c, err := opts.connect(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return ExitError
	}
	if err := c.RemoveTask(ctx, id, *files); err != nil {
		fmt.Fprintln(stderr, "删除失败: "+err.Error())
		return ExitError
	}
	fmt.Fprintf(stdout, "已删除任务 %s", id)
	if *files {
		fmt.Fprint(stdout, "（含数据文件）")
	}
	fmt.Fprintln(stdout)
	return ExitOK
}

func runWatch(ctx context.Context, opts globalOptions, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 0, "超时时间，0 表示不限制")
	if err := fs.Parse(reorderArgs(args, map[string]bool{"timeout": true})); err != nil {
		return ExitUsage
	}
	id := ""
	if fs.NArg() > 0 {
		id = fs.Arg(0)
	}

	c, err := opts.connect(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return ExitError
	}

	if id == "" {
		return watchAll(ctx, c, stdout, stderr)
	}
	final, err := watchProgress(ctx, c, id, *timeout, stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "监视失败: "+err.Error())
		return ExitError
	}
	fmt.Fprintln(stdout)
	if final.State != engine.StateCompleted {
		fmt.Fprintf(stderr, "任务结束状态: %s", final.State)
		if final.LastError != "" {
			fmt.Fprintf(stderr, "（%s）", final.LastError)
		}
		fmt.Fprintln(stderr)
		return ExitError
	}
	return ExitOK
}

// watchAll 打印全部任务的实时概览，直到它们都离开运行态。
func watchAll(ctx context.Context, c *client.Client, stdout, stderr io.Writer) int {
	events, err := c.SubscribeEvents(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "订阅事件失败: "+err.Error())
		return ExitError
	}

	latest := map[string]engine.TaskView{}
	render := func() {
		if len(latest) == 0 {
			return
		}
		ids := make([]string, 0, len(latest))
		for id := range latest {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		fmt.Fprintf(stdout, "\r\033[2K任务 %d 个: ", len(ids))
		for i, id := range ids {
			if i > 0 {
				fmt.Fprint(stdout, " | ")
			}
			v := latest[id]
			fmt.Fprintf(stdout, "%s %s %.0f%% %s", truncate(v.DisplayName, 20), v.State, v.Progress*100, limiter.FormatRate(v.Rate))
		}
	}
	render()

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(stdout)
			return ExitOK
		case ev, ok := <-events:
			if !ok {
				fmt.Fprintln(stdout)
				return ExitError
			}
			if ev.Task == nil {
				continue
			}
			latest[ev.Task.ID] = *ev.Task
			render()
		}
	}
}

// watchProgress 实时打印单个任务的进度，返回其终态。
func watchProgress(ctx context.Context, c *client.Client, id string, timeout time.Duration, stdout, stderr io.Writer) (engine.TaskView, error) {
	lastPrint := time.Time{}
	// 先取一次当前状态，避免任务在订阅前就已完成而看不到初始信息。
	if view, err := c.GetTask(ctx, id); err == nil {
		if view.State == engine.StateCompleted || view.State == engine.StateFailed {
			fmt.Fprintf(stdout, "%s %s %.1f%%", truncate(view.DisplayName, 24), view.State, view.Progress*100)
			return view, nil
		}
		latest := view
		_ = latest
	}

	return c.WaitForTask(ctx, id, timeout, func(v engine.TaskView) {
		if time.Since(lastPrint) < 150*time.Millisecond && v.State == engine.StateRunning {
			return
		}
		lastPrint = time.Now()
		fmt.Fprintf(stdout, "\r\033[2K%s %6.1f%% %12s %10s  %s",
			truncate(v.DisplayName, 24),
			v.Progress*100,
			limiter.FormatRate(v.Rate),
			humanSize(v.Done)+"/"+humanSize(v.Total),
			v.State)
	})
}

// ---------------------------------------------------------------------------
// 输出辅助
// ---------------------------------------------------------------------------

func printJSON(w io.Writer, payload any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func humanSize(n int64) string {
	if n <= 0 {
		return "未知"
	}
	return limiter.FormatBytes(n)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
