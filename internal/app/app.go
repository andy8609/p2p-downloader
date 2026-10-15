// Package app 把具体下载引擎（HTTP / BitTorrent）装配到统一调度器上，
// 形成可运行的应用内核。
//
// 分层职责：
//
//	internal/engine      任务模型、状态机、调度器（不依赖任何具体引擎）
//	internal/engine/httpdl       HTTP 多段下载实现
//	internal/engine/bittorrent   BT / 磁力链实现
//	internal/app         本包：按任务类型装配会话，管理引擎实例生命周期
//	internal/control     本地控制面（T10），只依赖本包与 engine
//
// 之所以需要装配层：engine 定义接口但不 import 具体实现（否则会形成循环依赖），
// 于是「按 Kind 选择实现」这一步必须放在更高一层。
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"p2pdl/internal/engine"
	"p2pdl/internal/engine/bittorrent"
	"p2pdl/internal/engine/httpdl"
	"p2pdl/internal/engine/limiter"
	"p2pdl/internal/engine/sink"
	"p2pdl/internal/engine/verify"
	"p2pdl/internal/store"
)

// 默认配置。
const (
	DefaultChunkSize           = int64(1 << 20) // 1MB
	DefaultConcurrency         = 8
	DefaultMaxConcurrentTasks  = 3
	defaultUnknownNameFallback = "download.bin"
)

// Options 是应用内核配置。
type Options struct {
	// DataDir 是下载文件与 BT 数据的根目录。
	DataDir string
	// MetaDir 是任务元数据目录（断点、位图、索引）。
	MetaDir string
	// ChunkSize 是 HTTP 分片位图的粒度。
	ChunkSize int64
	// Concurrency 是单任务的并发连接数。
	Concurrency int
	// MaxConcurrentTasks 是同时运行的任务数上限。
	MaxConcurrentTasks int
	// RateLimit 是单任务限速（字节/秒），0 表示不限。
	RateLimit int64
	// GlobalRateLimit 是全局限速（字节/秒），0 表示不限。
	GlobalRateLimit int64
	// MaxMemory 是单任务的缓冲区内存上限（字节），0 表示不限。
	MaxMemory int64
	// SkipSpaceCheck 跳过磁盘空间预检查（测试用）。
	SkipSpaceCheck bool
	// BT 是 BitTorrent 引擎配置（DataDir 会被 Options.DataDir 覆盖）。
	BT bittorrent.Config
	// Verifier 是完整性校验器。为 nil 时使用内置实现（verify.New）。
	Verifier engine.Verifier
	// MaxVerifyRepairs 是「校验→修复→重下」的循环上限，<=0 时用默认值。
	MaxVerifyRepairs int
	// BuildManifestOnSuccess 在校验通过后生成分块清单，
	// 使后续校验能精确定位损坏区间（代价是完成后多读一遍文件）。
	BuildManifestOnSuccess bool
	// PauseOnRestore 为 true 时，启动时把未完成的任务恢复为「暂停」态，
	// 等待用户手动继续。零值 false 表示自动继续（默认行为）。
	PauseOnRestore bool
	// Logf 为 nil 时静默。
	Logf func(format string, args ...any)
}

