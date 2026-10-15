# Basalt AI 编程实施指南

> 本文档是 Basalt 四份权威文档中的**编程实施篇（How 落地）**：把[需求文档](DemandAnalysis.md)（What）与[技术方案](TechnicalSolution.md)（How）翻译成"用 AI 编码工具（Claude Code 等）逐步实现"的可执行流程。行业背景见[行业调研](IndustryResearch.md)。
>
> 本文采用与 NivroOS 相同的 Spec-Kit 式两阶段工作流（准备一次 + 逐 US 增量），但**门禁、语言环境、陷阱全部换成 Go 形态**——NivroOS 第一阶段（US-1~US-7，667 测试 0 失败）用真金白银踩出来的教训，在本文第 7 节按 Go 形态蒸馏成陷阱表，是这份指南里最贵的部分。
>
> **配套工件**（三件套）：[Compatibility.md](Compatibility.md)（与 NivroOS 的逐项三态对照）、[contracts/](contracts/)（运行时契约四件：Run 与错误 / AGENT.md 与工具 / 完整 DDL / 静态 OpenAPI）、[Operations.md](Operations.md)（依赖条件、监听与出网、时区、备份、升级、凭证）。

---

## 1. 实施总览

### 1.1 两个阶段

| 阶段 | 时机 | 谁主导 | 产物 |
| --- | --- | --- | --- |
| **准备阶段** | 一次性，写第一行业务代码前 | 人（AI 辅助） | 项目宪法、仓库骨架、质量门禁、CI、Spec-Kit 工件模板 |
| **增量阶段** | 每个 US 一个循环 | AI 实现，人裁决 | 代码 + 测试 + 可演示成果 + 一个 commit |

**为什么准备阶段要一次性做完**：门禁与宪法是"地基的模具"——先立模具再浇混凝土，比浇完再补模具便宜一个数量级；门禁晚一天接入，就多一批"写完才发现格式/静态检查不过"的返工。

### 1.2 输入映射（文档 → 工件）

AI 编码工具每做一个 US，需要的输入按下表取，**只取所需，不塞全文**（上下文浪费会直接降低产出质量）：

| 工件 | 取自 | 方式 |
| --- | --- | --- |
| spec（这个 US 要什么） | 需求文档对应节 + 本指南 §4 的该 US 条目 | 提炼为 FR 列表（每条可测试） |
| plan（怎么做） | 技术方案对应章节（§3~§10）+ 本指南 §4 的"涉及包" | 落成包级改造点清单 |
| tasks（步骤） | plan 拆解 | 任务清单，含测试任务 |
| constitution（不可违背） | 本指南 §3.1（升格为仓库 `.specify` 宪法） | 逐条引用 |

**颗粒度文档**（NivroOS 实践中最有效的一件工件）在本项目的对应物：每个 US 开工前生成一份"模块执行颗粒度文档"（六节结构：概述 / 设计要点与约束 / 交付物清单 / 验收测试 / 范围边界 / 验证与验收），它是 spec 与 plan 的合并增强版，也是完工对账的底单。工具形态：把 NivroOS 的 `/module-doc-gen`、`/us-register`、`/module-dev`、`/init-foundation` 四个自制 skill 按"复制到新项目只改项目配置区"的设计移植到 Basalt（改写点：门禁命令换 Go、台账路径、技术栈句、全局不变量）。

### 1.3 US 依赖图与节奏

拆解为 **14 个 US**，递进式依赖：每个 US 只消费它左边已交付的能力，且**不出现"先做桩、后面替换"式的返工序**。

```text
US-1 → US-2 → US-3 → US-4 ─→ US-5 ─→ US-9 → US-10 → US-11 → US-12 → US-13 → US-14
                            ├─→ US-6（Memory，只依赖 US-4）
                            ├─→ US-7（MCP，只依赖 US-4）
                            └─→ US-8（通知渠道，只依赖 US-4）
```

主干 = 推荐执行序；挂枝 = 与 US-5 相互独立的 US（可按需调序）；精确硬依赖见下表。

| US | 名称 | 硬依赖 |
| --- | --- | --- |
| US-1 | 工程地基（骨架 / 门禁 / 配置 / 日志 / 工作区 / 持久化） | — |
| US-2 | Provider 对接 | US-1 |
| US-3 | Agent 目录与 Profile 加载 | US-2 |
| US-4 | ReAct 循环与首个工具闭环 | US-3 |
| US-5 | Shell 与 HTTP 工具（Demo 一） | US-4 |
| US-6 | Memory（Demo 二） | US-4 |
| US-7 | MCP 集成（Demo 三） | US-4 |
| US-8 | 通知渠道 | US-4 |
| US-9 | Web Service（Demo 四 / 五） | US-4 |
| US-10 | 定时任务 | US-4（调度本体）+ US-9（管理端点） |
| US-11 | 观测与管理台 | US-9 + US-10 |
| US-12 | Agent 动态管理 | US-11 + US-8（渠道 CRUD） |
| US-13 | 官网展示主页与 README（对外门面） | US-12 |
| US-14 | 发布收尾（三个每日 Demo + v0.1.0） | US-13 + US-5 / US-6 / US-7 / US-8 / US-10 |

- **五大能力 ↔ US 对照**：能力一 → US-2；能力二 → US-4；能力三 → US-6；能力四 → US-4 / 5 / 7 / 8；能力五 → US-9
- **粒度刻意不均匀**：够独立演示的拆细（地基与 Provider 分开各自验收），不够独立演示的不硬拆（US-4 的循环 + 工具是一整条竖切，再拆就是半截状态）
- 每个 US 结束：门禁全绿 → 可演示成果 → 一致性检查 → 一个 commit（commit 标记 US 完成）
- 完不成时：拆出可独立演示的子集先交付，其余挪扩展阶段——**不砍验收口径**

---

## 2. Spec-Kit 匹配度评估

### 2.1 适配良好的部分（照搬）

| 能力 | 说明 |
| --- | --- |
| **spec/plan/tasks 三件套** | 与"需求文档 → 技术方案 → 实施"的文档体系天然同构，逐 US 生成即可 |
| **宪法检查点** | spec/plan 阶段各查一次宪法，防止"做着做着把原则做没了" |
| **逐 US 独立验收** | 每个 US 有独立可演示成果，天然适合"一个 US 一个循环" |
| **一致性分析** | **实施前阻断检查 + 收口复查**：`tasks` 确认（固定软停点）后先跑一次，CRITICAL 问题清零前不进 implement；US 收口再跑一次（实现与文档一致性核对）。对应命令 `/speckit-analyze` |

### 2.2 需要项目化改造的部分

| 项 | 改造内容 |
| --- | --- |
| **门禁命令** | Java 的工具链句全部换成 Go（§3.2）——格式、静态检查、安全、竞态、覆盖率、单二进制构建 |
| **H3 核实纪律**（写前核实第三方 API） | Go 形态核实方式：`go doc <pkg>.<Symbol>` + 在依赖源码里 grep + 写一个最小测试实跑；**不得**凭记忆写 API 名 |
| **版本核实纪律** | 写 `go.mod` 坐标前先经 `goproxy.cn` 的 `@latest` 实测（本机 proxy.golang.org 不可达）；写入后由 `go.sum` 锁定；升级依赖 = 先跑全量测试再提交 |
| **"不发明概念"纪律** | 交付物只到包级 / 类型级 + 关键方法签名；技术方案之外的新类型、新配置键、新表、新端点 → 先停下问用户 |
| **控制台形态验证** | 与 Java 的"测试 JVM 无控制台"同族的坑在 Go 同样存在（surefire 等价物跑在管道下）——真控制台行为**必须跨进程实测**，不得以"决策函数被测过"冒充"分支测过" |

---

## 3. 准备阶段（一次性）

### 3.1 项目宪法（十条原则）

升格为仓库宪法（`.specify/memory/constitution.md` 或等价物），每条**不得/必须**句式 + 违反症状 + 复核方式；修改宪法必须经用户确认，AI **不得**自行修订。

| # | 原则 | 违反时的症状 | 复核方式（可验证） |
| --- | --- | --- | --- |
| 1 | **自实现 ReAct Loop**：核心循环手写；**不得**引入任何 Agent 框架（eino / langchaingo / adk-go / trpc-agent-go） | 框架接管循环，工具被调两次或行为不可控 | `go.mod` 不出现框架坐标；`ReActLoop.Run` 为仓库内代码 |
| 2 | **LLM 客户端只做协议调用**：**不得**启用其任何自动工具循环 | 工具被框架与执行器各调一次 | 调用面只有 Chat 请求/响应结构；装配处无"agent/runner"类 API |
| 3 | **Provider 必须显式映射**：`name → 客户端` 装配时一次构建；**不得**遍历推断、**不得**"取第一个" | 多 Provider 时路由错乱（Go map 遍历顺序**随机**，行为不可复现） | 装配处无 `range` 选择逻辑；路由只查 map |
| 4 | **一个目录 = 一个 Agent**：`AGENT.md` frontmatter 即运行配置；技能**声明式绑定**（`skills:` 声明即绑定，**不得**引入软连接/本地目录类绑定）；渐进披露 L1/L2/L3 | Agent 目录被当 Tool 注册；技能卡片不出现而其它一切正常 | 加载器有 `AGENT.md` 专项测试；`skills:` 缺实体 → 该 Agent 不注册 |
| 5 | **审计 day-one 且 fail-closed**：审计表在 US-1 随 schema 建齐；`runs` 自 US-4（首次运行）起、`llm_calls` 自 US-2（首次 LLM 调用）起、`tool_invocations` 自 US-4（首次工具执行）起写入；工具执行**先插 `pending`、插入失败即拒绝执行** | 扩展阶段审计无从落地；或"跑都跑了却没记录" | 任一 Demo 跑完查 SQL 有记录；删审计写权限后工具调用应被拒而非照跑 |
| 6 | **授权与执行分离（两接口）**：`Policy.Check(ctx, action) error`（允许吗）+ `CommandRunner.Run(ctx, cmd) (CommandResult, error)`（怎么执行）；核心阶段唯一组合 = `WhitelistPolicy` + `LocalRunner`；**不得**让沙箱细节泄漏进调用方 | 升级（容器 / microVM）要改多处；授权与执行职责糊在一起，无法单独替换 | 调用方不得直接依赖 `WhitelistPolicy` / `LocalRunner`（只在装配处与实现文件出现）；装配处显式选择实现；容器档 = 两接口各加实现 + 装配切换 |
| 7 | **同步执行 + context 只承载取消 / 截止 / 追踪**：全程同步阻塞；Profile / Session / 工具执行环境一律走**显式类型化参数**（`RunRequest` / `RunEnv`），**不得**用 `context.WithValue` 传递业务状态，**不得**用包级变量模拟 ThreadLocal；**不得**引入异步编排 | 隐式耦合、取消失效、并发串号（Go 无 ThreadLocal，塞进 context 等于把隐式依赖合法化） | 逐处审查业务代码中的 `context.WithValue`——只允许追踪标识；业务状态一律显式类型化参数传递；grep 无包级可变状态存 Profile/Session；请求路径无自建 worker pool |
| 8 | **单二进制纪律**：发布产物 `CGO_ENABLED=0`，运行时代码和管理台静态资源随单 Go 二进制交付；核心底座无需外部数据库 / 缓存 / 消息队列（模型服务与显式配置的 MCP Server 属按配置接入的业务能力）；竞态测试与发布构建分开执行；依赖最小化（新增第三方坐标先问用户） | 交叉编译失败 / 目标机缺动态库 | `CGO_ENABLED=0 go build` 通过；发布产物按各目标平台分别验证——Linux 用 `ldd`/`file` 断言静态，macOS / Windows 验证无额外依赖即可启动；`-race` 只在测试命令 / job 中启用 |
| 9 | **不用 ORM，手写 SQL**：schema 幂等脚本 + 显式 SQL + repository 接口（定义在 `core`，实现在 `storage`）；**不得**引入 ORM，**不得**由 ORM 自动推断或修改数据库结构（迁移按 `schema_version` 版本化追加，已发布迁移不得改写） | SQLite 迁移能力弱，ORM 自动迁移反复出坑；SQL 不可审计、不可优化 | `go.mod` 无 ORM 坐标；建表与迁移只在显式 SQL 脚本与 `schema_version` 记录里（复核覆盖升级迁移脚本） |
| 10 | **五大能力优先，范围卡紧** | 功能蔓延，内核迟迟不完整 | 每 US 范围对照需求文档 §6 边界 |

