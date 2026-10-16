# US-3 Agent 目录与 Profile 加载 — 模块执行颗粒度文档

> **文档状态**：草案 R1（2026-10-16 生成；同日评审回合 R1 修订——六处补强 + 六项待决裁决落案，**待决事项已清零**；`/us-register 3` 登记、`/module-dev 3` 消费）
>
> **R1 修订要点（供对照上一稿）**
>
> 1. `schedules` 校验补全契约已有规则：公共子集（dom / dow 不得同时被限定）、`L` / `W` / `#` 与 `7` 拒、`message` 必填、`zone: Local` 显式黑名单（§2.2 约束 5、§2.3 流程 A、A4、§4.3）；
> 2. 调度 id 占用按**整只 Agent 提交**：`validateSchedules` 只读 `claim` + 局部集，`LoadAll` 在注册成功分支一次性合并（§2.3 流程 A、§4.3 反例）；
> 3. 实例锁生命周期写全：取锁前不截断、释放只解锁+关闭不删文件、诊断写失败先释放句柄、诊断布局在**锁定区之外**（Windows 排他锁挡住其他句柄读锁定区）、`Release` 幂等；Harness 以 **helper-process** 为主判据（跨进程互斥 / 强杀后重取 / 拒锁零写入），同进程双句柄降为补充（§2.3 流程 D、§4.1、§4.3）；
> 4. frontmatter 字段映射写实：`workspace` 私有 **DTO**（显式 `yaml:` 标签、snake_case 逐字对齐契约）+ `*T` 区分"未声明 / 显式零值"，`core.Profile` 不带 yaml 标签（§2.2 约束 15、§2.4 注 9、§3.1、§4.3）；
> 5. 文件操作失败边界补齐：create / delete 共用名称校验、归档撞名不覆盖、归档失败保留原目录（单次 rename 原子）、创建失败清理本次残留、模板名字经 YAML 编码而非插值、目录扫描也经 `os.Root` + 显式排序（§2.2 约束 12、§2.4 注 11、§3.1、§4.3）；
> 6. 段一用例改**位置断言**（逐对比较次序 + 时间收尾），补「装载后 bootstrap / `SKILL.md` 消失 → 返回错误」用例，并写明与 US-4 的跨模块约定（注入内容与 manifest 共享同次读取产物）（§2.2 约束 6、§4.3）；
> 7. 文字修正两处：段一「当前时间」删去「US-4 装配用进程本地时区」——**缺省 UTC、地方时间须显式 IANA、输出含时区偏移**（§2.4 注 10）；日志伪代码落实 `detail` 键（§2.3 流程 A）；验收项「`git status` 干净」改为「**变动范围符合清单、无探针残留**」（§6）。
>
> **事实主源顺位（供 `/us-register 3` 派生登记）**
>
> 1. **`docs/TechnicalSolution.md`**（文档之间最权威）——§8.2 AgentLoader（加载流水线与七项校验逐字）/ §8.3 ContextLoader（段一组成、卡片 L1、末尾日期时间）/ §8.5 AgentScheduler 的**加载期部分**（cron 6 段、5 段加载期即拒、`zone` 缺省 UTC、`time.Local` 是禁用词、`id` 规则、每任务时区经 `CRON_TZ=` 前缀——**注册与调度本体归 US-10**）/ §8.7 CLI 命令与轻·重路径（`profile` 四命令、`profile create` / `profile delete` 取实例锁）/ §8.1 工作区初始化（幂等骨架，本 US 读不写）/ §11.1 目录树（`agents/` / `skills/` 形态）/ §11.2 AGENT.md 完整样例（`channels[].config` 形态来源）/ §11.3 渐进披露 L1 与技能库**真路径**字面量 / §11.4 变更入口与归档删除顺序 / §11.5 占位符模型名放行 + 快照语义（快照本体归 US-4 / US-11）/ §4.2 提示词四段（段 1 组合式）/ §10.1 包结构与依赖方向 / §13 实施节奏（US-3 行：任务、可演示成果「`profile` 子命令可用；技能卡片 L1 注入」、涉及包 workspace / core / cli）
> 2. **`docs/DemandAnalysis.md`**——§1.5 实施节奏（US-3 定位与依赖序）/ §5.1 工作区初始化（`.basalt/` 目录树）/ §5.2 一个目录 = 一个 Agent（frontmatter 面、校验规则、`scripts/` 语义）/ §5.10 定时任务（第三触发源）/ §5.12 命令行工具（`profile` 四命令 + 轻 / 重两条路 + 写模式命令）/ §5.13 配置与密钥 / §5.14 管理台与 Agent 动态管理（模板脚手架、归档删除）/ §9.1 启动加载 / §9.5 技能渐进披露 / §10.2 文件系统数据 / §13.4 质量门禁验收
> 3. **`.specify/memory/constitution.md`**（原则级最高）——原则 IV（一个目录 = 一个 Agent；技能**声明式绑定 + 渐进披露**；**不得**引入软连接类机制、**不得**预载正文、**不得**新增 `use_skill` 工具）/ VII（context 只承载取消 / 截止 / 追踪，业务状态显式传递）/ VIII（单二进制与依赖最小化）/ X（范围卡紧）+ 架构约束（包与依赖方向、凭证）+ 开发工作流（门禁、覆盖率判据）
> 4. **`docs/contracts/`**——`agent-md.md` §1 AGENT.md 结构（`---` 围栏 + 正文）、§2 frontmatter 字段表（**固定字面量**）、§2.1 加载期校验八条与 WARN 字面量（`name mismatch` / `provider not registered: <name>` / `blank model` / `tool not registered: <name>` / `skill not found: <name>` / `mcp server skipped` / `bootstrap file missing`）、§3 `schedules` 段规则（`message` 必填、`zone` 缺省 UTC）、§3.1 cron 公共子集与每任务时区前缀；`run-and-errors.md` §4.3.1（工作区实例锁：锁文件 `.basalt/basalt.lock`、诊断**只作诊断**、心跳**不构成**抢锁理由、exists 判据不得、锁文件**可留在磁盘不清理**、写模式命令准入、只读命令不取锁）
> 5. **附加事实源**——`docs/Compatibility.md` §2.1 命令入口行（`profile create` / `profile delete` 属写模式命令）/ §3 #6（CLI 写模式命令面承诺）/ §5.1 共享验收样例（样例 `AGENT.md` + 六条判据——本 US 的验收判据，逐条比对）/ §5.2 cron 对照抽查；`docs/EngineeringStandards.md` §1 Go / §2 测试 / §4 日志与错误（WARN / ERROR 级别与固定键）
> 6. **落地参考（非权威；冲突以技术方案为准）**——`docs/AiProgrammingGuide.md` §4.3 US-3 拆解（核心目标 / 涉及包 / task 大类 / 颗粒度 12~15 文件 / 硬依赖 US-2 / 验收 Demo）+ §4.3 尾注（L2/L3 与「工具名在注册池」门禁随 US-4）/ §7 陷阱表（#8 软连接形态、#9 不声明 tools = 空池、#27 热加载写成每轮重读、#32 YAML 值以 `*` 开头、#39 两次读取、#40 YAML 库错误文案脱敏）/ §8.3 治理（契约同批更新、评审排序）
>
> **固定裁决句**：本文与最新技术方案冲突时，**以技术方案为准**；本仓配套工件（spec / plan / 已批准 US 清单）已锁定的引用在不与前者冲突时沿用，不另行改判。**spec 锚点现状（核对于 2026-10-16）**：`specs/` 下现有 `001-foundation/` 与 `002-provider/`（US-1 / US-2 已完成验收的历史设计，**只读**——本规格不得回改它们）；US-3 的 spec 目录由 `/module-dev 3` 新建（`.specify/feature.json` 当前指向 `specs/002-provider`，随 speckit 流程重定）。与本 US **无冲突**，且有两条正向引用：① `specs/002-provider/` 对 `providers` 段的注册判定即本 US 校验规则 2 的输入面；② US-1 / US-2 颗粒度文档均记载「实例锁实现归 US-4」——本文 §2.4 注 1 记录 R1 裁决后的**归属调整**（§5 末行为登记处；历史工件不回改）。
>
> **登记派生信息**：技术方案章节 = §4.2 / §8.1 / §8.2 / §8.3 / §8.5 / §8.7 / §10.1 / §11.1 / §11.2 / §11.3 / §11.4 / §11.5 / §13；需求文档章节 = §1.5 / §5.1 / §5.2 / §5.10 / §5.12 / §5.13 / §5.14 / §9.1 / §9.5 / §10.2 / §13.4；模块落位（摘要）= `internal/{workspace,core,cli}`（技术方案 §13 的 US-3「涉及包」列）+ `internal/app`（端口适配器**改造**，非新落点）+ `go.mod`（新增坐标一处：`github.com/robfig/cron/v3 v3.0.1`，表内已核实；`golang.org/x/sys v0.48.0` 由 indirect 迁为直接 require——**无新增坐标**，见 §3.3）；`testutil` **不**新增（测试夹具内联，见 §2.4 注 8）。
>
> **台账状态**：登记时 `docs/us/REGISTRY.md` 查无 US-3 行——本章节对照由本 skill 自定位，供 `/us-register 3` 派生。

## 1. 模块概述

**定位**：把 `AGENT.md` 契约变成**可装载的 Agent 定义**——`.basalt/agents/<name>/AGENT.md` 经 frontmatter 解析与加载期校验派生成 `Profile`（任一不过**整只不注册** + 点名 WARN），system prompt **第一段**（AGENT.md 正文 + Bootstrap 文件 + 技能卡片 L1 + 当前日期时间）可组装，`profile list / show / create / delete` 四条子命令可用（`create` / `delete` 为写模式命令，取工作区实例锁）。

**价值（WHAT / WHY）**：US-3 是"一个目录 = 一个 Agent"这条产品命题第一次落到可执行代码上（宪法 IV 从条文变成可 grep 的加载器与校验流水线）。它一次交付四件可判卷的事：① **加载契约落地**——契约 `agent-md.md` 的字段表与八条校验（本 US 六条全量 + 两条占位登记）从文档变成用例；② **技能声明式绑定闭环**——`skills:` 声明 → 技能库实体查找 → 段一里的 L1 卡片（`name + description + 技能库真路径`），软连接形态从第一天起就不被识别（宪法 IV）；③ **`AGENT.md` 双底座可互换的第一次实证**——共享验收样例（Compatibility §5.1 的 `compat-check` 样例与 cron 判据）在本 US 被逐条比对；④ **写模式命令准入的最小面**——实例锁从条文（契约 §4.3.1）变成可跨进程判卷的句柄（R1 裁决归属，见 §2.4 注 1）。同时它是循环之前的**提示词半边**：US-4 的 `PromptBuilder` 消费本 US 的 `ContextLoader.SectionOne`，运行级快照冻结（"读即所记"）在 US-4 组装。规模参考：约 12~15 个实现文件 + 对应测试（编程指南 §4.3）。**硬依赖 US-1（已交付）与 US-2（已交付）**——provider 注册判定（规则 2）的输入名集来自 US-2 打通的配置面；本 US 不消费其他未交付能力。

## 2. 设计要点与约束

### 2.1 职责边界表

| 维度 | 本 US 职责（做） | 非本 US 职责（不做，归属） |
| --- | --- | --- |
| Agent 契约加载 | `workspace.AgentLoader`：围栏切分 → 未知键扫描 → YAML 解码进 DTO → 八条校验（六条全量）+ 派生 `Profile`；单只失败不拖垮整批 | 热加载 watcher 与原子发布（US-11）；多文件提交的 staging 交换（US-12） |
| 校验的占位面 | 规则 4（tools）解析进 `Profile.Tools` 但**不判存在**（占位登记）；规则 6（mcp_servers）只判**在配置内**（顶层键名） | 工具名在注册池的门禁（US-4 的 Registry）；MCP「连接成功」判定（US-7） |
| 段一拼装 | `workspace.ContextLoader.SectionOne`：正文 + Bootstrap 文件 + 技能卡片 L1 + 当前日期时间；技能库实体查找与 `SKILL.md` 头解析；**读取失败即报错** | 四段提示词的其余三段与 `PromptBuilder`（US-4）；运行级快照冻结与清单哈希（US-4 / US-11）；记忆注入（US-6） |
| 命令面 | `profile list` / `show`（轻路径：只读文件系统，无装配、不开库、不取锁）；`profile create` / `delete`（写模式：取实例锁） | `chat` / `serve` / `gateway`（US-4 / US-9 / US-10）；管理台页面与 Agent 动态管理（US-12） |
| 写模式与实例锁 | `profile create` / `delete` 的工作区写入取实例锁、被活实例持有时拒绝启动并给可读文案（**最小面**：锁本体 + 两命令接线——§2.4 注 1 R1 裁决） | 重路径接线（取锁 → 写模式打开 → 显式迁移 → 恢复）与崩溃恢复扫描（US-4）；`serve` 启动恢复（US-4） |
| 派生与调度 | `schedules` 的加载期校验（段数 / 公共子集 / zone / message / id）与 `CronSpec` 规范化（`CRON_TZ=` 前缀） | `AgentScheduler` 注册与执行、`scheduled_tasks` / `task_executions` 两表（US-10）；触发时间对拍的程序化对拍脚本（Compatibility §5.2，发布前） |
| 包与装配 | `internal/workspace` 新实现、`internal/core` 加声明（零语句）、`internal/cli` 新增命令组与端口、`internal/app` 适配器改造 | `core` 出现可执行语句（违包纪律 1）；为 CLI 另立解析路径（技术方案 §8.7「不得为同一语义写两份实现」） |
| 测试夹具 | 各测试包内联构造 `.basalt/` 夹具（`t.TempDir()` + 写入） | `testutil` 公开 API 扩充（本 US 不需要，见 §2.4 注 8）；MCP 测试服务（US-7） |

### 2.2 关键约束清单（每条对应宪法条款 / 契约，带症状或判据）

