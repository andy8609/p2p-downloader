package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"p2pdl/internal/engine"
)

// manifestVersion 是清单格式版本，便于未来演进。
const manifestVersion = 1

// ErrManifestMismatch 表示清单与文件结构不匹配（大小或块数不符）。
var ErrManifestMismatch = errors.New("verify: 清单与文件不匹配")

// Manifest 是分块哈希清单，用于把「文件坏了」精确定位成「哪几块坏了」。
type Manifest struct {
	Version   int    `json:"version"`
	Algorithm string `json:"algorithm"`
	ChunkSize int64  `json:"chunkSize"`
	TotalSize int64  `json:"totalSize"`
	// Chunks 是每块的十六进制摘要，顺序与文件偏移一致。
	Chunks []string `json:"chunks"`
}

// BuildManifest 为一个完整文件生成分块清单。
//
// 用途：下载完成后生成一次，之后任何一次校验都能精确定位损坏区间；
// 也可以分发给其他节点作为可信基线。
func BuildManifest(ctx context.Context, path string, chunkSize int64, algo Algorithm, progress func(int64)) (*Manifest, error) {
	if chunkSize <= 0 {
		return nil, fmt.Errorf("verify: 分块大小必须为正数，实际 %d", chunkSize)
	}
	if algo == AlgorithmNone {
		algo = AlgorithmSHA256
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("verify: 打开文件失败: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	total := fi.Size()

	m := &Manifest{
		Version:   manifestVersion,
		Algorithm: string(algo),
		ChunkSize: chunkSize,
		TotalSize: total,
		Chunks:    make([]string, 0, (total+chunkSize-1)/chunkSize),
	}

	for start := int64(0); start < total; start += chunkSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + chunkSize
		if end > total {
			end = total
		}
		sum, err := HashRange(ctx, f, start, end, algo)
		if err != nil {
			return nil, err
		}
		m.Chunks = append(m.Chunks, sum)
		if progress != nil {
			progress(end - start)
		}
	}
	return m, nil
}

// ChunkCount 返回块数。
func (m *Manifest) ChunkCount() int {
	if m == nil {
		return 0
	}
	return len(m.Chunks)
}

// AlgorithmType 返回解析后的算法。
func (m *Manifest) AlgorithmType() (Algorithm, error) {
	if m == nil {
		return AlgorithmNone, errors.New("verify: 清单为空")
	}
	return ParseAlgorithm(m.Algorithm)
}

// Save 原子写入清单文件。
func (m *Manifest) Save(path string) error {
	if m == nil {
		return errors.New("verify: 清单为空")
	}
	payload, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("verify: 序列化清单失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o644); err != nil {
		return fmt.Errorf("verify: 写入清单失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("verify: 提交清单失败: %w", err)
	}
	return nil
}

// LoadManifest 读取清单文件。
func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("verify: 清单格式非法: %w", err)
	}
	if m.ChunkSize <= 0 {
		return nil, fmt.Errorf("%w: 分块大小非法（%d）", ErrManifestMismatch, m.ChunkSize)
	}
	return &m, nil
}

// Report 是逐块校验的结果。
type Report struct {
	// OK 表示所有块都通过校验。
	OK bool `json:"ok"`
	// TotalChunks 是总块数。
	TotalChunks int `json:"totalChunks"`
	// BadChunks 是未通过校验的块下标。
	BadChunks []int `json:"badChunks,omitempty"`
	// BadRanges 是未通过校验的文件区间（已合并相邻块）。
	BadRanges []engine.ByteRange `json:"badRanges,omitempty"`
	// BadBytes 是未通过校验的字节数。
	BadBytes int64 `json:"badBytes,omitempty"`
	// VerifiedBytes 是实际读过的字节数。
	VerifiedBytes int64 `json:"verifiedBytes"`
}

// VerifyFile 逐块校验文件，返回精确定位的损坏区间。
//
// 内存占用与文件大小无关：每次只读一块。
func (m *Manifest) VerifyFile(ctx context.Context, path string) (*Report, error) {
	if m == nil {
		return nil, errors.New("verify: 清单为空")
	}
	algo, err := m.AlgorithmType()
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("verify: 打开文件失败: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() != m.TotalSize {
		return nil, fmt.Errorf("%w: 文件大小 %d 与清单 %d 不符", ErrManifestMismatch, fi.Size(), m.TotalSize)
	}

	report := &Report{TotalChunks: len(m.Chunks)}
	for i, want := range m.Chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		start := int64(i) * m.ChunkSize
		end := start + m.ChunkSize
		if end > m.TotalSize {
			end = m.TotalSize
		}
		got, err := HashRange(ctx, f, start, end, algo)
		if err != nil {
			return nil, err
		}
		if got != want {
			report.BadChunks = append(report.BadChunks, i)
			report.BadRanges = append(report.BadRanges, engine.ByteRange{Start: start, End: end})
			report.BadBytes += end - start
		}
		report.VerifiedBytes += end - start
	}
	report.BadRanges = mergeRanges(report.BadRanges)
	report.OK = len(report.BadChunks) == 0
	return report, nil
}

// mergeRanges 合并相邻/重叠区间，使「哪些块坏了」在日志与 UI 中更易读。
func mergeRanges(in []engine.ByteRange) []engine.ByteRange {
	if len(in) <= 1 {
		return in
	}
	out := make([]engine.ByteRange, 0, len(in))
	current := in[0]
	for _, r := range in[1:] {
		if r.Start <= current.End {
			if r.End > current.End {
				current.End = r.End
			}
			continue
		}
		out = append(out, current)
		current = r
	}
	return append(out, current)
}

// DefaultManifestPath 返回约定俗成的清单文件名：与原文件同目录，加后缀。
// 放在数据目录之外（用户不会误删），但也可以被其他工具发现。
func DefaultManifestPath(filePath string) string {
	return filePath + ".p2pdl.manifest"
}
