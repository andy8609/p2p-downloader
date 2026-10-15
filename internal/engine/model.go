package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SizeUnknown 表示下载源未提供内容长度（HTTP 无 Content-Length 的场景）。
const SizeUnknown int64 = -1

// SourceKind 标识任务的来源协议。
type SourceKind string

const (
	// SourceHTTP 覆盖 HTTP/HTTPS（含多段并发下载）。
	SourceHTTP SourceKind = "http"
	// SourceBitTorrent 覆盖磁力链与种子文件。
	SourceBitTorrent SourceKind = "bittorrent"
)

// Valid 判断协议类型是否为已知取值。
func (k SourceKind) Valid() bool {
	return k == SourceHTTP || k == SourceBitTorrent
}

// TaskState 是任务的持久化状态。
type TaskState string

const (
	// StatePending 已入队、尚未开始。
	StatePending TaskState = "pending"
	// StateRunning 正在传输。
	StateRunning TaskState = "running"
	// StatePaused 被用户或调度器暂停，分片进度保留。
	StatePaused TaskState = "paused"
	// StateVerifying 数据已收齐，正在做完整性校验。
	StateVerifying TaskState = "verifying"
	// StateCompleted 校验通过、文件已就位。
	StateCompleted TaskState = "completed"
	// StateFailed 在重试策略耗尽后失败。
	StateFailed TaskState = "failed"
)

// Valid 判断状态是否为已知取值。
func (s TaskState) Valid() bool {
	switch s {
	case StatePending, StateRunning, StatePaused, StateVerifying, StateCompleted, StateFailed:
		return true
	}
	return false
}

// Terminal 表示任务已不会再自行推进。
func (s TaskState) Terminal() bool {
	return s == StateCompleted || s == StateFailed
}

// SegmentState 是单个下载分片的状态。
type SegmentState string

const (
	// SegmentPending 该分片尚未完成。
	SegmentPending SegmentState = "pending"
	// SegmentRunning 该分片正在写入。
	SegmentRunning SegmentState = "running"
	// SegmentCompleted 该分片的字节已全部落盘。
	SegmentCompleted SegmentState = "completed"
	// SegmentFailed 该分片在重试耗尽后失败。
	SegmentFailed SegmentState = "failed"
)

// Valid 判断分片状态是否为已知取值。
func (s SegmentState) Valid() bool {
	switch s {
	case SegmentPending, SegmentRunning, SegmentCompleted, SegmentFailed:
		return true
	}
	return false
}

// VerifySpec 描述任务的完整性校验方式。
//
// Algorithm 为 "none" 时不做校验；为 "sha256"/"md5" 时，
// Value 是期望的十六进制摘要，由调用方（或服务器响应头）提供。
type VerifySpec struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value,omitempty"`
}

// Enabled 表示是否需要在完成后执行校验。
func (v VerifySpec) Enabled() bool {
	return v.Algorithm != "" && !strings.EqualFold(v.Algorithm, "none")
}

// Normalize 返回规范化后的校验配置：算法小写、摘要去空格并转为小写。
func (v VerifySpec) Normalize() VerifySpec {
	return VerifySpec{
		Algorithm: strings.ToLower(strings.TrimSpace(v.Algorithm)),
		Value:     strings.ToLower(strings.TrimSpace(v.Value)),
	}
}

// Validate 校验配置自身是否自洽。
func (v VerifySpec) Validate() error {
	n := v.Normalize()
	switch n.Algorithm {
	case "", "none":
		return nil
	case "sha256":
		if n.Value != "" && len(n.Value) != 64 {
			return fmt.Errorf("engine: sha256 摘要长度应为 64 个十六进制字符，实际 %d", len(n.Value))
		}
	case "md5":
		if n.Value != "" && len(n.Value) != 32 {
			return fmt.Errorf("engine: md5 摘要长度应为 32 个十六进制字符，实际 %d", len(n.Value))
		}
	default:
		return fmt.Errorf("engine: 不支持的校验算法 %q", n.Algorithm)
	}
	return nil
}

// Segment 是一个下载分片（HTTP 多段模型中的一段）。
//
// 不变量：
//   - 0 <= Start < End <= Task.TotalSize
//   - Bitmap 的位数 == ceil((End-Start) / Task.ChunkSize)
//   - 位图第 i 位对应文件区间 [Start+i*ChunkSize, min(Start+(i+1)*ChunkSize, End))
//
// 「已下载多少」不额外存储字段，一律由 Bitmap 推导，避免出现两份互相矛盾的进度。
type Segment struct {
	Index    int          `json:"index"`
	Start    int64        `json:"start"`
	End      int64        `json:"end"`
	State    SegmentState `json:"state"`
	Bitmap   *Bitmap      `json:"bitmap"`
	Attempts int          `json:"attempts,omitempty"`
	LastErr  string       `json:"lastError,omitempty"`
}

