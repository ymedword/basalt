# Phase 0 研究：US-4 ReAct 循环与首个工具闭环

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-16

**结论先行**：Technical Context 无 `NEEDS CLARIFICATION`——设计面已冻结（颗粒度文档 [docs/us/us4-react-loop.md](../../docs/us/us4-react-loop.md)「待决事项」为空：2026-10-16 评审回合八项裁决全部落案、已决事项 10 条；`/speckit-clarify` 会话 2026-10-16 零提问，六处口径按四类事实源收敛进规格）。本文件的职责是**把「怎么做」的关键决策与「写前必须实测核实」的项逐条固化**，供 implement 阶段逐项消化（module-dev 步骤 6 的 H3 门禁：第三方 API 在本地依赖核实不到 → 软门禁停下报告）。

## 决策记录

### D1 引擎落 `core`：判据从「零语句」改为「只依赖标准库」

- **Decision**: `ReActLoop` / `PromptBuilder` / `ToolExecutor` / `AgentService` 落在 `internal/core`（该包自本 US 起**首次出现可执行语句**）；纪律判据改为 `GOPROXY=https://goproxy.cn go list -f '{{.Imports}}' ./internal/core` 只出标准库包名（**不得**出现第三方路径与 `github.com/ymedword/basalt/internal/...`）。
- **Rationale**: 技术方案 §10.1 把这四个类型定在「核心抽象与引擎」——这是**已决事项 1**（不改归属）；纪律实质（依赖最小化、可移植、无第三方绑定）不变，只有「零语句」这一形态判据随实现落地而失去意义（§2.4 注 11）。
- **Alternatives considered**: 另开 `internal/engine` 包（与技术方案 §10.1 的定名冲突，且台账「模块落位」列不含新包——发明对外概念）；把引擎放 `app`（装配层不该承载业务算法，且 `app` 会反向被 `cli` 依赖）。

### D2 循环与 TS §4.1 的六处必要差异（§2.4 注 12）

- **Decision**: 与 [docs/TechnicalSolution.md](../../docs/TechnicalSolution.md) §4.1 伪代码**逐块同构**，六处差异逐字冻结：① `run(ctx, runID string, env *RunEnv)` 显式带 `runID`；② 半截组收尾经 `env.Session.CompleteToolGroup(ctx, runID, calls, reason)`；③ 工具结果经 `env.Session.AppendToolResult(call, result)`；④ `ToolExecutor.Execute(ctx, env ToolEnv, toolSet []string, call ToolCall) Result` **返回结果、不写消息**；⑤ `env.ToolEnv(ctx, runID)` 显式构造执行环境；⑥ `run` 的 error 恒 nil（运行期失败一律体现在 `Result.Status`）。
- **Rationale**: ① 恢复要靠 `messages.run_id` 定位半截组（`Message` 本身没有该字段）；④ 消息追加**唯一路径**是宪法 II 的硬要求——执行器写消息就会出现第二出口；⑥ 契约 §1.2「函数级 error 只用于调用方问题」。
- **Alternatives considered**: 照抄伪代码（缺 `runID` 则恢复不可判、执行器直写消息则破唯一追加路径）；把 `runID` 塞 `context.WithValue`（宪法 VII 只允许追踪标识，且本仓要求显式传入）。

### D3 终止与取消判定顺序：取消优先于预算，轮首取消不计当前轮

- **Decision**: **每次执行工具之前**先查取消、再查工具预算；工具预算耗尽**不**禁止「不使用工具的模型响应」；轮首取消不计当前轮（那次的模型调用未发出）；三类终止语义分明——完成 / `limit_reached` / `cancelled`，达上限**不是错误**。
- **Rationale**: 流程 B 的补记（P1 修正 ③）+ 契约 §1.3：取消路径命中的未执行调用取**「已取消」**合成文本，取成「未执行（工具调用次数超限）」就是**错报原因**；预算耗尽时模型仍可能直接给出最终回答，提前终止会丢答案。
- **Alternatives considered**: 预算判在取消之前（错报取消原因）；预算耗尽即 return（丢最终回答）；取消也算一次迭代（`iterations` 语义漂移）。

