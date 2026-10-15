package bittorrent

import (
	"strings"
	"testing"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
)

const sampleHash = "24169F3EFB914074853F5C6346F4486726F56BDD"

// normalizeMagnet 应只清理「结构性空白」，不碰参数值内部。
func TestNormalizeMagnet(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"标准写法不变", "magnet:?xt=urn:btih:" + sampleHash, "magnet:?xt=urn:btih:" + sampleHash},
		{"问号后空格（本次事故根因）", "magnet:? xt=urn:btih:" + sampleHash, "magnet:?xt=urn:btih:" + sampleHash},
		{"前导空白", "   magnet:?xt=urn:btih:" + sampleHash, "magnet:?xt=urn:btih:" + sampleHash},
		{"尾随换行", "magnet:?xt=urn:btih:" + sampleHash + "\n", "magnet:?xt=urn:btih:" + sampleHash},
		{"多行粘贴", "magnet:?\r\n\txt=urn:btih:" + sampleHash + "\n", "magnet:?xt=urn:btih:" + sampleHash},
		{"参数间空格", "magnet:?xt=urn:btih:" + sampleHash + "& dn=x", "magnet:?xt=urn:btih:" + sampleHash + "&dn=x"},
		{"全角空格", "magnet:?\u3000xt=urn:btih:" + sampleHash, "magnet:?xt=urn:btih:" + sampleHash},
		{"保留参数值内部空格", "magnet:?xt=urn:btih:" + sampleHash + "&dn=my file", "magnet:?xt=urn:btih:" + sampleHash + "&dn=my file"},
		{"非磁力链原样返回", "https://example.com/a.torrent", "https://example.com/a.torrent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeMagnet(c.in); got != c.want {
				t.Fatalf("normalizeMagnet(%q)\n 得到 %q\n 期望 %q", c.in, got, c.want)
			}
		})
	}
}

// 验收核心（回归）：脏输入要么被修复成可用，要么给出可读错误——
// 绝不能再让库内部断言 panic 冒成「内部错误: is zero」。
func TestBuildSpecHandlesDirtyMagnets(t *testing.T) {
	newEngine := func() *Engine {
		// 用包内 tempDir（忽略清理失败）：buildSpec 会为 BT 存储打开
		// .torrent.bolt.db，句柄由调用方负责关闭，测试里用 helper 兜底。
		return &Engine{cfg: Config{DataDir: tempDir(t)}}
	}

	ok := []struct {
		name string
		uri  string
	}{
		{"问号后空格", "magnet:? xt=urn:btih:" + sampleHash},
		{"前导与尾随空白", "  magnet:?xt=urn:btih:" + sampleHash + "  "},
		{"带换行", "magnet:?xt=urn:btih:" + sampleHash + "\r\n"},
		{"小写哈希", "magnet:?xt=urn:btih:" + strings.ToLower(sampleHash)},
		{"带 dn 与 tr", "magnet:?xt=urn:btih:" + sampleHash + "&dn=a&tr=udp%3A%2F%2Ft.example%3A80"},
	}
	for _, c := range ok {
		t.Run("可用/"+c.name, func(t *testing.T) {
			spec, err := newEngine().buildSpec(AddSpec{Magnet: c.uri, SavePath: tempDir(t)})
			closeSpecStorage(t, spec)
			if err != nil {
				t.Fatalf("应当能解析，却报错: %v", err)
			}
			if spec.InfoHash.IsZero() && !spec.InfoHashV2.Ok {
				t.Fatal("解析成功但 infoHash 为零（会导致后续 panic）")
			}
			t.Logf("infoHash = %s", spec.InfoHash.HexString())
		})
	}

	// 这些无法修复：必须是普通错误，且信息能指向问题所在
	bad := []struct {
		name string
		uri  string
	}{
		{"哈希长度不足", "magnet:?xt=urn:btih:24169F3E"},
		{"哈希非法字符", "magnet:?xt=urn:btih:ZZZZ9F3EFB914074853F5C6346F4486726F56BDD"},
		{"完全没有 xt", "magnet:?dn=test"},
		{"空 btih", "magnet:?xt=urn:btih:"},
	}
	for _, c := range bad {
		t.Run("应报错/"+c.name, func(t *testing.T) {
			spec, err := newEngine().buildSpec(AddSpec{Magnet: c.uri, SavePath: tempDir(t)})
			closeSpecStorage(t, spec)
			if err == nil {
				t.Fatal("应返回错误而不是让库 panic")
			}
			if strings.Contains(err.Error(), "is zero") {
				t.Fatalf("错误信息不应泄漏库内部断言: %v", err)
			}
			t.Logf("可读错误: %v", err)
		})
	}
}

// closeSpecStorage 释放 buildSpec 顺带创建的 BT 存储句柄。
// 不关闭会让临时目录里的 .torrent.bolt.db 一直占用，导致测试清理失败。
func closeSpecStorage(t *testing.T, spec *torrent.TorrentSpec) {
	t.Helper()
	if spec == nil {
		return
	}
	closer, ok := spec.Storage.(storage.ClientImplCloser)
	if !ok {
		return
	}
	if err := closer.Close(); err != nil {
		t.Logf("关闭存储句柄时出错（可忽略）: %v", err)
	}
}
