package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"p2pdl/internal/engine"
	"p2pdl/internal/engine/verify"
)

// newVerifyingApp 创建一个开启「成功后生成分块清单」的应用，用于校验/修复流程测试。
func newVerifyingApp(t *testing.T, dir string) *App {
	t.Helper()
	a, err := New(Options{
		DataDir:                dir,
		MetaDir:                filepath.Join(dir, "meta"),
		ChunkSize:              64 << 10,
		Concurrency:            4,
		MaxConcurrentTasks:     1,
		SkipSpaceCheck:         true,
		BuildManifestOnSuccess: true,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return a
}

func corruptFile(t *testing.T, path string, offset int64, length int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("打开文件失败: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xEE}, length), offset); err != nil {
		t.Fatalf("破坏文件失败: %v", err)
	}
}

// 验收核心（T9）：人为破坏已下载区间后触发校验，
// 必须能定位到具体分块并只重下这些分块，最终文件恢复为与源完全一致。
func TestRecheckLocatesAndRepairsCorruptedChunks(t *testing.T) {
	const size = 1 << 20 // 1MB → 16 个 64KB 分块
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	sum := sha256.Sum256(content)
	srv := rangeServer(t, content)

	dir := tempDir(t)
	a := newVerifyingApp(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer a.Stop()

	task, err := a.AddHTTP(ctx, srv.URL+"/payload.bin", AddOptions{
		Name:         "repair.bin",
		ExpectedHash: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("AddHTTP 失败: %v", err)
	}

	waitFor(t, 60*time.Second, "首次下载完成", func() bool {
		v, _ := a.Scheduler().Get(task.ID)
		return v.State == engine.StateCompleted
	})

	dataPath := filepath.Join(dir, "repair.bin")
	manifestPath := verify.DefaultManifestPath(dataPath)
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("校验通过后应生成分块清单: %v", err)
	}
	if _, err := os.Stat(task.TempPath); !os.IsNotExist(err) {
		t.Fatalf("正常完成后临时文件应已被重命名, statErr=%v", err)
	}

	// 人为破坏第 3 与第 9 个分块（每块 64KB），其余字节保持完好。
	corruptFile(t, dataPath, 3*64<<10, 128)
	corruptFile(t, dataPath, 9*64<<10, 64)
	corrupted, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if sha256.Sum256(corrupted) == sum {
		t.Fatal("前置条件失败：文件未被破坏")
	}

	// 触发重新校验。
	if err := a.Recheck(ctx, task.ID); err != nil {
		t.Fatalf("Recheck 失败: %v", err)
	}

	// 修复完成后任务应重新回到 completed。
	waitFor(t, 60*time.Second, "修复后任务完成", func() bool {
		v, _ := a.Scheduler().Get(task.ID)
		if v.State == engine.StateFailed {
			t.Fatalf("修复流程失败: %s", v.LastError)
		}
		return v.State == engine.StateCompleted
	})

	got, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatalf("读取修复结果失败: %v", err)
	}
	if sha256.Sum256(got) != sum {
		t.Fatalf("修复后文件摘要仍不匹配：定点重下未能恢复数据")
	}
	if !bytes.Equal(got, content) {
		t.Fatal("修复后文件内容与源不一致")
	}

	// 修复过程中必须完成过一次「定位 → 重下」，且计数在成功后归零。
	final, err := a.Store().Load(task.ID)
	if err != nil {
		t.Fatalf("加载任务失败: %v", err)
	}
	if final.State != engine.StateCompleted {
		t.Fatalf("持久化状态 = %q, 期望 completed", final.State)
	}
	if final.VerifyAttempts != 0 {
		t.Errorf("成功完成后修复计数应归零, 实际 %d", final.VerifyAttempts)
	}
	if final.LastErr != "" {
		t.Errorf("成功完成后不应残留错误信息: %q", final.LastErr)
	}
}

// 只重下受损分块：重下量必须远小于文件总长。
func TestRecheckOnlyRedownloadsDamagedRanges(t *testing.T) {
	const size = 2 << 20 // 2MB → 32 块
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	sum := sha256.Sum256(content)

	// 记录服务端实际吐出的字节数，用于证明「只重下了少量数据」。
	var served int64
	srv := countingRangeServer(t, content, &served)

	dir := tempDir(t)
	a := newVerifyingApp(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer a.Stop()

	task, err := a.AddHTTP(ctx, srv.URL+"/payload.bin", AddOptions{
		Name:         "partial.bin",
		ExpectedHash: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("AddHTTP 失败: %v", err)
	}
	waitFor(t, 60*time.Second, "首次下载完成", func() bool {
		v, _ := a.Scheduler().Get(task.ID)
		return v.State == engine.StateCompleted
	})
	firstPass := served

	// 只破坏 1 块（64KB / 2MB = 3%）。
	dataPath := filepath.Join(dir, "partial.bin")
	corruptFile(t, dataPath, 5*64<<10, 32)

	if err := a.Recheck(ctx, task.ID); err != nil {
		t.Fatalf("Recheck 失败: %v", err)
	}
	waitFor(t, 60*time.Second, "修复完成", func() bool {
		v, _ := a.Scheduler().Get(task.ID)
		if v.State == engine.StateFailed {
			t.Fatalf("修复失败: %s", v.LastError)
		}
		return v.State == engine.StateCompleted
	})

	redownloaded := served - firstPass
	t.Logf("首次下载 %d 字节，修复阶段重下 %d 字节（总长 %d）", firstPass, redownloaded, size)

	if got, err := os.ReadFile(dataPath); err != nil || sha256.Sum256(got) != sum {
		t.Fatalf("修复后文件不正确: err=%v", err)
	}
	// 关键断言：重下量应接近一个分块，而不是整个文件。
	if redownloaded >= int64(size)/4 {
		t.Fatalf("重下量 %d 过大（总长 %d），未做到定点重下", redownloaded, size)
	}
}

// 整体摘要不符且没有清单时，必须如实失败，而不是静默标记完成。
func TestVerificationFailureWithoutManifestIsReported(t *testing.T) {
	const size = 256 << 10
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	sum := sha256.Sum256(content)
	srv := rangeServer(t, content)

	dir := tempDir(t)
	// 不生成清单：故意关掉 BuildManifestOnSuccess。
	a, err := New(Options{
		DataDir:            dir,
		MetaDir:            filepath.Join(dir, "meta"),
		ChunkSize:          64 << 10,
		MaxConcurrentTasks: 1,
		SkipSpaceCheck:     true,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer a.Stop()

	task, err := a.AddHTTP(ctx, srv.URL+"/payload.bin", AddOptions{
		Name:         "nomanifest.bin",
		ExpectedHash: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("AddHTTP 失败: %v", err)
	}
	waitFor(t, 60*time.Second, "下载完成", func() bool {
		v, _ := a.Scheduler().Get(task.ID)
		return v.State == engine.StateCompleted
	})

	dataPath := filepath.Join(dir, "nomanifest.bin")
	corruptFile(t, dataPath, 100, 16)

	if err := a.Recheck(ctx, task.ID); err != nil {
		t.Fatalf("Recheck 失败: %v", err)
	}

	waitFor(t, 60*time.Second, "任务以失败结束", func() bool {
		v, _ := a.Scheduler().Get(task.ID)
		return v.State == engine.StateFailed
	})
	view, _ := a.Scheduler().Get(task.ID)
	if view.LastError == "" {
		t.Fatal("失败任务必须给出可读原因，不能静默失败")
	}
	t.Logf("预期失败原因: %s", view.LastError)
}

// 摘要正确时重新校验应直接通过，且不产生额外下载。
func TestRecheckOnIntactFilePassesWithoutDownload(t *testing.T) {
	const size = 512 << 10
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	sum := sha256.Sum256(content)

	var served int64
	srv := countingRangeServer(t, content, &served)

	dir := tempDir(t)
	a := newVerifyingApp(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer a.Stop()

	task, err := a.AddHTTP(ctx, srv.URL+"/payload.bin", AddOptions{
		Name:         "intact.bin",
		ExpectedHash: hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("AddHTTP 失败: %v", err)
	}
	waitFor(t, 60*time.Second, "下载完成", func() bool {
		v, _ := a.Scheduler().Get(task.ID)
		return v.State == engine.StateCompleted
	})
	firstPass := served

	if err := a.Recheck(ctx, task.ID); err != nil {
		t.Fatalf("Recheck 失败: %v", err)
	}
	waitFor(t, 60*time.Second, "重新校验完成", func() bool {
		v, _ := a.Scheduler().Get(task.ID)
		return v.State == engine.StateCompleted
	})

	// 允许探测请求（HEAD 与 Range: bytes=0-0）带来的一字节级开销，
	// 但不能出现真正的数据重下。
	if extra := served - firstPass; extra > 4096 {
		t.Fatalf("完整文件重新校验不应产生下载，实际 %d 字节", extra)
	}
	// 数据必须保持完好（就地修补路径不得破坏文件）。
	got, err := os.ReadFile(filepath.Join(dir, "intact.bin"))
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if sha256.Sum256(got) != sum {
		t.Fatal("重新校验后文件被破坏")
	}
}

// 错误摘要应立即失败且不产生下载（校验发生在下载之前不会发生，这里验证最终结论）。
func TestWrongExpectedHashFailsTask(t *testing.T) {
	content := bytes.Repeat([]byte("payload"), 4096)
	srv := rangeServer(t, content)

	dir := tempDir(t)
	a, err := New(Options{
		DataDir:            dir,
		MetaDir:            filepath.Join(dir, "meta"),
		ChunkSize:          64 << 10,
		MaxConcurrentTasks: 1,
		SkipSpaceCheck:     true,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer a.Stop()

	// 故意给一个错误的期望摘要。
	task, err := a.AddHTTP(ctx, srv.URL+"/payload.bin", AddOptions{
		Name:         "wrong.bin",
		ExpectedHash: hex.EncodeToString(bytes.Repeat([]byte{0xAB}, 32)),
	})
	if err != nil {
		t.Fatalf("AddHTTP 失败: %v", err)
	}
	waitFor(t, 60*time.Second, "任务失败", func() bool {
		v, _ := a.Scheduler().Get(task.ID)
		return v.State == engine.StateFailed
	})
	view, _ := a.Scheduler().Get(task.ID)
	if view.LastError == "" {
		t.Fatal("摘要不符应给出错误原因")
	}
}