func (o *Options) normalize() error {
	if o.DataDir == "" {
		return errors.New("app: 必须指定下载目录 DataDir")
	}
	if o.MetaDir == "" {
		o.MetaDir = filepath.Join(o.DataDir, ".p2pdl")
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = DefaultChunkSize
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.MaxConcurrentTasks <= 0 {
		o.MaxConcurrentTasks = DefaultMaxConcurrentTasks
	}
	if o.BT.DataDir == "" {
		o.BT.DataDir = o.DataDir
	}
	if o.BT.Logf == nil {
		o.BT.Logf = o.Logf
	}
	return nil
}

func (o *Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// App 是应用内核：持有元数据存储、调度器、BT 客户端与全局限速器。
type App struct {
	opts Options

	store     *store.FileStore
	scheduler *engine.Scheduler
	btEngine  *bittorrent.Engine
	global    *limiter.Limiter

	mu        sync.Mutex
	btHandles map[string]*bittorrent.Torrent // taskID -> torrent
}

// New 创建应用内核。此时不会启动调度循环。
func New(opts Options) (*App, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("app: 创建下载目录失败: %w", err)
	}

	fs, err := store.NewFileStore(opts.MetaDir)
	if err != nil {
		return nil, err
	}

	btEngine, err := bittorrent.NewEngine(opts.BT)
	if err != nil {
		return nil, err
	}

	a := &App{
		opts:      opts,
		store:     fs,
		btEngine:  btEngine,
		btHandles: make(map[string]*bittorrent.Torrent),
	}
	if opts.GlobalRateLimit > 0 {
		a.global = limiter.NewLimiter(opts.GlobalRateLimit)
	}
	verifier := opts.Verifier
	if verifier == nil {
		verifier = a.defaultVerifier()
	}

	scheduler, err := engine.NewScheduler(engine.SchedulerOptions{
		Store:              fs,
		Factory:            &sessionFactory{app: a},
		Verifier:           verifier,
		MaxConcurrentTasks: opts.MaxConcurrentTasks,
		RunOptions: engine.RunOptions{
			Concurrency:    opts.Concurrency,
			RateLimit:      opts.RateLimit,
			GlobalLimiter:  a.global,
			MaxMemory:      opts.MaxMemory,
			SkipSpaceCheck: opts.SkipSpaceCheck,
			Logf:           opts.Logf,
		},
	})
	if err != nil {
		_ = btEngine.Close()
		return nil, err
	}
	a.scheduler = scheduler
	return a, nil
}

// Store 暴露元数据存储（控制面需要）。
func (a *App) Store() *store.FileStore { return a.store }

// Scheduler 暴露调度器。
func (a *App) Scheduler() *engine.Scheduler { return a.scheduler }

// Bittorrent 暴露 BT 引擎。
func (a *App) Bittorrent() *bittorrent.Engine { return a.btEngine }

// Options 返回生效的配置。
func (a *App) Options() Options { return a.opts }

// GlobalRateLimit 返回当前全局限速（字节/秒），0 表示不限速。
func (a *App) GlobalRateLimit() int64 {
	if a.global == nil {
		return 0
	}
	return a.global.Limit()
}

// SetGlobalRateLimit 动态调整全局限速（字节/秒），0 表示不限速。
//
// 这是少数可以在运行期安全变更的设置：它只影响令牌桶，不改变已存在任务的
// 数据布局或文件位置。
func (a *App) SetGlobalRateLimit(bytesPerSecond int64) {
	if bytesPerSecond < 0 {
		bytesPerSecond = 0
	}
	if a.global == nil {
		if bytesPerSecond == 0 {
			return
		}
		a.global = limiter.NewLimiter(bytesPerSecond)
		a.scheduler.SetGlobalLimiter(a.global)
		return
	}
	a.global.SetLimit(bytesPerSecond)
}

// Start 从磁盘恢复任务队列并启动调度循环。
func (a *App) Start(ctx context.Context) error {
	tasks, corrupt, err := a.store.LoadAll()
	if err != nil {
		return fmt.Errorf("app: 恢复任务失败: %w", err)
	}
	for id, err := range corrupt {
		// 损坏元数据不能带入调度：隔离现场后放弃该任务，其余任务照常运行。
		a.opts.logf("app: 任务 %s 元数据损坏（%v），已隔离", id, err)
		if _, quarantineErr := a.store.Quarantine(id); quarantineErr != nil {
			a.opts.logf("app: 隔离任务 %s 失败: %v", id, quarantineErr)
		}
	}
	if len(tasks) > 0 {
		a.opts.logf("app: 从磁盘恢复 %d 个任务", len(tasks))
	}
	if a.opts.PauseOnRestore {
		// 在交给调度器之前把待调度任务改为暂停，Restore 会跳过 paused 任务。
		paused := 0
		for _, t := range tasks {
			if t.State == engine.StatePending || t.State == engine.StateRunning || t.State == engine.StateVerifying {
				t.State = engine.StatePaused
				paused++
			}
		}
		if paused > 0 {
			a.opts.logf("app: 已按设置把 %d 个未完成任务恢复为暂停状态", paused)
		}
	}
	if err := a.scheduler.Restore(tasks); err != nil {
		return err
	}
	a.scheduler.Start(ctx)
	return nil
}

// Stop 停止调度并释放 BT 客户端。
func (a *App) Stop() error {
	a.scheduler.Stop()
	return a.btEngine.Close()
}

// AddOptions 是新增任务的公共参数。
type AddOptions struct {
	// Name 覆盖从 URL/种子推断出的文件名。
	Name string
	// DestPath 覆盖最终保存路径。
	DestPath string
	// Header 是 HTTP 请求头（Cookie / Referer / 鉴权）。
	Header map[string]string
	// ExpectedHash 是可选的期望摘要（十六进制）。提供后下载完成会做完整性校验。
	ExpectedHash string
	// HashAlgorithm 是摘要算法，空值视为 sha256。
	HashAlgorithm string
	// Peers 是 BT 静态 peer（host:port）。
	Peers []string
	// Paused 为 true 时任务只入队不启动。
	Paused bool
}

// NewID 生成任务 ID：时间戳 + 随机后缀，保证可排序且不冲突。
func NewID() string {
	var buf [6]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102T150405"), hex.EncodeToString(buf[:]))
}

// AddHTTP 探测 URL 并创建一个 HTTP 多段下载任务。
func (a *App) AddHTTP(ctx context.Context, rawURL string, opts AddOptions) (*engine.Task, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, errors.New("app: 下载地址为空")
	}
	header := httpHeader(opts.Header)
	probe, err := httpdl.Probe(ctx, nil, rawURL, header)
	if err != nil {
		return nil, err
	}

	name := firstNonEmpty(opts.Name, probe.Filename, defaultUnknownNameFallback)
	dest := opts.DestPath
	if dest == "" {
		dest = filepath.Join(a.opts.DataDir, name)
	}

	task, err := engine.NewTask(NewID(), engine.SourceHTTP, rawURL, dest, sink.TempPathFor(dest),
		probe.TotalSize, a.opts.ChunkSize, a.opts.Concurrency)
	if err != nil {
		return nil, err
	}
	task.DisplayName = filepath.Base(dest)
	if opts.ExpectedHash != "" {
		algo := opts.HashAlgorithm
		if algo == "" {
			algo = string(verify.AlgorithmSHA256)
		}
		task.Verify = engine.VerifySpec{Algorithm: algo, Value: opts.ExpectedHash}.Normalize()
		if err := task.Verify.Validate(); err != nil {
			return nil, err
		}
	}
	if probe.ETag != "" || probe.LastModified != "" {
		// 远端指纹写入 LastErr 之外的字段没有位置，这里通过日志留痕；
		// T9 会在恢复前比对指纹以判断是否需要重下。
		a.opts.logf("app: 远端指纹 etag=%q lastModified=%q", probe.ETag, probe.LastModified)
	}
	if opts.Paused {
		task.State = engine.StatePaused
	}
	if err := a.scheduler.Add(task); err != nil {
		return nil, err
	}
	return task, nil
}

