# Implementation Plan: US-5 Shell 与 HTTP 工具（内置工具补齐六件）

**Branch**: `005-shell-http-tools` | **Date**: 2026-10-17 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/005-shell-http-tools/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 5` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us5-shell-http-tools.md](../../docs/us/us5-shell-http-tools.md) §2 / §3 为准，冲突时以 [docs/TechnicalSolution.md](../../docs/TechnicalSolution.md) 为准（§6.1 工具接口 / §6.2 内置工具与工作目录约定 / §6.5 授权与执行）。

## Summary

接通沙箱最后两档的**首个真实消费方**，让 Agent 第一次真正动手：内置工具从三件补到**六件**（新增 `shell` / `http_get` / `http_post`，共用一个 argv 直执通道与一份逐跳出网循环），沙箱三档（路径 / 命令 / 域名）**全部有真实消费方**，命令面补 `basalt tool list` 现场核对「注册了什么」。

技术路线的三个要点：**① 出网链路——逐跳判定只此一处 + 端口冻结**——两件 HTTP 工具**共用同一份逐跳循环**（`executeHTTP`）：**每一跳在发送之前**调一次 `policy.Check(ctx, Action{Type: ActionHTTPRequest, Target: 本跳 URL, …})`（scheme / 主机 / 端口 / 域名白名单**只此一处**判，工具不预判、不改写目标串）；HTTP 客户端**不自行跟随重定向**（`CheckRedirect` 返回 `http.ErrUseLastResponse`），3xx 由工具决定怎么跳——**只有 301/302/303/307/308 五种**（303 转 `GET` 并清正文头、307/308 保持方法与 body、304 等按普通响应返回），跳数上限 **5**（初始请求不计，超限判**沙箱拒绝**而非外部错误），跨跳请求头按**同源规则**（跨源一律不转发模型头，`POST` 转 `GET` 时清正文头——语义集中 `httpRequestSpec.next` 一处）；**端口档按 2026-10-17 用户终裁冻结**（`http` 配省略 / `:80`、`https` 配省略 / `:443` 放行；交叉档与非默认档一律拒、拒绝先于发送、指引固定「当前版本不支持非默认端口」且不指向域名白名单）；`Transport.Proxy` 恒 `nil`（不读环境代理，否则域名档形同虚设）。**② 命令链路——argv 直执 + 预算真正到读取端**——`shell` 把可执行名（**整串**，不拆分）与参数逐项拆开装进 `core.Command`，**不经任何 shell**；`bash` / `sh` 在策略侧**硬拒先行**；`timeout_seconds` **只缩短**（非正值按缺省、不报错）；cwd = Agent 目录（无上下文走进程默认，**不得**全局 chdir）；`Command.Env` 恒空；**非零退出码不是错误**（首行「退出码：N」）；输出**原样透传**（截断标记归执行侧）；`core.Command` 增 `MaxOutputBytes` 字段把**逐 Run 更小的读取上限**送到读取端，`LocalRunner` 取「装配值与逐命令值更小者」并夹到 256 KB——装配值接**既有** `budget.max_output_bytes` 键（`LocalRunner` **首次生产接线**：US-4 交付了实现却零消费方）。**③ 分类补齐与命令面收口**——`core` 新增两类**平级**错误承载（`ExternalError` + `IsExternalError`、`InvalidInputError` + `IsInvalidInput`，均**判类型不比文本**），`classifyToolError` 增两条分支（判序：沙箱 → 入参非法 → 取消/截止 → **外部系统** → 文件不存在 → 兜底；七值字面量冻结不变）；既有三件文件工具的解码 / 取值错误**同批改载体**（文案不变、只换类型）；`basalt tool list` 经**消费方定义的端口**取值（`cli` 不得 import `internal/tool`）、属**重路径**（「只读」= 清单读取本身不修改状态）；装配池 3 → **6 件**并同步过期占位注释。

## Technical Context

**Language/Version**: Go 1.27.2 构建版本（`go.mod` 的 `go` 行写最低版本要求 `go 1.27`；准确构建版本由 `toolchain go1.27.2` 与 CI 的 `actions/setup-go` 共同固定——**不得**表述为「锁定 1.27.2」）

**Primary Dependencies**: **零新增坐标**（宪法 VIII）。只用标准库 `net/http`（`http.Client` / `CheckRedirect` 返回 `http.ErrUseLastResponse` / `http.Transport.Proxy` 置 nil / `net/url` 解析 / `io.LimitReader`）+ 测试侧 `net/http/httptest`（**默认层允许本地回环** `127.0.0.1:0`，工程规范 §2 明列）。既有坐标全部不动（`spf13/cobra` v1.10.2 供 `tool` 命令组、`stretchr/testify` v1.12.1 供断言）；不引任何 HTTP 客户端库（`net/http` 是固定技术栈句内明列项）；无新增 `import _` 副作用。

**Storage**: SQLite——**六表既有 DDL 零迁移、零列变化**（判据：`git diff docs/contracts/schema.sql` 为空）。列级口径逐条重申：`tool_invocations` **无 `truncated` / 无 `round` 列**（截断标记与 `truncated` 都在 `result_json` 值内，格式 `{"content":…,"retryable":…,"truncated":true}`）；`input_json` = 模型给的 arguments **原样**（`headers` / `body` 里的内容因此也按契约在库内——如实边界，**不得**笼统承诺「凭证不落库」）；`error_kind` 取值集**冻结不变**——本 US 只新增**产出方**：`external_error` 自 US-2 起已在 `llm_calls` 上写入（供应商侧），本 US 起**首次可出现在 `tool_invocations` 上**；`runs` / `config_snapshots` 零改动；审计两阶段写与失败分类落库的全部逻辑仍只在 `ToolExecutor`（工具不写审计、不写消息——FR-023）。

**Testing**: `go test -race ./...` 默认层——新增 3 个测试文件（`tool/shelltools_test.go` 17 条 / `tool/httptools_test.go` 28 条 / `cli/tool_test.go` 6 条 = **51 条**）+ 改动 7 个测试文件追加 **15 条**（`core/tool_exec_test.go` 4 / `app/ports_test.go` 2 / `app/run_test.go` 1 / `tool/filetools_test.go` 2 / `tool/policy_test.go` 4 / `tool/runner_test.go` 2）+ `cli/ports_test.go` 判据扩 1 处；条数与颗粒度文档 §3.2 对齐（收口逐文件 `grep -c '^func Test'` 核对）。**不访问真实外网、不起真实子进程、不打真实模型**：HTTP 以**进程内假 doer**（`*http.Response` 由 `strings.Reader` 拼出，零 socket）为主 + **少量 httptest 真链路**（跳转 / 取消 / `Proxy=nil` 三条行为假 doer 判不了——生产 `newHTTPClient` 必须有真链路用例），shell 以**假 runner**（`core.CommandRunner` 实现，零进程，记账 `Command` 形态），清单以**假端口**（`cli.ToolLister` 实现）；链路截断用例经**真实 `core.ToolExecutor` × 假审计仓储**（仓储只做 I/O 边界，不得再造执行器）。判卷纪律四条（§4.1）：逐跳用例**两侧记账**（发了几次 + 判了几次判的哪个 target）、取消用例**等「请求已发出」信号再取消**并限时返回、全部在 `-race` 下跑且共享夹具带锁、链路用例经真实执行器。测试文件与生产**同包**（`package tool` / `package cli`），夹具走包内接缝（`newHTTPTool` / `newHTTPClient`），**不**为测试导出生产 API、**不**扩 `internal/testutil` 公开面；夹具 `t.TempDir()` 内联；时间显式传入。**集成层本 US 无新增**（`make test-integration` 跑既有）；评测基座不改（Demo 一按人工项执行）。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布产物 `CGO_ENABLED=0` 交叉编译三平台；**本 US 无新平台分片文件**（不新增 `_windows` / `_unix` 后缀文件）；`-race` 与发布构建**分属不同命令 / job**。

**Project Type**: CLI + 服务单二进制（本 US 新增命令组 `tool`——注册点按 US 增补的既有约定；**不新增内部包**、不新增顶层目录）

**Performance Goals**: 无显式吞吐 / 延迟指标（底座零件类 feature）；判据是**行为顺序与资源上限可解释**——每一跳**先判后发**（拒绝先于发送，跨跳路径实测「目标请求零发送」）；跳数上限 5（假 doer 恰收 6 次请求）；出站 body 上限 256 KiB（超限零发送）；响应体读取上限 = min(逐 Run 生效预算, 256 KB) **+ 1 字节**（让执行器 `len > maxBytes` 判据必定命中、标记恰一次）；`timeout_seconds` 只缩短（四态：缺省 / 更短 / 更长不生效 / 非正值按缺省）；逐 Run 预算 ≤ 装配值（读取侧取更小者再夹 256 KB——配置收紧了预算子进程不得照旧读满）。

**Constraints**: 三条包纪律的落法——`tool` 只 import `core` 与标准库（授权只经 `core.Policy`、执行只经 `core.CommandRunner`，**不得**自建 `exec.CommandContext` 第二套路径）；`core` 只依赖标准库（既有判据 `go list -f '{{.Imports}}' ./internal/core` 只出标准库）；`cli` **不得** import `internal/tool`（清单经消费方端口 `cli.ToolLister` + `app` 适配器，判据扩面覆盖新面）；`app` 是唯一装配层（注册池 +3、`LocalRunner` 首次接线、`heavyCommands` 增 `tool`）。**逐跳判定只此一处**：工具不得本地预判 scheme / 端口 / 域名，策略不得信任工具传来的"已检查"标记。`context` 只承载取消 / 截止 / 追踪（`ToolEnv` 显式传参）；工具执行**同步**、无异步编排、不起后台读取。审计链路**零改动**（`pending` 预写先于工具执行、终态回填与失败分类全在既有 `ToolExecutor`——本 US 的拒绝 / 超限路径因此**仍留审计行**）。**可观测性双轨**：工具本体**不记日志**（失败路径 WARN 固定键由执行器承载、键集逐字不变）；`tool list` 属重路径、启动四步各记**既有**日志——「只读」指清单读取本身不修改状态，**不得**写成「整命令零日志」。错误不吞（`err` 必回填 / 上抛）。敏感面：日志、模型可见文案与错误文案**不得**含 userinfo / query / headers / body 内容；策略拒绝分支**目标脱敏**（`policy.go` 两处 `action.Target` 回显改安全文案——本 US 改造点 §3.5 第 9 项）。覆盖率：`tool` ≥70%、`cli` 与其余 ≥50%，由既有 `make cover` 按包判失败（**不改**脚本与阈值）。

**Scale/Scope**: 代码 **新增 3 文件 + 改动 12 文件**（`core` 3 改 / `tool` 2 新 3 改 / `cli` 1 新 3 改 / `app` 3 改）+ 测试 **新增 3 文件 51 条 + 改动 7 文件追加 15 条 + 判据扩 1 处**（合计 25 文件，与人工项 10 的 `git diff --stat` 判据同口径）＋ 颗粒度文档本身；`go.mod` / `go.sum` / `basalt.yaml.example` / `Makefile` / CI / `docs/contracts/` / `docs/Operations.md` **零改动**；**零迁移脚本**；前序改造点 **11 项**（逐条软门禁——§3.5）+ 后续占位 6 项（只留装配位与文本占位，**不实现**）。

**环境实测读数（2026-10-17，本轮当场命令）**: `go version` → `go1.27.2 windows/amd64`（与 `toolchain go1.27.2` 一致，退出码 0）；包清单逐目录读取——`internal/tool` 13 文件（`doc.go` / `filetools{,_test}.go` / `pathguard.go` / `policy{,_test}.go` / `registry{,_test}.go` / `runner{,_test,_unix,_windows,_windows_test}.go`，**无** `shelltools.go` / `httptools.go`——两件新工具文件全新增）、`internal/core` 21 文件（`errors.go` / `action.go` / `tool_exec{,_test}.go` 均在改造面）、`internal/cli` 19 文件（`ports{,_test}.go` / `deps.go` / `root.go` 均在改造面）、`internal/app` 11 文件（`wire.go` / `ports{,_test}.go` / `run{,_test}.go` 均在改造面）；既有预算旋钮 grep 实测——`runner.go:78-81` 的 clamp **只兜非正值、不夹 256 KB 上限**（§3.5 第 10 项待补的收窄与夹取属实）、`core/run.go:293-299` 逐 Run 生效上限 = min(缺省 256 KB, 配置值)、`cli/chat.go:340-345` 的 `int64`→`int` 转换先例（含 `math.MaxInt` 守卫）就地在案；`grep -rn "CheckRedirect|http.Client|Transport" internal/tool/*.go` **零命中**（HTTP 客户端全新增）；`runner.go:56` 注释键名 `shell.max_output_bytes` **在配置面不存在**（注 3 已登记——实现期随 §3.5 第 10 项改动该文件时按「注释是事实登记面」对齐为 `budget.max_output_bytes`）。工具链与静态检查工具就绪读数沿用 US-1 / US-4 plan；本轮**未复测**的项不在此声称（**未跑** `make check`、**未跑**三平台编译、**未读**覆盖率读数）。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

对照 [.specify/memory/constitution.md](../../.specify/memory/constitution.md) 十原则 + 三条架构约束 + 开发工作流（GATE 判定：通过 / 不适用 / 需说明）：

| # | 宪法条目 | 本 feature 关系 | GATE |
| --- | --- | --- | --- |
| I | 自实现 ReAct 循环 | **适用**：`go.mod` **零新增坐标**（收尾 `grep -nE "eino\|langchaingo\|adk-go\|trpc-agent-go" go.mod` 零命中）；本 US 只新增工具本体与分类分支——循环体一字不动 | 通过 |
| II | LLM 客户端只做协议调用 | **适用**：工具调度唯一归属 `ToolExecutor`（本 US 不新增调度路径）；工具**不写消息**（`core.Result` 返回、结果消息追加点唯一在循环）；消息写入三条路径一个出口的形态不变 | 通过 |
| III | Provider 必须显式映射 | **不适用**（本 US 不触碰路由与装配映射表；出网是工具走出站 HTTP，与 provider 路由无关） | 不适用 |
| IV | 一个目录 = 一个 Agent；技能声明式绑定 + 渐进披露 | **适用（弱形态）**：注册池 3 → 6 件后，US-3 装载器的名字集规则继续生效（池内名字更新即生效）；技能面零改动——**不得**新增 `use_skill`（收尾 grep 零命中）、**不得**预载正文 | 通过 |
| V | 审计 day-one 且 fail-closed | **适用（零改动面）**：工具不写审计；`pending` 先写、失败即拒执行的全链路仍是既有 `ToolExecutor` 行为。本 US 的**超限 / 拒绝路径同样先有审计行**（预写先于工具执行）——精确语义已登记：出站 body 超限的效果 =「不发出去 + 该行终态 `failed` / `invalid_input`」，**不是**「库里零痕迹」 | 通过 |
| VI | 授权与执行分离（两接口） | **适用（首个完整消费落地）**：`ShellTool` 持 `core.Policy` + `core.CommandRunner`，**既不** import / 断言 `WhitelistPolicy` / `LocalRunner` 具体类型（编译期判据——`shelltools.go` 只 import `core` 与标准库），**也不**自建 `exec.CommandContext`（第二套执行路径会绕开环境白名单、读取限流与整树终止三条保证）；`LocalRunner` **首次生产接线**在装配层完成（唯一允许"什么都知道"的层） | 通过 |
| VII | 同步执行 + context 只承载取消 / 截止 / 追踪 | **适用（强形态）**：出网逐跳是**同步循环**（无后台读取、无异步编排）；工具经 `core.ToolEnv` 显式取 `AgentName` / `AgentDir` / `Timeout` / `MaxOutputBytes`；`context.WithValue` 零使用（收尾 grep 逐处确认）；取消传播是**强判据**（R4：等「请求已发出」信号再取消、限时返回、不留后台读取） | 通过 |
| VIII | 单二进制纪律 + 依赖最小化 | **适用**：**零新增坐标**（只用标准库 `net/http`；测试用 `net/http/httptest`——标准库，不产生坐标）；本 US 无新平台分片文件；发布 `CGO_ENABLED=0`；`-race` 与发布构建分属不同命令 / job | 通过 |
| IX | 不用 ORM，手写 SQL | **适用（零改动面）**：本 US 不写任何 SQL（审计落库全在既有 `storage`）；**零迁移脚本 / 零列变化** | 通过 |
| X | 五大能力优先，范围卡紧 | **适用**：交付面对照颗粒度文档 §5 逐项边界（并行调用 / 幂等重试 / 流式 / 容器化 / 凭证托管 / `tool show` / REST 端点 / `use_skill` / 非默认端口放行通道全不做——最后一项如实登记归**扩展阶段**：本地 `8080` 类服务在当前版本不可达）；对外概念零发明（导出签名逐字以 §3.1 为准） | 通过 |

**架构约束**：依赖方向 `channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`app` 是唯一装配层——本 US 的落法：`core` 新增两类错误承载与一个字段、只 import 标准库（判据机器可判）；`tool` 新增两文件只 import `core` + 标准库（`net/http` / `net/url` / `io` / `encoding/json` / `time` 等），改造三文件不新增跨包 import；`cli` 新增 `tool.go` 只 import `core`（渲染 `[]core.ToolSpec`）与自声明端口（**不** import `tool`——判据扩面进 `cli/ports_test.go`）；`app` 在装配层接 `tool.NewLocalRunner` / 三件工具构造器与端口适配器；`cmd/basalt` 不动。无环由 CI 用 `go list` 复核。接口定义在消费方：`cli.ToolLister`（清单端口）与既有 `core.Policy` / `core.CommandRunner` / 审计仓储接口同款；实现（`toolListerPort`、`WhitelistPolicy`、`LocalRunner`）放各自包与装配层。第三方实现仍一律不进 `core`（本 US 无新第三方）。

**开发工作流**：门禁一条命令 `make check`（七目标，**`Makefile` / CI / 覆盖率脚本零改动**——`tool` / `cli` / `core` / `app` 自 US-4 起已在判卷内）；三条实测纪律适用（格式判卷**先退出码后输出** / `-race` 与 `CGO_ENABLED=0` **分离** / 本 US **无集成层新增**，`test-integration` 跑既有）；覆盖率不可豁免（`tool` ≥70%、`cli` 与其余 ≥50%）；提交纪律（AI **不自动** commit / push / tag）。

**plan review 必查（宪法 §开发工作流）逐条**：

| 必查项 | 本 plan 的答复 |
| --- | --- |
| 依赖方向无环 | `core` 改动 3 文件 = 标准库 + 同包（收尾 `go list -f '{{.Imports}}' ./internal/core` 判据复跑）；`tool` 新文件只 import `core` + 标准库（授权只经 `core.Policy`、执行只经 `core.CommandRunner`——收尾 grep 无 `exec.CommandContext`、无 `WhitelistPolicy` / `LocalRunner` 类型断言）；`cli` **不** import `tool`（`cli/ports_test.go` 禁 import 清单**增** `internal/tool`——判据扩面）；`app` 装配全部；`cmd/basalt` 不动 |
| 审计写入路径唯一 | 工具不写审计（签名无关联标识参数——`Execute(ctx, env, input)` 形态不变）；`tool_invocations` 两阶段写仍只在 `storage.ToolInvocationRepo`、由 `core.ToolExecutor` 调用；本 US **零改**该链路（FR-023）；`grep -rn '\.BeginCall(' --include='*.go' internal/core` 零命中（不变量 11）复跑 |
| 无框架坐标 | `go.mod` **零新增**（判据 `git diff go.mod go.sum` 为空）；收尾 grep 零命中同上 |
| 无 `context.WithValue` 业务态 | 本 US 新增 / 改动文件逐处审查（收尾 `grep -rn "context.WithValue" --include='*.go' cmd internal` 每处命中人工确认）；`ToolEnv` / `AgentDir` / `MaxOutputBytes` 一律显式参数；`narrowTimeout` 只派生 ctx（取消 / 截止），不回填业务态 |
| 单二进制构建通过 | 本 US 无新平台分片文件（无 build tag 增量）；`build` 目标 `CGO_ENABLED=0 go build`；`-race` 只在 `test` 目标；三平台编译是**人工项**（§6.1 第 8 项，**不进** `make check`） |

**Phase 1 后复评（post-design）**：设计产物（[research.md](./research.md) / [data-model.md](./data-model.md) / [quickstart.md](./quickstart.md)）完成后重跑本表——结论同前，无新增违规、无 Complexity Tracking 条目（见文末）。

## Project Structure

### Documentation (this feature)

```text
specs/005-shell-http-tools/
├── spec.md              # /speckit-specify 输出（需求规格，含 Clarifications 会话 2026-10-17）
├── plan.md              # 本文件（/speckit-plan 输出）
├── research.md          # Phase 0 输出（决策与写前核实清单）
├── data-model.md        # Phase 1 输出（实体、字段与不变量）
├── quickstart.md        # Phase 1 输出（可复制执行的验证步骤）
├── checklists/
│   └── requirements.md  # 需求质量清单（/speckit-specify 输出）
└── tasks.md             # /speckit-tasks 输出（下一步生成）
```

**`contracts/` 本 feature 不新建**（与 US-1~US-4 同口径）：本 US 的固定字面量**已有唯一事实源**——运行时契约在 [docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md)（§6 预算表、§7.2 两阶段写、§7.3 七值 / §7.4 存什么不存什么）与 [docs/contracts/agent-md.md](../../docs/contracts/agent-md.md)（§5.2 `shell` / §5.3 `http_get` / `http_post` 参数表与失败类别、§6 四段式拒绝），列级事实以 [docs/contracts/schema.sql](../../docs/contracts/schema.sql) 为准；导出签名逐字来源在颗粒度文档 §3.1、「本文固定口径」18 条在 §6.2。再抄一份到 `specs/` 就是**第二份字面量**——抄一份必漂移一处。plan 的契约节只**引用**，不复制。

### Source Code (repository root)

```text
internal/
├── core/                      # 只依赖标准库（既有判据机器可判）
│   ├── errors.go              # 改造：增 ExternalError（Target / Reason / Cause + Error 安全摘要 + Unwrap）
│   │                          #       + IsExternalError 与 InvalidInputError（Tool / Reason）+ IsInvalidInput
│   │                          #       （与 SandboxViolationError 平级；判定只认类型不比文本；不动既有哨兵）
│   ├── action.go              # 改造：Command 增 MaxOutputBytes 字段（本次执行读取侧上限；<=0 = 用装配值）
│   └── tool_exec.go           # 改造：classifyToolError 增两条分支（判序见颗粒度文档流程 C——ctx 判定在前）
├── tool/                      # ≥70% 覆盖率判卷（本包只 import core 与标准库）
│   ├── shelltools.go          # 新增：ShellTool（NewShellTool / Name / Description / InputSchema / Execute）
│   │                          #       内部 decodeShellToolInput / command / narrowTimeout / shellContent
│   ├── httptools.go           # 新增：HTTPGetTool / HTTPPostTool（两件共用 executeHTTP 逐跳循环）
│   │                          #       内部 httpDoer / newHTTPTool / newHTTPClient / decodeHTTPToolInput /
│   │                          #       httpRequestSpec.next / httpFailure / maxRedirectHops=5 / maxRequestBodyBytes=256*1024
│   ├── policy.go              # 改造：checkHTTPRequest 增端口规则（defaultPortFor + 常量对 ruleNonDefaultPort /
│   │                          #       guidanceNonDefaultPort）+ 拒绝分支目标脱敏（两处 action.Target 回显改安全文案）
│   ├── runner.go              # 改造：effectiveMaxOutputBytes（min(装配值, 逐命令正值, 256 KB)）+ 过期键名注释对齐
│   └── filetools.go           # 改造：decodeFileToolInput / fileToolInput.check 改载体 InvalidInputError（文案不变）
├── cli/
│   ├── tool.go                # 新增：tool 命令组 + tool list（newToolCommand / newToolListCommand /
│   │                          #       runToolList / renderToolList——渲染独立可测，范式照 provider.go）
│   ├── ports.go               # 改造：增 ToolLister 端口（接口定义在消费方）
│   ├── deps.go                # 改造：Deps 增 Tools 注入位
│   └── root.go                # 改造：注册 tool 命令组
├── app/
│   ├── wire.go                # 改造：注册池 +3 → 六件；LocalRunner 首次接线（MaxOutputBytes ← budget 键，
│   │                          #       int64→int 照既有先例）；heavyRuntime 增 registry 只读访问器；占位注释同步
│   ├── ports.go               # 改造：toolListerPort 适配器（转调 Specs(Names())，保序）+ 编译期断言
│   └── run.go                 # 改造：heavyCommands 增 "tool"；重路径分支接 rt.tools() → deps.Tools
└── （其余包不动：channel / memory / provider / storage / testutil / web / workspace / cmd/basalt）

go.mod / go.sum                      # 零改动（零新增坐标）
basalt.yaml.example / Makefile / CI  # 零改动（键名逐字沿用；门禁与覆盖率判卷已覆盖本 US 的包）
```

**Structure Decision**: 单 Go module + `internal/` 分包；本 US 新增实现文件 3 个、改造 12 个、新增测试文件 3 个 + 追加 7 个（+ 判据扩 1 处），**不新增内部包、不新增顶层目录**。与台账「模块落位（摘要）」列一致：`internal/tool`（ShellTools / HttpTools + policy 端口规则与拒绝文案脱敏 + runner 输出预算下传）、`internal/cli`（`tool list`）、`internal/core`（`external_error` / `invalid_input` 承载与分类分支 + `Command.MaxOutputBytes`）、`internal/app`（装配接线：注册池 +3、`LocalRunner` 首次接线含预算接线、工具清单端口适配器）；`go.mod` 零新增坐标。前序改造点 **11 项**（§3.5 表）全部为**加字段 / 加分支 / 加接口与适配器 / 改注释 / 换错误载体（文案不变）**形态——**不得**删除或改写既有字段、签名与行为；后续占位 6 项（记忆 / MCP / `notify` / `manage_schedule` / REST 工具端点 / 容器化沙箱 / `use_skill` 永久禁）只留装配位与文本占位。

**分层落位（三条包纪律的逐条落地）**：

| 纪律 | 本 US 的落法 |
| --- | --- |
| 依赖方向无环 | `core` 改动文件只 import 标准库（判据 `go list`）；`tool` 新文件只 import `core` + 标准库（含 `net/http`——第三方实现不适用，标准库实现直接在消费方包内）；`cli` 端口在消费方、适配器在 `app`（**不** import `tool`）；`app` 装配全部 |
| 接口定义在消费方 | `cli.ToolLister` 由 `cli` 声明、`app.toolListerPort` 实现（编译期断言 `var _ cli.ToolLister = toolListerPort{}`）；`core.Policy` / `core.CommandRunner` / 审计仓储接口沿既有声明——`tool` 只依赖 `core` 侧接口 |
| 第三方实现放消费方包内 | 本 US 无新第三方；`net/http` 是标准库（与"第三方"纪律无涉）；`toolList` 渲染不经 `tool` 包的导出类型（`core.ToolSpec` 中立） |

**标签口径（`[US1]` 不混淆）**：`tasks.md` 中的 `[US1]` / `[US2]` / `[US3]` / `[US4]` 是 Spec-Kit 对**本 feature 内部**用户故事的编号（对应 [spec.md](./spec.md) 的 User Story），**不是**项目台账（[docs/us/REGISTRY.md](../../docs/us/REGISTRY.md)）里的 US-1 / US-2 / US-3 / US-4——本 feature 的全部故事都落在项目 **US-5（Shell 与 HTTP 工具）** 一个模块内。四个故事 ↔ 颗粒度文档 §4.2 测试类映射表（10 个测试文件面：3 新增 + 7 追加）的分组是本 plan 自己的记账口径，**不改动**颗粒度文档任何内容。

## Complexity Tracking

> 仅当 Constitution Check 存在需辩护的违规时填写。

无违规——本 feature 无 Constitution Check 不通过项，故无复杂度辩护条目。
