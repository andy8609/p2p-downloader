package engine

import (
	"encoding/json"
	"testing"
)

const (
	testChunkSize = int64(1 << 20) // 1MB
	testTotalSize = int64(10 << 20)
)

func newTestTask(t *testing.T, kind SourceKind, segments int) *Task {
	t.Helper()
	task, err := NewTask("t-1", kind, "https://example.com/big.iso",
		`D:\downloads\big.iso`, `D:\downloads\.p2pdl\t-1.part`,
		testTotalSize, testChunkSize, segments)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	return task
}

func TestNewTaskValidation(t *testing.T) {
	if _, err := NewTask("", SourceHTTP, "u", "d", "tmp", 100, 10, 2); err == nil {
		t.Error("空 ID 应被拒绝")
	}
	if _, err := NewTask("a", SourceKind("ftp"), "u", "d", "tmp", 100, 10, 2); err == nil {
		t.Error("未知协议应被拒绝")
	}
	if _, err := NewTask("a", SourceHTTP, "", "d", "tmp", 100, 10, 2); err == nil {
		t.Error("空下载源应被拒绝")
	}
	if _, err := NewTask("a", SourceHTTP, "u", "d", "tmp", 100, 0, 2); err == nil {
		t.Error("非正分块大小应被拒绝")
	}
}

func TestSplitCoversWholeFileWithoutOverlap(t *testing.T) {
	// 10MB / 3 段：无法整除，必须靠余数分配保证无缝无重叠。
	task := newTestTask(t, SourceHTTP, 3)

	if len(task.Segments) != 3 {
		t.Fatalf("分片数 = %d, 期望 3", len(task.Segments))
	}
	var total int64
	for i, s := range task.Segments {
		if s.Index != i {
			t.Errorf("分片 %d 的 Index = %d", i, s.Index)
		}
		total += s.Length()
	}
	if total != testTotalSize {
		t.Fatalf("分片总长度 = %d, 期望 %d", total, testTotalSize)
	}
	if err := task.Validate(); err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}

	// 边界：段数超过总字节数时应被缩小而不是产生空分片。
	tiny, err := NewTask("t-2", SourceHTTP, "u", "d", "tmp", 3, 1, 100)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	if len(tiny.Segments) != 3 {
		t.Fatalf("段数 = %d, 期望被裁剪为 3", len(tiny.Segments))
	}
}

func TestValidateDetectsInconsistentSegments(t *testing.T) {
	task := newTestTask(t, SourceHTTP, 4)

	// 位图位数与分片长度不匹配 → 必须被识别为损坏。
	task.Segments[1].Bitmap = NewBitmap(2)
	if err := task.Validate(); err == nil {
		t.Error("位图位数不符时应报错")
	}
	task = newTestTask(t, SourceHTTP, 4)

	// 分片区间出现空洞。
	task.Segments[2].Start += 10
	if err := task.Validate(); err == nil {
		t.Error("分片不连续时应报错")
	}
	task = newTestTask(t, SourceHTTP, 4)

	// 状态取值非法。
	task.Segments[0].State = SegmentState("weird")
	if err := task.Validate(); err == nil {
		t.Error("非法分片状态应报错")
	}
	task = newTestTask(t, SourceHTTP, 4)

	// 未知协议。
	task.Kind = SourceKind("ftp")
	if err := task.Validate(); err == nil {
		t.Error("非法协议应报错")
	}
	task = newTestTask(t, SourceHTTP, 4)

	// BT 任务缺少 infoHash。
	task.Kind = SourceBitTorrent
	if err := task.Validate(); err == nil {
		t.Error("BT 任务缺少 infoHash 应报错")
	}
}

func TestSegmentChunkMapping(t *testing.T) {
	// 总长 10MB、4 段，每段长度 2.5MB 不整除 1MB 分块，
	// 用于验证最后一块被裁剪到分片边界（不会越界写到下一段）。
	task := newTestTask(t, SourceHTTP, 4)
	seg := task.Segments[0]

	if want := uint64(3); seg.ChunkCount(task.ChunkSize) != want {
		t.Fatalf("ChunkCount = %d, 期望 %d", seg.ChunkCount(task.ChunkSize), want)
	}
	start, end := seg.ChunkRange(task.ChunkSize, 2)
	if start != seg.Start+2*testChunkSize || end != seg.End {
		t.Fatalf("末块区间 = [%d,%d), 期望 [%d,%d)", start, end, seg.Start+2*testChunkSize, seg.End)
	}

	if got := seg.DownloadedBytes(task.ChunkSize); got != 0 {
		t.Fatalf("初始 DownloadedBytes = %d, 期望 0", got)
	}
	// 置位全部 3 个分块中的 0 和 2：应为 1MB(整块) + 0.5MB(被分片边界裁剪的末块)。
	_ = seg.Bitmap.Set(0)
	_ = seg.Bitmap.Set(2)
	wantDownloaded := testChunkSize + (seg.Length() - 2*testChunkSize)
	if got := seg.DownloadedBytes(task.ChunkSize); got != wantDownloaded {
		t.Fatalf("DownloadedBytes = %d, 期望 %d", got, wantDownloaded)
	}
	if seg.Complete() {
		t.Fatal("尚有未完成分块时不应判定分片完成")
	}
	_ = seg.Bitmap.Set(1)
	if !seg.Complete() {
		t.Fatal("全部分块完成时应判定分片完成")
	}
}

