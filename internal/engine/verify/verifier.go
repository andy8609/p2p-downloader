package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"p2pdl/internal/engine"
)

// 默认的重试上限。
const (
	// DefaultMaxRepairAttempts 是「校验→修复→重下」循环的最大次数。
	// 它是防止无限循环的硬约束：持续损坏（例如磁盘写坏）会在几轮后直接失败，
	// 而不是永远重下。
	DefaultMaxRepairAttempts = 3
	// DefaultMaxRetryAttempts 是网络类错误的最大重试次数（由下载器消费）。
	DefaultMaxRetryAttempts = 5
)

// Options 配置校验器。
type Options struct {
	// ManifestDir 覆盖清单文件的存放目录；为空时与原文件同目录。
	ManifestDir string
	// BuildManifestOnSuccess 在校验成功后生成分块清单，供后续校验精确定位。
	BuildManifestOnSuccess bool
	// MaxRepairAttempts 是修复循环上限，<=0 时取 DefaultMaxRepairAttempts。
	MaxRepairAttempts int
	// Recheck 是 BitTorrent 等自带 piece 校验的引擎的重校验钩子。
	// 为 nil 时 BT 任务只做整体哈希校验。
	Recheck func(ctx context.Context, task *engine.Task) error
	// Logf 为 nil 时静默。
	Logf func(format string, args ...any)
}

// Verifier 实现 engine.Verifier。
type Verifier struct {
	opts Options
}

// New 创建校验器。
func New(opts Options) *Verifier {
	if opts.MaxRepairAttempts <= 0 {
		opts.MaxRepairAttempts = DefaultMaxRepairAttempts
	}
	return &Verifier{opts: opts}
}

func (v *Verifier) logf(format string, args ...any) {
	if v.opts.Logf != nil {
		v.opts.Logf(format, args...)
	}
}

// Verify 执行校验并尽可能定点修复。
//
// 决策顺序：
//  1. 任务未启用校验 → 直接通过；
//  2. BitTorrent 任务 → 用引擎自带的 piece 校验重新核对磁盘数据；
//  3. 有分块清单 → 逐块定位损坏区间，清除对应分片位图位（定点重下）；
//  4. 只有整体哈希 → 校验失败且无法定位时如实失败，避免掩盖问题。
func (v *Verifier) Verify(ctx context.Context, task *engine.Task) (engine.VerificationResult, error) {
	if task == nil {
		return engine.VerificationResult{}, errors.New("verify: 任务为空")
	}

	if task.Kind == engine.SourceBitTorrent {
		return v.verifyBitTorrent(ctx, task)
	}
	if !task.Verify.Enabled() {
		v.logf("verify: 任务 %s 未启用校验，跳过", task.ID)
		return engine.VerificationResult{OK: true, Reason: "未启用校验"}, nil
	}

	spec := task.Verify.Normalize()
	algo, err := ParseAlgorithm(spec.Algorithm)
	if err != nil {
		return engine.VerificationResult{}, err
	}

	report, verifyErr := v.verifyFile(ctx, task, algo)
	if verifyErr == nil && report.OK {
		return v.success(task, report)
	}

	// 整体校验失败：优先用分块清单定位。
	located, locateErr := v.locateBadChunks(ctx, task, algo)
	if locateErr == nil && len(located.BadRanges) > 0 {
		return v.repair(task, located)
	}

	if verifyErr != nil {
		return engine.VerificationResult{}, verifyErr
	}
	return engine.VerificationResult{
		OK:     false,
		Reason: fmt.Sprintf("整体摘要不匹配（期望 %s，实际 %s），且没有分块清单可用于定位",
			truncate(spec.Value), truncate(report.actual)),
		Err: fmt.Errorf("verify: 完整性校验失败且无法定位损坏区间"),
	}, nil
}

// verifyFile 计算整体摘要并按需生成清单。
func (v *Verifier) verifyFile(ctx context.Context, task *engine.Task, algo Algorithm) (*fileReport, error) {
	path := task.DestPath
	if _, err := os.Stat(path); err != nil {
		// 文件可能仍在临时位置（提交前的场景）。
		if task.TempPath != "" {
			if _, tempErr := os.Stat(task.TempPath); tempErr == nil {
				path = task.TempPath
			}
		}
	}

	var read int64
	actual, err := HashFile(ctx, path, algo, func(n int64) { read += n })
	if err != nil {
		return nil, err
	}

	spec := task.Verify.Normalize()
	report := &fileReport{
		path:     path,
		actual:   actual,
		expected: spec.Value,
		read:     read,
	}
	// 摘要值为空表示「只校验能否完整读取」，不做比对。
	report.OK = spec.Value == "" || strings.EqualFold(actual, spec.Value)
	return report, nil
}

type fileReport struct {
	path     string
	actual   string
	expected string
	read     int64
	OK       bool
}

// locateBadChunks 用清单逐块校验，返回精确定位的损坏区间。
func (v *Verifier) locateBadChunks(ctx context.Context, task *engine.Task, algo Algorithm) (*Report, error) {
	manifestPath := DefaultManifestPath(task.DestPath)
	if v.opts.ManifestDir != "" {
		manifestPath = v.opts.ManifestDir + string(os.PathSeparator) + manifestFileName(task.DestPath)
	}

	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return nil, err
	}

	path := task.DestPath
	if _, statErr := os.Stat(path); statErr != nil && task.TempPath != "" {
		path = task.TempPath
	}
	return manifest.VerifyFile(ctx, path)
}

