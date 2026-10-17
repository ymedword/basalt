# Phase 0 研究：US-3 Agent 目录与 Profile 加载

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-16

**结论先行**：Technical Context 无 `NEEDS CLARIFICATION`——设计面已冻结（颗粒度文档 [docs/us/us3-agent-profile.md](../../docs/us/us3-agent-profile.md)「待决事项」为空：R1 评审回合七项裁决全部落案；三条表达歧义经 `/speckit-clarify` 会话 2026-10-16 溯源自决并写入规格 Clarifications）。本文件的职责是**把「怎么做」的关键决策与「写前必须实测核实」的项逐条固化**，供 implement 阶段逐项消化（module-dev 步骤 6 的 H3 门禁：第三方 API 在本地依赖核实不到 → 软门禁停下报告）。

## 决策记录

### D1 装载流水线：两份 DTO + 显式映射，`core` 不加序列化标签

- **Decision**: frontmatter 解析走两段式——`workspace` 私有 `frontmatterDTO`（`yaml:` 标签逐字对齐契约 §2，`*T` 区分「未声明」与「显式零值」）→ `intoProfile` 显式映射到 `core.Profile`（**不带** yaml 标签）。
- **Rationale**: Go 的默认字段名匹配对 `agent_name` / `mcp_servers` / `max_iterations` / `max_history_turns` **一个都对不上**（大小写与下划线都不匹配）；而 `core` 是契约层，**不得** import 第三方（宪法·架构约束）——序列化绑定归 `workspace`（颗粒度文档 §2.4 注 9）。
- **Alternatives considered**: DTO 直接放 `core`（契约层绑序列化库 + 破包纪律）；`map[string]any` 手工逐键取值（丢类型错误的定位能力，八个 snake_case 键配八段手写转换）。

### D2 未知键「宽容但可见」：比对 + 逐条 WARN，不采用严格模式

- **Decision**: 契约字段表之外的键**忽略但可见**——逐条 WARN（`reason` 固定 `unknown frontmatter key`、`detail` 只放键路径、**不打印值**；路径集先排序后输出），继续装载。
- **Rationale**: 出处解读 = Compatibility §2.2「共有参数逐字一致 + Basalt 新增参数不改变共有参数含义」的**前向兼容精神**：基线（NivroOS）先于本仓演进协议，严格模式会把「基线新增可选项」升级为「双底座不兼容」。「不打印值」与陷阱 40 同源：未知键的值是**我们还没理解**的输入，落进日志等于写入审计外的第二份记录（R1 裁决，§2.4 注 5）。本裁决**只针对 AGENT.md**；`basalt.yaml` 的未知键口径以 `cli` 现有实现为准。
- **Alternatives considered**: `goccy/go-yaml` 的 `yaml.DisallowUnknownField()` 严格模式（可用但**未采纳**——同上）。

### D3 schedules 校验：robfig 6 段 parser + 本仓预检分层

- **Decision**: 6 段 parser 由 `cron.NewParser(cron.Second|cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow)` 构建；**字段数 ≠ 6 的固定句为本仓自产**（`Cron expression must consist of 6 fields`），其余语法错误取 robfig 原文进 `detail`；`L` / `W` / `#`、周字段 `7`、日周双限定、`message` 必填、`zone`（缺省 UTC / `Local` 拒 / IANA 名必须可加载）、`id`（派生与唯一）为本仓预检层，`reason` 恒为 `invalid schedule`——detail 字面量逐字冻结在颗粒度文档 §6 固定口径 5。
- **Rationale**: robfig v3.0.1 源码逐行核实（本轮，本地模块缓存）：`Parse` 缺省时区 `var loc = time.Local`；字段数错误原文 `expected exactly %d fields, found %d: %s`；`L` / `W` / `#` 零命中（不支持）；dow `bounds{0, 6}`（`7` 同样拒）。⇒ 预检**不是**为了拦 robfig 会放行的写法，而是：① 报出契约固定的句子（Compatibility §5.1 判据 4 要求的字面量）；② 让公共子集的拒绝有**本仓统一**的类别与细节（用户看到的分类由我们定，不是库的）（§2.4 注 6）。
- **Alternatives considered**: 纯自实现 6 段解析器（重复造轮子；且共享验收样例的「从固定起点起未来十次触发」判据要求与 robfig 同语义——Compatibility §5.1 判据 6）；只用 robfig 原文（固定句判据落不了地）。

