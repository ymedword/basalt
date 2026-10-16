# US-5 Shell 与 HTTP 工具 — 模块执行颗粒度文档

> **文档状态**：草案 **R3**（2026-10-17；R1 由 `/module-doc-gen 5` 产出；R2 = 评审回合裁决逐条落案；R3 = 待决 6 用户裁决落案——端口策略冻结（口径 15）、跨跳端口回归用例、条数同步；**待** `/us-register 5` 登记）。评审回合裁决（原待决 1~5 + P1/P2 + 其余修补）与待决 6 号**全部裁决、编号不复用**，见「评审回合裁决落案表」与「已决事项」——**待决事项已清空，设计面冻结**。「收口补登」按 US-4 的格式预留，收口时补登。
>
> **事实主源顺位（供 `/us-register 5` 派生登记）**
>
> 1. **`docs/TechnicalSolution.md`**（文档之间最权威）——§6.1 工具接口（`Tool` 四方法 / `Result` 两字段**逐字**；截断信息不进结构、走 `Content` 标记）/ §6.2 内置十工具（ShellTools 行：**argv 直执**、可执行名整串逐字、环境变量白名单、超时杀进程树、输出宽容解码；HttpTools 行：域名白名单大小写不敏感、scheme 固定；**工作目录约定**——带 Agent 上下文且目录存在时子进程 cwd = 该 Agent 目录，**不得**引入全局 `chdir`）/ §6.3 三档扩展（本 US 不做）/ §6.4 `ToolRegistry`（显式注册）/ §6.5 授权与执行（`Policy` / `CommandRunner` / `Action` / `Command` / `CommandResult` 逐字；**授权矩阵**——命令档 `bash`/`sh` 不得放行、整串逐字、解释器受控通道的 argv 形态与脚本范围归**执行侧**；出网档——**逐跳校验**、跳数上限 **5**、IP 字面量拒、**不读环境代理变量**、容器化属扩展阶段；`LocalRunner` 执行控制；四段式拒绝）/ §6.6 通知渠道（相邻：`notify` 归 US-8）/ §7 配置面（`http.allowed_domains` / `shell.{allowed_commands,interpreters,env_passthrough}` + **改完需重启**）/ §8.7 CLI 命令（12 个含 `tool list`；轻 / 重两条路——「凡答案涉及**容器内注册表或数据库**就必须走重路径」）/ §10.1 包结构与依赖方向 / §13 实施节奏（US-5 行：任务 = Shell 与 HTTP 工具；可演示成果 = **Demo 一：问天气给穿衣建议**；涉及包 = `tool` / `cli`）
> 2. **`docs/DemandAnalysis.md`**——§1.2 五大核心能力 / §5.6 能力四：Tool 体系（内置十工具表；沙箱**三档** = 路径 / 命令 / 域名；授权与执行分离；四段式拒绝）/ §5.7 Channel（核心阶段 CLI；「⚙ 在干活」可见）/ §5.12 命令行工具（`basalt tool list # 列可用工具`；重命令走完整装配）/ §13.1 功能验收 / §13.4 质量门禁验收（安全关键路径 = 沙箱三档有专项测试）
> 3. **`.specify/memory/constitution.md`**（原则级最高）——I（自实现 ReAct 循环；`go.mod` **不得**出现 Agent 框架坐标——本 US **零新增坐标**）/ II（工具调度唯一归属 `ToolExecutor`；工具只返回结果、**不写消息**）/ IV（一个目录 = 一个 Agent；`use_skill` **永久禁**；声明名无实体 → 整只不注册——工具名字集仍由装配期注入）/ V（审计 day-one 且 **fail-closed**——两阶段写的唯一归属是执行器，工具**不落审计**）/ VI（**授权与执行分离两接口**——工具只持有 `core.Policy` / `core.CommandRunner`，**不得**依赖 `WhitelistPolicy` / `LocalRunner` 具体类型）/ VII（同步执行；context 只承载取消 / 截止 / 追踪；业务状态**必须**显式传递）/ VIII（单二进制纪律；依赖最小化——技术栈表内直接落地、**表外新增坐标先问用户**）/ IX（不用 ORM；已发布迁移只追加——本 US **零迁移**）/ X（范围卡紧，以需求文档 §6 与台账为准）
> 4. **`docs/contracts/`**（**固定字面量**）——`agent-md.md` §5.2 `shell`（参数 `executable` 必填 / `arguments` 逐项原样 / `timeout_seconds` 扩展缺省 30 秒；返回退出码 + stdout + stderr（合并截断到 256 KB）；失败 `sandbox_denied` / `timeout` / `runtime_error`；**argv 直执不经任何 shell**；cwd 与工作目录；环境变量只透传白名单；`bash`/`sh` 不得放行）、§5.3 `http_get` / `http_post`（参数表——**无 timeout 参数**；`http_post` 的 `body` **必填**（与基线一致）、`content_type` 扩展缺省 `application/json`；返回状态码 + 响应体（截断到 256 KB）；失败 `sandbox_denied`（域名不在白名单 / scheme 非 http 或 https / 重定向目标越界 / **跳数超过 5**）/ `timeout` / `external_error`（4xx / 5xx）；**逐跳校验**——每一跳发送前过白名单与 scheme，客户端不得自行跟随；白名单条目与目标 host **必须是域名**（IP 字面量直接拒）；`http_post` 有外部副作用（不自动重试、崩溃恢复不自动重放））、§6 拒绝信息格式（四段式 + 例文 + 指引分叉规则：工作区之外的路径「没有配置键可以加」）；`run-and-errors.md` §3.3（承诺边界三条：不自动重试 / 不自动重放 / 客户端重传即新 Run）、§4.3.1（实例锁与写模式命令清单）、§6 预算表（`ToolTimeout` 30 秒「Profile 可覆盖」/ `MaxOutputBytes` 256 KB / 上下文 32768；**模型只能缩短**）、§7.2（两阶段写与残余窗口）、**§7.3 `error_kind` 七值**（`external_error` = 「外部系统错误 | HTTP 5xx、模型供应商 4xx/5xx」）、§7.4（存 / 不存：工具入参**原样**落 `input_json`）；`schema.sql`（**零改动**：六表 US-1 建齐，`tool_invocations` **无** `round` / `truncated` 列，截断标记在 `result_json` 内部）
> 5. **既有实现事实源（US-4 已交付面；**复用不重造**）**——`internal/tool/policy.go`：`WhitelistPolicy` / `PolicyOptions` / `checkShellCommand`（`bash`/`sh` **硬拒先行** → 多词串拒 → `allowed_commands` 整串比 → `interpreters` 放行到执行侧，`policy.go:212-230`）/ `checkHTTPRequest` + `domainAllowed`（`url.Parse` 失败 → 拒；scheme 只认 http/https（**代码固定，配置放行不了**）；host 为空或 **IP 字面量** → 拒；域名大小写不敏感、`*.suffix` 只匹配子域，`policy.go:233-277`）/ `deny` 四段式（`policy.go:338`）；注释已逐字声明「跳数上限 5 是调用方的代码常量（**US-5 的 http 工具**），本档只判"这一跳去哪"」（`policy.go:231-232`）与「逐跳校验由调用方在每一跳发送前再次 Check（**US-5 驱动**）」（`core/action.go:30`）；`internal/tool/runner.go`：`LocalRunner.Run`（环境白名单、超时 **杀整棵树**、输出**读取中**限流、**截断时由 runner 自行追加 `core.TruncationMarker`**（`runner.go:198-200`）、解释器 argv 形态与脚本真实路径校验；非零退出码**原样交给调用方**）；`internal/tool/registry.go`：`Register` / `Resolve` / `Names`（排序稳定）/ `Specs`（保序）；`internal/tool/filetools.go`：三件文件工具的 `decode + check + policy.Check` 范式与**内联 JSON Schema 常量**（`required` 数组是执行侧的必填判据）、`decodeFileToolInput` 的错误命名形态、`fileToolInput.check` 的空值兜底先例；`internal/core/tool.go`：`Tool` 接口 / `Result{Content, Retryable}`（两字段冻结，截断走 `Content` 标记）；`internal/core/action.go`：`Action{Type, Target, Agent}` / `Command{Executable, Args, Dir, Env}` / `CommandResult{Output, ExitCode, Truncated}` / `ActionHTTPRequest`；`internal/core/tool_exec.go`：`ToolExecutor.Execute`（审计两阶段 → 执行 → 终态回填）、入参必填校验（`missingRequiredField`）、输出截断（`truncateToolOutput`：`len > maxBytes` 才追加标记）、`classifyToolError`（**注释逐字预告**「外部系统错误 **US-5 起**另有 external_error」，`tool_exec.go:228`）；`internal/core/synthesis.go`：`TruncationMarker(maxBytes)`（N = 生效上限，不承诺原始长度）；`internal/app/wire.go`：注册池与 `PolicyOptions` 构造点（**`NewLocalRunner` 至今零生产调用点**——只有 `runner_test.go:199` 构造它，本 US 首次接线）、`RunnerOptions.MaxOutputBytes` 注释提到的 `shell.max_output_bytes` 键**在配置面不存在**（注 3）、`heavyRuntime` / `Runtime` 访问器纪律（访问器一律返回消费方声明的接口）；`internal/app/run.go`：`heavyCommands = ["chat"]`，重路径固定四步「取锁 → 迁移 → 恢复 → 开跑」**各记日志**（重路径判定与调用点 `app/run.go:94-98`，`heavyCommands` 定义在 `app/run.go:36`，注 9）；`internal/cli/{root,provider,ports,deps,config}.go`：命令组范式（组命令 + 子命令 + `render*` 独立可测）、`Deps` 为**值类型**（`root.go:17`，构造器一律 `newXxx(deps Deps)`）、端口定义在**消费方**、`ShellConfig` 只有三键（**没有** `shell.max_output_bytes`）、`HTTPConfig.AllowedDomains` 注释已写「消费归 **US-5** 的 http_get / http_post」、`BudgetConfig.MaxOutputBytes` 键**已存在**且既有接线在 `cli/chat.go:340-345`（运行侧夹取 256 KB 硬上限在 `core/run.go:297-299`，逐 Run 填充 `ToolEnv.MaxOutputBytes` 在 `run.go:186`）；`internal/cli/ports_test.go`：依赖方向判卷（禁 import 清单 = storage / workspace / provider，**本 US 增 `tool`**）
> 6. **附加事实源**——`docs/Compatibility.md` §2.2 shell 行（`timeout_seconds` 只能是「**缩短**」，见效值为三者最小）/ §5.1 共享验收样例（判据 2 = 10 个名字全部命中；装载器的名字集由**注入**决定——本 US 不改样例、不改判据）；`docs/Operations.md` §2.2（「改完需重启」清单含 `http.allowed_domains` 与 `shell.*` 各键——白名单在**装配期一次构建**，本 US 的配置生效时机因此是重启）/ §4.3 恢复演练判据 4（`basalt tool list` 或 `GET /api/v1/tools` 非空 = 工具注册表非空；「判据 4、5 **必须**走同一条路径，不得混用」）；`docs/AiProgrammingGuide.md` §4.5（US-5 完整定义：核心目标「内置十件补齐到六件（File×3 + Shell + Http×2）；**三档授权全部有真实消费方**」/ 涉及包 / task 大类 / 颗粒度「两个工具 + 一个命令；约 8~10 个文件」/ 硬依赖 US-4）/ §7 陷阱表（全 **56** 条；与本 US 直接相关：#47 nosec 字面量、#56 平台分片——本 US 无新平台分片文件）；`docs/EngineeringStandards.md` §1 Go / §2 测试 / §4 日志与错误 / §5 提交
> 7. **落地参考（非权威）**——`docs/Roadmap.md` §4（US 开放序列）；`docs/GettingStarted.md` §2（工具链读数）
>
> **固定裁决句**：本文与最新技术方案冲突时，**以技术方案为准**；本仓配套工件（spec / plan / 已批准 US 清单）已锁定的引用在不与前者冲突时沿用，不另行改判。**契约字面量一律服从契约**——`error_kind` 七值、四段式拒绝格式、两个工具的入参表与失败类别，本文明文冻结的部分只在契约留白处生效。**spec 锚点现状（核对于 2026-10-17）**：`specs/` 下现有 `001-foundation/` / `002-provider/` / `003-agent-profile/` / `004-react-loop/`（US-1~US-4 已完成验收的历史设计，**只读**）；US-5 的 spec 目录由 `/module-dev 5` 新建。
>
> **登记派生信息**：技术方案章节 = §6.1 / §6.2 / §6.3 / §6.4 / §6.5 / §6.6 / §7 / §8.7 / §10.1 / §13；需求文档章节 = §1.2 / §5.6 / §5.7 / §5.12 / §13.1 / §13.4；模块落位（摘要）= `internal/tool`（ShellTools / HttpTools + policy 端口规则与拒绝文案脱敏 + runner 输出预算下传——§3.5 第 9 / 10 项）+ `internal/cli`（`tool list`——技术方案 §13 US-5「涉及包」列）+ `internal/core`（`external_error` / `invalid_input` 承载与失败分类分支 + `Command.MaxOutputBytes`——§2.4 注 4 / 注 11）+ `internal/app`（装配接线：注册池 +3、`LocalRunner` 首次接线含预算接线、工具清单端口适配器——§3.5）；`go.mod` **零新增坐标**（§3.3）。
>
> **台账状态**：登记时 `docs/us/REGISTRY.md` 查无 US-5 行——本章节对照由本 skill 自定位，供 `/us-register 5` 派生。

## 1. 模块概述

**定位**：把内置十件补齐到**六件**——`shell`（argv 直执、可执行名整串逐字白名单）与 `http_get` / `http_post`（域名白名单 + 逐跳校验）三件新工具落进 US-4 交付的 `ToolExecutor → Policy / CommandRunner` 通道，并给命令面补上 `basalt tool list`；沙箱三档（**路径 / 命令 / 域名**）至此全部有真实消费方。

**价值（WHAT / WHY）**：US-5 交付三件可判卷的事：① **首个"真正动手"的通道**——`shell` 让 Agent 能在白名单内跑命令（argv 直执、不经任何 shell、超时或中断杀整棵进程树），`http_get` / `http_post` 让它能取外部数据，两件合起来就是编程指南 §4.5 的验收 Demo（**问天气给穿衣建议**：模型调 `http_get` 取数据 → 据此给出建议）；② **命令档与域名档从条文变成句柄**——US-4 交付了判定与执行两侧的**实现**却没有消费方（`LocalRunner` 至今零生产调用点、`checkHTTPRequest` 的逐跳驱动不存在），本 US 把这两条链路接通，"三档授权全部有真实消费方"这句承诺才成立；③ **工具清单命令**——`basalt tool list` 让"注册了什么"可现场核对，也是运维恢复演练**判据 4** 的读数来源（与判据 5 走同一条路径）。规模参考：约 8~10 个文件（编程指南 §4.5）；本 US 实际交付面见 §3（代码 新增 3 文件 + 改动 12 文件；测试 新增 3 文件 + 改动 7 文件，多于该参考——`external_error` / `invalid_input` 承载、端口规则与拒绝文案脱敏、`LocalRunner` 首次接线（含输出预算下传）与端口适配器同批交付所致，§2.4 注 1）。**硬依赖 US-4（已交付）**——`ToolExecutor` / `WhitelistPolicy` / `LocalRunner` / `Registry` / 端口注入范式；本 US 不消费其他未交付能力（记忆两工具归 US-6、MCP 归 US-7、`notify` 归 US-8、`manage_schedule` 归 US-10）。

## 2. 设计要点与约束

### 2.1 职责边界表

