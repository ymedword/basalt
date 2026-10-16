# Phase 1 Data Model: US-5 Shell 与 HTTP 工具

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-17

**总览**：本 feature **零迁移、零列变化**（判据：`git diff docs/contracts/schema.sql` 为空）。数据面变化全部在**内存实体**：`core` 增两类错误承载 + `Command` 增一个字段，`tool` 增两件工具的输入结构与请求规格。下表的既有实体形态**已逐字对源核实**（`internal/core/{action.go,tool.go,run.go,prompt.go}`，2026-10-17 本轮读取）。

## 1. 既有实体（只读引用，本 feature 的改动面以标注为准）

### 1.1 `core.Action` — Policy 的输入（[internal/core/action.go](../../internal/core/action.go)，**零改动**）

| 字段 | 类型 | 约束 |
| --- | --- | --- |
| `Type` | `ActionType` | 四类字面量；本 US 消费 `ActionShellCommand`（`"SHELL_COMMAND"`）与 `ActionHTTPRequest`（`"HTTP_REQUEST"`）——两者**已在案**（action.go:29-31 注释点明"逐跳校验由调用方在每一跳发送前再次 Check（US-5 驱动）"） |
| `Target` | `string` | 判据主体：命令档 = executable **整串**（不看 argv）；出网档 = **本跳完整 URL**（含显式端口，不得改写） |
| `Agent` | `string` | 发起方 Agent 名；空 = 无 Agent 上下文（**不是异常**） |

**无 argv 字段是设计**：命令行的形态校验属执行侧（`LocalRunner`）——工具**不得**为"帮策略判"而拆分或重写 Target。

### 1.2 `core.Command` — CommandRunner 的输入（[internal/core/action.go](../../internal/core/action.go)，**本 US 增 1 字段**）

| 字段 | 类型 | 现状 | 本 US |
| --- | --- | --- | --- |
| `Executable` | `string` | 在白名单/解释器名单中逐字比对的对象（多词串必拒） | 不变 |
| `Args` | `[]string` | argv 原样传递，不经任何 shell | 不变 |
| `Dir` | `string` | 工作目录；空 = 进程默认 CWD（**不引入全局 chdir**） | 不变（`shell` 工具在目录存在时填 `env.AgentDir`） |
| `Env` | `[]string` | 附加变量；透传白名单在 Runner 侧统一执行 | 不变（`shell` 工具恒传 `nil`，变量白名单由执行器配置面承担） |
| `MaxOutputBytes` | `int` | — | **新增**：本次执行读取侧上限；`<=0` = 用装配值；**只许更短**（FR-023 的传导体） |

### 1.3 `core.CommandResult`（**零改动**）

`{Output string, ExitCode int, Truncated bool}`——`Truncated` 只表达"达到生效输出上限"，**不承诺**原始长度。`shell` 工具对非零 `ExitCode` **不构成错误**（首行「退出码：N」进 Content）。

### 1.4 `core.Tool` 接口与 `core.Result`（[internal/core/tool.go](../../internal/core/tool.go)，**零改动**）

- `Tool`：`Name() / Description() / InputSchema() json.RawMessage / Execute(ctx, env, input) (Result, error)`——两件新工具实现它，签名不变；
- `Result` **只有两个字段** `{Content string, Retryable bool}`：截断标记走 `Content` 尾部（`TruncationMarker`），**不进结构**——工具**不得**自造第三字段或自行截断（FR-007：会出现两个标记）。

### 1.5 `core.ToolEnv`（[internal/core/run.go:112-119](../../internal/core/run.go#L112-L119)，**零改动**）

| 字段 | 两件新工具的消费方式 |
| --- | --- |
| `RunID` / `SessionID` | 只作审计关联（工具不写审计——FR-024） |
| `AgentName` | 空 = 无上下文；`shell` 据此决定命令档的 `Action.Agent` 与日志形态无关（工具不记日志） |
| `AgentDir` | `shell` 的 `Dir` 来源（**目录存在才填**，否则留空走进程默认——FR-004） |
| `Timeout` | `shell`：`narrowTimeout` 的上限基准；HTTP：**没有**工具参数可缩，由执行器统一承担（FR-008 明禁 timeout 参数） |
| `MaxOutputBytes` | HTTP：响应读取上限 = **+1 字节**（让执行器截断判据必定命中，D-11）；`shell`：原样传给 `Command.MaxOutputBytes` |

