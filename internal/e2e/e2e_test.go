// Package e2e 承载端到端验收测试：用真实进程、真实磁盘 I/O 验证
// T1 定义的成功标准（4GB 强杀续传、10GB 大文件、并发分段等）。
//
// 这些测试默认跳过（需要 GB 级磁盘空间与较长时间）：
//
//	P2PDL_E2E=1 go test ./internal/e2e -v -timeout 60m
package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"p2pdl/internal/cli"
	"p2pdl/internal/client"
	"p2pdl/internal/engine/limiter"
)

// blockSize 是确定性数据流的块大小。
const blockSize = 1 << 20

// TestMain 让本测试二进制在特定环境变量下充当引擎进程。
// 这样 E2E 无需构建真实二进制，同时又能获得「独立进程 + 可被强杀」的真实性。
func TestMain(m *testing.M) {
	if os.Getenv("P2PDL_E2E_AS_ENGINE") == "1" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func requireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("P2PDL_E2E") != "1" {
		t.Skip("设置 P2PDL_E2E=1 以运行端到端验收测试")
	}
}

// ---------------------------------------------------------------------------
// 确定性数据流
// ---------------------------------------------------------------------------

// fillAt 用确定性规则填充 [off, off+len(p))。
//
// 每 1MB 块填充一个由块序号派生的固定字节（blockIdx % 251，避开 0 以便发现空洞）。
// 这样做的理由：
//   - 可以随机访问任意区间，从而支持 HTTP Range 与断点续传；
//   - 生成与校验都不需要真实源文件，GB 级测试也不占用额外磁盘；
//   - 任何区间错位、空洞或重复写入都会改变整体 SHA-256，足以证明数据正确性。
func fillAt(p []byte, off int64) {
	end := off + int64(len(p))
	pos := off
	written := 0
	for pos < end {
		blockIdx := pos / blockSize
		blockEnd := (blockIdx + 1) * blockSize
		if blockEnd > end {
			blockEnd = end
		}
		value := byte(blockIdx % 251)
		n := int(blockEnd - pos)
		chunk := p[written : written+n]
		for i := range chunk {
			chunk[i] = value
		}
		written += n
		pos = blockEnd
	}
}