| 维度 | 本 US 职责（做） | 非本 US 职责（不做，归属） |
| --- | --- | --- |
| ShellTools | `tool.ShellTool` 单件：`executable` + `arguments` 组 `core.Command`、**argv 直执**；`timeout_seconds` 只做**收窄**（`context.WithTimeout` 再包一层）；有 Agent 上下文时 cwd = 该 Agent 目录；逐 Run 输出预算随 `Command.MaxOutputBytes` 下传到读取端（约束 28）；把命令档判定交给 `policy.Check` | `WhitelistPolicy.checkShellCommand` 与 `LocalRunner` 本体（US-4 已交付，本 US 只消费）；容器化隔离（扩展阶段 F）；后台 / 异步执行（宪法 VII 明禁） |
| HttpTools | `tool.HTTPGetTool` / `tool.HTTPPostTool` 两件：**逐跳校验循环**（每跳发送前 `policy.Check`——**端口按口径 15 冻结组合判定**：`http` 配省略 / `:80`、`https` 配省略 / `:443` 放行，其余含交叉档一律拒；**初始请求与每一次重定向同判、拒绝先于发送**）、跳数上限 5、响应体按上限读取；`http_post` 的 `Content-Type` 缺省与 `body` 必填；**出站 body 上限 256 KiB**（约束 29，本 US 新增裁决）；**跨跳请求头规则**（约束 30）与仅 301/302/303/307/308 执行重定向（口径 1） | 出网判定本体 `WhitelistPolicy.checkHTTPRequest` / `domainAllowed`（US-4 已交付，本 US 只补**端口规则与拒绝文案脱敏**——§3.5 第 9 项）；凭证注入与密钥托管（扩展阶段）；幂等键与自动重试（核心阶段不做，契约 §3.3；传输层条件重试见注 10） |
| 失败分类 | 工具侧产出**可判定**的两类错误：`*core.ExternalError`（4xx / 5xx 与出网失败）与 `*core.InvalidInputError`（工具入参解码 / 取值校验失败）；`core.classifyToolError` 增两条分支（判序见流程 C） | 七值字面量本身（契约 §7.3 冻结）与**供应商侧**的既有接线（`provider.classifyError` 不动——它会话链已自行产出 `external_error`） |
| 工具注册 | 装配期把注册池补到**六件**（US-4 的三件文件工具 + 本 US 三件） | `Registry` 本体（US-4 已交付）；MCP 工具（US-7）；`notify` / `manage_schedule`（US-8 / US-10）；`use_skill` 宪法 IV **永久禁** |
| 命令面 | `basalt tool` 命令组 + `tool list`（一行一工具 + 末尾汇总；**清单读取本身不修改状态**——命令属重路径，启动四步含日志，注 9） | `GET /api/v1/tools`（US-9）；`tool show`、工具启停与权限编辑（管理台与观测，US-12） |
| 装配与端口 | `wire.go` 注册池 +3、`LocalRunner` **首次接线**（`MaxOutputBytes ← cfg.Budget.MaxOutputBytes`——约束 28）；`heavyRuntime` 增 registry 只读访问器；`cli` 新增工具清单端口 + `app` 侧适配器；`heavyCommands` 增 `"tool"` | 热加载装配与原子发布（US-11）；`serve` / `gateway` 的 REST 装配（US-9） |
| 审计 | **零改动**——工具不写审计、不新增关联标识参数；两阶段写与失败分类的落库全在 `ToolExecutor` | 审计查询端点与界面（US-9 / US-12）；`runs` / `llm_calls` 面的任何改动 |

### 2.2 关键约束清单

1. **argv 直执，不经任何 shell**（契约 §5.2「argv 直执」/ 技术方案 §6.2）：`core.Command{Executable, Args}` 两字段**分开**承载，`Args` 逐项原样传；工具**不得**拼接命令串、**不得**调用任何 shell、**不得**经 `sh -c` 形态转发（评审修补：原判据「Args 出现 `-c` 即判红」过宽——`git -c color.ui=false status` 是**正常 argv**，`-c` 只是 `git` 的普通参数，按原判据会把整类正常命令判死）。判据分两层：① **shell 解释执行零路径**——`sh` / `bash` 在策略侧被硬拒（约束 3），工具侧不存在 shell 调用点（代码面无 `exec`/`os/exec` 之外的执行路径，执行唯一归属 runner）；② **解释器通道限形态**——仅对 `shell.interpreters` 名单内的可执行名，判 `arguments[0]` 不得以 `-` 开头（`-c` / `-m` / `-e` 内联代码形态即拒——`runner.go:285-299` 既有实现，属执行侧）。**不得**把判据扩成「Args 里出现 `-c` 形态即判红」。
2. **可执行名整串逐字**（契约 §5.2 / 技术方案 §6.5 授权矩阵）：`Action.Target` 传 **`executable` 整串**（不看 argv），策略侧"多词串必拒"因此天然成立；工具**不得**在本地拆分 executable——把 `bash -c "curl …"` 拆成 `bash` + 参数正是绕开命令档的标准姿势。症状：一旦本地拆分，"整串逐字"在白名单比对里就失效了。
3. **`bash` / `sh` 不得放行（含 `-c` 形态）**（契约 §5.2 / 技术方案 §6.5）：判定归 `WhitelistPolicy`（`policy.go:215` 硬拒**先行**——先查白名单会让 `allowed_commands: [bash]` 这个配置错误变成放行，`policy.go:207-211` 逐字写明该顺序的理由）；工具**不得**在本地放行任何 shell 形态。
4. **授权判定只经 `policy.Check`**（宪法 VI）：工具持有 `core.Policy` 接口，**不得** import 或类型断言 `*WhitelistPolicy`；执行只经 `core.CommandRunner`，**不得**自建 `exec.CommandContext`（那是第二套执行路径，绕开环境白名单、限流与整树终止三条保证）。
5. **`timeout_seconds` 只能缩短**（契约 §5.2 扩展字段 / 契约 §6「模型只能缩短」/ Compatibility §2.2）：生效值 = `min(env.Timeout, 正值)`，其中 `env.Timeout` 已由执行侧取「30 秒缺省 / 剩余运行预算 / 工具请求值」三者更小者（`tool_exec.go:100-107`）；**只有更短时**才再包一层 `context.WithTimeout`。症状：`timeout_seconds: 3600` 把墙钟预算拉长到一小时。
6. **`timeout_seconds` 非正值按缺省**（§6.2 固定口径 5）：不报错、不新增错误路径——契约 §7.3 的 `invalid_input` 由执行器的入参校验产出，工具内再产一条会与既有口径分叉。
7. **cwd 约定**（技术方案 §6.2）：有 `env.AgentDir` 且目录存在 → `Command.Dir = AgentDir`；无 Agent 上下文 → 空（进程默认 CWD）；**不得**引入全局 `chdir`。
8. **子进程环境变量只透传白名单**（契约 §5.2 / 技术方案 §6.5）：`Command.Env` 恒为空（`shell` 契约没有 env 参数），透传由 `LocalRunner` 按 `shell.env_passthrough` 过滤——代理与凭证类变量因此默认不进子进程；工具**不得**读 `os.Environ()`、**不得**读代理变量。
9. **超时 / 中断两条路径杀进程树**（技术方案 §6.2 / FR12）：归 `LocalRunner`（实现与用例 US-4 已交付）；工具只把 ctx 传下去，**不得**自己起进程、**不得**自己杀进程。
10. **非零退出码不是错误**（`core.CommandResult` 三字段 / `runner_test.go:316-324` 既有口径）：工具把 `ExitCode` 与 `Output` 如实放进结果内容；**不得**把非零码映射成 `error`——否则"没匹配到"（`grep`）会全变成失败。
11. **截断的责任分工（shell）**（`runner.go:198-200` + `tool_exec.go:128`）：runner 在**限流点**追加标记、执行器按 `env.MaxOutputBytes` 必要时再截一次并追加标记；工具**原样透传** `CommandResult.Output`，**不得**自行截断或追加标记（会出现两个标记）。
12. **截断的责任分工（HTTP）**（§6.2 固定口径 7）：响应体按 `env.MaxOutputBytes + 1` 字节读取——多读 1 字节使执行器的 `len > maxBytes` 判据**必定命中**，标记恰有一次且 N = 生效上限；工具**不得**自行追加标记。症状：读满上限的响应体被静默截断而模型看不到任何标记。
13. **逐跳校验**（契约 §5.3 / 技术方案 §6.5 / `core.Action` 注释逐字）：**每一跳在发送前**调一次 `policy.Check(ctx, Action{Type: ActionHTTPRequest, Target: 本跳 URL, ...})`；客户端**不得**自行跟随重定向（`CheckRedirect` 返回 `http.ErrUseLastResponse`）。判据：假 doer 记录的请求次数与假策略记录的判定次数逐跳对齐（越界的那一跳**必须**在发出之前被拦）。
14. **跳数上限 5，超限判 `sandbox_denied`**（契约 §5.3「跳数超过 5」/ 技术方案 §6.5）：常量 `maxRedirectHops = 5` 写在**工具侧**（`policy.go:231-232` 注释已声明该常量归 US-5）；超限与越界都归 `sandbox_denied`，**不是** `external_error`。口径见 §6.2 固定口径 4。
15. **scheme / host / 端口 / IP 字面量 / 域名判定归 policy**（技术方案 §6.5「每一跳在发送前校验 scheme + host + 端口」；`policy.go:233-277`）：工具把**完整 URL** 作为 `Action.Target` 传入；**不得**在本地预判任意一项（两份判定必然漂移）。文案里的目标值由 policy 决定（域名不在白名单时只写 **host**）——工具**不得**裁剪或改写目标串。**端口一项现状与方案不符**（评审 P1-3）：`checkHTTPRequest` 现在**没有**端口规则——本 US 把它补成**前序改造项**（§3.5 第 9 项）；策略已由用户裁决**冻结**（口径 15）：`http` 配省略端口 / `:80`、`https` 配省略端口 / `:443` 放行，其余组合（含 `http:443` / `https:80` 交叉档）一律拒；初始请求与每一跳同判、拒绝先于发送；指引固定写「当前版本不支持非默认端口」、**不得**指向域名白名单。本 US **不得**继续声称「Policy 零改动、端口已被覆盖」。另：策略拒绝分支的目标值**必须脱敏**（现读 `action.Target` 会把 userinfo / query 回显进四段式文案——`policy.go:236/245` 两处，§3.5 第 9 项同批修）。
16. **不读环境代理变量**（技术方案 §6.5 授权矩阵 HTTP 行逐字）：`http.Transport.Proxy` 显式置 **`nil`**。症状：`HTTP_PROXY` 一设，出网就从"白名单域名直连"变成"经代理发到任意地址"，域名档形同虚设。
17. **HTTP 工具不新增超时参数**（契约 §5.3 参数表逐字）：`http_get` / `http_post` **不得**出现 `timeout` 入参或 Profile 覆盖键；超时由执行侧 ctx 一体承担（工具**不**再包一层）。
18. **`http_post` 的 `body` 必填**（契约 §5.3「与基线一致」）：`required: ["url", "body"]` 写在 schema 里，判据由**执行器**的必填校验执行（缺失 → `invalid_input`）；空串是合法值（在场即算），工具**不**二次判空。
19. **4xx / 5xx → `external_error`**（契约 §7.3 + §5.3 失败表）：工具把状态码包成 `*core.ExternalError` 返回；**不得**把 5xx 当"成功带回内容"（模型会把错误页当数据用）。失败文案形如 `HTTP <status>：<host>`——**不含** query / 响应体 / Location 原文（口径 9、10）；`ExternalError` 带原因链（`Cause` + `Unwrap`）且 `Error()` 只输出安全摘要（口径 17）——`Cause` 供分类与排障，**不进文案**。
20. **DNS / 连接 / TLS / 响应读取失败 → `external_error`；取消与截止时间保留错误身份、优先归 `timeout`**（评审裁决 #3；既有先例 `provider/client_test.go:236`）：出网失败是外部系统事实，不是本仓缺陷；归 `runtime_error` 会把"外部依赖故障"与"本仓 bug"混成一列。落地顺序**先识别 ctx 再包装**：`doer.Do` 与响应体读取两个错误点都先判 ctx 身份——命中即上抛**身份哨兵本体**（`context.Canceled` / `context.DeadlineExceeded`），**不得**上抛传输层的包装错误：`url.Error` 的原文里带整条 URL（含 query），而失败文案会回填进模型可见的对话、进 WARN 日志与审计 `error_message`（2026-10-17 用户实测回合收紧，落案 #14）；`errors.Is`（乃至 `==`）的分类能力由哨兵本体保留，身份与文案都归 ctx，执行器分类归 `timeout`；其余才包 `*core.ExternalError`。**不得**先包装后判定（包完再靠 `Unwrap` 兜回来的形态把 ctx 语义先写进了外部错误文案，且依赖后续实现不变）。`Unwrap` 仍要实现——两条防线并存：即便未来某处把 ctx 错误经 `%w` 包进 `Cause`，`errors.Is` 也能穿透判出。
21. **审计：工具不写**（宪法 V / 契约 §7.2）：`tool_invocations` 的 `pending` 预写与终态回填唯一归属 `ToolExecutor`；工具签名里**没有**审计关联标识参数（`env.RunID` / `env.SessionID` 由执行器持有）；工具入参由执行器**原样**落 `input_json`（契约 §7.4）。
22. **工具不写消息**（宪法 II / FR17）：工具只返回 `core.Result`；工具结果消息的唯一追加点是循环。
23. **失败信息回填闭环**（FR7 / `tool_exec.go:126`）：工具返回的 `error` 文本会被**原样**回填给模型；四段式拒绝文案**不得**吞掉、替换或泛化（`core/errors.go:51-58` 逐字）——模型看不到"为什么被拒"就不会收敛。
24. **显式传参，不用 context 承载业务状态**（宪法 VII）：工具经 `core.ToolEnv` 显式取 `AgentName` / `AgentDir` / `Timeout` / `MaxOutputBytes`；`context.WithValue` **零使用**（追踪标识除外）。
25. **依赖方向与端口**（`cli/ports_test.go` / US-4 §2.1）：`cli` **不得** import `tool`（工具清单经 `cli.ToolLister` 端口 + `app` 适配器）；`tool` 只 import `core` 与标准库；`core` 只依赖标准库（判据 `go list -f '{{.Imports}}' ./internal/core` 只出标准库）。
26. **零新增坐标 / 零配置键 / 零迁移 / 零新表**（宪法 VIII / IX；编程指南 §4.5）：只用标准库 `net/http`；`basalt.yaml` 的键名**逐字沿用**（`http.allowed_domains` / `shell.{allowed_commands,interpreters,env_passthrough}`），不新造键；白名单在装配期一次构建 ⇒ **改配置的生效时机 = 重启**（Operations §2.2 清单）；`schema.sql` 与迁移目录零改动。
27. **可观测性**（工程规范 §4 / FR27）：工具本体**不记日志**（失败路径由 `ToolExecutor` 的 WARN 固定键承载，键集不变）。**`tool list` 与 `session list` 不同档**（评审修补）：`session list` 是轻路径（不取锁、不迁移、不恢复——FR24）；`tool list` 属**重路径**（注 2），启动即走「取锁 → 迁移 → 恢复 → 开跑」四步并**各记日志**（重路径判定与调用点 `app/run.go:94-98`）——准确表述是「**清单读取本身不修改状态**」，**不得**写成"整个命令零写、零日志"。日志与错误文案**不得**回显凭证 / userinfo / query（脱敏回归见 §3.2；本 US **不注入**运维凭证，模型在 `headers` / `body` 里自带的敏感数据按契约进审计——边界如实见 §3.4）。
28. **逐 Run 输出预算到达读取端**（评审裁决 #4；注 3）：装配期 `RunnerOptions.MaxOutputBytes` = `budget.max_output_bytes`（**不得**传 0 让读取侧恒用 256 KB 缺省）；`core.Command` 增 `MaxOutputBytes` 字段（§3.5 第 10 项前序改造点），shell 工具填 `env.MaxOutputBytes` ⇒ **读取侧上限 = min(装配值、该 Run 生效预算、256 KB 硬上限)**，越短越先停下。runner 取"装配值与逐命令值的更小者"并夹到 256 KB。HTTP 工具不经 runner——读取侧直接按 `env.MaxOutputBytes + 1` 限流（逐 Run 天然生效）。
29. **出站请求体上限 256 KiB（按 UTF-8 字节计）——本 US 新增裁决**（评审裁决 #5；**不冒称契约已有规定**）：`http_post` 的 `body` 超过 `maxRequestBodyBytes = 256 * 1024` 字节 → `*core.InvalidInputError` ⇒ `invalid_input`、**零发送**。保护范围写清：① 上下文预算**约束不了**模型给出的工具参数大小，此上限补的正是这一缺口；② 判定在工具入参校验段，而执行器的审计预写（`pending`）发生在工具执行**之前**——所以超限的效果是"不发出去 + 该行终态记 `failed` / `invalid_input`"，**不是**"库里零痕迹"；③ 它挡的是"把超大 body 发给外部系统"，不是"阻止审计留下这条调用"。
30. **跨跳请求头规则**（评审裁决 #1 / P1-2；冻结点在 `httpRequestSpec.next` 一处，口径 11）：模型提供的请求头**只随首跳发出**；跳转请求仅当目标与**当前跳同源**（scheme + host + 端口三者全同）时才转发模型头，**跨源一律不转发任何模型提供的头**（认证类头无法从名字可靠识别，宁严勿漏——标准库 `net/http` 跨域跳转剥 `Authorization` / `Cookie` 等即同款理由）；301/302/303 转 `GET` 时同时清除正文相关头（`Content-Type` / `Content-Length` / `Content-Encoding`）。单跳内模型头逐项 `Header.Set`、不设头部白名单的既有口径不变（`Host` / `Content-Length` 由 `net/http` 自管）。

