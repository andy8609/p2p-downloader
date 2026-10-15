# 技术调研与选型定稿（T1）

- 调研日期：由本次会话执行
- 调研方式：
  1. GitHub Search API（`topic:downloader language:go stars:>300`、`bittorrent language:go stars:>200`、`aria2 in:name stars:>1000`）
  2. GitHub Repos API（星数 / 许可证 / 活跃度 / 语言）
  3. `goproxy.cn` 模块代理（获取真实可拉取的版本号与 `go.mod` 约束）
  4. 下载并审阅 `github.com/anacrolix/torrent@v1.61.0` 源码包（README、`example_test.go`、`config.go`、`storage/`）
- 网络环境实测：`api.github.com` 可达（200，~0.5s）；`goproxy.cn` 可达（200，~0.16s）；`go.dev`、`raw.githubusercontent.com`、`cdn.jsdelivr.net` **不可达**（超时）。因此依赖一律走 `GOPROXY=https://goproxy.cn,direct`。

---

## 1. 同类项目调研结果（原始数据）

| 项目 | Stars | 许可证 | 语言 | 最后推送 | 可借鉴点 |
|---|---|---|---|---|---|
| aria2/aria2 | 43019 | GPL-2.0 | C++ | 2026-06-25 | 多协议多源、分片模型、`.aria2` 控制文件、RPC 控制面 |
| GopeedLab/gopeed | 26785 | **GPL-3.0** | Dart+Go | 2026-10-10 | Go 内核（HTTP/BT/磁力）+ 跨平台 GUI 的完整形态，架构对标最佳样本 |
| iawia002/lux | 31757 | MIT | Go | 2026-03-29 | 站点解析（非本目标） |
| wailsapp/wails | 36499 | MIT | Go | 2026-10-10 | Go 桌面壳，本文档 §3.3 选型 |
| anacrolix/torrent | 6122 | **MPL-2.0** | Go | 2026-09-27 | 纯 Go BT 协议栈，本文档 §3.2 选型 |
| SurgeDM/Surge | 3590 | MIT | Go | 2026-10-10 | TUI 下载管理器，任务队列/进度展示参考 |
| Code-Hex/pget | 1174 | MIT | Go | 2023-10-19 | 并发分段 HTTP 下载的最小实现参考（已停止维护） |
| uber/kraken | 6760 | Apache-2.0 | Go | 2026-10-09 | 大规模 P2P 分发（服务端场景，不采用） |

**结论性观察**：Go 生态中「HTTP 多段下载」与「BT 协议栈」都有成熟实现，但**没有任何一个 MIT/Apache 许可证的库能同时覆盖两者并直接嵌入**。直接依赖 aria2 或 Gopeed 作为内部引擎不可行：aria2 需外部进程 + RPC 且为 GPL-2.0；Gopeed 为 GPL-3.0，作为库链接会导致本项目整体被迫 GPL-3.0。因此路线确定为 **自研 HTTP 引擎 + 引入 MPL-2.0 的纯 Go BT 库**。

---

## 2. 选型结论一览

| 决策项 | 结论 | 切换成本 |
|---|---|---|
| HTTP 多段引擎 | **自研**（`net/http` + `Range` + `WriteAt`） | 不可切换（核心资产） |
| BitTorrent 引擎 | **`github.com/anacrolix/torrent v1.61.0`** | 中（接口已抽象，见 T8） |
| GUI 壳 | **Wails v2.16.0**（Windows WebView2） | 中（控制面为 HTTP，壳可替换） |
| 持久化 | **JSON sidecar 元数据 + 原子写（tmp+rename）+ 区间位图** | 低（存储接口隔离） |
| 进程模型 | **引擎常驻进程 + 本地 HTTP 控制面 + GUI 壳** | 低 |
| Go 工具链 | **go1.27.2 windows/amd64** | 低 |

---

## 3. 逐项论证

### 3.1 HTTP 多段并发下载引擎 → 自研

- **候选 A：自研（`net/http` + `Range` + `WriteAt`）** ✅ 采用
  - 依赖为零；完全掌控分段策略、限速、断点元数据格式；与「大文件低内存」目标直接可控。
  - 从 pget（MIT，1.1k stars）与 aria2 的分片模型获取设计参考：等分/动态切分分片、每片独立 worker、失败片重试。
- **候选 B：封装 aria2 作为外部引擎进程（`aria2c --enable-rpc`）**
  - 优点：功能最全（BT/Metalink/FTP），久经考验。
  - 否决理由：① GPL-2.0，随我们的安装包分发需一并提供源码与许可，商业化/闭源受限；② 引入外部二进制 + 版本漂移 + Windows 杀软误报；③ 无法实现「统一任务模型」下的细粒度控制（内存上限、段状态）。
  - 切换成本：上层 `Downloader` 接口已抽象（T8），若未来需要 Metalink/FTP，可新增 aria2 引擎实现而不改 GUI。
