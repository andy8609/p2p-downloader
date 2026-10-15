package gui

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"p2pdl/internal/config"
	"p2pdl/internal/engine"
)

// ---------------------------------------------------------------------------
// 设置
// ---------------------------------------------------------------------------

func TestSettingsRoundTripAndDefaults(t *testing.T) {
	dir := tempDir(t)
	settingsFile := filepath.Join(dir, "settings.json")

	s, err := config.Load(settingsFile)
	if err != nil {
		t.Fatalf("读取不存在的设置应返回默认值: %v", err)
	}
	if s.Concurrency != 8 || s.MaxConcurrentTasks != 3 {
		t.Fatalf("默认值异常: %+v", s)
	}
	if !s.AutoResume {
		t.Error("默认应自动继续未完成任务")
	}

	custom := config.Settings{
		DataDir:            filepath.Join(dir, "dl"),
		MetaDir:            filepath.Join(dir, "meta"),
		Port:               12345,
		Concurrency:        4,
		MaxConcurrentTasks: 2,
		ChunkSize:          512 << 10,
		GlobalRateLimit:    1 << 20,
		AutoResume:         false,
		NotifyOnComplete:   true,
		ClipboardMonitor:   false,
	}
	if err := config.Save(settingsFile, custom); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	loaded, err := config.Load(settingsFile)
	if err != nil {
		t.Fatalf("重新读取失败: %v", err)
	}
	if loaded.Concurrency != 4 || loaded.Port != 12345 || loaded.ChunkSize != 512<<10 ||
		loaded.GlobalRateLimit != 1<<20 || loaded.AutoResume || loaded.ClipboardMonitor {
		t.Fatalf("往返后设置不一致: %+v", loaded)
	}
}

func TestSettingsValidationRejectsBadValues(t *testing.T) {
	// Normalize 负责「把缺省/越界值修回默认」，Validate 负责「拒绝语义上不允许的值」。
	// 两者职责不同，必须分开验证，否则会得出「校验无效」的错误结论。
	rejected := []struct {
		name   string
		mutate func(*config.Settings)
	}{
		{"并发过大", func(s *config.Settings) { s.Concurrency = 999 }},
		{"任务数过大", func(s *config.Settings) { s.MaxConcurrentTasks = 99 }},
		{"分块过小", func(s *config.Settings) { s.ChunkSize = 1 }},
		{"分块过大", func(s *config.Settings) { s.ChunkSize = 1 << 30 }},
		{"目录为空白", func(s *config.Settings) { s.DataDir = "   " }},
	}
	for _, c := range rejected {
		t.Run(c.name, func(t *testing.T) {
			s := config.Default()
			s.DataDir = "D:\\dl"
			s.Normalize()
			c.mutate(&s)
			if err := s.Validate(); err == nil {
				t.Fatalf("应拒绝: %+v", s)
			}
		})
	}

	repaired := []struct {
		name   string
		mutate func(*config.Settings)
		check  func(config.Settings) bool
	}{
		{"并发为 0", func(s *config.Settings) { s.Concurrency = 0 },
			func(s config.Settings) bool { return s.Concurrency == 8 }},
		{"任务数为 0", func(s *config.Settings) { s.MaxConcurrentTasks = 0 },
			func(s config.Settings) bool { return s.MaxConcurrentTasks == 3 }},
		{"端口越界", func(s *config.Settings) { s.Port = 70000 },
			func(s config.Settings) bool { return s.Port == 0 }},
		{"限速为负", func(s *config.Settings) { s.GlobalRateLimit = -1 },
			func(s config.Settings) bool { return s.GlobalRateLimit == 0 }},
		{"目录为空", func(s *config.Settings) { s.DataDir = "   " },
			func(s config.Settings) bool { return s.DataDir != "" }},
	}
	for _, c := range repaired {
		t.Run(c.name, func(t *testing.T) {
			s := config.Settings{}
			c.mutate(&s)
			s.Normalize()
			if !c.check(s) {
				t.Fatalf("Normalize 未修复该值: %+v", s)
			}
			if err := s.Validate(); err != nil {
				t.Fatalf("修复后应通过校验: %v", err)
			}
		})
	}
}

