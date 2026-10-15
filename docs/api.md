# 本地控制面 API 契约（T10）

实现：`internal/control/`。契约由 `internal/control/server_test.go` 的集成测试逐项验证。

---

## 1. 安全模型（先读这一节）

| 约束 | 实现 | 理由 |
|---|---|---|
| **仅监听回环地址** | 默认 `127.0.0.1:0`，测试断言 `host == 127.0.0.1` 且从本机非回环 IP 连接失败 | 控制面能删除文件、改配置，暴露到局域网等于把文件系统交给他人 |
| **Bearer Token** | 32 字节随机十六进制，写入 `<MetaDir>/control.token`（权限 0600） | 同机其他进程（含浏览器页面）不应能操纵下载器 |
| **常量时间比较** | `subtle.ConstantTimeCompare` | 避免令牌被逐字节推断 |
| **严格 JSON 解析** | `DisallowUnknownFields` + 1MB 上限 | 拼错的字段名会立刻报错，而不是被静默忽略 |
| **panic 隔离** | 每个请求包裹 `recover` | 单请求异常不拖垮整个控制面 |

`GET /api/v1/health` 不需要令牌：GUI 需要它来判断引擎是否就绪。

SSE 端点允许用查询参数传令牌（`?token=`），因为浏览器 `EventSource` 无法自定义请求头。

---

