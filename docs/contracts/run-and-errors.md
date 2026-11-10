# Run 与错误契约

> 本文档是 Basalt 运行时契约的**执行与失败语义**篇，规定一次 Agent 运行的边界、状态迁移、并发与取消行为、崩溃恢复语义与错误码表。它是[技术方案](../TechnicalDesign.md) §2（ReAct 循环）、§6（Web Service）的**契约细化**，两者冲突时以技术方案为准。
>
> 契约中的方法名、字段名、状态字面量、错误码是**模型与外部系统可见的固定字面量**，实现必须逐字保真。
>
> **文档状态**：待验证设计草案（冻结条件：五链路原型验证通过 + 真实模型调用往返，见 [DevelopmentLoop.md](../DevelopmentLoop.md) §5.2）。

---

## 1. Run：执行与审计的单位

**Run 是一次 Agent 运行的最小完整单位**：从收到一条消息开始，到产生最终结果（或达到终止条件）为止，包括其中全部 LLM 调用轮次与工具执行。

所有触发源（CLI / REST / 定时器 / 管理台手动补跑）都归约为一次 Run：

- 会话消息、工具执行、LLM 调用、审计记录全部挂在 `run_id` 下，回答"这一次运行里发生了什么"
- 审计的关联单位是 Run，不是 Session——同一会话有多次运行，必须能区分

### 1.1 接口

```go
// RunRequest 承载一次运行的全部输入（显式类型化参数，不放在 context 里）。
type RunRequest struct {
    Agent      string   // Agent 名（Profile 解析的键）
    SessionID  string   // 会话标识；无状态调用时为空
    Message    string   // 用户消息
    Channel    string   // 来源渠道：cli / web / scheduler / admin
    UserID     string   // 用户标识
    Budget     Budget   // 执行预算（见 §6）
    ConfigHash string   // 由运行时填充的配置快照版本（调用方不填）
}

// RunResult 是运行的完整结果。无论成功失败，Run 都返回 RunResult，error 只表达"运行器本身无法工作"。
type RunResult struct {
    RunID      string    // 本次运行标识
    Status     RunStatus // 终止状态（见 §2）
    FinalText  string    // 最终回答文本；limit_reached / cancelled 时可能为空
    Iterations int       // 实际消耗的 ReAct 轮次
    ToolCalls  int       // 实际执行的工具调用次数
    ConfigHash string    // 本次运行使用的配置快照版本（ConfigSnapshot.hash，原义不变）
    ManifestHash string  // 本次运行的 RunResourceManifest 哈希；旧版未记录时为空
    Reason     string    // 非正常终止的可读原因（limit_reached / failed 时必填）
}

// Run 是 AgentService 的唯一入口。
func (s *AgentService) Run(ctx context.Context, req RunRequest) (RunResult, error)
```

**上下文纪律**（宪法原则）：`ctx` **只**承载取消、截止时间与追踪标识。Agent 配置、Session、工具执行环境一律走 `RunRequest` / `RunEnv` 显式类型化参数——**不得**用 `context.WithValue` 传业务状态。

```go
// RunEnv 是循环内部持有的运行环境：由 AgentService 依 RunRequest 与配置快照构造，值传递。
// 本节是 RunEnv / ToolEnv 字段字的唯一事实源——技术方案与实现**不得**另抄字段清单。
type RunEnv struct {
    Req       RunRequest
    Snapshot  *ConfigSnapshot     // 本次运行冻结的配置快照（agent_name + hash + content；中途改盘不换配置）
    AgentUID  string              // 准入时从生命周期状态文件读取的稳定 Agent 身份；资源资格判定只用它，不用名称
    Resources *RunResourceManifest // 准入时固定的资源版本清单（见下）；旧版 Run 为空
    Session   *Session            // 会话历史读写；无状态调用时为仅内存的临时会话
    Budget    Budget              // RunRequest.Budget 与 Profile 覆盖合并后的实际预算（见 §6）
    ToolSet   []string            // 本轮可用工具名（Profile 的 tools 解析结果，空 = 无工具）
}

// RunResourceManifest 是准入时固定的资源版本清单。它与 ConfigSnapshot 是两份记录：
// config_hash 保持 ConfigSnapshot 原义；本清单单独记录 Provider / MCP / 技能的发布版本、
// Provider 凭据版本与通知渠道 UID 集合，不得倒写进 config_hash，也不得用当前版本给历史 Run 补造。
type RunResourceManifest struct {
    FormatVersion  int                  // 清单格式版本；首版为 1，v2 增加 agent_uid / 凭据固定 / notify，当前为 3（v1 / v2 只读兼容，缺字段读作"未记录"）
    Hash           string               // 本清单规整序列化的 sha256
    AgentUID       string               // 准入时的 Agent 稳定身份（v2 起必填）
    AgentLifecycle string               // 准入时的生命周期状态（只可能为 active）
    AssetBundleHash string              // 准入时固定的内容寻址资产包哈希（v3 起；旧清单为空 = 未记录，执行期只按它定位 bundles/<agent>/<hash>/）
    Provider       ProviderVersionRef   // 本 Run 使用的 Provider 发布版本 + 凭据版本
    MCPServers     []ResourceVersionRef // 实际生效的 MCP 版本（待重启内容不算）
    Skills         []SkillVersionRef    // 每个绑定技能的已发布版本与完整文件清单
    Notify         NotifyManifest       // 通知模式与准入时冻结的具体渠道 UID（v2 起）
}

// ProviderVersionRef 在通用版本引用上增加模型名与凭据固定。
// 模式不单独存字段：CredentialID 为空即 env 模式，非空即 managed 模式（同一事实只记一处）。
type ProviderVersionRef struct {
    ResourceVersionRef
    Model             string // 本次 Run 使用的模型名（来自该发布版本的定义，准入时冻结）
    CredentialID      string // 准入时固定的凭据身份；env 模式为空
    CredentialVersion int    // 准入时固定的凭据版本；env 模式为 0
    TargetBinding     string // 规范化目标地址的 sha256（不含明文 URL）
}

// NotifyManifest 冻结通知授权：mode 取 none / all_eligible / allowlist；
// channels 是解析后的**具体渠道**，不设 '*' 通配。mode=none 时 channels 为空。
type NotifyManifest struct {
    Mode     string             `json:"mode"`
    Channels []NotifyChannelRef `json:"channels"`
}

type NotifyChannelRef struct {
    Name        string `json:"name"`
    ResourceUID string `json:"resource_uid"`
}

// ResourceVersionRef 指向一个不可变资源版本。
type ResourceVersionRef struct {
    Name        string // provider / MCP server / 技能名
    ResourceUID string // 稳定资源身份（v2 起必填）
    VersionID   int64  // resource_versions.id
    VersionNo   int    // 逐资源版本号（1 起）
    ContentHash string // 版本内容哈希（sha256 十六进制小写）
}

// SkillVersionRef 额外冻结完整 L1/L2/L3 文件清单；Run 内按需读取时从此清单取。
type SkillVersionRef struct {
    ResourceVersionRef
    Files []FileHash // 按 path 升序；path 相对技能目录
}

// FileHash 是技能清单中的一个文件。
type FileHash struct {
    Path   string
    SHA256 string // 十六进制小写
    Size   int64
}

// ToolEnv 是传给每个工具的执行环境：由 ReAct 循环经 `RunEnv.ToolEnv` 构造、值传递（ToolExecutor 只接收它并执行）。
// 工具的 Execute 方法**必须**显式接收它——业务参数（工作目录、作用域、身份）不走包级变量与 context。
type ToolEnv struct {
    RunID          string        // 关联审计的 Run
    SessionID      string        // 可为空（无状态调用）
    AgentName      string        // 空串 = 无 Agent 上下文（如 CLI 顶层调用），**不是**异常
    AgentDir       string        // Agent 目录绝对路径；空串 = 无上下文
    Timeout        time.Duration // 本次调用生效的超时（工具默认 / Profile 覆盖 / 剩余预算三者取最小，见 §6）
    MaxOutputBytes int           // 输出截断上限
    SkillDirs      []string      // 本次 Run 准入绑定的技能发布目录（storage_path 字面量，如 `.basalt/versions/skills/<name>/<no>`）；文件工具只对这些目录只读放行，空 = 无绑定（版本树一律拒）
    RunBound       bool          // 本次调用来自一次 Run（循环构造 ToolEnv 时置 true）；**不得**由 RunID / SkillDirs 推断。Run 内文件工具拒绝技能工作副本（`.basalt/skills`），空绑定也一样
    AgentUID       string        // 本次 Run 的 Agent 稳定身份（准入时固定）；资源范围判定用 UID，不用名称
    NotifyMode     string        // 本次 Run 冻结的通知模式：none / all_eligible / allowlist（准入时按兼容规则解析为显式值）
    NotifyChannels []string      // 本次 Run 冻结的**具体渠道 UID** 集合（准入时枚举）：notify 工具只放行集合内渠道；NotifyMode=none 时为空。空集合**不**表示「不限制」。工具不得在调用时回读 AGENT.md
}
```

### 1.2 返回语义

| 情形 | 返回值 | error |
| --- | --- | --- |
| 正常运行到最终回答 | `Status=completed` + 文本 | `nil` |
| 达 `max_iterations` | `Status=limit_reached` + **部分结果**（最后一轮的文本或空） | `nil`——这是**预期终止**，不是错误 |
| 被取消（客户端断开 / 管理台停止） | `Status=cancelled` | `nil` |
| 运行中致命错误（Provider 不可达且重试耗尽等） | `Status=failed` + `Reason` | `nil`（错误已入 RunResult；**不**双通道） |
| 调用方用法错误（Agent 不存在、会话忙） | 零值 `RunResult` | 非 nil 错误，映射 404 / 409 |

> **`error` 只用于"调用方问题"**（用法错误、前置条件不满足）。运行过程中的一切失败都归入 `RunResult.Status`——调用方只用看 Status 就能分类，不需要同时处理两个通道。

### 1.3 达上限的独立终态

调用方必须能够区分“模型已完成回答”与“运行耗尽迭代预算”。不能仅返回一段提示文本并把它当作正常完成；终态与可读提示分别承担机器判断与用户解释。

Basalt 的约定：达上限 = `limit_reached` 状态 + 部分结果。

- HTTP 层映射 **200** + 信封内 `status: "limit_reached"`（**不是 500**——服务没有故障）
- CLI 打印醒目提示 + 已获得的部分结果
- 审计侧该 Run 记 `status=limit_reached`，运维可统计"哪些 Agent 经常撞上限"

---

## 2. Run 状态机

```text
                    ┌──────────────┐
                    │   running    │
                    └──────┬───────┘
                           │
        ┌──────────────┬───┴────────┬──────────────┐
        ▼              ▼            ▼              ▼
  ┌───────────┐  ┌────────────┐ ┌──────────┐ ┌─────────┐
  │ completed │  │limit_reach │ │cancelled │ │ failed  │
  └───────────┘  └────────────┘ └──────────┘ └─────────┘
      终态            终态           终态         终态
```

| 状态 | 含义 | 触发条件 | 部分结果 |
| --- | --- | --- | --- |
| `running` | 执行中 | 进入 Run 即置此状态（**先落库再执行**） | — |
| `completed` | 正常完成 | 模型给出无工具调用的最终回答 | `FinalText` 有值 |
| `limit_reached` | 达迭代上限 | `Iterations == Budget.MaxIterations` 仍有工具调用，或工具调用次数达 `Budget.MaxToolCalls` | `FinalText` 可能为空 |
| `cancelled` | 被取消 | `ctx` 取消 / 截止时间到 | `FinalText` 可能为空 |
| `failed` | 运行失败 | Provider 调用失败且重试耗尽、审计写入不可用等致命错误 | 空，`Reason` 有值 |

**状态只前进一次**：`running` → 任一终态，**不得**从终态回退。崩溃恢复时未完成的行由重启流程标记（见 §4.3）。