func TestSettingsNormalizeFillsDefaults(t *testing.T) {
	s := config.Settings{}
	s.Normalize()
	if s.DataDir == "" || s.MetaDir == "" {
		t.Fatalf("应补齐目录: %+v", s)
	}
	if s.Concurrency != 8 || s.MaxConcurrentTasks != 3 || s.ChunkSize != 1<<20 {
		t.Fatalf("应补齐数值项: %+v", s)
	}
}

func TestDiffRestartRequired(t *testing.T) {
	base := config.Settings{DataDir: "a", Concurrency: 8, ChunkSize: 1 << 20, GlobalRateLimit: 0}

	// 只改运行期项 → 不需要重启
	runtimeOnly := base
	runtimeOnly.GlobalRateLimit = 1 << 20
	if changed := config.DiffRestartRequired(base, runtimeOnly); len(changed) != 0 {
		t.Fatalf("限速不应要求重启: %v", changed)
	}

	// 改启动期项 → 需要重启
	needsRestart := base
	needsRestart.Concurrency = 16
	needsRestart.Port = 9999
	changed := config.DiffRestartRequired(base, needsRestart)
	if len(changed) != 2 {
		t.Fatalf("应报告 2 个需重启字段, 实际 %v", changed)
	}
}

func TestServiceSaveSettingsAppliesRateLimitImmediately(t *testing.T) {
	dir := tempDir(t)
	settingsFile := filepath.Join(dir, "settings.json")

	svc := newTestService(t, dir)
	svc.settingsPath = settingsFile
	svc.settings = config.Default()
	// 必须与 Service 构造时一致地补齐默认值，否则「空目录 → 默认目录」会被误判为变更。
	svc.settings.Normalize()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc.Startup(ctx)
	t.Cleanup(func() { svc.Shutdown(context.Background()) })

	// 改限速：应立即生效，且不标记需要重启
	next := svc.AppliedSettings()
	next.GlobalRateLimit = 2 << 20
	view, err := svc.SaveSettings(next)
	if err != nil {
		t.Fatalf("SaveSettings 失败: %v", err)
	}
	if len(view.PendingRestart) != 0 {
		t.Fatalf("限速变更不应要求重启: %v", view.PendingRestart)
	}
	if got := svc.GetState().RateLimit; got != 2<<20 {
		t.Fatalf("限速未立即生效: %d", got)
	}

	// 改并发数：写入配置但需要重启
	next = svc.AppliedSettings()
	next.Concurrency = 16
	view, err = svc.SaveSettings(next)
	if err != nil {
		t.Fatalf("SaveSettings 失败: %v", err)
	}
	if len(view.PendingRestart) == 0 {
		t.Fatal("并发变更应提示需要重启")
	}

	persisted, err := config.Load(settingsFile)
	if err != nil {
		t.Fatalf("读取持久化设置失败: %v", err)
	}
	if persisted.Concurrency != 16 {
		t.Fatalf("设置未持久化: %d", persisted.Concurrency)
	}

	// 非法值应被拒绝且不写入
	bad := svc.AppliedSettings()
	bad.Concurrency = 999
	if _, err := svc.SaveSettings(bad); err == nil {
		t.Fatal("非法并发数应被拒绝")
	}
	if after := svc.AppliedSettings(); after.Concurrency != 16 {
		t.Fatalf("被拒绝的保存不应改变内存状态: %d", after.Concurrency)
	}
}

func TestServiceSettingsLoadsFromDisk(t *testing.T) {
	dir := tempDir(t)
	settingsFile := filepath.Join(dir, "settings.json")
	if err := config.Save(settingsFile, config.Settings{
		DataDir:            filepath.Join(dir, "custom-dl"),
		MetaDir:            filepath.Join(dir, "custom-meta"),
		Concurrency:        6,
		MaxConcurrentTasks: 7,
		AutoResume:         false,
	}); err != nil {
		t.Fatalf("预置设置失败: %v", err)
	}

	t.Setenv("P2PDL_TEST_AS_ENGINE", "1")
	exe, _ := os.Executable()
	svc := NewService(ServiceOptions{
		SettingsPath: settingsFile,
		EngineBinary: exe,
		Logf:         t.Logf,
	})

	view := svc.GetSettings()
	if view.Settings.Concurrency != 6 || view.Settings.MaxConcurrentTasks != 7 {
		t.Fatalf("设置未从磁盘加载: %+v", view.Settings)
	}
	if view.SettingsPath != settingsFile {
		t.Fatalf("设置路径 = %q", view.SettingsPath)
	}
	if len(view.RestartRequired) == 0 {
		t.Error("应告知哪些字段需要重启")
	}
	if svc.AppliedSettings().AutoResume {
		t.Error("AutoResume=false 应被保留")
	}
	// 命令行/构造参数应覆盖设置文件
	svc2 := NewService(ServiceOptions{
		SettingsPath: settingsFile,
		DataDir:      filepath.Join(dir, "override"),
		Logf:         t.Logf,
	})
	if svc2.AppliedSettings().DataDir != filepath.Join(dir, "override") {
		t.Fatalf("显式目录未覆盖设置文件: %q", svc2.AppliedSettings().DataDir)
	}
}

