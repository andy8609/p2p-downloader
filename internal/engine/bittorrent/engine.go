// Package bittorrent 基于 anacrolix/torrent 实现 BitTorrent / 磁力链下载，
// 并把进度桥接到统一的 engine.Task 模型（T1 选型结论：MPL-2.0、纯 Go、无需 CGO）。
//
// 与 HTTP 引擎的语义对齐：
//   - 同样是「加入任务 → 开始/暂停/恢复 → 进度写回 Task」；
//   - 同样把「已完成单元」表达为 engine.Bitmap（此处单元是 piece）；
//   - 暂停保留数据与进度，恢复时不重新下载已完成部分。
//
// 与 HTTP 引擎的本质差异：piece 的完成状态由 BT 库自己的 piece completion 存储
// （默认落盘在 DataDir）维护，本包只负责把它同步到 Task 供上层展示与调度，
// 不接管 BT 的分片调度——那是协议实现的核心价值所在。
package bittorrent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"p2pdl/internal/engine/limiter"
)

// Config 是 BT 引擎的配置。零值即为一组安全默认（随机端口、DHT 与 tracker 全开）。
type Config struct {
	// DataDir 是下载数据的根目录（BT 库的 piece 完成记录也落在这里）。
	DataDir string
	// ListenPort 是 BT 监听端口；0 表示随机。
	ListenPort int
	// DisableDHT 关闭 DHT（本地互测、离线场景建议关闭）。
	DisableDHT bool
	// DisableTrackers 关闭 tracker 通信。
	DisableTrackers bool
	// DisablePEX 关闭 Peer Exchange。
	DisablePEX bool
	// DisableUTP 只使用 TCP。纯 Go uTP 在部分环境有兼容问题时的兜底开关。
	DisableUTP bool
	// EnableWebtorrent 是否启用 WebTorrent（ws://、wss:// 形式的 WebSocket tracker）。
	//
	// 零值 false 表示**禁用**，这是刻意的安全默认：
	//   - 本程序是原生 BT 客户端，不与浏览器端（WebTorrent）互通，这条路径用不上；
	//   - 关掉可少一条对外连接路径，也不让复杂的 WebRTC 栈参与运行时；
	//   - udp/http tracker 与 DHT 完全不受影响，正常 BT 下载能力不变。
	// 仅当确实要连接只提供 wss tracker 的 WebTorrent 资源时才需要打开。
	EnableWebtorrent bool
	// NoUpload 拒绝上传（仅下载方使用；做种方必须为 false）。
	NoUpload bool
	// Seed 在下载完成后继续做种（做种方必须为 true）。
	Seed bool
	// DownloadRateLimit / UploadRateLimit 单位字节/秒，<=0 表示不限速。
	DownloadRateLimit int64
	UploadRateLimit   int64
	// Logf 为 nil 时静默。
	Logf func(format string, args ...any)
}

func (c *Config) clientConfig() *torrent.ClientConfig {
	cc := torrent.NewDefaultClientConfig()
	if c.DataDir != "" {
		cc.DataDir = c.DataDir
	}
	cc.ListenPort = c.ListenPort
	cc.NoDHT = c.DisableDHT
	cc.DisableTrackers = c.DisableTrackers
	cc.DisablePEX = c.DisablePEX
	cc.DisableUTP = c.DisableUTP
	cc.NoUpload = c.NoUpload
	cc.Seed = c.Seed
	// 禁用 WebTorrent 的 ws/wss tracker（库默认是启用）。
	cc.DisableWebtorrent = !c.EnableWebtorrent
	// 默认关闭 UPnP 端口映射：桌面工具不应在未告知用户的情况下改动路由器配置。
	cc.NoDefaultPortForwarding = true

	if c.DownloadRateLimit > 0 {
		cc.DownloadRateLimiter = limiter.NewRateLimiterFor(c.DownloadRateLimit)
	}
	if c.UploadRateLimit > 0 {
		cc.UploadRateLimiter = limiter.NewRateLimiterFor(c.UploadRateLimit)
	}
	return cc
}

