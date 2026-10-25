# Data Model: US-14 MCP 目录与模板 + 五个每日业务 Demo + 发布收尾

**Date**: 2026-10-25 | **Plan**: [plan.md](./plan.md) | **Spec**: [spec.md](./spec.md)

字段与枚举的固定字面量以模块卡 §3.2（D-01–D-10）与 [docs/contracts/openapi.yaml](../../docs/contracts/openapi.yaml) 为准；本文件给出实体关系、校验规则与状态矩阵，不新增字段。

## 1. MCP 目录（只读静态集合）

### 1.1 实体

```text
MCPCatalog
├── version: int (>0)
└── items: []MCPCatalogEntry   # 按 id 升序
    ├── id: string             # kebab-case，全目录唯一
    ├── display_name: string
    ├── summary: string
    ├── category: string
    ├── transport: "stdio" | "http"
    ├── command: string        # http 时为空串
    ├── args: []string         # 可为空数组
    ├── url: string            # 绝对 http(s)；stdio 时为空串
    ├── env: []MCPCatalogEnv   # 可为空数组
    │   ├── key: string        # 唯一
    │   ├── placeholder: "${KEY}"
    │   ├── required: bool
    │   └── description: string
    ├── requires: []string     # 非空
    ├── source_url: string     # 必须 https://
    ├── verified: bool
    ├── verification_note: string
    ├── verified_at: string    # RFC3339；verified=false 时为空
    ├── verified_platforms: []string  # windows|linux|darwin；verified=false 时为空
    └── warnings: []string     # 目录固定 code 集合；可为空
```

### 1.2 校验规则（`internal/catalog.Load`）

| 规则 | 说明 |
| --- | --- |
| `version > 0` | 版本号用于未来 schema 演进；当前只接受 1 |
| `id` 唯一 + kebab-case | 重复或非法字符即解析失败 |
| `transport` 闭集 | 只允许 `stdio` / `http` |
| 传输互斥 | stdio 必须有 `command`；http 必须有绝对 `url` |
| `env[].placeholder` | 恰为 `${KEY}` 且 `KEY == env[].key` |
| `env[].key` 唯一 | 单条目内不得重复 |
| `source_url` | 必须 `https://` |
| `verified` 四字段 | `false` → `verification_note` 非空、`verified_at` 空、`verified_platforms` 空；`true` → 后三者非空 |
| `verified_platforms` 取值 | 只允许 `windows` / `linux` / `darwin`，**不得**写 `all` |
| `warnings` 闭集 | 只允许目录集合的七个 code（D-10） |
| 配方可装载 | `TestCatalog_RecipesParse` 把条目转成 `mcp_servers.yaml` 片段，经 `workspace.ParseMCPServerBlock` 通过 |

### 1.3 状态

目录是**编译期快照**：无运行时状态、无生命周期、无写入。`verified` 只表达"该精确配方是否经 14B 实测"，不是连接状态、不是第三方兼容认证（模块卡 §2.5.13）。

## 2. MCP 诊断（只读投影）

### 2.1 实体

```text
MCPServerDiagnosis
├── name: string
├── transport: string
├── published_version_id: int64
├── applied_version_id: int64
├── pending_restart: bool        # published != applied && published != 0
├── command: MCPCommandDiagnosis | null   # http 时 null
│   ├── state: "located" | "not_found" | "unconfirmed"
│   ├── path: string             # 仅 located 非空
│   └── reason: "lookpath" | "absolute" | "not_found" | "relative_path" | "script" | "permission" | "unknown"
├── env: []MCPEnvDiagnosis       # 按 key 升序
│   ├── key: string
│   └── set: bool                # 存在且非空为 true；不回值
├── warnings: []string           # 诊断固定 code 集合
└── checked_at: time.Time
```

### 2.2 命令状态与原因组合（Windows 解析语义）

