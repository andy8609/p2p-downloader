package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"p2pdl/internal/engine"
	"p2pdl/internal/engine/bittorrent"
	"p2pdl/internal/engine/httpdl"
	"p2pdl/internal/engine/sink"
)

// sessionFactory 是 engine.SessionFactory 的真实实现：按任务类型选择引擎。
type sessionFactory struct {
	app *App
}

// Create 实现 engine.SessionFactory。
func (f *sessionFactory) Create(ctx context.Context, task *engine.Task, opts engine.RunOptions) (engine.Session, error) {
	switch task.Kind {
	case engine.SourceHTTP:
		return f.newHTTPSession(ctx, task, opts)
	case engine.SourceBitTorrent:
		return f.newBitTorrentSession(ctx, task, opts)
	default:
		return nil, fmt.Errorf("app: 不支持的任务类型 %q", task.Kind)
	}
}

// ---------------------------------------------------------------------------
// HTTP 会话
// ---------------------------------------------------------------------------

// httpSession 把 httpdl.Downloader 适配为 engine.Session。
//
// 生命周期细节：
//   - 打开时复用已存在的临时文件（断点续传的落地点在 sink.Open）；
//   - 若最终文件已完整存在，则改为「就地修补」（见 chooseWriteTarget）；
//   - 关闭时：数据完整且长度吻和则 Commit（原子重命名为最终文件），
//     否则只 Close 保留临时文件；
//   - 进度由下载器直接写入同一个 Task 指针，因此 Sync 是空操作。
type httpSession struct {
	task       *engine.Task
	sink       *sink.FileSink
	downloader *httpdl.Downloader
	inPlace    bool
}

// chooseWriteTarget 决定本次会话把数据写到哪里。
//
// 关键判断：最终文件已存在且长度与任务总长一致时，说明上一层已提交过完整数据，
// 此时必须就地写入——否则「只重下受损分块」的结果会被整文件重命名覆盖成
// 「仅受损块正确、其余全为零」的废文件。
func chooseWriteTarget(task *engine.Task) (target string, inPlace bool) {
	if task.TotalSize > 0 && task.DestPath != "" {
		if fi, err := os.Stat(task.DestPath); err == nil && fi.Mode().IsRegular() && fi.Size() == task.TotalSize {
			return task.DestPath, true
		}
	}
	return task.TempPath, false
}

func (f *sessionFactory) newHTTPSession(ctx context.Context, task *engine.Task, opts engine.RunOptions) (engine.Session, error) {
	target, inPlace := chooseWriteTarget(task)

	fs, err := sink.Open(task.DestPath, target, sink.Options{TotalSize: task.TotalSize})
	if err != nil {
		return nil, err
	}

	if !inPlace {
		// 确认位图与临时文件一致；不一致则清空进度重下，
		// 而不是在错位的偏移上继续写。
		plan, planErr := sink.PlanRecovery(task, target)
		if planErr != nil {
			_ = fs.Close()
			return nil, planErr
		}
		if plan.Decision == sink.DecisionRestart && task.Downloaded() > 0 {
			f.app.opts.logf("app: 任务 %s 的断点不可信，重新下载：%s", task.ID, plan.Reason)
			task.ResetProgress()
			if err := fs.Discard(); err != nil {
				f.app.opts.logf("app: 清理任务 %s 的临时文件失败: %v", task.ID, err)
			}
			fs, err = sink.Open(task.DestPath, target, sink.Options{TotalSize: task.TotalSize})
			if err != nil {
				return nil, err
			}
		}
	}

	saver := &sink.ProgressSaver{Store: f.app.store, Sink: fs}
	downloader, err := httpdl.New(task, fs, httpdl.Options{
		Header:         nil,
		Concurrency:    opts.Concurrency,
		RateLimit:      opts.RateLimit,
		GlobalLimiter:  opts.GlobalLimiter,
		MaxMemory:      opts.MaxMemory,
		Progress:       saver,
		SkipSpaceCheck: opts.SkipSpaceCheck,
		Logf:           opts.Logf,
	})
	if err != nil {
		_ = fs.Close()
		return nil, err
	}
	return &httpSession{task: task, sink: fs, downloader: downloader, inPlace: inPlace}, nil
}

