// Package gui 是桌面壳的业务层：负责引擎进程的生命周期管理，
// 以及把控制面客户端包装成前端可直接调用的方法。
//
// 分成两层的原因：进程管理与状态查询是纯逻辑，可以被单元测试完整覆盖；
// 而 Wails 绑定（cmd/p2pdl-gui）只做参数转发，不承载业务规则。
// 桌面窗口本身无法在无人值守环境中自动验证，因此把「能测的部分尽量前置到这里」。
package gui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"p2pdl/internal/client"
)

// 引擎启动相关常量。
const (
	// defaultStartTimeout 是等待引擎就绪的最长时间。
	// 首次启动需要加载 BT 库与恢复任务队列，给足余量但不至于让用户以为卡死。
	defaultStartTimeout = 20 * time.Second
	// engineBinaryNames 是按优先级尝试的引擎可执行文件名。
	engineBinaryNames = "p2pdl-engine.exe,dlengine.exe,dl.exe,p2pdl-engine,dl"
	// DefaultPort 是 GUI 启动引擎时使用的控制面端口。
	// 固定端口便于排查；冲突时引擎会退回自动分配。
	DefaultPort = 0
)

// EngineManager 负责「发现或启动引擎进程」，并监视其存活。
//
// 设计要点：
//   - **优先复用已在运行的引擎**：用户可能先开了 CLI，GUI 不应再起一个（两个引擎会
//     争抢同一个数据目录的 piece completion 数据库，见 docs/bittorrent.md §3.3）；
//   - **引擎是独立进程**：GUI 崩溃或卡顿不影响下载；
//   - **崩溃可感知**：引擎进程退出后立刻更新状态，前端能看到并一键重启。
type EngineManager struct {
	// BinaryPath 是引擎可执行文件路径；为空时按 engineBinaryNames 自动查找。
	BinaryPath string
	// DataDir 是下载目录。
	DataDir string
	// MetaDir 是元数据目录。
	MetaDir string
	// Port 是控制面监听端口，0 表示自动分配。
	Port int
	// SettingsPath 是设置文件路径，会以 --settings 传给引擎，
	// 保证界面里改的「启动期设置」在引擎重启后真正生效。
	SettingsPath string
	// StartTimeout 覆盖默认启动等待时间。
	StartTimeout time.Duration
	// Logf 为 nil 时静默。
	Logf func(format string, args ...any)

	mu        sync.Mutex
	cmd       *exec.Cmd
	client    *client.Client
	lastErr   string
	startedAt time.Time
	owned     bool // 引擎是否由本管理器启动（决定是否可以在退出时终止它）
	waitOnce  sync.Once
}

func (m *EngineManager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// Status 是引擎的当前状态快照。
type Status struct {
	Running   bool      `json:"running"`
	Addr      string    `json:"addr"`
	Owned     bool      `json:"owned"`
	PID       int       `json:"pid"`
	LastError string    `json:"lastError,omitempty"`
	StartedAt time.Time `json:"startedAt,omitempty"`
}

// Status 返回引擎状态。
func (m *EngineManager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Owned: m.owned, LastError: m.lastErr, StartedAt: m.startedAt}
	if m.cmd != nil && m.cmd.Process != nil {
		st.PID = m.cmd.Process.Pid
	}
	if m.client != nil {
		st.Running = true
		st.Addr = m.client.BaseURL()
	}
	return st
}

// Client 返回已连接的控制面客户端；未连接时返回 nil。
func (m *EngineManager) Client() *client.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.client
}

// EnsureRunning 返回一个可用客户端：优先复用已有引擎，其次启动新进程。
func (m *EngineManager) EnsureRunning(ctx context.Context) (*client.Client, error) {
	if c := m.Client(); c != nil {
		if err := c.Health(ctx); err == nil {
			return c, nil
		}
		// 连接失效（引擎被外部杀掉）：清空后走重启路径。
		m.markDown("引擎连接失效")
	}

	if c, err := client.Discover(m.MetaDir); err == nil {
		m.attach(c, false)
		m.logf("gui: 复用已在运行的引擎 %s", c.BaseURL())
		return c, nil
	}

	return m.Start(ctx)
}

