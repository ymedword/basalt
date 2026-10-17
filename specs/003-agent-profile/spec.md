# Feature Specification: US-3 Agent 目录与 Profile 加载

**Feature Branch**: `003-agent-profile`

**Created**: 2026-10-16

**Status**: Draft

**Input**: User description: 颗粒度文档 `docs/us/us3-agent-profile.md`（第 1、2 节）——把 `AGENT.md` 契约变成**可装载的 Agent 定义**：`.basalt/agents/<name>/AGENT.md` 经 frontmatter 解析与加载期校验派生成 Profile（任一规则不过**整只不注册** + 点名 WARN）；system prompt **第一段**（AGENT.md 正文 + Bootstrap 文件 + 技能卡片 L1 + 当前日期时间）可组装；`profile list / show / create / delete` 四条子命令可用（`create` / `delete` 为写模式命令，取工作区实例锁）。

> **事实源与裁决**：本规格从 US-3 颗粒度文档 `docs/us/us3-agent-profile.md` 派生，供 speckit 链（plan / tasks / analyze）消费；冲突时以四份权威文档的裁决顺位为准——实现设计以 `docs/TechnicalSolution.md` 为准、原则以 `.specify/memory/constitution.md` 为准、固定字面量以 `docs/contracts/` 为准。内部类型名、包落位与测试文件组织归 plan 与该文档 §3，不在本规格展开。可自动化验收由该文档「验收测试（Harness）」§4 承载（默认层全量；本 feature **无集成层**——不触外部依赖）；人工项见其 §6。**设计面已冻结**（该文档「待决事项」为空——R1 评审回合七项裁决全部落案：实例锁最小面前移、调度 id 占用按整只 Agent 提交、`create` 模板口径、`delete` 归档语义、缺 / 无效 `basalt.yaml` 边界、契约字面量缺口三项、文字修正两处），实现期若冒出新的未定口径，**必须**回该文档登记后再动手，不静默自决。
>
> **附加事实源（条件触发，本 feature 已触发）**：`docs/Compatibility.md` §5.1 共享验收样例（样例 `AGENT.md` + 六条判据）逐条件作**验收判据**；§5.2 cron 公共子集与固定起点对拍口径；§2.1 命令入口行、§3 #6（写模式命令面承诺）。

## Clarifications

### Session 2026-10-16

- Q: 同一技能在 `skills:` 里重复声明时，卡片渲染与注册判定按什么口径？ → A: 无独立权威字面量——按颗粒度文档 §2.3 流程 B 的伪代码机械读出：**逐条渲染**（每个声明一条卡片、不去重，`cardsFor(base, p.Skills)` 无去重步骤）；重复声明 MUST NOT 改变注册判定（名字在库中有实体即通过）、MUST NOT 产生重复卡片以外的副作用。
- Q: `profile show` 对未注册的 Agent 展示什么？ → A: 未注册但 frontmatter 解析成功的 Agent MUST 可 show，展示声明面（provider / 模型）与原因；解析失败者只展示原因（声明面不可得）——颗粒度文档 §2.4 注 3 + §4.2 A7。
- Q: `profile create` 模板里 `skills` / `schedules` 等其余列表字段写什么？ → A: 为空或省略（纯列表字段下未声明与显式空列表等价）；判据是**模板可被加载**——颗粒度文档 §3.1 `CreateAgent` 注释与 §6 固定口径 3（`tools: []` 同源理由）。

## User Scenarios & Testing *(mandatory)*

**标签口径（`[US1]` / `[US2]` / `[US3]` 不混淆）**：下面三个用户故事在 `tasks.md` 中以 Spec-Kit 的工具标签 `[US1]` / `[US2]` / `[US3]` 出现——那是**本 feature 内部**用户故事的编号，**不是**项目台账（[docs/us/REGISTRY.md](../../docs/us/REGISTRY.md)）里的 US-1 / US-2 / US-3。本 feature 的三个故事同属项目 **US-3（Agent 目录与 Profile 加载）** 一个模块，是它的三个独立可验场景；标签按工具产出保留。

### User Story 1 - 一个目录 = 一个 Agent：加载与校验（Priority: P1）

运维把一个 Agent 目录放进工作区的 `agents/` 下（`AGENT.md` 定义入口 + 附属物），执行 `profile list`：该 Agent 显示 `registered=true` 并带它声明的 provider 与模型；`profile show <name>` 看到同一声明面。若该 Agent 的声明坏了任何一条（目录名与 `name` 不一致 / provider 未注册 / 模型空白 / 技能库里没有声明的技能 / MCP server 不在配置内 / Bootstrap 文件缺失 / `schedules` 不合法 / 温度越界），**整只**显示 `registered=false` 并点名原因（日志同时落一条点名 WARN），**其余 Agent 照常装载**——坏一只不等于整批不可用。目录里的附属物（脚本、参考文档）不构成第二定义入口；本地 `skills/` 目录与软连接类形态**不被识别**（被无视，绑定结果不变）。

