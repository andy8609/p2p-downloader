package sink

import (
	"os"
	"path/filepath"
	"testing"

	"p2pdl/internal/engine"
)

const (
	rsChunk = int64(1 << 10) // 1KB
	rsTotal = int64(16 << 10)
)

func recoveryTask(t testing.TB, segments int) *engine.Task {
	t.Helper()
	task, err := engine.NewTask("r-1", engine.SourceHTTP, "http://example.com/a.bin",
		"a.bin", "a.part", rsTotal, rsChunk, segments)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	return task
}

// markChunks 把某分片的前 n 个分块标记为已完成，并扩展到文件：返回位图声称的最高偏移。
func markChunks(task *engine.Task, segIndex int, n uint64) int64 {
	seg := task.Segments[segIndex]
	count := seg.Bitmap.Len()
	if n > count {
		n = count
	}
	seg.Bitmap.SetRange(0, n)
	if n == 0 {
		return 0
	}
	_, end := seg.ChunkRange(task.ChunkSize, n-1)
	return end
}

func TestPlanRecoveryFreshTask(t *testing.T) {
	task := recoveryTask(t, 4)
	tempPath := filepath.Join(t.TempDir(), "a.part")

	plan, err := PlanRecovery(task, tempPath)
	if err != nil {
		t.Fatalf("PlanRecovery 失败: %v", err)
	}
	if plan.Decision != DecisionResume {
		t.Fatalf("全新任务应可续传（等价于从头下载），实际 %s: %s", plan.Decision, plan.Reason)
	}
	if plan.TempExists {
		t.Error("临时文件不应存在")
	}
	if plan.Downloaded != 0 || plan.MissingBytes != rsTotal {
		t.Fatalf("Downloaded=%d MissingBytes=%d, 期望 0 与 %d", plan.Downloaded, plan.MissingBytes, rsTotal)
	}
	if len(plan.Missing) != 1 || plan.Missing[0].Start != 0 || plan.Missing[0].End != rsTotal {
		t.Fatalf("缺口 = %+v, 期望整文件一个区间", plan.Missing)
	}
}

func TestPlanRecoveryResumesFromGap(t *testing.T) {
	task := recoveryTask(t, 4)
	dir := t.TempDir()
	tempPath := filepath.Join(dir, "a.part")

	// 分片 0 完整、分片 1 前两块完成。
	highest := markChunks(task, 0, task.Segments[0].Bitmap.Len())
	if h := markChunks(task, 1, 2); h > highest {
		highest = h
	}
	// 临时文件长度覆盖到位图声称的最高偏移，但尚未写入全部数据。
	writeSizedFile(t, tempPath, highest)

	plan, err := PlanRecovery(task, tempPath)
	if err != nil {
		t.Fatalf("PlanRecovery 失败: %v", err)
	}
	if plan.Decision != DecisionResume {
		t.Fatalf("决策 = %s, 期望 resume（%s）", plan.Decision, plan.Reason)
	}
	if plan.Downloaded >= rsTotal {
		t.Fatalf("Downloaded = %d, 应小于总长 %d", plan.Downloaded, rsTotal)
	}
	if plan.MissingBytes != rsTotal-plan.Downloaded {
		t.Fatalf("MissingBytes = %d, 期望 %d", plan.MissingBytes, rsTotal-plan.Downloaded)
	}
	if len(plan.Missing) != 1 {
		t.Fatalf("缺口 = %+v, 期望合并为 1 个区间", plan.Missing)
	}
	// 缺口必须从「分片 1 的第三块」开始，且不包含已完成的第 0 分片。
	seg1 := task.Segments[1]
	wantStart := seg1.Start + 2*rsChunk
	if got := plan.Missing[0].Start; got != wantStart {
		t.Fatalf("缺口起点 = %d, 期望 %d", got, wantStart)
	}
}

// 元数据不自洽时必须重下，而不是在错误的偏移上继续写。
func TestPlanRecoveryRestartsOnInconsistentMetadata(t *testing.T) {
	task := recoveryTask(t, 2)
	task.Segments[1].Start += 16 // 制造空洞
	plan, err := PlanRecovery(task, filepath.Join(t.TempDir(), "a.part"))
	if err != nil {
		t.Fatalf("PlanRecovery 失败: %v", err)
	}
	if plan.Decision != DecisionRestart {
		t.Fatalf("决策 = %s, 期望 restart（%s）", plan.Decision, plan.Reason)
	}
}

func TestPlanRecoveryRestartsWhenSizeUnknown(t *testing.T) {
	task, err := engine.NewTask("u-1", engine.SourceHTTP, "http://example.com/a.bin",
		"a.bin", "a.part", engine.SizeUnknown, rsChunk, 4)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	plan, err := PlanRecovery(task, filepath.Join(t.TempDir(), "a.part"))
	if err != nil {
		t.Fatalf("PlanRecovery 失败: %v", err)
	}
	if plan.Decision != DecisionRestart {
		t.Fatalf("长度未知时必须重下, 实际 %s: %s", plan.Decision, plan.Reason)
	}
}

