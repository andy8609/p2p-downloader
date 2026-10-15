package bittorrent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/anacrolix/torrent"

	"p2pdl/internal/engine"
)

// progressPollInterval 是等待下载推进的轮询间隔。
//
// BT 库没有提供「下载完成」的单一事件（piece 完成是分散的），
// 且我们还需要定期把进度写回 Task 供 GUI 展示，因此统一用轮询。
// 500ms 对展示足够平滑，对 CPU 也几乎无感。
const progressPollInterval = 500 * time.Millisecond

// rateWindow 是速率差分的最小时间窗口，避免高频快照导致速率抖动。
const rateWindow = 500 * time.Millisecond

// Torrent 是对 anacrolix/torrent.Torrent 的受管理封装。
type Torrent struct {
	engine *Engine
	t      *torrent.Torrent

	// rateMu 保护速率差分状态。BT 库只提供累计字节数，
	// 瞬时速率必须由调用方差分计算，因此在这里维护上一采样点。
	rateMu       sync.Mutex
	lastSampleAt time.Time
	lastRead     int64
	lastWritten  int64
	downRate     float64
	upRate       float64
}

// Underlying 暴露底层 torrent 对象。
func (h *Torrent) Underlying() *torrent.Torrent { return h.t }

// InfoHash 返回十六进制 infoHash。
func (h *Torrent) InfoHash() string { return h.t.InfoHash().HexString() }

// MagnetURI 返回该 torrent 的磁力链，便于分享或在别处复用。
func (h *Torrent) MagnetURI() string { return h.t.Metainfo().Magnet(nil, nil).String() }

// Name 返回种子名称（元数据未就绪时可能为空）。
func (h *Torrent) Name() string { return h.t.Name() }

// WaitInfo 等待元数据就绪。磁力链必须经过这一步才能知道文件列表与大小。
func (h *Torrent) WaitInfo(ctx context.Context) error {
	select {
	case <-h.t.GotInfo():
		return nil
	case <-ctx.Done():
		return fmt.Errorf("等待 BT 元数据超时/取消: %w", ctx.Err())
	}
}

// Pause 暂停数据传输（保留连接与已完成 piece，不占用下载带宽）。
func (h *Torrent) Pause() {
	h.t.DisallowDataDownload()
	h.engine.setPaused(h.InfoHash(), true)
}

// Resume 恢复数据传输。
func (h *Torrent) Resume() {
	h.t.AllowDataDownload()
	h.engine.setPaused(h.InfoHash(), false)
}

// Paused 返回是否处于暂停状态。BT 库未提供查询 API，由本包记录用户意图。
func (h *Torrent) Paused() bool {
	return h.engine.isPaused(h.InfoHash())
}

// Download 开始下载并阻塞直到完成、被取消或出错。
//
// 返回 context.Canceled 时，已完成的数据与 piece 状态都保留在磁盘与 BT 库的
// completion 存储中，再次调用会从断点继续（不会重新下载已完成的 piece）。
func (h *Torrent) Download(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := h.WaitInfo(ctx); err != nil {
		return err
	}
	h.Resume()
	h.t.DownloadAll()

	ticker := time.NewTicker(progressPollInterval)
	defer ticker.Stop()

	for {
		if h.Complete() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-h.engine.client.Closed():
			return errors.New("bittorrent: 客户端已关闭")
		case <-ticker.C:
		}
	}
}

// Complete 表示是否所有 piece 都已通过校验。
func (h *Torrent) Complete() bool {
	if h.t.Info() == nil {
		return false
	}
	return h.t.BytesMissing() == 0
}

// Snapshot 是 BT 任务的瞬时状态快照。
type Snapshot struct {
	InfoHash       string
	Name           string
	TotalBytes     int64
	CompletedBytes int64
	MissingBytes   int64
	PieceLength    int64
	PieceCount     int
	PiecesDone     int
	Complete       bool
	Seeding        bool
	ActivePeers    int
	TotalPeers     int
	ConnectedSeeds int
	DownloadRate   float64
	UploadRate     float64
	BytesRead      int64
	BytesWritten   int64
	Bitmap         *engine.Bitmap
	Files          []FileEntry
}

// FileEntry 描述种子中的一个文件。
type FileEntry struct {
	Path      string
	Length    int64
	Completed int64
}

