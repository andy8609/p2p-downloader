package verify

import (
	"bytes"
	"context"
	"errors"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"p2pdl/internal/engine"
)

const testChunk = int64(64 << 10)

func makeFile(t *testing.T, dir, name string, size int) (string, []byte) {
	t.Helper()
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
	return path, content
}

// 人为破坏一段字节，用于模拟磁盘损坏或写入错误。
func corruptAt(t *testing.T, path string, offset int64, length int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("打开文件失败: %v", err)
	}
	defer f.Close()
	bad := bytes.Repeat([]byte{0xFF}, int(length))
	if _, err := f.WriteAt(bad, offset); err != nil {
		t.Fatalf("破坏文件失败: %v", err)
	}
}

func TestHashFileMatchesStandardLibrary(t *testing.T) {
	dir := t.TempDir()
	path, content := makeFile(t, dir, "a.bin", 300<<10)

	got, err := HashFile(context.Background(), path, AlgorithmSHA256, nil)
	if err != nil {
		t.Fatalf("HashFile 失败: %v", err)
	}
	want := sha256.Sum256(content)
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("SHA-256 不匹配")
	}

	gotMD5, err := HashFile(context.Background(), path, AlgorithmMD5, nil)
	if err != nil {
		t.Fatalf("HashFile(md5) 失败: %v", err)
	}
	if len(gotMD5) != 32 {
		t.Fatalf("MD5 长度 = %d, 期望 32", len(gotMD5))
	}
}

func TestHashFileReportsProgress(t *testing.T) {
	dir := t.TempDir()
	path, content := makeFile(t, dir, "b.bin", 5<<20)
	var total int64
	if _, err := HashFile(context.Background(), path, AlgorithmSHA256, func(n int64) { total += n }); err != nil {
		t.Fatalf("HashFile 失败: %v", err)
	}
	if total != int64(len(content)) {
		t.Fatalf("进度累计 = %d, 期望 %d", total, len(content))
	}
}