| 入口形态 | state | reason | 判据 |
| --- | --- | --- | --- |
| 裸命令名且 PATH + PATHEXT 可定位 | `located` | `lookpath` | 能定位 ≠ 能直接启动（`.cmd` / `.bat` 另判） |
| 绝对路径且存在 | `located` | `absolute` | `os.Stat` 只读检查 |
| 裸命令名 / 绝对路径但不存在 | `not_found` | `not_found` | 构造返回明确"不存在" |
| 存在但无执行权限 | `not_found` | `permission` | 只读检查发现权限不足 |
| 含分隔符的相对路径 | `unconfirmed` | `relative_path` | 真实启动靠 `cmd.Dir` 解析，不启动无法完全证明 |
| `.cmd` / `.bat` | `unconfirmed` | `script` | 不默认 `cmd.exe /c`，不把 `where` 当启动证明 |
| 其他无法归类 | `unconfirmed` | `unknown` | 不得猜 |

`not_applicable` 只用于 http 传输的内部映射（`command=null`），不出现 JSON。

### 2.3 不变量

- 诊断**不**启动进程、**不**连接、**不**写操作行、**不**改 applied / published；
- `set` 与 `resolveMCPEnv` 同判据：未设置与空字符串都为 `false`；
- `unconfirmed` 只说明"不启动无法证明"，**不**预判测试失败。

## 3. MCP 测试记录与操作详情

### 3.1 实体

```text
ResourceTestResult（core；既有 + 追加四字段）
├── OK: bool
├── LatencyMS: int64
├── Model: string                 # 模型连接测试用；MCP 为空
├── Tools: []string               # 始终存在；失败时可为空数组
├── ErrorCode / ErrorMessage: string
├── OperationID: string           # 新增；管理操作标识
├── TestedVersionID: int64        # 新增；0 = 无法确定（历史 / 旧记录）
├── Stage: string                 # 新增；preflight|spawn|initialize|tools_list|complete
└── FailureClass: string          # 新增；成功为空串

OperationDetail.detail（mcp_server.test；脱敏）
├── ok / latency_ms
├── tools: []string               # 旧记录可能缺失
├── error_code / error_message
├── stage                        # 旧记录可能缺失
└── failure_class                # 旧记录可能缺失
# requested_version_id 是权威被测版本；detail 不重复版本字段
```

### 3.2 `stage` / `failure_class` 枚举（闭集）

- `stage`：`preflight` / `spawn` / `initialize` / `tools_list` / `complete`
- `failure_class`：`command_not_found` / `permission_denied` / `policy_denied` / `timeout` / `http_unauthorized` / `http_unreachable` / `handshake_failed` / `tools_list_failed` / `env_missing` / `dependency_unconfirmed`；成功为空串
- 未知错误 → `dependency_unconfirmed` + `stage` 尽力而为；**不解析 stderr / 日志**。

### 3.3 操作状态矩阵（不得统一改成 `ok=false → success`）

| 场景 | `management_operations.status` | `result_code` | `detail.ok` | `detail.stage` | `detail.failure_class` | HTTP |
| --- | --- | --- | --- | --- | --- | --- |
| 连接 + `tools/list` 成功 | `success` | `OK` | true | `complete` | 空 | 200 |
| 策略拒绝 | `success` | `OK` | false | `preflight` | `policy_denied` | 200 |
| 启动程序不存在 | `success` | `OK` | false | `spawn` | `command_not_found` | 200 |
| 权限不足 | `success` | `OK` | false | `spawn` | `permission_denied` | 200 |
| 启动失败无法归因 | `success` | `OK` | false | `spawn` | `dependency_unconfirmed` | 200 |
| 握手失败 | `success` | `OK` | false | `initialize` | `handshake_failed` | 200 |
| HTTP 401 / 403 | `success` | `OK` | false | `initialize` | `http_unauthorized` | 200 |
| HTTP 不可达 | `success` | `OK` | false | `initialize` | `http_unreachable` | 200 |
| `tools/list` 失败 | `success` | `OK` | false | `tools_list` | `tools_list_failed` | 200 |
| 超时 | `failed` | `TIMEOUT` | false | `spawn` / `initialize` / `tools_list` | `timeout` | 504 |
| env 缺失 / 空值 | `failed` | `NOT_CONFIGURED` | false | `preflight` | `env_missing` | 503 |
| 预写操作失败 | 无行 | — | — | — | — | 500 |

