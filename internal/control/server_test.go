package control

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"p2pdl/internal/app"
	"p2pdl/internal/engine"
)

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "p2pdl-control-")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func rangeServer(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "payload.bin", time.Time{}, bytes.NewReader(content))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testEnv 是一套「应用内核 + 控制面」的测试环境。
type testEnv struct {
	app    *app.App
	server *Server
	client *http.Client
	base   string
	token  string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := tempDir(t)
	a, err := app.New(app.Options{
		DataDir:            dir,
		MetaDir:            filepath.Join(dir, "meta"),
		ChunkSize:          64 << 10,
		Concurrency:        4,
		MaxConcurrentTasks: 2,
		SkipSpaceCheck:     true,
	})
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := a.Start(ctx); err != nil {
		t.Fatalf("启动应用失败: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = a.Stop()
	})

	srv, err := New(Options{App: a, EventInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("创建控制面失败: %v", err)
	}
	addr, err := srv.Start()
	if err != nil {
		t.Fatalf("启动控制面失败: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	return &testEnv{
		app:    a,
		server: srv,
		client: &http.Client{Timeout: 30 * time.Second},
		base:   "http://" + addr,
		token:  srv.Token(),
	}
}

// request 发起一次带认证的请求。
func (e *testEnv) request(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.base+path, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)

	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s 返回非 JSON（%d）：%s", method, path, res.StatusCode, raw)
		}
	}
	return res.StatusCode, out
}

