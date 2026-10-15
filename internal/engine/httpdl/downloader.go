package httpdl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"p2pdl/internal/engine"
	"p2pdl/internal/engine/limiter"
)

// 常量集中管理默认值与调优参数。
const (
	// DefaultConcurrency 是默认并发分片数，对应 8 条并行连接。
	DefaultConcurrency = 8
	// DefaultMaxRetries 是单个分片的最大重试次数。
	DefaultMaxRetries = 5
	// DefaultBufferSize 是单次读取的缓冲区大小。
	DefaultBufferSize = 32 << 10
	// progressInterval 是自动保存断点进度的时间间隔。
	progressInterval = time.Second
	// saveByteThreshold 是按数据量触发提前保存的阈值。
	//
	// 为什么需要它：只按时间保存时，强杀会丢掉「最后一次保存之后」的全部数据——
	// 在千兆/局域网环境下 1 秒就是数百 MB。按数据量兜底后，
	// 强杀最多重下 saveByteThreshold 字节，与带宽无关。
	saveByteThreshold = int64(32 << 20) // 32MB
	minBackoff       = 500 * time.Millisecond
	maxBackoff       = 30 * time.Second
	// minBufferSize 是内存预算下的缓冲区下限：再小会明显增加系统调用次数。
	minBufferSize = 4 << 10
	// unknownSizeReserve 是长度未知时为单流下载预留的最小磁盘空间。
	unknownSizeReserve = 64 << 20
	// spaceSafetyFactor 是磁盘空间预检查的安全余量分母（20 表示预留 5%）。
	spaceSafetyFactor = 20
)

// 下载模式，用于在统计信息中向调用方暴露实际生效的策略。
const (
	// ModeMultiSegment 表示服务端支持 Range，按分片并发下载。
	ModeMultiSegment = "multi-segment"
	// ModeSingleStream 表示服务端不支持 Range 或长度未知，退化为单流顺序下载，
	// 此模式下不具备断点续传能力（无法向服务端请求任意偏移）。
	ModeSingleStream = "single-stream"
)

// ErrRangeUnsupported 表示服务端在下载过程中拒绝或忽略了 Range 请求。
var ErrRangeUnsupported = errors.New("httpdl: 服务端不支持 Range 请求")

// Sink 是下载数据的落盘目标。os.File 天然满足该接口。
//
// 只需要 WriteAt：分片可以乱序抵达，必须支持任意偏移写入。
// 不要求 Truncate，因为大文件不做整文件预分配（见 T1 §6 风险 4）。
type Sink interface {
	io.WriterAt
	// Sync 把缓冲数据刷入持久存储。
	Sync() error
}

// ProgressSaver 用于持久化断点进度。
//
// 方法名故意选用 Save，使 store.FileStore 能直接满足该接口；
// 生产环境应传入 sink.ProgressSaver，它会在写元数据前先 fsync 数据文件，
// 保证「位图声称已完成的块」在磁盘上确实已经落盘。
type ProgressSaver interface {
	Save(t *engine.Task) error
}

// Options 是下载器的运行参数，零值即为一组合理默认。
type Options struct {
	// Header 是附加到每个请求上的请求头（Cookie、Referer、鉴权等）。
	Header http.Header
	// Client 允许调用方注入自定义 http.Client（代理、TLS、超时策略）。
	Client *http.Client
	// Concurrency 是并发分片数，<=0 时取 DefaultConcurrency。
	Concurrency int
	// MaxRetries 是单个分片最大重试次数。
	MaxRetries int
	// BufferSize 是单次读取缓冲区大小。
	BufferSize int
	// Probe 是预先探测的结果。为 nil 时下载器会自行探测。
	Probe *ProbeResult
	// Progress 非 nil 时，下载器会每秒（以及每个分片完成时）保存一次断点进度。
	Progress ProgressSaver
	// RateLimit 是本任务的限速值（字节/秒），<=0 表示不限速。
	RateLimit int64
	// GlobalLimiter 是可选的全局限速器；与 RateLimit 同时生效时取较小值。
	GlobalLimiter *limiter.Limiter
	// MaxMemory 是本任务允许使用的缓冲区内存上限（字节）。
	// <=0 表示不限制。超限时会自动缩小单连接缓冲、必要时降低并发数。
	MaxMemory int64
	// SkipSpaceCheck 为 true 时跳过下载前的磁盘空间预检查。
	SkipSpaceCheck bool
	// Logf 用于输出可观测性日志，为 nil 时静默。
	Logf func(format string, args ...any)
}