**判定**：`status=success` 只表示管理动作执行完毕并留下结论；测试通过只看 `detail.ok` + 版本相等（`requested_version_id == published_version_id != 0`）。

### 3.4 四轴与页面判据

| 轴 | 取值 | 数据源 |
| --- | --- | --- |
| 配置 | `not_configured` / `published_not_applied` / `applied` | 资源指针 |
| 连接 | `connected` / `disconnected` / `not_applicable` | supervisor 快照 |
| 测试 | `untested` / `passed` / `failed` / `stale` / `legacy` / `unknown` | 最近一次 `mcp_server.test` + 详情 |
| 诊断 | `command_state` + `env[].set` | `NewMCPCommand` + `os.LookupEnv` |

- `pending_restart` 是配置轴派生布尔，不是第五轴；
- 版本不等 → `stale`；`tested_version_id=0` → `legacy`（显示"历史记录字段不全，需重新测试"）；详情拉取失败 → `unknown`；
- 空工具清单 → `passed` + 页面提示"服务端未暴露工具"。

## 4. 工具来源（装配层投影）

```text
MCPToolDisposition
├── offered: int                  # = len(McpServerView.tools)
├── accepted: int
└── skipped: []MCPToolSkip
    ├── name: string
    ├── winner_source: string     # "builtin" | 具体 server 名 | "unknown"
    └── reason: string            # "duplicate_name" | "invalid_tool"

# 不变量：accepted + len(skipped) == offered；未连接 / 未知时整个对象为 null
# McpServerView.tools 保持 offered 语义；注册池重名 WARN 与拒绝语义不变
```

## 5. 五个每日 Demo 的证据模型

### 5.1 六段证据（每个 Demo 必填）

| # | 段 | 内容 |
| --- | --- | --- |
| 1 | 输入快照哈希 | `fixtures/**` 的 SHA-256，与运行时读取一致 |
| 2 | 时区与计划 | 6 段 cron + `zone`（`Asia/Shanghai`），取自 `AGENT.md` 实际值 |
| 3 | 候选产物哈希 | `report.md` / `report.json`（或对应产物）的 SHA-256 |
| 4 | 运行标识 | `run_id` + `task_executions.trigger`（`auto` / `manual` 分开） |
| 5 | 业务断言 | 字段 / 计数 / 集合（见 5.2） |
| 6 | 通知与故障读数 | 通知次数 / `schedule_deliveries` 终态 / 依赖失败结论 |

### 5.2 每个 Demo 的业务断言

| Demo | 必须断言 | 通知 / 投递 |
| --- | --- | --- |
| D1 `github-radar` | `observed_at`、`baseline_date`、候选数 / 去重数 / 裁剪数、每项来源 URL；首跑 `baseline_missing=true` 且无星差；第二次去重并算出差值；空结果写"本次无新增" | 模型 `notify` 一次；接收器确认送达 |
| D2 `repo-digest` | PR / Issue / Release 计数与待跟进项；缺字段标"待确认"；游标只前进到已处理项；技能 L1 卡片进入 prompt、L2/L3 经 `read_file` 实际读取 | `deliver_to` 系统代投；成功 Run 后 `sent`（`failed` / `unknown` 不算通过） |
| D3 `biz-daily` | 缺失 / 重复 / 非法值三类逐类计数且可复算；截断标记；DBHub 实际调用 | **零通知** |
| D4 `docs-diff` | 新增 / 修改 / 删除三集合；每份文档内容哈希（未变一致、变更不同）；首跑"无历史基线"；无变化也出报告；零历史会话仍读记忆 | 模型 `notify` 一次；接收器确认送达 |
| D5 `industry-brief` | Brave 与 Fetch 均被调用；来源表含 URL / 标题 / 日期 / 访问时间；未抓正文的 URL 只作线索；结论逐条可追溯到 Fetch 正文 | `deliver_to` 系统代投；`sent` |