func TestTaskProgressAndComplete(t *testing.T) {
	task := newTestTask(t, SourceHTTP, 2)
	if task.Progress() != 0 {
		t.Fatalf("初始进度 = %v, 期望 0", task.Progress())
	}
	if task.Complete() {
		t.Fatal("初始不应判定完成")
	}

	for _, s := range task.Segments {
		s.Bitmap.SetRange(0, s.Bitmap.Len())
		s.State = SegmentCompleted
	}
	if !task.Complete() {
		t.Fatal("所有分片完成后应判定任务完成")
	}
	if got := task.Downloaded(); got != testTotalSize {
		t.Fatalf("Downloaded = %d, 期望 %d", got, testTotalSize)
	}
	if got := task.Progress(); got != 1 {
		t.Fatalf("Progress = %v, 期望 1", got)
	}
	if n := len(task.PendingSegments()); n != 0 {
		t.Fatalf("PendingSegments 数量 = %d, 期望 0", n)
	}

	// 大小未知时进度恒为 0，且不会被误判为完成。
	unknown, err := NewTask("t-3", SourceHTTP, "u", "d", "tmp", SizeUnknown, testChunkSize, 4)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	if unknown.Progress() != 0 || unknown.Complete() {
		t.Fatalf("大小未知的任务进度应恒为 0 且未完成")
	}
	if err := unknown.Validate(); err != nil {
		t.Fatalf("大小未知的任务应合法: %v", err)
	}
}

// 强杀进程后重启时，元数据里会残留「运行中」的分片；
// RequeueInterrupted 负责把这些分片拉回待下载状态，并让任务回到待调度。
func TestRequeueInterruptedRestoresConsistentState(t *testing.T) {
	task := newTestTask(t, SourceHTTP, 3)
	task.State = StateRunning
	task.Segments[0].State = SegmentRunning
	task.Segments[1].State = SegmentRunning
	task.Segments[2].State = SegmentCompleted

	if n := task.RequeueInterrupted(); n != 2 {
		t.Fatalf("回退分片数 = %d, 期望 2", n)
	}
	// 强杀不是用户主动暂停：任务应回到待调度，重启后自动继续。
	if task.State != StatePending {
		t.Fatalf("任务状态 = %q, 期望 %q", task.State, StatePending)
	}
	for _, s := range task.Segments[:2] {
		if s.State != SegmentPending {
			t.Errorf("分片 %d 状态 = %q, 期望 %q", s.Index, s.State, SegmentPending)
		}
	}
	if task.Segments[2].State != SegmentCompleted {
		t.Error("已完成分片不应被回退")
	}
	if err := task.Validate(); err != nil {
		t.Fatalf("回退后任务应仍然自洽: %v", err)
	}
}

func TestVerifySpec(t *testing.T) {
	if (VerifySpec{}).Enabled() {
		t.Error("空配置不应启用校验")
	}
	if (VerifySpec{Algorithm: "none"}).Enabled() {
		t.Error("none 不应启用校验")
	}
	if !(VerifySpec{Algorithm: "SHA256", Value: "AB"}).Enabled() {
		t.Error("sha256 应启用校验")
	}
	if got := (VerifySpec{Algorithm: " SHA256 ", Value: " ABCD "}).Normalize(); got.Algorithm != "sha256" || got.Value != "abcd" {
		t.Fatalf("Normalize() = %+v", got)
	}
	if err := (VerifySpec{Algorithm: "sha256"}).Validate(); err != nil {
		t.Errorf("缺省摘要值应允许（运行期补齐）: %v", err)
	}
	if err := (VerifySpec{Algorithm: "sha256", Value: "abcd"}).Validate(); err == nil {
		t.Error("长度错误的 sha256 摘要应被拒绝")
	}
	if err := (VerifySpec{Algorithm: "crc32"}).Validate(); err == nil {
		t.Error("不支持的算法应被拒绝")
	}
}

// Task 是断点续传的唯一真相来源，必须能无损 JSON 往返。
func TestTaskJSONRoundTrip(t *testing.T) {
	task := newTestTask(t, SourceHTTP, 3)
	task.State = StatePaused
	task.Segments[0].Bitmap.SetRange(0, 4)
	task.Segments[0].Attempts = 2
	task.Segments[1].LastErr = "connection reset"
	task.Verify = VerifySpec{Algorithm: "sha256", Value: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}

	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	var back Task
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("往返后任务应仍然自洽: %v", err)
	}
	if back.Downloaded() != task.Downloaded() {
		t.Fatalf("往返后 Downloaded = %d, 期望 %d", back.Downloaded(), task.Downloaded())
	}
	if back.State != task.State || back.Segments[0].Attempts != 2 || back.Segments[1].LastErr != "connection reset" {
		t.Fatalf("往返后关键字段丢失: %+v", back.Segments[:2])
	}
}