func (o *Options) normalize() {
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = DefaultMaxRetries
	}
	if o.BufferSize <= 0 {
		o.BufferSize = DefaultBufferSize
	}
	if o.Client == nil {
		o.Client = defaultClient()
	}
}

// applyMemoryBudget 把内存上限换算为「并发数 × 单连接缓冲区」的约束。
// 先缩缓冲，再缩并发，保证总占用不超标。
func (o *Options) applyMemoryBudget() error {
	if o.MaxMemory <= 0 {
		return nil
	}
	if o.MaxMemory < minBufferSize {
		return fmt.Errorf("httpdl: 内存上限 %d 字节小于单连接最小值 %d", o.MaxMemory, minBufferSize)
	}

	if per := o.MaxMemory / int64(o.Concurrency); per < int64(o.BufferSize) {
		if per < minBufferSize {
			per = minBufferSize
		}
		o.BufferSize = int(per)
	}
	if used := int64(o.BufferSize) * int64(o.Concurrency); used > o.MaxMemory {
		maxConcurrency := int(o.MaxMemory / int64(o.BufferSize))
		if maxConcurrency < 1 {
			maxConcurrency = 1
		}
		o.Concurrency = maxConcurrency
	}
	return nil
}

func defaultClient() *http.Client {
	// 不使用 Client.Timeout：它会中断长时间的大文件下载。
	// 连接级超时通过 Transport 控制。
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.MaxIdleConnsPerHost = 32
	return &http.Client{Transport: transport}
}

// Downloader 负责按分片并发地把远端资源写入 Sink，并把进度记录在 Task 上。
//
// 并发安全：Stats 与进度保存可被其他 goroutine 并发调用。
type Downloader struct {
	task   *engine.Task
	sink   Sink
	opts   Options
	client *http.Client
	meter  *speedMeter
	probe  *ProbeResult

	// rates 与 buffers 是大文件场景的资源控制点：前者约束速率，后者约束在途内存。
	rates   limiter.Chain
	buffers *limiter.BufferPool

	// progressMu 串行化对「任务进度状态」的修改与读取。
	// 位图本身不加锁，靠这把锁保证并发 Set 与「保存进度时序列化」互斥。
	progressMu sync.Mutex
	// unsavedBytes 是自上次落盘以来新完成的字节数，用于按数据量触发保存。
	unsavedBytes int64

	mu          sync.Mutex
	mode        string
	activeSegs  map[int]bool
	activeConns int
	connsTotal  int64
	streamed    int64
	startedAt   time.Time
}

// New 构造下载器。task 的分片规划（Segments）可以留空：
// 若探测确认服务端支持 Range，Run 会自动按 Concurrency 切分。
func New(task *engine.Task, sink Sink, opts Options) (*Downloader, error) {
	if task == nil {
		return nil, errors.New("httpdl: 任务为空")
	}
	if sink == nil {
		return nil, errors.New("httpdl: 落盘目标为空")
	}
	if task.Kind != engine.SourceHTTP {
		return nil, fmt.Errorf("httpdl: 任务协议为 %q，不是 http", task.Kind)
	}
	opts.normalize()
	if err := opts.applyMemoryBudget(); err != nil {
		return nil, err
	}
	taskLimiter := limiter.NewLimiter(opts.RateLimit)
	d := &Downloader{
		task:       task,
		sink:       sink,
		opts:       opts,
		client:     opts.Client,
		meter:      newSpeedMeter(5 * time.Second),
		probe:      opts.Probe,
		activeSegs: make(map[int]bool),
		rates:      limiter.Chain{Global: opts.GlobalLimiter, Task: taskLimiter},
		buffers:    limiter.NewBufferPool(opts.BufferSize),
	}
	if opts.MaxMemory > 0 {
		d.logf("httpdl: 内存预算 %s → 并发 %d × 缓冲 %s = %s",
			limiter.FormatBytes(opts.MaxMemory), opts.Concurrency,
			limiter.FormatBytes(int64(opts.BufferSize)),
			limiter.FormatBytes(int64(opts.BufferSize)*int64(opts.Concurrency)))
	}
	return d, nil
}

