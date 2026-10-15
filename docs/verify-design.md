# 完整性校验与错误恢复（T9）

实现：`internal/engine/verify/`（`hash.go` 流式哈希、`manifest.go` 分块清单、`verifier.go` 校验与修复决策）。
接入点：`internal/engine/scheduler.go` 的 `finish`（校验）与 `internal/app/session.go`（就地修补）。

---

## 1. 为什么需要「分块清单」

整体摘要只能回答**文件对不对**，不能回答**哪里不对**。没有定位能力时，一个字节损坏的
代价是重下整个文件。分块清单（manifest）把「文件坏了」变成「第 5 块坏了」：

```
整文件 SHA-256 不符
   ↓ 有清单
逐块比对 → 第 5 块不符 → 清除该块位图 → 只重下 64KB
```

实测（`app.TestRecheckOnlyRedownloadsDamagedRanges`，2MB 文件、4 并发分片、64KB 分块）：

| 阶段 | 服务端实际吐出字节 |
|---|---|
| 首次完整下载 | 2,097,154（含 2 字节探测） |
| 破坏第 5 块后重新校验并修复 | **65,538**（= 1 个 64KB 块 + 2 字节探测） |

清单文件位置：`<文件路径>.p2pdl.manifest`，JSON 格式，可被外部工具读取与生成：

```json
{
  "version": 1,
  "algorithm": "sha256",
  "chunkSize": 65536,
  "totalSize": 2097152,
  "chunks": ["9f86d0…", "…"]
}
```

---

## 2. 三种校验来源与优先级

| 优先级 | 来源 | 定位能力 | 适用场景 |
|---|---|---|---|
| 1 | 分块清单（manifest） | **精确到块** | 下载完成后生成基线；之后任意时刻可定位 |
| 2 | 整体摘要（`AddOptions.ExpectedHash`、镜像站的 `.sha256`） | 只能判定整体损坏 | 最常见的用户输入 |
| 3 | BitTorrent piece hash | **精确到 piece** | 协议自带，由 BT 库负责校验与重下 |

`Verifier.Verify` 的决策顺序：

```
BT 任务                     → 触发引擎重校验（Recheck 钩子）
未启用校验                   → 直接通过
有期望摘要且摘要匹配          → 通过（可选生成清单）
有期望摘要但不匹配 + 有清单    → 定位受损块 → 标记重下（Repaired）
有期望摘要但不匹配 + 无清单    → 如实失败（不掩盖问题）
无期望摘要                   → 只验证「能否完整读取」
```

---

## 3. 一个必须解决的陷阱：数据分散在两个文件里

**场景**：文件首次下载完成、已提交到最终位置；之后校验发现第 5 块损坏需要重下。

如果仍按常规流程「写到临时文件 → 重命名为最终文件」，会发生什么：

1. 新建一个空的临时文件；
2. 只往里面写了第 5 块（其余位置靠 `WriteAt` 扩展成零）；
3. 任务位图显示「全部完成」→ 提交 → **最终文件被替换成「只有第 5 块正确、其余全是零」的废文件**。

首次修复前的实现还叠加了第二个放大问题：`streamSegment` 从第一个缺口一直请求到分片末尾，
于是「重下 64KB」实际变成「重下整个分片 512KB」（实测 196,610 字节）。

**两个修复**：

1. **就地修补**：`chooseWriteTarget` 发现最终文件已存在且长度与 `TotalSize` 一致时，
   让 `FileSink` 直接写最终文件（`InPlace` 模式），`Commit` 只做长度修正、不重命名。
   `Discard` 在就地模式下**不删除任何数据**。
2. **按连续缺口请求**：`streamSegment` 只请求「一段连续缺口」，跳过的已完成块连网络传输都省掉。

此外 `httpSession.Close` 增加了安全门：**只有临时文件长度等于总长度时才允许提交**，
否则放弃提交并报错——宁可让任务失败，也不能让残缺文件覆盖现有数据。

---

## 4. 重试与退避（不出现无限重试，也不静默失败）

| 层级 | 机制 | 上限 |
|---|---|---|
| 分片传输 | 指数退避 `500ms → 30s` | `httpdl.Options.MaxRetries`（默认 5） |
| 整体下载 | 单流模式同样退避重试 | 同上 |
| 校验→修复循环 | `task.VerifyAttempts` 计数 | `verify.DefaultMaxRepairAttempts`（3） |
| 调度器停止 | 状态回退为 `pending`，断点保留 | — |

- **修复计数在成功完成后归零**，使同一任务在后来的重新校验中仍有完整额度。
- 达到修复上限时返回明确错误（`修复次数达到上限（3），可能磁盘或网络持续损坏`），
  任务进入 `failed` 且 `LastErr` 可读——不存在「悄悄放弃」的路径。
- 摘要不符但**无法定位**时（无清单），任务失败并说明原因，而不是标记完成或反复重下整个文件。

---

## 5. 状态机补充

`verifying → pending` 是本任务新增的合法迁移：校验发现损坏且已定位到受损区间时，
任务需要重新排队只重下这些区间。其余终态规则不变（`completed` 仍需用户显式操作才能离开）。

---

## 6. 测试覆盖

| 验收项 | 测试 |
|---|---|
| 人为破坏后能定位到具体块 | `verify.TestManifestLocatesCorruptedChunks`（破坏第 1、3 块 → 精确报告下标与区间） |
| 相邻坏块合并 | `verify.TestManifestMergesAdjacentBadChunks` |
| 定点重下（端到端） | `app.TestRecheckLocatesAndRepairsCorruptedChunks`（修复后 SHA-256 与源一致） |
| 只重下受损区间 | `app.TestRecheckOnlyRedownloadsDamagedRanges`（重下量 < 文件 1/4，实测 65,538 字节） |
| 完整文件重新校验不产生下载 | `app.TestRecheckOnIntactFilePassesWithoutDownload` |
| 无法定位时如实失败 | `app.TestVerificationFailureWithoutManifestIsReported`、`verify.TestVerifierFailsWithoutManifest` |
| 错误摘要被发现 | `app.TestWrongExpectedHashFailsTask` |
| 持续损坏不无限重下 | `verify.TestVerifierStopsAfterMaxRepairs` |
| 就地修补不破坏未修补字节 | `sink.TestFileSinkInPlaceCommitKeepsFile`（`abcdefgh` → 修补中间两字节 → `abcXYfgh`） |
| 清单大小不符被拒绝 | `verify.TestManifestRejectsSizeMismatch` |
| 流式校验内存与文件大小无关 | `verify.TestHashFileReportsProgress`（固定缓冲区 + 进度回调） |
| BT 重校验路径 | `verify.TestVerifierHandlesBitTorrentRecheck` |