func (e *testEnv) mustRequest(t *testing.T, method, path string, body any, wantStatus int) map[string]any {
	t.Helper()
	status, out := e.request(t, method, path, body)
	if status != wantStatus {
		t.Fatalf("%s %s 状态码 = %d, 期望 %d（响应 %v）", method, path, status, wantStatus, out)
	}
	return out
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

// ---------------------------------------------------------------------------
// 基础端与认证
// ---------------------------------------------------------------------------

func TestHealthNeedsNoTokenButOthersDo(t *testing.T) {
	env := newTestEnv(t)

	// 无令牌也能探活：GUI 需要它来判断引擎是否就绪。
	res, err := env.client.Get(env.base + "/api/v1/health")
	if err != nil {
		t.Fatalf("健康检查失败: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("健康检查状态码 = %d", res.StatusCode)
	}

	// 其他端点必须要求令牌。
	res2, err := env.client.Get(env.base + "/api/v1/tasks")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("缺少令牌应返回 401, 实际 %d", res2.StatusCode)
	}

	// 错误令牌应被拒绝。
	req, _ := http.NewRequest("GET", env.base+"/api/v1/tasks", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	res3, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusForbidden {
		t.Fatalf("错误令牌应返回 403, 实际 %d", res3.StatusCode)
	}
}

// 验收核心：仅本机可访问。
func TestServerBindsLoopbackOnly(t *testing.T) {
	env := newTestEnv(t)
	host, port, err := net.SplitHostPort(env.server.Addr())
	if err != nil {
		t.Fatalf("解析地址失败: %v", err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("监听地址 = %q, 期望 127.0.0.1（控制面不得暴露到局域网）", host)
	}

	// 从非回环的本机地址连同一个端口必须失败。
	extIP := firstNonLoopbackIPv4(t)
	if extIP == "" {
		t.Skip("本机没有非回环 IPv4 地址，跳过外部可达性检查")
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(extIP, port), 2*time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("控制面在 %s:%s 上可达，存在被局域网访问的风险", extIP, port)
	}
	t.Logf("从 %s 连接 %s 预期失败: %v", extIP, port, err)
}

func firstNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if v4 := ipnet.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

func TestTokenPersistedWithRestrictivePermissions(t *testing.T) {
	env := newTestEnv(t)

	raw, err := os.ReadFile(env.server.TokenPath())
	if err != nil {
		t.Fatalf("读取令牌文件失败: %v", err)
	}
	if strings.TrimSpace(string(raw)) != env.token {
		t.Fatalf("令牌文件内容与服务器不一致")
	}
	if len(env.token) != 64 {
		t.Fatalf("令牌长度 = %d, 期望 64 位十六进制", len(env.token))
	}

	// 同一元数据目录重建控制面应复用同一个令牌（GUI 与 CLI 才能互通）。
	srv2, err := New(Options{App: env.app})
	if err != nil {
		t.Fatalf("重建控制面失败: %v", err)
	}
	defer srv2.Stop()
	if srv2.Token() != env.token {
		t.Fatal("重建后令牌应保持不变")
	}
}

// ---------------------------------------------------------------------------
// 任务生命周期
// ---------------------------------------------------------------------------

func TestTaskLifecycleOverHTTP(t *testing.T) {
	content := make([]byte, 256<<10)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	sum := sha256.Sum256(content)
	srv := rangeServer(t, content)
	env := newTestEnv(t)

	// 创建
	created := env.mustRequest(t, "POST", "/api/v1/tasks", map[string]any{
		"source":       srv.URL + "/payload.bin",
		"name":         "api.bin",
		"expectedHash": hex.EncodeToString(sum[:]),
	}, http.StatusCreated)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("创建任务应返回 id: %v", created)
	}
	if created["kind"] != string(engine.SourceHTTP) {
		t.Errorf("kind = %v", created["kind"])
	}

	// 列表
	list := env.mustRequest(t, "GET", "/api/v1/tasks", nil, http.StatusOK)
	if total, _ := list["total"].(float64); int(total) != 1 {
		t.Fatalf("任务总数 = %v, 期望 1", list["total"])
	}

	// 详情
	detail := env.mustRequest(t, "GET", "/api/v1/tasks/"+id, nil, http.StatusOK)
	if detail["id"] != id {
		t.Fatalf("详情 id 不匹配: %v", detail)
	}

	// 等待完成
	waitFor(t, 30*time.Second, "任务完成", func() bool {
		status, out := env.request(t, "GET", "/api/v1/tasks/"+id, nil)
		return status == http.StatusOK && out["state"] == string(engine.StateCompleted)
	})

	// 删除（含文件）
	deleted := env.mustRequest(t, "DELETE", "/api/v1/tasks/"+id+"?files=true", nil, http.StatusOK)
	if removed, _ := deleted["removed"].(bool); !removed {
		t.Fatalf("删除响应异常: %v", deleted)
	}
	if _, err := os.Stat(filepath.Join(env.app.Options().DataDir, "api.bin")); !os.IsNotExist(err) {
		t.Error("files=true 时数据文件应被删除")
	}

	// 删除不存在的任务
	status, out := env.request(t, "DELETE", "/api/v1/tasks/"+id, nil)
	if status != http.StatusConflict {
		t.Fatalf("删除不存在的任务状态码 = %d, 期望 409（%v）", status, out)
	}
}

func TestPauseResumeRecheckOverHTTP(t *testing.T) {
	content := make([]byte, 1<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	srv := rangeServer(t, content)
	env := newTestEnv(t)

	created := env.mustRequest(t, "POST", "/api/v1/tasks", map[string]any{
		"source": srv.URL + "/payload.bin",
		"name":   "pause.bin",
		"paused": true,
	}, http.StatusCreated)
	id := created["id"].(string)

	if state := created["state"]; state != string(engine.StatePaused) {
		t.Fatalf("创建时 paused=true 应以暂停态入队, 实际 %v", state)
	}

	// 恢复
	resumed := env.mustRequest(t, "POST", "/api/v1/tasks/"+id+"/resume", nil, http.StatusOK)
	if resumed["state"] == string(engine.StatePaused) {
		t.Fatalf("恢复后状态仍为暂停: %v", resumed)
	}

	// 暂停
	waitFor(t, 20*time.Second, "任务开始运行", func() bool {
		status, out := env.request(t, "GET", "/api/v1/tasks/"+id, nil)
		return status == http.StatusOK && out["state"] == string(engine.StateRunning)
	})
	paused := env.mustRequest(t, "POST", "/api/v1/tasks/"+id+"/pause", nil, http.StatusOK)
	if paused["state"] != string(engine.StatePaused) {
		t.Fatalf("暂停后状态 = %v", paused["state"])
	}

	// 重新校验（等价于重新排队并校验）
	env.mustRequest(t, "POST", "/api/v1/tasks/"+id+"/recheck", nil, http.StatusOK)

	// 不存在的任务
	status, out := env.request(t, "POST", "/api/v1/tasks/nope/pause", nil)
	if status != http.StatusConflict {
		t.Fatalf("暂停不存在的任务状态码 = %d（%v）", status, out)
	}
}

func TestCreateTaskValidation(t *testing.T) {
	env := newTestEnv(t)
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"缺少来源", map[string]any{}, http.StatusBadRequest},
		{"非法协议", map[string]any{"source": "ftp://example.com/a"}, http.StatusBadRequest},
		{"不存在的种子", map[string]any{"torrentPath": filepath.Join(tempDir(t), "missing.torrent")}, http.StatusBadRequest},
		{"非法摘要长度", map[string]any{"source": "http://127.0.0.1:1/a", "expectedHash": "abcd"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, out := env.request(t, "POST", "/api/v1/tasks", c.body)
			if status != c.want {
				t.Fatalf("状态码 = %d, 期望 %d（%v）", status, c.want, out)
			}
			if _, ok := out["error"]; !ok {
				t.Fatalf("错误响应应包含 error 字段: %v", out)
			}
		})
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	env := newTestEnv(t)
	req, _ := http.NewRequest("POST", env.base+"/api/v1/tasks",
		strings.NewReader(`{"source":"http://x/a","bogusField":1}`))
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知字段应被拒绝（严格解析）, 实际 %d", res.StatusCode)
	}
}

func TestBadJSONBodyRejected(t *testing.T) {
	env := newTestEnv(t)
	req, _ := http.NewRequest("POST", env.base+"/api/v1/tasks", strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+env.token)
	res, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应返回 400, 实际 %d", res.StatusCode)
	}
}