// 位图声称有进度但临时文件缺失/被截断 → 必须重下（否则会写出空洞文件）。
func TestPlanRecoveryRestartsWhenTempFileMissingOrTruncated(t *testing.T) {
	t.Run("文件缺失", func(t *testing.T) {
		task := recoveryTask(t, 2)
		markChunks(task, 0, 2)
		plan, err := PlanRecovery(task, filepath.Join(t.TempDir(), "missing.part"))
		if err != nil {
			t.Fatalf("PlanRecovery 失败: %v", err)
		}
		if plan.Decision != DecisionRestart {
			t.Fatalf("决策 = %s, 期望 restart（%s）", plan.Decision, plan.Reason)
		}
	})

	t.Run("文件被截断", func(t *testing.T) {
		task := recoveryTask(t, 2)
		highest := markChunks(task, 0, 4)
		tempPath := filepath.Join(t.TempDir(), "a.part")
		writeSizedFile(t, tempPath, highest-100) // 比位图声称的短
		plan, err := PlanRecovery(task, tempPath)
		if err != nil {
			t.Fatalf("PlanRecovery 失败: %v", err)
		}
		if plan.Decision != DecisionRestart {
			t.Fatalf("决策 = %s, 期望 restart（%s）", plan.Decision, plan.Reason)
		}
	})

	t.Run("文件比预期长不影响续传", func(t *testing.T) {
		task := recoveryTask(t, 2)
		highest := markChunks(task, 0, 4)
		tempPath := filepath.Join(t.TempDir(), "a.part")
		writeSizedFile(t, tempPath, highest+4096)
		plan, err := PlanRecovery(task, tempPath)
		if err != nil {
			t.Fatalf("PlanRecovery 失败: %v", err)
		}
		if plan.Decision != DecisionResume {
			t.Fatalf("决策 = %s, 期望 resume（%s）", plan.Decision, plan.Reason)
		}
	})
}

func TestPlanRecoveryCompleteTask(t *testing.T) {
	task := recoveryTask(t, 2)
	for _, seg := range task.Segments {
		seg.Bitmap.SetRange(0, seg.Bitmap.Len())
	}
	tempPath := filepath.Join(t.TempDir(), "a.part")
	writeSizedFile(t, tempPath, rsTotal)

	plan, err := PlanRecovery(task, tempPath)
	if err != nil {
		t.Fatalf("PlanRecovery 失败: %v", err)
	}
	if plan.Decision != DecisionResume {
		t.Fatalf("决策 = %s, 期望 resume", plan.Decision)
	}
	if len(plan.Missing) != 0 || plan.MissingBytes != 0 {
		t.Fatalf("已完成任务不应有缺口: %+v", plan.Missing)
	}
}

// 验收核心：反复恢复不得产生区间重叠或空洞。
func TestMissingRangesAreMergedAndDisjoint(t *testing.T) {
	task := recoveryTask(t, 4)
	// 在多个分片里制造交错碎片。
	seg0 := task.Segments[0]
	seg1 := task.Segments[1]
	seg0.Bitmap.Set(0)
	seg0.Bitmap.Set(1)
	seg0.Bitmap.Set(3)
	seg1.Bitmap.Set(0)
	seg1.Bitmap.Set(2)

	ranges := MissingRanges(task)
	if len(ranges) == 0 {
		t.Fatal("应有缺口")
	}
	for i := 1; i < len(ranges); i++ {
		if ranges[i].Start <= ranges[i-1].End {
			t.Fatalf("区间 %d 与前一个区间重叠或未合并: %+v", i, ranges)
		}
	}
	// 缺口 + 已完成 == 总长，说明既无重叠也无空洞。
	var missing int64
	for _, r := range ranges {
		missing += r.Length()
	}
	if missing+task.Downloaded() != rsTotal {
		t.Fatalf("缺口 %d + 已完成 %d != 总长 %d", missing, task.Downloaded(), rsTotal)
	}
}

func TestMergeRangesHandlesOverlapAndTouch(t *testing.T) {
	in := []ByteRange{
		{Start: 10, End: 20},
		{Start: 0, End: 10},   // 与上一个相接
		{Start: 15, End: 25},  // 与第一个重叠
		{Start: 40, End: 50},
		{Start: 50, End: 60},  // 相接
		{Start: 30, End: 30},  // 空区间应被丢弃
	}
	got := MergeRanges(in)
	want := []ByteRange{{Start: 0, End: 25}, {Start: 40, End: 60}}
	if len(got) != len(want) {
		t.Fatalf("MergeRanges = %+v, 期望 %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("MergeRanges = %+v, 期望 %+v", got, want)
		}
	}
	if MergeRanges(nil) != nil {
		t.Error("空输入应返回 nil")
	}
}

func TestPlanRecoveryRejectsNilTask(t *testing.T) {
	if _, err := PlanRecovery(nil, "x.part"); err == nil {
		t.Error("nil 任务应被拒绝")
	}
}

func writeSizedFile(t testing.TB, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建文件失败: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatalf("调整大小失败: %v", err)
	}
}