- **候选 C：直接 import Gopeed 内核**
  - 否决理由：GPL-3.0 传染性最强，作为库链接将强制本项目整体 GPL-3.0；且其内核与 Flutter 协议层耦合较深。

### 3.2 BitTorrent 引擎 → `anacrolix/torrent v1.61.0`

实测事实（来自源码包与 goproxy 元数据）：

- 版本 `v1.61.0`，`go.mod` 声明 **`go 1.24.0`** → 工具链必须 ≥ 1.24（本项目用 1.27.2）。
- 许可证 **MPL-2.0**（文件级 copyleft，可链接进闭源/其他许可证项目，合规成本低）。
- 纯 Go 实现，**不需要 CGO**，提供 DHT / PEX / uTP / 协议加密 / 磁力链 / tracker。
- 关键 API（源码核实）：
  - `torrent.NewClient(cfg *ClientConfig) (*Client, error)`、`client.Close()`、`client.WaitAll()`
  - `client.AddMagnet(magnetURI) (*Torrent, error)` → `<-t.GotInfo()` → `t.DownloadAll()`
  - `torrent.File.NewReader()` 支持随机读，`storage/` 提供 file / bolt / mmap / sqlite 等多种 piece 存储后端
  - `ClientConfig` 关键字段：`DataDir`、`ListenPort`（默认 42069）、`NoDHT`、`DisableTrackers`、`NoUpload`、`Seed`、`DisableUTP`、`DefaultStorage`、`DownloadRateLimiter`/`UploadRateLimiter`（`golang.org/x/time/rate`，**可复用为统一限速实现**）
- **候选 B：libtorrent CGO 绑定（`github.com/ElementumOrg/libtorrent-go` 类）**
  - 否决理由：Windows 需 MSYS2/MinGW 交叉编译 libtorrent + boost，构建链极重，产物不可复现，与「一条命令构建」的验收标准冲突。
  - 切换成本：中。BT 引擎被 `internal/engine/bittorrent` 适配层包裹，仅暴露统一 `Downloader` 接口；替换实现不影响控制面与 GUI。若 anacrolix 出现无法绕过的协议兼容问题，再评估 CGO 路线（预计 2~3 天工作量 + 打包脚本重写）。
- **候选 C：自研 BT 协议栈**
  - 否决理由：DHT + PEX + 加密 + uTP + 各种 BEP 的实现与长期维护成本远超本项目目标，且难以通过公网 swarm 验收。

### 3.3 GUI 壳 → Wails v2.16.0

- 实测：`github.com/wailsapp/wails/v2` 最新 `v2.16.0`，MIT 许可证，36.5k stars，仍活跃维护（2026-10-10 有推送）。
- 优点：与 Go 同语言同工具链、无需引入 Rust/Node 全量工具链、二进制体积远小于 Electron（依赖系统 WebView2）、`wails build` 可产出单个 exe。
- **候选 B：Tauri 2 + Go sidecar**
  - 缺点：需额外安装 Rust 工具链（Windows 上 MSVC + 数 GB 构建缓存），与「干净机器一条命令构建」冲突。
  - 切换成本：中。GUI 仅通过本地控制面（T10）通信，壳替换不触碰引擎；重写 UI 前端（HTML/JS 可复用）+ 打包脚本，预计 1~2 天。
- **候选 C：Electron + Go sidecar**
  - 缺点：安装包 ≥150MB，内存占用高，与「大文件低内存」的气质不符。
  - 切换成本：中（同 B）。
- **WebView2 风险已识别**：Windows 11 与近两年 Windows 10 默认内置；缺失时需引导安装（T14 处理，验收要求「明确提示或自动安装」）。

### 3.4 持久化 → JSON sidecar + 原子写 + 区间位图

- **候选 A：JSON sidecar（每任务一个 `.dltask` 文件 + 全局 `tasks.json` 索引）** ✅ 采用
  - 与 aria2 的 `.aria2` 控制文件同一思路，人可读、可调试、崩溃后可人工修复。
  - 零 CGO、零第三方依赖，二进制体积小。
  - 原子性由 `写临时文件 → fsync → rename` 保证（Windows 上 `os.Rename` 对同卷替换语义可用）。
  - 区间位图用紧凑编码（每片 1 bit 或 run-length），避免大文件元数据膨胀。