> 十条的事实源是[技术方案 §1.1](TechnicalSolution.md)——本表与它**必须**逐条一致，出现分歧以技术方案为准并当场修正本表。宪法正式文本落地（`.specify/memory/constitution.md` 或等价物）**必须**经用户确认，AI **不得**自行修订。每个 US 的验收 Demo 与门禁全绿（`make check`）**不**列为原则，是开发工作流的交付口径。

### 3.2 仓库骨架与门禁

**骨架**（§技术方案 10.1 的包结构一次建齐，空目录放 `doc.go`）：

```bash
go mod init github.com/<org>/basalt   # 模块路径以实际仓库为准
mkdir -p cmd/basalt internal/{app,core,provider,memory,tool,channel,web,storage,cli,workspace}
```

**门禁（`make check` 一条命令跑全）**：

```bash
gofmt -l .                               # 格式：不合规文件打印到 stdout 且退出码 0——须显式判空；语法错误则退出码非 0——须先查退出码（见下）
gofumpt -l .                             # 更严格式：同 gofmt，两条都要跑
go vet ./...                             # 标准静态检查
staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...  # 判定集显式指定（EngineeringStandards.md §1.1）——不得裸跑
gosec -quiet ./...                       # 安全扫描
govulncheck ./...                        # 依赖漏洞扫描
go test -race -cover ./...               # 测试 + 竞态 + 覆盖率——⚠ 需 CGO + C 编译器
go test -tags=integration ./...          # 集成测试——⚠ build tag 隔离的测试默认不跑，不写这行等于没有
CGO_ENABLED=0 go build ./...             # 单二进制构建——⚠ 与 -race 要求的 CGO 状态相反
```

**三条实测纪律**（否则门禁会"报绿而实际没检"）：

1. **`gofmt -l` / `gofumpt -l` 发现格式差异时不会自动失败**——它们只把不合规文件**打印到 stdout**、退出码仍是 0。写成 `gofmt -l . && gofumpt -l .` 的串联在"有文件未格式化"时照样通过；**必须**把输出捕获后显式判空、非空即 `exit 1`。⚠ **另一半恰好相反**：**语法错误**时 `gofmt` 的退出码非 0（报错打 stderr、stdout 为空）——只判输出空会把"文件根本解析不了"判成通过。故判卷顺序**必须**是**先查退出码、再判输出**（见 Makefile 的 `fmt` 目标）
2. **`go test -race` 需要 CGO 与 C 编译器**（`-race` 走 cgo 运行时），与 `CGO_ENABLED=0` 的发布构建**要求的 CGO 状态相反**——两者**必须**分属不同 job / 不同命令，不得混在一个环境里
3. **build tag 隔离的集成测试默认不执行**——`go test ./...` 不会碰它们，**必须**有独立的 `go test -tags=integration ./...` 步骤（CI 里单列一步）

**Makefile 建议**（目标名固定，AI 与 CI 共用；配方一律用 `目标: ; 命令` 单行式，故此处无 tab——多行配方才需要 tab 缩进）：

```makefile
.PHONY: check fmt lint sec test test-integration cover build
fmt:  ; out="$$(gofmt -l .)" || { echo "gofmt 异常退出（退出码 $$?，多为语法错误）"; exit 1; }; test -z "$$out" || { echo "未格式化（gofmt）："; echo "$$out"; exit 1; }; out="$$(gofumpt -l .)" || { echo "gofumpt 异常退出（退出码 $$?）"; exit 1; }; test -z "$$out" || { echo "未格式化（gofumpt）："; echo "$$out"; exit 1; }
test:  ; go test -race -cover ./...
test-integration: ; go test -tags=integration ./...
cover:  ; scripts/check-coverage.sh        # 按包阈值判失败；脚本由 US-1 交付（读数来源 go test -cover ./...）
lint:  ; go vet ./... && staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...
sec:  ; gosec -quiet ./... && govulncheck ./...
build:  ; CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/basalt ./cmd/basalt
check:  fmt lint sec test test-integration cover build
```

> `fmt` 目标的判卷顺序是**先查退出码、再判输出**（两个格式工具各查一遍）：`out="$$(工具)" || {...}` 捕获的是命令替换里工具的退出码——语法错误（非 0）与格式差异（0 但 stdout 非空）是两种失败，都要拦。

**CI（GitHub Actions）**：**两个 job，各自固定 CGO 状态**

| job | 内容 | 关键点 |
| --- | --- | --- |
| `test` | `make fmt lint sec test test-integration cover` | 默认开启 CGO（`-race` 必需）；Linux runner 自带 gcc；`cover` 按包阈值阻断——低于底线 CI 即红 |
| `build` | `CGO_ENABLED=0` 四平台交叉编译 | 验证单二进制承诺；**不得**在此 job 跑 `-race`（CGO 状态冲突） |

覆盖率报告入库或上传工件。两个 job 都**必须**经 `actions/setup-go` 把工具链钉到 `go-version: 1.27.2`（与 `go.mod` 的 `toolchain` 行一致）——**准确构建版本**由 `toolchain` 行与 CI 两处表达，`go.mod` 的 `go` 行只是**最低版本要求**（Go 1.21 起语义）。**"安全扫描拖慢默认构建"的问题在 Go 不存在**——gosec / govulncheck 都是秒级，全部进默认门禁。

**覆盖率底线（必须有可执行判据）**：core / tool（沙箱）/ memory / storage 四包 ≥ 70%，其余 ≥ 50%（可调，先立底线再逐步提高）。`go test -cover` 只打印百分比、**不会**因低于阈值而失败——故上表有独立的 `cover` 目标（由 US-1 交付的检查脚本承载），按包解析结果并 `exit 1`，且**必须**在 `make check` 里。判定口径：**有可执行语句但无测试文件的包**（`no test files`）计 0%，未达标即失败，**不得**豁免；**无可执行语句的包**（`coverage: [no statements]`）跳过（无从测量，不计通过也不计失败）。安全关键路径（沙箱三档、凭证红线、审计落库）必须有专项测试，不靠覆盖率兜底。

### 3.3 写第一行代码前的人工确认清单

- [x] 需求文档 §12.2 未决事项**已全部裁决完毕**（U-1~U-10，2026-10-15）
- [ ] **运行时契约四件套已冻结**：Run 契约（`RunRequest` / `RunResult` / 状态机）、完整消息模型（含 `reasoning_content` 回传）、配置快照语义、`Policy` / `CommandRunner` 边界（事实源：[contracts/run-and-errors.md](contracts/run-and-errors.md) + 技术方案 §4）。**冻结条件见各文件头部**——在 §3.4 的**对应链路**验证通过之前，四件契约的状态是**待验证设计草案**，不得当成已冻结使用（分阶段安排见 §3.4）
- [ ] **原型五链路验证通过**（**分阶段执行**，见 §3.4——US-1 探针 + US-4 / US-9 分批；不阻塞 US-2 开工）——结果回写 `contracts/`
- [ ] 宪法十条经用户确认（特别是第 1 条"不引框架"、第 6 条"两接口"与第 8 条"单二进制"）
- [ ] `go.mod` 坐标经 goproxy.cn 实测，`go.sum` 已锁定（`go` 行写最低要求 `go 1.27`；准确构建版本由 `toolchain` 行与 CI 固定）
- [ ] 门禁命令在本机全部跑通（哪怕只有空包；含 `-race` 与 `-tags=integration` 两次调用）
- [ ] 工作区目录约定（`.basalt/`）与基线对齐确认（对照 [Compatibility.md](Compatibility.md) §2.1）
- [ ] cron 公共子集对拍样例与"样例 `AGENT.md` 双底座加载"两项验证跑过一次（登记为原型期执行，见 [Compatibility.md](Compatibility.md) §5）
- [x] 开工前待落地工件就位（2026-10-15 落地）：`CLAUDE.md`（项目根）+ 四个自制 skill（`.claude/skills/`：`module-doc-gen` / `us-register` / `module-dev` / `init-foundation`）+ speckit 层（`.specify/` + 10 个 `speckit-*` skill）

### 3.4 原型五链路验证（分阶段执行）

五条链路决定底座可不可靠，比二进制体积与空载启动速度更早暴露问题、也更难事后补救。验收口径见[需求文档 §13.5](DemandAnalysis.md)；通过后把实测结论（尤其是任何偏离契约之处）回写 `contracts/`。

**执行时机已决（2026-10-15，口径"丙"，裁决记录全文见[起步手册 §4](GettingStarted.md)）**：**不**要求"写第一行业务代码之前全部跑通"——链路 5（崩溃恢复 + 实例锁）由 US-1 的**小探针**先验（不入库、内存 SQLite）；链路 1 / 3 / 4 在 **US-4** 交付后立即验证（含链路 5 的真实实现复验）；链路 2 的 HTTP 准入面（409 / 429）到 **US-9** 补齐。四件契约在对应链路验证通过前保持**待验证设计草案**状态（状态头见 `contracts/` 各文件），**不构成** US-2 / US-3 的开工阻断。五链路**全部通过**即为契约的最终冻结条件（另有真实模型调用往返，见各契约文件头部）。

