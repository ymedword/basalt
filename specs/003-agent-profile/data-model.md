# Phase 1 数据模型：US-3 Agent 目录与 Profile 加载

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-16

本文件是设计面实体清单：**载体 / 成员 / 判据 / 状态机 / 不变量**。字段名、字面量与校验规则**不在此重新定义**——唯一事实源是 [docs/contracts/agent-md.md](../../docs/contracts/agent-md.md) §2 字段表、颗粒度文档 [docs/us/us3-agent-profile.md](../../docs/us/us3-agent-profile.md) §3（交付物清单）与 §6（本文固定口径）；本文件只做**汇集与引用**，不复制整表（第二份字面量必漂移）。本 US **无数据库实体**（颗粒度文档 §3.4：不建表、不写库）。

## 实体关系总览

```text
.basalt/ 工作区                     加载期（workspace.AgentLoader.LoadAll）
─────────────────                   ────────────────────────────────────────
agents/<name>/AGENT.md ─含──▶ 围栏 frontmatter ──▶ frontmatterDTO ──显式映射──▶ core.Profile ─┐
      │         └─ 围栏外正文（段一素材之一）          │                                    │
      ├─ 附属物（本 US 无 —— AGENT.md 是唯一定义入口）  ├─ 未知键 ─▶ WARN（只记键路径）        ├─▶ core.AgentEntry
      │                                              └─ 八条校验 + temperature ─┘   {Name, Registered,
skills/<name>/SKILL.md ──头（name/description）──▶ skillCard（L1 卡片）                  Profile, RejectReason}
mcp_servers.yaml（顶层键集）──────────────────────────┘                                    │
                                                                                         │
        段一（ContextLoader.SectionOne，只接受 Registered）◀─────────────────────────────┤
        profile list / show（同一份 LoadAll 结果集过滤，不另立解析路径）◀───────────────────┘
        调度 id 占用表（claim）：本只 Agent 的 schedules 校过 → 整只提交（被拒 Agent 不占用）
```

## 实体 1：AGENT.md 声明面（frontmatterDTO，`workspace` 私有）

- **载体**：`.basalt/agents/<name>/AGENT.md` 的 `---` 围栏 frontmatter（YAML）；**唯一定义入口**（同目录附属物不得成为第二入口）。
- **映射规则**（出处：§2.4 注 9，解码面）：

| frontmatter 键 | DTO 字段（类型） | 落到 `core.Profile` | 缺省 / 语义 |
| --- | --- | --- | --- |
| `name` | `Name string` | `Name` | 必填；必须与目录名**逐字一致**（否则拒载） |
| `description` | `Description string` | `Description` | 无校验 |
| `identity.agent_name` | `AgentName string` | `Identity.AgentName` | 未声明或空白 → 回填 `name`（解析后必非空） |
| `identity.prompt` | `Prompt string` | `Identity.Prompt` | 缺省空串（段一首元素，仅配置时注入） |
| `provider.name` | `providerDTO.Name string` | `Provider.Name` | 必填；必须 ∈ 已注册 provider 名集（否则拒载） |
| `provider.model` | `providerDTO.Model string` | `Provider.Model` | 必填；空白即拒；占位文本「请在此填写模型名」天然放行 |
| `provider.temperature` | `providerDTO.Temperature *float64` | `Provider.Temperature` | `nil` = 未声明（供应商缺省）；显式值须 0–2 且**有限**（NaN / ±Inf 拒，`reason` = `invalid provider.temperature`） |
| `tools` | `Tools []string` | `Tools` | 不声明 = 空池；本 US **只解析不判存在**（门禁归 US-4——占位登记） |
| `skills` | `Skills []string` | `Skills` | 声明即绑定；元素 = 技能库目录名；重复声明逐条渲染（不去重） |
| `mcp_servers` | `MCPServers []string` | `MCPServers` | 元素 = `mcp_servers.yaml` 顶层键名；只判存在性（「连接成功」半条随 US-7） |
| `bootstrap` | `Bootstrap []string` | `Bootstrap` | 相对工作区根；声明的文件必须存在 |
| `channels` | `Channels []channelDTO` | `Channels` | `nil`（未声明）→ `[{name: cli}]`；显式 `[]` → 空（**可区分**） |
| `channels[].name` / `.config` | `Name string` / `Config map[string]any` | `Channel` | `config` 只解析不校验（契约 §2.1 八条无此规则；未知键扫描**不进入** `config` 内容） |
| `settings.max_iterations` | `*int` | `Settings.MaxIterations` | `nil` → 10；显式 `0` 保留为 `0`（**不得**写 `if v == 0 { v = 10 }`） |
| `settings.max_history_turns` | `*int` | `Settings.MaxHistoryTurns` | `nil` → 20；显式 `0` 保留 |
| `schedules[].id` / `.cron` / `.zone` / `.message` | `scheduleDTO` 四字段 | `Schedule` | 见实体 5 |

