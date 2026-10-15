# 架构说明（T16）

本文描述代码的实际分层、依赖方向与关键数据流。**与仓库目录一一对应**，
可在 `internal/` 下逐层核对。

---

## 1. 模块图

```
                      ┌─────────────────┐   ┌──────────────────┐
                      │ p2pdl-gui.exe   │   │ dl.exe           │
                      │ (Wails 桌面壳)   │   │ (命令行客户端)     │
                      └────────┬────────┘   └────────┬─────────┘
                               │  internal/gui                │ internal/cli
                               │  internal/client             │
                               └──────────┬───────────────────┘
                                          │  HTTP + SSE（仅 127.0.0.1，Bearer Token）
                                          ▼
                      ┌──────────────────────────────────────┐
                      │ p2pdl-engine.exe                     │
                      │   internal/control  本地控制面        │
                      │   internal/app      引擎装配层        │
                      │   internal/engine   调度器 + 抽象      │
                      └───────────────┬──────────────────────┘
                                      │
        ┌──────────────┬──────────────┼──────────────┬──────────────┐
        ▼              ▼              ▼              ▼              ▼
 internal/engine  internal/engine  internal/engine  internal/engine  internal/store
   /httpdl          /bittorrent      /sink           /verify          (元数据)
 HTTP 多段并发      BT/磁力链       分段落盘/恢复     完整性校验/定点修复
        │              │              │              │
        └──────────────┴──────────────┴──────────────┘
                              │
                    internal/engine/limiter
                 限速 · 缓冲池 · 磁盘空间预检查
```

依赖方向严格单向：**上层依赖下层，下层不感知上层**。
唯一的例外是 `internal/app`——它的职责正是「把下层实现按任务类型装配给上层的接口」。

---

## 2. 各包职责与文件对应

| 包 | 文件 | 职责 |
|---|---|---|
| `internal/engine` | `model.go` `bitmap.go` `state.go` | 任务/分片模型、区间位图、状态机 |
| | `downloader.go` | `Session`/`SessionFactory`/`Verifier`/`TaskView` 等抽象契约 |
| | `scheduler.go` | 任务队列、并发上限、生命周期推进、进度持久化 |
| `internal/engine/httpdl` | `probe.go` | HEAD / `Range: bytes=0-0` 探测（大小、是否支持分段） |
| | `downloader.go` | 多段并发下载、按连续缺口续传、单流降级、重试退避 |
| | `speed.go` | 滑动窗口速率估算 |
| `internal/engine/sink` | `file.go` | 临时文件写入、原子提交、**就地修补**模式 |
| | `resume.go` | 恢复决策（`resume` / `restart`）、缺口合并 |
| | `saver.go` | 进度落盘顺序保证（先 fsync 数据，再原子写元数据） |
| `internal/engine/verify` | `hash.go` | 流式 SHA-256/MD5（内存与文件大小无关） |
| | `manifest.go` | 分块哈希清单：生成、校验、**精确定位损坏块** |
| | `verifier.go` | 校验→定位→标记重下的决策与次数上限 |
| `internal/engine/bittorrent` | `engine.go` | BT 客户端生命周期、存储句柄管理、任务加入 |
| | `torrent.go` | 进度桥接（piece 位图 → 统一模型）、暂停/恢复、速率差分 |
| `internal/engine/limiter` | `limiter.go` | 令牌桶限速（全局 + 单任务串联） |
| | `pool.go` | 固定大小缓冲区复用 |
| | `diskspace*.go` | 跨平台磁盘可用空间查询 |
| `internal/store` | `file.go` | JSON sidecar 元数据 + 索引 + 损坏隔离 |
| | `atomic.go` | `临时文件 + fsync + rename` 原子写 |
| `internal/app` | `app.go` | 应用内核：装配调度器、BT 引擎、限速器；任务增删改查 |
| | `session.go` | `SessionFactory` 实现：HTTP 会话与 BT 会话 |
| `internal/control` | `server.go` | HTTP 路由、Bearer 认证、SSE 事件推送 |
| | `handlers.go` | 各端点实现与请求校验 |
| `internal/client` | `client.go` | 控制面客户端 + 端点自动发现 |
| `internal/cli` | `run.go` | 12 个命令的实现（可测试的 `Run` 函数） |
| `internal/gui` | `engine.go` | 引擎进程生命周期（发现/启动/监控/重启） |
| | `service.go` | 暴露给前端的 API、状态汇总 |
| | `settings.go` | 设置读写、速率历史、通知队列 |
| | `platform_*.go` | WebView2 检测与引导、隐藏子进程窗口 |
| `internal/config` | `config.go` | 用户设置的持久化与校验 |
| `cmd/*` | `main.go` | 三个入口的薄壳（GUI / 引擎 / CLI） |

---

## 3. 关键数据流

### 3.1 HTTP 多段下载

```
用户添加 URL
   │
   ├─▶ httpdl.Probe          HEAD → Range 探测 → 总长度 + 是否支持分段
   │
   ├─▶ engine.NewTask       按分片数等分文件，每片一个位图（粒度为 ChunkSize）
   │
   └─▶ Scheduler             入队 → 并发上限内启动会话
          │
          └─▶ httpSession
                 ├─ sink.Open         复用临时文件（断点续传的落地点）
                 ├─ PlanRecovery      位图与文件不一致时清空重下（宁可重下，不可续错）
                 └─ httpdl.Run
                       ├─ 每分片一个 worker，按「连续缺口」发 Range 请求
                       ├─ WriteAt 落盘 → markChunk 置位
                       ├─ 每 32MB 或每 1 秒：ProgressSaver.Save
                       │     └─ 先 fsync 数据，再原子写元数据（顺序不可颠倒）
                       └─ 完成后 sink.Commit：Truncate → Sync → Rename
```