// repair 把损坏区间对应的分块标记为未完成，使任务重新排队时只重下这些块。
func (v *Verifier) repair(task *engine.Task, report *Report) (engine.VerificationResult, error) {
	if task.VerifyAttempts >= v.opts.MaxRepairAttempts {
		return engine.VerificationResult{
			OK:     false,
			Reason: fmt.Sprintf("已尝试修复 %d 次仍未通过校验", task.VerifyAttempts),
			Err: fmt.Errorf("verify: 修复次数达到上限（%d），可能磁盘或网络持续损坏",
				v.opts.MaxRepairAttempts),
		}, nil
	}

	bytes, chunks := task.MarkChunksMissing(report.BadRanges)
	if chunks == 0 {
		return engine.VerificationResult{
			OK:     false,
			Reason: "定位到损坏区间但无法映射到分片（清单块大小与任务分块不一致）",
			Err:    errors.New("verify: 损坏区间无法映射到任务分片"),
		}, nil
	}
	task.VerifyAttempts++
	task.RepairedBytes += bytes
	task.LastErr = ""

	v.logf("verify: 任务 %s 定位到 %d 个损坏块（%d 字节），已标记重下（第 %d/%d 次修复）",
		task.ID, len(report.BadChunks), bytes, task.VerifyAttempts, v.opts.MaxRepairAttempts)

	return engine.VerificationResult{
		Repaired:     true,
		Missing:      report.BadRanges,
		MissingBytes: bytes,
		Reason: fmt.Sprintf("定位到 %d 个损坏块（%d 字节），已标记为重新下载",
			len(report.BadChunks), bytes),
	}, nil
}

// success 处理校验通过的情形，并可选地生成清单。
func (v *Verifier) success(task *engine.Task, report *fileReport) (engine.VerificationResult, error) {
	if v.opts.BuildManifestOnSuccess && task.ChunkSize > 0 {
		path := DefaultManifestPath(task.DestPath)
		if v.opts.ManifestDir != "" {
			path = v.opts.ManifestDir + string(os.PathSeparator) + manifestFileName(task.DestPath)
		}
		if err := os.MkdirAll(dirOf(path), 0o755); err == nil {
			manifest, err := BuildManifest(context.Background(), report.path, task.ChunkSize, AlgorithmSHA256, nil)
			if err != nil {
				v.logf("verify: 为任务 %s 生成清单失败: %v", task.ID, err)
			} else if err := manifest.Save(path); err != nil {
				v.logf("verify: 保存任务 %s 的清单失败: %v", task.ID, err)
			}
		}
	}
	return engine.VerificationResult{
		OK:     true,
		Reason: fmt.Sprintf("校验通过（%s，%d 字节）", report.actual[:minInt(12, len(report.actual))], report.read),
	}, nil
}

// verifyBitTorrent 处理 BT 任务：piece 哈希由协议自带，交给引擎重新核对。
func (v *Verifier) verifyBitTorrent(ctx context.Context, task *engine.Task) (engine.VerificationResult, error) {
	if v.opts.Recheck == nil {
		return engine.VerificationResult{OK: true, Reason: "BT piece 校验由协议层负责"}, nil
	}

	// 关键：数据已完整时直接通过，**不触发重校验**。
	//
	// BT 库在 Download 返回 nil 之前已经校验过每一个 piece，因此走到这里
	// 就意味着「所有 piece 均通过哈希校验」。若在这里无条件触发重校验并返回
	// Repaired，会形成「完成 → 重校验 → 重新排队 → 再完成」的循环，
	// 直到耗尽 VerifyAttempts 给任务留下一个假的失败原因
	// （实测表现：已完成的任务上挂着「BT 重校验次数达到上限」）。
	if task.Complete() {
		return engine.VerificationResult{
			OK:     true,
			Reason: "BT piece 已由协议层校验通过",
		}, nil
	}

	// 只在数据确实不完整时才触发重校验：这时的目的是让库重新核对磁盘数据，
	// 把与预期不符的 piece 标记为缺失，进而由后续下载补齐。
	if task.VerifyAttempts >= v.opts.MaxRepairAttempts {
		return engine.VerificationResult{
			OK:     false,
			Reason: fmt.Sprintf("已重新校验 %d 次仍未补齐", task.VerifyAttempts),
			Err:    errors.New("verify: BT 重校验次数达到上限"),
		}, nil
	}
	if err := v.opts.Recheck(ctx, task); err != nil {
		return engine.VerificationResult{}, fmt.Errorf("verify: BT 重校验失败: %w", err)
	}
	task.VerifyAttempts++
	return engine.VerificationResult{
		Repaired: true,
		Reason:   "已触发 BT piece 重校验，损坏的 piece 将被重新下载",
	}, nil
}

func manifestFileName(filePath string) string {
	base := filePath
	if idx := strings.LastIndexAny(base, `\/`); idx >= 0 {
		base = base[idx+1:]
	}
	return base + ".p2pdl.manifest"
}

func dirOf(path string) string {
	if idx := strings.LastIndexAny(path, `\/`); idx > 0 {
		return path[:idx]
	}
	return "."
}

func truncate(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16] + "…"
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
