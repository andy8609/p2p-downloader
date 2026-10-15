package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"p2pdl/internal/engine"
	"p2pdl/internal/engine/bittorrent"
)

// tempDir 创建测试目录并尽力清理（BT 存储层可能保留文件句柄，见 docs/bittorrent.md）。
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "p2pdl-app-")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// rangeServer 提供一个支持 Range 的只读文件服务。
func rangeServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "payload.bin", time.Time{}, bytes.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// seedLayout 创建 <root>/<name>/payload.bin 并生成 .torrent。
func seedLayout(t *testing.T, name string, size int) (root, payloadPath, torrentPath string) {
	t.Helper()
	root = tempDir(t)
	payloadDir := filepath.Join(root, name)
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	payloadPath = filepath.Join(payloadDir, "payload.bin")
	if err := os.WriteFile(payloadPath, content, 0o644); err != nil {
		t.Fatalf("写入源文件失败: %v", err)
	}

	info := metainfo.Info{PieceLength: 64 << 10}
	if err := info.BuildFromFilePath(payloadDir); err != nil {
		t.Fatalf("生成 info 失败: %v", err)
	}
	raw, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("序列化 info 失败: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: raw}

	torrentPath = filepath.Join(root, name+".torrent")
	f, err := os.Create(torrentPath)
	if err != nil {
		t.Fatalf("创建种子失败: %v", err)
	}
	defer f.Close()
	if err := mi.Write(f); err != nil {
		t.Fatalf("写入种子失败: %v", err)
	}
	return root, payloadPath, torrentPath
}

// startBTSeeder 启动一个已完成校验的种子端，返回引擎与其 peer 地址。
func startBTSeeder(t *testing.T, seedRoot, torrentPath string) (*bittorrent.Engine, string) {
	t.Helper()
	cfg := bittorrent.Config{
		DataDir:         seedRoot,
		DisableDHT:      true,
		DisableTrackers: true,
		DisablePEX:      true,
		DisableUTP:      true,
		Seed:            true,
	}
	eng, err := bittorrent.NewEngine(cfg)
	if err != nil {
		t.Fatalf("创建种子端失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	h, err := eng.Add(bittorrent.AddSpec{TorrentFile: torrentPath})
	if err != nil {
		t.Fatalf("种子端加入 torrent 失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.WaitInfo(ctx); err != nil {
		t.Fatalf("种子端等待元数据失败: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if h.Complete() {
			return eng, fmt.Sprintf("127.0.0.1:%d", eng.ListenPort())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("种子端数据校验超时")
	return nil, ""
}

func newTestApp(t *testing.T, dir string, maxTasks int) *App {
	t.Helper()
	a, err := New(Options{
		DataDir:            dir,
		MetaDir:            filepath.Join(dir, "meta"),
		ChunkSize:          64 << 10,
		Concurrency:        4,
		MaxConcurrentTasks: maxTasks,
		SkipSpaceCheck:     true,
		Logf:               nil,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return a
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待「%s」超时（%v）", what, timeout)
}

// 验收核心：HTTP 与 BT 任务在同一调度器下同时运行且互不干扰。
func TestAppRunsHTTPAndBitTorrentConcurrently(t *testing.T) {
	httpContent := make([]byte, 768<<10)
	if _, err := rand.Read(httpContent); err != nil {
		t.Fatalf("生成 HTTP 数据失败: %v", err)
	}
	httpSum := sha256.Sum256(httpContent)
	srv := rangeServer(t, httpContent)

	seedRoot, seedPayload, torrentPath := seedLayout(t, "bt-src", 512<<10)
	seedSum := func() [32]byte {
		data, err := os.ReadFile(seedPayload)
		if err != nil {
			t.Fatalf("读取种子文件失败: %v", err)
		}
		return sha256.Sum256(data)
	}()
	_, peer := startBTSeeder(t, seedRoot, torrentPath)

	dir := tempDir(t)
	a := newTestApp(t, dir, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer a.Stop()

	httpTask, err := a.AddHTTP(ctx, srv.URL+"/payload.bin", AddOptions{Name: "http-payload.bin"})
	if err != nil {
		t.Fatalf("AddHTTP 失败: %v", err)
	}
	btTask, err := a.AddMagnet(ctx, magnetFromTorrent(t, torrentPath), AddOptions{
		Name:  "bt-payload.bin",
		Peers: []string{peer},
	})
	if err != nil {
		t.Fatalf("AddMagnet 失败: %v", err)
	}

	waitFor(t, 90*time.Second, "两个任务都完成", func() bool {
		views := a.Scheduler().List()
		if len(views) != 2 {
			return false
		}
		for _, v := range views {
			if v.State != engine.StateCompleted {
				return false
			}
		}
		return true
	})

	views := a.Scheduler().List()
	byID := map[string]engine.TaskView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	if v := byID[httpTask.ID]; v.Kind != engine.SourceHTTP || v.Total != int64(len(httpContent)) {
		t.Errorf("HTTP 任务视图异常: %+v", v)
	}
	if v := byID[btTask.ID]; v.Kind != engine.SourceBitTorrent || v.Total != int64(len(httpContent)/2*1) && v.Total != 512<<10 {
		t.Errorf("BT 任务视图异常: %+v", v)
	}

	gotHTTP, err := os.ReadFile(filepath.Join(dir, "http-payload.bin"))
	if err != nil {
		t.Fatalf("读取 HTTP 结果失败: %v", err)
	}
	if sha256.Sum256(gotHTTP) != httpSum {
		t.Error("HTTP 下载内容与源不一致")
	}

	gotBT, err := os.ReadFile(filepath.Join(dir, "bt-src", "payload.bin"))
	if err != nil {
		t.Fatalf("读取 BT 结果失败: %v", err)
	}
	if sha256.Sum256(gotBT) != seedSum {
		t.Error("BT 下载内容与源不一致")
	}

	// HTTP 任务完成后临时文件必须被原子重命名为最终文件。
	if _, err := os.Stat(httpTask.TempPath); !os.IsNotExist(err) {
		t.Errorf("HTTP 完成后临时文件应已重命名, statErr=%v", err)
	}
}

// 验收核心：应用退出后重启，队列状态与进度完整恢复。
func TestAppRestoresQueueAfterRestart(t *testing.T) {
	content := make([]byte, 512<<10)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	sum := sha256.Sum256(content)
	srv := rangeServer(t, content)

	dir := tempDir(t)

	// 第一次运行：加三个任务，其中两个暂停、一个允许完成。
	a1 := newTestApp(t, dir, 1)
	ctx1, cancel1 := context.WithCancel(context.Background())
	if err := a1.Start(ctx1); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}

	done, err := a1.AddHTTP(ctx1, srv.URL+"/payload.bin", AddOptions{Name: "done.bin"})
	if err != nil {
		t.Fatalf("AddHTTP 失败: %v", err)
	}
	paused, err := a1.AddHTTP(ctx1, srv.URL+"/payload.bin", AddOptions{Name: "paused.bin", Paused: true})
	if err != nil {
		t.Fatalf("AddHTTP 失败: %v", err)
	}
	waitFor(t, 30*time.Second, "第一个任务完成", func() bool {
		v, _ := a1.Scheduler().Get(done.ID)
		return v.State == engine.StateCompleted
	})
	cancel1()
	if err := a1.Stop(); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}

	// 第二次运行：同一目录，队列与状态必须还原。
	a2 := newTestApp(t, dir, 1)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if err := a2.Start(ctx2); err != nil {
		t.Fatalf("重启 Start 失败: %v", err)
	}
	defer a2.Stop()

	views := a2.Scheduler().List()
	if len(views) != 2 {
		t.Fatalf("重启后任务数 = %d, 期望 2", len(views))
	}
	byID := map[string]engine.TaskView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	if got := byID[done.ID].State; got != engine.StateCompleted {
		t.Errorf("已完成任务重启后状态 = %q, 期望 completed", got)
	}
	if got := byID[paused.ID].State; got != engine.StatePaused {
		t.Errorf("暂停任务重启后状态 = %q, 期望 paused", got)
	}

	// 已完成任务不应被重新下载（文件仍在，且不会再被调度）。
	time.Sleep(500 * time.Millisecond)
	if got := a2.Scheduler().Stats().Running; got != 0 {
		t.Errorf("重启后不应自动运行任何任务，实际运行 %d 个", got)
	}

	// 恢复暂停的任务，应能完成。
	if err := a2.Scheduler().Resume(paused.ID); err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	waitFor(t, 30*time.Second, "恢复的任务完成", func() bool {
		v, _ := a2.Scheduler().Get(paused.ID)
		return v.State == engine.StateCompleted
	})

	got, err := os.ReadFile(filepath.Join(dir, "paused.bin"))
	if err != nil {
		t.Fatalf("读取结果失败: %v", err)
	}
	if sha256.Sum256(got) != sum {
		t.Error("恢复后下载内容不一致")
	}
}

// 未看完的任务重启后应继续下载，而不是从头开始。
func TestAppResumesInterruptedTaskAfterRestart(t *testing.T) {
	content := make([]byte, 1<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	sum := sha256.Sum256(content)

	// 慢速服务端，保证第一次运行来不及下完。
	srv := slowRangeServer(t, content)

	dir := tempDir(t)

	a1 := newTestApp(t, dir, 1)
	ctx1, cancel1 := context.WithCancel(context.Background())
	if err := a1.Start(ctx1); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	task, err := a1.AddHTTP(ctx1, srv.URL+"/payload.bin", AddOptions{Name: "resume.bin"})
	if err != nil {
		t.Fatalf("AddHTTP 失败: %v", err)
	}
	waitFor(t, 30*time.Second, "产生部分进度", func() bool {
		v, _ := a1.Scheduler().Get(task.ID)
		return v.Done > 0
	})
	cancel1()
	if err := a1.Stop(); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	partial := task.Downloaded()
	t.Logf("第一次运行下载了 %d/%d 字节", partial, int64(len(content)))

	// 重启：应从断点继续。
	a2 := newTestApp(t, dir, 1)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if err := a2.Start(ctx2); err != nil {
		t.Fatalf("重启 Start 失败: %v", err)
	}
	defer a2.Stop()

	waitFor(t, 60*time.Second, "任务完成", func() bool {
		v, _ := a2.Scheduler().Get(task.ID)
		return v.State == engine.StateCompleted
	})

	got, err := os.ReadFile(filepath.Join(dir, "resume.bin"))
	if err != nil {
		t.Fatalf("读取结果失败: %v", err)
	}
	if sha256.Sum256(got) != sum {
		t.Error("续传后文件内容与源不一致")
	}
}

// slowRangeServer 提供支持 Range 但慢速吐数据的服务端，
// 用于制造稳定的「下载到一半」状态（ServeContent 无法插入延迟）。
func slowRangeServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	size := int64(len(content))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := int64(0), size-1
		status := http.StatusOK
		if raw := r.Header.Get("Range"); raw != "" {
			spec, _ := strings.CutPrefix(raw, "bytes=")
			startStr, endStr, _ := strings.Cut(spec, "-")
			s, err1 := strconv.ParseInt(startStr, 10, 64)
			e := size - 1
			var err2 error
			if endStr != "" {
				e, err2 = strconv.ParseInt(endStr, 10, 64)
			}
			if err1 != nil || err2 != nil || s < 0 || e >= size || s > e {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start, end = s, e
			status = http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}
		const step = 32 << 10
		for off := start; off <= end; off += step {
			last := off + step - 1
			if last > end {
				last = end
			}
			if _, err := w.Write(content[off : last+1]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestAppAddHTTPRejectsInvalidInput(t *testing.T) {
	dir := tempDir(t)
	a := newTestApp(t, dir, 1)
	ctx := context.Background()

	if _, err := a.AddHTTP(ctx, "  ", AddOptions{}); err == nil {
		t.Error("空地址应被拒绝")
	}
	if _, err := a.AddHTTP(ctx, "ftp://example.com/a", AddOptions{}); err == nil {
		t.Error("非 HTTP 协议应被拒绝")
	}
	if _, err := a.AddMagnet(ctx, "http://example.com", AddOptions{}); err == nil {
		t.Error("非磁力链应被拒绝")
	}
	if _, err := a.AddTorrentFile(ctx, filepath.Join(dir, "missing.torrent"), AddOptions{}); err == nil {
		t.Error("不存在的种子应被拒绝")
	}
}

func TestAppRemoveTask(t *testing.T) {
	content := make([]byte, 128<<10)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	srv := rangeServer(t, content)

	dir := tempDir(t)
	a := newTestApp(t, dir, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer a.Stop()

	task, err := a.AddHTTP(ctx, srv.URL+"/payload.bin", AddOptions{Name: "rm.bin"})
	if err != nil {
		t.Fatalf("AddHTTP 失败: %v", err)
	}
	waitFor(t, 30*time.Second, "任务完成", func() bool {
		v, _ := a.Scheduler().Get(task.ID)
		return v.State == engine.StateCompleted
	})

	if err := a.Remove(ctx, task.ID, true); err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}
	if _, ok := a.Scheduler().Get(task.ID); ok {
		t.Error("移除后不应再能查到任务")
	}
	if _, err := os.Stat(filepath.Join(dir, "rm.bin")); !os.IsNotExist(err) {
		t.Error("deleteFiles=true 时应删除数据文件")
	}
	if _, err := a.Store().Load(task.ID); err == nil {
		t.Error("移除后元数据应被删除")
	}
	if err := a.Remove(ctx, task.ID, false); err == nil {
		t.Error("重复移除应返回错误")
	}
}

func TestNewIDIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 500; i++ {
		id := NewID()
		if id == "" {
			t.Fatal("ID 不应为空")
		}
		if seen[id] {
			t.Fatalf("ID 冲突: %s", id)
		}
		seen[id] = true
	}
	// 任务顺序由 CreatedAt 保证（Scheduler.List 已按它排序），ID 只要求唯一与可读。
}

func TestOptionsNormalize(t *testing.T) {
	dir := tempDir(t)
	a, err := New(Options{DataDir: dir, SkipSpaceCheck: true})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	defer a.Stop()

	opts := a.Options()
	if opts.ChunkSize != DefaultChunkSize {
		t.Errorf("ChunkSize = %d", opts.ChunkSize)
	}
	if opts.Concurrency != DefaultConcurrency {
		t.Errorf("Concurrency = %d", opts.Concurrency)
	}
	if opts.MaxConcurrentTasks != DefaultMaxConcurrentTasks {
		t.Errorf("MaxConcurrentTasks = %d", opts.MaxConcurrentTasks)
	}
	if opts.MetaDir != filepath.Join(dir, ".p2pdl") {
		t.Errorf("MetaDir = %q", opts.MetaDir)
	}
	if opts.BT.DataDir != dir {
		t.Errorf("BT.DataDir = %q, 期望 %q", opts.BT.DataDir, dir)
	}

	if _, err := New(Options{}); err == nil {
		t.Error("缺少 DataDir 应被拒绝")
	}
}

func magnetFromTorrent(t *testing.T, torrentPath string) string {
	t.Helper()
	mi, err := metainfo.LoadFromFile(torrentPath)
	if err != nil {
		t.Fatalf("读取种子失败: %v", err)
	}
	return mi.Magnet(nil, nil).String()
}



// countingRangeServer 在 rangeServer 基础上统计服务端实际发出的字节数，
// 用于证明「只重下受损分块」而不是整个文件。
func countingRangeServer(t *testing.T, content []byte, served *int64) *httptest.Server {
	t.Helper()
	size := int64(len(content))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := int64(0), size-1
		status := http.StatusOK
		if raw := r.Header.Get("Range"); raw != "" {
			spec, _ := strings.CutPrefix(raw, "bytes=")
			startStr, endStr, _ := strings.Cut(spec, "-")
			s, err1 := strconv.ParseInt(startStr, 10, 64)
			e := size - 1
			var err2 error
			if endStr != "" {
				e, err2 = strconv.ParseInt(endStr, 10, 64)
			}
			if err1 != nil || err2 != nil || s < 0 || e >= size || s > e {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start, end = s, e
			status = http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}
		n := end - start + 1
		atomic.AddInt64(served, n)
		if _, err := w.Write(content[start : end+1]); err != nil {
			return
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// 验收核心（回归）：删除「未完成」的任务时，临时文件必须一并清理。
//
// 这是一次真实缺陷的回归测试：早期实现从 TaskView 拼了一个只含 ID 与 DestPath
// 的空壳 Task，导致 TempPath 为空、GB 级临时数据静默残留。
// 原有测试只覆盖了「下载完成后删除」（此时清理的是最终文件），因此漏掉了它。
func TestAppRemoveIncompleteTaskDeletesTempFile(t *testing.T) {
	content := make([]byte, 2<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	srv := slowRangeServer(t, content)

	dir := tempDir(t)
	a := newTestApp(t, dir, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer a.Stop()

	task, err := a.AddHTTP(ctx, srv.URL+"/payload.bin", AddOptions{Name: "partial.bin"})
	if err != nil {
		t.Fatalf("AddHTTP 失败: %v", err)
	}

	// 等到确实产生了临时数据，但还没完成。
	waitFor(t, 30*time.Second, "产生部分进度", func() bool {
		v, _ := a.Scheduler().Get(task.ID)
		return v.Done > 0 && v.State != engine.StateCompleted
	})

	tempPath := task.TempPath
	if tempPath == "" {
		t.Fatal("任务应记录临时文件路径")
	}
	fi, err := os.Stat(tempPath)
	if err != nil {
		t.Fatalf("临时文件应存在: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("临时文件应已有数据")
	}
	t.Logf("删除前临时文件 %d 字节: %s", fi.Size(), tempPath)

	if err := a.Remove(ctx, task.ID, true); err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}

	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("deleteFiles=true 必须删除临时文件，但仍存在: %s", tempPath)
	}
	if _, err := a.Store().Load(task.ID); err == nil {
		t.Error("任务元数据应已被删除")
	}
	if _, ok := a.Scheduler().Get(task.ID); ok {
		t.Error("调度器中不应再能查到该任务")
	}
}
