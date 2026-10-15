// Package control 提供本地控制面：GUI 与 CLI 通过它与下载内核交互。
//
// 设计要点：
//   - **仅绑定回环地址**（默认 127.0.0.1）。控制面能删除文件、修改配置，
//     绝不能暴露到局域网；
//   - **Bearer Token 认证**：本机其他进程（含浏览器页面）不应能随意操纵下载器，
//     令牌写入元数据目录下的 control.token，供同机客户端读取；
//   - **REST + SSE**：普通操作用 REST，进度变化用 Server-Sent Events 推送，
//     避免客户端轮询（轮询在大文件下载时会产生可观的无效流量）。
package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"p2pdl/internal/app"
	"p2pdl/internal/engine"
	"p2pdl/internal/version"
)

// 默认值。
const (
	// DefaultAddr 只绑定回环地址：控制面具有删除文件与改配置的权限。
	DefaultAddr = "127.0.0.1:0"
	// DefaultEventInterval 是进度轮询与事件推送的间隔。
	// 500ms 足以满足「事件在 1 秒内反映进度变化」的验收要求。
	DefaultEventInterval = 500 * time.Millisecond
	// TokenFileName 是控制令牌的文件名（相对元数据目录）。
	TokenFileName = "control.token"
	// DefaultTokenPerm 是令牌文件的权限：仅当前用户可读。
	DefaultTokenPerm = 0o600
)

// Options 配置控制面。
type Options struct {
	// App 是必须的应用内核。
	App *app.App
	// Addr 是监听地址，默认 127.0.0.1:0（系统分配端口）。
	Addr string
	// Token 是访问令牌；为空时自动生成并写入 MetaDir/control.token。
	Token string
	// EventInterval 是事件推送间隔。
	EventInterval time.Duration
	// Logf 为 nil 时静默。
	Logf func(format string, args ...any)
}

func (o *Options) normalize() error {
	if o.App == nil {
		return errors.New("control: 缺少应用内核")
	}
	if o.Addr == "" {
		o.Addr = DefaultAddr
	}
	if o.EventInterval <= 0 {
		o.EventInterval = DefaultEventInterval
	}
	return nil
}

func (o *Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Server 是本地控制面服务。
type Server struct {
	opts  Options
	app   *app.App
	token string

	ln      net.Listener
	httpSrv *http.Server
	hub     *eventHub

	stopOnce sync.Once
	stopped  chan struct{}
	wg       sync.WaitGroup
}

// New 创建控制面（不启动监听）。
func New(opts Options) (*Server, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}

	token := opts.Token
	if token == "" {
		var err error
		token, err = loadOrCreateToken(opts.App)
		if err != nil {
			return nil, err
		}
	}

	s := &Server{
		opts:    opts,
		app:     opts.App,
		token:   token,
		hub:     newEventHub(),
		stopped: make(chan struct{}),
	}
	s.httpSrv = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// Start 启动监听与事件推送循环，返回实际监听地址。
func (s *Server) Start() (string, error) {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return "", fmt.Errorf("control: 监听失败: %w", err)
	}
	s.ln = ln

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.opts.logf("control: 服务退出: %v", err)
		}
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.watchAndBroadcast()
	}()

	addr := ln.Addr().String()
	s.opts.logf("control: 控制面已启动于 http://%s（令牌文件 %s）", addr, s.TokenPath())
	return addr, nil
}

// Addr 返回实际监听地址（Start 之后可用）。
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// URL 返回控制面根地址。
func (s *Server) URL() string {
	addr := s.Addr()
	if addr == "" {
		return ""
	}
	return "http://" + addr
}

// Token 返回访问令牌。
func (s *Server) Token() string { return s.token }

// TokenPath 返回令牌文件路径。
func (s *Server) TokenPath() string {
	return filepath.Join(s.app.Options().MetaDir, TokenFileName)
}

// Stop 关闭控制面并等待 goroutine 退出。
func (s *Server) Stop() error {
	var err error
	s.stopOnce.Do(func() {
		close(s.stopped)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err = s.httpSrv.Shutdown(ctx)
		s.hub.closeAll()
	})
	s.wg.Wait()
	return err
}

