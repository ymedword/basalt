# Phase 1 数据模型：US-9 Web Service

**Feature**: `009-web-service` | **Date**: 2026-10-18 | **Plan**: [plan.md](./plan.md) | **Research**: [research.md](./research.md)

**范围**：本 US **零迁移、零新增表 / 列 / 索引**（判据：`git diff -- internal/storage/schema.sql docs/contracts/schema.sql` 为空、`internal/storage/migrations/` 零新增文件）。下列「实体」分三层：① **持久实体**——既有 `sessions` / `messages` / `runs` 表，本 US 只首次读写其中若干列；② **传输实体**——信封与请求 / 视图 JSON，由 [docs/contracts/openapi.yaml](../../docs/contracts/openapi.yaml) 固定；③ **内存状态**——`web.Server` 的在途运行计数，进程内、不落库。字段与字面量以 `docs/contracts/` 为准，本文件只做结构化归纳，**不新增**任何字段。

## 1. 持久实体（既有表，schema.sql 零 diff）

### 1.1 Session（`sessions` 表）

来源：[docs/contracts/schema.sql:40-53](../../docs/contracts/schema.sql#L40-L53)；Go 侧记录类型 `core.SessionRecord`（[internal/core/store.go:167](../../internal/core/store.go#L167)，本 US 追加 `Status` / `ArchivedAt` 两个字段）。

| 列 | 类型 | 可空 | 本 US 行为 | 说明 |
| --- | --- | --- | --- | --- |
| `session_id` | TEXT PK | 否 | 读 / 写 | 由 `core.DeriveSessionID(channel, user, agent)` 确定性派生（见 §3.1） |
| `profile_name` | TEXT | 否 | 读 / 写 | 建会话时 = 请求的 `agent`；发消息运行时作为 `RunRequest.Agent` |
| `channel` | TEXT | 否 | 读 / 写 | 建会话缺省 `web`；发消息运行的 `RunRequest.Channel` 固定 `web`（来自会话行的是 `user_id`） |
| `user_id` | TEXT | 否 | 读 / 写 | 建会话缺省 `anonymous`；发消息运行时作为 `RunRequest.UserID` |
| `status` | TEXT DEFAULT `'active'` | 否 | **首次读 + 归档写** | 枚举 `active` / `archived`；新建行**显式置 `active`**，不依赖 DEFAULT 回读 |
| `message_count` | INTEGER DEFAULT 0 | 否 | 读 | 新建行为 0；消息追加由 `AgentService` 既有路径维护 |
| `created_at` | INTEGER | 否 | 写 / 读 | Unix 毫秒；幂等重复创建**不改写** |
| `last_active_at` | INTEGER | 否 | 写 / 读 | Unix 毫秒；幂等重复创建不改写（`GetOrCreateSession` 的 `ON CONFLICT DO NOTHING`） |
| `archived_at` | INTEGER | **是** | **首次写** | 毫秒；`ArchiveSession` 用 `COALESCE(archived_at, ?)`，重复归档保留**首次**时间 |

**索引**：既有 `idx_sessions_profile`、`idx_sessions_status` 不动；本 US 不新增索引（查询全部走主键）。

**约束**（映射到颗粒度文档 §2.2 / §6.3）：

- `session_id` 由 `(channel, user_id, agent)` 唯一确定（约束 6 / 7）——同元组并发创建由 `ON CONFLICT(session_id) DO NOTHING` 收敛为一行。
- 建会话幂等：重复 `POST /sessions` 返回既有行，`created_at` / `last_active_at` / `status` **不变**；已归档行**不复活**（`GetOrCreateSession` 不回写 `status`）。
- 归档只写 `status` + `archived_at`，**不删**消息 / 会话 / 审计（`DELETE` 不是物理删除）。
- 归档与在途运行互斥：在途计数 > 0 时归档返回 409，且**零写入**（D-01）。

### 1.2 Message（`messages` 表）

来源：[docs/contracts/schema.sql:55-70](../../docs/contracts/schema.sql#L55-L70)；Go 侧 `core.Message`（[internal/core/provider.go:18](../../internal/core/provider.go#L18)，本 US 追加 `CreatedAt time.Time json:"-"`）。

| 列 | 类型 | 可空 | 本 US 行为 | 说明 |
| --- | --- | --- | --- | --- |
| `id` | INTEGER PK AUTOINCREMENT | 否 | 读（内部） | 不对外 |
| `session_id` | TEXT REFERENCES sessions | 否 | 读 | 历史窗口按会话过滤 |
| `run_id` | TEXT | 是 | 读（不导出） | 消息与 Run 的关联；本 US 的视图**不输出**该列 |
| `seq` | INTEGER | 否 | 读 | 会话内序号（1 起）；窗口排序键 |
| `role` | TEXT | 否 | 读 | `system` / `user` / `assistant` / `tool` |
| `content` | TEXT | 是 | 读 | 可空（带工具调用的 assistant 消息 content 可能为空）→ 视图输出空串 |
| `tool_calls` | TEXT（JSON 数组） | 是 | 读 | 解析失败时的兜底见 §4.2；解析成功映射为 `ToolCallView` 数组 |
| `tool_call_id` | TEXT | 是 | 读 | tool 角色回填对应调用 |
| `reasoning_content` | TEXT | 是 | 不输出 | 供应商扩展，只在模型往返中使用 |
| `provider_extra` | TEXT（JSON 对象） | 是 | 不输出 | 供应商扩展透传位 |
| `created_at` | INTEGER | 否 | **首次读** | 毫秒 → 视图 RFC3339 UTC |

**窗口规则**：`MessagesTail(sessionID, 100)` 用 `ORDER BY seq DESC LIMIT 100` 取最近 100 条，Go 内反转为 `seq` 升序后输出（FR7）。**不接受**请求派生的排序 / 分页参数。

**写入纪律**：web 层**不写** `messages`——消息追加只由 `AgentService` / `ReActLoop` 既有单路径承担（宪法 II）。本 US 只读。

### 1.3 Run（`runs` 表）

来源：[docs/contracts/schema.sql:78-96](../../docs/contracts/schema.sql#L78-L96)。本 US **不直接读写该表**：`POST /messages` 与 `/invoke` 经 `RunService.Run` 进入 `core.AgentService`，Run 行（`running` → 终态）由既有执行链两阶段写；web 只消费返回的 `core.RunResult`（[internal/core/run.go:68-76](../../internal/core/run.go#L68-L76)）。`trigger` 固定 `api`（`triggerAPI`），由 `AgentService` 依据入口写入；`session_id` 在 `/invoke` 为 NULL、在 `/messages` 为会话 id。

**状态语义**（`RunStatus` 五值，[internal/core/run.go:49-62](../../internal/core/run.go#L49-L62)）：

| 状态 | 触发 | HTTP 结果 | `data.reason`（对外） |
| --- | --- | --- | --- |
| `completed` | 模型给出无工具调用的最终回答 | 200 `OK` | 空串 |
| `limit_reached` | 达迭代 / 工具调用上限（预期终止，不是错误） | 200 `LIMIT_REACHED` | 已知 reason 原样保留，其余投影「运行达到上限」 |
| `cancelled` | ctx 取消 / 截止时间到 | 200 `OK`（运行被正常取消）或 504（HTTP 超时判定先命中） | 固定「运行已取消」 |
| `failed` | 运行期致命错误 | 200 `OK`（运行完成但失败） | 固定「运行失败」 |
| `running` | 起始态 | 不会出现在响应中（响应总在运行结束后） | — |

> 504 与 200 `cancelled` 的区分：`handlePostMessage` 先判 `runCtx` 是否已到期——到期即 504 `TIMEOUT`（FR11）；客户端主动断开则**不写响应**。`RunResult.Status` 的脱敏规则见 research D11。

## 2. 传输实体（wire，契约固定）

### 2.1 Envelope（所有响应）

`docs/contracts/openapi.yaml` `components.schemas.Envelope`；Go 类型 `web.Envelope`（[docs/us/us9-web-service.md](../../docs/us/us9-web-service.md) §3.1）。

| 字段 | 类型 | 成功 | 失败 | 说明 |
| --- | --- | --- | --- | --- |
| `code` | string | `OK` / `LIMIT_REACHED` | 8 值之一 | `UNSUPPORTED_TYPE` / `NOT_CONFIGURED` 为契约保留，本 US **不产生** |
| `message` | string | `success` / 上限说明 | 固定或说明文案 | 冻结文案见 §2.4 |
| `data` | object | 存在 | **省略**（`omitempty` + nil 接口） | 唯一无载荷成功：`DELETE`（`writeOK(w, nil)`） |
| `timestamp` | string | RFC3339 UTC | RFC3339 UTC | `opts.Now().UTC().Format(time.RFC3339)` |

**HTTP 头**：`Content-Type: application/json; charset=utf-8`；**不输出**任何 `Access-Control-Allow-*` 头。

### 2.2 请求体

| 端点 | 字段 | 类型 | 必填 | 缺省 / 校验 |
| --- | --- | --- | --- | --- |
| `POST /sessions` | `agent` | string | **是** | 缺失 / null → 400「agent 必填」；未知或未注册 → 404；空串按未知 → 404 |
| | `channel` | string | 否 | 缺省 `web`；参与会话 id 派生（危险字符回落见 §3.1） |
| | `user_id` | string | 否 | 缺省 `anonymous` |
| `POST /sessions/{id}/messages` | `message` | string | **是** | 缺失 / null → 400「message 必填」；空串**允许**；UTF-8 字节 > 32768 → 400 |
| `POST /agents/{name}/invoke` | `message` | string | **是** | 同上；无会话可锁（无 409，只有 429） |

**解码规则**（全部端点一致）：请求体总量 ≤ `http.max_request_bytes`（缺省 1 MiB）；超限 → **400**（`*http.MaxBytesError` 被映射，不是 413）；未知字段忽略；decode 一次后必须到达 `io.EOF`，尾随内容 → 400；指针字段区分「缺失 / null」与「空串」。

### 2.3 视图（`internal/web/views.go`）

字段逐字取自 [docs/us/us9-web-service.md](../../docs/us/us9-web-service.md) §3.1 与 `openapi.yaml`；**只列不增**。

| 视图 | 字段（JSON 名） | 备注 |
| --- | --- | --- |
| `InfoView` | `version`, `uptime_seconds`, `agents`, `tools`, `mcp_servers`, `providers` | `agents` = 已注册数；`mcp_servers` = 真实连接数；**无凭证字段** |
| `ProviderView` | `name`, `models`, `configured` | 按配置顺序；`models` 只含 `default_model` |
| `ProfileView` | `name`, `description`, `provider`, `model`, `tools`, `skills`, `schedules`, `registered`, `problems` | 按装载顺序；`problems` = `RejectReason`（未注册时）；数组非 null |
| `ToolView` | `name`, `description`, `source` | `source ∈ {builtin, mcp, code}`；顺序 = `registry.Names()` |
| `MemoryView` | `scope`, `agent`, `backend`, `core`, `archive` | 全局层：`scope=global`、`agent=""`；空分区 → 空串 |
| `SessionSummaryView` | `session_id`, `profile_name`, `channel`, `user_id`, `status`, `message_count`, `created_at`, `last_active_at` | 时间字段 RFC3339 UTC |
| `SessionDetailView` | 摘要字段 + `messages` | `messages` 为最近 100 条、`seq` 升序、非 null |
| `MessageView` | `role`, `content`, `tool_calls`, `tool_call_id`, `created_at` | `content` 为空 → 空串；`tool_calls` 空 → `[]` |
| `ToolCallView` | `id`, `name`, `arguments` | `arguments` 是原始 JSON 字符串，不二次解析 |
| `RunView` | `run_id`, `session_id`, `agent`, `channel`, `trigger`, `status`, `reason`, `final_text`, `iterations`, `tool_calls`, `config_hash`, `started_at`, `finished_at` | `session_id` 在 invoke 为空串；`trigger=api`；`reason` 已经脱敏；时间为 RFC3339 UTC |

### 2.4 冻结文案（不得改写）

| 场景 | code | HTTP | message |
| --- | --- | --- | --- |
| 成功 | `OK` | 200 | `success` |
| 同会话在途 | `SESSION_BUSY` | 409 | 该会话有正在执行的任务，请等待完成或稍后重试 |
| 会话忙时归档 | `SESSION_BUSY` | 409 | 同上（逐字复用） |
| 实例并发超限 | `TOO_MANY_RUNS` | 429 | 点名当前上限（`max_concurrent_runs` 的实际生效值） |
| 达上限（已知 reason） | `LIMIT_REACHED` | 200 | 说明撞了哪条上限 + 可调配置键（`budget.max_iterations` / `budget.max_tool_calls`） |
| 达上限（未知 reason） | `LIMIT_REACHED` | 200 | 运行达到上限 |
| 超时 | `TIMEOUT` | 504 | 运行超过 60 秒时限 |
| 归档会话追加 | `BAD_REQUEST` | 400 | 会话已归档，不可追加消息 |
| 跨源拒绝 | `BAD_REQUEST` | 403 | 跨源请求被拒绝 |
| 服务器错误 | `INTERNAL` | 500 | 服务器内部错误 |

## 3. 派生与状态

### 3.1 会话标识派生（`core.DeriveSessionID`）

```text
输入: channel, user, profile
1. prefix = channel 若匹配 ^[a-z0-9][a-z0-9-]{0,31}$，否则固定 "ch"
2. 摘要输入 = "basalt/session/v1" 0x1F len(channel) channel 0x1F len(user) user 0x1F len(profile) profile
   （长度前缀消除分隔符歧义）
3. session_id = prefix + "-" + sha256(摘要输入) 的前 16 个 hex 字符
固定向量: ("cli","alice","demo") → cli-a2025c15b5cbc303
          ("web","anonymous","demo") → web-bb27c35eeb3cc34a
```

- 旧 CLI 行为逐字保留（固定向量不变）；`cli.deriveSessionID` 委托本函数（等价重构）。
- 危险 `channel`（含 `/`、`\n`、超长、大写之外的字符）回落 `ch`——不拒绝请求，只隔离标识输入。

### 3.2 会话状态机

```text
            POST /sessions（同元组重复）
                     │ 幂等：返回既有行，不改写
                     ▼
   ┌──────────┐   DELETE /sessions/{id}（无在途运行）   ┌────────────┐
   │  active  │ ─────────────────────────────────────► │  archived  │
   └──────────┘   只写 status + 首次 archived_at        └────────────┘
        │                                                  │
        │ 发消息（RUN）                                     │ 发消息 → 400
        ▼                                                  │ 归档 → 幂等 200（不覆盖首次时间）
   在途计数 +1 ──► 归档请求 → 409（零写入）                 │ 读历史 → 允许
   运行结束 ──► 计数 -1（归零删除表项）                      ▼
```

- **可读不可写**：归档会话 `GET` 正常返回；`POST /messages` → 400；`DELETE` 幂等 200。
- **不复活**：`GetOrCreateSession` 对已归档行不改 `status` / `created_at`；重复 `POST /sessions` 返回归档态。
- **不可逆**：本 US 无归档恢复（边界外）。

### 3.3 内存状态：在途运行计数

| 字段 | 类型 | 生命周期 | 语义 |
| --- | --- | --- | --- |
| `admissionMu` | `sync.Mutex` | 服务实例 | 串行化「状态复核 + 在途登记」与「归档裁决」 |
| `inflightRuns` | `map[string]int` | 服务实例 | 会话 id → 在途运行数；归零即删除（防无界增长） |
| `inflight` | `sync.WaitGroup` | 服务实例 | 在途 **HTTP 请求**数（停机阶段 ④ 的有界等待对象，与 `inflightRuns` 是两回事） |

**不变量**：`inflightRuns` 的每一次 `++` 都有配对的 `defer releaseRun`（成功 / 失败 / 取消 / 客户端断开全覆盖）；`archiveSession` 判空与 `ArchiveSession` 调用在同一锁内。

## 4. 校验规则汇总

1. **建会话**：`agent` 指针非 nil 且非空 → 查装载结果（`registeredAgent`）→ 未注册 / 未知 → 404，**零写入**；通过后 `GetOrCreateSession`，新建行 `Status` 显式 `active`。
2. **发消息**：`admitRun` 锁内 `GetSession` → 不存在 404 / `archived` → 400 / 在途计数 > 0 → 409；通过后运行；`releaseRun` 由 defer 保证。
3. **invoke**：`registeredAgent(name)` 不通过 → 404；**不**建会话、**不**查会话状态、无 409；`RunRequest.SessionID=""`、`UserID="anonymous"`、`Channel="web"`。
4. **归档**：`archiveSession` 锁内判在途 → 409；否则 `ArchiveSession`（`RowsAffected()==0` → `fs.ErrNotExist` → 404）；重复归档返回 200 且不改首次 `archived_at`。
5. **读历史**：`GetSession` 不存在 → 404；`MessagesTail(id, 100)` 后反转。
6. **消息长度**：`len(string) > 32768` → 400（字节数，运行前判定，零 Run 行）。
7. **请求体**：`MaxBytesReader` 上限 → 400；二次 decode 非 EOF → 400；必填指针 nil → 400。

## 5. 关键不变量（可复核）

| # | 不变量 | 复核方式 |
| --- | --- | --- |
| I1 | 会话标识确定性且唯一实现 | `core/sessionid_test.go` 固定向量；`grep -rn "basalt/session/v1" internal` 只命中 `core/sessionid.go` |
| I2 | 同一 `(channel,user,agent)` 只有一行 | `GetOrCreateSession` 的 `ON CONFLICT(session_id) DO NOTHING`；`storage/session_api_test.go` |
| I3 | 归档不删数据、不覆盖首次时间 | `ArchiveSession` 的 `COALESCE`；存储测试断言 `archived_at` 两次归档后不变 |
| I4 | 忙时归档零写入 | `handlers_test.go` 同步屏障用例：409 后 `status` / `archived_at` / 消息数不变 |
| I5 | 历史窗口 = 最近 100、升序 | `MessagesTail` 的 `ORDER BY seq DESC LIMIT ?` + 反转；边界用例 100 / 101 条 |
| I6 | 失败响应无 `data` | 契约测试断言失败 JSON 不含 `"data"` 键 |
| I7 | 对外无原始错误文本 | 脱敏夹具：`Reason` 含路径 / 片段时响应只出固定文案 |
| I8 | web 不写消息 / 审计 / Run | `grep -n 'AppendMessage\|INSERT INTO' internal/web/` 零命中 |
