// Package config 管理用户设置的持久化。
//
// 设置分为两类，界面上必须区分对待：
//
//   - **运行期可改**（限速、通知开关）：立刻生效，无需重启；
//   - **启动期生效**（下载目录、并发数、端口）：写入配置文件，下次启动引擎时读取。
//
// 之所以不通过控制面热改第二类：它们会影响已存在任务的数据布局
// （例如换下载目录会让断点找不到文件），热改只会制造难以排查的不一致。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"p2pdl/internal/engine"
)

// 与 app 包保持一致的默认值。此处不 import app 以避免循环依赖。
const (
	defaultChunkSize           = int64(1 << 20)
	defaultConcurrency         = 8
	defaultMaxConcurrentTasks  = 3
	settingsFileName           = "settings.json"
	settingsDirName            = "p2pdl"
)

// Settings 是全部可配置项。
type Settings struct {
	// DataDir 是下载目录（启动期生效）。
	DataDir string `json:"dataDir"`
	// MetaDir 是元数据目录（启动期生效）。
	MetaDir string `json:"metaDir"`
	// Port 是控制面端口，0 表示自动分配（启动期生效）。
	Port int `json:"port"`
	// Concurrency 是单任务并发连接数（启动期生效）。
	Concurrency int `json:"concurrency"`
	// MaxConcurrentTasks 是同时运行的任务数上限（启动期生效）。
	MaxConcurrentTasks int `json:"maxConcurrentTasks"`
	// ChunkSize 是分片位图粒度（启动期生效）。
	ChunkSize int64 `json:"chunkSize"`
	// MaxMemory 是单任务缓冲区内存上限，0 表示不限（启动期生效）。
	MaxMemory int64 `json:"maxMemory"`
	// GlobalRateLimit 是初始全局限速，运行期可通过控制面调整。
	GlobalRateLimit int64 `json:"globalRateLimit"`
	// BuildManifest 表示校验通过后生成分块清单（便于后续定点修复）。
	BuildManifest bool `json:"buildManifest"`
	// AutoResume 表示启动时自动继续未完成的任务。
	// 关闭后，中断的任务会以「暂停」状态恢复，等待用户手动继续。
	AutoResume bool `json:"autoResume"`

	// 以下为纯界面偏好，不影响引擎。
	MinimizeToTray   bool   `json:"minimizeToTray"`
	NotifyOnComplete bool   `json:"notifyOnComplete"`
	ClipboardMonitor bool   `json:"clipboardMonitor"`
	LastAddSource    string `json:"lastAddSource,omitempty"`
}

// Default 返回默认设置。
func Default() Settings {
	return Settings{
		Concurrency:        defaultConcurrency,
		MaxConcurrentTasks: defaultMaxConcurrentTasks,
		ChunkSize:          defaultChunkSize,
		AutoResume:         true,
		NotifyOnComplete:   true,
		ClipboardMonitor:   true,
	}
}

// DefaultDataDir 返回默认下载目录。
func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "p2pdl-downloads"
	}
	return filepath.Join(home, "Downloads", "p2pdl")
}

// DefaultPath 返回设置的默认存放位置。
// Windows 上是 %APPDATA%\p2pdl\settings.json。
func DefaultPath() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, settingsDirName, settingsFileName)
	}
	return filepath.Join(".", settingsFileName)
}

// Normalize 补齐缺省值并清理路径。
func (s *Settings) Normalize() {
	if strings.TrimSpace(s.DataDir) == "" {
		s.DataDir = DefaultDataDir()
	}
	if s.MetaDir == "" {
		s.MetaDir = filepath.Join(s.DataDir, ".p2pdl")
	}
	if s.Concurrency <= 0 {
		s.Concurrency = defaultConcurrency
	}
	if s.MaxConcurrentTasks <= 0 {
		s.MaxConcurrentTasks = defaultMaxConcurrentTasks
	}
	if s.ChunkSize <= 0 {
		s.ChunkSize = defaultChunkSize
	}
	if s.Port < 0 || s.Port > 65535 {
		s.Port = 0
	}
	if s.GlobalRateLimit < 0 {
		s.GlobalRateLimit = 0
	}
	if s.MaxMemory < 0 {
		s.MaxMemory = 0
	}
}