**Why this priority**: 这是"一个目录 = 一个 Agent"从产品命题变成可执行判卷的第一次落地——没有它，`AGENT.md` 契约只是文档；它同时是后续所有涉及 Agent 的 US（循环 / 工具 / 记忆 / 调度 / 管理台）的装载入口。

**Independent Test**: 在临时工作区写各形态 Agent 目录（合规范例 / 八类坏例各一 / 一只坏一只好），调用加载面断言每只的注册结果、点名原因与日志三键（`agent` / `reason` / `detail`），全程不触网络、不开数据库。

**Acceptance Scenarios**:

1. **Given** 工作区 `agents/` 下有一份完全合规的 Agent 目录（含 provider 已注册、技能库有实体、Bootstrap 文件齐备、`schedules` 合法），**When** 执行 `profile list`，**Then** 该 Agent `registered=true`、provider 与模型如实展示、汇总行计入；`profile show <name>` 展示同一声明面。
2. **Given** 同一批里有若干坏例（未注册 provider / 技能库无实体 / 模型空白 / 目录名与 `name` 不一致 / Bootstrap 缺失 / MCP server 未配置 / `schedules` 不合法 / 温度越界各一），**When** 执行加载，**Then** 逐只 `registered=false` + 点名原因；合规的那些照常 `registered=true`（单只失败不拖垮整批）。
3. **Given** 共享验收样例（Compatibility §5.1）原样放入 `agents/compat-check/`，**When** 加载，**Then** 注册成功、`tools` 的 10 个名字逐字解析进声明面、技能声明即绑定、`schedules` 派生命中（本仓侧判据 1 / 2 / 3；判据 6 的前提由人工项覆盖）。
4. **Given** 共享样例的 `cron` 被改成 5 段，**When** 加载，**Then** 该 Agent 不注册，原因文案**必须**含固定句 `Cron expression must consist of 6 fields`（Compatibility §5.1 判据 4）。
5. **Given** 把 `zone` 从 `schedules` 里删掉，**When** 加载并按固定起点计算触发时间，**Then** 按 **UTC** 解释（不是本机时区）（判据 5）。
6. **Given** 某 Agent 目录里另有 `scripts/` 附属物或本地 `skills/` 目录，**When** 加载，**Then** 它们不构成定义入口、不改变绑定结果（本地 `skills/` 被无视并记一条 WARN）。

---

### User Story 2 - 技能声明式绑定与提示词第一段（Priority: P2）

开发 / 模型侧看到的效果：Agent 的 `skills:` 声明即绑定——声明了哪个技能，系统提示词第一段里就出现哪张技能卡片（技能名 + 描述 + 技能库里的真路径三要素），**没有声明的技能卡片不出现**；技能库里的技能**正文不被预载**（模型要读时再按路径取）。第一段的组成与顺序固定：先（若配置了身份提示词）身份提示词，再自我称谓，然后是 `AGENT.md` 正文、按声明顺序的 Bootstrap 文件内容、技能卡片，最后以当前日期时间收尾（含时区偏移，缺省 UTC）。任一条素材在装载后消失或不可读 → **报错点名路径**，**不得**静默少一节（静默省略会让"注入了什么"与"记了什么"悄悄分叉）。

**Why this priority**: 它是"技能声明式绑定 + 渐进披露"这条原则的可见面，也是循环上线前提示词的半边；错在这里的表现是"技能卡片不出现而其它一切正常"——最难从现象反推的一类。

**Independent Test**: 用固定素材（正文 + 三件 Bootstrap + 技能库两卡，其中一张被声明）调用第一段拼装，按位置逐对断言次序与收尾格式；再删掉其中一份素材重跑，断言返回错误而非缺节；两次调用之间改文件内容，断言重读生效（无缓存）。

**Acceptance Scenarios**:

1. **Given** 一只已注册 Agent 声明了两个技能、技能库都有实体，**When** 组装第一段，**Then** 四要素（自我称谓 / 正文 / Bootstrap / 技能卡片）按固定顺序出现、卡片三要素齐备且路径为技能库真路径、末尾是当前日期时间且**含时区偏移**。
2. **Given** 同一 Agent 未声明技能库里的第三张卡，**When** 组装第一段，**Then** 该卡片**不出现**（声明即绑定）。
3. **Given** 装载之后 Bootstrap 文件（或 `SKILL.md` 头、或正文）被删除 / 不可读，**When** 组装第一段，**Then** 返回错误并点名路径，**不得**静默省略该节。
4. **Given** 两次组装之间修改了 Bootstrap 文件内容，**When** 再次组装，**Then** 注入内容反映最新文件（无缓存、不做热加载）。
5. **Given** 一只 `registered=false` 的 Agent，**When** 请求组装第一段，**Then** 点名拒绝（未注册的 Agent 不得组装系统提示词）。

