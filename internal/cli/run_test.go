package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"p2pdl/internal/client"
	"p2pdl/internal/engine"
	"p2pdl/internal/engine/bittorrent"
)

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "p2pdl-cli-")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("等待「%s」超时（%v）", what, timeout)
}

// embeddedEngine 在测试进程内启动一个真实引擎（等价于 `dl serve`）。
type embeddedEngine struct {
	dataDir string
	metaDir string
	cancel  context.CancelFunc
	done    chan int
}

func startEmbeddedEngine(t *testing.T) *embeddedEngine {
	t.Helper()
	dataDir := tempDir(t)
	metaDir := filepath.Join(dataDir, "meta")

	ctx, cancel := context.WithCancel(context.Background())
	e := &embeddedEngine{
		dataDir: dataDir,
		metaDir: metaDir,
		cancel:  cancel,
		done:    make(chan int, 1),
	}

	go func() {
		var out, errOut bytes.Buffer
		code := Run(ctx, []string{"serve", "--data-dir", dataDir, "--meta-dir", metaDir}, &out, &errOut)
		e.done <- code
	}()

	// 等待端点文件出现，说明控制面已就绪。
	waitFor(t, 20*time.Second, "引擎就绪", func() bool {
		_, err := client.ReadEndpoint(metaDir)
		return err == nil
	})
	t.Cleanup(func() {
		cancel()
		select {
		case <-e.done:
		case <-time.After(15 * time.Second):
			t.Errorf("引擎未能在超时内停止")
		}
	})
	return e
}

// run 执行一次 CLI 调用，返回退出码与合并后的输出。
func (e *embeddedEngine) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	full := append([]string{}, args...)
	full = append(full, "--meta-dir", e.metaDir)
	var out, errOut bytes.Buffer
	code := Run(context.Background(), full, &out, &errOut)
	return code, out.String() + errOut.String()
}

// ---------------------------------------------------------------------------
// 用法与退出码
// ---------------------------------------------------------------------------

func TestHelpIsCompleteAndExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, &out, &errOut); code != ExitOK {
		t.Fatalf("help 退出码 = %d, 期望 %d", code, ExitOK)
	}
	text := out.String()
	for _, cmd := range []string{"serve", "add", "list", "watch", "pause", "resume", "recheck", "rm", "status", "limit", "version", "help"} {
		if !strings.Contains(text, cmd) {
			t.Errorf("帮助缺少命令 %q", cmd)
		}
	}
	for _, opt := range []string{"--data-dir", "--meta-dir", "--json"} {
		if !strings.Contains(text, opt) {
			t.Errorf("帮助缺少选项 %q", opt)
		}
	}
	if !strings.Contains(text, "退出码") {
		t.Error("帮助应说明退出码语义")
	}
}

func TestNoArgsExitsWithUsageCode(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), nil, &out, &errOut); code != ExitUsage {
		t.Fatalf("无参数退出码 = %d, 期望 %d", code, ExitUsage)
	}
	if errOut.Len() == 0 {
		t.Error("用法错误应输出到 stderr")
	}
}

func TestUnknownCommandExitsWithUsageCode(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"frobnicate"}, &out, &errOut); code != ExitUsage {
		t.Fatalf("未知命令退出码 = %d, 期望 %d", code, ExitUsage)
	}
}

func TestVersionCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"version"}, &out, &errOut); code != ExitOK {
		t.Fatalf("version 退出码 = %d", code)
	}
	if !strings.Contains(out.String(), "dl ") {
		t.Fatalf("版本输出异常: %q", out.String())
	}
}

