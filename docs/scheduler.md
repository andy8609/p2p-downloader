# 统一任务调度与引擎抽象层（T8）

实现：`internal/engine/{downloader.go,state.go,scheduler.go}`（调度与抽象）、
`internal/app/{app.go,session.go}`（按任务类型装配真实引擎）。

---

## 1. 分层与依赖方向

```
internal/engine          任务模型、状态机、调度器      ← 不 import 任何具体引擎
      ↑                                            ↓ 只依赖 SessionFactory 接口
internal/engine/httpdl   HTTP 多段下载
internal/engine/bittorrent  BT / 磁力链
      ↑
internal/app             ✓ 本层：按 Kind 选择实现、管理引擎生命周期
      ↑
internal/control         本地控制面（T10）只依赖 app 与 engine
```

`engine` 定义接口但不 import 具体实现，因此「按 Kind 选择实现」这一步必须放在更高一层。
这样调度逻辑可以用假实现完整测试（`scheduler_test.go` 不依赖网络与磁盘），
而真实装配集中在 `app/session.go`。

---

## 2. `Session` 接口契约

```go
type Session interface {
    Run(ctx context.Context) error   // 阻塞直到完成 / 取消 / 失败
    Pause(ctx context.Context) error // 请求暂停，保留进度
    Progress() Progress              // 瞬时进度，可并发调用
    Sync(ctx context.Context) error  // 把引擎内部进度写回 Task（供持久化）
    Close() error                    // 释放资源，不删除已下载数据
}
```

| 方法 | HTTP 实现 | BT 实现 |
|---|---|---|
| `Run` | `httpdl.Downloader.Run` | `Torrent.Download` |
| `Pause` | 空操作（调度器取消 ctx 即暂停，httpdl 会把分片退回 pending） | `Torrent.Pause()`（`DisallowDataDownload`） |
| `Progress` | `httpdl.Stats()` | `Torrent.Snapshot()` |
| `Sync` | 空操作（下载器直接写同一个 `*Task`） | `Torrent.ApplyToTask()`（把 piece 位图写入 Task） |
| `Close` | 数据完整则 `Commit`（原子重命名），否则只 `Close` 保留临时文件 | 空操作（数据直接落在最终位置） |

**`Run` 的取消语义是契约的核心**：返回 `context.Canceled` 表示「可续传的中断」，
已落盘的进度必须保留。两种引擎都满足——HTTP 靠位图 + 临时文件，BT 靠库的 piece completion。

---

## 3. 状态机

`internal/engine/state.go` 中的 `allowedTransitions` 是任务生命周期的唯一权威定义，
所有变更都必须走 `Task.SetState`，非法迁移返回 `*ErrInvalidTransition`。

```
pending ──▶ running ──┬─▶ verifying ──▶ completed
   ▲   ▲              ├─▶ completed            │
   │   │              ├─▶ paused ──▶ running   │
   │   └──────────────┴─▶ failed ──▶ pending ◀─┘（用户显式重新下载）
   └── 重新排队
```

被明确**拒绝**的迁移（有测试覆盖）：`completed → running`、`completed → verifying`、
`completed → failed`、`pending → verifying`、`paused → verifying`。
含义是：终态不可被后台流程改写，只有用户显式的「重新下载」能让已完成任务回到 `pending`。

---

## 4. 调度策略

- **并发上限**：`MaxConcurrentTasks`（默认 3）。每个任务内部已有多个并发连接，
  任务级并发过大会互相抢带宽。名额用带缓冲 channel 实现，`dispatch` goroutine 只在此处发送。
- **队列**：FIFO（`order` 切片）。被暂停/恢复的任务会重新排到队列**头部**，
  避免用户显式操作的恢复被排在长队之后。
- **事件驱动**：`wake` channel + `select`，不做忙等。任务结束、任务入队、暂停/恢复、
  停止都会 `signal()`。`wake` 容量为 1，重复信号会合并。
- **进度回写**：每个运行中的任务有一个 progress goroutine，按 `ProgressInterval`（500ms）
  调用 `Session.Sync()`，按 `SaveInterval`（3s）落盘元数据。
- **`WaitIdle`**：等待运行数与队列都归零，供测试与优雅退出使用。

---

## 5. 恢复语义（T8 验收项 3）

`App.Start` 从磁盘读回任务后交给 `Scheduler.Restore`，逐类处理：

| 读回时的状态 | 处理 | 理由 |
|---|---|---|
| `running` / `verifying` | **回退为 `pending` 并排队** | 进程被强杀/退出留下的残留态，重启后应自动继续 |
| `paused` | 保持暂停，不排队 | 这是用户主动暂停，必须尊重 |
| `failed` | 保持失败，不自动重试 | 避免重启引发无限重试风暴；由用户决定是否重试 |
| `completed` | 保持完成 | 数据已在磁盘上 |
| 元数据损坏 | 隔离到 `corrupt/` 并跳过 | 单个任务损坏不影响其余任务启动 |

回退分片时调用的 `Task.RequeueInterrupted` 会把残留在「运行中」的分片退回待下载，
但不触碰其位图——因此已下载的字节数不会丢失。

### 5.1 一个必须区分的语义差异

「用户暂停」与「调度器停止」都会让 `Run` 返回 `context.Canceled`，但持久化结果必须不同：

- 用户暂停 → 存 `paused`，重启后保持暂停；
- 调度器停止（应用退出）→ **保持未完成语义**，重启后自动续传。

初版实现没区分，导致「退出应用再打开」会把手头的任务全部变成暂停，
用户必须逐个手动恢复。修复方式：`entry.pauseRequested` 记录用户意图，
只有它才允许把状态写成 `paused`。

验证（`TestAppResumesInterruptedTaskAfterRestart`）：

```
第一次运行下载了 524288/1048576 字节（应用被停止）
重启后 0.18 秒内完成剩余部分，文件 SHA-256 与源一致
```

---

## 6. 测试覆盖

| 验收项 | 测试 |
|---|---|
| HTTP 与 BT 任务同时运行且互不干扰 | `app.TestAppRunsHTTPAndBitTorrentConcurrently`（同一调度器下并发跑，两份文件的 SHA-256 均校验） |
| 并发上限生效 | `engine.TestSchedulerRespectsConcurrencyLimit`（5 个任务、上限 2、断言峰值恰好 2） |
| 非法状态迁移被拒绝 | `engine.TestSetStateRejectsIllegalTransition`、`TestSchedulerRejectsInvalidOperations` |
| 状态机完整性 | `TestCanTransitionMatrix`、`TestAllStatesReachable` |
| 重启后队列与进度恢复 | `app.TestAppRestoresQueueAfterRestart`、`TestAppResumesInterruptedTaskAfterRestart`、`engine.TestSchedulerRestoreRecoversInterruptedTasks` |
| 暂停/恢复 | `engine.TestSchedulerPauseAndResume`、`TestSchedulerResumeIsIdempotent` |
| 失败与工厂错误 | `engine.TestSchedulerMarksFailure`、`TestSchedulerHandlesFactoryError` |
| 移除运行中任务、停止调度器 | `engine.TestSchedulerRemoveRunningTask`、`TestSchedulerStopCancelsRunningTasks` |
| 资源清理 | `app.TestAppRemoveTask`（元数据 + 数据文件） |

`engine` 的调度测试全部使用假工厂，不碰网络与磁盘，单包 30 个用例在 1 秒内跑完。