func TestHashFileHonoursContext(t *testing.T) {
	dir := t.TempDir()
	path, _ := makeFile(t, dir, "c.bin", 8<<20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := HashFile(ctx, path, AlgorithmSHA256, nil); err == nil {
		t.Fatal("已取消的 context 应导致失败")
	}
}

func TestHashFileMissingFile(t *testing.T) {
	if _, err := HashFile(context.Background(), filepath.Join(t.TempDir(), "nope"), AlgorithmSHA256, nil); err == nil {
		t.Fatal("文件不存在应返回错误")
	}
}

func TestParseAlgorithm(t *testing.T) {
	for _, name := range []string{"", "sha256", "SHA256", " sha256 "} {
		algo, err := ParseAlgorithm(name)
		if err != nil || algo != AlgorithmSHA256 {
			t.Errorf("ParseAlgorithm(%q) = (%v, %v)", name, algo, err)
		}
	}
	if algo, err := ParseAlgorithm("md5"); err != nil || algo != AlgorithmMD5 {
		t.Errorf("md5 解析失败: %v", err)
	}
	if _, err := ParseAlgorithm("crc32"); err == nil {
		t.Error("不支持的算法应报错")
	}
	if AlgorithmSHA256.DigestLength() != 64 || AlgorithmMD5.DigestLength() != 32 {
		t.Error("摘要长度不正确")
	}
}

func TestBuildManifestAndVerifyClean(t *testing.T) {
	dir := t.TempDir()
	path, _ := makeFile(t, dir, "big.bin", 300<<10) // 300KB / 64KB = 5 块（最后一块不满）

	manifest, err := BuildManifest(context.Background(), path, testChunk, AlgorithmSHA256, nil)
	if err != nil {
		t.Fatalf("BuildManifest 失败: %v", err)
	}
	if manifest.ChunkCount() != 5 {
		t.Fatalf("块数 = %d, 期望 5", manifest.ChunkCount())
	}
	if manifest.TotalSize != 300<<10 {
		t.Fatalf("TotalSize = %d", manifest.TotalSize)
	}

	report, err := manifest.VerifyFile(context.Background(), path)
	if err != nil {
		t.Fatalf("VerifyFile 失败: %v", err)
	}
	if !report.OK {
		t.Fatalf("完整文件应通过校验: %+v", report)
	}
	if report.VerifiedBytes != 300<<10 {
		t.Fatalf("已校验字节 = %d", report.VerifiedBytes)
	}
}

// 验收核心：人为破坏区间后能定位到具体块。
func TestManifestLocatesCorruptedChunks(t *testing.T) {
	dir := t.TempDir()
	path, _ := makeFile(t, dir, "corrupt.bin", 300<<10)

	manifest, err := BuildManifest(context.Background(), path, testChunk, AlgorithmSHA256, nil)
	if err != nil {
		t.Fatalf("BuildManifest 失败: %v", err)
	}

	// 破坏第 1 块（偏移 64KB~128KB）与第 3 块的前 100 字节。
	corruptAt(t, path, testChunk, 1024)
	corruptAt(t, path, 3*testChunk, 100)

	report, err := manifest.VerifyFile(context.Background(), path)
	if err != nil {
		t.Fatalf("VerifyFile 失败: %v", err)
	}
	if report.OK {
		t.Fatal("被破坏的文件不应通过校验")
	}
	if len(report.BadChunks) != 2 {
		t.Fatalf("损坏块数 = %d, 期望 2（%+v）", len(report.BadChunks), report.BadChunks)
	}
	if report.BadChunks[0] != 1 || report.BadChunks[1] != 3 {
		t.Fatalf("损坏块下标 = %v, 期望 [1 3]", report.BadChunks)
	}
	if report.BadBytes != 2*testChunk {
		t.Fatalf("损坏字节 = %d, 期望 %d", report.BadBytes, 2*testChunk)
	}
	// 区间必须精确指向被破坏的块。
	if report.BadRanges[0].Start != testChunk || report.BadRanges[0].End != 2*testChunk {
		t.Fatalf("第一个损坏区间 = %+v", report.BadRanges[0])
	}
	if report.BadRanges[1].Start != 3*testChunk || report.BadRanges[1].End != 4*testChunk {
		t.Fatalf("第二个损坏区间 = %+v", report.BadRanges[1])
	}
}

// 相邻损坏块应合并为一个区间，避免重下指令碎片化。
func TestManifestMergesAdjacentBadChunks(t *testing.T) {
	dir := t.TempDir()
	path, _ := makeFile(t, dir, "adj.bin", 300<<10)
	manifest, err := BuildManifest(context.Background(), path, testChunk, AlgorithmSHA256, nil)
	if err != nil {
		t.Fatalf("BuildManifest 失败: %v", err)
	}

	corruptAt(t, path, testChunk, 10)
	corruptAt(t, path, 2*testChunk, 10)

	report, err := manifest.VerifyFile(context.Background(), path)
	if err != nil {
		t.Fatalf("VerifyFile 失败: %v", err)
	}
	if len(report.BadChunks) != 2 {
		t.Fatalf("损坏块数 = %d, 期望 2", len(report.BadChunks))
	}
	if len(report.BadRanges) != 1 {
		t.Fatalf("相邻损坏块应合并为 1 个区间, 实际 %+v", report.BadRanges)
	}
	if report.BadRanges[0].Start != testChunk || report.BadRanges[0].End != 3*testChunk {
		t.Fatalf("合并后区间 = %+v", report.BadRanges[0])
	}
}

func TestManifestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path, _ := makeFile(t, dir, "save.bin", 200<<10)
	manifest, err := BuildManifest(context.Background(), path, testChunk, AlgorithmSHA256, nil)
	if err != nil {
		t.Fatalf("BuildManifest 失败: %v", err)
	}

	manifestPath := DefaultManifestPath(path)
	if err := manifest.Save(manifestPath); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	loaded, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest 失败: %v", err)
	}
	if loaded.ChunkCount() != manifest.ChunkCount() || loaded.TotalSize != manifest.TotalSize {
		t.Fatalf("往返后清单不一致")
	}
	for i := range manifest.Chunks {
		if loaded.Chunks[i] != manifest.Chunks[i] {
			t.Fatalf("第 %d 块摘要不一致", i)
		}
	}

	if _, err := LoadManifest(filepath.Join(dir, "missing")); err == nil {
		t.Error("不存在的清单应报错")
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.manifest"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := LoadManifest(filepath.Join(dir, "bad.manifest")); err == nil {
		t.Error("非法清单应报错")
	}
	if err := (&Manifest{ChunkSize: 0}).Save(filepath.Join(dir, "x")); err != nil {
		// ChunkSize 为 0 仅影响 VerifyFile，保存本身允许。
		t.Logf("Save 返回: %v", err)
	}
}

func TestManifestRejectsSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	path, _ := makeFile(t, dir, "size.bin", 200<<10)
	manifest, err := BuildManifest(context.Background(), path, testChunk, AlgorithmSHA256, nil)
	if err != nil {
		t.Fatalf("BuildManifest 失败: %v", err)
	}

	// 文件被截断后校验必须报错，而不是给出错误的「全部通过」结论。
	if err := os.Truncate(path, 100<<10); err != nil {
		t.Fatalf("截断失败: %v", err)
	}
	if _, err := manifest.VerifyFile(context.Background(), path); !errors.Is(err, ErrManifestMismatch) {
		t.Fatalf("大小不符应返回 ErrManifestMismatch, 实际 %v", err)
	}
}

func TestBuildManifestRejectsInvalidChunkSize(t *testing.T) {
	dir := t.TempDir()
	path, _ := makeFile(t, dir, "inv.bin", 1024)
	if _, err := BuildManifest(context.Background(), path, 0, AlgorithmSHA256, nil); err == nil {
		t.Error("分块大小为 0 应被拒绝")
	}
	if _, err := BuildManifest(context.Background(), filepath.Join(dir, "nope"), testChunk, AlgorithmSHA256, nil); err == nil {
		t.Error("文件不存在应被拒绝")
	}
}

// ---------------------------------------------------------------------------
// Verifier
// ---------------------------------------------------------------------------

func newTestTaskWithFile(t *testing.T, path string, content []byte) *engine.Task {
	t.Helper()
	sum := sha256.Sum256(content)
	task, err := engine.NewTask("v-1", engine.SourceHTTP, "http://example.com/f",
		path, path+".part", int64(len(content)), testChunk, 2)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	task.Verify = engine.VerifySpec{Algorithm: "sha256", Value: hex.EncodeToString(sum[:])}
	if err := task.Validate(); err != nil {
		t.Fatalf("任务不自洽: %v", err)
	}
	return task
}

func TestVerifierPassesOnIntactFile(t *testing.T) {
	dir := t.TempDir()
	path, content := makeFile(t, dir, "ok.bin", 300<<10)
	task := newTestTaskWithFile(t, path, content)

	v := New(Options{})
	result, err := v.Verify(context.Background(), task)
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	if !result.OK {
		t.Fatalf("完整文件应通过: %+v", result)
	}
}

func TestVerifierSkipsWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	path, content := makeFile(t, dir, "skip.bin", 1<<10)
	task := newTestTaskWithFile(t, path, content)
	task.Verify = engine.VerifySpec{Algorithm: "none"}

	result, err := New(Options{}).Verify(context.Background(), task)
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	if !result.OK {
		t.Fatal("未启用校验应直接通过")
	}
}