### D4 `CronSpec` 规范化：装载层交付、调度注册归 US-10

- **Decision**: `CronSpec(s core.Schedule) (string, error)` 输出 `CRON_TZ=<zone> <cron>` 前缀形态；装载期校验与共享验收样例对拍都走它。**调度注册与执行不在本 US**。
- **Rationale**: 技术方案 §8.5 与契约 §3.1 的「装载时转前缀」；保证与 US-10 的注册形态**同一份来源**（§2.4 注 6）；`zone` 在装载期已被 `time.LoadLocation` 验证（`Local` 黑名单在前），`CronSpec` 的错误面只剩余区不可加载的兜底。
- **Alternatives considered**: `cron.WithLocation` 全局时区（每任务时区不同，parser 级选项只能全局——形态不符）；延后到 US-10 拼前缀（双份来源必漂移）。

### D5 实例锁本体：OS 级独占锁 + 锁定区之外的诊断布局

- **Decision**: `.basalt/basalt.lock` 上锁**区 `[0, 1)`**（Windows `windows.LockFileEx` 排他 / Unix `unix.Flock` 非阻塞）；诊断记录（instance_id / pid / 开始时间）写在锁定区**之外**的固定偏移 **4096** 起、定长、字段序固定；取锁前**不截断**锁文件（`O_CREATE|O_RDWR`，无 `O_TRUNC`）；`Release` 幂等（只解锁 + 关句柄，**不删除锁文件**）。
- **Rationale**: Windows 的 `LockFileEx` 排他锁**阻止其他句柄读取锁定区**——诊断与锁区重叠时，拿不到锁的进程恰好读不到「是谁占着」（正是要它的那一刻）；契约 §4.3.1 明确 pid / 心跳**只作诊断**：**不得**据心跳过期抢锁、**不得**用 exists 判据；锁的生命周期由内核决定（进程退出即释放，强杀可重取，§2.3 流程 D）。
- **Alternatives considered**: PID 文件「存在即锁」（残留死锁 + 活死不可分，契约已否）；全文件锁（诊断区不可读）；锁目录形态（无需，且 Windows 语义不齐）。
- 本机核实（本轮）：`windows.LockFileEx` / `windows.UnlockFileEx` / `windows.Overlapped` / `ERROR_LOCK_VIOLATION = 33` 与 `unix.Flock` 均在 `x/sys@v0.48.0` 本地模块缓存（读数见下节）。

### D6 归档：单次 `os.Root.Rename` + 显式撞名检查

- **Decision**: `DeleteAgent` 把 `agents/<name>/` **单次 rename** 到 `archive/<name>-<YYYYMMDD-HHMMSS>/`；目标已存在 → 拒绝且源目录保留；`now` 显式传入（CLI 层由 cobra 取 `time.Now()` 传入，`runProfileDelete(out, deps, name, now)` 保持可判卷）。
- **Rationale**: 「不覆盖」**必须**靠显式存在检查——Unix 的 `rename(2)` 会**静默替换空的同名目录**，不得指望 rename 的语义；「移动 = 单次 rename」在同一文件系统内原子：要么成功、要么源目录原样——**不得**「复制 + 删除」、**不得**「先删源后建目标」（R1 裁决，§2.4 注 11）。
- **Alternatives considered**: 复制 + 删除（非原子，中途失败源已半损）；物理删除（R1 已裁决不再列为等价备选——归档语义）。

### D7 段一无状态原语：每次重读 + `now` 显式传入

- **Decision**: `ContextLoader.SectionOne(root, entry, now)` 每次调用**重读**全部素材（AGENT.md 正文 / bootstrap 文件 / SKILL.md 头），不缓存；`now` 由调用方显式传入、传什么时区渲染什么（缺省 UTC、输出含偏移）；任一步读取失败 → 返回错误并点名路径（**不得**静默省略该节）；只接受 `Registered` 的条目。
- **Rationale**: FR-007 / FR-008 + 宪法 VII（业务状态显式传递、**不得**用包级变量模拟 ThreadLocal）；重读语义是 US-4 运行级快照「共享同次读取产物」的前置形态（§2.2 约束 6 / §3.5 占位点 9）。
- **Alternatives considered**: 加载期缓存素材（FR-008 禁——「每次调用重读、不缓存」）；函数内取 `time.Now()`（FR-008 禁——测试不可判卷）；`time.Local` 兜底（禁用词，全局不变量 7）。

### D8 技能卡片：声明即绑定，重复声明逐条渲染

