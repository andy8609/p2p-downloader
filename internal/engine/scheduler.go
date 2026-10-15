package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"p2pdl/internal/engine/limiter"
)

// 调度器默认值。
const (
	// DefaultMaxConcurrentTasks 是同时运行的任务数上限。
	// 取 3 而非更大：每个任务内部已有多个并发连接，任务级并发过大反而互相抢带宽。
	DefaultMaxConcurrentTasks = 3
	// defaultProgressInterval 是进度回写间隔。
	defaultProgressInterval = 500 * time.Millisecond
	// defaultSaveInterval 是元数据落盘间隔。
	defaultSaveInterval = 3 * time.Second
)

// SchedulerOptions 配置调度器。
type SchedulerOptions struct {
	// Store 用于持久化任务元数据；为 nil 时纯内存运行（测试用）。
	Store TaskStore
	// Factory 创建具体引擎会话。
	Factory SessionFactory
	// Verifier 在数据收齐后校验；为 nil 时跳过校验。
	Verifier Verifier
	// MaxConcurrentTasks 是同时运行的任务数上限，<=0 时取默认值。
	MaxConcurrentTasks int
	// ProgressInterval 是进度回写间隔。
	ProgressInterval time.Duration
	// SaveInterval 是元数据落盘间隔。
	SaveInterval time.Duration
	// RunOptions 是传给引擎的全局资源策略。
	RunOptions RunOptions
}

func (o *SchedulerOptions) normalize() error {
	if o.Factory == nil {
		return errors.New("engine: 调度器缺少 SessionFactory")
	}
	if o.MaxConcurrentTasks <= 0 {
		o.MaxConcurrentTasks = DefaultMaxConcurrentTasks
	}
	if o.ProgressInterval <= 0 {
		o.ProgressInterval = defaultProgressInterval
	}
	if o.SaveInterval <= 0 {
		o.SaveInterval = defaultSaveInterval
	}
	return nil
}

// entry 是调度器内部的任务条目。
type entry struct {
	task    *Task
	session Session
	cancel  context.CancelFunc
	// queued 表示该任务正在等待调度（避免重复入队）。
	queued bool
	// pauseRequested 区分「用户主动暂停」与「调度器停止」：
	// 前者要把 paused 持久化（重启后保持暂停），后者要保持「未完成、应继续」的语义
	// （重启后由 Restore 回退为 pending 并自动续传）。
	pauseRequested bool
	// done 在会话 goroutine 真正退出后关闭。
	// 删除任务时必须等它：否则会话可能在 Remove 返回后又写回元数据或数据文件。
	done chan struct{}
}

// Scheduler 负责任务队列、并发上限与状态机推进。
//
// 并发模型：一个 dispatch goroutine 负责派发，每个运行中的任务再有一个
// progress goroutine。所有共享状态都由 mu 保护，wake 用于事件驱动唤醒。
type Scheduler struct {
	opts SchedulerOptions

	mu      sync.Mutex
	entries map[string]*entry
	order   []string // 待调度任务 ID 的 FIFO 顺序
	running int
	stopped bool

	sem  chan struct{}
	wake chan struct{}

	wg sync.WaitGroup

	// dispatchDone 在派发循环退出后关闭。
	dispatchDone chan struct{}
}

// NewScheduler 创建调度器。此时不会启动任何 goroutine。
func NewScheduler(opts SchedulerOptions) (*Scheduler, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	return &Scheduler{
		opts:         opts,
		entries:      make(map[string]*entry),
		sem:          make(chan struct{}, opts.MaxConcurrentTasks),
		wake:         make(chan struct{}, 1),
		dispatchDone: make(chan struct{}),
	}, nil
}

func (s *Scheduler) logf(format string, args ...any) {
	if s.opts.RunOptions.Logf != nil {
		s.opts.RunOptions.Logf(format, args...)
	}
}

// Restore 把持久化读回的任务装载进调度器。
//
// 每个任务都会先经过 RequeueInterrupted：把元数据里残留的「运行中」状态
// 回退为可调度状态。这是「强杀后重启能继续」的关键一步——否则任务会永远
// 停在 running 而不会被重新调度。
func (s *Scheduler) Restore(tasks []*Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, t := range tasks {
		if t == nil {
			continue
		}
		if err := t.Validate(); err != nil {
			return fmt.Errorf("engine: 恢复任务 %s 失败: %w", t.ID, err)
		}
		interrupted := t.RequeueInterrupted()
		if interrupted > 0 {
			s.logf("engine: 任务 %s 有 %d 个中断的分片被回退为待下载", t.ID, interrupted)
		}
		s.entries[t.ID] = &entry{task: t}
		// 已完成的任务停在终态；失败的任务保持失败等用户决定；
		// 用户主动暂停的任务保持暂停——只有非用户意图的残留状态才重新排队。
		switch t.State {
		case StateCompleted, StateFailed, StatePaused:
			continue
		}
		t.State = StatePending
		s.enqueueLocked(t.ID)
	}
	return nil
}

