# 与 NivroOS 的兼容性对照

> 本文档回答一个问题：**同一份 Agent 资产、同一套使用方式，在两个底座之间能互换到什么程度。**
>
> 三态口径：每条对照项标 `保持一致` / `有意改变` / `暂不支持` 之一，**不允许含糊表述**（"基本兼容"这类词不出现）。
> 与 [技术方案](TechnicalSolution.md) 冲突时以技术方案为准；本文只记**差异与承诺**，重合内容（目录协议、工具契约、API 形状）不在此复制。

---

## 1. 基线与比较口径

| 项 | 值 |
| --- | --- |
| 被参照仓库 | NivroOS（Java / Spring Boot 实现） |
| 基线 commit | `50e1b30c4bbc451dac3ca94786b803522bcbad36`（2026-10-14，无 tag） |
| 基线内容 | US-1~US-7 全部能力 + MCP 真实链路演示；管理台前端入库；10 个内置工具 |
| 比较时间 | 2026-10-14 |

**口径**：

1. **矩阵只记差异与承诺**——两侧行为相同的项，在一行里合并陈述，不展开实现细节
2. **`保持一致` = 可互换**：同一份文件 / 同一次调用在两个底座上的结果相同（字符串逐字、行为等价）
3. **`有意改变` = 设计决定**：Basalt 主动改的，每一条必须能说出理由与迁移影响（清单见 §3）
4. **`暂不支持` = 功能缺口**：现在没有，必须写明登记去向（清单见 §4）——"暂不支持"**不得**被用作含糊其辞
5. 矩阵行**只用三态**；另有 §2.6「不适用」类——那些是 **Java 侧才存在的问题**，在 Go 下根本不存在，不算功能缺口

**迁移影响三档**：`无需动作` / `需要配置或手工动作` / `数据不迁移`。

**本文的角色**：本仓所有与 NivroOS 相关的**对照性叙述**（逐项对照、差异裁决、模块映射、有意改变清单）**集中在此**；四份权威文档与三件套只保留血缘一句话与兼容承诺句——引用纪律见 §6。

---

## 2. 对照矩阵

### 2.1 目录协议（工作区与 AGENT.md）

| 项 | 状态 | NivroOS | Basalt | 迁移影响 |
| --- | --- | --- | --- | --- |
| 工作区根目录名 | 有意改变 | `.nivroos/` | `.basalt/` | 手工改名即可：`mv .nivroos .basalt`，**文件内容零改动** |
| 工作区内部布局 | 保持一致 | `agents/` `skills/` `memory/` `mcp_servers.yaml` `logs/` `AGENTS.md` `SOUL.md` `USER.md` | 同构 + `archive/`（归档删除的落点） | 无需动作 |
| AGENT.md frontmatter 字段集 | 保持一致 | `name` / `description` / `identity` / `provider` / `tools` / `skills` / `mcp_servers` / `channels` / `bootstrap` / `settings` / `schedules` | 逐字相同 | 无需动作 |
| 一个目录 = 一个 Agent（目录名 = `name`） | 保持一致 | 是；不一致整只不注册 | 同 | 无需动作 |
| 技能绑定机制 | 保持一致 | frontmatter `skills:` 声明式（US-7 已决 18 起） | 同；本地 `skills/` 软连接同样无视 + WARN | 无需动作 |
| 技能库形态 | 保持一致 | `skills/<name>/SKILL.md` + 可选附属资源 | 同（路径前缀随根名变） | 无需动作 |
| 记忆分层 | 保持一致 | Agent 层 `memory/agents/<name>/MEMORY.md` + 全局回退层 `memory/MEMORY.md`；两层不叠加 | 同 | 无需动作 |
| 长期记忆门面工具 | 保持一致 | `save_memory` / `recall_memory`；`recall_memory` 传**单个关键词** | 逐字同（含"多词短语必不命中"这条已知行为） | 无需动作 |
| `manage_schedule` 与 `schedules:` 段 | 保持一致 | 第 10 个内置工具，只能改当前 Agent 自己的 `schedules:` | v0.1 同步补齐为第 10 个（**这是特意补的**：缺它则声明该工具的 Agent 在 Basalt 上因 `tool not registered` **整只不注册**） | 无需动作 |
| 通知渠道 | 保持一致 | SQLite 全局注册表；AGENT.md 正文按名引用；类型 → 适配器为装配层显式映射 | 同（表在 `basalt.db`） | 行数据不随文件走，需重新登记（见 §2.5） |
| 默认监听地址 | 有意改变 | Spring Boot 缺省绑所有接口的 `8080` | **`127.0.0.1:8080`**，远程访问须显式改配置 | 需要配置或手工动作（容器部署改一行） |
| 命令入口 | 保持一致 | `nivroos init / status / chat / serve / gateway / profile * / provider list / tool list / session list` | 同名同语义的 12 条命令（`basalt ...`）；其中 `profile create` / `profile delete` 会写工作区，属**写模式命令**——取实例锁、被活实例持有时拒绝启动（见 §3 第 6 条） | 无需动作（命令名换前缀） |

### 2.2 工具参数（模型可见契约）

10 个内置工具的**工具名逐字一致**；**共有参数**（两侧都存在的参数）的**名称与必填性逐字一致**；Basalt **新增**的参数一律在 [contracts/agent-md.md](contracts/agent-md.md) §5 标注「**扩展**」——不写「扩展」的参数**必须**与基线逐字同。这是 `AGENT.md` 可互换的前提，也是"参数逐字一致"这句话的精确边界（初稿曾把 `save_memory.scope` 改为必填 `section`、把 `http_post.body` 降为可选——两处均已改回与基线一致）。

