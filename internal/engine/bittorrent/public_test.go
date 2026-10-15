package bittorrent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// publicTorrentURL 是 Ubuntu 官方发布的种子文件（官方 tracker + WebSeed）。
// 只取种子文件本身，不下载完整 ISO。
const publicTorrentURL = "https://releases.ubuntu.com/24.04/ubuntu-24.04.3-desktop-amd64.iso.torrent"

// 公网 swarm 测试默认跳过：它依赖外网不可控因素（tracker 可达性、对等方是否在线、
// 运营商是否封锁 BT 流量），不适合放进常规测试套件。
//
// 运行方式：
//
//	P2PDL_PUBLIC_BT_TEST=1 go test ./internal/engine/bittorrent -run TestPublicSwarm -v -timeout 5m
//
// 验收目标（T7）：能从公网 swarm 连上真实 peer 并成功接收数据。
// 完整下载 6GB 的 ISO 不适合作为自动化验收，因此这里断言「连上 peer 且实际收到数据」，
// 完整文件的完整性校验由本地双客户端互测（必然可复现）承担。
func TestPublicSwarmReachesRealPeers(t *testing.T) {
	if os.Getenv("P2PDL_PUBLIC_BT_TEST") == "" {
		t.Skip("设置 P2PDL_PUBLIC_BT_TEST=1 以运行公网 swarm 测试")
	}

	raw, err := fetchTorrentFile(publicTorrentURL)
	if err != nil {
		t.Skipf("无法获取公网种子（%v），跳过", err)
	}
	mi, err := metainfo.Load(io.NopCloser(newByteReader(raw)))
	if err != nil {
		t.Fatalf("解析种子失败: %v", err)
	}
	t.Logf("种子: %s，announce=%v", mi.Magnet(nil, nil), mi.AnnounceList)

	dir := tempDir(t)
	cfg := Config{
		DataDir: dir,
		// DHT 引导域名在很多网络下会被 DNS 污染，公网验证只用 tracker。
		DisableDHT:      true,
		DisableTrackers: false,
		DisableUTP:      true,
		NoUpload:        true,
		Seed:            false,
		Logf:            t.Logf,
	}
	eng, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("创建引擎失败: %v", err)
	}
	defer eng.Close()

	h, err := eng.Add(AddSpec{TorrentBytes: raw})
	if err != nil {
		t.Fatalf("加入种子失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := h.WaitInfo(ctx); err != nil {
		t.Fatalf("等待元数据失败: %v", err)
	}

	snap := h.Snapshot()
	t.Logf("目标文件：%s，大小 %s，piece %d 个",
		snap.Name, humanBytes(snap.TotalBytes), snap.PieceCount)

	if err := h.Download(ctx); err != nil && ctx.Err() == nil {
		t.Fatalf("下载失败: %v", err)
	}

	// 用一个较短的观察窗口：只要连上 peer 并收到数据即可判定公网路径可用。
	deadline := time.Now().Add(90 * time.Second)
	var lastPeers, lastDone int
	for time.Now().Before(deadline) {
		s := h.Snapshot()
		if s.ActivePeers != lastPeers || s.PiecesDone != lastDone {
			t.Logf("peer=%d(总 %d) 已完成 piece=%d/%d 已接收 %s 速率 %s/s",
				s.ActivePeers, s.TotalPeers, s.PiecesDone, s.PieceCount,
				humanBytes(s.CompletedBytes), humanBytes(int64(s.DownloadRate)))
			lastPeers, lastDone = s.ActivePeers, s.PiecesDone
		}
		if s.CompletedBytes >= 1<<20 && s.ActivePeers > 0 {
			t.Logf("公网 swarm 验证通过：连上 %d 个 peer，已接收 %s",
				s.ActivePeers, humanBytes(s.CompletedBytes))
			return
		}
		time.Sleep(2 * time.Second)
	}

	final := h.Snapshot()
	t.Fatalf("公网 swarm 验证未通过：peer 峰值 %d，已接收 %s（可能是网络封锁或对等方不可达）",
		final.ActivePeers, humanBytes(final.CompletedBytes))
}

func fetchTorrentFile(url string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 4<<20))
}

type byteReader struct {
	data []byte
	pos  int
}

func newByteReader(b []byte) *byteReader { return &byteReader{data: b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func humanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}
