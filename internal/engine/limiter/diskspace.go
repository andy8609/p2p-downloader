package limiter

import (
	"fmt"
	"os"
	"path/filepath"
)

// FreeSpace 返回 path 所在卷的可用字节数（非特权用户视角）。
// 具体实现按平台分文件提供。
func freeSpaceChecked(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("limiter: 路径为空")
	}
	probe := path
	if _, err := os.Stat(probe); err != nil {
		// 目录可能尚未创建：退到最近的已存在祖先目录。
		for {
			parent := filepath.Dir(probe)
			if parent == probe {
				return "", fmt.Errorf("limiter: 无法定位 %q 所在卷: %w", path, err)
			}
			probe = parent
			if _, statErr := os.Stat(probe); statErr == nil {
				break
			}
		}
	}
	return probe, nil
}

// FreeSpace 返回 path 所在卷的可用字节数。
func FreeSpace(path string) (int64, error) {
	probe, err := freeSpaceChecked(path)
	if err != nil {
		return 0, err
	}
	return freeSpace(probe)
}

// EnsureFreeSpace 在可用空间不足以容纳 need 字节时返回 ErrInsufficientSpace。
//
// 它的价值在于把「下载到 8.7GB 时磁盘写满」提前到「开始前就拒绝任务」，
// 避免用户白等几十分钟后再看到失败。
func EnsureFreeSpace(path string, need int64) error {
	if need <= 0 {
		return nil
	}
	free, err := FreeSpace(path)
	if err != nil {
		return err
	}
	if free < need {
		return fmt.Errorf("%w: 需要 %s，可用 %s（路径 %s）",
			ErrInsufficientSpace, FormatBytes(need), FormatBytes(free), path)
	}
	return nil
}
