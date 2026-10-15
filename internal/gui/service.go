package gui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"p2pdl/internal/client"
	"p2pdl/internal/config"
	"p2pdl/internal/engine"
)

// ServiceOptions 配置桌面壳服务。
type ServiceOptions struct {
	// DataDir 是下载目录。
	DataDir string
	// MetaDir 是元数据目录。
	MetaDir string
	// EngineBinary 是引擎可执行文件路径；为空时自动查找。
	EngineBinary string
	// Port 是控制面端口，0 表示自动分配。
	Port int
	// SettingsPath 是设置文件路径；为空时使用 config.DefaultPath()。
	SettingsPath string
	// PickFile / PickDirectory 由宿主注入（Wails 的文件对话框）；
	// 不注入时相关方法会返回可读错误而不是崩溃。
	PickFile      func(ctx context.Context) (string, error)
	PickDirectory func(ctx context.Context) (string, error)
	// ConfirmClose 由宿主注入：窗口关闭时若有任务在跑，用它征求用户意见。
	ConfirmClose func(ctx context.Context, activeTasks int) (CloseChoice, error)
	// Logf 为 nil 时静默。
	Logf func(format string, args ...any)
}

// StateView 是前端渲染所需的全部状态。
type StateView struct {
	EngineRunning bool               `json:"engineRunning"`
	EngineAddr    string             `json:"engineAddr"`
	EnginePID     int                `json:"enginePid"`
	EngineOwned   bool               `json:"engineOwned"`
	EngineError   string             `json:"engineError,omitempty"`
	DataDir       string             `json:"dataDir"`
	MetaDir       string             `json:"metaDir"`
	Tasks         []engine.TaskView  `json:"tasks"`
	Stats         engine.Stats       `json:"stats"`
	RateLimit     int64              `json:"rateLimit"`
	Logs          []string           `json:"logs"`
	ServerTime    time.Time          `json:"serverTime"`
}

// AddTaskRequest 是前端新建任务的参数。
type AddTaskRequest struct {
	Source        string `json:"source"`
	Name          string `json:"name"`
	Paused        bool   `json:"paused"`
	ExpectedHash  string `json:"expectedHash"`
	HashAlgorithm string `json:"hashAlgorithm"`
	Peers         string `json:"peers"`
}

// Service 是暴露给前端（Wails 绑定）的桌面壳服务。
//
// 所有方法必须对「引擎未运行」保持健壮：前端在引擎崩溃后仍会继续轮询，
// 此时应返回可展示的错误信息，而不是让界面卡死或崩溃。
type Service struct {
	opts   ServiceOptions
	engine *EngineManager

	mu   sync.Mutex
	ctx  context.Context
	logs []string

	// settings 是当前生效的设置；pendingRestart 记录已保存但需重启才生效的字段。
	settings       config.Settings
	pendingRestart []string
	settingsPath   string

	history      *historyStore
	lastStates   map[string]engine.TaskView
	notifications []Notification

	// autoStart 为 false 时不会自动启动引擎（供测试控制）。
	autoStart bool
	// keepEngineOnExit 表示用户选择「关界面但继续下载」。
	keepEngineOnExit bool
}

// CloseChoice 是窗口关闭时的用户选择。
type CloseChoice int

const (
	// CloseStopEngine 停止引擎并退出（默认）。
	CloseStopEngine CloseChoice = iota
	// CloseKeepEngineRunning 保留引擎在后台继续下载，仅退出界面。
	CloseKeepEngineRunning
	// CloseCancel 取消关闭。
	CloseCancel
)

