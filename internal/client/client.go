// Package client 是控制面的 Go 客户端，供 CLI 与（未来的）其他前端复用。
//
// 端点发现：引擎启动后会把「监听地址 + 令牌文件路径」写入
// <MetaDir>/control.endpoint，客户端据此自动连接，无需用户手工配置端口。
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"p2pdl/internal/engine"
)

// 与 control 包约定的文件名。
const (
	// EndpointFileName 是端点描述文件。
	EndpointFileName = "control.endpoint"
	// TokenFileName 是访问令牌文件。
	TokenFileName = "control.token"
)

// Endpoint 是引擎写入的端点描述。
type Endpoint struct {
	Addr      string    `json:"addr"`
	TokenFile string    `json:"tokenFile"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
}

// WriteEndpoint 写出端点描述文件（由引擎调用）。
//
// tokenFile 会先转为绝对路径：引擎可能以相对路径启动（例如 GUI 传的是 "./meta"），
// 而读取端点的一方（CLI、另一个 GUI 实例）工作目录未必相同。
func WriteEndpoint(metaDir, addr, tokenFile string) (string, error) {
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return "", fmt.Errorf("client: 创建元数据目录失败: %w", err)
	}
	if abs, err := filepath.Abs(tokenFile); err == nil {
		tokenFile = abs
	}
	payload, err := json.MarshalIndent(Endpoint{
		Addr:      addr,
		TokenFile: tokenFile,
		PID:       os.Getpid(),
		StartedAt: time.Now().UTC(),
	}, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(metaDir, EndpointFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("client: 写入端点文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("client: 提交端点文件失败: %w", err)
	}
	return path, nil
}

// RemoveEndpoint 删除端点描述文件（由引擎在退出时调用）。
func RemoveEndpoint(metaDir string) {
	_ = os.Remove(filepath.Join(metaDir, EndpointFileName))
}

// ReadEndpoint 读取端点描述文件。
func ReadEndpoint(metaDir string) (*Endpoint, error) {
	raw, err := os.ReadFile(filepath.Join(metaDir, EndpointFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("未发现正在运行的引擎（缺少 %s）", filepath.Join(metaDir, EndpointFileName))
		}
		return nil, err
	}
	var ep Endpoint
	if err := json.Unmarshal(raw, &ep); err != nil {
		return nil, fmt.Errorf("端点文件解析失败: %w", err)
	}
	if ep.Addr == "" {
		return nil, errors.New("端点文件缺少 addr 字段")
	}
	return &ep, nil
}

// Client 是控制面客户端。
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New 用显式地址与令牌创建客户端。
func New(addr, token string) *Client {
	return &Client{
		base:  normalizeBase(addr),
		token: token,
		http:  &http.Client{Timeout: 60 * time.Second},
	}
}

func normalizeBase(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimSuffix(addr, "/")
	}
	return "http://" + strings.TrimSuffix(addr, "/")
}

// Discover 从元数据目录发现并连接正在运行的引擎。
func Discover(metaDir string) (*Client, error) {
	ep, err := ReadEndpoint(metaDir)
	if err != nil {
		return nil, err
	}
	tokenPath := ep.TokenFile
	if tokenPath == "" {
		tokenPath = filepath.Join(metaDir, TokenFileName)
	}
	raw, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("读取访问令牌失败（%s）: %w", tokenPath, err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, fmt.Errorf("访问令牌文件为空: %s", tokenPath)
	}

	c := New(ep.Addr, token)
	// 立刻验证一次连通性，让「引擎没在跑」这类错误在第一条命令处就暴露，
	// 而不是等到某个具体操作时才报「连接被拒绝」。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Health(ctx); err != nil {
		return nil, fmt.Errorf("端点文件 %s 指向 %s，但该地址无响应（引擎可能已退出或正在启动）: %w",
			filepath.Join(metaDir, EndpointFileName), ep.Addr, err)
	}
	return c, nil
}

// BaseURL 返回控制面根地址。
func (c *Client) BaseURL() string { return c.base }

// Token 返回访问令牌。
func (c *Client) Token() string { return c.token }

// ---------------------------------------------------------------------------
// 端点封装
// ---------------------------------------------------------------------------

// apiErrorBody 是控制面的错误响应结构。
type apiErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Error 是客户端用错误类型，携带 HTTP 状态码与控制面错误码。
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("控制面返回 %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("%s (%s)", e.Message, e.Code)
}

// Health 探活，不需要令牌。
func (c *Client) Health(ctx context.Context) error {
	res, err := c.do(ctx, http.MethodGet, "/api/v1/health", nil, false)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("健康检查返回 %d", res.StatusCode)
	}
	return nil
}

// Stats 返回调度器统计。
func (c *Client) Stats(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.getJSON(ctx, "/api/v1/stats", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Version 返回引擎版本信息。
func (c *Client) Version(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.getJSON(ctx, "/api/v1/version", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Settings 返回运行期设置。
func (c *Client) Settings(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.getJSON(ctx, "/api/v1/settings", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetGlobalRateLimit 修改全局限速（字节/秒，0 表示不限速）。
func (c *Client) SetGlobalRateLimit(ctx context.Context, bytesPerSecond int64) (map[string]any, error) {
	body := map[string]any{"globalRateLimit": bytesPerSecond}
	var out map[string]any
	if err := c.sendJSON(ctx, http.MethodPatch, "/api/v1/settings", body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListTasks 返回全部任务。
func (c *Client) ListTasks(ctx context.Context) ([]engine.TaskView, error) {
	var out struct {
		Tasks []engine.TaskView `json:"tasks"`
	}
	if err := c.getJSON(ctx, "/api/v1/tasks", &out); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

// GetTask 返回单个任务。
func (c *Client) GetTask(ctx context.Context, id string) (engine.TaskView, error) {
	var out engine.TaskView
	if err := c.getJSON(ctx, "/api/v1/tasks/"+url.PathEscape(id), &out); err != nil {
		return engine.TaskView{}, err
	}
	return out, nil
}

// AddRequest 是新建任务的参数。
type AddRequest struct {
	Source        string            `json:"source,omitempty"`
	TorrentPath   string            `json:"torrentPath,omitempty"`
	Name          string            `json:"name,omitempty"`
	DestPath      string            `json:"destPath,omitempty"`
	Header        map[string]string `json:"header,omitempty"`
	Peers         []string          `json:"peers,omitempty"`
	ExpectedHash  string            `json:"expectedHash,omitempty"`
	HashAlgorithm string            `json:"hashAlgorithm,omitempty"`
	Paused        bool              `json:"paused,omitempty"`
}

// AddTask 新建任务。
func (c *Client) AddTask(ctx context.Context, req AddRequest) (engine.TaskView, error) {
	var out engine.TaskView
	if err := c.sendJSON(ctx, http.MethodPost, "/api/v1/tasks", req, &out); err != nil {
		return engine.TaskView{}, err
	}
	return out, nil
}

// PauseTask 暂停任务。
func (c *Client) PauseTask(ctx context.Context, id string) (engine.TaskView, error) {
	return c.taskAction(ctx, id, "pause")
}

// ResumeTask 恢复任务。
func (c *Client) ResumeTask(ctx context.Context, id string) (engine.TaskView, error) {
	return c.taskAction(ctx, id, "resume")
}

// RecheckTask 重新校验任务。
func (c *Client) RecheckTask(ctx context.Context, id string) (engine.TaskView, error) {
	return c.taskAction(ctx, id, "recheck")
}

func (c *Client) taskAction(ctx context.Context, id, action string) (engine.TaskView, error) {
	var out engine.TaskView
	path := "/api/v1/tasks/" + url.PathEscape(id) + "/" + action
	if err := c.sendJSON(ctx, http.MethodPost, path, nil, &out); err != nil {
		return engine.TaskView{}, err
	}
	return out, nil
}

// RemoveTask 删除任务。
func (c *Client) RemoveTask(ctx context.Context, id string, deleteFiles bool) error {
	path := "/api/v1/tasks/" + url.PathEscape(id)
	if deleteFiles {
		path += "?files=true"
	}
	res, err := c.do(ctx, http.MethodDelete, path, nil, true)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return decodeIfError(res)
}

// Event 是控制面推送的事件（与 control.Event 的 JSON 结构一致）。
type Event struct {
	Type  string           `json:"type"`
	At    time.Time        `json:"at"`
	Task  *engine.TaskView `json:"task,omitempty"`
	Stats *engine.Stats    `json:"stats,omitempty"`
}

// SubscribeEvents 连接事件流，返回的 channel 在连接结束或 ctx 取消时关闭。
//
// 令牌通过查询参数传递：浏览器 EventSource 无法设置请求头，
// 服务端为此保留了这条通道。
func (c *Client) SubscribeEvents(ctx context.Context) (<-chan Event, error) {
	endpoint := c.base + "/api/v1/events?token=" + url.QueryEscape(c.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")

	// 事件流是长连接，必须使用不带超时的客户端。
	streamClient := &http.Client{}
	res, err := streamClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		return nil, decodeIfError(res)
	}

	out := make(chan Event, 64)
	go func() {
		defer close(out)
		defer res.Body.Close()
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
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// WaitForTask 订阅事件直至任务进入终态，返回最终视图。
// timeout 为 0 表示不设超时。
func (c *Client) WaitForTask(ctx context.Context, id string, timeout time.Duration, onUpdate func(engine.TaskView)) (engine.TaskView, error) {
	events, err := c.SubscribeEvents(ctx)
	if err != nil {
		return engine.TaskView{}, err
	}

	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return engine.TaskView{}, ctx.Err()
		case <-deadline:
			return engine.TaskView{}, fmt.Errorf("等待任务 %s 完成超时", id)
		case ev, ok := <-events:
			if !ok {
				return engine.TaskView{}, errors.New("事件流已断开")
			}
			if ev.Task == nil || ev.Task.ID != id {
				continue
			}
			view := *ev.Task
			if onUpdate != nil {
				onUpdate(view)
			}
			switch view.State {
			case engine.StateCompleted, engine.StateFailed:
				return view, nil
			}
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP 基础设施
// ---------------------------------------------------------------------------

func (c *Client) getJSON(ctx context.Context, path string, dst any) error {
	res, err := c.do(ctx, http.MethodGet, path, nil, true)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := decodeIfError(res); err != nil {
		return err
	}
	return json.NewDecoder(res.Body).Decode(dst)
}

func (c *Client) sendJSON(ctx context.Context, method, path string, body any, dst any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	res, err := c.do(ctx, method, path, reader, true)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := decodeIfError(res); err != nil {
		return err
	}
	if dst == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(dst)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, withAuth bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求控制面失败: %w", err)
	}
	return res, nil
}

// decodeIfError 把非 2xx 响应转换为带结构信息的 *Error。
func decodeIfError(res *http.Response) error {
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	var body apiErrorBody
	if err := json.Unmarshal(raw, &body); err == nil && body.Error.Message != "" {
		return &Error{Status: res.StatusCode, Code: body.Error.Code, Message: body.Error.Message}
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		text = http.StatusText(res.StatusCode)
	}
	return &Error{Status: res.StatusCode, Message: text}
}

// ParseSize 解析 "10MB"、"1.5G"、"512k" 形式的容量，供 CLI 参数使用。
// 无单位时按字节处理。
func ParseSize(s string) (int64, error) {
	v := strings.TrimSpace(strings.ToLower(s))
	if v == "" {
		return 0, errors.New("空的大小表达式")
	}
	units := []struct {
		suffix string
		scale  int64
	}{
		{"tb", 1 << 40}, {"gb", 1 << 30}, {"mb", 1 << 20}, {"kb", 1 << 10},
		{"t", 1 << 40}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}, {"b", 1},
	}
	scale := int64(1)
	for _, u := range units {
		if strings.HasSuffix(v, u.suffix) {
			scale = u.scale
			v = strings.TrimSuffix(v, u.suffix)
			break
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, fmt.Errorf("无法解析大小 %q: %w", s, err)
	}
	if f < 0 {
		return 0, errors.New("大小不能为负数")
	}
	return int64(f * float64(scale)), nil
}
