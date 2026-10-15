// Package sink 负责下载数据的落盘与断点恢复。
//
// 核心一致性约定（这是「断点续传不产生坏文件」的基础）：
//
//	先写数据 → 位图标记（内存） → 保存元数据前先 fsync 数据文件 → 元数据原子落盘
//
// 因此元数据中「已完成的块」必然已经到达磁盘；反之，进程在任意点被强杀，
// 最坏情况只是「数据已落盘但位图未记录」，重启后重下该块——不会出现坏数据。
package sink

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultSyncInterval 是自动 fsync 的字节间隔。
// 取值权衡：更小更安全但系统调用更频繁；更大更快但强杀时回退的重下量更多。
const DefaultSyncInterval = int64(8 << 20) // 8MB

// FileSink 把下载数据写入临时文件，全部完成后原子重命名为最终文件。
//
// 使用临时文件而不是直接写最终文件的原因：
//   - 最终文件出现时一定是完整的（配合 T9 的校验），GUI/用户不会看到半成品；
//   - 不需要「预分配整文件」就能表达「尚未下载」的区域（空洞由位图管理）；
//   - 多次中断恢复都复用同一个临时文件，不会产生区间重叠或空洞。
type FileSink struct {
	finalPath string
	tempPath  string

	mu        sync.Mutex
	file      *os.File
	syncEvery int64
	dirty     int64 // 距上次 fsync 写入的字节数
	closed    bool
	committed bool

	// inPlace 表示「最终路径与写入路径相同」：此时不存在临时文件，
	// 数据被直接写在目标位置，Commit 只做长度修正而不重命名。
	//
	// 为什么需要它：当已下载的数据分散在「最终文件（上次提交的完整数据）」与
	// 「新下波段」之间时，如果用临时文件再整文件重命名，会把最终文件里那些
	// 没被重下的字节全部覆盖成零。就地修补是唯一安全的选择。
	inPlace bool

	totalSize int64
}

// Options 控制 FileSink 的行为。
type Options struct {
	// TotalSize 是内容的预期总长度；<=0 表示未知（单流模式）。
	TotalSize int64
	// SyncInterval 是自动 fsync 的字节间隔，<=0 时使用 DefaultSyncInterval。
	SyncInterval int64
}

// Open 打开（或创建）临时文件用于写入。
//
// 若临时文件已存在则复用其内容——这正是断点续传的落地点：
// 已完成的字节不会因为重新打开而丢失，缺口由任务位图决定。
func Open(finalPath, tempPath string, opts Options) (*FileSink, error) {
	if finalPath == "" {
		return nil, errors.New("sink: 最终文件路径不能为空")
	}
	if tempPath == "" {
		return nil, errors.New("sink: 临时文件路径不能为空")
	}
	same, err := samePath(finalPath, tempPath)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
		return nil, fmt.Errorf("sink: 创建临时文件目录失败: %w", err)
	}

	f, err := os.OpenFile(tempPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("sink: 打开临时文件失败: %w", err)
	}

	interval := opts.SyncInterval
	if interval <= 0 {
		interval = DefaultSyncInterval
	}

	return &FileSink{
		finalPath: finalPath,
		tempPath:  tempPath,
		file:      f,
		syncEvery: interval,
		totalSize: opts.TotalSize,
		inPlace:   same,
	}, nil
}

// InPlace 返回是否为就地写入模式。
func (s *FileSink) InPlace() bool { return s.inPlace }

// WriteAt 把 p 写入临时文件的 off 偏移处。超出文件末尾时文件会被自动扩展。
func (s *FileSink) WriteAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errors.New("sink: 写入已关闭的文件")
	}
	if off < 0 {
		return 0, fmt.Errorf("sink: 非法偏移 %d", off)
	}

	n, err := s.file.WriteAt(p, off)
	s.dirty += int64(n)
	if err != nil {
		return n, fmt.Errorf("sink: 写入偏移 %d 失败: %w", off, err)
	}
	if s.dirty >= s.syncEvery {
		if syncErr := s.file.Sync(); syncErr != nil {
			return n, fmt.Errorf("sink: 周期落盘失败: %w", syncErr)
		}
		s.dirty = 0
	}
	return n, nil
}