// NewService 创建服务。
func NewService(opts ServiceOptions) *Service {
	// 统一转成绝对路径：GUI 可能以相对路径收到参数，而引擎子进程的工作目录
	// 是它自己的可执行文件目录，相对路径会让下载落到意料之外的位置。
	opts.DataDir = absolutePath(opts.DataDir)
	opts.MetaDir = absolutePath(opts.MetaDir)
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	settingsPath := opts.SettingsPath
	if settingsPath == "" {
		settingsPath = config.DefaultPath()
	}
	settings, err := config.Load(settingsPath)
	if err != nil {
		// 配置损坏不应让界面起不来：退回默认值并记录，用户可在设置页重新保存。
		settings = config.Default()
		opts.logf("gui: 读取设置失败，已回退到默认值: %v", err)
	}
	// 显式传入的目录/端口优先于配置文件（命令行覆盖场景）。
	if opts.DataDir != "" {
		settings.DataDir = opts.DataDir
	}
	if opts.MetaDir != "" {
		settings.MetaDir = opts.MetaDir
	}
	if opts.Port != 0 {
		settings.Port = opts.Port
	}
	settings.Normalize()

	opts.DataDir = settings.DataDir
	opts.MetaDir = settings.MetaDir
	if opts.Port == 0 {
		opts.Port = settings.Port
	}

	return &Service{
		opts:         opts,
		autoStart:    true,
		settings:     settings,
		settingsPath: settingsPath,
		history:      newHistoryStore(),
		lastStates:   make(map[string]engine.TaskView),
		engine: &EngineManager{
			BinaryPath:   opts.EngineBinary,
			DataDir:      opts.DataDir,
			MetaDir:      opts.MetaDir,
			Port:         opts.Port,
			SettingsPath: settingsPath,
			Logf:         opts.logf,
		},
	}
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

func (o *ServiceOptions) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// recordLog 记录一条日志供前端展示（保留最近 200 条）。
func (s *Service) recordLog(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	s.mu.Lock()
	s.logs = append(s.logs, line)
	if len(s.logs) > 200 {
		s.logs = s.logs[len(s.logs)-200:]
	}
	s.mu.Unlock()
	s.opts.logf("%s", line)
}

// Startup 由 Wails 在窗口就绪时调用。
func (s *Service) Startup(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	s.recordLog("桌面壳已启动，下载目录 %s", s.opts.DataDir)
	if s.autoStart {
		if _, err := s.ensureEngine(ctx); err != nil {
			s.recordLog("启动引擎失败: %v", err)
		}
	}
}

// OnBeforeClose 在窗口即将关闭时调用，返回 true 表示阻止关闭。
//
// 行为：仍有任务在运行时弹出确认，避免用户以为关掉窗口下载就停了
// （实际上引擎是独立进程，关掉界面后下载会继续）。
func (s *Service) OnBeforeClose(ctx context.Context) bool {
	active := 0
	for _, t := range s.stateSnapshot() {
		if t.State == engine.StateRunning || t.State == engine.StatePending {
			active++
		}
	}
	if active == 0 {
		return false
	}
	if s.opts.ConfirmClose == nil {
		return false
	}
	choice, err := s.opts.ConfirmClose(ctx, active)
	if err != nil {
		s.recordLog("关闭确认失败，按取消处理: %v", err)
		return true
	}
	switch choice {
	case CloseCancel:
		return true
	case CloseKeepEngineRunning:
		// 保留引擎继续下载（不停止子进程）。
		s.mu.Lock()
		s.keepEngineOnExit = true
		s.mu.Unlock()
		return false
	default:
		return false
	}
}

// stateSnapshot 返回最近一次轮询得到的任务视图。
func (s *Service) stateSnapshot() []engine.TaskView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]engine.TaskView, 0, len(s.lastStates))
	for _, v := range s.lastStates {
		out = append(out, v)
	}
	return out
}

// Shutdown 由 Wails 在窗口关闭时调用。
func (s *Service) Shutdown(context.Context) {
	s.recordLog("桌面壳正在退出")
	s.mu.Lock()
	keepRunning := s.keepEngineOnExit
	s.mu.Unlock()
	if keepRunning {
		// 用户选择了「继续后台下载」：引擎是独立进程，界面退出不影响它。
		s.recordLog("按用户选择保留引擎继续下载（pid 见「引擎运行中」提示）")
		return
	}
	// 默认行为：关掉界面即完全退出，避免留下无人管理的孤儿引擎进程。
	s.engine.Stop()
}

// context 返回启动时注入的 context。
func (s *Service) context() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// ensureEngine 返回可用客户端，必要时启动引擎。
func (s *Service) ensureEngine(ctx context.Context) (*client.Client, error) {
	return s.engine.EnsureRunning(ctx)
}

// GetState 汇总前端需要的全部状态。前端按 1 秒轮询它。
func (s *Service) GetState() StateView {
	ctx := s.context()
	view := StateView{
		DataDir:    s.opts.DataDir,
		MetaDir:    s.opts.MetaDir,
		ServerTime: time.Now(),
	}

	status := s.engine.Status()
	view.EngineRunning = status.Running
	view.EngineAddr = status.Addr
	view.EnginePID = status.PID
	view.EngineOwned = status.Owned
	view.EngineError = status.LastError

	s.mu.Lock()
	view.Logs = append([]string(nil), s.logs...)
	s.mu.Unlock()

	if !status.Running {
		view.Tasks = []engine.TaskView{}
		return view
	}

	c := s.engine.Client()
	if c == nil {
		view.EngineRunning = false
		view.EngineError = "引擎客户端不可用"
		return view
	}

	// 查询超时要短：前端每秒轮询，长超时会拖住界面。
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tasks, err := c.ListTasks(queryCtx)
	if err != nil {
		view.EngineRunning = false
		view.EngineError = fmt.Sprintf("读取任务列表失败: %v", err)
		return view
	}
	view.Tasks = tasks
	if view.Tasks == nil {
		view.Tasks = []engine.TaskView{}
	}
	// 对比上一次轮询的结果，产生速率采样与完成/失败通知。
	s.trackTransitions(view.Tasks)

	if stats, err := c.Stats(queryCtx); err == nil {
		if raw, ok := stats["tasks"].(map[string]any); ok {
			view.Stats = statsFromMap(raw)
		}
		if limit, ok := stats["rateLimit"].(float64); ok {
			view.RateLimit = int64(limit)
		}
	}
	return view
}

