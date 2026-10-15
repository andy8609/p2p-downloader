package httpdl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"p2pdl/internal/engine"
)

// benchSink 是预分配容量的内存 sink，避免扩容复制干扰吞吐测量。
type benchSink struct {
	buf []byte
}

func newBenchSink(size int) *benchSink { return &benchSink{buf: make([]byte, size)} }

func (b *benchSink) WriteAt(p []byte, off int64) (int, error) {
	end := int(off) + len(p)
	if end > len(b.buf) {
		return 0, fmt.Errorf("benchSink: 越界写入 %d（容量 %d）", end, len(b.buf))
	}
	copy(b.buf[off:end], p)
	return len(p), nil
}

func (b *benchSink) Sync() error { return nil }

// 多分段并发下载到内存的上限吞吐（本机 loopback）。
func BenchmarkDownloadToMemory(b *testing.B) {
	const (
		size        = 8 << 20
		bufferSize  = 64 << 10
		concurrency = 8
	)
	ts := newTrackingServer(b, size, false)
	b.SetBytes(size)
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		task, err := engine.NewTask(fmt.Sprintf("bench-mem-%d", i), engine.SourceHTTP,
			ts.URL+"/blob.bin", "dest.bin", "dest.part", size, 1<<20, concurrency)
		if err != nil {
			b.Fatalf("NewTask 失败: %v", err)
		}
		sink := newBenchSink(size)
		d, err := New(task, sink, Options{
			Concurrency:    concurrency,
			BufferSize:     bufferSize,
			SkipSpaceCheck: true,
		})
		if err != nil {
			b.Fatalf("New 失败: %v", err)
		}
		if err := d.Run(context.Background()); err != nil {
			b.Fatalf("Run 失败: %v", err)
		}
	}
}

// 单分段下载的比较基线：用于说明多分段带来的吞吐提升。
func BenchmarkDownloadSingleSegment(b *testing.B) {
	const size = 8 << 20
	ts := newTrackingServer(b, size, false)
	b.SetBytes(size)
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		task, err := engine.NewTask(fmt.Sprintf("bench-single-%d", i), engine.SourceHTTP,
			ts.URL+"/blob.bin", "dest.bin", "dest.part", size, 1<<20, 1)
		if err != nil {
			b.Fatalf("NewTask 失败: %v", err)
		}
		sink := newBenchSink(size)
		d, err := New(task, sink, Options{
			Concurrency:    1,
			BufferSize:     64 << 10,
			SkipSpaceCheck: true,
		})
		if err != nil {
			b.Fatalf("New 失败: %v", err)
		}
		if err := d.Run(context.Background()); err != nil {
			b.Fatalf("Run 失败: %v", err)
		}
	}
}

// 真实文件落盘的多分段下载（含临时文件写入与 fsync）。
func BenchmarkDownloadToFile(b *testing.B) {
	const (
		size        = 8 << 20
		bufferSize  = 64 << 10
		concurrency = 8
	)
	ts := newTrackingServer(b, size, false)
	dir := b.TempDir()
	b.SetBytes(size)
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		finalPath := filepath.Join(dir, fmt.Sprintf("out-%d.bin", i))
		tempPath := filepath.Join(dir, fmt.Sprintf("out-%d.part", i))
		f, err := os.OpenFile(tempPath, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			b.Fatalf("创建文件失败: %v", err)
		}
		task, err := engine.NewTask(fmt.Sprintf("bench-file-%d", i), engine.SourceHTTP,
			ts.URL+"/blob.bin", finalPath, tempPath, size, 1<<20, concurrency)
		if err != nil {
			b.Fatalf("NewTask 失败: %v", err)
		}
		d, err := New(task, f, Options{
			Concurrency:    concurrency,
			BufferSize:     bufferSize,
			SkipSpaceCheck: true,
		})
		if err != nil {
			b.Fatalf("New 失败: %v", err)
		}
		if err := d.Run(context.Background()); err != nil {
			b.Fatalf("Run 失败: %v", err)
		}
		_ = f.Close()
	}
}

// 不同缓冲大小对系统调用次数与吞吐的影响，用于选择默认缓冲值。
func BenchmarkDownloadBufferSizes(b *testing.B) {
	const size = 8 << 20
	for _, bufSize := range []int{8 << 10, 32 << 10, 64 << 10, 256 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("buffer=%dKB", bufSize>>10), func(b *testing.B) {
			ts := newTrackingServer(b, size, false)
			b.SetBytes(size)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				task, err := engine.NewTask(fmt.Sprintf("bench-buf-%d", i), engine.SourceHTTP,
					ts.URL+"/blob.bin", "dest.bin", "dest.part", size, 1<<20, 8)
				if err != nil {
					b.Fatalf("NewTask 失败: %v", err)
				}
				sink := newBenchSink(size)
				d, err := New(task, sink, Options{
					Concurrency:    8,
					BufferSize:     bufSize,
					SkipSpaceCheck: true,
				})
				if err != nil {
					b.Fatalf("New 失败: %v", err)
				}
				if err := d.Run(context.Background()); err != nil {
					b.Fatalf("Run 失败: %v", err)
				}
			}
		})
	}
}

// 每分段一个连接的探测与调度开销（不含数据传输）。
func BenchmarkProbeOnly(b *testing.B) {
	ts := newTrackingServer(b, 1<<20, false)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Probe(context.Background(), nil, ts.URL+"/blob.bin", nil); err != nil {
			b.Fatal(err)
		}
	}
}
