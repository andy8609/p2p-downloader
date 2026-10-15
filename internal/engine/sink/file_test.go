package sink

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"p2pdl/internal/engine"
)

func TestFileSinkWriteCommit(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "out.bin")
	tempPath := TempPathFor(finalPath)

	s, err := Open(finalPath, tempPath, Options{TotalSize: 16})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	// 乱序写入：分片可以任意顺序到达。
	if _, err := s.WriteAt([]byte("efgh"), 4); err != nil {
		t.Fatalf("WriteAt 失败: %v", err)
	}
	if _, err := s.WriteAt([]byte("abcd"), 0); err != nil {
		t.Fatalf("WriteAt 失败: %v", err)
	}
	if _, err := s.WriteAt([]byte("ijklmnop"), 8); err != nil {
		t.Fatalf("WriteAt 失败: %v", err)
	}

	// 提交前最终文件不应存在。
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Fatalf("提交前最终文件不应存在, 实际 err=%v", err)
	}

	if err := s.Commit(0); err != nil {
		t.Fatalf("Commit 失败: %v", err)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("读取最终文件失败: %v", err)
	}
	if string(got) != "abcdefghijklmnop" {
		t.Fatalf("最终内容 = %q", got)
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Error("提交后临时文件应已被重命名")
	}
}

// 提交时必须把文件长度修正为内容总长度，防止尾部残留多余数据。
func TestFileSinkCommitTruncatesTail(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "out.bin")
	s, err := Open(finalPath, TempPathFor(finalPath), Options{TotalSize: 4})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if _, err := s.WriteAt([]byte("abcdefgh"), 0); err != nil {
		t.Fatalf("WriteAt 失败: %v", err)
	}
	if err := s.Commit(0); err != nil {
		t.Fatalf("Commit 失败: %v", err)
	}
	fi, err := os.Stat(finalPath)
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if fi.Size() != 4 {
		t.Fatalf("文件长度 = %d, 期望 4", fi.Size())
	}
}

// 重新打开同一个临时文件必须保留已有数据：这是断点续传的基础。
func TestFileSinkReopenKeepsExistingData(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "out.bin")
	tempPath := TempPathFor(finalPath)

	s1, err := Open(finalPath, tempPath, Options{TotalSize: 8})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if _, err := s1.WriteAt([]byte("abcd"), 0); err != nil {
		t.Fatalf("WriteAt 失败: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	s2, err := Open(finalPath, tempPath, Options{TotalSize: 8})
	if err != nil {
		t.Fatalf("重新 Open 失败: %v", err)
	}
	defer s2.Close()
	if got, err := s2.Size(); err != nil || got != 4 {
		t.Fatalf("复用后文件长度 = %d (err=%v), 期望 4", got, err)
	}
	if _, err := s2.WriteAt([]byte("efgh"), 4); err != nil {
		t.Fatalf("续写失败: %v", err)
	}
	if err := s2.Commit(8); err != nil {
		t.Fatalf("Commit 失败: %v", err)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if string(got) != "abcdefgh" {
		t.Fatalf("内容 = %q, 期望 abcdefgh", got)
	}
}

func TestFileSinkDiscardRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "out.bin")
	tempPath := TempPathFor(finalPath)
	s, err := Open(finalPath, tempPath, Options{TotalSize: 8})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if _, err := s.WriteAt([]byte("abcd"), 0); err != nil {
		t.Fatalf("WriteAt 失败: %v", err)
	}
	if err := s.Discard(); err != nil {
		t.Fatalf("Discard 失败: %v", err)
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Error("Discard 后临时文件应被删除")
	}
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Error("Discard 不应产生最终文件")
	}
}