**正常收尾**（`AgentService.finish`）分三步且顺序固定：**终态先落库**（`FinishRun`，独立 5 秒预算 + `WithoutCancel`）→ **学习抽取钩子**（同步、有界；US-22，见 [memory-learning.md](memory-learning.md) §2.3）→ **引用释放**（`ReleaseRunRefs`，无条件 defer）。释放调用无条件执行，但**释放前先确认终态已持久化**：`status` 仍为 `running`（`FinishRun` 写失败或未提交）或 Run 行不存在时**不释放**，引用留给启动恢复——否则钩子尚未运行凭据就被回收。释放失败**不回滚终态**：记 ERROR，`run_resource_refs.released_at` 仍为空的行由下次启动的 `releaseTerminalRefs` 按**已终态** Run 补齐。**不得**先删引用行来“释放”——行是历史证据，释放只置 `released_at`。

**崩溃恢复**关闭 `running` 行是另一条路径：状态标记与释放**仍在同一事务**内完成（§4.3.2），事务失败整笔回滚、Run 保持 `running` 等下次启动。两条路径判据不同是设计使然：正常收尾必须让抽取钩子在引用释放之前运行，崩溃恢复没有钩子要跑。旧判据 `TestRunStore_FinishRunRollsBackWhenReleaseFails` 已按 US-22 §2.4.6 替换为 `TestUS22_FinishRunKeepsTerminalWhenReleaseFails`，恢复子用例 `release_failure_keeps_run_running` 保留。

---

## 3. 并发与准入

### 3.1 同会话串行，跨会话并行

| 场景 | 行为 |
| --- | --- |
| 同一 Session 有 Run 正在执行，又来一条消息 | **拒绝**：HTTP **409** + 可读文案（`该会话有正在执行的任务，请等待完成或稍后重试`）；CLI 提示等待 |
| 不同 Session | 并行执行（goroutine，互不阻塞） |
| 实例级并发上限 | 超过**本 Worker** 的 `server.max_concurrent_runs`（缺省 32）→ 429 + 可读文案；多 Worker 部署下该上限**按实例计**，不是集群总量（见 §3.2） |

**不排队**。排队会把"这条消息什么时候有结果"变成不可预测的问题，且队列本身需要持久化——核心阶段明确不做。拒绝是即时、可解释、可重试的。

> 定时任务的"跳过"与之不同：任务到点时若上一轮同任务未完成，**直接跳过本次触发**并记日志（见技术方案 §6.3），不产生 409——定时器没有交互方，没有"重试"语义。

### 3.2 会话锁的粒度

锁在**会话**上（`session_id`），不在 Agent 上：同一 Agent 的不同会话可以并行（例如两个工程师分别和运维助手对话）。

单进程仍用进程内快路径；多进程的**权威判定**是数据库内同会话未过期 run 租约 + 事务 advisory lock（会话键**在 Go 侧**由 `advisoryKey("basalt/session/"+session_id)` 派生——SHA-256 前 8 字节按两个 32 位半段拼成 `int64`，再以 `$1` 绑定给 `pg_advisory_xact_lock`；US-24 / 迁移 0012 的 `execution_leases`）。准入在 `StartRun` 事务内、持 advisory lock 复查并插入，二者是同一互斥边界。**恢复期间租约不释放**，故旧会话在新 Run 准入时仍为 `SESSION_BUSY`——直到恢复事务提交、旧 Run 落到终态为止。

- **准入顺序原样保留**：并发 → 会话 / 实例 → 配额，并在 `StartRun` 事务内复查。任务次数记账并入同一事务，被拒请求整笔回滚（零占用）；**不再有"先预留后释放"的窗口**（Q21 改判）。
- **实例并发上限按每个 Worker 计**：`server.max_concurrent_runs`（缺省 32）不是集群全局量，第二个 Worker 不因第一个满载被误拒。
- **QPS 是每 Worker 软限额**：`internal/callers` 的 Limiter 各自放行 N，集群总量不承诺硬上限；周期配额走数据库记账，跨 Worker 生效。

### 3.3 重试与幂等（v0.1 承诺边界）

有外部副作用的调用（`http_post` / `notify`）在 v0.1 **没有**端到端 at-most-once 机制（幂等键在扩展阶段，见下）；能承诺的只有下面三条——**不得**把"at-most-once"当整体承诺对外说：

- **运行时不会自动重试**：LLM 调用与工具执行失败即进入本轮结果（失败原样回填模型），**不得**在应用层做隐式重试；供应商 SDK 自带的传输层重试除外，且必须收敛在单次调用内部，不产生第二次审计行
- **恢复不会自动重放**：崩溃恢复只做标记（`running` → `failed`、`pending` → `unknown`），**不得**重放任何已发生的调用——`unknown` 就是"可能已经发生"的诚实答案
- **客户端重传 = 新的一次 Run**：客户端（含管理台"重试"按钮）手工重发同一消息，服务端**视为全新运行**，同一份 `http_post` / `notify` 可能因此再次执行。服务端**无法识别**它是重传——v0.1 **不做**幂等键

> **幂等键登记为扩展阶段**：届时消息端点接受调用方提供的幂等键，服务端落库去重（同键返回上次结果）。核心阶段调用方自己的对账手段：`GET /runs?session_id=` 查这次动作实际产生了几个 Run（见 [openapi.yaml](openapi.yaml)）。

### 3.4 Agent 级准入一致性边界

Run 准入与 Agent 禁用 / 启用共用**同一把按 Agent 的进程内读写锁**（`AgentGate`），所有入口（HTTP、CLI、定时器、内部调用）都必须经过；资源发布与准入另共用**进程级唯一**的 `ResourceGate`（发布写锁、准入读租约，进程内不得出现第二套互斥）。两道锁分工：

| 操作 | 持锁方式 | 边界内动作 |
| --- | --- | --- |
| Run 准入 | `AgentGate` 读锁 | 重读状态文件并校验（`active` 才放行）；**只覆盖状态重读与校验这一小段**，判定后立即释放——状态放行判定即准入线性化点 |
| Run 准入 | `ResourceGate` 读租约 | 校验通过后取得：解析并固定 `RunResourceManifest` v2（Provider / MCP / 技能发布版本与哈希、Provider 凭据版本与目标绑定、按 `notify_mode` 枚举出的具体渠道 UID）；校验绑定 UID 的资格（共享或本 Agent 专属）与绑定完整性，写入 `run_resource_refs` / `run_credential_refs` / `run_notify_refs`；租约覆盖"资源解析 → 固定资源 → `StartRun` 事务成功"，失败路径归还租约并释放已固定运行时 |
| 禁用 / 启用 | `AgentGate` 写锁 | 校验 `expected_state` / `expected_generation` → 原子写状态文件（generation + 1）→ 更新生命周期索引 |
| 归档 | `AgentGate` 写锁 | **先立准入屏障**（`agent_lifecycle.state` 置 `disabled`，`prior_state` 落 `management_operations.detail_json`）→ 注销定时 → 发 `cancel_requested` → 有界等待属主观察 → 移出注册索引 → 移动目录；超时按 `prior_state` 回滚并 `409 AGENT_BUSY`；失败按 §9.4 查证并恢复注册 |

- **禁用不取消已在跑的 Run**：它们继续完成；禁用返回后该 Agent 不再接受新 Run。但准入线性化点在状态放行判定——在禁用写锁取得**之前**已判定放行（并随即释放读锁）的准入，仍可能在禁用返回后完成 `StartRun`；**不得**声称"禁用返回即保证没有新 Run 开始"。归档才取消并等待。
- 状态文件缺失 / 损坏时，准入在锁内直接拒绝（`409 AGENT_UNAVAILABLE`），**不得**回退为“按目录存在即 active”。
- `AgentGate` 读锁只覆盖"重读状态文件并校验"，**不得**跨越资源解析、LLM 调用或工具执行；"资源已固定、账本未落库"的窗口由 `ResourceGate` 读租约覆盖（到 `StartRun` 事务成功为止），发布写锁因此插不进来；Run 开始后配置与资源版本已固定。
- **归档先立屏障再等待**（US-24）：先把 `agent_lifecycle.state` 置为非 `active`（原值 `prior_state` 落 `management_operations.detail_json`），`StartRun` 事务内复查同一行 `FOR UPDATE` 即拒绝新 Run——屏障覆盖**所有 Worker**，不是只查本地忙碌。屏障落库后才发跨 Worker 取消（写 `runs.cancel_requested`），再有界等待属主观察。超时回滚状态并 `409 AGENT_BUSY` 点名仍在运行的 Run。**不是**"远程在跑即立刻 409"；远端 Run 仍在跑时**不得**假装取消成功或宣称归档完成。
- 归档等待有界（缺省 30 秒）；超时返回 `409 AGENT_BUSY`，注册与生命周期状态回滚到归档前（按 `prior_state`）。
- **准入绑定校验**：`resource_bindings` 缺少对 `provider.name` / `notify_channels` 的绑定 → `409 AGENT_RESOURCE_BINDING_MISSING`；绑定 UID 与当前发布指针不符、已删除或未发布 → `409 AGENT_RESOURCE_BINDING_INVALID`；绑定的资源是其他 Agent 的专属（或当前 Agent 使用声明外渠道）→ `409 RESOURCE_NOT_ELIGIBLE`；Provider 凭据已撤销 → `409 CREDENTIAL_REVOKED`；托管凭据因主密钥不可用无法解密 → `503 CREDENTIAL_UNAVAILABLE`。以上都只拒绝**新 Run**，不改写配置、不自动改绑、不静默回退到同名资源。
- **准入固定粒度**：Provider 固定到**凭据版本**（`run_credential_refs`），Run 内每次模型调用前检查该版本状态；通知只固定**渠道 UID 集合**（`run_notify_refs`），**不**固定渠道凭据版本——每次发送解析当前 `active` 版本。两者不得互相套用。

---

## 4. 取消语义

### 4.1 传播链路

```text
客户端断开 / 管理台「停止」/ 上下文截止时间到
  → ctx.Done()
  → ReAct 循环在每轮开头、每次工具执行前检查 ctx.Err()；工具组返回后再复查一次（取消**优先于**
     预算判定——最后一轮最后一个工具执行中被取消，仍以 cancelled 收场，**不得**先落 limit_reached）
  → 正在执行的 shell：向进程组发终止信号 → 超时未见退出则强杀进程树
  → 已完成的 LLM 调用与工具调用照常落审计（不撤销）
  → Run 置 cancelled
```

### 4.2 审计写入与取消解耦

被取消的 Run 也必须落审计——这正是"为什么停了"最需要证据的时刻。做法：

- 审计写入使用 `context.WithoutCancel(ctx)` + **独立短超时**（缺省 5 秒），不受请求取消影响
- 取消路径上审计写入失败只记 ERROR 日志，**不得**让审计失败覆盖 cancelled 状态

### 4.3 崩溃恢复

进程崩溃时可能有：`runs` 表有 `running` 行、`llm_calls` / `tool_invocations` 有 `pending` 行（见 §7 两阶段写）。**恢复由谁做、什么时候做**必须先定——否则一个只为读数据而启动的进程，会把另一个活实例正在跑的 Run 误标成 `failed`。

#### 4.3.1 工作区实例锁：谁来恢复