### 2.3 核心逻辑

本 US 核心逻辑为**四条流程**：A `ShellTool.Execute`（命令档驱动）、B `executeHTTP`（出网档驱动，两件 HTTP 工具**共用同一份循环**）、C `classifyToolError`（失败分类增**两条**分支）、D `runToolList`（命令面）。伪代码中的方法名与 §3.1 交付物清单**逐字一致**。

**留痕顺序（宪法 V / 契约 §7.2）**：流程 A / B 都在 `ToolExecutor.Execute` 的审计预写**之后**被调用——`pending` 行先落库、再执行工具（`beginInvocation` → `tool.Execute`，`tool_exec.go:94-109`；行形态由 `pendingInvocation` 组装，`:195-204`）；预写失败则**不调用工具**、直接回填"审计不可用"（`refuseInvalidInput` 同样先落行再返回，`:155-160`）。工具**不写审计、不写消息**（约束 21 / 22）——本 US 的伪代码里出现不了留痕步骤，因为留痕这件事整个归属执行器，这是设计而非遗漏。

#### 流程 A — 命令驱动（`internal/tool/shelltools.go`，`ShellTool.Execute`）

```text
ShellTool.Execute(ctx, env, input)
  │
  ├─ ① in, err = decodeShellToolInput(input)
  │     └─ 解码失败（字段类型不符 / 结构不符）→ 返回 *core.InvalidInputError
  │        （⇒ invalid_input——评审裁决：`arguments: "oops"` 之类给错类型不能落成"本仓缺陷"；
  │         FileTools 的 decodeFileToolInput 同批改载体，§3.5 第 11 项，注 11）
  │        必填缺失**不在这里判**：required 数组由执行器先判（入口更早）
  │
  ├─ ② err = t.policy.Check(ctx, Action{Type: ActionShellCommand,
  │        Target: in.Executable /* 整串，不看 argv */, Agent: env.AgentName})
  │     └─ 拒 → *core.SandboxViolationError（四段式）**原样**上抛
  │           → 执行器分类 sandbox_denied → 审计 failed + 文案回填给模型 → Run 照常继续
  │
  ├─ ③ ctx, cancel = narrowTimeout(ctx, env.Timeout, in.TimeoutSeconds)   # 只缩短；defer cancel()
  │     └─ 非正值 → 用 env.Timeout；正但 ≥ env.Timeout → 不包（执行侧超时已更短）
  │
  ├─ ④ cmd = t.command(env, in)
  │     └─ Command{Executable: in.Executable, Args: in.Arguments /* 逐项原样 */,
  │                Dir: env.AgentDir /* 存在才填，否则空 = 进程默认 */, Env: nil /* 契约无 env 参数 */,
  │                MaxOutputBytes: env.MaxOutputBytes /* 逐 Run 读取上限——约束 28，前序改造点 */,
  │                TimeoutSeconds → 已体现在 ③ 的 ctx 上}
  │
  ├─ ⑤ res, err = t.runner.Run(ctx, cmd)
  │     ├─ err → 原样上抛：ctx 错误 → timeout（执行器口径）；启动失败 / 解释器形态 / 脚本越界 → runtime_error / sandbox_denied
  │     └─ **非零退出码不是 err**——它是结果的一部分（约束 10）
  │
  └─ ⑥ return core.Result{Content: shellContent(res), Retryable: false（§6.2 口径 2）}
        └─ shellContent：首行「退出码：<N>」，换行后接 res.Output（含 runner 已追加的截断标记）
```

#### 流程 B — 出网逐跳驱动（`internal/tool/httptools.go`，`executeHTTP`）

```text
executeHTTP(ctx, toolName, policy, doer, env, spec)          # spec = {Method, URL, Body, ContentType, Headers}
  │                                                          # http_get / http_post 只差初始 spec
  hop = 0
  loop:
  ├─ ① err = policy.Check(ctx, Action{Type: ActionHTTPRequest, Target: spec.URL, Agent: env.AgentName})
  │     └─ 拒（scheme 非 http/https · host 为空或 IP 字面量 · **端口不是该 scheme 的默认端口**
  │        （含 `http:443` / `https:80` 交叉档——口径 15 冻结；首跳与每一跳目标同判）·
  │        域名不在白名单）→ 四段式上抛 → sandbox_denied
  │        ※ scheme / 端口 / 域名的判定**只此一处**（约束 15）：工具不预判、不改写目标串
  │
  ├─ ② req = http.NewRequestWithContext(ctx, spec.Method, spec.URL, body)
  │     ├─ headers 逐项 req.Header.Set（§6.2 口径 11；**跨跳规则见约束 30**——next() 产出的 spec
  │     │   已把"是否带模型头"决定好：首跳与同源跳带、跨源跳空集）
  │     └─ POST 恒设 Content-Type = spec.ContentType（缺省 application/json；**空串 body 也设**——口径 19）
  │
  ├─ ③ resp, err = doer.Do(req)         # 生产 doer = newHTTPClient()：Proxy 恒 nil + CheckRedirect=ErrUseLastResponse
  │     ├─ 命中 ctx 身份 → 上抛**哨兵本体**（Canceled / DeadlineExceeded；**不得**回传带整条 URL 的
  │     │   包装错误——身份优先，分类归 timeout；约束 20）
  │     └─ 其余 err → *core.ExternalError{Cause: err, Target: "<host>", Reason: "<自产类别>：<host>"}
  │           （DNS / 连接 / TLS 归 external_error——口径 3；文案只出**自产类别 + host**，
  │            不回显 err 原文——`url.Error` 的原文里带整条 URL——口径 17）
  │
  ├─ ④ 按 status 分三条路（**每条路都必须 Close resp.Body**——连接不泄漏）：
  │     ├─ 301 / 302 / 303 / 307 / 308（**只有这五种**——评审裁决 #1；304 等**不跳转**）：
  │     │    ├─ hop++；hop > maxRedirectHops(=5) → deny（**sandbox_denied**，不是 external_error）
  │     │    ├─ Location 缺失 / 无法解析 → *core.ExternalError（外部系统给不出目标；**不回显 Location 原文**）
  │     │    ├─ 相对 Location 按**当前 URL**解析（ResolveReference）
  │     │    ├─ spec, err = spec.next(status, target)  # 方法 / body / 请求头的跨跳语义集中此处（口径 1 + 约束 30）
  │     │    └─ continue（下一圈从 ① 重新判定——**没有**任何跳过判定的快路径）
  │     ├─ ≥ 400 → *core.ExternalError{Target: "<status> <host>", Reason: "HTTP <status>：<host>"}
  │     │    （文案不含 query / 响应体 / Location——§6.2 口径 9、10）
  │     └─ 其余（2xx 与**未被列为跳转的 3xx**，如 304）→ 普通返回路径：
  │            n, readErr = io.ReadAll(io.LimitReader(resp.Body, int64(env.MaxOutputBytes)+1))   # 多读 1 字节（约束 12）
  │            ├─ readErr 先判 ctx（同 ③ 的两分支：哨兵本体上抛 / *core.ExternalError{Cause: readErr, …}）
  │            │   ——**不得**丢弃读取错误（原稿 `n, _ =` 形态被评审否决：EOF 与取消读成同一种"读完了"）
  │            └─ return Result{Content: "HTTP " + resp.Status + "\n" + string(n)}
  │
  └─ ★ 取消传播：ctx 取消 → doer 的请求与响应体读取都随 ctx 中断，Execute 在限时内返回取消错误
        （**不得**起后台 goroutine 继续读、**不得**把取消读成"读完了"——宪法 VII 同步执行）
```

#### 流程 C — 失败分类（`internal/core/tool_exec.go`，`classifyToolError` 增两条分支）

```text
classifyToolError(err)
  ├─ err == nil                        → ""（成功留空 → 列写 NULL）
  ├─ IsSandboxViolation(err)           → sandbox_denied
  ├─ IsInvalidInput(err)               → invalid_input    # ← 本 US 新增：工具解码 / 取值校验的承载（注 11）
  ├─ ctx.Canceled | DeadlineExceeded   → timeout      # 位置**不动**：出网超时仍归 timeout，不被外部分支截胡
  ├─ IsExternalError(err)              → external_error   # ← 本 US 新增，插在 ctx 判定**之后**
  ├─ fs.ErrNotExist(err)               → not_found
  └─ default                           → runtime_error
```

判序说明：`invalid_input` 与 `sandbox_denied` 同段位——两者都是**本仓自产、文案要回填给模型**的拒绝（工具解码失败与策略拒绝）；ctx 判定仍插在 `external_error` **之前**（取消 / 超时的身份优先——约束 20 的"先判后包"在工具侧已保证不混装，此处是第二道防线）。

#### 流程 D — 工具清单（`internal/cli/tool.go`，`runToolList`）

```text
runToolList(out io.Writer, deps Deps)          # Deps 为**值类型**（与 root.go 既有构造范式一致）
  ├─ deps.Tools == nil → 返回错误（装配缺件：工具清单只在重路径存在——§2.4 注 2）
  ├─ specs = deps.Tools.Tools()                 # app 侧实现 = registry.Specs(registry.Names())
  │     └─ 顺序由实现侧给（Names() 排序稳定）——命令面**不得**重排、不得依赖 map 遍历
  └─ renderToolList(out, specs)
        ├─ 逐行「<名称>：<描述>」（描述取 ToolSpec.Description）
        ├─ 末尾空行 + 「共 N 件工具」（判定 4 的读数：N 即注册表规模）
        └─ 空池 → 「未注册任何工具」（重路径下不可达的防御分支）
      ※ 命令属重路径：启动已完成取锁 / 迁移 / 恢复三步（各记日志）——"只读"指的是
        **清单读取本身不修改状态**（注 9），不是"整个命令无副作用、无日志"
```

### 2.4 差异裁决注与补记

**注 1（规模参考差异）**：编程指南 §4.5 的颗粒度参考是"两个工具 + 一个命令；约 8~10 个文件"。本 US 实际交付 **代码：新增 3 文件 + 改动 12 文件；测试：新增 3 文件 + 改动 7 文件**，多出的是非"两个工具一个命令"的若干面：`core` 的 `external_error` / `invalid_input` 承载与分类分支（契约 §7.3 已冻结字面量、工具链却无承载，`tool_exec.go:228` 注释逐字预告归 US-5；入参类型错现落 `runtime_error` 与语义不符——注 11）、`LocalRunner` 的**首次生产接线**（US-4 交付实现时无消费方）与逐 Run 输出预算下传（约束 28）、`policy` 的端口规则与拒绝文案脱敏（技术方案 §6.5 要求端口校验、代码没有——§3.5 第 9 项）、`cli` 端口 + `app` 适配器（依赖方向不允许命令面直接 import 实现包）。差异是设计使然，不"修正"编程指南的参考值。

**注 2（`tool list` 走重路径的推导链）**：需求 §5.12 把命令分成两类——「重命令（涉及容器注册表或数据库）走完整装配」；技术方案 §8.7 的同一条口径写作「凡答案涉及**容器内注册表或数据库**就必须走重路径」；本 US 的"工具清单"答案来自**装配期构建的内存注册池**（`Registry`），重路径之外根本不存在 ⇒ `tool list` 属重路径，`heavyCommands` 增 `"tool"`（`commandName` 取首个非选项参数 = 组名，与既有 `readStoreCommands = ["session"]` 同形）。佐证：Operations §4.3 判据 4 把 `basalt tool list` 与 `GET /api/v1/tools` 并列，并规定「判据 4、5 **必须**走同一条路径」——判据 5 的 `session list` 之外的读面只有重路径一条。**不得**为它写轻路径第二套装配（技术方案 §8.7「不得为同一语义写两份实现」）。

**注 3（`LocalRunner` 的接线：接配置预算，不接 0）**：本仓 `NewLocalRunner` 的调用点只有 `internal/tool/runner_test.go:199`——US-4 交付了实现与用例，**零生产调用点**（无消费方）；本 US 在装配期首次接线。评审**否决**了"传 0 → 包内缺省 256 KB"的提案：读取侧**必须**接 `budget.max_output_bytes`——配置面的键已存在（`BudgetConfig.MaxOutputBytes`，`cli/config.go`），运行预算的既有接线见 `cli/chat.go:340-345`，运行侧夹取 256 KB 硬上限见 `core/run.go:297-299`，逐 Run 值填 `ToolEnv.MaxOutputBytes` 见 `run.go:186`。**不得**让读取侧恒用 256 KB、只在执行器末端才截断（原提案的后果：配置收紧了预算，子进程仍把 256 KB 读完）。逐 Run 更小的预算经 `core.Command.MaxOutputBytes`（§3.5 第 10 项前序改造点）到达读取端：shell 工具填 `env.MaxOutputBytes`，runner 取"装配值与逐命令值的更小者"（并夹到 256 KB 硬上限）。`RunnerOptions` 注释里的键名 `shell.max_output_bytes` **在配置面并不存在**（`ShellConfig` 只有 `allowed_commands` / `interpreters` / `env_passthrough` 三键）——"Profile 收紧 budget"一说同样不成立（Profile 只覆盖 `settings.max_iterations`，契约 §6）：**预算口径的出口是运维配置**，本 US **不新增配置键**。

**注 4（`external_error` 的承载落 `core`，不复用 `provider.CallError`）**：`internal/provider/client.go:536` 已有 `CallError{Kind core.ErrorKind…}`，供应商侧自 US-2 起就产出 `external_error`（4xx / 5xx / DNS / 连接失败 / 响应不可解析）。工具链**不得**复用它：`core` 不得 import `provider`（包依赖方向），`tool` 也不该认识 provider 的内部类型。故 `core` 新增**平级**承载 `ExternalError` + `IsExternalError`（与 `SandboxViolationError` / `IsSandboxViolation` 同款：判定只认类型、不比文本）。供应商侧既有接线（`provider.classifyError` 已信任 `CallError.Kind == external_error`）本 US **一字不动**。