func (c *Config) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Engine 持有一个 BT 客户端实例，负责创建/关闭客户端并管理其中的 torrent。
//
// 端口与资源策略：一个进程只创建一个 Engine（一个监听端口、一套 DHT 节点表），
// 所有 BT 任务共享它。这与「每个任务一个客户端」相比，避免了端口冲突、
// 重复的 DHT 引导流量与成倍的连接数；BT 协议本身就支持在单端口上承载多 torrent。
type Engine struct {
	client *torrent.Client
	cfg    Config

	mu      sync.Mutex
	handles map[string]*Torrent // 以 infoHash（hex）为键
	paused  map[string]bool     // 暂停意图（BT 库未提供查询 API）

	// storages 按数据目录缓存 storage 实例。
	//
	// 两个必需的理由：
	//  1. 同一目录必须只打开一个 piece completion 数据库——boltdb 是独占锁，
	//     第二个实例会因拿不到锁而**静默回退到内存实现**，表现为「重启后进度全丢」；
	//  2. 必须由我们持有引用并在 Close 时显式关闭，否则数据库句柄与文件句柄
	//     会一直保留到进程退出（Windows 上还导致文件无法删除）。
	storages map[string]storage.ClientImplCloser
}

// NewEngine 创建并启动 BT 客户端。
func NewEngine(cfg Config) (*Engine, error) {
	cl, err := torrent.NewClient(cfg.clientConfig())
	if err != nil {
		return nil, fmt.Errorf("bittorrent: 创建客户端失败: %w", err)
	}
	e := &Engine{
		client:   cl,
		cfg:      cfg,
		handles:  make(map[string]*Torrent),
		storages: make(map[string]storage.ClientImplCloser),
	}
	e.cfg.logf("bittorrent: 客户端已启动，监听 %s，数据目录 %q，DHT=%v trackers=%v",
		e.listenDescription(), cfg.DataDir, !cfg.DisableDHT, !cfg.DisableTrackers)
	return e, nil
}

// listenDescription 返回一个可读的监听地址描述，用于日志与故障定位。
// BT 客户端的监听是懒启动的，ListenAddrs 在未启动时返回空列表。
func (e *Engine) listenDescription() string {
	addrs := e.client.ListenAddrs()
	if len(addrs) == 0 {
		return fmt.Sprintf("TCP/UDP 端口 %d（未就绪）", e.client.LocalPort())
	}
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, a.String())
	}
	return fmt.Sprintf("%v", parts)
}