### D4 三套字面量分开冻结 + fail-closed 两条链 + 消息写入 fail-closed

- **Decision**: 审计 `status` 四值 `pending` / `success` / `failed` / `unknown`（**无** `cancelled`）；`error_kind` 七值 `sandbox_denied` / `timeout` / `not_found` / `invalid_input` / `external_error` / `runtime_error` / `unknown`；Run `status` 五值 `running` / `completed` / `limit_reached` / `cancelled` / `failed`——**两套不得混用**（可以「运行 cancelled」而「审计 `failed` + `timeout`」，与 `ProviderService.classifyError` 同口径：ctx 错误一律归 `timeout`）。工具 `pending` 预写失败 → **拒绝执行** + 回填「审计不可用」+ **Run 继续**；模型调用预写失败 → **拒绝发出** + Run failed；**消息写入失败 → Run failed**（fail-closed）；审计**终态回填**失败**不在** fail-closed 之列（记 ERROR、行留 `pending`、**不覆盖**真实结果）。
- **Rationale**: 契约 §7.2 / §7.3 / §2 + 已决事项 7。消息写入失败若只记日志，就会出现「执行了但历史里没有」的调用——恢复与审计都看不见，等于审计被绕过。
- **Alternatives considered**: 审计加 `cancelled` 取值（**契约无此值**——发明字面量）；消息写失败仅记 ERROR（破坏 fail-closed 的同一纪律）；回填失败改行内容为「未知」（覆盖真实结果，恢复分支会误判为「结果未知」）。

### D5 工具名与归属的两级拒绝**不写审计行**

- **Decision**: 名字不在注册池 / 已注册但不属本 Agent 的 `toolSet` → **不执行、不写审计行**，回填四段式放行指引并点名声明处（第一级点名 `AGENT.md` 的 `tools:` 声明；第二级点名注册池）；沙箱拒绝**在执行之内** → `failed` + `sandbox_denied`（有行有分类）。
- **Rationale**: 已决事项 9——`tool_invocations` 记的是「一次工具执行」，执行**之前**的拒绝无入参也无结果可言（写行就得编造 `input_json` 与 `error_kind`）；尝试本身已由 assistant 消息与回填文本留痕。
- **Alternatives considered**: 两级拒绝也落审计行（语义污染 + 需发明 error_kind 取值）。

### D6 快照冻结：同一次读取 + 哈希不含时间 + 运行前落库失败即拒启动

- **Decision**: `ContextLoader.Snapshot(root, entry, now)` 在**同一次读取**里解析 Profile、组装段一正文并保留 AGENT.md 原文（**不得**用旧 `entry.Profile` 混用）；`Hash` = manifest 规整序列化的 sha256（**当前时间不得混入**——同内容不同时刻同哈希）；落库走 insert-if-absent（`UNIQUE(agent_name, hash)` 冲突即 no-op）；**Run 开跑之前**保存，**失败即拒绝启动**（零模型调用）。`content` 存 **AGENT.md 全文**（不能只存哈希）。
- **Rationale**: 技术方案 §11.5 + 裁决 3 + 契约 DDL 的「读即所记」——运行结束后再读盘，快照记的就不是这次运行看到的内容。
- **Alternatives considered**: 只存哈希与路径（裁决 3 明确不通过——事后无法重建提示词来源）；运行中现读现用（改盘即漂移）；哈希含时间戳（同内容不同时刻哈希不同，去重失效）。

### D7 上下文预算：两级裁剪（轮数 → 完整工具交互组）+ 余量 + 准入拒绝