- **候选 B：`modernc.org/sqlite v1.60.1`（纯 Go，无 CGO）**
  - 优点：事务强、支持复杂查询。
  - 否决理由：二进制体积增加约 10MB+，对「任务数量在千级以内」的场景收益不明显；调试与人工恢复不如 JSON 直观。
  - 切换成本：低。序列化层被 `internal/store` 接口封装，替换只需重写该包（预计 0.5 天）。
- **候选 C：`mattn/go-sqlite3`**
  - 否决理由：需要 CGO 与 MinGW，破坏「一条命令构建」。

### 3.5 进程模型 → 引擎独立进程 + 本地 HTTP 控制面

- 引擎编译为独立二进制（`dlengine.exe`），GUI（Wails）启动并托管其生命周期；两者通过 `127.0.0.1` 上的 HTTP + 事件推送（SSE/WebSocket）通信。
- 收益：GUI 崩溃/卡顿不中断下载；CLI 可在无 GUI 环境完整验证引擎（T11 验收依赖此设计）；未来可被第三方前端复用。
- 代价：多一层协议与启动时序处理（T12 处理「引擎被手动杀死」的恢复）。

---

## 4. 许可证合规清单

| 依赖 | 许可证 | 合规动作 |
|---|---|---|
| `github.com/anacrolix/torrent` | MPL-2.0 | 保留其 LICENSE 与版权声明；若修改其源文件，需公开被修改文件（本项目不改，仅调用） |
| `github.com/wailsapp/wails/v2` | MIT | 保留版权声明 |
| 其余间接依赖 | 需在 T14 打包前用 `go-licenses` 或 `go list -m -json` 全量核查 | 输出 `dist/licenses.txt` |
| **不引入** aria2（GPL-2.0）/ Gopeed 内核（GPL-3.0） | — | 避免传染，已在 §3.1 论证 |

**本项目自身**：不内置任何资源索引、种子站点、搜索能力，工具中立（法律声明在 T16 落地）。

---

## 5. 环境基线（T2 执行依据）

| 项 | 值 | 来源 |
|---|---|---|
| Go 工具链 | `go1.27.2` windows/amd64，SHA-256 `1314008898bd40df77af4b014f777f08873dbdfbcd3d92308728ee03304fe04f` | `golang.google.cn/dl/?mode=json` |
| 下载地址 | `https://golang.google.cn/dl/go1.27.2.windows-amd64.zip`（官方镜像；`go.dev` 与 `dl.google.com` 在本机不可达） | 实测 |
| 模块代理 | `GOPROXY=https://goproxy.cn,direct`、`GOSUMDB` 保持默认（`sum.golang.org` 若不可达则 `GOFLAGS=-mod=mod GONOSUMDB`/`GONOSUMCHECK` 由 T2 实测决定） | 实测 |
| 安装位置 | **项目本地 `.tools/go`**，不修改系统 PATH、不污染用户环境 | 本机无 Go，且用户未授权系统级安装 |
| 主机 | Windows 10 19041（PowerShell 5.1）、Node v22.23.2、git 2.8.4.windows.1、磁盘剩余 322GB | 实测 |
| 包管理器 | winget 与 choco 可用（本次不使用） | 实测 |

---

## 6. 风险与待验证项（交接给后续任务）

1. **公网 swarm 不可控** → T7 主验收路径为「本地双客户端 + 本地 tracker 互测」，公网磁力链下载作为加分项。
2. **Wails/WebView2 在本机的实际可用性** → T2 完成后立即做一次最小可运行原型（早于 T12），失败则按 §3.3 切换。
3. **anacrolix/torrent 的 uTP（`go-libutp`）是否需 CGO** → T2 拉取依赖后实测构建；若需 CGO 则通过 `DisableUTP` 或 build tag 规避（TCP-only 仍可联通大多数 swarm）。
4. **Windows 大文件预分配性能** → 不整文件预分配，采用分块追加 + 位图（T5）。
5. **`sum.golang.org` 可达性** → T2 实测；不可达时用 `GONOSUMDB`/`GOFLAGS=-mod=mod` 或 `GONOSUMCHECK`（Go 1.27 下为 `GONOSUMDB`/`GOPRIVATE` 组合），并记录在构建脚本注释中。

---

## 7. 结论

四项选型均已定稿，无「待定」项：

1. **HTTP 多段引擎**：自研（`net/http` + `Range` + `WriteAt`），参考 aria2/pget 分片模型。
2. **BitTorrent 引擎**：`github.com/anacrolix/torrent v1.61.0`（MPL-2.0，纯 Go，需 Go ≥1.24）。
3. **GUI 壳**：Wails v2.16.0（MIT，Windows WebView2），已有 Tauri/Electron 两条切换路径与成本评估。
4. **持久化**：JSON sidecar（原子写）+ 区间位图，已评估 pure-Go SQLite 备选与 0.5 天切换成本。