// Add 新增一个任务并排队。若 ID 已存在则返回错误。
func (s *Scheduler) Add(t *Task) error {
	if t == nil {
		return errors.New("engine: 任务为空")
	}
	if err := t.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	if _, exists := s.entries[t.ID]; exists {
		s.mu.Unlock()
		return fmt.Errorf("engine: 任务 %s 已存在", t.ID)
	}
	s.entries[t.ID] = &entry{task: t}
	if t.State == StateRunning || t.State == StateVerifying {
		t.State = StatePending
	}
	s.enqueueLocked(t.ID)
	s.mu.Unlock()

	s.save(t)
	s.signal()
	return nil
}

// Start 启动调度循环（非阻塞）。返回的 stop 函数用于停止调度。
func (s *Scheduler) Start(ctx context.Context) (stop func()) {
	s.wg.Add(1)
	go s.dispatch(ctx)

	return func() {
		s.Stop()
	}
}

// Stop 停止调度：取消所有运行中的任务并等待 goroutine 退出。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		s.wg.Wait()
		return
	}
	s.stopped = true
	cancels := make([]context.CancelFunc, 0, len(s.entries))
	for _, e := range s.entries {
		if e.cancel != nil {
			cancels = append(cancels, e.cancel)
		}
	}
	s.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	// 唤醒派发循环，让它看到 stopped 并退出。
	s.signal()
	s.wg.Wait()
}

// Pause 暂停任务。运行中的任务会被取消其 context（进度保留），
// 未运行的任务直接标记为暂停。
func (s *Scheduler) Pause(id string) error {
	s.mu.Lock()
	e, ok := s.entries[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("engine: 未找到任务 %s", id)
	}
	if e.task.State == StateCompleted {
		s.mu.Unlock()
		return fmt.Errorf("engine: 任务 %s 已完成，无法暂停", id)
	}
	cancel := e.cancel
	e.queued = false
	e.pauseRequested = true
	s.removeFromOrderLocked(id)
	// 先记录用户意图：运行中的任务被取消后不应回到队列。
	// running→paused 与 pending→paused 都是合法迁移；其余情况（如 completed）已在上面拦下，
	// 此处忽略错误以保证 Pause 幂等。
	_ = e.task.SetState(StatePaused)
	task := e.task
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.save(task)
	s.signal()
	return nil
}

// Resume 恢复被暂停的任务，重新排队。
func (s *Scheduler) Resume(id string) error {
	s.mu.Lock()
	e, ok := s.entries[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("engine: 未找到任务 %s", id)
	}
	switch e.task.State {
	case StatePaused, StateFailed:
		e.task.State = StatePending
		e.pauseRequested = false
	case StatePending, StateRunning, StateVerifying:
		s.mu.Unlock()
		return nil // 已在推进中，幂等返回
	case StateCompleted:
		s.mu.Unlock()
		return fmt.Errorf("engine: 任务 %s 已完成，无需恢复", id)
	}
	task := e.task
	s.enqueueLocked(id)
	s.mu.Unlock()

	s.save(task)
	s.signal()
	return nil
}

// Remove 移除任务。deleteFiles 为 true 时要求调用方自行删除数据文件
// （引擎层不持有文件路径语义，删除由上层执行）。
func (s *Scheduler) Remove(id string) error {
	s.mu.Lock()
	e, ok := s.entries[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("engine: 未找到任务 %s", id)
	}
	cancel := e.cancel
	session := e.session
	done := e.done
	delete(s.entries, id)
	s.removeFromOrderLocked(id)
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if session != nil {
		_ = session.Close()
	}

	// 等待会话 goroutine 真正退出，再让调用方去删除数据与元数据。
	// 否则会话可能在删除完成后又写回元数据（实测导致「删除后任务又出现」）。
	if done != nil {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			s.logf("engine: 等待任务 %s 的会话退出超时，仍继续移除", id)
		}
	}
	s.signal()
	return nil
}

