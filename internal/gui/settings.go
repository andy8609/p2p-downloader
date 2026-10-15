package gui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"p2pdl/internal/config"
	"p2pdl/internal/engine"
)

// historyWindow 是速率曲线保留的采样点数（配合 1 秒轮询约等于 2 分钟）。
const historyWindow = 120

// RateSample 是速率曲线上的一个点。
type RateSample struct {
	At   time.Time `json:"at"`
	Rate float64   `json:"rate"`
	Done int64     `json:"done"`
}

// Notification 是待展示给用户的提示。
type Notification struct {
	ID    string    `json:"id"`
	Title string    `json:"title"`
	Body  string    `json:"body"`
	Kind  string    `json:"kind"` // info | success | error
	At    time.Time `json:"at"`
}

// SettingsView 是设置界面的完整数据：当前值 + 哪些项需要重启才生效。
type SettingsView struct {
	Settings config.Settings `json:"settings"`
	// SettingsPath 是配置文件位置，便于用户直接编辑。
	SettingsPath string `json:"settingsPath"`
	// RestartRequired 列出「修改后需重启引擎才生效」的字段名。
	RestartRequired []string `json:"restartRequired"`
	// PendingRestart 列出「已保存但尚未生效」的字段（下次启动应用时应用）。
	PendingRestart []string `json:"pendingRestart,omitempty"`
}

// GetSettings 返回当前设置。
func (s *Service) GetSettings() SettingsView {
	s.mu.Lock()
	settings := s.settings
	pending := append([]string(nil), s.pendingRestart...)
	s.mu.Unlock()
	return SettingsView{
		Settings:        settings,
		SettingsPath:    s.settingsPath,
		RestartRequired: config.RestartRequiredFields(),
		PendingRestart:  pending,
	}
}

// SaveSettings 持久化设置。
//
// 行为约定：
//   - 运行期可改的项（限速、通知开关）立即生效；
//   - 启动期项（目录、并发、端口）写入配置文件，界面会提示需要重启。
func (s *Service) SaveSettings(next config.Settings) (SettingsView, error) {
	next.Normalize()
	if err := next.Validate(); err != nil {
		return SettingsView{}, err
	}

	s.mu.Lock()
	previous := s.settings
	s.settings = next
	s.mu.Unlock()

	if err := config.Save(s.settingsPath, next); err != nil {
		// 保存失败要回滚内存状态，否则界面显示的值与实际持久化的不一致。
		s.mu.Lock()
		s.settings = previous
		s.mu.Unlock()
		return SettingsView{}, err
	}

	// 运行期可立即生效的一项：全局限速。
	if next.GlobalRateLimit != previous.GlobalRateLimit {
		if err := s.SetRateLimit(next.GlobalRateLimit); err != nil {
			s.recordLog("应用新限速失败: %v", err)
		}
	}

	pending := config.DiffRestartRequired(previous, next)
	s.mu.Lock()
	s.pendingRestart = pending
	s.mu.Unlock()

	if len(pending) > 0 {
		s.recordLog("设置已保存，以下项需要重启引擎后生效：%v", pending)
	} else {
		s.recordLog("设置已保存并生效")
	}
	return s.GetSettings(), nil
}

// AppliedSettings 返回启动引擎时应当使用的设置。
func (s *Service) AppliedSettings() config.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// ---------------------------------------------------------------------------
// 速率历史
// ---------------------------------------------------------------------------

type historyStore struct {
	mu     sync.Mutex
	points map[string][]RateSample
}

func newHistoryStore() *historyStore {
	return &historyStore{points: make(map[string][]RateSample)}
}

// record 追加采样点，并丢弃过期数据。
func (h *historyStore) record(id string, sample RateSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	series := append(h.points[id], sample)
	if len(series) > historyWindow {
		series = series[len(series)-historyWindow:]
	}
	h.points[id] = series
}

// series 返回某个任务的速率曲线副本。
func (h *historyStore) series(id string) []RateSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]RateSample, len(h.points[id]))
	copy(out, h.points[id])
	return out
}

// prune 删除已不存在任务的曲线，避免长期运行后内存泄漏。
func (h *historyStore) prune(alive map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.points {
		if !alive[id] {
			delete(h.points, id)
		}
	}
}