---

### User Story 3 - 写模式命令：建模板、归档与实例锁准入（Priority: P3）

开发执行 `profile create demo-agent`：命令**先取工作区实例锁**（非阻塞），拿到锁才动手——生成一份可装载的模板并把本次创建的文件逐条列出，输出**明示选中了哪个 provider**（配置数组首项；无配置时写占位文本并说明"模板尚未注册"）。已存在同名目录 → 拒绝且**不覆盖**；Agent 名非法 → 拒绝；创建中途失败 → 清理本次残留，不留半截目录。`profile delete <name>`：取锁后把该 Agent 目录**单次原子移动**进归档目录（名字带时间戳），会话 / 记忆 / 审计数据一律不动；归档撞名 → 拒绝且**源目录保留**；缺目录 → 点名拒绝。若工作区已被一个活实例（服务进程）持锁，两条写模式命令**拒绝启动**并给可读文案（点名占用者的进程号与开始时间，指引改用 REST / 管理台）；而 `profile list` / `show` 等只读命令**照常可用**——它们不取锁、不清理锁文件。

**Why this priority**: 它是"写模式命令准入"的最小可判卷面（宪法级服务单实例承诺的第一块实现），也是唯一会**改工作区**的命令面——做错的代价（覆盖用户目录、半截残留、双写）比前两个故事高；但它可被前两个故事独立验证，故排 P3。

**Independent Test**: 在临时工作区跑 `create` / `delete` 的正反例（合规 / 已存在 / 非法名 / 缺目录 / 归档撞名），断言文件系统终态与输出；实例锁以**跨进程**用例判卷（helper-process：父进程持锁时子进程取锁被拒且零写入、持锁进程被强杀后子进程可重取、释放幂等、锁文件留在磁盘）。

**Acceptance Scenarios**:

1. **Given** 已初始化工作区、配置里有两家 provider，**When** 执行 `profile create demo-agent`，**Then** 生成可被加载的模板（`tools` 为空列表、`provider.name` 为配置首项）、输出明示选中的 provider、逐条列出创建的文件；随后 `profile list` 可见该 Agent。
2. **Given** 同名目录已存在，**When** 再次 `create`，**Then** 拒绝且**不覆盖**（既有文件逐字节不变）。
3. **Given** 无 `basalt.yaml` 或 provider 段为空，**When** 执行 `create`，**Then** 仍生成模板（`provider.name` 写占位文本），输出**明示**该模板补齐配置前不会注册。
4. **Given** 一个已注册 Agent，**When** 执行 `profile delete <name>`，**Then** 目录被**单次移动**进归档位置（名字含时间戳），源位置不再存在，会话 / 记忆 / 审计数据不动。
5. **Given** 同一秒内对同一 Agent 二次 `delete`（归档目标已存在），**When** 执行，**Then** 拒绝并点名目标路径，**源目录保留原位**。
6. **Given** 另一个进程持有实例锁（真进程，非模拟），**When** 执行 `profile create` / `profile delete`，**Then** 命令拒绝启动、文案点名占用者进程号与开始时间、工作区**零写入**；同一时刻 `profile list` 照常成功。
7. **Given** 持锁进程被强杀（无清理机会），**When** 再次执行写模式命令，**Then** 取锁成功（内核随进程退出释放）——**不得**依赖心跳过期或"锁文件是否存在"判据。

### Edge Cases

- `agents/` 目录整体缺失 → 装载**报错**（不是"零只 Agent"）；`agents/` 存在但为空 → 成功并说明"未发现 Agent 目录"。
- 工作区未初始化（`.basalt/` 缺失）→ 命令失败并点名路径 + 指引 `basalt init`（只读命令同样失败，不吞）。
- 配置文件缺失 → 输出明示"未发现配置——provider 注册判定按未注册处理"，**不**计失败（与 `provider list` 的「点名型失败」刻意不同）；配置文件存在但无效 → 原错误上抛（点名键路径与变量名、不含凭证值）。
- `AGENT.md` 不存在的子目录 / 非目录条目 → 该条目如实按未注册处理并点名，不使整批失败。
- frontmatter 出现字段表之外的键 → **忽略该键但每条一条 WARN**（`detail` 只记键路径、**不打印值**），继续装载（前向兼容）。
- frontmatter 是畸形 YAML → 该只不注册；文案压缩为「位置 + 自产类别」，**不回显 YAML 源码片段**、不外传解析库原文。
- `provider.temperature` 显式为 `0` 与未声明是两件事（前者生效为 0，后者用供应商缺省）；`settings` 显式 `0` 与未声明同理（后者取文档缺省 10 / 20）；`channels` 显式空列表与未声明同理（后者取 CLI 渠道）。
- 调度 id 撞车：`id` 恰一个任务时可缺省（派生 `<agent>-1`），两个及以上必须逐项显式；全局唯一先到先得，且**只有完整校验通过的 Agent 才占用**——被拒 Agent 不得占用任何 id。
- `zone` 写 `Local` → 显式拒绝（本地时区会随部署漂移）；`zone` 写不存在的 IANA 名 → 拒绝并附原因；`cron` 里出现 `L` / `W` / `#`、周字段写 `7`、日与周**同时**被限定 → 逐例拒绝并给出统一类别与细节。
- 并发发起两条写模式命令（跨进程）→ 恰一条取锁成功、另一条拒绝且零写入；锁文件"存在但没人持锁"不是占用（**不得**用存在性判据）。
- 拦截点：拒绝启动的写模式命令不产生任何工作区副作用（不建目录、不写文件、不改锁文件内容）。