1. **一个目录 = 一个 Agent**（宪法 IV + 契约 §1）：加载单元 = `.basalt/agents/` 下的**每个子目录**；`AGENT.md` 是唯一定义入口（`scripts/` / `REFERENCE.md` 是附属物，**不得**成为第二入口）。目录名即 Agent 名，`name` 必须逐字一致（规则 1）。症状：为发现 Agent 引入别的手段（扫文件名模式、读索引文件）= 违约束。
2. **技能声明式绑定 + 渐进披露**（宪法 IV + 技术方案 §11.3）：绑定**只**由 frontmatter `skills:` 声明（声明即绑定，各平台一致）；**不得**识别 Agent 本地 `skills/` 目录或软连接类形态（遗留形态备案：本地 `skills/` 目录被**无视**并记一条 WARN，不改变绑定结果）；段一只注入 L1 卡片，**不得**预载 `SKILL.md` 正文、**不得**新增 `use_skill` 工具。判据可执行：加载测试断言"声明即绑定"（声明 → 注册 + 卡片出现；不声明 → 卡片不出现）；收尾 grep 无 `use_skill`。
3. **校验不过即整只不注册**（契约 §2.1）：八条规则中任一不过 = **整只**不注册（不是"跳过那一条继续"）+ 点名 WARN；**其余 Agent 照常**（单只失败不拖垮整批，技术方案 §8.2「其余照常」）。同一条纪律延伸到**调度 id 的占用**：只有完整校验通过的 Agent 才占用（§2.4 注 6）。症状：坏配置只少一个工具面而 Agent 仍注册 = 违约束。
4. **两条占位必须显式登记、不得静默通过**（编程指南 §4.3 尾注 + §5 范围边界）：规则 4（tools 全部可解析）随 US-4 的 Registry 接入——本 US 只解析进 `Profile.Tools`；规则 6 的「**且连接成功**」半条随 US-7——本 US 只判名字在 `mcp_servers.yaml` 顶层键内。两处占位在 §5 逐条登记，module-dev **不得**顺手补齐或改写占位范围。
5. **schedules 加载期规则（全量）**（契约 §3 / §3.1 + 技术方案 §8.5）：`cron` **6 段**（秒 分 时 日 月 周，`cron.NewParser(Second|Minute|Hour|Dom|Month|Dow)`——5 段制式拒）；**公共子集**：`*` / 数字 / 范围 / 列表 / 步长 / 月周缩写（大小写不敏感，经 robfig `parseIntOrName` 小写化）可用，`7` / `L` / `W` / `#` **拒**（robfig v3.0.1 同样不支持——预检的作用是统一报错口径与细节，见 §2.4 注 6），且 **dom 与 dow 不得同时被限定**（至少一个是裸 `*`；`*/2`、`1-15`、`1,5`、单值都算限定）；`message` **必填**；`zone` 缺省 **UTC**，`Local` **显式黑名单**（`time.LoadLocation("Local")` 会成功——不能只靠它校验），其余取值经 `time.LoadLocation` 校验，**不得**回落进程本地时区（`time.Local` 是禁用词）；`id` 单项可缺省（派生 `<agent>-1`，**仅当恰一个任务**）、**≥2 项每项必须显式**、全局唯一（先到先得 + **占用按整只 Agent 提交**——§2.4 注 6）；每任务时区经 `CRON_TZ=<zone>` 前缀（`CronSpec` 规范化函数本 US 交付，调度注册归 US-10）。5 段拒绝的文案**必须含固定句** `Cron expression must consist of 6 fields`（Compatibility §5.1 判据 4）。
6. **段一组成与顺序**（技术方案 §4.2 / §8.3 + 契约 §2）：`identity.prompt`（若配置）→ 自我称谓（`identity.agent_name`，缺省 = `name`）→ AGENT.md 正文 → Bootstrap 文件（按声明顺序，相对工作区根）→ 技能卡片 L1 → 末尾当前日期时间。卡片三要素**逐字来自**技术方案 §11.3：`name + description + 技能库真路径`，真路径字面量为 `.basalt/skills/<name>/SKILL.md`——US-4 的 `read_file` 相对路径解析**必须**能吃下该形态。**读取失败即错误**：正文 / Bootstrap 文件 / `SKILL.md` 在装载后消失或不可读 → `SectionOne` 返回**错误**（点名路径），**不得**静默省略该节——静默省略会让"注入内容"与"清单哈希"悄悄分叉。**跨模块约定（US-4）**：注入内容与运行级 manifest 哈希**必须**共享同次读取产物——**不得**在本接口返回后对同一份素材（正文 / Bootstrap / `SKILL.md` 头）二次读盘算哈希（§2.2 约束 7 与 §2.4 注 7）。
7. **快照语义不在本 US**（技术方案 §8.3 / §11.5）：本 US 的 `SectionOne` 是**无状态拼装原语**——每次调用重读文件（正文 / bootstrap / `SKILL.md` 头），**不缓存**（反向陷阱见编程指南 §7 陷阱 27：热加载不得写成"每轮重读"——本 US 既不做缓存也不做热加载）；运行内冻结、"读即所记"的清单哈希与原子发布归 US-4 组装（占位点见 §3.5）。**不得**在本 US 引入注册表或快照类型，也**不得**让后续实现绕过约束 6 的"同次读取产物"约定。
8. **context 纪律**（宪法 VII）：`Profile` / `AgentEntry` / provider 名集 / `now time.Time` 一律**显式类型化参数**；`context.WithValue` 零使用；**不得**用包级变量模拟 ThreadLocal。`now` 显式传入而非函数内 `time.Now()`——可测性即判据（与 Compatibility §5.1「固定起点、不得用当前时间」同源；归档名含时间戳同理，§2.4 注 11）。
9. **包与依赖方向**（宪法·架构约束 + 技术方案 §10.1）：`core` **只放声明**（类型 / 常量，零可执行语句——与 US-2 同纪律）；实现进 `workspace`；`cli` **不得** import `workspace`（端口定义在消费方、适配器在 `app`；现有 `ports_test.go` 的 `go list` 判据自动覆盖本 US）；无环由 CI 复核。
10. **写模式命令准入与锁生命周期**（契约 §4.3.1 + 技术方案 §8.7 + Compatibility §2.1 / §3 #6）：`profile create` / `profile delete` 是写工作区的**写模式命令**——取工作区实例锁（**非阻塞**），被活实例持有 → **拒绝启动**并给可读文案（点名诊断信息里的 pid 与开始时间；服务在跑时指引改用 REST / 管理台）；**不得**用 exists 判据或心跳判据抢锁；**只读命令**（`init`、`profile list` / `profile show`、`session list`）不取锁、不做恢复、**不得**清理或删除锁文件。锁手段按 US-1 §6 三场景读数：Windows `windows.LockFileEx`、类 Unix `flock`——`O_EXCL` 文件与心跳 TTL **不得**用于实例锁。**锁生命周期（R1 补强，细节见 §2.3 流程 D）**：取锁前**不截断**锁文件；只锁 offset 0 的 1 字节；诊断记录写在**锁定区之外**（Windows 排他锁阻止其他句柄读锁定区——布局必须让竞争方读得到）；诊断写失败 → **先释放句柄**再返回错误；`Release` **幂等**（重复调用 no-op 返回 nil），只解锁 + 关闭，**不删除锁文件**（契约 §4.3.1：锁文件可留在磁盘，删除会引入"解锁后删文件"竞态）。
11. **单一实现纪律**（技术方案 §8.7 / §11.4）：`profile list` / `show` 的注册判定与 `AgentLoader.LoadAll` 是**同一份**解析与校验代码（**不得**为 CLI 另立解析路径，也**不得**在轻路径写死假数据）；`show` 复用 `LoadAll` 结果过滤，不另立单只装载路径。`create` / `delete` 的**名称校验**同判据同函数（§3.1 `validateAgentName`）。
12. **文件系统纪律**（沿用 US-1 口径 + 本轮补强）：所有读写经 `os.Root` 钉在工作区目录内（软连接跳出去在根上被挡）——**包括目录扫描**（`base.Open("agents")` + `ReadDir`，**不得**裸 `os.ReadDir(filepath.Join(...))`）；扫描序**显式排序**（`slices.SortFunc` 按名字节序——"先到先得"以扫描序确定为前提，**不得**依赖库的隐式序）；条目是否目录按 `DirEntry.IsDir()`（**不跟随软连接**）判定——软连接形态的"目录"如实忽略（宪法 IV 的反面形态）；新建目录 0o750、新建文件 0o644（US-1 已有常量）；**不覆盖已有文件**（`create` 遇已存在目录拒绝）。症状：加载器跟软连接读到工作区外 = 违约束。
13. **文案脱敏与可观测性双轨**（工程规范 §4 + 编程指南 §7 陷阱 40）：加载失败 = **WARN**（每只一条，固定键 `agent` / `reason` / `detail`——`reason` 取契约字面量原样、点名细节进 `detail`；日志**分键**输出，`AgentEntry.RejectReason` 是同一对的人读合并，见「本文固定口径」6）；**未知键**不拒载但每条一条 WARN（`reason` = `unknown frontmatter key`、`detail` = 键路径，**不打印值**；路径集先排序再逐条输出——不得依赖 map 遍历序）；工作区写入失败（create / delete）= **ERROR**；日志与错误文案**不得**拼文件全文、**不得**回显 YAML 原始片段——AGENT.md 的解析 / 解码错误经与 `cli.yamlErrorSummary` **同口径**的压缩（位置 + 自产类别，库文案一个字不外传；`workspace` 不得 import `cli`——两处各一份小函数是**既知重复**，§2.4 注 12）。本 US 无审计表写入（`AgentEntry` 的注册结果走日志与命令输出两轨，不进数据库）。
14. **依赖最小化 + 坐标先核实**（宪法 VIII）：本 US 新增坐标**唯一一个**——`github.com/robfig/cron/v3 v3.0.1`（在固定技术栈句与已批准计划内，**直接落地**）。**本轮已核实（2026-10-16，源码在模块缓存逐行读）**：`GOPROXY=https://goproxy.cn go list -m -versions github.com/robfig/cron/v3` 输出末位 `v3.0.1`、退出码 0；`Parser.Parse` 支持 `TZ=` / `CRON_TZ=` 前缀与 `time.LoadLocation`（坏时区报 `provided bad location`）；**缺省时区是 `time.Local`**（`parser.go` 的 `var loc = time.Local`——这正是"每任务必须显式 `CRON_TZ=` 前缀"的机制性理由）；字段数错误原文 `expected exactly %d fields, found %d: %s`（故固定句必须由本仓自产）；**`L` / `W` / `#` 在 v3.0.1 源码零命中（不支持）**、dow 上界 **6**（`spec.go` 的 `bounds{0, 6}`——`7` 同样被拒）；`SpecSchedule.Next` 严格晚于传入时刻（"下一个整秒"）。本 US 只借 `Parser` / `Schedule` 两件做校验与触发时间计算；**不得**让 `core` import 它（调度本体归 US-10 的 `internal/adapters/cron`，技术方案 §10.1 纪律 3）。`time/tzdata` 已在 US-1 内嵌——`time.LoadLocation("Asia/Shanghai")` 可用的前提。
15. **frontmatter 解析与映射纪律**（R1 新增；裁决出处见 §2.4 注 9）：解析经 `workspace` **私有 DTO**——显式 `yaml:` 标签、键名 snake_case **逐字**对齐契约 §2（`agent_name` / `mcp_servers` / `max_iterations` / `max_history_turns` 靠默认字段名一个都匹配不上）；`core.Profile` **不带** yaml 标签（契约层不绑序列化库——宪法·架构约束）。`*T` 区分"**未声明**"与"**显式零值 / 空列表**"：`temperature *float64`（nil = 未声明、显式 `0` 保留为 0）、`settings.* *int`（nil → 10 / 20、显式 `0` 保留）、`channels` **未声明** → `[{name: cli}]` 而**显式 `[]`** → 空（契约 §2 的缺省列与显式给出是两件事）；纯列表字段（`tools` / `skills` / `mcp_servers` / `bootstrap` / `schedules`）未声明与显式 `[]` **等价**（契约缺省列都是 `[]`）。**未知键宽容但可见**：先解 `map[string]any` 比对已知键集 → WARN 键路径（不打印值）→ 继续装载（前向兼容，Compatibility §2.2 精神）。`provider.temperature` 已声明则**必须**落在 0–2 且有限（NaN / ±Inf 拒——契约 §2 的字段约束在装载期执行，理由见 §2.4 注 9）。同一份字节可解两遍（未知键扫描 + DTO），**不得**把文件读两遍（陷阱 39 的根因形态）。

### 2.3 核心逻辑

本 US 核心逻辑为**四条流程**：A 装载（扫描 → 解析 → 校验 → 派生 / 拒绝 → 占用提交）、B 段一拼装、C `profile` 命令组（轻 / 写两条路）、D 实例锁取用（R1 裁决后的归属已落案，不再"条件交付"）。伪代码方法名与 §3 交付物清单**逐字一致**。

#### 流程 A — 装载（`internal/workspace`，`AgentLoader.LoadAll`）

```text
AgentLoader.LoadAll(root)                    # root = <WorkDir>/.basalt（与 US-1 同口径）
  │
  ├─ base = os.OpenRoot(root)                # 全部读取经根约束（US-1 Inspect 同形态）
  ├─ dir = base.Open("agents")；dirs = dir.ReadDir(-1)
  ├─ slices.SortFunc(dirs, 按名字节序)         # **显式排序**——扫描序确定才谈得上"先到先得"
  ├─ 读 base 内 mcp_servers.yaml 顶层键名集（文件缺失 → 空集）
  │
  └─ 逐目录 loadOne（单只失败不拖垮整批）：
       ① 读 AGENT.md（base.ReadFile）→ 围栏切分 → 未知键扫描（**同一份字节**）→ 解码进 DTO → 映射 + 缺省填充
       ② 规则 1  name == 目录名                         不过 → `name mismatch`
       ③ 规则 2  provider.name ∈ providers              不过 → `provider not registered: <name>`
       ④ 规则 3  model 非空白（占位符天然放行）            不过 → `blank model`
       ⑤ 字段约束 provider.temperature 已声明则须 0–2 且有限  不过 → `invalid provider.temperature`
       ⑥ 规则 4  tools 全部可解析          —— 占位（US-4 的 Registry 接入）
       ⑦ 规则 5  skills 在技能库有实体                   不过 → `skill not found: <name>`
       ⑧ 规则 6  mcp_servers 在配置内      —— 只判存在性    不过 → `mcp server skipped`
       ⑨ 规则 7  bootstrap 文件存在                     不过 → `bootstrap file missing`
       ⑩ 规则 8  schedules（段数 / 公共子集 / zone / message / id）  不过 → `invalid schedule`
       ⑪ 全过 → 占用集合并进全程表（**唯一提交点**）+ Registered = true；
          任一不过 → 整只不注册 + WARN（agent / reason / detail），**本只的占用一律不入表**
```