### 3.2 BitTorrent

```
用户添加磁力链 / 种子
   │
   ├─▶ bittorrent.Engine.Add     TorrentSpecFromMagnetUri → AddTorrentSpec
   ├─▶ handle.WaitInfo()         等待元数据（BEP 9）
   ├─▶ handle.ApplyToTask()      piece 位图 → Task.PieceBitmap
   │
   └─▶ Scheduler → btSession → Torrent.Download
          ├─ 库负责 peer 连接、chunk 请求编排、piece 哈希校验
          ├─ 每 500ms：Sync → ApplyToTask（进度写回统一模型）
          └─ 暂停：DisallowDataDownload（保留连接与已完成 piece）
```

BT 与 HTTP 的差异被收敛在 `Session` 接口内：`Progress()` 返回同一套字段，
`Sync()` 的语义都是「把引擎内部进度写回 Task」。

### 3.3 完整性校验与定点重下

```
数据收齐 → Scheduler.finish
   │
   ├─ 置为 verifying，调用 Verifier.Verify
   │
   ├─ 有分块清单 → Manifest.VerifyFile 逐块比对
   │        └─ 命中坏块 → Task.MarkChunksMissing（只清除这些块的位图位）
   │              └─ 返回 Repaired → 任务回到 pending → 只重下受损区间
   │
   ├─ 只有整体摘要 → 摘要不符且无法定位 ⇒ 如实失败（不掩盖问题）
   │
   └─ 通过 → completed（成功时把修复计数归零）
```

修复次数上限 `DefaultMaxRepairAttempts = 3`，防止磁盘持续损坏时无限重下。

### 3.4 进度持久化的一致性约定

```
写数据(WriteAt) → 位图置位(内存) → fsync 数据文件 → 元数据原子落盘
```

`internal/engine/sink/saver.go` 强制了这个顺序。它保证：
**元数据里声称已完成的块，其数据一定已经到达磁盘**；
任意时刻被强杀，最坏情况只是「数据已落盘但位图未记录」，重启后重下该块。

**保存时机** = 每 32MB 数据量（兜底）或每 1 秒（时间兜底），取先到者。

---

## 4. 进程模型与故障隔离

| 进程 | 崩溃影响 |
|---|---|
| `p2pdl-gui.exe` | 界面消失，**下载继续**（引擎是独立进程）；重启 GUI 通过端点文件重新接管 |
| `p2pdl-engine.exe` | 下载暂停；GUI 立即感知并显示原因，可一键重启；重启后从断点续传 |
| `dl.exe` | 无影响（一次性客户端） |

控制面仅绑定 `127.0.0.1` 并用随机令牌保护：它具备删除文件与修改配置的能力，
不能暴露到局域网（`docs/api.md` §1 有完整的安全模型说明）。

---

## 5. 状态机

```
pending ──▶ running ──┬─▶ verifying ──▶ completed
   ▲   ▲              ├─▶ completed            │
   │   │              ├─▶ paused ──▶ running   │
   │   └──────────────┴─▶ failed ──▶ pending ◀─┘（用户显式重新下载）
   └── 重新排队
```

终态不可被后台流程改写；`verifying → pending` 是「校验发现损坏需重下」的专用路径。
详见 `docs/scheduler.md` §3。

---

## 6. 设计中的几个取舍（为什么是这样）

| 取舍 | 选择 | 理由 |
|---|---|---|
| BT 写入方式 | 临时文件 + 原子重命名 | 避免出现「看起来完整、实际有空洞」的文件 |
| 是否整文件预分配 | **不预分配** | 10GB 文件在 0.1% 进度时不占用满磁盘；空洞由位图表达 |
| 「已下载」如何存储 | **只由位图推导**，不存冗余字段 | 消除「两份进度互相矛盾」的可能 |
| 校验失败且无法定位 | 如实失败 | 掩盖问题会污染用户数据 |
| 就地修补（in-place） | 支持 | 定点重下时若仍走「临时文件 + 重命名」，会覆盖掉未被重下的字节 |
| 分段请求范围 | 只请求**连续缺口** | 否则「重下 64KB」会放大成「重下整个分片」 |
| GUI 与引擎通信 | 轮询而非事件推送 | 状态量小；让「引擎崩溃/重连」的异常路径无需额外状态机 |
| 桌面壳 | Wails + 纯静态前端 | 不引入 Node/npm 工具链，保证「一条命令构建」 |

---

## 7. 相关文档索引

| 主题 | 文档 |
|---|---|
| 技术选型与调研（含被否决方案） | `docs/tech-survey.md` |
| 任务模型与元数据格式 | `docs/data-model.md` |
| 落盘与断点恢复设计 | `docs/resume-design.md` |
| 性能基准与参数依据 | `docs/perf-baseline.md` |
| BitTorrent 集成细节 | `docs/bittorrent.md` |
| 调度器与状态机 | `docs/scheduler.md` |
| 校验与定点修复 | `docs/verify-design.md` |
| 控制面 API 契约 | `docs/api.md` |
| CLI 使用 | `docs/cli.md` |
| 桌面壳 | `docs/gui.md` |
| 打包与分发 | `docs/packaging.md` |
| 端到端验收报告 | `docs/e2e-report.md` |
| 第三方依赖与法律声明 | `docs/third-party.md` |