## Requirements *(mandatory)*

### Functional Requirements

#### 加载单元与加载期校验

- **FR-001**: 加载单元 MUST 是工作区 `agents/` 下的**每个子目录**；`AGENT.md` MUST 是唯一定义入口（附属物如脚本目录、参考文档 MUST NOT 构成第二入口）；目录名即 Agent 名，且 MUST 与 frontmatter 的 `name` 逐字一致（不一致 → 该只不注册并点名）。非目录条目 MUST 如实忽略。
- **FR-002**: 加载期 MUST 执行契约的八条校验：①目录名与 `name` 一致；②`provider.name` 在**已注册 provider 名集**内（名集由调用方显式注入，加载面不自己再读一次配置）；③模型非空白（占位文本天然放行）；④`tools` 全部可解析——**本 feature 只解析进声明面、不判存在**（占位登记，随工具注册表 US 接入）；⑤`skills` 在技能库有实体；⑥`mcp_servers` 在 `mcp_servers.yaml` 顶层键内（只判存在性，"连接成功"半条随 MCP US）；⑦`bootstrap` 声明的文件存在；⑧`schedules` 段规则全量校验。
- **FR-003**: 除八条外，`provider.temperature` **已声明**时 MUST 落在 0–2 且为有限值（越界 / NaN / 正负无穷 → 该只不注册，点名 `provider.temperature`）。该约束在**装载期**执行（不推迟到调用期）。
- **FR-004**: 八条中任一不过（含 FR-003）⇒ 该 Agent **整只不注册**——MUST NOT "跳过那一条继续"；MUST 记一条点名 WARN；**其余 Agent 照常装载**（单只失败不拖垮整批）。未注册的 Agent MUST NOT 组装系统提示词。
- **FR-005**: frontmatter 到声明面的字段映射 MUST 显式（`snake_case` 键名逐字对齐契约字段表）。缺省填充 MUST 只补"**未声明**"：`temperature` 未声明 vs 显式 `0` 可区分；`settings` 未声明 → 10 / 20、显式 `0` 保留；`channels` 未声明 → CLI 渠道、显式空列表 → 空；纯列表字段（`tools` / `skills` / `mcp_servers` / `bootstrap` / `schedules`）未声明与显式空列表等价。MUST NOT 用"值为零就填缺省"的写法（那正是丢失显式零值的形态）。
- **FR-006**: frontmatter 出现字段表之外的键 ⇒ **宽容但可见**：忽略该键、逐条记 WARN（`detail` 只记键路径、**不打印值**），继续装载；键路径集 MUST 先排序再逐条输出（不得依赖映射遍历顺序）。同一份文件字节 MUST 只读一次（未知键扫描与字段解码共用同一份内容）。

#### 技能声明式绑定与渐进披露

- **FR-007**: 技能绑定 MUST 只由 frontmatter `skills:` 声明（**声明即绑定**，声明名在技能库中无实体 → 该 Agent 整只不注册）。MUST NOT 识别 Agent 本地 `skills/` 目录或软连接类形态——本地 `skills/` 被**无视**并记一条 WARN，绑定结果不变；MUST NOT 新增 `use_skill` 类工具。
- **FR-008**: 第一段只注入 **L1 卡片**（技能名 + 描述 + **技能库真路径**）；MUST NOT 预载 `SKILL.md` 正文（L2 由后续 US 的文件工具按需取）。真路径字面量 MUST 为 `.basalt/skills/<name>/SKILL.md` 形态，供后续工具的相对路径解析吃下。
- **FR-009**: `skills` 的实体查找 MUST 经工作区根约束进行；同一技能重复声明 MUST NOT 改变注册判定（名字在库中有实体即通过），渲染按**声明逐条**（重复声明产生重复卡片）且 MUST NOT 产生重复卡片以外的副作用（口径见 Clarifications 会话 2026-10-16）。

#### 调度段（`schedules`）加载期规则