```go
// 伪代码（方法名与 §3 逐字一致）
func (l *AgentLoader) LoadAll(root string) ([]core.AgentEntry, error):
    base, err := os.OpenRoot(root)                # 钉根：软连接越界在根上被挡（US-1 形态）
    if err != nil: return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
    defer base.Close()                            # 失败经 errors.Join 并入（US-1 口径）
    dir, err := base.Open("agents")               # agents/ 缺失 → 装载错误（≠"零只 Agent"）
    if err != nil: return nil, fmt.Errorf("打开 Agent 目录: %w", err)
    dirs, err := dir.ReadDir(-1)
    if err != nil: return nil, fmt.Errorf("读取 Agent 目录: %w", err)
    slices.SortFunc(dirs, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })

    mcpNames := readMCPServerNames(base)          # mcp_servers.yaml 顶层键；缺失 → 空集
    claim := make(map[string]string)              # 任务 id → 占用者；**只由完整校验通过的 Agent 提交**
    entries := make([]core.AgentEntry, 0, len(dirs))
    for _, d := range dirs:
        if !d.IsDir(): continue                   # 非目录 / 软连接条目如实忽略（附属物不得成为入口）
        entry, rej := l.loadOne(base, d.Name(), mcpNames, claim)
        if rej.reason == "":
            for id, owner := range rej.claimed: claim[id] = owner    # **整只提交：本行是唯一的占用点**
            entry.Registered = true
        else:
            entry.RejectReason = rej.mergedReason()
            l.logger.Warn("Agent 未注册", "agent", entry.Name,
                "reason", rej.reason, "detail", rej.detail)          # 固定键：agent / reason / detail
        entries = append(entries, entry)
    return entries, nil

func (l *AgentLoader) loadOne(base *os.Root, name string, mcpNames []string,
                              claim map[string]string) (core.AgentEntry, reject):
    entry := core.AgentEntry{Name: name}
    fm, err := base.ReadFile(filepath.Join(name, "AGENT.md"))
    if err != nil: return entry, reject{reason: "read AGENT.md failed", detail: filepath.Join(name, "AGENT.md")}
    p, warns, err := parseFrontmatter(fm)         # 同一份字节：先查未知键、再解 DTO → 映射 + 缺省填充
    for _, w := range warns:                      # 未知键不拒载，逐条 WARN（键路径，**不打印值**）
        l.logger.Warn("frontmatter 含未知键——已忽略", "agent", name,
            "reason", "unknown frontmatter key", "detail", w)
    if err != nil:                                # 解析 / 解码失败：位置 + 自产类别，库文案不外传（注 12）
        return entry, reject{reason: "invalid frontmatter", detail: frontmatterErrorSummary(err)}
    entry.Profile = p                             # 解析成功即有 Profile（即使校验未过——供 list/show 展示）
    if p.Name != name: return entry, reject{reason: "name mismatch", detail: ...}          # 规则 1
    if p.Identity.AgentName == "": p.Identity.AgentName = p.Name
    if !slices.Contains(l.providers, p.Provider.Name):                                     # 规则 2
        return entry, reject{reason: "provider not registered: " + p.Provider.Name}
    if strings.TrimSpace(p.Provider.Model) == "": return entry, reject{reason: "blank model"}  # 规则 3
    if t := p.Provider.Temperature; t != nil && !validTemperature(*t):                     # 字段约束
        return entry, reject{reason: "invalid provider.temperature", detail: ...}
    # 规则 4：占位（US-4 的 Registry 接入；本 US 不判存在性）
    if missing := missingSkill(lib, p.Skills); missing != "":                              # 规则 5（lib = 技能库根）
        return entry, reject{reason: "skill not found: " + missing}
    for _, m := range p.MCPServers:                                                        # 规则 6（存在性半条）
        if !slices.Contains(mcpNames, m): return entry, reject{reason: "mcp server skipped", detail: m}
    for _, b := range p.Bootstrap:                                                         # 规则 7（须为普通文件）
        if detail, ok := checkBootstrapEntry(base, b); !ok: return entry, reject{reason: "bootstrap file missing", detail: detail}
    claimed, err := validateSchedules(p.Name, p.Schedules, claim)                          # 规则 8
    if err != nil: return entry, reject{reason: "invalid schedule", detail: err.Error()}
    return entry, reject{claimed: claimed}        # 占用集**只在全过时**非空；被拒时为零值（不提交）
```

```go
// reject 是加载失败的内部形态（非导出）：reason 取契约字面量原样或自描述短语，detail 放点名细节。
// 两者分开是因为日志要**分键**（agent / reason / detail），而 AgentEntry.RejectReason 给人读。
// claimed 是本只 Agent 的调度 id 占用集——只有"reason 为空"（全过）时非零，LoadAll 据此提交。
type reject struct {
    reason  string            // name mismatch / blank model / skill not found: <n> / invalid schedule / invalid frontmatter / …
    detail  string            // 键路径 / 文件路径 / 任务 id / cron 原文——契约字面量含名字时不重复填
    claimed map[string]string // 任务 id → 占用者（本只）；被拒时为零值
}

// mergedReason 人读合并：detail 为空 → reason；否则 "reason: detail"（契约字面量原样在前——断言用前缀）。
func (r reject) mergedReason() string

// validateSchedules 校验一只 Agent 的全部任务，返回**本只**的占用集（局部集）；
// claim **只读**——占用由 LoadAll 在整只通过后一次性提交（§2.4 注 6 的 R1 裁决）。
// 逐任务的判定序（全部失败共用 reason "invalid schedule"）：
//   ⓪ cron 自带 TZ= / CRON_TZ= 前缀 → 拒（时区只有 zone 一个来源；R2 裁决，见 §2.4 注 14）
//   ① 段数 ≠ 6                        → 固定句 "Cron expression must consist of 6 fields"
//   ② 语法非法（robfig 拒，**解析 CronSpec 的规范化结果**）→ 附 robfig 原文（如 expected exactly 6 fields, found 5: [...]）
//   ③ 公共子集：L / W / # ；dow=7 ；dom 与 dow 同时被限定（*/2 计为限定）
//   ④ zone：空 → 缺省 UTC；"Local" → 黑名单拒；其余经 time.LoadLocation（失败附原文）
//   ⑤ message 空白 → 拒
//   ⑥ id：恰一个任务时缺省派生 <agent>-1；≥2 项未显式 → 拒；本只内重复 → 拒；与 claim 撞车 → 拒（点名占用者）
func validateSchedules(agent string, ss []core.Schedule, claim map[string]string) (map[string]string, error)
```

> **id 撞车的形态说明**：派生只在"恰一个任务"时发生 ⇒ **同 Agent 内不存在**"显式 vs 派生"的撞车；该形态只在**跨 Agent** 出现（A 单任务派生 `a-1`，B 显式 `a-1`——按键序 A 先到，B 拒）。同 Agent 内的重复只可能是**两条显式 id 相同**。两种形态各有用例（§4.3）。

#### 流程 B — 段一拼装（`ContextLoader.SectionOne`）

```text
SectionOne(root, entry, now)
  │
  ├─ entry.Profile == nil 或 !entry.Registered → 点名错误（未注册的 Agent 不得组装系统提示词）
  ├─ base = os.OpenRoot(root)（全部读取经根约束）
  ├─ identity.prompt（若配置）                    # 「拼在系统提示词开头」（契约 §2）
  ├─ 自我称谓行（identity.agent_name，缺省 = name）
  ├─ AGENT.md 正文（**调用时重读**——本 US 无缓存）
  ├─ bootstrap 文件（声明顺序；相对工作区根，经 os.Root 读）
  ├─ 技能卡片 L1（每个绑定技能一行）：
  │      name + description（SKILL.md frontmatter）+ 真路径 .basalt/skills/<name>/SKILL.md
  └─ 末尾「当前时间：<YYYY-MM-DD HH:MM:SS ±hh:mm>」   # now 显式传入；缺省 UTC，输出含偏移（§2.4 注 10）
  任一步读取失败（正文 / bootstrap 文件 / SKILL.md 消失或不可读）→ **返回错误**（点名路径），不得静默省略
```

```go
// 伪代码（方法名与 §3 逐字一致）
func (c *ContextLoader) SectionOne(root string, entry core.AgentEntry, now time.Time) (string, error):
    if entry.Profile == nil || !entry.Registered:
        return "", fmt.Errorf("未注册的 Agent %s，不得组装系统提示词", entry.Name)
    base, err := os.OpenRoot(root)
    if err != nil: return "", fmt.Errorf("打开工作区目录 %s: %w", root, err)
    defer base.Close()
    p := entry.Profile
    body, err := base.ReadFile(filepath.Join(entry.Name, "AGENT.md"))    # 装载后消失 / 不可读 → 报错
    if err != nil: return "", fmt.Errorf("读取 Agent 正文 %s: %w", entry.Name, err)
    cards, err := cardsFor(base, p.Skills)      # SKILL.md 读不到 → 报错（不得回落空卡片）
    if err != nil: return "", err
    var b strings.Builder
    if p.Identity.Prompt != "": b.WriteString(p.Identity.Prompt + "\n\n")
    b.WriteString("你的名字是「" + p.Identity.AgentName + "」。\n\n")
    b.WriteString(string(body))
    for _, f := range p.Bootstrap:
        content, err := base.ReadFile(filepath.FromSlash(f))
        if err != nil: return "", fmt.Errorf("读取 bootstrap 文件 %s: %w", f, err)   # 不得静默省略
        b.WriteString("\n\n## " + f + "\n\n" + string(content))
    if len(cards) > 0:
        b.WriteString("\n\n## 技能\n")
        for _, c := range cards:
            b.WriteString("- " + c.name + "：" + c.description + "（" + c.path + "）\n")
    b.WriteString("\n\n当前时间：" + now.Format("2006-01-02 15:04:05 -07:00"))
    return b.String(), nil
```

> **消费方时序**：本 US 的 `SectionOne` **无生产调用方**——第一个生产消费者是 US-4 的 `PromptBuilder`（每次运行组装一次快照、运行内冻结；§3.5 占位点）。US-3 的验收由 harness 直接调用同一条拼装路径断言（四要素**逐位置**），保证"交付即可用"，而不是"到 US-4 才第一次被编译进来"（与 US-2 装配函数同一口径）。

#### 流程 C — `profile` 命令组（`internal/cli`）

```text
basalt profile list                     # 轻路径：只读文件系统；不开库、不取锁、不做恢复
  wsRoot = <WorkDir>/.basalt
  ├─ 未初始化（.basalt/ 或 agents/ 缺失）→ 失败：点名路径 + 指引 basalt init（与 provider list 同为「点名型」）
  ├─ LoadConfig(<WorkDir>/basalt.yaml)  # 命令面自己读配置（cli 的既有职责，US-1 / US-2）
  │     缺失 → providers = []，输出明示「未发现配置——provider 注册判定按未注册处理」，**不**计失败
  │     存在但无效 → 原错误上抛（失败）
  ├─ entries = deps.Agents.List(wsRoot, providers)     # 适配器转调 AgentLoader.LoadAll（同一份代码）
  └─ 渲染：逐条 registered=true/false + provider/model + 未注册原因；汇总行；exit 0

basalt profile show <name>              # 轻路径：同一结果集过滤；name 不在目录清单 → 失败点名
basalt profile create <name>            # 写模式：deps.Agents.Create(wsRoot, name, providers)
                                        #   选中 provider = providers[0]（无配置 → 占位文本）——输出**明示选中了谁**
                                        #   → workspace.CreateAgent（取锁 → 建目录 → 写模板 → 释放；失败清理本次残留）
basalt profile delete <name>            # 写模式：deps.Agents.Delete(wsRoot, name, now)
                                        #   now 由 cobra 入口传 time.Now()（时间显式传入——归档名含时间戳，§2.4 注 11）
                                        #   → workspace.DeleteAgent（取锁 → 单次 rename 入 archive/ → 释放）
```

```go
// 伪代码（展示层与装载分离，供注入判卷——与 provider list 的分层同形态）
func renderProfileList(out io.Writer, entries []core.AgentEntry):
    for _, e := range entries:
        if e.Registered: fmt.Fprintf(out, "  %s：registered=true（provider %s / 模型 %s）\n", e.Name, e.Profile.Provider.Name, e.Profile.Provider.Model)
        else:            fmt.Fprintf(out, "  %s：registered=false——%s\n", e.Name, e.RejectReason)
    fmt.Fprintf(out, "汇总：%d 个 Agent 目录（已注册 %d）\n", len(entries), registeredCount(entries))

func runProfileCreate(out io.Writer, deps Deps, name string) error:
    providers := deps 已装载的 provider 名集（缺失配置 → 空）
    if len(providers) > 0: fmt.Fprintf(out, "已选择 provider：%s（配置首项）\n", providers[0])
    else:                  fmt.Fprintln(out, "未发现 provider 配置——模板里的 provider.name 是占位文本，补齐配置前该 Agent 不会注册")
    created, err := deps.Agents.Create(wsRoot, name, providers)     # 错误原样上抛（拒锁 / 已存在 / name 非法）
    if err != nil: return err
    for _, rel := range created: fmt.Fprintf(out, "  创建：%s\n", rel)
    return nil
```

#### 流程 D — 实例锁（`internal/workspace`；R1 裁决归属已落案）

```text
AcquireInstanceLock(root)                # 非阻塞；锁文件 root/basalt.lock（契约 §4.3.1 字面量）
  ├─ 打开（无则创建）锁文件——O_CREATE|O_RDWR，**不带 O_TRUNC**（取锁前不截断——竞争方可能正在读诊断）
  ├─ 只锁 offset 0 起的 1 字节                       # 锁区就这 1 字节
  │     Windows：windows.LockFileEx（golang.org/x/sys/windows；syscall 无此符号——US-1 §6 读数）
  │     类 Unix：flock(LOCK_EX|LOCK_NB)（golang.org/x/sys/unix）
  ├─ 取到 → WriteAt(诊断记录, 4096)                  # 诊断在**锁定区之外**的固定偏移（见下）
  │        诊断写失败 → **先释放句柄**（Release）再返回错误——不得留下"持锁但无诊断"的句柄
  │        → 返回句柄（调用方 defer Release）
  └─ 取不到（ERROR_LOCK_VIOLATION / EWOULDBLOCK）
        → 另开一个**免锁**读句柄读诊断区（锁定区外可读——Windows 排他锁只挡锁定区）
        → *LockBusyError{pid, startedAt}（文案点名；诊断缺失如实说明）
        → 调用方（CreateAgent / DeleteAgent）把错误原样上抛：**拒绝启动**，不做任何写入

Release()                                # 幂等：重复调用 no-op 返回 nil（sync.Once 或标志位）
  ├─ 只做两件事：解锁 + 关闭句柄
  └─ **不删除锁文件**（契约 §4.3.1：锁文件可留在磁盘——互斥由内核锁承担；删除会引入
     "解锁后删文件"竞态：后到者可能锁在已被 unlink 的 inode 上）
```

> **诊断布局为什么必须在锁定区之外**：Windows 的 `LockFileEx` 排他锁**阻止其他句柄读取锁定区**——若诊断与锁区重叠，拿不到锁的进程恰好读不到"是谁占着"（正是要它的那一刻）。契约 §4.3.1 明确 `instance_id` / `pid` / 心跳**只作诊断**：布局 = 锁区 `[0, 1)`；诊断区自 offset **4096** 起、定长、字段序固定（同一时刻只有持锁者写同一位置，无写竞争）。**不得**据心跳过期抢锁、**不得**用 exists 判据（契约 §4.3.1 原文；US-1 §6 探针读数）。锁的生命周期由内核决定（进程退出即释放）——本 US 只做 `Release` 的显式提前释放，**不做**恢复扫描。

### 2.4 差异裁决注（技术方案未逐字段规定处的解读与出处；不引入文档之外的概念）

**注 1 —— 实例锁的落位（R1 已裁决，归属落案）**：编程指南 §4.3 把「`create` / `delete` 取实例锁、被活实例持有时拒绝启动」写进 **US-3** 的交付面；而 US-1 颗粒度文档 §5 / §6（「实例锁与崩溃恢复的**实现**归 US-4」）、US-2 颗粒度文档 §5（同句）、起步手册 §4「待落位」均记「实现归 US-4」。**用户 2026-10-16（R1）裁决**：同意**最小面前移**——US-3 落写模式命令准入的最小面（锁本体：取 / 拒 / 释放 + 幂等语义 + 诊断布局 + `create` / `delete` 接线），**US-4 接重路径接线**（取锁 → 写模式打开 → 显式迁移 → 恢复）**与恢复扫描**。「US-3 不取锁交付」的备选**已删除**（不再保留为分支方案）。已验收的历史工件（US-1 / US-2 颗粒度文档、起步手册）**不回改**——调整登记在本文与 §5 末行，US-4 规格引用本登记。

