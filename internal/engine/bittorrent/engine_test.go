package bittorrent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"p2pdl/internal/engine"
)


// tempDir 创建测试目录并在结束时尽力清理。
//
// 为什么不用 t.TempDir()：anacrolix/torrent 的文件存储层会在库内部保留文件句柄
// （hash check 与读写缓存），Windows 上这会阻止目录删除，而 t.TempDir() 的清理失败
// 会直接把测试判为 FAIL——那是库的资源管理特性，不是本包的功能缺陷。
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "p2pdl-bt-")
	if err != nil {
		t.Fatalf("创建临时目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

const (
	testPieceLength = 64 << 10
	testPayloadSize = 1 << 20
	testTimeout     = 60 * time.Second
)

// localConfig 返回一套「完全离线」的配置：无 DHT、无 tracker、仅 TCP，
// 使测试结果只取决于本实现，不受公网 peer 发现影响。
func localConfig(dataDir string) Config {
	return Config{
		DataDir:         dataDir,
		DisableDHT:      true,
		DisableTrackers: true,
		DisablePEX:      true,
		DisableUTP:      true,
	}
}

// seedLayout 创建种子端布局：<root>/<name>/<file>，并返回 root 与种子文件路径。
func seedLayout(t *testing.T, name string, size int) (root string, payloadPath string, torrentPath string) {
	t.Helper()
	root = tempDir(t)
	payloadDir := filepath.Join(root, name)
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}

	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("生成测试数据失败: %v", err)
	}
	payloadPath = filepath.Join(payloadDir, "payload.bin")
	if err := os.WriteFile(payloadPath, content, 0o644); err != nil {
		t.Fatalf("写入源文件失败: %v", err)
	}

	info := metainfo.Info{PieceLength: testPieceLength}
	if err := info.BuildFromFilePath(payloadDir); err != nil {
		t.Fatalf("生成 info 失败: %v", err)
	}
	mi := metainfo.MetaInfo{}
	// 不设置 Announce：本地互测通过静态 peer 直连，不依赖 tracker。
	raw, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("序列化 info 失败: %v", err)
	}
	mi.InfoBytes = raw

	torrentPath = filepath.Join(root, name+".torrent")
	f, err := os.Create(torrentPath)
	if err != nil {
		t.Fatalf("创建种子文件失败: %v", err)
	}
	defer f.Close()
	if err := mi.Write(f); err != nil {
		t.Fatalf("写入种子文件失败: %v", err)
	}
	return root, payloadPath, torrentPath
}

func fileDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return sha256.Sum256(data)
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时（%v）", what, timeout)
}