| # | 链路 | 最小验证方式 |
| --- | --- | --- |
| 1 | 多轮工具调用 | 一个假 provider + 两个工具，跑三轮带工具调用的对话；含一轮并列两个工具调用（验证 `tool_call_id` 回填）；再串一次思考模式真实端点验证 `reasoning_content` 回传 |
| 2 | 同会话并发 | 同一 session 并发两个 `Run`：后者回 409；跨会话并发互不阻塞；压到实例上限回 429 |
| 3 | 执行取消 | 起一个长跑子进程（`sleep` / 长脚本），取消 ctx 后确认进程树被杀、Run 落 `cancelled`、审计有终态；未完成工具组补齐为「已取消」；**取消后在同一会话再发一条消息，能正常跑完**（会话可继续——半截工具组不得让会话哑掉） |
| 4 | 运行中热加载 | 运行进行中改写 `AGENT.md`：本次运行以旧快照跑完，下一次运行取到新正文（断言不出现混搭） |
| 5 | 崩溃后恢复 | 运行进行中 `kill -9`：重启后 `running` → `failed`、`llm_calls` 与 `tool_invocations` 的 `pending` 行都 → `unknown`，无遗留 `running` 记录；未完成工具组以**消息**为入口扫出、与 Run 状态**同事务**补齐（终态行还原真实结果 / `pending` 或 `unknown` 或缺失补「结果未知」「未执行」）；**重启后同一会话再发一条消息能跑完**；**恢复执行到一半再 `kill -9`，下次启动仍能补齐同一批组**（**工具组**的入口是消息、不看 Run 状态，所以恢复可重入；**没有半截组可补**的 `running` Run 由补组后的扫表兜底收尾——**组看消息、Run 看表**）；活实例持锁时第二个写模式进程**拒绝启动**、只读命令不触发恢复；实例锁为**操作系统级独占文件锁**——被 `SIGSTOP` 暂停的实例心跳会停，但**不得**因此被抢锁（实例锁语义见 [contracts/run-and-errors.md](contracts/run-and-errors.md) §4.3） |

---

## 4. 逐 US 拆解

每个 US 给六件事：**核心目标 / 涉及包 / task 大类 / 颗粒度 / 硬依赖 / 验收 Demo**。开工前按 §1.2 生成该 US 的颗粒度文档，完工按 §5 交付。粒度刻意不均匀——够独立演示的拆细，不够独立演示的不硬拆。

### 4.1 US-1 工程地基（骨架 + 门禁 + 配置 + 日志 + 工作区 + 持久化）

- **核心目标**：从干净检出到 `make check` 全绿、`basalt init` 产出 `.basalt/` 工作区、SQLite 库与全部表就位（幂等 schema）
- **涉及包**：根（`go.mod` / `Makefile` / CI）、`app`（装配骨架）、`cli`（init / status）、`workspace`（目录约定）、`storage`（SQLite 打开 + schema.sql 幂等 + 仓储接口与实现）
- **task 大类**：`go.mod` + `internal/` 十包骨架（**起点布局，包数不冻结**——纪律是依赖方向 / 接口定义在消费方 / 第三方实现就近封装，见 §3.2 与技术方案 §10）→ Makefile 门禁（gofmt / gofumpt / vet / staticcheck / gosec / govulncheck / test -race / `test -tags=integration` / `CGO_ENABLED=0` build）+ CI 双 job（test / build，CGO 状态各自固定）→ `basalt.yaml` 加载与 `${ENV_VAR}` 占位校验（未解析残留即拒、不静默取默认）→ slog 双档日志（dev 文本 / prod JSON）→ `basalt init` 工作区幂等生成（agents / skills / memory / logs / archive / AGENTS.md / SOUL.md / USER.md / mcp_servers.yaml）→ SQLite（modernc、WAL；读写池分离、写池 1）→ `schema.sql` 幂等建齐**核心八张 + 支撑三张**（sessions / messages / runs / config_snapshots / llm_calls / tool_invocations / scheduled_tasks / task_executions + notify_channels / memory_entries / schema_version）→ 仓储接口（core 定义 / storage 实现）→ **时区数据库内嵌**（`import _ "time/tzdata"`——裸镜像里 IANA 时区名才解析得出来，否则带 `zone: Asia/Shanghai` 的 Agent 到了容器里整只不注册；见 [Operations.md](Operations.md) §3）→ 契约 YAML 解析单测（用项目已有的 YAML 依赖解析 `docs/contracts/*.yaml`——别名指示符类隐性错误当场红，见 §7 陷阱 32）→ **测试基础设施**（临时工作区 / 本地 HTTP 测试服务——假 Provider 随 US-2、MCP 测试服务随 US-7 以同口径补，见 §8.3 第 2 项）→ `basalt status`
- **颗粒度**：最大的管线类 US（多为样板代码）；约 25~30 个文件
- **硬依赖**：无
- **验收 Demo**：干净检出 `make check` 全绿；`basalt init` 出工作区、二跑幂等（已有文件不覆盖）；`basalt status` 显示库与表就绪；`CGO_ENABLED=0 go build` 出静态二进制；`TZ=` 清空 + 无系统 zoneinfo 的环境里 `time.LoadLocation("Asia/Shanghai")` 仍成功（tzdata 内嵌生效）；测试基础设施跑通首个不依赖真实网络的用例（本地 HTTP 测试服务）

### 4.2 US-2 Provider 对接（能力一）

- **核心目标**：一次真实 LLM 调用打通——配置 → 显式映射 → 调用 → `llm_calls` 落库（成败都写）；`basalt provider list` 可用
- **涉及包**：`core`（ProviderClient 接口 + 审计接口）、`provider`（OpenAI 兼容实现）、`storage`（llm_calls 仓储）、`app`（装配）、`cli`（provider list）
- **task 大类**：`providers` 段配置解析 → 客户端构造（base_url / key / 超时）→ 显式映射装配（启动时一次构建为 map，不做遍历推断）→ 调用路径 + usage 解析（可空兜底）→ 审计落库（成败都写）→ `provider list`（含配置状态）→ 错误语义（未注册 provider 点名报错）
- **颗粒度**：接口 + 一个实现 + 一个命令；约 8~10 个文件
- **硬依赖**：US-1
- **验收 Demo**：配置两家 provider，真实调用成功一次；`SELECT provider, model, total_tokens FROM llm_calls` 可见记录；断网调用失败也落审计

### 4.3 US-3 Agent 目录与 Profile 加载

- **核心目标**：`.basalt/agents/<name>/AGENT.md` 契约落地——frontmatter 加载期校验、system prompt 第一段（正文 + Bootstrap + 技能卡片 L1）、`profile` 四个子命令
- **涉及包**：`workspace`（AgentLoader / ContextLoader）、`core`（Profile）、`cli`（profile 组：`list` / `show` 为只读命令；`create` / `delete` 会写工作区，属**写模式命令**——取实例锁，锁被活实例持有时**拒绝启动**并指引改用服务 API / 管理台，见 [contracts/run-and-errors.md](contracts/run-and-errors.md) §4.3.1）
- **task 大类**：frontmatter 解析（`schedules` 6 段 cron / zone 缺省 UTC / `skills:` 声明）→ 加载期校验（`name` 等于目录名 / provider 已注册 / `model` 非空白（占位符放行）/ bootstrap 文件存在 / `mcp_servers` 在配置内 / 技能在库；任一失败 → 该 Agent **整只不注册** + 点名 WARN）→ Profile 派生 → ContextLoader 段一（AGENT.md 正文 + Bootstrap 三件 + 技能卡片 L1（name + description + 库内真路径）+ 当前日期时间）→ `profile create / list / show / delete`
- **颗粒度**：加载器 + 段落拼装 + 四命令；约 12~15 个文件
- **硬依赖**：US-2（provider 注册判定）
- **验收 Demo**：`profile create` 出模板 → `list / show` 可见；故意写坏配置（未注册 provider / 库里没有的技能 / 5 段 cron / 空白 model）→ 整只不注册 + WARN 点名；技能声明即绑定（卡片 L1 出现在 system prompt）

> 技能渐进披露的 L2/L3（模型经 `read_file` 取 SKILL.md 与附属资源）依赖文件工具与技能库白名单——在 US-4 走通；"工具名必须在注册池"的加载期门禁也随 US-4 的 Registry 落位接入。

### 4.4 US-4 ReAct 循环与首个工具闭环（能力二）

- **核心目标**：第一条真正的 agent 竖切——四段提示词 → 循环 → 工具执行（沙箱正式实现，非桩）→ 结果回填 → 多轮会话落库；`basalt chat` 可用
- **涉及包**：`core`（ReActLoop / PromptBuilder / ToolExecutor / AgentService / `Policy` 与 `CommandRunner` 接口）、`tool`（Registry / WhitelistPolicy / LocalRunner / FileTools）、`channel`（CliChannel）、`cli`（chat / session list）、`storage`（sessions / messages / runs 读写）
- **task 大类**：PromptBuilder 四段拼接（段 4 工具列表来自 Registry）→ Tool 接口 + Registry 显式注册 → **`WhitelistPolicy`（授权）+ `LocalRunner`（执行）**——文件真实路径投影、可执行名整串比对、域名 + scheme 固定、子进程环境变量白名单、超时杀进程树——与四段式拒绝信息（模型可见契约）→ ToolExecutor（**先 `Policy.Check` 再 `CommandRunner.Run`**）→ **完整消息模型**（`tool_calls` / `tool_call_id` / `reasoning_content` 原样回传；裁剪按完整工具交互组）→ ReActLoop（工具顺序执行、失败信息回填、`max_iterations` → `limit_reached`）→ **`AgentService.Run(ctx, RunRequest) (RunResult, error)`**（生成 `run_id`、每次运行重建配置快照并运行内冻结、同会话准入检查、取消与超时）→ FileTools 三件（read_file / write_file / list_dir，写范围 = 当前 Agent 自己的目录）→ **审计两阶段写**（`tool_invocations` 先插 `pending`，插入失败即拒绝执行；`llm_calls` 带 `run_id` + `round`）→ CliChannel（真控制台 vs 管道分流的装配，平台差异走 build tag）→ `chat`（多轮 + `--message` 单条）→ 会话落 SQLite 并跨重启恢复 + `session list` → 加载期"工具名在注册池"门禁接入 AgentLoader → **行为评测基座**（`evals/` 首批固定任务集 + 运行脚本：工具选择 / 实际结果 / 越权尝试 / 调用次数 / 成本读数，业务结果判定——见 §8.3 第 3 项）→ **注入类安全用例**（网页 / 文件 / 工具返回中的恶意指令不改变执行层权限——见 §8.3 第 4 项）
- **颗粒度**：全项目**唯一保留的大 US**——再拆会出现"有循环没工具"或"有工具没人调"的半截状态；约 25~30 个文件
- **硬依赖**：US-3
- **验收 Demo**：`basalt chat` 多轮对话（AGENT.md 身份生效）；模型调 `read_file` 读工作区文件并用于回答；越界路径被四段式拒绝且审计落 `failed` + `error_kind=sandbox_denied`；思考模式模型连续多轮带工具调用**不报 400**（`reasoning_content` 回传）；重启进程后会话历史仍在并可继续；行为评测首批任务集跑通（五读数入库）；注入用例的越权尝试被四段式拒绝且审计落 `failed`