- **未知键规则**（出处置信：§2.4 注 5，R1 裁决）：契约字段表之外的键**忽略但可见**——逐条 WARN（`reason` = `unknown frontmatter key`、`detail` 只放键路径、**不打印值**），继续装载；**不采用**严格模式。

## 实体 2：Profile（`core.Profile` 及子结构，声明层）

- **载体**：内存（解析产物）；**纯声明**（`core/profile.go` 零可执行语句，不带 yaml 标签）。
- **成员**：`Profile{Name, Description, Identity, Provider, Tools, Skills, MCPServers, Bootstrap, Channels, Settings, Schedules}` + `Identity{AgentName, Prompt}` + `ProviderRef{Name, Model, Temperature *float64}` + `Channel{Name, Config}` + `Settings{MaxIterations, MaxHistoryTurns}` + `Schedule{ID, Cron, Zone, Message}`——签名逐字见颗粒度文档 §3.1。
- **判据（八条加载期校验 + 一条字段约束）**（出处：颗粒度文档 FR2（对应 spec FR-002）/ 契约 §2.1；本 US 执行面如下）：

| # | 校验 | 本 US 执行面 |
| --- | --- | --- |
| 1 | `name` 与目录名一致 | 全量 |
| 2 | `provider.name` ∈ 已注册 provider 名集 | 全量（名集由 `AgentLoaderOptions.Providers` **显式注入**） |
| 3 | `model` 非空白 | 全量 |
| 4 | `tools` 全部可解析 | **占位登记**（只解析进 `Profile`，不判存在——随工具注册表模块） |
| 5 | `skills` 在技能库有实体 | 全量 |
| 6 | `mcp_servers` 在 `mcp_servers.yaml` 顶层键内 | 存在性半条（「连接成功」半条随 US-7） |
| 7 | `bootstrap` 声明的文件存在 | 全量 |
| 8 | `schedules` 段规则全量校验 | 全量（实体 5） |
| + | `provider.temperature`（已声明）落在 0–2 且有限 | 全量（R1 裁决：**在装载期执行**，不推迟到 US-4） |

- **状态机**：无运行态——`Profile` 是纯数据；生命周期 = 单次 `LoadAll` 内构造。

## 实体 3：装卸条目 `AgentEntry`（两段语义）

- **载体**：内存（`LoadAll` 返回值）；每个 Agent 目录**恰一条**（失败不静默消失）。
- **成员**：`Name`（目录名）/ `Registered` / `Profile *Profile` / `RejectReason string`。
- **两段语义**（出处：§2.4 注 3）：`Profile` 非 `nil` ⇔ frontmatter **解析成功**；`Registered` ⇔ 八条校验**全过**（+ temperature）——两者**独立**：

| 状态 | `Profile` | `Registered` | `RejectReason` | 命令面表现 |
| --- | --- | --- | --- | --- |
| 解析失败 | `nil` | `false` | `reason + ": " + detail` | `show` 只展示原因（声明面不可得） |
| 解析成功但校验拒 | 非 `nil` | `false` | `reason + ": " + detail` | `show` 展示声明面（provider / 模型）+ 原因 |
| 注册成功 | 非 `nil` | `true` | 空 | `list` 渲染 `registered=true（provider <p> / 模型 <m>）` |

- **判据**：`RejectReason` 的合并形态 = `reason`（detail 为空）或 `reason + ": " + detail`（契约字面量原样在前；用例断言一律用 `Contains` 判前缀——§6 固定口径 6）。`SectionOne` **只接受** `Registered` 的条目。
- **不变量**：任一规则不过 → **整只不注册**（不是跳过该条继续）；其余 Agent 照常装载（单只失败不拖垮整批）；每只失败恰一条点名 WARN（固定键 `agent` / `reason` / `detail`）。