// Length 返回分片覆盖的字节数。
func (s *Segment) Length() int64 {
	if s == nil || s.End <= s.Start {
		return 0
	}
	return s.End - s.Start
}

// ChunkCount 返回分片在给定分块大小下的分块数。
func (s *Segment) ChunkCount(chunkSize int64) uint64 {
	if s == nil || chunkSize <= 0 || s.Length() == 0 {
		return 0
	}
	return uint64((s.Length() + chunkSize - 1) / chunkSize)
}

// ChunkRange 返回分片内第 i 个分块对应的文件字节区间（绝对偏移）。
func (s *Segment) ChunkRange(chunkSize int64, i uint64) (start, end int64) {
	start = s.Start + int64(i)*chunkSize
	end = start + chunkSize
	if end > s.End {
		end = s.End
	}
	return start, end
}

// DownloadedBytes 返回该分片已落盘的字节数（按分块粒度统计）。
func (s *Segment) DownloadedBytes(chunkSize int64) int64 {
	if s == nil || s.Bitmap == nil || chunkSize <= 0 {
		return 0
	}
	var total int64
	for _, r := range s.Bitmap.Ranges() {
		bs := s.Start + int64(r[0])*chunkSize
		be := s.Start + int64(r[1])*chunkSize
		if be > s.End {
			be = s.End
		}
		if be > bs {
			total += be - bs
		}
	}
	return total
}

// Complete 表示分片的所有分块都已落盘。
func (s *Segment) Complete() bool {
	return s != nil && s.Bitmap != nil && s.Bitmap.Complete()
}