**注 5（网络层失败的归类有既有先例）**：契约 §7.3 给 `external_error` 举的例子只写了「HTTP 5xx、模型供应商 4xx/5xx」，DNS / 连接 / TLS 失败未点名。本仓已有先例：`internal/provider/client_test.go:236` 断言「DNS / 连接失败归 external_error」，`client.go:135` 的实现同源。HTTP 工具取**同口径**（§6.2 固定口径 3）——这不是新发明，是既有实现事实的延续。

**注 6（截断标记的"恰一次"由谁保证）**：本仓现有两条路径各追加一次标记，且**不会**叠加——runner 在限流点追加（`runner.go:198-200`），执行器在 `env.MaxOutputBytes` 处截断时追加（`tool_exec.go:128`，判据是 `len(content) > maxBytes`）。约束 28 的 min 链（runner 读取上限 ≤ 执行器截断上限）保证 shell 侧"runner 的标记恰好落在外层截断点之外"：外层截断把它连同多出的字节一起切掉、只留自己的标记；若配置把 runner 上限收得更短，内容到执行器时已带标记、长度可能不再超限，标记来自 runner——**两种形态都恰一次**。HTTP 侧没有上游限流点，故本 US 用**多读 1 字节**（`env.MaxOutputBytes + 1`）让执行器的判据必定命中。**预算口径 = 最终 `Content`**（含首行 `HTTP <status>` / `退出码：<N>` 的开销，口径 7 / 8）——三条路径的终态都是"标记恰一次、N = 生效上限、不承诺原始长度"，链路断言见 §4.3 R3。

**注 7（`Retryable` 全仓无消费方）**：`Result.Retryable` 目前只在 `tool_exec.go:131/137` 透传、在 `synthesis.go:63` 落 `result_json`（`retryable,omitempty`），**没有任何控制流读它**（无重试机制）。本 US 两件工具一律取 `false`（评审裁决 #2），语义写定为「**当前不给出可重试建议**」——**不得**把该字段读成"运行时会不会自动重试"（运行时零重试是契约 §3.3 的独立承诺，与本字段无关；传输层的条件重试见注 10）。

**注 8（`ports_test.go` 的禁 import 清单扩面）**：`internal/cli/ports_test.go` 当前的清单是 `storage` / `workspace` / `provider`——不含 `tool`（US-4 时命令面还没有取值工具元数据的动机）。本 US 给命令面加了工具清单，最容易顺手写下的形态正是 `import "…/internal/tool"` 取 `ToolSpec` 渲染。故本 US 把 `tool` 加进清单——这是判据**扩面**（覆盖新面），不是放宽阈值。

**注 9（`tool list` 的命令副作用：重路径 ≠ 零日志）**：`tool list` 属重路径（注 2），`app/run.go:94-98` 的重路径固定顺序「取锁 → 写模式打开 → 迁移 → 恢复 → 开跑」**四步各记日志**。故"只读"的准确表述是「**清单读取本身不修改状态**」：命令自身不写库、不改工作区，但启动阶段有取锁、迁移、恢复三件事（含日志）——这是重路径的既有成本，不是本 US 引入的。**不得**把 `tool list` 与 `session list`（轻路径：不取锁、不迁移、不恢复——FR24）写成同档。

**注 10（应用层不重试 ≠ 只发送一次；与契约 §3.3 例外口径对齐）**：工具与执行器**没有**任何重试 / 重发循环（应用层零重试）；但标准库 `http.Transport` 对**幂等请求**在"连接复用失败"（对端在 keep-alive 连接上关闭）时会自动重发：幂等判定 = 方法属 GET / HEAD / OPTIONS / TRACE，**或**请求带 `Idempotency-Key` 头。本 US **不**自定义该行为（自有 Transport 用默认配置，未关该机制），如实登记：① `http_get` 天然落在该口径内；② `http_post` 若模型在 `headers` 里放了 `Idempotency-Key`，传输层重发**可能**发生——这正是契约 §3.3「供应商 SDK 自带的传输层重试除外，且必须收敛在单次调用内部，不产生第二次审计行」的同款例外（重发发生在**一次**审计行之内）。"代码里没写重试循环"**不能**证明"只发送一次"——这句边界写进文档，不写进承诺。

**注 11（入参类型不符的归类：`InvalidInputError`，与 US-4 口径的边界）**：契约 §7.3 的 `invalid_input` = 「参数非法 | JSON 不合法、必填缺失」；US-4 文档把"其余本仓内部错误"归 `runtime_error`。现状：`checkToolInput` 只判"合法 JSON 对象 + 必填键"（`tool_exec.go:238-275`），**字段类型不符**（如 `arguments: "oops"`、非字符串 `body`）会漏到工具内部的解码处——现经 `fmt.Errorf` 走 default 分支落 `runtime_error`，**与"参数非法"的语义不符**（模型给错类型不是本仓缺陷）。本 US 增 `core.InvalidInputError` + `IsInvalidInput` 承载（§3.5 第 11 项），并把既有 `decodeFileToolInput` / `fileToolInput.check` 一并改载体。这是对 US-4 分类表 catch-all 行的**细化**，不是改判：`runtime_error` 从此指"本仓运行期缺陷"，"模型给的入参不合法"归它该在的类别；分层不重复——执行器判结构与必填（入口），工具判类型与取值（解码点），两处都产出同一类别。

## 3. 交付物清单

### 3.1 代码交付物

#### `internal/core`（改动 3 文件；`core` 只依赖标准库的红线不变）

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `errors.go`（改） | 新增 `ExternalError` + `IsExternalError` 与 `InvalidInputError` + `IsInvalidInput`（与 `SandboxViolationError` **平级**；契约 §7.3 `external_error` / `invalid_input` 的承载——注 4、注 11） | `type ExternalError struct { Target string /* 外部目标：状态码与 host 的组合，或 host */; Reason string /* **自产类别摘要**，不含 URL 原文 / query / 响应体 / Location */; cause error /* 原因链：只经 Unwrap 供分类与排障，不进文案 */ }`；`func (e *ExternalError) Error() string`（**安全摘要**——口径 17）；`func (e *ExternalError) Unwrap() error`；`func IsExternalError(err error) bool`；`type InvalidInputError struct { Tool string /* 工具名 */; Reason string /* 可读原因 */ }`；`func (e *InvalidInputError) Error() string`；`func IsInvalidInput(err error) bool`（均 `errors.As` 判类型，**不比文本**） |
| `action.go`（改） | `Command` 增 `MaxOutputBytes` 字段（逐 Run 读取上限——约束 28；前序改造点 §3.5 第 10 项） | `type Command struct { Executable string; Args []string; Dir string; Env []string; MaxOutputBytes int /* 本次执行的读取侧上限；<=0 = 用 runner 装配值；只许比装配值更短 */ }` |
| `tool_exec.go`（改） | `classifyToolError` 增外部系统错误与入参非法两条分支（**判序见流程 C**） | 无签名变化；新分支 `case IsInvalidInput(err): return ErrorKindInvalidInput`；`case IsExternalError(err): return ErrorKindExternalError` |

#### `internal/tool`（新增 2 文件、改动 3 文件——本包自 US-4 起在 ≥70% 覆盖率判卷内）

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `shelltools.go`（新） | `ShellTool`：契约 §5.2 参数与失败类别逐字；流程 A | `func NewShellTool(policy core.Policy, runner core.CommandRunner) *ShellTool`；`func (t *ShellTool) Name() string`（`"shell"`）；`func (t *ShellTool) Description() string`；`func (t *ShellTool) InputSchema() json.RawMessage`（内联常量 `shellInputSchema`，`required: ["executable"]`——执行侧的必填判据）；`func (t *ShellTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error)`；内部：`type shellToolInput struct { Executable string \| Arguments []string \| TimeoutSeconds int }`；`func decodeShellToolInput(input json.RawMessage) (shellToolInput, error)`（失败载体 `*core.InvalidInputError`——注 11）；`func (t *ShellTool) command(env core.ToolEnv, in shellToolInput) core.Command`（填 `MaxOutputBytes: env.MaxOutputBytes`——约束 28）；`func narrowTimeout(ctx context.Context, base time.Duration, requested int) (context.Context, context.CancelFunc)`；`func shellContent(res core.CommandResult) string` |
| `httptools.go`（新） | `HTTPGetTool` / `HTTPPostTool`：契约 §5.3 参数与失败类别逐字；**两件共用一份逐跳循环**（流程 B）；出站 body 上限（约束 29）与跨跳头规则（约束 30，集中 `next`） | `func NewHTTPGetTool(policy core.Policy) *HTTPGetTool`；`func NewHTTPPostTool(policy core.Policy) *HTTPPostTool`；两件各自 `Name()`（`"http_get"` / `"http_post"`）/ `Description()` / `InputSchema()`（内联常量 `httpGetInputSchema` / `httpPostInputSchema`；后者 `required: ["url", "body"]`）/ `Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error)`；内部：`type httpDoer interface { Do(*http.Request) (*http.Response, error) }`（`*http.Client` 天然满足）；`func newHTTPTool(policy core.Policy, doer httpDoer) httpTool`（包内测试接缝——**测试文件与生产同包**，不为此导出生产 API，§4.1；生产装配走 `NewHTTPGetTool` / `NewHTTPPostTool`，它们内部用 `newHTTPClient()`）；`func newHTTPClient() *http.Client`（`Transport.Proxy = nil`、`CheckRedirect` 返回 `http.ErrUseLastResponse`）；`type httpToolInput struct { URL string \| Body string \| ContentType string \| Headers map[string]string }`；`func decodeHTTPToolInput(toolName string, input json.RawMessage) (httpToolInput, error)`（类型不符 / **body 超 `maxRequestBodyBytes`** → `*core.InvalidInputError`——约束 29；上限按 UTF-8 字节计）；`type httpRequestSpec struct { Method, URL, Body, ContentType string; Headers map[string]string }`；`func (spec httpRequestSpec) next(status int, location string) (httpRequestSpec, error)`（**只对 301/302/303/307/308 调用**；方法 / body / 跨跳头语义集中此处——口径 1 + 约束 30）；`func executeHTTP(ctx context.Context, toolName string, policy core.Policy, doer httpDoer, env core.ToolEnv, spec httpRequestSpec) (core.Result, error)`（`Do` 与读取两个错误点**先判 ctx 再包装**——约束 20）；`func httpFailure(status int, host string) error`；`const maxRedirectHops = 5`；`const maxRequestBodyBytes = 256 * 1024` |
| `policy.go`（改） | `checkHTTPRequest` 增**端口规则**（口径 15 冻结：`http` 配省略 / `:80`、`https` 配省略 / `:443` 放行，其余组合含交叉档拒——前序改造点 §3.5 第 9 项）；**拒绝分支目标脱敏**（两处 `action.Target` 回显改安全文案——§3.5 第 9 项） | 无签名变化；内部：`func defaultPortFor(scheme string) (int, bool)`；新常量对 `ruleNonDefaultPort` / `guidanceNonDefaultPort`（指引固定「当前版本不支持非默认端口」，**不得**指向域名白名单——口径 15）；端口判定插在 hostname / IP 字面量判定之后、域名白名单之前 |
| `runner.go`（改） | `LocalRunner` 支持**逐命令收窄**（`Command.MaxOutputBytes`）并夹到 256 KB 硬上限——约束 28 | `RunnerOptions.MaxOutputBytes` 语义不变（装配值）；内部：`func (r *LocalRunner) effectiveMaxOutputBytes(cmd core.Command) int`（`min(装配值、cmd 值取正者、256 KB)`；两者都非正 → 256 KB 缺省） |
| `filetools.go`（改） | 解码与取值错误改载体：`decodeFileToolInput` / `fileToolInput.check` 产出 `*core.InvalidInputError`（注 11，§3.5 第 11 项）——文案不变、只换类型 | 既有签名不变；返回类型仍是 `error`（承载改型） |

#### `internal/cli`（新增 1、改动 3 文件）

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `tool.go`（新） | `tool` 命令组 + `tool list`（流程 D；渲染独立可测，范式照 `provider.go`） | `func newToolCommand(deps Deps) *cobra.Command`；`func newToolListCommand(deps Deps) *cobra.Command`；`func runToolList(out io.Writer, deps Deps) error`；`func renderToolList(out io.Writer, specs []core.ToolSpec)`（**`Deps` 为值类型**——与 `root.go:17` 及全部既有构造器一致，评审 P2-6 修正原稿的 `*Deps`） |
| `ports.go`（改） | 新增**工具清单端口**（接口定义在消费方，实现由装配层注入） | `type ToolLister interface { Tools() []core.ToolSpec }` |
| `deps.go`（改） | `Deps` 增一个注入位 | `Tools ToolLister` |
| `root.go`（改） | 注册命令组（注册点按 US 增补的既有约定） | `cmd.AddCommand(newToolCommand(deps))` |

#### `internal/app`（改动 3 文件）

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `wire.go`（改） | 注册池 +3 → **六件**；`LocalRunner` **首次装配接线**——`MaxOutputBytes` 接 `cfg.Budget.MaxOutputBytes`（注 3、约束 28；`int64`→`int` 转换照 `cli/chat.go:340-345` 既有先例，非正值留给 runner 缺省）；`heavyRuntime` 增 registry 只读字段 + 访问器；更新注册池注释（占位只剩 `notify` / `manage_schedule` 与 US-6 / US-7 / US-10 各件） | `func (r *heavyRuntime) tools() cli.ToolLister`；装配处：`tool.NewLocalRunner(tool.RunnerOptions{Root: root, EnvPassthrough: cfg.Shell.EnvPassthrough, Interpreters: cfg.Shell.Interpreters, MaxOutputBytes: outputBudget(cfg)})`，注册 `tool.NewShellTool(policy, runner)` / `tool.NewHTTPGetTool(policy)` / `tool.NewHTTPPostTool(policy)` |
| `ports.go`（改） | `toolListerPort` 适配器（转调 `Specs(Names())`，保序）+ 编译期断言 | `type toolListerPort struct { registry *tool.Registry }`；`func (p toolListerPort) Tools() []core.ToolSpec`；`var _ cli.ToolLister = toolListerPort{}` |
| `run.go`（改） | `heavyCommands` 增 `"tool"`（注 2）；重路径分支把 `rt.tools()` 接到 `deps.Tools` | — |

> 装配值是**运维配置**的直读（`budget.max_output_bytes`）；Profile **没有**该覆盖权（契约 §6），运行时把逐 Run 值填 `ToolEnv.MaxOutputBytes` 的夹取逻辑（`core/run.go:297-299`）不变——runner 的 256 KB 是**读取侧**的第二道夹取，两者各司其职、不互相替代。

### 3.2 测试交付物

#### 新增 3 文件（55 条 = 51 + 第四轮追加 4 条；测试文件与生产同包 `package tool` / `package cli`——评审 P2-6：不导出生产 API 接缝）