### 4.5 US-5 Shell 与 HTTP 工具（Demo 一）

- **核心目标**：内置十件补齐到六件（File×3 + Shell + Http×2）；三档授权全部有真实消费方
- **涉及包**：`tool`（ShellTools / HttpTools）、`cli`（tool list）
- **task 大类**：ShellTools（`executable` + `arguments` argv 直执、不经任何 shell、可执行名整串逐字白名单、超时 / 中断两条路径杀**进程树**、有 Agent 上下文时工作目录 = 该 Agent 目录、**子进程只透传白名单环境变量**）→ HttpTools（scheme 固定 http/https、域名白名单大小写不敏感、超时）→ `tool list`
- **颗粒度**：两个工具 + 一个命令；约 8~10 个文件
- **硬依赖**：US-4
- **验收 Demo**：**Demo 一**——`basalt chat` 问天气，模型调 `http_get` 取数据后给出穿衣建议；沙箱拒绝样例（`bash -c "…"` 串接被整串白名单拒绝）

### 4.6 US-6 Memory（能力三，Demo 二）

- **核心目标**：长期记忆两层作用域（Agent 层 / 全局回退层，不叠加）+ 三档载体接口（本 US 交付 markdown 与 sqlite 两档，mem0 按 U-8 裁决）+ save / recall 两个记忆工具
- **涉及包**：`memory`、`storage`（memory_entries）、`core`（记忆接口）、`workspace`（memory 路径约定）
- **task 大类**：MemoryService 门面 → 两层作用域解析（无 Agent 上下文 = 全局层，`null` 语义不得用空串顶替；Agent 层有内容不叠加全局层）→ markdown 载体（核心 / 归档分区、运行级快照语义（每次运行重建、运行内冻结）、核心区**有界**（单条 ≤ 4KB、每 Agent ≤ 32KB，超限可读拒绝）、裁剪只作用归档区）→ sqlite 载体（同语义断言，换后端只改一行配置）→ save_memory / recall_memory 工具（单关键词契约、分区与 scope 入参）→ 断言 `memory/` 不在文件工具白名单（记忆只能由记忆工具写）
- **颗粒度**：接口 + 两载体 + 两工具；约 12~15 个文件
- **硬依赖**：US-4
- **验收 Demo**：**Demo 二**——会话 A 告诉它偏好，进程重启，会话 B 仍按偏好输出；`MEMORY.md` 两分区可见条目

### 4.7 US-7 MCP 集成（Demo 三）

- **核心目标**：MCP client（stdio）+ 工具适配进 Registry + 连接失败不影响启动；零代码扩展路径走通
- **涉及包**：`tool`（MCP Client / 工具适配）、`app`（装配）、`workspace`（`mcp_servers.yaml` 解析）
- **task 大类**：`mcp_servers.yaml` 解析（env 值只允许 `${VAR}` 占位）→ 官方 go-sdk stdio client（初始化 / 列工具 / 调用）→ 工具适配进 Registry（ReAct 循环不感知工具来源）→ 连接失败不中断启动（WARN + 跳过该 server）→ 依赖它的 Agent 因"工具解析不到"整只不注册（沿用 US-4 门禁）
- **颗粒度**：一个子系统；约 10~12 个文件
- **硬依赖**：US-4
- **验收 Demo**：**Demo 三**——Agent 目录 + 现成 MCP server 完成 PR digest；故意写错 `command`：启动不中断 + WARN + 该 Agent 不注册

### 4.8 US-8 通知渠道

- **核心目标**：`notify` 工具 + 渠道注册表 + 两适配器（webhook / feishu）+ 凭证红线
- **涉及包**：`tool`（NotifyTools / 适配器）、`storage`（notify_channels）
- **task 大类**：`notify_channels` 表与仓储 → 类型 → 适配器装配层显式映射（类型支持集只有**一份真值**）→ WebhookNotifyAdapter（通用 `{"content": …}`；发送前同样过域名白名单）→ FeishuNotifyAdapter（`{"msg_type":"text",…}`；**只认响应体 `code == 0`**，回执解析不出判失败）→ 凭证红线（响应 / 日志 / 错误文案只出 scheme + host；沙箱越界文案不得透传）
- **颗粒度**：一个工具 + 两适配器 + 一张表；约 8~10 个文件
- **硬依赖**：US-4
- **验收 Demo**：真 webhook 发一条（人工执行，凭证人手输入）；飞书故意用错关键词 → HTTP 200 但判定失败（`code=19024`）；渠道 CRUD 在 US-12（管理台批）

### 4.9 US-9 Web Service（能力五，Demo 四 / 五）

- **核心目标**：REST 10 端点 + 统一信封 + `serve` / `gateway` 命令；外部系统可完整调用
- **涉及包**：`web`（server / 中间件 / handlers）、`cli`（serve / gateway）
- **task 大类**：ServeMux 路由 + 中间件链（recover / 日志 / CORS）→ 信封与错误码翻译（含 `SESSION_BUSY` 409 / `TOO_MANY_RUNS` 429 / `LIMIT_REACHED` 200——**达上限是正常终止不是故障**）→ 10 个端点（口径以 [contracts/openapi.yaml](contracts/openapi.yaml) 为准）→ **并发准入**（同会话串行、实例级上限、**不排队**）→ 会话历史返回上限与消息大小限制 → `http.Server` 四超时 + **默认监听 `127.0.0.1:8080`** + 优雅关闭 → `serve` / `gateway` 命令
- **颗粒度**：约 15~18 个文件
- **硬依赖**：US-4
- **验收 Demo**：**Demo 四 + 五**——curl 全链路（建会话 → 发消息 → 查历史 → 归档）；`POST /agents/{name}/invoke` 无状态调用；同一会话并发第二请求回 409（可读文案）；`ss -tlnp` 只见 `127.0.0.1:8080`

### 4.10 US-10 定时任务

- **核心目标**：钟推第三触发源——按 cron 动态注册、到点生成消息走同一 `AgentService`；四个管理端点；补第 10 个内置工具 `manage_schedule`
- **涉及包**：`core`（Scheduler）、`storage`（scheduled_tasks / task_executions）、`web`（schedules 端点）、`tool` + `workspace`（`manage_schedule` 工具与写入门）
- **task 大类**：robfig/cron/v3 **6 段 + zone**——**结论已实测写死**（v3.0.1 源码核实）：六段用 `cron.NewParser(cron.Second｜cron.Minute｜cron.Hour｜cron.Dom｜cron.Month｜cron.Dow)`；**该版本没有 `parser.WithLocation`**，每任务时区经 `CRON_TZ=<zone>` 前缀（装载时由 `zone` 字段生成，缺省 UTC）；`L` / `W` / `#` 不支持、星期 0–6（`7` 无效）→ 与 NivroOS 的**互换范围限定公共子集**，承诺互换前先对拍未来十次触发时间（[Compatibility.md](Compatibility.md) §5）→ 启动扫描各 Profile 的 `schedules` 动态注册 → 每任务一把锁（上一次没跑完则跳过，**跳过不产生 409**、不排队）→ 失败只记日志不崩调度器、审计照走 → 两表状态与历史（**下线 = 置 `retired_at` 标记、不物理删**（`task_executions` 的外键因此永不撞）；**重启对账：`enabled` 以库为准、定义以文件为准**；**停机期间错过的触发点不补跑**——记 WARN，补跑走 `/schedules/{id}/run`）→ **启动恢复收尾遗留 `started` 执行行**——按 Run 终态映射：`completed` / `limit_reached` → `completed`，`failed` / `cancelled` → `failed` → 四端点（list / executions / run / enable-disable）→ **`manage_schedule` 工具**（`set` / `remove` / `list`，**只能改当前 Agent 自己的 `schedules:` 段**、身份取自运行时上下文而非入参；经与页面编辑器同一道注册写入门）→ session_id 沿用既有公式（channel 与 user 固定为 scheduler）
- **颗粒度**：约 12~15 个文件
- **硬依赖**：US-4（调度本体）+ US-9（管理端点）
- **验收 Demo**：到点触发真跑一次（含手动补跑）；`scheduled_tasks` / `task_executions` 可见状态与历史；Agent 用 `manage_schedule` 改自己的任务成功、改别人的被拒

### 4.11 US-11 观测与管理台

- **核心目标**：观测端点 + 配置热加载 + 管理台（前端 = Vue + Vite 产物 `go:embed`，U-9 已定）
- **涉及包**：`web`（含 embed 静态资源）、`workspace`（watcher）、`memory`（按层查询）、根（`assets/`）
- **task 大类**：只读观测端点（技能 / MCP / 通知渠道（**只回 scheme + host**）/ 沙箱白名单 / 会话列表 / 记忆分层）→ **热加载 = 校验通过的不可变配置快照 + 原子发布 + 失败保旧**（`config_snapshots` 表 + `config_hash` 进 `runs` / `llm_calls`）→ fsnotify **非递归**（逐目录 watch、新目录补 watch、编辑器原子替换的 rename 事件、漏事件兜底 rescan；`mcp_servers.yaml` 与白名单改动**需重启**）→ 归档 = 注销定时 → 移出注册 → **取消该 Agent 正在跑的 Run 并等待退出**（有界等待；超时报错并恢复注册，不留半残态）→ 目录移入 `archive/` → 管理台页面（模型产出文本走自研安全子集渲染器；无外部资源 / 无路由库 / 无 Markdown 库）→ 页面写动作白名单与 CI 断言
- **颗粒度**：约 20~25 个文件（含前端工程）
- **硬依赖**：US-9 + US-10（定时页）
- **验收 Demo**：浏览器里看清一个 Agent 全貌；改 `AGENT.md` 保存 → **下一次运行**生效（正在跑的运行以旧快照跑完，不出现"旧 Profile × 新正文"混搭）

### 4.12 US-12 Agent 动态管理

- **核心目标**：页面三分钟上新一个 Agent；归档不删数据；工作区只读浏览；通知渠道 CRUD 落位
- **涉及包**：`web`、`workspace`（生命周期：创建 / 生成 / 提交 / 归档 / 浏览）、`storage`（notify CRUD）
- **task 大类**：草稿生成（复用 LLM 路径 + `agent-author-<name>` 审计标识 + 作者模型未配置 503 且零调用）→ 脚手架创建（先验后写 + 回滚 + 未选模型写可见占位符）→ 多文件提交（**staging 构建 + 两次 `rename` 目录交换**：先在 `.staging` 构建完整副本、校验通过后原目录 → `.backup`、staging → 正式目录、删 `.backup`；崩溃窗口有界，启动按 `.backup` 残留判定回滚 / 前滚）→ 归档删除（注销定时 → 移出注册 → **取消该 Agent 正在跑的 Run 并等待退出**（有界；超时报错并恢复注册）→ 移入 `archive/`；会话 / 记忆 / 审计不动）→ 工作区浏览（真实路径投影 + 256KB 上限 + 只读）→ 通知渠道 CRUD（**`url` 列存 `${ENV_VAR}` 引用、发送时解析**；删除前扫 `AGENT.md` 整词匹配引用，命中 400 点名，`?force=true` 强删；url 留空 = 保留原值；响应 / 日志 / 错误文案只出 scheme + host）
- **颗粒度**：约 20~25 个文件
- **硬依赖**：US-11 + US-8
- **验收 Demo**：页面上新 Agent 全流程（生成草稿 → 就地编辑 → 创建 → 立即生效）；删除被引用的通知渠道被拒并点名