- **Decision**: 先按**轮数**保留最近若干轮（**不得**按消息条数）；仍超预算则按**完整工具交互组**从最老处继续丢弃（**不得**拆散任何一组）；不可裁部分（系统提示词 / 工具定义 / 当前消息）估算超限 → **拒绝启动**（运行 `failed`、点名上下文预算键、**零模型调用**）；估算器版本常量 `estimateVersion = "v1"`，覆盖**完整出站字段**；准入保留 `reserve_tokens`（缺省 1024）吸收协议包装开销。
- **Rationale**: 技术方案 §4.7 + 契约 §6 + 裁决 6（**不宣称**「不会低估」/「保守上界」——改以五类夹具标定 + 人工项 8 用真分词器对拍）。按条数裁剪会把「assistant + N 条 tool」切一半，模型看到孤立的工具结果即幻觉源头。
- **Alternatives considered**: 「字节数 ÷ 2 是保守上界」（裁决 6 明确不通过——中英混排与 JSON schema 的比值分散，上界说法不成立）；按条数裁剪（拆散工具组）。

### D8 单工具超时：三值取小 + 实际建立超时 context + 不新增 Profile 覆盖键

- **Decision**: 生效超时 = `min(30s 缺省, 剩余 Run 预算, 更小的工具请求值)`，并**实际建立**带该截止的 context 传给工具（不是只在结果里写「超时」）；**单工具超时 → Run 继续**（`failed` + `timeout` 审计行 + 失败信息回填模型）；Run 自身取消 / 截止 → `cancelled`。
- **Rationale**: 裁决 5（本 US **不新增** Profile 键——`AGENT.md` 字段表是契约，加键即发明对外概念）+ 契约 §7.3。
- **Alternatives considered**: 新增 `tool_timeout` Profile 键（契约无此字段）；只在包装层记超时不建 context（工具内部阻塞不收敛，进程树杀不掉）。

### D9 进程树生命周期：Windows Job Object 挂起—关联 + Unix 进程组

- **Decision**: Windows 侧顺序固定为 `CreateJobObject` → `SetInformationJobObject(JobObjectExtendedLimitInformation, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)` → **`CREATE_SUSPENDED` 创建子进程** → `AssignProcessToJobObject` → `ResumeThread`；超时 / 取消 → `TerminateJobObject(job)`（整树）；收尾顺序固定：`WaitForSingleObject` 回收 → 关进程 / 线程句柄 → 关 Job 句柄 → 错误 Join 上抛。Unix 侧 `SysProcAttr{Setpgid: true}` + `kill(-pgid)`（组随 exec **原子生效**，无「关联窗口」）。
- **Rationale**: §3.1「落地顺序」（P2 补齐项）——**保证来自关联关系**，不是来自 `killTree(pid)`：先启动再关联的窗口里子进程可能已派生孙进程，那些孙进程不在 Job 内；`KILL_ON_JOB_CLOSE` 是「句柄关掉即杀全树」的兜底。输出限流在**读取中**生效（内存不随输出膨胀），截断标记只写生效上限、**不承诺**原始长度。
- **Alternatives considered**: `TerminateProcess(pid)` 单杀（孙进程存活）；先 `CreateProcess` 再关联（窗口泄漏）；读完整输出再截断（内存无界）。

### D10 真控制台 vs 管道：控制台 API 判定，不猜环境变量

- **Decision**: `IsTerminal` 经**控制台 API**（Windows `GetConsoleMode`、Linux `IoctlGetTermios(fd, TCGETS)`、Darwin `IoctlGetTermios(fd, TIOCGETA)`）判定；真控制台经 `ReadConsole` / `WriteConsole` 读写（不经标准输入输出的字节流假设）；管道 / 重定向两侧**显式** UTF-8；Darwin **独立文件**（常量不同，**不得**与 Linux 合并写 `TCGETS`）。
- **Rationale**: 技术方案 §8.4 + §3.1 平台表；Windows 控制台在重定向后仍可能带句柄但不是控制台、代码页与 UTF-8 不等价——「字节流假设」是真控制台上中文乱码的根因。
- **Alternatives considered**: `mattn/go-isatty`（现为 indirect、**不提升**为直接依赖，§3.3）；`golang.org/x/term`（不在模块缓存、不在技术栈句内——不引入）；Linux/Darwin 合并一个文件（`TCGETS` / `TIOCGETA` 常量不同，编译不过）。

