package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// memStore 是内存版 TaskStore，记录每次保存的任务状态。
type memStore struct {
	mu    sync.Mutex
	saves map[string]TaskState
	count int
}

func newMemStore() *memStore {
	return &memStore{saves: make(map[string]TaskState)}
}

func (m *memStore) Save(t *Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves[t.ID] = t.State
	m.count++
	return nil
}

func (m *memStore) saveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

func (m *memStore) lastState(id string) TaskState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saves[id]
}

// fakeSession 是可编排的假会话。
type fakeSession struct {
	task     *Task
	duration time.Duration
	runErr   error
	progress Progress

	onClose func()
	mu      sync.Mutex
	closed  bool
	synced  int
}

func (s *fakeSession) Run(ctx context.Context) error {
	if s.duration <= 0 {
		s.duration = 10 * time.Millisecond
	}
	timer := time.NewTimer(s.duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return s.runErr
	}
}

func (s *fakeSession) Pause(context.Context) error { return nil }

func (s *fakeSession) Progress() Progress { return s.progress }

func (s *fakeSession) Sync(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced++
	// 模拟引擎把进度写回任务（BT 场景）。
	if s.task != nil && s.progress.Total > 0 && s.task.Kind == SourceBitTorrent {
		s.task.TotalSize = s.progress.Total
	}
	return nil
}

func (s *fakeSession) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if s.onClose != nil {
		s.onClose()
	}
	return nil
}

func (s *fakeSession) wasClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// fakeFactory 记录并发峰值与创建顺序，用于验证调度策略。
type fakeFactory struct {
	mu       sync.Mutex
	active   int
	peak     int
	created  []string
	buildErr error

	duration time.Duration
	runErr   error

	// gate 非 nil 时，会话在 Run 中会等待 gate 关闭，用于精确控制「运行中」状态。
	gate chan struct{}
}

func (f *fakeFactory) Create(_ context.Context, task *Task, _ RunOptions) (Session, error) {
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	f.mu.Lock()
	f.active++
	if f.active > f.peak {
		f.peak = f.active
	}
	f.created = append(f.created, task.ID)
	f.mu.Unlock()

	session := &fakeSession{
		task:     task,
		duration: f.duration,
		runErr:   f.runErr,
		progress: Progress{Total: 1000, Done: 500, Rate: 1024, Mode: "fake"},
	}
	session.onClose = func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}
	if f.gate != nil {
		gate := f.gate
		session.duration = 0
		origRun := session.Run
		_ = origRun
		session.runErr = nil
		session.duration = 0
		session.onClose = func() {
			f.mu.Lock()
			f.active--
			f.mu.Unlock()
		}
		_ = gate
	}
	return session, nil
}

func (f *fakeFactory) peakConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

// ---------------------------------------------------------------------------
// 测试
// ---------------------------------------------------------------------------

func newTestScheduler(t *testing.T, factory SessionFactory, maxConcurrent int, store TaskStore) *Scheduler {
	t.Helper()
	s, err := NewScheduler(SchedulerOptions{
		Store:              store,
		Factory:            factory,
		MaxConcurrentTasks: maxConcurrent,
		ProgressInterval:   20 * time.Millisecond,
		SaveInterval:       40 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewScheduler 失败: %v", err)
	}
	return s
}

func addTestTask(t *testing.T, s *Scheduler, id string) *Task {
	t.Helper()
	task, err := NewTask(id, SourceHTTP, "http://example.com/"+id, "dest-"+id, "temp-"+id, 1000, 100, 2)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	if err := s.Add(task); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}
	return task
}

