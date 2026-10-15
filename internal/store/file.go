package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"p2pdl/internal/engine"
)

const (
	// headerLine 是任务元数据文件的固定首行，包含格式标识与版本号。
	// 它让「文件不是本工具写的」与「文件被截断」可以被立刻识别，
	// 而不是把任意 JSON 当成任务读进来。
	headerLine = "P2PDL-STORE 1"

	taskExt   = ".dltask"
	indexName = "tasks.json"

	corruptDirName = "corrupt"
	indexVersion   = 1
)

// FileStore 是基于文件系统的任务元数据存储实现（T1 选型结论 A）。
//
// 布局：
//
//	<dir>/tasks.json          任务索引（ID 列表），损坏时可自动重建
//	<dir>/<id>.dltask         单个任务的完整状态，原子写入
//	<dir>/corrupt/<id>.<ts>.bak  被隔离的损坏元数据，保留供人工排查
//
// FileStore 可被多个 goroutine 并发使用。
type FileStore struct {
	dir string
	mu  sync.Mutex
}

// NewFileStore 创建（或打开）位于 dir 的存储。
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("store: 存储目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: 创建存储目录失败: %w", err)
	}
	return &FileStore{dir: dir}, nil
}

// Dir 返回存储根目录。
func (s *FileStore) Dir() string { return s.dir }

// indexFile 是 tasks.json 的结构。保留 Version 以便未来格式演进时能识别旧文件。
type indexFile struct {
	Version int      `json:"version"`
	TaskIDs []string `json:"taskIds"`
}

// Save 原子写入任务元数据，并把其 ID 登记到索引中。
// 写入前会执行 Task.Validate，拒绝把自相矛盾的状态落盘。
func (s *FileStore) Save(t *engine.Task) error {
	if t == nil {
		return errors.New("store: 任务为空")
	}
	if err := t.Validate(); err != nil {
		return fmt.Errorf("store: 拒绝保存不自洽的任务: %w", err)
	}
	path, err := s.taskPath(t.ID)
	if err != nil {
		return err
	}

	payload, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("store: 序列化任务失败: %w", err)
	}
	data := make([]byte, 0, len(payload)+len(headerLine)+1)
	data = append(data, headerLine...)
	data = append(data, '\n')
	data = append(data, payload...)
	data = append(data, '\n')

	if err := WriteFileAtomic(path, data, 0o644); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addToIndexLocked(t.ID)
}

// Load 读回任务元数据。
// 返回的错误可用 IsNotFound / IsCorrupt / IsInvalidID 分类判断：
// 损坏时调用方应把任务降级为「重新下载」（可先 Quarantine 保留现场），而不是崩溃。
func (s *FileStore) Load(id string) (*engine.Task, error) {
	path, err := s.taskPath(id)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("store: 读取 %s 失败: %w", path, err)
	}
	return decodeTask(raw)
}

// decodeTask 解析元数据字节流，任何结构性问题都以 ErrCorrupt 包装返回。
func decodeTask(raw []byte) (*engine.Task, error) {
	head, body, ok := strings.Cut(string(raw), "\n")
	if !ok {
		return nil, fmt.Errorf("%w: 缺少文件头", ErrCorrupt)
	}
	if strings.TrimSpace(head) != headerLine {
		return nil, fmt.Errorf("%w: 文件头 %q 不符合预期 %q", ErrCorrupt, strings.TrimSpace(head), headerLine)
	}

	var t engine.Task
	if err := json.Unmarshal([]byte(body), &t); err != nil {
		return nil, fmt.Errorf("%w: JSON 解析失败: %v", ErrCorrupt, err)
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return &t, nil
}

// Delete 删除任务元数据并从索引中移除。任务不存在时不报错（幂等）。
func (s *FileStore) Delete(id string) error {
	path, err := s.taskPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: 删除 %s 失败: %w", path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.removeFromIndexLocked(id)
}

// List 返回全部任务 ID（字典序）。
//
// 索引与目录扫描的结果取并集：索引提供顺序与快速读取，目录扫描保证即使索引丢失、
// 损坏或被外部修改（例如手工放入元数据文件），任务也不会凭空消失。
// 两者不一致时会原子重写索引。
func (s *FileStore) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	indexed, indexErr := s.readIndexLocked()
	scanned, scanErr := s.scanTaskFiles()
	if scanErr != nil {
		if indexErr == nil {
			return indexed, nil
		}
		return nil, fmt.Errorf("store: 索引与目录扫描均失败: %w", scanErr)
	}

	merged, changed := mergeIDs(indexed, scanned)
	if indexErr != nil || changed {
		_ = s.writeIndexLocked(merged) // 重建失败不影响本次读取结果，仅索引仍待修复
	}
	return merged, nil
}