| 工具 | 状态 | 说明 |
| --- | --- | --- |
| `read_file` / `write_file` / `list_dir` | 保持一致（参数）· **有意改变**（范围） | 参数名与类型逐字同，**无扩展参数**；**读写范围收窄**见下 |
| `shell` | 保持一致 | `executable` + `arguments` argv 契约、可执行名整串比对、不经 shell、超时杀进程树、UTF-8 宽容解码、CWD 语义（有 Agent 上下文 = 该 Agent 目录 / 无上下文 = 进程默认）逐字同；另有 `timeout_seconds`（**扩展**，可选，且只能是"缩短"——见效值为工具默认 / Profile 覆盖 / 剩余预算三者最小）；Basalt 另有**解释器受控通道**（`shell.interpreters`：只接受 `[<脚本路径>, <参数…>]` 形态、脚本须落在该 Agent `scripts/` 或技能库、`scripts/` 对模型只读——见 §3 第 20 条），`bash` / `sh` 两侧都不得放行 |
| `http_get` / `http_post` | 保持一致 | 共有参数同（**`http_post` 的 `body` 两侧都必须**——初稿曾误降为可选）；`http_get` 另有 `headers`、`http_post` 另有 `content_type` / `headers`（均**扩展**，可选）；域名白名单 + http/https scheme 固定；重定向策略由 Basalt **显式定义**——**逐跳**发送前校验、跳数上限 5、越界判 `sandbox_denied`（HTTP 客户端不得自行跟随）、白名单按域名匹配故 **IP 字面量目标直接拒**；`http_post` 为有外部副作用调用（承诺边界见 §2.4 与 contracts/run-and-errors.md §3.3） |
| `save_memory` / `recall_memory` | 保持一致 | `save_memory`：`content`（必填）+ `scope`（可选，`CORE` / `ARCHIVAL`，**省略 = `ARCHIVAL`**）与基线逐字一致——**没有**新增必填参数（初稿曾误改，已还原；注：此 `scope` 是"分区"，与库里 `memory_entries.scope` 的"分层"不是一回事）；`recall_memory` 参数同（单关键词检索） |
| `notify` | 保持一致 | 参数同（`channel` 与基线一致为 schema 可选、运行期无默认渠道即失败）；飞书**只认响应体 `code == 0`**（投递失败也回 HTTP 200）；渠道类型未装配报专用错误 |
| `manage_schedule` | 保持一致 | `action` / `id` / `cron` / `zone` / `message` 参数同；作用域边界同（只能改自己的 `schedules:`） |
| 沙箱拒绝文案 | 保持一致 | 四段式 `<规则>：<越界值>。这是安全策略，请勿反复重试；<放行指引>`，逐字保真且点名配置键 + 需重启 |
| **文件读写范围** | **有意改变** | NivroOS：读 = `agents/` + `skills/` 两根；写同两根（另拒写 `AGENT.md` 与技能库）。Basalt：**读 = 当前 Agent 自己的目录 + 技能库；写 = 当前 Agent 自己的目录**；跨 Agent 读取须在 `sandbox.cross_agent_read` 显式授权（授权方 → 被授权读的 Agent 名列表），未授权即 `sandbox_denied` |
| **子进程环境变量** | **有意改变** | NivroOS：子进程继承调用进程完整环境。Basalt：**只透传白名单变量**（`shell.env_passthrough`），其余不继承 |
| 工具输出上限 | 保持一致 | 单次输出截断 256 KB；Basalt 另在回填内容里标记 `truncated: true` |
| 失败分类 | 有意扩展 | NivroOS：`success` + `error_message` 自由文本。Basalt：另有 `error_kind` 七类（`sandbox_denied` / `timeout` / `not_found` / `invalid_input` / `external_error` / `runtime_error` / `unknown`） |

**文件读写范围收窄的迁移影响**：多个 Agent 互相读取对方目录的工作流（例如共享一份参考文档）在 Basalt 上**默认不可用**，需要在配置里显式授权或把共享内容放进技能库。这是**有意为之**——"一个 Agent 能读遍所有 Agent 的提示词与脚本"不是应该默认成立的性质。

### 2.3 API

| 项 | 状态 | 说明 |
| --- | --- | --- |
| 统一前缀 / 信封 | 保持一致 | `/api/v1`；`{code, message, data, timestamp}` 四字段 |
| 核心 10 端点 | 保持一致 | 路径、方法、语义逐条一致（见 [contracts/openapi.yaml](contracts/openapi.yaml)） |
| 管理台追加端点 | 保持一致 | Skills / MCP / Notify / Sandbox / 会话列表 / Agent 记忆 / schedules×4 / Agent 动态管理×4 / workspace×2 / notify CRUD×5——与 NivroOS US-7 终态同集合 |
| 运行查询与取消 | **有意扩展** | 新增 `GET /runs`、`GET /runs/{run_id}`、`POST /runs/{run_id}/cancel`（NivroOS 无运行视图） |
| 错误码 | **有意扩展** | 新增 `LIMIT_REACHED`(200)、`SESSION_BUSY`(409)、`TOO_MANY_RUNS`(429)、`UNSUPPORTED_TYPE`(400)；其余（`BAD_REQUEST` / `NOT_FOUND` / `TIMEOUT` / `NOT_CONFIGURED` / `INTERNAL`）同 |
| 消息与历史限制 | 保持一致 | 单条消息 ≤ 32 KB；会话历史返回最多最近 100 条；单次调用最长 60 秒（超时 504） |
| 排序参数 | 保持一致 | **两侧都不暴露**排序参数，顺序由服务端固定（安全约束，不是简化） |
| 管理台静态资源 | 保持一致 | `go:embed`（Basalt）/ jar 内资源（NivroOS）；前端同为 Vue + Vite 构建产物**入库**；模型产出文本一律经自研安全渲染器 |
| 认证 | 保持一致 | 核心阶段都没有（内网假设）；限流、RBAC 同属扩展阶段 |

### 2.4 运行行为