## 实体 4：技能卡片 L1（`skillCard`）与技能库实体

- **载体**：`.basalt/skills/<name>/SKILL.md`（技能库）；条目形态 `skillCard{name, description, path}`（`workspace` 私有）。
- **成员 / 判据**：`name` = SKILL.md frontmatter name，**缺失回落技能目录名**（声明即绑定以目录名为准）；`description` = frontmatter description，**缺失为空串**（契约 §2.1 无此校验规则——不发明）；`path` **逐字** `.basalt/skills/<name>/SKILL.md`（技术方案 §11.3 字面量）。
- **绑定规则**（出处：颗粒度文档 FR4（对应 spec FR-007）/ 宪法 IV）：绑定只由 `skills:` 声明（声明即绑定）；声明名在库中无实体 → 该 Agent 整只不注册；Agent 本地 `skills/` 目录被无视并记 WARN（不改变绑定结果）；只注入 L1 卡片（`## 技能` 小节的卡片行），**不得**预载正文、**不得**新增 `use_skill` 工具。
- **重复声明**：逐条渲染（不去重；`cardsFor(base, p.Skills)` 机械读法——spec.md Clarifications 2026-10-16）；重复声明不改变注册判定、不产生重复卡片以外的副作用。

## 实体 5：Schedule 与调度 id 占用表（claim）

- **载体**：`Schedule{ID, Cron, Zone, Message}`（每 Agent 的 `schedules[]`）；占用表为**单次 `LoadAll` 内的内存结构**（不落盘）。
- **字段判定**：

| 字段 | 规则 |
| --- | --- |
| `cron` | 必须 6 段（秒 分 时 日 月 周）；5 段拒绝且文案含固定句 `Cron expression must consist of 6 fields`；公共子集 = 通配 / 数字 / 范围 / 列表 / 步长 / 月周缩写；`L` / `W` / `#` 与周字段 `7` 拒绝；日字段与周字段**不得同时被限定**（至少一个是裸通配） |
| `message` | 必填（空白拒） |
| `zone` | 缺省 `UTC`；`Local` 显式黑名单拒绝；其余为 IANA 名且必须可加载（`time.LoadLocation`）；**不得**回落进程本地时区；解析后必非空 |
| `id` | 恰一个任务时可缺省（派生 `<agent>-1`）；两个及以上每项必须显式；全局唯一 |

- **占用表语义**（出处：§2.4 注 6 + R1 裁决 2）：遍历序 = **目录名字节序 + Agent 内声明序**；`claim` 先到先得——**但只有完整校验通过的 Agent 才占用**：`validateSchedules` 只读 `claim` + 本只局部集，`LoadAll` 在注册成功分支**一次性提交**；被拒 Agent **不占用任何 id**；派生只在「恰一个任务」时发生（同 Agent 内不存在「显式 vs 派生」撞车，该形态只在跨 Agent 出现）。
- **detail 字面量**：十行场景表逐字冻结在颗粒度文档 §6 固定口径 5（段数 / 语法 / L·W·# / 周 7 / 日周共限 / `Local` / zone 名非法 / message / id 缺省 / id 撞车）——`reason` 恒为 `invalid schedule`。
- **规范化产物**：`CronSpec(s) = CRON_TZ=<zone> <cron>`（实体面唯一出口；**调度注册与执行不在本 US**——US-10）。

## 实体 6：实例锁（`InstanceLock`）与锁文件布局

- **载体**：`.basalt/basalt.lock`（OS 级独占文件锁；契约 §4.3.1）。
- **字节布局**（出处：§2.3 流程 D + 颗粒度文档 §2.4 注 1）：

| 偏移 | 内容 | 说明 |
| --- | --- | --- |
| `[0, 1)` | 锁区（1 字节） | OS 排他锁的载体；Windows 排他锁**阻止其他句柄读取锁定区** |
| `4096` 起，定长、字段序固定 | 诊断区（instance_id / pid / 开始时间） | 写在锁定区**之外**——竞争者（拿不到锁的进程）恰好需要读「是谁占着」；同一时刻只有持锁者写同一位置，无写竞争 |