// Snapshot 采集当前状态，包括逐 piece 的完成位图与瞬时速率。
//
// piece 位图通过 PieceState(i).Complete 逐块查询：对 10 万 piece 的巨型种子
// 这是一次 O(n) 的加锁遍历（每次 <100ns），在 500ms 的轮询频率下可以接受。
func (h *Torrent) Snapshot() Snapshot {
	stats := h.t.Stats()

	snap := Snapshot{
		InfoHash:       h.InfoHash(),
		Name:           h.t.Name(),
		TotalBytes:     h.t.Length(),
		CompletedBytes: h.t.BytesCompleted(),
		MissingBytes:   h.t.BytesMissing(),
		Seeding:        h.t.Seeding(),
		ActivePeers:    stats.ActivePeers,
		TotalPeers:     stats.TotalPeers,
		ConnectedSeeds: stats.ConnectedSeeders,
		BytesRead:      stats.BytesReadData.Int64(),
		BytesWritten:   stats.BytesWrittenData.Int64(),
	}
	snap.Complete = snap.TotalBytes > 0 && snap.MissingBytes == 0
	snap.DownloadRate, snap.UploadRate = h.updateRates(snap.BytesRead, snap.BytesWritten)

	if info := h.t.Info(); info != nil {
		snap.PieceLength = info.PieceLength
		if name := info.BestName(); name != "" {
			snap.Name = name
		}
	}

	numPieces := int(h.t.NumPieces())
	snap.PieceCount = numPieces
	bitmap := engine.NewBitmap(uint64(numPieces))
	for i := 0; i < numPieces; i++ {
		if h.t.PieceState(i).Complete {
			_ = bitmap.Set(uint64(i))
		}
	}
	snap.Bitmap = bitmap
	snap.PiecesDone = int(bitmap.Count())

	for _, f := range h.t.Files() {
		snap.Files = append(snap.Files, FileEntry{
			Path:      f.Path(),
			Length:    f.Length(),
			Completed: f.BytesCompleted(),
		})
	}
	return snap
}

// updateRates 用累计字节数差分出瞬时速率。窗口不足时沿用上一次的结果。
func (h *Torrent) updateRates(read, written int64) (down, up float64) {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()

	now := time.Now()
	if h.lastSampleAt.IsZero() {
		h.lastSampleAt = now
		h.lastRead = read
		h.lastWritten = written
		return 0, 0
	}
	elapsed := now.Sub(h.lastSampleAt)
	if elapsed < rateWindow {
		return h.downRate, h.upRate
	}
	seconds := elapsed.Seconds()
	h.downRate = float64(read-h.lastRead) / seconds
	h.upRate = float64(written-h.lastWritten) / seconds
	if h.downRate < 0 {
		h.downRate = 0
	}
	if h.upRate < 0 {
		h.upRate = 0
	}
	h.lastSampleAt = now
	h.lastRead = read
	h.lastWritten = written
	return h.downRate, h.upRate
}

// ApplyToTask 把当前状态写入统一任务模型，使 BT 与 HTTP 任务在上层表现一致。
func (h *Torrent) ApplyToTask(task *engine.Task) error {
	if task == nil {
		return errors.New("bittorrent: 任务为空")
	}
	snap := h.Snapshot()

	task.Kind = engine.SourceBitTorrent
	task.InfoHash = snap.InfoHash
	task.PieceLength = snap.PieceLength
	task.PieceBitmap = snap.Bitmap
	if snap.TotalBytes > 0 {
		task.TotalSize = snap.TotalBytes
	}
	if snap.Name != "" {
		task.DisplayName = snap.Name
		task.DestPath = h.engine.resolvedPath(snap.Name)
	}
	if snap.Complete && task.State != engine.StateCompleted {
		task.State = engine.StateCompleted
	}
	task.Touch()
	return task.Validate()
}

// resolvedPath 返回 BT 数据的落地位置：单文件种子是文件路径，多文件种子是目录。
func (e *Engine) resolvedPath(name string) string {
	if e.cfg.DataDir == "" {
		return name
	}
	return filepath.Join(e.cfg.DataDir, name)
}

// setPaused / isPaused 维护暂停意图（BT 库未提供查询 API）。
func (e *Engine) setPaused(infoHash string, paused bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.paused == nil {
		e.paused = make(map[string]bool)
	}
	if paused {
		e.paused[infoHash] = true
		return
	}
	delete(e.paused, infoHash)
}

func (e *Engine) isPaused(infoHash string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.paused[infoHash]
}