// AddMagnet 添加磁力链任务。会先等待元数据就绪以确定文件大小与名称。
func (a *App) AddMagnet(ctx context.Context, magnet string, opts AddOptions) (*engine.Task, error) {
	if !strings.HasPrefix(magnet, "magnet:") {
		return nil, errors.New("app: 不是合法的磁力链")
	}
	return a.addTorrent(ctx, bittorrent.AddSpec{Magnet: magnet, PeerAddrs: opts.Peers}, magnet, opts)
}

// AddTorrentFile 添加 .torrent 文件任务。
func (a *App) AddTorrentFile(ctx context.Context, path string, opts AddOptions) (*engine.Task, error) {
	return a.addTorrent(ctx, bittorrent.AddSpec{TorrentFile: path, PeerAddrs: opts.Peers}, "file:"+path, opts)
}

// AddTorrentBytes 添加 .torrent 内容任务。
func (a *App) AddTorrentBytes(ctx context.Context, data []byte, opts AddOptions) (*engine.Task, error) {
	return a.addTorrent(ctx, bittorrent.AddSpec{TorrentBytes: data, PeerAddrs: opts.Peers}, fmt.Sprintf("torrent:%d bytes", len(data)), opts)
}

func (a *App) addTorrent(ctx context.Context, spec bittorrent.AddSpec, source string, opts AddOptions) (*engine.Task, error) {
	handle, err := a.btEngine.Add(spec)
	if err != nil {
		return nil, err
	}
	// 磁力链在拿到元数据前无法知道大小与文件名，因此这里必须等待。
	if err := handle.WaitInfo(ctx); err != nil {
		return nil, err
	}
	snap := handle.Snapshot()

	name := firstNonEmpty(opts.Name, snap.Name, defaultUnknownNameFallback)
	dest := opts.DestPath
	if dest == "" {
		dest = filepath.Join(a.opts.DataDir, name)
	}

	task, err := engine.NewTask(NewID(), engine.SourceBitTorrent, source, dest,
		a.opts.DataDir, snap.TotalBytes, a.opts.ChunkSize, 0)
	if err != nil {
		return nil, err
	}
	task.DisplayName = name
	if err := handle.ApplyToTask(task); err != nil {
		return nil, err
	}
	if opts.Paused {
		task.State = engine.StatePaused
		handle.Pause()
	}

	a.mu.Lock()
	a.btHandles[task.ID] = handle
	a.mu.Unlock()

	if err := a.scheduler.Add(task); err != nil {
		return nil, err
	}
	return task, nil
}