// Close 关闭客户端、释放存储句柄与监听端口。
func (e *Engine) Close() error {
	if e == nil || e.client == nil {
		return nil
	}
	var errs []error
	if closeErrs := e.client.Close(); len(closeErrs) > 0 {
		errs = append(errs, closeErrs...)
	}

	e.mu.Lock()
	storages := e.storages
	e.storages = nil
	e.mu.Unlock()

	// 必须先关客户端再关存储：客户端关闭时会做最后的 piece 完成态写入。
	for dir, st := range storages {
		if st == nil {
			continue
		}
		if err := st.Close(); err != nil {
			errs = append(errs, fmt.Errorf("bittorrent: 关闭存储 %s 失败: %w", dir, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// storageFor 返回（并缓存）指定数据目录的 storage 实例。
func (e *Engine) storageFor(dir string) (storage.ClientImpl, error) {
	if dir == "" {
		return nil, errors.New("bittorrent: 数据目录为空，无法初始化存储")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.storages == nil {
		e.storages = make(map[string]storage.ClientImplCloser)
	}
	if st, ok := e.storages[dir]; ok {
		return st, nil
	}
	st := storage.NewFile(dir)
	e.storages[dir] = st
	return st, nil
}

// ListenPort 返回实际监听的端口（配置为 0 时由系统分配）。
func (e *Engine) ListenPort() int {
	if e == nil || e.client == nil {
		return 0
	}
	return e.client.LocalPort()
}

// ListenAddr 返回本地监听地址的字符串形式，可直接作为静态 peer 使用。
// 监听尚未就绪时会先触发一次监听。
func (e *Engine) ListenAddr() string {
	if e == nil || e.client == nil {
		return ""
	}
	addrs := e.client.ListenAddrs()
	if len(addrs) == 0 {
		return ""
	}
	// 优先返回 IPv4 回环可达的地址，便于本地互测直接使用。
	for _, a := range addrs {
		if host, _, err := net.SplitHostPort(a.String()); err == nil && !strings.Contains(host, ":") {
			return a.String()
		}
	}
	return addrs[0].String()
}

// Client 暴露底层客户端，供需要高级能力的调用方使用。
func (e *Engine) Client() *torrent.Client { return e.client }

// normalizeMagnet 清理用户在粘贴磁力链时可能引入的空白。
//
// 实测触发场景（曾导致用户看到「内部错误: is zero」）：
//
//	magnet:? xt=urn:btih:24169F...     ← 问号后多一个空格
//
// 这种情况下 url 解析不会报错，但 `xt` 参数不存在，于是得到一个**全零 infoHash**；
// 库把它交给 AddTorrentSpec 时内部断言失败，异常最终以「内部错误」的形式冒到界面上。
//
// 处理策略：只清理「结构位置」的空白（问号后、参数分隔符后、首尾与换行），
// 不动参数值内部的空格（例如 dn=my file 这类虽不合规但不该被我们改写的写法）。
func normalizeMagnet(raw string) string {
	s := strings.TrimSpace(raw)
	// 多行粘贴会把换行带进来，url.Parse 会直接判定为非法控制字符。
	s = strings.NewReplacer("\r", "", "\n", "", "\t", "").Replace(s)

	const prefix = "magnet:?"
	if !strings.HasPrefix(s, prefix) {
		return s
	}
	query := strings.TrimPrefix(s, prefix)
	// 问号后的空白（`? xt=` → `?xt=`）与参数之间的空白（`& dn=` → `&dn=`）。
	query = strings.TrimLeft(query, " 　")
	query = strings.ReplaceAll(query, "& ", "&")
	query = strings.ReplaceAll(query, "&　", "&")
	return prefix + query
}

// AddSpec 描述一个待加入的 BT 任务。
type AddSpec struct {
	// Magnet 是磁力链；与 TorrentFile / TorrentBytes 三选一。
	Magnet string
	// TorrentFile 是 .torrent 文件路径。
	TorrentFile string
	// TorrentBytes 是 .torrent 文件内容。
	TorrentBytes []byte
	// SavePath 覆盖 Config.DataDir，作为该 torrent 的数据目录。
	SavePath string
	// PeerAddrs 是静态 peer 列表（host:port）。用于无 tracker/DHT 的场景
	// （本地互测、局域网直连）或加速初始 peer 发现。
	PeerAddrs []string
	// Trackers 追加的 tracker 地址。
	Trackers []string
}

// Add 把磁力链或种子转化为一个受管理的 Torrent。
func (e *Engine) Add(spec AddSpec) (*Torrent, error) {
	ts, err := e.buildSpec(spec)
	if err != nil {
		return nil, err
	}
	t, isNew, err := e.client.AddTorrentSpec(ts)
	if err != nil {
		return nil, fmt.Errorf("bittorrent: 加入 torrent 失败: %w", err)
	}
	hash := t.InfoHash().HexString()
	e.cfg.logf("bittorrent: 已加入 torrent %s（新增=%v）", hash, isNew)

	h := &Torrent{engine: e, t: t}
	e.mu.Lock()
	e.handles[hash] = h
	e.mu.Unlock()
	return h, nil
}

func (e *Engine) buildSpec(spec AddSpec) (*torrent.TorrentSpec, error) {
	var ts *torrent.TorrentSpec
	switch {
	case spec.Magnet != "":
		normalized := normalizeMagnet(spec.Magnet)
		parsed, err := torrent.TorrentSpecFromMagnetUri(normalized)
		if err != nil {
			return nil, fmt.Errorf("bittorrent: 解析磁力链失败: %w（请确认链接完整、无换行）", err)
		}
		// 库在「xt 参数缺失或格式不对」时不报错而是返回零值 infoHash，
		// 随后会在 AddTorrentSpec 内部断言失败。这里必须自己拦住，
		// 换成能指向问题所在的错误信息。
		if parsed.InfoHash.IsZero() && !parsed.InfoHashV2.Ok {
			return nil, errors.New("bittorrent: 磁力链里没有可识别的 infoHash，" +
				"请检查 xt=urn:btih: 之后的 40 位哈希是否完整、链接中是否混入了空格或换行")
		}
		ts = parsed
	case spec.TorrentFile != "":
		mi, err := metainfo.LoadFromFile(spec.TorrentFile)
		if err != nil {
			return nil, fmt.Errorf("bittorrent: 读取种子文件失败: %w", err)
		}
		ts = torrent.TorrentSpecFromMetaInfo(mi)
	case len(spec.TorrentBytes) > 0:
		mi, err := metainfo.Load(bytes.NewReader(spec.TorrentBytes))
		if err != nil {
			return nil, fmt.Errorf("bittorrent: 解析种子内容失败: %w", err)
		}
		ts = torrent.TorrentSpecFromMetaInfo(mi)
	default:
		return nil, errors.New("bittorrent: 需要提供磁力链或种子文件")
	}

	if spec.SavePath != "" {
		// 调用方指定了独立数据目录。
		st, err := e.storageFor(spec.SavePath)
		if err != nil {
			return nil, err
		}
		ts.Storage = st
	} else if ts.Storage == nil {
		// 显式注入 storage，而不是依赖 bt 库按 DataDir 自行创建：
		// 只有这样我们才能在 Engine.Close 时释放 piece completion 数据库句柄。
		if e.cfg.DataDir == "" {
			return nil, errors.New("bittorrent: 未指定 DataDir 或 SavePath")
		}
		st, err := e.storageFor(e.cfg.DataDir)
		if err != nil {
			return nil, err
		}
		ts.Storage = st
	}
	if len(spec.PeerAddrs) > 0 {
		ts.PeerAddrs = append(ts.PeerAddrs, spec.PeerAddrs...)
	}
	if len(spec.Trackers) > 0 {
		ts.Trackers = append(ts.Trackers, [][]string{spec.Trackers}...)
	}
	return ts, nil
}

// Handle 返回已加入的 torrent（按 infoHash 十六进制）。
func (e *Engine) Handle(infoHash string) (*Torrent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h, ok := e.handles[infoHash]
	return h, ok
}

// Handles 返回全部已加入的 torrent。
func (e *Engine) Handles() []*Torrent {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Torrent, 0, len(e.handles))
	for _, h := range e.handles {
		out = append(out, h)
	}
	return out
}

// Drop 从客户端移除 torrent 并停止其所有连接。
func (e *Engine) Drop(infoHash string) error {
	e.mu.Lock()
	h, ok := e.handles[infoHash]
	delete(e.handles, infoHash)
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("bittorrent: 未找到 torrent %s", infoHash)
	}
	h.t.Drop()
	return nil
}

// WaitAll 阻塞直到所有 torrent 完成（用于做种端与测试收尾）。
func (e *Engine) WaitAll() bool {
	if e == nil || e.client == nil {
		return true
	}
	return e.client.WaitAll()
}

// ContextFromDone 把 BT 库的完成信号转换为 context 取消，
// 便于与 HTTP 引擎统一用 context 控制生命周期。
func ContextFromDone(parent context.Context, done <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