| 文件 | 条数 | 覆盖面 |
| --- | --- | --- |
| `internal/tool/shelltools_test.go` | 17 | schema 与元数据；argv 直执（`Command.Args` 逐项、`Action.Target` 是**整串**、`Dir` 随 Agent 上下文、`Env` 恒空）；真 `WhitelistPolicy` × 假 runner 的 `bash -c "…"` 整串拒（四段式 + runner **零调用**）；空 executable 走策略拒（非本地兜底）；`timeout_seconds` 四态（缺省 / 更短 / 更长不生效 / 非正值）；非零退出码不是错误；输出原样透传（工具不加标记）；runner 错误原样上抛；**逐 Run 预算随 `Command.MaxOutputBytes` 下传**（约束 28）；**decode 类型错 → `IsInvalidInput`**（注 11）；**链路：真 `core.ToolExecutor` × 假审计仓储**——退出码行的开销计入最终 `Content`、中文按 rune 边界、标记恰一次、`truncated` 值正确 |
| `internal/tool/httptools_test.go` | 32 | 两件工具的 schema 必填（`url`；`http_post` 加 `body`）；逐跳校验（每跳一次 `Check`；第 2 跳越界 → 拒 + 假 doer **只收到 1 次**请求）；跳数上限 5 → `sandbox_denied`；相对 `Location` 解析；**301/302/303/307/308 才跳转**——303 转 `GET` 且清 `Content-Type`、307 保持方法与 body、**304 按普通响应返回**（口径 1）；3xx 缺 `Location` → `external_error`（不回显 Location 原文）；4xx 与 5xx → `external_error`（文案不含 query 与响应体）；2xx 内容首行 `HTTP <status>`；空体；上限读取 `max+1` 字节且不含标记；`http_post` 的 `Content-Type` 缺省与显式覆盖；headers 逐项设置；**跨跳头规则**——同源转发 / 跨源不转发（约束 30）；`newHTTPClient` 两条配置（`CheckRedirect` 返回 `ErrUseLastResponse` / `Transport.Proxy` 为 nil）；取消传播（**等"请求已发出"信号再取消**、限时等待）；**读取错误保留 ctx 身份**（约束 20）；真 `WhitelistPolicy` × 假 doer 的 IP 字面量拒（零请求）；**端口规则**（口径 15）：直接请求的非默认端口拒（零请求）、省略与显式默认端口放行对照、交叉档（`http:443` / `https:80`）拒；**白名单域名重定向到非默认端口 → 跳转目标零发送**（首跳 302 正常返回、`Location` 指向 `:8443`、假 doer 只收到 1 次请求——2026-10-17 用户裁决点名的回归）；**body 超上限 → `invalid_input` + 零请求**（含 UTF-8 多字节边界——约束 29）；**httptest 真链路三条**（生产 `newHTTPClient` 逐跳 302 相对 Location 成功 / `HTTP_PROXY` 置不可达仍直连 / 取消传播限时返回——§4.1 允许本地回环）；**链路：真 `core.ToolExecutor` × 假审计仓储**（`< / = / >` 上限、中文截断、空体、标记恰一次、`truncated` 值——§4.3 R3）；**第四轮追加 4 条（2026-10-17 用户实测回归）**：内部截止错误零 URL 泄漏（发送 / 读取两个错误点，断言 query 标记与 `http://` 均不出现在文案里）、`body: null` 判 `invalid_input` 且**零发送**（对照极：空串合法、照发一次）、空串 POST 仍设正文类型（缺省 `application/json` / 显式覆盖两态）、301 / 302 / 303 转 GET 后清 `Content-Encoding`（连同另外两个正文头，覆盖模型头大小写变体） |
| `internal/cli/tool_test.go` | 6 | 渲染排序稳定（同 `Names()` 顺序）+ 汇总行；空池文案；端口缺失报错（点名装配缺件）；未知子命令提示（组命令范式）；`--help` 可用；**清单读取本身不修改状态**（假端口记账；命令属重路径、启动四步的日志不在本判据内——注 9） |

#### 改动 7 文件（追加 15 条 + 判据扩 1 处）

| 文件 | 追加 | 覆盖面 |
| --- | --- | --- |
| `internal/core/tool_exec_test.go` | 4 条 | `*ExternalError` → `external_error`；`*InvalidInputError` → `invalid_input`；`ExternalError` 包着 `context.DeadlineExceeded` 仍归 `timeout`（ctx 分支在外部之前——流程 C 的顺序判据 + `Unwrap` 穿透）；分类全表夹具（七值各一条，防 default 吞新分支） |
| `internal/app/ports_test.go` | 2 条 | `toolListerPort` 转调（保序、非空、与 `Registry.Names()` 同源）；重路径装配池**六件**名字集（`Resolve` 命中三件新工具） |
| `internal/app/run_test.go` | 1 条 | `tool` 属重路径（与 `chat` 同路）；`session` 仍走轻路径 |
| `internal/cli/ports_test.go` | 判据扩 1 处 | 禁 import 清单增 `internal/tool`（注 8） |
| `internal/tool/filetools_test.go` | 2 条 | `decodeFileToolInput` 类型错 / `fileToolInput.check` 空值 → `IsInvalidInput`（载体断言；文案不变——§3.5 第 11 项） |
| `internal/tool/policy_test.go` | 4 条 | **默认端口放行**（缺省与**显式** `:80` / `:443` 等价——口径 15 冻结组合）；**非默认端口拒**（`:8080` / `:8443`，四段式 + `IsSandboxViolation`）；**交叉档拒**（`http:443` / `https:80`——scheme 与端口不匹配）；**文案与指引冻结 + 脱敏**（第 2 段点名 host:port、不含 userinfo / query；第 4 段含「当前版本不支持非默认端口」且**不含** `http.allowed_domains`——口径 15 / §3.5 第 9 项） |
| `internal/tool/runner_test.go` | 2 条 | 逐命令收窄（`Command.MaxOutputBytes=32 <` 装配 64 → 读取 32 + 标记 N=32——约束 28）；装配值超 256 KB → 夹取到 256 KB（标记同值） |

> 覆盖率的判定交给既有 `make cover`（`tool` 包 ≥70% 自 US-4 起已在判卷内），本 US **不改**脚本与阈值。

### 3.3 配置与工程

- **`go.mod` / `go.sum` 零新增坐标**（宪法 VIII）：只用标准库 `net/http`——含 `net/http/httptest`（**默认层允许本地回环测试服务**，工程规范 §2 明列「HTTP 用 `net/http/httptest`、假服务监听 `127.0.0.1:0`」；评审 P2-6 修正原稿"httptest 不需要"的表述——假 doer 绕过真实 Client / Transport，跳转、代理与取消三条行为只能在真链路上判）；判据 = `git diff go.mod go.sum` 为空。
- **`basalt.yaml.example` 零改动**：`http.allowed_domains` / `shell.{allowed_commands,interpreters,env_passthrough}` 已存在且键名逐字沿用；**不新造配置键**——`budget.max_output_bytes` 也是**既有键**（`cli/config.go` 的 `BudgetConfig`），本 US 只做**消费侧接线**（装配把它接到 `RunnerOptions.MaxOutputBytes`——注 3、约束 28）。
- **`Makefile` / CI 零改动**：覆盖率目标与门禁七目标自 US-4 起已覆盖本 US 的包（`tool` / `cli` / `core` / `app`）。
- **文档**：本 US **不改**四份权威文档、不改契约、不改 `docs/Operations.md`；台账由 `/us-register 5` 写；本文件是唯一新增文档。
- **日志**：**零新增日志键**——工具本体不记日志（失败路径的 WARN 固定键由 `ToolExecutor` 承载，键集逐字不变）；`tool list` 的"只读"指**清单读取本身不修改状态**——命令属重路径，启动四步（取锁 / 迁移 / 恢复 / 开跑）各记既有日志（注 9），**不得**写成"零日志"。
- **零迁移脚本 / 零新表**：`schema.sql` 与 `internal/storage/migrations/` 一字不动。
- **编码前必读**：[docs/EngineeringStandards.md](../EngineeringStandards.md) §1 Go / §2 测试 / §4 日志与错误（规范前置纪律）。

### 3.4 表与列级口径

**零迁移、零列变化**——本 US 不新增表、不新增列，列面口径逐条重申（判据：`git diff docs/contracts/schema.sql` 为空）：

| 面 | 口径 | 出处 |
| --- | --- | --- |
| `tool_invocations.result_json` | 截断标记与 `truncated` **都在值内**；列面**没有** `truncated` / `round` 列 | 契约 §7.4 + US-4 §6.2 口径 7 |
| `tool_invocations.input_json` | 工具入参**原样**保存（模型给的 arguments 不重编码）——`headers` / `body` 里的任何内容因此也在库内 | 契约 §7.4；`tool_exec.go:195-204` |
| 敏感数据边界（如实说明，**不得**笼统承诺"凭证不落库"） | 本 US **不注入**运维凭证（没有 `${ENV_VAR}` 解析面、没有密钥托管）；模型在 `headers` / `body` 里自带的内容按契约**原样**进 `input_json`（上一行口径）——保护面在"**日志与模型可见文案**不额外扩散"（脱敏回归 §3.2 + 口径 17），不在"库里没有" | 评审 P1-5；契约 §7.4 |
| `tool_invocations.error_kind` | `external_error` 自 US-2 起已在 `llm_calls` 上写入（供应商侧），本 US 把它接到**工具链**——本 US 起该值可出现在 `tool_invocations` 上 | 契约 §7.3 |
| `runs` / `config_snapshots` | 零改动（本 US 不写这两张表的新字段） | 契约 §7.2 / US-4 §3.4 |

### 3.5 前序改造点与后续占位

#### 前序改造点（逐条软门禁：不完成则本 US 的功能断言立不住）

| # | 改造点 | 触及面 | 理由与依据 | 影响既有测试 |
| --- | --- | --- | --- | --- |
| 1 | `core` 增 `ExternalError`（含 `Cause` / `Unwrap`）与 `InvalidInputError` 承载 + `classifyToolError` **两条分支**（判序见流程 C） | `internal/core/errors.go` / `tool_exec.go` | 契约 §7.3 字面量已冻结、工具链无承载；`tool_exec.go:228` 注释逐字预告「US-5 起」（注 4、注 11） | 既有分类用例不受影响（新分支只在错误链含对应类型时命中）；追加 4 条 |
| 2 | `LocalRunner` **首次装配接线**（含 `MaxOutputBytes ← cfg.Budget.MaxOutputBytes`，`int64`→`int` 转换） | `internal/app/wire.go` | US-4 交付实现与用例、**零生产调用点**（注 3）；评审否决"传 0"，预算须到读取端（约束 28） | 无（`runner_test.go` 自建 fixture，不走装配）；追加 2 条（runner 侧收窄与夹取） |
| 3 | 注册池 3 → **6 件** | `internal/app/wire.go` | 编程指南 §4.5「内置十件补齐到六件」 | 装配池断言（`ports_test.go` 侧）追加 2 条 |
| 4 | `heavyCommands` 增 `"tool"` | `internal/app/run.go` | 注 2 的推导链 | 命令名解析是共享函数——加一项不改既有断言；追加 1 条 |
| 5 | `cli` 工具清单端口 + `app` 适配器 + `Deps.Tools` | `cli/ports.go` / `cli/deps.go` / `app/ports.go` | `cli` 不得 import 实现包（依赖方向）；端口定义在消费方 | 无既有断言受影响；追加 2 条 |
| 6 | `root.go` 注册 `tool` 命令组 | `internal/cli/root.go` | 命令面按 US 增补（注册点注释已写明该约定） | 无 |
| 7 | `ports_test.go` 禁 import 清单增 `tool` | `internal/cli/ports_test.go` | 判据扩面（注 8） | 这条判据本身改判据面——不是放宽阈值 |
| 8 | 注册池注释同步（占位从「shell / http_get / http_post 归 US-5」改为剩余件） | `internal/app/wire.go` | 注释是事实登记面，不得留过期占位 | 无 |
| 9 | `checkHTTPRequest` 增**端口规则**（口径 15 冻结组合：`http` + 省略 / `:80`、`https` + 省略 / `:443` 放行，其余含 `http:443` / `https:80` 交叉档拒；指引固定「当前版本不支持非默认端口」）+ 拒绝分支**目标脱敏**（不可解析 URL 不回声原文、IP / 域名分支只回 host） | `internal/tool/policy.go` | 技术方案 §6.5「校验 scheme + host + 端口」——现状缺端口一项；两处拒绝分支回显完整 `action.Target`（`policy.go:236/245`）是凭证泄漏面（评审 P1-3 / P1-5；端口放行口径 = 2026-10-17 用户裁决） | 既有用例均用缺省端口 → 不受影响；追加 4 条 |
| 10 | `core.Command` 增 `MaxOutputBytes` + `LocalRunner` 逐命令收窄与 256 KB 夹取 | `internal/core/action.go` / `internal/tool/runner.go` | 评审裁决 #4：逐 Run 更小的预算须到达读取端（约束 28）；现 clamp 只兜非正值、不夹上限（`runner.go:78-81`） | 既有用例不传该字段（零值 = 不设）→ 行为不变；追加 2 条 |
| 11 | 工具解码 / 取值错误改载体 `*core.InvalidInputError`：`decodeFileToolInput` / `fileToolInput.check`（HTTP / shell 新工具的同类错误按新载体直接写） | `internal/tool/filetools.go` | 注 11：类型不符现经 default 落 `runtime_error`，与"参数非法"语义不符 | 文案不变、只换类型——既有文本断言不受影响；追加 2 条 |

#### 后续占位（只留装配位、本 US 不实现）

| 占位点 | 现状 | 归属 |
| --- | --- | --- |
| `save_memory` / `recall_memory` 两件记忆工具 | `Registry.Register` 是唯一入口；名字集由装配期导出喂装载器（宪法 IV） | US-6 |
| MCP 工具接入 | 同上——MCP 适配器按 `core.Tool` 接口注册，注册表不认识来源 | US-7 |
| `notify` / `manage_schedule` 两件 | `Registry` 留注册位（本 US 更新占位注释） | US-8 / US-10 |
| `GET /api/v1/tools` | 数据源同一份（`Registry.Specs`），REST 面只是另一个消费者 | US-9 |
| 容器化沙箱（`Policy` / `CommandRunner` 第二实现） | 两接口不动、装配处切换（技术方案 §6.5 升级路径） | 扩展阶段 F |
| `use_skill` | **永久禁**（宪法 IV；编程指南 §4.5 同款） | 不实现 |

## 4. 验收测试（Harness）

### 4.1 测试分层标准

| 层 | 判据 | 本 US 适用面 |
| --- | --- | --- |
| 默认层（`make test` = `go test -race ./...`） | **不访问真实外网、不起真实子进程、不打真实模型**——**本地回环**的 `net/http/httptest` 服务（`127.0.0.1:0`）**允许**（工程规范 §2 明列）；夹具内联构造（`t.TempDir()`） | 本 US **全部新增用例**——`httptools_test.go` 以**进程内假 doer**（`*http.Response` 由 `strings.Reader` 拼出，零 socket）为主 + **少量 httptest 真链路**（生产 `newHTTPClient` 的跳转 / 取消 / `Proxy=nil` 三条行为——假 doer 绕过真实 Client / Transport，这三条只能在真链路上判）、`shelltools_test.go` 用**假 runner**（`core.CommandRunner` 实现，零进程）、`tool_test.go` 用**假端口**（`cli.ToolLister` 实现）；**链路截断用例经真实 `core.ToolExecutor` + 假审计仓储**（`core.ToolInvocationStore` 实现——仓储是 I/O 边界，执行器不再造第二份） |
| 集成层（`make test-integration` = `-tags=integration`） | 真库 / 真文件系统，仍不打真网络 | 本 US **无新增**（跑既有套件） |
| 评测层（`evals/`） | 默认 fake 后端、可重复；真模型显式门控 | 本 US **不改评测基座**（用例集扩面归 US-12 收尾；Demo 一按 §6.1 手工执行） |
| 人工层 | 真模型 + 真控制台 + 真网络 | §6.1 |

**判卷纪律四条**：① **逐跳用例必须记账两侧**——假 doer 记"发了几次请求"、假策略记"判了几次、判的哪个 target"，只断言最终结果会把"先发后判"判成通过；② **取消 / 泄漏用例必须等信号再取消**（"请求已发出"由假 doer 用 channel 通知），并限时等待返回——goroutine 总数只作辅助观察；③ 全部用例在 `-race` 下跑（`make test` 自带），共享状态（假 doer / 假策略的记录切片）必须带锁；④ **链路用例必须经真实 `core.ToolExecutor` 与真实结果包装**（假审计仓储只做 I/O 边界，**不得**再造一个执行器——再造就等于把被测对象换成了夹具）。

### 4.2 测试类 → 验收点映射表

