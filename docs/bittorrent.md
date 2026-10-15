# BitTorrent 引擎集成（T7）

实现：`internal/engine/bittorrent/`（`engine.go` 客户端与任务管理、`torrent.go` 进度桥接与生命周期）。
底层库：`github.com/anacrolix/torrent v1.61.0`（MPL-2.0，纯 Go，无需 CGO）。

---

## 1. 为什么用库而不是自研协议栈

T1 已论证：DHT、PEX、uTP、协议加密与各类 BEP 的实现与长期维护成本远超本项目目标。
本包的职责边界是**把 BT 引擎接入统一任务模型**，而不是重新实现 BT 调度：

| 由库负责 | 由本包负责 |
|---|---|
| peer 连接、chunk 请求编排、piece 校验 | 客户端生命周期、端口与存储策略 |
| DHT / tracker / PEX 的 peer 发现 | 静态 peer 注入（本地互测、局域网直连） |
| `piece completion` 持久化 | 把 piece 状态桥接为 `engine.Bitmap` |
| 上传/做种 | 暂停意图的维护（库未提供查询 API） |

---

## 2. 端口与资源策略（作业要求中的「与 HTTP 任务共存」）

**一个进程一个 `Engine`**：一个监听端口、一套 DHT 节点表，所有 BT 任务共享它。

- 与「每个任务一个客户端」相比：避免端口冲突、避免成倍重复的 DHT 引导流量与连接数。
  BT 协议本身就支持在单端口上承载多个 torrent（按 infoHash 区分）。
- 与 HTTP 引擎共存：BT 使用独立的 TCP/UDP 监听端口（`ListenPort`，0 表示系统分配），
  HTTP 引擎只用出向连接，两者不争抢端口。带宽治理由 `limiter` 统一提供：
  HTTP 侧用 `Options.RateLimit`/`GlobalLimiter`，BT 侧映射为 `ClientConfig.DownloadRateLimiter`。
- **默认关闭 UPnP 端口映射**（`NoDefaultPortForwarding = true`）：桌面工具不应在未告知用户的情况下改动路由器配置。

---

## 3. 存储句柄与 piece completion（一个被修复的真实缺陷）

### 3.1 问题

初版实现把 `spec.Storage` 留给库按 `DataDir` 自行创建。后果有两层：

1. **同一个数据目录会被打开多个 piece completion 数据库**。boltdb 是独占锁，
   第二个实例拿不到锁（`Timeout: 1s`）后会**静默回退到内存实现**——
   表现是「重启后进度全部消失」。库只打一条 `WRN` 日志，调用方无从感知。
2. **数据库与文件句柄永不释放**：`fileClientImpl.Close()` 只关闭 completion，
   而谁都持有它的引用。Windows 上这会阻止删除/重命名已下载的文件。

### 3.2 修复

`Engine` 按数据目录缓存 storage 实例，并在 `Close` 时显式关闭：

```go
storages map[string]storage.ClientImplCloser   // dir -> storage

storageFor(dir)   // 同一目录只创建一次，多个 torrent 共享
Close()           // client.Close() → 逐个 storage.Close()
```

修复后的验证（`TestPauseAndRestartKeepsProgress`）：

```
首轮已完成 16/64 个 piece（1294336 字节），关闭客户端
重启后恢复 16 个 piece（重启前 16 个）      ← 断点续传成立
```

### 3.3 仍需知悉的边界

若 completion 数据库确实无法打开（例如另一进程正在占用），库会回退到内存实现。
此时**数据不会丢**，但重启后需要对已有文件做一次 hash check 才能恢复 piece 状态
（代价是校验耗时，不是重新下载）。这是库的内部行为，本包无法覆盖，因此**下载目录不宜被两个客户端实例同时使用**。

---

## 4. 进度桥接（BT ↔ 统一任务模型）

`Torrent.ApplyToTask` 把 BT 状态写入 `engine.Task`：

| Task 字段 | 来源 |
|---|---|
| `Kind` | 固定 `bittorrent` |
| `InfoHash` | `torrent.InfoHash().HexString()` |
| `PieceLength` / `PieceBitmap` | `metainfo.Info.PieceLength` / 逐 piece `PieceState(i).Complete` |
| `TotalSize` | `torrent.Length()` |
| `DisplayName` / `DestPath` | `info.BestName()` / `filepath.Join(DataDir, name)` |
| `State` | 全部 piece 完成 → `completed` |

**BT 任务不使用 HTTP 的分片模型**（`Segments` 为空）：BT 的调度单元是 piece，
由库负责编排，强行映射到 `Segment` 只会产生两套互相矛盾的真相。