// 引擎未运行时必须给出清晰错误并返回非 0。
func TestCommandsFailWhenEngineNotRunning(t *testing.T) {
	metaDir := filepath.Join(tempDir(t), "meta")
	for _, args := range [][]string{
		{"list"}, {"status"}, {"pause", "x"}, {"resume", "x"}, {"recheck", "x"}, {"rm", "x"},
		{"add", "http://example.com/a"},
	} {
		var out, errOut bytes.Buffer
		full := append(append([]string{}, args...), "--meta-dir", metaDir)
		code := Run(context.Background(), full, &out, &errOut)
		if code != ExitError {
			t.Errorf("%v 退出码 = %d, 期望 %d", args, code, ExitError)
		}
		if !strings.Contains(errOut.String(), "引擎") {
			t.Errorf("%v 的错误信息应提到引擎不可用: %q", args, errOut.String())
		}
	}
}

func TestAddRequiresSource(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"add"}, &out, &errOut); code != ExitUsage {
		t.Fatalf("缺少来源时退出码 = %d, 期望 %d", code, ExitUsage)
	}
}

// ---------------------------------------------------------------------------
// 端到端：HTTP
// ---------------------------------------------------------------------------

func rangeServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "payload.bin", time.Time{}, bytes.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 验收核心：不启动任何 GUI，用 CLI 完成一次 HTTP 下载。
func TestCLIHTTPDownloadEndToEnd(t *testing.T) {
	content := make([]byte, 512<<10)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	sum := sha256.Sum256(content)
	srv := rangeServer(t, content)

	eng := startEmbeddedEngine(t)

	// add 并等待完成（--watch 默认开启）。
	code, out := eng.run(t, "add", srv.URL+"/payload.bin",
		"--name", "cli-http.bin",
		"--hash", hex.EncodeToString(sum[:]),
		"--timeout", "60s")
	if code != ExitOK {
		t.Fatalf("add 退出码 = %d，输出:\n%s", code, out)
	}
	if !strings.Contains(out, "完成") {
		t.Errorf("输出应包含完成提示:\n%s", out)
	}

	got, err := os.ReadFile(filepath.Join(eng.dataDir, "cli-http.bin"))
	if err != nil {
		t.Fatalf("读取下载结果失败: %v", err)
	}
	if sha256.Sum256(got) != sum {
		t.Fatal("CLI 下载的文件与源不一致")
	}

	// list 应显示已完成。
	code, out = eng.run(t, "list")
	if code != ExitOK {
		t.Fatalf("list 退出码 = %d", code)
	}
	if !strings.Contains(out, "cli-http.bin") || !strings.Contains(out, string(engine.StateCompleted)) {
		t.Errorf("list 输出未反映已完成任务:\n%s", out)
	}

	// --json 输出应可被解析。
	code, out = eng.run(t, "list", "--json")
	if code != ExitOK {
		t.Fatalf("list --json 退出码 = %d", code)
	}
	if !strings.Contains(out, "\"state\": \"completed\"") {
		t.Errorf("JSON 输出异常:\n%s", out)
	}
}

// 下载失败（服务器返回 404）时 add 必须返回非 0。
func TestCLIAddFailsOnBadURL(t *testing.T) {
	eng := startEmbeddedEngine(t)
	code, out := eng.run(t, "add", "http://127.0.0.1:1/definitely-missing", "--timeout", "10s")
	if code != ExitError {
		t.Fatalf("无效地址退出码 = %d, 期望 %d，输出:\n%s", code, ExitError, out)
	}
}

// ---------------------------------------------------------------------------
// 端到端：BitTorrent
// ---------------------------------------------------------------------------

// seedLayout 生成 <root>/<name>/payload.bin 与对应的 .torrent。
func seedLayout(t *testing.T, name string, size int) (root, torrentPath string, content []byte) {
	t.Helper()
	root = tempDir(t)
	payloadDir := filepath.Join(root, name)
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	content = make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(payloadDir, "payload.bin"), content, 0o644); err != nil {
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
	return root, torrentPath, content
}