// Task 返回被驱动的任务对象（进度会实时反映在其中）。
func (d *Downloader) Task() *engine.Task { return d.task }

// Probe 返回实际生效的探测结果。Run 之前为 nil。
func (d *Downloader) Probe() *ProbeResult { return d.probe }

func (d *Downloader) logf(format string, args ...any) {
	if d.opts.Logf != nil {
		d.opts.Logf(format, args...)
	}
}

// Run 阻塞执行下载，直到完成、被取消或重试耗尽。
//
// 返回 context.Canceled 时，已落盘的进度保留在 Task 与 Sink 中，
// 再次调用 Run 会从缺口处继续（暂停/恢复语义）。
func (d *Downloader) Run(ctx context.Context) error {
	d.mu.Lock()
	d.startedAt = time.Now()
	d.mu.Unlock()

	probe := d.opts.Probe
	if probe == nil {
		// 先用任务已知信息做一次空间预检，避免为了探测而先发出网络请求。
		if err := d.checkDiskSpace(); err != nil {
			return err
		}
		p, err := Probe(ctx, d.client, d.task.Source, d.opts.Header)
		if err != nil {
			return err
		}
		probe = p
	}
	d.probe = probe

	// 探测结果可能补齐了任务先前未知的长度。
	if probe.TotalSize > 0 {
		d.progressMu.Lock()
		if d.task.TotalSize != probe.TotalSize {
			d.task.TotalSize = probe.TotalSize
		}
		d.progressMu.Unlock()
	}

	if err := d.checkDiskSpace(); err != nil {
		return err
	}

	stopSaver := d.startProgressSaver(ctx)
	defer func() {
		stopSaver()
		if err := d.sink.Sync(); err != nil {
			d.logf("httpdl: 落盘同步失败: %v", err)
		}
		d.saveProgress()
	}()

	useSegments := probe.AcceptRanges && d.task.TotalSize > 0
	if !useSegments {
		d.setMode(ModeSingleStream)
		d.logf("httpdl: 服务端不支持 Range 或长度未知（acceptRanges=%v size=%d），降级为单流下载",
			probe.AcceptRanges, d.task.TotalSize)
		return d.runSingleStream(ctx)
	}

	d.setMode(ModeMultiSegment)
	return d.runSegments(ctx)
}

// ---------------------------------------------------------------------------
// 多分片并发下载
// ---------------------------------------------------------------------------

func (d *Downloader) runSegments(ctx context.Context) error {
	d.progressMu.Lock()
	if len(d.task.Segments) == 0 {
		if err := d.task.Split(d.opts.Concurrency); err != nil {
			d.progressMu.Unlock()
			return err
		}
	}
	pending := d.task.PendingSegments()
	total := len(d.task.Segments)
	d.progressMu.Unlock()

	if len(pending) == 0 {
		d.logf("httpdl: 所有 %d 个分片均已完成，无需下载", total)
		return nil
	}

	concurrency := d.opts.Concurrency
	if concurrency > len(pending) {
		concurrency = len(pending)
	}
	d.logf("httpdl: 启动 %d 个并发分片（共 %d 个分片待下载）", concurrency, len(pending))

	sem := make(chan struct{}, concurrency)
	errCh := make(chan error, len(pending))
	var wg sync.WaitGroup

	for _, seg := range pending {
		wg.Add(1)
		go func(seg *engine.Segment) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errCh <- fmt.Errorf("分片 %d: %w", seg.Index, ctx.Err())
				return
			}
			defer func() { <-sem }()

			if err := d.downloadSegment(ctx, seg); err != nil {
				errCh <- fmt.Errorf("分片 %d: %w", seg.Index, err)
			}
		}(seg)
	}
	wg.Wait()
	close(errCh)

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// checkDiskSpace 在开始下载前确认目标卷有足够空间。
// 把「写满磁盘」提前为「开始前即拒绝」，避免用户白等。
func (d *Downloader) checkDiskSpace() error {
	if d.opts.SkipSpaceCheck {
		return nil
	}
	dir := filepath.Dir(d.task.TempPath)
	if d.task.TempPath == "" || dir == "" || dir == "." {
		dir = filepath.Dir(d.task.DestPath)
	}
	if dir == "" || dir == "." {
		return nil
	}

	d.progressMu.Lock()
	need := d.task.TotalSize - d.task.Downloaded()
	d.progressMu.Unlock()

	if need > 0 {
		need += need / spaceSafetyFactor
	} else {
		need = unknownSizeReserve
	}
	if err := limiter.EnsureFreeSpace(dir, need); err != nil {
		return err
	}
	return nil
}