| 项 | 状态 | NivroOS | Basalt | 迁移影响 |
| --- | --- | --- | --- | --- |
| ReAct 循环 | 保持一致 | 自实现、同步阻塞、无框架 Agent 抽象 | 同（Go 侧同样手写，不用任何 agent 框架） | 无需动作 |
| 达 `max_iterations` | **有意改变** | 返回一段中文提示文本作为**正常响应**——调用方无法区分"答完了"与"没答完" | `status = limit_reached` + **部分结果**；HTTP **200**（不是 500，服务没故障） | 需要配置或手工动作：靠解析响应文本判成败的调用方改看 `status` 字段 |
| 同会话并发 | **有意新增** | 无显式契约（同会话并发请求行为未定义） | **同会话串行**：忙则 409 `SESSION_BUSY`（不排队）；实例级并发上限 → 429 | 需要配置或手工动作：并发调用的客户端需处理 409 |
| 定时任务并发 | 保持一致 | 每任务一把进程内锁，上一轮未完成则**跳过**本次触发 | 同（跳过记日志，不产生 409——定时器没有交互方） | 无需动作 |
| 取消 | **有意新增** | 核心阶段无取消路径（同步阻塞到底） | `ctx` 取消：每轮开头与工具执行前检查；shell 向进程组发信号 → 强杀进程树；已完成的调用照常落审计 | 无需动作 |
| 审计写入策略 | **有意改变** | best-effort（写失败不阻断执行） | **fail-closed**：`llm_calls` 与 `tool_invocations` **同一套两阶段写**——执行前先插 `status=pending`，**插入失败即拒绝发出该次调用 / 执行该工具**；调用后更新为终态 | 无需动作（对外行为不变，只在审计不可用时表现为拒绝执行） |
| 崩溃恢复 | **有意新增** | 无状态列，无恢复语义 | **工作区实例锁**（`.basalt/basalt.lock`，**操作系统级独占文件锁**——心跳只作诊断、**不得**据心跳过期抢锁）决定"谁来恢复"：只有拿到锁的启动进程做扫描（`running` → `failed`、两表 `pending` → `unknown`），写模式命令在锁被活实例持有时拒绝启动、只读命令不触发恢复；未完成的工具交互组以**消息**为入口**按位置**扫出（assistant 组 + 其后 `seq` 紧邻的连续 tool 段；**不得**全会话按 `tool_call_id` 查找——旧 Run 同 id 的结果行会让新组被误判完成）、**与 Run 状态修复同一个短事务**补齐为显式结果（会话可继续）；补组后再扫 `runs` 收尾**没有半截组可补**的 `running` 行（等首次 LLM 响应 / 最终回答已落库 / 无状态 invoke）——**组看消息、Run 看表** | 无需动作 |
| 审计关联 | **有意扩展** | `llm_calls` 九列，无运行关联、无成败 | 增 `run_id` / `round` / `status` / `error_kind` / `config_hash`；`tool_invocations` 增 `run_id` / `tool_call_id` / `status` / `error_kind`（成败只由 `status` 列表达，不另设布尔列） | 数据不迁移 |
| 配置生效时机 | **有意改变** | 每次重读（同一轮内可能读到旧 Profile × 新正文的混搭） | **校验通过的不可变配置快照**：一次 Run 内配置冻结，磁盘此刻怎么改都不影响运行；发布原子替换、失败保旧 | 无需动作（对外只表现为行为更可预期） |
| 热加载范围 | 保持一致 | agents 与 skills 目录感知；`mcp_servers.yaml` 变更需重启 | 同（fsnotify 非递归，逐目录 watch + 新目录补 watch + 漏事件兜底 rescan） | 无需动作 |
| 执行预算 | **有意扩展** | 仅 `max_iterations` 与工具超时 | 五项预算：Run 总超时 / 工具超时 / 工具调用次数 / 单工具输出上限 / 上下文预算 + 同会话串行 + 实例级并发上限 | 需要配置或手工动作（长任务可能撞 `RunTimeout`，见 contracts/run-and-errors.md §6） |
| 外部副作用重试 | **有意明确** | 未明示 | 承诺边界**逐字**见 contracts/run-and-errors.md §3.3：**运行时不自动重试、恢复不自动重放**；但客户端手工重发同一消息 = **新的一次 Run**，服务端无法识别重传（v0.1 不做幂等键，登记为扩展阶段） | 无需动作（是显式声明，不是新限制） |
| 渠道凭证形态 | **有意改变** | `notify_channels.url` 存**完整凭证地址** | `url` 列存 `${ENV_VAR}` **引用**，发送时解析；对外一律只回 scheme + host | 行数据不迁移（重新登记，地址从环境变量注入） |

### 2.5 数据库与迁移

| 项 | 状态 | NivroOS | Basalt | 迁移影响 |
| --- | --- | --- | --- | --- |
| 库引擎 | 保持一致 | SQLite（WAL） | SQLite（WAL，`modernc.org/sqlite` 纯 Go） | 无需动作 |
| 会话历史结构 | **有意改变** | `sessions.messages_json` 一列存整段 JSON | `sessions` 元数据 + **`messages` 逐条追加表**（承载 tool_calls / tool_call_id / reasoning_content / provider 扩展） | 数据不迁移 |
| 审计表结构 | **有意改变** | `llm_calls` 九列 / `tool_invocations` 九列 | 扩列 + 新增 `runs` 表（运行主表） | 数据不迁移 |
| 新增表 | 有意扩展 | — | `runs` / `config_snapshots` / `schema_version`（+ `memory_entries` 供 sqlite 记忆档） | 无 |
| 迁移机制 | **有意改变** | 幂等 `schema.sql` 手工建表（无版本表；SQLite `ALTER TABLE` 支持弱，此前明确不依赖 ORM 自动迁移） | `schema_version` 表 + **受控前向 SQL**：启动时逐版迁移；已发布迁移不得修改 | 无需动作 |
| 库级直迁（NivroOS 库 → Basalt 库） | **暂不支持** | — | 结构不同，不做自动转换 | 数据不迁移：**从新库开始**；如需保留历史会话，导出为只读文本归档（会话历史不影响运行） |