func TestNotFoundHandlers(t *testing.T) {
	env := newTestEnv(t)
	status, _ := env.request(t, "GET", "/api/v1/tasks/nope", nil)
	if status != http.StatusNotFound {
		t.Fatalf("查询不存在的任务状态码 = %d", status)
	}
	status, _ = env.request(t, "POST", "/api/v1/tasks/nope/resume", nil)
	if status != http.StatusConflict {
		t.Fatalf("恢复不存在的任务状态码 = %d", status)
	}
}

// ---------------------------------------------------------------------------
// 设置
// ---------------------------------------------------------------------------

func TestSettingsReadAndPatch(t *testing.T) {
	env := newTestEnv(t)

	settings := env.mustRequest(t, "GET", "/api/v1/settings", nil, http.StatusOK)
	if settings["dataDir"] == "" {
		t.Fatalf("设置应包含 dataDir: %v", settings)
	}
	if settings["chunkSize"].(float64) != float64(64<<10) {
		t.Errorf("chunkSize = %v", settings["chunkSize"])
	}

	patched := env.mustRequest(t, "PATCH", "/api/v1/settings",
		map[string]any{"globalRateLimit": 123456}, http.StatusOK)
	if patched["globalRateLimit"].(float64) != 123456 {
		t.Fatalf("限速修改未生效: %v", patched["globalRateLimit"])
	}
	if env.app.GlobalRateLimit() != 123456 {
		t.Fatalf("内核限速 = %d", env.app.GlobalRateLimit())
	}

	// 关闭限速
	patched = env.mustRequest(t, "PATCH", "/api/v1/settings",
		map[string]any{"globalRateLimit": 0}, http.StatusOK)
	if patched["globalRateLimit"].(float64) != 0 {
		t.Fatalf("关闭限速未生效: %v", patched["globalRateLimit"])
	}

	// 负数被拒绝
	status, _ := env.request(t, "PATCH", "/api/v1/settings", map[string]any{"globalRateLimit": -1})
	if status != http.StatusBadRequest {
		t.Fatalf("负限速应返回 400, 实际 %d", status)
	}
}

func TestStatsEndpoint(t *testing.T) {
	env := newTestEnv(t)
	out := env.mustRequest(t, "GET", "/api/v1/stats", nil, http.StatusOK)
	tasks, ok := out["tasks"].(map[string]any)
	if !ok {
		t.Fatalf("stats 应包含 tasks 对象: %v", out)
	}
	if tasks["maxConcurrent"].(float64) != 2 {
		t.Fatalf("maxConcurrent = %v, 期望 2", tasks["maxConcurrent"])
	}
}

func TestVersionEndpoint(t *testing.T) {
	env := newTestEnv(t)
	out := env.mustRequest(t, "GET", "/api/v1/version", nil, http.StatusOK)
	for _, key := range []string{"version", "commit", "go", "platform"} {
		if _, ok := out[key]; !ok {
			t.Errorf("版本响应缺少字段 %s: %v", key, out)
		}
	}
}

// ---------------------------------------------------------------------------
// 事件推送
// ---------------------------------------------------------------------------

