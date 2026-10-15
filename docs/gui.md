# 桌面壳集成（T12）

实现：`cmd/p2pdl-gui`（Wails v2 + WebView2 入口）、`internal/gui/`（进程管理与服务层）、
`cmd/p2pdl-gui/frontend/`（纯静态前端）。

---

## 1. 架构：界面与引擎是两个进程

```
┌──────────────────────────┐        HTTP/SSE (127.0.0.1)      ┌──────────────────────────┐
│  p2pdl-gui.exe (Wails)   │ ───────────────────────────────▶ │  p2pdl-engine.exe        │
│  · 窗口与界面            │                                  │  · 调度器 / 下载引擎      │
│  · 引擎进程生命周期       │ ◀─────────────────────────────── │  · 本地控制面            │
│  · 每秒轮询 GetState     │            任务视图/统计          │  · 断点与校验            │
└──────────────────────────┘                                  └──────────────────────────┘
```

| 选择 | 理由 |
|---|---|
| 引擎独立进程 | 界面崩溃或卡顿都不中断下载；这也是验收项「GUI 卡顿不影响下载」的结构性保证 |
| 优先复用已有引擎 | 用户可能先开了 CLI。两个引擎会争抢同一数据目录的 bolt piece-completion 数据库（见 `bittorrent.md` §3.3） |
| 前端每秒轮询而非事件推送 | 状态总量小；轮询让「引擎崩溃 / 界面重连」的异常路径无需额外状态机 |
| 纯静态前端（无 npm） | 保证「一条命令构建」，不引入 Node 工具链与构建缓存 |

---

## 2. 启动链路

`p2pdl-gui.exe` 启动后：

1. `Service.Startup` → `EngineManager.EnsureRunning`
2. 先尝试 `client.Discover(metaDir)`：读端点文件 + 令牌并做一次健康检查
   - 成功 → 复用（`Owned=false`，退出时**不会**杀掉别人的引擎）
3. 失败 → 按 `p2pdl-engine.exe → dlengine.exe → dl.exe` 顺序查找引擎二进制（优先同目录），
   以 `serve --data-dir … --meta-dir …` 启动子进程
4. 轮询端点文件直到引擎就绪（默认 20 秒超时）
5. 前端每秒 `GetState()`，逐帧重绘任务列表

**目录统一为绝对路径**：GUI 会在创建服务时把 `--data-dir` / `--meta-dir` 转成绝对路径，
引擎写端点文件时也会把 `tokenFile` 转绝对路径。否则以相对路径启动时，
从别的工作目录运行的 CLI 会读不到令牌、或把文件下载到意外位置（实测踩到过）。

---

## 3. 崩溃与重启

| 场景 | 行为 |
|---|---|
| 引擎被外部杀掉 | `EngineManager.watch` 通过 `cmd.Wait()` 立即感知，`Status().LastError` 变为「引擎进程已退出: …」，界面状态灯变红并显示原因 |
| 用户点「重启引擎」 | `RestartEngine` → `Stop` + `Start`，界面自动恢复 |
| GUI 被强杀（任务管理器结束进程） | 引擎成为孤儿进程，**继续下载**；下次打开 GUI 会通过端点文件复用它 |
| 正常关闭窗口 | `Shutdown` 会停止由本壳启动的引擎，保证「关窗口 = 完全退出」的可预期行为 |

`GetState` 在引擎不可用时不会报错或阻塞：返回 `engineRunning=false` 与可读的 `engineError`，
前端照常渲染（有单测覆盖：`TestServiceGetStateIsFast` 断言 20 次调用 < 2 秒）。

---

## 4. 界面功能

- 顶部：引擎状态灯（运行中 / 未运行 + 原因）、一键重启引擎
- 输入区：URL / 磁力链 / `.torrent` 路径（按前缀自动识别）、可选期望 SHA-256、暂停创建
- 概览：任务数 / 运行中 / 已完成 / 失败、全局限速下拉、打开下载目录
- 任务行：名称与落盘路径、进度条与百分比、速度、已下载/总大小、状态徽章、失败原因、
  操作按钮（暂停 / 继续 / 校验 / 删除）
- 底部：可折叠运行日志（含引擎输出，便于排障）

删除任务默认**只删任务记录、保留数据文件**，避免误删已完成的内容。

---

## 5. 构建与运行

```powershell
# 构建（需要 -tags desktop,production，Wails 的资产服务依赖它）
go build -tags desktop,production -o dist\p2pdl-gui.exe ./cmd/p2pdl-gui
go build -o dist\p2pdl-engine.exe ./cmd/dlengine

# 运行
.\dist\p2pdl-gui.exe --data-dir D:\downloads
```

`build.ps1` 已包含 `gui` 目标：

```powershell
.\build.ps1 -Target gui
```

**依赖**：Windows 上需要 WebView2 Runtime。本机实测已安装 `153.0.4234.48`
（T1 风险 #2 早已识别，此处确认可用）。打包时对缺失环境的引导见 `docs/packaging.md`（T14）。

---

## 6. 测试覆盖与已知限制

`internal/gui/service_test.go` 共 11 个用例全部通过。为免在测试中调用 `go build`，
测试二进制在 `P2PDL_TEST_AS_ENGINE=1` 时会把自身当作引擎运行（`TestMain` 分支），
因此 `EngineManager` 启动的是一个**真实的、独立进程的引擎**。