- 进程启动时获取**工作区实例锁**——**操作系统级的独占文件锁**（Unix：`flock(fd, LOCK_EX | LOCK_NB)`；Windows：`LockFileEx` 独占锁），锁文件 `.basalt/basalt.lock`
- **锁的生命周期由内核决定，不由写进文件的值推断**：进程存活期间一直持有；进程结束（正常退出 / 崩溃 / `kill -9` / 断电后重启）由**操作系统**释放。于是"锁拿不到"的唯一含义就是"另一个活进程正持有它"——Windows 上同理，进程消亡即释放
- 锁文件里记录的 `instance_id` / `pid` / 心跳时间戳**只作诊断用**（拿不到锁时告诉运维"是哪个进程、从何时开始占着"）；**不得**仅凭心跳过期（如"超过 30 秒没刷新"）抢占锁——被 `SIGSTOP`、宿主挂起或长时间停顿的进程心跳同样会停，但它并没有退出，两个进程同时写库会直接击穿 §3.2 的会话串行契约。**心跳超时永远不构成抢锁的理由**
- **不得**用"创建文件成功即持锁"（exists 判据）或"定期刷新时间戳"（心跳判据）实现互斥：前者在崩溃后因遗留文件**永久挡住启动**，后者在进程被暂停时**把锁骗给第二个进程**。锁文件**可以**留在磁盘上不清理——互斥由内核的锁承担，残留文件只是诊断信息；下次启动打开同一路径重新加锁即可
- 拿不到锁 = 另一**活实例**在跑 → **写模式命令**（`serve` / `gateway` / `chat`，以及 `profile create` / `profile delete` 等会写工作区或数据库的 CLI 子命令）**拒绝启动**并给出可读文案（点名诊断信息里的 pid 与开始时间；服务在跑时指引改用 REST / 管理台——保存即生效）。工作区实例锁语义**不变**——它守的是"同一工作区不被两个进程同时写文件与库"。多 Worker 部署是**每个 Worker 一个工作区**（各自的 `.basalt/basalt.lock`），跨 Worker 的会话串行由 §3.2 的数据库租约 + advisory lock 承担，**不是**由这把文件锁承担
- **启动不再做全局崩溃恢复扫描**。恢复**只作用于本 Worker 合法领取的过期租约**（§4.3.2），其他主机上的活 Worker 与其在跑行不受影响——这正是 US-24 取代旧"拿到锁即扫全库"口径的地方：那把工作区锁只证明本工作区无第二个写者，不证明数据库里没有别的活 Worker
- **只读命令**（`init`、`profile list` / `profile show`、`session list` 等）不取锁、不做恢复，也**不得**去清理或删除锁文件（那会破坏在跑实例的锁语义），直接读库；打开库前**必须**校验 `schema_version`（高于二进制支持的版本 → 拒绝并提示升级二进制，见 [schema.sql](schema.sql) 迁移纪律 2）

> 实例锁是**工作区级**的（一个进程对一份 `.basalt`）：它只把**同一个工作目录**的文件写入串行化，**不**保证"库里只有一个写者"。多 Worker 共用一套 PostgreSQL 时，跨 Worker 的单写者纪律由 §3.2 的 `execution_leases`（负责人 + 归属代次 + 过期判据）与数据库 advisory lock 承担；"同一会话只有一个 Run"同样由 §3.2 的会话 advisory lock 与 `run` 租约承担，与本文件锁无关。
>
> 边界：工作区放在 **NFS / SMB 等网络文件系统**上时，OS 锁的语义取决于服务端实现（可能不生效）——v0.1 **不支持**网络文件系统上的工作区（本地盘或块设备挂载）。

#### 4.3.2 恢复扫描做什么

**输入集合改为本次合法领取的租约**（US-24）：六步算法原样保留，但不再由"启动拿到工作区锁"触发全局扫描，而由固定有界的回收循环（独立于 cron，条数上限 + 超时，不执行业务推进与模型 / 工具调用）对**本次合法领取的过期或已释放租约**执行。每一步都**幂等**——重复执行结果相同，中途再崩一次，下一轮重跑也会收敛到同一结果。

**无租约的 `pending` 只报告不改写**：改写它等于替一个可能仍活着的 Worker 下结论。

**A 类 / B 类分工**：A 类（`run` / `schedule_execution` / `flow_root` 三种租约）先以 `SKIP LOCKED` + 代次条件领取，再按作用域收尾；**只有接管路径（A1）**把归一化 → 补组 → 引用释放 → 投递结算 → 终态 → 释放租约合并为**一个事务**（那条路径上学习钩子已丢失，没有需要跨事务保留的进程内资源）。B 类（终态残留、Run 创建前的 `pending` 投递、孤儿学习抽取）**不领租约**，用幂等条件更新，重复执行无害——但幂等不是所有权证明，所有权仍由租约判。

1. **归一化审计行**：`llm_calls` 与 `tool_invocations` 中 `status = 'pending'` 的行 → 标 `unknown`（调用 / 工具是否已发生**无法确定**——外部副作用可能已产生）；**范围限定在本次领取的 `run_id` 集合内**，库中其余 `pending` 行只进报告
2. **扫未完成组（工具组的入口是消息，不是 `runs` 表）——按位置判定，范围限定在本次领取的 `run_id`**：每条 `role = 'assistant'` 且 `tool_calls` 非空的消息即一组，取其在**同一会话内紧随其后（`seq` 更大且连续）的 `role = 'tool'` 消息段**（直到下一条非 `tool` 角色消息为止）；组内 `tool_call_id` 集合未覆盖该 assistant 的全部 `id` → 半截组。**不得**用"全会话按 `tool_call_id` 查找"作判据——旧 Run 留下过同 id 的结果行时，新 Run 复用该 id 后的半截组会被误判为已完成（供应商的调用 id 跨 Run 复用不受控，内部关联**不得**只靠它）
3. **逐组修复，且与 Run 状态修复放进同一个短事务**：对每个含半截组的 Run（`messages.run_id`），在**一个事务**里做完两件事再提交——按 §4.4 补齐该 Run **全部**半截组缺的结果（合成结果只写 `messages`），并把该 Run 若仍是 `running` 则一并标 `failed`、写 `finished_at`，`reason` 按后端见下方「租约丢失的 reason 字面量」。崩溃发生在提交前 → 整个事务回滚，组与 Run 都保持原样，下次启动重新扫到；提交后 → 两者同时落地。因此**"Run 已翻成 failed 而组仍半截"这一中间态不可能持久化**
   - 半截组的 `run_id` 为空、或找不到对应 `runs` 行（异常数据）→ **仍然补组**（Run 状态无从修复），记 WARN 点名该 `run_id` 与会话
   - 崩溃时的 Run 一律标 `failed`，即使它当时正走在取消路径上：进程死在把 `cancelled` 写下来之前，"已取消"是没写成的意图、不是事实
4. **收尾剩余 `running` 的 Run，并释放已终态 Run 的资源引用（这一步扫 `runs` 表）**：第 3 步只覆盖**含半截组**的 Run，而崩溃还会留下**没有半截组可补**的 `running` 行——典型三种：正在等第一次 LLM 响应（还没有 `tool_calls` 落库）；最终回答已落库但终态未写；无状态 `invoke`（不落会话消息，消息里根本没有它的组）。它们同样必须收尾。故在**补组完成之后**再扫一次 `runs`（同样限定在本次领取的租约作用域）：仍为 `running` 的行全部 → `failed`、写 `finished_at`，`reason` 按后端见下方「租约丢失的 reason 字面量」；**同一轮**把 `run_resource_refs` / `run_credential_refs` / `run_notify_refs` 中 `released_at IS NULL` 且所属 Run 已是终态的行补写释放时间（覆盖“Run 已终态但引用未释放”的崩溃窗口，B 类的终态残留分支）。第 3 步与第 4 步互补——**"这个组补齐了吗"只有消息能答，"这个 Run 结束了吗"只有 `runs` 能答**，两件事的入口不同，缺任何一个都会留下"永远 running"的记录
5. **工作区替换残留判定**（多文件提交的目录交换中断，见技术方案 §3.4.2）：有 `<agent>/.backup` 且正式目录缺失 → **回滚**（把 `.backup` 恢复为正式目录）；正式目录完整且 `.backup` 尚在 → **前滚**（删除 `.backup` 与 `.staging` 残留）。判定只看文件系统状态，幂等可重跑，结果记 INFO 日志。这一步只看**本工作区**的文件系统，与数据库租约无关，仍随进程启动做一次
6. 恢复过程记 INFO 日志（数据库与工作区两部分），条数写进回收摘要 / 启动摘要

**租约丢失的 reason 字面量**（机器可读，按后端）：

| 触发路径 | 后端 | `runs.reason` |
| --- | --- | --- |
| 回收器接管已过期 run 租约收尾 | PostgreSQL | `worker_lost`（固定字面量） |
| 同上，但绑定的调用方 Key 已失效（`owner_unauthorized` 优先于 `worker_lost`） | PostgreSQL | `owner_unauthorized` |
| 属主连续续约失败达 `ttl - renewInterval`，或续约返回失权后自行收场 | PostgreSQL | `renew_failed`（固定字面量；Run 状态为 `cancelled`） |
| 调度器处理错过宽限 | 两者 | `missed_grace_exceeded` |
| 旧单机启动恢复（SQLite 谱系，保留不动） | SQLite | `进程中断（启动时发现未完成的运行）` |

**reason 原文不出 API**：`runs.reason` 与日志保留上述字面量；HTTP 响应经 `publicRunReason` 投影（cancelled / failed / limit_reached 各有固定公开文案，仅 `ReasonIterationLimit` / `ReasonToolBudget` 透出），原始 reason 不得直接出现在响应里。

> 为什么**工具组修复**不以 `runs` 表为入口：`runs.status = 'running'` 只说明"这个 Run 没结束"，而**半截组才是要修的东西**。以 Run 为入口、且"先标 `failed`、后补组"分两步提交时，中间再崩一次，下次启动的候选集里已经没有这个 Run（它不再是 `running`），半截组就**永久遗留**——用当前 DDL 在内存 SQLite 上构造过这个反例：恢复候选为 0、工具结果仍缺失。以**消息**为入口（步骤 2，不看 Run 状态）+ **状态与补齐同事务**（步骤 3），两个坑一起消掉：工作永远可被重新发现，中间态永远不可持久化。
>
> 这只说明**工具组**不能拿 `runs` 当入口，不等于恢复不看 `runs`：没有半截组的 `running` Run（步骤 4）只有 `runs` 认得出来，必须在补组之后另扫一次收尾。一句话——**组看消息，Run 看表**。
>
> **`unknown` 是诚实的状态**：崩溃时 shell / HTTP 工具可能已产生外部影响。审计不猜测，如实标记，由人判断。这也是 §3.3"恢复不会自动重放"的原因。

### 4.4 会话可继续：未完成工具交互组的补齐（Option A）

崩溃或取消会在会话历史里留下**半截的工具交互组**：assistant 消息带了 `tool_calls`，却没有（或只有一部分）对应的 tool 结果。这样的历史送进下一次调用，供应商会因"工具调用缺结果"报 400——**会话从此哑掉**。核心阶段的解不是清历史，而是**在历史进入下一次调用之前，把组补完整**：

**补齐规则（每个工具调用一个合成结果，按审计行判定）**。判据只看**有没有终态行**：`success` / `failed` 是终态；`pending` 与 `unknown` **同判**为"结果不可知"——§4.3.2 第 1 步会把 `pending` 归一化成 `unknown`，先归一还是先补组都必须得到同一结论：

| 情况 | 判据 | 合成的 tool 结果 |
| --- | --- | --- |
| 工具已执行且审计有终态 | `tool_invocations` 有 `success` / `failed` 行 | **还原真实结果**（由 `result_json` 重建） |
| 取消时尚未执行 | 取消路径——进程还活着，循环内存里已知 | `已取消（运行被取消，该调用未执行）` |
| 预算耗尽未轮到 | `MaxToolCalls` 已用尽，本轮该调用未执行 | `未执行（工具调用次数超限）` |
| 崩溃时执行中（含执行完但终态未落） | 有 `pending` **或 `unknown`** 行 | `结果未知（进程中断，无法确定是否已产生外部副作用）` |
| 崩溃时尚未轮到 | 无对应行（fail-closed 下"无行 = 确定没执行"） | `未执行（进程中断）` |

- **本轮调用"人人有结果"是补齐的底线**：工具预算用尽时（如只剩一次预算、模型返回三个调用），已执行的写真实结果、**剩余的逐个合成** `未执行（工具调用次数超限）`，全部有结果后 Run 才落 `limit_reached`——否则下一轮会话仍会把半截组送进供应商，同样报 400
- **取消优先于预算判定**：工具组返回后先查 `ctx.Err()` 再判上限（见 §4.1）；被取消的运行以 `cancelled` 收场，其组内未执行调用按下表"取消时尚未执行"合成

