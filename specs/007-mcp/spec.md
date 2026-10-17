# Feature Specification: US-7 MCP 集成（stdio 客户端 + 工具适配 + 故障隔离）

**Feature Branch**: `007-mcp`

**Created**: 2026-10-17

**Status**: Draft

**Input**: User description: 颗粒度文档 `docs/us/us7-mcp.md`（第 1、2、5 节）——让 Basalt 用**零代码**方式接入外部能力：读工作区根部的 `mcp_servers.yaml`，以 **stdio** 方式启动并连接各 MCP server，把 server 提供的工具**适配成与内置工具同形**的注册池成员；单个 server 连不上只 WARN 跳过、**不中断启动**，依赖它的 Agent 因"工具解析不到"**整只不注册**。

> **事实源与裁决**：本规格从 US-7 颗粒度文档 `docs/us/us7-mcp.md` 派生，供 speckit 链（plan / tasks / analyze）消费；冲突时以四份权威文档的裁决顺位为准——**原则**以 `.specify/memory/constitution.md` 为准、**实现设计**以 `docs/TechnicalSolution.md` 为准（§1.1 决策 1 允许协议 SDK / 决策 8 单二进制、§1.2 栈表 MCP 坐标、§6.3 零代码扩展、§6.4 注册池与准入、§6.5 授权矩阵与信任边界、§8.8 `${VAR}` 装配期解析、§10.1 包结构、§11.5 热加载范围、§13 US-7 行）、**固定字面量**以 `docs/contracts/` 为准（`agent-md.md` 的 `mcp_servers` 字段与规则 4 / 6 文案、`run-and-errors.md` 的 `error_kind` 七值）。形态裁决的**附加事实源** = `docs/Compatibility.md` §2.1（工作区布局一致、文件内容零改动）与 §2.4（热加载范围）。内部类型名、包落位与测试文件组织归 plan 与该文档 §3，不在本规格展开。可自动化验收由该文档「验收测试（Harness）」§4 承载（默认层新增 6 文件 51 条 + 改动 2 文件追加 6 条 + 夹具改造；集成层真实子进程 5 条）；人工项见其 §6.1 十三项。**设计面已冻结**（该文档「待决事项」三条已于 2026-10-17 全部裁决为方案 A：非 stdio 拒绝启动 / 轻路径引入第三态 `unverified` / 子进程环境复用 `shell.env_passthrough`），实现期若冒出新的未定口径，**必须**回该文档登记后再动手，不静默自决。

## Clarifications

### Session 2026-10-17

- **会话记录（2026-10-17）**：歧义扫描**零提问**——规格从颗粒度文档派生、设计面已冻结（「待决事项」三条全部裁决落案；§6.2 固定口径 25 条），四份事实源（宪法 / 技术方案 / 契约 / 颗粒度文档）逐条对拍后无悬空项。以下为扫描中就地收敛的项（口径在事实源内可查，按「先回事实源找口径、找到即按它收敛」执行，**不向用户提问**）：
- **待决 #1 落案（2026-10-17 用户裁决，来源：颗粒度文档「待决事项」#1）**：非 stdio `transport`（含 `sse` / `http` 等任何非空且非 `stdio` 的值）→ **拒绝启动**（方案 A，fail-closed），不沿用 NivroOS 的 WARN 后跳过。`transport` 缺省或空串 = `stdio`（来源：§2.3 伪代码判据 `transport != "" && transport != "stdio"`）。
- **待决 #2 落案（2026-10-17 用户裁决，来源：颗粒度文档「待决事项」#2）**：轻路径引入**第三态 `unverified`**（方案 A）——只改变展示与摘要（`profile list` 显示 `registered=unverified`），**不改变运行面**（运行仍只收已注册条目）；零 MCP 配置时轻路径与 US-6 逐字节相同。
- **待决 #3 落案（2026-10-17 用户裁决，来源：颗粒度文档「待决事项」#3）**：MCP 子进程环境**复用 `shell.env_passthrough`**（方案 A）——透传白名单 + 配置文件 `env` 声明覆盖同名键，其余清空；空透传 = 不继承任何变量（结果为非 nil 空集）。
- **收敛项 ① 零工具 server 的语义**（来源：约束 5 的连接成功判据 + 约束 23 的 `mcp_connected` 定义）：连接成功 = 初始化 + 列工具两步成功，与工具数量无关；server 连上但工具列表为空 → 计入 `mcp_connected`、注册 0 件工具、启动照常，**不**视为失败。
- **收敛项 ② 准入规则的判据集与顺序**（来源：颗粒度文档 §2.3 伪代码 C 与约束 6）：重路径规则 4 用**注册后**名字集（含 MCP 工具）；规则 6 先判「server ∈ 已配置」，再判「server ∈ 已连接」——已配置但未连接的 server 与未配置同等报 `mcp server skipped`；两条规则都按**声明序**报首个失败名，规则 4 先于规则 6。

## User Scenarios & Testing *(mandatory)*