- **FR-010**: `cron` MUST 为 **6 段**（秒 分 时 日 月 周）；5 段及以下 → 拒绝，且拒绝文案 MUST 含固定句 `Cron expression must consist of 6 fields`（跨底座判据）。
- **FR-011**: `cron` 取值 MUST 落在**公共子集**内：通配、数字、范围、列表、步长、月 / 周缩写（大小写不敏感）；`L` / `W` / `#` 与周字段 `7` MUST 被拒；**日字段与周字段 MUST NOT 同时被限定**（至少一个是裸通配——`*/2`、`1-15`、`1,5`、单值都算限定）。拒绝文案 MUST 给出本产品统一类别与细节（不直接把第三方库错误当用户文案）。
- **FR-012**: `message` MUST 必填（空白 → 拒绝）。`zone` 缺省 MUST 为 **UTC**；`Local` MUST 显式拒绝（本地时区随部署漂移）；其余取值 MUST 是经加载校验的 IANA 名，MUST NOT 回落进程本地时区。
- **FR-013**: 任务 `id`：恰一个任务时缺省 MUST 派生 `<agent>-1`；两个及以上 MUST 每项显式（缺失 → 拒绝）；全局唯一先到先得，且占用 MUST 按**整只 Agent** 提交——被拒 Agent MUST NOT 占用任何 id（无部分占用）。`id` 的判定序与细节字面量以颗粒度文档「本文固定口径」第 5 条为准。
- **FR-014**: 规范化 MUST 交付：把每任务 `zone` 转为 `CRON_TZ=<zone>` 前缀（装载层交付、校验与后续触达时间计算共用同一份来源）。**调度注册与执行**（把表达式交给调度器并触发）MUST NOT 在本 feature 内实现。

#### 系统提示词第一段

- **FR-015**: 第一段组成与顺序 MUST 为：`identity.prompt`（若配置）→ 自我称谓（`identity.agent_name`，未声明取 Agent 名）→ `AGENT.md` 正文 → Bootstrap 文件（**按声明顺序**，路径相对工作区根）→ 技能卡片 L1 → 末尾当前日期时间。日期时间格式 MUST 为 `2006-01-02 15:04:05 -07:00`（缺省 UTC、**输出含时区偏移**）。
- **FR-016**: 正文 / Bootstrap 文件 / `SKILL.md` 头任一条在装载后消失或不可读 ⇒ 组装 MUST 返回**错误**并点名路径；MUST NOT 静默省略该节（静默省略使"注入内容"与"清单哈希"分叉——下游 US 明确不得对同一素材二次读盘）。
- **FR-017**: 第一段拼装 MUST 是无状态原语：每次调用**重读**文件、**不缓存**；当前时间 MUST 由调用方显式传入（MUST NOT 在函数内取真实当前时间）。本 feature MUST NOT 引入注册表 / 快照类型，也 MUST NOT 引入热加载。

#### 命令面

- **FR-018**: `profile list` / `profile show` MUST 走**轻路径**：只读文件系统，**不开数据库、不取实例锁、不做恢复、不改动锁文件**。注册判定 MUST 与加载面是**同一份**解析与校验代码（MUST NOT 为命令面另立解析路径、MUST NOT 写死假数据）；`show` MUST 复用同一结果集过滤。
- **FR-019**: `profile list` 的边界行为 MUST 为：工作区未初始化 → 失败并点名路径 + 指引 `basalt init`；配置缺失 → 明示"按未注册处理"且**不计失败**；配置存在但无效 → 原错误上抛（点名键路径与变量名、不含凭证值）；目录清单为空 → 成功 + "未发现 Agent 目录"；正常 → 逐条 `registered` 状态 + 原因 + 汇总行，退出码 0。`profile show <name>` 的 name 不在目录清单 → 失败并点名；未注册的 Agent MUST 也可 show——解析成功者展示声明面（provider / 模型）与原因，解析失败者只展示原因（声明面不可得）。
- **FR-020**: `profile create` MUST：取实例锁后才动手；生成可被加载的模板——`provider.name` 取配置数组**首项**并**输出明示选中了谁**（无配置 → 写占位文本并说明补齐配置前不注册）、`tools` 写**空列表**（记忆工具池未就绪，现在写会让模板在后续 US 窗口期拒载）、其余列表字段（`skills` / `schedules` / `mcp_servers` / `bootstrap`）MUST 为空或省略（非空声明在全新工作区必然拒载，违背**模板可被加载**的约束）；遇已存在目录 → 拒绝且**不覆盖**；Agent 名非法 → 拒绝（与 `delete` 共用同一名称校验判据）；模板内的名字 MUST 经 YAML 编码而非字符串插值；创建中途失败 MUST 清理本次残留，不留半截目录。
- **FR-021**: `profile delete` MUST：取实例锁；语义为**归档**——**单次原子移动**进 `archive/<name>-<时间戳>/`（MUST NOT 复制 + 删除、MUST NOT 先删源后建目标）；归档撞名 → 拒绝并点名目标路径且**源目录保留**（不覆盖空目录也不能靠移动语义兜底）；缺目录 → 点名拒绝；会话 / 记忆 / 审计数据 MUST NOT 被动；时间戳 MUST 由调用方显式传入（CLI 入口取真实当前时间）。退出码沿用成功 0 / 失败 1（原因由文案区分）。