- **合成结果只进会话历史（`messages` 表），不进审计表**——审计永远只记真实发生的事，合成属于历史修复
- **取消路径**由 `ReActLoop` 在收到 `ctx.Done()` 时对本轮整组调用同步补齐（见技术方案 §2.2.1），不依赖启动恢复
- **崩溃路径**由 §4.3.2 第 2、3 步在启动恢复时补齐（消息扫描定位 + 短事务提交）；没有半截组可补的 `running` Run 由第 4 步兜底收尾
- 补齐是**幂等**的：判据是"assistant 的 `tool_calls` 里每个 id 是否已有结果"——判据落在**消息**上而非任何内存列表上，所以恢复流程随时可以整段重跑，重复执行不会重复插入
- **Option B（发送历史时排除未完成组）被否决**：排除发生在**每次读取**的路径上，后续消息与旧组交错后排除逻辑只会更复杂；且历史里永远留着半截组，任何新读取方（导出、管理台、下一位维护者）都得再实现一遍过滤。补齐是**写一次、以后都干净**

> 只补**模型发起过的调用**；用户消息与最终回答不受影响。合成结果在管理台按失败样式呈现（"上一步被中断"），人一眼能看出这次运行不完整。

---

## 5. 触发源与 Run 的对应

| 触发源 | Channel | SessionID 形态 | 备注 |
| --- | --- | --- | --- |
| CLI `basalt chat` | `cli` | 由 `channel+user+profile` 派生 | 交互循环内每轮一条消息 = 一次 Run |
| REST `POST /sessions/{id}/messages` | `web` | 请求指定 | 同步等待 Run 完成（≤ 预算超时） |
| REST `POST /agents/{name}/invoke` | `web` | 无状态（临时会话或空） | 不落会话历史，仍落 Run 与审计 |
| 定时器 | `scheduler` | `channel=user=scheduler`，同 Agent 复用 | 到点触发一次 Run |
| 管理台手动补跑 | `scheduler` | 与该任务一致 | 走同一链路，审计可区分（`trigger` 字段） |

**RunID 生成规则**：`<channel>-<unix_millis>-<随机 6 位>`，全局唯一、可排序、可 grep。管理台"手动补跑"的 RunID 前缀仍为 `scheduler-`，靠 `runs.trigger` 列区分 `cron` / `manual`。

### 5.1 运行来源段（模型可见，不是权限边界）

每次 Run 的段一末尾**必须**追加一段固定的运行来源，供模型区分"即时对话"与"定时任务"：

```text
（段一正文）

## 本次运行来源
<下表对应的一句>
```

| Channel | Trigger | 追加的句子（逐字） |
| --- | --- | --- |
| `scheduler` | `cron` | `定时任务到期触发。请根据本次任务消息执行。` |
| `scheduler` | `manual` | `用户手动运行了定时任务。请根据本次任务消息执行。` |
| `scheduler` | 空 | `来自调度器，但触发方式未确认。请根据本次任务消息执行。` |
| `web` / `cli` | 任意 | `来自用户或业务接口的即时请求。请根据本次消息的实际意图处理，不要仅因存在定时流程就自行执行它。` |
| 其他（含 `admin`） | 任意 | **不追加任何字符**（连标题一起不出现，不是空小节） |

**硬约束**：

- 来源**必须**只取自服务端构造的 `RunRequest.Channel` + `Trigger`；**不得**从用户消息、会话行、请求头或模型输出推断——用户正文里写出同样的标题与句子只是普通文本。
- 来源**不是**权限隔离或强制执行机制，也**不是**"本次一定是定时任务"的证据：它只是给模型的提示信息，**不得**被实现成第二道权限面。
- 判定权限的**不是**来源，而是**本次 Run 的身份与授权状态**：相同 Agent 配置、相同授权条件下，两种来源**适用同一套**工具、策略与预算规则；实际是否放行仍按各次 Run 的调用方身份、凭据版本及其撤销状态逐次判断——授权不足或已撤销时两种来源**同样**被拒，不因来源不同而放宽或收紧。
- 审计按**一致的结构与记录规则**如实记录各次 Run 的实际行为（模型调用与工具调用逐条落库、工具执行先写 `pending` 再写终态、`runs` 记 `channel` 与 `trigger`），但**不要求两次 Run 的审计内容相同**：`run_id` 必然不同（格式 `<channel>-<unix_millis>-<随机6位>`，渠道前缀本身就不同），时间戳、实际发生的模型调用与工具调用同样可以不同（一次即时对话可能零工具调用，一次定时运行可能多次）。这些差异来自各次实际行为，不构成权限或执行面的差异。
- 来源不计入 `ConfigSnapshot` / `config_hash`，未映射渠道时提示词与旧版本逐字节相同。
- 来源段拼接后计入段一的固定 token（`FixedParts.System`），因为它确实占模型上下文。

违反时的症状与复核方式见 [技术方案 §3.3](../TechnicalDesign.md) 的运行来源段条目；实现判据为 `TestPrompt_RunOriginByChannel` / `TestPrompt_RunOriginOmittedForUnmappedChannel` / `TestPrompt_UserMessageCannotForgeRunOrigin`。

---

## 6. 执行预算与准入控制

一次 Run 的资源消耗**必须有界**，五项预算缺一不可：

| 预算项 | 缺省值 | 超限行为 |
| --- | --- | --- |
| `MaxIterations` | 10（Profile 可覆盖） | `limit_reached` |
| `RunTimeout` | 60 秒（REST 单次调用）/ 300 秒（定时任务与手动补跑 `POST /schedules/{id}/run`） | `cancelled`，`Reason=超过运行时限`；504 文案点名实际生效秒数 |
| `ToolTimeout` | 30 秒（Profile 可覆盖） | 该次工具调用失败（`timeout`），**Run 继续**——模型看得到失败并能换个做法 |
| `MaxToolCalls` | 50 | `limit_reached`，`Reason=工具调用次数超限`；本轮剩余调用逐个合成「未执行（工具调用次数超限）」（§4.4） |
| `MaxOutputBytes` | 单工具输出 256 KB | 截断 + 标记 `truncated: true` 回填模型 |

**上下文预算**：历史裁剪以"实际发送的 token 估算"为准（`max_history_turns` 是轮数上限，`budget.context_tokens` 是硬上限，缺省 32768）。超预算时按完整工具交互组从最早开始整组丢弃（见 [agent-md.md](agent-md.md) §4）。**不可裁剪内容超限即拒绝启动**：系统提示词、核心记忆、当前消息等不可裁剪部分本身超过预算时，该次 Run 不发出（`failed` + `Reason` 点名 `budget.context_tokens`），**零 LLM 调用**——不得静默超发。

**预算的制定权在运维，模型只能缩短**：`ToolEnv.Timeout` 取"工具默认 / Profile 覆盖 / 剩余预算"三者最小值——模型给工具传的参数（如 `timeout_seconds`）**不得**把生效超时改大。数值预算的配置入口是 `basalt.yaml` 的 `budget:` 段（`max_iterations` / `max_tool_calls` / `max_output_bytes` / `context_tokens`，含缺省值，见技术方案 §2.5.1）；Profile 只能覆盖其允许项（`settings.max_iterations`），**不得**放大运维上限。

**核心记忆写配额**：`save_memory` 写核心区时，单条 ≤ 4 KB、每 Agent 核心区总量 ≤ 32 KB，超限返回**可读拒绝**（模型可自纠：改写归档区或压缩内容）。核心区"永不截断"的前提是有界。手工编辑记忆文件导致的超配额**不**按写拒绝处理：注入时记 WARN 并**完整注入**（不静默截断——静默截断会让排障者误判为模型失忆）；若完整注入使上下文超预算，走上面"不可裁剪内容超限拒绝启动"路径。同一作用域内的写入串行化（进程内 per-scope 锁，见技术方案 §3.5.1）："读-检查-写"全程不得交错。

---

## 7. 审计模型

**三张运行审计表**共同回答"这一次运行为什么那样做"，全部挂在 `run_id` 下；**第四张管理操作表**回答"谁改了什么、结果如何"，使用独立 `operation_id`，不挂 `run_id`。完整 DDL 见 [schema.sql](schema.sql)。

### 7.1 四张表的分工

| 表 | 一行代表 | 关联键 |
| --- | --- | --- |
| `runs` | 一次运行 | `run_id`（主键） |
| `llm_calls` | 一次模型调用 | `run_id` + `round`（该 Run 内第几轮） |
| `tool_invocations` | 一次工具执行 | `run_id` + `tool_call_id`（模型给的调用标识） |
| `management_operations` | 一次管理面高权限变更 / 管理探针 | `operation_id`（主键；**不挂** `run_id`） |

管理操作**不得**混写为模型 / 工具调用，也**不得**为了复用 `llm_calls` 而伪造一次 Run。模型连接测试不产生 Run：它仍先写 `management_operations` 的 `pending` 行（预写失败不得发出调用），结果、provider / model、耗时与 token 用量记在脱敏的 `detail_json`。

`run_resource_refs` / `run_credential_refs` / `run_notify_refs` / `notify_deliveries` 是**关联与明细记录**，不构成独立审计结论：它们固定 Run 使用的资源版本、Provider 凭据版本与通知渠道（具体 UID），成败仍由 `llm_calls` / `tool_invocations` 表达。`notify_deliveries` 不设单独的 success / failed 列，查询时与 `tool_invocations` join。

### 7.2 审计失败策略：fail-closed

**写入审计是执行的前置条件**，不是事后动作。`llm_calls` 与 `tool_invocations` **同一套两阶段写**：

1. **执行前**先插入 `status=pending` 行——插入失败即**拒绝发出该次模型调用 / 执行该工具**（工具侧把失败结果回填模型并说明审计不可用；模型调用侧让 Run 以 `failed` 收场）
2. **执行后**把该行更新为终态（`success` / `failed`）

理由：审计是严监管场景的交付物，"少数几条没记上"比"服务不可用"更糟——前者无人知晓，后者立刻被看见。"预注册"把"调用了却没记录"的窗口从**整次调用**缩小到**两次写之间**：后者只影响单行的终态，不影响"这次调用存在过"这一事实。

**残余窗口（如实登记，不假装没有）**：

| 窗口 | 后果 | 处理 |
| --- | --- | --- |
| 调用 / 工具已发生，但**终态更新失败** | 行停在 `pending` | 记 ERROR 日志，行保持 `pending`；下次启动恢复标 `unknown`（§4.3.2） |
| Run 的**终态持久化失败** | Run 行停在 `running` | 记 ERROR 日志；下次启动恢复标 `failed`（§4.3.2） |
| 调用已发出但审计**预写**失败 | **不存在** | 预写先于发出（第 1 步）——失败即不发，这正是选择"预注册"而非"事后写"的原因 |

管理面高权限变更使用**同一 fail-closed 方向、独立的操作契约**：

1. 执行变更前先插入 `management_operations(status=pending)`；插入失败**不得**执行变更
2. 变更完成后更新终态（`success` / `failed`）；终态更新失败记 ERROR，行保持 `pending`
3. 启动恢复只对 `pending` 操作**查证实际结果**，再写 `success` / `failed` / `unknown` 与 `verification`（§9.4）；**不得**套用模型 / 工具调用的 `unknown` 规则，**不得**盲目重做

管理操作的 `unknown` 表示“无法从当前文件 / 数据库 / 指针状态判断变更是否已生效”，不是“调用可能已发生”的同义句；两者不得混用。

### 7.3 失败分类

失败必须有类别，否则审计只能回答"失败了"、不能回答"为什么"：

| `error_kind` | 含义 | 例子 |
| --- | --- | --- |
| `sandbox_denied` | 沙箱拒绝 | 路径越界、域名不在白名单 |
| `timeout` | 超时 | shell 超时、HTTP 超时 |
| `not_found` | 目标不存在 | 文件、渠道、MCP 工具 |
| `notify_disabled` | 通知被配置或凭据状态禁用 | `notify_mode: none`；渠道托管凭据已撤销 |
| `invalid_input` | 参数非法 | JSON 不合法、必填缺失 |
| `external_error` | 外部系统错误 | HTTP 5xx、模型供应商 4xx/5xx |
| `runtime_error` | 运行时错误 | 进程启动失败、编码错误 |
| `unknown` | 无法归类 | 崩溃恢复时的 pending 行 |

**`sandbox_denied` 的细分原因（14D 起）**：统一的"策略拒绝"无法解释"该改哪一条"，因此工具结果与运行记录另带 `policy_denial_reason`，取值固定四类：