### D11 会话标识派生：规范元组编码 + 长度前缀 + 命令面在准入前算好

- **Decision**: `sha256("basalt/session/v1" ‖ 0x1F ‖ <len(channel)>:channel ‖ 0x1F ‖ <len(user)>:user ‖ 0x1F ‖ <len(profile)>:profile)` 取 hex 前 16 位，前缀渠道名 `<channel>-<hex16>`；**命令面在准入之前**算好（`AgentService` **不派生**）；CLI 的 `user` = 当前 OS 用户名（取不到 → `unknown`）；**空会话标识保留「无状态」语义**（不落会话历史、仍落 Run 与审计）。
- **Rationale**: §6.2 固定口径 1 逐字 + 契约 §5；长度前缀消除拼接歧义（`user=a-b, profile=c` 与 `user=a, profile=b-c` 连字符拼接会撞成同一 id）。
- **Alternatives considered**: 直接拼接三元组（碰撞）；`AgentService` 内派生（破坏「空 = 无状态」的契约语义）；用 RunID 当会话标识（每轮新会话，历史续不上）。

### D12 半截组补齐：先判消息缺失、再查审计分类；只进消息、不进审计

- **Decision**: 判据落在**消息**上（`MissingToolResults(calls, msgs)`，导出——取消路径与恢复路径共用同一实现），**再**按审计行分类；五分支文本逐字取自契约 §4.4（有终态行 → 由 `ResultJSON` 重建 content；`pending` / `unknown` → 「结果未知（进程中断，无法确定是否已产生外部副作用）」+ WARN；无行 + 取消 / 预算 / 中断 → 对应三条文本）；补齐**只进消息、不进审计**；幂等；收尾用**独立有限时长** context（`finishTimeout = 5 * time.Second`，父上下文已取消仍必须写得进去）；补齐失败**返回 error**（调用方记 ERROR，Run 照常收场）。四段文本与 `Reason*` 常量的**唯一**定义处是 `core/synthesis.go`。
- **Rationale**: 流程 E + 契约 §4.4 Option A + 已决事项 8；顺序颠倒会把「执行过但终态回填失败」误判成「未执行」（前者有外部副作用，措辞必须区分——这正是"错报"里代价最大的一种）。
- **Alternatives considered**: 以审计行为主判据（漏掉「有工具调用消息、无审计行」的组）；把补齐写进审计（审计只记真实发生的事）；用父 context 收尾（父已取消则写不进，恢复静默失败）。

### D13 恢复扫描：六步、同一短事务、只改仍 `running` 的行、幂等

- **Decision**: 启动时（取实例锁后、开跑前）扫描——① `pending` 审计行 → `unknown`；② 按**位置**（不按 id）判半截组并补齐；③ 组修复与运行收尾在**同一短事务**内；④ 只覆盖**仍为** `running` 的运行（已结束状态**不得**被覆盖）；⑤ `.backup` / `.staging` 残留判定（`workspace`，回滚 / 前滚）；⑥ 幂等（连跑两次零变化）；`pending → unknown` 与运行收尾两处留痕可查（INFO 日志键 `unknowns` / `repaired_groups` / `closed_runs`）。
- **Rationale**: 流程 G + 契约 §4.3.2 + 已决事项 4（①②③④⑥ 在 `storage`、⑤ 在 `workspace`、`app` 取锁后编排）；同一短事务是「组补齐了但运行没收尾」半截状态的唯一防法；**不得**跨 LLM 调用与工具执行开事务。
- **Alternatives considered**: 按审计行 id 判组（id 是插入序，不代表消息位置——取消后重排即错）；多次独立事务（中间崩溃留半截）。