> 迁移结论一句话：**文件资产（`agents/` / `skills/` / `memory/` / bootstrap 四件）是主要互换面，数据库不互换。** 换底座 = 拷目录 + 重新登记渠道行 + 从新库开始。

### 2.6 不适用项（Java 侧才存在的问题）

这些**不是** Basalt 的缺口——是问题本身在 Go 下不存在，因此不计入三态矩阵，仅备案供迁移对照：

| NivroOS 的坑 | 在 Basalt 下 | 为什么 |
| --- | --- | --- |
| Windows 控制台编码分流（`adaptStdoutEncoding` / `Console` 假不了，需跨进程外侧控制台实测） | 不适用 | Go 运行时在 Windows 控制台按 UTF-8 收发（控制台经 `WriteConsoleW` 转换），无"同一进程两套编码"的问题 |
| `getBeansOfType` 整表枚举物化 prototype Bean 导致启动失败 | 不适用 | 手写装配，没有容器扫描 |
| Spring AI eager 自动装配索要 api-key（`autoconfigure.exclude` 排除六个自动配置） | 不适用 | 构造期显式装配 Provider，没有自动配置 |
| `@Lazy` 解循环依赖（两个方向都要挂） | 不适用 | 手工装配顺序可控；若出现初始化环，是装配顺序写错了，就地改正 |
| `hibernate.ddl-auto=update` 在 SQLite 上不可靠 | 不适用 | 从未引入 ORM |

### 2.7 工程形态（备案）

两侧职责与约束相同、只换实现载体的维度，同样不计入三态矩阵，备案供迁移与 review 参照：

| 维度 | NivroOS 口径 | Basalt 口径 | 差异性质 |
| --- | --- | --- | --- |
| 运行形态 | fat JAR + JVM | 单一静态二进制（`CGO_ENABLED=0`），零运行时依赖 | 交付形态差异——接口与行为不受影响 |
| 装配 | Spring Boot 容器 + 9 个 Maven 模块 | 手写装配（`internal/app`）+ 单 Go module 的 `internal/` 分包 | 形态等价：装配层职责相同，依赖方向约束相同 |
| 并发载体 | 同步阻塞 + 虚拟线程（每请求一虚拟线程） | 同步阻塞 + goroutine（`net/http` 每请求一 goroutine） | 语义等价：同为"同步阻塞 + 轻量并发"，两侧都不引异步编排 |
| 请求级状态 | `ProfileContext` ThreadLocal | 显式类型化参数（`RunRequest` / `RunEnv`）；`context` 只承载取消与截止 | 语义等价：两侧都拒绝隐式耦合；**不得**用包级变量或 `context.WithValue` 传业务状态 |
| LLM 层 | Spring AI（协议转换 + `@Tool` schema；禁用自动执行） | OpenAI 兼容客户端（Chat + 工具 schema；无自动循环） | 语义等价：调度权归 `ToolExecutor` |
| 工具 schema | `@Tool` 注解生成 | 工具接口显式给出 JSON Schema（手写 schema 常量） | 形态不同：Go 无注解生态，schema 是显式契约（防漂移靠测试） |
| 持久化 | Spring Data JPA | `database/sql` + 手写 SQL + repository 接口 | 形态不同：接口位置（core）与依赖倒置不变 |
| 上下文终止 | 中断标志 + 设回 | `context.Context` 取消传播 | Go 原生机制替代 |
| 管理台前端 | Vue 3 + Vite 构建产物入库 | **同左**：Vue + Vite 产物经 `go:embed` 进二进制（Node 仅构建期；U-9 已按评审翻转，见技术方案 §7.4） | 形态一致：一套前端心智，产物入库，无 Node 也能出包 |
| cron 表达式 | 6 段（秒 分 时 日 月 周） | 同左 | **公共子集内逐字对齐**：两侧语法差异（`7` / `L` / `W` / `#`）见 §5.2 |

### 2.8 模块 / 包结构对照（备案）

包不是兼容面（Go 单二进制下包是编译单元、不是交付单元），下表仅作两仓库的导航对照；Basalt 的包纪律（依赖方向 + 职责清晰 + 无环）见 [技术方案](TechnicalSolution.md) §10.1：

| NivroOS 模块 | Basalt 包 | 说明 |
| --- | --- | --- |
| nivroos-core | `internal/core` | 抽象与引擎 |
| nivroos-provider | `internal/provider` | 能力一 |
| nivroos-memory | `internal/memory` | 能力三 |
| nivroos-tool | `internal/tool` | 能力四（三合一不变） |
| nivroos-channel-cli | `internal/channel` | Channel + CLI 实现 |
| nivroos-web | `internal/web` | 能力五 |
| nivroos-storage | `internal/storage` | 持久化 |
| nivroos-cli | `internal/cli` | 命令行与配置 |
| nivroos-boot | `internal/app` + `cmd/basalt` | 装配与唯一 main |
| （NivroOS 无独立模块） | `internal/workspace` | 工作区与 Agent 生命周期（原分散在 core/boot） |

---

## 3. 有意改变全量清单

每一条都是**设计决定**，附理由、迁移影响与可验证的验收点。

