# 第三方依赖与许可（T16）

本项目自身以 **MIT** 许可发布（见根目录 `LICENSE`）。以下是运行时会用到的第三方组件。

---

## 1. 直接依赖（编译进二进制的库）

| 模块 | 版本 | 许可 | 用途 | 合规动作 |
|---|---|---|---|---|
| `github.com/anacrolix/torrent` | v1.61.0 | **MPL-2.0** | BitTorrent 协议栈（DHT/PEX/uTP/tracker/piece 校验） | 保留其 LICENSE 与版权声明；**未修改其源码**，仅作为库调用，因此 MPL 的文件级 copyleft 不触发源码公开义务 |
| `github.com/wailsapp/wails/v2` | v2.16.0 | MIT | 桌面壳（WebView2 宿主、Go↔JS 绑定） | 保留版权声明 |
| `golang.org/x/time` | v0.16.0 | BSD-3-Clause | 令牌桶限速（`rate.Limiter`），HTTP 与 BT 两侧共用 | 保留版权声明与免责声明 |

**为什么没有引入 aria2 / Gopeed**：两者分别是 GPL-2.0 与 GPL-3.0。
把 GPL 代码链接进本项目会使整个项目被迫以 GPL 发布（详见 `docs/tech-survey.md` §3.1）。
`anacrolix/torrent` 的 MPL-2.0 是文件级 copyleft，可以在保持本项目 MIT 的前提下使用。

---

## 2. 间接依赖

`go.mod` 中标记为 `// indirect` 的模块（约 100 个）是上述三个直接依赖的传递依赖，
其中体量较大的几类：

| 类别 | 代表模块 | 许可 |
|---|---|---|
| BT 生态（anacrolix 系列） | `anacrolix/dht/v2`、`anacrolix/go-libutp`、`anacrolix/btree` | MPL-2.0 / MIT |
| Wails 运行时 | `wailsapp/go-webview2`、`wailsapp/mimetype`、`jchv/go-winloader` | MIT |
| WebRTC（Wails WebView2 桥） | `pion/*` | MIT |
| 纯 Go SQLite（BT 的 piece completion 后端之一） | `modernc.org/sqlite`、`modernc.org/libc` | BSD-3-Clause |
| Go 官方扩展库 | `golang.org/x/{crypto,net,sys,text,sync,exp}` | BSD-3-Clause |
| 存储/序列化 | `go.etcd.io/bbolt`、`google/btree`、`tidwall/btree` | MIT |

完整清单可用以下命令生成（需要网络）：

```powershell
go list -m all
# 若需逐模块的许可证文本（会下载 go-licenses 工具）：
go install github.com/google/go-licenses@latest
go-licenses report ./... --ignore p2pdl
```

`package.ps1` 会把上述「直接依赖」的清单写入发行包内的 `LICENSE.txt`，
并在 `README.txt` 中指明完整清单的位置。

---

## 3. 运行时外部组件（非 Go 依赖，随系统提供）

| 组件 | 说明 | 许可/来源 |
|---|---|---|
| Microsoft Edge WebView2 Runtime | 界面渲染引擎。Windows 11 与较新的 Windows 10 已内置；缺失时程序会提示并引导到微软官方下载页 | 微软专有，免费随应用分发 |
| KB 更新与系统 DLL | `kernel32`/`user32`/`advapi32`/`psapi` 等系统组件（用于隐藏子进程窗口、WebView2 检测、内存采样） | 操作系统自带 |

**没有任何运行时组件需要用户额外安装**（除了 WebView2，而它在绝大多数目标机器上已存在）。

---

## 4. 法律与合规声明

### 4.1 工具中立性

本工具**只实现通用的 HTTP/HTTPS 与 BitTorrent 协议**，具体包括：

- HTTP Range 请求驱动的多段并发下载；
- BitTorrent 协议客户端的下载侧行为（含磁力链、DHT、tracker announce）。

它**不包含**：

- ❌ 任何资源索引、种子站点、搜索功能或内置链接；
- ❌ 任何绕过访问控制、DRM 或付费墙的机制；
- ❌ 任何规避著作权保护或匿名化上传者身份的功能。

**用户责任**：下载内容是否合法完全取决于用户与内容提供方之间的法律关系。
请遵守所在地区的法律法规与著作权要求，仅用于下载你有权获取的内容。

### 4.2 关于 BitTorrent 的说明

BitTorrent 是广泛用于合法分发的协议（Linux 发行版镜像、开源数据集、企业内部大文件分发等）。
本项目默认**不做种**（`Seed=false` 为默认值），并默认**关闭 UPnP 端口映射**——
桌面工具不应在未告知用户的情况下改动路由器配置。

### 4.3 隐私

- 控制面仅监听 `127.0.0.1`，并用随机令牌保护（`docs/api.md` §1）；
- 下载器不向任何第三方服务器上报用户数据；
- 唯一的外部网络行为是用户主动添加的下载源，以及 BitTorrent 协议本身的对等方/tracker 通信（可通过设置关闭 DHT、tracker、PEX）。

### 4.4 免责

软件按「原样」提供，不附带任何明示或暗示的担保。作者不对因使用本软件产生的
任何数据丢失、带宽费用或法律后果承担责任。详见根目录 `LICENSE`。