// Validate 检查设置是否可用。持久化前调用，避免把非法值写进配置。
func (s Settings) Validate() error {
	if strings.TrimSpace(s.DataDir) == "" {
		return errors.New("下载目录不能为空")
	}
	if s.Concurrency < 1 || s.Concurrency > 64 {
		return fmt.Errorf("单任务并发连接数应在 1~64 之间，当前 %d", s.Concurrency)
	}
	if s.MaxConcurrentTasks < 1 || s.MaxConcurrentTasks > 32 {
		return fmt.Errorf("并发任务数应在 1~32 之间，当前 %d", s.MaxConcurrentTasks)
	}
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("端口应在 0~65535 之间，当前 %d", s.Port)
	}
	if s.ChunkSize < 64<<10 || s.ChunkSize > 64<<20 {
		return fmt.Errorf("分块大小应在 64KB~64MB 之间，当前 %d", s.ChunkSize)
	}
	if s.GlobalRateLimit < 0 {
		return errors.New("限速不能为负数")
	}
	return nil
}

// Load 读取设置；文件不存在时返回默认值（不视为错误）。
func Load(path string) (Settings, error) {
	if path == "" {
		path = DefaultPath()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s := Default()
			return s, nil
		}
		return Settings{}, fmt.Errorf("读取设置失败: %w", err)
	}
	// 从默认值出发反序列化：新增字段在旧配置文件上会自动取默认值。
	s := Default()
	if err := json.Unmarshal(raw, &s); err != nil {
		return Settings{}, fmt.Errorf("设置文件格式非法（%s）: %w", path, err)
	}
	s.Normalize()
	return s, nil
}

// Save 原子写入设置。
func Save(path string, s Settings) error {
	if path == "" {
		path = DefaultPath()
	}
	s.Normalize()
	if err := s.Validate(); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化设置失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建设置目录失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o644); err != nil {
		return fmt.Errorf("写入设置失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("提交设置失败: %w", err)
	}
	return nil
}

// RestartRequiredFields 返回「修改后需要重启引擎才生效」的字段名，供界面提示。
func RestartRequiredFields() []string {
	return []string{"dataDir", "metaDir", "port", "concurrency", "maxConcurrentTasks", "chunkSize", "maxMemory"}
}

// DiffRestartRequired 比较两份设置，返回需要重启才能生效且确有变化的字段。
func DiffRestartRequired(old, new Settings) []string {
	var changed []string
	if old.DataDir != new.DataDir {
		changed = append(changed, "dataDir")
	}
	if old.MetaDir != new.MetaDir {
		changed = append(changed, "metaDir")
	}
	if old.Port != new.Port {
		changed = append(changed, "port")
	}
	if old.Concurrency != new.Concurrency {
		changed = append(changed, "concurrency")
	}
	if old.MaxConcurrentTasks != new.MaxConcurrentTasks {
		changed = append(changed, "maxConcurrentTasks")
	}
	if old.ChunkSize != new.ChunkSize {
		changed = append(changed, "chunkSize")
	}
	if old.MaxMemory != new.MaxMemory {
		changed = append(changed, "maxMemory")
	}
	return changed
}

// VerifySpecFromHash 把界面输入的摘要转换为校验配置。
func VerifySpecFromHash(algo, value string) (engine.VerifySpec, error) {
	spec := engine.VerifySpec{Algorithm: algo, Value: value}.Normalize()
	if spec.Algorithm == "" {
		spec.Algorithm = "sha256"
	}
	if err := spec.Validate(); err != nil {
		return engine.VerifySpec{}, err
	}
	return spec, nil
}
