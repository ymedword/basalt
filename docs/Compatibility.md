# 与 NivroOS 的兼容性对照

> 本文档回答一个问题：**同一份 Agent 资产、同一套使用方式，在两个底座之间能互换到什么程度。**
>
> 三态口径：每条对照项标 `保持一致` / `有意改变` / `暂不支持` 之一，**不允许含糊表述**（"基本兼容"这类词不出现）。
> 与 [技术方案](TechnicalDesign.md) 冲突时以技术方案为准；本文只记**差异与承诺**，重合内容（目录协议、工具契约、API 形状）不在此复制。

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
| 长期记忆门面工具 | 保持一致 | `save_memory` / `recall_memory`；`recall_memory` 传**单个关键词** | 逐字同（含"整串子串匹配——连续短语命中、分散词不自动组合"这条已知行为；2026-10-17 澄清，原文"多词短语必不命中"与 `contains` 语义不符） | 无需动作 |
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
| 管理台前端 | Vue 3 + Vite 构建产物入库 | **同左**：Vue + Vite 产物经 `go:embed` 进二进制（Node 仅构建期；U-9 已按评审翻转，见技术方案 §6.2.4） | 形态一致：一套前端心智，产物入库，无 Node 也能出包 |
| cron 表达式 | 6 段（秒 分 时 日 月 周） | 同左 | **公共子集内逐字对齐**：两侧语法差异（`7` / `L` / `W` / `#`）见 §5.2 |

### 2.8 模块 / 包结构对照（备案）