### 5.3 投递状态

| 场景 | `schedule_deliveries` 终态 |
| --- | --- |
| 成功 Run + 实际发送成功 | `sent` |
| 成功 Run + 发送失败 / 回执未知 | `failed` / `unknown` |
| 取消 / 失败 / 超时的 Run | `skipped`（按规则未发送） |
| 恢复路径无法判定 | `unknown` |

**不自动补发**；人工用保存的报告补跑时必须注明原 `run_id`。

## 6. 八个模板

```text
Template
├── name: string
├── tier: "A" | "A*" | "B"     # 固定，不自动降档
├── agent_dir: examples/templates/<name>/
├── tools.builtin: []string    # 必须可解析到已注册内置工具
├── tools.mcp: []string        # ⊆ 对应 server tools/list（允许子集）
├── fixtures: path            # 固定输入
└── evidence: string          # "可复跑（夹具）" | "未验证" | "依赖未就绪（A*）"
```

| 档位 | 通过判据 | 不通过时 |
| --- | --- | --- |
| A（六个） | 可装载；内置工具可解析；MCP 工具 ⊆ `tools/list`；配置 / 技能 / 脚本 / 输入 / 说明齐全；夹具可复跑；有业务断言 | 修复；未修复则 14B 不完成；**不得**降 B |
| A\*（`web-watch`） | 浏览器就绪 → 按 A 判；未就绪 → 保持 A\*（档位不变） | 不得先写 A 后补，不得改 B |
| B（`funding-watch`） | 可装载；配方 + 依赖 / 凭据 / 安装 / 生效说明 + 边界 | 不得写"可运行 / 已验证" |

## 7. 发布证据

```text
ReleaseEvidence
├── version: "v0.1.0"                # tag 由用户创建；证据不代打
├── commit: <SHA>
├── binary_path: <绝对路径>
├── binary_sha256: <hex>             # 与分发文件逐字节一致
├── binary_source: "candidate"
├── acceptance: pass | fail          # 干净检出 make acceptance
├── e2e: {scenario: pass | fail}     # 七场景逐项
├── platforms: {windows, linux}      # 均必验；缺一 = 未完成
├── dependencies: [go|node|python|mcp]  # 版本 + 许可 + 来源
└── upgrade_drill: pass | fail       # 按 Operations §5
```

七场景：工具闭环 / 越权 / 取消 + 工具超时 / 崩溃恢复 / 版本冲突（409）/ 热加载 / Web·调度·通知。取消与工具超时**分两个用例**：取消 → Run `cancelled` + 无子进程残留 + 会话可继续；超时 → 工具审计 `failed` + `timeout` + Run `completed` + 失败结果回填。

## 8. 不变量

1. **零迁移**：不新增表 / 列 / 索引；`schema.sql` 哈希不变。
2. **管理操作只追加**：行不改写；`RequestedVersionID` 在 `BeginOperation` 前设置；`detail_json` 脱敏。
3. **资源版本只追加**：闸门失败不建版本；发布失败不推进指针。
4. **指针语义不变**：`published != applied` 即待重启；`published == 0` 时 `pending_restart=false`。
5. **目录只读**：请求期零写、零进程、零安装、零发布。
6. **诊断只读**：零操作行、零进程、零连接。
7. **凭据**：`${VAR}` 占位；错误只点名；日志 / 响应 / 示例 / 文档零明文。
8. **示例**：无实例 UID / 凭据；`state/` 不提交；不改共享技能、不扩大白名单。