**标签口径（`[US1]`~`[US4]` 不混淆）**：下面四个用户故事在 `tasks.md` 中以 Spec-Kit 的工具标签 `[US1]`~`[US4]` 出现——那是**本 feature 内部**用户故事的编号，**不是**项目台账（[docs/us/REGISTRY.md](../../docs/us/REGISTRY.md)）里的 US-1 ~ US-7。本 feature 的四个故事同属项目 **US-7（MCP 集成）** 一个模块，是它的四个独立可验场景；标签按工具产出保留。

### User Story 1 - 零代码接入走通：配置文件 → 连接 → 工具同形可用（Priority: P1）

运营者要接入一个新能力时**不改代码**：在工作区根部的 `mcp_servers.yaml` 里登记一个现成的 MCP server（`name` / `transport: stdio` / `command` / `env` 用 `${VAR}` 占位），在 Agent 的 `AGENT.md` 的 `tools:` 与 `mcp_servers:` 里声明其工具与 server 名。启动后该 server 被拉起并完成握手、其工具出现在工具清单里；模型调用时与内置工具**完全同形**——注册、声明、调度、审计、失败分类都不因来源不同而分叉。这条链路就是**验收 Demo 三**（零代码 PR digest）。

**Why this priority**: 它是本模块的旗舰路径与全部价值的前提——没有"连上并把工具交出去"，其余故事（隔离、安全、可审计）都无从谈起。放 P1 还因为它是**唯一的正向链路**：这条不对，外部能力对 Basalt 完全不可见。

**Independent Test**: 以**进程内 MCP 测试对**（内存传输 + 脚本化工具）驱动真装配：给定一份含 1 个 server（2 个工具）的工作区与一个声明这些工具的 Agent，断言：①启动成功且工具注册数 = 内置 8 + MCP 2；②`AGENT.md` 声明的工具可被解析（Agent 注册）；③模型侧工具清单含 MCP 工具名、描述与入参 schema 与 server 声明**逐字一致**；④一次成功调用的结果文本按固定规则渲染并回填；⑤审计表出现该调用的完整两阶段行；⑥启动摘要 `mcp_connected = 1`。全程不启动真实子进程。

**Acceptance Scenarios**:

1. **Given** 工作区有 `mcp_servers.yaml`（1 个 server，2 个工具）与一个在 `tools:` / `mcp_servers:` 中声明它们的 Agent，**When** 执行重路径装配，**Then** 装配成功、注册池 = 内置 8 + MCP 2、该 Agent 状态为已注册、启动摘要 `mcp_connected = 1`。
2. **Given** 已注册的 MCP 工具，**When** 模型发起一次工具调用，**Then** server 收到的入参字节与原入参**一致**、结果按固定渲染规则返回、审计行经历 `pending → succeeded`、失败路径则 WARN 且终态如实。
3. **Given** 工具名与内置工具**重名**的 MCP server，**When** 注册，**Then** 内置工具保留、后到者被拒绝并出现 `拒绝重复注册的工具` WARN，解析同名工具的既有语义不漂移。
4. **Given** 零 MCP 配置（文件缺失 / 空 / `{}`），**When** 启动，**Then** 不建连接、不发连接日志、启动摘要 `mcp_connected = 0`，系统行为与 US-6 **逐字节相同**。

---

### User Story 2 - 故障隔离与准入一致：坏 server 不阻断启动、依赖它的 Agent 整只不注册（Priority: P2）

配置里某个 server 的 `command` 写错（可执行文件不存在 / 初始化失败 / 列工具失败）时，启动**照常完成**：该 server 被 WARN 点名跳过，其余 server 照常连接、其余 Agent 照常可聊；依赖失败 server 的 Agent 因工具解析不到而**整只不注册**——声明了它的工具的命中「`tool not registered: <tool>`」，只声明 server 名的命中「`mcp server skipped: <server>`」。轻路径（`profile list`）不连接 MCP，因此对**可能依赖 MCP** 的 Agent 给出第三态「待确认（`unverified`）」，而不是误报为拒载。

**Why this priority**: 故障隔离是"单二进制多 Agent 底座"的生存条件——一个坏配置让整个实例起不来，等于把配置错误放大成全站故障。两条准入规则的二阶后果与两路径一致性决定了运维看到的诊断是否指得准。放 P2 是因为它建立在 P1 的连接链路之上。

**Independent Test**: 以**可注入失败的假连接器**驱动真装配：①一个 server 失败、一个成功 → 启动成功、失败项 WARN 含固定键 `server` / `reason` / `detail`、成功项工具可用；②声明失败 server 工具的 Agent → 拒载且原因 = `tool not registered: <tool>`；③只声明失败 server 名的 Agent → 拒载且原因 = `mcp server skipped: <server>`；④两条声明同时存在时规则 4 先报；⑤轻路径对零 MCP 配置的 Agent 结论与 US-6 一致，对可能依赖 MCP 的 Agent 标 `unverified` 且**不**进入运行面；⑥被其他确定性规则拒绝的 Agent 仍按原规则拒绝。

**Acceptance Scenarios**:

1. **Given** 两个 server，其一的 `command` 指向不存在的可执行文件，**When** 启动，**Then** 启动成功；日志出现「MCP server 未连接——已跳过」并点名 server；另一个 server 的工具可用；摘要 `mcp_connected` = 实际连上数。
2. **Given** server 连接失败，**When** 装载声明其工具的 Agent，**Then** 该 Agent 整只不注册、原因为 `tool not registered: <tool>`；只声明 server 名的 Agent 原因为 `mcp server skipped: <server>`；两者同时声明时先报前者。
3. **Given** 存在 MCP 配置且轻路径未连接，**When** `profile list`，**Then** 可能依赖 MCP 的 Agent 显示 `unverified`，零 MCP 配置时与 US-6 逐字节一致，运行面仍只收已注册条目。
4. **Given** Agent 同时违反一条确定性规则（如缺 frontmatter 必填字段）与可能依赖 MCP，**When** 轻路径装载，**Then** 按确定性规则拒绝，不因 `unverified` 而放行。

---

### User Story 3 - 凭证与子进程边界：配置 fail-closed、env 只允许占位、子进程环境可预测（Priority: P3）

`mcp_servers.yaml` 的每次启动校验都是**拒绝式**的：形态写错、缺 `name` / `command`、server 名重复、`transport` 非 stdio、`env` 值是明文或半截占位 → **拒绝启动**并点名文件、条目或 server 与字段，绝不静默跳过；`${VAR}` 在装配期解析，未设置或值为空 → 拒绝启动；**错误文案与日志永不回显配置值**（值可能正是凭证）。MCP server 子进程的环境 = 透传白名单 + 配置声明覆盖（其余清空），工作目录 = 工作区根，`command` 只按空白切分、不经 shell——配置字面量不能被解释成可注入命令行。

**Why this priority**: 这三处都是"错了不立刻显形"的安全面——静默跳过要到工具消失才被发现，明文凭证要到日志外泄才被发现，shell 解释要到被注入才被发现。放 P3 是因为它守住 P1 / P2 的边界而非新增功能。

**Independent Test**: ①逐条负向配置（顶层非映射 / `servers` 非列表 / 条目非映射 / 缺 `name` / 缺 `command` / 重名 / `env` 非映射 / 明文 / 半截占位 / 非 stdio）→ 拒绝启动、错误点名、且**不含**被拒值；②未设置 / 空值变量 → 拒绝启动、点名 server 与变量；③子进程环境恰为「白名单 + 声明」、声明覆盖白名单、空透传 = 不继承任何变量；④子进程 cwd = 工作区根；⑤`command` 含引号不被解释、`sh -c` 形态不被接受为 shell。

**Acceptance Scenarios**:

1. **Given** `env` 值为明文 token，**When** 启动，**Then** 拒绝启动；错误与日志**不包含**该 token（负向断言逐字）。
2. **Given** `env` 值为 `${MISSING_VAR}` 且该变量未设置或为空，**When** 启动，**Then** 拒绝启动并点名 `server <name>`、`env.<key>` 与变量名。
3. **Given** 两个 server 的环境透传配置相同而声明不同，**When** 各自启动，**Then** 子进程环境恰等于「透传白名单 + 声明覆盖」，不含父进程的其余变量；空透传时结果为**非 nil 空集**（一个都不继承）。
4. **Given** `command` 含需引号才能表达的空格路径或 `.cmd` 文件，**When** 启动，**Then** 按空白切分如实失败或需显式写解释器——**不得**自动包裹 `cmd /c`、**不得**引入 shell。

---

### User Story 4 - 同形、可审计与生命周期：来源透明、两阶段审计、预算与进程分离、停机有序（Priority: P4）

MCP 工具在循环与执行器眼里与内置工具**没有区别**；调用经执行器完整两阶段审计（先 `pending` 再终态），适配器自身不落审计、不写消息；失败分类严守边界——`IsError` 与协议错误都归外部错误、可观测正文一律自产固定文案（server 文本与原始错误不进日志 / 审计 / 模型上下文），取消 / 截止的身份优先于外部错误；列工具分页游标驱动、重复游标即失败；30 秒握手预算**只罩握手与列工具**，预算释放后子进程存活到池关闭；停机顺序 = MCP → 数据库 → 实例锁、池关闭幂等；`mcp_servers.yaml` 变更需重启（不热加载）。

**Why this priority**: 这些是"实现错了不一定立刻红"的判别力面——审计缺一行、错误正文漏进模型、预算误杀进程，都要靠专门用例钉住。放 P4 是因为它们收敛 P1~P3 的行为，而非新增用户可见能力。

**Independent Test**: ①阻塞 server 的调用期间，审计存储已见 `pending` 行；②`IsError` / 协议错误 / 取消三档分别断言 Reason 逐字、正文不出现、分类正确；③分页与重复游标终止；④真实子进程在握手预算释放并等待后仍可调用（集成层）；⑤`Close` 幂等，停机顺序以可观察证据（库版本读数）验证；⑥改配置文件不重建连接、重启后生效。

**Acceptance Scenarios**:

1. **Given** server 的处理函数阻塞，**When** 发起工具调用，**Then** 审计存储里已存在 `pending` 行；放行后同一行更新为终态。
2. **Given** server 返回 `IsError = true` 且文本含敏感值，**When** 工具返回，**Then** 结果归外部错误、可观测正文 = 固定文案 `MCP 工具返回错误：<server>/<tool>`、敏感值不出现在日志 / 审计 / 模型上下文**任何一处**。
3. **Given** 协议 / 传输错误，**When** 工具返回，**Then** 可观测正文 = 固定文案 `MCP 调用失败：<server>/<tool>`、原始错误只在原因链；**Given** 上下文已取消，**Then** 返回上下文原错误、**不得**包成外部错误。
4. **Given** 已连接的真实子进程与已释放的握手预算，**When** 等待一段时间后再次调用工具，**Then** 调用成功（预算不杀进程）；`Close` 两次不报错；停机顺序为 MCP → 库 → 锁。

---

### Edge Cases

- **零 server**：文件缺失 / 空文件 / `{}` / 无 `servers` 键 → 零 server，不建连接、不发日志；启动摘要 `mcp_connected = 0` 仍显式输出。
- **`{}` 与 `null` 文档**：`{}` 与纯注释 / `null` 文档均表示零 server，不报错。
- **未知字段**：条目里的未知键被忽略（前向兼容），不影响其余字段校验。
- **`transport` 缺省**：缺省或空串 = stdio；写 `sse` / `http` 等 → 拒绝启动（v1 只支持 stdio）。
- **`command` 纯空白**：视同缺失 → 拒绝启动。
- **server 名重复**：加载期即拒绝启动（不进入连接阶段）。
- **半截占位与明文**：`${A}${B}` / `${A}x` / `$A` / 明文 → 拒绝启动；错误不含值。
- **变量为空串**：与未设置同档 → 拒绝启动（不得静默空凭证）。
- **连接后列工具失败**：视为该 server 未连接——关闭半截会话、WARN、跳过；其工具不注册。
- **重复游标**：该 server 列工具失败并跳过，不得无限循环。
- **工具名为空或描述为空**：原样透传（server 侧责任），Basalt 不做二次校验。
- **非文本内容块**：渲染为类型化占位符，不回显 base64；全空结果有固定兜底文案。
- **大整数入参**（> 2^53）：原样字节透传，不得经浮点失真。
- **预算释放后的子进程**：继续存活到池关闭；预算取消**不得**关闭连接。
- **`Close` 与在途调用交错**：不 panic，`-race` 无告警。
- **配置热改**：运行中改 `mcp_servers.yaml` 不生效，重启后生效。
- **轻路径**：不连接 MCP；零配置时与 US-6 逐字节一致；有配置时可能依赖 MCP 的 Agent 标 `unverified`，不误报为拒载。

## Requirements *(mandatory)*

### Functional Requirements

#### 配置文件与装配期校验

- **FR-001**: `mcp_servers.yaml` 形态 MUST 为**顶层映射 + `servers:` 列表**；条目字段 = `name` / `transport` / `command` / `env`；列表顺序 MUST 原样保留（即连接顺序与工具注册顺序）；未知字段 MUST 被忽略。MUST NOT 采用「顶层键名集」形态——NivroOS 迁移承诺是**文件内容零改动**（Compatibility §2.1）。
- **FR-002**: 文件缺失、空文件、`{}`（含纯注释 / `null` 文档）、无 `servers` 键 → MUST 视为**零 server** 并整段跳过：不建连接、不发连接日志；零 MCP 配置时系统行为 MUST 与 US-6 逐字节相同。
- **FR-003**: 以下任一情形的 MUST **拒绝启动**（fail-closed），错误点名 `mcp_servers.yaml`、条目序号或 server 名与字段：顶层非映射、`servers` 非列表、条目非映射、`name` 缺失、`command` 缺失或纯空白、server 名重复、`env` 非映射、`transport` 为非空且非 `stdio` 的值。MUST NOT 静默跳过任何一条非法配置。
- **FR-004**: `env` 值 MUST 只允许精确形态 `${VAR}`（人名规则：`^${[A-Za-z_][A-Za-z0-9_]*}$`）；明文、半截占位（`${A}${B}`、`${A}x`、`$A`）MUST 拒绝启动。错误文案与日志 MUST NOT 回显配置值（值可能正是凭证）。
- **FR-005**: `${VAR}` 的解析责任 MUST 在**装配层**；工作区解析 MUST 只校验形态、不读环境变量。变量未设置**或值为空串** MUST 拒绝启动，点名 `server <name>`、`env.<key>` 与变量名；MUST NOT 静默用空串。解析后的值 MUST NOT 进入错误与日志。

#### 连接、分页与生命周期

- **FR-006**: 连接 MUST 逐 server **串行**执行「启动子进程 → 初始化 → 列工具」；每个 server 的握手预算 MUST 为 **30 秒代码常量**（不新增配置键），且只覆盖初始化与列工具两步。任一步失败 MUST 记 WARN（固定键 `server` / `reason` / `detail`；`detail` 为固定短语集）、关闭半截会话并**继续下一个**；启动 MUST NOT 中断。
- **FR-007**: 连接过程 MUST 只在**整体上下文取消 / 截止**时返回错误；其余失败 MUST 一律吸收为跳过（WARN），不得向上失败。
- **FR-008**: 启动子进程 MUST NOT 接收上下文、MUST NOT 使用可取消的启动方式；握手预算释放 MUST NOT 关闭会话或杀死子进程——子进程 MUST 存活到连接池关闭（进程生命周期与握手预算分离）。
- **FR-009**: 列工具 MUST 以游标驱动直到空游标；**同一游标第二次出现** MUST 判定为该 server 列工具失败（WARN + 跳过），MUST NOT 无限循环。
- **FR-010**: 停机顺序 MUST 为**连接池 → 数据库 → 实例锁**；池关闭 MUST 幂等（重复调用不报错）；逐会话关闭 MUST 完成子进程收尾；任一步失败 MUST 合并返回，MUST NOT 静默。
- **FR-011**: `mcp_servers.yaml` 的变更 MUST **重启后生效**；本模块 MUST NOT 接入文件监听、MUST NOT 重建连接池。