// downloadSegment 下载单个分片，内部按分块粒度续传并重试。
func (d *Downloader) downloadSegment(ctx context.Context, seg *engine.Segment) error {
	if seg.Complete() {
		return nil
	}
	d.markSegmentActive(seg, true)
	defer d.markSegmentActive(seg, false)

	var lastErr error
	for attempt := 1; attempt <= d.opts.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			d.setSegmentState(seg, engine.SegmentPending)
			return err
		}

		d.setSegmentState(seg, engine.SegmentRunning)
		err := d.streamSegment(ctx, seg)
		if err == nil {
			d.setSegmentState(seg, engine.SegmentCompleted)
			d.saveProgress()
			return nil
		}
		if ctx.Err() != nil {
			// 取消不是失败：把分片退回待下载，保留已完成的位图进度。
			d.setSegmentState(seg, engine.SegmentPending)
			return ctx.Err()
		}

		lastErr = err
		d.recordSegmentFailure(seg, err)
		d.logf("httpdl: 分片 %d 第 %d/%d 次尝试失败: %v", seg.Index, attempt, d.opts.MaxRetries, err)
		if attempt == d.opts.MaxRetries {
			break
		}
		if !d.sleepBackoff(ctx, attempt) {
			d.setSegmentState(seg, engine.SegmentPending)
			return ctx.Err()
		}
	}

	d.setSegmentState(seg, engine.SegmentFailed)
	d.saveProgress()
	return lastErr
}

// streamSegment 逐段补齐分片内的缺口。
//
// 关键设计：每次只请求「一段连续缺口」，而不是「从第一个缺口一直到分片末尾」。
// 后者会把缺口之后已经下载好的数据重新下一遍——在「校验发现少量坏块」的场景下，
// 这会把 64KB 的重下量放大成整个分片（数百 MB）。
func (d *Downloader) streamSegment(ctx context.Context, seg *engine.Segment) error {
	chunkCount := seg.Bitmap.Len()
	if chunkCount == 0 {
		return nil
	}
	buf := d.buffers.Get()
	defer d.buffers.Put(buf)

	for idx := uint64(0); idx < chunkCount; {
		if seg.Bitmap.Get(idx) {
			idx++
			continue
		}
		// 向后扫描出一段连续的缺口。
		endIdx := idx
		for endIdx < chunkCount && !seg.Bitmap.Get(endIdx) {
			endIdx++
		}
		start, _ := seg.ChunkRange(d.task.ChunkSize, idx)
		_, end := seg.ChunkRange(d.task.ChunkSize, endIdx-1)

		if err := d.fetchGap(ctx, seg, idx, endIdx, start, end, buf); err != nil {
			return err
		}
		idx = endIdx
	}
	return nil
}