#### 写模式与实例锁

- **FR-022**: `profile create` / `profile delete` MUST 是**写模式命令**：取工作区实例锁（**非阻塞**）；被活实例持有时 MUST **拒绝启动**并给可读文案（点名诊断信息中的占用者进程号与开始时间，指引改用 REST / 管理台）；拒绝路径 MUST NOT 产生任何工作区副作用。只读命令 MUST NOT 取锁、MUST NOT 清理或删除锁文件。
- **FR-023**: 锁判据 MUST 是 **OS 级独占文件锁**（进程退出由内核释放）；MUST NOT 用"文件是否存在"判据、MUST NOT 凭心跳过期抢锁（心跳类字段**只作诊断**）。锁文件 MUST 允许留在磁盘不清理。
- **FR-024**: 锁生命周期 MUST 为：取锁前**不截断**锁文件；只锁固定的一小段区域；诊断记录（进程号、开始时间等）写在**锁定区之外的固定偏移**（使竞争方能读到是谁占着——排他锁会挡住锁定区本身的读）；诊断写失败 ⇒ **先释放句柄**再返回错误；释放 MUST **幂等**（重复调用为 no-op 返回成功），只解锁 + 关闭句柄，MUST NOT 删除锁文件。
- **FR-025**: 锁行为 MUST 可**跨进程**判卷：父进程持锁时子进程取锁被拒且**零写入**；持锁进程被强杀后子进程可立即重取（内核随进程退出释放）；同一进程内重复取锁的补充用例 MAY 存在但 MUST NOT 替代跨进程判据。

#### 工程纪律

- **FR-026**: 所有文件读写（**含目录扫描**）MUST 经工作区根约束进行——软连接越界在根上被挡；扫描顺序 MUST 显式确定（按名字节序）——"先到先得"以扫描序确定为前提，MUST NOT 依赖库的隐式顺序；条目是否为目录 MUST 按"不跟随软连接"的口径判定；新建目录 / 文件权限沿用既有口径；MUST NOT 覆盖已有文件。
- **FR-027**: 依赖方向 MUST 遵守：声明层（类型 / 常量 / 哨兵）只依赖标准库且**零可执行语句**；实现落实现包；命令面 MUST NOT 直接依赖实现包（端口定义在消费方、适配器在唯一装配层）；无环由 CI 复核。
- **FR-028**: 可观测性 MUST 双轨：加载失败每只一条 **WARN**，固定键 `agent` / `reason` / `detail`（`reason` 取契约字面量原样、点名细节进 `detail`）；未知键 WARN 的 `detail` 只放键路径；工作区写入失败（create / delete）记 **ERROR**。日志 / 错误 / 命令输出 MUST NOT 拼文件全文、MUST NOT 回显 YAML 原始片段或 YAML 解析库原文（压缩为「位置 + 自产类别」）。cron 段的语法错误是**唯一例外面**：`reason` 恒为本产品统一类别 `invalid schedule`，第三方库原文只进 `detail`（FR-011 + 颗粒度文档 §6 固定口径 5）。
- **FR-029**: 业务状态（Agent 声明面、provider 名集、当前时间）MUST 一律**显式类型化参数**传递；`context.WithValue` MUST NOT 出现在业务代码里（跟踪标识除外，本 feature 零使用）。
- **FR-030**: 本 feature MUST NOT 有网络访问、MUST NOT 有数据库写入（Agent 注册结果走"日志 + 命令输出"两轨）；默认测试层 MUST 无一例访问真实网络；跨进程判据 MUST 经子进程 stdout 行同步，MUST NOT 用固定睡眠赌时序。
- **FR-031**: 新增第三方坐标 MUST 唯一且已在固定技术栈句与已批准计划内（cron 表达式库，版本经模块代理核实）；其余坐标不动（含一处由间接依赖迁为直接依赖、版本不变的迁移）。

### 自动化验收锚点（关键回归守卫）

自动化验收由颗粒度文档「验收测试（Harness）」§4 的测试套件承载（测试全绿即通过；**本 feature 无集成层**——不触外部依赖）。本 feature 的关键回归守卫（实现期 MUST 原样落地断言逻辑，函数名按工程规范译为英文、文档原文以注释保留）：