| # | 改变 | 为什么 | 迁移影响 | 验证方式 |
| --- | --- | --- | --- | --- |
| 1 | 工作区根 `.nivroos/` → `.basalt/` | 品牌隔离；两底座可共存于同一目录互不污染 | 手工改名 | 改名后 `basalt status` 能列出既有 Agent |
| 2 | 默认监听 `127.0.0.1:8080`，远程须显式配置 | 安全默认：不配置就不对外 | 容器 / 远程部署改一行 | `ss -ltn` / `netstat` 实测只绑回环 |
| 3 | 达上限 = `limit_reached` + 部分结果（HTTP 200） | "没答完"必须**看得见**；旧行为让调用方无从区分，运维无法统计"哪些 Agent 常撞上限" | 调用方改看 `status` | contracts/run-and-errors.md §1.3；REST 与 CLI 各一条用例 |
| 4 | 同会话串行 + 409 / 实例级并发 + 429，且**不排队** | 排队把"什么时候有结果"变成不可预测的问题，且需持久化队列 | 并发客户端处理 409 | contracts/run-and-errors.md §3 |
| 5 | 取消语义（ctx 传播 → 杀进程树 → 审计**解耦**写入，`context.WithoutCancel` + 5 秒短超时 → 未完成工具组补齐为「已取消」） | 被取消的 Run 正是最需要证据的时刻，审计不能随请求一起被掐掉；半截的工具组会让会话哑掉（供应商 400），取消后必须仍可继续 | 无 | contracts/run-and-errors.md §4.1~4.2、§4.4；取消后同会话再发一条消息能完成 |
| 6 | 崩溃恢复：**实例锁**决定谁恢复（**操作系统级独占文件锁**，心跳仅诊断；写模式被活实例持锁时拒绝启动）；`running` → `failed`、两表 `pending` → `unknown`（**不猜测**外部副作用）；未完成工具组以**消息**为入口扫出（**按位置判定**：assistant 组 + 其后 `seq` 紧邻的连续 tool 段，**不得**全会话按 `tool_call_id` 全局查找——旧 Run 同 id 的结果行会让新组被误判完成）、与 Run 状态修复**同事务**按审计补齐（会话可继续），补组后另扫 `runs` 收尾无半截组的 `running` Run——**组看消息、Run 看表**；CLI `profile create` / `profile delete` 同样属**写模式命令**（会写工作区），被活实例持锁时拒绝启动并指引服务 API / 管理台 | 崩溃时 shell / HTTP 可能已产生外部影响，猜"没执行"就是撒谎；且"读一次库"的进程不该把活实例的 Run 标成失败；恢复以 Run 为入口且分两步提交时，中途再崩会把半截组**永久遗留**（Run 已不再是 `running`，扫不到）——故**工具组**的入口放在消息、状态与补齐同事务；而没有半截组可补的 `running` Run（等首次 LLM 响应 / 最终回答已落库而终态未写 / 无状态 invoke）只有扫 `runs` 找得到，故补组后仍需再扫一次表（**组看消息、Run 看表**） | 无 | contracts/run-and-errors.md §4.3~4.4；重启用例（含 kill -9 后同会话继续；恢复中再 kill -9 后重启仍收口） |
| 7 | 审计 fail-closed（**两表同一套两阶段写**，插入失败即拒绝执行）+ 扩列（run_id / round / tool_call_id / status / error_kind / config_hash） | 从"有记录"到"能关联、能解释失败"；审计有洞比服务不可用更糟——前者无人知晓 | 数据不迁移 | contracts/run-and-errors.md §7；schema.sql |
| 8 | 配置快照（**冻结清单 manifest 内容寻址**——AGENT.md + bootstrap 文件 + 绑定技能 `SKILL.md` 逐文件 `{path, sha256}`，运行内冻结）+ 原子发布 + 失败保旧 | 消解"旧 Profile × 新正文"混搭；热加载的正确性基础。**读即所记**：同一次读取产出注入内容与清单哈希，任一文件读失败则该次 Run 拒绝启动——否则 `config_hash` 证明不了"当时用的就是这份配置" | 无（行为更可预期） | 技术方案 §11.5；contracts/schema.sql `config_snapshots` 节 |
| 9 | 文件读写范围收窄：写 = 当前 Agent 目录（**不含其 `scripts/` 子目录**——该区对模型只读，代码常量判定）；读 = 当前 Agent 目录 + 技能库；跨 Agent 读须显式授权（`sandbox.cross_agent_read`） | "一个 Agent 能读遍所有 Agent 的提示词与脚本"不应默认成立；`scripts/` 只读是脚本受控通道（第 20 条）成立的前提——脚本能被模型改写，通道就拦不住它 | 跨 Agent 共享内容需授权或移入技能库 | 白名单用例：未授权读别的 Agent 目录 → `sandbox_denied`；写 `scripts/` 被拒 |
| 10 | 子进程环境变量白名单（`shell.env_passthrough`） | 子进程默认继承**完整**环境 = 把所有凭证一并交给被执行的脚本 | 依赖自定义变量的脚本需登记白名单（拒绝文案点名配置键） | shell 用例：未登记变量在子进程中不可见 |
| 11 | 渠道地址存 `${ENV_VAR}` 引用，发送时解析 | "零明文落盘"字面成立；地址的路径 / 查询串**就是凭证**，且这类凭证通常不过期，泄漏即长期有效 | 行数据不迁移（重新登记） | 登记后查库：`url` 列含 `${`；响应只回 host |
| 12 | 数据库结构重做 + `schema_version` 受控前向迁移 | 消息与审计需要结构化，才能按 run 关联、增量追加、承载供应商扩展字段 | 数据不迁移 | schema.sql；迁移用例（旧库 → 逐版前向） |
| 13 | 失败分类 `error_kind` 七类 | 没有类别，审计只能答"失败了"、不能答"为什么" | 数据不迁移 | contracts/run-and-errors.md §7.3 |
| 14 | 执行预算五项 + 上下文预算 + 核心记忆写配额 | 一次 Run 的消耗必须有界；"永不截断"的前提是有界 | 长任务可能撞 `RunTimeout` | contracts/run-and-errors.md §6 |
| 15 | 运行查询与取消的 3 个 API | 运维要能看"这一次跑了什么"、要能停 | 无（纯新增） | contracts/openapi.yaml `runs` 组 |
| 16 | 前端复用 NivroOS 的 Vue + Vite 方案（产物 embed，Node 仅构建期） | 一套前端心智、产物入库、无 Node 也能出包——与 NivroOS 的验证过的形态一致 | 无 | 技术方案 U-9 结论；干净检出可构建 |
| 17 | 工具参数**扩展**：`shell.timeout_seconds`、`http_get.headers`、`http_post.content_type` / `headers`（均可选，agent-md.md §5 标「扩展」） | 超时可控、请求头可控是真实需求；共有参数与必填性**逐字不动**是互换底线 | 无需动作（模型不填即零影响；`timeout_seconds` 只能缩短，不能放大） | contracts/agent-md.md §5 的「扩展」标注；Compat 样例 AGENT.md 两侧加载 |
| 18 | 沙箱拆成两个接口：`Policy.Check`（授权）+ `CommandRunner.Run`（执行） | 授权（"允许吗"）与执行（"怎么做"）职责不同、升级路径也不同；容器化档要连执行方式一起换，"单接口不变、只新增实现类"的承诺不成立 | 无（接口为新增设计，无历史调用方） | 技术方案 §6.5；白名单授权与进程树用例 |
| 19 | 包结构约束改为**依赖方向 + 职责清晰 + 无环**（包数是起点、按需增删，不再"固定不拆不并"） | Go 单二进制下包是编译单元不是交付单元，"固定包数"没有工程意义且阻碍第三方实现（cron / MCP / storage）落位 | 无 | 技术方案 §10；CI `go list` 无环复核 |
| 20 | **脚本受控通道**：新增 `shell.interpreters`（解释器白名单）；解释器只接受 `[<脚本路径>, <参数…>]` 形态（`arguments[0]` 以 `-` 开头一律拒）；脚本真实路径须落在该 Agent `scripts/` 或技能库；`scripts/` 对模型**只读**；`bash` / `sh` 仍在禁用之列 | 给"模型能跑脚本"开一条**形态受控**的路：解释器不是普通可执行名——只放行 `python` 而不约束形态，`-c` 内联代码就等于任意代码执行，`scripts/` 只读也白设；脚本区只读是该通道成立的前提（模型改不动自己要跑的脚本） | 想跑脚本的 Agent 须把脚本放进自己的 `scripts/`（或技能库）并在 `basalt.yaml` 登记解释器路径 | contracts/agent-md.md §5.2；shell 用例：`-c` 形态被拒、脚本越界被拒、`scripts/` 写入被拒（代码常量判定） |
| 21 | 定时任务 `id` 规则：**单项可缺省**（派生 `<agent>-1`）、**两项及以上每项必须显式声明**、显式 `id` 全局唯一 | 全缺省派生的 id 绑在"列表顺序"上——调一次 `schedules:` 顺序就把 `enabled` 开关与执行历史换到别的任务头上，且无声无息 | 多任务 Agent 的 `AGENT.md` 须补显式 `id`（加载期拒绝并点名）；给派生任务补显式 `id` = 换身份（旧状态不跟随） | contracts/agent-md.md §3；加载期用例（≥2 项缺 `id` 被拒） |
| 22 | 定时任务**下线 = 标记不物理删**（`scheduled_tasks.retired_at`）；重启对账（`enabled` 以库为准、定义以文件为准）；停机期间错过的触发点**不补跑**（记 WARN，补跑走 `/schedules/{id}/run`） | 物理删登记行会撞 `task_executions` 的外键（历史执行记录要么级联删、要么删不掉），而"历史执行记录"正是审计资产；"错过不补跑"与"不自动重试、恢复不重放"是同一条纪律 | 归档删除的 Agent 其任务行留在库里（`retired_at` 置位），查历史仍可见 | contracts/schema.sql `scheduled_tasks` 节；contracts/openapi.yaml `/schedules`；用例：归档后任务不触发、`executions` 仍在 |
| 23 | 多文件提交 = **staging 构建 + 目录交换**（先在 `.staging` 构建完整副本，校验通过后原目录 → `.backup`、staging → 正式目录、删 `.backup`） | 就地写入的多文件提交会留下"半个新配置"的中间态：崩溃后磁盘上既有旧文件又有新文件，加载器读到的是**从没被校验过**的组合 | 无（对外行为不变） | 技术方案 §11.4；contracts/run-and-errors.md §4.3.2 第 5 步（残留判定：回滚 / 前滚）；用例：提交中途 `kill -9` → 重启后按残留收敛 |

