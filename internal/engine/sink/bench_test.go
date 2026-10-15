package sink

import (
	"os"
	"path/filepath"
	"testing"
)

// 测量按偏移写入的原始吞吐，用于确认「不做整文件预分配」不会拖慢写入路径。
func BenchmarkFileSinkWriteAt(b *testing.B) {
	const total = 64 << 20
	dir := b.TempDir()

	for _, blockSize := range []int{4 << 10, 32 << 10, 256 << 10, 1 << 20} {
		b.Run(blockSizeName(blockSize), func(b *testing.B) {
			b.SetBytes(total)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				path := filepath.Join(dir, "write.bin")
				s, err := Open(path, filepath.Join(dir, "write.part"), Options{
					TotalSize:    total,
					SyncInterval: int64(total), // 基准中避免 fsync 抖动干扰
				})
				if err != nil {
					b.Fatalf("Open 失败: %v", err)
				}
				buf := make([]byte, blockSize)
				for off := int64(0); off < total; off += int64(blockSize) {
					if _, err := s.WriteAt(buf, off); err != nil {
						b.Fatalf("WriteAt 失败: %v", err)
					}
				}
				if err := s.Commit(total); err != nil {
					b.Fatalf("Commit 失败: %v", err)
				}
				_ = os.Remove(path)
			}
		})
	}
}

// 测量周期性 fsync 的代价，用于确认默认 8MB 间隔是合理的。
func BenchmarkFileSinkSync(b *testing.B) {
	const total = 16 << 20
	dir := b.TempDir()
	b.SetBytes(total)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		path := filepath.Join(dir, "sync.bin")
		s, err := Open(path, filepath.Join(dir, "sync.part"), Options{TotalSize: total, SyncInterval: 4 << 20})
		if err != nil {
			b.Fatalf("Open 失败: %v", err)
		}
		buf := make([]byte, 256<<10)
		for off := int64(0); off < total; off += int64(len(buf)) {
			if _, err := s.WriteAt(buf, off); err != nil {
				b.Fatalf("WriteAt 失败: %v", err)
			}
		}
		if err := s.Commit(total); err != nil {
			b.Fatalf("Commit 失败: %v", err)
		}
		_ = os.Remove(path)
	}
}

// 缺口计算在碎片化位图下的开销：大文件可能产生上万个缺口。
func BenchmarkMissingRangesFragmented(b *testing.B) {
	task := recoveryTask(b, 64)
	// 每个分片只标记奇数块：长度为 1 块的分片因此完全未完成，保证确实存在缺口。
	for _, seg := range task.Segments {
		for i := uint64(1); i < seg.Bitmap.Len(); i += 2 {
			_ = seg.Bitmap.Set(i)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := MissingRanges(task); len(got) == 0 {
			b.Fatal("应有缺口")
		}
	}
}

// 完整（无缺口）大位图下的恢复决策开销。
func BenchmarkPlanRecoveryComplete(b *testing.B) {
	dir := b.TempDir()
	tempPath := filepath.Join(dir, "a.part")
	task := recoveryTask(b, 16)
	for _, seg := range task.Segments {
		seg.Bitmap.SetRange(0, seg.Bitmap.Len())
	}
	writeSizedFile(b, tempPath, task.TotalSize)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := PlanRecovery(task, tempPath); err != nil {
			b.Fatal(err)
		}
	}
}

func blockSizeName(n int) string {
	switch {
	case n >= 1<<20:
		return "block=1MB"
	case n >= 256<<10:
		return "block=256KB"
	case n >= 32<<10:
		return "block=32KB"
	default:
		return "block=4KB"
	}
}