// startSeeder 启动一个已完成数据校验、正在做种的客户端。
func startSeeder(t *testing.T, seedRoot, torrentPath string) *Engine {
	t.Helper()
	cfg := localConfig(seedRoot)
	cfg.Seed = true
	engine, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("创建种子端失败: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	h, err := engine.Add(AddSpec{TorrentFile: torrentPath})
	if err != nil {
		t.Fatalf("种子端加入 torrent 失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := h.WaitInfo(ctx); err != nil {
		t.Fatalf("种子端等待元数据失败: %v", err)
	}
	// 种子端需要先对已有文件做校验（hash check），完成后才能提供服务。
	waitUntil(t, testTimeout, "种子端数据校验完成", func() bool { return h.Complete() })
	return engine
}

func leecherPeerAddr(seeder *Engine) string {
	// 监听地址可能是 0.0.0.0/[::]，直接作为 peer 不可连接；
	// 本地互测统一用回环地址 + 实际端口。
	return fmt.Sprintf("127.0.0.1:%d", seeder.ListenPort())
}

// 验收核心：本地双客户端 + 静态 peer 互测（种子文件路径）。
func TestLocalSeederAndLeecherViaTorrentFile(t *testing.T) {
	seedRoot, payloadPath, torrentPath := seedLayout(t, "src", testPayloadSize)
	wantSum := fileDigest(t, payloadPath)

	seeder := startSeeder(t, seedRoot, torrentPath)
	t.Logf("种子端监听 %s", leecherPeerAddr(seeder))

	leechRoot := tempDir(t)
	cfg := localConfig(leechRoot)
	cfg.NoUpload = true
	leecher, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("创建下载端失败: %v", err)
	}
	defer leecher.Close()

	h, err := leecher.Add(AddSpec{
		TorrentFile: torrentPath,
		PeerAddrs:   []string{leecherPeerAddr(seeder)},
	})
	if err != nil {
		t.Fatalf("下载端加入 torrent 失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := h.Download(ctx); err != nil {
		t.Fatalf("下载失败: %v", err)
	}

	gotPath := filepath.Join(leechRoot, "src", "payload.bin")
	if gotSum := fileDigest(t, gotPath); gotSum != wantSum {
		t.Fatalf("下载文件 SHA-256 不匹配\n得到 %x\n期望 %x", gotSum, wantSum)
	}

	// 进度必须能桥接到统一任务模型。
	task, err := engine.NewTask("bt-1", engine.SourceBitTorrent, h.MagnetURI(),
		gotPath, leechRoot, 0, 1<<20, 0)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	if err := h.ApplyToTask(task); err != nil {
		t.Fatalf("ApplyToTask 失败: %v", err)
	}
	if task.InfoHash == "" {
		t.Error("任务应记录 infoHash")
	}
	if task.PieceBitmap == nil || !task.PieceBitmap.Complete() {
		t.Error("完成后 piece 位图应全部置位")
	}
	if task.TotalSize != int64(testPayloadSize) {
		t.Errorf("TotalSize = %d, 期望 %d", task.TotalSize, testPayloadSize)
	}
	if task.State != engine.StateCompleted {
		t.Errorf("State = %q, 期望 %q", task.State, engine.StateCompleted)
	}
}

// 验收核心：磁力链路径（元数据经 BEP 9 从 peer 获取）。
func TestLocalMagnetRoundTrip(t *testing.T) {
	seedRoot, payloadPath, torrentPath := seedLayout(t, "magnet-src", testPayloadSize)
	wantSum := fileDigest(t, payloadPath)

	seeder := startSeeder(t, seedRoot, torrentPath)

	// 从种子文件读出 infoHash，构造磁力链。
	mi, err := metainfo.LoadFromFile(torrentPath)
	if err != nil {
		t.Fatalf("读取种子失败: %v", err)
	}
	magnet := mi.Magnet(nil, nil).String()
	t.Logf("磁力链: %s", magnet)

	leechRoot := tempDir(t)
	cfg := localConfig(leechRoot)
	cfg.NoUpload = true
	leecher, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("创建下载端失败: %v", err)
	}
	defer leecher.Close()

	h, err := leecher.Add(AddSpec{
		Magnet:    magnet,
		PeerAddrs: []string{leecherPeerAddr(seeder)},
	})
	if err != nil {
		t.Fatalf("加入磁力链失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// 磁力链在拿到元数据前 Name/Length 都是空的，WaitInfo 后才有。
	if err := h.WaitInfo(ctx); err != nil {
		t.Fatalf("磁力链获取元数据失败: %v", err)
	}
	if h.Name() == "" {
		t.Error("获取元数据后应有名称")
	}
	if err := h.Download(ctx); err != nil {
		t.Fatalf("磁力链下载失败: %v", err)
	}

	gotPath := filepath.Join(leechRoot, "magnet-src", "payload.bin")
	if gotSum := fileDigest(t, gotPath); gotSum != wantSum {
		t.Fatalf("磁力链下载文件 SHA-256 不匹配\n得到 %x\n期望 %x", gotSum, wantSum)
	}
}

// 验收核心：BT 任务暂停后重启能续传而非重下。
func TestPauseAndRestartKeepsProgress(t *testing.T) {
	seedRoot, payloadPath, torrentPath := seedLayout(t, "resume-src", 4<<20)
	wantSum := fileDigest(t, payloadPath)

	seeder := startSeeder(t, seedRoot, torrentPath)
	peer := leecherPeerAddr(seeder)
	leechRoot := tempDir(t)

	// 第一段：下载一部分后取消（模拟暂停/退出）。
	cfg := localConfig(leechRoot)
	cfg.NoUpload = true
	first, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("创建下载端失败: %v", err)
	}
	h1, err := first.Add(AddSpec{TorrentFile: torrentPath, PeerAddrs: []string{peer}})
	if err != nil {
		t.Fatalf("加入 torrent 失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := h1.WaitInfo(ctx); err != nil {
		t.Fatalf("等待元数据失败: %v", err)
	}

	// 跑一会拿到进度后取消。
	runCtx, runCancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- h1.Download(runCtx) }()

	// 等到至少完成 2 个 piece 再取消，保证「已完成 piece」有可验证的量。
	waitUntil(t, testTimeout, "首轮下载完成 2 个 piece", func() bool {
		return h1.Snapshot().PiecesDone >= 2
	})
	runCancel()
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Fatalf("首轮下载错误: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("取消后下载未及时返回")
	}

	h1.Pause()
	if !h1.Paused() {
		t.Error("Pause 后 Paused() 应为 true")
	}
	// 用「已完成 piece 数」而不是「已下载字节数」作为比较基准：
	// BytesCompleted 包含未完成 piece 的脏 chunk，重启后只能恢复到 piece 边界。
	snap1 := h1.Snapshot()
	pieces1 := snap1.PiecesDone
	if pieces1 == 0 {
		t.Fatalf("首轮应至少完成 1 个 piece（已下载 %d 字节）", snap1.CompletedBytes)
	}
	if snap1.Complete {
		t.Skip("首轮已下载完成，无法验证续传")
	}
	t.Logf("首轮已完成 %d/%d 个 piece（%d 字节），关闭客户端",
		pieces1, snap1.PieceCount, snap1.CompletedBytes)
	if err := first.Close(); err != nil {
		t.Fatalf("关闭首轮客户端失败: %v", err)
	}

	// 第二段：重启客户端，用同一数据目录，应保留已完成 piece。
	second, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("重启下载端失败: %v", err)
	}
	defer second.Close()

	h2, err := second.Add(AddSpec{TorrentFile: torrentPath, PeerAddrs: []string{peer}})
	if err != nil {
		t.Fatalf("重启后加入 torrent 失败: %v", err)
	}
	if err := h2.WaitInfo(ctx); err != nil {
		t.Fatalf("重启后等待元数据失败: %v", err)
	}
	// 重启后应先从 completion 存储恢复已完成的 piece，而不是从 0 开始。
	// 这里不主动触发下载：若需要重新下载才能达到 pieces1，说明进度根本没恢复。
	waitUntil(t, 30*time.Second, "重启后恢复已完成 piece", func() bool {
		return h2.Snapshot().PiecesDone >= pieces1
	})
	restored := h2.Snapshot().PiecesDone
	t.Logf("重启后恢复 %d 个 piece（重启前 %d 个）", restored, pieces1)

	if err := h2.Download(ctx); err != nil {
		t.Fatalf("续传下载失败: %v", err)
	}
	gotPath := filepath.Join(leechRoot, "resume-src", "payload.bin")
	if gotSum := fileDigest(t, gotPath); gotSum != wantSum {
		t.Fatalf("续传后文件 SHA-256 不匹配\n得到 %x\n期望 %x", gotSum, wantSum)
	}
}

func TestSnapshotFieldsAndBitmap(t *testing.T) {
	seedRoot, _, torrentPath := seedLayout(t, "snap-src", 512<<10)
	seeder := startSeeder(t, seedRoot, torrentPath)

	leechRoot := tempDir(t)
	cfg := localConfig(leechRoot)
	cfg.NoUpload = true
	leecher, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("创建下载端失败: %v", err)
	}
	defer leecher.Close()

	h, err := leecher.Add(AddSpec{TorrentFile: torrentPath, PeerAddrs: []string{leecherPeerAddr(seeder)}})
	if err != nil {
		t.Fatalf("加入 torrent 失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := h.WaitInfo(ctx); err != nil {
		t.Fatalf("等待元数据失败: %v", err)
	}

	snap := h.Snapshot()
	if snap.InfoHash == "" {
		t.Error("快照应包含 infoHash")
	}
	if snap.Name == "" {
		t.Error("快照应包含名称")
	}
	if snap.TotalBytes != int64(512<<10) {
		t.Errorf("TotalBytes = %d, 期望 %d", snap.TotalBytes, 512<<10)
	}
	if snap.PieceLength != testPieceLength {
		t.Errorf("PieceLength = %d, 期望 %d", snap.PieceLength, testPieceLength)
	}
	if snap.Bitmap == nil || snap.Bitmap.Len() != uint64(snap.PieceCount) {
		t.Fatalf("位图长度与 piece 数不一致: %v vs %d", snap.Bitmap, snap.PieceCount)
	}
	if snap.Complete {
		t.Error("下载前不应判定完成")
	}
	if len(snap.Files) != 1 {
		t.Fatalf("文件数 = %d, 期望 1", len(snap.Files))
	}
	if snap.Files[0].Length != int64(512<<10) {
		t.Errorf("文件长度 = %d", snap.Files[0].Length)
	}

	if err := h.Download(ctx); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	final := h.Snapshot()
	if !final.Complete {
		t.Error("完成后 Complete 应为 true")
	}
	if final.PiecesDone != final.PieceCount {
		t.Errorf("已完成 piece 数 = %d, 期望 %d", final.PiecesDone, final.PieceCount)
	}
	if !final.Bitmap.Complete() {
		t.Error("完成后位图应全部置位")
	}
	if final.CompletedBytes != final.TotalBytes {
		t.Errorf("CompletedBytes = %d, 期望 %d", final.CompletedBytes, final.TotalBytes)
	}
}

func TestEngineRejectsEmptySpec(t *testing.T) {
	e, err := NewEngine(localConfig(tempDir(t)))
	if err != nil {
		t.Fatalf("NewEngine 失败: %v", err)
	}
	defer e.Close()

	if _, err := e.Add(AddSpec{}); err == nil {
		t.Error("空来源应被拒绝")
	}
	if _, err := e.Add(AddSpec{Magnet: "not-a-magnet"}); err == nil {
		t.Error("非法磁力链应被拒绝")
	}
	if _, err := e.Add(AddSpec{TorrentFile: filepath.Join(tempDir(t), "missing.torrent")}); err == nil {
		t.Error("不存在的种子文件应被拒绝")
	}
	if _, err := e.Add(AddSpec{TorrentBytes: []byte("garbage")}); err == nil {
		t.Error("非法种子内容应被拒绝")
	}
}

func TestEngineLifecycleAndHandles(t *testing.T) {
	seedRoot, _, torrentPath := seedLayout(t, "life-src", 128<<10)
	seeder := startSeeder(t, seedRoot, torrentPath)

	leechRoot := tempDir(t)
	e, err := NewEngine(localConfig(leechRoot))
	if err != nil {
		t.Fatalf("NewEngine 失败: %v", err)
	}
	defer e.Close()

	if e.ListenPort() == 0 {
		t.Error("应返回实际监听端口")
	}

	h, err := e.Add(AddSpec{TorrentFile: torrentPath, PeerAddrs: []string{leecherPeerAddr(seeder)}})
	if err != nil {
		t.Fatalf("加入 torrent 失败: %v", err)
	}
	hash := h.InfoHash()
	if _, ok := e.Handle(hash); !ok {
		t.Error("应能按 infoHash 找到 handle")
	}
	if len(e.Handles()) != 1 {
		t.Errorf("Handles 数量 = %d, 期望 1", len(e.Handles()))
	}
	if h.MagnetURI() == "" {
		t.Error("应能生成磁力链")
	}
	if err := e.Drop(hash); err != nil {
		t.Fatalf("Drop 失败: %v", err)
	}
	if _, ok := e.Handle(hash); ok {
		t.Error("Drop 后不应再找到 handle")
	}
	if err := e.Drop(hash); err == nil {
		t.Error("重复 Drop 应返回错误")
	}
}

func TestContextFromDone(t *testing.T) {
	done := make(chan struct{})
	ctx, cancel := ContextFromDone(context.Background(), done)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("done 未关闭时不应取消")
	default:
	}
	close(done)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("done 关闭后应取消 context")
	}
}