**注 2 —— 注册判定的输入是显式参数，不是适配器内部再读配置**：契约 §2.1 规则 2 需要"已注册 provider 表"。`workspace` **不得** import `cli`（依赖方向），故 `AgentLoaderOptions.Providers []string` 由调用方注入；命令面上该名集由 `cli` 自己经 `LoadConfig` 取（`cli` 的既有职责），经端口参数 `List(root, providers)` 传入。**不得**让适配器另读一次配置——同一文件两次读取正是编程指南 §7 陷阱 39 的根因形态。

**注 3 —— `AgentEntry` 的两段语义**：`Profile` 非 nil ⇔ frontmatter **解析成功**；`Registered` ⇔ 八条校验**全过**（+ temperature 字段约束）。未注册但解析成功的 Agent 仍把 `Profile` 挂给 `list` / `show` 展示声明面（provider / model 可见），`RejectReason` 非空。`SectionOne` 只接受 `Registered` 的条目——未注册的 Agent 不得组装系统提示词（契约 §2.1「整只不注册」的执行面）。

**注 4 —— 段一渲染模板为本文口径，要素与顺序有权威出处**：`identity.prompt`「拼在系统提示词开头」/ `identity.agent_name`「注入提示词的自我称谓」（契约 §2）、段一组合式（技术方案 §4.2）、卡片三要素与真路径字面量（§11.3）、日期时间在末尾（§8.3）都有出处；**逐字排版（分隔符、`##` 小节标题、`你的名字是「…」。` 一行）无权威字面量**——本文给出并**逐字冻结**在实现与 §4.3 用例里（改措辞 = 改测试）。Compatibility §5.1 不比对提示词正文，故排版自由不影响兼容判据。

**注 5 —— 未声明键按宽容解析 + 逐条 WARN（R1 裁决；严格备选未采纳）**：YAML 里出现字段表之外的键 → **忽略但可见**（不拒载、每条一条 WARN，`detail` 记键路径，**不打印值**；路径集先排序后输出）。出处解读：Compatibility §2.2「共有参数逐字一致 + Basalt 新增参数不改变共有参数含义」的**前向兼容精神**——基线（NivroOS）先于本仓演进协议，严格模式会让"基线新增可选项"升级为"双底座不兼容"（严格备选 `goccy/go-yaml` 的 `yaml.DisallowUnknownField()` 可用，但**未采纳**）。"不打印值"的理由与陷阱 40 同源：未知键的值是**我们还没理解**的输入，落进日志等于把未审计内容写进审计外的第二份记录。本裁决**只针对 AGENT.md**（共享协议面）；`basalt.yaml` 的未知键口径以 `cli` 现有实现为准，本文不改判。

**注 6 —— `CRON_TZ=` 前缀的转换时机、公共子集与 5 段固定文案（R1 后重写）**：技术方案 §8.5 写「`cron.NewParser(...)` 解析后给表达式加 `CRON_TZ=<zone>` 前缀再 `Schedule`」，契约 §3.1 写「**装载时**把 `zone` 字段转成前缀」——本文解读不变：**装载层交付规范化函数 `CronSpec`**（校验与对拍都用它，保证与 US-10 的注册形态同一份来源），**调度注册**（`Schedule` 调用本体）归 US-10。**robfig v3.0.1 源码实测（本轮逐行核实，纠正上一稿的一处错误认知）**：`Parse` 缺省时区 `var loc = time.Local`；字段数错误原文 `expected exactly %d fields, found %d: %s`；**`L` / `W` / `#` 零命中**（不支持）；dow `bounds{0, 6}`（`7` 同样拒）。⇒ 本仓预检**不是**为了拦 robfig 会放行的写法（上一稿的措辞有误），而是为了：① 报出契约固定的句子；② 让公共子集的拒绝有**本仓统一**的类别与细节（用户看到的分类由我们定，不是库的）。`id` 占用：各 Agent 的 `schedules` 按"目录名字节序 + Agent 内声明序"遍历，`claim` 表**先到先得**——**但只有完整校验通过的 Agent 才占用**（`validateSchedules` 只读 `claim` + 局部集，`LoadAll` 在注册成功分支一次性合并；被拒的 Agent 不占用任何 id——R1 裁决）。**派生只在"恰一个任务"时发生** ⇒ 同 Agent 内不存在"显式 vs 派生"撞车；该形态只在跨 Agent 出现（§2.3 流程 A 末注）。

**注 7 —— 规则 6 的两半条与 WARN 字面量的用法**：契约 §2.1 规则 6 的完整要求是「名字在 `mcp_servers.yaml` 里**且连接成功**」，而编程指南 §4.4 的 US-7 条目写「连接失败不中断启动（WARN + 跳过该 server）→ 依赖它的 Agent 因'工具解析不到'整只不注册」。本文按契约表格 header 的语义执行**存在性半条**：名字不在顶层键内 → 该 Agent **整只不注册**，WARN 字面量 `mcp server skipped`（名字进 `detail`）；「连接成功」半条随 US-7 落位时按其文档 + 契约再裁——**本 US 不得**预设连接语义。

**注 8 —— 夹具内联，`testutil` 不新增公开 API**：本 US 的测试夹具（`.basalt/agents/<name>/AGENT.md` + `skills/<name>/SKILL.md` + bootstrap 文件 + `mcp_servers.yaml`）由各测试包的**非导出** helper 写入 `t.TempDir()`（家族：`writeCompatSampleWorkspace` / `writeWorkspaceWithSchedule` / `writeAgentFrontmatter` / `writeFileInRoot` / `writeMCPServers` / `loadOnlyAgent` / `bufferedLogger`）；不新增 `testutil` 公开类型（US-2 的 `FakeProvider` 有「随接口落位」的明文授权，本 US 无对应授权——不发明测试概念）。若 `/module-dev 3` 实测出现三处以上重复夹具，再提给用户裁决抽取。

**注 9 —— frontmatter 的解析 DTO、缺省填充与 temperature 的执行时点（R1 新增）**：字段表到 `Profile` 的映射**必须显式**——Go 的默认字段名匹配对 `agent_name` / `mcp_servers` / `max_iterations` / `max_history_turns` **一个都对不上**（大小写与下划线都不匹配），故 `workspace` 持有私有 DTO（`yaml:` 标签逐字对齐契约 §2，见 §3.1）；`core.Profile` 不加标签（契约层不绑序列化库——`core` 不得 import 第三方，宪法·架构约束）。**缺省填充只补"未声明"**：`channels` 未声明 → `[{name: cli}]`、显式 `[]` → 空；`settings.*` 未声明 → 10 / 20、显式 `0` 保留为 `0`（**不得**写 `if v == 0 { v = 10 }`——那正是丢失"显式零值"的形态）；`identity.agent_name` 未声明或空白 → `name`；`zone` 未声明或空白 → `UTC`。`provider.temperature` 的 **0–2 范围在装载期执行**（越界与非有限值 NaN / ±Inf 都拒，`reason` = `invalid provider.temperature`）：契约 §2 已把 0–2 写为字段约束，八条校验表未列该项——按"契约说过的字段约束在最近可见点执行"处理：不执行等于文档说了不算，且越界值要到 US-4 构造请求才炸，装载期才是运维看得见的位置。

**注 10 —— 段一「当前时间」的格式与时区（R1 修正）**：格式 `2006-01-02 15:04:05 -07:00`；**缺省 UTC**——`now` 由调用方显式传入，传什么时区就渲染什么（UTC 的 `now` → `+00:00`）；需要地方时间（如定时任务在 `Asia/Shanghai` 触发）时由调用方**显式**取 IANA 时区构造 `now`，**不得**使用进程本地时区（`time.Local` 是禁用词，§2.2 约束 5）；**输出包含时区偏移**——只有偏移能让段一里的时间无歧义（否则"09:30"是哪个 09:30 全靠上下文猜）。上一稿所写的「US-4 装配用进程本地时区」**已删除**（与约束 5 的禁用词冲突）。

**注 11 —— `delete` 归档的时间戳来源与移动语义（R1 裁决）**：归档名 `<name>-<YYYYMMDD-HHMMSS>`（DA §5.14 时间戳唯一化）⇒ 时间戳进入**持久副作用**，故 `DeleteAgent(root, name, now)` 的时间**显式传入**（§2.2 约束 8 / §4.1；CLI 入口由 cobra 层传 `time.Now()`，`runProfileDelete(out, deps, name, now)` 保持可判卷）。**撞名不覆盖**：同一秒内的二次删除 → 拒绝并点名目标路径——Unix 的 `rename(2)` 会**静默替换空的同名目录**，故"不覆盖"**必须**靠显式存在检查，不得指望 rename 的语义。**移动 = 单次 `os.Root.Rename`**（同一文件系统内原子：要么成功、要么源目录原样）——**不得**"复制 + 删除"，**不得**"先删源后建目标"（归档失败必须保留原目录）。

**注 12 —— AGENT.md 的 YAML 错误文案压缩口径**：与 `cli.yamlErrorSummary` **同口径**（位置取自 `yaml.Error` 接口的结构化字段 + 自产类别；库文案一个字不外传——陷阱 40 的实测理由在 `cli` 侧已有一份：库文案会带出错处的源码摘录）。为什么不在同一个函数里：`workspace` **不得** import `cli`（依赖方向），`core` 零语句不能承载函数——**两处各一份小函数是本 US 的既知重复**，判据（不外传库文案）在两侧各有用例（§3.2）；若 US-4 / US-11 出现第三处，抽取新包的提议交用户裁决（**不擅自新建包**）。

**注 13 —— 技能实体的查找面与技能名的形态（R2 裁决，评审回合 P1）**：技能实体的查找与卡片读取一律经**技能库根**（`skills/` 子目录自己的 `os.Root`）——`missingSkill` / `cardsFor` / `skillCardFor` 收的是**库根**句柄，不是工作区根；名字另**必须**是单个目录名（非空、非 `.` / `..`、不含 `/` `\` 与 NUL——与 `create` / `delete` 的 `ValidateAgentName` **共用同一份判据**）。理由：`skills: [../agents/<name>/skills/secret]` 出得了工作区级约束、出不了工作区，但经 `path.Join` 清洁后**正好命中 Agent 本地 `skills/` 目录里的实体**——它的 description 会随 L1 卡片进段一，卡片真路径也会被清洁成技能库之外的路径（`.basalt/agents/…/SKILL.md`）。越界名按「没有实体」判（`reason` 仍取契约字面量的 `skill not found: <name>`，**不新增字面量**），且**不做任何路径拼接**——"恰好命中"不得让整只 Agent 注册成功。`CronSpec` 同源的教训（注 14）适用此处：判定面在前，文件系统的反馈不得进入判定。

**注 14 —— cron 的时区只有一个来源（R2 裁决，评审回合 P2）**：`zone` 字段是**唯一**的时区来源；`cron` 自带 `TZ=` / `CRON_TZ=` 前缀在装载期**拒**（`reason` 恒为 `invalid schedule`）。理由：此前装载期按「剥掉前缀再数段」放行，出口 `CronSpec` 又追加一次前缀 ⇒ 产出 `CRON_TZ=UTC CRON_TZ=Asia/Shanghai 0 0 8 * * *`（robfig 随后解析失败）——**"过了校验"与"注册时炸"各说各话**。两条约束同时落地：① 拒绝是**一处判据两处使用**（装载期与 `CronSpec` 共用同一条常量 `cronTimezonePrefixRule`，不得各写一份）；② 语法校验**解析的是 `CronSpec` 的规范化结果**，不再解析原始串——校验与出口从此是同一份表达式（`zone` 非法时 robfig 的 `TZ=` 解析同样走 `time.LoadLocation`，失败原文与 §6 固定口径 5 的 `zone` 行一致——本轮用例实测）。

**注 15 —— bootstrap 与 delete 的条目类型核对（R2 裁决，评审回合 P2）**：`bootstrap` 声明的条目**必须存在且是普通文件**；`delete` 的源条目**必须是有类型的目录**（`Lstat`：符号链接不跟随）。两处都用契约既有字面量承载（`bootstrap file missing` / `待归档条目不是目录：<路径>（<类型>）`——后者是自产文案，登记进 §6）。理由：目录形态的 `bootstrap` 此前要拖到段一读取时才失败（错的阶段、错的报错面），普通文件则会被"归档"进 `archive/`（把运维的手记挪走）。`os.Root.Stat` 跟随软链接但**出不了工作区根**（越界在根上被挡），链接指向根内普通文件时按普通文件放行——读取面读到的正是同一份内容。

## 3. 交付物清单

### 3.1 代码（类级粒度 + 关键方法签名；不写方法体）

**`internal/core/profile.go`（新增；声明层——零可执行语句，与 US-2 同纪律）**

```go
// Profile 是 AGENT.md frontmatter 的解析结果；字段逐字对齐 docs/contracts/agent-md.md §2 字段表。
// **不带 yaml 标签**——映射由 workspace 的私有 DTO 显式完成（§2.4 注 9）。
type Profile struct {
    Name        string      // name（必填；等于目录名）
    Description string      // description
    Identity    Identity    // identity.*
    Provider    ProviderRef // provider.*
    Tools       []string    // tools（不声明 = 空池）
    Skills      []string    // skills（声明即绑定；元素 = 技能库目录名）
    MCPServers  []string    // mcp_servers（元素 = mcp_servers.yaml 里的 server 名）
    Bootstrap   []string    // bootstrap（相对工作区根）
    Channels    []Channel   // channels（未声明 → [{name: cli}]；显式 [] → 空）
    Settings    Settings    // settings.*
    Schedules   []Schedule  // schedules（见 §3 契约）
}

type Identity struct {
    AgentName string // identity.agent_name；缺省 = name（解析后必非空）
    Prompt    string // identity.prompt；缺省空
}

type ProviderRef struct {
    Name        string   // provider.name（必填；必须已注册）
    Model       string   // provider.model（必填；空白即拒；占位符「请在此填写模型名」是普通文本、天然放行）
    Temperature *float64 // provider.temperature；nil = 未声明（供应商缺省）；显式值 0–2 且有限（§2.2 约束 15）
}

type Channel struct {
    Name   string         // channels[].name
    Config map[string]any // channels[].config（技术方案 §11.2 样例形态；本 US 只解析不校验——契约 §2.1 八条无此规则）
}

type Settings struct {
    MaxIterations   int // settings.max_iterations；缺省 10（显式 0 保留为 0——§2.4 注 9）
    MaxHistoryTurns int // settings.max_history_turns；缺省 20（同上）
}

// Schedule 是 schedules[] 的一项；Zone 解析后必非空（缺省已填 "UTC"——不得回落进程本地时区）。
type Schedule struct {
    ID      string // id；单项可缺省（派生 <agent>-1，仅当恰一个任务）；≥2 项每项必须显式
    Cron    string // cron；6 段（秒 分 时 日 月 周）
    Zone    string // zone；缺省 "UTC"；"Local" 显式拒
    Message string // message（必填）
}

// AgentEntry 是一个 Agent 目录的装载结果——注册成功与失败**都有一条**（失败不静默消失）。
type AgentEntry struct {
    Name         string   // 目录名（= Agent 名）
    Registered   bool     // 八条校验全过（+ temperature 字段约束）
    Profile      *Profile // frontmatter 解析成功时非 nil（未注册也可非 nil——供 list / show 展示，见 §2.4 注 3）
    RejectReason string   // Registered == false 时非空：reason（契约 §2.1 字面量原样）+ detail 的人读合并
}
```

**`internal/workspace/agent.go`（新增）**

```go
// AgentLoaderOptions 是装载器的显式依赖面（宪法 VII：不塞 context、不用包级变量）。
type AgentLoaderOptions struct {
    Providers []string      // 已注册 provider 名（由调用方经 LoadConfig 取得——§2.4 注 2）
    Logger    *slog.Logger  // 点名 WARN 的出口；nil 时用 slog.Default
}