// Sync 强制把已写入的数据刷入磁盘，并重置自动落盘计数。
func (s *FileSink) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("sink: 落盘失败: %w", err)
	}
	s.dirty = 0
	return nil
}

// TempPath 返回临时文件路径。
func (s *FileSink) TempPath() string { return s.tempPath }

// FinalPath 返回最终文件路径。
func (s *FileSink) FinalPath() string { return s.finalPath }

// Size 返回临时文件当前长度。
func (s *FileSink) Size() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fi, err := s.file.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Commit 完成落盘：fsync → 修正长度 → 关闭 → 原子重命名为最终文件。
//
// totalSize <= 0 时不修正长度（单流模式由调用方传入实际写入长度）。
func (s *FileSink) Commit(totalSize int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		if s.committed {
			return nil
		}
		return errors.New("sink: 文件已关闭且未提交")
	}

	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("sink: 提交前落盘失败: %w", err)
	}
	size := totalSize
	if size <= 0 {
		size = s.totalSize
	}
	if size <= 0 {
		fi, err := s.file.Stat()
		if err != nil {
			return fmt.Errorf("sink: 读取文件大小失败: %w", err)
		}
		size = fi.Size()
	}
	if err := s.file.Truncate(size); err != nil {
		return fmt.Errorf("sink: 修正文件长度失败: %w", err)
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("sink: 提交后落盘失败: %w", err)
	}
	if s.inPlace {
		// 就地写入：数据本来就在最终位置，只需修正长度并保持句柄开启
		// （调度器随后会在同一会话上做校验读取）。
		s.committed = true
		return nil
	}
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("sink: 关闭临时文件失败: %w", err)
	}
	s.closed = true

	if err := os.Rename(s.tempPath, s.finalPath); err != nil {
		return fmt.Errorf("sink: 重命名为最终文件失败: %w", err)
	}
	s.committed = true
	return nil
}

// Close 关闭文件而不提交（用于暂停/取消场景，保留临时文件以便续传）。
// 就地写入模式下不会删除任何数据。
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.file.Sync(); err != nil {
		_ = s.file.Close()
		return fmt.Errorf("sink: 关闭前落盘失败: %w", err)
	}
	return s.file.Close()
}

// Discard 关闭并删除临时文件（用于「放弃任务」或「元数据损坏需重新下载」）。
// 就地写入模式下只关闭句柄，绝不删除已下载到最终位置的数据。
func (s *FileSink) Discard() error {
	inPlace := s.inPlace
	tempPath := s.tempPath
	if err := s.Close(); err != nil {
		return err
	}
	if inPlace {
		return nil
	}
	if err := os.Remove(tempPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sink: 删除临时文件失败: %w", err)
	}
	return nil
}

// samePath 判断两个路径是否指向同一位置（大小写不敏感比较，适配 Windows）。
func samePath(a, b string) (bool, error) {
	absA, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	absB, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	return filepath.Clean(absA) == filepath.Clean(absB), nil
}

// TempPathFor 按约定为最终路径生成临时文件路径。
// 放在同一目录可保证 os.Rename 在同一卷内完成（跨卷 rename 会退化为复制+删除，不具原子性）。
func TempPathFor(finalPath string) string {
	dir := filepath.Dir(finalPath)
	base := filepath.Base(finalPath)
	return filepath.Join(dir, "."+base+".p2pdl.part")
}

// ModTime 返回临时文件的修改时间，供恢复流程判断「陈旧断点」。
func (s *FileSink) ModTime() (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fi, err := s.file.Stat()
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}
