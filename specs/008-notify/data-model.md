# Phase 1 Data Model: US-8 通知工具（notify）

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Research**: [research.md](./research.md) | **Date**: 2026-10-18

**范围**：本 US 涉及四类数据面——① 核心值对象 `NotifyChannel`（渠道）；② 既有存储表 `notify_channels`（只读）；③ 装配期配置 `type → adapter` 映射；④ 工具边界入参 / 飞书回执两个协议值对象。另有审计行沿用既有 `tool_invocations`（Schema 零改动，只登记映射关系）。**零迁移、零新增表 / 列 / 索引、零新增配置键。**

## 1. `NotifyChannel`（核心值对象）

**定义位置**：`internal/core/notify.go`（纯类型，零 import 内部包）。

| 字段 | 类型 | 来源列 | 约束 |
| --- | --- | --- | --- |
| `Name` | `string` | `notify_channels.name`（主键） | 非空；作为 `channel` 入参的匹配对象；大小写敏感精确匹配；直接用于成功文案 |
| `Type` | `string` | `notify_channels.type` | 必须命中装配映射的键（`webhook` / `feishu`），否则 `runtime_error`；工具不枚举合法值 |
| `URL` | `string` | `notify_channels.url` | **必须**是精确 `${VAR}` 引用形态（见 §1.1）；**不得**在任何对外文案中出现原值 |

**不进入核心值对象的列**：`description`、`created_at`——本 US 不读（`SELECT` 只取三列，见 §2）。

### 1.1 `URL` 形态校验（`core.EnvPlaceholderName`）

| 输入形态 | 判定 | 证据级理由 |
| --- | --- | --- |
| `${BASALT_NOTIFY_OPS}` | 接受，返回 `("BASALT_NOTIFY_OPS", true)` | 精确整串引用 |
| `${A}`、`${_x1}` | 接受 | 标识符 `[A-Za-z_][A-Za-z0-9_]*` |
| `https://example.com/hook`（明文） | 拒绝 | 凭证明文落盘 |
| `${A}${B}`、`${A}x`、`x${A}` | 拒绝 | 只接受**整串**精确引用（人工 review ① 裁决） |
| `$A`、`${}`、`${1A}`、`${A-B}` | 拒绝 | 非合法标识符 / 非合法形态 |
| 空串 | 拒绝 | 无引用即无地址 |

**调用点**：`tool`（渠道 `url` 判定）、`workspace`（MCP `env` 值判定）、`cli`（配置凭证键判定）——三处共用同一函数，**不得**复制第二份。拒绝时的错误文案**不得**回显 `url` 原值。

## 2. `notify_channels`（既有存储表，本 US 只读）

