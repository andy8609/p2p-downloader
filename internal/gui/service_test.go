package gui

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"p2pdl/internal/cli"
)

// TestMain 让本测试二进制在特定环境变量下充当「引擎进程」。
//
// 这样就不必在测试里调用 go build 去产出真实二进制：EngineManager 启动的
// 子进程其实就是同一个测试程序，只是带着 serve 参数并进入引擎分支。
func TestMain(m *testing.M) {
	if os.Getenv("P2PDL_TEST_AS_ENGINE") == "1" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "p2pdl-gui-")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待「%s」超时（%v）", what, timeout)
}

// newTestManager 构造一个把「测试二进制自身」当作引擎的管理器。
func newTestManager(t *testing.T, dir string) *EngineManager {
	t.Helper()
	// 子进程继承环境变量后会在 TestMain 中进入引擎分支。
	t.Setenv("P2PDL_TEST_AS_ENGINE", "1")

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("无法定位测试二进制: %v", err)
	}
	return &EngineManager{
		BinaryPath:   exe,
		DataDir:      filepath.Join(dir, "dl"),
		MetaDir:      filepath.Join(dir, "meta"),
		StartTimeout: 30 * time.Second,
		Logf:         t.Logf,
	}
}

func startManager(t *testing.T, dir string) *EngineManager {
	t.Helper()
	m := newTestManager(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if _, err := m.EnsureRunning(ctx); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(m.Stop)
	return m
}

// ---------------------------------------------------------------------------
// 引擎进程生命周期
// ---------------------------------------------------------------------------

func TestEngineManagerStartsAndStops(t *testing.T) {
	dir := tempDir(t)
	m := startManager(t, dir)

	status := m.Status()
	if !status.Running {
		t.Fatal("启动后引擎应处于运行态")
	}
	if !status.Owned {
		t.Error("由管理器启动的引擎应标记为 owned")
	}
	if status.PID == 0 {
		t.Error("应记录引擎进程 PID")
	}
	if status.Addr == "" || !strings.HasPrefix(status.Addr, "http://127.0.0.1:") {
		t.Fatalf("引擎地址异常: %q", status.Addr)
	}

	// 客户端可用
	c := m.Client()
	if c == nil {
		t.Fatal("应能取得控制面客户端")
	}
	if _, err := c.Stats(context.Background()); err != nil {
		t.Fatalf("控制面不可用: %v", err)
	}

	m.Stop()
	waitFor(t, 15*time.Second, "引擎停止", func() bool { return !m.Status().Running })
}

// 已有引擎时不应重复启动（两个引擎会争抢同一个数据目录）。
func TestEngineManagerReusesRunningEngine(t *testing.T) {
	dir := tempDir(t)
	first := startManager(t, dir)
	addr := first.Status().Addr

	second := newTestManager(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := second.EnsureRunning(ctx); err != nil {
		t.Fatalf("第二个管理器连接失败: %v", err)
	}

	status := second.Status()
	if !status.Running {
		t.Fatal("应复用正在运行的引擎")
	}
	if status.Owned {
		t.Error("复用外部引擎时不应标记为 owned（否则退出时会误杀别人的进程）")
	}
	if status.Addr != addr {
		t.Fatalf("复用后的地址 = %q, 期望 %q", status.Addr, addr)
	}
}

// 验收核心：手动杀掉引擎进程后，GUI 能感知到。
func TestEngineManagerDetectsEngineCrash(t *testing.T) {
	dir := tempDir(t)
	m := startManager(t, dir)

	status := m.Status()
	proc, err := os.FindProcess(status.PID)
	if err != nil {
		t.Fatalf("查找进程失败: %v", err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("杀进程失败: %v", err)
	}
	t.Logf("已杀掉引擎进程 pid=%d", status.PID)

	waitFor(t, 20*time.Second, "管理器察觉引擎退出", func() bool {
		st := m.Status()
		return !st.Running && st.LastError != ""
	})
	t.Logf("感知到的错误: %s", m.Status().LastError)
}

// 验收核心：引擎崩溃后可一键重启。
func TestEngineManagerRestartsAfterCrash(t *testing.T) {
	dir := tempDir(t)
	m := startManager(t, dir)

	status := m.Status()
	if proc, err := os.FindProcess(status.PID); err == nil {
		_ = proc.Kill()
	}
	waitFor(t, 20*time.Second, "引擎停止", func() bool { return !m.Status().Running })

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	c, err := m.Restart(ctx)
	if err != nil {
		t.Fatalf("重启失败: %v", err)
	}
	if _, err := c.Stats(ctx); err != nil {
		t.Fatalf("重启后控制面不可用: %v", err)
	}
	if got := m.Status(); !got.Running || got.PID == status.PID {
		t.Fatalf("重启后状态异常: %+v（旧 pid=%d）", got, status.PID)
	}
}

func TestEngineManagerReportsMissingBinary(t *testing.T) {
	m := &EngineManager{
		BinaryPath: filepath.Join(tempDir(t), "not-exists.exe"),
		DataDir:    tempDir(t),
		MetaDir:    tempDir(t),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := m.EnsureRunning(ctx); err == nil {
		t.Fatal("引擎二进制不存在时应返回错误")
	}
	if st := m.Status(); st.Running {
		t.Error("失败后不应标记为运行中")
	}
}

func TestResolveBinaryFindsSameDirectoryBinary(t *testing.T) {
	dir := tempDir(t)
	fake := filepath.Join(dir, "p2pdl-engine.exe")
	if err := os.WriteFile(fake, []byte("stub"), 0o755); err != nil {
		t.Fatalf("写入桩文件失败: %v", err)
	}
	m := &EngineManager{BinaryPath: fake}
	got, err := m.resolveBinary()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != fake {
		t.Fatalf("解析结果 = %q, 期望 %q", got, fake)
	}

	// 未找到任何候选时应给出可读错误。
	empty := &EngineManager{BinaryPath: filepath.Join(dir, "missing.exe")}
	if _, err := empty.resolveBinary(); err == nil {
		t.Fatal("指定了不存在的路径时应报错")
	}
}

// ---------------------------------------------------------------------------
// 服务层
// ---------------------------------------------------------------------------

func rangeServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "payload.bin", time.Time{}, bytes.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestService(t *testing.T, dir string) *Service {
	t.Helper()
	t.Setenv("P2PDL_TEST_AS_ENGINE", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("无法定位测试二进制: %v", err)
	}
	svc := NewService(ServiceOptions{
		DataDir:      filepath.Join(dir, "dl"),
		MetaDir:      filepath.Join(dir, "meta"),
		EngineBinary: exe,
		Logf:         t.Logf,
	})
	svc.engine.StartTimeout = 30 * time.Second
	return svc
}

func TestServiceAddAndManageTask(t *testing.T) {
	content := make([]byte, 256<<10)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	srv := rangeServer(t, content)

	dir := tempDir(t)
	svc := newTestService(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc.Startup(ctx)
	t.Cleanup(func() { svc.Shutdown(context.Background()) })

	// 启动后引擎应可用
	view := svc.GetState()
	if !view.EngineRunning {
		t.Fatalf("引擎应处于运行态: %+v", view)
	}
	if len(view.Tasks) != 0 {
		t.Fatalf("初始不应有任务: %+v", view.Tasks)
	}
	if view.DataDir == "" || view.MetaDir == "" {
		t.Error("状态应包含目录信息")
	}

	// 新建任务
	task, err := svc.AddTask(AddTaskRequest{Source: srv.URL + "/payload.bin", Name: "gui.bin"})
	if err != nil {
		t.Fatalf("AddTask 失败: %v", err)
	}
	if task.ID == "" || task.DisplayName != "gui.bin" {
		t.Fatalf("任务视图异常: %+v", task)
	}

	// 轮询状态直到完成（前端就是这么做的）
	waitFor(t, 30*time.Second, "任务完成", func() bool {
		st := svc.GetState()
		for _, tk := range st.Tasks {
			if tk.ID == task.ID && tk.State == "completed" {
				return true
			}
		}
		return false
	})

	state := svc.GetState()
	if state.Stats.Completed != 1 || state.Stats.Total != 1 {
		t.Fatalf("统计异常: %+v", state.Stats)
	}
	// 进度必须是 1，且与 CLI 读到的数值一致（同一个控制面）。
	for _, tk := range state.Tasks {
		if tk.ID != task.ID {
			continue
		}
		if tk.Progress != 1 {
			t.Errorf("完成后进度 = %v, 期望 1", tk.Progress)
		}
		if tk.Total != int64(len(content)) {
			t.Errorf("总大小 = %d, 期望 %d", tk.Total, len(content))
		}
	}

	// 校验与删除
	if err := svc.RecheckTask(task.ID); err != nil {
		t.Fatalf("RecheckTask 失败: %v", err)
	}
	if err := svc.RemoveTask(task.ID, false); err != nil {
		t.Fatalf("RemoveTask 失败: %v", err)
	}
	if got := len(svc.GetState().Tasks); got != 0 {
		t.Fatalf("删除后任务数 = %d", got)
	}
	// 重复删除应报错
	if err := svc.RemoveTask(task.ID, false); err == nil {
		t.Error("删除不存在的任务应报错")
	}
}

func TestServicePauseAndResume(t *testing.T) {
	content := make([]byte, 2<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	srv := rangeServer(t, content)

	dir := tempDir(t)
	svc := newTestService(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc.Startup(ctx)
	t.Cleanup(func() { svc.Shutdown(context.Background()) })

	task, err := svc.AddTask(AddTaskRequest{Source: srv.URL + "/payload.bin", Name: "pause.bin", Paused: true})
	if err != nil {
		t.Fatalf("AddTask 失败: %v", err)
	}
	if task.State != "paused" {
		t.Fatalf("paused=true 时初始状态 = %q", task.State)
	}

	if err := svc.ResumeTask(task.ID); err != nil {
		t.Fatalf("ResumeTask 失败: %v", err)
	}
	if err := svc.PauseTask(task.ID); err != nil {
		t.Fatalf("PauseTask 失败: %v", err)
	}
	waitFor(t, 20*time.Second, "任务进入暂停态", func() bool {
		for _, tk := range svc.GetState().Tasks {
			if tk.ID == task.ID && tk.State == "paused" {
				return true
			}
		}
		return false
	})
}

func TestServiceValidationAndEngineDown(t *testing.T) {
	dir := tempDir(t)
	svc := newTestService(t, dir)
	// 不调用 Startup：模拟引擎未运行（也验证了「界面仍可用」）。

	if _, err := svc.AddTask(AddTaskRequest{Source: "   "}); err == nil {
		t.Error("空来源应被拒绝")
	}
	if err := svc.PauseTask(""); err == nil {
		t.Error("空任务 ID 应被拒绝")
	}
	if err := svc.SetRateLimit(-1); err == nil {
		t.Error("负限速应被拒绝")
	}
	if err := svc.RemoveTask("x", false); err == nil {
		t.Error("引擎未运行时删除应报错")
	}

	// GetState 必须能在引擎未运行时安全返回，前端才不会卡死。
	state := svc.GetState()
	if state.EngineRunning {
		t.Error("引擎未启动时不应报告运行中")
	}
	if state.Tasks == nil {
		t.Error("任务列表应为空切片而不是 nil（前端会直接遍历）")
	}
	if state.DataDir == "" {
		t.Error("状态仍应包含本地配置信息")
	}
}

func TestServiceRateLimitAndLogs(t *testing.T) {
	dir := tempDir(t)
	svc := newTestService(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc.Startup(ctx)
	t.Cleanup(func() { svc.Shutdown(context.Background()) })

	if err := svc.SetRateLimit(1024 * 1024); err != nil {
		t.Fatalf("SetRateLimit 失败: %v", err)
	}
	state := svc.GetState()
	if state.RateLimit != 1024*1024 {
		t.Fatalf("限速未生效: %d", state.RateLimit)
	}

	if err := svc.SetRateLimit(0); err != nil {
		t.Fatalf("关闭限速失败: %v", err)
	}
	if got := svc.GetState().RateLimit; got != 0 {
		t.Fatalf("关闭后限速 = %d", got)
	}

	logs := svc.GetLogs()
	if len(logs) == 0 {
		t.Error("应记录启动日志")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "桌面壳已启动") {
		t.Errorf("日志缺少启动记录: %v", logs)
	}
}

// 前端会每秒轮询 GetState，必须保证它不会因为引擎瞬时不可用而阻塞。
func TestServiceGetStateIsFast(t *testing.T) {
	dir := tempDir(t)
	svc := newTestService(t, dir)
	svc.autoStart = false

	start := time.Now()
	for i := 0; i < 20; i++ {
		_ = svc.GetState()
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("20 次 GetState 耗时 %v，界面会明显卡顿", elapsed)
	}
}

// slowServer 支持 Range 但慢速输出，用于制造「下载到一半」的稳定状态。
func slowServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	size := int64(len(content))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := int64(0), size-1
		status := http.StatusOK
		if raw := r.Header.Get("Range"); raw != "" {
			spec := strings.TrimPrefix(raw, "bytes=")
			startStr, endStr, _ := strings.Cut(spec, "-")
			s, err1 := strconv.ParseInt(startStr, 10, 64)
			e := size - 1
			var err2 error
			if endStr != "" {
				e, err2 = strconv.ParseInt(endStr, 10, 64)
			}
			if err1 != nil || err2 != nil || s < 0 || e >= size || s > e {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start, end = s, e
			status = http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}
		const step = 32 << 10
		for off := start; off <= end; off += step {
			last := off + step - 1
			if last > end {
				last = end
			}
			if _, err := w.Write(content[off : last+1]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(25 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}