- **Decision**: 卡片三要素 `name`（SKILL.md frontmatter name，缺失回落技能目录名）/ `description`（缺失为空串，不发明校验）/ `path`（**逐字** `.basalt/skills/<name>/SKILL.md`）；`skills:` 声明名在技能库无实体 → 该 Agent 整只不注册；同一技能重复声明**逐条渲染**（不去重——`cardsFor(base, p.Skills)` 无去重步骤）；Agent 本地 `skills/` 目录被无视（WARN，不改变绑定结果）；只注入 L1 卡片、**不得**预载 SKILL.md 正文、**不得**新增 `use_skill` 工具。
- **Rationale**: FR-004 / FR-009 + 宪法 IV；真路径字面量出处 = 技术方案 §11.3。「声明即绑定、各平台一致」（**不得**引入软连接类机制）。重复声明的裁定说明：该点**无独立权威字面量**，按颗粒度文档 §2.3 流程 B 伪代码**机械读出**（零额外逻辑）——已向用户透明说明（spec.md Clarifications 2026-10-16，改去重为零成本）。
- **Alternatives considered**: 去重渲染（需引入一条文档无记载的规则——按冻结伪代码机械读法不采纳）。

### D9 命令面：端口在消费方，`providers` 显式注入

- **Decision**: `cli` 自声明 `Agents` 端口（`List(root, providers)` / `Create(root, name, providers)` / `Delete(root, name, now)`），适配器 `app.agentsPort{WorkDir, Logger}` 转调 `workspace`；`providers` 由命令面经既有 `LoadConfig` 取得后显式传入（**不得**让适配器另读一次配置）；`profile show` 复用 `LoadAll` 结果集过滤（**不提供 `LoadOne`**）。
- **Rationale**: 依赖方向 `cli → core ← workspace`（宪法·架构约束；既有 `cli/ports_test.go` 把 `workspace` 列在禁止 import 清单——判据已由 CI 判卷，本 US 不新增判据文件）；同一文件两次读取是陷阱 39 的根因形态（§2.4 注 2）；「注册判定与加载必须是同一份解析与校验代码」（FR-012 / 技术方案 §8.7「不得为同一语义写两份实现」）。
- **Alternatives considered**: `cli` 直接 import `workspace`（破依赖方向红线）；适配器内部再读配置（双读 + 与命令面展示可能不一致）；`LoadOne`（第二套语义入口，FR-012 禁）。

### D10 测试基础设施：夹具内联 + helper-process

- **Decision**: 夹具由各测试包**非导出** helper 写入 `t.TempDir()`（家族 `writeCompatSampleWorkspace` / `writeWorkspaceWithSchedule` / `writeAgentFrontmatter` / `writeFileInRoot` / `writeMCPServers` / `loadOnlyAgent` / `bufferedLogger`）；**不新增 `testutil` 公开 API**；跨进程判据用 helper-process（exec 自身测试二进制 + 环境变量分派 + 子进程 stdout 行同步——**不得**用睡眠等待）。
- **Rationale**: §2.4 注 8——US-2 的 `FakeProvider` 有「随接口落位」的明文授权，本 US 无对应授权（不发明测试概念）；helper-process 是唯一可**真判**跨进程互斥的形态（同进程双句柄用例 `TestInstanceLock_SecondAcquireFailsWhileHeld` 只是补充）。
- **Alternatives considered**: 抽 `testutil` 公开夹具（无授权；若实测出现三处以上重复夹具再提用户裁决——§2.4 注 8）；睡眠等待锁竞争（时序脆弱，CI 上时紧时松）。

## 写前必须实测核实清单（H3 门禁）

implement 阶段**写码前**逐项在本地依赖内核实（核实不到 → 软门禁停下报告）。可前置核实的项本轮已核实（读数见下节）；余项在对应任务动手前就地复核：