### 1.6 `core.ToolSpec`（[internal/core/prompt.go:51-55](../../internal/core/prompt.go#L51-L55)，**零改动**）

`{Name, Description, Parameters json.RawMessage}`——`tool list` 渲染与出站编码共用；`Parameters` **原样透传**（不解析、不重序列化）。清单顺序由实现侧给出，命令面**不得**重排（FR-021）。

## 2. 新增实体

### 2.1 `core.ExternalError` — 外部系统错误的承载（`internal/core/errors.go` 新增）

| 字段 | 类型 | 约束 |
| --- | --- | --- |
| `Target` | `string` | **只放 host**（不含 path / query / userinfo）——`url.Error` 原文含整条 URL，**不得**回显（FR-016） |
| `Reason` | `string` | **自产类别摘要**（如"DNS 解析失败"/"连接失败"/"TLS 握手失败"/"响应读取失败"/`HTTP <status>`） |
| `Cause` | `error` | 原始错误，经 `Unwrap()` 保留——**只在内层**，`Error()` 文案不得拼接它 |

- 配套判据：`IsExternalError(err) bool`（`errors.As` 形态）——**判类型不比文本**；
- 与既有 `SandboxViolationError` **平级**（不嵌不继承）；**不复用** `provider.CallError`（`core` 只依赖标准库，反向 import 禁）。

### 2.2 `core.InvalidInputError` — 入参非法的承载（`internal/core/errors.go` 新增）

| 字段 | 类型 | 约束 |
| --- | --- | --- |
| `Tool` | `string` | 点名工具（`shell` / `http_get` / `http_post` / 既有三件文件工具） |
| `Reason` | `string` | 自产摘要（缺必填键 / 字段类型错 / 取值越界 / 出站 body 超限） |

- 配套判据：`IsInvalidInput(err) bool`；
- **两个产出方产出同一类别**（FR-019）：执行器入口判「合法 JSON 对象 + 必填键」；工具在解码点判「字段类型与取值」；既有三件文件工具的解码 / 取值错误**同批换载体**（**文案不变**——D-05）。

### 2.3 `ShellToolInput` — `shell` 的入参结构（`internal/tool/shelltools.go` 新增，包内）

| 字段 | 类型 | 必填 | 约束 |
| --- | --- | --- | --- |
| `executable` | `string` | ✅ | **整串**送判定，不可为空；**不得**本地拆分（拆 `bash -c` 即绕过——FR-001） |
| `arguments` | `[]string` | 否 | 逐项原样进 `Command.Args`，不做任何解释 |
| `timeout_seconds` | `int` | 否 | **只缩短**：`narrowTimeout(ctx, env.Timeout, 请求值)`；非正值按缺省、**不报错**（FR-003，四态：缺省 / 更短 / 更长不生效 / 非正值按缺省） |

### 2.4 `HTTPToolInput` — `http_get` / `http_post` 的入参结构（`internal/tool/httptools.go` 新增，包内）

| 字段 | `http_get` | `http_post` | 约束 |
| --- | --- | --- | --- |
| `url` | ✅ 必填 | ✅ 必填 | 送判定前**不得**改写；每跳以本跳 URL 重新判定 |
| `body` | ❌ 无此参数 | ✅ 必填 | 按 UTF-8 字节计 ≤ 256 KiB（`maxRequestBodyBytes`），超限 → `invalid_input`、**零发送**（D-09） |
| `content_type` | ❌ 无此参数 | 否，缺省 `application/json` | 仅作请求头，不进判定 |
| `timeout` / 类似参数 | ❌ **两件都不得出现**（FR-008） | 同左 | 超时由执行侧统一承担 |

### 2.5 `httpRequestSpec` — 跨跳请求规格（`internal/tool/httptools.go` 新增，包内）

承载"当前这一跳"的方法 / URL / 请求头 / body，唯一变更路径为 `next(status, location)`：

| 状态码 | 方法 | body | 请求头 |
| --- | --- | --- | --- |
| 301 / 302 / 303 | 转 `GET` | **丢弃** | 清正文相关头（`Content-Type` / `Content-Length` 等） |
| 307 / 308 | **保持原方法** | **保持 body** | 保留 |
| 304 / 其他 3xx | 不跳转——按普通响应返回（< 400 走正常内容路径） | — | — |