| 测试文件 | 条数 | 覆盖的验收点（带出处） |
| --- | --- | --- |
| `internal/tool/shelltools_test.go` | 17 | 契约 §5.2 参数与语义（argv 直执、逐项原样、可执行名整串、cwd 约定、环境变量不自行透传）；技术方案 §6.2 ShellTools 行；契约 §6 预算（`timeout_seconds` 只缩短、逐 Run 预算到读取端——约束 28）；Compatibility §2.2（缩短口径）；FR12（非零退出码不是错误、截断归 runner）；注 11（解码类型错 → `invalid_input`）；最终 `Content` 预算口径（§6.2 口径 7） |
| `internal/tool/httptools_test.go` | 32 | 契约 §5.3 参数与失败类别（逐跳校验、跳数上限 5、IP 字面量、`body` 必填、`content_type` 缺省、4xx/5xx → `external_error`）；口径 1（只认五种 3xx）；**口径 15（端口冻结组合；跳转目标同判、越格的那一跳零发送——2026-10-17 用户裁决点名的回归）**；技术方案 §6.5 授权矩阵 HTTP 行（不读代理变量、端口校验）；契约 §7.3（`external_error` / `timeout` / `invalid_input` 分界）；约束 29（出站 body 上限、零发送）；约束 30（跨跳头）；§6.2 口径 7 的"截断标记恰一次"（链路用例） |
| `internal/cli/tool_test.go` | 6 | 需求 §5.12 命令清单；技术方案 §8.7（重的归属由 §2.4 注 2 交给 `run_test.go` 判）；Operations §4.3 判据 4 的读数形态（"共 N 件工具"）；`cli` 端口纪律（假端口注入）；注 9（清单读取本身不修改状态） |
| `internal/core/tool_exec_test.go` | 追加 4 | 流程 C 的分类顺序（ctx 先于 external）；`invalid_input` 载体；`Unwrap` 穿透；契约 §7.3 七值完整性 |
| `internal/app/ports_test.go` | 追加 2 | 装配池六件（编程指南 §4.5）；端口转调保序（渲染顺序的唯一来源） |
| `internal/app/run_test.go` | 追加 1 | 注 2（`tool` 属重路径） |
| `internal/cli/ports_test.go` | 判据扩 1 处 | 注 8（依赖方向） |
| `internal/tool/filetools_test.go` | 追加 2 | 注 11（`invalid_input` 载体改造，filetools 同步） |
| `internal/tool/policy_test.go` | 追加 4 | 口径 15（默认端口放行 / 非默认端口拒 / `http:443` 与 `https:80` 交叉档拒 / 指引与脱敏冻结——技术方案 §6.5 端口句 + 2026-10-17 用户裁决）；§3.5 第 9 项（拒绝文案脱敏） |
| `internal/tool/runner_test.go` | 追加 2 | 约束 28（逐命令收窄、256 KB 夹取——评审裁决 #4） |

**FR 覆盖自检**：本 US 直接相关的 FR（工具侧）——FR7（调度唯一归属，本 US 只提供工具本体）、FR8（审计字面量冻结——`external_error` / `invalid_input` 都是既有七值，本 US 只新增**产出方**不改取值集）、FR11（沙箱三档判定，复用 US-4 交付；**域名档补端口规则**——§3.5 第 9 项）、FR12（受控执行，复用；逐 Run 预算到读取端是既有 `MaxOutputBytes` 口径的补全）、FR13（注册与查找，复用）、FR20（预算与时限，本 US 消费"只缩短"且把预算接到读取端）、FR27（可观测性双轨，工具不记日志、键集不变）——均由上表覆盖；循环 / 提示词 / 会话 / 恢复类 FR 不在本 US 的核对面（US-4 已验收，本 US 零改动）。

### 4.3 关键回归测试写出代码

> 风格：函数名英文、中文验收点以注释保留；`require` 断言逐条对应上面的验收点。夹具（`fakeDoer` / `spyPolicy` / `fakeRunner` / `fakeInvocationStore`）在各自测试文件内定义，**不**扩 `internal/testutil` 公开面。**新测试文件与生产同包**（`package tool` / `package cli`）——夹具直接经包内接缝（`newHTTPTool` / `newHTTPClient`）组装，**不**为测试导出生产 API（评审 P2-6 修正原稿的外部测试包写法：外部包够不着未导出符号，会逼出"仅为测试导出"的生产面）。链路用例的调用形态统一为 `core.NewToolExecutor(resolver, audit, nil)`——`resolver` 用包内 `NewRegistry(slog.New(...))`（它天然满足 `core.ToolResolver`）、`audit` 用假仓储。

#### R1 — 每一跳发送前都过判定（越界的那一跳必须在发出之前被拦）

```go
// TestHttpGet_PerHopCheckPrecedesEverySend：契约 §5.3「逐跳校验——每一跳发送前过白名单与
// scheme，客户端不得自行跟随」。判据取最严形态：第 2 跳的目标被策略拒 → 假 doer 记录的
// 请求次数必须**恰好是 1**（拒的那一跳不得发出），且错误是四段式沙箱拒绝
// （执行器按类型分类 ⇒ IsSandboxViolation 即 error_kind=sandbox_denied 的判据）。
func TestHttpGet_PerHopCheckPrecedesEverySend(t *testing.T) {
    doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
        return redirectResponse(301, "http://evil.example.com/next"), nil // 第一跳：3xx 指到白名单外
    }}
    policy := &spyPolicy{allowed: []string{"api.example.com"}} // 只放行 api.example.com
    httpGet := newHTTPTool(policy, doer)

    _, err := httpGet.Execute(context.Background(),
        core.ToolEnv{MaxOutputBytes: 256 * 1024}, []byte(`{"url":"http://api.example.com/weather"}`))

    require.Error(t, err)
    require.True(t, core.IsSandboxViolation(err), "越界跳必须判 sandbox_denied（不是 external_error）")
    require.Contains(t, err.Error(), "evil.example.com", "四段式第 2 段点名越界目标")
    require.Equal(t, 1, doer.count(), "被拒的那一跳不得发出——请求次数必须恰好 1")
    require.Equal(t, []string{"http://api.example.com/weather", "http://evil.example.com/next"}, policy.targets(),
        "每一跳发送前各判一次（顺序即发送顺序）")
}
```

#### R2 — 跳数上限 5，超限判 `sandbox_denied`

```go
// TestHttpGet_RedirectHopLimitIsFive：契约 §5.3「跳数超过 5」归 sandbox_denied（不是
// external_error）；技术方案 §6.5 授权矩阵同款。口径（§6.2 固定口径 4）：hop 计"已跟随的
// 重定向次数"，初始请求不计——5 跳跟完后第 6 个 3xx 即拒，故 doer 恰收到 6 次请求。
func TestHttpGet_RedirectHopLimitIsFive(t *testing.T) {
    n := 0
    doer := &fakeDoer{respond: func(req *http.Request) (*http.Response, error) {
        n++
        return redirectResponse(302, req.URL.String()+"/hop"+strconv.Itoa(n)), nil // 永远再跳一次
    }}
    policy := &spyPolicy{allowed: []string{"api.example.com"}} // 每跳都在白名单内

    _, err := newHTTPTool(policy, doer).Execute(context.Background(),
        core.ToolEnv{MaxOutputBytes: 256 * 1024}, []byte(`{"url":"http://api.example.com/weather"}`))

    require.Error(t, err)
    require.True(t, core.IsSandboxViolation(err), "超限判 sandbox_denied")
    require.Equal(t, 6, doer.count(), "初始请求 + 5 跳；第 6 个 3xx 被拒（未发出第 7 次请求）")
    require.Equal(t, 0, policy.denials(), "6 次判定都放行了——拒的原因只可能是跳数")
}
```

#### R3 — 工具 → Executor → `result_json` 链路：预算口径 = 最终 `Content`（含状态行开销）

```go
// TestHTTPChain_ExecutorCapsFinalContent：§6.2 固定口径 7 的完整判卷——首行 `HTTP <status>`
// 的开销**计入**预算；读取上限 = min(env.MaxOutputBytes, 256 KB) 且工具多读 1 字节，执行器
// 的 `len(content) > maxBytes` 判据因此**必定命中**；标记恰一次、N = 生效上限、中文按 rune
// 边界回退、空体只有首行。链路 = 真工具 → **真实 core.ToolExecutor** → 假审计仓储
// （仓储只是 I/O 边界——判卷纪律 ④：不得再造一个执行器）。
func TestHTTPChain_ExecutorCapsFinalContent(t *testing.T) {
    const capBytes = 64
    statusLine := "HTTP 200 OK\n" // 开销常量：等于上限的用例把它算进去，差 1 字节必现形
    cases := []struct {
        name          string
        body          string
        wantTruncated bool
    }{
        {"小于上限", strings.Repeat("a", 16), false},
        {"等于上限", strings.Repeat("a", capBytes-len(statusLine)), false}, // 状态行计入 ⇒ 恰为上限
        {"超过上限一字节", strings.Repeat("a", capBytes-len(statusLine)+1), true},
        {"中文截断回退", strings.Repeat("地", 100), true}, // 3 字节/字：不得劈出半个 rune
        {"空体只有首行", "", false},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            httpGet := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}},
                &fakeDoer{respond: okResponder(200, tc.body)})
            registry := NewRegistry(slog.New(slog.DiscardHandler))
            require.NoError(t, registry.Register(httpGet))
            audit := &fakeInvocationStore{}

            res := core.NewToolExecutor(registry, audit, nil).Execute(context.Background(),
                core.ToolEnv{RunID: "cli-test-run", MaxOutputBytes: capBytes},
                []string{"http_get"},
                core.ToolCall{ID: "call_1", Name: "http_get",
                    Arguments: `{"url":"http://api.example.com/x"}`})

            require.Len(t, audit.rows, 1, "一条审计行（预写 + 终态回填各一次）")
            var out struct {
                Content   string `json:"content"`
                Truncated bool   `json:"truncated"`
                Retryable bool   `json:"retryable"`
            }
            require.NoError(t, json.Unmarshal(audit.rows[0].result.ResultJSON, &out))

            require.Equal(t, out.Content, res.Content,
                "返回给循环的内容与落库值同一份（tool_exec.go:129-137 同一 content 变量）")
            require.Equal(t, tc.wantTruncated, out.Truncated)
            require.True(t, strings.HasPrefix(out.Content, statusLine), "首行是响应状态行")
            marker := core.TruncationMarker(capBytes) // N = 生效上限
            if tc.wantTruncated {
                require.Equal(t, 1, strings.Count(out.Content, marker), "标记恰一次")
                require.True(t, utf8.ValidString(out.Content), "截断不得劈出半个 rune")
                require.LessOrEqual(t, len(strings.TrimSuffix(out.Content, marker)), capBytes,
                    "截断后的前缀不超过生效上限——中文用例会在此回退到 rune 边界")
            } else {
                require.NotContains(t, out.Content, marker)
                require.LessOrEqual(t, len(out.Content), capBytes)
            }
        })
    }
}
```

> 执行器对成功路径**同时**返回 `res.Content` 与落库 `result_json`（`tool_exec.go:128-137` 同一 `content` 变量、截断标记在两处一致）——用例把"两处同值"也钉住：单看一处会把"回传值没截断而库里截了"这类偏差放过。

#### R4 — 取消传播：等"请求已发出"再取消，读取端返回取消错误（并发泄漏类回归，`-race` 下跑）

```go
// TestHTTPGet_CancelDuringBodyReadReturnsPromptly：取消传播在工具侧的形状——请求已发出后
// 取消，Execute 必须在限时内返回**取消错误**（评审 P2-6 修正：阻塞体在 ctx.Done 时返回
// io.EOF 会被 ReadAll 读成"读完了"、取消被静默吞掉——夹具必须走与真实网络一致的形态：
// 返回 ctx.Err()），且**不得**留后台 goroutine 继续读响应体（宪法 VII 同步执行）。
// 断言以限时等待为主（goroutine 总数只作辅助观察）；等"已发出"信号也设超时。
func TestHTTPGet_CancelDuringBodyReadReturnsPromptly(t *testing.T) {
    started := make(chan struct{})
    doer := &fakeDoer{respond: func(req *http.Request) (*http.Response, error) {
        close(started)
        return http.Response{
            Status: "200 OK", StatusCode: 200,
            Body: io.NopCloser(cancelAwareReader{ctx: req.Context()}), // 读到 ctx.Done 时**返回 ctx.Err()**
        }, nil
    }}
    ctx, cancel := context.WithCancel(context.Background())
    t.Cleanup(cancel) // 清理注册：用例失败提前退出也不悬挂

    done := make(chan error, 1)
    go func() {
        _, err := newHTTPTool(&spyPolicy{allowed: []string{"api.example.com"}}, doer).Execute(
            ctx, core.ToolEnv{MaxOutputBytes: 64}, []byte(`{"url":"http://api.example.com/x"}`))
        done <- err
    }()

    select {
    case <-started: // 等"请求已发出"信号**再**取消——先取消会把用例变成"没发出去"，判不出泄漏
    case <-time.After(2 * time.Second):
        t.Fatal("请求未在限时内发出")
    }
    cancel()
    select {
    case err := <-done:
        require.ErrorIs(t, err, context.Canceled, "错误链保留 ctx 身份（执行器据此归 timeout）")
    case <-time.After(2 * time.Second):
        t.Fatal("取消后未在限时内返回：响应体读取没有跟随 ctx")
    }
}
```

#### R5 — `timeout_seconds` 只缩短（四态：缺省 / 更短 / 更长不生效 / 非正值）

```go
// TestShellTool_TimeoutSecondsOnlyNarrows：契约 §5.2 的扩展字段 + Compatibility §2.2
// 「只能是缩短」+ 契约 §6「模型只能缩短」。**父 deadline 先建立**——生产链路由执行器在
// env.Timeout 处建立（tool_exec.go:102-107，取值 <= 0 视为不设限）；本用例直接调用工具，
// 就自己把等价形态建出来（评审 P2-6 修正原稿：用 context.Background() 却断言"缺省"分支
// 有 deadline，是把执行器的职责错当工具的）。判据不比对文本：假 runner 记录**收到的 ctx
// 截止时刻**。
func TestShellTool_TimeoutSecondsOnlyNarrows(t *testing.T) {
    cases := []struct {
        name      string
        requested string
        want      time.Duration
    }{
        {"缺省 → 用父 deadline", `{"executable":"git"}`, 30 * time.Second},
        {"更短 → 收窄一层", `{"executable":"git","timeout_seconds":5}`, 5 * time.Second},
        {"更长 → 不生效", `{"executable":"git","timeout_seconds":3600}`, 30 * time.Second},
        {"非正值 → 按缺省", `{"executable":"git","timeout_seconds":0}`, 30 * time.Second},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            runner := &fakeRunner{} // 记录 Run 收到的 ctx 与 core.Command
            shell := NewShellTool(&spyPolicy{allowed: []string{"git"}}, runner)

            parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
            defer cancel()

            _, err := shell.Execute(parent,
                core.ToolEnv{Timeout: 30 * time.Second, AgentName: "demo"}, []byte(tc.requested))

            require.NoError(t, err)
            deadline, ok := runner.deadline()
            require.True(t, ok, "生效超时必须以 deadline 形态交给 runner（工具只做收窄、自身不建超时路径）")
            require.InDelta(t, tc.want.Seconds(), time.Until(deadline).Seconds(), 1.0,
                "生效超时 = min(env.Timeout, timeout_seconds)")
        })
    }
}
```

#### R6 — 非零退出码不是错误（工具如实呈现退出码与合流输出，预算随 `Command` 下传）