// AgentLoader 扫描 .basalt/agents/ 的每个子目录：解析 → 校验 → 派生 Profile / 拒绝（技术方案 §8.2）。
type AgentLoader struct{ /* unexported: providers, logger */ }

func NewAgentLoader(opts AgentLoaderOptions) *AgentLoader

// LoadAll 装载全部 Agent 目录；单只失败不拖垮整批（其余照常）。不提供 LoadOne——`show` 复用本
// 结果过滤（技术方案 §8.7「不得为同一语义写两份实现」）。
// 扫描与读取一律经 os.Root；扫描序显式排序（§2.2 约束 12）。
func (l *AgentLoader) LoadAll(root string) ([]core.AgentEntry, error)
```

```go
// CronSpec 把 zone 字段转成 CRON_TZ= 前缀形态（契约 §3.1；调度注册归 US-10，见 §2.4 注 6）。
func CronSpec(s core.Schedule) (string, error)
```

```go
// frontmatterDTO 是 AGENT.md frontmatter 的解析 DTO（workspace 私有）：显式 yaml 标签、
// 键名 snake_case **逐字**对齐契约 §2；`*T` 区分"未声明"与"显式零值"（§2.2 约束 15 / §2.4 注 9）。
// 解析后由 intoProfile 显式映射到 core.Profile（core 侧不带标签——契约层不绑序列化库）。
type frontmatterDTO struct {
    Name        string         `yaml:"name"`
    Description string         `yaml:"description"`
    Identity    identityDTO    `yaml:"identity"`
    Provider    providerDTO    `yaml:"provider"`
    Tools       []string       `yaml:"tools"`
    Skills      []string       `yaml:"skills"`
    MCPServers  []string       `yaml:"mcp_servers"`
    Bootstrap   []string       `yaml:"bootstrap"`
    Channels    []channelDTO   `yaml:"channels"`     // nil（未声明）→ [{name: cli}]；[] → 空
    Settings    settingsDTO    `yaml:"settings"`
    Schedules   []scheduleDTO  `yaml:"schedules"`
}

type identityDTO struct {
    AgentName string `yaml:"agent_name"`   // 未声明或空白 → 回填 name
    Prompt    string `yaml:"prompt"`
}

type providerDTO struct {
    Name        string   `yaml:"name"`
    Model       string   `yaml:"model"`
    Temperature *float64 `yaml:"temperature"` // nil = 未声明；显式值须 0–2 且有限
}

type channelDTO struct {
    Name   string         `yaml:"name"`
    Config map[string]any `yaml:"config"`     // 自由映射：内容归渠道实现，未知键扫描不进入（§3.1 约束在附录 A 行 13）
}

type settingsDTO struct {
    MaxIterations   *int `yaml:"max_iterations"`    // nil → 10；显式 0 保留
    MaxHistoryTurns *int `yaml:"max_history_turns"` // nil → 20；显式 0 保留
}

type scheduleDTO struct {
    ID      string `yaml:"id"`
    Cron    string `yaml:"cron"`
    Zone    string `yaml:"zone"`      // 未声明或空白 → "UTC"；"Local" 拒
    Message string `yaml:"message"`
}
```

> 非导出辅助（围栏切分、未知键扫描、`intoProfile`、缺省填充、`missingSkill`、`validateSchedules`、`reject`、`frontmatterErrorSummary`、`os.Root` 内读写小函数）随实现落地，不计入本清单——对外概念以本节为准。

**`internal/workspace/skill.go`（新增；内部形态）**

```go
type skillCard struct {
    name        string // SKILL.md frontmatter name；缺失回落技能目录名（声明即绑定以目录名为准）
    description string // SKILL.md frontmatter description；缺失为空串（契约 §2.1 无此校验规则——不发明）
    path        string // 逐字 .basalt/skills/<name>/SKILL.md（技术方案 §11.3 字面量）
}
```

**`internal/workspace/context_loader.go`（新增）**

```go
// ContextLoader 组装系统提示词段一（技术方案 §8.3：本 US 只交付段一；快照与其余三段归 US-4）。
type ContextLoader struct{ /* unexported: logger */ }

func NewContextLoader(logger *slog.Logger) *ContextLoader

// SectionOne 组装段一：identity.prompt + 自我称谓 + AGENT.md 正文 + Bootstrap 文件 +
// 技能卡片 L1 + 当前日期时间（格式 2006-01-02 15:04:05 -07:00——缺省 UTC、含偏移，§2.4 注 10）。
// now 显式传入；每次调用重读文件（无缓存——§2.2 约束 7）；任一步读取失败 → 返回错误（§2.2 约束 6）。
func (c *ContextLoader) SectionOne(root string, entry core.AgentEntry, now time.Time) (string, error)
```

**`internal/workspace/agent_files.go`（新增）**

```go
// ValidateAgentName 是 create / delete **共用**的名称校验（同一函数，不得各写一份）：
// 非空、单个目录名——不含路径分隔符（/ 与 \）、不为 "." / ".."、不含 NUL。
// 导出是因为它同时是端口层（cli 命令面）的前置判据。
func ValidateAgentName(name string) error

// CreateAgent 在 root/agents/<name>/ 写入最小 AGENT.md 模板（写模式：内部先取实例锁）。
// 前置：ValidateAgentName；agents/<name> 已存在 → 拒绝（**不覆盖**）。拒绝路径零写入。
// 失败：建目录后写文件失败 → 清理**本次创建**的目录（只删本次新建物——既有目录一律不动，
//       这一点比"清理干净"更重要）；清理失败 → 错误文案如实点名残留路径（不掩盖）。
// 模板：provider.name 取 providers[0]（无配置 → 占位文本「请在此填写 provider 名」，输出层说明
//       "模板尚未注册"）；provider.model 恒为占位「请在此填写模型名」（§11.5 放行）；tools: []；
//       两个名字经 goccy/go-yaml 编码写入（**不得**字符串插值——名字可含空格 / 冒号 / 中文）。
// created 是本次新落盘的相对路径（供命令输出）。
func CreateAgent(root, name string, providers []string) (created []string, err error)

// DeleteAgent 归档：把 root/agents/<name>/ **单次 rename** 到 root/archive/<name>-<YYYYMMDD-HHMMSS>/；
// 会话 / 记忆 / 审计数据不动（DA §5.14）。写模式：内部先取实例锁；持有实例锁时不存在
// 在跑的 Run 与调度注册，故 §11.4 的「注销定时 → 移出注册 → 取消 Run」三步在 CLI 路径上为空集。
// 归档目标已存在 → 拒绝（**不覆盖**——同一秒内的二次删除不得吃掉已归档的副本，§2.4 注 11）；
// rename 原子：失败即源目录原样保留（**不得**复制 + 删除 / 先删源后建目标）。
// now 显式传入（归档名含时间戳——时间显式传入是可判卷前提，§2.2 约束 8）。
func DeleteAgent(root, name string, now time.Time) (archived string, err error)
```

**`internal/workspace/lock.go` + `lock_windows.go` + `lock_unix.go`（新增；build tag 分文件）**

```go
// InstanceLock 是工作区实例锁（.basalt/basalt.lock，OS 级独占文件锁）的持有句柄；契约 §4.3.1。
// 锁区 = offset 0 起 1 字节；诊断记录（instance_id / pid / 开始时间）写在**锁定区之外**的
// 固定偏移（§2.3 流程 D）——Windows 排他锁阻止其他句柄读锁定区，诊断必须在区外才读得到。
type InstanceLock struct{ /* unexported: file, once */ }

// AcquireInstanceLock 非阻塞取锁：成功返回句柄（defer Release）；被活实例持有 → *LockBusyError。
// 取锁前**不截断**锁文件（O_CREATE|O_RDWR，无 O_TRUNC）；诊断写失败 → **先释放句柄**再返回错误。
func AcquireInstanceLock(root string) (*InstanceLock, error)

// Release 幂等（重复调用 no-op 返回 nil）：只解锁 + 关闭句柄，**不删除锁文件**（契约 §4.3.1）。
func (l *InstanceLock) Release() error

// LockBusyError 携带诊断信息（pid / 开始时间）；诊断缺失时 Error() 如实说明而不是编造。
type LockBusyError struct{ PID int; StartedAt time.Time; Detail string }
func (e *LockBusyError) Error() string
```

**`internal/workspace/templates.go`（改造）**：新增 Agent 模板构造（与既有五模板同风格；frontmatter 经 `yaml.Marshal` 编码，**不插值**——见 `CreateAgent` 注释）。

**`internal/cli/profile.go`（新增）**

```go
func newProfileCommand(deps Deps) *cobra.Command       // 命令组：可运行父命令 + 未知子命令非零（与 provider 组同形态）
func newProfileListCommand(deps Deps) *cobra.Command
func newProfileShowCommand(deps Deps) *cobra.Command
func newProfileCreateCommand(deps Deps) *cobra.Command
func newProfileDeleteCommand(deps Deps) *cobra.Command   // cobra 层取 time.Now() 传 runProfileDelete（§2.4 注 11）

func runProfileList(out io.Writer, deps Deps) error
func runProfileShow(out io.Writer, deps Deps, name string) error
func runProfileCreate(out io.Writer, deps Deps, name string) error
func runProfileDelete(out io.Writer, deps Deps, name string, now time.Time) error  // 时间显式传入（可判卷）

// 展示层与装载分离，供注入判卷（与 provider list 的 renderProviderList 同形态）
func renderProfileList(out io.Writer, entries []core.AgentEntry)
func renderProfileShow(out io.Writer, entry core.AgentEntry)
```

**`internal/cli/ports.go`（改造）**

```go
// Agents 是命令面对 Agent 目录的端口：list / show 读（轻路径），create / delete 写（写模式，取实例锁）。
// providers 是注册判定的显式输入（命令面经 LoadConfig 取得——§2.4 注 2）；适配器不另读配置。
// delete 的 now 由命令层传入（归档名含时间戳——§2.4 注 11）。
type Agents interface {
    List(root string, providers []string) ([]core.AgentEntry, error)
    Create(root, name string, providers []string) (created []string, err error)
    Delete(root, name string, now time.Time) (archived string, err error)
}
```

**`internal/cli/deps.go`（改造）**：`Deps` 新增 `Agents Agents` 字段（注释指明写模式取锁语义）。

**`internal/cli/root.go`（改造）**：注册 `newProfileCommand(deps)`（注册点注释同步）。

**`internal/app/ports.go`（改造）**：新增 `agentsPort{WorkDir string; Logger *slog.Logger}` 适配器（三方法转调 `workspace.AgentLoader` / `CreateAgent` / `DeleteAgent`；错误原样上抛）。

**`internal/app/run.go`（改造）**：装配 `Deps{... Agents: agentsPort{WorkDir: workDir, Logger: logger}}`。

### 3.2 测试（file → cases；函数名英文，中文验收点以注释保留）

| 文件 | 用例（函数名） | 验收点 |
| --- | --- | --- |
| `internal/workspace/agent_test.go` | `TestAgentLoader_LoadsCompatSampleAgent` / `TestAgentLoader_RejectsNameMismatch` / `TestAgentLoader_RejectsUnregisteredProvider` / `TestAgentLoader_RejectsBlankModel` / `TestAgentLoader_AcceptsPlaceholderModel` / `TestAgentLoader_RejectsMissingSkill` / `TestAgentLoader_RejectsUnconfiguredMCPServer` / `TestAgentLoader_RejectsMissingBootstrapFile` / `TestAgentLoader_IgnoresLocalSkillsDirectory` / `TestAgentLoader_OneRejectedAgentDoesNotBlockOthers` / `TestAgentLoader_ParsesAllFrontmatterFields`（非缺省值：`agent_name` / `mcp_servers` / `max_iterations` / `max_history_turns` / `temperature: 0`）/ `TestAgentLoader_FillsDocumentedDefaultsWhenAbsent`（缺省 10 / 20 / `[{name: cli}]` / nil temperature）/ `TestAgentLoader_EmptyChannelsStaysEmpty` / `TestAgentLoader_ToleratesUnknownFrontmatterKeys` / `TestAgentLoader_UnknownKeyWarnNamesPathWithoutValue` / `TestAgentLoader_RejectsOutOfRangeTemperature` / `TestAgentLoader_RejectsNonFiniteTemperature` / `TestAgentLoader_ParseErrorOmitsSourceExcerpt` | A1 / A2 / A3 / A13 / A15 |
| `internal/workspace/schedule_test.go` | `TestAgentLoader_RejectsFiveFieldCron` / `TestAgentLoader_CronSyntaxErrorCarriesParserText` / `TestAgentLoader_DefaultZoneIsUTC` / `TestAgentLoader_InvalidZoneRejected` / `TestAgentLoader_RejectsLocalZone` / `TestAgentLoader_RejectsBothDomAndDowRestricted` / `TestAgentLoader_RejectsSevenInDayOfWeek` / `TestAgentLoader_RejectsUnsupportedTokens`（`L` / `W` / `#`）/ `TestAgentLoader_RejectsBlankScheduleMessage` / `TestAgentLoader_ScheduleIDSingleTaskDerivesAgentSuffix` / `TestAgentLoader_ScheduleIDRequiredFromTwoTasks` / `TestAgentLoader_ScheduleIDDuplicateWithinAgentRejects` / `TestAgentLoader_ScheduleIDCollisionRejectsLaterAgent` / `TestAgentLoader_RejectedAgentClaimsNoScheduleIDs`（**整只提交反例**）/ `TestAgentLoader_ExplicitIDCollidesWithDerivedID` / `TestCronSpec_PrefixesEveryZone` | A4 |
| `internal/workspace/context_loader_test.go` | `TestContextLoader_SectionOneCarriesAllFourParts`（位置断言）/ `TestContextLoader_SectionOneEndsWithTimestamp` / `TestContextLoader_OnlyDeclaredSkillsReachSectionOne` / `TestContextLoader_RefusesUnregisteredAgent` / `TestContextLoader_RereadsBetweenCalls`（改文件后第二次调用取新内容——无缓存语义）/ `TestContextLoader_FailsWhenBootstrapFileDisappears` / `TestContextLoader_FailsWhenSkillFileDisappears` | A5 / A6 |
| `internal/workspace/agent_files_test.go` | `TestCreateAgent_WritesLoadableTemplate`（占位模型放行——§11.5）/ `TestCreateAgent_RefusesExistingDirectory`（零写入；**既有目录及内容逐字节保留**）/ `TestCreateAgent_RejectsInvalidName`（零写入；与 delete 同判据）/ `TestCreateAgent_EncodesNamesThroughYAML`（provider 名含空格 / 冒号仍可解析回原名）/ `TestCreateAgent_RefusesWhenInstanceLockHeld`（拒锁零业务写入）/ `TestDeleteAgent_ArchivesWithoutTouchingData` / `TestDeleteAgent_RefusesMissingAgent` / `TestDeleteAgent_RejectsInvalidName` / `TestDeleteAgent_RefusesCollidingArchiveTarget`（`now` 固定 → 目标已存在 → 拒绝且**源目录仍在**——"不得先删后移"的可判据形态） | A8 / A9 |
| `internal/workspace/lock_test.go` | `TestInstanceLock_HelperProcessMutualExclusion`（**主判据**：跨进程）/ `TestInstanceLock_HelperProcessReacquiresAfterHolderKilled`（强杀后重取）/ `TestInstanceLock_SecondAcquireFailsWhileHeld`（同进程双句柄——**补充**）/ `TestInstanceLock_ReleaseIsIdempotent` / `TestInstanceLock_KeepsLockFileAfterRelease` / `TestInstanceLock_BusyErrorNamesHolder`（诊断在锁定区之外，竞争方可读）/ `TestInstanceLock_ContendedAcquireLeavesLockFileIntact`（竞争失败不改锁文件字节——取锁前不截断的可判据形态）/ `TestInstanceLock_ReadOnlyCommandsDoNotTouchLockFile` | A10 |
| `internal/cli/profile_test.go` | `TestProfileList_RendersRegisteredAndRejected` / `TestProfileList_FailsWhenWorkspaceMissing` / `TestProfileList_WithoutConfigStillListsWithNotice` / `TestProfileShow_UnknownNameFails` / `TestProfileShow_RendersDeclaredFields` / `TestProfileCreate_CommandUsesWriteModePort`（输出明示选中的 provider）/ `TestProfileCreate_WithoutProvidersNotesUnregisteredTemplate` / `TestProfileDelete_CommandUsesWriteModePort`（`now` 注入固定时刻） | A7 / A8 / A9 |
| `internal/cli/ports_test.go`（改造） | 既有依赖方向判据沿用（`profile` 命令走端口即自动受判） | A11 |
| `internal/app/ports_test.go`（改造） | `TestAgentsPort_ListForwardsLoaderEntries` / `TestAgentsPort_CreateAndDeleteReachWorkspace` | A11 |