- **状态机**：未持有 → `AcquireInstanceLock` 成功（持有）→ `Release`（幂等：重复调用 no-op 返回 `nil`；只解锁 + 关句柄，**不删除锁文件**）。失败路径：被活实例持有 → `*LockBusyError{PID, StartedAt, Detail}`（诊断缺失时 `Error()` 如实说明而不是编造）。
- **判据（宪法级形态）**：非阻塞获取；**不得**用「文件是否存在」或心跳过期作为抢锁判据；取锁前**不截断**锁文件；诊断写失败 → **先释放句柄**再返回错误；锁的生命周期由内核决定（进程退出 / 强杀即释放——`TestInstanceLock_HelperProcessReacquiresAfterHolderKilled`）。
- **命令面边界**：`create` / `delete`（写模式）取锁，**拒锁零业务写入**；`list` / `show`（轻路径）不取锁、**不得**清理或删除锁文件。

## 实体 7：归档目录（`profile delete` 的产物）

- **载体**：`.basalt/archive/<name>-<YYYYMMDD-HHMMSS>/`（R1 裁决：归档语义，**不再**列为「物理删除」等价备选）。
- **状态机**：`agents/<name>/` 存在 → 目标已存在 → **拒绝且源目录保留**；否则**单次 rename**（同文件系统内原子：要么成功、要么源目录原样）。
- **判据**：撞名检查**显式**（Unix `rename(2)` 会静默替换空的同名目录——不得指望 rename 的语义）；`now` 显式传入（归档名含时间戳 = 持久副作用）；会话 / 记忆 / 审计数据**不动**；缺目录拒绝；名字非法拒绝（与 create 共用 `ValidateAgentName`）。
- **不变量**：**不得**「复制 + 删除」、**不得**「先删源后建目标」（归档失败必须保留原目录）。

## 实体 8：段一文本（`SectionOne` 产物）与模板

- **载体**：内存字符串（无状态原语——每次调用重读素材、不缓存；`now` 显式传入）。
- **组成与顺序**（出处：颗粒度文档 FR7（对应 spec FR-015）；逐字排版冻结在颗粒度文档 §6 固定口径 1）：`identity.prompt`（若配置）→ 自我称谓 `你的名字是「<agent_name>」。` → AGENT.md 正文 → bootstrap 文件（按声明顺序，`## <bootstrap 文件>` 小节）→ `## 技能` 下的卡片行 `- <name>：<description>（<真路径>）` → 末尾 `当前时间：<YYYY-MM-DD HH:MM:SS ±hh:mm>`（格式 `2006-01-02 15:04:05 -07:00`，缺省 UTC、**输出含时区偏移**）。
- **判据**：`SectionOne` 只接受 `Registered` 的条目；任一素材（正文 / bootstrap 文件 / SKILL.md 头）读取失败 → 返回错误并**点名路径**，**不得**静默省略该节。
- **`profile create` 模板**（与段一共享素材形态但独立实体）：可被加载；`provider.name` = 配置数组**首项**（输出明示选中了谁；无配置 → 占位文本 + 说明「模板尚未注册」）；`provider.model` 恒为占位「请在此填写模型名」；`tools: []`（记忆工具到 US-6 才在池）；其余列表字段（`skills` / `schedules` / `mcp_servers` / `bootstrap`）为空或省略；名字经 **YAML 编码**（不得字符串插值）；已存在目录 → 拒绝且不覆盖；创建失败清理**本次**残留（既有目录一律不动）。

## 全局不变量

1. 扫描序 = 目录名**字节序**（显式排序）——「先到先得」以扫描序确定为前提（FR-017）。
2. 所有文件读写**经工作区根约束**（`os.Root`，含目录扫描）；不跟随越界软连接；新建权限沿用既有口径（`0o750` / `0o644`）；不覆盖已有文件。
3. 单只 Agent 的任何解析 / 校验失败**不产生** `error` 上抛——进 `AgentEntry`（`Registered=false`）+ 点名 WARN；顶层 `error` 只用于「工作区结构不可读」（未初始化等，命令面负责点名 + 指引 `basalt init`）。
4. 日志固定键 `agent` / `reason` / `detail`；`reason` 取契约字面量原样；日志与错误文案**不得**拼文件全文、**不得**回显 YAML 原始片段（解析错误压缩为位置 + 自产类别，库文案一个字不外传——`frontmatterErrorSummary`，与 `cli.yamlErrorSummary` 同口径，两处各一份为既知重复，§2.4 注 12）。
5. 禁用词：`time.Local` / `use_skill` / `context.WithValue`（本 US 全零命中）；时间一律显式传入。