### 4.13 US-13 官网展示主页（对外门面）

- **核心目标**：对外门面立起来——官网展示主页（讲清 Basalt 是什么 / 五大核心能力 / 架构 / 快速开始）+ README 快速开始（与站点同源）；与核心代码同期发布，作为 `v0.1.0` 对外亮相的一部分
- **涉及包**：仓库根（新增站点工程 `website/`、README、`.github/workflows/` 的 Pages 部署）
- **task 大类**：站点工程（VitePress；**仅构建期依赖**，不进单二进制）→ 首页（一句话定位 + 五大能力卡片）→ 架构页（ReAct 循环 / 四段提示词 / 沙箱三档 / 记忆分层，配图入库）→ 快速开始页（干净检出 → 编译 → `init` → `chat` 跑通）→ README 快速开始（与站点 quick-start 同源，不写第二份口径）→ Pages 部署工作流 → 内容纪律自查（每条能力对应已交付实现，**不得**写"即将支持"）
- **颗粒度**：约 10~15 个文件（站点工程 + 三页 + README + 部署工作流）
- **硬依赖**：US-12（站点展示的每条能力必须已经真实存在）
- **验收 Demo**：站点本地构建可访问，页面内容与已交付能力逐条对应；新克隆仓库按 README 起服务、跑通一次对话（**只需 Go 工具链**）

### 4.14 US-14 发布收尾（三个每日 Demo + v0.1.0）

- **核心目标**：三个每日 Demo 入素材库并真跑；干净检出发布走查；`v0.1.0` 锚点
- **涉及包**：素材库 `examples/`、e2e 测试目录与脚本（`e2e` 目标）、Makefile 与发布工作流、根（走查与 tag）
- **task 大类**：三份 Agent 目录（天气光杆 / 科技日报带 `skills:` 声明 / GitHub 日报带 `scripts/`）→ 素材钉住测试（可被加载器读回、cron 6 段合法、声明的工具名在池、技能与脚本文件存在）→ 免 key 数据源域名进白名单 → 三个 Demo 真跑（含真通知渠道，人工验收）→ 干净检出全量门禁 + **mock 模型集成测试**（假 OpenAI 兼容服务 + 发布候选二进制，**零真实凭证**、确定性判定；七场景——工具闭环 / 越权拒绝 / 取消与工具超时 / 崩溃恢复 / 同会话并发 409 / 运行中热加载 / Web·调度·通知，覆盖 §3.4 五链路 + 越权 + Web·调度·通知；独立 Make 目标 `e2e`（`//go:build e2e`），新构建步骤按「构建步骤追加」登记并注明**仅发布流程必跑**——见 §8.3 第 8 项与 §8.4）+ 发布走查（按 US-13 的 README / 站点口径）→ **数据与长期运行策略落实**（保留期 / 访问权限 / 磁盘告警与满盘行为 / 清理时消息组与关联记录完整——策略文本落 [Operations.md](Operations.md)，见 §8.3 第 7 项）→ **发布与平台验证**（版本号 + commit + 产物 sha256；锁 / 中文控制台 / 进程树取消在目标系统实跑；前端资源随工件；升级 / 恢复演练——见 §8.3 第 8 项）→ `v0.1.0`（打 tag 由用户手动）
- **颗粒度**：素材 + 文案 + 发布验证代码（e2e 场景 / 假模型服务 / 脚本）；约 15~20 个文件
- **硬依赖**：US-13（README / 站点就绪，走查按它做）+ US-5 / US-6 / US-7 / US-8 / US-10（Demo 会用到 HTTP / 记忆 / 技能 / 脚本 / 通知 / 定时）
- **验收 Demo**：三个每日 Demo 自动跑通（人工验收）；`v0.1.0` 锚点；升级 / 恢复演练一次通过（按 Operations §5）；mock 模型集成测试全绿（独立目标 `e2e`，七场景覆盖五链路 + 越权 + Web·调度·通知；绑定候选产物 sha256——发布闸门，口径见 §8.4）

### 4.15 协作模式（人与 AI 分工）

| 事项 | 人 | AI |
| --- | --- | --- |
| 宪法与未决事项裁决 | **决定** | 提供建议与影响分析 |
| 颗粒度文档 | 审核 | 生成 |
| 代码 + 测试 | 抽查 | 实现 |
| 已批准 US 内的实现细节（辅助函数 / 私有类型 / 包内测试） | 抽查 | 自主完成、不逐项请示（边界见 §8.3 第 1 项） |
| 门禁 | 看结果 | 修复（修实现，不改断言） |
| 真实凭证类冒烟 | **执行**（凭证人手输入） | 准备步骤与核对清单 |
| commit / push / tag | **决定时机** | 不自动执行 |

### 4.16 跑偏纠正表

| 跑偏症状 | 纠正 |
| --- | --- |
| 引入框架"省事" | 回到宪法 I / II 条；Agent 框架不得作为直接或间接依赖引入；协议 SDK（如 MCP SDK）可用，但不得接管 ReAct 循环或自动执行工具 |
| 出现包级可变状态 | grep 包级变量；改 context 显式传递 |
| 新增**对外契约 / 架构边界**未申报（新配置键 / 端点字段 / 错误码 / Run 状态 / 表结构 / 新包 / 依赖坐标） | 停下确认（进颗粒度文档或用户裁决）；**私有实现类型**（辅助函数、私有结构、包内测试）不属此列——已批准范围内自主落地（§8.3 第 1 项） |
| 测试变红改断言 | **禁止**；改实现；认为测试错 → 停下报告 |
| 门禁不过想加豁免 | 优先改实现；确需抑制 → 逐条说明理由并经用户确认 |
| "顺便重构"前序代码 | 越出本 US 范围即停；重构单独提案 |

---

## 5. 交付物（每个 US 的完成定义）

一个 US 算完成，七项证据齐：

1. **门禁全绿**：`make check` 通过（贴关键输出）
2. **测试映射**：颗粒度文档"验收测试"节列出的测试逐个存在且通过；关键回归测试（安全路径）有专项用例
3. **交付物核对**：颗粒度文档"交付物清单"逐项存在性核对（文件 / 表 / 端点 / 配置键）
4. **回归**：前序 US 的测试全部仍绿
5. **全局不变量自查**：宪法十条 + 安全约束逐条 grep 复核（§3.1 复核方式列）
6. **验收 Demo**：该 US 的 Demo 实跑通过（需要真实凭证的部分，由人执行、AI 给核对清单）
7. **变更导读**：给 reviewer 的总结——改动点（按包分组）/ 重点 review 清单（按风险排序）/ 如何验证（可复制的命令块）

---

## 6. 增量阶段提示词模板（可直接复制）

**新 US 开工模板**：

```text
按 docs/AiProgrammingGuide.md §4.{N} 实现 US-{N}：{名称}。

输入（只读所需章节，不要读全文）：
- docs/DemandAnalysis.md §5.{x}
- docs/TechnicalSolution.md §{y}
- 本 US 的颗粒度文档：{路径}

纪律：
- 遵守 docs/AiProgrammingGuide.md §3.1 宪法十条
- 第三方 API 先 go doc 核实再写（H3）；新依赖坐标先停下来问
- 测试与实现一起落地；不得删断言、不得放宽阈值
- 完工跑 make check 全绿 + 该 US 验收 Demo，按 §5 七项证据汇报
```

**日常修复模板**：

```text
{现象}。遵守宪法与陷阱表（docs/AiProgrammingGuide.md §7）。
先定位根因再改；改完跑相关测试 + make check；如实汇报（含没验证的部分）。
```

**发布收尾模板**：

```text
按 docs/TechnicalSolution.md §10.2 出四平台产物，记录版本号 + commit + 产物 sha256；
干净检出跑全量门禁（make check）+ mock 模型集成测试（make e2e，绑定候选产物
sha256、录读数——口径见 docs/AiProgrammingGuide.md §8.4；缺前置即失败、不得跳过、
产物变化重跑）；
跑三个每日 Demo 冒烟（人工项列清单）；对照 docs/DemandAnalysis.md §13 逐条验收；
不 commit / 不 push / 不打 tag（由用户决定）。
```

---

## 7. 家族陷阱表（Go 形态，蒸馏自 NivroOS 第一阶段实测）

> 用法：开工前扫一遍、收尾自查一遍；每条都带"怎么发现"与"怎么复核"。标注「**Go 特有**」的是语言形态引入的新坑，其余是从 Java 形态蒸馏的等价物。