| 验收项 | 覆盖方式 |
|---|---|
| 双击运行后能下载 | 真实二进制冒烟：`p2pdl-gui.exe` 启动 → 拉起 `p2pdl-engine.exe`（pid 可查）→ 端点文件生成 → CLI 连上同一引擎添加任务 → **文件落盘哈希一致** |
| 杀掉引擎后 GUI 能提示 | `TestEngineManagerDetectsEngineCrash`（`LastError` = 「引擎进程已退出: exit status 1」） |
| 支持重启引擎 | `TestEngineManagerRestartsAfterCrash`（PID 变化且控制面恢复可用） |
| GUI 卡顿不影响下载 | 结构性保证（引擎是独立进程），并有 `TestServiceGetStateIsFast` 保证轮询不拖住界面 |
| 任务操作闭环 | `TestServiceAddAndManageTask`、`TestServicePauseAndResume`、`TestServiceRateLimitAndLogs` |
| 引擎未运行时界面可用 | `TestServiceValidationAndEngineDown` |

**已知限制（诚实记录）**：

1. **窗口渲染与肉眼可见的进度条无法在无人值守环境中断言**。自动化覆盖到的是
   「进程启动 → 引擎被拉起 → 控制面可访问 → 任务可创建与推进」这条完整链路，
   以及前端所依赖的全部数据结构；像素级渲染留给 T15 的人工验收。
2. `Stop-Process -Force` 强杀 GUI 后引擎会成为孤儿进程（设计如此），
   需要复用或在任务管理器结束 `p2pdl-engine.exe`。

---

## 7. T13：功能完善

### 7.1 新增能力

| 能力 | 实现要点 |
|---|---|
| **拖拽添加** | 整窗口拖放层（用计数器抵消子元素造成的 dragenter/dragleave 抖动）。拖入 `.torrent` 取文件路径直接建任务；拖入文本按链接/磁力链处理 |
| **剪贴板识别** | 每 1.5 秒检测一次，仅在**窗口获得焦点**时读取（不在后台偷偷访问剪贴板）；只在输入框为空时自动填入，不打断用户正在输入的内容；`isLikelySource` 过滤掉普通文本 |
| **剩余时间 (ETA)** | 由瞬时速率外推；速率过低、进度未知或预计超过 7 天时返回空串，避免显示 `∞` 或荒谬数值 |
| **速度曲线** | 后端每秒采样一次（保留 120 点 ≈ 2 分钟），前端用 canvas 绘制 sparkline；按 `devicePixelRatio` 放大避免发虚；只为运行中/暂停中的任务拉取 |
| **设置面板** | 明确区分「启动期设置」（目录/并发/端口/分块，改后提示需重启）与「运行期设置」（限速，保存即生效） |
| **通知** | 任务完成/失败时产生提醒（可在设置中关闭）；用「拉取即清空」的队列，无需订阅状态机 |
| **关闭确认** | 仍有任务在跑时询问：保留后台下载 / 停止并退出 / 取消 |

### 7.2 配置优先级（单一真相来源）

```
命令行参数  >  设置文件（%APPDATA%\p2pdl\settings.json）  >  内置默认值
```

引擎与桌面壳**读同一个设置文件**：GUI 以 `--settings <path>` 启动引擎子进程，
因此界面里改的「启动期设置」在重启引擎后确实会生效（实测：设置文件写入
`maxConcurrentTasks: 2`、`globalRateLimit: 2097152` 后，CLI `status` 显示
「并发上限: 2 / 全局限速: 2.00 MB/s」）。

设置文件损坏时不会让界面起不来：`config.Load` 回退到默认值并在日志中记录原因。

### 7.3 更新的测试覆盖

`internal/gui` 共 27 个用例全部通过，T13 新增覆盖：

| 验收项 | 测试 |
|---|---|
| 设置往返与默认值 | `TestSettingsRoundTripAndDefaults`、`TestSettingsNormalizeFillsDefaults` |
| 非法设置被拒绝（区分 Normalize 修复与 Validate 拒绝） | `TestSettingsValidationRejectsBadValues` |
| 「需重启才生效」的判定 | `TestDiffRestartRequired`、`TestServiceSaveSettingsAppliesRateLimitImmediately`（限速立即生效且不提示重启；并发变更提示重启并落盘） |
| 设置从磁盘加载 / 命令行覆盖 | `TestServiceSettingsLoadsFromDisk` |
| 速率曲线与内存边界 | `TestHistoryRecordsAndPrunes`、`TestHistoryWindowIsBounded` |
| 完成/失败通知与开关 | `TestNotificationsOnCompletionAndFailure`、`TestNotificationsRespectToggle`、`TestTrackTransitionsIgnoresUnchangedStates` |
| 关闭确认三种结果 | `TestOnBeforeCloseAsksWhenTasksActive`、`TestOnBeforeCloseWithoutHostDialogDoesNotBlock` |
| 文件对话框注入 | `TestServicePickersWithoutHost`、`TestOpenContainingFolderFallsBackToDirectory` |
| AutoResume 关闭后暂停恢复 | `TestAutoResumeDisabledKeepsTasksPaused` |
| 前端语法 | `node --check frontend/app.js` |

### 7.4 未纳入项：系统托盘（诚实记录）

计划中的「系统托盘」**未实现**，原因与替代方案如下：

- Wails v2 没有官方托盘 API。可选方案是 `getlantern/systray`，但它需要在
  自己的 goroutine 上运行独立的消息循环，与 Wails 的主消息循环共存存在稳定性风险
  （托盘图标不出现、窗口无法恢复、偶发崩溃），而这类问题极难自动化验证。
- **替代实现**：关闭窗口时弹出确认框，用户可选择「保留后台下载」。
  由于引擎是独立进程，选择该项后关掉界面下载会继续，下次打开界面可重新接管。
  这覆盖了托盘最核心的使用诉求（「关窗口不想中断下载」）。
- 若后续确有托盘需求，建议在 T14 打包阶段用 Win32 `Shell_NotifyIcon` 自行实现，
  或评估迁移到 Tauri 2（其官方支持托盘）——两者都需要单独排期验证。
