// Package limiter 提供带宽限速、缓冲区复用与磁盘空间预检查。
//
// 三类控制的职责边界：
//   - 限速：控制「单位时间进入多少数据」，防止单个任务或全部任务占满带宽；
//   - 缓冲区池：控制「内存中有多少块数据在途」，避免大文件下载产生分配风暴；
//   - 磁盘空间预检查：把「中途写满磁盘」提前到「开始前就报错」。
package limiter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/time/rate"
)

// ErrInsufficientSpace 表示目标磁盘可用空间不足以完成下载。
var ErrInsufficientSpace = errors.New("limiter: 磁盘可用空间不足")

// Limiter 是字节级令牌桶限速器。
//
// 限速值 <= 0 表示不限速，此时所有 WaitN 立即返回，不产生任何调度开销。
type Limiter struct {
	mu        sync.RWMutex
	lim       *rate.Limiter
	unlimited bool
	bytesRate int64
}

// NewLimiter 创建限速器。bytesPerSecond <= 0 表示不限速。
//
// burst 决定瞬时突发上限。取值策略：
//
//	burst = clamp(bytesPerSecond/16, 8KB, 4MB)，且不超过 1 秒的额度。
//
// burst 取「1 秒额度」会让短时间的下载完全不受限（例如限速 1MB/s 时前 1MB
// 会瞬间完成），使限速在实测中失真；取速率/16 能在平滑度与唤醒频率间平衡。
func NewLimiter(bytesPerSecond int64) *Limiter {
	if bytesPerSecond <= 0 {
		return &Limiter{unlimited: true}
	}
	burst := bytesPerSecond / 16
	if burst < 8<<10 {
		burst = 8 << 10
	}
	if burst > 4<<20 {
		burst = 4 << 20
	}
	if burst > bytesPerSecond {
		burst = bytesPerSecond
	}
	return &Limiter{
		lim:       rate.NewLimiter(rate.Limit(bytesPerSecond), int(burst)),
		bytesRate: bytesPerSecond,
	}
}

// burstFor 返回给动态调整限速时使用的突发额度，与 NewLimiter 保持一致。
func burstFor(bytesPerSecond int64) int {
	burst := bytesPerSecond / 16
	if burst < 8<<10 {
		burst = 8 << 10
	}
	if burst > 4<<20 {
		burst = 4 << 20
	}
	if burst > bytesPerSecond {
		burst = bytesPerSecond
	}
	return int(burst)
}

// WaitN 阻塞直到允许传输 n 个字节，或 ctx 结束。
//
// 当 n 大于令牌桶突发额度时会自动拆分为多次等待：
// x/time/rate 对超过 burst 的请求直接报错而不阻塞，而我们的读取缓冲区
// 大小与用户设置的限速值互不相干，必须在这里兼容。
func (l *Limiter) WaitN(ctx context.Context, n int) error {
	if l == nil || l.unlimited || n <= 0 {
		return nil
	}
	l.mu.RLock()
	lim := l.lim
	l.mu.RUnlock()
	if lim == nil {
		return nil
	}

	burst := lim.Burst()
	if burst <= 0 {
		return lim.WaitN(ctx, n)
	}
	for n > 0 {
		chunk := n
		if chunk > burst {
			chunk = burst
		}
		if err := lim.WaitN(ctx, chunk); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

// Limit 返回当前限速值（字节/秒），不限速时返回 0。
func (l *Limiter) Limit() int64 {
	if l == nil {
		return 0
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.unlimited {
		return 0
	}
	return l.bytesRate
}

// SetLimit 动态调整限速值。<=0 表示切换为不限速。
func (l *Limiter) SetLimit(bytesPerSecond int64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if bytesPerSecond <= 0 {
		l.unlimited = true
		l.lim = nil
		l.bytesRate = 0
		return
	}
	l.unlimited = false
	l.bytesRate = bytesPerSecond
	if l.lim == nil {
		l.lim = rate.NewLimiter(rate.Limit(bytesPerSecond), burstFor(bytesPerSecond))
		return
	}
	l.lim.SetLimit(rate.Limit(bytesPerSecond))
	l.lim.SetBurst(burstFor(bytesPerSecond))
}

// Chain 表示「全局限速 + 单任务限速」两级串联。
// 两级都必须放行才能继续，因此实际速率是两者的较小值。
type Chain struct {
	Global *Limiter
	Task   *Limiter
}

// WaitN 依次等待两级限速器。
func (c Chain) WaitN(ctx context.Context, n int) error {
	if err := c.Global.WaitN(ctx, n); err != nil {
		return err
	}
	return c.Task.WaitN(ctx, n)
}

// Enabled 表示此链条是否至少有一级限速生效。
func (c Chain) Enabled() bool {
	return (c.Global != nil && !c.Global.unlimited) || (c.Task != nil && !c.Task.unlimited)
}

// ThrottledReader 按实际读取到的字节数等待令牌，使平均速率受控。
//
// 选择「读取后扣费」而不是「读取前预扣整块」：小块读取不会被整块额度惩罚，
// 限速更贴近真实带宽占用。
type ThrottledReader struct {
	ctx   context.Context
	r     io.Reader
	chain Chain
}

// NewThrottledReader 包装读取器。chain 未启用时返回原读取器，零开销。
func NewThrottledReader(ctx context.Context, r io.Reader, chain Chain) io.Reader {
	if r == nil || !chain.Enabled() {
		return r
	}
	return &ThrottledReader{ctx: ctx, r: r, chain: chain}
}

// Read 实现 io.Reader。
func (t *ThrottledReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		if waitErr := t.chain.WaitN(t.ctx, n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

// NewRateLimiterFor 基于本包的 burst 策略构造 *rate.Limiter，
// 供其他需要直接使用 x/time/rate 的组件（如 BitTorrent 客户端配置）复用。
func NewRateLimiterFor(bytesPerSecond int64) *rate.Limiter {
	if bytesPerSecond <= 0 {
		return rate.NewLimiter(rate.Inf, 0)
	}
	return rate.NewLimiter(rate.Limit(bytesPerSecond), burstFor(bytesPerSecond))
}

// FormatRate 把字节速率格式化为便于阅读的字符串。
func FormatRate(bytesPerSecond float64) string {
	units := []string{"B/s", "KB/s", "MB/s", "GB/s"}
	v := bytesPerSecond
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}

// FormatBytes 把字节数格式化为便于阅读的字符串。
func FormatBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}
