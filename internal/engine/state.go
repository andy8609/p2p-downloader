package engine

import (
	"fmt"
)

// 状态迁移表是任务生命周期的唯一权威定义。
//
// 明确列出的原因：任务状态会被存储层持久化、被控制面读写、被 GUI 展示，
// 任何一处「顺手改状态」都会产生互相矛盾的状态（例如 completed 又被改成 running），
// 因此所有状态变更都必须经过 CanTransition / Transition 校验。
var allowedTransitions = map[TaskState]map[TaskState]bool{
	StatePending: {
		StateRunning:   true,
		StatePaused:    true,
		StateFailed:    true,
		StateCompleted: true, // 数据已在磁盘上（例如重建任务时文件已存在）
	},
	StateRunning: {
		StatePaused:    true,
		StateVerifying: true,
		StateCompleted: true,
		StateFailed:    true,
		StatePending:   true, // 被调度器重新排队（例如并发名额回收）
	},
	StatePaused: {
		StateRunning: true,
		StatePending: true,
		StateFailed:  true,
	},
	StateVerifying: {
		StateCompleted: true,
		StateFailed:    true,
		StatePaused:    true,
		// 校验发现损坏且已定位到受损区间时，需要重新排队只重下这些区间。
		StatePending: true,
	},
	StateCompleted: {
		// 终态：只允许用户显式的「重新下载」把任务打回 pending。
		StatePending: true,
	},
	StateFailed: {
		StatePending:   true,
		StatePaused:    true,
		StateCompleted: true, // 失败后校验发现数据其实完整
	},
}

// CanTransition 判断状态迁移是否合法。
func CanTransition(from, to TaskState) bool {
	if from == to {
		return true
	}
	targets, ok := allowedTransitions[from]
	if !ok {
		return false
	}
	return targets[to]
}

// ErrInvalidTransition 表示被拒绝的状态迁移。
type ErrInvalidTransition struct {
	From TaskState
	To   TaskState
}

func (e *ErrInvalidTransition) Error() string {
	return fmt.Sprintf("engine: 非法的状态迁移 %s → %s", e.From, e.To)
}

// SetState 校验并设置任务状态，非法迁移返回 *ErrInvalidTransition。
func (t *Task) SetState(to TaskState) error {
	if t == nil {
		return fmt.Errorf("engine: 任务为空")
	}
	if !to.Valid() {
		return fmt.Errorf("engine: 未知的任务状态 %q", to)
	}
	if !CanTransition(t.State, to) {
		return &ErrInvalidTransition{From: t.State, To: to}
	}
	t.State = to
	t.Touch()
	return nil
}