// GetHistory 返回指定任务的速率曲线（最多 120 个点，约 2 分钟）。
func (s *Service) GetHistory(id string) []RateSample {
	return s.history.series(id)
}

// ---------------------------------------------------------------------------
// 通知
// ---------------------------------------------------------------------------

// Notifications 返回并清空待展示的通知。
//
// 用「拉取即清空」而不是事件推送：前端本来就是每秒轮询，
// 这样无需维护额外的订阅状态，也不会因为界面卡顿而丢通知。
func (s *Service) Notifications() []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.notifications) == 0 {
		return []Notification{}
	}
	out := append([]Notification(nil), s.notifications...)
	s.notifications = nil
	return out
}

func (s *Service) addNotification(kind, title, body string) {
	if !s.settings.NotifyOnComplete {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifications = append(s.notifications, Notification{
		ID:    fmt.Sprintf("%d", time.Now().UnixNano()),
		Title: title,
		Body:  body,
		Kind:  kind,
		At:    time.Now(),
	})
	if len(s.notifications) > 50 {
		s.notifications = s.notifications[len(s.notifications)-50:]
	}
}

// trackTransitions 对比上一次轮询的任务状态，产生通知与速率采样。
// 由 GetState 调用，保证前端每次轮询都能拿到最新的一致视图。
func (s *Service) trackTransitions(tasks []engine.TaskView) {
	alive := make(map[string]bool, len(tasks))
	previous := s.snapshotStates()

	for _, task := range tasks {
		alive[task.ID] = true
		if task.State == engine.StateRunning || task.State == engine.StatePaused {
			// 暂停时速率应记为 0，避免曲线在暂停期间仍显示旧值。
			rate := task.Rate
			if task.State != engine.StateRunning {
				rate = 0
			}
			s.history.record(task.ID, RateSample{At: time.Now(), Rate: rate, Done: task.Done})
		}

		old, existed := previous[task.ID]
		if !existed {
			continue
		}
		if old.State == task.State {
			continue
		}
		switch task.State {
		case engine.StateCompleted:
			s.addNotification("success", "下载完成", fmt.Sprintf("%s（%s）", task.DisplayName, formatBytes(task.Total)))
			s.recordLog("任务完成: %s", task.DisplayName)
		case engine.StateFailed:
			s.addNotification("error", "下载失败",
				fmt.Sprintf("%s：%s", task.DisplayName, truncateText(task.LastError, 120)))
			s.recordLog("任务失败: %s（%s）", task.DisplayName, task.LastError)
		}
	}

	s.history.prune(alive)
	s.storeStates(tasks)
}

func (s *Service) snapshotStates() map[string]engine.TaskView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]engine.TaskView, len(s.lastStates))
	for k, v := range s.lastStates {
		out[k] = v
	}
	return out
}

func (s *Service) storeStates(tasks []engine.TaskView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastStates = make(map[string]engine.TaskView, len(tasks))
	for _, t := range tasks {
		s.lastStates[t.ID] = t
	}
}

// ---------------------------------------------------------------------------
// 文件/目录选择
// ---------------------------------------------------------------------------

// PickTorrentFile 打开文件选择对话框挑选 .torrent。
func (s *Service) PickTorrentFile() (string, error) {
	if s.opts.PickFile == nil {
		return "", errors.New("当前环境不支持文件选择对话框")
	}
	return s.opts.PickFile(s.context())
}

// PickDataDir 打开目录选择对话框挑选下载目录。
func (s *Service) PickDataDir() (string, error) {
	if s.opts.PickDirectory == nil {
		return "", errors.New("当前环境不支持目录选择对话框")
	}
	return s.opts.PickDirectory(s.context())
}

// OpenContainingFolder 在资源管理器中定位已下载的文件。
func (s *Service) OpenContainingFolder(path string) error {
	path = filepath.Clean(path)
	if path == "" || path == "." {
		return errors.New("缺少文件路径")
	}
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		// 文件可能已被移动或删除：退化为打开其所在目录。
		return s.OpenPath(filepath.Dir(path))
	}
	return s.OpenPath(path)
}

// PickFile 打开文件选择对话框，由宿主注入（Wails 实现）。
func formatBytes(n int64) string {
	if n <= 0 {
		return "未知大小"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}

func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

var _ = context.Background