// Requeue 把任务重新排队，用于「重新校验」与「重新下载」。
//
// 对已完成的任务，这是唯一能把它们重新投入调度的入口（状态机只允许
// completed → pending 这一条离开终态的路径，且必须由用户显式触发）。
func (s *Scheduler) Requeue(id string) error {
	s.mu.Lock()
	e, ok := s.entries[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("engine: 未找到任务 %s", id)
	}
	if e.cancel != nil {
		// 正在运行：取消后由 runSession 回落到 pending。
		cancel := e.cancel
		s.mu.Unlock()
		cancel()
		return nil
	}
	if e.task.State != StatePending {
		if err := e.task.SetState(StatePending); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	e.task.LastErr = ""
	task := e.task
	s.enqueueLocked(id)
	s.mu.Unlock()

	s.save(task)
	s.signal()
	return nil
}

// SetGlobalLimiter 替换全局限速器，影响之后创建的所有会话。
//
// 注意：已运行中的会话持有创建时的 RunOptions 副本，不会自动切换到新实例；
// 但若只是通过 Limiter.SetLimit 调整速率（常用的情形），因为共享同一个实例，
// 所有会话会立即生效。
func (s *Scheduler) SetGlobalLimiter(l *limiter.Limiter) {
	s.mu.Lock()
	s.opts.RunOptions.GlobalLimiter = l
	s.mu.Unlock()
	s.signal()
}

// Task 返回调度器内部持有的任务对象（含 TempPath 等未暴露到 TaskView 的字段）。
//
// 供需要完整路径信息的内部调用方使用，例如「删除任务并清理数据文件」。
// 返回的是内部指针：调用方只应在任务已停止/移除后读取其路径字段。
func (s *Scheduler) Task(id string) (*Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[id]
	if !ok {
		return nil, false
	}
	return e.task, true
}

// Get 返回任务的只读视图。
func (s *Scheduler) Get(id string) (TaskView, bool) {
	s.mu.Lock()
	e, ok := s.entries[id]
	var view TaskView
	if ok {
		view = e.task.View()
		// 速率/对等方数量只存在于会话存活期间；模式则优先用会话的实时值，
		// 会话结束后回退到任务上持久化的值，避免任务一完成「模式」就凭空消失。
		rate, peers, mode := s.progressOfLocked(e)
		view.Rate, view.Peers = rate, peers
		if mode != "" {
			view.Mode = mode
		}
	}
	s.mu.Unlock()
	return view, ok
}

// List 返回全部任务视图（按创建时间升序，保证输出稳定）。
func (s *Scheduler) List() []TaskView {
	s.mu.Lock()
	views := make([]TaskView, 0, len(s.entries))
	for _, e := range s.entries {
		view := e.task.View()
		rate, peers, mode := s.progressOfLocked(e)
		view.Rate, view.Peers = rate, peers
		if mode != "" {
			view.Mode = mode
		}
		views = append(views, view)
	}
	s.mu.Unlock()

	sort.Slice(views, func(i, j int) bool {
		if views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].ID < views[j].ID
		}
		return views[i].CreatedAt.Before(views[j].CreatedAt)
	})
	return views
}

