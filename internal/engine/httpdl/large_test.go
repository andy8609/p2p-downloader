package httpdl

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"p2pdl/internal/engine"
	"p2pdl/internal/engine/limiter"
)

// 该测试默认跳过：它会真实写入 10GB 数据并占用大量磁盘与时间。
// 运行方式：
//
//	P2PDL_LARGE_TEST=1 go test ./internal/engine/httpdl -run TestLargeFileMemoryFootprint -v -timeout 30m
//
// 验收标准（T6）：10GB 下载全程峰值内存 < 500MB。
func TestLargeFileMemoryFootprint(t *testing.T) {
	if os.Getenv("P2PDL_LARGE_TEST") == "" {
		t.Skip("设置 P2PDL_LARGE_TEST=1 以运行 10GB 大文件实测")
	}

	const (
		totalSize   = int64(10) << 30 // 10GB
		chunkSize   = int64(16) << 20 // 16MB 位图粒度
		concurrency = 8
		bufferSize  = 32 << 10
	)
	const memLimit = int64(500) << 20

	dir := t.TempDir()
	free, err := limiter.FreeSpace(dir)
	if err != nil {
		t.Fatalf("读取可用空间失败: %v", err)
	}
	if free < totalSize+(1<<30) {
		t.Skipf("可用空间不足（%s），跳过 10GB 实测", limiter.FormatBytes(free))
	}
	t.Logf("目标目录可用空间 %s", limiter.FormatBytes(free))

	srv := newZeroRangeServer(t, totalSize)
	defer srv.Close()

	finalPath := filepath.Join(dir, "big.bin")
	tempPath := filepath.Join(dir, "big.part")
	f, err := os.OpenFile(tempPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("创建临时文件失败: %v", err)
	}

	task, err := engine.NewTask("large-1", engine.SourceHTTP, srv.URL+"/zero.bin",
		finalPath, tempPath, totalSize, chunkSize, concurrency)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}

	// 采样进程内存：只统计 Go 堆的存活对象（HeapAlloc）与向 OS 申请的总量（Sys）。
	var peakHeap, peakSys uint64
	stop := make(chan struct{})
	var samplerWG sync.WaitGroup
	var samples atomic.Int64
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				samples.Add(1)
				for {
					cur := atomic.LoadUint64(&peakHeap)
					if ms.HeapAlloc <= cur || atomic.CompareAndSwapUint64(&peakHeap, cur, ms.HeapAlloc) {
						break
					}
				}
				for {
					cur := atomic.LoadUint64(&peakSys)
					if ms.Sys <= cur || atomic.CompareAndSwapUint64(&peakSys, cur, ms.Sys) {
						break
					}
				}
			}
		}
	}()

	d, err := New(task, f, Options{
		Concurrency:    concurrency,
		BufferSize:     bufferSize,
		SkipSpaceCheck: true,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	start := time.Now()
	runErr := d.Run(context.Background())
	elapsed := time.Since(start)

	close(stop)
	samplerWG.Wait()
	stats := d.Stats()

	t.Logf("10GB 下载完成：耗时 %v（平均 %s），采样 %d 次",
		elapsed.Round(time.Millisecond),
		limiter.FormatRate(float64(totalSize)/elapsed.Seconds()),
		samples.Load())
	t.Logf("内存峰值：Go 堆 %s，进程 Sys %s",
		limiter.FormatBytes(int64(peakHeap)), limiter.FormatBytes(int64(peakSys)))
	t.Logf("分片统计：模式=%s 并发峰值=%d 连接总数=%d",
		stats.Mode, stats.ActiveSegments, stats.ConnectionsTotal)

	if runErr != nil {
		t.Fatalf("Run 失败: %v", runErr)
	}
	if !task.Complete() {
		t.Fatal("任务应判定为完成")
	}

	// 校验落盘长度与内容首尾片段，避免为 10GB 数据做全量哈希（由 T15 的端到端测试负责）。
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if fi.Size() != totalSize {
		t.Fatalf("落盘长度 = %d, 期望 %d", fi.Size(), totalSize)
	}
	_ = f.Close()

	if int64(peakHeap) >= memLimit {
		t.Fatalf("Go 堆峰值 %s 超过 500MB 上限",
			limiter.FormatBytes(int64(peakHeap)))
	}
	// 缓冲区理论占用：并发 × 缓冲 = 8 × 32KB = 256KB。
	theoretical := int64(concurrency) * int64(bufferSize)
	t.Logf("缓冲区理论占用 %s，实测堆峰值 %s（%.0f 倍）",
		limiter.FormatBytes(theoretical), limiter.FormatBytes(int64(peakHeap)),
		float64(peakHeap)/float64(theoretical))
}

// newZeroRangeServer 提供一个支持 Range、按需产生指定长度全零内容的服务器，
// 使 10GB 测试不需要真实源文件，也不占用服务器内存。
func newZeroRangeServer(t *testing.T, size int64) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := int64(0), size-1
		status := http.StatusOK
		if raw := r.Header.Get("Range"); raw != "" {
			var err error
			start, end, err = parseRangeSpec(raw, size)
			if err != nil {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			status = http.StatusPartialContent
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}
		buf := make([]byte, 256<<10)
		_, _ = io.CopyBuffer(w, io.LimitReader(zeroReader{}, end-start+1), buf)
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// parseRangeSpec 解析 "bytes=start-end"，end 可省略。
func parseRangeSpec(raw string, size int64) (int64, int64, error) {
	spec := raw
	if trimmed, ok := strings.CutPrefix(raw, "bytes="); ok {
		spec = trimmed
	}
	spec = strings.TrimSpace(spec)
	startStr, endStr, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, fmt.Errorf("非法 Range: %q", raw)
	}
	start, err := strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	end := size - 1
	if s := strings.TrimSpace(endStr); s != "" {
		if end, err = strconv.ParseInt(s, 10, 64); err != nil {
			return 0, 0, err
		}
	}
	if start < 0 || end >= size || start > end {
		return 0, 0, fmt.Errorf("Range 区间非法: %q", raw)
	}
	return start, end, nil
}