> 夹具 helper 家族见 §2.4 注 8；helper-process 形态的辅助入口（`TestInstanceLock_HelperProcessMutualExclusion` 与 `TestInstanceLock_HelperProcessReacquiresAfterHolderKilled` 里的子进程分支 + `lockHelperMain`）是**测试内**设施，不是 `testutil` 公开 API。
>
> **R2 追加（评审回合四修复的回归用例，追加登记不改上表）**：`agent_test.go` + `TestAgentLoader_RejectsSkillNameOutsideLibrary`（越界技能名三形态：逃到 Agent 本地 `skills/` / 回环进库 / Windows 分隔符）、+ `TestAgentLoader_RejectsBootstrapDirectory`；`schedule_test.go` + `TestAgentLoader_RejectsCronWithTimezonePrefix`（`CRON_TZ=` 与 `TZ=` 两形态）、+ `TestCronSpec_NormalizedResultCarriesOnePrefix`（规范化结果可解析 + 前缀只一层 + 直接调用方给带前缀输入也拒）；`agent_files_test.go` + `TestDeleteAgent_RefusesNonDirectoryEntry`、+ `TestDeleteAgent_RefusesSymlinkEntry`；**新增文件** `agent_unix_test.go`（`//go:build unix`）+ `TestAgentLoader_RejectsBootstrapSpecialFile`（FIFO——特殊文件里唯一可程序化造出的形态；Windows 上不参与编译，该平台的目录形态由 `TestAgentLoader_RejectsBootstrapDirectory` 覆盖同一处判定）。

### 3.3 配置与工程文件

- `go.mod` / `go.sum`：新增 `github.com/robfig/cron/v3 v3.0.1`（固定技术栈句内，已核实——§2.2 约束 14）；`golang.org/x/sys v0.48.0` 由 indirect 迁入直接 require（锁落位已裁决，**非条件**；**无新增坐标**、版本不变）。
- CI（`.github/workflows/ci.yml`）：**无变更**——但**不得**据此认为"既有门禁覆盖了锁"：CI 的 `test` job 跑在 `ubuntu-latest`（只covers `lock_unix.go` 分支），`build` job 的四平台交叉编译只保证 `lock_windows.go` **能编译**。Windows 分支的**运行**读数由本机（Windows 11）门禁登记（§4.1 / §6）。
- 契约文件：**不新建**——固定字面量一律引用 `docs/contracts/agent-md.md` 与 `run-and-errors.md` §4.3.1，不复制。

### 3.4 表

**无**——本 US 不建表、不写数据库（`profile` 命令面为轻路径与文件写入；`AgentEntry` 的注册结果只走日志与命令输出）。

### 3.5 前序改造点与依赖副作用

| # | 改造点 | 性质 / 软门禁 |
| --- | --- | --- |
| 1 | `internal/cli/ports.go`：新增 `Agents` 端口（三方法，`Delete` 带 `now`） | 接口在消费方定义；改签名需同步 `app` 适配器 |
| 2 | `internal/cli/deps.go`：`Deps` 新增 `Agents` 字段 | 装配层与全部构造 `Deps` 的测试同步 |
| 3 | `internal/cli/root.go`：注册 `newProfileCommand(deps)` | 命令面新增第 3 组子命令；`--help` 文案随动 |
| 4 | `internal/app/ports.go`：新增 `agentsPort` 适配器 | 适配器是唯一允许 import 实现包的地方（US-1 D1 裁决） |
| 5 | `internal/app/run.go`：装配注入 `agentsPort` | 构造参数含 `workDir` / `logger`（显式传递） |
| 6 | `internal/workspace/templates.go`：新增 Agent 模板 | 与既有五模板同风格；经 YAML 编码（不插值）；不覆盖纪律沿用 |
| 7 | `go.mod`：新增 `robfig/cron/v3 v3.0.1`；`x/sys` 迁直接依赖 | 表内坐标直接落地；`x/sys` 无新增坐标（落位已裁决，非条件） |
| 8 | `internal/workspace/doc.go`：包职责句补 AgentLoader / ContextLoader / 实例锁 | 文档同步（非行为） |
| 9 | **占位点（后续模块接入，登记不改写）** | 规则 4 的 tools 门禁 → US-4；规则 6「连接成功」半条 → US-7；段一的运行级快照 / 清单哈希 / 原子发布 → US-4 / US-11（**共享同次读取产物**——§2.2 约束 6）；`schedules` 注册与调度 → US-10；记忆注入（段二）→ US-6；`PromptBuilder` 消费 `SectionOne` → US-4；REST / 管理台的 Agent 动态管理 → US-9 / US-12；实例锁**重路径接线**与恢复扫描 → US-4 |

## 4. 验收测试（Harness）

### 4.1 测试分层标准

- **默认层**（`go test -race`，门禁判卷）：全部用例**不访问真实网络**（本 US 无网络面）；文件系统操作用 `t.TempDir()`；时间一律**显式传入**（`now time.Time` 参数——不得依赖真实当前时间做断言）；跨进程判据用 **helper-process**（exec 自身测试二进制 + 环境变量分派，经子进程 stdout 行同步——**不得**用睡眠等待）。
- **无需集成层**：本 US 不触外部依赖；真实模型 / 真 webhook 不在本 US 范围。
- **平台分支读数分别登记（R1 补强）**：`lock_windows.go` 与 `lock_unix.go` 是**两份实现**——Ubuntu CI 只执行后者；前者在本机（Windows 11）门禁执行并登记读数，CI 只保证其**可编译**（四平台交叉编译）。**不得**笼统写"既有门禁全部覆盖"，也不得因 CI 绿就宣称 Windows 分支已验。
- **覆盖率**：`internal/workspace` 自本 US 起纳入判卷（≥ 50%，宪法·开发工作流「其余」档）；`internal/core` 仍记「无可执行语句」（只放声明，跳过判卷——US-2 同口径）。

### 4.2 测试类 → 验收点映射

| 验收点 | 内容（出处） | 覆盖用例（代表） |
| --- | --- | --- |
| A1 | 一个目录 = 一个 Agent；`AGENT.md` 唯一定义入口；目录名 = `name` | `TestAgentLoader_RejectsNameMismatch`、`TestAgentLoader_ParsesAllFrontmatterFields` |
| A2 | 八条加载期校验（本 US 六条全量 + 两条占位登记）+ temperature 字段约束 | `RejectsUnregisteredProvider` / `RejectsBlankModel` / `AcceptsPlaceholderModel` / `RejectsMissingSkill` / `RejectsUnconfiguredMCPServer` / `RejectsMissingBootstrapFile` / `RejectsOutOfRangeTemperature` / `RejectsNonFiniteTemperature` |
| A3 | 任一不过 = 整只不注册 + 点名 WARN；其余照常 | 各 `Rejects*` 的 `Registered=false` + `RejectReason` 断言；`OneRejectedAgentDoesNotBlockOthers` |
| A4 | schedules：6 段 / 公共子集（dom-dow 互斥、`7`、`L`/`W`/`#`）/ `message` 必填 / zone（缺省 UTC、`Local` 拒）/ id（派生与唯一）/ 占用**整只提交** / 5 段拒（固定句） | `schedule_test.go` 十六例 |
| A5 | 技能声明即绑定；L1 卡片三要素；不预载正文；本地 `skills/` 被无视 | `RejectsMissingSkill`、`OnlyDeclaredSkillsReachSectionOne`、`IgnoresLocalSkillsDirectory` |
| A6 | 段一四要素**顺序**正确、时间收尾；读取失败返回错误 | `SectionOneCarriesAllFourParts`、`SectionOneEndsWithTimestamp`、`FailsWhenBootstrapFileDisappears`、`FailsWhenSkillFileDisappears`、`RefusesUnregisteredAgent`、`RereadsBetweenCalls` |
| A7 | `profile list` / `show` 轻路径（只读、不开库、未注册可见、排序稳定） | `cli/profile_test.go` 前半 |
| A8 | `profile create` 模板（不覆盖、零写入、名字经 YAML 编码、占位符放行加载、输出明示选中 provider） | `WritesLoadableTemplate`、`RefusesExistingDirectory`、`RejectsInvalidName`、`EncodesNamesThroughYAML`、`CommandUsesWriteModePort` |
| A9 | `profile delete` 归档（数据不动；缺目录拒绝；撞名不覆盖且源目录保留） | `ArchivesWithoutTouchingData`、`RefusesMissingAgent`、`RefusesCollidingArchiveTarget` |
| A10 | 写模式命令取实例锁（**跨进程**互斥 / 强杀重取 / 拒锁零写入 / 幂等释放 / 不删锁文件 / 竞争不改文件）；只读命令不取锁 | `lock_test.go` 八例 |
| A11 | 依赖方向：`cli` 不 import `workspace`；`core` 零语句 | 既有 `ports_test.go` 判据 + `app/ports_test.go` 转调 |
| A12 | Compatibility §5.1 样例六条判据（本仓侧可判部分） | `LoadsCompatSampleAgent`（判据 1 / 2 part / 3）、`RejectsFiveFieldCron`（判据 4）、`DefaultZoneIsUTC`（判据 5） |
| A13 | 可观测性：WARN / ERROR 固定键（`agent` / `reason` / `detail`）；未知键 WARN 只记键路径；文案不回显文件内容与库原文 | 各 `Rejects*` 的日志断言（缓冲 handler）+ `UnknownKeyWarnNamesPathWithoutValue`、`ParseErrorOmitsSourceExcerpt`、`IgnoresLocalSkillsDirectory` 的 WARN |
| A14 | 门禁与覆盖率（`make check` 七目标；`workspace` ≥ 50%） | CI / `make check` 人工读数（Windows 分支由本机登记） |
| A15 | frontmatter 映射与缺省填充：显式标签、snake_case、未声明 vs 显式零值、未知键宽容 | `ParsesAllFrontmatterFields`、`FillsDocumentedDefaultsWhenAbsent`、`EmptyChannelsStaysEmpty`、`ToleratesUnknownFrontmatterKeys` |

### 4.3 关键回归测试写出代码（断言逐条对应验收点）

```go
// 中文验收点：Compatibility §5.1 共享样例加载——注册成功、10 个工具名逐字解析、
// 技能声明即绑定、schedules 派生命中；判据 3：从**固定起点** 2026-10-01T00:00:00Z
// 起未来 10 次触发逐条相同（Next 严格晚于传入时刻——robfig v3.0.1 源码实测）。
// Acceptance point: the shared compatibility sample loads, and its cron fires on
// the same ten instants on both platforms from a fixed start.
func TestAgentLoader_LoadsCompatSampleAgent(t *testing.T) {
    root := writeCompatSampleWorkspace(t) // Compatibility §5.1 样例逐字：.basalt/agents/compat-check/AGENT.md
    // + .basalt/{AGENTS.md, SOUL.md, USER.md}（bootstrap 三件）+ .basalt/skills/digest-format/SKILL.md
    loader := NewAgentLoader(AgentLoaderOptions{
        Providers: []string{"deepseek"},
        Logger:    bufferedLogger(t),
    })

    entries, err := loader.LoadAll(root)
    require.NoError(t, err)
    require.Len(t, entries, 1)

    entry := entries[0]
    require.True(t, entry.Registered, "样例必须注册：%s", entry.RejectReason)
    require.NotNil(t, entry.Profile)
    require.Equal(t, "compat-check", entry.Profile.Name)
    require.Equal(t, []string{
        "read_file", "write_file", "list_dir", "shell", "http_get", "http_post",
        "save_memory", "recall_memory", "notify", "manage_schedule",
    }, entry.Profile.Tools) // 判据 2：10 个名字全部命中
    require.Equal(t, []string{"digest-format"}, entry.Profile.Skills)
    require.Len(t, entry.Profile.Schedules, 1)

    sched := entry.Profile.Schedules[0]
    require.Equal(t, "compat-daily", sched.ID)
    require.Equal(t, "Asia/Shanghai", sched.Zone)

    spec, err := CronSpec(sched) // "CRON_TZ=Asia/Shanghai 0 0 8 * * *"
    require.NoError(t, err)
    require.Equal(t, "CRON_TZ=Asia/Shanghai 0 0 8 * * *", spec)

    parsed, err := cron.NewParser(
        cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(spec)
    require.NoError(t, err)

    // 固定起点：2026-10-01T00:00:00Z 恰是当日 08:00 +08:00——Next 严格晚于起点，
    // 故第一次落在次日；daily 08:00 +08:00 == 00:00:00 UTC。
    // Fixed start: the first firing is the NEXT day, because Next() is strictly after t.
    start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
    at := start
    for i := 1; i <= 10; i++ {
        want := time.Date(2026, 10, 1+i, 0, 0, 0, 0, time.UTC) // 08:00 +08:00
        at = parsed.Next(at)
        require.True(t, want.Equal(at), "第 %d 次触发：want %s，got %s", i, want, at)
    }
}
```

```go
// 中文验收点：5 段 cron 拒载，文案**必须**含固定句 "Cron expression must consist of 6
// fields"（Compatibility §5.1 判据 4）；robfig 原文（expected exactly 6 fields, found 5: …）
// 另附 detail，不替代固定句（见 §2.4 注 6）。
// Acceptance point: a five-field cron is rejected with the promised wording.
func TestAgentLoader_RejectsFiveFieldCron(t *testing.T) {
    root := writeWorkspaceWithSchedule(t, "0 8 * * *", "") // 5 段、无 zone
    entries, err := NewAgentLoader(AgentLoaderOptions{Providers: []string{"deepseek"},
        Logger: bufferedLogger(t)}).LoadAll(root)
    require.NoError(t, err)
    require.Len(t, entries, 1)

    entry := entries[0]
    require.False(t, entry.Registered, "5 段 cron 必须整只不注册")
    require.Contains(t, entry.RejectReason, "Cron expression must consist of 6 fields")
}
```