| # | 核实对象 | 本地核实方式 | 状态 |
| --- | --- | --- | --- |
| H1 | `robfig/cron/v3@v3.0.1`：`NewParser` / `(Parser) Parse` / `Schedule.Next(time.Time) time.Time` / `CRON_TZ=` 前缀 | 模块缓存源码（parser.go / cron.go） | 已核实 |
| H2 | `golang.org/x/sys@v0.48.0/windows`：`LockFileEx` / `UnlockFileEx` / `Overlapped` / `ERROR_LOCK_VIOLATION` | `zsyscall_windows.go` / `types_windows.go` / `zerrors_windows.go` | 已核实 |
| H3 | `golang.org/x/sys@v0.48.0/unix`：`Flock`（Linux 分支在 CI 编译**并运行**） | `zsyscall_linux.go` | 已核实（仅存在性；运行读数不属本机） |
| H4 | `os.Root` 方法面（Go 1.27.2）：`Open` / `OpenFile` / `Mkdir` / `MkdirAll` / `Lstat` / `Stat` / `Rename` / `ReadFile` / `WriteFile` 存在；**`ReadDir` 不存在**（目录扫描 = `root.Open(dir)` 返回的 `*os.File` 上的 `ReadDir`） | GOROOT `src/os/root.go` | 已核实（前序评审出过一次误认知「`os.Root` 有 `ReadDir`」——写前必须复核） |
| H5 | `(*os.File).Fd()`（取锁句柄的来源） | GOROOT `os/file_unix.go` / `os/file_windows.go` | 写前复核 |
| H6 | `goccy/go-yaml v1.19.2`：`*float64` / `*int` 指针字段未声明解码为 `nil`；`yaml.Marshal` 输出可被自身 `Unmarshal` 回读（模板「名字经 YAML 编码」用例的前提） | 模块缓存 | 写前复核 |
| H7 | Windows 错误判定：`errors.Is(err, windows.ERROR_LOCK_VIOLATION)` 对 `LockFileEx` 返回的 `syscall.Errno` 成立 | 模块缓存 + 写后用例 | 写前复核 |

## 探针与本地读数（2026-10-16，本轮当场命令）

- **环境**：`go version` → `go version go1.27.2 windows/amd64`（退出码 0；与 `toolchain go1.27.2` 一致）；`GOROOT` = `C:\Program Files\Go`；`GOMODCACHE` = `C:\Users\76562\go\pkg\mod`。
- **坐标核实**：`GOPROXY=https://goproxy.cn go list -m -versions github.com/robfig/cron/v3` → `v3.0.0-rc1 v3.0.0 v3.0.1`（退出码 0；拟用 GA 末位 `v3.0.1`，不用 rc）。
- **模块缓存就地核实**：`github.com/robfig/cron/v3@v3.0.1/` 与 `golang.org/x/sys@v0.48.0/` 均已在本地（H3 门禁可就地 grep 方法签名，无需网络）。
- **robfig v3.0.1 API 读数**（模块缓存源码）：`func NewParser(options ParseOption) Parser`（parser.go:71）；`func (p Parser) Parse(spec string) (Schedule, error)`（parser.go:88）；`CRON_TZ=` / `TZ=` 前缀在 `Parse` 内处理（parser.go:95）；`type Schedule interface` 含 `Next(time.Time) time.Time`（cron.go:40）；字段数错误 `expected exactly %d fields, found %d: %s`（parser.go:187）。
- **x/sys v0.48.0 API 读数**：`windows.LockFileEx`（zsyscall_windows.go:2923）/ `windows.UnlockFileEx`（zsyscall_windows.go:3524）/ `type Overlapped struct`（types_windows.go:789）/ `ERROR_LOCK_VIOLATION syscall.Errno = 33`（zerrors_windows.go:186）；`unix.Flock(fd int, how int) (err error)`（zsyscall_linux.go:837）。
- **`os.Root` 方法面读数**（GOROOT `src/os/root.go`）：`Name` :90 / `Close` :96 / `Open` :102 / `Create` :108 / `OpenFile` :117 / `OpenRoot` :132 / `Chmod` :139 / `Mkdir` :149 / `MkdirAll` :161 / `Remove` :188 / `RemoveAll` :194 / `Stat` :200 / `Lstat` :209 / `Readlink` :216 / `Rename` :223 / `Link` :235 / `Symlink` :247 / `ReadFile` :253 / `WriteFile` :264——**无 `ReadDir`**。
- **`go.mod` 三行读取**：`go 1.27` / `toolchain go1.27.2` / `golang.org/x/sys v0.48.0 // indirect`（`x/sys` 迁直接依赖是本 US 落位的一部分，**版本不变**）。
- **如实登记的未探测项**：Windows 锁分支的**行为**读数（helper-process 互斥、强杀重取、竞争不改文件）属实现后的门禁读数，本阶段不声称；`unix.Flock` 的**运行**只发生在 Ubuntu CI（本机不跑；CI 绿也**不得**据此宣称 Windows 分支已验，§4.1）。