速率是差分计算的：BT 库只提供累计字节数，本包维护上一采样点并在
`rateWindow = 500ms` 的窗口上算出瞬时速率（窗口内的重复调用复用上次结果，避免高频快照导致速率抖动）。

---

## 5. 本地互测（主验收路径）

`P2PDL_PUBLIC_BT_TEST` 未设置时，所有验证都在完全离线环境下完成：
`DisableDHT + DisableTrackers + DisablePEX + DisableUTP`，通过**静态 peer 直连**（`127.0.0.1:<端口>`）。

| 测试 | 验证内容 |
|---|---|
| `TestLocalSeederAndLeecherViaTorrentFile` | 种子文件路径：种子端做种 → 下载端 receive → **SHA-256 与源一致** |
| `TestLocalMagnetRoundTrip` | 磁力链路径：元数据经 BEP 9 从 peer 获取（`WaitInfo` 后 Name/Length 才可用）→ 下载校验一致 |
| `TestPauseAndRestartKeepsProgress` | 暂停 → 关闭客户端 → 同目录重启 → **恢复 16/64 piece 而非重下** → 续传完成并校验 |
| `TestSnapshotFieldsAndBitmap` | 位图长度 == piece 数；完成后全部置位；`CompletedBytes == TotalBytes` |
| `TestApplyToTaskProducesValidTask` | 桥接后的 Task 能通过 `Validate()`（否则上层会当作损坏元数据） |
| `TestEngineLifecycleAndHandles` | 端口分配、按 infoHash 查找、`Drop` 幂等性、磁力链生成 |
| `TestEngineRejectsEmptySpec` | 空来源 / 非法磁力链 / 不存在的种子 / 垃圾字节全部被拒绝 |

运行结果（本地）：7 个用例全部 PASS，单例耗时 2~3.5 秒。

---

## 6. 公网 swarm 验证（未能通过，已记录）

`TestPublicSwarmReachesRealPeers`（`P2PDL_PUBLIC_BT_TEST=1` 启用）使用 Ubuntu 24.04 官方种子：

```
种子: magnet:?xt=urn:btih:d160b8d8ea35a5b4e52837468fc8f03d55cef1f7&tr=...torrent.ubuntu.com...
目标文件：ubuntu-24.04.3-desktop-amd64.iso，大小 5.91 GB，piece 24208 个
公网 swarm 验证未通过：peer 峰值 0，已接收 0.00 B
```

**根因（环境限制，非实现缺陷）**：

| 现象 | 证据 |
|---|---|
| DHT 引导域名被 DNS 污染 | `router.bittorrent.com` 解析到 `31.13.84.34`（与 BT 引导无关的地址） |
| tracker 主站可达但 swarm 不可达 | `https://torrent.ubuntu.com/` 返回 200，但 announce 回来的 peer 无一可建立 TCP 连接 |
| 结论 | 当前网络出口阻断 BT 流量，公网 swarm 无法作为可复现的验收手段 |

**这与 T1 的风险预案一致**（「以本地 tracker + 两个本地客户端互测作为 T7 主验收路径，公网下载作为加分项」）。
因此 T7 的验收以 §5 的本地互测为准，它覆盖了协议全路径：peer 握手、元数据交换（BEP 9）、
piece 传输、piece 哈希校验、断点续传。**未覆盖的是「公网 peer 发现」这一环节**，
它依赖库的 tracker/DHT 实现，本包只是把参数透传给它。

---

## 7. 配置速查

```go
cfg := bittorrent.Config{
    DataDir:         "D:\\downloads",
    ListenPort:      0,        // 0 = 系统分配
    DisableDHT:      false,
    DisableTrackers: false,
    DisablePEX:      false,
    DisableUTP:      false,    // 纯 Go uTP 的兼容性兜底开关
    NoUpload:        false,    // 下载端可设 true 节省上行
    Seed:            false,    // 做种端必须为 true
    DownloadRateLimit: 0,      // 字节/秒，0 = 不限
    UploadRateLimit:   0,
    Logf:            log.Printf,
}
eng, _ := bittorrent.NewEngine(cfg)
defer eng.Close()

h, _ := eng.Add(bittorrent.AddSpec{
    Magnet:    "magnet:?xt=urn:btih:...",
    SavePath:  "",                       // 覆盖 DataDir
    PeerAddrs: []string{"10.0.0.5:6881"}, // 局域网直连/测试
})
task := /* engine.Task */
h.WaitInfo(ctx)
h.Download(ctx)          // 阻塞直到完成，返回 context.Canceled 表示可续传的取消
h.ApplyToTask(task)      // 进度写回统一模型
```