// fetchGap 下载 [start,end) 区间并把分块 [idx,endIdx) 标记为已完成。
func (d *Downloader) fetchGap(ctx context.Context, seg *engine.Segment, idx, endIdx uint64, start, end int64, buf []byte) error {
	body, err := d.openRange(ctx, start, end-1)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	chunkSize := d.task.ChunkSize
	pos := start
	for i := idx; i < endIdx; i++ {
		_, chunkEnd := seg.ChunkRange(chunkSize, i)
		if pos > chunkEnd {
			continue
		}
		// 逐块读满并写入：只有整块落盘才置位，保证位图与磁盘内容一致。
		for pos < chunkEnd {
			want := int64(len(buf))
			if chunkEnd-pos < want {
				want = chunkEnd - pos
			}
			if _, err := io.ReadFull(body, buf[:want]); err != nil {
				return fmt.Errorf("读取 %d 偏移处数据失败: %w", pos, err)
			}
			if _, err := d.sink.WriteAt(buf[:want], pos); err != nil {
				return fmt.Errorf("写入 %d 偏移处数据失败: %w", pos, err)
			}
			pos += want
			d.recordBytes(want)
		}
		if d.markChunk(seg, i) {
			// 攒够一个阈值就落盘，避免强杀时丢失过多已完成数据。
			d.saveProgress()
		}
	}
	return nil
}

// openRange 发起一个 Range 请求并返回响应体（已套上限速）。
func (d *Downloader) openRange(ctx context.Context, start, end int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.task.Source, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	applyHeader(req, d.opts.Header)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	res, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	d.connOpened()
	// closeBody 与任何返回路径绑定，确保连接计数不会泄漏。
	closeBody := func() {
		_ = res.Body.Close()
		d.connClosed()
	}

	switch res.StatusCode {
	case http.StatusPartialContent:
		return &countedBody{
			reader:  limiter.NewThrottledReader(ctx, res.Body, d.rates),
			closer:  res.Body,
			onClose: closeBody,
		}, nil
	case http.StatusOK:
		// 服务端忽略了 Range：返回整个文件，继续按分片偏移写入会写错位置。
		closeBody()
		return nil, fmt.Errorf("%w（请求 %d-%d 时返回 200）", ErrRangeUnsupported, start, end)
	default:
		closeBody()
		return nil, fmt.Errorf("请求 %d-%d 返回状态码 %d", start, end, res.StatusCode)
	}
}

// countedBody 在响应体上叠加限速，并保证连接计数只归还一次。
type countedBody struct {
	reader  io.Reader
	closer  io.Closer
	once    sync.Once
	onClose func()
}

func (c *countedBody) Read(p []byte) (int, error) {
	if c.reader == nil {
		return c.closer.(io.Reader).Read(p)
	}
	return c.reader.Read(p)
}

func (c *countedBody) Close() error {
	err := c.closer.Close()
	c.once.Do(c.onClose)
	return err
}

// ---------------------------------------------------------------------------
// 单流降级路径
// ---------------------------------------------------------------------------

// runSingleStream 用于「服务端不支持 Range」或「长度未知」的场景。
// 此模式下无法向服务端索取任意偏移，因此不具备断点续传能力：
// 任务失败或取消后需要重新下载（这是协议能力限制，不是实现缺陷）。
func (d *Downloader) runSingleStream(ctx context.Context) error {
	var lastErr error
	for attempt := 1; attempt <= d.opts.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := d.streamWhole(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
		d.logf("httpdl: 单流下载第 %d/%d 次尝试失败: %v", attempt, d.opts.MaxRetries, err)
		if attempt == d.opts.MaxRetries || !d.sleepBackoff(ctx, attempt) {
			break
		}
	}
	return lastErr
}

func (d *Downloader) streamWhole(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.task.Source, nil)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	applyHeader(req, d.opts.Header)

	res, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	d.connOpened()
	defer func() {
		_ = res.Body.Close()
		d.connClosed()
	}()

	if res.StatusCode >= 400 {
		return fmt.Errorf("单流下载返回状态码 %d", res.StatusCode)
	}
	body := limiter.NewThrottledReader(ctx, res.Body, d.rates)
	expected := parseContentLength(res.Header.Get("Content-Length"))
	if expected > 0 {
		d.progressMu.Lock()
		d.task.TotalSize = expected
		d.progressMu.Unlock()
	}

	buf := d.buffers.Get()
	defer d.buffers.Put(buf)
	var offset int64
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, err := d.sink.WriteAt(buf[:n], offset); err != nil {
				return fmt.Errorf("写入 %d 偏移处数据失败: %w", offset, err)
			}
			offset += int64(n)
			d.recordBytes(int64(n))
			d.addStreamed(int64(n))
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return fmt.Errorf("读取响应体失败: %w", readErr)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	// 下载完成后把任务结构与实际落盘结果对齐，使 Complete()/Validate() 成立。
	d.finalizeSingleStream(offset)
	return nil
}