| `policy_denial_reason` | 触发 |
| --- | --- |
| `profile_denied` | 档位授权范围拒绝（`strict` 档下目标不在白名单等），与具体地址无关 |
| `denied_domain` | 命中 `http.denied_domains`（两档都先生效，不再看白名单） |
| `protected_address` | 解析结果落在受保护地址段（私有 / 回环 / 链路本地 / 云元数据） |
| `exception_missing` | 该地址本可由访问例外放行，但当前没有生效例外 |

四类互斥且必须可区分：`error_kind` 仍是 `sandbox_denied`（向外层分类不变），`policy_denial_reason` 只作细分；**不得**把四类压回一个字段值，也不得在运行记录里只留自由文本。

### 7.4 存什么、不存什么

**存**：完整入参、完整结果（超 256 KB 截断并标记）、失败原文（截断到 4 KB）、耗时、轮次、模型与供应商、token 计量、配置快照版本。

**不存**：通知渠道地址原文（凭证）、模型推理的 `reasoning_content`（体积大且含模型内部过程；仅在会话消息里按供应商要求回传，**不**入审计表）。

**脱敏不是承诺**：本表如实记录工具入参与结果——**Agent 通过工具读到的内容会进审计**。运维需知晓这一点（这也是"审计能解释失败"的前提）。凭证的防线在别处：env 模式下渠道地址与 api_key 只以精确 `${ENV_VAR}` 引用形态入库、使用前解析；managed 模式下地址与密钥以 AES-256-GCM 密文落库（仅 `key_id` / `last4` / 目标绑定哈希可读），主密钥在工作区之外，解密只在按需使用时发生。管理操作的 `detail_json`、日志、诊断与错误响应**不得**出现明文 URL、密钥或主密钥；`notify_deliveries` 只记渠道 UID / 名称 / 目标 host。

---

## 8. 错误码表

统一信封 `{code, message, data, timestamp}`（见 [openapi.yaml](openapi.yaml)）。

| HTTP | `code` | 触发场景 | message 要求 |
| --- | --- | --- | --- |
| 200 | `OK` | 成功 | — |
| 200 | `LIMIT_REACHED` | Run 达迭代/工具次数上限 | 说明撞了哪条上限 + 可调配置键 |
| 400 | `BAD_REQUEST` | 参数非法、名字不合法、路径越界、**超限**（消息 > 32 KB / 文件预览 > 256 KB） | 点名非法值或给出上限值 |
| 400 | `UNSUPPORTED_TYPE` | 渠道类型未装配 | 点名支持的取值集合 |
| 400 | `EDIT_UNSUPPORTED` | 结构化编辑无法在保留注释 / 未改内容的前提下完成 | 说明改用整份原文编辑；**不得**静默重排 |
| 400 | `IMPORT_REJECTED` | 技能导入含软连接 / 越界路径 / 非普通文件 / 超限 | 点名违规路径与上限 |
| 400 | `CREDENTIAL_MODE_REQUIRED` | 凭据值与当前模式不符且未显式给出目标 `credential_mode`（创建时普通值、更新时模式不匹配） | 点名字段与两种模式的合法值；不推断模式 |
| 400 | `CREDENTIAL_REF_READ_ONLY` | 请求体出现只读 `credential_ref` / 客户端自造凭据 ID | 说明凭据引用由服务端生成 |
| 400 | `RESOURCE_NOT_ELIGIBLE` | 绑定的资源对当前 Agent 无资格（其他 Agent 专属，或渠道不在 Agent 声明内） | 点名资源、所有者与资格规则；保存零写入 |
| 400 | `AGENT_RESOURCE_UNAVAILABLE` | Agent 引用的资源不存在、已删除或没有已发布版本 | 点名引用名与 UID 前 8 位；提示重新选择 |
| 400 | `AGENT_RESOURCE_BINDING_MISSING` | 保存或准入时 `resource_bindings` 缺少对某个引用的绑定 | 点名缺失的 `{kind, name}`；不按名补绑 |
| 400 | `AGENT_RESOURCE_BINDING_INVALID` | 绑定结构非法、UID 与当前发布指针不符、重复名或孤儿绑定 | 点名冲突字段；零写入 |
| 400 | `CREDENTIAL_REVOKED` | 绑定的 Provider 凭据已撤销（保存时重新校验） | 点名资源与凭据状态；提示重新录入新值 |
| 400 | `INVALID_SCOPE` | Key 发放 / 轮换时 scope 未知、为空，或属于管理面（`callers:admin` / `diagnostics:read`） | 点名非法 scope；零写入、零操作行，不留 `pending` |
| 401 | `UNAUTHORIZED` | 管理凭据缺失或无效（token 模式或 local_trust 模式）；或 token 模式启动配置无效时的管理请求 | 不区分"没提供"与"提供错"，不回显 token |
| 403 | `FORBIDDEN` | 跨源 / 来源被拒、Host 不在白名单、已认证但无权执行 | 说明被拒的边界；不回显内部路径 |
| 404 | `NOT_FOUND` | Agent / 会话 / 渠道 / 资源 / 版本 / 归档不存在 | 点名查不到的名字 |
| 409 | `SESSION_BUSY` | 同会话已有 Run 在执行 | 说明"等待完成或稍后重试" |
| 409 | `AGENT_BUSY` | 归档 / 删除 Agent 时仍有 Run 未退出（有界等待超时） | 点名仍在运行的 run_id；状态已回滚 |
| 409 | `AGENT_UNAVAILABLE` | Agent 生命周期状态文件缺失 / 损坏，或状态为 disabled / archived | 说明当前状态与修复入口；**不得**回退为 active |
| 409 | `CONFLICT` | 同名活动技能 / 模型连接 / MCP / Agent 已存在（通知渠道同名创建为 400 `BAD_REQUEST`）、复制或恢复目标名冲突、生命周期 expected_generation 不匹配 | 点名冲突对象与当前 generation |
| 409 | `RESOURCE_SCOPE_IN_USE` | 共享转专属时仍被其他 Agent 绑定、活动 Run 或消息路由引用 | 点名全部引用者；不提供 force，不自动解绑 |
| 409 | `RESOURCE_UID_MISMATCH` | 请求携带的 `resource_uid` 与该名称当前指针不一致 | 给出当前 UID（可公开部分）；不按名猜测 |
| 409 | `CREDENTIAL_OWNER_MISMATCH` | 试图把凭据绑定到其他资源 UID，或借用其他资源的凭据 ID | 点名凭据与目标资源；不跨资源复用 |
| 409 | `SECRETS_RECOVERY_REQUIRED` | 实例加密处于恢复态（密钥文件缺失 / 损坏 / `key_id` 与记录不符 / 发布残留来源不可证明），任何以密文为前提的操作被拒（含托管凭据解密、`POST /instance/secrets/init`） | 点名恢复态与恢复入口；**不**生成替代密钥、**不**回落 env、**不**删除来源不明的文件 |
| 409 | `EXECUTION_PROFILE_MISMATCH` | `PUT /execution-policy` 的 `expected_profile` 与库内当前档位不一致 | 给出当前档位；零写入、零操作行；与修订号 CAS 的 `CONFLICT` 分开 |
| 409 | `IDEMPOTENCY_CONFLICT` | 同一幂等键的再次请求与首次参数（`class` / `cost`）不一致 | 点名冲突参数；**不得**静默复用旧结果或新建第二行 |
| 409 | `MCP_APPLY_SUPERSEDED` | `apply` 期间发生 publish / revoke / 另一次 `apply`，本次被取代 | 点名取代者配置的 `superseded_by`；候选注册与候选连接已撤回，`applied` 与旧连接不变 |
| 409 | `FLOW_ROOT_BUSY` | 管理面 `resume` / `revise` 需要把根推进（写 `queued`）时，`flow_root` 租约已被某个执行者持有 | 说明根任务正由执行者推进、请在其交棒后重试；**不重试、不降级成无租约写入**，`flow_tasks` 一行不改 |
| 500 | `MCP_APPLY_FAILED` | `apply` 在预检之后的启动 / 握手阶段失败，候选已可靠回收 | 点名 `failure_class` 与目标版本；`data.operation_id` 供查证；旧连接状态如实说明（含"旧连接也不合法"的情形） |
| 409 | `AGENT_COPY_BLOCKED` | 复制源 Agent 引用了源 Agent 专属资源，且未提供显式映射 | 列出需要映射的资源名与类型；不产生副本 |
| 409 | `ASSET_VERSION_MISMATCH` | 本地资产包的目录名 / 逐文件哈希 / `bundle_hash` 与共享发布记录（`agent_bundle` 指针的 `content_hash`）不一致 | 点名 Agent 与"重新安装"入口；不回显文件内容或哈希明细；不静默换版本 |
| 409 | `AGENT_LIFECYCLE_STALE` | 本地生命周期代次**低于**共享索引（另一个 Worker 已推进） | 给出两侧代次；本地**不得**把旧代次写回索引 |
| 409 | `AGENT_LIFECYCLE_MISMATCH` | 同代次内容不一致、索引行缺失 / 非 active，或准入未携带生命周期期望值（零值即拒绝） | 给出状态与代次；走管理操作单调修复，不自动放行 |
| 409 | `RESOURCE_VERSION_MISMATCH` | 准入事务内复查发现 Provider 的 `published_version_id` / MCP 的 `applied_version_id` / 凭据 pin 在预检之后被推进或删除（发布 / apply / 轮换插进了窗口） | 说明这一笔固定的是过期事实、重试即可；被拒请求零占用；**不得**以最新为准 |
| 503 | `ASSET_UNAVAILABLE` | `agent_bundle` 发布指针缺失 / 已软删除，或本地内容寻址目录缺失 / 不可读；**领取记录或执行租约复核不符**（钉住哈希、租约 owner / epoch / 未释放 / 未过期任一不成立）同样归此码 | 点名 Agent 与"先执行 `basalt assets install --agent <agent>`（首次发布）或 `basalt assets install <bundle_dir>`（装入已导出副本）"；**不**自动发布本地内容为真相；领取复核失败时**零写入、零新 Run**，**不**以重放补救 |
| 503 | `RESOURCE_VERSION_MISMATCH` | Provider / MCP 的本地实例版本证明不成立（含 MCP 尚无已应用版本），且按同一目标版本重建-换入-再比后仍不符 | 说明可重试但**不得**以最新为准；修复入口是重启服务或重新发布该资源；不回显凭据或连接细节 |
| 410 | `GONE` | 目标已被保留策略 / 软删移除，**不再**可读（被裁剪的消息、已软删的文档与版本） | 说明已不可读与原因；不回显原文、标题或片段 |
| 412 | `PRECONDITION_FAILED` | `If-Match` 缺失 / 不等于当前 ETag | 给出当前 ETag（可安全公开的部分） |
| 429 | `TOO_MANY_RUNS` | 实例级并发超限 | 给出当前上限 |
| 429 | `AUTH_RATE_LIMITED` | 同一来源认证失败次数超限（管理面与业务面**共用**同一个来源限速器；业务面的凭据失败同样计数） | 给出重试等待秒数；先于凭据判定给出，此时请求还没有身份 |
| 429 | `CALLER_RATE_LIMITED` | 调用方入口速率超限（每个已认证业务 HTTP 请求计一次，按 `IngressRun` / `IngressIngest` / `IngressRead` 分类） | 给 `Retry-After`；此时**尚未**进入并发与配额判定 |
| 429 | `CALLER_CONCURRENCY_LIMITED` | 调用方并发 Run 名额用尽 | 给出当前上限；名额由**所有**退出路径归还 |
| 429 | `QUOTA_EXHAUSTED` | 周期配额不足，或 `overage > 0` 期间拒绝新预留 | 给出恢复时间（下一周期起点）或管理员核对入口 |
| 500 | `INTERNAL` | 未归类错误 | **不回显内部细节**（路径、堆栈、SQL） |
| 503 | `NOT_CONFIGURED` | 能力未配置（作者模型缺失等） | **点名配置键** + 说明怎么配 |
| 503 | `CREDENTIAL_UNAVAILABLE` | 托管凭据无法在当前主密钥下读取（文件缺失 / 权限不符 / 格式错误 / `key_id` 不匹配 / 解密失败） | 点名固定原因；说明 env 模式不受影响；不回显密钥材料 |
| 503 | `POLICY_STALE` | 四类运行期策略（允许域 / 拒绝域 / 网络例外 / 执行档位）最近一次成功核对已超过 `policy.max_stale`，或最近一次刷新失败 | 点名过期类别与最近成功核对时间；拒绝**新 Run**，不改写策略、不把旧快照当有效；恢复由控制面刷新后自动解除 |
| 504 | `TIMEOUT` | Run 超过调用方时限 | 给出时限值；Run 本身记为 `cancelled` |