| 守卫点 | 用例（颗粒度文档 §4.3） |
| --- | --- |
| 共享验收样例加载：注册成功、10 个工具名逐字解析、技能绑定、调度派生命中 | `TestAgentLoader_LoadsCompatSampleAgent` |
| 判据 3：从**固定起点** 2026-10-01T00:00:00Z 起未来 10 次触发逐条相同（禁止当前时间） | `TestAgentLoader_LoadsCompatSampleAgent`（十次触发断言在该用例内——§4.3） |
| 判据 4：5 段 cron 拒载且文案含固定句 | `TestAgentLoader_RejectsFiveFieldCron` |
| 判据 5：缺省 zone 为 UTC（不是本机时区） | `TestAgentLoader_DefaultZoneIsUTC` |
| 调度 id 占用按**整只 Agent** 提交：被拒 Agent 不占用任何 id | `TestAgentLoader_RejectedAgentClaimsNoScheduleIDs` |
| 段一四要素**位置**断言 + 时间收尾 + 素材消失即报错 + 两次调用重读 | §4.3 段一组（`SectionOneCarriesAllFourParts` 家族） |
| 跨进程锁：互斥 / 强杀后重取 / 拒锁零写入 / 释放幂等 / 不删锁文件 | `TestInstanceLock_HelperProcess...` 家族（helper-process） |
| 归档撞名不覆盖且源目录保留 | `TestDeleteAgent_RefusesCollidingArchiveTarget` |
| 模板名字经 YAML 编码（不是字符串插值） | `TestCreateAgent_EncodesNamesThroughYAML` |
| 未知键 WARN 只记键路径、不打印值 | `TestAgentLoader_UnknownKeyWarnNamesPathWithoutValue` |
| 解析错误文案不回显源码片段 / 库原文 | `TestAgentLoader_ParseErrorOmitsSourceExcerpt` |
| `create` / `delete` 经写模式端口（接线判据） | `TestProfileCreate_CommandUsesWriteModePort` |

治理关键词的 grep 判据（`use_skill` 零命中、本地 `skills/` 被无视、`time.Local` 零命中、软连接越界被挡）逐条见颗粒度文档 §4.2，实现期由该层用例名承接。

### Key Entities

- **Agent 目录**：工作区 `agents/` 下的一个子目录——`AGENT.md`（唯一定义入口）+ 附属物；目录名即 Agent 名。
- **Agent 声明面（Profile）**：由 frontmatter 派生的运行配置——标识（名字 / 描述 / 身份提示词 / 自我称谓）、provider 引用与模型（含可选温度）、工具名列表、技能名列表（绑定）、MCP server 名列表、Bootstrap 文件列表、渠道列表、设置（最大轮次 / 历史轮数）、调度任务列表。
- **装载条目**：目录 + 声明面 + 注册结果（`registered`）+ 拒绝原因（人读合并形态）。声明面非空 ⇔ frontmatter 解析成功；`registered` ⇔ 全部校验通过（两段语义）。
- **调度任务**：`id`（可缺省派生）+ `cron`（6 段）+ `zone`（缺省 UTC）+ `message`；占用表"任务 id → 占用者"全局唯一。
- **技能卡片（L1）**：技能名 + 描述 + 技能库真路径（`.basalt/skills/<name>/SKILL.md` 形态）。
- **实例锁**：工作区级 OS 独占文件锁句柄 + 锁文件 + 锁定区外的诊断记录（进程号 / 开始时间）；释放幂等、锁文件不删除。

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 加载判卷零例外：八条校验 + 温度约束各至少一例坏例 100% 判为不注册并点名；合规 Agent 100% 注册成功；"一只坏一只好"批次的合规侧注册率 100%（单只失败对整批的影响 = 0）。
- **SC-002**: 契约判据全覆盖：Compatibility §5.1 的六条判据中，本仓侧可判的四条（1 / 2 / 4 / 5）100% 由自动化用例判卷；判据 3 的十次触发在**固定起点**下逐条可复现（两次运行读数相同率 100%）。
- **SC-003**: 调度面拒绝率 100%：5 段 / `L` / `W` / `#` / 周字段 `7` / 日周双限定 / `zone: Local` / 缺 `message` / 多任务缺 `id` 逐例拒绝；固定句在 5 段例中的出现率 100%；被拒 Agent 占用的调度 id 数 = **0**。
- **SC-004**: 第一段保真：四要素顺序断言 100% 通过；素材消失场景报错率 100%（静默省略 = 0）；两次调用之间的文件改动 100% 被重读（缓存命中 = 0）。
- **SC-005**: 命令面：`list` / `show` / `create` / `delete` 四命令在正例形态退出码 0、在六类边界（未初始化 / 缺配置 / 无效配置 / 已存在 / 缺目录 / 归档撞名）100% 判红并点名；输出凭证值命中 = 0。
- **SC-006**: 写模式准入：跨进程互斥场景中，被拒命令的工作区写入 = **0**；持锁进程强杀后重取成功率 100%；释放幂等（二次调用返回成功且无副作用）100%；锁文件在释放后仍存在 100%（删除 = 0）。
- **SC-007**: 环境独立性：默认测试层访问真实网络 = 0 例、数据库写入 = 0 次；固定睡眠同步 = 0 处（跨进程同步一律信号 / stdout 行）；测试产物 100% 落在临时目录。
- **SC-008**: 工程纪律：声明层可执行语句 = 0；业务代码 `context.WithValue` 命中 = 0；命令面直接依赖实现包命中 = 0（`go list` 判据）；`use_skill` 文字命中 = 0；`time.Local` 命中 = 0；覆盖率按可执行判据达标（实现包首次纳入判卷，无豁免）。