#### 工具适配与注册

- **FR-012**: MCP 工具 MUST 与内置工具在工具接口、注册池、`AGENT.md` 的 `tools:` 声明、运行时工具池、审计、失败分类上**完全同形**；工具名 MUST 原样使用（不加前缀、不做命名空间）；`InputSchema` 与描述 MUST 原样透传；循环与执行器 MUST NOT 按来源分支。
- **FR-013**: 注册顺序 MUST 为**内置工具先、MCP 工具后**（server 文件序 → 工具列表序）；重名 MUST **保留先注册者**并 WARN `拒绝重复注册的工具`——内置优先，既有语义不漂移。
- **FR-014**: 既有「注册池名字集 == 声明的内置工具名集」等值门禁 MUST 先于 MCP 注册执行；MUST NOT 把 MCP 注册挪到门禁之前（否则门禁因池变大拒绝启动）。
- **FR-015**: 调用入参 MUST 原样透传：空串 / `null` → 空对象（MUST NOT 传 `null`）；其余 MUST 返回**原始字节**交给协议层序列化，MUST NOT 先反序列化成通用 map（大整数 > 2^53 会无报错地失真）。非法 / 非对象入参（执行器已挡，防御分支）MUST 归入参错误分类。
- **FR-016**: 结果渲染 MUST 固定：文本块按序以换行连接；非文本块 MUST 渲染为**类型化占位符**（如 `[非文本内容：image image/png]`），MUST NOT 把 base64 原文放进模型上下文；无文本块时用结构化内容的 JSON；全空 MUST 返回固定兜底文案。

#### 失败分类与安全边界

- **FR-017**: 工具级错误（`IsError = true`）MUST 归外部错误分类，`Reason` = 固定文案 `MCP 工具返回错误：<server>/<tool>`；server 返回的文本 MUST **整段丢弃**（不进入 `Reason` / `Cause` / 日志 / 审计 / 模型上下文）。MUST NOT 把 `IsError` 当成功返回；MUST NOT 单行化 / 截断后塞进 `Reason`（截断不是脱敏）。
- **FR-018**: 传输 / 协议错误 MUST 归外部错误分类，`Target = mcp:<server>/<tool>`、`Reason` = 固定文案 `MCP 调用失败：<server>/<tool>`；原始错误 MUST 只进原因链（供 `errors.Is` / `errors.As` 与进程内排障），MUST NOT 写进任何可观测面。
- **FR-019**: 取消 / 截止的**身份优先**：适配器 MUST 先判上下文错误（与 HTTP 工具同一判序函数）；上下文已取消 / 截止 → MUST 返回上下文原错误（归超时分类），MUST NOT 包成外部错误。
- **FR-020**: 错误正文安全边界 MUST 统一：可观测正文一律自产固定文案；原始错误只进原因链；`IsError` 的 server 文本整段丢弃；连接失败日志的 `detail` MUST 用固定短语集（可执行文件不存在 / 权限不足 / 超时 / 已取消 / 子进程退出码 N / 外部交互失败），MUST NOT 用原始错误单行化 / 截断充数。一句话边界：外部正文可进原因链供进程内排障，但**永不**进任何可观测面。

#### 审计

- **FR-021**: MCP 工具调用 MUST 经既有工具执行器完成两阶段审计（执行前写 `pending`、执行后更新同一行终态、失败再记 WARN）；适配器自身 MUST NOT 落审计、写消息或记日志；`pending` 预写失败 MUST NOT 发出调用（fail-closed）。

#### 子进程边界

- **FR-022**: MCP 子进程环境 MUST = **透传白名单 + 配置声明覆盖**（其余清空）；透传清单 MUST 复用 `shell.env_passthrough`——只透传**显式登记**的变量，示例配置登记 `PATH` / `HOME` / `LANG` / `TZ`，**代码无内置默认白名单**（省略或配置为空列表 → 不透传父进程环境变量）；声明的 `env` MUST 覆盖同名透传键；结果 MUST **永不为 nil**（空集 = 一个都不继承）；MUST NOT 继承完整父进程环境。透传与声明的合并语义 MUST 只有一处实现，`shell` 工具与 MCP 共用。
- **FR-023**: MCP 子进程工作目录 MUST = **工作区根**（实例级资源，不绑定单个 Agent）；MUST NOT 使用进程默认 CWD。
- **FR-024**: `command` MUST 只按空白切分（首个 token = 可执行文件）；MUST NOT 有 shell 引号语义、MUST NOT 自动包裹 `cmd /c`、MUST NOT 引入 shell（`sh -c`）——配置字面量不得变成可注入命令行。