// 验收核心：整体哈希不符且无清单时，必须如实失败而不是静默通过。
func TestVerifierFailsWithoutManifest(t *testing.T) {
	dir := t.TempDir()
	path, content := makeFile(t, dir, "nom.bin", 300<<10)
	task := newTestTaskWithFile(t, path, content)
	corruptAt(t, path, testChunk, 10)

	result, err := New(Options{}).Verify(context.Background(), task)
	if err != nil {
		t.Fatalf("Verify 不应返回系统错误: %v", err)
	}
	if result.OK {
		t.Fatal("损坏文件不应通过校验")
	}
	if result.Err == nil {
		t.Fatal("无法定位损坏区间时应返回错误")
	}
	if result.Repaired {
		t.Fatal("没有清单时不应声称已修复")
	}
}

// 验收核心：有清单时能定位损坏块并标记为「只重下这些块」。
func TestVerifierRepairsOnlyCorruptedChunks(t *testing.T) {
	dir := t.TempDir()
	path, content := makeFile(t, dir, "repair.bin", 512<<10) // 8 块
	task := newTestTaskWithFile(t, path, content)

	// 生成清单（模拟下载完成后生成的基线）。
	manifest, err := BuildManifest(context.Background(), path, task.ChunkSize, AlgorithmSHA256, nil)
	if err != nil {
		t.Fatalf("BuildManifest 失败: %v", err)
	}
	if err := manifest.Save(DefaultManifestPath(path)); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	// 全部标记为已完成，再破坏第 2 与第 5 块。
	for _, seg := range task.Segments {
		seg.Bitmap.SetRange(0, seg.Bitmap.Len())
	}
	if !task.Complete() {
		t.Fatal("前置条件：任务应处于已完成状态")
	}
	corruptAt(t, path, 2*task.ChunkSize, 32)
	corruptAt(t, path, 5*task.ChunkSize, 32)

	v := New(Options{})
	result, err := v.Verify(context.Background(), task)
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	if !result.Repaired {
		t.Fatalf("应定位并标记重下: %+v", result)
	}
	if len(result.Missing) == 0 {
		t.Fatal("应给出需重下的区间")
	}
	if result.MissingBytes == 0 || result.MissingBytes > task.TotalSize {
		t.Fatalf("需重下字节数异常: %d", result.MissingBytes)
	}
	if task.VerifyAttempts != 1 {
		t.Errorf("修复次数 = %d, 期望 1", task.VerifyAttempts)
	}

	// 关键：只有损坏块被标记为未完成，其余块必须保持已完成（定点重下）。
	if task.Complete() {
		t.Fatal("损坏块应被标记为未完成")
	}
	missingBytes := task.TotalSize - task.Downloaded()
	if missingBytes != result.MissingBytes {
		t.Fatalf("任务缺口 %d 与报告 %d 不一致", missingBytes, result.MissingBytes)
	}
	// 8 块里只有 2 块被清掉。
	totalChunksSeen := int64(0)
	for _, seg := range task.Segments {
		totalChunksSeen += int64(seg.Length())
	}
	if missingBytes >= task.TotalSize/2 {
		t.Fatalf("重下量 %d 过大，说明没有做到定点重下", missingBytes)
	}
}

// 验收核心：持续损坏时不会无限重下。
func TestVerifierStopsAfterMaxRepairs(t *testing.T) {
	dir := t.TempDir()
	path, content := makeFile(t, dir, "loop.bin", 256<<10)
	task := newTestTaskWithFile(t, path, content)

	manifest, err := BuildManifest(context.Background(), path, task.ChunkSize, AlgorithmSHA256, nil)
	if err != nil {
		t.Fatalf("BuildManifest 失败: %v", err)
	}
	if err := manifest.Save(DefaultManifestPath(path)); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	for _, seg := range task.Segments {
		seg.Bitmap.SetRange(0, seg.Bitmap.Len())
	}
	corruptAt(t, path, testChunk, 32)

	v := New(Options{MaxRepairAttempts: 2})
	for attempt := 1; attempt <= 2; attempt++ {
		result, err := v.Verify(context.Background(), task)
		if err != nil {
			t.Fatalf("第 %d 次 Verify 失败: %v", attempt, err)
		}
		if !result.Repaired {
			t.Fatalf("第 %d 次应标记修复: %+v", attempt, result)
		}
		// 模拟重新下载失败：数据仍然损坏，再次标记为完成。
		for _, seg := range task.Segments {
			seg.Bitmap.SetRange(0, seg.Bitmap.Len())
		}
	}

	result, err := v.Verify(context.Background(), task)
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	if result.Repaired {
		t.Fatal("达到修复上限后不应继续重下")
	}
	if result.OK || result.Err == nil {
		t.Fatalf("应如实失败并给出原因: %+v", result)
	}
}

