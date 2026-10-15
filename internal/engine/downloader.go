package engine

import (
	"context"
	"time"

	"p2pdl/internal/engine/limiter"
)

// Progress 是引擎无关的统一进度快照。
//
// 两种引擎的内部进度模型完全不同（HTTP 用分片位图，BT 用 piece 位图），
// 但上层（调度器、控制面、GUI）只关心这几个数字。
type Progress struct {
	// Total 是内容总长度；未知时为 SizeUnknown。
	Total int64 `json:"total"`
	// Done 是已完成字节数。
	Done int64 `json:"done"`
	// Rate 是瞬时速率（字节/秒）。
	Rate float64 `json:"rate"`
	// Peers 是当前活跃对等方数量（HTTP 场景恒为 0）。
	Peers int `json:"peers"`
	// Mode 描述实际生效的传输模式（如 multi-segment / single-stream / bittorrent）。
	Mode string `json:"mode"`
}

// Ratio 返回完成比例（0~1）；总长度未知时返回 0。
func (p Progress) Ratio() float64 {
	if p.Total <= 0 {
		return 0
	}
	r := float64(p.Done) / float64(p.Total)
	if r > 1 {
		return 1
	}
	if r < 0 {
		return 0
	}
	return r
}

// Session 是一次「运行中的下载」。每种引擎提供一个实现。
//
// 生命周期：Create → Run（阻塞）→ Close。
// Run 返回 context.Canceled 表示被暂停/取消，此时已落盘的进度必须保留，
// 再次 Create + Run 能从断点继续。
type Session interface {
	// Run 阻塞执行直到完成、被取消或失败。
	Run(ctx context.Context) error
	// Pause 请求暂停；实现应尽快停止传输但保留进度。
	Pause(ctx context.Context) error
	// Progress 返回瞬时进度，可被任意 goroutine 并发调用。
	Progress() Progress
	// Sync 把引擎内部进度写回 Task，供调度器持久化。
	//
	// 对 HTTP 引擎而言，下载器直接操作同一个 Task 指针，这里是空操作；
	// 对 BT 引擎而言，需要把 piece 位图复制进 Task。
	Sync(ctx context.Context) error
	// Close 释放本次运行占用的资源（不解压、不删除已下载数据）。
	Close() error
}

// SessionFactory 按任务类型创建会话。
//
// 分层的意义：engine 包只依赖这个接口，不依赖 httpdl / bittorrent，
// 于是调度逻辑可以用假实现完整测试，而真实装配放在 internal/app。
type SessionFactory interface {
	Create(ctx context.Context, task *Task, opts RunOptions) (Session, error)
}

// RunOptions 是调度器传递给工厂的运行参数，承载全局资源策略。
type RunOptions struct {
	// Concurrency 是单任务的并发连接/分片数。
	Concurrency int
	// RateLimit 是单任务限速（字节/秒），0 表示不限。
	RateLimit int64
	// GlobalLimiter 是全局共享的限速器，与 RateLimit 同时生效时取较小值。
	GlobalLimiter *limiter.Limiter
	// MaxMemory 是单任务允许使用的缓冲区上限（字节），0 表示不限。
	MaxMemory int64
	// SkipSpaceCheck 跳过下载前的磁盘空间预检查（测试与特殊场景）。
	SkipSpaceCheck bool
	// Logf 用于输出引擎日志。
	Logf func(format string, args ...any)
}

// ByteRange 是文件中的半开区间 [Start, End)，用于描述「需要重下的缺口」。
type ByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Length 返回区间长度。
func (r ByteRange) Length() int64 {
	if r.End <= r.Start {
		return 0
	}
	return r.End - r.Start
}

// VerificationResult 是校验结果。
type VerificationResult struct {
	// OK 表示数据完整可信。
	OK bool `json:"ok"`
	// Repaired 表示已定位到损坏区间并标记为需重下（任务应重新排队继续下载）。
	Repaired bool `json:"repaired"`
	// Missing 是判定为损坏、需要重新下载的区间。
	Missing []ByteRange `json:"missing,omitempty"`
	// MissingBytes 是损坏字节数。
	MissingBytes int64 `json:"missingBytes,omitempty"`
	// Reason 是人类可读的结论描述，用于日志与 UI。
	Reason string `json:"reason,omitempty"`
	// Err 是不可修复时的错误。
	Err error `json:"-"`
}

// Verifier 在数据收齐后执行完整性校验。
//
// 返回 (Repaired=true, nil) 时，调度器会把任务重新排队继续下载；
// 返回 error 或 OK=false 且未修复时，任务进入 failed。
type Verifier interface {
	Verify(ctx context.Context, task *Task) (VerificationResult, error)
}

// TaskStore 是调度器需要的持久化能力（store.FileStore 直接满足）。
type TaskStore interface {
	Save(t *Task) error
}

// TaskView 是任务对外的只读视图，控制面与 GUI 只消费它。
type TaskView struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"displayName"`
	Kind        SourceKind `json:"kind"`
	State       TaskState  `json:"state"`
	Source      string     `json:"source"`
	DestPath    string     `json:"destPath"`
	Total       int64      `json:"total"`
	Done        int64      `json:"done"`
	Progress    float64    `json:"progress"`
	Rate        float64    `json:"rate"`
	Peers       int        `json:"peers"`
	Mode        string     `json:"mode"`
	LastError   string     `json:"lastError,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// View 把任务构造为对外视图。
func (t *Task) View() TaskView {
	if t == nil {
		return TaskView{}
	}
	return TaskView{
		ID:          t.ID,
		DisplayName: t.DisplayName,
		Kind:        t.Kind,
		State:       t.State,
		Source:      t.Source,
		DestPath:    t.DestPath,
		Total:       t.TotalSize,
		Done:        t.Downloaded(),
		Progress:    t.Progress(),
		LastError:   t.LastErr,
		Mode:        t.Mode,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	}
}