```go
// 中文验收点：删 zone → 按 UTC（Compatibility §5.1 判据 5）——**不得**回落本机时区；
// 判据落在规范化结果上：spec 必须显式带 CRON_TZ=UTC（本机时区若被告状进 spec 即判红）。
// Acceptance point: a missing zone defaults to UTC, never the host time zone.
func TestAgentLoader_DefaultZoneIsUTC(t *testing.T) {
    root := writeWorkspaceWithSchedule(t, "0 0 8 * * *", "") // 6 段、无 zone
    entries, err := NewAgentLoader(AgentLoaderOptions{Providers: []string{"deepseek"},
        Logger: bufferedLogger(t)}).LoadAll(root)
    require.NoError(t, err)
    require.True(t, entries[0].Registered, "缺 zone 不得导致拒载：%s", entries[0].RejectReason)
    require.Equal(t, "UTC", entries[0].Profile.Schedules[0].Zone)

    spec, err := CronSpec(entries[0].Profile.Schedules[0])
    require.NoError(t, err)
    require.Equal(t, "CRON_TZ=UTC 0 0 8 * * *", spec)
}
```

```go
// 中文验收点：cron 公共子集硬规则——**日与周不得同时被限定**（至少一个是裸 *；
// */2、1-15、1,5、单值都算限定）——契约 §3.1。robfig v3.0.1 对 dom / dow 同时被限定的
// 表达式是**接受**的（它不做公共子集约束——本轮源码实测），故本仓预检必须自己拦。
// Acceptance point: dom and dow may not both be restricted.
func TestAgentLoader_RejectsBothDomAndDowRestricted(t *testing.T) {
    cases := []struct {
        name           string
        cron           string
        wantRegistered bool
    }{
        {"both_restricted", "0 0 8 1-15 * mon", false},
        {"dom_step_and_dow_list", "0 0 8 */2 * 1,5", false}, // */2 计为限定
        {"only_dom_restricted", "0 0 8 1-15 * *", true},
        {"only_dow_restricted", "0 0 8 * * mon", true},
        {"dom_step_only", "0 0 8 */2 * *", true},
        {"both_star", "0 0 8 * * *", true},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            root := writeWorkspaceWithSchedule(t, tc.cron, "UTC")
            entries, err := NewAgentLoader(AgentLoaderOptions{Providers: []string{"deepseek"},
                Logger: bufferedLogger(t)}).LoadAll(root)
            require.NoError(t, err)
            require.Len(t, entries, 1)

            entry := entries[0]
            require.Equal(t, tc.wantRegistered, entry.Registered, "cron %q：%s", tc.cron, entry.RejectReason)
            if !tc.wantRegistered {
                require.Contains(t, entry.RejectReason, "日与周不得同时受限")
            }
        })
    }
}
```

```go
// 中文验收点：占用按**整只 Agent** 提交——alpha 的第一条任务先声明 id "x"、第二条非法
// （5 段）；alpha 整只不注册，且 "x" **未被占用**：随后字典序靠后的 beta 显式声明 id "x"
// 必须注册成功（占用的唯一提交点是 LoadAll 的注册成功分支——§2.3 流程 A / §2.4 注 6）。
// Acceptance point: a rejected agent must not hold any schedule id.
func TestAgentLoader_RejectedAgentClaimsNoScheduleIDs(t *testing.T) {
    root := t.TempDir()
    writeAgentFrontmatter(t, root, "alpha", `
name: alpha
provider: {name: deepseek, model: deepseek-chat}
tools: []
schedules:
  - id: x
    cron: "0 0 8 * * *"
    zone: UTC
    message: 早报
  - id: y
    cron: "0 8 * * *"        # 5 段——第二条非法
    zone: UTC
    message: 晚报
`)
    writeAgentFrontmatter(t, root, "beta", `
name: beta
provider: {name: deepseek, model: deepseek-chat}
tools: []
schedules:
  - id: x
    cron: "0 0 9 * * *"
    zone: UTC
    message: 午报
`)

    entries, err := NewAgentLoader(AgentLoaderOptions{Providers: []string{"deepseek"},
        Logger: bufferedLogger(t)}).LoadAll(root)
    require.NoError(t, err)

    byName := make(map[string]core.AgentEntry, len(entries))
    for _, e := range entries {
        byName[e.Name] = e
    }
    require.False(t, byName["alpha"].Registered, "第二条非法 → 整只不注册")
    require.True(t, byName["beta"].Registered,
        "被拒的 Agent 不得占用 id：beta 应能注册（%s）", byName["beta"].RejectReason)
}
```

```go
// 中文验收点：段一四要素齐备且**顺序正确**——比较各标记的位置（只 Contains 的话，
// 正文 / 卡片 / 时间顺序颠倒也能通过）、时间**在末尾**、技能正文（L2）**不得**预载。
// Acceptance point: section one keeps its part order and ends with the timestamp.
func TestContextLoader_SectionOneCarriesAllFourParts(t *testing.T) {
    root := writeCompatSampleWorkspace(t)
    entry := loadOnlyAgent(t, root)
    now := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC) // 缺省 UTC——输出含偏移（§2.4 注 10）

    section, err := NewContextLoader(bufferedLogger(t)).SectionOne(root, entry, now)
    require.NoError(t, err)

    iPrompt := strings.Index(section, "你是一个用于兼容性验证的助手。")              // identity.prompt（样例原文）
    iName := strings.Index(section, "你的名字是「对照小助手」。")                  // 自我称谓（agent_name 样例原文）
    iBody := strings.Index(section, "compat ok")                              // AGENT.md 正文（样例原文）
    iBoot := strings.Index(section, "## AGENTS.md")                           // bootstrap 小节（样例声明）
    iCard := strings.Index(section, "- digest-format：摘要格式统一")              // 卡片 L1（description 夹具自定）
    iPath := strings.Index(section, ".basalt/skills/digest-format/SKILL.md")  // 技能库真路径（§11.3 字面量）
    iTime := strings.Index(section, "当前时间：2026-10-16 09:30:00 +00:00")        // 末尾日期时间（now 显式传入）

    marks := []struct {
        label string
        idx   int
    }{
        {"人格", iPrompt}, {"称谓", iName}, {"正文", iBody},
        {"bootstrap", iBoot}, {"卡片", iCard}, {"真路径", iPath}, {"时间", iTime},
    }
    for _, m := range marks {
        require.GreaterOrEqual(t, m.idx, 0, "段一缺少%s", m.label)
    }

    // 次序：人格 → 称谓 → 正文 → bootstrap → 卡片（name/description）→ 真路径 → 时间
    require.Less(t, iPrompt, iName)
    require.Less(t, iName, iBody)
    require.Less(t, iBody, iBoot)
    require.Less(t, iBoot, iCard)
    require.Less(t, iCard, iPath)
    require.Less(t, iPath, iTime)

    // 时间**收尾**（不是"存在即可"）：段一以时间行结束。
    require.True(t, strings.HasSuffix(section, "当前时间：2026-10-16 09:30:00 +00:00"),
        "段一必须以当前时间收尾，实际尾部：%q", tail(section, 40))
    require.NotContains(t, section, "技能说明正文占位") // L2 正文不得预载（夹具负向探针）
}
```

```go
// 中文验收点：装载后 bootstrap 文件消失 → SectionOne **返回错误**并点名路径，
// 禁止静默省略（静默省略会让"注入内容"与"清单哈希"悄悄分叉——§2.2 约束 6）。
// Acceptance point: a vanished bootstrap file is a loud error, never a silent omission.
func TestContextLoader_FailsWhenBootstrapFileDisappears(t *testing.T) {
    root := writeCompatSampleWorkspace(t)
    entry := loadOnlyAgent(t, root)
    require.NoError(t, os.Remove(filepath.Join(root, "SOUL.md"))) // 样例声明的三件之一

    section, err := NewContextLoader(bufferedLogger(t)).SectionOne(root, entry,
        time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC))
    require.Error(t, err)
    require.Empty(t, section)
    require.Contains(t, err.Error(), "SOUL.md") // 点名缺失路径
}
```

```go
// 中文验收点（**主判据**）：**跨进程**互斥——父进程持锁时子进程取锁必须失败并点名持有者
// （pid 即父进程），父进程释放后子进程再取必须成功。同进程双句柄只作补充：锁的判据必须
// 落在进程边界上，否则"锁只在本进程内生效"也能全绿（§2.3 流程 D）。
// Acceptance point (primary): the instance lock excludes across process boundaries.
const (
    lockHelperEnv    = "BASALT_LOCK_HELPER" // "1" = 本进程是 helper
    lockHelperRoot   = "BASALT_LOCK_ROOT"   // 工作区路径
    lockHelperAction = "BASALT_LOCK_ACTION" // acquire（取到即释放）/ hold（取到后阻塞，等被强杀）
)

func TestInstanceLock_HelperProcessMutualExclusion(t *testing.T) {
    if os.Getenv(lockHelperEnv) == "1" {
        lockHelperMain() // 子进程分支：做一遍并如实打印 "acquired" / "busy pid=…"，退出码恒 0
        return
    }
    root := t.TempDir()

    first, err := AcquireInstanceLock(root)
    require.NoError(t, err)
    t.Cleanup(func() { _ = first.Release() })

    // ① 父进程持锁 → 子进程取锁必须失败、且能读到持有者诊断（诊断在锁定区之外）
    out := runLockHelper(t, root, "acquire")
    require.Contains(t, out, "busy pid="+strconv.Itoa(os.Getpid()), "子进程应点名持锁者：%q", out)

    // ② 释放后子进程再取必须成功（重取路径）
    require.NoError(t, first.Release())
    require.Equal(t, "acquired", strings.TrimSpace(runLockHelper(t, root, "acquire")))
}

// runLockHelper 以自身测试二进制为 helper 跑一次，返回子进程 stdout；非零退出即用例失败。
// exec + 环境变量分派是标准形态；同步靠子进程**输出行**，不用睡眠（US-2 的 Started 信号同纪律）。
func runLockHelper(t *testing.T, root, action string) string {
    t.Helper()
    cmd := exec.Command(os.Args[0], "-test.run=TestInstanceLock_HelperProcessMutualExclusion")
    cmd.Env = append(os.Environ(),
        lockHelperEnv+"=1", lockHelperRoot+"="+root, lockHelperAction+"="+action)
    out, err := cmd.CombinedOutput()
    require.NoError(t, err, "helper 退出异常：%s", out)
    return string(out)
}
```

```go
// 中文验收点（**主判据**）：强杀持锁进程 → 父进程必须能**立即**重取（OS 级锁随进程消亡释放），
// 且**不得**依赖心跳 / exists 判据（契约 §4.3.1；US-1 §6 三场景读数）。
// Acceptance point: killing the holder releases the OS-level lock immediately.
func TestInstanceLock_HelperProcessReacquiresAfterHolderKilled(t *testing.T) {
    if os.Getenv(lockHelperEnv) == "1" {
        lockHelperMain()
        return
    }
    root := t.TempDir()

    cmd := exec.Command(os.Args[0], "-test.run=TestInstanceLock_HelperProcessMutualExclusion")
    cmd.Env = append(os.Environ(),
        lockHelperEnv+"=1", lockHelperRoot+"="+root, lockHelperAction+"=hold")
    stdout, err := cmd.StdoutPipe()
    require.NoError(t, err)
    require.NoError(t, cmd.Start())

    // 等子进程**打印** "acquired"——它是"锁已保住"的信号（不是睡一会儿再赌）
    lines := bufio.NewScanner(stdout)
    require.True(t, lines.Scan() && strings.TrimSpace(lines.Text()) == "acquired",
        "helper 未报告持锁：%q", lines.Text())

    require.NoError(t, cmd.Process.Kill())
    _ = cmd.Wait()

    lock, err := AcquireInstanceLock(root) // 强杀后必须立刻可取——锁文件残留不构成障碍
    require.NoError(t, err, "强杀后未释放锁：%v", err)
    require.NoError(t, lock.Release())
}
```

```go
// 中文验收点：frontmatter → Profile 的**逐字段映射**用非缺省值判卷（snake_case 键名逐字
// 对齐契约 §2：agent_name / mcp_servers / max_iterations / max_history_turns），且
// "未声明"与"显式零值 / 空列表"可区分（temperature: 0 与未声明不同；channels: [] 不回落 cli）。
// Acceptance point: snake_case keys map through an explicit DTO, and explicit zero
// values stay distinguishable from absent ones.
func TestAgentLoader_ParsesAllFrontmatterFields(t *testing.T) {
    root := t.TempDir()
    writeAgentFrontmatter(t, root, "mapper", `
name: mapper
description: 映射用例
identity:
  agent_name: 小帮手
  prompt: 你是映射用例。
provider:
  name: deepseek
  model: deepseek-chat
  temperature: 0
tools: [read_file]
skills: []
mcp_servers: [search]
bootstrap: [AGENTS.md]
channels: []
settings:
  max_iterations: 42
  max_history_turns: 7
schedules: []
`, "正文占位")
    writeFileInRoot(t, root, "AGENTS.md", "引导")
    writeMCPServers(t, root, []string{"search"})

    entries, err := NewAgentLoader(AgentLoaderOptions{Providers: []string{"deepseek"},
        Logger: bufferedLogger(t)}).LoadAll(root)
    require.NoError(t, err)
    require.Len(t, entries, 1)
    require.True(t, entries[0].Registered, "样例应注册：%s", entries[0].RejectReason)

    p := entries[0].Profile
    require.Equal(t, "映射用例", p.Description)
    require.Equal(t, "小帮手", p.Identity.AgentName)  // identity.agent_name（snake_case）
    require.Equal(t, "deepseek", p.Provider.Name)
    require.Equal(t, []string{"read_file"}, p.Tools)
    require.Equal(t, []string{"search"}, p.MCPServers) // mcp_servers（snake_case）
    require.Equal(t, []string{"AGENTS.md"}, p.Bootstrap)
    require.Empty(t, p.Channels, "显式 channels: [] 不得回落缺省 [{name: cli}]")
    require.Equal(t, 42, p.Settings.MaxIterations)     // max_iterations（snake_case）
    require.Equal(t, 7, p.Settings.MaxHistoryTurns)    // max_history_turns（snake_case）
    require.NotNil(t, p.Provider.Temperature, "显式 temperature: 0 与未声明不同")
    require.Equal(t, 0.0, *p.Provider.Temperature)
}
```

## 5. 范围边界（核心阶段不做项，逐项标注依据）