func TestFileSinkRejectsInvalidPaths(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.bin")
	if _, err := Open("", p, Options{}); err == nil {
		t.Error("空最终路径应被拒绝")
	}
	if _, err := Open(p, "", Options{}); err == nil {
		t.Error("空临时路径应被拒绝")
	}
	// 相同路径是合法的「就地写入」模式：用于对已存在的完整文件做定点修补，
	// 避免用临时文件重命名时覆盖掉未被重下的字节。
	s, err := Open(p, p, Options{TotalSize: 4})
	if err != nil {
		t.Fatalf("同一路径应支持就地写入: %v", err)
	}
	if !s.InPlace() {
		t.Error("应处于就地写入模式")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
}

func TestFileSinkInPlaceCommitKeepsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inplace.bin")
	if err := os.WriteFile(path, []byte("abcdefgh"), 0o644); err != nil {
		t.Fatalf("预置文件失败: %v", err)
	}

	s, err := Open(path, path, Options{TotalSize: 8})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	// 只修补中间两个字节。
	if _, err := s.WriteAt([]byte("XY"), 3); err != nil {
		t.Fatalf("WriteAt 失败: %v", err)
	}
	if err := s.Commit(8); err != nil {
		t.Fatalf("Commit 失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	// 关键：未被修补的字节必须原样保留（不会被零覆盖）。
	if string(got) != "abcXYfgh" {
		t.Fatalf("就地提交后被修改为 %q, 期望 abcXYfgh", got)
	}

	// Discard 在就地模式下不得删除数据。
	s2, err := Open(path, path, Options{TotalSize: 8})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if err := s2.Discard(); err != nil {
		t.Fatalf("Discard 失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("就地模式下 Discard 不应删除文件: %v", err)
	}
}

func TestFileSinkWriteAfterCloseFails(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "out.bin")
	s, err := Open(finalPath, TempPathFor(finalPath), Options{TotalSize: 4})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if _, err := s.WriteAt([]byte("x"), 0); err == nil {
		t.Error("关闭后写入应返回错误")
	}
	// Close 可重复调用且幂等。
	if err := s.Close(); err != nil {
		t.Errorf("重复 Close 应幂等, 实际 %v", err)
	}
}

func TestFileSinkPeriodicSync(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "out.bin")
	s, err := Open(finalPath, TempPathFor(finalPath), Options{TotalSize: 64, SyncInterval: 16})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer s.Close()
	if _, err := s.WriteAt(bytes.Repeat([]byte("a"), 32), 0); err != nil {
		t.Fatalf("WriteAt 失败: %v", err)
	}
	// 超过同步间隔后 dirty 应被重置。
	s.mu.Lock()
	dirty := s.dirty
	s.mu.Unlock()
	if dirty >= 16 {
		t.Fatalf("周期落盘后 dirty = %d, 期望被重置", dirty)
	}
	if err := s.Sync(); err != nil {
		t.Fatalf("Sync 失败: %v", err)
	}
}

func TestProgressSaverSyncsBeforeSave(t *testing.T) {
	log := &eventLog{}
	sampler := &ProgressSaver{Store: &loggingStore{log: log}, Sink: &loggingSink{log: log}}
	if err := sampler.Save(nil); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	got := log.snapshot()
	want := []string{"sync", "save"}
	if len(got) != len(want) {
		t.Fatalf("事件序列 = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("事件序列 = %v, 期望 %v（必须先落盘数据再写元数据）", got, want)
		}
	}

	if err := (&ProgressSaver{}).Save(nil); err == nil {
		t.Error("未配置 Store 时应返回错误")
	}
	// Sink 为 nil 时仍允许保存（用于无数据文件的任务，如 BT）。
	if err := (&ProgressSaver{Store: &loggingStore{log: log}}).Save(nil); err != nil {
		t.Errorf("Sink 为 nil 时应允许保存: %v", err)
	}
}

type eventLog struct {
	mu   sync.Mutex
	evs  []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evs = append(l.evs, e)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.evs))
	copy(out, l.evs)
	return out
}

type loggingSink struct{ log *eventLog }

func (s *loggingSink) Sync() error { s.log.add("sync"); return nil }

type loggingStore struct{ log *eventLog }

func (s *loggingStore) Save(_ *engine.Task) error { s.log.add("save"); return nil }