| # | 陷阱 | 症状（怎么发现） | 修复与复核 |
| --- | --- | --- | --- |
| 1 | 引入 Agent 框架的循环 | 工具被调两次；行为不可控 | 宪法 1/2 条；复核：`go.mod` 无框架坐标 |
| 2 | **Go 特有**：靠 map 遍历"取第一个"推断 Provider | 路由随进程随机漂移、不可复现 | 显式映射；需要顺序处显式排序；复核：装配处无 `range` 选取逻辑 |
| 3 | 审计只写日志 | 事后无法回放"为什么那样答" | 两表 day-one 落库；复核：跑 Demo 查 SQL |
| 4 | 用 ORM / 自动迁移 | SQLite `ALTER TABLE` 弱 → 表结构漂移 | 手写 SQL + `schema.sql` 幂等 + `schema_version`；复核：迁移脚本入库可重放 |
| 5 | **Go 特有**：`exec.CommandContext` 超时只杀直接子进程 | 孙进程成孤儿继续跑（进程泄漏可复现） | 独立进程组（Unix `Setpgid` + 杀 `-pgid`；Windows 任务对象 / `taskkill /T /F`）；复核：测试跑"父→孙"进程树，超时后断言孙进程消失 |
| 6 | shell 走 `sh -c` 或参数拼进可执行名 | 命令注入：`git status; curl evil.com` 首 token 合法但串接执行 | argv 直执 + 可执行名整串白名单、不给 `bash`/`sh` 放行；复核：测试断言含空格参数原样传递 |
| 7 | 路径白名单只做词法规范化 | `..` / 软连接绕过 | `filepath.EvalSymlinks` 投影真实路径后比前缀、不可解析失败关闭；复核：软连接放行 / 拒绝两用例 |
| 8 | 技能绑定用软连接 / 本地目录 | Windows 上不可行；平台行为分叉 | `skills:` 声明式绑定、软连接形态 day-one 不识别；复核：加载测试断言"声明即绑定" |
| 9 | `AGENT.md` 不声明 `tools` | 空工具池——模型把 tool call 当**文本**吐出（不报错、看起来答了） | frontmatter 显式声明；复核：Demo 断言工具真被调用（审计表有行） |
| 10 | cron 写 5 段 / zone 取本机时区 | 解析拒 → Agent 整只不注册；"早上 8 点"随部署环境漂移 | 6 段（秒分时日月周）+ zone 缺省 UTC；`time.Local` 与 `time.Now().Location()` 是禁用词；复核：加载期专项测试 |
| 11 | `recall_memory` 传多词 query | 整串匹配必不命中（"SQLite 方案 评估" vs "SQLite"） | 单关键词契约；复核：测试钉住多词不命中行为 |
| 12 | 通知渠道地址回显进响应 / 日志 / 错误 | 凭证（url 即 token）进浏览器历史与日志文件，长期有效 | 对外只回 scheme + host；沙箱越界文案**不得**透传；复核：测试断言响应体不含完整 url |
| 13 | 飞书只看 HTTP 200 | 投递失败也 200 → 审计出现假成功 | 只认响应体 `code == 0`；回执解析不出同样判失败；复核：19024 样例测试 |
| 14 | **Go 特有**：Windows 控制台编码 | 真控制台中文乱码、管道正常（最难查的一类）；`os.Stdin` 是字节流不替你解码 | 按 `IsTerminal` 分流：控制台经 ConsoleW API 收发、管道显式 UTF-8；复核：真控制台跨进程冒烟（不得用测试进程冒充） |
| 15 | MCP / shell 配裸名 `python` | Windows 商店存根静默空转 → server 不回话，报错不指向根因 | `mcp_servers.yaml` 用绝对路径；复核：`-c "print(1)"` 实探候选解释器 |
| 16 | 自写 MCP server 未统一 UTF-8 | 中文乱码 / 解码异常，报"工具调用失败"看不出根因 | server 启动即 `stdin/stdout.reconfigure(encoding="utf-8")`、日志走 stderr；复核：中文样例协议往返测试 |
| 17 | **Go 特有**：goroutine 泄漏 / context 未传播 | 长跑内存涨、请求取消失效、关闭时挂起 | context 显式贯穿；watcher / 调度器有关闭路径与测试；复核：`go test -race` + 长跑冒烟看 goroutine 数 |
| 18 | `http.Server` 未设超时 | 慢速连接拖住（gosec G112 报警） | `ReadHeaderTimeout` 等四超时必设；复核：门禁 gosec 零告警 |
| 19 | 忘记 `CGO_ENABLED=0` | 二进制混入动态库依赖，目标机跑不起 / 交叉编译失败 | 构建命令与 CI 固定该变量；复核：`file` / `ldd` 断言静态 |
| 20 | 生成物类交付物无单测 | 命令行模板 / 脚手架产出"能看不能用"（如模板漏记忆工具，生成的 Agent 永远记不住事） | 凡会被交到用户手里的生成物，必须有"能被下游解析器读回 + 关键能力声明齐"的单测；复核：模板解析测试 |
| 21 | **Go 特有**：`gofmt -l` / `gofumpt -l` 发现格式差异**不会自动失败**（退出码仍 0）；反过来语法错误时退出码**非 0** | ①有未格式化文件而门禁"报绿"（CI 形同虚设）②只判 stdout 空 → 把"文件根本解析不了"判成通过——两个方向都错 | 先查退出码、再判输出，两个格式工具各查一遍（见 §3.2 Makefile 的 `fmt` 目标）；复核：留一个未格式化文件 `make fmt` 必须非零退出，再留一个语法错误文件同样必须非零退出 |
| 22 | 把达 `max_iterations` 当错误 | 只是"轮次用尽"却回 500，运维去查"服务为什么坏了" | `limit_reached` 是**正常终止**：HTTP 200 + 部分结果 + `status=limit_reached`；复核：集成测试断言 200 与 `data.status` |
| 23 | 历史里丢 `tool_calls` / `tool_call_id` / `reasoning_content` | 供应商 **HTTP 400**；历史被"洗干净"后**整个会话持续 400**（不可恢复） | 完整消息模型原样往返；裁剪以**完整工具交互组**为单位；复核：思考模式模型连续三轮带工具调用的集成测试 |
| 24 | 用 `context.WithValue` 传 Profile / Session | 隐式依赖换个皮复活（Java 的 ThreadLocal 等价物），并发串号难查 | 显式类型化参数（`RunRequest` / `RunEnv`）；复核：grep `context.WithValue` 命中处**逐处确认为追踪标识**（trace id 等，随日志透传），**业务状态零命中**——Profile / Session / 预算都不得经 `context` 携带 |
| 25 | 调用"先执行、后写审计" | 崩溃 / 取消留下"跑了但没记录"的窗口，审计与事实对不上 | `llm_calls` 与 `tool_invocations` **同一套两阶段写**：先插 `pending`、**插入失败即拒绝发出调用 / 执行工具**、返回后覆盖终态；残余窗口（终态更新失败保 `pending` → 重启标 `unknown`）如实登记；复核：让审计写失败，断言工具**未被调用**（见 [contracts/run-and-errors.md](contracts/run-and-errors.md) §7.2） |
| 26 | `go test -race` 与 `CGO_ENABLED=0` 混在一个 job | 竞态检测跑不起来，或产物不再是纯静态二进制 | 两个 job 各自固定 CGO 状态（见 §3.2）；复核：CI 中 `-race` 步骤与交叉编译步骤分属不同 job |
| 27 | 热加载写成"每轮重读" | 一次运行里前几轮用旧 Profile、后几轮用新正文（混搭），排障对不上号 | 每次运行开始重建**不可变快照**、运行内冻结；复核：原型链路 4（运行中改 `AGENT.md`） |
| 28 | 靠 fsnotify 递归监控目录树 | fsnotify **非递归**——新建的子目录收不到事件，新 Agent 静默不注册 | 逐目录 watch + 新目录补 watch + `rename` 事件处理 + 漏事件兜底 rescan；复核：测试里新建目录后断言其变更被感知 |
| 29 | 事务跨 LLM 调用 / 工具执行 | 一次几十秒的持锁把全部写入堵死（SQLite 单写者） | 事务只覆盖单条记录读写，LLM 与工具执行期间**不持**事务；复核：代码走查 + 并发写测试 |
| 30 | 启动时"重放"崩溃前的工具调用 | 有副作用的调用（`http_post` / `notify`）被执行两次 | 恢复语义 = **标记不重放**：`running` → `failed`、两表 `pending` → `unknown`；且**只有持工作区实例锁的进程做恢复**（只读的一次性命令不得把活实例的 Run 标成失败）；复核：原型链路 5（`kill -9` 后重启 + 第二实例拒绝启动），另验证多文件提交中断后 `.backup` 残留的两条判定分支（正式目录缺失 → 回滚；正式目录完整 → 前滚删残留） |
| 31 | 崩溃 / 取消后的会话留下**半截工具交互组**（assistant 带 `tool_calls`、结果缺失或不齐） | 下一条消息送进供应商 → `tool_call 无对应结果` **HTTP 400**；历史被"洗干净"前**整个会话持续 400**（取消或崩溃一次，会话从此哑掉） | 组的定位**必须按位置判定**——每条 assistant（`tool_calls` 非空）取其后 `seq` 更大且**连续**的 tool 消息段，段内 `tool_call_id` 未全覆盖即半截组；**不得**全会话按 `tool_call_id` 全局查找（旧 Run 同 id 的结果行会让新组被误判成已完成）；再把组**补齐为显式结果**（写入 `messages`，不进审计）：有终态行 → 还原真实结果；`pending` **或 `unknown`** → 「结果未知」；无行 → 「未执行」；取消路径由循环同步补齐、崩溃路径由启动恢复补齐；补齐**幂等**且判据落在**消息**上（不看内存列表，恢复可整段重跑）；复核：取消后与 `kill -9` 重启后**各发一条新消息，必须能跑完**（契约见 [contracts/run-and-errors.md](contracts/run-and-errors.md) §4.4） |
| 32 | 契约 YAML 里**值以 `*` 开头又不加引号**（如 `description: **url 留空 = 保留原值**`） | YAML 把 `*` 当**别名指示符** → 整个文件解析失败，OpenAPI 校验器/生成器直接拒收——而文件在编辑器里"看着完全正常"（2026-10-14 评审用 SnakeYAML 实测复现） | 含 `*` / `&` / `!` / `%` 等指示符开头的标量**必须**加引号；复核：仓库内一个单测用**项目已有的 YAML 依赖**解析 `docs/contracts/*.yaml`（frontmatter 与 `basalt.yaml` 本就用同一个库，零新依赖），解析失败即红 |
| 33 | 用"心跳超过 N 秒没刷新"判断持锁进程**已死**并抢锁 | 进程被 `SIGSTOP` / 宿主挂起 / 长停顿后心跳同样会停，但**它没有退出**——第二个进程抢到锁并执行恢复，两个进程同时写库，"同会话串行"当场失效 | 互斥**必须**用**操作系统级独占文件锁**（Unix：`flock` 加 `LOCK_EX` + `LOCK_NB`；Windows：`LockFileEx` 独占）：锁随进程消亡由内核释放，"拿不到锁"就等价于"有活进程"；心跳 / pid **仅作诊断**，**不得**据心跳过期抢锁；**不得**用 exists 判据（"创建文件即持锁"会让崩溃遗留的文件永久挡住启动）；复核：原型链路 5 加一条——把实例 `SIGSTOP` 后启动第二个写模式进程，必须**拒绝启动**而不是抢锁 |
| 34 | 崩溃恢复以 `runs.status = 'running'` 为**入口**，且"Run 标 `failed`"与"补齐半截工具组"**分两步提交** | 前一步提交后恢复进程再崩（或被 `kill -9`），下次启动已找不到这个 Run（它不再是 `running`）→ 半截组**永久遗留**，该会话此后每次请求 400；且"归一化"步骤已把 `pending` 翻成 `unknown`，补齐规则若只认 `pending`，同一个组在下次恢复里判不出该合成什么（用当前 DDL 在内存 SQLite 里构造过该反例：恢复候选为 0、工具结果仍缺失） | 恢复**必须**以**消息**为入口修**工具组**（**按位置判定**：每条 assistant（`tool_calls` 非空）取其后 `seq` 更大且连续的 tool 消息段、段内 id 未全覆盖即为半截组，**不得**全会话按 `tool_call_id` 全局查找——旧 Run 同 id 的结果行会让新组被误判完成；**不看** Run 状态），且每个 Run 的**状态修复与组补齐放进同一个短事务**提交——崩溃在提交前整体回滚、提交后一起落地，"Run 已翻而组仍半截"不可能持久化；补齐判据**必须**同时把 `pending` 与 `unknown` 认作「结果未知」；整段恢复**幂等**、每次启动重跑；补组完成后、开放服务前**必须**再扫一次 `runs`，把**没有半截组可补**的 `running` Run 一并标 `failed`（等首次 LLM 响应 / 最终回答已落库而终态未写 / 无状态 invoke）——**组看消息、Run 看表**，只做一样会留下永远 `running` 的记录；复核：原型链路 5 加一条——恢复执行到一半再 `kill -9`，下次启动仍能补齐同一批组 |
| 35 | 把解释器当普通可执行名放行（`python -c "…"` / `python -m …` / `bash -c`） | "脚本受控通道"退化成**任意代码执行**：模型一轮就能用 `-c` 内联代码绕过 `scripts/` 只读区，只读区形同虚设；且这条口子一开，后续"脚本须经评审"的纪律全落空 | **必须**在形态上钉死：解释器白名单（`shell.interpreters`）只接受 `[<脚本路径>, <参数…>]`，`arguments[0]` 以 `-` 开头一律拒（`-c` / `-m` / `-e`），脚本真实路径必须落在该 Agent `scripts/` 或技能库内（模型改不动脚本是该通道的前提）；`bash` / `sh` **不得**进白名单（等于把解释层请回来）；解释器进程拥有宿主同等权限，容器化隔离属 [Roadmap.md](Roadmap.md) F 方向；复核：shell 单测断言 `-c` 形态被拒、脚本越界被拒（契约见 [contracts/agent-md.md](contracts/agent-md.md) §5.2） |
| 36 | 多任务 Agent 用缺省派生 id（`<agent>-1` / `<agent>-2` 按列表顺序） | 调整 `schedules:` 顺序（或中间插一条）就把 id 换到别的任务头上——库里的 `enabled` 开关与执行历史**跟错人**，且全程无声 | **必须**：单项任务才允许缺省派生 `<agent>-1`；**两项及以上每项必须显式 `id`**（加载期拒绝并点名）；显式 id 全局唯一；给派生任务补显式 `id` = 换身份（旧状态不跟随）；复核：加载期单测（≥2 项缺 `id` 被拒）（契约见 [contracts/agent-md.md](contracts/agent-md.md) §3） |
| 37 | 恢复时按 `tool_call_id` 在**整个会话**里查结果行，判"组是否已完成" | 供应商的 `tool_call_id` 会重复，旧 Run 的结果行给新组的缺行"顶包" → 半截组判成完整、**永久遗留**，该会话此后每次请求 400；而恢复日志显示"无异常"，排查从"为什么又 400"重新开始 | **必须按位置判定**：组 = 一条 assistant（`tool_calls` 非空）**紧邻其后的连续 tool 段**（按 `seq` 相邻，直到角色不再是 tool），只看段内 id 覆盖情况；复核：构造"同一 id 在旧 Run 已收尾"的反例，断言新组仍被判为半截（契约见 [contracts/run-and-errors.md](contracts/run-and-errors.md) §4.3.2） |
| 38 | `PRAGMA foreign_keys` / `busy_timeout` 只在初始化时设一次（或只设在一条连接上） | 两者是**连接级**设置：连接池里其它连接没有 → 外键约束**静默失效**（脏引用照落库）、并发写直接 `SQLITE_BUSY` 报错；本地单连接跑测试全绿，上线压测才炸 | **必须逐物理连接设置**：modernc DSN 参数 `?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)` 随每条连接生效；`journal_mode=WAL` 是**库级持久**设置，设一次即可（两者语义不同，不得混为一谈）；复核：并发写测试 + 删父行断言被外键拒（见 [contracts/schema.sql](contracts/schema.sql) 迁移纪律 6） |
| 39 | `config_snapshots` 只存一个 `hash`，而哈希的对象与运行实际注入的内容来自**两次读取** | 两次读取之间文件被改（或读取失败被吞）→ 哈希对应的内容与模型实收提示词不是同一份；`runs` / `llm_calls` 上挂的 `config_hash` **证明不了**"当时用的就是这份配置"，审计链断在最有价值的一环，而表面一切正常 | **必须**"读即所记"：同一次读取产出**注入内容**与**冻结清单 manifest**（`[{path, sha256}]`，覆盖 AGENT.md + bootstrap 文件 + 绑定技能 `SKILL.md`），`hash` = manifest **规整序列化**的 sha256（内容寻址）；任一清单文件读失败 → 该次 Run **拒绝启动**（可读 reason），**不得**降级为部分清单；L2/L3 技能正文与 `scripts/` 按需读取、不在运行冻结范围（读时为准）；复核：快照单测——构建清单后改动文件，断言本次 Run 仍用旧内容且 `hash` 可复算（见 [contracts/schema.sql](contracts/schema.sql) `config_snapshots` 节） |
| 40 | 把 YAML 库的错误文案当"已脱敏"；凭证键的占位检查只在**字符串分支**做 | ①**首行也会带源文本，不只是源码摘录**：未定义别名带别名名（实测 `api_key: *SENTINEL` 的 `could not find alias "SENTINEL"` 把 SENTINEL 送进 stdout / stderr）、重复映射键带所重复的键名、溢出错误带源数字——"去了摘录"不等于脱敏；②**凭证类型检查缺位**：凭证值不是字符串时整个形态检查被绕过——数字 / 布尔经解码器被无损转成字符串（实测 `api_key: 123456789` 与 `api_key: true` 在完整工作区下 `status` 判 exit 0，配置被接受） | **库文案一个字都不外传**：位置取自错误的**结构化字段**（goccy `yaml.Error.GetToken()` 的行列），类别由具体错误类型自产（本包固定文案，不含配置数据）；**凭证键路径在解码器上游先过"类型关"**（必须是字符串）**再进"形态关"**（必须整体 `${VAR}`）；复核：未定义别名 / 重复映射键 / 数字 / 布尔四份夹具——报文含 `[行:列]` 且不含别名名、键名、原始值（`internal/cli/config_test.go` 的 `TestConfig_UndefinedAliasDoesNotLeakSource` / `_DuplicateMappingKeyDoesNotLeakSource` / `_RejectsNonStringCredential`） |
| 41 | 把"拒绝 / Kept **零写**"当成"不建表、不写版本行"就算数——**持久化初始化（WAL 切换）排在判态之后但仍在拒绝 / Kept 路径上** | `journal_mode=WAL` 是**库级持久属性**、写进库头：被拒绝的旧版本 / 过高 / 损坏库在 init 里被切成 WAL、SHA256 改变；已有当前版本库二跑 init 报"零写"、exit 0，库头仍变 WAL、SHA256 改变（两项均实测复现） | 装配顺序 = **判态 → 持久化修改 → 读池**：WAL 切换只在本进程**实际创建**（或长期服务入口）时执行，**Kept 路径连 WAL 都不切**；"零写"的判据是**库文件逐字节不变**（SHA256 前后相等 + 无 `-wal` / `-shm` + 回读 journal_mode 不变）；复核：DELETE 模式的完整库夹具跑 Kept 路径、拒绝两态夹具跑拒绝路径，各自断言逐字节不变（`internal/storage/bootstrap_test.go`） |
| 42 | DSN 的路径不转义 URI 保留字符 | `#` 被当 fragment 起始 → 路径被截断，**库静默落到兄弟路径**（实测：init 报"已创建"、退出 0，库落在名为 `hash` 的兄弟文件上，随后 status 判"库不存在"）；`%` / `?` 同类 | 转义 `%` / `#` / `?` 三个字符（`%` **先**转、单遍处理，避免替换产物被二次转义），其余按字节字面保留；复核：`#` / `%` / `?` / `%#` 同现四类路径夹具，断言库落**原路径**、父目录无截断残骸（`internal/storage/bootstrap_test.go` 的 `TestBootstrap_PathWithURIReservedCharacters`） |
| 43 | 覆盖率阈值按**四舍五入后的百分比**判卷 | 1249/2500 = 49.96% 先舍成 50.0% 再比较 → 不达标包判 ok（实测）；显示端若同时保留"50.0% (< 50%)"式输出，判据自相矛盾 | 判定用**原始计数**（100 × covered ≥ threshold × total），舍入只用于显示（两位小数）；复核：合成 profile 1249/1250 夹具必须判 FAIL 且显示 49.96%（`scripts/check_coverage_test.go` 的 `TestCheckCoverage_RoundingDoesNotPassBelowThreshold`） |
| 44 | 覆盖率 profile 的输入校验只认"`mode:` 前缀"；修校验循环时用 `tail -n +2 \| while` 或 `head` 取值不消费 | ①合法 mode 头 + 损坏正文（垃圾行 / 数值非法 / `mode: banana`）被聚合器静默跳过或强转，"输入坏了"判成"全部达标"；②`head -n 1` **只取值不消费**文件，循环仍从首行读起 → 表头被当正文拒绝（实测把真 profile 判红，报行号还错位）；③`cmd \| while …` 的循环体在**子 shell** 里，`exit 1` 只退子 shell，父进程照旧判卷、损坏输入被放行 | 首行**显式跳过**（行号先增后用；**不得**改成管道子 shell 写法）；正文逐行匹配记录语法，违例即非零退出且错误文案**不回显输入内容**；复核：`mode: banana` / 垃圾行 / 非法数值三条判红 + 真 profile 必须判绿（脚本自检逐条覆盖六类判据） |
| 45 | 以为 `busy_timeout` 覆盖所有锁竞争 | **本次切换场景的观察（口径收窄，不外推）**：`journal_mode` 的切换要求此刻没有别的连接持有该库，否则 SQLite **静默拒绝**——不报错、回读仍是原值；先开读池再设 WAL 即命中（实测）。本条只覆盖本场景，**不得**据此断言"所有 journal_mode 竞争都不等待"——一般锁竞争的等待行为另有实测口径 | **装配顺序固定为 WAL 先于读池**；切换后**必须回读判卷**（回读值非 `wal` 即失败，不得只发语句不验结果）；复核：先开读池再设 WAL 的夹具必须判红（回读仍是 `delete`；`internal/storage/pragmas.go` 的 `applyWAL` 回读循环） |