```go
// TestShellTool_NonZeroExitIsNotAnError：core.CommandResult 的既有口径（runner_test.go:316
// 已判"runner 不报错"）——工具侧再判一次**呈现**：退出码进结果首行、输出原样保留（工具
// 不追加标记——约束 11）、逐 Run 预算随 Command 下传（约束 28）。把非零码判成失败会让
// grep 类"没匹配到"全变失败。
func TestShellTool_NonZeroExitIsNotAnError(t *testing.T) {
    runner := &fakeRunner{result: core.CommandResult{Output: "no match\n", ExitCode: 1}}
    shell := NewShellTool(&spyPolicy{allowed: []string{"grep"}}, runner)

    res, err := shell.Execute(context.Background(),
        core.ToolEnv{Timeout: 30 * time.Second, MaxOutputBytes: 256 * 1024},
        []byte(`{"executable":"grep","arguments":["-r","needle","."]}`))

    require.NoError(t, err, "非零退出码不是 error")
    require.Equal(t, "退出码：1\nno match\n", res.Content)
    require.False(t, res.Retryable) // §6.2 固定口径 2 的默认值
    require.Equal(t, core.Command{
        Executable: "grep", Args: []string{"-r", "needle", "."},
        MaxOutputBytes: 256 * 1024, // 逐 Run 读取上限（约束 28）——漏传 = 读取端恒用装配值
    }, runner.command(), "argv 逐项原样、不经任何 shell；预算随行")
}
```

#### R7 — httptest 真链路：生产 `newHTTPClient` 的跳转驱动与 `Proxy=nil`（默认层允许本地回环）

```go
// TestHTTPWithRealClient_DrivesHopsAndIgnoresEnvProxy：假 doer 绕过真实 Client / Transport，
// 两条行为只能在真链路上判——① 重定向由**逐跳循环**驱动而不是 Client 自行跟随（CheckRedirect
// 返回 ErrUseLastResponse，302 的相对 Location 经 ResolveReference 后由工具再判再发）；
// ② Transport.Proxy=nil：环境里有 HTTP_PROXY 也不读（技术方案 §6.5 授权矩阵 HTTP 行）。
// 夹具策略直接放行——本用例的判卷面是**客户端行为**；白名单与端口判定归 policy 用例。
func TestHTTPWithRealClient_DrivesHopsAndIgnoresEnvProxy(t *testing.T) {
    var mu sync.Mutex
    var hops []string
    server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        mu.Lock()
        hops = append(hops, r.URL.Path)
        mu.Unlock()
        if r.URL.Path == "/second" {
            _, _ = io.WriteString(w, "weather: sunny")
            return
        }
        http.Redirect(w, r, "/second", http.StatusFound) // 302 + **相对** Location
    }))
    t.Cleanup(server.Close)

    // 不可达代理：Transport 若读环境代理变量，请求必失败——成功即证明 Proxy=nil 生效
    t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
    t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
    t.Setenv("NO_PROXY", "")

    httpGet := NewHTTPGetTool(&spyPolicy{allowed: []string{"127.0.0.1"}}) // 生产装配：真实 newHTTPClient
    res, err := httpGet.Execute(context.Background(),
        core.ToolEnv{MaxOutputBytes: 256 * 1024}, []byte(`{"url":"`+server.URL+`/first"}`))

    require.NoError(t, err)
    require.Contains(t, res.Content, "HTTP 200 OK")
    require.Contains(t, res.Content, "weather: sunny")
    mu.Lock()
    defer mu.Unlock()
    require.Equal(t, []string{"/first", "/second"}, hops, "两跳都由工具驱动（Client 不自行跟随）")
}
```

#### R8 — 白名单域名重定向到非默认端口：目标请求零发送（口径 15 跨跳回归，2026-10-17 用户裁决点名）

```go
// TestHttpGet_RedirectToNonDefaultPortIsRefusedBeforeSend：口径 15（2026-10-17 用户裁决）——
// 端口判定与域名判定同源同判：初始请求与**每一次重定向**发送前都过 policy，白名单域名跳到
// 非默认端口同样拒、且拒在**发送之前**。分工写清：域名白名单管 host、**不管端口**——本条
// 即"host 在白名单内、端口不在默认档"的组合。夹具用**真 WhitelistPolicy**（端口规则落在
// 策略层，假策略测不到它）× 假 doer。
func TestHttpGet_RedirectToNonDefaultPortIsRefusedBeforeSend(t *testing.T) {
    doer := &fakeDoer{respond: func(*http.Request) (*http.Response, error) {
        // 第一跳：白名单域名内的 3xx，Location 指到**同一 host 的非默认端口**
        return redirectResponse(302, "http://api.example.com:8080/next"), nil
    }}
    policy := NewWhitelistPolicy(PolicyOptions{
        Root:           t.TempDir(),
        AllowedDomains: []string{"api.example.com"}, // host 命中白名单，端口不在默认档
    })
    httpGet := newHTTPTool(policy, doer)

    _, err := httpGet.Execute(context.Background(),
        core.ToolEnv{MaxOutputBytes: 256 * 1024}, []byte(`{"url":"http://api.example.com/weather"}`))

    require.Error(t, err)
    require.True(t, core.IsSandboxViolation(err), "非默认端口判 sandbox_denied（不是 external_error）")
    require.Contains(t, err.Error(), "api.example.com:8080", "四段式第 2 段点名目标 host 与端口（脱敏后不含 userinfo / query）")
    require.Contains(t, err.Error(), "当前版本不支持非默认端口", "第 4 段指引固定写这个原因")
    require.NotContains(t, err.Error(), "http.allowed_domains",
        "指引不得提示\"加入域名白名单即可解决\"——域名白名单管 host、不管端口")
    require.Equal(t, 1, doer.count(), "目标请求零发送——被拒的那一跳必须没发出（初始请求才是 1 次）")
}
```

## 5. 范围边界

### 5.1 核心阶段与整体不做（逐项标依据）

| 不做项 | 依据 |
| --- | --- |
| 工具**并行**调用（一轮内多工具并发） | 宪法 VII（同步执行）；契约 §2（工具串行） |
| 幂等键 / 重试 / 重放 | 契约 §3.3（不自动重试、不自动重放；`http_post` 的外部副作用因此不重放，重传即新 Run） |
| 流式响应（SSE / 假流式） | 需求 §6 核心阶段不做；Compatibility §4 第 4 条 |
| 容器化沙箱 / 命名空间隔离 | 技术方案 §6.5 授权矩阵（容器化属扩展阶段）；Compatibility §4 第 6 条 |
| Tool Policy（按 Agent 的权限矩阵） | 技术方案 §6.5；Compatibility §4 第 10 条（当前粒度 = 每个 Agent 的 `tools:` 清单） |
| 出网凭证托管（OAuth / KMS / 密钥轮换） | Compatibility §4 第 9 条（核心阶段凭证经环境变量注入，轮换即重启） |

### 5.2 本 US 不做（归后续）

| 不做项 | 归属 |
| --- | --- |
| `save_memory` / `recall_memory` 两件记忆工具 | US-6 |
| MCP 客户端与工具接入 | US-7 |
| `notify` / `manage_schedule` 两件工具与通知渠道 | US-8 / US-10 |
| `GET /api/v1/tools`、`tool show`、工具启停与权限编辑 | US-9 / US-12 |
| 评测用例集扩面（含 shell / http 用例） | US-12 收尾（本 US 只手工跑 Demo 一） |
| `use_skill` | 宪法 IV **永久禁** |
| 非默认端口的放行通道（须新增配置键——本 US 只允许默认端口 `http:80` / `https:443`） | 扩展阶段（§6.2 口径 15；**2026-10-17 用户终裁**——见落案表 #13）。**边界如实登记**：本地 `8080` 类服务在当前版本不可达。原列的三项留白——重定向跨跳的方法语义 / `Retryable` 取值口径 / 出站请求体上限——**均已评审裁决**，见「评审回合裁决落案表」 |

## 6. 验证与验收

### 6.1 剩余人工项

| # | 人工项 | 判据 / 读数 |
| --- | --- | --- |
| 1 | 门禁七目标 | `make check` 全绿；`make cover` 末行「全部包达标」，`tool` 包覆盖率读数记录（≥70%） |
| 2 | **Demo 一（验收 Demo）** | 配置 `http.allowed_domains` 加真实天气 API 域 + 真 provider → `basalt chat` 问「今天天气怎么样」→ 模型调 `http_get` 取数据 → 给出**穿衣建议**；留读数：`tool_invocations` 行（`tool_name=http_get`、`success`）、对话文本、审计与 `config_snapshots` 关联。**未执行前不得勾选** |
| 3 | 沙箱拒绝样例 | 让模型试 `bash -c "…"`、工作区外路径、**非默认端口 URL**（如 `https://example.com:8443/x`——口径 15 的拒绝分支）或非白名单域名 → 记录**真实**四段式文案 + 审计 `failed` + `sandbox_denied`（`bash` 形态判 `ruleBashSh`，与 US-4 的路径档样例并列登记）；**端口档核对点**：第 4 段指引写「当前版本不支持非默认端口」且**不指向** `http.allowed_domains`（域名白名单管 host、不管端口） |
| 4 | `basalt tool list` 冒烟 | 六件工具 + 汇总行「共 6 件工具」；对照 Operations §4.3 判据 4（工具注册表非空）与「判据 4、5 同一条路径」 |
| 5 | `shell` 真命令读数 | 白名单内命令在 Agent 目录下执行（cwd 生效）、退出码非零不判失败、输出含 stderr 合流顺序 |
| 6 | 超时 / 中断的进程树复核 | 经**工具路径**再跑一次强杀（父子一并消失）；US-4 的 runner 级读数作为基线对照 |
| 7 | gosec 复核 | 新增出网与子进程调用面是否吹哨（如"变量 URL"类规则）；**不得**加抑制注释——如实登记后按 US-4 的 G204 先例走用户裁决 |
| 8 | 三平台编译 | windows / linux / darwin 三平台 `go build`（本 US 无新平台分片文件，仍按陷阱表 #56 的口径复核"两平台判的不是同一批文件"） |
| 9 | 凭证与敏感面复核 | 本 US **不注入**运维凭证（新增代码零 `${ENV_VAR}` 解析面）；`basalt.yaml` 的 `http` / `shell` 段不含明文凭证。复核点 = 脱敏：**日志 / 模型可见文案 / 错误文案**不含 userinfo / query / headers / body 内容（默认层的脱敏回归 §3.2 + 第 12 项抽验）；**不得**笼统写成"凭证不落库"——模型参数按契约原样进 `input_json`（§3.4 末行口径，如实说明） |
| 10 | 变动范围核对 | `git diff --stat` 只落在 §3 列的 25 个文件（代码 新增 3 + 改动 12；测试 新增 3 + 改动 7）＋ 本文件；`git diff go.mod go.sum docs/contracts/schema.sql` 为空 |
| 11 | Windows 真控制台渲染 | `tool list` 中文渲染在真控制台可读（与既有命令同源，低风险复核） |
| 12 | 端口与脱敏抽验 | 手工构造 `https://user:secret@example.com:8443/x?token=…` 形态的调用 → ① `:8443` 被拒（四段式点名"非默认端口"）；② 拒绝文案与日志**不含** `secret` / `token`；③ `https://example.com:443/x`（显式默认端口）放行到"域名不在白名单"或出网（按配置）；④ **交叉档** `http://…:443/x` / `https://…:80/x` 同拒（scheme 与端口不匹配）；⑤ **指引断言**：第 4 段含「当前版本不支持非默认端口」且**不含** `http.allowed_domains`（口径 15；域名白名单管 host、不管端口） |

### 6.2 本文固定口径

**编号冻结**（后续修订只追加、不改号）：

1. **重定向语义（评审裁决 #1）**：**只对 301 / 302 / 303 / 307 / 308 执行重定向**——**不得**把所有 3xx 当跳转；301 / 302 / 303 → 方法转 `GET` 且丢弃 body（同时清除正文相关头）；307 / 308 → 保持原方法与 body；**其余 3xx（如 304）不跳转**，按普通响应返回（口径 8）。跨跳请求头规则见约束 30；方法 / body / 头的语义集中实现在 `httpRequestSpec.next` 一处。
2. **`Retryable` 取值（评审裁决 #2）**：本 US 两件工具一律 `false`，语义为「**当前不给出可重试建议**」——**不得**把该字段读成"运行时会不会自动重试"（运行时零重试是契约 §3.3 的独立承诺；传输层的条件重试见注 10）。
3. **网络层失败归类（评审裁决 #3）**：DNS 解析失败 / 连接被拒 / TLS 握手失败 → `*core.ExternalError` ⇒ `external_error`；**取消与超时保留错误身份、优先归 `timeout`**——工具侧两个错误点（`Do` 与响应体读取）先判 ctx 再包装（约束 20），执行器分类时 ctx 判定也在外部错误之前（流程 C）。依据：既有先例 `provider/client_test.go:236`（注 5）。
4. **跳数口径**：`hop` 计"已跟随的重定向次数"，**初始请求不计**；`hop > 5` 即拒（`sandbox_denied`）⇒ doer 最多收到 6 次请求。
5. **`timeout_seconds` 非正值**：按缺省处理（`env.Timeout`），不报错、不产生新错误路径。
6. **shell 成功内容**：首行 `退出码：<N>`，换行后接 `CommandResult.Output`（含 runner 追加的截断标记）；**不**追加其他装饰（工具不发明格式）。
7. **HTTP 响应体读取**：至多 `env.MaxOutputBytes + 1` 字节（约束 12）；截断标记由执行器追加一次。**预算口径 = 最终 `Content`**（含首行 `HTTP <status>` 的开销——与 shell 的 `退出码：<N>` 首行同款；链路断言见 §4.3 R3）。
8. **HTTP 普通返回**：**非重定向、且 < 400**（2xx 与被排除在跳转之外的 3xx）→ 首行 `HTTP <resp.Status>`（如 `HTTP 200 OK`），换行后接响应体原文；空体 = 只有首行。
9. **HTTP 失败文案（4xx / 5xx）**：`HTTP <resp.Status>：<host>`（`*core.ExternalError`）——**不含** query 与响应体（query 可能带凭证、响应体可能极大且不可信）。
10. **3xx 缺 `Location` / `Location` 不可解析**：判 `external_error`（外部系统给不出目标，不是我们的错误）；相对 `Location` 按当前 URL 解析；**不回显 `Location` 原文**（可能带 userinfo / 凭证）。
11. **请求头（单跳）**：逐项 `Header.Set`，**不**加黑名单——`Host` / `Content-Length` 由 `net/http` 自管，模型给的同名头不生效、也不报错；不新造"头部白名单"概念。**跨跳规则见约束 30**（同源转发 / 跨源清空 / POST→GET 清正文头；冻结点在 `next` 一处）。
12. **`tool list` 渲染**：按 `Names()` 顺序逐行 `<名称>：<描述>`；末尾空行 + `共 N 件工具`；空池 `未注册任何工具`；**不**打印参数 schema（`tool show` 类形态归管理域，US-12）。
13. **端口返回值**：`cli.ToolLister.Tools()` 由 `app` 侧实现为 `registry.Specs(registry.Names())`——顺序与渲染同源，命令面**不得**重排。
14. **`Command.Env` 恒为空**：契约 §5.2 没有 env 参数；透传白名单在 runner 侧统一执行（约束 8）。
15. **端口策略（2026-10-17 用户裁决，冻结）**：放行面只由 **scheme 与端口的组合**决定，逐格冻结——`http` + 省略端口 / `http:80` → **放行**；`https` + 省略端口 / `https:443` → **放行**；**其余一律拒**（`http:443`、`https:80` 交叉档，`http` / `https` 配 `8080`、`8443` 等非默认档）。判定归 `checkHTTPRequest`（前序改造项 §3.5 第 9 项）；拒绝 → `sandbox_denied`，四段式第 2 段点名 **host 与端口**（**不含** userinfo / query），第 4 段指引固定写「**当前版本不支持非默认端口**」（新常量对 `ruleNonDefaultPort` / `guidanceNonDefaultPort`）——**不得**提示"加入域名白名单即可解决"（域名白名单管 host、不管端口，指向它等于给出无效指引）。**初始请求与每一次重定向目标同判、拒绝发生在发送之前**（工具侧逐跳循环每跳都调它——`next` 不豁免判定）。依据：技术方案 §6.5「校验 scheme + host + 端口」；域名放行 ≠ 端口放行（同一域名的非默认端口可能挂完全不同的服务）。**边界如实登记**：本地 8080 类服务（开发机 / 内网 API）在当前版本**不可达**；放行通道须新增配置键 ⇒ 归**扩展阶段**（§5.2），改判只动 `defaultPortFor` 与其用例。
16. **出站 `body` 上限 256 KiB（UTF-8 字节计）**：超限 → `*core.InvalidInputError` ⇒ `invalid_input`、**零发送**（约束 29）。**这是本 US 的新增裁决，契约与方案均无此规定**——依据评审裁决 #5：上下文预算约束不了模型给出的工具参数大小。保护范围（不发出 + 该行终态记非法入参）与局限（审计预写先于工具执行，行会留下）见约束 29。
17. **`ExternalError` 的文案与原因链**：字段 `{Target, Reason, Cause}`；`Error()` 输出**自产摘要**（类别 + host / 状态码），**不得**带 URL 原文 / query / 响应体 / Location 原文（`url.Error` 的原文里带整条 URL——直接透传即泄漏）；`Cause` 只经 `Unwrap()` 供 `errors.Is` / `errors.As` 分类与排障，**不进文案**。类别按错误**类型**判定（DNS 解析失败 / 连接失败 / TLS 握手失败 / 响应读取失败——与 `cli` 侧 `yamlErrorSummary`「类型是代码事实、原文是数据」同款纪律）。
18. **入参非法（`invalid_input`）的承载与判序**：工具自身解码 / 取值校验失败 → `*core.InvalidInputError`（语义 = 模型给的参数不合法，不是本仓缺陷——注 11）；`classifyToolError` 判序 = 沙箱 → **入参非法** → ctx → 外部系统 → 文件不存在 → 兜底（流程 C）；执行器的前置校验（合法 JSON 对象 + 必填键，`checkToolInput`）与工具内的类型 / 取值校验**分层不重复**——前者在入口、后者在解码点，两处都产出同一类别。

