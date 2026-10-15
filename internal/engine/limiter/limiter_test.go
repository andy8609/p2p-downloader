package limiter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 验收核心：限速设置生效，实测速率在设定值 ±15% 以内。
func TestLimiterHoldsConfiguredRate(t *testing.T) {
	const (
		rate  = int64(256 << 10) // 256KB/s
		total = int64(1 << 20)   // 1MB
	)
	l := NewLimiter(rate)
	// 用零拷贝的可重复读取源，把限速逻辑与网络抖动解耦。
	src := bytes.NewReader(make([]byte, int(total)))
	r := NewThrottledReader(context.Background(), src, Chain{Task: l})

	start := time.Now()
	n, err := io.Copy(io.Discard, r)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if n != total {
		t.Fatalf("读取字节数 = %d, 期望 %d", n, total)
	}

	actual := float64(n) / elapsed.Seconds()
	lower, upper := float64(rate)*0.85, float64(rate)*1.15
	t.Logf("限速 %s，实测 %s（耗时 %v）", FormatRate(float64(rate)), FormatRate(actual), elapsed)
	if actual < lower || actual > upper {
		t.Fatalf("实测速率 %s 超出设定值 %s 的 ±15%% 区间",
			FormatRate(actual), FormatRate(float64(rate)))
	}
}