---

## 8. 风险与纪律

### 8.1 风险

| 风险 | 缓解 |
| --- | --- |
| AI 产出量大但一致性漂移（命名、错误语义、日志格式） | 宪法 + 颗粒度文档 + 每 US 一致性检查；样板在 US-1/US-2 期就定死（命名与错误风格一次定准） |
| 上下文污染导致"顺手重构" | 提示词只喂所需章节；越界即停、单独提案 |
| 门禁形同虚设（跳过、豁免） | 门禁一条命令、CI 强制执行；豁免必须逐条说明理由并经用户确认 |
| 演示环境的真实凭证操作出错 | 凭证类操作一律由人执行；AI 只给步骤与核对清单；凭证不入库、不入日志、不进对话 |
| 范围蔓延（新想法插队） | 新想法进"扩展阶段方向清单"或未决事项，不插当前 US |

### 8.2 纪律（不可违背）

1. **测试纪律**：不得删断言、不得跳过测试、不得放宽阈值让测试变绿。实现错修实现；认为测试错 → 停下报告
2. **如实汇报**：报错贴原文、跳过步骤明说、没验证的部分标注——**不得**掩饰
3. **凭证纪律**：真实凭证不明文落盘 / 落日志 / 进对话；通知渠道地址在库里也只存 `${ENV_VAR}` 引用、**发送时解析**（解析失败点名环境变量、不静默）；凭证类操作由人执行
4. **提交纪律**：AI 不自动 commit / push / tag；提交时机由用户决定（提交信息 = 英文 Conventional Commits 标题 + 四段**双语**正文（Problem / Design / Testing / Impact——每段先英文段、空行、再中文段逐段对照）；规范原文见 [EngineeringStandards.md](EngineeringStandards.md) §5，模板 `.gitmessage` 入库）
5. **文档纪律**：四份权威文档各司其职，**不得**互相"修正"；有冲突先裁决（技术方案最权威）再动手
6. **完成证据纪律**：任何「完成 / 通过 / 修好」的表述**必须**附**本轮当场**跑出的命令与输出（含退出码）——「没在本轮跑过那条验证命令，就不能声称它通过」；**不得**以「应该能过 / 上次跑过 / 局部过了」外推（2026-10-15 摘编自 superpowers 的 `verification-before-completion`，MIT，按本仓口径重写）
7. **审查回执纪律**：收到审查 / 评审意见——先验证再实施、不清楚先问，**不得**猜着改；审查方「跳过不判」的项**必须**显式列出、由执行者裁决，**不得**静默丢弃；**不得**表演式应答（「你说得对 / 好问题 / 感谢指正」式客套免掉，以技术回应与动作作答）（2026-10-15 摘编自 superpowers 的 code-review 两技能，MIT）