包不是兼容面（Go 单二进制下包是编译单元、不是交付单元），下表仅作两仓库的导航对照；Basalt 的包纪律（依赖方向 + 职责清晰 + 无环）见 [技术方案](TechnicalDesign.md) §7.1：

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
| 8 | 配置快照（**冻结清单 manifest 内容寻址**——AGENT.md + bootstrap 文件 + 绑定技能 `SKILL.md` 逐文件 `{path, sha256}`，运行内冻结）+ 原子发布 + 失败保旧 | 消解"旧 Profile × 新正文"混搭；热加载的正确性基础。**读即所记**：同一次读取产出注入内容与清单哈希，任一文件读失败则该次 Run 拒绝启动——否则 `config_hash` 证明不了"当时用的就是这份配置" | 无（行为更可预期） | 技术方案 §3.6；contracts/schema.sql `config_snapshots` 节 |
| 9 | 文件读写范围收窄：写 = 当前 Agent 目录（**不含其 `scripts/` 子目录**——该区对模型只读，代码常量判定）；读 = 当前 Agent 目录 + 技能库；跨 Agent 读须显式授权（`sandbox.cross_agent_read`） | "一个 Agent 能读遍所有 Agent 的提示词与脚本"不应默认成立；`scripts/` 只读是脚本受控通道（第 20 条）成立的前提——脚本能被模型改写，通道就拦不住它 | 跨 Agent 共享内容需授权或移入技能库 | 白名单用例：未授权读别的 Agent 目录 → `sandbox_denied`；写 `scripts/` 被拒 |
| 10 | 子进程环境变量白名单（`shell.env_passthrough`） | 子进程默认继承**完整**环境 = 把所有凭证一并交给被执行的脚本 | 依赖自定义变量的脚本需登记白名单（拒绝文案点名配置键） | shell 用例：未登记变量在子进程中不可见 |
| 11 | 渠道地址存 `${ENV_VAR}` 引用，发送时解析 | "零明文落盘"字面成立；地址的路径 / 查询串**就是凭证**，且这类凭证通常不过期，泄漏即长期有效 | 行数据不迁移（重新登记） | 登记后查库：`url` 列含 `${`；响应只回 host |
| 12 | 数据库结构重做 + `schema_version` 受控前向迁移 | 消息与审计需要结构化，才能按 run 关联、增量追加、承载供应商扩展字段 | 数据不迁移 | schema.sql；迁移用例（旧库 → 逐版前向） |
| 13 | 失败分类 `error_kind` 七类 | 没有类别，审计只能答"失败了"、不能答"为什么" | 数据不迁移 | contracts/run-and-errors.md §7.3 |
| 14 | 执行预算五项 + 上下文预算 + 核心记忆写配额 | 一次 Run 的消耗必须有界；"永不截断"的前提是有界 | 长任务可能撞 `RunTimeout` | contracts/run-and-errors.md §6 |
| 15 | 运行查询与取消的 3 个 API | 运维要能看"这一次跑了什么"、要能停 | 无（纯新增） | contracts/openapi.yaml `runs` 组 |
| 16 | 前端复用 NivroOS 的 Vue + Vite 方案（产物 embed，Node 仅构建期） | 一套前端心智、产物入库、无 Node 也能出包——与 NivroOS 的验证过的形态一致 | 无 | 需求文档 §7.6（U-9）；干净检出可构建 |
| 17 | 工具参数**扩展**：`shell.timeout_seconds`、`http_get.headers`、`http_post.content_type` / `headers`（均可选，agent-md.md §5 标「扩展」） | 超时可控、请求头可控是真实需求；共有参数与必填性**逐字不动**是互换底线 | 无需动作（模型不填即零影响；`timeout_seconds` 只能缩短，不能放大） | contracts/agent-md.md §5 的「扩展」标注；Compat 样例 AGENT.md 两侧加载 |
| 18 | 沙箱拆成两个接口：`Policy.Check`（授权）+ `CommandRunner.Run`（执行） | 授权（"允许吗"）与执行（"怎么做"）职责不同、升级路径也不同；容器化档要连执行方式一起换，"单接口不变、只新增实现类"的承诺不成立 | 无（接口为新增设计，无历史调用方） | 技术方案 §4.3；白名单授权与进程树用例 |
| 19 | 包结构约束改为**依赖方向 + 职责清晰 + 无环**（包数是起点、按需增删，不再"固定不拆不并"） | Go 单二进制下包是编译单元不是交付单元，"固定包数"没有工程意义且阻碍第三方实现（cron / MCP / storage）落位 | 无 | 技术方案 §7.1.2；CI `go list` 无环复核 |
| 20 | **脚本受控通道**：新增 `shell.interpreters`（解释器白名单）；解释器只接受 `[<脚本路径>, <参数…>]` 形态（`arguments[0]` 以 `-` 开头一律拒）；脚本真实路径须落在该 Agent `scripts/` 或技能库；`scripts/` 对模型**只读**；`bash` / `sh` 仍在禁用之列 | 给"模型能跑脚本"开一条**形态受控**的路：解释器不是普通可执行名——只放行 `python` 而不约束形态，`-c` 内联代码就等于任意代码执行，`scripts/` 只读也白设；脚本区只读是该通道成立的前提（模型改不动自己要跑的脚本） | 想跑脚本的 Agent 须把脚本放进自己的 `scripts/`（或技能库）并在 `basalt.yaml` 登记解释器路径 | contracts/agent-md.md §5.2；shell 用例：`-c` 形态被拒、脚本越界被拒、`scripts/` 写入被拒（代码常量判定） |
| 21 | 定时任务 `id` 规则：**单项可缺省**（派生 `<agent>-1`）、**两项及以上每项必须显式声明**、显式 `id` 全局唯一 | 全缺省派生的 id 绑在"列表顺序"上——调一次 `schedules:` 顺序就把 `enabled` 开关与执行历史换到别的任务头上，且无声无息 | 多任务 Agent 的 `AGENT.md` 须补显式 `id`（加载期拒绝并点名）；给派生任务补显式 `id` = 换身份（旧状态不跟随） | contracts/agent-md.md §3；加载期用例（≥2 项缺 `id` 被拒） |
| 22 | 定时任务**下线 = 标记不物理删**（`scheduled_tasks.retired_at`）；重启对账（`enabled` 以库为准、定义以文件为准）；停机期间错过的触发点**不补跑**（记 WARN，补跑走 `/schedules/{id}/run`） | 物理删登记行会撞 `task_executions` 的外键（历史执行记录要么级联删、要么删不掉），而"历史执行记录"正是审计资产；"错过不补跑"与"不自动重试、恢复不重放"是同一条纪律 | 归档删除的 Agent 其任务行留在库里（`retired_at` 置位），查历史仍可见 | contracts/schema.sql `scheduled_tasks` 节；contracts/openapi.yaml `/schedules`；用例：归档后任务不触发、`executions` 仍在 |
| 23 | 多文件提交 = **staging 构建 + 目录交换**（先在 `.staging` 构建完整副本，校验通过后原目录 → `.backup`、staging → 正式目录、删 `.backup`） | 就地写入的多文件提交会留下"半个新配置"的中间态：崩溃后磁盘上既有旧文件又有新文件，加载器读到的是**从没被校验过**的组合 | 无（对外行为不变） | 技术方案 §3.4.2；contracts/run-and-errors.md §4.3.2 第 5 步（残留判定：回滚 / 前滚）；用例：提交中途 `kill -9` → 重启后按残留收敛 |
| 24 | MCP 配置条目的 `transport` 只接受 `stdio`（缺省 / 空串 / `null` = `stdio`）；非字符串或 `sse` / `http` 等取值 → **拒绝启动**并点名 server 与字段（NivroOS：WARN 后跳过该条目） | v1 只实现 stdio；跳过等于把"配了但不生效"静默化，与加载期 fail-closed 纪律（格式非法明确报错、不静默取默认）一致；代价是含非 stdio 条目的 NivroOS 配置在 Basalt 上整体起不来——**有意接受的迁移摩擦**。非字符串若被强转成空串，还会悄悄按缺省 stdio 获得执行含义 | 删除或注释非 stdio 条目后重启（`mcp_servers.yaml` 变更需重启） | `TestParseMCPServers_RejectsUnsupportedTransport` / `TestParseMCPServers_TransportDefaultsToStdio` / `TestParseMCPServers_RejectsNonStringTransport`（`1` / `true` / `[]` / `{}` 四态拒载）；错误点名文件、条目与字段 |
| 25 | MCP 结果中的非文本内容块（图片 / 音频 / 资源链接 / 内嵌资源）渲染为**类型化占位符**（如 `[非文本内容：image image/png]`），base64 原文**不进模型上下文**（NivroOS：`toString()` 直接把 base64 当文本） | base64 进上下文既烧预算又无工具价值，还会在会话历史留下二进制副本；文本块仍按序换行连接、无文本时用结构化内容 JSON | 需要原始二进制的消费方须经其他路径获取（v1 不做落盘 / 附件引用） | `TestMCPToolAdapter_RendersNonTextAsPlaceholderNoBase64`；`TestMCPToolAdapter_RendersTextBlocksJoined` / `TestMCPToolAdapter_FallsBackToStructuredContentAndNoOutput` 守其余口径 |
| 26 | MCP server 子进程环境 = `shell.env_passthrough` 透传白名单（示例配置 `basalt.yaml.example` 登记 `PATH` / `HOME` / `LANG` / `TZ`；**代码层无内置缺省**，未配置即不透传）+ 条目 `env` 声明覆盖同名键、其余**清空**；工作目录 = 工作区根（NivroOS：继承完整父进程环境） | 继承完整环境会把 `BASALT_*` / 云凭证 / 代理设置一并交给外部 server 进程；与 shell 子进程同一纪律、复用同一配置键（少一个键、少一处漂移）。白名单约束的是**继承面**，不是 server 内部的文件 / 网络行为——后者不经 Basalt Policy，可治理面 = 管理员登记的配置行 | server 依赖的自定义变量须登记 `shell.env_passthrough` 或在条目 `env` 声明；`${VAR}` 在装配期解析，未设置即拒绝启动 | `TestMCPPool_DialCommandEnvAndDir`（透传 / 覆盖 / 空清单非 nil）+ shell 用例（未登记变量子进程不可见） |

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

