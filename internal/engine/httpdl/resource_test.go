package httpdl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"p2pdl/internal/engine"
	"p2pdl/internal/engine/limiter"
)

// 内存预算应被换算成「并发 × 缓冲」的约束，而不是被静默忽略。
func TestMemoryBudgetAdjustsConcurrencyAndBuffer(t *testing.T) {
	ts := newTrackingServer(t, int(testTotal), false)
	task := newTask(t, ts.URL+"/blob.bin", 8)

	t.Run("缩小缓冲", func(t *testing.T) {
		d, err := New(task, &memSink{}, Options{
			Concurrency: 8,
			BufferSize:  64 << 10,
			MaxMemory:   64 << 10, // 8 × 64KB=512KB 超预算 → 缓冲缩到 8KB
		})
		if err != nil {
			t.Fatalf("New 失败: %v", err)
		}
		if used := int64(d.opts.BufferSize) * int64(d.opts.Concurrency); used > d.opts.MaxMemory {
			t.Fatalf("预算超限: 并发 %d × 缓冲 %d = %d > %d",
				d.opts.Concurrency, d.opts.BufferSize, used, d.opts.MaxMemory)
		}
		if d.opts.BufferSize != 8<<10 {
			t.Errorf("缓冲 = %d, 期望 8192", d.opts.BufferSize)
		}
	})

	t.Run("进一步缩小并发", func(t *testing.T) {
		d, err := New(newTask(t, ts.URL+"/blob.bin", 8), &memSink{}, Options{
			Concurrency: 8,
			BufferSize:  32 << 10,
			MaxMemory:   16 << 10, // 缓冲下限 4KB 时只允许 4 个并发
		})
		if err != nil {
			t.Fatalf("New 失败: %v", err)
		}
		if used := int64(d.opts.BufferSize) * int64(d.opts.Concurrency); used > d.opts.MaxMemory {
			t.Fatalf("预算超限: 并发 %d × 缓冲 %d = %d > %d",
				d.opts.Concurrency, d.opts.BufferSize, used, d.opts.MaxMemory)
		}
	})

	t.Run("预算过小直接拒绝", func(t *testing.T) {
		if _, err := New(newTask(t, ts.URL+"/blob.bin", 8), &memSink{}, Options{
			MaxMemory: 1024,
		}); err == nil {
			t.Fatal("内存上限小于单连接下限时应报错")
		}
	})

	t.Run("预算生效于实际下载并发", func(t *testing.T) {
		ts2 := newTrackingServer(t, int(testTotal), false)
		t2 := newTask(t, ts2.URL+"/blob.bin", 8)
		sink := &memSink{}
		d, err := New(t2, sink, Options{
			Concurrency:    8,
			BufferSize:     8 << 10,
			MaxMemory:      8 << 10, // 缓冲下限 4KB → 只允许 2 个并发
			SkipSpaceCheck: true,
		})
		if err != nil {
			t.Fatalf("New 失败: %v", err)
		}
		if used := int64(d.opts.BufferSize) * int64(d.opts.Concurrency); used > d.opts.MaxMemory {
			t.Fatalf("预算超限: 并发 %d × 缓冲 %d = %d > %d",
				d.opts.Concurrency, d.opts.BufferSize, used, d.opts.MaxMemory)
		}
		wantConcurrency := int(d.opts.Concurrency)
		if wantConcurrency != 2 {
			t.Fatalf("预算换算后的并发 = %d, 期望 2", wantConcurrency)
		}
		// 让服务端等到并发到齐再响应，从而可观测实际并发。
		ts2.waitForPeak.Store(int64(wantConcurrency))

		if err := d.Run(context.Background()); err != nil {
			t.Fatalf("Run 失败: %v", err)
		}
		if got := ts2.maxConcurrent(); got > int64(wantConcurrency) {
			t.Fatalf("实测并发 = %d, 内存预算下应不超过 %d", got, wantConcurrency)
		}
		if !bytes.Equal(sink.Bytes(), ts2.content) {
			t.Error("内容不一致")
		}
	})
}