19. **`http_post` 的正文语义与 `null` 判据（2026-10-17 用户实测回合，编号追加）**：① **正文类型随方法定，不随正文内容定**——只要这一跳是 `POST` 就设 `Content-Type`（`spec.ContentType`，缺省 `application/json`）：**空串 body 是合法的"无正文"写法**（约束 18：在场即算），但请求本身携带正文语义，漏设会让服务端按别的猜测解释正文；301 / 302 / 303 转出的 `GET` 不设，且 `next` 已清模型给的正文头（约束 30）。② **`null` 不是"空正文"的合法写法**：`json.Unmarshal` 把 `null` 解进 string 字段不留任何痕迹（等同缺席），执行器的必填键检查又只看键存在——两者叠加会让 `body: null` 静默通过并**真发出去**；判据落在**工具解码点**（口径 18 的分层：执行器管必填键、工具管类型与取值）：`body` 声明为 `*string`——`null` 得 `nil`、`""` 得非 `nil`，二者由此分清；`nil` → `*core.InvalidInputError` ⇒ `invalid_input`、**零发送**。`http_get` 的 schema 没有 `body`，它的 `"body": null` 只是被忽略的多余字段（与其余多余字段同待遇），**不**判。③ 依据：约束 29 的"零发送"与口径 18 的分层是同一套判据面，本条只是把"POST 正文语义"与"null 判据"两个此前未写明的点补齐（2026-10-17 用户实测回合，落案 #14）。

### 评审回合裁决落案表

**评审回合（2026-10-17，评审者逐条对照活代码）**——原「待决事项」1~6 全部裁决（编号不复用；#13 为用户对 6 号的终裁），P1 / P2 与其余修补逐项落案。落点用本文件章节号；影响面只记"写了什么"。

| # | 事项 | 裁决 | 落点 | 影响面 |
| --- | --- | --- | --- | --- |
| 1 | 重定向跨跳的方法与请求体语义（原待决 1） | **有条件通过**：301 / 302 / 303 → `GET` 且丢 body；307 / 308 → 保持方法与 body；**只对这五种状态码执行重定向**，其余 3xx（304 等）按普通响应返回；补**跨源请求头规则** | 口径 1 / 8 / 10；流程 B ④；约束 30 | `next` 一处集中（含头规则）；用例改扩（303 / 307 / 304 / 跨源头） |
| 2 | `Retryable` 取值口径（原待决 2） | **通过**：一律 `false`；措辞写「当前不给出可重试建议」，**不得**与该字段和运行时自动重试混为一谈 | 口径 2；注 7；注 10（传输层例外单列） | `result_json.retryable` 恒 false；无控制流读它（现状不变） |
| 3 | 网络层失败归类（原待决 3） | **通过** `external_error`；**但取消与超时保留错误身份、优先归 `timeout`** | 口径 3；约束 20；流程 B ③ / ④ | 工具侧两个错误点先判 ctx 再包装；`classifyToolError` 顺序不动 |
| 4 | `RunnerOptions.MaxOutputBytes` 接法（原待决 4） | **不新增配置键**；**否决固定传 0**：首次装配接既有键 `budget.max_output_bytes`（运维配置——"Profile 收紧 budget"不成立，Profile 无该覆盖权）；逐 Run 更小值经 `Command.MaxOutputBytes` 到达读取端 | 注 3；约束 28；口径 7；§3.5 第 2 / 10 项 | `action.go` + `runner.go` + `wire.go`；追加 runner 用例 2 条、链路用例 R3 |
| 5 | `http_post` 出站 body 上限（原待决 5） | **不采纳"默认无限制"**：新增上限 256 KiB（UTF-8 字节计），超限 `invalid_input`、零发送；**明确标注为新增裁决、不冒称契约已有规定**；原"上下文预算约束模型输出"的理由**不成立**（预算约束模型输入，不约束工具参数大小；工具内限制出站也不能阻止此前已发生的审计预写） | 口径 16；约束 29；§3.4（保护范围与局限如实登记） | `decodeHTTPToolInput` 校验段；追加用例（超限零请求 / UTF-8 字节计） |
| 6 | P1-1：出网错误身份与读取错误 | `Do` 与读取错误**先判取消 / 截止**、原样上抛；普通失败包 `ExternalError`（加 `Cause` + `Unwrap`，`Error()` 只出安全摘要）；**不得**丢弃读取错误（原稿 `n, _ =` 形态否决） | 约束 20；口径 17；流程 B ③ / ④；§3.1 core 行 | 追加 core 分类用例 2 条、HTTP 读取错误用例 1 条 |
| 7 | P1-2：跨跳请求头 | 见裁决 1 的补规则——同源（scheme + host + 端口全同）转发 / 跨源一律不转发 / POST→GET 清正文头；"3xx" 收窄为五种、304 另定 | 约束 30；口径 1 / 8 / 10 | `next` 一处；追加用例（同源 / 跨源） |
| 8 | P1-3：端口判定缺失 | 补成**前序改造项**（policy 增端口规则 + 直接请求 / 重定向两条路径的测试）；**不再冻结"Policy 零改动、端口已覆盖"**；具体策略经用户终裁 = 口径 15 冻结组合（见 #13） | 约束 15；口径 15；§3.5 第 9 项 | `policy.go` + 用例 4 条（默认端口放行 / 非默认端口 / 交叉档 / 指引与脱敏） |
| 9 | P1-4：预算接读取端与链路口径 | 接配置预算；逐 Run 值经 `Command.MaxOutputBytes`；**预算口径 = 最终 `Content`**（含状态行 / 退出码行开销）；补"工具 → Executor → `result_json`"链路测试 | 约束 28；口径 7；注 6；§4.3 R3 / R6 | `action.go` / `runner.go` / `wire.go`；用例见 §3.2 |
| 10 | P1-5：凭证脱敏 | 策略拒绝分支脱敏（解析失败分支与 IP / host 分支均不回显完整 URL）；失败路径脱敏回归（秘密 URL / 畸形 Location / 非法头 / 网络失败 → 日志与错误文本零秘密 / userinfo / query）；§3.4 **不得**笼统承诺"凭证不落库"——本 US 不注入运维凭证、模型参数按契约审计 | 约束 27；口径 17；§3.4 末行；§3.5 第 9 项；§6.1 第 9 / 12 项 | `policy.go` + 用例；§3.4 表述 |
| 11 | P2-6：harness 缺陷 | 新测试文件与生产**同包**（不导出生产 API）；R5 先建父 deadline 再断言只缩短；R4 阻塞体在 ctx.Done 返回**取消错误**（非 EOF）、等信号设超时、`t.Cleanup(cancel)`；CLI 签名统一 `Deps` 值类型；CLI 与 §3.1 签名对齐 | §4.1；§4.3 R1–R8；§3.1 cli 行 | 仅测试与签名，无行为变化 |
| 12 | 其余修补 | ① argv 判据收窄（`-c` 只对解释器通道查参数形态，`git -c …` 属正常 argv）；② 新增 `InvalidInputError` 承载入参类型错（注 11）；③ 补 httptest 真链路用例（默认层允许本地回环；§3.3 删"httptest 不需要"）；④ `tool list` 表述改"清单读取本身不修改状态"（注 9）；⑤ 区分应用层零重试与传输层条件重试（注 10）；⑥ 本篇 MD036 等格式问题随本修订修复、链接修正 `../EngineeringStandards.md`、其余重路径 / 只读措辞统一 | 约束 1 / 27 / 29；注 9 / 10 / 11；口径 18；§3.3 / §4.1 / §4.3 | 用例条数变化见 §3.2（新增 51 + 追加 15 + 判据扩 1 处） |
| 13 | 待决 6：非默认端口**是否放行**（用户 2026-10-17 终裁） | **只允许 scheme 的默认端口**——`http` + 省略端口 / `http:80`、`https` + 省略端口 / `https:443` **放行**；`http:443`、`https:80`、`8080` / `8443` 等一律拒、归 `sandbox_denied`；拒绝指引固定写「**当前版本不支持非默认端口**」，**不得**提示"加入域名白名单即可解决"；**初始请求与每一次重定向同判、拒绝发生在发送之前** | 口径 15；约束 15；流程 B ①；§2.1 HttpTools 行；§3.1 `policy.go` 行；§3.5 第 9 项；§6.1 第 12 项 | `ruleNonDefaultPort` / `guidanceNonDefaultPort` 常量对；policy 用例 4 条；**跨跳回归 1 条**（白名单域名重定向到非默认端口 → 目标请求零发送）；**待决事项至此清空**；边界（本地 8080 类服务不可达）如实登记 |
| 14 | 用户实测回合（2026-10-17 用户四项实测：1 P1 + 3 P2） | **P1** `ctxPreferred` 命中 ctx 身份时**只回哨兵本体**——**不得**上抛传输层的包装错误（`url.Error` 原文含整条 URL 与 query，会进模型可见文案 / WARN 日志 / 审计 `error_message`；`errors.Is` 乃至 `==` 的分类能力由哨兵保留）；**P2a** `body: null` 判 `invalid_input`、**零发送**（`*string` 区分 null 与空串）；**P2b** POST 恒设 `Content-Type`（空串 body 也设；301 / 302 / 303 转出的 GET 不设）；**P2c** 转 GET 时补清 `Content-Encoding`（三个正文头同扫，大小写变体同判） | 约束 20（措辞收紧）；口径 19（新增）；流程 B ② / ③ / ④；约束 30 | `ctxPreferred` 两条路都回哨兵；`decodeHTTPToolInput` 增 `carriesBody` 形参与 null 判据；正文类型条件改判方法；`withoutBodyHeaders` 清单补第三项；**回归 4 条**（`TestHTTP_InternalDeadlineNeverLeaksURLText` / `TestHttpPost_NullBodyIsInvalidInputBeforeSend` / `TestHttpPost_EmptyBodyStillSetsContentType` / `TestHTTP_PostToGetRedirectClearsEncodingHeader`）——红态经 `go test -overlay` 逆改旧行为逐条复现（探针建在仓库外、用毕即删）、绿态复跑通过；§3.2 条数 28 → 32 |

### 收口补登

**尚无**——收口时按 US-4 的 F 编号格式补登（实现与文档的偏差、后补登记项）。

### 已决事项

1. **工具不写审计、不写消息**（宪法 II / V + 契约 §7.2）：两阶段写与结果消息的唯一归属分别是 `ToolExecutor` 与循环；本 US 零改动该链路。
2. **三档授权判定全部复用 US-4 的 policy 分支**：不复写、不在工具侧预判（约束 15）——"三档授权全部有真实消费方"指的是**驱动**归本 US，**判定**归 US-4。
3. **`external_error` 承载落 `core`**（`ExternalError` + `IsExternalError`），不复用 `provider.CallError`（注 4）；分类分支插在 ctx 判定**之后**（取消 / 超时优先于外部错误——约束 20）；入参非法的承载 `InvalidInputError` 插在沙箱判定之后、ctx 判定**之前**（裁决 12 ② / 流程 C）。
4. **DNS / 连接 / TLS 失败归 `external_error`**（注 5 的既有先例）。
5. **`tool list` 属重路径**（注 2 的推导链）：`heavyCommands` 增 `"tool"`。
6. **截断责任分工**（注 6）：runner 限流点追加、执行器按 `env.MaxOutputBytes` 追加、HTTP 侧以"多读 1 字节"驱动执行器——标记恰一次。
7. **零新增坐标 / 零配置键 / 零迁移 / 零新表**（约束 26）：`go.mod`、`basalt.yaml.example`、`schema.sql` 全零改动；白名单装配期一次构建 ⇒ 生效时机 = 重启。
8. **端口策略补进前序改造项**（裁决 8 / 约束 15）：`checkHTTPRequest` 现状**没有**端口规则——不再冻结"Policy 零改动、端口已覆盖"；判定在策略层一次落地，直接请求与重定向目标两条路径同源；只允许默认端口（放行口径 = 裁决 13 / 口径 15，2026-10-17 用户终裁）。
9. **出站 body 上限 256 KiB 是本 US 的新增裁决**（裁决 5 / 约束 29）：契约 §5.3 与技术方案均无此规定——落案表与本条都如实标注，**不**冒称契约已有；超限 → `invalid_input`、零发送。
10. **入参非法用 `*core.InvalidInputError` 承载**（裁决 12 ② / 注 11 / 口径 18）：不复用字符串前缀匹配，也不在工具侧自定义错误类型；执行器入口校验与工具内取值校验分层不重复。
11. **重定向只认五种状态码**（裁决 1 / 口径 1）：301 / 302 / 303 / 307 / 308 判跳转，304 等其余 3xx 按普通响应返回。
12. **端口策略（用户 2026-10-17 终裁，裁决 13 / 口径 15）**：只允许 scheme 的默认端口——`http` + 省略端口 / `:80`、`https` + 省略端口 / `:443` 放行；`http:443`、`https:80`、`8080` / `8443` 等一律拒 `sandbox_denied`；指引固定「当前版本不支持非默认端口」、**不得**指向域名白名单；初始请求与每次重定向同判、拒绝先于发送。**边界如实登记**：本地 8080 类服务在当前版本不可达，放行通道归扩展阶段。

### 待决事项

**空**——原 1~5 号与 6 号**全部裁决**（编号不复用，见上表与「已决事项」第 12 条），**设计面冻结**：本文件的每一条边界行为都有出处（契约 / 技术方案 / 本文固定口径 / 用户裁决），实现期如需偏离，按 `/module-dev` 的差异登记流程走，不得静默改判。