// Stats 是调度器级统计。
type Stats struct {
	Total     int `json:"total"`
	Running   int `json:"running"`
	Pending   int `json:"pending"`
	Paused    int `json:"paused"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	// MaxConcurrent 是并发上限，便于控制面展示「为什么任务没开始」。
	MaxConcurrent int `json:"maxConcurrent"`
}

// Stats 返回调度器统计。
func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Total: len(s.entries), MaxConcurrent: s.opts.MaxConcurrentTasks}
	for _, e := range s.entries {
		switch e.task.State {
		case StateRunning:
			st.Running++
		case StatePending:
			st.Pending++
		case StatePaused:
			st.Paused++
		case StateCompleted:
			st.Completed++
		case StateFailed:
			st.Failed++
		}
	}
	return st
}

// WaitIdle 等待所有任务离开运行态（用于测试与优雅退出）。
func (s *Scheduler) WaitIdle(ctx context.Context) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		busy := s.running > 0 || len(s.order) > 0
		s.mu.Unlock()
		if !busy {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// ---------------------------------------------------------------------------
// 内部实现
// ---------------------------------------------------------------------------

func (s *Scheduler) enqueueLocked(id string) {
	e, ok := s.entries[id]
	if !ok || e.queued {
		return
	}
	e.queued = true
	s.order = append(s.order, id)
}

func (s *Scheduler) removeFromOrderLocked(id string) {
	for i, queuedID := range s.order {
		if queuedID == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			return
		}
	}
}

// nextLocked 取出下一个可调度的任务（从队首开始跳过状态已变的条目）。
func (s *Scheduler) nextLocked() *entry {
	for len(s.order) > 0 {
		id := s.order[0]
		s.order = s.order[1:]
		e, ok := s.entries[id]
		if !ok {
			continue
		}
		e.queued = false
		if e.task.State != StatePending {
			continue
		}
		return e
	}
	return nil
}

func (s *Scheduler) pushBackLocked(e *entry) {
	if e.queued {
		return
	}
	e.queued = true
	s.order = append([]string{e.task.ID}, s.order...)
}

func (s *Scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) dispatch(ctx context.Context) {
	defer s.wg.Done()
	defer close(s.dispatchDone)

	for {
		// 尽量填满并发名额。
		for {
			s.mu.Lock()
			if s.stopped {
				s.mu.Unlock()
				return
			}
			if len(s.sem) >= cap(s.sem) {
				s.mu.Unlock()
				break
			}
			e := s.nextLocked()
			if e == nil {
				s.mu.Unlock()
				break
			}
			e.task.State = StateRunning
			e.task.Touch()
			s.running++
			s.sem <- struct{}{}
			runCtx, cancel := context.WithCancel(ctx)
			e.cancel = cancel
			e.done = make(chan struct{})
			s.mu.Unlock()

			s.wg.Add(1)
			go func(e *entry, runCtx context.Context) {
				defer s.wg.Done()
				defer func() {
					<-s.sem
					s.mu.Lock()
					s.running--
					s.mu.Unlock()
					s.signal()
				}()
				s.runSession(runCtx, e)
			}(e, runCtx)
		}

		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.stopped = true
			s.mu.Unlock()
			return
		case <-s.wake:
		}
	}
}

// runSession 执行一个任务直到结束，并把结果反映到状态机上。
func (s *Scheduler) runSession(ctx context.Context, e *entry) {
	defer func() {
		// 关闭 done 让 Remove 知道「会话已完全退出」，可以安全清理数据与元数据。
		s.mu.Lock()
		done := e.done
		e.done = nil
		s.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()

	task := e.task

	session, err := s.opts.Factory.Create(ctx, task, s.opts.RunOptions)
	if err != nil {
		s.mu.Lock()
		e.cancel = nil
		s.mu.Unlock()
		s.fail(task, fmt.Errorf("创建下载会话失败: %w", err))
		return
	}

	s.mu.Lock()
	e.session = session
	s.mu.Unlock()

	stopProgress := make(chan struct{})
	var progressWG sync.WaitGroup
	progressWG.Add(1)
	go func() {
		defer progressWG.Done()
		s.trackProgress(ctx, e, stopProgress)
	}()

	runErr := session.Run(ctx)

	close(stopProgress)
	progressWG.Wait()

	s.mu.Lock()
	e.session = nil
	e.cancel = nil
	s.mu.Unlock()

	if closeErr := session.Close(); closeErr != nil {
		s.logf("engine: 关闭任务 %s 的会话出错: %v", task.ID, closeErr)
	}

	switch {
	case runErr == nil:
		s.finish(task, session)
	case errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded):
		// 区分用户暂停与调度器停止：前者持久化 paused（重启后保持暂停），
		// 后者保留「未完成」语义（重启后自动续传）。
		s.mu.Lock()
		pausedByUser := e.pauseRequested
		e.pauseRequested = false
		s.mu.Unlock()

		if pausedByUser {
			if err := task.SetState(StatePaused); err != nil {
				s.logf("engine: 任务 %s 置为暂停失败: %v", task.ID, err)
			}
		} else {
			// 调度器停止/移除：回到 pending，既不在 UI 上表现为「还在跑」，
			// 也让重启后的 Restore 直接把任务重新排队继续。
			s.logf("engine: 任务 %s 因调度器停止而中断，断点已保留", task.ID)
			if err := task.SetState(StatePending); err != nil {
				s.logf("engine: 任务 %s 回退为待调度失败: %v", task.ID, err)
			}
		}
		s.save(task)
	default:
		s.fail(task, runErr)
	}
}

// finish 在传输完成后执行校验并推进到终态。
func (s *Scheduler) finish(task *Task, session Session) {
	if err := session.Sync(context.Background()); err != nil {
		s.logf("engine: 同步任务 %s 进度失败: %v", task.ID, err)
	}

	if s.opts.Verifier != nil {
		// running → verifying 是正常路径；若引擎在结束时已把状态推进到终态
		// （单流模式曾有此行为），则跳过校验态并直接校验，避免非法迁移。
		if task.State != StateCompleted {
			if err := task.SetState(StateVerifying); err != nil {
				s.logf("engine: 任务 %s 进入校验态失败: %v", task.ID, err)
			}
			s.save(task)
		}

		result, err := s.opts.Verifier.Verify(context.Background(), task)
		switch {
		case err != nil:
			s.fail(task, err)
			return
		case result.OK:
			// 继续向下推进到 completed
		case result.Repaired:
			// 已定位到损坏区间并标记重下：任务重新排队。
			// VerifyAttempts 由校验器递增，超过上限时会返回错误而非无限重下。
			if err := task.SetState(StatePending); err != nil {
				s.fail(task, fmt.Errorf("重新排队失败: %w", err))
				return
			}
			task.LastErr = result.Reason
			s.save(task)
			s.mu.Lock()
			s.enqueueLocked(task.ID)
			s.mu.Unlock()
			s.signal()
			s.logf("engine: 任务 %s 校验后需重下: %s", task.ID, result.Reason)
			return
		default:
			err := result.Err
			if err == nil {
				err = errors.New(result.Reason)
			}
			s.fail(task, err)
			return
		}
	}
	if err := task.SetState(StateCompleted); err != nil {
		s.logf("engine: 任务 %s 状态推进失败: %v", task.ID, err)
	}
	// 成功完成意味着之前所有修复尝试都已被证明有效：计数归零，
	// 使同一任务在后来的重新校验中仍有完整的修复额度。
	task.VerifyAttempts = 0
	task.RepairedBytes = 0
	// 成功就是成功：清空历史错误信息。
	// 只按前缀匹配（例如仅清「定位到…」）会在任务最终成功时留下一条
	// 早已失效的错误提示，用户看到「已完成」却挂着报错，无法判断到底成没成。
	task.LastErr = ""
	s.save(task)
}

func (s *Scheduler) fail(task *Task, err error) {
	task.LastErr = err.Error()
	_ = task.SetState(StateFailed)
	s.logf("engine: 任务 %s 失败: %v", task.ID, err)
	s.save(task)
}

// trackProgress 周期把进度写回任务并落盘。
func (s *Scheduler) trackProgress(ctx context.Context, e *entry, stop <-chan struct{}) {
	progressTicker := time.NewTicker(s.opts.ProgressInterval)
	saveTicker := time.NewTicker(s.opts.SaveInterval)
	defer progressTicker.Stop()
	defer saveTicker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-progressTicker.C:
			s.syncSession(e)
		case <-saveTicker.C:
			if s.syncSession(e) == nil {
				s.save(e.task)
			}
		}
	}
}

// syncSession 把会话内部进度写回任务对象，返回遇到的错误。
func (s *Scheduler) syncSession(e *entry) error {
	s.mu.Lock()
	session := e.session
	s.mu.Unlock()
	if session == nil {
		return nil
	}
	if err := session.Sync(context.Background()); err != nil {
		return err
	}
	// 记录实际生效的传输模式：会话结束后仍需展示（例如「多段并发」还是「单流降级」）。
	if p := session.Progress(); p.Mode != "" {
		s.mu.Lock()
		e.task.Mode = p.Mode
		s.mu.Unlock()
	}
	return nil
}

func (s *Scheduler) save(task *Task) {
	if s.opts.Store == nil {
		return
	}
	// 已被移除的任务不再回写：否则「删除任务」之后元数据会被后台 goroutine 重新写出来。
	s.mu.Lock()
	_, stillManaged := s.entries[task.ID]
	s.mu.Unlock()
	if !stillManaged {
		return
	}
	if err := s.opts.Store.Save(task); err != nil {
		s.logf("engine: 保存任务 %s 失败: %v", task.ID, err)
	}
}

// progressOfLocked 读取会话的瞬时进度；无会话时用任务自身的数据。
func (s *Scheduler) progressOfLocked(e *entry) (rate float64, peers int, mode string) {
	if e.session == nil {
		return 0, 0, ""
	}
	p := e.session.Progress()
	return p.Rate, p.Peers, p.Mode
}
