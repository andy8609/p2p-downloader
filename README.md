# p2pdl

一个 Windows 下载工具：**HTTP 多段并发** + **BitTorrent / 磁力链** + **断点续传** + **GB 级大文件**。

用 Go 编写，自带桌面界面（Wails + WebView2）与命令行客户端，引擎作为独立进程运行。

---

## 核心特性

| 特性 | 说明 |
|---|---|
| **HTTP 多段并发** | 默认 8 段并发（可配 1~64），按「连续缺口」发 Range 请求，不做无谓重传 |
| **BitTorrent / 磁力链** | 纯 Go 协议栈（DHT / PEX / uTP / tracker / piece 校验），支持种子文件与磁力链 |
| **断点续传** | 数据 + 位图双持久化；进程被强杀后重启即续传，实测 4GB 任务零丢失 |
| **大文件友好** | 不做整文件预分配；10GB 下载实测进程工作集峰值 **23.8 MB** |
| **完整性校验** | 流式 SHA-256；配合分块清单可**精确定位损坏块并只重下这些块** |
| **限速与资源控制** | 全局 + 单任务两级令牌桶；缓冲区内存上限；下载前磁盘空间预检查 |
| **GUI / CLI 双前端** | 二者共享同一个引擎与控制面 API，可混用（GUI 里看进度，CLI 里加任务） |
| **崩溃隔离** | 引擎独立进程：界面崩溃或卡顿都不中断下载；引擎被杀界面能立即感知并一键重启 |

---

## 快速开始

### 使用发行包（免安装）

1. 解压 `p2pdl-<版本>-win-x64.zip`
2. 双击 `p2pdl-gui.exe`
3. 粘贴链接或磁力链 → 「开始下载」

详细说明见 **[用户手册](docs/user-guide.md)**。

### 从源码构建

前置条件：**Windows + PowerShell 5.1+**。不需要预装 Go —— 构建脚本会把
Go 工具链下载到项目内的 `.tools/`（不污染系统环境）。

```powershell
# 一条命令构建全部产物到 dist/（首次运行会自动下载 Go 工具链）
.\build.ps1

# 构建并运行全部测试
.\build.ps1 -Test

# 只构建 GUI
.\build.ps1 -Target gui
```

产物：

```
dist\dl.exe            命令行客户端
dist\dlengine.exe      下载引擎
dist\p2pdl-gui.exe     桌面界面
```

### 打包分发

```powershell
.\package.ps1              # 含测试门禁，产出免安装 ZIP + SHA-256 + 安装脚本
```

见 **[打包与分发](docs/packaging.md)**。

---

## 文档索引

| 文档 | 内容 |
|---|---|
| [用户手册](docs/user-guide.md) | 安装、界面、命令行、设置、FAQ |
| [架构说明](docs/architecture.md) | 模块图、依赖方向、关键数据流、设计取舍 |
| [技术调研与选型](docs/tech-survey.md) | 同类项目对比、选型结论、被否决方案与理由 |
| [数据模型与元数据](docs/data-model.md) | 任务/分片模型、状态机、原子写与恢复流程 |
| [落盘与断点恢复](docs/resume-design.md) | 一致性约定、恢复决策表、缺口合并 |
| [性能基准](docs/perf-baseline.md) | 吞吐/内存/限速实测数据与参数依据 |
| [BitTorrent 集成](docs/bittorrent.md) | 端口与存储策略、进度桥接、本地互测 |
| [调度器](docs/scheduler.md) | 队列、并发上限、恢复语义、测试覆盖 |
| [校验与定点修复](docs/verify-design.md) | 分块清单、就地修补、重试上限 |
| [控制面 API](docs/api.md) | 端点契约、事件类型、安全模型 |
| [命令行](docs/cli.md) | 全部命令与退出码 |
| [桌面壳](docs/gui.md) | 进程模型、崩溃处理、前端能力 |
| [打包分发](docs/packaging.md) | 产物、图标与版本信息、WebView2 引导 |
| [端到端验收报告](docs/e2e-report.md) | 五条成功标准的逐条结论与实测数据 |
| [第三方依赖与法律](docs/third-party.md) | 依赖许可、工具中立性声明、隐私说明 |

---

## 项目结构

```
cmd/
  dl/             命令行客户端入口
  dlengine/       引擎进程入口
  p2pdl-gui/      桌面壳入口（含纯静态前端）
internal/
  engine/         任务模型、状态机、调度器、引擎抽象
    httpdl/       HTTP 多段并发下载
    bittorrent/   BitTorrent / 磁力链
    sink/         分段落盘、原子提交、断点恢复
    verify/       完整性校验与定点修复
    limiter/      限速、缓冲池、磁盘空间检查
  store/          任务元数据持久化
  app/            引擎装配层
  control/        本地控制面（HTTP + SSE）
  client/         控制面客户端
  cli/            命令行实现
  gui/            桌面壳业务层
  config/         用户设置
  e2e/            端到端验收测试
build/tools/makeicon/  图标生成器
```

---

## 开发

```powershell
.\build.ps1 -Test                                  # 构建 + vet + 全部测试
go test ./internal/engine/... -bench=. -benchtime=3x   # 基准测试

# 端到端验收（需要 GB 级磁盘空间）
$env:P2PDL_E2E="1"; go test ./internal/e2e -v -timeout 90m
```

约 2 万行 Go 代码 + 27 个测试文件（13 个包）。测试策略：

- **调度器**用假工厂测试，不碰网络与磁盘；
- **HTTP/BT 引擎**用 `httptest` 与本地双客户端互测，完全离线可复现；
- **端到端**用独立引擎进程 + 真实磁盘 I/O，覆盖强杀、GB 级文件与跨进程内存。

---

## 已知限制

| 限制 | 说明 |
|---|---|
| 公网 BT swarm 未验证 | 当前网络出口阻断 BT 流量且 DHT 引导域名被 DNS 污染；本地双客户端互测全通过。详见 [验收报告](docs/e2e-report.md) 标准 4 |
| 无系统托盘 | Wails v2 无官方托盘 API，未引入有稳定性风险的第三方方案；用「关闭确认 + 保留后台下载」覆盖核心诉求。详见 [桌面壳](docs/gui.md) §7.4 |
| 仅 Windows 打包 | 引擎与 CLI 代码是跨平台的，但桌面壳只提供 Windows 产物且未在其他平台验证 |
| 未做代码签名 | 可能触发杀软启发式误报；建议核对 SHA-256 |
| 界面渲染无自动化测试 | 自动化覆盖到数据层与进程链路；像素级渲染需人工验收（步骤见验收报告） |

---

## 法律声明

本工具**只实现通用的 HTTP/HTTPS 与 BitTorrent 协议**，用于下载你有权获取的内容。

- **不包含**任何资源索引、种子站点、搜索功能或内置链接；
- **不包含**任何绕过访问控制、DRM 或付费墙的机制；
- 默认**不做种**，默认关闭 UPnP 端口映射；
- 下载内容的合法性完全取决于用户与内容提供方之间的法律关系。

请遵守所在地区的法律法规与著作权要求。完整声明见 [第三方依赖与法律](docs/third-party.md)。

---

## 许可

Apache-2.0，见 [LICENSE](LICENSE)。第三方组件许可见 [docs/third-party.md](docs/third-party.md)。
