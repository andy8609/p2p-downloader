package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic 以「临时文件 + fsync + rename」的方式原子写入文件。
//
// 这是断点续传元数据不被打断的前提：进程在任意时刻被强杀，
// 目标文件要么是旧内容、要么是新内容，绝不会是半写状态。
//
// 注意：不执行目录 fsync（Windows 不支持对目录调用 fsync），
// 因此极端断电场景下 rename 本身可能丢失，但不会出现内容撕裂。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: 创建目录 %s 失败: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("store: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()

	// 任何失败路径都要清理临时文件，避免 .tmp-* 堆积。
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: 写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("store: 落盘临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: 关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("store: 设置文件权限失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("store: 原子替换 %s 失败: %w", path, err)
	}
	return nil
}

// 错误集合：调用方用 errors.Is 判断，不要用字符串比较。
var (
	// ErrNotFound 表示请求的任务元数据不存在。
	ErrNotFound = errors.New("store: 任务不存在")
	// ErrCorrupt 表示元数据存在但已损坏。调用方应据此降级为「重新下载」，
	// 而不是让整个进程崩溃或用错误进度继续下载。
	ErrCorrupt = errors.New("store: 元数据损坏")
	// ErrInvalidID 表示任务 ID 不安全（可能被用于路径穿越）。
	ErrInvalidID = errors.New("store: 非法的任务 ID")
)

// IsNotFound 判断错误是否为「任务不存在」。
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsCorrupt 判断错误是否为「元数据损坏」。
func IsCorrupt(err error) bool { return errors.Is(err, ErrCorrupt) }

// IsInvalidID 判断错误是否为「非法任务 ID」。
func IsInvalidID(err error) bool { return errors.Is(err, ErrInvalidID) }