跨跳头规则：模型提供的头**只随首跳**；仅当目标与当前跳**同源**（scheme + host + **端口**全同）才转发；跨源一律不转发（FR-013）。跳数上限 `maxRedirectHops = 5`（**初始请求不计**；超限 → `sandbox_denied`，**不是** `external_error`——FR-012）。

## 3. 失败分类（七值**冻结不变**，本 US 只新增产出方与两条分支）

| 字面量 | 语义 | 本 US 的产出方 |
| --- | --- | --- |
| `sandbox_denied` | 沙箱拒绝 | 策略拒绝（域名 / 命令 / 端口 / 跨域跳转）；**跳数超限** |
| `invalid_input` | 入参非法 | 执行器入口（JSON / 必填键）+ 工具解码点 + **出站 body 超限**（新分支） |
| `timeout` | 取消 / 截止 | ctx 原样上抛（**MUST NOT** 被外部系统分支截胡——D-12 先查 `ctx.Err()`） |
| `external_error` | 外部系统错误 | 4xx / 5xx + DNS / 连接 / TLS / 读取失败（**新分支**，`tool_invocations` 上首次出现） |
| `not_found` | 目标不存在 | 既有（文件工具）；本 US 无新增产出方 |
| `runtime_error` | 兜底 | 既有 |
| `unknown` | 崩溃恢复的 pending 行 | 既有（恢复路径） |

**判序（流程 C，逐字）**：沙箱 → 入参非法 → 取消/截止 → 外部系统 → 文件不存在 → 兜底。

## 4. 落库面（**零改动**，列级口径逐条重申）

| 表 | 本 US 的关系 |
| --- | --- |
| `tool_invocations` | **无 `truncated` / 无 `round` 列**（截断标记与 `truncated` 都在 `result_json` 值内）；`input_json` = 模型给的 arguments **原样**（`headers` / `body` 因此也按契约在库内——敏感面如实边界：**不得**笼统承诺"凭证不落库"）；`error_kind` 首次可被工具侧写 `external_error` / `invalid_input`；`status` 两阶段：`pending` 先写（fail-closed）→ 终态回填 |
| `llm_calls` | 零改动（本 US 不碰模型调用面） |
| `runs` / `config_snapshots` / 其余表 | 零改动 |
| `schema_version` / 迁移 | **零迁移脚本**（判据：迁移目录无新文件、`git diff schema.sql` 为空） |

审计两阶段写与分类落库的全部逻辑仍只在 `core.ToolExecutor`（工具不写审计、不写消息——FR-023 / FR-024）。**本 US 的拒绝 / 超限路径同样先有 `pending` 行**：出站 body 超限的精确语义 =「不发出去 + 该行终态 `failed` / `invalid_input`」，**不是**"库里零痕迹"。

## 5. 本 feature 的内存态不变量（implement 期逐条自查）

1. **逐跳判定唯一处**：scheme / host / 端口 / 域名只在 `checkHTTPRequest` 判；工具无本地预判、无目标改写；
2. **先判后发**：每一跳的 `policy.Check` 在**发送之前**；拒绝的那一跳**零发送**（判定与发送两侧记账可核）；
3. **端口冻结**：`http` 省略/`:80`、`https` 省略/`:443` 放行；交叉档与非默认档拒、指引固定「当前版本不支持非默认端口」；
4. **预算只收窄**：`Command.MaxOutputBytes <= 0` 视为未设；`effectiveMaxOutputBytes = min(装配值, 逐命令正值, 256 KB)`，两者皆非正 → 256 KB；读取上限 = 生效值 **+ 1 字节**；
5. **超时不放大**：`shell.timeout_seconds` 生效值 ≤ `env.Timeout`；HTTP 两工具无 timeout 参数；
6. **分类判据是类型不是文本**：`IsExternalError` / `IsInvalidInput` / `IsSandboxViolation` 一律 `errors.As`；
7. **工具不记日志、不写审计、不写消息**；错误不吞（`err` 必回填或上抛，无 `_ =`）；
8. **context 只承载取消 / 截止 / 追踪**：本 US 新增文件 `grep context.WithValue` 零命中。