## Assumptions

- 硬依赖 US-1 已交付物：工作区初始化与检视、配置文件装载与 `${ENV_VAR}` 解析、命令面骨架与退出码口径、端口注入范式、结构化日志、`os.Root` 文件系统惯用法、时区内嵌（IANA 数据已随二进制内嵌——`Asia/Shanghai` 可解析的前提）、门禁七目标与测试基础设施（临时工作区）。
- 硬依赖 US-2 已交付物：已注册 provider 名集来自配置面（`providers` 段的重名 / 必填 / 地址成分三条装载期拒绝已就位）、声明层（类型 / 常量 / 哨兵）范式、命令组注册范式。
- 本 feature 新增第三方坐标**唯一一个**（cron 表达式解析库，版本已裁决并经模块代理核实；已在固定技术栈句与已批准计划内，直接落地、不重复询问）；另一处是把既有间接依赖迁为直接依赖（版本不变、无新增坐标）。
- 前序改造点：命令面新增 `profile` 命令组注册、装配层新增一个端口适配器（转调加载面）、声明层新增类型与哨兵——逐条为软门禁，开工前报告。
- 实例锁的**归属调整**已裁决：本 feature 落"写模式命令准入的最小面（锁本体 + 两条命令接线）"，重路径接线与崩溃恢复扫描归 US-4；已验收的历史工件（US-1 / US-2 颗粒度文档、起步手册）不回改，调整登记在 US-3 颗粒度文档 §2.4 注 1 / §5 末行。
- 平台分支：锁的 Windows 与类 Unix 实现是两份代码——Ubuntu CI 只执行后者，前者在开发机（Windows 11）执行并登记读数，CI 只保证其**可编译**；不得笼统宣称"既有门禁全覆盖"。
- 段一的逐字排版（分隔符、小节标题、自我称谓句式）无权威字面量，由本 feature 的颗粒度文档冻结（改措辞 = 改测试）；共享验收样例不比对提示词正文，故排版自由不影响兼容判据。
- `testutil` 公开 API 本 feature **不扩充**（测试夹具在各自测试包内联写入临时工作区）；若实施期出现三处以上重复夹具，再提给用户裁决抽取。
- 契约状态：本 feature 触及的运行时契约（`agent-md.md` / `run-and-errors.md` §4.3.1）在对应链路验证通过前为"待验证设计草案"——不构成本 feature 开工阻断（与 US-2 / US-3 既定安排一致）。
- `AGENT.md` 内的 `${ENV_VAR}` 解析**不在**本 feature 范围（无文档依据，凭证只活在配置文件里）；`settings.*` 取值合理性（如 `max_iterations: 0` 的语义）**不发明规则**，显式零值原样保留。

### Out of Scope（本 feature 明确不做）

- 规则 4「`tools` 全部可解析」的门禁（随工具注册表 US）；规则 6「且连接成功」半条（随 MCP US）。
- 技能 L2 / L3（模型按需读取技能正文与附属资源）与技能库白名单（随文件工具 US）。
- 四段提示词的段 2 / 3 / 4 与提示词组装器；运行级配置快照（冻结清单、哈希、原子发布、运行内冻结、"读即所记"）。
- 热加载 watcher；记忆注入。
- `schedules` 的注册与执行（交付规范化函数，注册归调度 US）；`scheduled_tasks` / `task_executions` 两表的写入。
- 实例锁的**重路径接线**（取锁 → 写模式打开 → 显式迁移 → 恢复）与崩溃恢复扫描、服务启动恢复。
- 管理台的模板脚手架、页面编辑器、归档删除的取消 Run 序列、多文件提交（staging 交换）。
- REST 端点与错误码映射；`chat` / `serve` / `gateway` 命令。
- 沙箱、工具执行、提示词组装器的工具列表段。
- `AGENT.md` 内 `${ENV_VAR}` 解析；`settings.*` 取值合理性校验。

## 人工验收项（harness 之外）

以下人工项在模块收口报告里单独列出（harness 已判卷之外）：门禁七目标全绿（覆盖率读数里实现包首次给出百分比）；验收 Demo 三条（建模板 → 列表面可见；六类坏例逐只点名；技能卡片 L1 的可见面）；Compatibility §5.1 判据 3 的"两侧逐条相同"与判据 6 的前提需对侧底座执行（人工对拍，读数回登记）；写模式拒绝文案与跨进程行为的人工复核（判别以 harness 的跨进程用例为准）；Windows 锁分支读数本机登记；凭证面 grep 零命中；装配确认（端口注入 + 依赖方向判据）；变动范围符合清单、无探针残留。
