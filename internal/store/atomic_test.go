package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAtomicReplacesContentAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")

	if err := WriteFileAtomic(path, []byte("v1"), 0o644); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	if err := WriteFileAtomic(path, []byte("v2-longer"), 0o644); err != nil {
		t.Fatalf("覆盖写入失败: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(got) != "v2-longer" {
		t.Fatalf("内容 = %q, 期望 v2-longer", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("残留临时文件 %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("目录中文件数 = %d, 期望 1", len(entries))
	}
}

func TestWriteFileAtomicCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "data.bin")
	if err := WriteFileAtomic(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("目标文件不存在: %v", err)
	}
}