#### Agent 准入与两路径一致

- **FR-025**: 连接失败的**二阶后果** MUST 如实呈现：失败 server 的工具不进注册池 → 声明该工具的 Agent 命中规则 4（`tool not registered: <tool>`）；只声明 server 名的 Agent 命中规则 6（`mcp server skipped: <server>`）；两条规则各自按**声明序**报首个失败名，规则 4 MUST 先于规则 6 判定。
- **FR-026**: 轻 / 重两条路 MUST 对同一份 `AGENT.md` 给出一致结论：重路径注入「已配置 + 已连接」名字集；轻路径不连接 MCP，对**可能依赖 MCP** 的 Agent MUST 给出第三态「待确认（`unverified`）」，只改变展示与摘要、MUST NOT 改变运行面（运行仍只收已注册条目）；被其他确定性规则拒绝的 MUST 仍按原规则拒绝，MUST NOT 因待确认而放行；零 MCP 配置时轻路径 MUST 与 US-6 逐字节相同。

#### 可观测、信任边界与纪律

- **FR-027**: 启动摘要 MUST 为一条 INFO，键 = `agents` / `tools` / `mcp_connected`；`mcp_connected` MUST 为**实际连上**的 server 数（非配置数）；零配置时 MUST 显式输出 `0`，MUST NOT 省略（键集稳定才可机器读）。
- **FR-028**: 信任边界 MUST 如实登记：MCP server 的文件 / 网络行为**不经** Policy；可治理面 = 管理员登记的配置行；v1 定位 = 经评审的 server；文档与日志 MUST NOT 宣称 MCP 工具受沙箱白名单约束。
- **FR-029**: 协议 SDK MUST 只做协议编解码、只出现在工具包；MUST NOT 启用 SDK 的自动工具循环 / 托管能力；工具调度唯一入口 MUST 仍是既有执行器；工作区解析 MUST NOT 依赖工具包，工具包 MUST NOT 依赖装配层 / 命令面。
- **FR-030**: 范围 MUST 零膨胀：零新增配置键、零迁移、`basalt.yaml.example` 零改动、`schema.sql` 零 diff；`go.mod` MUST 只新增一个直接坐标 `github.com/modelcontextprotocol/go-sdk v1.8.0`（技术方案 §1.2 栈表已批准）；30 秒预算、超时与渲染文案 MUST 都是代码常量。

### 自动化验收锚点（关键回归守卫）

自动化验收由颗粒度文档「验收测试（Harness）」§4 的测试套件承载（测试全绿即通过）。关键回归守卫（实现期 MUST 原样落地断言逻辑，函数名按工程规范译为英文、文档原文以注释保留）：

| 守卫点 | 用例（颗粒度文档 §4.3） |
| --- | --- |
| 明文 `env` 被拒且错误 / 日志不含该值 | R1a |
| 装配期 `env` 解析 fail-closed（未设置 / 空值拒绝、不静默空串） | R1b |
| 单 server 失败隔离（WARN + 跳过 + 其余照常 + 启动成功） | R2 |
| 等值门禁先于 MCP 注册（注册挪前 → 装配失败红） | R3 |
| 规则 4 先于规则 6 且失败 server 的 Agent 整只不注册 | R4 |
| `IsError` → 外部错误且 server 文本不出现在任何可观测面（含敏感值负向） | R5 |
| 取消身份优先（判定**不是**外部错误） | R6 |
| 分页与重复游标终止（不得无限循环） | R7 |
| 重名先到先得（保留者可被解析、后到者不存在） | R8 |
| 阻塞 server 期间审计已见 `pending` 行（两阶段审计） | R9 |
| 关闭幂等 + 停机顺序（池 → 库 → 锁，可观察证据） | R10a / R10b |
| 真实子进程在握手预算释放后仍可调用（生命周期与预算分离，集成层） | R11 |
| 连接 / 协议失败日志与错误不含敏感值 | R12 |
| 大整数入参经原样字节透传不失真 | R13 |
| 配置文件解析面：缺失 / 空 / `{}` / 未知字段 / 形态各负向 / 顺序保留 | `workspace/mcp_test.go`（18 条） |
| 适配器面：入参透传 / 渲染各档 / `IsError` / 协议错误 / 取消 / 分页 / 重名 / 关闭 | `tool/mcp_test.go`（18 条） |
| 真实子进程集成面：握手 / 调用 / 预算释放存活 / 停机 / 敏感值不回显 | `tool/mcp_integration_test.go`（5 条，`-tags=integration`） |
| 装配面：env 解析 / 连接 / 注册顺序 / 门禁位置 / 失败隔离 / 摘要 / 停机顺序 | `app/wire_mcp_test.go`（7 条）+ `app/mcp_test.go`（1 条） |
| 测试基建自身：内存测试对的工具声明与调用行为 | `testutil/mcpserver_test.go`（2 条） |
| 装载器两路径：规则 4 / 6 二阶后果 + 第三态 `unverified` + 零配置回归 | `workspace/agent_test.go` 追加（4 条，含夹具改造）；`cli/profile_test.go` 追加（2 条） |