> 第 16 条推翻了本文档初稿（与需求文档 U-9 的原始建议）中"服务端渲染"的方向——**已按评审结论翻转**，需求文档同步。

---

## 4. 暂不支持清单

| # | 项 | 为什么现在不做 | 登记去向 |
| --- | --- | --- | --- |
| 1 | NivroOS 数据库级直迁 | 结构不同（消息拆表、审计扩列、新增 runs），自动转换的复杂度远大于收益；文件资产已覆盖主要互换面 | **不做**（永久；历史会话可导出为只读文本归档） |
| 2 | Mem0 记忆后端 | 核心阶段只交付 markdown + sqlite 两档（核心闭环不需要第三档）；接口预留，真服务联调（以部署实例 `/openapi.json` 核对路径与字段）界面向外 | **延后至扩展阶段**（需求文档 U-8 已定） |
| 3 | 多副本 / 分布式部署（跨进程会话锁、共享状态） | 核心阶段单实例（内网假设）；会话锁是进程内的 | 扩展阶段 |
| 4 | SSE / WebSocket / 流式响应 | 同步执行模型（宪法原则）；流式会引入第二套取消与背压语义 | 扩展阶段 |
| 5 | 认证 / SSO / RBAC / 多租户 / 限流 | 内网假设；先行验证内核可用性 | 扩展阶段（企业级治理层） |
| 6 | 容器 / microVM 沙箱（Policy 与 Runner 的第二实现） | 接口先行走在前面（Policy / Runner 两个接口），实现按信号驱动升级 | 扩展阶段（**只加实现 + 换装配，调用方零改动**——这条承诺已按评审改写为"两接口的新实现"，不再宣称单接口不变） |
| 7 | 向量记忆与知识库 | 需要嵌入模型与向量存储两道新依赖面，先验证核心闭环 | 扩展阶段 |
| 8 | 完整审计查询 API（按 run / agent / 时间检索） | 表已按可查询结构落库（day one 写入不是 day one 查询） | 扩展阶段 |
| 9 | 密钥轮换 / KMS / Vault 对接 | 核心阶段凭证经环境变量注入，轮换即重启 | 扩展阶段 |
| 10 | Tool Policy（按 Agent 精细控制工具权限矩阵） | 当前粒度是"每个 Agent 的 `tools:` 清单"，够用 | 扩展阶段 |

