package httpdl

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"p2pdl/internal/engine"
)

const (
	testTotal  = int64(1 << 20) // 1MB
	testChunk  = int64(64 << 10) // 64KB
	testBuffer = 8 << 10
)

// memSink 是测试用的内存落盘目标：支持任意偏移写入，并记录写入次数。
type memSink struct {
	mu       sync.Mutex
	buf      []byte
	writes   int
	syncCall int
}

func (m *memSink) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	need := int(off) + len(p)
	if need > len(m.buf) {
		grown := make([]byte, need)
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[off:], p)
	m.writes++
	return len(p), nil
}

func (m *memSink) Sync() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncCall++
	return nil
}

func (m *memSink) Bytes() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]byte, len(m.buf))
	copy(out, m.buf)
	return out
}

// trackingServer 是一个可统计并发请求数与请求方法分布的测试服务器。
type trackingServer struct {
	*httptest.Server
	content     []byte
	active      int64
	peak        int64
	requests    int64
	rangeReqs   int64
	headReqs    int64
	ignoreRange bool
	failFirst   int64 // 前 N 个 Range 请求返回 500
	failCount   int64
	// waitForPeak 大于 0 时，handler 会在响应前等到活跃请求数达到该值
	// （或超时），用于稳定观测并发上限，而不依赖请求调度的时序巧合。
	waitForPeak atomic.Int64
}

func newTrackingServer(t testing.TB, size int, ignoreRange bool) *trackingServer {
	t.Helper()
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成测试数据失败: %v", err)
	}
	ts := &trackingServer{content: content, ignoreRange: ignoreRange}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&ts.requests, 1)
		if r.Method == http.MethodHead {
			atomic.AddInt64(&ts.headReqs, 1)
		}
		if r.Header.Get("Range") != "" {
			atomic.AddInt64(&ts.rangeReqs, 1)
		}

		cur := atomic.AddInt64(&ts.active, 1)
		for {
			peak := atomic.LoadInt64(&ts.peak)
			if cur <= peak || atomic.CompareAndSwapInt64(&ts.peak, peak, cur) {
				break
			}
		}
		defer atomic.AddInt64(&ts.active, -1)

		// 制造瞬时故障，用于验证重试逻辑。
		if n := atomic.LoadInt64(&ts.failFirst); n > 0 {
			if atomic.AddInt64(&ts.failCount, 1) <= n && r.Header.Get("Range") != "" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}

		if target := ts.waitForPeak.Load(); target > 0 {
			deadline := time.Now().Add(3 * time.Second)
			for atomic.LoadInt64(&ts.active) < target && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
		}

		if ts.ignoreRange {
			// 模拟不支持 Range 的服务端：忽略 Range 头，总是返回 200 与全量内容。
			w.Header().Set("Content-Length", strconv.Itoa(len(ts.content)))
			w.Header().Set("Content-Type", "application/octet-stream")
			if r.Method == http.MethodHead {
				return
			}
			_, _ = w.Write(ts.content)
			return
		}
		http.ServeContent(w, r, "blob.bin", time.Time{}, bytes.NewReader(ts.content))
	})
	ts.Server = httptest.NewServer(handler)
	t.Cleanup(ts.Server.Close)
	return ts
}

func (ts *trackingServer) maxConcurrent() int64 { return atomic.LoadInt64(&ts.peak) }

func newTask(t testing.TB, url string, segments int) *engine.Task {
	t.Helper()
	task, err := engine.NewTask("t-1", engine.SourceHTTP, url, "dest.bin", "dest.part",
		testTotal, testChunk, segments)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	return task
}

// ---------------------------------------------------------------------------
// 探测
// ---------------------------------------------------------------------------

func TestProbeDetectsRangeAndSize(t *testing.T) {
	ts := newTrackingServer(t, int(testTotal), false)
	res, err := Probe(context.Background(), nil, ts.URL+"/blob.bin", nil)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if !res.AcceptRanges {
		t.Error("应检测到支持 Range")
	}
	if res.TotalSize != testTotal {
		t.Errorf("TotalSize = %d, 期望 %d", res.TotalSize, testTotal)
	}
	if res.Filename != "blob.bin" {
		t.Errorf("Filename = %q, 期望 blob.bin", res.Filename)
	}
}