func statsFromMap(m map[string]any) engine.Stats {
	num := func(key string) int {
		if v, ok := m[key].(float64); ok {
			return int(v)
		}
		return 0
	}
	return engine.Stats{
		Total:         num("total"),
		Running:       num("running"),
		Pending:       num("pending"),
		Paused:        num("paused"),
		Completed:     num("completed"),
		Failed:        num("failed"),
		MaxConcurrent: num("maxConcurrent"),
	}
}

// AddTask 新建任务。
func (s *Service) AddTask(req AddTaskRequest) (engine.TaskView, error) {
	source := strings.TrimSpace(req.Source)
	if source == "" {
		return engine.TaskView{}, errors.New("请填写下载地址或磁力链")
	}

	ctx, cancel := context.WithTimeout(s.context(), 60*time.Second)
	defer cancel()

	c, err := s.ensureEngine(ctx)
	if err != nil {
		return engine.TaskView{}, err
	}

	addReq := client.AddRequest{
		Name:          strings.TrimSpace(req.Name),
		Paused:        req.Paused,
		ExpectedHash:  strings.TrimSpace(req.ExpectedHash),
		HashAlgorithm: strings.TrimSpace(req.HashAlgorithm),
	}
	switch {
	case strings.HasPrefix(source, "magnet:"):
		addReq.Source = source
	case strings.HasPrefix(source, "http://"), strings.HasPrefix(source, "https://"):
		addReq.Source = source
	default:
		// 本地种子文件路径
		addReq.TorrentPath = source
	}
	for _, p := range strings.Split(req.Peers, ",") {
		if p = strings.TrimSpace(p); p != "" {
			addReq.Peers = append(addReq.Peers, p)
		}
	}

	task, err := c.AddTask(ctx, addReq)
	if err != nil {
		s.recordLog("新建任务失败: %v", err)
		return engine.TaskView{}, err
	}
	s.recordLog("已新建任务 %s（%s）", task.DisplayName, task.Kind)
	return task, nil
}

// PauseTask 暂停任务。
func (s *Service) PauseTask(id string) error { return s.taskAction(id, "pause") }

// ResumeTask 恢复任务。
func (s *Service) ResumeTask(id string) error { return s.taskAction(id, "resume") }

// RecheckTask 重新校验。
func (s *Service) RecheckTask(id string) error { return s.taskAction(id, "recheck") }

func (s *Service) taskAction(id, action string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("缺少任务 ID")
	}
	ctx, cancel := context.WithTimeout(s.context(), 30*time.Second)
	defer cancel()

	c := s.engine.Client()
	if c == nil {
		return errors.New("引擎未运行")
	}

	var err error
	switch action {
	case "pause":
		_, err = c.PauseTask(ctx, id)
	case "resume":
		_, err = c.ResumeTask(ctx, id)
	case "recheck":
		_, err = c.RecheckTask(ctx, id)
	}
	if err != nil {
		s.recordLog("任务 %s 执行 %s 失败: %v", id, action, err)
		return err
	}
	s.recordLog("任务 %s 已执行 %s", id, action)
	return nil
}

// RemoveTask 删除任务，deleteFiles 为 true 时同时删除数据。
func (s *Service) RemoveTask(id string, deleteFiles bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("缺少任务 ID")
	}
	ctx, cancel := context.WithTimeout(s.context(), 30*time.Second)
	defer cancel()

	c := s.engine.Client()
	if c == nil {
		return errors.New("引擎未运行")
	}
	if err := c.RemoveTask(ctx, id, deleteFiles); err != nil {
		s.recordLog("删除任务 %s 失败: %v", id, err)
		return err
	}
	s.recordLog("已删除任务 %s（含文件=%v）", id, deleteFiles)
	return nil
}

// RestartEngine 重启引擎进程。
func (s *Service) RestartEngine() error {
	ctx, cancel := context.WithTimeout(s.context(), 60*time.Second)
	defer cancel()

	s.recordLog("正在重启引擎…")
	if _, err := s.engine.Restart(ctx); err != nil {
		s.recordLog("重启引擎失败: %v", err)
		return err
	}
	s.recordLog("引擎已重启")
	return nil
}

// SetRateLimit 设置全局限速（字节/秒，0 表示不限）。
func (s *Service) SetRateLimit(bytesPerSecond int64) error {
	if bytesPerSecond < 0 {
		return errors.New("限速不能为负数")
	}
	ctx, cancel := context.WithTimeout(s.context(), 15*time.Second)
	defer cancel()

	c := s.engine.Client()
	if c == nil {
		return errors.New("引擎未运行")
	}
	if _, err := c.SetGlobalRateLimit(ctx, bytesPerSecond); err != nil {
		return err
	}
	s.recordLog("全局限速已设为 %d 字节/秒", bytesPerSecond)
	return nil
}

// OpenPath 用系统默认程序打开路径（下载目录、已完成文件）。
func (s *Service) OpenPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		path = s.opts.DataDir
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("无法打开 %s: %w", path, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// GetLogs 返回最近的日志行。
func (s *Service) GetLogs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.logs...)
}

// EngineStatus 返回引擎进程状态（供前端单独查询）。
func (s *Service) EngineStatus() Status { return s.engine.Status() }