// Start 启动一个新的引擎进程并等待其就绪。
func (m *EngineManager) Start(ctx context.Context) (*client.Client, error) {
	m.mu.Lock()
	if m.cmd != nil {
		m.mu.Unlock()
		return nil, errors.New("引擎进程已存在")
	}
	m.mu.Unlock()

	binary, err := m.resolveBinary()
	if err != nil {
		m.markDown(err.Error())
		return nil, err
	}

	args := []string{"serve"}
	if m.SettingsPath != "" {
		args = append(args, "--settings", m.SettingsPath)
	}
	if m.DataDir != "" {
		args = append(args, "--data-dir", m.DataDir)
	}
	if m.MetaDir != "" {
		args = append(args, "--meta-dir", m.MetaDir)
	}
	if m.Port > 0 {
		args = append(args, "--port", fmt.Sprintf("%d", m.Port))
	}

	cmd := exec.Command(binary, args...)
	cmd.Dir = filepath.Dir(binary)
	// 引擎的标准输出交给 GUI 日志，避免子进程管道写满导致其阻塞。
	cmd.Stdout = logWriter{m.logf}
	cmd.Stderr = logWriter{m.logf}
	// 由桌面壳启动时不弹控制台窗口（引擎本身仍是控制台程序，可手动运行查看日志）。
	cmd.SysProcAttr = hideWindowAttr()

	if err := cmd.Start(); err != nil {
		m.markDown(fmt.Sprintf("启动引擎失败: %v", err))
		return nil, fmt.Errorf("启动引擎失败: %w", err)
	}
	m.logf("gui: 已启动引擎进程 pid=%d (%s)", cmd.Process.Pid, binary)

	m.mu.Lock()
	m.cmd = cmd
	m.owned = true
	m.lastErr = ""
	m.startedAt = time.Now()
	m.mu.Unlock()

	// 监视进程退出：无论是正常退出还是被强杀，都要让前端立刻知道。
	go m.watch(cmd)

	timeout := m.StartTimeout
	if timeout <= 0 {
		timeout = defaultStartTimeout
	}
	c, err := m.waitReady(ctx, timeout)
	if err != nil {
		m.markDown(err.Error())
		return nil, err
	}
	m.attach(c, true)
	return c, nil
}

// Restart 终止当前引擎（如果是本进程启动的）并重新启动。
func (m *EngineManager) Restart(ctx context.Context) (*client.Client, error) {
	m.Stop()
	return m.Start(ctx)
}

// Stop 终止由本管理器启动的引擎进程（不触碰外部启动的引擎）。
func (m *EngineManager) Stop() {
	m.mu.Lock()
	cmd := m.cmd
	owned := m.owned
	m.cmd = nil
	m.client = nil
	m.owned = false
	m.mu.Unlock()

	if cmd == nil || !owned || cmd.Process == nil {
		return
	}
	m.logf("gui: 正在终止引擎进程 pid=%d", cmd.Process.Pid)
	if runtime.GOOS == "windows" {
		// 引擎自身没有子进程，直接 Kill 即可。
		_ = cmd.Process.Kill()
	} else {
		_ = cmd.Process.Signal(os.Interrupt)
	}
	_, _ = cmd.Process.Wait()
}

// waitReady 轮询端点文件，直到引擎可用或超时。
func (m *EngineManager) waitReady(ctx context.Context, timeout time.Duration) (*client.Client, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c, err := client.Discover(m.MetaDir); err == nil {
			return c, nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = errors.New("等待引擎就绪超时")
			}
			return nil, fmt.Errorf("等待引擎就绪超时（%v）: %w", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// watch 监视引擎进程，退出时更新状态。
func (m *EngineManager) watch(cmd *exec.Cmd) {
	err := cmd.Wait()

	m.mu.Lock()
	stillCurrent := m.cmd == cmd
	if stillCurrent {
		m.cmd = nil
		m.client = nil
		m.owned = false
		if err != nil {
			m.lastErr = fmt.Sprintf("引擎进程已退出: %v", err)
		} else {
			m.lastErr = "引擎进程已退出"
		}
	}
	m.mu.Unlock()

	if stillCurrent {
		m.logf("gui: 引擎进程已退出（err=%v）", err)
	}
}

func (m *EngineManager) attach(c *client.Client, owned bool) {
	m.mu.Lock()
	m.client = c
	m.owned = owned
	m.lastErr = ""
	m.mu.Unlock()
}

func (m *EngineManager) markDown(reason string) {
	m.mu.Lock()
	m.client = nil
	m.lastErr = reason
	m.mu.Unlock()
}

// resolveBinary 查找引擎可执行文件。
//
// 查找顺序：显式配置 → 可执行文件同目录 → PATH。
// 打包后 GUI 与引擎放在同一目录，因此「同目录」是主路径。
func (m *EngineManager) resolveBinary() (string, error) {
	if m.BinaryPath != "" {
		if _, err := os.Stat(m.BinaryPath); err == nil {
			return m.BinaryPath, nil
		}
		return "", fmt.Errorf("指定的引擎路径不存在: %s", m.BinaryPath)
	}

	names := strings.Split(engineBinaryNames, ",")
	dirs := []string{}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}

	for _, dir := range dirs {
		for _, name := range names {
			candidate := filepath.Join(dir, strings.TrimSpace(name))
			if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
				return candidate, nil
			}
		}
	}
	for _, name := range names {
		if path, err := exec.LookPath(strings.TrimSpace(name)); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("未找到引擎可执行文件（期望 %s 与 GUI 位于同一目录）", engineBinaryNames)
}

// logWriter 把引擎输出转发到 GUI 日志。
type logWriter struct {
	logf func(format string, args ...any)
}

func (w logWriter) Write(p []byte) (int, error) {
	if w.logf != nil {
		text := strings.TrimRight(string(p), "\r\n")
		if text != "" {
			for _, line := range strings.Split(text, "\n") {
				w.logf("engine> %s", strings.TrimRight(line, "\r"))
			}
		}
	}
	return len(p), nil
}
