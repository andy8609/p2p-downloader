# 命令行客户端（T11）

实现：`cmd/dl`（薄壳）+ `internal/cli`（命令实现）+ `internal/client`（控制面客户端）。

CLI 不直接链接下载引擎，而是通过本地控制面（`docs/api.md`）通信——
因此引擎可以独立于 CLI 长期运行，CLI 崩溃或重启不影响下载。

---

## 1. 快速开始

```bash
# 1) 启动引擎（前台运行，Ctrl+C 退出）
dl serve --data-dir D:\downloads --limit 5MB

# 2) 另开一个终端：新建任务并实时看进度
dl add "https://releases.ubuntu.com/24.04/ubuntu-24.04.3-desktop-amd64.iso" --name ubuntu.iso

# 3) 查看与管理
dl list
dl pause <任务ID>
dl resume <任务ID>
dl rm <任务ID> --files
```

`dl serve` 启动后会把「监听地址 + 令牌文件路径」写入 `<meta-dir>/control.endpoint`，
其余命令据此自动发现引擎，**无需手工传端口或令牌**。

---

## 2. 命令一览

| 命令 | 说明 | 关键参数 |
|---|---|---|
| `serve` | 前台运行下载引擎与控制面 | `--port`、`--max-tasks`、`--concurrency`、`--limit`、`--manifest` |
| `add <URL\|磁力链>` | 新建任务并（默认）等待完成 | `--torrent`、`--name`、`--dest`、`--hash`、`--paused`、`--watch`、`--timeout`、`--peers`、`--header` |
| `list` | 列出全部任务 | `--json` |
| `watch [任务ID]` | 实时打印进度（缺省监视全部） | `--timeout` |
| `pause` / `resume` | 暂停 / 恢复 | — |
| `recheck` | 重新校验（发现损坏时只重下受损块） | — |
| `rm` | 删除任务 | `--files` |
| `status` | 引擎状态与统计 | `--json` |
| `limit <速率>` | 修改全局限速 | `1MB` / `500k` / `0`（不限） |
| `version` / `help` | 版本 / 帮助 | — |

全局选项：`--data-dir`、`--meta-dir`、`--addr`、`--token`、`--json`。

位置参数与选项顺序任意：`dl add <url> --name x` 与 `dl add --name x <url>` 等价
（实现见 `internal/cli/reorderArgs`——标准库 `flag` 在遇到位置参数后会停止解析，
因此解析前会把位置参数移到末尾）。

---

## 3. 退出码

| 码 | 含义 | 示例 |
|---|---|---|
| 0 | 成功 | 下载完成、列表获取成功、暂停成功 |
| 1 | 运行期错误 | 引擎未运行、任务失败、状态机拒绝（暂停已完成任务） |
| 2 | 用法错误 | 未知命令、缺少参数、非法速率表达式 |

```bash
dl add "$URL" || echo "下载失败，退出码 $?"
```

---

## 4. 常用场景

### 带完整性校验下载

```bash
SHA=$(curl -sL https://example.com/big.iso.sha256 | awk '{print $1}')
dl add https://example.com/big.iso --hash "$SHA"
```

若之后文件损坏：

```bash
dl recheck <任务ID>   # 用分块清单定位受损块，只重下这几块
```

### 磁力链 / 种子文件

```bash
dl add "magnet:?xt=urn:btih:..." --timeout 30m
dl add --torrent ./release.torrent --name release
# 局域网直连（无 DHT/tracker）
dl add --torrent ./a.torrent --peers 10.0.0.5:6881,10.0.0.6:6881
```

### 后台创建 + 单独监视

```bash
dl add "$URL" --watch=false     # 立即返回，任务在引擎里继续
dl watch <任务ID>               # 需要时再看进度
dl watch                        # 一屏概览所有任务
```

### 脚本友好输出

```bash
dl list --json | jq -r '.[] | select(.state=="completed") | .destPath'
dl status --json | jq '.tasks'
```

### 限速

```bash
dl limit 2MB      # 全局限速 2MB/s
dl limit 0        # 关闭限速
```

---

## 5. 输出示例

```
$ dl list
ID                           类型         状态           进度         速度  名称
20261012T143005-a1b2c3d4e5f6 http         completed    100.0%      0.00 B/s  ubuntu.iso
20261012T143101-b2c3d4e5f6a7 bittorrent  running       43.7%    3.21 MB/s  debian.iso

$ dl status
控制面: http://127.0.0.1:52341
引擎版本: 1.0.0
任务: 共 2（运行 1 / 等待 0 / 暂停 0 / 完成 1 / 失败 0）
并发上限: 3
下载目录: D:\downloads
全局限速: 2.00 MB/s
```

进度行使用 `\r` 原地刷新（`dl add` / `dl watch <id>`），在管道中重定向时建议配 `--json`。

---

## 6. 测试覆盖

`internal/cli/run_test.go` 在同一进程内启动**真实引擎**（等价于 `dl serve`）并驱动全部命令，
因此这些用例就是「不启动 GUI 完成下载」的端到端验证：

| 验收项 | 测试 |
|---|---|
| **CLI 完成一次 HTTP 下载** | `TestCLIHTTPDownloadEndToEnd`（校验 SHA-256 与源一致） |
| **CLI 完成一次 BT 下载** | `TestCLIBitTorrentDownloadEndToEnd`（本地种子端 + 静态 peer） |
| 帮助完整 | `TestHelpIsCompleteAndExitsZero`（12 个命令 + 3 个选项 + 退出码说明） |
| 退出码语义 | `TestNoArgsExitsWithUsageCode`(2)、`TestUnknownCommandExitsWithUsageCode`(2)、`TestCommandsFailWhenEngineNotRunning`(1)、`TestCLIAddFailsOnBadURL`(1) |
| 任务管理闭环 | `TestCLIPauseResumeRemove`（暂停 → 恢复 → 暂停 → 重新校验 → 删除含文件 → 重复删除报错） |
| 实时进度 | `TestCLIWatchReachesCompletion` |
| 状态与限速 | `TestCLIStatusAndLimit` |
| 参数顺序兼容 | 全部用例都以 `add <url> --name x` 形式调用 |