---

## 5. 共享验收样例

这三组样例是**"可互换"这句话的判据**——两个底座各跑一遍，结果逐条比对。

> 执行时点：本机无 Go 工具链，以下样例在**原型期**（见 [AiProgrammingGuide.md](AiProgrammingGuide.md) §3.4）两底座分别执行并记录读数。本仓当前只固化样例与判据。

### 5.1 样例 AGENT.md（两底座都能加载）

```markdown
---
name: compat-check
description: 兼容性对照样例 Agent
identity:
  agent_name: 对照小助手
  prompt: 你是一个用于兼容性验证的助手。
provider:
  name: deepseek
  model: deepseek-chat
tools:
  - read_file
  - write_file
  - list_dir
  - shell
  - http_get
  - http_post
  - save_memory
  - recall_memory
  - notify
  - manage_schedule
skills:
  - digest-format
mcp_servers: []
bootstrap:
  - AGENTS.md
  - SOUL.md
  - USER.md
settings:
  max_iterations: 10
  max_history_turns: 20
schedules:
  - id: compat-daily
    cron: "0 0 8 * * *"
    zone: Asia/Shanghai
    message: "输出一句当前时间"
---

你是一个用于兼容性验证的助手。收到任何消息时，回复"compat ok"。
```

**判据**（两个底座逐条比对）：

1. 分别放入 `.nivroos/agents/compat-check/` 与 `.basalt/agents/compat-check/`，两侧 `status` / `profiles` 都能列出该 Agent（`registered: true`）
2. 两侧的 `tools` 字段解析结果相同（10 个名字全部命中，`tool not registered` 零命中）
3. 两侧都接受 `0 0 8 * * *`（6 段）与 `zone: Asia/Shanghai`，且**未来 10 次触发时间逐条相同**（见 §5.2）
4. 把 `cron` 改成 5 段（`0 8 * * *`）→ 两侧都拒绝该 Agent 注册（Basalt 报 "Cron expression must consist of 6 fields"）
5. 把 `zone` 删掉 → 两侧都按 **UTC**（不是本机时区）触发
6. 前提：两侧都必须已注册 `deepseek` provider 且配置了对应的 API key 环境变量

### 5.2 cron 公共子集与十次触发时间对拍

**公共子集**（语法矩阵与不可用语法清单见 [contracts/agent-md.md](contracts/agent-md.md) §3.1）：

```text
可用：* 数字 范围(1-5) 列表(1,3,5) 步长(*/5, 1-30/5) 缩写(JAN, MON) 星期 0
不可用：星期 7（用 0）  L  W  #（Spring 支持，robfig/cron 不支持）
硬性子集规则：日（dom）与周（dow）不得同时受限——至少一个是裸 *。
```

**实现差异**：NivroOS 用 Spring `CronExpression`；Basalt 用 `robfig/cron` v3.0.1——该版本**没有** `parser.WithLocation`，每任务时区经 `CRON_TZ=<zone>` 前缀实现（装载时由 `zone` 字段生成前缀）。

实现差异之外还有一条**语义差异**（"六段制式相同 ≠ 语义相同"）：

- **`dom` 与 `dow` 同时被限定时，两侧含义不同**——Spring 取**交集**（AND：两者都满足才触发）；robfig 取**并集**（OR：任一满足即触发）
- 因此公共子集**加上**硬性规则：`dom` / `dow` 至少一个是裸 `*`。同时受限的表达式**不在公共子集内**（两底座各按自己的语义跑，不算"不一致"）

**已知差异例（不用于对拍，只作差异备案）**：

```text
0 15 9 1-7 * 1   Asia/Shanghai    每月前 7 天内的周一 09:15
```

以固定起点 2026-10-01（北京）实测两侧的第一次触发：NivroOS（Spring，AND）= **2026-10-05 09:15 +08:00**（10 月 5 日既是 1–7 日又是周一）；按 robfig 的 OR 语义预期为 **2026-10-01 09:15 +08:00**（10 月 1 日在 1–7 日内即触发）——robfig 侧读数待原型期实跑确认。**该表达式不得写入任何样例 `AGENT.md`**。

**对拍判据**：对下面**三条**公共子集内表达式，两侧各从**同一固定起点**打印未来 10 次触发时间（UTC 表示），**逐条相同**：

```text
起点（两侧统一）：2026-10-01T00:00:00Z（绝对时刻，不得用"当前时间"）
0 0 8 * * *      Asia/Shanghai    每天 08:00:00 +08:00
0 30 23 * * 0    UTC              每周日 23:30:00 UTC
0 0 */6 * * *    Asia/Shanghai    每 6 小时整点
```

> **对拍程序必须用固定起点**，**不得**用 `time.Now()` / `now()`：否则两条序列从不同的时刻出发，本就无法逐条比对；且每次运行结果都不同，无法复现（本轮基线验证时发现的缺陷，两侧程序都已改为固定起点）。

Basalt 侧对拍程序（原型期执行；`go run` 于本仓之外的空目录，`go.mod` 引 `github.com/robfig/cron/v3 v3.0.1`）：