// ---------------------------------------------------------------------------
// 路由与中间件
// ---------------------------------------------------------------------------

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// 健康检查不需要认证：用于 GUI 探测引擎是否已就绪。
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)

	mux.Handle("GET /api/v1/version", s.auth(http.HandlerFunc(s.handleVersion)))
	mux.Handle("GET /api/v1/tasks", s.auth(http.HandlerFunc(s.handleListTasks)))
	mux.Handle("POST /api/v1/tasks", s.auth(http.HandlerFunc(s.handleCreateTask)))
	mux.Handle("GET /api/v1/tasks/{id}", s.auth(http.HandlerFunc(s.handleGetTask)))
	mux.Handle("DELETE /api/v1/tasks/{id}", s.auth(http.HandlerFunc(s.handleDeleteTask)))
	mux.Handle("POST /api/v1/tasks/{id}/pause", s.auth(http.HandlerFunc(s.handlePauseTask)))
	mux.Handle("POST /api/v1/tasks/{id}/resume", s.auth(http.HandlerFunc(s.handleResumeTask)))
	mux.Handle("POST /api/v1/tasks/{id}/recheck", s.auth(http.HandlerFunc(s.handleRecheckTask)))
	mux.Handle("GET /api/v1/stats", s.auth(http.HandlerFunc(s.handleStats)))
	mux.Handle("GET /api/v1/settings", s.auth(http.HandlerFunc(s.handleGetSettings)))
	mux.Handle("PATCH /api/v1/settings", s.auth(http.HandlerFunc(s.handlePatchSettings)))
	mux.Handle("GET /api/v1/events", s.auth(http.HandlerFunc(s.handleEvents)))

	return s.recoverPanic(mux)
}

// auth 校验 Bearer 令牌。使用常量时间比较，避免令牌被逐字节推断。
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := bearerToken(r)
		if got == "" {
			writeError(w, http.StatusUnauthorized, "missing_token", "缺少访问令牌")
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeError(w, http.StatusForbidden, "invalid_token", "访问令牌无效")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverPanic 避免单个请求 panic 拖垮整个控制面。
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.opts.logf("control: 请求 %s %s 触发 panic: %v", r.Method, r.URL.Path, rec)
				writeError(w, http.StatusInternalServerError, "internal_error", fmt.Sprintf("内部错误: %v", rec))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	if tok := r.URL.Query().Get("token"); tok != "" {
		// SSE 无法自定义请求头（浏览器 EventSource 限制），因此允许查询参数。
		return tok
	}
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

// ---------------------------------------------------------------------------
// 响应辅助
// ---------------------------------------------------------------------------

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": apiError{Code: code, Message: message}})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", fmt.Sprintf("请求体解析失败: %v", err))
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// 令牌管理
// ---------------------------------------------------------------------------

// loadOrCreateToken 读取（或生成）控制令牌。
//
// 生成的令牌写入元数据目录，权限 0600：同一台机器上的其他用户不应能读取，
// 否则他们就能操纵本用户的下载器。
func loadOrCreateToken(a *app.App) (string, error) {
	metaDir := a.Options().MetaDir
	if metaDir == "" {
		return "", errors.New("control: 无法确定元数据目录")
	}
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return "", fmt.Errorf("control: 创建元数据目录失败: %w", err)
	}
	path := filepath.Join(metaDir, TokenFileName)

	if raw, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(raw)); tok != "" {
			return tok, nil
		}
	}

	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("control: 生成令牌失败: %w", err)
	}
	token := hex.EncodeToString(buf[:])
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), DefaultTokenPerm); err != nil {
		return "", fmt.Errorf("control: 写入令牌失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("control: 提交令牌失败: %w", err)
	}
	return token, nil
}

// ---------------------------------------------------------------------------
// 事件推送
// ---------------------------------------------------------------------------

// EventType 是事件类型常量。
const (
	EventTaskAdded     = "task.added"
	EventTaskUpdated   = "task.updated"
	EventTaskCompleted = "task.completed"
	EventTaskFailed    = "task.failed"
	EventTaskRemoved   = "task.removed"
	EventStats         = "stats"
	EventHeartbeat     = "heartbeat"
)