// Task 是一个下载任务的完整持久化状态。
//
// 该结构是「断点续传」的唯一真相来源：进程重启后由 store 读回，
// 调度器据此恢复运行状态与各分片进度，因此所有字段都必须可 JSON 往返。
type Task struct {
	ID          string     `json:"id"`
	Kind        SourceKind `json:"kind"`
	Source      string     `json:"source"`
	DisplayName string     `json:"displayName"`
	DestPath    string     `json:"destPath"`
	TempPath    string     `json:"tempPath"`
	TotalSize   int64      `json:"totalSize"` // SizeUnknown 表示未知
	ChunkSize   int64      `json:"chunkSize"`
	State       TaskState  `json:"state"`
	Verify      VerifySpec `json:"verify"`
	Segments    []*Segment `json:"segments,omitempty"`

	// BitTorrent 专用字段。
	InfoHash    string  `json:"infoHash,omitempty"`
	PieceLength int64   `json:"pieceLength,omitempty"`
	PieceBitmap *Bitmap `json:"pieceBitmap,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	LastErr   string    `json:"lastError,omitempty"`

	// Mode 是最近一次实际生效的传输模式（multi-segment / single-stream / bittorrent）。
	// 持久化它的原因：会话结束后仍要能向用户解释「这个任务是用哪种方式下的」，
	// 否则任务一完成，前端就只剩一个空模式字段。
	Mode string `json:"mode,omitempty"`

	// VerifyAttempts 是已执行的「校验→修复→重下」循环次数。
	// 它是「不出现无限重试」的逗号：超过上限后任务直接失败并保留错误原因。
	VerifyAttempts int `json:"verifyAttempts,omitempty"`
	// RepairedBytes 是历次修复累计重下的字节数，用于向用户解释为何耗时变长。
	RepairedBytes int64 `json:"repairedBytes,omitempty"`
}

// NewTask 创建一个待调度的 HTTP 多段任务，并生成覆盖整个文件的等分分片。
//
// totalSize <= 0 表示大小未知：此时不切分片（Segments 为空），由下载器降级为单流模式。
// chunkSize 代表落盘位图的粒度，与并发段数无关。
func NewTask(id string, kind SourceKind, source, destPath, tempPath string, totalSize, chunkSize int64, segmentCount int) (*Task, error) {
	if id == "" {
		return nil, errors.New("engine: 任务 ID 不能为空")
	}
	if !kind.Valid() {
		return nil, fmt.Errorf("engine: 未知的下载协议 %q", kind)
	}
	if source == "" {
		return nil, errors.New("engine: 下载源不能为空")
	}
	if chunkSize <= 0 {
		return nil, fmt.Errorf("engine: 分块大小必须为正数，实际 %d", chunkSize)
	}

	now := time.Now().UTC()
	t := &Task{
		ID:        id,
		Kind:      kind,
		Source:    source,
		DestPath:  destPath,
		TempPath:  tempPath,
		TotalSize: totalSize,
		ChunkSize: chunkSize,
		State:     StatePending,
		Verify:    VerifySpec{Algorithm: "none"},
		CreatedAt: now,
		UpdatedAt: now,
	}
	// 仅对 HTTP 任务切分下载分片：BitTorrent 任务的进度以 piece 位图表达，
	// 且 infoHash 由上层在解析磁力链/种子后补齐，构造时不需要也无法校验。
	if kind == SourceHTTP && totalSize > 0 && segmentCount > 0 {
		if err := t.Split(segmentCount); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// Split 按给定段数把整个文件切分为等长分片，并重置分片进度。
//
// 已下载的分片不会被保留：Split 只用于任务创建或下载源发生变化（大小/ETag 变化）后的重新规划。
func (t *Task) Split(segmentCount int) error {
	if t.TotalSize <= 0 {
		return errors.New("engine: 文件大小未知，无法切分分片")
	}
	if segmentCount <= 0 {
		return fmt.Errorf("engine: 分片数必须为正数，实际 %d", segmentCount)
	}
	if int64(segmentCount) > t.TotalSize {
		segmentCount = int(t.TotalSize)
	}
	if segmentCount == 0 {
		segmentCount = 1
	}

	base := t.TotalSize / int64(segmentCount)
	remainder := t.TotalSize % int64(segmentCount)

	t.Segments = make([]*Segment, 0, segmentCount)
	var offset int64
	for i := 0; i < segmentCount; i++ {
		size := base
		if int64(i) < remainder {
			size++
		}
		if size == 0 {
			continue
		}
		seg := &Segment{
			Index:  i,
			Start:  offset,
			End:    offset + size,
			State:  SegmentPending,
			Bitmap: NewBitmap(uint64((size + t.ChunkSize - 1) / t.ChunkSize)),
		}
		t.Segments = append(t.Segments, seg)
		offset += size
	}
	t.Touch()
	return t.Validate()
}

// Touch 刷新 UpdatedAt。
func (t *Task) Touch() {
	t.UpdatedAt = time.Now().UTC()
}

// Validate 检查任务结构是否自洽。任何不满足不变量的任务都应被视为损坏元数据，
// 调用方据此降级为「重新下载」而不是继续使用脏状态。
func (t *Task) Validate() error {
	if t.ID == "" {
		return errors.New("engine: 任务 ID 为空")
	}
	if !t.Kind.Valid() {
		return fmt.Errorf("engine: 未知的下载协议 %q", t.Kind)
	}
	if t.Source == "" {
		return errors.New("engine: 下载源为空")
	}
	if !t.State.Valid() {
		return fmt.Errorf("engine: 未知的任务状态 %q", t.State)
	}
	if err := t.Verify.Validate(); err != nil {
		return err
	}
	if t.ChunkSize <= 0 {
		return fmt.Errorf("engine: 分块大小必须为正数，实际 %d", t.ChunkSize)
	}
	if t.TotalSize < SizeUnknown {
		return fmt.Errorf("engine: 非法文件大小 %d", t.TotalSize)
	}

	var covered int64
	for i, s := range t.Segments {
		if s == nil {
			return fmt.Errorf("engine: 第 %d 个分片为空", i)
		}
		if s.Index != i {
			return fmt.Errorf("engine: 分片下标错位，期望 %d 实际 %d", i, s.Index)
		}
		if s.Start != covered {
			return fmt.Errorf("engine: 分片 %d 起点 %d 与前一分片终点 %d 不连续", s.Index, s.Start, covered)
		}
		if s.End <= s.Start {
			return fmt.Errorf("engine: 分片 %d 的区间 [%d,%d) 非法", s.Index, s.Start, s.End)
		}
		if !s.State.Valid() {
			return fmt.Errorf("engine: 分片 %d 的状态 %q 未知", s.Index, s.State)
		}
		wantChunks := (s.Length() + t.ChunkSize - 1) / t.ChunkSize
		if s.Bitmap == nil {
			return fmt.Errorf("engine: 分片 %d 缺少位图", s.Index)
		}
		if s.Bitmap.Len() != uint64(wantChunks) {
			return fmt.Errorf("engine: 分片 %d 位图位数 %d 与期望 %d 不符",
				s.Index, s.Bitmap.Len(), wantChunks)
		}
		covered = s.End
	}
	if len(t.Segments) > 0 && t.TotalSize > 0 && covered != t.TotalSize {
		return fmt.Errorf("engine: 分片仅覆盖 %d 字节，与文件大小 %d 不一致", covered, t.TotalSize)
	}
	if t.Kind == SourceBitTorrent {
		if t.InfoHash == "" {
			return errors.New("engine: BitTorrent 任务缺少 infoHash")
		}
	}
	return nil
}

// Downloaded 返回整个任务已落盘的字节数。
func (t *Task) Downloaded() int64 {
	if t == nil {
		return 0
	}
	if t.Kind == SourceBitTorrent {
		if t.PieceBitmap == nil || t.PieceLength <= 0 {
			return 0
		}
		done := int64(t.PieceBitmap.Count()) * t.PieceLength
		if t.TotalSize > 0 && done > t.TotalSize {
			done = t.TotalSize
		}
		return done
	}
	var total int64
	for _, s := range t.Segments {
		total += s.DownloadedBytes(t.ChunkSize)
	}
	return total
}

// Progress 返回完成比例（0~1）。大小未知时返回 0。
func (t *Task) Progress() float64 {
	if t == nil || t.TotalSize <= 0 {
		return 0
	}
	ratio := float64(t.Downloaded()) / float64(t.TotalSize)
	if ratio > 1 {
		return 1
	}
	return ratio
}

// Complete 表示数据是否已全部落盘（尚未经过校验）。
func (t *Task) Complete() bool {
	if t == nil {
		return false
	}
	if t.Kind == SourceBitTorrent {
		return t.PieceBitmap != nil && t.PieceBitmap.Complete()
	}
	if len(t.Segments) == 0 {
		return false
	}
	for _, s := range t.Segments {
		if !s.Complete() {
			return false
		}
	}
	return true
}

// ResetProgress 清空所有分片进度，使任务回到「从头下载」状态。
//
// 仅在已确认磁盘数据不可信（元数据与文件不一致、清单大小不符）时调用：
// 它是「宁可重下，不可续错」原则的执行手段。
func (t *Task) ResetProgress() {
	if t == nil {
		return
	}
	for _, seg := range t.Segments {
		if seg == nil {
			continue
		}
		chunks := (seg.Length() + t.ChunkSize - 1) / t.ChunkSize
		if chunks < 0 {
			chunks = 0
		}
		seg.Bitmap = NewBitmap(uint64(chunks))
		seg.State = SegmentPending
		seg.Attempts = 0
		seg.LastErr = ""
	}
	t.Touch()
}

// MarkChunksMissing 把指定文件区间对应的分块标记为「未完成」，
// 使其在下次调度时被重新下载。这正是「定点重下」的实现方式。
// 返回受影响的字节数与分块数。
func (t *Task) MarkChunksMissing(ranges []ByteRange) (int64, int) {
	if t == nil || len(ranges) == 0 || t.ChunkSize <= 0 {
		return 0, 0
	}
	var bytes int64
	var chunks int
	for _, seg := range t.Segments {
		if seg == nil || seg.Bitmap == nil {
			continue
		}
		for _, r := range ranges {
			start := max64(r.Start, seg.Start)
			end := min64(r.End, seg.End)
			if start >= end {
				continue
			}
			firstIdx := uint64((start - seg.Start) / t.ChunkSize)
			lastIdx := uint64((end - 1 - seg.Start) / t.ChunkSize)
			for idx := firstIdx; idx <= lastIdx && idx < seg.Bitmap.Len(); idx++ {
				if seg.Bitmap.Get(idx) {
					_ = seg.Bitmap.Clear(idx)
					chunkStart, chunkEnd := seg.ChunkRange(t.ChunkSize, idx)
					bytes += chunkEnd - chunkStart
					chunks++
				}
			}
		}
		if !seg.Complete() {
			seg.State = SegmentPending
		}
	}
	t.Touch()
	return bytes, chunks
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
// PendingSegments 返回尚未完成的分片，供调度器恢复下载时使用。
func (t *Task) PendingSegments() []*Segment {
	if t == nil {
		return nil
	}
	var out []*Segment
	for _, s := range t.Segments {
		if !s.Complete() {
			out = append(out, s)
		}
	}
	return out
}

// RequeueInterrupted 把重启前处于运行态的分片回退为待下载，
// 使进程被强杀后的元数据恢复到「可继续调度」的一致状态。
//
// 任务级状态回到 pending 而不是 paused：强杀（断电、结束进程）并非用户主动暂停，
// 重启后应当自动继续；而用户主动暂停的任务状态本来就是 paused，不经过这里。
// 返回被回退的分片数量。
func (t *Task) RequeueInterrupted() int {
	if t == nil {
		return 0
	}
	n := 0
	for _, s := range t.Segments {
		if s.State == SegmentRunning {
			s.State = SegmentPending
			n++
		}
	}
	if t.State == StateRunning || t.State == StateVerifying {
		t.State = StatePending
	}
	t.Touch()
	return n
}