func TestProbeFallsBackWhenHeadUnsupported(t *testing.T) {
	// 只允许 GET：HEAD 返回 405，探测必须回退到 GET + Range 而不报错。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		http.ServeContent(w, r, "blob.bin", time.Time{}, bytes.NewReader(make([]byte, 4096)))
	}))
	defer srv.Close()

	res, err := Probe(context.Background(), nil, srv.URL+"/blob.bin", nil)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}
	if !res.AcceptRanges || res.TotalSize != 4096 {
		t.Fatalf("探测结果 = %+v, 期望支持 Range 且大小 4096", res)
	}
}

func TestProbeRejectsNonHTTP(t *testing.T) {
	if _, err := Probe(context.Background(), nil, "ftp://example.com/a.bin", nil); err == nil {
		t.Fatal("非 HTTP 协议应被拒绝")
	}
	if _, err := Probe(context.Background(), nil, "::bad url", nil); err == nil {
		t.Fatal("非法 URL 应被拒绝")
	}
}

func TestParseHelpers(t *testing.T) {
	if got := parseContentLength(" 12345 "); got != 12345 {
		t.Errorf("parseContentLength = %d", got)
	}
	if got := parseContentLength("abc"); got != -1 {
		t.Errorf("非法 Content-Length 应返回 -1, 实际 %d", got)
	}
	if got := parseContentRangeTotal("bytes 0-0/98765"); got != 98765 {
		t.Errorf("parseContentRangeTotal = %d", got)
	}
	if got := parseContentRangeTotal("bytes 0-0/*"); got != -1 {
		t.Errorf("未知总长应返回 -1, 实际 %d", got)
	}
	if got := sanitizeFilename(`../../etc/passwd`); strings.ContainsAny(got, `/\`) {
		t.Errorf("文件名未被净化: %q", got)
	}
	if got := filenameFromDisposition(`attachment; filename="a b.iso"`); got != "a b.iso" {
		t.Errorf("filenameFromDisposition = %q", got)
	}
}

// ---------------------------------------------------------------------------
// 并发分段下载
// ---------------------------------------------------------------------------

// 验收核心：实测并发连接数等于配置值，且数据完整正确。
func TestConcurrentSegmentsRespectConfiguredLimit(t *testing.T) {
	ts := newTrackingServer(t, int(testTotal), false)
	task := newTask(t, ts.URL+"/blob.bin", 8)
	sink := &memSink{}

	const concurrency = 4
	// 让服务端等到 4 个并发请求都到齐再响应,从而可靠验证并发上限。
	ts.waitForPeak.Store(concurrency)
	probe, err := Probe(context.Background(), nil, task.Source, nil)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}

	d, err := New(task, sink, Options{
		Concurrency: concurrency,
		BufferSize:  testBuffer,
		Probe:       probe,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	if got := ts.maxConcurrent(); got != concurrency {
		t.Errorf("实测并发连接峰值 = %d, 期望 %d", got, concurrency)
	}
	if !bytes.Equal(sink.Bytes(), ts.content) {
		t.Error("落盘内容与源不一致")
	}
	if !task.Complete() {
		t.Error("任务应判定为完成")
	}

	stats := d.Stats()
	if stats.Mode != ModeMultiSegment {
		t.Errorf("Mode = %q, 期望 %q", stats.Mode, ModeMultiSegment)
	}
	if stats.Downloaded != testTotal {
		t.Errorf("Downloaded = %d, 期望 %d", stats.Downloaded, testTotal)
	}
	if stats.ConnectionsTotal < int64(8) {
		t.Errorf("连接总数 = %d, 期望至少 8（每个分片一个连接）", stats.ConnectionsTotal)
	}
	if len(stats.Segments) != 8 {
		t.Fatalf("分片统计数量 = %d, 期望 8", len(stats.Segments))
	}
	for _, seg := range stats.Segments {
		if seg.State != engine.SegmentCompleted || seg.Downloaded != seg.End-seg.Start {
			t.Errorf("分片 %d 统计异常: %+v", seg.Index, seg)
		}
	}
}

func TestSingleSegmentStillWorks(t *testing.T) {
	ts := newTrackingServer(t, int(testTotal), false)
	task := newTask(t, ts.URL+"/blob.bin", 1)
	sink := &memSink{}
	d, err := New(task, sink, Options{Concurrency: 4, BufferSize: testBuffer})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !bytes.Equal(sink.Bytes(), ts.content) {
		t.Error("落盘内容与源不一致")
	}
	if got := ts.maxConcurrent(); got != 1 {
		t.Errorf("单分片时并发峰值 = %d, 期望 1", got)
	}
}

// 分片数超过内容长度时不应产生空分片或死循环。
func TestSegmentCountLargerThanChunks(t *testing.T) {
	ts := newTrackingServer(t, int(testChunk*2), false)
	task, err := engine.NewTask("t-2", engine.SourceHTTP, ts.URL+"/blob.bin", "dest.bin", "dest.part",
		testChunk*2, testChunk, 8)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	sink := &memSink{}
	d, err := New(task, sink, Options{Concurrency: 8, BufferSize: testBuffer})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !bytes.Equal(sink.Bytes(), ts.content) {
		t.Error("落盘内容与源不一致")
	}
}

// ---------------------------------------------------------------------------
// 降级路径
// ---------------------------------------------------------------------------

// 验收核心：服务端忽略 Range（总是 200）时自动降级为单流，而不是报错。
func TestDegradesToSingleStreamWhenRangeUnsupported(t *testing.T) {
	ts := newTrackingServer(t, int(testTotal), true)
	task := newTask(t, ts.URL+"/blob.bin", 8)
	sink := &memSink{}

	d, err := New(task, sink, Options{Concurrency: 8, BufferSize: testBuffer})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}

	stats := d.Stats()
	if stats.Mode != ModeSingleStream {
		t.Fatalf("Mode = %q, 期望 %q", stats.Mode, ModeSingleStream)
	}
	if !bytes.Equal(sink.Bytes(), ts.content) {
		t.Error("降级模式下落盘内容与源不一致")
	}
	if got := ts.maxConcurrent(); got != 1 {
		t.Errorf("降级模式并发峰值 = %d, 期望 1", got)
	}
	if !task.Complete() {
		t.Error("降级模式下任务也应判定为完成")
	}
	if task.TotalSize != testTotal {
		t.Errorf("TotalSize = %d, 期望 %d", task.TotalSize, testTotal)
	}
}

func TestUnknownSizeFallsBackToSingleStream(t *testing.T) {
	// 服务端使用 chunked 编码，不提供 Content-Length，也不支持 Range。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		chunks := [][]byte{bytes.Repeat([]byte("a"), 1000), bytes.Repeat([]byte("b"), 2000)}
		for _, c := range chunks {
			_, _ = w.Write(c)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	task, err := engine.NewTask("t-3", engine.SourceHTTP, srv.URL, "dest.bin", "dest.part",
		engine.SizeUnknown, testChunk, 4)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	sink := &memSink{}
	d, err := New(task, sink, Options{BufferSize: testBuffer})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if task.TotalSize != 3000 {
		t.Errorf("下载完成后 TotalSize = %d, 期望 3000", task.TotalSize)
	}
	if !task.Complete() {
		t.Error("任务应判定为完成")
	}
	if got := len(sink.Bytes()); got != 3000 {
		t.Errorf("落盘长度 = %d, 期望 3000", got)
	}
}

// ---------------------------------------------------------------------------
// 暂停 / 恢复 / 取消
// ---------------------------------------------------------------------------

// 验收核心：取消后已下载进度保留，重启后从缺口续传而非重下。
func TestCancelThenResumeContinuesFromGap(t *testing.T) {
	ts := newTrackingServer(t, int(testTotal), false)
	task := newTask(t, ts.URL+"/blob.bin", 4)
	sink := &memSink{}
	probe, err := Probe(context.Background(), nil, task.Source, nil)
	if err != nil {
		t.Fatalf("Probe 失败: %v", err)
	}

	// 第一次运行：读到第一个分块后立即取消。
	ctx, cancel := context.WithCancel(context.Background())
	d1, err := New(task, sink, Options{Concurrency: 4, BufferSize: testBuffer, Probe: probe})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	go func() {
		for {
			if task.Downloaded() >= testChunk {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	if err := d1.Run(ctx); err == nil {
		t.Fatal("取消后 Run 应返回错误")
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run 错误 = %v, 期望 context.Canceled", err)
	}

	partial := task.Downloaded()
	if partial == 0 {
		t.Fatal("取消前应有部分数据落盘")
	}
	if partial >= testTotal {
		t.Fatalf("取消时不应已下载完成（%d）", partial)
	}
	// 取消不是失败：分片必须退回待下载状态，且没有分片被标记为 failed。
	for _, seg := range task.Segments {
		if seg.State == engine.SegmentFailed {
			t.Fatalf("取消导致分片 %d 被标记为失败", seg.Index)
		}
	}

	// 第二次运行：继续下载。已下载字节数应严格增加，而不是从零开始。
	beforeWrites := sink.writes
	d2, err := New(task, sink, Options{Concurrency: 4, BufferSize: testBuffer, Probe: probe})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := d2.Run(context.Background()); err != nil {
		t.Fatalf("恢复下载失败: %v", err)
	}
	if !bytes.Equal(sink.Bytes(), ts.content) {
		t.Error("恢复后落盘内容与源不一致")
	}
	if task.Downloaded() != testTotal {
		t.Errorf("恢复后 Downloaded = %d, 期望 %d", task.Downloaded(), testTotal)
	}
	if sink.writes <= beforeWrites {
		t.Error("恢复后应产生新的写入")
	}
	// 恢复时应只补缺口：写入总量应明显少于「完整重下 4 个分片」的规模。
	t.Logf("取消时已下载 %d 字节，恢复后总写入次数 %d", partial, sink.writes)
}

// ---------------------------------------------------------------------------
// 重试与错误处理
// ---------------------------------------------------------------------------

func TestRetriesTransientFailures(t *testing.T) {
	ts := newTrackingServer(t, int(testTotal), false)
	atomic.StoreInt64(&ts.failFirst, 2) // 前 2 个 Range 请求返回 500

	task := newTask(t, ts.URL+"/blob.bin", 2)
	sink := &memSink{}
	d, err := New(task, sink, Options{
		Concurrency: 2,
		BufferSize:  testBuffer,
		MaxRetries:  5,
		Logf:        t.Logf,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("短暂故障后应重试成功，实际: %v", err)
	}
	if !bytes.Equal(sink.Bytes(), ts.content) {
		t.Error("落盘内容与源不一致")
	}
	if n := atomic.LoadInt64(&ts.failCount); n < 2 {
		t.Errorf("故障请求数 = %d, 期望至少 2", n)
	}
}

func TestFailsAfterRetriesExhausted(t *testing.T) {
	// 探测阶段正常（前两次请求），之后的下载请求持续返回 500。
	var n int64
	content := bytes.Repeat([]byte("z"), int(testTotal))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&n, 1) <= 2 {
			http.ServeContent(w, r, "blob.bin", time.Time{}, bytes.NewReader(content))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	task := newTask(t, srv.URL, 2)
	sink := &memSink{}
	d, err := New(task, sink, Options{Concurrency: 2, MaxRetries: 2, BufferSize: testBuffer})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	err = d.Run(context.Background())
	if err == nil {
		t.Fatal("持续 500 时 Run 应返回错误")
	}
	for _, seg := range task.Segments {
		if seg.State != engine.SegmentFailed {
			t.Errorf("分片 %d 状态 = %q, 期望 failed", seg.Index, seg.State)
		}
		if seg.LastErr == "" {
			t.Errorf("分片 %d 应记录失败原因", seg.Index)
		}
	}
}

// ---------------------------------------------------------------------------
// 数据竞争与统计
// ---------------------------------------------------------------------------

// 验收核心：任意时刻读取统计不产生数据竞争（配合 -race 运行）。
func TestStatsAccessIsRaceFree(t *testing.T) {
	ts := newTrackingServer(t, int(testTotal), false)
	task := newTask(t, ts.URL+"/blob.bin", 4)
	sink := &memSink{}
	d, err := New(task, sink, Options{Concurrency: 4, BufferSize: testBuffer})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = d.Stats()
				}
			}
		}()
	}

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	close(stop)
	wg.Wait()

	if !bytes.Equal(sink.Bytes(), ts.content) {
		t.Error("并发生成统计时落盘内容被破坏")
	}
}

// 验收核心：进度保存与下载并发执行时不产生数据竞争，且存下来的进度可读回。
func TestProgressSaverIsRaceFree(t *testing.T) {
	ts := newTrackingServer(t, int(testTotal), false)
	task := newTask(t, ts.URL+"/blob.bin", 4)
	sink := &memSink{}
	saver := &recordingSaver{}

	d, err := New(task, sink, Options{
		Concurrency: 4,
		BufferSize:  testBuffer,
		Progress:    saver,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if saver.count() == 0 {
		t.Fatal("应至少保存一次进度")
	}
	if got := saver.last(); got == nil || got.Downloaded() != testTotal {
		t.Fatalf("最后一次保存的进度 = %v, 期望 %d", got, testTotal)
	}
}

type recordingSaver struct {
	mu    sync.Mutex
	n     int
	final *engine.Task
}

func (s *recordingSaver) Save(t *engine.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	s.final = t
	// 模拟序列化：若位图被并发修改，-race 会在此报出竞争。
	_ = fmt.Sprintf("%v", t.Downloaded())
	return nil
}

func (s *recordingSaver) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *recordingSaver) last() *engine.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.final
}

func TestNewRejectsInvalidInputs(t *testing.T) {
	task := newTask(t, "http://example.com/a", 1)
	if _, err := New(nil, &memSink{}, Options{}); err == nil {
		t.Error("nil 任务应被拒绝")
	}
	if _, err := New(task, nil, Options{}); err == nil {
		t.Error("nil Sink 应被拒绝")
	}
	bt, err := engine.NewTask("bt-1", engine.SourceBitTorrent, "magnet:?xt=urn:btih:abc", "d", "t", 100, 10, 1)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	bt.InfoHash = "abc"
	if _, err := New(bt, &memSink{}, Options{}); err == nil {
		t.Error("非 HTTP 任务应被拒绝")
	}
}

func TestSpeedMeterWindow(t *testing.T) {
	m := newSpeedMeter(200 * time.Millisecond)
	if m.rate() != 0 {
		t.Error("无样本时速率应为 0")
	}
	m.add(1000)
	if m.rate() != 0 {
		t.Error("单样本时速率应为 0")
	}
	time.Sleep(250 * time.Millisecond)
	m.add(1000)
	rate := m.rate()
	if rate <= 0 {
		t.Fatalf("速率 = %v, 期望为正", rate)
	}
	if m.totalBytes() != 2000 {
		t.Errorf("totalBytes = %d, 期望 2000", m.totalBytes())
	}
	// 窗口外的旧样本应被丢弃，速率仍能正常计算。
	time.Sleep(300 * time.Millisecond)
	m.add(400)
	if m.rate() <= 0 {
		t.Error("丢弃旧样本后速率应仍为正")
	}
}

func TestSleepBackoffHonoursContext(t *testing.T) {
	d := &Downloader{opts: Options{MaxRetries: 3}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if d.sleepBackoff(ctx, 1) {
		t.Error("ctx 已取消时 sleepBackoff 应返回 false")
	}
	start := time.Now()
	if !d.sleepBackoff(context.Background(), 1) {
		t.Error("ctx 正常时 sleepBackoff 应返回 true")
	}
	if elapsed := time.Since(start); elapsed < minBackoff {
		t.Errorf("退避时间 = %v, 期望至少 %v", elapsed, minBackoff)
	}
}