// ApplyToTask 必须产出自洽的任务（Validate 通过），否则上层会把它当作损坏元数据。
func TestApplyToTaskProducesValidTask(t *testing.T) {
	seedRoot, _, torrentPath := seedLayout(t, "apply-src", 256<<10)
	seeder := startSeeder(t, seedRoot, torrentPath)

	leechRoot := tempDir(t)
	cfg := localConfig(leechRoot)
	cfg.NoUpload = true
	leecher, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("NewEngine 失败: %v", err)
	}
	defer leecher.Close()

	h, err := leecher.Add(AddSpec{TorrentFile: torrentPath, PeerAddrs: []string{leecherPeerAddr(seeder)}})
	if err != nil {
		t.Fatalf("加入 torrent 失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if err := h.WaitInfo(ctx); err != nil {
		t.Fatalf("等待元数据失败: %v", err)
	}

	task, err := engine.NewTask("bt-2", engine.SourceBitTorrent, h.MagnetURI(), "", leechRoot, 0, 1<<20, 0)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	if err := h.ApplyToTask(task); err != nil {
		t.Fatalf("ApplyToTask 失败: %v", err)
	}
	if err := task.Validate(); err != nil {
		t.Fatalf("桥接后的任务应自洽: %v", err)
	}
	if task.Progress() < 0 || task.Progress() > 1 {
		t.Errorf("进度越界: %v", task.Progress())
	}
	// BT 任务不使用分片模型。
	if len(task.Segments) != 0 {
		t.Errorf("BT 任务不应有 HTTP 分片，实际 %d 个", len(task.Segments))
	}

}