func (s *httpSession) Run(ctx context.Context) error { return s.downloader.Run(ctx) }

// Pause 是空操作：调度器通过取消 context 暂停，httpdl 会把分片退回待下载。
func (s *httpSession) Pause(context.Context) error { return nil }

func (s *httpSession) Progress() engine.Progress {
	stats := s.downloader.Stats()
	return engine.Progress{
		Total: stats.TotalSize,
		Done:  stats.Downloaded,
		Rate:  stats.BytesPerSecond,
		Mode:  stats.Mode,
	}
}

// Sync 记录传输模式并返回：分片位图由 httpdl 直接写入同一个 Task 指针，
// 这里只需要把「实际用了哪种模式」固化下来，供任务完成后展示。
func (s *httpSession) Sync(context.Context) error {
	s.task.Mode = s.downloader.Stats().Mode
	return nil
}

// Close 释放文件句柄：数据齐全时原子提交为最终文件。
func (s *httpSession) Close() error {
	if s.inPlace {
		// 数据本来就在最终位置，Commit 只修正长度，绝不重命名。
		if s.task.Complete() {
			return s.sink.Commit(s.task.TotalSize)
		}
		return s.sink.Close()
	}

	if !s.task.Complete() {
		return s.sink.Close()
	}

	// 安全门：只有临时文件确实写满了全部字节，才允许它重命名覆盖最终文件。
	// 否则（例如上一轮已提交、本轮无缺口）会把残缺的临时文件变成最终文件。
	if size, err := s.sink.Size(); err == nil && s.task.TotalSize > 0 && size != s.task.TotalSize {
		_ = s.sink.Close()
		return fmt.Errorf("app: 任务 %s 的临时文件长度 %d 与总长 %d 不符，已放弃提交以避免覆盖现有文件",
			s.task.ID, size, s.task.TotalSize)
	}
	return s.sink.Commit(s.task.TotalSize)
}

// ---------------------------------------------------------------------------
// BitTorrent 会话
// ---------------------------------------------------------------------------

type btSession struct {
	task   *engine.Task
	handle *bittorrent.Torrent
}

func (f *sessionFactory) newBitTorrentSession(ctx context.Context, task *engine.Task, opts engine.RunOptions) (engine.Session, error) {
	// 进程重启后 BT 引擎是空的：按 infoHash/磁力链重新加入，
	// 库会从数据目录的 piece completion 记录恢复已完成 piece，因此不会重下。
	spec := bittorrent.AddSpec{
		Magnet:   task.Source,
		SavePath: filepath.Dir(task.DestPath),
	}
	if task.Source != "" && !isMagnet(task.Source) {
		spec.Magnet = ""
	}
	handle, err := f.app.btHandle(ctx, task, spec)
	if err != nil {
		return nil, err
	}
	return &btSession{task: task, handle: handle}, nil
}

func isMagnet(source string) bool {
	return len(source) > 7 && source[:7] == "magnet:"
}

func (s *btSession) Run(ctx context.Context) error { return s.handle.Download(ctx) }

func (s *btSession) Pause(context.Context) error {
	s.handle.Pause()
	return nil
}

func (s *btSession) Progress() engine.Progress {
	snap := s.handle.Snapshot()
	return engine.Progress{
		Total: snap.TotalBytes,
		Done:  snap.CompletedBytes,
		Rate:  snap.DownloadRate,
		Peers: snap.ActivePeers,
		Mode:  "bittorrent",
	}
}

// Sync 把 BT 库内部的 piece 状态写回任务，供调度器持久化。
func (s *btSession) Sync(context.Context) error {
	if err := s.handle.ApplyToTask(s.task); err != nil {
		return err
	}
	s.task.Mode = "bittorrent"
	return nil
}

// Close 停止该任务的 BT 数据请求。
//
// 必须显式 Pause：BT 库在 DownloadAll 后会持续向 peer 请求数据，
// 会话结束（暂停/重新排队）后如果不禁止，任务会在后台继续消耗带宽。
func (s *btSession) Close() error {
	if s.task.State != engine.StateCompleted {
		s.handle.Pause()
	}
	return nil
}