// 摘要值缺失时只验证「能否完整读取」。
func TestVerifierWithoutExpectedHashOnlyChecksReadability(t *testing.T) {
	dir := t.TempDir()
	path, content := makeFile(t, dir, "read.bin", 128<<10)
	task := newTestTaskWithFile(t, path, content)
	task.Verify = engine.VerifySpec{Algorithm: "sha256"} // 无期望值

	result, err := New(Options{}).Verify(context.Background(), task)
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	if !result.OK {
		t.Fatalf("可完整读取即通过: %+v", result)
	}
}

func TestVerifierBuildsManifestOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path, content := makeFile(t, dir, "build.bin", 256<<10)
	task := newTestTaskWithFile(t, path, content)

	v := New(Options{BuildManifestOnSuccess: true})
	result, err := v.Verify(context.Background(), task)
	if err != nil || !result.OK {
		t.Fatalf("Verify 失败: %v %+v", err, result)
	}
	if _, err := os.Stat(DefaultManifestPath(path)); err != nil {
		t.Fatalf("应生成清单文件: %v", err)
	}
}

func TestVerifierHandlesBitTorrentRecheck(t *testing.T) {
	dir := t.TempDir()
	path, content := makeFile(t, dir, "bt.bin", 128<<10)
	task := newTestTaskWithFile(t, path, content)
	task.Kind = engine.SourceBitTorrent
	task.InfoHash = "0123456789abcdef0123456789abcdef01234567"
	task.Segments = nil

	called := 0
	v := New(Options{Recheck: func(context.Context, *engine.Task) error {
		called++
		return nil
	}})
	result, err := v.Verify(context.Background(), task)
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	if called != 1 {
		t.Fatalf("应触发一次 piece 重校验, 实际 %d", called)
	}
	if !result.Repaired {
		t.Fatalf("BT 重校验应要求重新调度: %+v", result)
	}
	if task.VerifyAttempts != 1 {
		t.Errorf("修复计数 = %d", task.VerifyAttempts)
	}

	// 无 Recheck 钩子时直接通过（piece 校验由协议层负责）。
	plain := New(Options{})
	result, err = plain.Verify(context.Background(), task)
	if err != nil || !result.OK {
		t.Fatalf("无钩子时应直接通过: %v %+v", err, result)
	}
}

func TestVerifierRejectsNilTask(t *testing.T) {
	if _, err := New(Options{}).Verify(context.Background(), nil); err == nil {
		t.Fatal("nil 任务应被拒绝")
	}
}

func TestVerifierUnknownAlgorithm(t *testing.T) {
	dir := t.TempDir()
	path, content := makeFile(t, dir, "algo.bin", 1024)
	task := newTestTaskWithFile(t, path, content)
	// 绕过 Validate 直接塞入未知算法，验证校验器不 panic 而是返回错误。
	task.Verify = engine.VerifySpec{Algorithm: "crc32", Value: "abcd"}
	if _, err := New(Options{}).Verify(context.Background(), task); err == nil {
		t.Fatal("未知算法应返回错误")
	}
}

func TestDefaultManifestPath(t *testing.T) {
	got := DefaultManifestPath(filepath.Join("D:", "dl", "file.iso"))
	if filepath.Base(got) != "file.iso.p2pdl.manifest" {
		t.Fatalf("清单路径 = %q", got)
	}
}
