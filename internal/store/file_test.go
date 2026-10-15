package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"p2pdl/internal/engine"
)

const (
	stChunkSize = int64(1 << 20)
	stTotalSize = int64(8 << 20)
)

func newTask(t *testing.T, id string, segments int) *engine.Task {
	t.Helper()
	task, err := engine.NewTask(id, engine.SourceHTTP, "https://example.com/big.iso",
		filepath.Join("D:", "downloads", "big.iso"),
		filepath.Join("D:", "downloads", ".p2pdl", id+".part"),
		stTotalSize, stChunkSize, segments)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	return task
}

// 验收核心：写入 → 关闭 → 重开 → 状态等价。
func TestSaveLoadRoundTripAcrossReopen(t *testing.T) {
	dir := t.TempDir()

	task := newTask(t, "task-a", 3)
	task.State = engine.StatePaused
	task.Segments[0].Bitmap.SetRange(0, 3)
	task.Segments[0].State = engine.SegmentRunning
	task.Segments[1].Bitmap.SetRange(0, 1)
	task.Segments[1].Attempts = 2
	task.Segments[1].LastErr = "unexpected EOF"
	task.Verify = engine.VerifySpec{Algorithm: "sha256", Value: strings64("a")}

	first, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore 失败: %v", err)
	}
	if err := first.Save(task); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	// 模拟进程重启：丢弃原实例，重新打开同一目录。
	reopened, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("重开 NewFileStore 失败: %v", err)
	}
	got, err := reopened.Load("task-a")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	if !reflect.DeepEqual(got, task) {
		t.Fatalf("往返后状态不等价:\n得到 %s\n期望 %s", dump(t, got), dump(t, task))
	}
	if got.Downloaded() != task.Downloaded() {
		t.Fatalf("往返后进度 = %d, 期望 %d", got.Downloaded(), task.Downloaded())
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("往返后任务应自洽: %v", err)
	}
}

func TestLoadMissingAndInvalidID(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore 失败: %v", err)
	}
	if _, err := s.Load("nope"); !IsNotFound(err) {
		t.Fatalf("不存在的任务应返回 ErrNotFound, 实际 %v", err)
	}
	for _, bad := range []string{"", "..", "../escape", "a/b", `a\b`, ".hidden", "a b", strings64("x") + "/../y"} {
		if _, err := s.Load(bad); !IsInvalidID(err) {
			t.Errorf("Load(%q) 应返回 ErrInvalidID, 实际 %v", bad, err)
		}
	}
	// Save 路径上的非法 ID：空 ID 会先被模型校验拦住，其余由存储层拒绝。
	for _, bad := range []string{"..", "../escape", "a/b", `a\b`, ".hidden", "a b"} {
		task := newTask(t, "valid", 2)
		task.ID = bad
		if err := s.Save(task); !IsInvalidID(err) {
			t.Errorf("Save(%q) 应返回 ErrInvalidID, 实际 %v", bad, err)
		}
	}
	if err := s.Save(&engine.Task{}); err == nil {
		t.Error("Save(空任务) 应返回错误")
	}
}

// 验收核心：损坏元数据必须被检测出来并以可识别的错误返回，供调用方降级重下。
func TestCorruptMetadataIsDetected(t *testing.T) {
	cases := map[string]string{
		"缺少文件头":   `{"id":"task-x"}`,
		"文件头版本不符": "P2PDL-STORE 99\n{}",
		"JSON 截断":  headerLine + "\n{\"id\":\"task-x\",",
		"结构不自洽":   headerLine + "\n" + `{"id":"task-x","kind":"http","source":"u","chunkSize":1024,"totalSize":1048576,"state":"pending","segments":[{"index":0,"start":0,"end":1048576,"state":"pending"}]}`,
		"位图长度不符":  headerLine + "\n" + `{"id":"task-x","kind":"http","source":"u","chunkSize":1048576,"totalSize":1048576,"state":"pending","segments":[{"index":0,"start":0,"end":1048576,"state":"pending","bitmap":{"n":1,"bits":"AAAAAAAA"}}]}`,
		"空文件":     "",
	}
	for name, content := range cases {
		dir := t.TempDir()
		s, err := NewFileStore(dir)
		if err != nil {
			t.Fatalf("NewFileStore 失败: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "task-x"+taskExt), []byte(content), 0o644); err != nil {
			t.Fatalf("写入损坏文件失败: %v", err)
		}
		if _, err := s.Load("task-x"); !IsCorrupt(err) {
			t.Errorf("%s: 期望 ErrCorrupt, 实际 %v", name, err)
		}
	}
}

// 单个任务损坏不应影响其他任务加载：这是「降级而非整体失败」的保证。
func TestLoadAllIsolatesCorruptTasks(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore 失败: %v", err)
	}
	good := newTask(t, "good-1", 2)
	if err := s.Save(good); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken-1"+taskExt), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("写入损坏文件失败: %v", err)
	}

	tasks, corrupt, err := s.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll 失败: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != "good-1" {
		t.Fatalf("健康任务加载结果 = %+v, 期望仅 good-1", tasks)
	}
	if len(corrupt) != 1 {
		t.Fatalf("损坏任务数 = %d, 期望 1", len(corrupt))
	}
	if _, ok := corrupt["broken-1"]; !ok {
		t.Fatalf("损坏任务列表缺少 broken-1: %v", corrupt)
	}

	// 隔离后可继续使用同一 ID 重新创建任务。
	dest, err := s.Quarantine("broken-1")
	if err != nil {
		t.Fatalf("Quarantine 失败: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("隔离文件不存在: %v", err)
	}
	if _, err := s.Load("broken-1"); !IsNotFound(err) {
		t.Fatalf("隔离后应返回 ErrNotFound, 实际 %v", err)
	}
	if err := s.Save(newTask(t, "broken-1", 2)); err != nil {
		t.Fatalf("隔离后重建任务失败: %v", err)
	}
}

func TestListAndDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore 失败: %v", err)
	}
	for _, id := range []string{"b", "a", "c"} {
		if err := s.Save(newTask(t, id, 2)); err != nil {
			t.Fatalf("Save(%s) 失败: %v", id, err)
		}
	}
	ids, err := s.List()
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"a", "b", "c"}) {
		t.Fatalf("List() = %v, 期望字典序 [a b c]", ids)
	}

	// 重复保存不应产生重复 ID。
	if err := s.Save(newTask(t, "a", 2)); err != nil {
		t.Fatalf("重复 Save 失败: %v", err)
	}
	if ids, _ = s.List(); len(ids) != 3 {
		t.Fatalf("重复保存后 List() = %v, 期望 3 项", ids)
	}

	if err := s.Delete("b"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	// 删除必须幂等：任务不存在时不报错。
	if err := s.Delete("b"); err != nil {
		t.Fatalf("重复 Delete 应幂等, 实际 %v", err)
	}
	if ids, _ = s.List(); !reflect.DeepEqual(ids, []string{"a", "c"}) {
		t.Fatalf("删除后 List() = %v, 期望 [a c]", ids)
	}
}

// 索引不是数据源：索引损坏时必须由目录扫描自愈，任务不能丢失。
func TestIndexSelfHealsWhenCorrupted(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore 失败: %v", err)
	}
	for _, id := range []string{"k1", "k2"} {
		if err := s.Save(newTask(t, id, 2)); err != nil {
			t.Fatalf("Save(%s) 失败: %v", id, err)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, indexName), []byte("{ 这不是 JSON"), 0o644); err != nil {
		t.Fatalf("破坏索引失败: %v", err)
	}

	ids, err := s.List()
	if err != nil {
		t.Fatalf("索引损坏时 List 应自愈, 实际 %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"k1", "k2"}) {
		t.Fatalf("自愈后 List() = %v, 期望 [k1 k2]", ids)
	}

	// 自愈应把索引写回为合法内容。
	raw, err := os.ReadFile(filepath.Join(dir, indexName))
	if err != nil {
		t.Fatalf("读取索引失败: %v", err)
	}
	var idx indexFile
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("自愈后的索引仍不可解析: %v", err)
	}
	if len(idx.TaskIDs) != 2 {
		t.Fatalf("自愈后的索引项数 = %d, 期望 2", len(idx.TaskIDs))
	}
}

func TestSaveRejectsInconsistentTask(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore 失败: %v", err)
	}
	task := newTask(t, "bad", 2)
	task.Segments[1].Start += 4096 // 制造空洞
	if err := s.Save(task); err == nil {
		t.Fatal("保存不自洽的任务应被拒绝")
	}
	if err := s.Save(nil); err == nil {
		t.Fatal("保存 nil 任务应被拒绝")
	}
}

func TestNewFileStoreRejectsEmptyDir(t *testing.T) {
	if _, err := NewFileStore(""); err == nil {
		t.Fatal("空目录应被拒绝")
	}
}

// strings64 生成 64 位长度的十六进制摘要，用于构造合法的 sha256 校验配置。
func strings64(seed string) string {
	out := ""
	for len(out) < 64 {
		out += seed
	}
	return out[:64]
}

func dump(t *testing.T, task *engine.Task) string {
	t.Helper()
	raw, err := json.MarshalIndent(task, "", "  ")
	if err != nil {
		return "<marshal error>"
	}
	return string(raw)
}