**US-16 准入类错误的判定顺序与合并禁区**（对应三段式 `Limiter`：`AdmitIngress` / `AdmitRun` / `AdmitCall`）：

- 顺序固定：认证 → **入口速率** → 作用域与归属 → 调用方并发 → Run 准入 / 会话串行 → 配额预留 → 审计预写 → 真正调用（C-F03，2026-10-31 裁决：先限流、后作用域）。**不得**倒置：401 / 403 / 404 / 409 / 429 都是**纯准入判定**，失败时**不得**已发出任何外部调用，也**不得**留下未解释的预留（[5] 之后才需要 `Release`）。
- **先限流的代价与收益**（同一裁决的判据面）：限流键 = ⟨调用方主体, 入口类别 `IngressRun` / `IngressIngest` / `IngressRead`⟩，与目标资源**无关**；管理凭据不进调用方桶；未认证请求在认证阶段即 401、不扣任何调用方的桶。作用域被拒的请求**扣自己的桶**——代价是调用方自伤，收益是无权调用方不能无限探测而不消耗任何名额。桶按调用方隔离：一个调用方耗尽不影响另一个，也不影响管理员。
- **可观察差异（不得反转为存在性侧信道）**：同一调用方桶空后，无论请求是否越权、目标资源是否存在，都先得到**同形** 429 `CALLER_RATE_LIMITED`（带 `Retry-After`）；桶未空时，作用域不足回 403 `FORBIDDEN`（固定文案，不点名缺哪个 scope），跨调用方资源仍是 404 `NOT_FOUND`。三者判据分离，**不得**为了"少扣一次桶"把作用域判定提前。
- 三个 429 **不得**合并为一个码：`CALLER_RATE_LIMITED`（每请求，HTTP 边界）、`CALLER_CONCURRENCY_LIMITED`（每 Run，一个名额）与 `QUOTA_EXHAUSTED`（每次真实消耗，周期额度）生命周期不同；`AUTH_RATE_LIMITED` 计的是**两个认证面共用**的同一来源认证失败（管理面与业务面都计数），它是凭据失败限速，**不得**当作调用方入口限流；`TOO_MANY_RUNS` 仍是实例级容量拒绝。
- 调用方认证失败统一 401 `UNAUTHORIZED`，不区分"没带 Key / 无效 / 过期 / 已撤销"，不泄露 Key 是否存在。
- 跨调用方访问他人会话 / 历史 → **404** `NOT_FOUND`（不是 403），不泄露资源存在性；管理端点收到 caller key → 403 `FORBIDDEN`。

**错误文案纪律**：面向运维，点名"要改什么"；**不得**回显凭证、绝对路径、堆栈。沙箱拒绝的文案是**模型可见契约**（四段式，见技术方案 §4.3），与其他错误的读者不同——它的读者是模型，目的是让模型停止重试并告知人类。

**语言分工（US-12 R8 起）**：错误码、HTTP 状态与生命周期拒绝语义是契约，**不随文案语言变化**。

- 内部错误值、系统日志模板与原因分类一律**英文**：日志要可检索、可聚合，并能与上游 SDK 的原始错误直接对照
- 管理台与调用方可见的响应文案一律**中文**，写明当前状态与修复建议
- 中文文案只由**类型化 reason → 中文映射**产生（枚举分支，见 `internal/web/errors_zh.go`）；**不得**匹配英文错误字符串、按前缀推断，或依赖 `error.Error()` 的措辞
- 执行历史展示使用**安全中文投影**：只呈现受控的状态与原因短语，**不回显原始错误文本**
- 同一故障"日志英文、页面中文"是设计使然而非缺陷；验收**必须**同时检查中文界面与英文日志

---

## 9. 管理面认证、条件写与管理操作

### 9.1 配置键

管理面认证配置位于 `basalt.yaml` 的 `http.admin_auth` 段。键名是**固定字面量**；缺键、非法取值或无法解析都拒绝启动，**不得**静默取默认值。

| 键 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `http.admin_auth.mode` | string | **是** | `token` / `local_trust`。未显式配置 → 拒绝启动；默认监听 `127.0.0.1` **不自动**等于已启用管理认证 |
| `http.admin_auth.token` | string | `token` 模式必填 | 只允许 `${ENV_VAR}` 引用；解析后非空。明文 token、空值或未解析占位 → 拒绝启动 |
| `http.admin_auth.allowed_hosts` | []string | 否 | 允许的 `Host`（含端口）。缺省按监听地址派生 `127.0.0.1:<port>` / `localhost:<port>` / `[::1]:<port>`；非回环部署必须显式列出 |
| `http.admin_auth.allowed_origins` | []string | 否 | 允许的浏览器 Origin，**完整匹配**（`scheme://host[:port]`）。缺省与缺省 Host 的 `http://` 形态一致 |
| `http.admin_auth.trusted_proxies` | []string | 否 | 可信反向代理的 CIDR。只有来自这些地址的转发头才可用于**来源归因**；**不得**用转发头证明回环本机信任 |

- `mode=local_trust` 只在**所有监听地址都是回环**时允许；否则拒绝启动。
- `mode=token` 且 token 未配置 / 解析为空 / 仍含 `${...}` → 拒绝启动。
- 浏览器 token 只在**页面内存**保存；不得写 localStorage / sessionStorage / cookie / URL / 日志 / 审计。
- admin token 不是调用方 API Key，也不是企业 SSO / RBAC；共享 token 只证明持有管理凭据，不提供按人追责。

### 9.2 认证与请求保护

| 模式 | 凭据 | 适用 |
| --- | --- | --- |
| `token` | `Authorization: Bearer <token>` | 回环与非回环管理部署；非回环必须经 HTTPS 或明确的可信加密通道 |
| `local_trust` | 直接回环连接 + `X-Basalt-Local-Trust: 1` + Host / Origin 校验 | 仅直接回环；**不得**经反向代理 / 隧道使用 |

无论哪种模式，管理请求都必须满足：

1. Host 在白名单；缺失或不匹配 → 403 `FORBIDDEN`
2. 浏览器来源的 Origin 必须完整匹配白名单；缺失 / `null` Origin **不得**单独作为放行依据
3. 写请求（`POST` / `PUT` / `PATCH` / `DELETE`）必须是 JSON 内容类型（明确声明的端点除外），并经正文大小上限
4. 转发头只信 `trusted_proxies`；**不得**因为 `X-Forwarded-For` 里出现 `127.0.0.1` 就判为回环本机信任
5. 反向代理 / 转发**不得**因上游连接是回环而绕过认证；认证结论只看直接对端 + 配置模式
6. 标准库 `http.CrossOriginProtection` 只覆盖其声明范围，不能代替 admin 认证或 Host 策略

- 凭据缺失 / 无效：401 `UNAUTHORIZED`，响应不区分两种情形
- 来源 / Host / Origin 被拒，或已认证但无权执行：403 `FORBIDDEN`
- 同一来源 60 秒内失败 10 次：429 `AUTH_RATE_LIMITED`，`Retry-After` 给出剩余秒数；计数在进程内，重启清零。
  管理面与业务面共用同一个来源限速器（凭据失败与 `local_trust` 的 403 计数；Host / Origin 的 403 在认证之前，不计数）
- `/api/v1/health` 与静态页面壳可公开；其余管理数据读面与**所有可触发执行 / 修改的端点**（含 `POST /sessions`、`POST /sessions/{id}/messages`、`POST /agents/{name}/invoke`）一律认证

### 9.3 管理操作标识与审计

- 操作标识格式：`op-<unix_millis>-<6 位小写字母数字>`，全局唯一、可排序；**不是** `run_id`，不得伪造成 Run
- 预写：变更前插入 `pending`；预写失败 → **不得执行变更**，向调用方返回 500 `INTERNAL`（审计不可用）
- 终态：`success` / `failed`；失败原文截断 4 KB，且不得含凭证
- 记录：`action`、`target_kind`、`target_id`、`actor_subject`、`auth_method`、`source`、`requested_version_id`、`expected_etag`、脱敏 `detail_json`、时间
- actor：
  - `bearer` → `actor_subject = token:<sha256(token) 前 16 位十六进制>`；token 明文不落盘
  - `local_trust` → `actor_subject = local_trust`，语义为“未验证个人身份”
  - CLI → `actor_subject = cli:<本机用户>`；不冒充管理台登录身份
  - 迁移 → `actor_subject = migration`
