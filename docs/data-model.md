# 核心数据模型与持久化（T3）

本文档描述「断点续传」的真相来源：任务元数据模型、位图表示与文件存储格式。
实现位于 `internal/engine/{model.go,bitmap.go}` 与 `internal/store/`。

---

## 1. 分层与依赖方向

```
internal/engine   任务模型（Task / Segment / Bitmap）、状态机、后续的调度器
      ↑
internal/store    持久化实现（FileStore），依赖 engine 的模型类型
```

依赖是单向的：`engine` 不感知存储介质，`store` 不感知下载逻辑。
将来若把 JSON 换成 SQLite（T1 §3.4 候选 B），只需重写 `internal/store`。

---

## 2. 任务状态机（`TaskState`）

| 状态 | 含义 | 可迁移到 |
|---|---|---|
| `pending` | 已入队，等待调度 | `running`, `paused`, `failed` |
| `running` | 正在传输 | `paused`, `verifying`, `failed` |
| `paused` | 用户或调度器暂停，分片进度保留 | `running`, `failed` |
| `verifying` | 数据已收齐，正在校验 | `completed`, `failed` |
| `completed` | 校验通过、文件已就位（终态） | — |
| `failed` | 重试耗尽后失败（终态） | `pending`（用户手动重试） |

分片状态（`SegmentState`）为 `pending` / `running` / `completed` / `failed`，
它只描述调度单元，不参与任务级状态判定。

---

## 3. 分片与位图

### 3.1 两个粒度的区分

- **分片（Segment）** 是并发调度单元，长度由 `Task.Split(segmentCount)` 等分切分，
  余数从前往后逐段 +1 字节，保证**无缝、无重叠、合计等于文件大小**。
- **分块（Chunk）** 是进度记录粒度，全局统一为 `Task.ChunkSize`（默认 1MB，
  T4 接入时由配置决定）。分块与并发度无关：并发度可以随时调整，进度记录不失效。

### 3.2 位图而非区间列表

每个 `Segment` 持有一个相对该段起点的定长位图：

```
位图第 i 位  ==  文件区间 [Start + i*ChunkSize, min(Start + (i+1)*ChunkSize, End)) 已落盘
```

选择位图的理由：下载过程中已完成区间高度碎片化，区间列表的长度随碎片数增长，
而位图的空间确定且紧凑（1MB 分块 → 1 bit/分块，100GB 文件约 12.8KB）。

### 3.3 不存储冗余进度

「已下载多少字节」不落任何字段，一律由位图推导（`Segment.DownloadedBytes`、
`Task.Downloaded`）。这消除了「两份进度互相矛盾」的可能——强杀进程后不可能出现
「字段说下载了 80%，位图只记录了 60%」的状态。

### 3.4 强制不变量（`Task.Validate`）

1. `0 <= Start < End <= TotalSize`
2. 分片下标连续、区间首尾相接（第 i 段起点 == 第 i-1 段终点）
3. 分片总长度 == `TotalSize`
4. `Bitmap.Len() == ceil(Segment.Length() / ChunkSize)`
5. `ChunkSize > 0`、状态取值合法、BT 任务必须有 `InfoHash`

违反任何一条 → 元数据判定为**损坏**，不允许落盘，也不允许继续使用。

---

## 4. 存储格式

```
<storedir>/tasks.json                     任务索引（ID 列表），可自动重建
<storedir>/<taskid>.dltask                单任务完整状态，原子写入
<storedir>/corrupt/<taskid>.<ts>.dltask.bak   被隔离的损坏元数据
```

`*.dltask` 文件内容：

```
P2PDL-STORE 1                     ← 固定文件头：格式标识 + 版本号
{                                 ← 缩进 JSON，便于人工排查与修复
  "id": "...", "kind": "http",
  "segments": [ { "index": 0, "start": 0, "end": 2621440,
                  "bitmap": { "n": 3, "bits": "AQ==" }, ... } ],
  ...
}
```

位图自描述（`n` + `bits`）是损坏检测的基础：位数与字节数不匹配（被截断、被手工改坏）
会在反序列化阶段直接报错，而不是静默算出错误进度。

### 4.1 原子写

`store.WriteFileAtomic` 采用 **临时文件 + fsync + rename**：

```
写 <name>.tmp-<random>  →  tmp.Sync()  →  Close  →  os.Rename(tmp, 目标)
```

任意时刻被强杀，目标文件只会是「旧的完整内容」或「新的完整内容」，
绝不会出现半写 JSON。副作用是目录下可能残留 `*.tmp-*`，已在失败路径用 `defer` 清理。

*已知限制*：不执行目录 fsync（Windows 不支持），极端断电场景下 rename 本身可能丢失，
但不会出现内容撕裂。

---

## 5. 恢复流程（`Load` / `LoadAll` / `RequeueInterrupted`）

进程重启后：

1. `FileStore.List()` 读取索引；索引缺失或损坏时**合并目录扫描结果**并原子重建索引，
   保证任务不会因索引问题而消失。
2. `FileStore.LoadAll()` 逐个读回任务；单个任务损坏只进入 `corrupt` 映射，
   不影响其余任务（**降级而非整体失败**）。
3. `Task.RequeueInterrupted()` 把元数据中残留的 `running` 分片与任务级的
   `running`/`verifying` 一并回退为 `pending`——强杀不是用户主动暂停，重启后应自动继续；
   而用户主动暂停的任务状态本来就是 `paused`，不会被误提为待调度。
4. 调用方（T8 调度器）据此恢复队列，只对未完成分片的缺口区间续下。

---

## 6. 损坏处理策略

| 场景 | 检测点 | 处理 |
|---|---|---|
| 文件头不匹配 / 缺失 | `decodeTask` | `ErrCorrupt`，隔离后重建任务 |
| JSON 截断或语法错误 | `json.Unmarshal` | 同上 |
| 结构不自洽（分片空洞、位图长度不符） | `Task.Validate` | 同上 |
| 索引文件损坏 | `readIndexLocked` | 合并目录扫描结果并重建，不丢任务 |
| 任务 ID 含路径分隔符 | `validateID` | `ErrInvalidID`，拒绝读写 |

调用方用 `store.IsCorrupt(err)` / `IsNotFound(err)` / `IsInvalidID(err)` 分类，
不要比较错误字符串。损坏任务可通过 `FileStore.Quarantine(id)` 保留现场后重建。

---

## 7. 测试覆盖（`go test ./internal/...`）

| 验收项 | 测试 |
|---|---|
| 写入 → 关闭 → 重开 → 状态等价 | `TestSaveLoadRoundTripAcrossReopen`（`reflect.DeepEqual` 全结构比较） |
| 原子写、无临时文件残留 | `TestWriteFileAtomicReplacesContentAndLeavesNoTempFiles` |
| 损坏元数据可检测、可降级 | `TestCorruptMetadataIsDetected`、`TestLoadAllIsolatesCorruptTasks` |
| 索引自愈 | `TestIndexSelfHealsWhenCorrupted` |
| 分片切分无缝无重叠 | `TestSplitCoversWholeFileWithoutOverlap`、`TestValidateDetectsInconsistentSegments` |
| 位图 JSON 自描述与损坏拒绝 | `TestBitmapJSONRoundTrip`、`TestBitmapJSONRejectsCorruptData` |
| 强杀后状态回退 | `TestRequeueInterruptedRestoresConsistentState` |
