# 端到端验收报告（T15）

对应 T1（GOAL 阶段）定义的五条成功标准，逐条给出「是/否」结论与可复现证据。

- 执行时间：本次交付会话
- 环境：Windows 10 19041（x64）、go1.27.2、D 盘可用 321.8 GB、WebView2 `153.0.4234.48`
- 全量回归：`build.ps1 -Test` → **13 个包全部 PASS**

复现命令：

```powershell
# 常规回归（含全部单元与集成测试）
.\build.ps1 -Test

# 端到端验收（需要 GB 级磁盘空间与较长时间）
$env:P2PDL_E2E="1"
go test ./internal/e2e -v -timeout 90m

# 公网 swarm（依赖外网，可能失败）
$env:P2PDL_PUBLIC_BT_TEST="1"
go test ./internal/engine/bittorrent -run TestPublicSwarm -v -timeout 6m
```

---

## 结论总览

| # | 成功标准 | 结论 | 关键证据 |
|---|---|---|---|
| 1 | 4GB 断点续传：强杀进程后续接而非重下，SHA-256 一致 | ✅ **通过** | 强杀时 1.22 GB → 重启后 1.25 GB；服务端累计发出 4.04 GB / 总长 4.00 GB；SHA-256 一致 |
| 2 | 10GB 大文件：全程完成、峰值内存 <500MB、无数据错位 | ✅ **通过** | 耗时 2m36s；**进程工作集峰值 23.8 MB**；10GB 全量 SHA-256 一致 |
| 3 | 分段下载：并发段数可配（≥8）且实测生效 | ✅ **通过** | 服务端观测并发峰值 = 8，模式 = `multi-segment`，内容校验一致 |
| 4 | BitTorrent：磁力链可从公网 swarm 完成下载并校验 | ❌ **未通过（环境限制）** | peer 峰值 0；本地双客户端互测全通过 |
| 5 | GUI：Windows 双击可用、粘贴 URL/磁力链看进度、一条命令构建 | ⚠️ **部分通过** | 启动链路与数据链路已自动化验证；**窗口视觉部分需人工确认** |

---

## 标准 1：4GB 断点续传（强杀进程）

**测试**：`internal/e2e.TestE2E_FourGigabyteResumeAfterHardKill`

**方法**：确定性数据流服务器（支持 Range）+ **真实独立引擎进程**；
下载到约 30% 时用 `Process.Kill()` 强杀（不触发任何优雅退出路径）；
同目录重启引擎，验证进度保留并完成；最后流式校验 4GB 的 SHA-256。

**实测输出**：

```
强杀前: 已下载 1.22 GB (30.6%)，服务端累计发出 1.25 GB
已强杀引擎进程（未做优雅关闭）
重启后任务状态=running，已下载 1.25 GB
强杀丢失 -23 MB（占已验证进度 -1.8%），断点保留率 101.8%
下载完成：4.00 GB，服务端累计发出 4.04 GB
文件哈希计算完成，耗时 7s
✅ 成功标准 1 通过：强杀后断点续传完成，SHA-256 = 6ce6d673a0efaf08…
```

**判读**：

- 服务端累计只发出 **4.04 GB**（总长 4.00 GB，多出的 1% 是重连与探测开销），
  说明**没有发生重下**；若重下会接近 5.2 GB（1.25 + 4.00）。
- 重启后已下载量（1.25 GB）**高于**强杀瞬间采样到的值（1.22 GB），
  因为元数据落盘比内存快照更晚——即「强杀时最多丢失最后一次未落盘的块」。

### 测试过程中发现并修复的真实缺陷

**问题**：首轮测试强杀后进度从 1.25 GB 退回 0.98 GB（**丢失 21%**，约 270 MB）。
根因是断点进度**只按时间保存**（每 1 秒一次），而本地回环下载速度达数百 MB/s，
1 秒就是数百 MB——在网络更快的用户机器上会更严重。

**修复**（`internal/engine/httpdl`）：新增**按数据量兜底保存** `saveByteThreshold = 32MB`，
`markChunk` 累计未落盘字节，攒够阈值立即调用 `saveProgress`（并保持每秒一次的时间兜底）。
修复后丢失量降到 **0（<32MB 阈值内）**，且与带宽无关。

---

## 标准 2：10GB 大文件下载