// startSeeder 启动本地做种端并返回其 peer 地址。
func startSeeder(t *testing.T, seedRoot, torrentPath string) string {
	t.Helper()
	eng, err := bittorrent.NewEngine(bittorrent.Config{
		DataDir:         seedRoot,
		DisableDHT:      true,
		DisableTrackers: true,
		DisablePEX:      true,
		DisableUTP:      true,
		Seed:            true,
	})
	if err != nil {
		t.Fatalf("创建种子端失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	h, err := eng.Add(bittorrent.AddSpec{TorrentFile: torrentPath})
	if err != nil {
		t.Fatalf("加入种子失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := h.WaitInfo(ctx); err != nil {
		t.Fatalf("等待元数据失败: %v", err)
	}
	waitFor(t, 60*time.Second, "种子端校验完成", func() bool { return h.Complete() })
	return fmt.Sprintf("127.0.0.1:%d", eng.ListenPort())
}

// 验收核心：不启动任何 GUI，用 CLI 完成一次 BitTorrent 下载。
func TestCLIBitTorrentDownloadEndToEnd(t *testing.T) {
	seedRoot, torrentPath, content := seedLayout(t, "cli-bt-src", 512<<10)
	sum := sha256.Sum256(content)
	peer := startSeeder(t, seedRoot, torrentPath)

	eng := startEmbeddedEngine(t)

	code, out := eng.run(t, "add",
		"--torrent", torrentPath,
		"--name", "cli-bt.bin",
		"--peers", peer,
		"--timeout", "90s")
	if code != ExitOK {
		t.Fatalf("BT add 退出码 = %d，输出:\n%s", code, out)
	}

	// BT 数据按种子内的目录结构落地。
	got, err := os.ReadFile(filepath.Join(eng.dataDir, "cli-bt-src", "payload.bin"))
	if err != nil {
		t.Fatalf("读取 BT 下载结果失败: %v（输出:\n%s）", err, out)
	}
	if sha256.Sum256(got) != sum {
		t.Fatal("CLI 下载的 BT 内容与源不一致")
	}
}

// ---------------------------------------------------------------------------
// 任务操作与设置
// ---------------------------------------------------------------------------

func TestCLIPauseResumeRemove(t *testing.T) {
	// 慢速服务端，保证有足够时间暂停。
	content := make([]byte, 4<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	srv := slowRangeServer(t, content)
	defer srv.Close()

	eng := startEmbeddedEngine(t)

	// 先暂停创建，避免任务立即完成。
	code, out := eng.run(t, "add", srv.URL+"/payload.bin", "--name", "ops.bin", "--paused", "--watch=false")
	if code != ExitOK {
		t.Fatalf("add --paused 退出码 = %d，输出:\n%s", code, out)
	}
	id := extractID(t, out)

	code, out = eng.run(t, "resume", id)
	if code != ExitOK {
		t.Fatalf("resume 退出码 = %d，输出:\n%s", code, out)
	}
	if !strings.Contains(out, string(engine.StatePending)) && !strings.Contains(out, string(engine.StateRunning)) {
		t.Errorf("resume 后状态异常:\n%s", out)
	}

	code, out = eng.run(t, "pause", id)
	if code != ExitOK {
		t.Fatalf("pause 退出码 = %d，输出:\n%s", code, out)
	}
	if !strings.Contains(out, string(engine.StatePaused)) {
		t.Errorf("pause 后状态异常:\n%s", out)
	}

	// recheck 会把任务重新排队。
	code, out = eng.run(t, "recheck", id)
	if code != ExitOK {
		t.Fatalf("recheck 退出码 = %d，输出:\n%s", code, out)
	}

	// 删除（含文件）
	if err := os.WriteFile(filepath.Join(eng.dataDir, "ops.bin"), []byte("x"), 0o644); err != nil {
		t.Fatalf("预置文件失败: %v", err)
	}
	code, out = eng.run(t, "rm", id, "--files")
	if code != ExitOK {
		t.Fatalf("rm 退出码 = %d，输出:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(eng.dataDir, "ops.bin")); !os.IsNotExist(err) {
		t.Error("rm --files 应删除数据文件")
	}

	// 再次删除应失败（退出码非 0）。
	if code, _ = eng.run(t, "rm", id); code != ExitError {
		t.Fatalf("重复删除退出码 = %d, 期望 %d", code, ExitError)
	}

	// 对不存在的任务操作应返回错误。
	if code, _ = eng.run(t, "pause", "nope"); code != ExitError {
		t.Fatalf("暂停不存在的任务退出码 = %d, 期望 %d", code, ExitError)
	}
}

// slowRangeServer 支持 Range 但慢速输出，用于制造「下载进行中」的稳定状态。
func slowRangeServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	size := int64(len(content))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			time.Sleep(20 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCLIStatusAndLimit(t *testing.T) {
	eng := startEmbeddedEngine(t)

	code, out := eng.run(t, "status")
	if code != ExitOK {
		t.Fatalf("status 退出码 = %d，输出:\n%s", code, out)
	}
	for _, want := range []string{"控制面", "任务:", "并发上限", "下载目录", "全局限速"} {
		if !strings.Contains(out, want) {
			t.Errorf("status 输出缺少 %q:\n%s", want, out)
		}
	}

	code, out = eng.run(t, "limit", "1MB")
	if code != ExitOK {
		t.Fatalf("limit 退出码 = %d，输出:\n%s", code, out)
	}
	if !strings.Contains(out, "MB/s") {
		t.Errorf("limit 输出异常:\n%s", out)
	}

	code, out = eng.run(t, "status")
	if !strings.Contains(out, "MB/s") {
		t.Errorf("status 应反映新的限速:\n%s", out)
	}

	// 关闭限速
	if code, _ = eng.run(t, "limit", "0"); code != ExitOK {
		t.Fatalf("limit 0 退出码 = %d", code)
	}

	// 非法速率
	if code, _ = eng.run(t, "limit", "abc"); code != ExitUsage {
		t.Fatalf("非法速率退出码 = %d, 期望 %d", code, ExitUsage)
	}
}

func TestCLIWatchReachesCompletion(t *testing.T) {
	content := make([]byte, 256<<10)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	srv := rangeServer(t, content)

	eng := startEmbeddedEngine(t)

	// 后台创建任务（不等待）。
	code, out := eng.run(t, "add", srv.URL+"/payload.bin", "--name", "watch.bin", "--watch=false")
	if code != ExitOK {
		t.Fatalf("add 退出码 = %d，输出:\n%s", code, out)
	}
	id := extractID(t, out)

	code, out = eng.run(t, "watch", id, "--timeout", "60s")
	if code != ExitOK {
		t.Fatalf("watch 退出码 = %d，输出:\n%s", code, out)
	}
	if !strings.Contains(out, "watch.bin") {
		t.Errorf("watch 输出应包含文件名:\n%s", out)
	}
}

// extractID 从 add 的文本输出中解析任务 ID。
func extractID(t *testing.T, out string) string {
	t.Helper()
	const marker = "已创建任务 "
	idx := strings.Index(out, marker)
	if idx < 0 {
		t.Fatalf("输出中找不到任务 ID:\n%s", out)
	}
	rest := out[idx+len(marker):]
	if line, _, ok := strings.Cut(rest, "\n"); ok {
		return strings.TrimSpace(line)
	}
	return strings.TrimSpace(rest)
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"0":     0,
		"1024":  1024,
		"1KB":   1 << 10,
		"1k":    1 << 10,
		"1.5MB": 1<<20 + (1 << 19),
		"2GB":   2 << 30,
	}
	for in, want := range cases {
		got, err := client.ParseSize(in)
		if err != nil {
			t.Errorf("ParseSize(%q) 失败: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, 期望 %d", in, got, want)
		}
	}
	for _, bad := range []string{"abc", "-1", ""} {
		if _, err := client.ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) 应返回错误", bad)
		}
	}
}