### 8.3 工程治理八项（2026-10-15 立；逐项带生效时点与可验证判据）

八项规则本体集中在本节；US 章节只保留一行挂点（§4.1 / §4.4 / §4.14），跑偏纠正表（§4.16）与 CI 表（§3.2）同口径。测试案例与检查脚本**随代码维护**（`evals/` / `scripts/` / 测试文件本身），本节只记规则与判据。

1. **修改权限（自主 vs 确认分档）**——已批准 US 范围内（颗粒度文档 §3 交付物清单内）的**实现细节**——辅助函数、私有类型、包内测试、不涉契约的内部重构——**自主完成、不逐项请示**；清单**外**的结构性变更——新增对外契约（API / 错误码 / Run 状态机 / 配置键 / 表结构）、架构边界（新包 / 依赖方向）、依赖坐标与版本、跨 US 重构——**必须**停下确认（软门禁）。**生效**：即日（US-1 起）。**判据**：每 US 收尾报告逐条列出本轮停等确认项（无则记「无」），§4.16 同口径。
2. **测试策略（基础设施 + 五类重点 + 验收先定）**——自 US-1 起建立**可复用测试基础设施**：临时工作区、本地 HTTP 测试服务（`httptest`）先落，固定假 Provider 随 US-2 接口落位、MCP 测试服务随 US-7 补齐——后续 US 一律复用，**不得**各自造桩（同 §1.3 不出现「先做桩、后面替换」式返工序）；五类重点在各 US 的验收测试（Harness）中**显式覆盖**：取消、并发、崩溃恢复、错误模型响应、审计写入失败；验收条件（Harness）**编码前定稿**（颗粒度文档先行），实现期**不得**事后放宽——放宽 = 变更，走用户确认。**生效**：US-1 起。**判据**：基础设施包被后续 US 复用（import 可知）；五个关键词在测试名可 grep。
3. **Agent 行为评测（固定任务集 + 变更重跑）**——自 US-4 起积累**固定任务集**（`evals/`，随代码维护），每次运行记录五项读数：工具选择、实际结果、越权尝试、调用次数、成本；**模型 / 提示词 / 技能变更后必须重跑**；按**业务结果**判定（任务是否达成），**不得**要求回答逐字一致。**生效**：US-4 起（首批随 US-4 落）。**判据**：每 US「变更导读」附重跑读数（通过率 / 调用次数 / 成本）。
4. **安全专项测试（注入与越权由执行层兜底）**——网页内容、文件内容、工具返回中嵌入恶意指令的用例：权限**仍由执行层**（模型之外的代码）控制；覆盖三类手法：路径绕过、重定向、环境变量泄露；授权与最小权限由模型外代码执行（宪法第 6 条），用例设计参考 OWASP LLM Top 10（提示注入 / 过度代理）。**生效**：US-4 起（有工具即有注入面）。**判据**：注入用例组全绿——越权尝试 → 四段式拒绝 + 审计 `failed`。
5. **契约变更规则（同批更新 + 旧态显式）**——修改 API / 错误码 / Run 状态 / 配置键 / 数据库结构时，**同一变更内**同步更新 `docs/contracts/`、消费方（实现 + 测试）与 [Compatibility.md](Compatibility.md)（双侧契约变化时）；**旧配置 / 旧工作区的处理必须显式**——迁移或拒绝（报错点名），**不得**静默兼容；契约冻结（§3.3）后的修改 = 变更提案，走用户确认。**生效**：即日。**判据**：变更类 commit 里 contracts / 实现 / 测试同批出现。
6. **CI 与人工审查（门禁全量入 CI + 审查重点排序）**——测试 / 覆盖率 / 安全扫描 / 构建**全部**为合并前检查（§3.2 双 job 全跑、`cover` 按包阈值阻断）；GitHub 侧两个 job 设为**必需检查**（branch protection，由用户设置、设置后回读一次）；人工审查按风险排序覆盖五类：文件访问 / 进程执行 / 凭证处理 / 迁移 / 恢复逻辑——每 US「变更导读」的 review 清单按此排序。**生效**：US-1（CI 内容）；远端就绪后（必需检查）。**判据**：CI 配置含 `cover`；branch protection 回读一次；变更导读符合五类排序。
7. **数据与长期运行（发布前必须定策略）**——**首次发布前必须明确**：审计 / 会话 / 日志 / 备份的**保留期与访问权限**、**磁盘告警**、**磁盘满时的行为**（失败关闭、报错可读、审计记录不破坏）、**清理如何保持消息组与关联记录完整**（工具调用组与 run 关联不得因清理断裂）；策略文本落 [Operations.md](Operations.md)、涉存储行为的设计落技术方案（改动走用户确认）；**未定即阻断发布**。**生效**：US-14 发布收尾前。**判据**：US-14 走查对照 Operations 对应节；满盘行为有实测读数。
8. **发布与平台验证（记录校验值 + 平台实跑 + 回归闸门）**——固定构建工具版本（`toolchain` 行 / CI setup-go 已锁）；每次发布的记录 = **版本号 + commit + 产物校验值（sha256）**；**锁、中文控制台、进程树取消**在目标系统（Windows + Linux）**实跑**验证；发布工件**包含前端资源**（US-13 站点）；升级与恢复按 [Operations.md](Operations.md) §5 **演练一次**；**每次发布前必须先跑一次 mock 模型集成测试**（独立目标 `e2e`，**零真实凭证**：假 OpenAI 兼容服务 + 发布候选二进制全链路回归；场景挂点见 §4.14，六条口径见 §8.4）。**生效**：首次发布（US-14）。**判据**：发布记录含校验值清单；平台实测读数；升级 / 恢复演练记录；mock 集成测试全绿（记录绑定候选产物 sha256，产物变化后重跑）。

### 8.4 mock 模型集成测试口径（六条，2026-10-15 定；US-14 落地）

与 §3.4 五链路原型验证（开发期）、§4.14 每日 Demo（真实链路人工验收）并列的**发布前自动化回归**——发布闸门，不进日常门禁；场景集（七场景）与挂点见 §4.14。

1. **构造三条硬边界**——① **不加入 `make check`**（七目标不含 `e2e`，日常门禁不被发布回归拖累）；② **生产代码无测试开关**（不设 test-only 分支 / 旁路环境变量，测试只经正式配置项——如 provider `base_url`——指到假服务）；③ **执行发布候选二进制**（跑将要发布的那份产物，不是源码形态）。测试记录**绑定候选产物的 sha256**；产物发生任何变化后**必须重跑**。
2. **入口隔离与防假绿**——测试代码用独立 build tag `//go:build e2e`（**不得**并入 `integration`，日常 `-tags=integration` 不携带它）；独立 Make 目标，登记构建步骤时注明**仅发布流程必跑**（不进入每个 US 的全量门禁）；发布运行**不得**因缺少产物 / 环境而跳过（缺前置 = 失败）；若由 `go test` 驱动，**必须** `-count=1` 禁用测试缓存（Go 官方推荐的禁缓存写法——缓存会把"没真跑"渲染成通过）。
3. **假模型双向断言**——假服务除返回预设响应外，**必须**断言**收到的请求**：`tool_call_id` 回填正确、工具结果已进入下一轮请求、消息顺序符合契约；**遇到预期之外的请求即失败**——专防"数据库记录齐了、发给模型的协议错了"。
4. **取消与超时分开验收**——**主动取消 / Run 超时 → Run 落 `cancelled`**（契约 §4.1：取消优先于预算判定）；**工具自身超时 → 工具审计落 `failed`（`error_kind = timeout`）、Run 继续**（契约 §7.3；失败结果回填后续轮继续）——两者判据不同，用例分开写；两者都验证**进程树清理**（无孤儿进程）；取消后**在同一会话再发一条消息**，确认会话可继续（契约 §4.4 补齐链路）。
5. **崩溃测试先确认命中故障点**——**不得**靠固定 sleep 掐时机：先等到"assistant 工具声明已落库 + 工具处于阻塞（经测试服务 / 辅助进程握手确认）+ 对应审计行为 `pending`"三点齐备，再强杀进程；恢复后核对：Run 收尾、审计归一化（`pending` → `unknown`）、工具交互组补齐、**无副作用重放**（恢复语义 = 标记不重放）；**再次重启**确认没有重复追加（恢复可重入，契约 §4.3.2）。
6. **环境完全独立、调度断言有来源**——每个场景：临时工作区 + 独立端口 + 测试环境变量。"零凭证"的准确表述是**零真实凭证**：启动校验仍要求非空 key，用虚构值（环境变量指向假值）。通知接收器用**域名形态**（如 `localhost`）——渠道域名不在白名单即 `sandbox_denied`（契约 §5.5），IP 字面量目标直接拒（§5.3），域名形态顺带把白名单匹配纳入回归。cron 断言**同时**核对 `task_executions` 与关联 Run（证明通知来自自动触发，而非手工发消息）；一切等待**有界**（到点即失败，不无限等）。

---

## 9. 总结

这份指南的核心只有三句话：

1. **先立模具**：宪法、包结构、门禁、CI 一次性立好——之后每次 AI 产出都有可当场执行的判卷标准
2. **逐 US 浇筑**：一个 US 一个循环，七项证据齐全才算完成——**可演示**是唯一诚实的进度单位
3. **带着答案开工**：第 7 节的 45 条陷阱不是假设，是 NivroOS 用一整个第一阶段（外加 Basalt 契约的历轮评审）换来的——**每一条都曾真实发生过，或在评审里被构造出反例**。Go 形态的开局优势，就是不必再重新发生一遍。