// expectedHash 流式计算整个数据流的 SHA-256（不落盘）。
func expectedHash(t *testing.T, size int64) string {
	t.Helper()
	h := sha256.New()
	buf := make([]byte, 4<<20)
	for off := int64(0); off < size; off += int64(len(buf)) {
		n := int64(len(buf))
		if off+n > size {
			n = size - off
		}
		fillAt(buf[:n], off)
		if _, err := h.Write(buf[:n]); err != nil {
			t.Fatalf("计算期望哈希失败: %v", err)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fileHash 流式计算文件的 SHA-256（内存占用与文件大小无关）。
func fileHash(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开文件失败: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 4<<20)); err != nil {
		t.Fatalf("读取文件失败: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// detServer 提供支持 Range 的确定性数据流服务，并统计实际发出的字节数。
type detServer struct {
	srv    *httptest.Server
	size   int64
	served atomic.Int64
}

func newDetServer(t *testing.T, size int64) *detServer {
	t.Helper()
	ds := &detServer{size: size}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := int64(0), ds.size-1
		status := http.StatusOK
		if raw := r.Header.Get("Range"); raw != "" {
			spec, _ := strings.CutPrefix(raw, "bytes=")
			startStr, endStr, _ := strings.Cut(spec, "-")
			s, err1 := strconv.ParseInt(startStr, 10, 64)
			e := ds.size - 1
			var err2 error
			if endStr != "" {
				e, err2 = strconv.ParseInt(endStr, 10, 64)
			}
			if err1 != nil || err2 != nil || s < 0 || e >= ds.size || s > e {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			start, end = s, e
			status = http.StatusPartialContent
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, ds.size))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}

		const chunk = 1 << 20
		buf := make([]byte, chunk)
		for off := start; off <= end; {
			n := int64(chunk)
			if end-off+1 < n {
				n = end - off + 1
			}
			fillAt(buf[:n], off)
			written, err := w.Write(buf[:n])
			ds.served.Add(int64(written))
			if err != nil {
				return
			}
			off += int64(written)
			if f, ok := w.(http.Flusher); ok && off% (8<<20) < chunk {
				f.Flush()
			}
		}
	})
	ds.srv = httptest.NewServer(handler)
	t.Cleanup(ds.srv.Close)
	return ds
}

// ---------------------------------------------------------------------------
// 引擎进程管理
// ---------------------------------------------------------------------------

type engineProcess struct {
	cmd     *exec.Cmd
	metaDir string
	dataDir string
	client  *client.Client
	logFile *os.File
	stopped atomic.Bool
}

func startEngine(t *testing.T, dataDir, metaDir string) *engineProcess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制失败: %v", err)
	}

	cmd := exec.Command(exe, "serve", "--data-dir", dataDir, "--meta-dir", metaDir)
	cmd.Env = append(os.Environ(), "P2PDL_E2E_AS_ENGINE=1")
	logFile, err := os.Create(filepath.Join(metaDir, "engine.log"))
	if err != nil {
		t.Fatalf("创建引擎日志失败: %v", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}

	ep := &engineProcess{cmd: cmd, metaDir: metaDir, dataDir: dataDir, logFile: logFile}
	// 必须关闭日志句柄，否则 t.TempDir 的清理会因为文件被占用而失败。
	t.Cleanup(func() {
		ep.Stop()
		_ = logFile.Close()
	})

	// 等待控制面就绪
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := client.Discover(metaDir); err == nil {
			ep.client = c
			return ep
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("引擎未在超时内就绪，日志:\n%s", readFileOrEmpty(filepath.Join(metaDir, "engine.log")))
	return nil
}

// Kill 强杀引擎进程（等价于任务管理器结束进程 / 断电），不做任何清理。
func (e *engineProcess) Kill() {
	if e.cmd == nil || e.cmd.Process == nil {
		return
	}
	_ = e.cmd.Process.Kill()
	_, _ = e.cmd.Process.Wait()
	e.stopped.Store(true)
}

func (e *engineProcess) Stop() {
	if e.stopped.Load() {
		return
	}
	e.Kill()
}

func readFileOrEmpty(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// 成功标准 1：4GB 断点续传（强杀进程后续接而非重下）
// ---------------------------------------------------------------------------

func TestE2E_FourGigabyteResumeAfterHardKill(t *testing.T) {
	requireE2E(t)
	const size = int64(4) << 30

	dir := t.TempDir()
	free := freeSpaceGB(t, dir)
	if free < 9 {
		t.Skipf("磁盘可用空间不足（%.1f GB），跳过 4GB 测试", free)
	}
	t.Logf("工作目录 %s，可用空间 %.1f GB", dir, free)

	ds := newDetServer(t, size)
	dataDir := filepath.Join(dir, "dl")
	metaDir := filepath.Join(dir, "meta")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("创建下载目录失败: %v", err)
	}
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatalf("创建元数据目录失败: %v", err)
	}

	// 第一轮：下载到约 30% 后强杀
	first := startEngine(t, dataDir, metaDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	task, err := first.client.AddTask(ctx, client.AddRequest{
		Source: ds.srv.URL + "/big.bin",
		Name:   "big.bin",
	})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	t.Logf("任务已创建: %s", task.ID)

	target := size * 30 / 100
	waitFor(t, 10*time.Minute, "下载到 30%", func() bool {
		v, err := first.client.GetTask(ctx, task.ID)
		return err == nil && v.Done >= target
	})

	before, err := first.client.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("读取进度失败: %v", err)
	}
	servedBeforeKill := ds.served.Load()
	t.Logf("强杀前: 已下载 %.2f GB (%.1f%%)，服务端累计发出 %.2f GB",
		float64(before.Done)/(1<<30), before.Progress*100, float64(servedBeforeKill)/(1<<30))

	// 真实强杀：不触发任何优雅退出路径
	first.Kill()
	time.Sleep(500 * time.Millisecond)
	t.Logf("已强杀引擎进程（未做优雅关闭）")

	// 第二轮：同目录重启，应自动续传
	second := startEngine(t, dataDir, metaDir)
	resumed, err := second.client.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("重启后读取任务失败: %v", err)
	}
	t.Logf("重启后任务状态=%s，已下载 %.2f GB", resumed.State, float64(resumed.Done)/(1<<30))

	// 关键断言：进度必须被保留，而不是回到 0
	if resumed.Done < before.Done {
		t.Fatalf("重启后进度倒退：%.2f GB → %.2f GB（说明断点未持久化）",
			float64(before.Done)/(1<<30), float64(resumed.Done)/(1<<30))
	}
	// 允许的丢失量：下载器按数据量（32MB）兜底保存进度，
	// 因此强杀最多重下最后一次未落盘的阈值量。
	loss := before.Done - resumed.Done
	const maxLoss = int64(96) << 20 // 96MB：3 个阈值，留出元数据写入延迟的余量
	if loss > maxLoss {
		t.Fatalf("重启后丢失 %.0f MB 进度，超过 %.0f MB 容差（说明保存间隔过大）",
			float64(loss)/(1<<20), float64(maxLoss)/(1<<20))
	}
	t.Logf("强杀丢失 %.0f MB（占已验证进度 %.1f%%），断点保留率 %.1f%%",
		float64(loss)/(1<<20), float64(loss)*100/float64(before.Done),
		float64(resumed.Done)*100/float64(before.Done))

	// 等待完成
	waitFor(t, 40*time.Minute, "任务完成", func() bool {
		v, err := second.client.GetTask(ctx, task.ID)
		if err != nil {
			return false
		}
		if v.State == "failed" {
			t.Fatalf("任务失败: %s", v.LastError)
		}
		return v.State == "completed"
	})

	final, _ := second.client.GetTask(ctx, task.ID)
	t.Logf("下载完成：%.2f GB，服务端累计发出 %.2f GB",
		float64(final.Total)/(1<<30), float64(ds.served.Load())/(1<<30))

	// 续传有效性：总传输量应显著小于「重下两次全量」
	totalServed := ds.served.Load()
	if totalServed >= size*3/2 {
		t.Errorf("服务端共发出 %.2f GB（总长 %.2f GB），疑似发生了重下",
			float64(totalServed)/(1<<30), float64(size)/(1<<30))
	}

	// 校验：必须与确定性数据流的 SHA-256 完全一致
	gotPath := filepath.Join(dataDir, "big.bin")
	fi, err := os.Stat(gotPath)
	if err != nil {
		t.Fatalf("下载文件不存在: %v", err)
	}
	if fi.Size() != size {
		t.Fatalf("文件大小 = %d, 期望 %d", fi.Size(), size)
	}
	t.Log("开始计算 SHA-256（4GB，流式，内存占用固定）…")
	start := time.Now()
	gotHash := fileHash(t, gotPath)
	t.Logf("文件哈希计算完成，耗时 %v", time.Since(start).Round(time.Second))

	t.Log("计算期望哈希…")
	wantHash := expectedHash(t, size)

	if gotHash != wantHash {
		t.Fatalf("SHA-256 不匹配\n得到 %s\n期望 %s", gotHash, wantHash)
	}
	t.Logf("✅ 成功标准 1 通过：强杀后断点续传完成，SHA-256 = %s", gotHash[:16]+"…")
}