> 执行时点：本机无 Go 工具链，以下样例在**原型期**（见[开发操作](DevelopmentLoop.md) §5.2）两底座分别执行并记录读数。本仓当前只固化样例与判据。

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
4. 本文的每条 `有意改变` 必须能指回 [技术方案](TechnicalDesign.md) 或 [contracts/](contracts/) 的具体段落——没有依据的改变是缺陷
5. 双侧契约变化时更新本文；文档间的差异**不擅自"修正"**，以技术方案为准并在此记差异
6. **两仓库关系**：Basalt 把 NivroOS 第一阶段实践的结论作为 day-one 输入（本文即该输入的存档），此后两仓库**各自独立演进、互不回写**；文档互为参照、不互为权威，本文也不随 NivroOS 演进自动更新（见第 1 条）
7. **引用纪律（防漂移）**：项目历史来源、跨项目差异、模块映射与迁移对照仅在本文记录。其他项目文档直接陈述 Basalt 的需求、设计、契约和验收依据，可以链接本文，但不重复来源叙事；既有历史 US 与访谈材料保持原记录。本文保留真实历史，不由项目介绍的措辞调整推导出新的兼容承诺。

---

## 7. 跨文档指引

| 主题 | 文档 |
| --- | --- |
| 运行状态机 / 并发 / 取消 / 崩溃恢复 / 错误码 | [contracts/run-and-errors.md](contracts/run-and-errors.md) |
| AGENT.md 字段与工具契约（模型可见字面量） | [contracts/agent-md.md](contracts/agent-md.md) |
| 数据库结构与迁移纪律 | [contracts/schema.sql](contracts/schema.sql) |
| REST 端点与信封 | [contracts/openapi.yaml](contracts/openapi.yaml) |
| 部署、备份与升级（两个底座通用口径） | [Operations.md](Operations.md) |

<a id="document-reference-index"></a>

## 8. 文档引用索引（2026-10-18）

四份正文按产品任务、平台评估、Run 生命周期和开发 Loop 重新组织。文件改名不改变宪法顺位、固定契约、已批准 US 的范围或验收语义；历史文件不批量改写。

| 历史路径（现为导航页） | 正文 |
| --- | --- |
| `DemandAnalysis.md` | [需求文档](Requirements.md) |
| `IndustryResearch.md` | [行业调研](IndustrySurvey.md) |
| `TechnicalSolution.md` | [技术方案](TechnicalDesign.md) |
| `AiProgrammingGuide.md` | [开发操作](DevelopmentLoop.md) |

本次重排时台账 `docs/us/REGISTRY.md` 的 SHA-256 为 `3956f610d8d94cdc218ffd5f89e6cad0bedfd278fd9c08050cda27e74a7dd7c2`；该基线已有行的章节按旧版解析，新增行使用当前正文路径与语义锚点。

旧编号按下表定位。表中保留历史标题只为解释已有引用，不再作为新正文目录；新增模块引用当前正文的语义锚点。

### 8.1 `DemandAnalysis.md` 的章节迁移