// ---------------------------------------------------------------------------
// 速率曲线与通知
// ---------------------------------------------------------------------------

func TestHistoryRecordsAndPrunes(t *testing.T) {
	svc := &Service{
		history:    newHistoryStore(),
		lastStates: map[string]engine.TaskView{},
		settings:   config.Default(),
	}

	tasks := []engine.TaskView{
		{ID: "a", DisplayName: "a", State: engine.StateRunning, Rate: 1000, Done: 1},
		{ID: "b", DisplayName: "b", State: engine.StateRunning, Rate: 2000, Done: 2},
	}
	svc.trackTransitions(tasks)
	svc.trackTransitions(tasks)

	if got := len(svc.GetHistory("a")); got != 2 {
		t.Fatalf("a 的采样数 = %d, 期望 2", got)
	}
	if got := len(svc.GetHistory("b")); got != 2 {
		t.Fatalf("b 的采样数 = %d, 期望 2", got)
	}

	// 暂停时速率应记为 0，避免曲线在暂停期间保持旧值
	paused := []engine.TaskView{{ID: "a", DisplayName: "a", State: engine.StatePaused, Rate: 1000, Done: 1}}
	svc.trackTransitions(paused)
	series := svc.GetHistory("a")
	if last := series[len(series)-1]; last.Rate != 0 {
		t.Fatalf("暂停时速率应记为 0, 实际 %v", last.Rate)
	}

	// 任务消失后曲线应被清理，避免长期运行内存泄漏
	svc.trackTransitions(nil)
	if got := len(svc.GetHistory("a")); got != 0 {
		t.Fatalf("任务消失后应清理曲线, 实际 %d 个点", got)
	}
}

func TestHistoryWindowIsBounded(t *testing.T) {
	h := newHistoryStore()
	for i := 0; i < historyWindow*3; i++ {
		h.record("x", RateSample{At: time.Now(), Rate: float64(i), Done: int64(i)})
	}
	series := h.series("x")
	if len(series) != historyWindow {
		t.Fatalf("采样点数 = %d, 期望被限制在 %d", len(series), historyWindow)
	}
	// 应保留最近的采样
	if series[len(series)-1].Rate != float64(historyWindow*3-1) {
		t.Fatalf("最后一点的速率 = %v", series[len(series)-1].Rate)
	}
}

func TestNotificationsOnCompletionAndFailure(t *testing.T) {
	svc := &Service{
		history:    newHistoryStore(),
		lastStates: map[string]engine.TaskView{},
		settings:   config.Default(),
	}
	svc.settings.NotifyOnComplete = true

	svc.trackTransitions([]engine.TaskView{{ID: "x", DisplayName: "文件.iso", State: engine.StateRunning}})

	// 完成 → 成功通知
	svc.trackTransitions([]engine.TaskView{{ID: "x", DisplayName: "文件.iso", State: engine.StateCompleted, Total: 1 << 20}})
	items := svc.Notifications()
	if len(items) != 1 || items[0].Kind != "success" {
		t.Fatalf("完成通知异常: %+v", items)
	}
	if items[0].Title != "下载完成" {
		t.Errorf("通知标题 = %q", items[0].Title)
	}
	// 拉取即清空
	if got := svc.Notifications(); len(got) != 0 {
		t.Fatalf("通知应被清空, 实际 %+v", got)
	}

	// 失败 → 错误通知，并带上原因
	svc.trackTransitions([]engine.TaskView{{ID: "y", DisplayName: "bad.iso", State: engine.StateRunning}})
	svc.trackTransitions([]engine.TaskView{{
		ID: "y", DisplayName: "bad.iso", State: engine.StateFailed,
		LastError: "连接被重置",
	}})
	items = svc.Notifications()
	if len(items) != 1 || items[0].Kind != "error" {
		t.Fatalf("失败通知异常: %+v", items)
	}
	if items[0].Body == "" {
		t.Error("失败通知应包含原因")
	}
}