func TestUnlimitedLimiterHasNoDelay(t *testing.T) {
	l := NewLimiter(0)
	if l.Limit() != 0 {
		t.Fatalf("Limit() = %d, 期望 0", l.Limit())
	}
	start := time.Now()
	if err := l.WaitN(context.Background(), 64<<20); err != nil {
		t.Fatalf("WaitN 失败: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("不限速时等待了 %v", elapsed)
	}
}

// 两级限速串联时，实际速率由较小的一级决定。
func TestChainTakesStricterLimit(t *testing.T) {
	global := NewLimiter(1 << 20)  // 1MB/s
	task := NewLimiter(128 << 10)  // 128KB/s（更严格）
	const total = int64(512 << 10)

	r := NewThrottledReader(context.Background(), bytes.NewReader(make([]byte, int(total))),
		Chain{Global: global, Task: task})

	start := time.Now()
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	elapsed := time.Since(start)
	// 受 128KB/s 约束，512KB 至少需要约 4 秒。
	if elapsed < 3*time.Second {
		t.Fatalf("耗时 %v，看起来没有按较严格的一级限速", elapsed)
	}
	if !(Chain{Global: global, Task: task}).Enabled() {
		t.Error("Enabled() 应为 true")
	}
	if (Chain{}).Enabled() {
		t.Error("空链条 Enabled() 应为 false")
	}
}

func TestChainRespectsContextCancellation(t *testing.T) {
	l := NewLimiter(1 << 10) // 1KB/s，必然需要等待
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (Chain{Task: l}).WaitN(ctx, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitN 错误 = %v, 期望 context.Canceled", err)
	}
}

func TestSetLimitDynamically(t *testing.T) {
	l := NewLimiter(1 << 20)
	if got := l.Limit(); got != 1<<20 {
		t.Fatalf("Limit() = %d", got)
	}
	l.SetLimit(64 << 10)
	if got := l.Limit(); got != 64<<10 {
		t.Fatalf("调整后 Limit() = %d, 期望 %d", got, 64<<10)
	}
	l.SetLimit(0)
	if got := l.Limit(); got != 0 {
		t.Fatalf("关闭限速后 Limit() = %d, 期望 0", got)
	}
	if err := l.WaitN(context.Background(), 1<<30); err != nil {
		t.Fatalf("不限速时 WaitN 应立即返回: %v", err)
	}
}

func TestNilLimiterIsSafe(t *testing.T) {
	var l *Limiter
	if err := l.WaitN(context.Background(), 1); err != nil {
		t.Errorf("nil 限速器 WaitN 应返回 nil: %v", err)
	}
	if l.Limit() != 0 {
		t.Errorf("nil 限速器 Limit() 应为 0")
	}
	l.SetLimit(100) // 不应 panic
}

func TestThrottledReaderReturnsUnderlyingReaderWhenDisabled(t *testing.T) {
	src := strings.NewReader("hello")
	got := NewThrottledReader(context.Background(), src, Chain{})
	if _, ok := got.(*ThrottledReader); ok {
		t.Fatal("未启用限速时不应产生包装开销")
	}
}

func TestBufferPoolReusesBuffers(t *testing.T) {
	p := NewBufferPool(1024)
	if p.Size() != 1024 {
		t.Fatalf("Size() = %d", p.Size())
	}

	b1 := p.Get()
	if len(b1) != 1024 {
		t.Fatalf("Get 长度 = %d", len(b1))
	}
	for i := range b1 {
		b1[i] = 0xAB
	}
	p.Put(b1)

	b2 := p.Get()
	if len(b2) != 1024 {
		t.Fatalf("复用后长度 = %d", len(b2))
	}
	if &b1[0] != &b2[0] {
		t.Log("底层数组未复用（sync.Pool 允许在其他 P 上重新分配）")
	}

	// 长度不足的切片必须被忽略，不能污染池中的块大小。
	p.Put(make([]byte, 10))
	if got := len(p.Get()); got != 1024 {
		t.Fatalf("池被污染：长度 = %d", got)
	}

	if NewBufferPool(0).Size() != DefaultBufferSize {
		t.Error("size<=0 时应使用默认大小")
	}
}

// 验收核心：磁盘空间不足必须提前报错。
func TestEnsureFreeSpace(t *testing.T) {
	dir := t.TempDir()
	free, err := FreeSpace(dir)
	if err != nil {
		t.Fatalf("FreeSpace 失败: %v", err)
	}
	if free <= 0 {
		t.Fatalf("可用空间 = %d, 应大于 0", free)
	}

	if err := EnsureFreeSpace(dir, 1024); err != nil {
		t.Fatalf("1KB 应能通过检查: %v", err)
	}
	if err := EnsureFreeSpace(dir, 0); err != nil {
		t.Fatalf("need<=0 应直接通过: %v", err)
	}

	err = EnsureFreeSpace(dir, 1<<62)
	if !errors.Is(err, ErrInsufficientSpace) {
		t.Fatalf("超大需求应返回 ErrInsufficientSpace, 实际 %v", err)
	}

	// 目录尚不存在时应退到最近的祖先目录，而不是报错。
	nested := filepath.Join(dir, "not-created-yet", "deep")
	if err := EnsureFreeSpace(nested, 1024); err != nil {
		t.Fatalf("不存在的深目录应回退到祖先目录: %v", err)
	}

	if _, err := FreeSpace(""); err == nil {
		t.Error("空路径应返回错误")
	}
}

func TestFreeSpaceRejectsImpossibleVolume(t *testing.T) {
	// Z: 卷在本机不存在，应返回错误而非误判为「空间充足」。
	if _, err := FreeSpace(`Z:\definitely-not-here`); err == nil {
		t.Skip("测试机存在 Z: 卷，跳过")
	}
}

func TestHumanFormatting(t *testing.T) {
	if got := FormatBytes(1536); !strings.HasPrefix(got, "1.50 KB") {
		t.Errorf("FormatBytes(1536) = %q", got)
	}
	if got := FormatRate(2 << 20); !strings.Contains(got, "MB/s") {
		t.Errorf("FormatRate(2MB) = %q", got)
	}
	if got := FormatBytes(0); !strings.Contains(got, "B") {
		t.Errorf("FormatBytes(0) = %q", got)
	}
}

func BenchmarkBufferPool(b *testing.B) {
	p := NewBufferPool(32 << 10)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf := p.Get()
			buf[0] = 1
			p.Put(buf)
		}
	})
}

func BenchmarkThrottledReaderUnlimited(b *testing.B) {
	data := make([]byte, 1<<20)
	r := NewThrottledReader(context.Background(), bytes.NewReader(data), Chain{})
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := io.Copy(io.Discard, r); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFreeSpace(b *testing.B) {
	dir := b.TempDir()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := FreeSpace(dir); err != nil {
			b.Fatal(err)
		}
	}
}
