# 大文件性能与资源控制基准（T6）

本文件记录可复现的性能与资源数据，以及默认参数的取值依据。

---

## 1. 测试环境

| 项 | 值 |
|---|---|
| CPU | 16 逻辑核（基准输出中的 `-16`） |
| OS | Windows 10 19041 |
| Go | go1.27.2 windows/amd64 |
| 磁盘 | D: 卷，SSD，剩余 322GB |
| 网络 | `httptest` loopback（排除公网抖动，专注引擎自身开销） |

复现命令：

```powershell
# 基准
.\build.ps1 -Test
$env:GOTMPDIR="$PWD\.tools\tmp"
go test -c -o .tools\tmp\bench.test.exe -test.run=XXX ./internal/engine/httpdl
.\.tools\tmp\bench.test.exe -test.bench=. -test.benchtime=3x -test.run=XXX

go test -c -o .tools\tmp\sinkbench.test.exe -test.run=XXX ./internal/engine/sink
.\.tools\tmp\sinkbench.test.exe -test.bench=. -test.benchtime=3x -test.run=XXX

# 10GB 大文件实测（需要 ≥11GB 可用空间，约 10 秒）
$env:P2PDL_LARGE_TEST="1"
go test ./internal/engine/httpdl -run TestLargeFileMemoryFootprint -v -timeout 30m
```

---

## 2. 10GB 大文件实测（T6 验收项 1）

`TestLargeFileMemoryFootprint`（默认跳过，`P2PDL_LARGE_TEST=1` 启用）真实写入 10GB 到磁盘：

```
目标目录可用空间 319.87 GB
10GB 下载完成：耗时 10.124s（平均 1011.46 MB/s），采样 202 次
内存峰值：Go 堆 2.86 MB，进程 Sys 18.48 MB
分片统计：模式=multi-segment 连接总数=8
缓冲区理论占用 256.00 KB，实测堆峰值 2.86 MB（11 倍）
```

| 指标 | 实测 | 验收上限 | 结论 |
|---|---|---|---|
| **Go 堆峰值** | **2.86 MB** | 500 MB | ✅ 通过（余量 175 倍） |
| 进程 Sys（向 OS 申请总量） | 18.48 MB | — | 参考值 |
| 平均吞吐 | 1011 MB/s | — | 受 loopback 与磁盘限制 |
| 峰值活跃连接 | 8（等于配置并发） | — | 并发控制生效 |

**内存为何与文件大小无关**：占用只由「并发数 × 缓冲区」与「位图元数据」决定。
本例中 8 × 32KB = 256KB，位图 10GB/16MB = 640 位 ≈ 80 字节。
实测略高于理论值（2.86MB vs 256KB）来自 HTTP 栈、TLS/连接缓冲与采样期间的其他分配。

---

## 3. 下载吞吐基准

```
BenchmarkDownloadToMemory-16                  3   5576667 ns/op   1504.23 MB/s   12403930 B/op   1890 allocs/op
BenchmarkDownloadSingleSegment-16             3   5894500 ns/op   1423.12 MB/s   11406485 B/op    451 allocs/op
BenchmarkDownloadToFile-16                    3  15006533 ns/op    559.00 MB/s    3960085 B/op   1839 allocs/op
BenchmarkProbeOnly-16                         3    417433 ns/op             —     429709 B/op    233 allocs/op
```

**解读**：
- 内存 sink 下并发与单连接差异不大（1504 vs 1423 MB/s）——loopback 不是瓶颈，瓶颈在 HTTP 解析与内存拷贝。多分段的真正价值在高延迟公网（每个连接都是独立流水线窗口）。
- 落到真实文件后吞吐降到 559 MB/s：这部分开销属于磁盘写入路径（对比 §4：单个 `WriteAt` 就能到 935 MB/s，说明剩余损失在 HTTP 与写盘的交错上）。
- `Probe` 单次约 0.42ms（含 1~2 次网络往返），相对下载耗时可以忽略，但**只应在创建任务时做一次**。

## 4. 缓冲大小对吞吐的影响（默认值 32KB 的依据）

```
BenchmarkDownloadBufferSizes/buffer=8KB-16      3   6493667 ns/op   1291.81 MB/s   11972138 B/op   1859 allocs/op
BenchmarkDownloadBufferSizes/buffer=32KB-16     3   3945267 ns/op   2126.25 MB/s   12163090 B/op   1850 allocs/op
BenchmarkDownloadBufferSizes/buffer=64KB-16     3   3913833 ns/op   2143.32 MB/s   12439370 B/op   1839 allocs/op
BenchmarkDownloadBufferSizes/buffer=256KB-16    3   4739967 ns/op   1769.76 MB/s   14043437 B/op   1832 allocs/op
BenchmarkDownloadBufferSizes/buffer=1024KB-16   3   4778167 ns/op   1755.61 MB/s   20325360 B/op   1881 allocs/op
```