// 验收核心：下载前的磁盘空间预检查能在空间不足时提前失败。
func TestDiskSpacePrecheckRejectsMissingVolume(t *testing.T) {
	ts := newTrackingServer(t, int(testTotal), false)
	task := newTask(t, ts.URL+"/blob.bin", 2)
	// 指向不存在的卷：无法确认空间 → 必须在开始下载前失败。
	task.TempPath = `Z:\p2pdl-test\blob.part`

	d, err := New(task, &memSink{}, Options{SkipSpaceCheck: false})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	err = d.Run(context.Background())
	if err == nil {
		t.Fatal("无法确认磁盘空间时应提前报错")
	}
	if atomicRequests(ts) > 0 {
		t.Fatalf("预检查失败时不应发起任何下载请求（实际 %d 个）", atomicRequests(ts))
	}
}

func TestDiskSpacePrecheckCanBeSkipped(t *testing.T) {
	ts := newTrackingServer(t, int(testTotal), false)
	task := newTask(t, ts.URL+"/blob.bin", 2)
	task.TempPath = `Z:\p2pdl-test\blob.part`
	sink := &memSink{}

	d, err := New(task, sink, Options{SkipSpaceCheck: true})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("跳过检查后应能正常下载: %v", err)
	}
	if !bytes.Equal(sink.Bytes(), ts.content) {
		t.Error("内容不一致")
	}
}

