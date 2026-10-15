package engine

import (
	"errors"
	"testing"
)

func TestCanTransitionMatrix(t *testing.T) {
	legal := []struct{ from, to TaskState }{
		{StatePending, StateRunning},
		{StatePending, StatePaused},
		{StatePending, StateCompleted},
		{StateRunning, StatePaused},
		{StateRunning, StateVerifying},
		{StateRunning, StateCompleted},
		{StateRunning, StateFailed},
		{StateRunning, StatePending},
		{StatePaused, StateRunning},
		{StatePaused, StatePending},
		{StatePaused, StateFailed},
		{StateVerifying, StateCompleted},
		{StateVerifying, StateFailed},
		{StateVerifying, StatePending}, // 校验发现损坏 → 重新排队只重下受损区间
		{StateCompleted, StatePending}, // 用户显式重新下载
		{StateFailed, StatePending},
		{StateFailed, StateCompleted},
	}
	for _, c := range legal {
		if !CanTransition(c.from, c.to) {
			t.Errorf("应允许 %s → %s", c.from, c.to)
		}
	}

	illegal := []struct{ from, to TaskState }{
		{StatePending, StateVerifying},
		{StatePaused, StateVerifying},
		{StateCompleted, StateRunning},
		{StateCompleted, StateVerifying},
		{StateCompleted, StateFailed},
	}
	for _, c := range illegal {
		if CanTransition(c.from, c.to) {
			t.Errorf("应拒绝 %s → %s", c.from, c.to)
		}
	}

	// 自反：任何合法状态迁移到自身都允许（幂等设置）。
	for _, s := range []TaskState{StatePending, StateRunning, StatePaused, StateVerifying, StateCompleted, StateFailed} {
		if !CanTransition(s, s) {
			t.Errorf("应允许 %s → %s（幂等）", s, s)
		}
	}
	// 未知状态一律拒绝。
	if CanTransition(TaskState("bogus"), StateRunning) {
		t.Error("未知来源状态应被拒绝")
	}
}

func TestSetStateRejectsIllegalTransition(t *testing.T) {
	task := newTestTask(t, SourceHTTP, 2)

	if err := task.SetState(StateRunning); err != nil {
		t.Fatalf("pending → running 应允许: %v", err)
	}
	if err := task.SetState(StateRunning); err != nil {
		t.Fatalf("重复设置同状态应允许: %v", err)
	}
	if err := task.SetState(StateCompleted); err != nil {
		t.Fatalf("running → completed 应允许: %v", err)
	}

	err := task.SetState(StateRunning)
	if err == nil {
		t.Fatal("completed → running 必须被拒绝")
	}
	var transitionErr *ErrInvalidTransition
	if !errors.As(err, &transitionErr) {
		t.Fatalf("错误类型 = %T, 期望 *ErrInvalidTransition", err)
	}
	if transitionErr.From != StateCompleted || transitionErr.To != StateRunning {
		t.Errorf("错误内容 = %+v", transitionErr)
	}
	if task.State != StateCompleted {
		t.Errorf("被拒绝后状态不应改变, 实际 %q", task.State)
	}

	if err := task.SetState(TaskState("nonsense")); err == nil {
		t.Error("未知目标状态应被拒绝")
	}
	if err := (*Task)(nil).SetState(StateRunning); err == nil {
		t.Error("nil 任务应被拒绝")
	}
}

// 每个声明的状态都必须是「可达的」：即至少有一条从 pending 出发的路径。
func TestAllStatesReachable(t *testing.T) {
	states := []TaskState{StatePending, StateRunning, StatePaused, StateVerifying, StateCompleted, StateFailed}
	reached := map[TaskState]bool{StatePending: true}
	changed := true
	for changed {
		changed = false
		for from := range reached {
			for _, to := range states {
				if CanTransition(from, to) && !reached[to] {
					reached[to] = true
					changed = true
				}
			}
		}
	}
	for _, s := range states {
		if !reached[s] {
			t.Errorf("状态 %s 从 pending 出发不可达", s)
		}
	}
}