## 2. 端点总览

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/health` | 健康检查（**免认证**） |
| GET | `/api/v1/version` | 版本与构建信息 |
| GET | `/api/v1/tasks` | 任务列表 |
| POST | `/api/v1/tasks` | 新建任务 |
| GET | `/api/v1/tasks/{id}` | 任务详情 |
| DELETE | `/api/v1/tasks/{id}?files=true` | 删除任务（可选同时删除文件） |
| POST | `/api/v1/tasks/{id}/pause` | 暂停 |
| POST | `/api/v1/tasks/{id}/resume` | 恢复 |
| POST | `/api/v1/tasks/{id}/recheck` | 重新校验（发现损坏时定点重下） |
| GET | `/api/v1/stats` | 调度器统计 |
| GET | `/api/v1/settings` | 读取设置 |
| PATCH | `/api/v1/settings` | 修改设置（当前仅全局限速） |
| GET | `/api/v1/events` | SSE 事件流 |

所有响应均为 `application/json; charset=utf-8`，SSE 为 `text/event-stream`。

---

## 3. 数据结构

### TaskView

```json
{
  "id": "20261012T143000-a1b2c3d4e5f6",
  "displayName": "ubuntu.iso",
  "kind": "http",                  // http | bittorrent
  "state": "running",              // pending | running | paused | verifying | completed | failed
  "source": "https://.../ubuntu.iso",
  "destPath": "D:\\downloads\\ubuntu.iso",
  "total": 1073741824,
  "done": 524288000,
  "progress": 0.488,
  "rate": 8388608,
  "peers": 0,
  "mode": "multi-segment",         // multi-segment | single-stream | bittorrent
  "lastError": "",
  "createdAt": "2026-10-12T14:30:00Z",
  "updatedAt": "2026-10-12T14:31:02Z"
}
```

### 错误响应

```json
{ "error": { "code": "task_not_found", "message": "任务不存在: abc" } }
```

| HTTP | code | 触发场景 |
|---|---|---|
| 400 | `invalid_body` / `invalid_query` / `invalid_value` | 请求体或查询参数非法 |
| 400 | `missing_source` | 未提供 `source` 与 `torrentPath` |
| 400 | `add_failed` | 探测失败、摘要长度非法、种子不可解析 |
| 401 | `missing_token` | 缺少令牌 |
| 403 | `invalid_token` | 令牌不匹配 |
| 404 | `task_not_found` | 任务不存在 |
| 409 | `pause_failed` / `resume_failed` / `recheck_failed` / `delete_failed` | 状态机拒绝（例如暂停已完成任务、恢复不存在的任务） |
| 500 | `internal_error` / `no_streaming` | 内部异常 / 运行环境不支持流式响应 |

---

## 4. 端点详情

### POST /api/v1/tasks

请求体（`source` 与 `torrentPath` 二选一）：

```json
{
  "source": "https://releases.ubuntu.com/24.04/ubuntu-24.04.3-desktop-amd64.iso",
  "torrentPath": "",
  "name": "ubuntu.iso",
  "destPath": "",
  "header": { "Referer": "https://example.com" },
  "peers": ["10.0.0.5:6881"],
  "expectedHash": "9f86d081884c7d65...",
  "hashAlgorithm": "sha256",
  "paused": false
}
```

- 来源类型按内容自动判定：`magnet:` 前缀 → BT 磁力链；`torrentPath` 非空 → 种子文件；否则按 HTTP URL。
- `expectedHash` 提供后，下载完成会执行完整性校验；校验失败且存在分块清单时只重下受损块。
- `peers` 用于无 DHT/tracker 场景（局域网直连、测试）。
- `paused: true` 时任务以 `paused` 状态入队，不会自动开始。

响应：`201 Created` + `TaskView`。

实测校验规则（`TestCreateTaskValidation`）：

| 请求 | 结果 |
|---|---|
| `{}`（无来源） | 400 `missing_source` |
| `{"source":"ftp://..."}` | 400 `add_failed` |
| `{"torrentPath":"不存在"}` | 400 `add_failed` |
| `{"source":"...","expectedHash":"abcd"}` | 400 `add_failed`（sha256 需要 64 位十六进制） |
| 含未知字段 | 400 `invalid_body` |

### GET /api/v1/tasks

```json
{ "tasks": [ /* TaskView */ ], "total": 3 }
```

按创建时间升序（与 `Scheduler.List` 一致），保证前端渲染顺序稳定。

### DELETE /api/v1/tasks/{id}?files=true

```json
{ "id": "...", "removed": true, "filesDeleted": true }
```

`files=true` 时同时删除目标文件、临时文件与分块清单。BT 数据文件若被 BT 库持有句柄，
删除会失败并**如实返回 409**（见 `docs/bittorrent.md` §3.3），不会假装成功。

### POST /api/v1/tasks/{id}/recheck

把任务重新排队。引擎确认数据已就位（无缺口时立即返回）后执行校验：
摘要不符且有分块清单 → 精确定位受损块 → 只重下这些块；无清单 → 任务以可读原因失败。

### GET /api/v1/stats

```json
{
  "tasks": { "total": 3, "running": 1, "pending": 0, "paused": 1, "completed": 1, "failed": 0, "maxConcurrent": 3 },
  "dataDir": "D:\\downloads",
  "rateLimit": 0
}
```

### PATCH /api/v1/settings

```json
{ "globalRateLimit": 1048576 }
```

- 单位字节/秒，`0` 表示不限速，负数返回 400。
- **只暴露运行期可安全变更的设置**。`dataDir`、`chunkSize` 等会影响已存在任务的数据布局，
  必须在内核重建时才生效，因此不通过控制面修改（避免「改了设置但已有任务按旧参数运行」的困惑）。

---

## 5. 事件流（SSE）

```
GET /api/v1/events?token=<token>
Content-Type: text/event-stream
```

连接建立后**立即**推送一条 `stats` 与当前所有任务的 `task.added`（客户端无需再调列表接口即可渲染）。
之后每 100ms（可配置）对比一次任务快照并广播差异；每 15 秒发送一次 `heartbeat` 保活。

事件类型：

| type | 载荷 | 触发条件 |
|---|---|---|
| `task.added` | `task` | 新任务出现 |
| `task.updated` | `task` | `done` 变化，或状态变化且不是完成/失败 |
| `task.completed` | `task` | 状态变为 `completed` |
| `task.failed` | `task` | 状态变为 `failed` |
| `task.removed` | `task` | 任务从列表消失 |
| `stats` | `stats` | 每个推送周期 |
| `heartbeat` | 无 | 每 15 秒 |

```
event: task.updated
data: {"type":"task.updated","at":"2026-10-12T14:30:05.123Z","task":{"id":"...","done":1048576,...}}
```

**验收实测**（`TestEventStreamPushesProgressWithinOneSecond`，8MB 文件持续下载）：
最大相邻进度事件间隔 **132.8ms**，远低于 1 秒的要求。

订阅者过慢时事件会被丢弃（channel 缓冲 64），而不是让控制面无限积压内存——
下一次 `stats`/任务快照即可让客户端恢复一致。

---

## 6. 客户端示例

```bash
TOKEN=$(cat <MetaDir>/control.token)

# 新建任务
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"source":"https://example.com/big.iso","expectedHash":"..."}' \
  http://127.0.0.1:PORT/api/v1/tasks

# 观察事件流
curl -N "http://127.0.0.1:PORT/api/v1/events?token=$TOKEN"

# 全局限速 1MB/s
curl -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"globalRateLimit":1048576}' http://127.0.0.1:PORT/api/v1/settings
```