```go
package main

import (
    "fmt"
    "time"

    "github.com/robfig/cron/v3"
)

func main() {
    parser := cron.NewParser(cron.Second | cron.Minute | cron.Hour |
        cron.Dom | cron.Month | cron.Dow)
    // 固定起点：与 Java 侧同一个绝对时刻（2026-10-01T00:00:00Z）
    var start = time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
    for _, expr := range []string{
        "CRON_TZ=Asia/Shanghai 0 0 8 * * *",
        "CRON_TZ=UTC 0 30 23 * * 0",
        "CRON_TZ=Asia/Shanghai 0 0 */6 * * *",
    } {
        sched, err := parser.Parse(expr)
        if err != nil {
            panic(err)
        }
        fmt.Println(expr)
        next := start
        for i := 0; i < 10; i++ {
            next = sched.Next(next)
            fmt.Println("  ", next.UTC().Format(time.RFC3339))
        }
    }
}
```

NivroOS 侧对拍：在**不修改该仓库**的前提下，用 jshell 加载 `spring-context` 的 jar（classpath 取自本机 `~/.m2`）跑等价循环：

```java
import org.springframework.scheduling.support.CronExpression;
import java.time.*;

var cron = CronExpression.parse("0 0 8 * * *");
var tz = ZoneId.of("Asia/Shanghai");
// 固定起点：与 Go 侧同一个绝对时刻（2026-10-01T00:00:00Z）
var t = ZonedDateTime.ofInstant(Instant.parse("2026-10-01T00:00:00Z"), tz);
for (int i = 0; i < 10; i++) {
    t = (ZonedDateTime) cron.next(t);
    System.out.println(t.withZoneSameInstant(ZoneOffset.UTC));
}
```

**DST 边界例（必须实测，不得推断）**：`America/New_York`，2027-03-14 本地 02:00–03:00 不存在（春季跳变）。

- 表达式 `0 30 2 * * *` 在该日的行为，两底座**各自测出什么就记什么**
- **若两侧不同**：把该例登记进本文档 §3 作为已知差异（该表达式随即**不在公共子集**内），并在 contracts/agent-md.md §3.1 加注
- **不得**在实测前宣称"两底座一致"

### 5.3 工具调用与 `reasoning_content` 回传样例

**正确**（思考模式 + 工具调用，完整往返）：

```json
[
  {
    "role": "assistant",
    "content": "",
    "reasoning_content": "用户问天气，我需要调工具查。",
    "tool_calls": [
      { "id": "call_abc123", "name": "http_get",
        "arguments": "{\"url\":\"https://api.example.com/weather\"}" }
    ]
  },
  {
    "role": "tool",
    "tool_call_id": "call_abc123",
    "content": "{\"temp\": 12, \"desc\": \"多云\"}"
  },
  { "role": "assistant", "content": "今天 12 度，多云。" }
]
```

三条硬约束（见 [contracts/agent-md.md](contracts/agent-md.md) §4.1）：

1. `tool_calls[].id` 与 `tool_call_id` **逐字对应**，结果按对应 ID 回填
2. `reasoning_content` **必须原样回传**——丢掉它，DeepSeek 等思考模式模型对带工具调用的轮次返回 **HTTP 400**；且历史被"洗干净"后**整个会话持续 400**（不可恢复）
3. 供应商扩展字段经 `provider_extra` 透传，不得在序列化往返中丢失

**裁剪样例**（`max_history_turns` 触发时）：

```text
正确：丢掉 [assistant(tool_calls=[A,B])] + [tool(A)] + [tool(B)]   ← 整组
错误：只丢 [tool(A)]，留下 [assistant(tool_calls=[A,B])] + [tool(B)]
      → 供应商报 "tool_call 无对应结果"，HTTP 400
```

**判据**：两侧各跑一次带工具的会话（≥3 轮工具调用），均能完成且历史里 `reasoning_content` 非空；把该字段人为清空后**必须复现 400**（证明这条约束确实是必须的，不是防御性猜测）。

---

## 6. 维护纪律

1. **基线锚在 commit 上**，不写"最新版"——NivroOS 侧事实以 `50e1b30` 为准，其后续演进不自动进入本文
2. 矩阵行**只用三态**；发现含糊表述即视为文档缺陷（"基本/大概/部分兼容"在本文里是禁词）
3. `保持一致` 的行如果两侧实现路径不同（如 cron 时区），**必须在说明列写明路径差异**
4. 本文的每条 `有意改变` 必须能指回 [技术方案](TechnicalSolution.md) 或 [contracts/](contracts/) 的具体段落——没有依据的改变是缺陷
5. 双侧契约变化时更新本文；文档间的差异**不擅自"修正"**，以技术方案为准并在此记差异
6. **两仓库关系**：Basalt 把 NivroOS 第一阶段实践的结论作为 day-one 输入（本文即该输入的存档），此后两仓库**各自独立演进、互不回写**；文档互为参照、不互为权威，本文也不随 NivroOS 演进自动更新（见第 1 条）
7. **引用纪律（防漂移）**：四份权威文档与三件套**自述为主**——NivroOS 只允许以两种身份出现：①血缘一句话（文档头部）+ 指向本文的指针；②兼容承诺句里作为语法宾语的必要指称（如"参数名与 NivroOS 基线逐字一致""同一份 `AGENT.md` 两底座可加载"）。**对照性叙述**（逐项对照、差异裁决、模块映射、有意改变清单）**只存在于本文**；别处再出现第二份对照表即视为漂移缺陷

---

## 7. 跨文档指引

| 主题 | 文档 |
| --- | --- |
| 运行状态机 / 并发 / 取消 / 崩溃恢复 / 错误码 | [contracts/run-and-errors.md](contracts/run-and-errors.md) |
| AGENT.md 字段与工具契约（模型可见字面量） | [contracts/agent-md.md](contracts/agent-md.md) |
| 数据库结构与迁移纪律 | [contracts/schema.sql](contracts/schema.sql) |
| REST 端点与信封 | [contracts/openapi.yaml](contracts/openapi.yaml) |
| 部署、备份与升级（两个底座通用口径） | [Operations.md](Operations.md) |