**测试**：`internal/e2e.TestE2E_TenGigabyteLargeFile`

**方法**：10GB 确定性数据流；独立引擎进程；每 300 ms 通过
`GetProcessMemoryInfo` 采样**引擎进程的工作集**（操作系统视角的真实占用，
比同进程内读 `runtime.MemStats` 更有说服力）；完成后对 10GB 做全量 SHA-256。

**实测输出**：

```
10GB 完成：耗时 2m36s（平均 66 MB/s），模式=multi-segment，服务端发出 10.00 GB
引擎进程工作集峰值 23.8 MB，采样 518 次
计算 10GB 文件 SHA-256（流式，内存占用固定）…
哈希完成，耗时 19s
✅ 成功标准 2 通过：10GB 完整下载，工作集峰值 23.8 MB，SHA-256 = 4014c7f74cf20790…
```

**判读**：

- **工作集峰值 23.8 MB**，相对 500 MB 上限有 21 倍余量。内存占用由
  「并发数 × 缓冲区」（8 × 32KB = 256KB）+ 位图元数据（10GB / 1MB = 1280 字节）+ HTTP/BT 运行时组成，
  **与文件大小无关**。
- 服务端发出 10.00 GB（= 总长），无重下、无重复区间。
- 10GB 全量 SHA-256 一致 ⇒ 既无空洞（否则有零填充区）也无重叠（否则字节错位）。
- 平均 66 MB/s 低于 T6 单进程基准（1011 MB/s），差异来自：跨进程 IPC、
  服务端逐块生成数据（`fillAt`）、以及真实磁盘写入与 fsync——这是更接近真实场景的数字。

### 交叉验证

`internal/engine/httpdl.TestLargeFileMemoryFootprint`（同进程内测量，T6 引入）：
耗时 10.124 s、Go 堆峰值 **2.86 MB**、进程 Sys 18.48 MB。

---

## 标准 3：多段并发

**测试**：`internal/e2e.TestE2E_ConcurrentSegmentsAreActuallyConcurrent`

**方法**：512MB 文件；在测试服务器中统计**并发请求数峰值**（服务端视角，不依赖客户端自述）。

```
任务完成：模式=multi-segment，服务端并发峰值=8
✅ 成功标准 3 通过：8 段并发，内容校验一致
```

**判读**：默认并发数 8（`DefaultConcurrency`），服务端实际观测到 8 个并发请求，
说明分段调度真实生效。用户在设置中可调整（1~64）。

### 测试过程中发现并修复的缺陷

**问题**：任务完成后 `TaskView.Mode` 为空——`Scheduler.Get/List` 用会话的实时模式
**覆盖**了任务上的字段，而会话在完成后已销毁，导致「下载完了却看不出用的哪种模式」。

**修复**：`Task` 新增持久化的 `Mode` 字段（会话期间实时更新，完成后保留），
`Get/List` 只在会话返回非空模式时才覆盖。

---

## 标准 4：BitTorrent 公网 swarm —— ❌ 未通过

**测试**：`internal/engine/bittorrent.TestPublicSwarmReachesRealPeers`（Ubuntu 24.04 官方种子）

```
目标文件：ubuntu-24.04.3-desktop-amd64.iso，大小 5.91 GB，piece 24208 个
公网 swarm 验证未通过：peer 峰值 0，已接收 0.00 B
```

**根因（环境限制，非实现缺陷）**：

| 现象 | 证据 |
|---|---|
| DHT 引导域名被 DNS 污染 | `router.bittorrent.com` 解析到 `31.13.84.34`（与 BT 引导无关的地址） |
| tracker 可达但 swarm 不可达 | `https://torrent.ubuntu.com/` 返回 200，announce 回来的 peer 无一能建立 TCP 连接 |
| 结论 | 当前网络出口阻断 BT 流量 |

**已覆盖的部分**（`internal/engine/bittorrent`，7 个用例全通过，完全离线）：

| 用例 | 覆盖环节 |
|---|---|
| `TestLocalSeederAndLeecherViaTorrentFile` | peer 握手、piece 请求与传输、piece 哈希校验 → **SHA-256 与源一致** |
| `TestLocalMagnetRoundTrip` | 磁力链 + BEP 9 元数据交换 |
| `TestPauseAndRestartKeepsProgress` | 暂停 → 关闭 → 同目录重启 → **恢复 16/64 piece 而非重下** |
| `TestSnapshotFieldsAndBitmap` / `TestApplyToTaskProducesValidTask` | 进度桥接与位图正确性 |