// 验收核心：限速对真实 HTTP 下载生效，实测速率落在设定值 ±15% 内。
func TestDownloadRateLimitIsEnforced(t *testing.T) {
	const (
		rate    = int64(256 << 10) // 256KB/s
		content = int64(1 << 20)   // 1MB
	)
	ts := newTrackingServer(t, int(content), false)
	task := newTask(t, ts.URL+"/blob.bin", 2)
	// 让任务的实际大小与服务器内容一致。
	task, err := engine.NewTask("rl-1", engine.SourceHTTP, ts.URL+"/blob.bin",
		"dest.bin", "dest.part", content, testChunk, 2)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	sink := &memSink{}

	d, err := New(task, sink, Options{
		Concurrency:    2,
		BufferSize:     8 << 10,
		RateLimit:      rate,
		SkipSpaceCheck: true,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	start := time.Now()
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	elapsed := time.Since(start)
	actual := float64(content) / elapsed.Seconds()

	t.Logf("限速 %s，实测 %s（耗时 %v）",
		limiter.FormatRate(float64(rate)), limiter.FormatRate(actual), elapsed)
	if actual > float64(rate)*1.15 {
		t.Fatalf("实测速率 %s 超过设定值 %s 的 15%%", limiter.FormatRate(actual), limiter.FormatRate(float64(rate)))
	}
	if !bytes.Equal(sink.Bytes(), ts.content) {
		t.Error("限速下载内容不一致")
	}
}

// 全局限速与任务限速同时存在时，较严格的一级生效。
func TestGlobalLimiterAppliesToDownload(t *testing.T) {
	const content = int64(512 << 10)
	ts := newTrackingServer(t, int(content), false)
	task, err := engine.NewTask("gl-1", engine.SourceHTTP, ts.URL+"/blob.bin",
		"dest.bin", "dest.part", content, testChunk, 2)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}

	global := limiter.NewLimiter(128 << 10) // 128KB/s，比任务限速更严格
	d, err := New(task, &memSink{}, Options{
		Concurrency:    2,
		BufferSize:     8 << 10,
		RateLimit:      10 << 20,
		GlobalLimiter:  global,
		SkipSpaceCheck: true,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	start := time.Now()
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 3*time.Second {
		t.Fatalf("耗时 %v，全局限速未生效", elapsed)
	}
}

// 大文件场景的内存占用应来自「并发 × 缓冲」这一项，而不是随文件体积增长。
func TestMemoryFootprintIsIndependentOfFileSize(t *testing.T) {
	// 用超大 TotalSize 构造任务（不实际下载），检查内存占用估算与文件体积无关。
	task, err := engine.NewTask("big-1", engine.SourceHTTP, "http://example.com/10gb.iso",
		"dest.iso", "dest.part", 10<<30, 1<<20, 8)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	d, err := New(task, &memSink{}, Options{
		Concurrency: 8,
		BufferSize:  64 << 10,
		MaxMemory:   1 << 20, // 1MB 预算
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	budget := int64(d.opts.BufferSize) * int64(d.opts.Concurrency)
	if budget > 1<<20 {
		t.Fatalf("缓冲区占用 %d 字节，超过 1MB 预算", budget)
	}

	// 位图等元数据的规模只与「块数」相关：10GB / 1MB = 10240 位 ≈ 1.25KB。
	metaBytes := int64(0)
	for _, seg := range task.Segments {
		metaBytes += int64(len(seg.Bitmap.Bytes()))
	}
	if metaBytes > 64<<10 {
		t.Fatalf("10GB 任务的位图元数据 = %d 字节，超出预期", metaBytes)
	}
	t.Logf("10GB 任务：缓冲预算 %d 字节，位图元数据 %d 字节", budget, metaBytes)
}

func TestOptionsNormalizeDefaults(t *testing.T) {
	var o Options
	o.normalize()
	if o.Concurrency != DefaultConcurrency {
		t.Errorf("Concurrency = %d", o.Concurrency)
	}
	if o.MaxRetries != DefaultMaxRetries {
		t.Errorf("MaxRetries = %d", o.MaxRetries)
	}
	if o.BufferSize != DefaultBufferSize {
		t.Errorf("BufferSize = %d", o.BufferSize)
	}
	if o.Client == nil {
		t.Error("Client 应有默认值")
	}
	if o.Client.Timeout != 0 {
		t.Error("默认 Client 不应设置整体超时（会中断大文件下载）")
	}
}

// 写入真实文件时不应出现高频小写入：每块写入应接近缓冲区大小。
func TestRealFileWritesAreCoalesced(t *testing.T) {
	const content = int64(512 << 10)
	ts := newTrackingServer(t, int(content), false)
	task, err := engine.NewTask("rf-1", engine.SourceHTTP, ts.URL+"/blob.bin",
		"dest.bin", "dest.part", content, testChunk, 2)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}

	dir := t.TempDir()
	finalPath := filepath.Join(dir, "out.bin")
	fs, err := os.OpenFile(filepath.Join(dir, "out.part"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("创建文件失败: %v", err)
	}
	defer func() { _ = fs.Close(); _ = os.Remove(finalPath) }()

	counter := &countingSink{inner: fs}
	d, err := New(task, counter, Options{
		Concurrency:    2,
		BufferSize:     32 << 10,
		SkipSpaceCheck: true,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	// 512KB / 32KB = 16 次写入；允许因分片边界切分而略多，但不应是碎片化的小写入。
	if counter.writes > 40 {
		t.Fatalf("写入次数 = %d，缓冲未被有效利用", counter.writes)
	}
	if counter.bytes != content {
		t.Fatalf("写入字节 = %d, 期望 %d", counter.bytes, content)
	}
	t.Logf("512KB 内容产生 %d 次 WriteAt（缓冲 32KB）", counter.writes)
}

type countingSink struct {
	inner  io.WriterAt
	writes int
	bytes  int64
}

func (c *countingSink) WriteAt(p []byte, off int64) (int, error) {
	c.writes++
	c.bytes += int64(len(p))
	return c.inner.WriteAt(p, off)
}

func (c *countingSink) Sync() error { return nil }

// 无法确认磁盘空间时的错误必须可被调用方识别为「空间问题」而非通用失败。
func TestSpaceErrorIsClassifiable(t *testing.T) {
	err := limiter.EnsureFreeSpace(t.TempDir(), 1<<62)
	if !errors.Is(err, limiter.ErrInsufficientSpace) {
		t.Fatalf("错误 = %v, 期望可被识别为 ErrInsufficientSpace", err)
	}
}

func atomicRequests(ts *trackingServer) int64 { return atomic.LoadInt64(&ts.requests) }