**事实源**：[internal/storage/schema.sql:216-222](../../internal/storage/schema.sql#L216-L222) 与 [docs/contracts/schema.sql:216-222](../../docs/contracts/schema.sql#L216-L222)（逐字相同，本轮已核）。

```text
notify_channels
├── name        TEXT PRIMARY KEY     —— 渠道名（大小写敏感；入参匹配对象）
├── type        TEXT NOT NULL        —— 适配器类型（webhook / feishu，运行时由装配映射裁决）
├── url         TEXT NOT NULL        —— ${ENV_VAR} 引用（发送时解析；对外只出 scheme://host）
├── description TEXT                 —— 本 US 不读（管理台展示归 US-12）
└── created_at  INTEGER NOT NULL     —— 本 US 不读
```

**读取契约**：`ListNotifyChannels(ctx) ([]NotifyChannel, error)`——每次发送调用一次，`SELECT name, type, url FROM notify_channels ORDER BY name`，走只读连接池；空表返回空切片（错误文案须能表达「注册表为空」）。**零缓存**：US-12 / 手工 SQL 新增的行下一发即可见。

**写入边界**：本 US **零写入**。渠道登记在演示 / 人工验收时由具权限的人手工 SQL 完成（§6.1 第 4 项）；增删改查归 US-12。

**失败语义**：读取失败包装为 `runtime_error`（文案前缀「读取通知渠道注册表失败」，不回显 SQL / 底层错误细节以外的内容；底层错误经 `%w` 保留供排障）。

## 3. `type → adapter` 映射（装配期配置）

**定义位置**：`internal/app/notify.go` 的 `newNotifyAdapters()`——全仓唯一真值。

| 键 | 值 | 成功判据 | 载荷 |
| --- | --- | --- | --- |
| `webhook` | `*tool.WebhookNotifyAdapter` | HTTP 2xx（不读响应体） | `{"content":"…"}` |
| `feishu` | `*tool.FeishuNotifyAdapter` | HTTP 2xx 且回执 `code == 0` | `{"msg_type":"text","content":{"text":"…"}}` |

**不变量**：

- 映射**恰好两键**；新增类型只改此处（`tool` 内核零改动）。
- 工具只做 `adapters[ch.Type]` 查表；查不到 → 专用错误 `*notifyTypeUnregisteredError`（归 `runtime_error`），**零环境查询、零策略判定、零外呼**。
- 映射在构造 `NotifyTool` 时**克隆**一份，调用方持有原 map 不影响工具行为。

## 4. `notify` 入参（工具边界值对象）

**Schema**（固定字面量，见 `docs/contracts/agent-md.md` §5.5）：`required` 恰好 `["content"]`；`channel` 可选。

| 字段 | 类型 | 必填 | 解码与校验 |
| --- | --- | --- | --- |
| `channel` | `string` | 否 | JSON 类型错误 → `invalid_input`；`TrimSpace` 后为空 → 视为未提供；精确大小写匹配渠道名 |
| `content` | `*string` | **是** | 缺失由执行器 schema 必填校验在工具前拦截；`null` / 非字符串 → `invalid_input`；UTF-8 字节数 ≤ 20 × 1024（= 20480）；**空串放行**（人工 review ① 裁决） |

**解码规则**：整段 `json.RawMessage` 必须是一个 JSON 对象；未知字段容忍（与既有工具一致）；重复键按标准库后者覆盖（不在文档外增判据）。

**上限判定顺序**：`content` 20 KiB 检查在**读注册表之前**——超限请求不触达存储层，也不产生任何外呼。

## 5. 调用状态机

**唯一事实源**：颗粒度文档 §2.3 图 A 与伪代码；本图是同一链路的数据视角。

```text
Execute(ctx, env, raw)
│
├─ ① decodeNotifyInput(raw)                  失败 → invalid_input（零副作用）
├─ ② len([]byte(*in.Content)) > 20*1024      失败 → invalid_input（零查表 / 零外呼）
├─ ③ channels.ListNotifyChannels(ctx)        失败 → runtime_error（零外呼）
├─ ④ TrimSpace(channel) → 精确匹配 Name      缺失 / 空白 / 查无 → not_found + 可用渠道名
├─ ⑤ adapters[ch.Type]                       未注册 → runtime_error（零 env / 零 policy / 零外呼）
├─ ⑥ EnvPlaceholderName(ch.URL)              非精确引用 → runtime_error（零外呼，不回显 url）
├─ ⑦ lookupEnv(name)                         未设置 / 空串 → runtime_error（点名 ${VAR}，零外呼）
├─ ⑧ policy.Check(ctx, Action{HTTP, 真值 URL, env.AgentName})
│     ├─ 沙箱拒绝 → sandbox_denied（Rule / Guidance 原文；Target 重写为 scheme://host）
│     └─ 其他错误 → 原样上抛（执行器分类；不回显 url）
└─ ⑨ adapter.Send(ctx, 真值 URL, *in.Content)
      ├─ 成功 → Result{"已推送到通知渠道：" + ch.Name, Retryable: false}
      ├─ ctx 取消 / 截止 → 裸哨兵（→ timeout）
      └─ 其余失败 → *ExternalError（→ external_error；文案只出 scheme://host）
```

**状态不变量**：

- ①~⑧ 任一失败 ⇒ **发送调用次数 = 0**（R2 / R10 断言点）。
- ⑦ 之前**不得**把引用串当 URL 交给策略判定；⑧ 的判定对象**必须是**解析后的真实 URL。
- 每次 `Execute` 至多一次注册表读取、至多一次发送；无重试、无队列。
- 工具内不落审计、不写消息、不记日志；审计状态由执行器在调用前后维护（§7）。

## 6. 飞书回执（协议值对象）

**定义位置**：`internal/tool/notify_adapters.go` 的 `feishuNotifyReceipt{Code *int}`。

| 回执形态 | 解析结果 | 判定 |
| --- | --- | --- |
| `{"StatusCode":0,"code":0,"msg":"success","data":{}}` | `Code = 0` | 成功（额外字段忽略） |
| 空体 / 全空白 | 不可解析 | 失败「飞书回执无法解析」 |
| 非 JSON（HTML、纯文本） | 不可解析 | 同上 |
| `{}` 或缺 `code` 字段 | `Code = nil` | 同上 |
| `{"code":null}` | `Code = nil` | 同上 |
| `{"code":1.5}` / `{"code":"0"}` | 类型错误 | 同上 |
| `{"code":0} trailing` | 尾随垃圾 | 同上 |
| `{"code":19024}` | `Code = 19024` | `external_error`，文案含 `code=19024`，**不含**回执正文 |
| 回执 > 64 × 1024 字节（多读 1 字节判出） | 不进入解析 | 失败「飞书回执无法解析」 |

**读取纪律**：`io.LimitReader(body, 64*1024+1)`，`len > 64*1024` 即超限；**不得**截断后解析。成功与失败分支都关闭 `resp.Body`。

## 7. 审计行（复用既有 `tool_invocations`，Schema 零改动）

| 调用结果 | 审计 `status` | `error_kind` | 备注 |
| --- | --- | --- | --- |
| 调用前（预写） | `pending` | — | 预写失败 → **拒绝发出**（fail-closed，既有执行器保证） |
| 成功 | `success` | — | 工具结果 = 固定成功文案 |
| 入参 / 超限 | `error` | `invalid_input` | |
| 渠道缺失 / 未知 | `error` | `not_found` | |
| 策略拒绝 | `error` | `sandbox_denied` | |
| 传输 / 非 2xx / 回执失败 | `error` | `external_error` | |
| 注册表 / 环境 / 未知类型 / 装配 | `error` | `runtime_error` | |
| ctx 取消 / 截止 | `error` | `timeout` | |
| 执行器兜底（未被工具显式分类的错误） | `error` | `runtime_error` | 契约 §7.3 无 `internal_error`；`unknown` 只由崩溃恢复写入半截 `pending` 行 |

**不变量**：一次调用**一行**，先 `pending` 后终态；`error_message` 沿用脱敏后的工具错误文本（只含 `scheme://host` / 变量名）；写入路径唯一在 `storage`，本 US 零改动。

## 8. 不变量汇总（供独立复核对号）

1. `notify_channels` 只读、零迁移、零缓存、每次发送读一次、`ORDER BY name`。
2. `url` 只接受精确 `${VAR}`；解析只在发送时；未设置 / 空串 fail-closed。
3. 类型支持集恰好 `{webhook, feishu}`，唯一真值在装配层；未知类型零副作用。
4. `content` 上限 20 × 1024 字节且在查表前判定；空串放行。
5. 成功文案固定 `已推送到通知渠道：<渠道名>`；`Retryable = false`。
6. 对外只出 `scheme://host`；`Rule` / `Guidance` 原文保留。
7. 工具与适配器零审计 / 零消息 / 零日志；两阶段写由执行器独担。
8. 飞书只认 `code == 0`；不可解析与超限一律失败；webhook 只认 2xx。