| 缓冲 | 吞吐 | 相对 32KB |
|---|---|---|
| 8KB | 1292 MB/s | −39% |
| **32KB（默认）** | **2126 MB/s** | 基准 |
| 64KB | 2143 MB/s | +0.8% |
| 256KB | 1770 MB/s | −17% |
| 1MB | 1756 MB/s | −17% |

**结论**：32KB 是拐点。64KB 只快 0.8%，却让 8 并发多占 256KB 内存；
256KB 以上反而变慢（分配与大块拷贝成本上升）。
选择 **32KB**：吞吐接近最优、单连接内存占用最小。

## 5. FileSink 按偏移写入基准

```
BenchmarkFileSinkWriteAt/block=4KB-16      3  136392900 ns/op    492.03 MB/s     7733 B/op    23 allocs/op
BenchmarkFileSinkWriteAt/block=32KB-16     3   71738167 ns/op    935.47 MB/s    36336 B/op    22 allocs/op
BenchmarkFileSinkWriteAt/block=256KB-16    3   52760500 ns/op   1271.95 MB/s   265690 B/op    21 allocs/op
BenchmarkFileSinkWriteAt/block=1MB-16      3   50896433 ns/op   1318.54 MB/s  1052122 B/op    21 allocs/op
BenchmarkFileSinkSync-16                   3   18586867 ns/op    902.64 MB/s   267272 B/op    29 allocs/op
BenchmarkPlanRecoveryComplete-16           3      34467 ns/op               —      912 B/op    35 allocs/op
```

**解读**：
- 写入块从 4KB 提到 32KB，吞吐翻倍（492 → 935 MB/s）：这就是「按偏移写」配合足够大缓冲的价值，
  说明**不需要整文件预分配**也能获得可接受的写盘性能（同时避免了 T1 §6 风险 4）。
- 每 4MB fsync 一次的写入吞吐仍有 902 MB/s，说明默认 8MB 的 fsync 间隔不会成为瓶颈。
- `PlanRecovery` 在 16 分片、已完成任务上是 34.5µs：断点恢复决策的成本可忽略，
  可以在每次任务启动时无条件执行。

---

## 6. 限速精度（T6 验收项 2）

`TestDownloadRateLimitIsEnforced`：真实 HTTP 下载 1MB，限速 256KB/s。

```
限速 256.00 KB/s，实测 259.94 KB/s（耗时 3.9394384s）
```

| 指标 | 值 | 验收区间 | 结论 |
|---|---|---|---|
| 实测速率 | 259.94 KB/s | 设定值 ±15%（217.6 ~ 294.4） | ✅ 偏差 +1.5% |

**关键实现细节**：令牌桶 burst 取 `clamp(rate/16, 8KB, 4MB)` 而不是「1 秒额度」。
若 burst 取 1 秒额度，限速 1MB/s 时前 1MB 会瞬间完成，实测速率会虚高数倍。
另外 `WaitN` 会把「大于 burst 的请求」自动拆分为多次等待——`x/time/rate` 对超过 burst
的 `WaitN` 直接报错，而缓冲大小与用户设定的限速值互不相干，必须在此兼容。

---

## 7. 磁盘空间预检查（T6 验收项 3）

| 场景 | 行为 |
|---|---|
| 空间充足 | 正常开始 |
| `EnsureFreeSpace(dir, 1<<62)` | 返回 `ErrInsufficientSpace`（可用 `errors.Is` 识别） |
| 目标卷不存在（如 `Z:\`） | `Run` 在**发出任何下载请求之前**失败 |
| 目录尚不存在 | 回退到最近的已存在祖先目录，仍能正确判断 |
| `SkipSpaceCheck: true` | 跳过检查（供测试/特殊场景） |

需要空间按 `剩余字节数 × 1.05` 计算（预留 5% 给文件系统元数据与 fsync 抖动）；
长度未知时按 64MB 预留。检查在**探测前后各执行一次**：探测前用已知尺寸快速失败，
探测后可修正为精确值。

---

## 8. 参数默认值与依据汇总

| 参数 | 默认值 | 依据 |
|---|---|---|
| 并发分片数 | 8 | 常见宽带下能跑满带宽；配合内存预算可自动下调 |
| 单连接缓冲 | 32KB | §4 的吞吐拐点，单连接内存占用最小 |
| 位图分块（chunk） | 1MB | 位图体积与 fsync/标记频率的平衡（10GB → 1.25KB 位图） |
| fsync 间隔 | 8MB | §5：每 4MB fsync 仍有 902 MB/s，8MB 更省 |
| 分片重试上限 | 5 | 配合 0.5s → 30s 指数退避，覆盖常见瞬断 |
| 限速 burst | `clamp(rate/16, 8KB, 4MB)` | §6：避免「1 秒额度」导致的限速失真 |
| 内存预算 | 不限（可用 `MaxMemory` 约束） | 设置后自动换算为「并发 × 缓冲」 |