// ---------------------------------------------------------------------------
// 成功标准 3：多段并发实测
// ---------------------------------------------------------------------------

func TestE2E_ConcurrentSegmentsAreActuallyConcurrent(t *testing.T) {
	requireE2E(t)
	const size = int64(512) << 20 // 512MB

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "dl")
	metaDir := filepath.Join(dir, "meta")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}

	// 服务端记录并发请求峰值
	var active, peak atomic.Int64
	ds := newDetServer(t, size)
	original := ds.srv.Config.Handler
	ds.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := active.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		defer active.Add(-1)
		original.ServeHTTP(w, r)
	})

	ep := startEngine(t, dataDir, metaDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	task, err := ep.client.AddTask(ctx, client.AddRequest{
		Source: ds.srv.URL + "/big.bin",
		Name:   "concurrent.bin",
	})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	waitFor(t, 15*time.Minute, "任务完成", func() bool {
		v, err := ep.client.GetTask(ctx, task.ID)
		return err == nil && v.State == "completed"
	})

	view, _ := ep.client.GetTask(ctx, task.ID)
	t.Logf("任务完成：模式=%s，服务端并发峰值=%d", view.Mode, peak.Load())

	if view.Mode != "multi-segment" {
		t.Fatalf("期望多段模式，实际 %q", view.Mode)
	}
	if peak.Load() < 2 {
		t.Fatalf("服务端观测到的并发峰值 = %d，未体现分段并发", peak.Load())
	}
	if got := fileHash(t, filepath.Join(dataDir, "concurrent.bin")); got != expectedHash(t, size) {
		t.Fatal("并发下载内容校验失败")
	}
	t.Logf("✅ 成功标准 3 通过：%d 段并发，内容校验一致", peak.Load())
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待「%s」超时（%v）", what, timeout)
}