// defaultVerifier 构造内置校验器，并注入 BT 引擎的 piece 重校验钩子。
func (a *App) defaultVerifier() engine.Verifier {
	return verify.New(verify.Options{
		BuildManifestOnSuccess: a.opts.BuildManifestOnSuccess,
		MaxRepairAttempts:      a.opts.MaxVerifyRepairs,
		Logf:                   a.opts.Logf,
		Recheck: func(_ context.Context, task *engine.Task) error {
			a.mu.Lock()
			handle := a.btHandles[task.ID]
			a.mu.Unlock()
			if handle == nil {
				return nil
			}
			// VerifyData 让 BT 库重新校验磁盘上的 piece，并把损坏的 piece 标记为缺失，
			// 之后调度重新运行时会自动重下这些 piece。
			handle.Underlying().VerifyData()
			return nil
		},
	})
}

// btHandle 返回任务对应的 BT handle，必要时按 infoHash 重新加入。
//
// 恢复流程需要它：进程重启后 BT 引擎是空的，必须重新 Add 才能继续做种/下载；
// BT 库会从数据目录中的 piece completion 记录恢复已完成 piece，因此不会重下。
func (a *App) btHandle(ctx context.Context, task *engine.Task, spec bittorrent.AddSpec) (*bittorrent.Torrent, error) {
	a.mu.Lock()
	if h, ok := a.btHandles[task.ID]; ok {
		a.mu.Unlock()
		return h, nil
	}
	a.mu.Unlock()

	handle, err := a.btEngine.Add(spec)
	if err != nil {
		return nil, err
	}
	if err := handle.WaitInfo(ctx); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.btHandles[task.ID] = handle
	a.mu.Unlock()
	return handle, nil
}