func TestNotificationsRespectToggle(t *testing.T) {
	svc := &Service{
		history:    newHistoryStore(),
		lastStates: map[string]engine.TaskView{},
		settings:   config.Default(),
	}
	svc.settings.NotifyOnComplete = false

	svc.trackTransitions([]engine.TaskView{{ID: "x", State: engine.StateRunning}})
	svc.trackTransitions([]engine.TaskView{{ID: "x", State: engine.StateCompleted}})
	if got := svc.Notifications(); len(got) != 0 {
		t.Fatalf("关闭通知后不应产生提醒: %+v", got)
	}
}

func TestTrackTransitionsIgnoresUnchangedStates(t *testing.T) {
	svc := &Service{
		history:    newHistoryStore(),
		lastStates: map[string]engine.TaskView{},
		settings:   config.Default(),
	}
	task := engine.TaskView{ID: "x", State: engine.StateRunning}
	for i := 0; i < 5; i++ {
		svc.trackTransitions([]engine.TaskView{task})
	}
	if got := svc.Notifications(); len(got) != 0 {
		t.Fatalf("状态未变化时不应产生通知: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// 关闭确认与对话框
// ---------------------------------------------------------------------------

func TestOnBeforeCloseAsksWhenTasksActive(t *testing.T) {
	dir := tempDir(t)
	svc := newTestService(t, dir)
	svc.autoStart = false
	svc.opts.ConfirmClose = func(context.Context, int) (CloseChoice, error) {
		return CloseKeepEngineRunning, nil
	}

	// 没有任务 → 直接关闭，不打扰用户
	if svc.OnBeforeClose(context.Background()) {
		t.Fatal("无活跃任务时不应阻止关闭")
	}

	// 有运行中的任务 → 询问
	svc.storeStates([]engine.TaskView{{ID: "x", State: engine.StateRunning}})
	active := 0
	svc.opts.ConfirmClose = func(_ context.Context, n int) (CloseChoice, error) {
		active = n
		return CloseCancel, nil
	}
	if !svc.OnBeforeClose(context.Background()) {
		t.Fatal("用户取消时应阻止关闭")
	}
	if active != 1 {
		t.Fatalf("传给界面的活跃任务数 = %d, 期望 1", active)
	}

	// 选择保留后台下载 → 允许关闭且标记保留引擎
	svc.opts.ConfirmClose = func(context.Context, int) (CloseChoice, error) {
		return CloseKeepEngineRunning, nil
	}
	if svc.OnBeforeClose(context.Background()) {
		t.Fatal("选择保留后台下载时应允许关闭")
	}
	svc.mu.Lock()
	keep := svc.keepEngineOnExit
	svc.mu.Unlock()
	if !keep {
		t.Fatal("应标记为保留引擎")
	}
}

func TestOnBeforeCloseWithoutHostDialogDoesNotBlock(t *testing.T) {
	dir := tempDir(t)
	svc := newTestService(t, dir)
	svc.autoStart = false
	// 未注入 ConfirmClose（例如无头环境）：不应卡住关闭流程
	svc.storeStates([]engine.TaskView{{ID: "x", State: engine.StateRunning}})
	if svc.OnBeforeClose(context.Background()) {
		t.Fatal("没有宿主对话框时不应阻止关闭")
	}
}

func TestServicePickersWithoutHost(t *testing.T) {
	dir := tempDir(t)
	svc := newTestService(t, dir)

	if _, err := svc.PickTorrentFile(); err == nil {
		t.Error("未注入 PickFile 时应返回可读错误")
	}
	if _, err := svc.PickDataDir(); err == nil {
		t.Error("未注入 PickDirectory 时应返回可读错误")
	}

	called := ""
	svc.opts.PickFile = func(context.Context) (string, error) {
		called = "file"
		return "D:\\a.torrent", nil
	}
	svc.opts.PickDirectory = func(context.Context) (string, error) {
		called = "dir"
		return "D:\\dl", nil
	}
	if got, err := svc.PickTorrentFile(); err != nil || got != "D:\\a.torrent" {
		t.Fatalf("PickTorrentFile = (%q, %v)", got, err)
	}
	if got, err := svc.PickDataDir(); err != nil || got != "D:\\dl" {
		t.Fatalf("PickDataDir = (%q, %v)", got, err)
	}
	if called == "" {
		t.Fatal("应调用注入的实现")
	}
}

func TestOpenContainingFolderFallsBackToDirectory(t *testing.T) {
	dir := tempDir(t)
	svc := newTestService(t, dir)
	svc.autoStart = false

	if err := svc.OpenContainingFolder(""); err == nil {
		t.Error("空路径应被拒绝")
	}
	// 目标文件不存在时应退化为打开其目录，而不是报错
	missing := filepath.Join(tempDir(t), "nope.bin")
	if err := svc.OpenContainingFolder(missing); err != nil {
		t.Fatalf("文件缺失时应退化为打开目录: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 与真实引擎的端到端（复用设置文件）
// ---------------------------------------------------------------------------

func TestAutoResumeDisabledKeepsTasksPaused(t *testing.T) {
	content := make([]byte, 512<<10)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	// 慢速服务器，保证第一次运行来得及中断
	srv := slowServer(t, content)
	defer srv.Close()

	dir := tempDir(t)
	settingsFile := filepath.Join(dir, "settings.json")
	dataDir := filepath.Join(dir, "dl")
	metaDir := filepath.Join(dir, "meta")

	// 第一次：正常下载到一半后中断
	if err := config.Save(settingsFile, config.Settings{
		DataDir: dataDir, MetaDir: metaDir, ChunkSize: 64 << 10,
		Concurrency: 2, MaxConcurrentTasks: 1, AutoResume: true,
	}); err != nil {
		t.Fatalf("保存设置失败: %v", err)
	}

	svc1 := startServiceWithSettings(t, settingsFile)
	task, err := svc1.AddTask(AddTaskRequest{Source: srv.URL + "/payload.bin", Name: "resume.bin"})
	if err != nil {
		t.Fatalf("AddTask 失败: %v", err)
	}
	waitFor(t, 30*time.Second, "产生部分进度", func() bool {
		for _, tk := range svc1.GetState().Tasks {
			if tk.ID == task.ID && tk.Done > 0 {
				return true
			}
		}
		return false
	})
	svc1.Shutdown(context.Background())

	// 第二次：关闭自动继续
	saved, err := config.Load(settingsFile)
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	saved.AutoResume = false
	if err := config.Save(settingsFile, saved); err != nil {
		t.Fatalf("保存设置失败: %v", err)
	}

	svc2 := startServiceWithSettings(t, settingsFile)
	t.Cleanup(func() { svc2.Shutdown(context.Background()) })

	waitFor(t, 30*time.Second, "任务以暂停状态恢复", func() bool {
		for _, tk := range svc2.GetState().Tasks {
			if tk.ID == task.ID && tk.State == engine.StatePaused {
				return true
			}
		}
		return false
	})

	// 手动继续后应能完成
	if err := svc2.ResumeTask(task.ID); err != nil {
		t.Fatalf("ResumeTask 失败: %v", err)
	}
	waitFor(t, 60*time.Second, "任务完成", func() bool {
		for _, tk := range svc2.GetState().Tasks {
			if tk.ID == task.ID && tk.State == engine.StateCompleted {
				return true
			}
		}
		return false
	})
}

func startServiceWithSettings(t *testing.T, settingsFile string) *Service {
	t.Helper()
	t.Setenv("P2PDL_TEST_AS_ENGINE", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("无法定位测试二进制: %v", err)
	}
	svc := NewService(ServiceOptions{
		SettingsPath: settingsFile,
		EngineBinary: exe,
		Logf:         t.Logf,
	})
	svc.engine.StartTimeout = 30 * time.Second
	// 注意不能用 defer cancel：Service 会保存这个 ctx 供后续所有控制面调用使用，
	// 提前取消会让 AddTask 等操作立刻报 context canceled。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	svc.Startup(ctx)
	return svc
}