// finalizeSingleStream 把「长度未知」的任务收敛为自洽的数据结构。
//
// 注意这里**不设置任务状态**：状态推进是调度器的职责，下载器擅自置为
// completed 会让调度器随后的「进入校验态」变成非法迁移（实测踩到过：
// 非法的状态迁移 completed → verifying）。下载器只负责让数据自洽。
func (d *Downloader) finalizeSingleStream(written int64) {
	d.progressMu.Lock()
	defer d.progressMu.Unlock()

	d.task.TotalSize = written
	d.task.Segments = []*engine.Segment{{
		Index:  0,
		Start:  0,
		End:    written,
		State:  engine.SegmentCompleted,
		Bitmap: NewFilledBitmap(written, d.task.ChunkSize),
	}}
	d.task.Touch()
}

// NewFilledBitmap 生成一个「全部置位」的位图，长度按 chunkSize 覆盖 size 字节。
// 用于把单流下载的结果表达为与多段模式一致的数据结构。
func NewFilledBitmap(size, chunkSize int64) *engine.Bitmap {
	if size <= 0 || chunkSize <= 0 {
		return engine.NewBitmap(0)
	}
	b := engine.NewBitmap(uint64((size + chunkSize - 1) / chunkSize))
	b.SetRange(0, b.Len())
	return b
}

// ---------------------------------------------------------------------------
// 进度、状态与统计
// ---------------------------------------------------------------------------

// markChunk 记录一个分块已完成，并返回是否应当立刻保存进度。
//
// 返回值的语义是「按数据量已攒够一次保存」——调用方需在释放 progressMu 后调用
// saveProgress（saveProgress 自身也会加锁，不能在持锁时调用）。
func (d *Downloader) markChunk(seg *engine.Segment, idx uint64) bool {
	d.progressMu.Lock()
	_ = seg.Bitmap.Set(idx)
	if seg.Bitmap.Complete() {
		seg.State = engine.SegmentCompleted
	}
	d.unsavedBytes += d.task.ChunkSize
	needSave := d.unsavedBytes >= saveByteThreshold
	if needSave {
		d.unsavedBytes = 0
	}
	d.progressMu.Unlock()
	return needSave
}

func (d *Downloader) setSegmentState(seg *engine.Segment, state engine.SegmentState) {
	d.progressMu.Lock()
	seg.State = state
	d.progressMu.Unlock()
}

func (d *Downloader) recordSegmentFailure(seg *engine.Segment, err error) {
	d.progressMu.Lock()
	seg.Attempts++
	seg.LastErr = err.Error()
	d.progressMu.Unlock()
}

func (d *Downloader) addStreamed(n int64) {
	d.mu.Lock()
	d.streamed += n
	d.mu.Unlock()
}

// saveProgress 在锁内把任务快照写入存储，保证序列化时位图不被并发修改。
func (d *Downloader) saveProgress() {
	if d.opts.Progress == nil {
		return
	}
	d.progressMu.Lock()
	d.task.Touch()
	err := d.opts.Progress.Save(d.task)
	d.progressMu.Unlock()
	if err != nil {
		d.logf("httpdl: 保存断点进度失败: %v", err)
	}
}

func (d *Downloader) startProgressSaver(ctx context.Context) func() {
	if d.opts.Progress == nil {
		return func() {}
	}
	ticker := time.NewTicker(progressInterval)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				d.saveProgress()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			ticker.Stop()
			close(done)
		})
	}
}

func (d *Downloader) setMode(mode string) {
	d.mu.Lock()
	d.mode = mode
	d.mu.Unlock()
}