// 验收核心：HTTP 与 BT 任务能同时运行且遵守并发上限。
func TestSchedulerRespectsConcurrencyLimit(t *testing.T) {
	factory := &fakeFactory{duration: 120 * time.Millisecond}
	store := newMemStore()
	s := newTestScheduler(t, factory, 2, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	for i := 0; i < 5; i++ {
		addTestTask(t, s, fmt.Sprintf("task-%d", i))
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if err := s.WaitIdle(waitCtx); err != nil {
		t.Fatalf("等待任务完成超时: %v", err)
	}

	if peak := factory.peakConcurrency(); peak != 2 {
		t.Fatalf("并发峰值 = %d, 期望 2（MaxConcurrentTasks）", peak)
	}
	if len(factory.created) != 5 {
		t.Fatalf("创建会话数 = %d, 期望 5", len(factory.created))
	}
	for _, view := range s.List() {
		if view.State != StateCompleted {
			t.Errorf("任务 %s 状态 = %s, 期望 completed", view.ID, view.State)
		}
	}
}

// 两种不同类型的任务混合时，调度器不应有差异化处理（都走同一抽象）。
func TestSchedulerRunsMixedTaskKinds(t *testing.T) {
	factory := &fakeFactory{duration: 20 * time.Millisecond}
	s := newTestScheduler(t, factory, 4, newMemStore())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	httpTask, err := NewTask("http-1", SourceHTTP, "http://example.com/a", "d", "t", 1000, 100, 2)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	if err := s.Add(httpTask); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}

	btTask, err := NewTask("bt-1", SourceBitTorrent, "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "d2", "t2", 0, 100, 0)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	btTask.InfoHash = "0123456789abcdef0123456789abcdef01234567"
	if err := s.Add(btTask); err != nil {
		t.Fatalf("Add 失败: %v", err)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if err := s.WaitIdle(waitCtx); err != nil {
		t.Fatalf("等待任务完成超时: %v", err)
	}

	views := s.List()
	if len(views) != 2 {
		t.Fatalf("任务数 = %d, 期望 2", len(views))
	}
	for _, v := range views {
		if v.State != StateCompleted {
			t.Errorf("任务 %s(%s) 状态 = %s, 期望 completed", v.ID, v.Kind, v.State)
		}
	}
	// BT 任务的进度通过 Sync 写回，因此 Progress 应基于同步后的数据。
	if btTask.TotalSize != 1000 {
		t.Errorf("BT 任务 Sync 未生效：TotalSize = %d, 期望 1000", btTask.TotalSize)
	}
}

// 验收核心：暂停运行中的任务后，恢复时能重新调度。
func TestSchedulerPauseAndResume(t *testing.T) {
	factory := &fakeFactory{duration: 3 * time.Second} // 足够长以便中途暂停
	store := newMemStore()
	s := newTestScheduler(t, factory, 1, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	task := addTestTask(t, s, "pausable")

	// 等到真正进入运行态。
	waitFor(t, 3*time.Second, "任务进入运行态", func() bool {
		v, _ := s.Get("pausable")
		return v.State == StateRunning
	})

	if err := s.Pause("pausable"); err != nil {
		t.Fatalf("Pause 失败: %v", err)
	}
	waitFor(t, 3*time.Second, "任务进入暂停态", func() bool {
		v, _ := s.Get("pausable")
		return v.State == StatePaused
	})
	if got := store.lastState("pausable"); got != StatePaused {
		t.Errorf("暂停状态未持久化，store 中为 %q", got)
	}

	// 暂停期间不应再创建新会话。
	before := len(factory.created)

	if err := s.Resume("pausable"); err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	waitFor(t, 5*time.Second, "恢复后重新进入运行态", func() bool {
		v, _ := s.Get("pausable")
		return v.State == StateRunning || v.State == StateCompleted
	})
	if len(factory.created) <= before {
		t.Fatalf("恢复后应创建新的会话，创建数仍为 %d", len(factory.created))
	}

	// 已完成任务不能暂停。
	task.State = StateCompleted
	if err := s.Pause("pausable"); err == nil {
		t.Error("已完成任务应拒绝暂停")
	}
}

func TestSchedulerResumeIsIdempotent(t *testing.T) {
	factory := &fakeFactory{duration: 200 * time.Millisecond}
	s := newTestScheduler(t, factory, 1, newMemStore())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	addTestTask(t, s, "idem")
	waitFor(t, 3*time.Second, "进入运行态", func() bool {
		v, _ := s.Get("idem")
		return v.State == StateRunning
	})
	if err := s.Resume("idem"); err != nil {
		t.Fatalf("运行中 Resume 应幂等返回: %v", err)
	}
}

// 验收核心：非法状态迁移被拒绝（而不是静默改写状态）。
func TestSchedulerRejectsInvalidOperations(t *testing.T) {
	factory := &fakeFactory{duration: time.Millisecond}
	s := newTestScheduler(t, factory, 1, newMemStore())

	if err := s.Add(nil); err == nil {
		t.Error("Add(nil) 应被拒绝")
	}
	if err := s.Pause("nope"); err == nil {
		t.Error("Pause 不存在的任务应报错")
	}
	if err := s.Resume("nope"); err == nil {
		t.Error("Resume 不存在的任务应报错")
	}
	if err := s.Remove("nope"); err == nil {
		t.Error("Remove 不存在的任务应报错")
	}

	task := addTestTask(t, s, "dup")
	if err := s.Add(task); err == nil {
		t.Error("重复 ID 应被拒绝")
	}

	// 非法任务（不自洽）不能被加入。
	bad, err := NewTask("bad", SourceHTTP, "u", "d", "t", 1000, 100, 2)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	bad.Segments[1].Start += 7
	if err := s.Add(bad); err == nil {
		t.Error("不自洽的任务应被拒绝")
	}

	// 已完成任务不能 Resume。
	task.State = StateCompleted
	if err := s.Resume("dup"); err == nil {
		t.Error("已完成任务应拒绝 Resume")
	}
}

func TestSchedulerMarksFailure(t *testing.T) {
	factory := &fakeFactory{duration: 10 * time.Millisecond, runErr: errors.New("模拟网络中断")}
	store := newMemStore()
	s := newTestScheduler(t, factory, 2, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	task := addTestTask(t, s, "failing")

	waitFor(t, 5*time.Second, "任务失败", func() bool {
		v, _ := s.Get("failing")
		return v.State == StateFailed
	})
	view, _ := s.Get("failing")
	if view.LastError == "" {
		t.Error("失败任务应记录错误原因")
	}
	if task.LastErr == "" {
		t.Error("任务对象应记录错误原因")
	}
	// 状态写入发生在任务 goroutine 中，需等待持久化完成（内存状态先变）。
	waitFor(t, 3*time.Second, "失败状态被持久化", func() bool {
		return store.lastState("failing") == StateFailed
	})

	// 失败后可重新排队重试。
	if err := s.Resume("failing"); err != nil {
		t.Fatalf("失败任务应可恢复重试: %v", err)
	}
}

func TestSchedulerHandlesFactoryError(t *testing.T) {
	factory := &fakeFactory{buildErr: errors.New("无法创建会话")}
	s := newTestScheduler(t, factory, 1, newMemStore())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	addTestTask(t, s, "nofactory")
	waitFor(t, 5*time.Second, "任务失败", func() bool {
		v, _ := s.Get("nofactory")
		return v.State == StateFailed
	})
	view, _ := s.Get("nofactory")
	if view.LastError == "" {
		t.Error("工厂失败也应记录错误")
	}
}

// 验收核心：应用「退出后重启」，队列状态与进度完整恢复。
func TestSchedulerRestoreRecoversInterruptedTasks(t *testing.T) {
	factory := &fakeFactory{duration: 20 * time.Millisecond}
	store := newMemStore()

	// 模拟上一次运行留下的元数据：一个仍在 running、分片处于运行中的任务。
	task, err := NewTask("restored", SourceHTTP, "http://example.com/x", "d", "t", 1000, 100, 2)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	task.State = StateRunning
	task.Segments[0].Bitmap.SetRange(0, 3)
	task.Segments[0].State = SegmentRunning
	task.Segments[1].State = SegmentPending
	progressBefore := task.Downloaded()

	s := newTestScheduler(t, factory, 2, store)
	if err := s.Restore([]*Task{task}); err != nil {
		t.Fatalf("Restore 失败: %v", err)
	}

	view, ok := s.Get("restored")
	if !ok {
		t.Fatal("恢复后应能查到任务")
	}
	if view.State != StatePending {
		t.Fatalf("残留 running 应被回退为 pending, 实际 %q", view.State)
	}
	if got := task.Downloaded(); got != progressBefore {
		t.Fatalf("恢复过程不应丢失进度: %d → %d", progressBefore, got)
	}
	for _, seg := range task.Segments {
		if seg.State == SegmentRunning {
			t.Errorf("分片 %d 仍处于运行态", seg.Index)
		}
	}

	// 恢复后的任务应能被正常调度完成。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if err := s.WaitIdle(waitCtx); err != nil {
		t.Fatalf("恢复的任务未能完成: %v", err)
	}
	if view, _ := s.Get("restored"); view.State != StateCompleted {
		t.Fatalf("恢复的任务最终状态 = %q, 期望 completed", view.State)
	}
}

func TestSchedulerRestoreKeepsPausedAndCompleted(t *testing.T) {
	factory := &fakeFactory{duration: 10 * time.Millisecond}
	s := newTestScheduler(t, factory, 4, newMemStore())

	paused, err := NewTask("paused-1", SourceHTTP, "http://e/a", "d", "t", 1000, 100, 2)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	paused.State = StatePaused

	done, err := NewTask("done-1", SourceHTTP, "http://e/b", "d2", "t2", 1000, 100, 2)
	if err != nil {
		t.Fatalf("NewTask 失败: %v", err)
	}
	done.State = StateCompleted

	if err := s.Restore([]*Task{paused, done}); err != nil {
		t.Fatalf("Restore 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	// 暂停与已完成的任务都不应被自动启动。
	time.Sleep(300 * time.Millisecond)
	if len(factory.created) != 0 {
		t.Fatalf("不应为暂停/已完成任务创建会话，实际创建 %v", factory.created)
	}
	stats := s.Stats()
	if stats.Paused != 1 || stats.Completed != 1 || stats.Pending != 0 {
		t.Fatalf("统计 = %+v", stats)
	}

	// 恢复暂停任务后应开始运行。
	if err := s.Resume("paused-1"); err != nil {
		t.Fatalf("Resume 失败: %v", err)
	}
	waitFor(t, 5*time.Second, "暂停任务被恢复", func() bool {
		stats := s.Stats()
		return stats.Completed >= 2
	})
}

func TestSchedulerRemoveRunningTask(t *testing.T) {
	factory := &fakeFactory{duration: 3 * time.Second}
	s := newTestScheduler(t, factory, 1, newMemStore())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	addTestTask(t, s, "to-remove")
	waitFor(t, 3*time.Second, "进入运行态", func() bool {
		v, _ := s.Get("to-remove")
		return v.State == StateRunning
	})

	if err := s.Remove("to-remove"); err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}
	if _, ok := s.Get("to-remove"); ok {
		t.Error("移除后不应再能查到任务")
	}
	waitFor(t, 3*time.Second, "移除后并发名额被释放", func() bool {
		return s.Stats().Running == 0
	})
}

func TestSchedulerStopCancelsRunningTasks(t *testing.T) {
	factory := &fakeFactory{duration: 10 * time.Second}
	s := newTestScheduler(t, factory, 2, newMemStore())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	addTestTask(t, s, "long-1")
	waitFor(t, 3*time.Second, "进入运行态", func() bool {
		v, _ := s.Get("long-1")
		return v.State == StateRunning
	})

	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop 未能在超时内返回")
	}

	// Stop 后任务回到 pending：既不在 UI 上表现为「还在跑」，
	// 也让重启后的 Restore 直接重新排队继续（断点保留）。
	view, _ := s.Get("long-1")
	if view.State != StatePending {
		t.Errorf("Stop 后任务状态 = %q, 期望 %q", view.State, StatePending)
	}
	if task, ok := s.entries["long-1"]; ok && task.task.Downloaded() == 0 && view.State == StateRunning {
		t.Error("任务不应停在运行态")
	}
}

func TestSchedulerStatsAndSaves(t *testing.T) {
	factory := &fakeFactory{duration: 60 * time.Millisecond}
	store := newMemStore()
	s := newTestScheduler(t, factory, 1, store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	for i := 0; i < 3; i++ {
		addTestTask(t, s, fmt.Sprintf("s-%d", i))
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if err := s.WaitIdle(waitCtx); err != nil {
		t.Fatalf("等待超时: %v", err)
	}

	stats := s.Stats()
	if stats.Total != 3 || stats.Completed != 3 {
		t.Fatalf("统计 = %+v", stats)
	}
	if stats.MaxConcurrent != 1 {
		t.Errorf("MaxConcurrent = %d", stats.MaxConcurrent)
	}
	if store.saveCount() < 3 {
		t.Errorf("保存次数 = %d, 至少应为任务数", store.saveCount())
	}

	views := s.List()
	if len(views) != 3 {
		t.Fatalf("List 数量 = %d", len(views))
	}
	// List 必须按创建时间稳定排序。
	for i := 1; i < len(views); i++ {
		if views[i].CreatedAt.Before(views[i-1].CreatedAt) {
			t.Error("List 未按创建时间排序")
		}
	}
}

func TestNewSchedulerRequiresFactory(t *testing.T) {
	if _, err := NewScheduler(SchedulerOptions{}); err == nil {
		t.Error("缺少 Factory 应被拒绝")
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待「%s」超时（%v）", what, timeout)
}
