// Package verify 提供完整性校验与坏块定位。
//
// 为什么需要「定位坏块」而不是只比对整体哈希：整体哈希只能回答「文件对不对」，
// 不能回答「哪里不对」。有了分块哈希清单（manifest），校验失败时可以精确定位到
// 具体区间，只重下那几块——对 10GB 文件来说这是「重下 10GB」与「重下 4MB」的区别。
//
// 三种校验数据来源，优先级从高到低：
//  1. 分块清单（manifest）：可精确定位；
//  2. 整体哈希（.sha256/.md5 或服务端 Digest 头）：只能判定整体是否损坏；
//  3. BitTorrent piece hash：由协议自带，库负责校验与坏 piece 重下。
package verify

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
)

// Algorithm 是支持的哈希算法。
type Algorithm string

const (
	// AlgorithmNone 表示不校验。
	AlgorithmNone Algorithm = "none"
	// AlgorithmSHA256 是默认算法。
	AlgorithmSHA256 Algorithm = "sha256"
	// AlgorithmMD5 用于兼容只提供 md5 的镜像站。
	AlgorithmMD5 Algorithm = "md5"
)

// ParseAlgorithm 解析算法名，空值视为 sha256。
func ParseAlgorithm(name string) (Algorithm, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", string(AlgorithmSHA256):
		return AlgorithmSHA256, nil
	case string(AlgorithmNone):
		return AlgorithmNone, nil
	case string(AlgorithmMD5):
		return AlgorithmMD5, nil
	default:
		return "", fmt.Errorf("verify: 不支持的哈希算法 %q", name)
	}
}

// newHash 构造哈希器。
func (a Algorithm) newHash() (hash.Hash, error) {
	switch a {
	case AlgorithmSHA256:
		return sha256.New(), nil
	case AlgorithmMD5:
		return md5.New(), nil
	default:
		return nil, fmt.Errorf("verify: 算法 %q 不支持流式计算", a)
	}
}

// DigestLength 返回该算法的十六进制摘要长度。
func (a Algorithm) DigestLength() int {
	switch a {
	case AlgorithmSHA256:
		return 64
	case AlgorithmMD5:
		return 32
	default:
		return 0
	}
}

// DefaultBufferSize 是流式校验的缓冲区大小。
// 校验 10GB 文件时内存占用必须与文件大小无关，因此只能固定缓冲区流式读取。
const DefaultBufferSize = 1 << 20

// HashReader 流式计算 reader 的摘要。
func HashReader(ctx context.Context, r io.Reader, algo Algorithm, progress func(int64)) (string, error) {
	h, err := algo.newHash()
	if err != nil {
		return "", err
	}
	buf := make([]byte, DefaultBufferSize)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := r.Read(buf)
		if n > 0 {
			if _, err := h.Write(buf[:n]); err != nil {
				return "", err
			}
			if progress != nil {
				progress(int64(n))
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return "", fmt.Errorf("verify: 读取数据失败: %w", readErr)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashFile 流式计算文件摘要。进度回调以「累计已读取字节数」的形式调用。
func HashFile(ctx context.Context, path string, algo Algorithm, progress func(int64)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("verify: 打开文件失败: %w", err)
	}
	defer f.Close()
	return HashReader(ctx, f, algo, progress)
}

// HashRange 计算文件指定区间的摘要，用于分块校验。
func HashRange(ctx context.Context, f *os.File, start, end int64, algo Algorithm) (string, error) {
	h, err := algo.newHash()
	if err != nil {
		return "", err
	}
	if end <= start {
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	section := io.NewSectionReader(f, start, end-start)
	if _, err := io.CopyBuffer(h, section, make([]byte, 256<<10)); err != nil {
		return "", fmt.Errorf("verify: 读取区间 [%d,%d) 失败: %w", start, end, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