func (d *Downloader) markSegmentActive(seg *engine.Segment, active bool) {
	d.mu.Lock()
	if active {
		d.activeSegs[seg.Index] = true
	} else {
		delete(d.activeSegs, seg.Index)
	}
	d.mu.Unlock()
}

func (d *Downloader) connOpened() {
	d.mu.Lock()
	d.activeConns++
	d.connsTotal++
	d.mu.Unlock()
}

func (d *Downloader) connClosed() {
	d.mu.Lock()
	if d.activeConns > 0 {
		d.activeConns--
	}
	d.mu.Unlock()
}

func (d *Downloader) recordBytes(n int64) { d.meter.add(n) }

// SegmentStats 是单个分片的对外快照。
type SegmentStats struct {
	Index      int                 `json:"index"`
	Start      int64               `json:"start"`
	End        int64               `json:"end"`
	State      engine.SegmentState `json:"state"`
	Downloaded int64               `json:"downloaded"`
	Achieved   float64             `json:"progress"`
	Attempts   int                 `json:"attempts,omitempty"`
	LastError  string              `json:"lastError,omitempty"`
	Active     bool                `json:"active"`
}

// Stats 是下载器的瞬时统计快照，供 CLI/GUI 展示。
type Stats struct {
	Mode           string         `json:"mode"`
	TotalSize      int64          `json:"totalSize"`
	Downloaded     int64          `json:"downloaded"`
	Progress       float64        `json:"progress"`
	BytesPerSecond float64        `json:"bytesPerSecond"`
	ActiveSegments int            `json:"activeSegments"`
	ActiveConns    int            `json:"activeConnections"`
	ConnectionsTotal int64        `json:"connectionsTotal"`
	ElapsedSeconds float64        `json:"elapsedSeconds"`
	Segments       []SegmentStats `json:"segments,omitempty"`
}

// Stats 返回当前统计快照，可被其他 goroutine 并发调用。
func (d *Downloader) Stats() Stats {
	d.mu.Lock()
	mode := d.mode
	activeConns := d.activeConns
	connsTotal := d.connsTotal
	activeSegs := make(map[int]bool, len(d.activeSegs))
	for k, v := range d.activeSegs {
		activeSegs[k] = v
	}
	streamed := d.streamed
	startedAt := d.startedAt
	d.mu.Unlock()

	d.progressMu.Lock()
	total := d.task.TotalSize
	segments := make([]SegmentStats, 0, len(d.task.Segments))
	var downloaded int64
	activeCount := 0
	for _, seg := range d.task.Segments {
		done := seg.DownloadedBytes(d.task.ChunkSize)
		downloaded += done
		active := activeSegs[seg.Index]
		if active {
			activeCount++
		}
		ss := SegmentStats{
			Index:      seg.Index,
			Start:      seg.Start,
			End:        seg.End,
			State:      seg.State,
			Downloaded: done,
			Attempts:   seg.Attempts,
			LastError:  seg.LastErr,
			Active:     active,
		}
		if l := seg.Length(); l > 0 {
			ss.Achieved = float64(done) / float64(l)
		}
		segments = append(segments, ss)
	}
	if len(d.task.Segments) == 0 {
		// 单流模式没有分片结构，进度以已写入字节数为准。
		downloaded = streamed
	}
	d.progressMu.Unlock()

	stats := Stats{
		Mode:           mode,
		TotalSize:      total,
		Downloaded:     downloaded,
		BytesPerSecond: d.meter.rate(),
		ActiveSegments: activeCount,
		ActiveConns:    activeConns,
		ConnectionsTotal: connsTotal,
		Segments:       segments,
	}
	if total > 0 {
		stats.Progress = float64(downloaded) / float64(total)
		if stats.Progress > 1 {
			stats.Progress = 1
		}
	}
	if !startedAt.IsZero() {
		stats.ElapsedSeconds = time.Since(startedAt).Seconds()
	}
	return stats
}

// sleepBackoff 按指数退避等待，返回 false 表示 ctx 已结束。
func (d *Downloader) sleepBackoff(ctx context.Context, attempt int) bool {
	delay := minBackoff << (attempt - 1)
	if delay > maxBackoff || delay <= 0 {
		delay = maxBackoff
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
