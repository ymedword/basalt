// Package tool 承载工具与沙箱：工具注册、授权判定、受控命令执行与内置文件工具。
// Package tool holds tools and the sandbox: registration, authorization,
// controlled command execution and the built-in file tools.
//
// 本包当前交付：注册表（显式注册、重名拒绝、名字集顺序稳定）、白名单授权（Policy
// 的 v1 实现）、受控命令执行器（CommandRunner 的 v1 实现及平台半边）与十一件内置
// 工具：read_file / write_file / list_dir、shell、http_get / http_post、
// save_memory / recall_memory、notify、manage_schedule 与 web_search（第 11 件）。调度写入同样
// 经过工作区写闸门（`AgentLoader.ApplySchedule`，唯一写入路径）——本包只做入参
// 校验与分派，不自行落盘。
//
// 两条原则约束先行（.specify/memory/constitution.md）：
//   - 授权与执行分离为 Policy.Check 与 CommandRunner.Run 两接口（第 6 条）：调用方
//     只依赖接口，**不得**依赖 WhitelistPolicy / LocalRunner 等具体实现——升级
//     容器化执行只换实现、不改调用方；
//   - 工具调度唯一归属 core 的 ToolExecutor（第 2 条）：本包只做"判定 → 执行 →
//     返回值"，**不**写审计行、**不**写会话消息——审计的两阶段写与失败分类归调度方
//     与仓储。
//
// 授权矩阵摘要（面向实现者；逐条判据与配置键见技术方案 §4.3「授权矩阵」）：
//   - 判据只看"谁、要碰什么"：Action 的 Type / Target / Agent 三项；"打算怎么做"
//     （命令行参数、执行细节）不携带授权；
//   - 文件读：可读根 = 当前 Agent 自己的目录 + 技能库；跨 Agent 读取须显式授权
//     （未授权即拒并点名配置键）；目标无法投影为真实路径（断链 / 成环 / 无权限）
//     一律失败关闭；
//   - 文件写：范围收窄为当前 Agent 自己的目录；保护规则（技能库、scripts/、
//     AGENT.md 与受保护配置、memory/）**先于**一般放行判定；
//   - 命令：可执行名与白名单**整串逐字**比对（多词串必拒）；解释器只接受
//     [脚本路径, 参数…] 形态，内联代码形态（-c 等）一律拒；
//   - 网络：域名通配白名单（大小写不敏感）、scheme 固定 http / https（**不可**经
//     配置放行）、每一跳发送前校验且跳数有上限；IP 字面量目标直接拒。
//
// 拒绝是模型可见契约：一律返回四段式文案（规则 / 目标 / 出处 / 放行指引），放行
// 指引点名配置键（契约 agent-md.md §6）。
//
// 执行控制摘要（面向实现者；设计原文见技术方案 §4.3「执行控制」）：子进程**不得**
// 继承调用进程完整环境（只透传配置列出的变量）；超时 / 取消终止**整棵进程树**
// （Windows 走 Job 对象、Unix 走进程组，"启动与关联之间"的窗口必须消除）；输出
// 边读边受上限约束、截断只记生效上限；argv 直执，**不经**任何 shell 解释。
//
// 失败类别不在本包定义：本包只返回错误与结果，由调度方按契约 run-and-errors.md
// §7.3 归并进审计 error_kind（七值）；审计的 fail-closed 两阶段写见同文 §7.2。
//
// 逐字口径的出处（本文件不复制其正文）：
//   - 内置工具入参 / 出参 / 失败类别：docs/contracts/agent-md.md §5.1；
//   - 拒绝信息四段式：docs/contracts/agent-md.md §6；
//   - 授权矩阵与执行控制的设计原文：docs/TechnicalDesign.md §4.3；
//   - 工具接口与工具池组装：docs/TechnicalDesign.md §4.1。
//
// The package currently ships the tool registry (explicit registration, duplicate
// rejection, stable name order), the v1 whitelist policy, the v1 command runner
// with its per-platform halves, and ten built-in tools: read_file / write_file /
// list_dir, shell, http_get / http_post, save_memory / recall_memory, notify, and
// manage_schedule. Schedule writes go through the workspace write gate
// (AgentLoader.ApplySchedule) as well; this package only validates and dispatches
// and never writes files itself. The constitution binds
// here first-hand: authorization and execution stay apart as Policy.Check and
// CommandRunner.Run (principle 6 — callers must not depend on concrete
// implementations), and tool scheduling belongs to core's ToolExecutor
// (principle 2) — this package neither writes audit rows nor session messages.
// Refusals are model-facing and always four-part (rule / target / source /
// guidance) per contract agent-md.md §6. Failure classification lives in the
// caller (contract run-and-errors.md §7.3); this package only returns errors and
// results. See docs/TechnicalDesign.md §4.3 for the authorization matrix and
// execution control, and §4.1 for the tool interface and pool assembly.
package tool