// mergeIDs 返回两个 ID 列表的并集（字典序）以及结果是否与 a 不同。
func mergeIDs(a, b []string) (merged []string, changed bool) {
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, id := range a {
		seen[id] = struct{}{}
	}
	changed = false
	for _, id := range b {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			changed = true
		}
	}
	if len(seen) != len(a) {
		changed = true
	}
	merged = make([]string, 0, len(seen))
	for id := range seen {
		merged = append(merged, id)
	}
	sort.Strings(merged)
	return merged, changed
}

// LoadAll 读回全部任务。返回的 corrupt 映射包含损坏任务的 ID 与原因；
// 单个任务损坏不会影响其余任务的加载（这是 T9「降级而非失败」的基础）。
func (s *FileStore) LoadAll() (tasks []*engine.Task, corrupt map[string]error, err error) {
	ids, err := s.List()
	if err != nil {
		return nil, nil, err
	}
	corrupt = make(map[string]error)
	for _, id := range ids {
		t, loadErr := s.Load(id)
		if loadErr != nil {
			if IsCorrupt(loadErr) {
				corrupt[id] = loadErr
				continue
			}
			return nil, corrupt, loadErr
		}
		tasks = append(tasks, t)
	}
	return tasks, corrupt, nil
}

// Quarantine 把损坏的元数据移动到 corrupt/ 子目录并保留时间戳，
// 随后从索引移除，使任务可以被重新创建。返回被移动文件的路径。
func (s *FileStore) Quarantine(id string) (string, error) {
	path, err := s.taskPath(id)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return "", err
	}

	dir := filepath.Join(s.dir, corruptDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("store: 创建隔离目录失败: %w", err)
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dest := filepath.Join(dir, fmt.Sprintf("%s.%s%s.bak", id, stamp, taskExt))
	if err := os.Rename(path, dest); err != nil {
		return "", fmt.Errorf("store: 隔离损坏元数据失败: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.removeFromIndexLocked(id); err != nil {
		return dest, err
	}
	return dest, nil
}

// ---------------------------------------------------------------------------
// 内部实现
// ---------------------------------------------------------------------------

// taskPath 校验 ID 并返回其元数据文件路径。
// ID 会被直接用作文件名，因此必须拒绝路径分隔符与相对路径片段。
func (s *FileStore) taskPath(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, id+taskExt), nil
}

func validateID(id string) error {
	if id == "" || len(id) > 200 {
		return fmt.Errorf("%w: 长度非法（%d）", ErrInvalidID, len(id))
	}
	if id == "." || id == ".." || strings.HasPrefix(id, ".") {
		return fmt.Errorf("%w: %q 不能以点开头", ErrInvalidID, id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("%w: %q 含非法字符 %q", ErrInvalidID, id, r)
		}
	}
	return nil
}

func (s *FileStore) indexPath() string { return filepath.Join(s.dir, indexName) }

func (s *FileStore) readIndexLocked() ([]string, error) {
	raw, err := os.ReadFile(s.indexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var idx indexFile
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("store: 索引解析失败: %w", err)
	}
	ids := make([]string, 0, len(idx.TaskIDs))
	for _, id := range idx.TaskIDs {
		if validateID(id) == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *FileStore) writeIndexLocked(ids []string) error {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	payload, err := json.MarshalIndent(indexFile{Version: indexVersion, TaskIDs: sorted}, "", "  ")
	if err != nil {
		return fmt.Errorf("store: 序列化索引失败: %w", err)
	}
	return WriteFileAtomic(s.indexPath(), append(payload, '\n'), 0o644)
}

func (s *FileStore) addToIndexLocked(id string) error {
	ids, err := s.readIndexLocked()
	if err != nil {
		// 索引损坏：以目录扫描结果为准重建，避免丢失既有任务。
		if ids, err = s.scanTaskFiles(); err != nil {
			return err
		}
	}
	for _, existing := range ids {
		if existing == id {
			return nil
		}
	}
	return s.writeIndexLocked(append(ids, id))
}

func (s *FileStore) removeFromIndexLocked(id string) error {
	ids, err := s.readIndexLocked()
	if err != nil {
		if ids, err = s.scanTaskFiles(); err != nil {
			return err
		}
	}
	out := ids[:0]
	for _, existing := range ids {
		if existing != id {
			out = append(out, existing)
		}
	}
	return s.writeIndexLocked(out)
}

// scanTaskFiles 扫描目录中合法的任务文件，作为索引的权威重建来源。
func (s *FileStore) scanTaskFiles() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("store: 扫描存储目录失败: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), taskExt) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), taskExt)
		if validateID(id) == nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