| 历史章节 | 当前定位 |
| --- | --- |
| §1 · 1. 项目概述 | [需求文档 §1](Requirements.md#product-boundary) |
| §1.1 · 1.1 Basalt 是什么 | [需求文档 §1.1](Requirements.md#product-users) |
| §1.2 · 1.2 能干什么——五大核心能力 | [需求文档 §1.3](Requirements.md#product-capabilities) |
| §1.3 · 1.3 交付分两段 | [需求文档 §7.1](Requirements.md#release-scope) |
| §1.4 · 1.4 产品边界与演进依据 | [需求文档 §1.4](Requirements.md#product-scope-changes) |
| §1.5 · 1.5 实施节奏（US 依赖序） | [需求文档 §7.2](Requirements.md#delivery-slices) |
| §2 · 2. 术语 | [需求文档 §8](Requirements.md#terms) |
| §3 · 3. 设计目标 | [需求文档 §1.2](Requirements.md#product-success) |
| §4 · 4. 典型场景 | [需求文档 §2](Requirements.md#user-outcomes) |
| §5 · 5. 核心功能 | [需求文档 §3–6](Requirements.md#agent-assets) |
| §5.1 · 5.1 工作区初始化（`basalt init`） | [需求文档 §3.1](Requirements.md#workspace-init) |
| §5.2 · 5.2 一个目录 = 一个 Agent | [需求文档 §3.2](Requirements.md#agent-definition) |
| §5.3 · 5.3 能力一：对接 LLM（Provider） | [需求文档 §4.2](Requirements.md#provider-routing) |
| §5.4 · 5.4 能力二：ReAct 循环 | [需求文档 §4.1](Requirements.md#run-progress) |
| §5.5 · 5.5 能力三：Memory | [需求文档 §4.4](Requirements.md#long-term-memory) |
| §5.6 · 5.6 能力四：Tool 体系 | [需求文档 §5.1](Requirements.md#tool-permissions) |
| §5.7 · 5.7 Channel（核心阶段：CLI） | [需求文档 §5.3](Requirements.md#human-channel) |
| §5.8 · 5.8 能力五：Web Service | [需求文档 §5.4](Requirements.md#http-access) |
| §5.9 · 5.9 Session 管理 | [需求文档 §4.3](Requirements.md#session-history) |
| §5.10 · 5.10 定时任务（第三触发源） | [需求文档 §5.5](Requirements.md#scheduled-work) |
| §5.11 · 5.11 三种运行模式 | [需求文档 §5.7](Requirements.md#process-modes) |
| §5.12 · 5.12 命令行工具（12 个） | [需求文档 §5.8](Requirements.md#cli-surface) |
| §5.13 · 5.13 配置与密钥 | [需求文档 §5.2](Requirements.md#configuration) |
| §5.14 · 5.14 管理台与 Agent 动态管理（核心阶段收尾批次） | [需求文档 §3.3](Requirements.md#agent-management) |
| §5.15 · 5.15 官网展示主页（对外门面） | [需求文档 §6.1](Requirements.md#public-site) |
| §6 · 6. 扩展功能（方向定义，不排期） | [需求文档 §7.4](Requirements.md#extension-options) |
| §7 · 7. 社区共建 | [需求文档 §7.5](Requirements.md#contribution-surface) |
| §8 · 8. 非功能需求 | [需求文档 §6.2](Requirements.md#nonfunctional) |
| §9 · 9. 关键流程 | [需求文档 §4](Requirements.md#run-promises) |
| §9.1 · 9.1 启动加载 | [需求文档 §3.5](Requirements.md#startup-outcome) |
| §9.2 · 9.2 一次对话（含工具） | [需求文档 §4.5](Requirements.md#conversation-events) |
| §9.3 · 9.3 记忆写入与召回 | [需求文档 §4.6](Requirements.md#memory-events) |
| §9.4 · 9.4 定时触发 | [需求文档 §5.6](Requirements.md#schedule-events) |
| §9.5 · 9.5 技能渐进披露 | [需求文档 §3.6](Requirements.md#skill-disclosure) |
| §10 · 10. 数据模型 | [需求文档 §3.4、§6.6](Requirements.md#file-ownership) |
| §10.1 · 10.1 SQLite 表 | [需求文档 §6.6](Requirements.md#stored-data) |
| §10.2 · 10.2 文件系统数据 | [需求文档 §3.4](Requirements.md#file-ownership) |
| §11 · 11. 里程碑 | [需求文档 §7.3](Requirements.md#milestones) |
| §12 · 12. 风险与未决 | [需求文档 §7.6](Requirements.md#scope-decisions) |
| §12.1 · 12.1 风险 | [需求文档 §6.7](Requirements.md#product-risks) |
| §12.2 · 12.2 未决事项（U-1~U-10 已于 2026-10-15 全部裁决） | [需求文档 §7.6](Requirements.md#scope-decisions) |
| §13 · 13. 验收标准 | [需求文档 §2.4、§6](Requirements.md#quality-evidence) |
| §13.1 · 13.1 功能验收（五个可演示 Demo） | [需求文档 §2.4](Requirements.md#feature-examples) |
| §13.2 · 13.2 五个每日 Demo 验收（钟推，D1–D5） | [需求文档 §2.5](Requirements.md#daily-workflows) |
| §13.3 · 13.3 运维验收 | [需求文档 §6.3](Requirements.md#operations-acceptance) |
| §13.4 · 13.4 质量门禁验收 | [需求文档 §6.4](Requirements.md#quality-gates) |
| §13.5 · 13.5 运行时契约验收（按依赖分阶段） | [需求文档 §6.5](Requirements.md#runtime-proofs) |
| §14 · 14. 总结 | [需求文档 §1.2](Requirements.md#product-success) |

### 8.2 `IndustryResearch.md` 的章节迁移

| 历史章节 | 当前定位 |
| --- | --- |
| Basalt 行业调研：Agent OS 版图、Go 生态位与 Basalt 定位 · Basalt 行业调研：Agent OS 版图、Go 生态位与 Basalt 定位 | [行业调研 §7](IndustrySurvey.md#research-notes) |
| 一、什么是 Agent OS · 一、什么是 Agent OS | [行业调研 §1](IndustrySurvey.md#selection-questions) |
| §1.1 · 1.1 让一个 Agent "能干活"需要什么 | [行业调研 §1–2](IndustrySurvey.md#selection-questions) |
| §1.2 · 1.2 从 runtime 到 Agent OS | [行业调研 §1–2](IndustrySurvey.md#selection-questions) |
| §1.3 · 1.3 与相邻概念的区别 | [行业调研 §1–2](IndustrySurvey.md#selection-questions) |
| §1.4 · 1.4 部署形态 | [行业调研 §1–2](IndustrySurvey.md#selection-questions) |
| 二、业界两个最具代表性的开源项目 · 二、业界两个最具代表性的开源项目 | [行业调研 §3](IndustrySurvey.md#candidate-samples) |
| §2.1 · 2.1 OpenClaw（TypeScript，个人级 Agent 的爆发样本） | [行业调研 §3](IndustrySurvey.md#candidate-samples) |
| §2.2 · 2.2 Hermes Agent（Python，NousResearch） | [行业调研 §3](IndustrySurvey.md#candidate-samples) |
| §2.3 · 2.3 两个项目共同留下的空白 | [行业调研 §3](IndustrySurvey.md#candidate-samples) |
| 三、企业为什么需要私有可控的 Agent 底座 · 三、企业为什么需要私有可控的 Agent 底座 | [行业调研 §1](IndustrySurvey.md#selection-questions) |
| §3.1 · 3.1 需求是真实的，且集中在大中型组织 | [行业调研 §1](IndustrySurvey.md#selection-questions) |
| §3.2 · 3.2 严监管企业的铁律 | [行业调研 §1](IndustrySurvey.md#selection-questions) |
| §3.3 · 3.3 价值锚点是知识沉淀，不是自动化率 | [行业调研 §1](IndustrySurvey.md#selection-questions) |
| 四、Go 生态在这件事上的位置 · 四、Go 生态在这件事上的位置 | [行业调研 §3–4](IndustrySurvey.md#candidate-samples) |
| §4.1 · 4.1 分层盘点（2026-10-14 实测） | [行业调研 §3–4](IndustrySurvey.md#candidate-samples) |
| §4.2 · 4.2 空白确认（"空白"到底还剩什么） | [行业调研 §3–4](IndustrySurvey.md#candidate-samples) |
| §4.3 · 4.3 Go 作为底座语言的六个技术支点 | [行业调研 §3–4](IndustrySurvey.md#candidate-samples) |
| §4.4 · 4.4 MCP：Go 生态已经就位 | [行业调研 §3–4](IndustrySurvey.md#candidate-samples) |
| §4.5 · 4.5 Basalt 的独立验证命题 | [行业调研 §5](IndustrySurvey.md#validation-plan) |
| 五、Basalt 定位与愿景 · 五、Basalt 定位与愿景 | [行业调研 §4–5](IndustrySurvey.md#design-hypotheses) |
| §5.1 · 5.1 Basalt 是什么 | [行业调研 §4–5](IndustrySurvey.md#design-hypotheses) |
| §5.2 · 5.2 锚在需求，不锚在概念 | [行业调研 §4–5](IndustrySurvey.md#design-hypotheses) |
| §5.3 · 5.3 边界：做什么，不做什么 | [行业调研 §4–5](IndustrySurvey.md#design-hypotheses) |
| §5.4 · 5.4 安全怎么不重蹈开路者的覆辙 | [行业调研 §4–5](IndustrySurvey.md#design-hypotheses) |
| §5.5 · 5.5 跟业界项目的关系 | [行业调研 §4–5](IndustrySurvey.md#design-hypotheses) |
| 六、未来方向：从单机到分布式 · 六、未来方向：从单机到分布式 | [行业调研 §6](IndustrySurvey.md#evolution-triggers) |
| 附录 A：术语 · 附录 A：术语 | [行业调研 §7.2](IndustrySurvey.md#survey-terms) |
| 附录 B：参考资料与核实记录 · 附录 B：参考资料与核实记录 | [行业调研 §7.1](IndustrySurvey.md#research-evidence) |

### 8.3 `TechnicalSolution.md` 的章节迁移

| 历史章节 | 当前定位 |
| --- | --- |
| §1 · 1. 方案概述 | [技术方案 §1](TechnicalDesign.md#architecture-constraints) |
| §1.1 · 1.1 关键决策 | [技术方案 §1.1](TechnicalDesign.md#invariants) |
| §1.2 · 1.2 技术栈（版本均为 2026-10-14 实测） | [技术方案 §1.3](TechnicalDesign.md#dependency-baseline) |
| §1.3 · 1.3 设计事实与兼容性边界 | [技术方案 §1.2](TechnicalDesign.md#design-boundary) |
| §2 · 2. 整体架构 | [技术方案 §2.1、§7.1](TechnicalDesign.md#run-entry) |
| §2.1 · 2.1 三条入口，一条链路 | [技术方案 §2.1](TechnicalDesign.md#run-entry) |
| §2.2 · 2.2 分层视图 | [技术方案 §7.1.1](TechnicalDesign.md#layer-view) |
| §2.3 · 2.3 五大能力的关系 | [技术方案 §2.6](TechnicalDesign.md#runtime-collaboration) |
| §2.4 · 2.4 架构总览图 | [技术方案 §2.7](TechnicalDesign.md#deployment-view) |
| §3 · 3. 能力一：对接 LLM（Provider） | [技术方案 §2.4](TechnicalDesign.md#model-request) |
| §3.1 · 3.1 设计 | [技术方案 §2.4.1](TechnicalDesign.md#model-client) |
| §3.2 · 3.2 显式映射（硬约束） | [技术方案 §2.4.2](TechnicalDesign.md#model-mapping) |
| §3.3 · 3.3 核心阶段不做 | [技术方案 §2.4.3](TechnicalDesign.md#model-limits) |
| §4 · 4. 能力二：ReAct 循环 | [技术方案 §2](TechnicalDesign.md#run-path) |
| §4.1 · 4.1 算法（核心循环） | [技术方案 §2.2.1](TechnicalDesign.md#loop-algorithm) |
| §4.2 · 4.2 四个部件 | [技术方案 §2.2.2](TechnicalDesign.md#loop-components) |
| §4.3 · 4.3 边界行为 | [技术方案 §2.5.2](TechnicalDesign.md#termination-behavior) |
| §4.4 · 4.4 完整消息模型（工具调用轮次的硬约束） | [技术方案 §2.3](TechnicalDesign.md#message-protocol) |
| §4.5 · 4.5 Run 状态机 | [技术方案 §5.1](TechnicalDesign.md#run-state) |
| §4.6 · 4.6 并发、取消与崩溃恢复 | [技术方案 §5.2](TechnicalDesign.md#recovery-order) |
| §4.7 · 4.7 执行预算与准入控制 | [技术方案 §2.5.1](TechnicalDesign.md#admission-budget) |
| §5 · 5. 能力三：Memory | [技术方案 §3.5](TechnicalDesign.md#memory-input) |
| §5.1 · 5.1 统一门面与行为契约 | [技术方案 §3.5.1](TechnicalDesign.md#memory-contract) |
| §5.2 · 5.2 作用域：Agent 层 + 全局回退层 | [技术方案 §3.5.2](TechnicalDesign.md#memory-scopes) |
| §5.3 · 5.3 三档载体 | [技术方案 §3.5.3](TechnicalDesign.md#memory-backends) |
| §5.4 · 5.4 MEMORY.md 的形态 | [技术方案 §3.5.4](TechnicalDesign.md#memory-format) |
| §6 · 6. 能力四：Tool 体系 | [技术方案 §4](TechnicalDesign.md#action-boundary) |
| §6.1 · 6.1 工具接口 | [技术方案 §4.1.1](TechnicalDesign.md#tool-interface) |
| §6.2 · 6.2 内置十工具 | [技术方案 §4.2](TechnicalDesign.md#builtin-actions) |
| §6.3 · 6.3 三档扩展 | [技术方案 §4.4](TechnicalDesign.md#tool-extension) |
| §6.4 · 6.4 ToolRegistry | [技术方案 §4.1.2](TechnicalDesign.md#tool-registry) |
| §6.5 · 6.5 授权与执行：Policy / Runner 两个接口 | [技术方案 §4.3](TechnicalDesign.md#policy-runner) |
| §6.6 · 6.6 通知渠道（NotifyTools） | [技术方案 §6.4](TechnicalDesign.md#notification) |
| §7 · 7. 能力五：Web Service | [技术方案 §6.2](TechnicalDesign.md#http-service) |
| §7.1 · 7.1 服务器装配 | [技术方案 §6.2.1](TechnicalDesign.md#http-assembly) |
| §7.2 · 7.2 统一信封与错误 | [技术方案 §6.2.2](TechnicalDesign.md#http-envelope) |
| §7.3 · 7.3 10 个核心端点 | [技术方案 §6.2.3](TechnicalDesign.md#http-endpoints) |
| §7.4 · 7.4 管理台与追加端点（核心阶段收尾批次） | [技术方案 §6.2.4](TechnicalDesign.md#management-surface) |
| §7.5 · 7.5 限制与不做 | [技术方案 §6.2.5](TechnicalDesign.md#http-limits) |
| §8 · 8. 支撑模块 | [技术方案 §3、§6](TechnicalDesign.md#execution-inputs) |
| §8.1 · 8.1 工作区初始化（`basalt init`） | [技术方案 §3.1.1](TechnicalDesign.md#instance-init) |
| §8.2 · 8.2 AgentLoader：加载与校验 | [技术方案 §3.2](TechnicalDesign.md#agent-loader) |
| §8.3 · 8.3 ContextLoader：系统提示词 | [技术方案 §3.3](TechnicalDesign.md#prompt-inputs) |
| §8.4 · 8.4 Channel 与 CLI 交互 | [技术方案 §6.1.1](TechnicalDesign.md#terminal-channel) |
| §8.5 · 8.5 AgentScheduler（第三触发源） | [技术方案 §6.3](TechnicalDesign.md#scheduler) |
| §8.6 · 8.6 三种运行模式 | [技术方案 §6.5](TechnicalDesign.md#runtime-modes) |
| §8.7 · 8.7 CLI 命令（12 个，cobra） | [技术方案 §6.1.2](TechnicalDesign.md#cli-commands) |
| §8.8 · 8.8 ConfigLoader | [技术方案 §3.7](TechnicalDesign.md#config-loader) |
| §9 · 9. 数据持久化 | [技术方案 §5](TechnicalDesign.md#recovery) |
| §9.1 · 9.1 SQLite 装配 | [技术方案 §5.3](TechnicalDesign.md#sqlite-runtime) |
| §9.2 · 9.2 表清单 | [技术方案 §5.4](TechnicalDesign.md#persistence-tables) |
| §9.3 · 9.3 文件系统数据 | [技术方案 §5.5](TechnicalDesign.md#persistent-files) |
| §9.4 · 9.4 审计模型：从"有记录"到"能解释失败" | [技术方案 §4.5](TechnicalDesign.md#audit-links) |
| §10 · 10. 工程结构 | [技术方案 §7](TechnicalDesign.md#code-and-artifacts) |
| §10.1 · 10.1 包结构（依赖方向约束） | [技术方案 §7.1.2](TechnicalDesign.md#package-layout) |
| §10.2 · 10.2 构建与发布 | [技术方案 §7.2](TechnicalDesign.md#release-build) |
| §11 · 11. 目录协议与渐进披露（单一事实源） | [技术方案 §3](TechnicalDesign.md#execution-inputs) |
| §11.1 · 11.1 一个目录 = 一个 Agent | [技术方案 §3.1.2](TechnicalDesign.md#agent-directory) |
| §11.2 · 11.2 AGENT.md 完整样例 | [技术方案 §3.1.3](TechnicalDesign.md#agent-example) |
| §11.3 · 11.3 渐进披露（Skill 的三层，形态对齐 agentskills 开放标准） | [技术方案 §3.4.1](TechnicalDesign.md#skill-levels) |
| §11.4 · 11.4 核心文件系统 vs 扩展 API | [技术方案 §3.4.2](TechnicalDesign.md#configuration-writers) |
| §11.5 · 11.5 热加载：不可变配置快照 | [技术方案 §3.6](TechnicalDesign.md#immutable-snapshot) |
| §12 · 12. 五个每日端到端 Demo | [技术方案 §8.1](TechnicalDesign.md#daily-proof) |
| §12.1 · 12.1 可复现输入与依赖 | [技术方案 §8.1.1](TechnicalDesign.md#daily-proof-1) |
| §12.2 · 12.2 时钟、故障与证据 | [技术方案 §8.1.2](TechnicalDesign.md#daily-proof-2) |
| §13 · 13. 实施节奏 | [技术方案 §8.2](TechnicalDesign.md#delivery-sequence) |
| §13.1 · 13.1 开发 Harness 与 Loop | [技术方案 §8.2.1](TechnicalDesign.md#delivery-sequence-1) |
| §14 · 14. 性能目标 | [技术方案 §7.3](TechnicalDesign.md#performance-targets) |
| §15 · 15. 总结 | [技术方案 §8.3](TechnicalDesign.md#reliability-first) |

### 8.4 `AiProgrammingGuide.md` 的章节迁移

| 历史章节 | 当前定位 |
| --- | --- |
| §1 · 1. 实施总览 | [开发操作 §1](DevelopmentLoop.md#work-entry) |
| §1.1 · 1.1 两个阶段 | [开发操作 §1.1](DevelopmentLoop.md#next-action) |
| §1.2 · 1.2 输入映射（文档 → 工件） | [开发操作 §1.3](DevelopmentLoop.md#execution-artifacts) |
| §1.3 · 1.3 US 依赖图与节奏 | [开发操作 §4.1](DevelopmentLoop.md#us-dependencies) |
| §2 · 2. Spec-Kit 匹配度评估 | [开发操作 §3](DevelopmentLoop.md#planning-boundary) |
| §2.1 · 2.1 适配良好的部分（照搬） | [开发操作 §3.1](DevelopmentLoop.md#planning-tools) |
| §2.2 · 2.2 需要项目化改造的部分 | [开发操作 §3.2](DevelopmentLoop.md#before-code) |
| §3 · 3. 准备阶段（一次性） | [开发操作 §2](DevelopmentLoop.md#worksite) |
| §3.1 · 3.1 项目宪法（十条原则） | [开发操作 §6.1](DevelopmentLoop.md#engineering-invariants) |
| §3.2 · 3.2 仓库骨架与门禁 | [开发操作 §2.2](DevelopmentLoop.md#project-gates) |
| §3.3 · 3.3 写第一行代码前的人工确认清单 | [开发操作 §6.4](DevelopmentLoop.md#foundation-record) |
| §3.4 · 3.4 原型五链路验证（分阶段执行） | [开发操作 §5.2](DevelopmentLoop.md#runtime-validation) |
| §4 · 4. 逐 US 拆解 | [开发操作 §4](DevelopmentLoop.md#delivery-map) |
| §4.1 · 4.1 US-1 工程地基（骨架 + 门禁 + 配置 + 日志 + 工作区 + 持久化） | [开发操作 §4.2.1](DevelopmentLoop.md#us-1) |
| §4.2 · 4.2 US-2 Provider 对接（能力一） | [开发操作 §4.2.2](DevelopmentLoop.md#us-2) |
| §4.3 · 4.3 US-3 Agent 目录与 Profile 加载 | [开发操作 §4.2.3](DevelopmentLoop.md#us-3) |
| §4.4 · 4.4 US-4 ReAct 循环与首个工具闭环（能力二） | [开发操作 §4.2.4](DevelopmentLoop.md#us-4) |
| §4.5 · 4.5 US-5 Shell 与 HTTP 工具（Demo 一） | [开发操作 §4.3.1](DevelopmentLoop.md#us-5) |
| §4.6 · 4.6 US-6 Memory（能力三，Demo 二） | [开发操作 §4.3.2](DevelopmentLoop.md#us-6) |
| §4.7 · 4.7 US-7 MCP 集成（Demo 三） | [开发操作 §4.3.3](DevelopmentLoop.md#us-7) |
| §4.8 · 4.8 US-8 通知渠道 | [开发操作 §4.3.4](DevelopmentLoop.md#us-8) |
| §4.9 · 4.9 US-9 Web Service（能力五，Demo 四 / 五） | [开发操作 §4.4.1](DevelopmentLoop.md#us-9) |
| §4.10 · 4.10 US-10 定时任务 | [开发操作 §4.4.2](DevelopmentLoop.md#us-10) |
| §4.11 · 4.11 US-11 观测与管理台 | [开发操作 §4.4.3](DevelopmentLoop.md#us-11) |
| §4.12 · 4.12 US-12 Agent 动态管理 | [开发操作 §4.4.4](DevelopmentLoop.md#us-12) |
| §4.13 · 4.13 US-13 官网展示主页（对外门面） | [开发操作 §4.5.1](DevelopmentLoop.md#us-13) |
| §4.14 · 4.14 US-14 发布收尾（五个每日 Demo + v0.1.0） | [开发操作 §4.5.2](DevelopmentLoop.md#us-14) |
| §4.15 · 4.15 协作模式（人与 AI 分工） | [开发操作 §1.2](DevelopmentLoop.md#delivery-owners) |
| §4.16 · 4.16 跑偏纠正表 | [开发操作 §3.3](DevelopmentLoop.md#scope-correction) |
| §5 · 5. 交付物（每个 US 的完成定义） | [开发操作 §5.1](DevelopmentLoop.md#module-evidence) |
| §6 · 6. 增量阶段提示词模板（可直接复制） | [开发操作 §8.5](DevelopmentLoop.md#work-prompts) |
| §7 · 7. 工程陷阱表（Go 实现与可复现反例） | [开发操作 §7](DevelopmentLoop.md#failure-cases) |
| §8 · 8. 风险与纪律 | [开发操作 §6](DevelopmentLoop.md#engineering-boundary) |
| §8.1 · 8.1 风险 | [开发操作 §6.3](DevelopmentLoop.md#engineering-risks) |
| §8.2 · 8.2 纪律（不可违背） | [开发操作 §6.2](DevelopmentLoop.md#change-discipline) |
| §8.3 · 8.3 工程治理八项（2026-10-15 立；逐项带生效时点与可验证判据） | [开发操作 §8.1](DevelopmentLoop.md#governance) |
| §8.4 · 8.4 mock 模型集成测试口径（六条，2026-10-15 定；US-14 落地） | [开发操作 §5.3](DevelopmentLoop.md#release-e2e) |
| §9 · 9. Loop 编程操作规程 | [开发操作 §9](DevelopmentLoop.md#loop-protocol) |
| §9.1 · 9.1 输入：项目基准、公共底座、模块执行卡 | [开发操作 §9.1](DevelopmentLoop.md#loop-protocol-1) |
| §9.2 · 9.2 进入循环与已有工作区保护 | [开发操作 §9.2](DevelopmentLoop.md#loop-protocol-2) |
| §9.3 · 9.3 三层 Harness 与验收所有权 | [开发操作 §9.3](DevelopmentLoop.md#loop-protocol-3) |
| §9.4 · 9.4 单轮执行与反馈 | [开发操作 §9.4](DevelopmentLoop.md#loop-protocol-4) |
| §9.5 · 9.5 持久状态、指纹与中断恢复 | [开发操作 §9.5](DevelopmentLoop.md#loop-protocol-5) |
| §9.6 · 9.6 停止条件与授权边界 | [开发操作 §9.6](DevelopmentLoop.md#loop-protocol-6) |
| §9.7 · 9.7 独立复核、反馈归属与最小交接 | [开发操作 §9.7](DevelopmentLoop.md#loop-protocol-7) |
| §9.8 · 9.8 可直接使用的启动提示词 | [开发操作 §9.8](DevelopmentLoop.md#loop-protocol-8) |
| §9.9 · 9.9 两次系统整合 | [开发操作 §9.9](DevelopmentLoop.md#loop-protocol-9) |
| §10 · 10. 开发机准备与起步操作（合并正文） | [开发操作 §10](DevelopmentLoop.md#environment) |
| §10.1 · 10.1. 开工前先定三件事 | [开发操作 §10.1](DevelopmentLoop.md#environment-1) |
| §10.1.1 · 10.1.1 模块路径（module path） | [开发操作 §10.1.1](DevelopmentLoop.md#environment-1-1) |
| §10.1.2 · 10.1.2 远程仓库（决定 CI 是否生效） | [开发操作 §10.1.2](DevelopmentLoop.md#environment-1-2) |
| §10.1.3 · 10.1.3 本机装不装 C 编译器（决定能否本地跑竞态检测） | [开发操作 §10.1.3](DevelopmentLoop.md#environment-1-3) |
| §10.1.4 · 10.1.4 定案记录（2026-10-15） | [开发操作 §10.1.4](DevelopmentLoop.md#environment-1-4) |
| §10.2 · 10.2. 环境准备（你执行） | [开发操作 §10.2](DevelopmentLoop.md#environment-2) |
| §10.2.1 · 10.2.1 现状盘点（本机实测，2026-10-15） | [开发操作 §10.2.1](DevelopmentLoop.md#environment-2-1) |
| §10.2.2 · 10.2.2 装 Go 1.27.2 | [开发操作 §10.2.2](DevelopmentLoop.md#environment-2-2) |
| §10.2.3 · 10.2.3 装 make | [开发操作 §10.2.3](DevelopmentLoop.md#environment-2-3) |
| §10.2.4 · 10.2.4 装 GCC（`-race` 需要，见 10.1.3） | [开发操作 §10.2.4](DevelopmentLoop.md#environment-2-4) |
| §10.2.5 · 10.2.5 装四个 Go 工具（`go install`） | [开发操作 §10.2.5](DevelopmentLoop.md#environment-2-5) |
| §10.2.6 · 10.2.6 Go 环境变量与一次探针验证 | [开发操作 §10.2.6](DevelopmentLoop.md#environment-2-6) |
| §10.2.7 · 10.2.7 现在**不要**装的东西 | [开发操作 §10.2.7](DevelopmentLoop.md#environment-2-7) |
| §10.2.8 · 10.2.8 环境完成判据 | [开发操作 §10.2.8](DevelopmentLoop.md#environment-2-8) |
| §10.3 · 10.3. 仓库初始化（你执行） | [开发操作 §10.3](DevelopmentLoop.md#environment-3) |
| §10.3.1 · 10.3.1 `git init` 与三个仓库级配置文件 | [开发操作 §10.3.1](DevelopmentLoop.md#environment-3-1) |
| §10.3.2 · 10.3.2 首个 commit | [开发操作 §10.3.2](DevelopmentLoop.md#environment-3-2) |
| §10.3.3 · 10.3.3 远程仓库与 CI（可选） | [开发操作 §10.3.3](DevelopmentLoop.md#environment-3-3) |
| §10.3.4 · 10.3.4 完成判据 | [开发操作 §10.3.4](DevelopmentLoop.md#environment-3-4) |
| §10.4 · 10.4 原型时序的历史裁决（2026-10-15） | [开发操作 §10.4](DevelopmentLoop.md#environment-4) |
| §10.5 · 10.5 从起步转入开发循环 | [开发操作 §10.5](DevelopmentLoop.md#environment-5) |
| §10.6 · 10.6. 提交、版本锚点与发布形态 | [开发操作 §10.6](DevelopmentLoop.md#environment-6) |
| §10.7 · 10.7. 排障 | [开发操作 §10.7](DevelopmentLoop.md#environment-7) |
| §10.8 · 10.8. 本指南的验证状态（如实登记） | [开发操作 §10.8](DevelopmentLoop.md#environment-8) |
| §11 · 11. 文档维护与证据同步 | [开发操作 §8.2–8.4](DevelopmentLoop.md#document-ownership) |
| §11.1 · 11.1 单一正文与来源边界 | [开发操作 §8.2](DevelopmentLoop.md#document-ownership) |
| §11.2 · 11.2 已冻结模块与项目文档修订 | [开发操作 §8.3](DevelopmentLoop.md#frozen-inputs) |
| §11.3 · 11.3 文档变更的最低检查 | [开发操作 §8.4](DevelopmentLoop.md#document-checks) |