// 验收核心：事件推送能在 1 秒内反映进度变化。
func TestEventStreamPushesProgressWithinOneSecond(t *testing.T) {
	// 慢速服务端，保证下载过程持续数秒以便观察多次进度事件。
	content := make([]byte, 8<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成数据失败: %v", err)
	}
	srv := slowRangeServer(t, content)
	defer srv.Close()

	env := newTestEnv(t)

	// 建立 SSE 连接。
	req, err := http.NewRequest("GET", env.base+"/api/v1/events?token="+env.token, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("连接事件流失败: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("事件流状态码 = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	events := make(chan Event, 256)
	go func() {
		scanner := bufio.NewScanner(res.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var eventType string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				eventType = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var ev Event
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
					continue
				}
				if ev.Type == "" {
					ev.Type = eventType
				}
				events <- ev
			}
		}
	}()

	// 等首批快照事件到达（证明流已就绪）。
	waitFor(t, 10*time.Second, "收到初始事件", func() bool {
		select {
		case ev := <-events:
			return ev.Type == EventStats || ev.Type == EventTaskAdded
		default:
			return false
		}
	})

	// 创建任务。
	env.mustRequest(t, "POST", "/api/v1/tasks", map[string]any{
		"source": srv.URL + "/payload.bin",
		"name":   "events.bin",
	}, http.StatusCreated)

	// 必须收到 task.added。
	sawAdded := false
	addedDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(addedDeadline) && !sawAdded {
		select {
		case ev := <-events:
			if ev.Type == EventTaskAdded {
				sawAdded = true
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
	if !sawAdded {
		t.Fatal("未收到 task.added 事件")
	}

	// 验收核心：进度变化必须在 1 秒内被推送。
	//
	// 判定方式：统计下载过程中相邻两次「进度推进」事件的时间间隔。
	// 事件推送间隔为 100ms，因此正常情况下间隔远小于 1 秒；
	// 若实现退化为「只在结束时推一次」，则该间隔会接近任务总时长。
	var (
		lastDone   int64 = -1
		lastAt     time.Time
		maxGap     time.Duration
		sawUpdate  bool
	)
	pollStart := time.Now()
	for time.Since(pollStart) < 25*time.Second {
		select {
		case ev := <-events:
			if ev.Task == nil {
				continue
			}
			// 完成事件同样携带最终进度，一并计入。
			if ev.Type != EventTaskUpdated && ev.Type != EventTaskCompleted {
				continue
			}
			done := ev.Task.Done
			if lastDone >= 0 && done > lastDone {
				if gap := time.Since(lastAt); gap > maxGap {
					maxGap = gap
				}
				sawUpdate = true
			}
			lastDone = done
			lastAt = time.Now()
			if ev.Type == EventTaskCompleted {
				if !sawUpdate {
					t.Fatal("直到任务完成都未收到任何进度推进事件")
				}
				t.Logf("最大进度事件间隔 = %v（事件推送间隔 100ms）", maxGap)
				if maxGap > time.Second {
					t.Fatalf("进度事件间隔 %v 超过 1 秒，无法及时反映进度变化", maxGap)
				}
				return
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("在 25 秒内未收到完成事件（最后 done=%d）", lastDone)
}

// slowRangeServer 支持 Range 但慢速输出，用于制造持续数十毫秒以上的进度变化，
// 使「进度事件」有足够时间被 SSE 推送出去。
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
			time.Sleep(25 * time.Millisecond)
		}
	})
	srv := httptest.NewServer(handler)
	return srv
}

func TestEventStreamRequiresToken(t *testing.T) {
	env := newTestEnv(t)
	res, err := env.client.Get(env.base + "/api/v1/events")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("事件流无令牌应返回 401, 实际 %d", res.StatusCode)
	}
}

func TestServerStopIsIdempotent(t *testing.T) {
	dir := tempDir(t)
	a, err := app.New(app.Options{DataDir: dir, MetaDir: filepath.Join(dir, "m"), SkipSpaceCheck: true})
	if err != nil {
		t.Fatalf("创建应用失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer a.Stop()

	srv, err := New(Options{App: a})
	if err != nil {
		t.Fatalf("创建控制面失败: %v", err)
	}
	if _, err := srv.Start(); err != nil {
		t.Fatalf("启动控制面失败: %v", err)
	}
	if err := srv.Stop(); err != nil {
		t.Fatalf("第一次 Stop 失败: %v", err)
	}
	if err := srv.Stop(); err != nil {
		t.Fatalf("重复 Stop 应幂等: %v", err)
	}
}

func TestNewRequiresApp(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("缺少应用内核应被拒绝")
	}
}