- `action` 字面量（点号分隔，`<resource>.<verb>`）：`agent.create` / `agent.content.update` / `agent.files.update` / `agent.generate_files` / `agent.disable` / `agent.enable` / `agent.archive` / `agent.copy` / `agent.restore` / `schedule.enable` / `schedule.disable` / `model_connection.create` / `model_connection.update` / `model_connection.delete` / `model_connection.test` / `model_connection.publish` / `model_connection.credential.write` / `model_connection.credential.revoke` / `model_connection.scope` / `notify_channel.test` / `mcp_server.create` / `mcp_server.update` / `mcp_server.delete` / `mcp_server.test` / `mcp_server.publish` / `mcp_server.apply` / `mcp_server.credential.revoke` / `skill.create` / `skill.import` / `skill.update` / `skill.delete` / `skill.publish` / `notify_channel.create` / `notify_channel.update` / `notify_channel.delete` / `notify_channel.credential.write` / `notify_channel.credential.revoke` / `notify_channel.scope` / `system.baseline_v2` / `system.baseline_v3` / `system.secrets_init` / `network_allowlist.update` / `network_denylist.update` / `network_exception.create` / `network_exception.delete` / `execution_policy.update` / `execution_policy.confirm` / `caller.create` / `caller.key.issue` / `caller.key.revoke` / `caller.key.rotate` / `caller.limit.update` / `caller.reservation.reconcile` / `instance.adopt` / `memory.propose` / `memory.confirm` / `memory.correct` / `memory.delete` / `knowledge_document.upload` / `knowledge_document.reindex` / `knowledge_collection.description_update` / `skill.admission.propose` / `skill.admission.evaluate` / `skill.admission.approve` / `skill.admission.reject` / `skill.admission.cancel` / `skill.admission.publish` / `skill.admission.withdraw`
- `target_kind` 字面量：`agent` / `schedule` / `model_connection` / `mcp_server` / `skill` / `notify_channel` / `system` / `network_allowlist` / `network_denylist` / `network_exception` / `execution_policy` / `caller` / `instance` / `memory` / `knowledge_upload` / `knowledge_document` / `knowledge_collection` / `skill_admission`
- 三个 `knowledge_*` 动作的落点：`knowledge_document.upload` 的 `target_kind = knowledge_upload`、`target_id` = 源文件相对路径；`knowledge_document.reindex` 的 `target_kind = knowledge_document`、`target_id` = `document_id`；`knowledge_collection.description_update` 的 `target_kind = knowledge_collection`、`target_id` = `collection_id`。三者的 `detail_json` 只记标识、版本号与结果分类，**不**记文件正文、检索文本或片段内容
- `caller.*` 动作的 `target_id` = `caller_id`（Key 是调用方的从属凭据，不是独立资源）；`detail_json` 只记 `key_id` / `key_prefix` / `scopes` / `expires_at_unix_ms`（0 = 不过期）/ 轮换的 `old_key_id` 与再绑定结果（`rebound_task_ids` / `left_bound[{task_id, reason}]`）以及吊销 `reason`。Key 明文**不得**出现在 `action` / `target_id` / `detail_json` / 错误原文的任一位置
- 凭据与范围动作的 `target_id` = 资源 `resource_uid`（不是名称）；`detail_json` 只记资源名、`credential_id`、版本号、模式与状态，**不记明文 URL / 密钥 / 主密钥**。资源更新若同时改变凭据，按主操作写一条 `*.update`，不重复拆成两条互不相关的动作
- `network_allowlist.update` 的 `target_id` = `policy_scope` 固定字面量 `http.allowed_domains`；`detail_json` 只记 `before_revision` / `after_revision` / `candidate_sha256`（规整域名按行连接的摘要），供启动恢复三态判定，不另存请求体
- `network_denylist.update` 与上一条逐字同口径，`target_id` = `http.denied_domains`；白名单与禁止域名共用一张表但**不共用动作名**，读侧按 `target_kind` + `target_id` 就能区分两行
- `network_exception.create` / `network_exception.delete` 的 `target_id` = `exception_id`；`detail_json` 记 `kind` / `value` / `note`（放行理由必填，属审计内容）与前后集合摘要，**不**记请求体其余字段
- `execution_policy.update` 的 `target_id` = 固定字面量 `global`；`detail_json` 只记 `before_profile` / `after_profile` / `before_revision` / `after_revision`，**不**放待确认项正文
- `execution_policy.confirm` 的 `target_id` = 待确认项 id（`<axis>:<value>`）；`detail_json` 只记 `axis` 与 `decision`，**不**记解析到的地址集合（它随时会变，落库只会制造陈旧证据）
- `mcp_server.apply` 的 `target_id` = 资源 `resource_uid`；`detail_json` 固定键集为 `target_version_id` / `previous_version_id` / `applied_version_id` / `outcome` / `failure_class` / `superseded_by` / `drain_deadline`，**不**记 command / args / env / 头值
- `mcp_server.credential.revoke` 的 `target_id` = 资源 `resource_uid`，`detail_json` 记 `binding_key`、被撤销的版本号、撤销前后 `revocation_epoch` 与状态；**不**记值、不记 `last4` 以外的任何片段
- `system.secrets_init` 的 `target_id` = 固定字面量 `master_key`；`detail_json` 只记 `key_id`、`fingerprint`（可公开的密钥材料摘要）与 `recovered`（是否走了残留恢复），**不**记路径、不记密钥材料
- `instance.adopt` 的 `target_kind` = `instance`，`target_id` = 固定字面量 `instance_identity`；`detail_json` 固定键集为 `candidate_instance_id` / `candidate_instance_id_sha256` / `case`（四判定编号 1～4）/ `key_path_persisted` / `key_path_config_key` / `managed_ciphertexts`。候选标识必须在**预写**时就写全，崩溃后只按它查证；**不**记密钥路径的值、密钥材料、密文或明文
- 四个 `memory.*` 动作的 `target_kind` = `memory`，`target_id` 表达分层（全局层固定字面量 `global`，Agent 层 `agent:<name>`），条目主键只进 `detail_json`。`detail_json` 固定键集为 `entry_id` / `replaced_id` / `section` / `agent` / `state_before` / `state_after` / `content_sha256`；**正文不得**进 `detail_json`（记忆正文可能是用户隐私），只留规范化正文的 sha256。`memory.propose` 在预写时还拿不到新主键（领域层才分配），`entry_id` 允许为 0；命中去重时它是既有条目的主键，`state_after` 取该行的真实状态（新增行恒为 `candidate`；合并进 `active` 记忆时为 `active`，**不得**伪造成候选）
- `memory.confirm` 自 US-22 起**追加**两个**可选**键（其余三个 `memory.*` 动作不写它们）：`resolution`（`keep_both` / `supersede`；空 body 归一化为 `keep_both` 后照写）与 `supersedes_id`（supersede 成功后被退役的旧条目主键；未替代时省略）。旧确认路径不写这两个键，旧操作行与旧客户端照常可读；**不新增**动作字面量，自动候选仍走 `memory.confirm`。语义与门禁见 [memory-learning.md](memory-learning.md) §6 / §7
- 七个 `skill.admission.*` 动作的 `target_kind` = `skill_admission`，`target_id` = `candidate_id`，`detail_json` **固定为 `{}`**——证据在候选目录、评估 / 决策 / 发布行与 `request_hash` 里，不在审计详情里重抄正文。`request_hash` 逐动作固定：`propose` 取 `[skill_name, candidate_hash]`、`evaluate` 取 `[candidate_id, candidate_hash]`、`approve` / `reject` 取决策文件原始字节的 SHA-256、`cancel` 取 `[candidate_id, revision, reason]`、`publish` 取 `[candidate_id, decision_id]`、`withdraw` 取 `[candidate_id, version_id, reason]`（`AdmissionRequestHash` = 对 `json.Marshal([]string)` 取 SHA-256）。失败原文同样截断 4 KB，且**不得**含凭证、候选文件正文或来源声明全文。准入动作不产生 Run：它们既不是模型调用也不是工具调用，三种审计互不混写
- 只读浏览、列表、校验（`/skills/{name}/validate`）不写管理操作审计；模型连接测试**要写**（它是可能计费的模型调用，但没有 Run）；通知渠道测试**也要写**（它会对外发送一条 `Basalt 连通性测试`），同样不产生 Run、不伪造模型工具调用
- `agent.generate_files` 字面量保留，但该端点**不写**管理操作行（唯一写入是 LLM 调用审计）；实现**不得**据字面量补恢复行、伪造操作或把草稿当变更
- 管理操作审计与模型 / 工具调用审计**互不混写**；同一请求不得既写 `management_operations` 又把变更伪装成一次 Run

### 9.4 管理操作的恢复

启动时持工作区实例锁，在开放服务前扫描 `status='pending'` 的管理操作。**只查证，不重做**：

| action 组 | 查证对象 | `applied` | `not_applied` | 仍不确定 |
| --- | --- | --- | --- | --- |
| `agent.content.update` / `agent.files.update` / `agent.create` / `agent.copy` / `agent.restore` | 目标文件树 / 状态文件的内容哈希与 operation_id | 哈希与 `detail_json` 的候选一致 | 目标仍为旧哈希或不存在 | 部分文件匹配、目标被第三方改写 |
| `agent.disable` / `agent.enable` | 状态文件的 `state` + `generation` + `operation_id` | 三者与候选一致 | 仍是旧状态 / 旧 generation | 文件缺失或校验和损坏 |
| `agent.archive` | 归档目录存在且清单哈希一致，状态为 `archived` | 是 | 目录仍在活动区且状态为旧值 | 归档目录存在但清单不完整 |
| `schedule.enable` / `schedule.disable` | `scheduled_tasks.enabled` 与 operation_id | 目标值已写 | 仍是旧值 | 行不存在或多行冲突 |
| `model_connection.*` / `mcp_server.*` / `skill.*` | 定义文件 / 版本行 / 发布指针 / 删除标记 | 指针或删除标记指向候选版本 | 仍指向旧版本 / 未删除 | 文件与指针不一致 |
| `notify_channel.*` | 行存在性与脱敏字段哈希 | 与候选一致 | 仍是旧值 / 不存在 | 行存在但字段被第三方改写 |
| `model_connection.credential.write` / `model_connection.credential.revoke` / `model_connection.scope` / `notify_channel.credential.write` / `notify_channel.credential.revoke` / `notify_channel.scope` / `mcp_server.credential.revoke` | **四证**：`operation_id`、DB 提交事实（凭据行 / 版本行 / `revocation_epoch` / 资源 `revision`）、凭据版本号、目标绑定哈希（MCP 逐键撤销另含 `binding_key`） | 四证一致且全部指向候选终态 | 可证明未提交：旧状态完好且无候选版本行 | 任一证据缺失、彼此矛盾或只对上一部分 → `unknown`；**不得**重新加密、重放撤销或自动补写 |
| `model_connection.test` / `mcp_server.test` / `notify_channel.test` | `detail_json` 是否已有完整结果 | 已有结果 | 未发出调用（可证明） | 无法区分是否已发出 → `unknown`，**不得重发**；通知测试即使成功也不补发第二条消息 |
| `system.baseline_v2` | 各资源基线版本 / 指针 / 状态文件 | 全部完成 | 可证明未开始 | 部分完成 → 继续补齐缺失项（确定性幂等，不是重做已生效变更） |
| `system.baseline_v3` | 活动区各 Agent 状态文件的 `schema_version` 与 `agent_uid`，以及 `agent_lifecycle` 镜像 | 全部为 schema 2 且 UID 合法、镜像一致 | 全部仍为 schema 1 / 无 UID（可证明未写入） | 部分升级、文件损坏或镜像与文件不一致 → `unknown`；**不得**给同一 Agent 补发第二个 UID |
| `network_allowlist.update` / `network_denylist.update` | 对应 `network_domain_policy` 单行（`policy_scope` 分别为 `http.allowed_domains` / `http.denied_domains`）的 `revision` 与域名摘要，对照 `detail_json` 的 `before_revision` / `after_revision` / `candidate_sha256` | `revision = after_revision` 且摘要与 `candidate_sha256` 一致 | `revision = before_revision` | 行缺失、读库失败或 `revision` 是第三个值 → `unknown`；**不得**重放写入 |
| `network_exception.create` | `network_exceptions` 行（按 `exception_id` + `operation_id`） | 行存在且 `operation_id` 一致 | 无该行（可证明未写入） | 行存在但 `operation_id` 不一致（同 `(kind, value)` 已被他人占用）→ `unknown`；**不得**改写已有行 |
| `network_exception.delete` | 该 `exception_id` 是否仍存在，以及集合摘要 | 行已不存在 | 行仍在且内容未被改写 | 行不存在但同 `(kind, value)` 已出现 `operation_id` 不同的新行 → `unknown`；**不得**再删一次 |
| `execution_policy.update` | `execution_policy` 单行（`policy_scope = 'global'`）的 `profile` 与 `revision`，对照 `detail_json` 的 `before_profile` / `after_profile` / `before_revision` / `after_revision` | `profile = after_profile` 且 `revision = after_revision` | `profile = before_profile` 且 `revision = before_revision` | 档位与修订号分别对上前 / 后两侧，或 `revision` 是第三个值 → `unknown`；**不得**重放 |
| `execution_policy.confirm` | 只查证处置行本身：是否存在同一 `target_id` 且 `status = 'success'` 的 `execution_policy.confirm` 行 | 存在 `success` 行 | **不存在**（只有本 `pending` 行或无行） | 存在多个互相矛盾的处置行 → `unknown`。本动作没有"半个处置"：失败的复核按 fail-closed 记 `failed`，条目继续留在待确认列表，由人重做；**不得**把 `pending` 行补成 `success` |
| `mcp_server.apply` | `applied` 指针（`applied_version_id`）与注册 epoch，对照 `detail_json` 的 `target_version_id` / `previous_version_id` | 指针 = `target_version_id` | 指针 = `previous_version_id` | 指针是第三个版本，或版本行已不存在 → `unknown`。候选注册与候选连接是进程内的，重启后自然消失；**不得**据 `pending` 行重放 `apply` |
| `system.secrets_init` | `instance_secrets` 记录与密钥文件（走完整 `LoadMasterKey` + 来源可证明的残留恢复规则） | 记录存在、`key_id` 一致、文件校验通过且 `nlink` 已回到 1 | 记录不存在且可证明未创建任何文件（含临时链接） | 记录缺失但文件存在、或文件存在而来源不可证明 → `unknown` 并进恢复态；**不得**删除文件、**不得**生成第二把密钥 |
| `instance.adopt` | 工作区名册 `instance.json` 的 `instance_id`，对照 `detail_json` 的 `candidate_instance_id`（该键缺省时才用 `candidate_instance_id_sha256`） | 名册存在且标识与候选逐字相等（或摘要相等） | 名册不存在（可证明未写入） | 名册存在但标识是第三个值、`detail_json` 解析失败、两个候选键都缺 → `unknown`。**不得**覆盖或补写已有名册，**不得**重做采纳 |
| `skill.admission.propose` | 候选行（按 `target_id` = `candidate_id`）与 `.basalt/admissions/candidates/<candidate_id>/` 目录 | 候选行存在 | 行不存在且目录不存在（可证明未写入） | 行不存在但目录存在（孤儿目录，交人工检查）或读库失败 → `unknown`；**不得**删目录、**不得**补插候选行 |
| `skill.admission.evaluate` / `skill.admission.approve` / `skill.admission.reject` / `skill.admission.publish` / `skill.admission.withdraw` | 按 `operation_id` 查评估行 / 决策行 / 发布记录；`withdraw` 按 `withdraw_operation_id` 查发布记录 | 行存在 | 行不存在（可证明未提交） | 查询本身失败 → `unknown`；**不得**重放评估、决策、发布或撤回，**不得**据 `pending` 行补写记录 |
| `skill.admission.cancel` | 候选状态与 `terminal_operation_id` | `state = canceled` 且终态操作 = 本 `operation_id` | 仍是 `proposed` / `evaluated` / `approved` | 其他状态、终态操作指向别的 `operation_id`，或候选行读不到 → `unknown`；**不得**补写取消、**不得**改写已有终态 |
| `memory.propose` / `memory.confirm` / `memory.correct` / `memory.delete` | `memory_entries` 行：按 `detail_json.entry_id` 读目标（提议预写时主键未知，改按同层同分区 + `content_sha256` 认领），对照 `state_before` / `state_after` / `content_sha256`；纠正按 `supersedes_id` = 目标 + 正文摘要认领新条 | 状态已到后态（确认 → `active`、删除 → `deleted`、纠正的旧条 → `corrected`，或新条 `supersedes_id` + 摘要对上） | 目标仍是 `state_before`；或提议在同层同分区找不到任何正文摘要相同的行（可证明未写入） | 目标存在但层 / 分区 / 正文摘要与 `detail_json` 不符；或提议找到同源行（合并**无法**归属本操作，不得据此判 `applied`）→ `unknown`。**不得**重放提议 / 确认 / 纠正 / 删除 |
| 学习抽取（US-22，**独立账本**，不是 `management_operations` 行） | `memory_extractions` 的 `pending` 行：按 `extraction_id` 查 `memory_candidate_meta` 与 `memory_method_drafts` 是否已有该行 | 已有候选元数据**或**草案行 → `success` | —（零条目提交与未提交不可区分，**不判** `not_applied`） | 两者都不存在 → `unknown`。两种结论都在同一事务写 `status` + `verified_at` 并推进来源游标；**不得**重读消息、重调模型、删草案目录或补插候选行。完整规则见 [memory-learning.md](memory-learning.md) §8 |