### Key Entities

- **MCP server 配置**：名称（唯一）、传输方式（v1 只 stdio）、启动命令（空白切分的字面量）、环境声明（键 → `${VAR}` 占位符）。
- **连接结果**：server 名 + 该 server 的工具列表；失败时无结果、只有 WARN。
- **MCP 工具**：名称（原样）、描述、输入 schema（原样）、执行语义（经连接会话调用）。
- **工具调用入参**：JSON 对象原始字节（空 / `null` 归一为空对象）。
- **工具调用结果**：内容块序列（文本 / 非文本）或结构化内容；渲染为单一文本。
- **Agent 准入门禁输入**：已配置 server 名字集（文件序）、已连接 server 名字集（重路径）或 `nil`（轻路径）、注册池名字集；输出 = 已注册 / 拒载（含原因）/ 待确认。
- **启动摘要**：`agents` / `tools` / `mcp_connected` 三个可机器读的键。

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 零代码接入：给定一份合法配置与声明齐全的 Agent，启动成功率 = **100%**；MCP 工具出现在工具清单且描述 / schema 与 server 声明**逐字一致率 100%**；一次成功调用产生审计行 = **1 行**、`status=succeeded` = **100%**；启动摘要 `mcp_connected` 与实际连接数一致率 = **100%**。
- **SC-002**: 故障隔离：单 server 失败时启动中断 = **0** 例；失败 WARN 点名 server = **100%**；未受影响 server 的工具可用率 = **100%**；依赖失败 server 的 Agent 被误注册 = **0** 例；两条准入规则报错顺序错误（先 6 后 4）= **0** 例。
- **SC-003**: 配置与凭证：非法配置被静默接受 = **0** 例；拒绝启动点名率 = **100%**；错误 / 日志回显配置值 = **0** 例；未设置或空值变量被静默接受 = **0** 例；子进程继承白名单外变量 = **0** 个；shell 解释参与命令执行 = **0** 例。
- **SC-004**: 同形与审计：循环 / 执行器按来源分支 = **0** 处；MCP 调用缺 `pending` 行 = **0** 例；适配器自落审计 / 自写消息 = **0** 处；`IsError` 被当成功 = **0** 例。
- **SC-005**: 敏感面：server 文本 / 原始错误出现在日志、审计、模型上下文 = **0** 例（三档负向：连接失败、协议错误、`IsError`）；可观测正文与固定文案不一致 = **0** 例。
- **SC-006**: 生命周期与工程纪律：握手预算释放后首次调用失败 = **0** 例（集成层）；`Close` 两次报错 = **0** 例；停机顺序错误 = **0** 例；配置键新增 = **0**；表结构与迁移改动 = **0**；样例配置 diff = **0**；第三方坐标新增 ≠ 1 = 失败；抑制静态检查的注释 = **0** 处；默认测试层真实子进程 = **0** 例（集成层除外）。
- **SC-007**: 两路径一致：零 MCP 配置时轻路径与 US-6 逐字节差异 = **0** 例；可能依赖 MCP 的 Agent 被误报为拒载 = **0** 例；`unverified` 条目进入运行面 = **0** 例。

## Assumptions

- 硬依赖 US-4 已交付物：工具接口与结果形态、工具执行器（入参校验 + 两阶段审计 + 失败分类判序）、注册池（拒绝重复并保留原注册）、加载期八条校验与规则 4 / 6 骨架、外部错误与入参错误类型、取消判序函数。本模块**不消费**其他未交付能力（管理台 MCP 页归 US-12、技能加载归 US-11、`notify` / `manage_schedule` 归 US-8 / US-10）。
- 依赖 US-1 已交付物：工作区根句柄（符号链接越界在根上被挡）、`basalt init` 的 `mcp_servers.yaml` 模板（本 US **只改注释**，`{}` 字节不动）；依赖 US-3 已交付的 Agent 装载器与 `mcp_servers` 字段；依赖 US-5 已交付的外部错误 / 入参错误与 HTTP 工具的取消判序函数（同包复用）；依赖 US-6 已交付的装配接线（内置工具 8 件）。
- 外部依赖：`github.com/modelcontextprotocol/go-sdk v1.8.0`（技术方案 §1.2 栈表已批准；2026-10-17 已在模块代理核实为最新 GA）。
- SDK 行为假设（R11 钉住）：v1.8.0 的命令传输在连接时**忽略上下文**，进程生命周期由进程句柄持有；若未来版本开始把上下文取消传播到传输，R11 立即红——那是升级的前置判据（需求 §12.1 的缓解措施）。
- 形态裁决：`mcp_servers.yaml` 的「顶层映射 + `servers:` 列表」形态以 Compatibility §2.1 为准（工作区文件内容零改动即可迁移）；US-1~US-6 的临时解析器（顶层键名集）本 US **替换而非叠加**，同一文件只留一份解析器。
- 信任边界：MCP server 是管理员登记并评审过的独立进程；其内部文件 / 网络行为不经 Policy 是**有意接受的边界**（技术方案 §6.5），不是本模块缺口；v1 不承诺对不可信 server 的隔离。
- 平台：默认测试层不启动真实子进程（进程内测试对）；集成层真实子进程在测试二进制路径含空白时**显式跳过并写明理由**（`command` 无引号语义的已知边界）。
- 前序改造点（逐条属**软门禁**，开工前报告）：见颗粒度文档 §3.5 与 `plan.md`；其中「模板注释修正」「临时解析器替换」「装载器注入新参数」触及前序交付面，但均为本 US 已登记的设计选择，非计划外变更。
- 契约状态：本模块触及的运行时契约（`agent-md.md` 规则 4 / 6 文案、`run-and-errors.md` 七值分类）在对应链路验证通过前为「待验证设计草案」——不构成本模块开工阻断（与既有安排一致）。

