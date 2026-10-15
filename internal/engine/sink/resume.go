package sink

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"p2pdl/internal/engine"
)

// Decision 是恢复流程给出的结论。
type Decision string

const (
	// DecisionResume 表示可以复用已有数据，只补缺口。
	DecisionResume Decision = "resume"
	// DecisionRestart 表示已有数据不可信，必须从零重下。
	// 这不是错误：宁可重下，也不能在错位的偏移上继续写。
	DecisionRestart Decision = "restart"
)

// ByteRange 是文件中的半开区间 [Start, End)。
type ByteRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Length 返回区间长度。
func (r ByteRange) Length() int64 {
	if r.End <= r.Start {
		return 0
	}
	return r.End - r.Start
}

// Plan 是恢复决策的完整结果，可直接展示给用户或在日志中记录。
type Plan struct {
	Decision   Decision    `json:"decision"`
	Reason     string      `json:"reason"`
	TempPath   string      `json:"tempPath"`
	TempExists bool        `json:"tempExists"`
	TempSize   int64       `json:"tempSize"`
	TotalSize  int64       `json:"totalSize"`
	Downloaded int64       `json:"downloaded"`
	Missing    []ByteRange `json:"missing,omitempty"`
	// MissingBytes 是仍需要下载的字节数；与 Downloaded 之和等于 TotalSize（决策为 resume 时）。
	MissingBytes int64 `json:"missingBytes"`
}

// PlanRecovery 判断任务能否续传，并给出仍需下载的字节区间。
//
// 判定原则（宁可重下，不可续错）：
//  1. 元数据不自洽 → 重下；
//  2. 长度未知（单流模式）→ 重下（无法向服务端定位任意偏移）；
//  3. 元数据声称有进度、但临时文件不存在或比「已标记完成的最大偏移」还短 →
//     说明数据与位图不一致，重下；
//  4. 其余情况 → 续传，缺口由位图推导。
func PlanRecovery(task *engine.Task, tempPath string) (*Plan, error) {
	if task == nil {
		return nil, errors.New("sink: 任务为空")
	}
	plan := &Plan{TempPath: tempPath, TotalSize: task.TotalSize}

	if err := task.Validate(); err != nil {
		plan.Decision = DecisionRestart
		plan.Reason = fmt.Sprintf("元数据不自洽: %v", err)
		return plan, nil
	}
	if task.TotalSize <= 0 {
		plan.Decision = DecisionRestart
		plan.Reason = "内容长度未知，无法定位续传偏移"
		return plan, nil
	}

	seen := task.Downloaded()
	plan.Downloaded = seen

	fi, statErr := os.Stat(tempPath)
	switch {
	case statErr == nil:
		plan.TempExists = true
		plan.TempSize = fi.Size()
	case os.IsNotExist(statErr):
		plan.TempExists = false
	default:
		return nil, fmt.Errorf("sink: 读取临时文件信息失败: %w", statErr)
	}

	// 位图声称已完成的最高偏移：磁盘文件必须至少有这么长，否则数据不可信。
	highest := highestCompletedOffset(task)
	if seen > 0 && (!plan.TempExists || plan.TempSize < highest) {
		plan.Decision = DecisionRestart
		plan.Reason = fmt.Sprintf("临时文件与进度记录不一致（文件 %d 字节，位图声称至少 %d 字节）",
			plan.TempSize, highest)
		return plan, nil
	}

	plan.Missing = MissingRanges(task)
	for _, r := range plan.Missing {
		plan.MissingBytes += r.Length()
	}
	plan.Decision = DecisionResume
	if len(plan.Missing) == 0 {
		plan.Reason = "数据已齐全，可进入校验阶段"
	} else {
		plan.Reason = fmt.Sprintf("从断点续传，仍有 %d 个缺口", len(plan.Missing))
	}
	return plan, nil
}

// MissingRanges 返回任务仍需下载的绝对字节区间（已合并相邻区间，无重叠）。
func MissingRanges(task *engine.Task) []ByteRange {
	if task == nil || task.TotalSize <= 0 {
		return nil
	}
	var out []ByteRange
	for _, seg := range task.Segments {
		if seg == nil {
			continue
		}
		for _, gap := range seg.Bitmap.Gaps() {
			start, end := seg.ChunkRange(task.ChunkSize, gap[0])
			_, gapEnd := seg.ChunkRange(task.ChunkSize, gap[1]-1)
			if gapEnd > end {
				end = gapEnd
			}
			out = append(out, ByteRange{Start: start, End: end})
		}
	}
	return MergeRanges(out)
}

// MergeRanges 排序并合并重叠/相接的区间，保证结果互不重叠且按起点升序。
// 「重复恢复多次不产生区间重叠或空洞」的保证就落在这里。
func MergeRanges(in []ByteRange) []ByteRange {
	if len(in) == 0 {
		return nil
	}
	sorted := make([]ByteRange, len(in))
	copy(sorted, in)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start < sorted[j].Start
		}
		return sorted[i].End < sorted[j].End
	})

	out := make([]ByteRange, 0, len(sorted))
	for _, r := range sorted {
		if r.Length() <= 0 {
			continue
		}
		if len(out) == 0 {
			out = append(out, r)
			continue
		}
		last := &out[len(out)-1]
		if r.Start <= last.End {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// highestCompletedOffset 返回位图声称已完成的最大偏移。
func highestCompletedOffset(task *engine.Task) int64 {
	var highest int64
	for _, seg := range task.Segments {
		if seg == nil {
			continue
		}
		ranges := seg.Bitmap.Ranges()
		if len(ranges) == 0 {
			continue
		}
		last := ranges[len(ranges)-1]
		if _, end := seg.ChunkRange(task.ChunkSize, last[1]-1); end > highest {
			highest = end
		}
	}
	return highest
}