- `applied` → `success`；`not_applied` → `failed`；其余 → `unknown`；写 `recovered_at` 与 `verification`
- 恢复**不得**重放模型 / 工具调用，也**不得**重发连接测试；凭据动作**不得**重做加密、重新生成密文或补写凭据版本——四证不足时停在 `unknown`，由人核对后显式重做
- `unknown` 操作在管理台单独列出，由人判断；不得自动删除或再次执行
- 恢复完成后，该操作的 `pending` 行不再参与后续启动扫描

### 9.5 条件写（ETag / If-Match）

- 强 ETag 值：`"sha256:<64 位小写十六进制>"`（含双引号）；弱 ETag 与 `*` 不被接受
- `GET /agents/{name}/content`、`GET /agents/{name}/files`、`GET /skills/{name}` 与资源详情返回当前 ETag；`PUT` / `DELETE` / `publish` / `POST /agents/{name}/files` 必须带 `If-Match`
- Agent 多文件提交使用**受管文件集合 ETag**（按 path 升序的 `(path, sha256(内容))` 规整序列化）；它与 `AGENT.md` 单文件 ETag 不可互换。不匹配 → 412，且**不落盘、不写管理操作行**；比较与 staging 构建在同一 Agent 写锁内完成
- `If-Match` 缺失、格式非法或不等于当前 ETag → 412 `PRECONDITION_FAILED`；**不得**用 409 `SESSION_BUSY` 表达
- 412 的 `data` 可包含 `current_etag` 与当前版本号，供客户端重新读取；不含文件正文
- 生命周期变更使用 `expected_state` + `expected_generation`（请求体）校验；不匹配 → 409 `CONFLICT`，不回退到“按名字猜状态”
- 条件写必须在同一写入闸门内“比较 + 写入”；先比较后写入之间不得插入第二次写
- **资源条件 ETag** 覆盖的不只是定义内容：还包括 `resource_uid`、`scope` / `owner_agent_uid`、凭据版本与状态、撤销序号与 `revision`。仅替换 Key、撤销凭据或改变范围也必须使 ETag 变化，否则并发替换无法被发现。Agent 的 `content_etag` 不吸收资源变化（见 [agent-md.md](agent-md.md) §2.3）
- 涉及资源身份的动作（范围转换、凭据撤销、删除）在 `If-Match` 之外还必须携带 `resource_uid`；与当前指针不一致 → `409 RESOURCE_UID_MISMATCH`，不按名称猜测目标
- `PUT /execution-policy` 有两个前置条件且**都要满足**：`If-Match`（等价于修订号 CAS，不符 → 412）与 `expected_profile`（用户确认过的方向，不符 → 409 `EXECUTION_PROFILE_MISMATCH`）。两者不可互相替代——ETag 只证明"没人改过"，`expected_profile` 证明"用户看到的就是当前值"
- 访问例外使用**集合 ETag**：`GET /network-exceptions` 返回的 `etag` 覆盖整份集合（按 `exception_id` 排序的规整摘要），`DELETE /network-exceptions/{id}` 的 `If-Match` 只能取这个值；空集合也有稳定 ETag。新增例外**不**要求 `If-Match`（它是追加而非覆盖），删除必须要求
- `POST /execution-policy/confirmations` **不**需要 `If-Match`：它只追加一条处置记录，不修改 `execution_policy` 行、不推进修订号；重复处置按幂等返回，不新增审计行

### 9.6 资源版本、发布与引用

- 资源种类 `kind` 固定为 `model_connection` / `mcp_server` / `skill`；`version_id` 全局稳定，`version_no` 逐资源从 1 递增
- **稳定身份**：每个资源实例有不可变 `resource_uid`（`resource_identities` 记录身份与墓碑）。名称只是当前标签：活动名称唯一；删除后允许同名重建，但生成**新 UID**，版本号从 1 重新计，旧 UID 的版本、凭据与引用保留在墓碑下可查。版本唯一键为 `(resource_kind, resource_uid, version_no)`，只按名称查询默认只回当前指针 UID 的版本
- 版本内容不可变；撤回 / 回滚 = **用旧版本内容创建新版本**，不回退 `version_no`、不改历史版本、不恢复已撤销的 `${ENV_VAR}` 解析值或已撤销的托管凭据
- Provider / 技能发布后对新准入 Run 立即生效；MCP 首版重启生效，`applied_version_id` 在重启时追平，管理面显示“待重启”
- **凭据按版本**：托管凭据的每个不可变版本绑定目标地址（AAD）；替换 Key 创建新版本，发布版本只记录发布时的活动凭据版本供审计。Run 准入固定凭据版本，调用检查点发现撤销即终止该 Run；通知每次发送解析当前 `active` 版本。撤销按**整个凭据身份的全部版本**执行（`revocation_epoch` +1），旧版本永不复活；重新录入生成新版本
- 删除资源前必须检查**绑定**（AGENT.md `resource_bindings` 中的 UID，含归档 Agent）、定义名引用与活动 Run 引用：被引用 → 400 `BAD_REQUEST` 并点名引用者，**不提供 force**；通知渠道保留既有 `?force=true`，但不得自动强删。删除只置墓碑（`deleted_at` / 身份 `state='deleted'`），不物理删除历史
- 版本回收只看 `run_resource_refs.released_at` 与发布指针：当前发布版本即使零 Run 也不得回收；历史引用行不删除。通知的 Run 冻结集合记录在 `run_notify_refs`（具体 `channel_uid`，无 `'*'` 通配），发送明细在 `notify_deliveries`（与 `tool_invocations` 一对一）
- Run 资源清单缺失的历史 Run 标 `legacy_unrecorded`；**不得**用当前版本补造关联。Agent 生命周期状态文件 schema v2 的 `agent_uid` 是身份权威，`agent_lifecycle` 索引与 `agent_archives` 只做镜像，索引可重建

---

## 10. 调用方凭据与配额（US-16 起）

### 10.1 两类凭据互不代换

| 维度 | 管理面（既有） | 业务面（US-16 新增） |
| --- | --- | --- |
| 凭据 | admin token（或 `local_trust` 回环直连） | 调用方 API Key：`Authorization: Bearer <key>` |
| 主体 | `core.LocalAdminPrincipal()` / `core.LocalAdminCLIPrincipal()`（`Kind=local-admin`） | `core.Principal{Kind=caller, CallerID, KeyID, Project, Scopes, Actor=caller}` |
| 端点 | 管理端点（`/agents`、`/resources`、`/callers`、`/diagnostics` 等） | 业务端点（`/sessions`、`/search`、`/sources/view`、`/history/search` 等） |
| 限流 | 管理认证失败计数 → 429 `AUTH_RATE_LIMITED` | 三类入口桶 → 429 `CALLER_RATE_LIMITED` |

- 两类凭据**不可互换**：caller key 访问管理端点 → 403 `FORBIDDEN`；admin token 不以调用方身份参与调用方配额，其请求以内置 `local-admin`（`AuthorizingKind=admin-builtin`、无 `authorizing_key_id`）执行。
- **零值 Principal 一律拒绝**（401）：**不得**把"没传身份"默认成管理员，也**不得**按请求体里的字段推断调用方。
- `callers:admin` / `diagnostics:read` **只随** admin token 的内置 `local-admin` 生效，**不得**出现在 `caller_keys`；签发时给这两个 scope → 400 `INVALID_SCOPE`。
- Key 只存 `key_hash`（SHA-256）与 `key_prefix`；明文只在签发 / 轮换响应出现一次。**不得**写进日志、审计、诊断、导出或错误文案。

### 10.2 scope 字面量（固定枚举）

固定为 `sessions:read` / `sessions:write` / `knowledge:read` / `knowledge:write` / `agents:invoke:<name>` / `agents:invoke:*`，以及仅管理面可用的 `diagnostics:read` / `callers:admin`。

- 空 scope 的 Key **不得**签发；未知 scope 在签发 / 轮换时即 400 `INVALID_SCOPE`，零写入。
- 端点未声明所需 scope 视为配置错误，启动自检失败（**不得**默认放行）。
- `agents:invoke:<name>` 无匹配 → 403 `FORBIDDEN`；`agents:invoke:*` 只允许 admin 发放。
- 归属与授权分开：`Principal.CallerID` 始终是任务归属方（配额记在它名下），`Principal.Actor` 只记录这次是谁执行（`caller` / `scheduler` / `recovery` / `admin-cli`）；`AuthorizingKind` 随任务持久化，**不得**运行时按"有没有 key_id"猜。

### 10.3 预留（Reservation）生命周期

- 状态只允许 `pending → committed / released / reconciled`；`pending` 继续占 `reserved`，**不**按零计量，也**不**由后台任务自动释放或自动结清。
- `Reconcile` 只由管理面发起，必须给理由并写管理审计；判为已消耗时 `actual` 大于 `amount` 记 `overage`，**不**截断、**不**回滚已发生的调用。
- `overage > 0` 期间拒绝一切新预留（429 `QUOTA_EXHAUSTED`），直到管理员通过 `POST /callers/{id}/usage/clear-overage` 填理由清零；清零**只**清 `overage`，不动 `used` / `reserved`，不删预留行。
- 周期按 `caller_limits.timezone` 的本地 00:00 起算（缺省 UTC），`period_start` 存 Unix 毫秒 UTC；`Commit` / `Release` / `Reconcile` **总是**更新创建时那个周期的行，**不得**按当前时间重算。
- 额度按 `CallerID` 计，**不按 `KeyID`**：同一调用方的多个 Key 共享额度，轮换 / 吊销单个 Key **不得**重置周期或清零用量。
- 配额服务（PG）故障 → **拒绝新的受控消耗**（fail-closed），不放行；在途 reservation 保持 `pending`，恢复后核对。

## US-20 执行隔离补充

执行隔离不增加顶层 `error_kind`，也不扩展四类 `policy_denial_reason`。隔离原因、生命周期账本、两档执行边界与只读投影见 [执行隔离契约](isolation.md)。取消/超时保留 context 错误身份；未知执行不得自动重试或降级本地。调用审计与消息仍由既有唯一执行路径写入。