### Out of Scope（本 feature 明确不做）

- SSE / Streamable HTTP transport（扩展阶段；v1 只 stdio）。
- 对不可信 MCP server 的容器化隔离（扩展阶段；技术方案 §6.5 有意接受的边界）。
- MCP server 内部行为的工具级 Policy（不是遗漏，是有意接受的边界）。
- MCP resources / prompts / sampling（扩展阶段；v1 只用 tools 一面）。
- MCP 工具调用审核 / 内容过滤（企业级治理归扩展阶段）。
- 断线重连 / 健康检查 / 连接池重建（扩展阶段；v1 失败即跳过、重启才重连）。
- `mcp_servers.yaml` 热加载（需重启；技术方案 §11.5 / Compatibility §2.4）。
- 管理台 MCP 观测页与「测试连接」（US-12）。
- 工具名命名空间 `server.tool`（扩展阶段；v1 原样名 + 先到先得）。
- server 级启停开关 / 优先级配置（扩展阶段；v1 以文件顺序为唯一优先级）。
- 工具级超时覆盖（零新增配置键；单工具超时沿用既有预算）。
- 非文本内容的二进制落盘 / 附件引用（扩展阶段；v1 只渲染类型化占位符）。

## 人工验收项（harness 之外）

以下人工项在模块收口报告里单独列出（harness 已判卷之外，执行人 = 用户；逐条对应颗粒度文档 §6.1）：

1. **门禁七目标与覆盖率读数**：`make check` 七个目标逐条读数（`fmt` / `lint` / `sec` / `test` / `test-integration` / `cover` / `build`）；`tool` / `core` 覆盖率实测数值写入 DoD。
2. **Demo 三（零代码接入）**：真模型 + 一个现成 MCP server——只写 `AGENT.md` 与 `mcp_servers.yaml`，全程不改代码；让 Agent 完成一次 PR digest（或选定的真实小任务），核对：工具出现在 `basalt tool list`、模型确实调用了它、`tool_invocations` 有对应行（`status=succeeded`）、启动摘要 `mcp_connected = 1`。
3. **负向 Demo（故意写错 `command`）**：启动不中断；日志出现「MCP server 未连接——已跳过」（点名 server）；依赖它的 Agent 出现 `Agent 未注册`（`tool not registered` 或 `mcp server skipped`）；其余 Agent 正常可聊。
4. **两档展示面的一致性抽验**：重路径（`chat`）与轻路径（`profile list`）对同一份工作区——零 MCP 配置时读数不变（US-3 基线）；有 MCP 配置且未验证时 `profile list` 显示 `unverified`。
5. **`tool list` 冒烟**：八件内置 + N 件 MCP 工具；MCP 工具的描述与参数 JSON 与 server 声明一致（抽一件比对）。
6. **`env` 占位与明文负向**：写明文 token → 启动拒绝，错误与日志**不含该值**；写 `${MISSING_VAR}` → 拒绝启动，错误点名 server 与变量名。
7. **停机子进程检查**：`serve` / `chat` 退出后，用任务管理器 / `ps` 确认 MCP server 子进程**不残留**（Windows 任务管理器 + `tasklist` 双读数）；池关闭错误若有，日志与退出码如实反映。
8. **SDK 升级前置**：记录集成层（真实子进程）读数为基线，**含 R11 生命周期用例**；将来升级 `go-sdk` 先跑该测试。
9. **Windows 真控制台**：中文工具描述 / 错误文案的渲染（无 BOM、多行不交错）；`command` 含中文路径时按空白切分的实际行为如实记录（已知边界）。
10. **凭证与敏感面复核**：日志与错误不打印 `env` 值、不打印 server stderr 全文、不打印工具结果正文、不打印 `IsError` 的 server 文本与协议错误原文；抽查 `tool_invocations.error_message` 与回填模型的 content，确认与固定文案一致；`mcp_servers.yaml` 在仓库外或已脱敏。
11. **变更范围核对**：`git status --short` 与交付物清单逐项对齐；`schema.sql` / `basalt.yaml.example` / `Makefile` 零 diff；`go.mod` 只多 `go-sdk` 一条直接依赖。
12. **四目标编译矩阵**：linux/amd64、linux/arm64、windows/amd64、darwin/arm64 逐目标 `CGO_ENABLED=0 go build`（不只 `make build` 宿主档）。
13. **并发抽验**：并发触发多个含 MCP 工具的会话 → `-race` 无告警；`Close` 与在途调用交错时不 panic。