**未覆盖的环节**：公网 peer 发现（tracker announce 结果的使用、DHT 爬取）。
这部分由 `anacrolix/torrent` 实现，本包只透传参数。

**建议的后续动作**（如有条件）：

1. 在不受限网络出口复跑 `TestPublicSwarmReachesRealPeers`；
2. 或使用内网 tracker（BEP 3 announce）搭建跨机 swarm 验证 peer 发现链路；
3. 若需在受限网络下可用，需为 DHT 引导补充可信的节点列表（配置项），当前未实现。

---

## 标准 5：GUI —— ⚠️ 部分通过

### 已自动化验证（结构性链路）

| 项 | 证据 |
|---|---|
| 双击运行 | 干净 PATH（仅 `system32`+`Windows`，无 Go/Node）下 `p2pdl-gui.exe` 启动成功 |
| 自动拉起引擎 | 日志：`gui: 已启动引擎进程 pid=17792 (...\p2pdl-engine.exe)` |
| WebView2 | 启动首行：`WebView2 Runtime 可用：153.0.4234.48`；`[WebView2] Environment created successfully` |
| 完成一次下载 | 包内 `dl.exe` 通过同一引擎下载：`完成: cleaned.bin（768.00 KB）`，**SHA-256 一致** |
| 引擎被杀后提示 | `TestEngineManagerDetectsEngineCrash`：`引擎进程已退出: exit status 1` |
| 一键重启引擎 | `TestEngineManagerRestartsAfterCrash`：新 PID 且控制面恢复可用 |
| GUI 卡顿不影响下载 | 结构性保证（引擎是独立进程）+ `TestServiceGetStateIsFast`（20 次 `GetState` < 2 s） |
| 设置项生效 | 设置文件写入 `maxConcurrentTasks: 2`、`globalRateLimit: 2097152` 后，CLI `status` 显示「并发上限: 2 / 全局限速: 2.00 MB/s」 |
| 前端语法 | `node --check frontend/app.js` 通过 |

### 需人工确认（无法在无人值守环境断言）

以下为**渲染与交互的视觉结果**，自动化只能验证到数据层：

1. 窗口是否正常显示、任务行的进度条与速度曲线是否随下载推进；
2. 拖拽 `.torrent` 到窗口是否触发添加（逻辑已实现，`dragenter/drop` 事件链无法在无头环境模拟）；
3. 「读剪贴板」按钮与自动识别是否按预期填入链接；
4. 关闭窗口且有任务在跑时，确认框的三个选项是否按预期工作。

**建议的人工验收步骤**（3 分钟）：

```
1. 解压 dist\p2pdl-1.0.0-win-x64.zip，双击 p2pdl-gui.exe
2. 粘贴一个 HTTP 大文件链接（或拖入一个 .torrent），点「开始下载」
3. 观察：进度条推进、速度与剩余时间更新、速度曲线出现
4. 在任务管理器结束 p2pdl-engine.exe → 界面应变红并显示原因 → 点「重启引擎」→ 恢复
5. 关闭窗口 → 确认框选择「保留后台下载」→ 用 dl.exe list 验证任务仍在推进
```

---

## 附：本次验收过程中修复的问题汇总

| # | 问题 | 影响 | 修复 |
|---|---|---|---|
| 1 | 断点进度只按时间（1 秒）保存 | 高速下载下强杀丢失数百 MB（实测 270 MB / 21%） | 新增按数据量兜底（32MB）触发保存 |
| 2 | `TaskView.Mode` 被会话实时值覆盖 | 任务完成后无法展示所用模式 | `Task.Mode` 持久化，仅在会话有值时覆盖 |
| 3 | E2E 测试自身：日志句柄未关闭 | `t.TempDir` 清理失败导致测试误报 | cleanup 中显式关闭日志文件 |
| 4 | E2E 测试自身：`fsutil` 取磁盘空间失败 | 空间检查退化 | 复用 `limiter.FreeSpace` |

> 问题 1 与 2 是**产品缺陷**（会被用户直接感知），由端到端验收暴露出来——
> 这正是「单元测试全绿但仍需 E2E」的价值所在。