// Event 是推送给订阅者的消息。
type Event struct {
	Type  string           `json:"type"`
	At    time.Time        `json:"at"`
	Task  *engine.TaskView `json:"task,omitempty"`
	Stats *engine.Stats    `json:"stats,omitempty"`
}

type eventHub struct {
	mu     sync.Mutex
	nextID int
	subs   map[int]chan Event
	closed bool
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[int]chan Event)}
}

// subscribe 注册一个订阅者；缓冲足够小以在慢客户端上主动丢弃事件，
// 而不是让控制面因某个卡住的消费者而积压内存。
func (h *eventHub) subscribe() (int, <-chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	ch := make(chan Event, 64)
	h.subs[id] = ch
	return id, ch
}

func (h *eventHub) unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(ch)
	}
}

func (h *eventHub) broadcast(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for _, ch := range h.subs {
		select {
		case ch <- e:
		default:
			// 订阅者过慢：丢弃本条事件（SSE 客户端会在下一次快照中恢复一致）。
		}
	}
}

func (h *eventHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, ch := range h.subs {
		delete(h.subs, id)
		close(ch)
	}
}

// watchAndBroadcast 周期性对比任务快照并广播差异。
//
// 为什么用「快照对比」而不是让引擎主动上报：会话进度由不同引擎在各自 goroutine
// 中更新，统一入口只有调度器的 List()。500ms 的对比开销在一个小切片上可以忽略。
func (s *Server) watchAndBroadcast() {
	ticker := time.NewTicker(s.opts.EventInterval)
	defer ticker.Stop()

	previous := map[string]engine.TaskView{}

	for {
		select {
		case <-s.stopped:
			return
		case <-ticker.C:
		}

		current := make(map[string]engine.TaskView)
		for _, v := range s.app.Scheduler().List() {
			current[v.ID] = v
		}

		// 不做「首个快照只建基线」的特殊处理：从空基线开始对比，
		// 会把当前已有的任务作为 task.added 广播出去。这样任何时刻连上来的
		// 订阅者都能通过事件流重建完整状态，而不用额外调用一次列表接口；
		// 重复的 added 事件对客户端是幂等的。
		now := time.Now()
		for id, view := range current {
			old, existed := previous[id]
			switch {
			case !existed:
				v := view
				s.hub.broadcast(Event{Type: EventTaskAdded, At: now, Task: &v})
			case old.State != view.State:
				v := view
				eventType := EventTaskUpdated
				switch view.State {
				case engine.StateCompleted:
					eventType = EventTaskCompleted
				case engine.StateFailed:
					eventType = EventTaskFailed
				}
				s.hub.broadcast(Event{Type: eventType, At: now, Task: &v})
			case old.Done != view.Done:
				v := view
				s.hub.broadcast(Event{Type: EventTaskUpdated, At: now, Task: &v})
			}
		}
		for id, view := range previous {
			if _, ok := current[id]; !ok {
				v := view
				s.hub.broadcast(Event{Type: EventTaskRemoved, At: now, Task: &v})
			}
		}

		s.hub.broadcast(Event{Type: EventStats, At: now, Stats: statsOf(s.app)})
		previous = current
	}
}

func statsOf(a *app.App) *engine.Stats {
	st := a.Scheduler().Stats()
	return &st
}

// handleEvents 以 SSE 形式推送事件。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_streaming", "当前运行环境不支持流式响应")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id, ch := s.hub.subscribe()
	defer s.hub.unsubscribe(id)

	// 立即推送一次全量快照，客户端无需额外调用 List 即可渲染界面。
	s.writeSSE(w, flusher, Event{Type: EventStats, At: time.Now(), Stats: statsOf(s.app)})
	for _, view := range s.app.Scheduler().List() {
		v := view
		s.writeSSE(w, flusher, Event{Type: EventTaskAdded, At: time.Now(), Task: &v})
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stopped:
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			s.writeSSE(w, flusher, ev)
		case <-heartbeat.C:
			s.writeSSE(w, flusher, Event{Type: EventHeartbeat, At: time.Now()})
		}
	}
}

func (s *Server) writeSSE(w http.ResponseWriter, flusher http.Flusher, ev Event) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload)
	flusher.Flush()
}

var _ = version.Version