// Recheck 重新校验任务。
//
// 流程：重新排队 → 引擎确认数据已在位置（无缺口时立即返回）→ 校验器比对摘要 →
// 若摘要不符且有分块清单，则精确定位受损块、只重下这些块。
// 这是「下载完之后才发现文件坏了」场景的入口。
func (a *App) Recheck(ctx context.Context, id string) error {
	// BT 任务需要真正核对磁盘数据：BT 库的 piece 校验是唯一的权威来源。
	//
	// 注意 VerifyData 是异步的（库在后台重新哈希文件），因此这里要等一小段
	// 时间让它把结果反映到 piece 状态上，再把真实进度写回任务；
	// 否则会出现「点了校验但什么都没发生」的假象。
	if task, ok := a.scheduler.Task(id); ok && task.Kind == engine.SourceBitTorrent {
		a.mu.Lock()
		handle := a.btHandles[id]
		a.mu.Unlock()
		if handle != nil {
			handle.Underlying().VerifyData()
			a.opts.logf("app: 任务 %s 已触发 BT 磁盘重校验", id)

			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if err := ctx.Err(); err != nil {
					break
				}
				if handle.Underlying().BytesMissing() > 0 {
					a.opts.logf("app: 任务 %s 重校验发现缺失数据，将重新下载", id)
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if err := handle.ApplyToTask(task); err != nil {
				a.opts.logf("app: 同步任务 %s 的校验结果失败: %v", id, err)
			}
		}
	}
	return a.scheduler.Requeue(id)
}

// Remove 移除任务。deleteFiles 为 true 时同时删除已下载的数据。
func (a *App) Remove(ctx context.Context, id string, deleteFiles bool) error {
	task, ok := a.schedulerTask(id)
	if !ok {
		return fmt.Errorf("app: 未找到任务 %s", id)
	}

	if err := a.scheduler.Remove(id); err != nil {
		return err
	}

	a.mu.Lock()
	handle := a.btHandles[id]
	delete(a.btHandles, id)
	a.mu.Unlock()
	if handle != nil {
		_ = a.btEngine.Drop(handle.InfoHash())
	}

	if err := a.store.Delete(id); err != nil {
		a.opts.logf("app: 删除任务元数据失败: %v", err)
	}
	if deleteFiles {
		if err := removeData(task); err != nil {
			return err
		}
	}
	return nil
}

// removeData 删除任务产生的数据：HTTP 的临时/目标文件，BT 的目标路径。
//
// 注意：BT 库可能仍持有文件句柄（见 docs/bittorrent.md §3.3），
// 此时删除会失败并如实返回错误，由上层提示用户。
func removeData(task *engine.Task) error {
	var errs []error
	if task.TempPath != "" {
		if err := os.Remove(task.TempPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("删除临时文件 %s 失败: %w", task.TempPath, err))
		}
	}
	if task.DestPath != "" {
		if err := os.Remove(task.DestPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("删除文件 %s 失败: %w", task.DestPath, err))
		}
		// 一并清理分块清单，避免残留的清单把「文件已不存在」误判成损坏。
		if err := os.Remove(verify.DefaultManifestPath(task.DestPath)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("删除清单文件失败: %w", err))
		}
	}
	return errors.Join(errs...)
}

// schedulerTask 返回调度器内部的完整任务对象。
//
// 曾经的实现是从 List() 的视图里拼一个空壳 Task，只填了 ID 与 DestPath——
// 结果「删除任务并清理文件」永远不会删掉临时文件（TempPath 为空），
// 用户的 GB 级临时数据会静默残留。必须取内部对象而不是视图。
func (a *App) schedulerTask(id string) (*engine.Task, bool) {
	return a.scheduler.Task(id)
}

func httpHeader(in map[string]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[k] = []string{v}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