### D14 装配与依赖方向：取锁 → 打开 → 迁移 → 恢复；名字集注入装载器

- **Decision**: 重路径顺序**固定**：取实例锁 → 写模式打开 → 显式迁移 → 崩溃恢复 → 建 `tool.Registry` → `Names()` 喂 `AgentLoaderOptions.Tools` → 构造 `AgentService`（`Policy` / `Runner` / 四个仓储 / 解析器 / 时钟注入）；轻路径（`session list`、`profile list/show`）**不开数据库写入、不取锁、不恢复**，但**打开库前校验 `schema_version`**。配置键**逐字沿用**既有面：`sandbox.cross_agent_read` / `shell.allowed_commands` / `shell.interpreters` / `shell.env_passthrough` / `http.allowed_domains`（**不得**另造 `sandbox.allowed_domains`）；重定向跳数上限 5 是**代码常量**（不是配置键）。出站请求体按供应商形态追加 `tools` 键，**空池不带该键**（旧行为逐字不变）。
- **Rationale**: §3.5 第 6 项 + 契约 §4.3.1 + 裁决 2 + 改造点 2；`workspace` **不得**反向 import `tool`（兄弟包都指向 `core`），故门禁靠 `app` 装配期注入名字集；恢复必须在开跑前——否则旧 `running` 行与新运行混在一起。
- **Alternatives considered**: `workspace` import `tool`（破依赖方向）；`AgentService` 内部读注册表（`core` 引 `tool`，破核心纪律）；配置再造一套 `sandbox.allowed_domains`（裁决 2 明确不通过）。

### D15 评测基座：YAML 用例 + 可执行断言，fake 默认 / 真模型门控

- **Decision**: `evals/`（仓库根新顶层目录、`package main`）——用例以 YAML 承载（输入 / 夹具 / 预算 / **可执行断言**）；**复用** `AgentService` 与既有审计链（不新建第二套入口）；默认进程内 fake `ProviderClient`（**可重复**、进日常回归）；真模型**显式门控**（`--provider <name>` + 环境变量凭证）；结果文件 `evals/results/<时间戳>.json` 每例含用例 id / RunID / 状态 / 工具序列 / **资源用量**（token 用量 + 调用次数）/ **计量完整性**标记；**不新增评测表**；资源用量**不换算金额**，usage 缺失 / `pending` / `unknown` 标「计量不完整」，**不得**把库中默认零解释为零成本。
- **Rationale**: 裁决 7 / 裁决 8 逐条；「不改称成本」是产品口径（价格表会漂移，token 用量不会）。
- **Alternatives considered**: 用例直接写 `go test`（裁决 7 要 YAML 用例集 + 结果文件）；新增评测表（裁决 7 明确不通过）；真实模型进 CI（不可重复、耗凭证）。

## 写前必须实测核实清单（H3 门禁）

implement 阶段**写码前**逐项在本地依赖内核实（核实不到 → 软门禁停下报告）。可前置核实的项本轮已核实（读数见下节）；余项在对应任务动手前就地复核：