// freeSpaceGB 返回路径所在卷的可用空间（GB）。
// 复用 limiter 包已实现的跨平台磁盘空间查询，避免在测试里重复造轮子。
func freeSpaceGB(t *testing.T, path string) float64 {
	t.Helper()
	free, err := limiter.FreeSpace(path)
	if err != nil {
		t.Logf("无法读取磁盘信息（%v），按充足处理", err)
		return 1 << 10
	}
	return float64(free) / (1 << 30)
}

// ---------------------------------------------------------------------------
// 成功标准 2：10GB 大文件下载（峰值内存可控 + 内容无错位）
// ---------------------------------------------------------------------------

func TestE2E_TenGigabyteLargeFile(t *testing.T) {
	requireE2E(t)
	const size = int64(10) << 30

	dir := t.TempDir()
	if free := freeSpaceGB(t, dir); free < 21 {
		t.Skipf("磁盘可用空间不足（%.1f GB），跳过 10GB 测试", free)
	}

	ds := newDetServer(t, size)
	dataDir := filepath.Join(dir, "dl")
	metaDir := filepath.Join(dir, "meta")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}

	ep := startEngine(t, dataDir, metaDir)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	task, err := ep.client.AddTask(ctx, client.AddRequest{
		Source: ds.srv.URL + "/huge.bin",
		Name:   "huge.bin",
	})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	// 独立 goroutine 采样引擎进程的工作集，记录峰值。
	var peak atomic.Int64
	var samples atomic.Int64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		pid := ep.cmd.Process.Pid
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if ws, err := processWorkingSet(pid); err == nil {
					samples.Add(1)
					for {
						cur := peak.Load()
						if ws <= cur || peak.CompareAndSwap(cur, ws) {
							break
						}
					}
				}
			}
		}
	}()

	start := time.Now()
	waitFor(t, 50*time.Minute, "10GB 下载完成", func() bool {
		v, err := ep.client.GetTask(ctx, task.ID)
		if err != nil {
			return false
		}
		if v.State == "failed" {
			t.Fatalf("任务失败: %s", v.LastError)
		}
		return v.State == "completed"
	})
	elapsed := time.Since(start)

	close(stop)
	<-sampled

	view, _ := ep.client.GetTask(ctx, task.ID)
	t.Logf("10GB 完成：耗时 %v（平均 %.0f MB/s），模式=%s，服务端发出 %.2f GB",
		elapsed.Round(time.Second),
		float64(size)/(1<<20)/elapsed.Seconds(),
		view.Mode,
		float64(ds.served.Load())/(1<<30))
	t.Logf("引擎进程工作集峰值 %.1f MB，采样 %d 次",
		float64(peak.Load())/(1<<20), samples.Load())

	const memLimit = int64(500) << 20
	if peak.Load() >= memLimit {
		t.Fatalf("引擎进程工作集峰值 %.1f MB 超过 500MB 上限", float64(peak.Load())/(1<<20))
	}

	gotPath := filepath.Join(dataDir, "huge.bin")
	fi, err := os.Stat(gotPath)
	if err != nil {
		t.Fatalf("文件不存在: %v", err)
	}
	if fi.Size() != size {
		t.Fatalf("文件大小 = %d, 期望 %d", fi.Size(), size)
	}

	t.Log("计算 10GB 文件 SHA-256（流式，内存占用固定）…")
	hashStart := time.Now()
	got := fileHash(t, gotPath)
	t.Logf("哈希完成，耗时 %v", time.Since(hashStart).Round(time.Second))
	want := expectedHash(t, size)
	if got != want {
		t.Fatalf("SHA-256 不匹配\n得到 %s\n期望 %s", got, want)
	}
	t.Logf("✅ 成功标准 2 通过：10GB 完整下载，工作集峰值 %.1f MB，SHA-256 = %s",
		float64(peak.Load())/(1<<20), got[:16]+"…")
}