| 不做项 | 依据 / 归属 |
| --- | --- |
| 规则 4「`tools` 全部可解析」的门禁 | 编程指南 §4.3 尾注 + 本 US §2.2 约束 4——随 US-4 的 Registry 接入（占位登记，不得顺手补齐） |
| 规则 6「且连接成功」半条 | 契约 §2.1 规则 6 的连接面归 US-7（§2.4 注 7——本 US 不预设连接语义） |
| 技能 L2/L3（模型经 `read_file` 取 SKILL.md 与附属资源、技能库白名单） | 编程指南 §4.3 尾注——依赖文件工具与白名单，US-4 走通 |
| 四段提示词的段 2 / 3 / 4 与 `PromptBuilder` | 技术方案 §4.2——US-4 |
| 运行级配置快照（冻结清单 manifest、`hash`、原子发布、运行内冻结、"读即所记"） | 技术方案 §8.3 / §11.5——US-4 / US-11（本 US 的 `SectionOne` 是它的**输入**，不是它的实现） |
| 热加载 watcher（fsnotify / 防抖 / 全量 rescan） | 技术方案 §11.5 表——US-11 |
| 记忆注入与长期记忆（段二素材） | 需求 §5.5——US-6 |
| `schedules` 的注册与执行（`AgentScheduler`、`scheduled_tasks` / `task_executions`、`CRON_TZ` 前缀喂给 `Schedule`） | 技术方案 §8.5——US-10（本 US 交付 `CronSpec` 规范化函数，注册归 US-10） |
| 实例锁的**重路径接线**（取锁 → 写模式打开 → 显式迁移 → 恢复）与崩溃恢复扫描、`serve` 启动恢复 | 契约 §4.3.1 / US-1 §5·§6 / 起步手册 §4——US-4（本 US 落写模式命令准入的**最小面**，见 §2.4 注 1 的 R1 裁决） |
| 管理台的模板脚手架、页面编辑器、归档删除的取消 Run 序列、多文件提交（staging 交换） | 技术方案 §11.4 / 需求 §5.14——US-12（CLI 路径已持锁 ⇒ 无在跑 Run 与调度注册，三步为空集） |
| REST 端点（`/agents` 等）与错误码映射 | 技术方案 §8.7 重路径——US-9（占位登记） |
| `chat` / `serve` / `gateway` | US-4 / US-9 / US-10 |
| 沙箱、工具执行、PromptBuilder 的工具列表段 | 技术方案 §6——US-4 / US-5 |
| `AGENT.md` 内 `${ENV_VAR}` 解析 | **无文档依据**——本 US 不引入（凭证只活在 `basalt.yaml`，US-1 已解析；见附录 A 行 9） |
| `settings.*` 取值合理性（如 `max_iterations: 0` 的语义）校验 | 契约 §2.1 八条无此项——本 US **不发明规则**（显式零值原样保留，§2.4 注 9）；语义与范围落点随 US-4 构造请求时定 |

> **归属调整登记（R1 裁决，2026-10-16）**：US-1 颗粒度文档 §5 / §6、US-2 颗粒度文档 §5、起步手册 §4 记「实例锁与崩溃恢复的实现归 US-4」；本次裁决**调整为**：US-3 落**写模式命令准入的最小面**（锁本体 + `profile create` / `delete` 接线），US-4 接**重路径接线与恢复扫描**。已验收的历史工件**不回改**——调整以本文（§2.4 注 1）与台账（`/us-register 3` 派生）为准，US-4 规格引用本登记。
>
> **US-4 跨模块约定**：段一注入内容与运行级 manifest 哈希**共享同次读取产物**（§2.2 约束 6）——US-4 **不得**对本 US 的读取面二次读盘算哈希；若需素材级产物（读取原文而非拼接结果），由 US-4 在本包内扩展导出面（读取原语留在 `workspace` 内），**不**允许"调用返回后再读一遍"。

## 6. 验证与验收（剩余人工项）

| # | 人工项 | 判据 / 操作 |
| --- | --- | --- |
| 1 | `make check` 七目标全绿 | 退出码 0；`cover` 读数里 `internal/workspace` 给出百分比并判 `>= 50%`；`internal/core` 仍记「无可执行语句」（只放声明成立） |
| 2 | 验收 Demo（编程指南 §4.3）逐条走一遍 | ① `profile create demo-agent` 出模板（**输出明示选中的 provider**）→ `profile list` / `show` 可见；② 故意写坏六例（未注册 provider / 库里没有的技能 / 5 段 cron / 日周双限定 / `zone: Local` / 空白 model）→ 逐只 `registered=false` + WARN 点名（`agent` / `reason` / `detail` 三键）；③ 技能声明即绑定：样例 Agent 的段一含卡片 L1（harness 断言；**人工可见面在 US-4 的 `chat` 上线后**——本 US 无 chat） |
| 3 | Compatibility §5.1 六条判据（本仓侧） | 判据 1 / 2 / 4 / 5 由 harness 判卷（A12）；**判据 3 的「两侧逐条相同」与判据 6 的前提（两侧均已注册 `deepseek` 且配置 API key env）需要 NivroOS 侧执行**——人工对拍，读数登记回本文档 |
| 4 | 写模式命令的拒绝文案与跨进程行为 | 手工：起一个持锁进程（或另开一个 `profile create` 挂在锁上）→ 第二条写模式命令拒绝启动、文案点名 pid 与开始时间、指引 REST / 管理台；只读命令（`profile list`）照常可用；**双进程手工复跑的判别以 harness 的 helper-process 用例为准** |
| 5 | 实例锁**平台分支读数**（分别登记） | Windows：本机跑 `lock_test.go` 全套并登记读数（CI 不覆盖 `lock_windows.go` 的运行）；类 Unix：Ubuntu CI 读数；CI 的 `build` job 只证明 `lock_windows.go` **可编译**——**不得**据此宣称其行为已验证 |
| 6 | 凭证面复核 | `grep -rnE "sk-\|api_key: [^$]" .basalt/agents/` 零命中（AGENT.md 不含凭证）；日志缓冲断言零凭证（A13） |
| 7 | 装配确认 | `internal/app/run.go` 注入 `agentsPort`；`go list` 判据证明 `cli` 不 import `workspace`（A11） |
| 8 | 门禁外读数登记 | **变动范围符合清单、无探针残留**（锁文件、`agents/<name>/` 测试残留、临时归档目录逐项核对——**不要求** `git status` 干净，本阶段尚未提交）；`make check` 输出、覆盖率读数写入本文档 §6 或收口报告（完成证据纪律） |

## 已决事项

### 自 US-1 / US-2 带入，本 US 沿用

1. **轻 / 重路径与只读边界**：技术方案 §8.7 + 契约 §4.3.1；「只读 / 写模式」是**实例锁语义**分类（能否与活实例并行、谁做恢复、谁持锁迁移），不是"零字节写入"承诺；「只读文件系统状态」读作"不依赖业务装配（注册表 / 全量装配）"（US-1 §2.4 裁决注）。
2. **退出码口径**：成功 0 / 失败 1，原因由文案区分（US-1 落地；`profile` 四命令沿用）。
3. **包纪律与端口注入**：接口定义在消费方（`cli` 自声明端口）、适配器只放 `internal/app`、`cli` 不 import 实现包（US-1 D1 裁决：无环**不等于**依赖方向合规）；`core` 只放声明（US-2 落地）。
4. **文件系统纪律**：`os.Root` 钉根、0o750 / 0o644、不覆盖已有文件（US-1 / US-2 口径；本 US 扩到目录扫描，§2.2 约束 12）。
5. **日志与错误**：失败路径可读文案 + 结构化日志两轨都在、错误不吞、级别按 EngineeringStandards §4（US-1 / US-2 口径）。
6. **测试纪律**：测试函数名英文、默认层不触网、`-race`、时间显式传入（不得依赖真实当前时间）。

### 本文固定口径（无权威字面量，逐字冻结在实现与用例里）

1. 段一渲染模板（§2.4 注 4）：`identity.prompt` → `你的名字是「<agent_name>」。` → 正文 → `## <bootstrap 文件>` 小节 → `## 技能` 下的 `- <name>：<description>（<真路径>）` → `当前时间：<YYYY-MM-DD HH:MM:SS ±hh:mm>`（格式 `2006-01-02 15:04:05 -07:00`，缺省 UTC）。
2. 日志固定键：`agent` / `reason` / `detail`（`reason` 取契约字面量原样，点名细节进 `detail`；未知键 WARN 的 `reason` 取 `unknown frontmatter key`、`detail` 只放键路径）。
3. `profile list` / `show` 渲染：`<name>：registered=true（provider <p> / 模型 <m>）` 或 `<name>：registered=false——<原因>`；汇总行 `汇总：N 个 Agent 目录（已注册 M）`。
4. **5 段 cron 文案**：`invalid schedule: Cron expression must consist of 6 fields`（固定句来自 Compatibility §5.1 判据 4；robfig 原文另附 `detail`）。
5. **schedules 校验的 detail 字面量**（冻结；`reason` 恒为 `invalid schedule`）：

   | 场景 | detail |
   | --- | --- |
   | 段数 ≠ 6 | `Cron expression must consist of 6 fields` |
   | 语法非法（robfig 拒） | robfig 错误原文（如 `expected exactly 6 fields, found 5: [...]`） |
   | 含 `L` / `W` / `#` | `字段不支持 L/W/# 语法：<字段原文>` |
   | 周字段含 `7` | `周字段 7 超出范围（0–6）` |
   | 日与周同时被限定 | `日与周不得同时受限（至少一个是裸 *）` |
   | `zone` 为 `Local` | `zone Local 被禁用（需地方时间请显式指定 IANA 时区）` |
   | `zone` 名非法 | `time.LoadLocation` 失败原文（zone 名不是秘密，可入文案） |
   | `message` 空白 | `message 为必填` |
   | ≥2 个任务未显式 id | `id 仅可在恰一个任务时缺省（派生 <agent>-1）` |
   | 本只内 id 重复 / 与已占用 id 撞车 | `id 重复：<id>` / `id 已被 <占用者> 占用：<id>` |
   | cron 自带 `TZ=` / `CRON_TZ=` 前缀（R2 追加） | `cron 内不得自带时区前缀（TZ= / CRON_TZ=）——时区一律由 zone 字段声明`（装载期与 `CronSpec` **共用同一条常量**） |

6. **`RejectReason` 的合并形态**：`reason`（detail 为空）或 `reason + ": " + detail`（人读合并；契约字面量原样在前——用例断言一律用 `Contains` 判前缀）。
7. **R2 追加的自产字面量**（评审回合四修复；追加登记，不回改第 1~6 条）：① `bootstrap` 条目的类型不符 —— `reason` 仍取契约字面量 `bootstrap file missing`，detail 为 `<路径>（不是普通文件：<类型>）`（类型取 `目录` / `符号链接` / `FIFO（命名管道）` / `设备文件` / `套接字` / `特殊文件`）；② `delete` 拒绝非目录条目 —— `待归档条目不是目录：<agents 相对路径>（<类型>）`（类型取 `目录` / `符号链接` / `普通文件` / …）；③ cron 自带时区前缀的拒绝句见第 5 表末行。三处的**判定面**都在 stat 的类型位 / 名字形态上，不在文件系统的反馈上（§2.4 注 13 / 注 14 / 注 15）。

### 评审回合 R1 裁决（2026-10-16，用户裁决；逐项落案）

1. **实例锁落位**：同意最小面前移 US-3（写模式命令准入：锁本体 + `create` / `delete` 接线），US-4 接重路径与恢复；**删除**「本阶段不取锁交付」的备选；归属调整登记在 §5 末行与 §2.4 注 1（历史工件不回改）。
2. **调度 id 撞车**：先到先得——**但只由完整校验通过的 Agent 占用**（局部集校验 → 通过后整只提交；被拒 Agent 不占用任何 id）。
3. **`profile create` 模板**：`provider.name` 取配置数组**首项**，输出**明示选中了谁**；无配置时写占位文本并说明"模板尚未注册"；`tools: []`（记忆工具到 US-6 才在池——现在写会让模板在 US-4 ~ US-6 窗口期因 `tool not registered` 拒载）。
4. **`profile delete` 语义**：**归档**（单次 rename 入 `archive/<name>-<时间戳>/`）；物理删除**不再列为等价备选**；会话 / 记忆 / 审计数据不动。
5. **缺 / 无效 `basalt.yaml` 的边界**：缺失 → 可诊断（输出明示"按未注册处理"、不计失败）；**无效 → 失败**（原错误上抛）；**未初始化 → 失败**（点名 + 指引 `basalt init`）；`agents/` 空 → 成功 + "未发现 Agent 目录"。
6. **契约字面量缺口**：① `invalid schedule` 前缀**可接受**（固定句原样嵌在 detail 里）；② 未知键**宽容但可见**（WARN 键路径、**不打印值**）；③ `provider.temperature` 的 0–2 **在装载期执行**（越界与非有限值都拒）——不再推迟到 US-4。
7. **文字修正两处**：段一「当前时间」**缺省 UTC**、地方时间须**显式 IANA 时区**、**输出含时区偏移**（删除「US-4 装配用进程本地时区」）；日志伪代码落实 `detail` 键；§6 第 8 项由「`git status` 干净」改为「**变动范围符合清单、无探针残留**」。

### 评审回合 R2 裁决（2026-10-16，用户评审四项，逐项落案）

用户以评审者身份报告 **1 个 P1 + 3 个 P2**（均以临时夹具复现），要求修复后再终验；修法与判据落案如下（实现与用例同批落地，回归用例见 §3.2 的 R2 追加段，落案细节见 §2.4 注 13 / 注 14 / 注 15）：

1. **P1 技能名可跳出公共技能库**（`skill.go` 只做拼接与清理）→ 技能名**必须**是单个目录名 + 实体查找与卡片读取一律经**技能库根**；越界名按「没有实体」拒载（`skill not found: <name>`，**不新增 reason 字面量**）、**不做路径拼接**。
2. **P2 cron 自带时区前缀与出口双前缀**（`validateScheduleEntry` 放行 + `CronSpec` 追加 ⇒ `CRON_TZ=UTC CRON_TZ=Asia/Shanghai …` 随后解析失败）→ `zone` 是**唯一**时区来源：装载期与 `CronSpec` **共用同一条拒绝句**；语法校验**解析规范化结果**（校验与出口同一份表达式）。
3. **P2 `profile delete` 会归档普通文件**（只判存在性）→ 源条目**必须**是目录（`Lstat`：符号链接不跟随），拒绝且源条目原样保留。
4. **P2 `bootstrap` 指向目录仍能注册**（忽略 `Stat` 的类型）→ 装载期**必须**核对文件类型（普通文件才放行）；`reason` 沿用契约字面量 `bootstrap file missing`，类型进 detail。

另：① 用户指出的**报告计数错误**已更正（六个新增测试文件合计 **2216** 行，不是 3336 行）；② 唯一 `//lint:ignore ST1005` 豁免**保留**（与冻结字面量对应，用户确认）；③ 本回合新增一处 `t.Skip`（符号链接用例——Windows 需开发者模式 / 特权，理由写在用例里），[quickstart 场景 5](../../specs/003-agent-profile/quickstart.md) 的「无 `SKIP`」表述随之订正。

## 待决事项

**无**——R1 评审回合的六项待决与 R2 评审回合的四项修复（1 个 P1 + 3 个 P2）已全部裁决并落案（见上「评审回合 R1 裁决」与「评审回合 R2 裁决」）。文档进入**冻结候选**：`/module-dev 3` 实施期间若发现新的事实性缺口，按门禁停下报告，由用户裁决后**追加**登记（不回改已冻结口径）。