| # | 核实对象 | 本地核实方式 | 状态 |
| --- | --- | --- | --- |
| H1 | `golang.org/x/sys@v0.48.0/windows`：Job Object 面 `CreateJobObject` / `AssignProcessToJobObject` / `SetInformationJobObject` / `TerminateJobObject` + 类型 `JOBOBJECT_EXTENDED_LIMIT_INFORMATION` + 常量 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` / `JobObjectExtendedLimitInformation` | 模块缓存 `zsyscall_windows.go` / `types_windows.go` | 已核实 |
| H2 | 同包：进程面 `CreateProcess` / `ResumeThread` / `WaitForSingleObject` / `GetExitCodeProcess` / `OpenProcess` + 常量 `CREATE_SUSPENDED` / `STARTF_USESTDHANDLES` / `CREATE_NO_WINDOW` | 同上 | 已核实 |
| H3 | 同包：控制台面 `GetConsoleMode` / `SetConsoleMode` / `ReadConsole` / `WriteConsole` / `GetConsoleOutputCP` / `SetConsoleOutputCP` / `GetStdHandle` | `zsyscall_windows.go` | 已核实 |
| H4 | `golang.org/x/sys@v0.48.0/unix`：`IoctlGetTermios` / `IoctlSetTermios` + 平台常量 `TCGETS`（linux/amd64）/ `TIOCGETA`（darwin/amd64） | `unix/ioctl_signed.go` / `ioctl_unsigned.go` / `zerrors_{linux,darwin}_amd64.go` | 已核实（**仅存在性**；Linux / Darwin 分支的运行读数不属本机——CI 绿也**不得**据此宣称 Windows 分支已验，反之亦然） |
| H5 | `github.com/sashabaranov/go-openai@v1.43.0`：`ToolCalls []ToolCall`（`json:"tool_calls,omitempty"`）/ `ReasoningContent`（`json:"reasoning_content,omitempty"`）/ `type ToolCall` / `ToolType` / `FunctionCall` | 模块缓存 `chat.go` | 已核实（入站解析与出站编码的协议类型就位） |
| H6 | `os.Root` 方法面：写入经**相对根句柄**（`os.OpenRoot` → `OpenFile`，**不是**「先校验再按路径写」） | GOROOT `src/os/root.go` | 写前复核（US-3 已登记读数；本 US 用它防「校验与写入之间被换成越界软连接」的竞争窗口） |
| H7 | `encoding/json`：`Decoder.UseNumber` 语义（`ProviderExtra` 往返保真）与 `json.RawMessage` 原样透传（`result_json` / `input_json`） | 标准库 | 写前复核 |
| H8 | `crypto/rand` 6 字符 base36 取值域（`NewRunID` 随机段）与 `crypto/sha256` + `encoding/hex` 前 16 位（会话标识派生） | 标准库 | 写前复核（随机源红线：**不得** `math/rand`，全局不变量 8） |
| H9 | Windows 错误判定：Job 关联 / 恢复失败路径上的 `syscall.Errno` 与 `errors.Is` 判据 | 模块缓存 + 写后用例 | 写前复核 |

## 探针与本地读数（2026-10-16，本轮当场命令）

- **环境**：`go version` → `go version go1.27.2 windows/amd64`（退出码 0；与 `toolchain go1.27.2` 一致）；`GOMODCACHE` = `C:\Users\76562\go\pkg\mod`，`golang.org/x/sys@v0.48.0` 就地在缓存（H3 门禁可离线 grep 方法签名）。
- **`go.mod` 三行读取**：`go 1.27` / `toolchain go1.27.2` / `golang.org/x/sys v0.48.0`（位于**直接依赖**块、无 `// indirect`——US-3 的迁移结论复验通过）；同轮读到 `github.com/mattn/go-isatty v0.0.24 // indirect` 仍在 indirect 块（**不提升**）。
- **零新增坐标的复核依据**：模块缓存与 `go.mod` 的既有坐标集合已覆盖本 US 全部第三方需求（`x/sys` / `go-openai` / `modernc.org/sqlite` / `cobra` / `goccy/go-yaml` / `testify`）——**本轮未跑** `go list -m -versions`（无新坐标需要核实版本）。
- **`x/sys` windows API 读数**（模块缓存源码）：`CreateJobObject`（zsyscall_windows.go:1907）/ `AssignProcessToJobObject`（:1785）/ `SetInformationJobObject`（:3388）/ `TerminateJobObject`（:3492）；`JOBOBJECT_EXTENDED_LIMIT_INFORMATION`（types_windows.go:2572）/ `JobObjectExtendedLimitInformation = 9`（:2607）/ `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x00002000`（:2552）/ `JOB_OBJECT_LIMIT_PROCESS_MEMORY = 0x00000100`（:2555）；`CreateProcess`（zsyscall:1955）/ `ResumeThread`（:3195）/ `WaitForSingleObject`（:3640）/ `GetExitCodeProcess`（:2399）/ `OpenProcess`（:3016）；`CREATE_SUSPENDED = 0x00000004`（types:249）/ `CREATE_NEW_PROCESS_GROUP = 0x00000200`（:243）/ `CREATE_UNICODE_ENVIRONMENT = 0x00000400`（:250）/ `STARTF_USESTDHANDLES = 0x00000100`（:148）/ `CREATE_NO_WINDOW = 0x08000000`（:244）；`GetConsoleMode`（zsyscall:2321）/ `SetConsoleMode`（:3252）/ `ReadConsole`（:3115）/ `WriteConsole`（:3649）/ `GetConsoleOutputCP`（:2329）/ `SetConsoleOutputCP`（:3260）/ `GetStdHandle`（:2678）。
- **`x/sys` unix API 读数**：`IoctlGetTermios` / `IoctlSetTermios`（`unix/ioctl_signed.go:70` / `:48`，另有 unsigned 与 zos 变体由构建约束选择）；`TCGETS = 0x5401`（`zerrors_linux_amd64.go:419`）；`TIOCGETA = 0x40487413`（`zerrors_darwin_amd64.go:1507`）。
- **`go-openai v1.43.0` 读数**：`ToolCalls []ToolCall` + tag `json:"tool_calls,omitempty"`（`chat.go:116`）、`ReasoningContent string` + tag `json:"reasoning_content,omitempty"`（`chat.go:111`，另在 :133 / :147 / :162 的流式结构体上重复出现）、`type ToolCall struct`（`chat.go:190`）。
- **既有实现读数（本 US 改造点的落点）**：`internal/provider/client.go:218` `func (c *OpenAIClient) buildRequestBody(model string, msgs []core.Message) ([]byte, error)`（先 `json.Marshal(openai.ChatCompletionRequest{Model: model})` 取 base，再合并编码后的 messages 信封——**追加 `tools` 键的位置就在这里**）；`internal/provider/client.go:273` `encodeToolCalls` 已用 `openai.ToolCall` / `openai.ToolFunction` 编码**入站解析出的**工具调用（出站回传路径现成）；`internal/core/provider.go:58` `ChatRequest.ConfigHash` 注释仍写「本 US 恒空（§2.4 裁决注 3）」——**本 US 之后由运行期填充**（改造点 2 的语义变更点，注释一并更新）。
- **覆盖率判卷读数**：`scripts/check-coverage.sh` 存在（8147 字节，可执行）；第 12 行注释与第 146 行判据 `core | tool | memory | storage) threshold=70`、其余 `threshold=50`——`core` / `tool` / `storage` 自本 US 起**首次**进入判卷，**无需改脚本或 Makefile**。
- **包目录读数**：`internal/` 现有 11 个包目录（`app` / `channel` / `cli` / `core` / `memory` / `provider` / `storage` / `testutil` / `tool` / `web` / `workspace`）——本 US **不新增内部包**（`evals/` 在仓库根）。
- **如实登记的未探测项**：① 三平台编译命令（§3.3 的 `CGO_ENABLED=0 GOOS=… go build ./...`）本轮**未跑**——属 §6.1 人工项 5；② `make check` 全量门禁本轮**未跑**（实现未落地，跑了也无意义）；③ Windows Job Object 与真控制台的**行为**读数（进程树强杀、中文往返）属实现后的门禁 / 人工项读数，本阶段不声称；④ Linux / Darwin 分支的**运行**只发生在 CI / 真机——`CGO_ENABLED=0 GOOS=…` 能编译通过**不等于**运行正确，人工项 5 只判「编译到」。
