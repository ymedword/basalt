# US-1 工程地基 — 模块执行颗粒度文档

> **文档状态**：草案（2026-10-15 生成；同日两轮评审修订——二稿七处：init 写入边界 / 环境变量缺失探测 / 首次建库原子性 / Makefile 版本注入 / 工作区路径口径 / tzdata 隔离对照 / 门禁自身覆盖；三稿六处：写入口分层（能力 vs 准入）/ 建库回滚夹具非空验 / 并发引导序列化 / tzdata 隔离分层（对照出默认层）/ 工具核实用 module 路径 / 凭证三路输出断言；`/us-register 1` 登记、`/module-dev 1` 消费）
>
> **事实主源顺位（供 `/us-register 1` 派生登记）**
>
> 1. **`docs/TechnicalSolution.md`**（文档之间最权威）——§1.2 技术栈与版本语义 / §8.1 工作区初始化 / §8.7 CLI 命令与轻·重路径 / §8.8 ConfigLoader / §9.1 SQLite 装配与迁移纪律 / §9.2 表清单（仅表名）/ §10.1 包结构与依赖方向 / §10.2 构建与发布 / §11.1 目录协议
> 2. **`docs/DemandAnalysis.md`**——§1.5 实施节奏（US-1 定位）/ §5.1 工作区初始化（产物树与文件用途注释）/ §5.12 命令行工具 / §5.13 配置与密钥 / §8 非功能需求 / §10.1–10.2 数据模型 / §13.4 质量门禁验收
> 3. **`.specify/memory/constitution.md`**（原则级最高）——原则 V（审计表随本 US 建齐）/ VII / VIII / IX + 架构约束（技术栈、依赖方向与包结构、凭证）+ 开发工作流（门禁、覆盖率判据、提交纪律）
> 4. **`docs/contracts/schema.sql`**——表 / 列 / 状态字面量的唯一事实源（本 US 的落地物）；`docs/contracts/run-and-errors.md` §4.3.1——实例锁边界与只读命令语义（`init` / `status`：不取锁、不恢复、不得动锁文件；打开库前必须校验 `schema_version`；只读路径只校验不迁移——与 init 引导写入的边界裁决见本文 §2.4）
> 5. **附加事实源**——`docs/Compatibility.md` §2.1 目录协议（工作区布局与基线同构）/ §2.5 数据库与迁移（`messages` 拆表、`schema_version` 受控前向迁移）；`docs/Operations.md` §1.1 与 §5.3（工作区 = 当前工作目录 `.basalt/`）/ §3（时区）/ §6（凭证复核口径）
> 6. **落地参考（非权威；冲突以技术方案为准）**——`docs/AiProgrammingGuide.md` §3.2 门禁与 Makefile / §4.1 US-1 拆解 / §8.3 工程治理八项（第 2 项测试基础设施自本 US 起）；`docs/GettingStarted.md` §2（工具链与四工具装法）、§4「已决·口径丙」、§7（make/shell 待实测项）；`.claude/skills/init-foundation/SKILL.md`（装法 runbook）
>
> **固定裁决句**：本文与最新技术方案冲突时，**以技术方案为准**；本仓配套工件（spec / plan / 已批准 US 清单）已锁定的引用在不与前者冲突时沿用，不另行改判。`specs/` 目录当前不存在（已核实）——本 US 开工前无 spec 锚点可引。
>
> **登记派生信息**：技术方案章节 = §1.2 / §8.1 / §8.7 / §8.8 / §9.1 / §9.2 / §10.1 / §10.2 / §11.1；需求文档章节 = §1.5 / §5.1 / §5.12 / §5.13 / §8 / §10.1 / §10.2 / §13.4；模块落位（摘要）= 根（`go.mod` / `Makefile` / CI / `scripts/`）、`cmd/basalt`、`internal/{app,cli,workspace,storage,core,testutil}`。

## 1. 模块概述

**定位**：把 Basalt 的工程地基一次性装好——骨架、门禁、配置装载、日志、工作区初始化、持久化基座（幂等引导建库与受控前向迁移机制）。业务逻辑（五大能力）不在本模块范围内。

**价值（WHAT / WHY）**：US-1 是整个递进依赖链的起点（需求 §1.5：`US-1 → US-2 → …`，每个 US 只消费左侧已交付的能力，不出现「先做桩、后面替换」式返工）。它交付三件可演示的事：从干净检出到 `make check` 一条命令全绿；`basalt init` 一键得到完整工作区且二跑幂等；工作区内的库与全部表就位、`basalt status` **无需配置文件**即可给出诊断报告。同时它是全仓的**样板来源**——命名与错误风格、依赖倒置的首个样例、日志装配、测试基础设施、覆盖率判卷脚本与脚本自检都在本 US 定准（编程指南 §8.1 风险表：「样板在 US-1/US-2 期就定死」）。规模参考：约 25~30 个文件，多数为样板类代码。

## 2. 设计要点与约束

### 2.1 职责边界表

| 维度 | 本 US 职责（做） | 非本 US 职责（不做，归属） |
| --- | --- | --- |
| 工程骨架 | 单 Go module、`internal/` 十包骨架（空包放 `doc.go`）、依赖方向纪律就位 | 各能力的真实实现（US-2 起逐包填充） |
| 门禁 | `make check` 七目标（fmt / lint / sec / test / test-integration / cover / build）+ CI 双 job（前置工具版本钉死） | 各 US 新增测试用例（基础设施本 US 交付、被后续 US 复用） |
| 配置 | `basalt.yaml` 装载：结构解析 + `${ENV_VAR}` 占位解析（存在性判定）+ 校验失败即拒 | 各段语义消费：providers（US-2）/ sandbox（US-4）/ memory（US-6）/ author（US-12） |
| 日志 | `log/slog` 双档装配（dev 文本 / prod JSON） | 请求级字段接入（US-4 起，且仅追踪标识） |
| 工作区 | `basalt init` 幂等骨架 + `basalt status` 只读报告（工作区路径 = 当前工作目录口径，§2.4） | `AGENT.md` 加载与校验（US-3）/ 技能库读取（US-3/4）/ 热加载 watcher（US-11） |
| 持久化 | 库**引导**（缺库创建：全量建表 + 版本行同事务，并发经写锁内重判串行化）+ **写模式打开**（读写池装配 + 状态判定，`OpenWritable`——US-2 审计写入即消费）+ **只读校验打开**（status）+ 受控前向迁移**机制**（`fs.FS` 注入 + `target` 参数，可测合成版本） | 各表读写路径与仓储（US-2 起）；**实例锁 / 恢复扫描 / 持锁调用顺序**（US-4 接线，§5）；备份 / 保留期策略（US-14 前，Operations） |
| 审计 | 表与关联列随 schema 建齐（宪法 V 的时序定义） | 两阶段写：`llm_calls` 自 US-2、`runs` / `tool_invocations` 自 US-4 起 |
| 命令面 | `init` / `status` 两条 + `--version` 全局选项 | 其余 10 条命令（chat US-4、profile US-3、serve US-9、gateway US-10、provider list US-2、tool list US-5、session list US-4 等） |
| 实例锁与恢复 | **不实现**；按「口径丙」用不入库小探针先钉死链路 5 语义（§6） | 实现归 US-4（起步手册 §4「待落位」；US-4 颗粒度文档定案） |
| 测试基础设施 | 临时工作区 + 本地 HTTP 测试服务（`testutil`，编程指南 §8.3 第 2 项）+ 覆盖率判卷脚本自检 | 假 Provider（US-2 以同口径补）/ MCP 测试服务（US-7 同口径补）/ mock 模型 e2e（US-14 发布闸门） |

### 2.2 关键约束清单（每条对应宪法条款 / 契约，带症状或判据）

1. **单二进制纪律**（宪法 VIII）：发布构建**必须** `CGO_ENABLED=0`；`-race` 与发布构建**分属不同命令 / job**。症状：竞态检测跑不起来，或产物不再是纯静态单二进制。
2. **不用 ORM，手写 SQL**（宪法 IX）：`go.mod` 不得出现 ORM 坐标；建表与迁移只在显式 SQL 脚本与 `schema_version` 记录里；**已发布迁移不得改写**，只许追加。库内版本**高于**二进制支持的版本 → **拒绝启动**（不得让旧二进制打开新库；契约 schema.sql 迁移纪律 2）。
3. **审计 day-one 的时序定义**（宪法 V）：审计表在 US-1 随 schema **建齐**；写入自对应 US 起（`llm_calls` US-2、`runs` / `tool_invocations` US-4）。本 US **不**写审计——表在、写入路径不在，属预期而非遗漏。
4. **context 纪律**（宪法 VII）：`context.Context` 只承载取消 / 截止 / 追踪标识；Profile / Session 等业务状态**必须**显式类型化参数传递；**不得**用包级变量模拟 ThreadLocal。本 US 的配置、库句柄、工作区根目录（`Deps.WorkDir`）、目标版本一律显式传递；`context.WithValue` 零使用（复核 grep）。
5. **依赖最小化 + 坐标先核实**（宪法 VIII）：本 US 新增坐标仅四个（§3.3）；版本以技术方案 §1.2（2026-10-14 实测）为基准，**写入 `go.mod` 前必须**经 `GOPROXY=https://goproxy.cn go list -m -versions <module>` 复核（软门禁：不一致停下报告）；**表外新增坐标先问用户**。
6. **Go 版本语义**（宪法·架构约束）：`go.mod` 的 `go` 行写**最低版本要求** `go 1.27`；准确构建版本由 `toolchain go1.27.2` 与 CI 的 `actions/setup-go` 共同固定。**不得**写成「`go.mod` 锁定 1.27.2」。
7. **凭证纪律**（宪法·架构约束 + 需求 §5.13 + Operations §6）：凭证一律 `${ENV_VAR}` 占位；解析用**注入的 `lookupEnv func(string) (string, bool)`**（缺省 `os.LookupEnv`）——**变量不存在** → 点名配置键路径与变量名、拒绝；**凭证键解析为空**（变量存在但值为空）→ 拒绝（Operations §6：「未解析或为空 → 拒绝启动并点名配置项」）；**不静默取默认值**。错误文案与运行日志**不得**回显凭证值（点名键路径 / 变量名即可）；明文**不得**落盘 / 落日志 / 进库。样例文件（`basalt.yaml.example`）**零明文**。
8. **写能力与命令准入分离；只读命令边界**（契约 run-and-errors §4.3.1 + 本文 §2.4 裁决注）：**能力层**——`storage.OpenWritable` 交付读写池装配 + 新库创建 + 状态判定（旧库打开成功但**不迁移**；迁移必须是显式 `Migrate` 调用），US-2 的审计写入（`llm_calls`）即消费此入口（编程指南 §4.2）；**准入层**——`init` 经 `Bootstrap` 只做引导（已存在的库一律零写：当前版本 Kept；旧 / 过高 / 损坏均拒绝且**不迁移**），`status` 只可 `OpenReadOnly`，本 US 无其他写库调用方；**重路径（US-4 起）**必须在**持锁后**调用 `OpenWritable`，旧库再显式迁移（顺序归 US-4 定案）。`init` / `status` **不取**实例锁、**不做**恢复、**不得**清理或删除锁文件；打开库前**必须**校验 `schema_version`（过高 → 拒绝并提示升级二进制）；**只读路径只校验、不调用迁移器**。状态矩阵见 §2.3。
9. **格式与静态检查纪律**（宪法·开发工作流 + CLAUDE.md 陷阱表）：`gofmt -l` / `gofumpt -l` 判卷**必须先查退出码、再判输出**；`staticcheck` 判定集**显式指定**（`-checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022`，EngineeringStandards §1.1）。**本 US 的初始全仓 `gofumpt -w .` 是唯一合法的全仓格式化场景**；此后只许 `gofumpt -w <本轮改动文件>`，全仓 `-l` 只读。
10. **覆盖率可执行判据**（宪法·开发工作流）：`make cover` 调 `scripts/check-coverage.sh` 按包判失败（`core` / `tool` / `memory` / `storage` ≥ 70%，其余 ≥ 50%；**有可执行语句但无测试文件的包计 0%、不得豁免**；无语句包跳过）。**入口包不豁免**：`cmd/basalt` 与 `scripts/stmtcount` 以「薄 `main` + 可测 `run` 包装 + 用例」达成 ≥ 50%，实现后不足则补用例，**不得**用豁免收口。判卷脚本自身判据六类（低于阈值 / 恰好达标 / 无语句跳过 / 有语句无测试计 0 / 测试命令失败 / 输入异常），由脚本自检用例逐条覆盖（§3.2）。安全关键路径（凭证红线、审计落库、数据库）**必须**有专项测试，不靠覆盖率兜底。
11. **build tag 单列**（宪法·开发工作流）：`-tags=integration` 的测试默认不跑，**必须**有独立的 `test-integration` 目标与 CI 步骤（本 US 无用例属预期，目标与步骤就位）；tzdata 隔离组（`//go:build tzisolated`）**不入** `make check`，由 CI **必跑**的隔离容器步骤执行（§3.3）——本机与装有系统 tzdata 的 Linux 主机都**不构成**隔离；独立目标 `e2e`（`//go:build e2e`）**不属本 US**（US-14 发布闸门，不进 `make check`）。
12. **契约字面量唯一源**（EngineeringStandards §3）：表 / 列 / 状态字面量以 [docs/contracts/schema.sql](../contracts/schema.sql) 为准，本文与代码**不另抄列清单**；`internal/storage/schema.sql` 是契约文件的嵌入副本，**必须**由单测强制逐字节一致（契约变更 = 同批更新两处，编程指南 §8.3 第 5 项）。
13. **包与依赖方向**（宪法·架构约束 + 技术方案 §10.1）：`channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`core` 只依赖标准库；接口**必须**定义在消费方；无环由 CI 用 `go list` 复核（US-9 起有真实消费面，本 US 建立首个样例：`core.SchemaIntrospector` 被 `cli` 消费、由 `storage` 实现）。10 包是**起点，不冻结**。
14. **日志纪律**（CLAUDE.md + init-foundation 决策记录）：统一 `log/slog`，**禁止**用 `fmt.Println` 当日志；dev 文本 / prod JSON 双档；请求级字段经 context 携带——**只承载追踪标识**。
15. **失败路径留痕**（EngineeringStandards §4）：配置装载失败、迁移拒绝、`init` 失败等路径**必须**记 ERROR / WARN 日志 + 退出码（**成功 0 / 失败 1**，原因由文案区分——2026-10-15 裁决）+ 可读文案，二者双轨（可观测性双轨在本 US = 日志轨落地 + 审计轨表就位）。
16. **迁移原子性与拒绝口径（fail-closed）**（宪法 IX + 契约迁移纪律 1–3）：首次全量建表与版本行**同一事务**；每版增量 SQL 与其版本行**同一事务**——中途失败**整体回滚**，库回到运行前状态（首次失败 → 无 `schema_version` 表残留，重跑按新库重试）；`schema_version` 表存在但**零行** = 损坏态，**拒绝**（不得猜测为新库或旧库）；目标版本缺少迁移文件 → **拒绝并点名文件**（不跳过、不静默）。**判态不得早于写权**：创建 / 迁移的判定与写入必须在同一 `BEGIN IMMEDIATE` 事务内完成——文件锁与 `IF NOT EXISTS` **不能**替代取得写权后的再次判态；两个 `init`、或 `init` 与「已持锁但初始化事务未提交」的启动窗口并发时，只有实际创建者执行初始化，竞争方在写锁释放后重判 → 见到版本行即以 `created=false`（Kept）成功返回，不报错、不重复初始化。库内版本与目标版本的全部组合行为以 §2.3 状态矩阵为准。

### 2.3 核心逻辑

本 US 核心逻辑为**四条流程**：A `basalt init`（引导）、B 存储打开（写 / 只读）、引导创建与迁移机制、C 配置装载、D `basalt status`（只读报告）。伪代码方法名与 §3 交付物清单**逐字一致**。

#### 命令 × 库状态 × 允许副作用（状态判定以 `schema_version` 表为准——契约迁移纪律 2）

| 命令 | 新库（`schema_version` 表不存在：库文件缺失 / 空文件 / 残留） | 当前版本（= target） | 旧版本（< target） | 过高版本（> 支持） | 损坏（表存在但零行） |
| --- | --- | --- | --- | --- | --- |
| `basalt init`（`storage.Bootstrap`） | **写**：全量建表 + 记 `version = target`，**同一事务**；失败回滚（无版本表残留），重跑按新库重试 | **零写**：Kept（幂等路径） | **零写**：拒绝（`core.ErrSchemaOutdated`），提示由持锁重命令升级——**不迁移** | **零写**：拒绝（`core.ErrSchemaTooNew`），提示升级二进制 | **零写**：拒绝（`core.ErrSchemaCorrupt`，fail-closed） |
| `basalt status`（`storage.OpenReadOnly`） | **零写**：报「库未就绪 → 先 `basalt init`」（`core.ErrDatabaseMissing`），计失败 | **零写**：报 `version` + 11 表清点 | **零写**：报「库版本 vN 旧于二进制 vM，需升级」，计失败 | **零写**：报「请升级二进制」，计失败 | **零写**：报「库损坏（版本表为空）」，计失败 |
| 重路径（`serve` 等；**US-1 不实现**，US-4 起） | 取锁 → `OpenWritable` 创建（创建路径同 init） | 取锁 → 打开（无写） | 取锁 → 打开（不迁移）→ 显式 `Migrate` 逐版（每版 SQL + 版本记录同事务） | 取锁 → 拒绝 | 取锁 → 拒绝 |

- **只读路径只校验、不调用迁移器**。写路径的并发由「取写权 → 重判态 → 只有创建者初始化」序列化：判定与写入在同一 `BEGIN IMMEDIATE` 事务内（约束 16），**不得**以先前判态或 `IF NOT EXISTS` 代替——两个 `init`、或 `init` 与「已持锁但初始化事务未提交」的 serve 启动窗口并发时，竞争方在写锁释放后重判，见到版本行即以 Kept（`created=false`）成功返回；`busy_timeout(5000)` 兜底等待，超时如实报错（边界裁决见 §2.4）。
- init / 重路径均**不引入**锁文件交互（契约 §4.3.1：不得触碰锁文件）——数据库侧并发完全由上述写权序列化承担。
- 版本目标值（`target`）与库状态判定一律**显式传参**：生产装配传 `storage.SupportedSchemaVersion`，测试传合成值以覆盖单版本期不可达分支（旧版本 / 损坏态）。

> 写模式打开的**能力**（`OpenWritable`）在 US-1 交付（US-2 审计写入 `llm_calls` 即消费，编程指南 §4.2）；**命令何时可以调用它**是独立的准入问题——`init` / `status` 的准入见约束 8，重路径的持锁顺序归 US-4 定案。

#### 流程 A — `basalt init`

```text
basalt init（不取实例锁、不做恢复——契约 §4.3.1；写入仅限引导创建，边界见 §2.4 裁决注）
  │
  ├─ workspace.Init(./.basalt)          # 幂等：缺则建、已有则跳过（不覆盖、不报错）
  │    ├─ 目录：agents/  skills/  memory/  logs/  archive/  sessions/
  │    └─ 文件：AGENTS.md  SOUL.md  USER.md  mcp_servers.yaml  memory/MEMORY.md
  │         （成员与基线同构，Compatibility §2.1；模板内容判据：MEMORY.md 含「## 核心记忆 /
  │           ## 归档记忆」两分区；mcp_servers.yaml 为空表且可被 go-yaml 解析；
  │           memory/agents/ 不预建——save_memory 时落盘）
  │
  ├─ storage.Bootstrap(./.basalt/basalt.db, deps.SupportedSchemaVersion)   # 见「状态矩阵」
  │    └─ 缺库 → 创建（全量建表 + version=target，同一事务）；已存在 → 零写（当前 Kept / 旧·高·损坏拒）；并发竞争方 → 写锁后重判为 Kept
  │
  ├─ 输出 Created / Kept 清单（二跑 Created 为空即幂等证据）
  └─ exit 0；任一步失败 → ERROR 日志 + 可读文案 + exit 1
```

```go
// 伪代码（方法名与 §3 逐字一致）
func workspace.Init(root string) (InitReport, error):
    report := InitReport{}
    for each dir in 目录清单:                 // 缺则建；已存在 → Kept
        ensureDir(path)
    for each file in 文件清单:                // 缺则写模板；已存在 → Kept（逐字节不动）
        if not exists(path):
            writeFile(path, template) → report.Created
    return report

func app.Run(ctx context.Context, args []string) int:
    deps := buildDeps()                      // WorkDir = os.Getwd()；SupportedSchemaVersion 与 Logger 显式传递
    root := cli.NewRootCommand(deps)         // deps 显式类型化传递（不塞 context）
    root.SetArgs(args)
    err := root.ExecuteContext(ctx)          // 子命令内按需装载配置（流程 C）：
                                             //   init 不读配置；status 容忍缺失（流程 D）；
                                             //   重命令（US-9 起）必读
    return exitCode(err)                     // 退出码唯一出口（成功 0 / 失败 1）
```

#### 流程 B — 存储打开（写 / 只读）、引导创建与迁移机制

```text
storage.OpenWritable(ctx, path, target)       # 能力层：US-2 审计写入起消费；US-4 起重路径在持锁后调用
  ├─ 打开（库文件缺失则创建）+ 装配：
  │    ├─ 连接池：写池 max 1（SQLite 单写者）；读池若干
  │    ├─ PRAGMA 逐物理连接（经 DSN：?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)）
  │    │    ——连接级设置；只在启动执行一次只影响一条连接（契约迁移纪律 6）
  │    └─ journal_mode=WAL 执行一次（库级持久属性；须在事务外执行）
  └─ BEGIN IMMEDIATE（取得写权）→ **重判态**（判态不得早于写权——约束 16）：
       ├─ 无 schema_version 表 → 创建：schema.sql 全量 + 写 version=target（**同一事务**）；
       │     失败回滚（无版本表残留，重跑按新库重试）；**本进程即实际创建者**
       ├─ 当前版本 → 提交空事务，正常打开（Kept 的 writable 形态）
       ├─ 旧版本   → 提交空事务，**正常打开但不迁移**——升级是调用方的显式动作
       │     （init 经 Bootstrap 在封装层拒绝；US-4 重路径随后显式 Migrate）
       ├─ 过高版本 → 回滚、关闭、core.ErrSchemaTooNew
       └─ 零行     → 回滚、关闭、core.ErrSchemaCorrupt

storage.Bootstrap(ctx, path, target)          # init 引导 = OpenWritable 的薄封装（策略层）
  ├─ 内部与 OpenWritable 同序（装配 + BEGIN IMMEDIATE 重判态）
  ├─ 旧版本（非创建者）→ 立即关闭 + core.ErrSchemaOutdated（init 不迁移——§2.4）
  ├─ created：true = 本进程实际创建；false = 打开时已存在（含并发竞争方写锁后重判所见——Kept）
  └─ 调用方取 created 后即关闭（init 不留长期写句柄）

storage.OpenReadOnly(ctx, path, target)       # status 用；不取锁、不恢复、不得动锁文件
  ├─ 库文件缺失 → core.ErrDatabaseMissing（报「库未就绪」，不代建——建库是 init / 重路径的事）
  └─ 只读打开 → 读 max(version) → 与 target 比较（**只校验、不迁移**；上方矩阵除「新库」列外同口径）

storage.Migrate(ctx, db, fsys, target)        # 机制层：迁移是显式动作，唯一生产调用方 = US-4 持锁重路径
  ├─ schema_version 表缺失 → 新库：执行 fsys 的 schema.sql（全量）+ 写 version=target
  │     （判定与写入同一 BEGIN IMMEDIATE 事务）
  ├─ 存在 → v := max(version)
  │    ├─ v == target → 无操作
  │    ├─ v >  target → ErrSchemaTooNew
  │    └─ v <  target → 逐版 v+1..target：每版 [增量 SQL + 版本记录] 同一事务；
  │                      缺该版迁移文件 → 拒绝并点名文件（不跳过、不静默）
  └─ 表存在但零行 → ErrSchemaCorrupt
```

> 迁移版本常量：`storage.SupportedSchemaVersion = 1`（本 US 即首个版本记录锚点）。增量迁移的**正路**（旧库逐版前向 1→2→3）由测试用合成迁移集覆盖——`Migrate` 以 `fs.FS` 注入 + `target` 显式传参；本 US 尚无真实的 `0002+` 文件，`Migrate` 也无生产调用方（唯一调用方 = US-4 持锁重路径）。

#### 流程 C — 配置装载（`cli.LoadConfig`）

```text
LoadConfig(path, lookupEnv)                   # lookupEnv 注入 func(string) (string, bool)；缺省 os.LookupEnv
  ├─ 读文件：缺失 → 返回可判错误（errors.Is(err, fs.ErrNotExist)）——行为由调用方定（status 容忍并报告，流程 D）
  ├─ YAML 解析（goccy/go-yaml——与 AGENT.md frontmatter 共用同一库，零第二依赖）
  ├─ 递归解析 ${VAR}：逐占位符 lookupEnv(VAR)
  │    ├─ **变量不存在**（ok=false）→ 报错【点名配置键路径与变量名，
  │    │     如 providers[0].api_key ← DEMO_KEY】、拒绝        ——「空串替换」不能绕过（已实测复现）
  │    ├─ **凭证键解析为空**（变量存在但为空值）→ 拒绝（Operations §6）
  │    └─ 替换完成后仍含 `${` 残留（模板畸形，如未闭合）→ 报错【点名键路径】、拒绝
  ├─ 凭证字段明文检查：原始值非 `${VAR}` 形态的凭证键 → 明文即拒（点名键路径；**不得回显明文值**）
  ├─ 必填 / 类型校验：失败 → 拒绝 + 可读信息，不静默取默认值
  └─ 通过 → *cli.Config
```

> 配置消费面：顶层含服务段 / `providers` / `sandbox` / `memory` / `budget` / `author`（键集来源：技术方案 §8.8、需求 §5.13、契约 run-and-errors §6 的 `budget:` 段；监听键字面量 `http.addr`，出处 contracts/openapi.yaml 与 Operations §2）。**本 US 消费**：日志档位（级别与形态，需求 §5.13 服务配置项）；其余段**解析并保留**，语义校验随消费 US 落位——不预造键。**工作区路径不从配置取**（流程 D 固定口径；配置覆盖键见「待决事项」#1）。

#### 流程 D — `basalt status`（只读诊断报告）

```text
basalt status（逐节尽力报告；节内失败不阻断后续节；汇总后 exit 0 / exit 1）
  ├─ 先定**唯一**工作区路径：<当前工作目录>/.basalt（Operations §1.1 / §5.3；需求 §5.1「当前目录」）
  │     ——不从配置取：一份报告的目录与库检查出自**同一路径**，不会检查两个工作区
  ├─ 节 1 工作区：workspace.Inspect(<前一步路径>)
  │     └─ 未初始化 → 报「工作区未初始化 + 建议先跑 basalt init」（计失败）
  ├─ 节 2 配置：cli.LoadConfig(<当前目录>/basalt.yaml, os.LookupEnv)
  │     ├─ 缺失（fs.ErrNotExist）→ 报「未发现配置（**路径明示**）——按纯工作区诊断继续」**不计失败**
  │     └─ 存在但无效 → 报错细节（点名路径与键）计失败
  └─ 节 3 存储：storage.OpenReadOnly(<工作区路径>/basalt.db, deps.SupportedSchemaVersion)
        ├─ 库缺失 → 报「库未就绪 → 先 basalt init」（ErrDatabaseMissing）计失败
        ├─ 过高 → 报「请升级二进制」（ErrSchemaTooNew）计失败
        ├─ 旧版本 → 报「库版本 vN 旧于二进制 vM，需由重命令升级」（ErrSchemaOutdated）计失败——不迁移
        ├─ 损坏 → 报「schema_version 表存在但无版本行」（ErrSchemaCorrupt）计失败
        └─ 就绪 → (*DB).SchemaVersion + (*DB).TableNames（11 表清点）
  汇总 → 全通过 exit 0；任一节失败 exit 1（原因由文案区分——已决）
```

### 2.4 差异裁决注

- **裁决注（契约 §4.3.1「只读命令」与 init 引导写入的边界）**：契约把 `init` 列入只读命令（不取锁、不做恢复、不得触碰锁文件），而初始化必然产生写入——此处给出明确边界，不按「无实质冲突」一笔带过：
  - 契约的「只读 / 写模式」是**实例锁语义**分类（能否与活实例并行、谁做恢复、谁持锁迁移），**不是**「零字节写入」承诺；技术方案 §8.7 的「只读文件系统状态」同读作「不依赖业务装配（注册表 / 全量装配）」。
  - `init` 的写入被限定为**引导创建**：仅在「无 `schema_version` 表」时全量建表并用**同一事务**记录版本行；**已存在的库一律零写**——当前版本 Kept、旧版本 / 过高 / 损坏均拒绝（§2.3 状态矩阵）。
  - 由此**迁移不是引导行为、也不是打开动作**：`OpenWritable` 打开旧库**不迁移**；版本升级必须是持锁调用方的**显式** `Migrate`（US-4 接线）——与「不得让旧二进制打开新库」同源。**存储能力与命令准入分开规定**（约束 8）：读写池装配 / 创建 / 判态是能力，US-1 交付（US-2 审计写入即消费）；命令何时可以调用（持锁顺序）归 US-4 定案。
  - **失败时的写入边界（明确不做"整体零写"承诺）**：`init` 中途失败**可能**留下已创建的骨架（缺失项保持缺失）——这是允许的；本 US **不新增**跨文件系统与数据库的整体零写要求，也不规定"失败即回滚整个工作区"。但无论成败：**不覆盖已有文件**（逐字节）、**不改不兼容的数据库**（旧 / 过高 / 损坏一律拒绝且不迁移）、**不触碰实例锁**。前两条由用例判卷（`TestInit_Idempotent` / `TestInit_DoesNotOverwriteExisting` 的逐字节比对；`TestBootstrap_RefusesExistingOutdated` / `_RefusesTooNew` / `_RefusesCorrupt` 的版本不变与拒绝断言）；"不触碰实例锁"本 US **无需判卷**——`init` / `status` 都不取锁，实例锁实现归 US-4，届时由其用例判卷。
- **取数澄清（工作区路径口径）**：Operations §1.1 / §5.3——工作区 = **当前工作目录的 `.basalt/`**（CWD 决定；需求 §5.1「在当前目录创建」同向）；DA §5.13 把「工作区路径」列为服务配置项——本 US **不引入**该配置键（init / status 均按 CWD 落），是否引入随 US-9 装配面复核（「待决事项」#1）。
- **取数澄清（`basalt.yaml` 位置）**：以 Operations §6 为准——与工作区**同级、不在 `.basalt/` 内**；DA §5.1 的 init 产物树不含它，故 `basalt init` **不生成**实例配置，只提供仓库内样例 `basalt.yaml.example`。
- **归类澄清（轻·重路径）**：技术方案 §8.7 轻命令清单含 `init`，而 init 的引导创建触及数据库——本 US 按「`init` / `status` 均不依赖业务装配」落地；`status` 的轻·重归类随 US-9 完整装配面落位时复核。

## 3. 交付物清单

### 3.1 代码（类级 / 关键签名；注释纪律：包级与导出符号级中英并列、中文在前）

```text
cmd/basalt/main.go
  - main()：唯一 main——os.Exit(run(os.Args[1:]))；import _ "time/tzdata"（见 3.5 依赖副作用）
  - run(args []string) int                          # 薄入口包装：main 之外零逻辑，可测（--version / 未知命令）
internal/app
  - Run(ctx context.Context, args []string) int    # 装配骨架；退出码唯一出口
  - NewLogger(cfg *cli.Config) *slog.Logger        # dev 文本 / prod JSON 双档
  - ports.go：storageStore / workspacePort          # 把实现包接到 cli 声明的端口上（D1 裁决的落地，见「已决事项」）
        # OpenReadOnly 的错误路径**必须**在这一步显式返回 nil——*storage.DB 装箱进接口会得到
        #   「非空接口 + 空指针」，上层 err != nil 判据随之失效（typed-nil；判据见 §3.2 的 ports_test.go）
internal/cli
  - ports.go：Store / SchemaView / Workspace       # 三个端口：接口定义在**消费方**（技术方案 §10.1；D1 裁决）
        # 返回类型刻意收成 cli 侧接口 / 裸切片——实现包因此无需（也不能）反向依赖 cli；
        #   实现由装配层注入（app），cli 零 import storage / workspace
  - NewRootCommand(deps Deps) *cobra.Command       # 根命令：注册 init / status；--version 全局选项（版本经 -X main.version 注入）
  - LoadConfig(path string, lookupEnv func(string) (string, bool)) (*Config, error)
  - Config / Deps                                  # 显式类型化参数（宪法 VII）：Deps 含 WorkDir（工作区根目录注入，
                                                   #   测试免 chdir）、SupportedSchemaVersion（目标版本；生产装配传
                                                   #   storage.SupportedSchemaVersion，测试传合成值）与 Logger（*slog.Logger，
                                                   #   测试注入带缓冲的 handler 以断言凭证不泄露）
internal/workspace
  - Init(root string) (InitReport, error)          # 幂等骨架（清单与模板判据见流程 A）
  - Inspect(root string) (Report, error)           # 只读清点（status 节 1）
internal/storage
  - OpenWritable(ctx context.Context, path string, target int) (*DB, error)
        # 写模式打开（能力层）：读写池装配（写池 max 1）+ 逐连接 PRAGMA + WAL；判定与写入同一
        #   BEGIN IMMEDIATE 事务（取得写权后重判态）——新库创建 / 当前打开 / 旧库打开不迁移 / 过高·损坏拒绝
  - Bootstrap(ctx context.Context, path string, target int) (created bool, err error)
        # init 引导（策略层）：OpenWritable 薄封装 + 旧库拒绝（ErrSchemaOutdated）；created 语义见 §2.3
  - OpenReadOnly(ctx context.Context, path string, target int) (*DB, error)
        # status 用：只读打开 + 状态矩阵校验；**只校验、不迁移**；不取锁、不恢复、不得动锁文件
  - Migrate(ctx context.Context, db *sql.DB, fsys fs.FS, target int) error
        # 机制层（迁移是显式动作；唯一生产调用方 = US-4 持锁重路径）：新库全量 / 旧库逐版（每版同事务）
        #   / 过高拒绝 / 零行拒绝 / 缺迁移文件拒绝；target 显式传参
  - SupportedSchemaVersion = 1                     # 常量：生产装配传给 OpenWritable / Bootstrap / OpenReadOnly / Migrate 的 target
  - (*DB) SchemaVersion(ctx) (int, error) / (*DB) TableNames(ctx) ([]string, error) / (*DB) Close() error
  - schema.sql（go:embed；与契约逐字节一致的副本）+ migrations/（目录就位，本 US 无增量文件）
internal/core
  - SchemaIntrospector 接口                          # status 消费 / storage 实现——依赖倒置首个样例
  - MetricsRegistry 接口 + NoopMetrics              # 观测占位（实装属扩展阶段）
  - 哨兵错误：ErrSchemaTooNew / ErrSchemaOutdated / ErrSchemaCorrupt / ErrDatabaseMissing / ErrWorkspaceNotInitialized
        # 依次对应：过高版本拒绝 / 旧版本拒绝（迁移归持锁重路径）/ 损坏态拒绝 / 库未就绪 / 工作区未初始化
internal/testutil（测试专用包；编程指南 §8.3 第 2 项）
  - TempWorkspace(t *testing.T) string              # 临时工作区（t.TempDir 系）
  - NewHTTPServer(t *testing.T, h http.Handler) *httptest.Server   # 本地回环，不访问真实网络
  - （FakeProvider 随 US-2、MCP 测试服务随 US-7，以同口径补——占位）
scripts/stmtcount/（package main：薄 main + 可测 run 包装；逻辑独立成文件）
  - count.go：标准库 go/parser 实现——按包统计可执行语句数（供 check-coverage.sh 判「无测试文件包」）
其余五包（provider / memory / tool / channel / web）：
  - doc.go 仅包声明 + 中英包注释（ST1000 判卷）；provider / memory / tool 本 US 无语句（覆盖率跳过）
```

### 3.2 测试（用例名英文，中文验收点以注释保留）

| 文件 | 用例 |
| --- | --- |
| `internal/workspace/init_test.go` | `TestInit_CreatesFullSkeleton` / `TestInit_TemplateContent`（MEMORY.md 两分区；`mcp_servers.yaml` 为空表且经 goccy/go-yaml 解析为空映射）/ `TestInit_Idempotent` / `TestInit_DoesNotOverwriteExisting` |
| `internal/workspace/inspect_test.go` | `TestInspect_ReadyAfterInit` / `TestInspect_Uninitialized`（`core.ErrWorkspaceNotInitialized`）/ `TestInspect_ReportsMissingEntries` / `TestInspect_DetectsTypeMismatch`（该是目录却是普通文件）/ `TestInspect_RootNotADirectory` |
| `internal/storage/migrate_test.go`（合成 FS 注入 + `target` 参数） | `TestMigrate_NewDatabaseCreatesAllTables` / `TestMigrate_Idempotent` / `TestMigrate_AppliesIncrementalWithFixtures`（1→2→3）/ `TestMigrate_RejectsNewerSchema` / `TestMigrate_NewDatabaseFailureRollsBack`（夹具：先建版本表与 a、再确定性失败——断言两者均**不存在**，见 §4.3）/ `TestMigrate_IncrementalFailureRollsBack`（0002 = [建表 + 确定性失败] → 版本仍 1 且该表不存在）/ `TestMigrate_EmptyVersionTableRejected` / `TestMigrate_MissingMigrationFileRejected` |
| `internal/storage/openwritable_test.go` | `TestOpenWritable_CreatesNewDatabase` / `TestOpenWritable_OpensExistingCurrent` / `TestOpenWritable_OldVersionOpensWithoutMigrating`（打开成功、版本不变——迁移是显式动作）/ `TestOpenWritable_RejectsTooNew` / `TestOpenWritable_RejectsCorrupt` / `TestOpenWritable_PragmasOnEveryConnection` / `TestOpenWritable_JournalModeWALPersists` / `TestOpenWritable_ExistingDatabaseStillSwitchesToWAL`（策略分叉的另一侧：长期服务入口对已有库仍确保 WAL） |
| `internal/storage/bootstrap_test.go` | `TestBootstrap_CreatesNewDatabase` / `TestBootstrap_KeepsExistingCurrent` / `TestBootstrap_RefusesExistingOutdated`（target 传合成值 2 以达到旧版本态）/ `TestBootstrap_RefusesTooNew` / `TestBootstrap_RefusesCorrupt` / `TestBootstrap_RefusalLeavesDatabaseUntouched`（拒绝路径同样逐字节不变）/ `TestBootstrap_ExistingCurrentStaysUntouched`（**Kept 零写的判据**：DELETE 模式完整库夹具 → SHA256 前后相等 + 无 `-wal` / `-shm` + 回读 journal_mode 仍为 `delete`）/ `TestBootstrap_PathWithURIReservedCharacters`（路径含 `#` / `%` / 空格 → 库落**原路径**）/ `TestBootstrap_ConcurrentInitRace`（并发：恰一个 `created=true`，其余 Kept 无错，终态一致——见 §4.3） |
| `internal/storage/openreadonly_test.go` | `TestOpenReadOnly_ReadsCurrent` / `TestOpenReadOnly_RejectsTooNew` / `TestOpenReadOnly_RejectsOutdated`（target 注入；拒绝后库版本不变——只校验不迁移）/ `TestOpenReadOnly_RejectsCorrupt` / `TestOpenReadOnly_MissingDatabase` |
| `internal/storage/schema_contract_test.go` | `TestSchemaFile_MatchesContract`（防漂移） |
| `internal/storage/pragmas_test.go` | `TestDSN_EncodesURIReservedCharacters`（DSN 装配对 `%` / `#` / `?` 的转义——`%` 先转、单遍）/ `TestDSN_ReadOnlyMode`（只读 DSN 形态） |
| `internal/storage/example_test.go` | `Example`（只读打开的**可执行文档**：`Example` 即由 `go test` 编译并核对输出，示例漂移即判红） |
| `internal/cli/config_test.go` | `TestConfig_ResolvesEnvPlaceholders`（正常值）/ `TestConfig_RejectsMissingEnvVar`（不存在 → 点名键路径与变量名）/ `TestConfig_RejectsEmptyCredential`（存在但为空 + 凭证键 → 拒）/ `TestConfig_RejectsMalformedPlaceholder`（`${` 残留）/ `TestConfig_RejectsPlaintextCredential`（拒且**错误文案不含明文值**）/ `TestConfig_RejectsSyntaxErrorWithoutEchoingSource` / `TestConfig_RejectsTypeErrorWithoutEchoingResolvedCredential`（类型错误发生在 `${VAR}` 解析**之后**，摘录里是凭证真值）/ `TestConfig_UndefinedAliasDoesNotLeakSource`（库文案**首行**带别名名——只输出自产类别 + `[行:列]`）/ `TestConfig_DuplicateMappingKeyDoesNotLeakSource`（重复映射键带键名）/ `TestConfig_RejectsNonStringCredential`（数字 / 布尔 / 空值三子例：凭证键路径先过**类型关**再进**形态关**，报文不回显原值）/ `TestConfig_RejectsInvalidValues` |
| `internal/cli/ports_test.go` | `TestDependencyDirection_CliDoesNotImportImplementations`（`go list` 断言 `internal/cli` 的**非测试导入集**不含 `internal/storage` / `internal/workspace`——D1 裁决的可执行判据） |
| `internal/cli/contracts_test.go` | `TestContractYAMLParses`（解析 `docs/contracts/*.yaml`——当前仅 `openapi.yaml` 一个 YAML 契约文件；编程指南 §7 陷阱 32） |
| `internal/cli/status_test.go` | `TestStatus_ReadyWithoutConfig`（init 后**无配置文件**：exit 0，节 2 报「未发现配置」不计失败——验收 Demo 主路径）/ `TestStatus_UninitializedWorkspace` / `TestStatus_ConfigInvalidFails` / `TestStatus_SchemaStateFails`（过高 / 损坏 / 旧版本（Deps 注入支持版本 2）逐子测试 → exit 1）/ `TestStatus_NoCredentialLeakInOutputs`（明文凭证 → exit 1；logger 缓冲 + stdout/stderr 捕获**三路**均不含凭证值）/ `TestStatus_SyntaxErrorDoesNotLeakCredential` / `TestStatus_TypeErrorDoesNotLeakResolvedCredential` / `TestStatus_UndefinedAliasDoesNotLeakCredential`（别名原文不得进任何一路）/ `TestStatus_NonStringCredentialFailsConfigSection`（数字 / 布尔 → 节 2 判无效 + 汇总计数，且不回显原值） |
| `internal/app/run_test.go` | `TestRun_ExitCodes`（成功 0 / 失败 1 分派） |
| `internal/app/ports_test.go` | `TestStorageStore_BootstrapThenOpenReadOnly`（端口往返 + 11 表清点）/ `TestStorageStore_OpenReadOnlyErrorReturnsNilView`（**typed-nil 判据用 `view == nil`**——`require.Nil` 对 typed-nil 同样通过，判不出该缺陷）/ `TestWorkspacePort_InitAndInspect` / `TestWorkspacePort_InspectUninitialized` / `TestWorkspacePort_InitErrorForwarded` / `TestLogLevel_MapsConfigValues`（四档取值 + 未知值回落 info） |
| `cmd/basalt/run_test.go` | `TestEntryRun_VersionFlag`（`--version` → 0 且输出含注入版本）/ `TestEntryRun_UnknownCommand`（→ 非零）——入口包覆盖率判据（§2.2 约束 10） |
| `cmd/basalt/tzdata_test.go`（默认层，可移植） | `TestTZData_LoadLocationPortable`（进程内加载检查；**不**判隔离有效性） |
| `cmd/basalt/tzdata_isolated_test.go`（`//go:build tzisolated`；**不入** `make check`——CI 必跑隔离步骤，§3.3） | `TestTZData_IsolatedEmbeddedFallback`（隔离主组）+ `TestTZData_IsolatedControlWithoutImport`（对照必须**以「时区加载失败」（exit 3 + 标记）**失败——见 §4.3） |
| `internal/testutil/testutil_test.go` | `TestTestutil_HTTPServer`（基础设施自证：首个不依赖真实网络的本地服务用例） |
| `scripts/check_coverage_test.go`（脚本自检；测试专用包——无可执行语句，覆盖率跳过） | `TestCheckCoverage_BelowThresholdFails` / `TestCheckCoverage_AtThresholdPasses` / `TestCheckCoverage_NoStatementsSkipped` / `TestCheckCoverage_StatementsWithoutTestsCountZero` / `TestCheckCoverage_GoTestFailurePropagates` / `TestCheckCoverage_MalformedInputFails` |
| `scripts/stmtcount/count_test.go` | `TestStmtCount_CountsExecutableStatements` / `TestStmtCount_NoStatements` / `TestStmtCount_ParseError` |

### 3.3 配置与工程文件

**本 US 新增坐标（仅四个；写入 `go.mod` 前逐坐标 `GOPROXY=https://goproxy.cn go list -m -versions` 复核——软门禁，不一致停下报告）**：

| 坐标 | 版本（技术方案 §1.2，2026-10-14 实测） | 用途 |
| --- | --- | --- |
| `github.com/spf13/cobra` | v1.10.2 | CLI |
| `github.com/goccy/go-yaml` | v1.19.2 | `basalt.yaml` + 契约 YAML 解析（与 frontmatter 共用同库） |
| `modernc.org/sqlite` | v1.60.1 | SQLite 驱动（纯 Go，无 CGO） |
| `github.com/stretchr/testify` | v1.12.1 | 测试断言 |

其余坐标（go-openai v1.43.0 / cron v3.0.1 / mcp go-sdk v1.8.0 / fsnotify v1.10.1）随消费 US 引入，本 US **不**预占。

- `go.mod` / `go.sum`：`go 1.27`（最低版本）+ `toolchain go1.27.2`（准确构建版本）。
- `Makefile`：七目标 `check fmt lint sec test test-integration cover build`（配方照编程指南 §3.2 建议原文；`fmt` 判卷**先退出码后输出**；`lint` = `go vet` + staticcheck 显式判定集；`build` = `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$$(git describe --tags --always)" -o bin/basalt ./cmd/basalt`）。
  - **Makefile 内必须写 `$$(git describe --tags --always)`**（recipes 里 `$(...)` 会被 Make 当变量引用展开为空——已用 `make -n` 复现实测）；顶部加 `SHELL := /bin/sh`（起步手册 §7 待实测项：原生 make 在 Git Bash 下配方的 shell 归属——写进 Makefile 后由 US-1 本机实跑一次 `make check` 消项）。
  - 验收：产物 `./bin/basalt --version` 输出**包含**注入值（§6 验收 Demo 步骤）。
- `.github/workflows/ci.yml`：双 job——`test`（默认 CGO，`make fmt lint sec test test-integration cover`）/ `build`（`CGO_ENABLED=0` 四平台交叉编译；**不得**跑 `-race`）。**前置条件（缺一不可）**：
  - `actions/setup-go` 钉 `go-version: 1.27.2`（与 `toolchain` 行一致）；Linux runner 自带 gcc（`-race` 必需）。
  - **四个检查工具经 `go install` 固定版本安装**（版本取技术方案 §1.2，命令同起步手册 §2.5）：`mvdan.cc/gofumpt@v0.12.0` / `honnef.co/go/tools/cmd/staticcheck@v0.8.2` / `github.com/securego/gosec/v2/cmd/gosec@v2.29.0` / `golang.org/x/vuln/cmd/govulncheck@v1.9.0`——写入 workflow 前逐条复核（软门禁），**必须用 module 路径**（`mvdan.cc/gofumpt` / `honnef.co/go/tools` / `github.com/securego/gosec/v2` / `golang.org/x/vuln`）跑 `go list -m -versions`——**不得**把含 `/cmd/...` 的安装路径传给 `go list -m`（那不是 module 路径）。「干净检出 + Go」**不足以**跑全门禁。
  - 本机（Windows dev）同四项 + GNU Make 4.4.1 + GCC（MSYS2 mingw-w64，`-race` 需要）——装法与实测读数见起步手册 §2.3–2.5。
  - **tzdata 隔离步骤（必跑，test job 内）**：容器内执行——先删除系统时区数据与 GOROOT 的 `zoneinfo.zip`（删后**断言不存在**，删不动即步骤失败——fail-closed），再跑隔离组。默认层**不做**正反对照：平台时区源（Linux `/usr/share/zoneinfo`、Windows 注册表）无法用环境变量遮蔽，对照放默认层会让正确实现在装有系统 tzdata 的机器上判红。步骤示意（镜像内路径以实现期实测为准）：

```yaml
- name: tzdata isolated check
  run: |
    docker run --rm -v "$PWD":/src -w /src golang:1.27.2 bash -c '
      set -e
      rm -rf /usr/share/zoneinfo "$(go env GOROOT)/lib/time/zoneinfo.zip"
      test ! -e /usr/share/zoneinfo && test ! -e "$(go env GOROOT)/lib/time/zoneinfo.zip"
      go test -tags=tzisolated ./cmd/basalt -run "TestTZData_Isolated" -count=1
    '
```

- `scripts/check-coverage.sh [--profile <file>] [--tree <dir>]`：按包阈值判失败（口径见 §2.2 约束 10）。缺省自跑 `go test -coverprofile`（于 `--tree`，缺省仓库根）；`--profile` 注入合成输入供自检。**判据六类**（§3.2 自检用例逐条覆盖）：低于阈值 → 失败；**恰好达标（≥）→ 通过**；无语句包（`[no statements]`）→ 跳过；有语句但无测试文件 → 计 0% 判失败；`go test` 自身失败 → 非零且不误判（不得把"无输出"当"通过"）；输入异常（不可读 / 格式非法）→ 非零 + 可读错误。
- `scripts/stmtcount/`：标准库 `go/parser` 小工具——判定无测试文件包「有无可执行语句」。判定口径固定，实现形态可在实现期替换（如发现更简路径），**口径不变**；入口覆盖同约束 10。
- `basalt.yaml.example`（仓库根）：样例配置——服务段（含 `http.addr: 127.0.0.1:8080` 缺省）+ `providers` / `sandbox` / `memory` / `budget` / `author` 段骨架；凭证一律 `${ENV_VAR}`；实例真实配置（与工作区同级）由用户 / 运维复制改名创建，**不入库**。

### 3.4 表（11 张，`schema.sql` 幂等建齐）

| 表 | 用途（一句话） |
| --- | --- |
| `sessions` / `messages` | 会话元数据 + 消息逐条追加表（承载工具调用结构） |
| `runs` | 一次运行（执行与审计单位） |
| `llm_calls` / `tool_invocations` | 两阶段写审计（US-2 / US-4 起写入；本 US 仅建表） |
| `config_snapshots` | 配置快照（内容寻址；US-4 起写入） |
| `scheduled_tasks` / `task_executions` | 定时任务状态与执行历史 |
| `notify_channels` / `memory_entries` | 通知渠道注册表 / SQLite 记忆档载体 |
| `schema_version` | 迁移版本记录（本 US 写入首行 version = 1） |

> 列 / 状态字面量的唯一事实源是 [docs/contracts/schema.sql](../contracts/schema.sql)——本文**不另抄列清单**；实现副本与契约的逐字节一致由 `TestSchemaFile_MatchesContract` 强制。

### 3.5 前序改造点与依赖副作用

- **前序改造点**：无——US-1 是首个 US（需求 §1.5：硬依赖无），不触及前序交付契约。
- **依赖初始化副作用**（必须显式知晓）：①`modernc.org/sqlite` 经 `import _` 注册 `"sqlite"` 驱动（driver 名用于 `database/sql`）；②`time/tzdata` 经 `import _` 内嵌 IANA 时区库——裸镜像里 `zone: Asia/Shanghai` 才解析得出来，否则带时区的 Agent 到容器里整只不注册（本 US 无 Agent 加载，属**为 US-3/US-10 预置**；兜底生效性由 §4.3 隔离 + 对照用例判卷）；③`goccy/go-yaml` 无全局副作用。

## 4. 验收测试（Harness）

### 4.1 测试分层标准

| 层 | 判据 | 本 US 用例 |
| --- | --- | --- |
| 单元 / 组件 | 不触碰**真实外部**资源（真实网络 / 真实模型 / 真实 webhook / 外部服务）。`httptest` 本地回环与**自持子进程**（helper-process：自调测试二进制或现场构建的探针、无外部依赖，如 tzdata 用例）都不是真实外部 | §3.2 除 tzisolated 组外的全部（默认层） |
| 集成（build tag `integration`） | 触碰真实外部资源（真实网络 / 真实模型 / 真实 webhook / 外部进程或服务）或跨环境的链路 | **无**——目标与 CI 步骤就位，空跑通过即绿（这是预期，不是跳过） |
| e2e（`//go:build e2e`，发布闸门） | 针对**发布候选二进制**的全链路 + 假模型服务 | **不属本 US**（US-14；不进 `make check`） |
| 隔离专项组（`//go:build tzisolated`） | 仅在「系统时区数据已移除」的隔离环境有意义（CI **必跑**容器步骤，§3.3）；**不入** `make check` / `make test`——本机（Windows 注册表源）与装有系统 tzdata 的 Linux 主机都不构成隔离 | `TestTZData_Isolated*`（§3.2 单列） |

本 US 默认层含并发用例（`TestBootstrap_ConcurrentInitRace`，goroutine + 独立连接竞争同一库文件）；`make test` 全量 `go test -race -cover ./...`（入口包、脚本自检与自持子进程用例都在默认层）。tzdata 隔离组会现场 `go build` 一个最小探针——只在 CI 容器步骤内执行（容器即 Go 工具链环境，不构成新前置）。

### 4.2 测试类 → 验收点映射

| 测试用例 | 验收点 | 依据（来源） |
| --- | --- | --- |
| `TestInit_CreatesFullSkeleton` | 目录 6 项 + 文件 5 项齐备；`memory/agents/` 不预建 | 需求 §5.1 产物树；Compatibility §2.1 |
| `TestInit_TemplateContent` | `memory/MEMORY.md` 含「## 核心记忆 / ## 归档记忆」两分区；`mcp_servers.yaml` 为空表且经 goccy/go-yaml 解析为空映射 | 需求 §5.1 树内用途注释（字面） |
| `TestInit_Idempotent` / `_DoesNotOverwriteExisting` | 二跑不报错、无新建；已有文件逐字节未变 | 技术方案 §8.1；需求 §5.1 |
| `TestMigrate_NewDatabaseCreatesAllTables` | 新库建齐 11 表 + `schema_version` 首行 = target；建表与版本行**同一事务** | 契约迁移纪律 2；技术方案 §9.1；本文 §2.2 约束 16 |
| `TestMigrate_Idempotent` | 二跑无错、版本行不变 | 契约 schema.sql（幂等） |
| `TestMigrate_AppliesIncrementalWithFixtures` | 合成迁移集 1→2→3：逐版前向、每版版本记录同事务；**target 可指定** | 宪法 IX；契约迁移纪律 1/3；评审修订 P1-3 |
| `TestMigrate_RejectsNewerSchema` | 库内版本 > target → `ErrSchemaTooNew` | 契约迁移纪律 2（不得让旧二进制打开新库） |
| `TestMigrate_NewDatabaseFailureRollsBack` | 夹具 [建版本表 → 建 a → 确定性失败语句]；失败后**版本表与 a 均不存在**（非空虚断言）；完好 FS 重跑成功 | 本文约束 16（首次建库原子性）；评审修订三稿 P1（夹具修正） |
| `TestMigrate_IncrementalFailureRollsBack` | 合成 0002 = [建表 + 确定性失败语句] → 版本仍为 1 且该表不存在；修好后重试到 2 | 本文约束 16（增量原子性） |
| `TestMigrate_EmptyVersionTableRejected` | 版本表零行 → `ErrSchemaCorrupt`（不猜测为新库 / 旧库） | 本文约束 16；契约迁移纪律 2 的 fail-closed 延伸 |
| `TestMigrate_MissingMigrationFileRejected` | 目标 2 而 0002 缺失 → 拒绝并点名文件；版本不变 | 契约迁移纪律 3（不跳过版本） |
| `TestOpenWritable_CreatesNewDatabase` / `_OpensExistingCurrent` | 新库创建（11 表 + version=target 同事务）；当前版本正常打开 | 本文 §2.3 状态矩阵（能力层） |
| `TestOpenWritable_OldVersionOpensWithoutMigrating` | 旧库打开**成功**且版本不变——迁移是显式动作 | 评审修订三稿 P1（写入口分层） |
| `TestOpenWritable_RejectsTooNew` / `_RejectsCorrupt` | 过高 → `ErrSchemaTooNew`；零行 → `ErrSchemaCorrupt` | 契约迁移纪律 2；本文约束 16 |
| `TestOpenWritable_PragmasOnEveryConnection` | 两条独立连接各自 `foreign_keys=1`、`busy_timeout=5000`（装配在能力层） | 契约迁移纪律 6（逐物理连接） |
| `TestOpenWritable_JournalModeWALPersists` | 创建后 `PRAGMA journal_mode` 回读 `wal`；重开仍为 WAL | 技术方案 §9.1（库级持久） |
| `TestBootstrap_CreatesNewDatabase` | init 缺库 → created=true、11 表 + version=target | 需求 §5.1（`basalt.db`）；本文 §2.3 状态矩阵 |
| `TestBootstrap_KeepsExistingCurrent` | 当前版本 → created=false、零写（版本行不变） | 技术方案 §8.1（幂等）；契约 §4.3.1（已存在库零写） |
| `TestBootstrap_RefusesExistingOutdated` | 旧版本（target 注入 2 构造）→ `ErrSchemaOutdated` 且**不迁移**（版本不变） | 契约 §4.3.1 分类 + 本文 §2.4 裁决注 |
| `TestBootstrap_RefusesTooNew` / `_RefusesCorrupt` | 过高 / 零行 → 各自哨兵错误 | 契约迁移纪律 2；本文约束 16 |
| `TestBootstrap_ConcurrentInitRace` | 并发 init：恰好一个 `created=true`，其余 Kept 无错；终态 11 表 + version=target | 评审修订三稿 P1（并发引导）——见 §4.3 |
| `TestOpenReadOnly_ReadsCurrent` | 当前版本可读：返回 version 与表名（status 节 3 数据源） | 技术方案 §9.2 |
| `TestOpenReadOnly_RejectsTooNew` / `_RejectsOutdated` / `_RejectsCorrupt` | 三种异常态各自哨兵错误；**只校验不迁移**（拒绝后版本不变） | 契约 §4.3.1（打开库前必须校验） |
| `TestOpenReadOnly_MissingDatabase` | 库缺失 → `ErrDatabaseMissing`，不代建 | 本文 §2.3 状态矩阵（先 init） |
| `TestSchemaFile_MatchesContract` | `internal/storage/schema.sql` 与契约逐字节一致 | 本文 §2.2 约束 12；编程指南 §8.3 第 5 项 |
| `TestConfig_ResolvesEnvPlaceholders` | 正常值：全部解析；取值来自注入的 lookupEnv | 需求 §5.13；技术方案 §8.8 |
| `TestConfig_RejectsMissingEnvVar` | 变量不存在 → 点名键路径与变量名（空串替换绕不过） | 技术方案 §8.8；需求 §5.3（缺失给清晰报错） |
| `TestConfig_RejectsEmptyCredential` | 凭证键变量存在但为空 → 拒绝 | Operations §6「未解析或为空 → 拒绝启动」 |
| `TestConfig_RejectsMalformedPlaceholder` | `${` 残留（模板畸形）→ 拒绝、点名键路径 | 技术方案 §8.8（未解析残留） |
| `TestConfig_RejectsPlaintextCredential` | 明文凭证 → 拒；**错误文案不含明文值** | 需求 §5.13；Operations §6；本文约束 7 |
| `TestConfig_RejectsInvalidValues` | 必填缺失 / 类型非法 → 拒 | 技术方案 §8.8（校验失败即退出） |
| `TestContractYAMLParses` | `docs/contracts/*.yaml` 经 goccy/go-yaml 全解析通过 | 编程指南 §7 陷阱 32 |
| `TestStatus_ReadyWithoutConfig` | init 后无配置：exit 0；节 2 报「未发现配置（路径明示）」不计失败 | 编程指南 §4.1 验收 Demo；本文流程 D |
| `TestStatus_UninitializedWorkspace` | 未初始化 → 可读提示 + exit 1 | 契约 §4.3.1（只读命令语义） |
| `TestStatus_ConfigInvalidFails` | 配置存在但无效 → exit 1（点名路径与键） | 技术方案 §8.8 |
| `TestStatus_SchemaStateFails` | 旧（支持版本注入）/ 过高 / 损坏三态 → 各自文案 + exit 1 | 本文 §2.3 状态矩阵；已决（0 / 1 退出码） |
| `TestStatus_NoCredentialLeakInOutputs` | 明文凭证配置 → exit 1；**logger 缓冲 + stdout/stderr 捕获三路均不含凭证值**（补 err.Error() 之外的实测面） | 约束 7；Operations §6；评审修订三稿补充 |
| `TestRun_ExitCodes` | 子命令成功 0 / 失败 1；`--version` 输出版本 | 技术方案 §10.2；已决 |
| `TestEntryRun_VersionFlag` / `_UnknownCommand` | 入口薄包装行为 + 入口包覆盖率 ≥ 50%（不豁免） | 本文约束 10；技术方案 §10.2 |
| `TestTZData_LoadLocationPortable` | 默认层可移植加载检查：进程内 `Asia/Shanghai` 解析成功（此层**不**判隔离——平台时区源不可遮蔽） | 需求 US-1 验收（编程指南 §4.1）；Operations §3 |
| `TestTZData_IsolatedEmbeddedFallback` + `TestTZData_IsolatedControlWithoutImport` | 隔离容器内：内嵌生效（主组过）+ 无导入对照**以「时区加载失败」（exit 3 + `TZ_LOAD_FAILED`）**失败——启动失败 / 崩溃 / 任意错误都**不算**对照成立 | 评审修订三稿 P2（隔离分层 + 失败原因确认） |
| `TestTestutil_HTTPServer` | 本地服务起停、请求-响应闭环 | 编程指南 §8.3 第 2 项 |
| `TestCheckCoverage_*`（6 条） | 低于阈值失败 / 恰好达标通过 / 无语句跳过 / 有语句无测试计 0 / `go test` 失败传导 / 输入异常报错 | 编程指南 §3.2 覆盖率底线；本文约束 10 |
| `TestStatus_UndefinedAliasDoesNotLeakCredential` / `TestConfig_UndefinedAliasDoesNotLeakSource` / `_DuplicateMappingKeyDoesNotLeakSource` | **库错误文案零外传**：YAML 库错误的**首行**（不只是源码摘录）也会带源文本（别名名 / 键名 / 源数字）——报文只输出自产类别 + `[行:列]`，四路输出（stdout / stderr / 日志 / err.Error()）均不含原文 | 编程指南 §7 陷阱 40；约束 7（凭证零明文） |
| `TestConfig_RejectsNonStringCredential` / `TestStatus_NonStringCredentialFailsConfigSection` | **凭证类型关在解码器上游**：非字符串凭证（数字 / 布尔 / 空值）经解码器无损转字符串后曾连形态检查一起绕过（完整工作区下 status 判 exit 0）——现在节 2 必须判失败并点名键路径，不回显原值 | 编程指南 §7 陷阱 40；本文约束 7 |
| `TestBootstrap_ExistingCurrentStaysUntouched` | **Kept 的零写是"库字节零改动"**：已有当前版本库连 WAL 都**不切**（WAL 是库级持久属性、写库头）——判据 = SHA256 前后相等 + 无 `-wal` / `-shm` + 回读 journal_mode 不变 | 编程指南 §7 陷阱 41；契约 §4.3.1（已存在库零写） |
| `TestOpenWritable_ExistingDatabaseStillSwitchesToWAL` / `TestBootstrap_PathWithURIReservedCharacters` / `TestDSN_EncodesURIReservedCharacters` | 策略分叉另一侧（长期服务入口对已有库仍确保 WAL）；路径含 URI 保留字符（`#` / `%` / `?`）时库落**原路径**而非静默落到兄弟路径 | 编程指南 §7 陷阱 41 / 42 |
| `TestDependencyDirection_CliDoesNotImportImplementations` / `TestStorageStore_OpenReadOnlyErrorReturnsNilView` | **依赖方向合规**（无环**不等于**合规：cli 不得 import 实现包；端口由装配层注入）+ 适配器错误路径返回**真 nil 接口**（typed-nil 陷阱） | 已决事项「依赖方向裁决（D1）」；技术方案 §10.1 |
| `TestStmtCount_*` | 可执行语句计数正确；无语句包判定；解析错误报错 | 同上 |

### 4.3 关键回归测试（写出代码）

```go
// TestInit_Idempotent：二跑幂等——已有文件不覆盖（需求 §5.1；技术方案 §8.1）
func TestInit_Idempotent(t *testing.T) {
    root := t.TempDir()
    ws := filepath.Join(root, ".basalt")

    first, err := workspace.Init(ws)
    require.NoError(t, err)
    require.NotEmpty(t, first.Created)

    // 预置"被运维手工改过"的文件，验证已有内容不被覆盖
    custom := []byte("# 自定义 bootstrap，init 不得覆盖\n")
    require.NoError(t, os.WriteFile(filepath.Join(ws, "AGENTS.md"), custom, 0o644))

    second, err := workspace.Init(ws)
    require.NoError(t, err)          // 二跑不报错
    require.Empty(t, second.Created) // 二跑无新建

    got, err := os.ReadFile(filepath.Join(ws, "AGENTS.md"))
    require.NoError(t, err)
    require.Equal(t, custom, got) // 已有文件逐字节未变
}
```

```go
// TestMigrate_NewDatabaseFailureRollsBack：首次建库失败必须整体回滚（约束 16）——
// 半途失败不得留下"已有但为空 / 只有版本表"的部分状态；完好 FS 重跑按新库成功。
// 夹具设计（评审修订三稿 P1）：① 失败点**不得**用 DDL 类型名构造——SQLite 类型名宽松，
//    `id INTEGER SYNTAX ERROR` 会被当作多词类型名接受、建表照样成功（已实测复现）；
// ② 夹具必须先建出"可观察的残留"（版本表 + a），否则"断言版本表不存在"在未回滚时也会通过（空虚断言）。
func TestMigrate_NewDatabaseFailureRollsBack(t *testing.T) {
    broken := fstest.MapFS{
        "schema.sql": &fstest.MapFile{Data: []byte(
            "CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL);\n" +
                "CREATE TABLE a (id INTEGER);\n" +
                "INSERT INTO nonexistent_probe (x) VALUES (1);\n")}, // 确定性失败："no such table"
    }
    db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "basalt.db"))
    require.NoError(t, err)
    defer func() { _ = db.Close() }()

    err = Migrate(context.Background(), db, broken, 1)
    require.Error(t, err)

    // 同事务回滚：版本表与 a **均不存在**（任一残留都说明未回滚）
    require.Zero(t, countTable(t, db, "schema_version"))
    require.Zero(t, countTable(t, db, "a"))

    // 完好 FS 重跑：按新库成功（版本表未残留，仍走新库分支）
    require.NoError(t, Migrate(context.Background(), db, schemaFS, 1)) // schemaFS = 包内 go:embed
    require.Equal(t, 1, mustVersion(t, db))

    // （countTable：sqlite_master 中点名表的存在计数；与 mustVersion 同为测试内小助手，写法以实现期为准）
}
```

```go
// TestMigrate_AppliesIncrementalWithFixtures：合成迁移集覆盖「新库全量 → 旧库逐版前向 1→2→3」——
// target 显式传参（评审修订 P1-3：支持版本写死的实现无法自然覆盖此路径）。
func TestMigrate_AppliesIncrementalWithFixtures(t *testing.T) {
    migrationFS := fstest.MapFS{ // 合成集：schema.sql = v1 全量；0002 / 0003 = 增量
        "schema.sql": &fstest.MapFile{Data: []byte(
            "CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL);\n" +
                "CREATE TABLE IF NOT EXISTS a (id INTEGER);\n")},
        "migrations/0002_add_b.sql": &fstest.MapFile{Data: []byte("CREATE TABLE b (id INTEGER);")},
        "migrations/0003_add_c.sql": &fstest.MapFile{Data: []byte("CREATE TABLE c (id INTEGER);")},
    }
    db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "basalt.db"))
    require.NoError(t, err)
    defer func() { _ = db.Close() }()

    require.NoError(t, Migrate(context.Background(), db, migrationFS, 1)) // 新库：v1 全量 + 记 1
    require.NoError(t, Migrate(context.Background(), db, migrationFS, 3)) // 旧库：1→2→3 逐版
    require.Equal(t, 3, mustVersion(t, db))                               // 版本记录到 3
    requireTableExists(t, db, "b")                                        // 0002 落地
    requireTableExists(t, db, "c")                                        // 0003 落地
    // （mustVersion / requireTableExists 为测试内小助手，写法以实现期为准）
}
```

```go
// TestMigrate_RejectsNewerSchema：库内版本高于目标 → 拒绝（契约迁移纪律 2：不得让旧二进制打开新库）。
func TestMigrate_RejectsNewerSchema(t *testing.T) {
    db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "basalt.db"))
    require.NoError(t, err)
    defer func() { _ = db.Close() }()

    // 构造"更晚版本的库"：schema_version 记入高于本二进制支持的版本
    _, err = db.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL)`)
    require.NoError(t, err)
    _, err = db.Exec(`INSERT INTO schema_version (version, applied_at, description) VALUES (?, 0, 'future')`,
        SupportedSchemaVersion+1)
    require.NoError(t, err)

    err = Migrate(context.Background(), db, schemaFS, SupportedSchemaVersion)
    require.ErrorIs(t, err, core.ErrSchemaTooNew)
}
```

```go
// TestBootstrap_ConcurrentInitRace：并发引导的序列化必须由用例兜底（评审修订三稿 P1）——
// 语义：恰好一个调用实际创建（created=true），其余在写锁释放后重判态 → Kept（created=false）
// 且不报错；终态一致（11 表 + version=target）。
// 缺「写权后重判」的实现在此判红：不用 IF NOT EXISTS 则竞争方报 "table already exists"；
// 只用 IF NOT EXISTS 而多个调用都报 created=true，则 created 计数为 2。
func TestBootstrap_ConcurrentInitRace(t *testing.T) {
    dbPath := filepath.Join(t.TempDir(), "basalt.db")

    const n = 4
    results := make([]bool, n)
    errs := make([]error, n)
    var wg sync.WaitGroup
    for i := range n {
        wg.Add(1)
        go func() { // 各 goroutine 经各自连接竞争同一文件（SQLite 文件写锁真串行）
            defer wg.Done()
            results[i], errs[i] = Bootstrap(context.Background(), dbPath, SupportedSchemaVersion)
        }()
    }
    wg.Wait()

    created := 0
    for i := range n {
        require.NoError(t, errs[i]) // busy_timeout(5000) 兜底；超时应如实报错而非被吞
        if results[i] {
            created++
        }
    }
    require.Equal(t, 1, created) // 恰好一个实际创建者

    db, err := OpenReadOnly(context.Background(), dbPath, SupportedSchemaVersion)
    require.NoError(t, err)
    defer func() { _ = db.Close() }()
    v, err := db.SchemaVersion(context.Background())
    require.NoError(t, err)
    require.Equal(t, SupportedSchemaVersion, v) // 终态：版本行就位
}
```

```go
// TestConfig_RejectsMissingEnvVar / _RejectsEmptyCredential / _RejectsPlaintextCredential：
// 「变量不存在」与「存在但为空」必须区分（lookupEnv 注入）且都 fail-closed；
// 明文拒绝时错误文案不得回显凭证值（约束 7；评审修订 P1-2）。
const demoConfig = "providers:\n  - name: demo\n    api_key: ${DEMO_KEY}\n"

func TestConfig_RejectsMissingEnvVar(t *testing.T) {
    path := filepath.Join(t.TempDir(), "basalt.yaml")
    require.NoError(t, os.WriteFile(path, []byte(demoConfig), 0o644))

    lookupMissing := func(string) (string, bool) { return "", false } // 变量不存在
    _, err := cli.LoadConfig(path, lookupMissing)
    require.Error(t, err)
    require.Contains(t, err.Error(), "providers[0].api_key") // 点名配置键路径
    require.Contains(t, err.Error(), "DEMO_KEY")             // 点名变量名
}

func TestConfig_RejectsEmptyCredential(t *testing.T) {
    path := filepath.Join(t.TempDir(), "basalt.yaml")
    require.NoError(t, os.WriteFile(path, []byte(demoConfig), 0o644))

    lookupEmpty := func(string) (string, bool) { return "", true } // 存在但为空
    _, err := cli.LoadConfig(path, lookupEmpty)
    require.Error(t, err) // Operations §6：「未解析或为空 → 拒绝启动」
}

func TestConfig_RejectsPlaintextCredential(t *testing.T) {
    path := filepath.Join(t.TempDir(), "basalt.yaml")
    require.NoError(t, os.WriteFile(path,
        []byte("providers:\n  - name: demo\n    api_key: \"literal-secret-value\"\n"), 0o644))

    _, err := cli.LoadConfig(path, func(string) (string, bool) { return "", false })
    require.Error(t, err)
    require.Contains(t, err.Error(), "providers[0].api_key")    // 点名键路径
    require.NotContains(t, err.Error(), "literal-secret-value") // 不回显明文值
}
```

```go
// TestSchemaFile_MatchesContract：实现副本与契约逐字节一致（防"字面量第二份定义"漂移）
func TestSchemaFile_MatchesContract(t *testing.T) {
    impl, err := os.ReadFile("schema.sql") // internal/storage/schema.sql
    require.NoError(t, err)
    contract, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "schema.sql"))
    require.NoError(t, err)
    require.Equal(t, contract, impl) // 契约变更 = 同批更新两处（编程指南 §8.3 第 5 项）
}
```

```go
// TestTZData_LoadLocationPortable（默认层，可移植）：进程内加载检查——**不**判隔离有效性。
// 平台时区源（Linux /usr/share/zoneinfo、Windows 注册表等）无法用环境变量遮蔽，
// 强隔离断言放默认层会让正确实现在装有系统 tzdata 的机器上判红（评审修订三稿 P2）。
func TestTZData_LoadLocationPortable(t *testing.T) {
    if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
        t.Fatalf("Asia/Shanghai 解析失败: %v", err)
    }
}
```

```go
//go:build tzisolated

// 隔离组（构建 tag `tzisolated`，**不入** make check；CI 必跑步骤在「已删系统时区数据与
// GOROOT zoneinfo.zip」的容器内执行，见 §3.3）。
//
// TestTZData_IsolatedEmbeddedFallback（主组）：helper 子进程 = 本测试二进制（携带 time/tzdata），
// 隔离环境内必须成功。
func TestTZData_IsolatedEmbeddedFallback(t *testing.T) {
    if os.Getenv("TZ_PROBE_HELPER") == "1" {
        if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
            t.Fatalf("内嵌 tzdata 未生效: %v", err)
        }
        return
    }
    out, err := runIsolated(t, os.Args[0], "-test.run=TestTZData_IsolatedEmbeddedFallback")
    require.NoError(t, err, string(out)) // 主组：隔离环境内必须成功
}

// TestTZData_IsolatedControlWithoutImport（对照）：无 time/tzdata 导入的探针在隔离环境内
// **必须失败**，且必须是「时区加载失败」这一种失败——探针契约：LoadLocation 失败 →
// stderr 打印 TZ_LOAD_FAILED: <err> 且 exit 3；成功 → exit 0。
// 启动失败 / 崩溃 / 任意非 3 退出码 = 对照无效 → 本测试判红（不得把任意错误当成隔离成功）。
func TestTZData_IsolatedControlWithoutImport(t *testing.T) {
    probe := buildProbeWithoutTZData(t) // 现场 go build；构建细节以实现期实测收敛，判据固定
    out, err := runIsolated(t, probe)
    var ee *exec.ExitError
    require.ErrorAs(t, err, &ee, "对照探针未失败或非进程退出错误——隔离无效或探针构造有误: %s", out)
    require.Equal(t, 3, ee.ExitCode(), "对照失败原因不是时区加载失败: %s", out)
    require.Contains(t, string(out), "TZ_LOAD_FAILED")
}

// runIsolated：子进程环境 = 继承 + TZ="" + ZONEINFO=<不存在路径> + GOROOT=<不存在路径>
// （容器步骤已实际删除系统 tzdata 与 GOROOT zip，环境变量为双保险）。
// buildProbeWithoutTZData 生成的探针 main（无 `import _ "time/tzdata"`）：
//     func main() {
//         if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
//             fmt.Fprintf(os.Stderr, "TZ_LOAD_FAILED: %v\n", err)
//             os.Exit(3)
//         }
//         os.Exit(0)
//     }
```

> 断言写法上标注「以实测为准（写前核实）」的六处：① modernc DSN 的 `_pragma` 参数语法；② modernc 多语句执行 / 建库脚本的切分方式（回滚断言与切分实现无关，但夹具失败点的构造依赖它）；③ tzdata 各平台时区源查找顺序与隔离手段（含 Windows 注册表是否为 `LoadLocation` 候选源）；④ cobra `--version` 输出格式；⑤ 对照探针的现场构建与调用姿势（含 exit 3 + `TZ_LOAD_FAILED` 约定的落地）；⑥ CI 隔离容器步骤的镜像内路径（基底镜像 `/usr/share/zoneinfo` 存在性、GOROOT `zoneinfo.zip` 布局、容器内模块下载代理）。

## 5. 范围边界（核心阶段不做项，逐项标注依据）

| 不做项 | 依据 |
| --- | --- |
| 五大能力的业务面（Provider US-2 / Agent 加载与 profile US-3 / ReAct 与工具 US-4–5 / Memory US-6 / MCP US-7 / 通知 US-8 / Web US-9 / 调度 US-10 / 管理台与热加载 US-11 / 动态管理 US-12） | 需求 §1.5 依赖序（本 US 只交付依赖链起点） |
| 12 条命令中的 10 条（本 US 仅 `init` / `status` / `--version`） | 技术方案 §8.7 命令表；各命令归对应 US |
| **实例锁、恢复扫描与持锁调用顺序**（重路径接线：取锁 → 写模式打开 → 显式迁移 → 恢复） | 契约 §4.3.1（恢复归持锁进程）——归 US-4；**写模式打开能力（`OpenWritable`）与迁移机制（`Migrate`）本 US 交付**（US-2 审计写入 `llm_calls` 即消费 OpenWritable，编程指南 §4.2）；命令何时可调用归 US-4 定案（约束 8；评审修订三稿 P1） |
| 实例锁与崩溃恢复的**实现**（含 `serve` 启动恢复） | 起步手册 §4「待落位：建议归 US-4」；本 US 仅不入库探针（§6） |
| 审计两阶段写（`llm_calls` US-2 起、`runs` / `tool_invocations` US-4 起） | 宪法 V 的时序定义（表本 US 建齐） |
| 沙箱三档执行、授权矩阵、工具注册 | 技术方案 §6；US-4 / US-5 |
| 监控指标实装（本 US 仅 `MetricsRegistry` 接口 + no-op） | init-foundation 决策记录（监控属扩展阶段） |
| 认证 / 多租户 / 审计查询 / Tool Policy / SSO | 需求 §6 扩展功能表；宪法 X（企业级治理放扩展阶段） |
| 官网 `website/`、`examples/` 演示素材、mock 模型 e2e 发布闸门 | 需求 §1.5（US-13 / US-14）；编程指南 §8.4 |
| 网络文件系统（NFS / SMB）上的工作区 | 契约 run-and-errors §4.3.1 边界（v0.1 不支持） |

## 6. 验证与验收（剩余人工项）

| 项 | 做法与判据 |
| --- | --- |
| 门禁全绿 | 干净检出 + **完整前置**（Go 1.27.2 + make + GCC + 四工具固定版本，见 §3.3）：`make check` 七目标全绿（本轮当场输出为准——完成证据纪律，编程指南 §8.2 第 6 条） |
| 验收 Demo（编程指南 §4.1 五条 + 修订） | ①`make check` 全绿；②`basalt init` 出工作区（含 `basalt.db`）、**二跑幂等**；③`basalt status` 在**无配置文件**下 exit 0：显示库、`schema_version`、11 表就绪（节 2 报「未发现配置」不计失败——评审修订 P2-5；**无需**配置准备步骤）；④`CGO_ENABLED=0 go build` 出静态二进制，且 `./bin/basalt --version` **包含**注入版本（`$$(git describe --tags --always)` 生效——评审修订 P2-4）；⑤测试基础设施跑通首个不依赖真实网络的用例（`TestTestutil_HTTPServer`）；⑥ tzdata 见下行（必验） |
| 负向三连 | 故意留未格式化文件 → `make fmt` 非零；语法错误文件 → 同非零（**先查退出码**）；留 `${UNRESOLVED}` 残留 → 启动即拒并点名 |
| tzdata 内嵌实测（**必验**，三稿口径） | ①本机：`TestTZData_LoadLocationPortable` 通过（可移植读数——**不**作隔离结论，Windows 注册表源不可遮蔽）；②**CI 隔离容器步骤（必跑）**：主组通过 + 对照组**以「时区加载失败」（exit 3 + `TZ_LOAD_FAILED`）**失败（启动失败 / 崩溃 / 任意错误不算通过）。隔离结论只取 ②；步骤缺失或隔离断言未落实 → 如实登记为未完成项 |
| 探针（「口径丙」，**不入库**） | 在本仓之外空目录、内存 SQLite：①按契约 §4.3 恢复语义手工构造反例状态（`running` Run / `pending` 审计行 / 半截工具组），验恢复算法可重入、补组以消息为入口、事务边界；②验实例锁语义（活实例持锁 → 第二写模式进程拒绝启动；暂停的实例**不得**被抢锁——Windows 对应手段见「待决事项」#2）。**记读数**（锁语义与恢复可重入性**分开记**）；偏离契约处 → 给出偏差与拟改点**回写 `contracts/`**（交用户确认，`/speckit-analyze` 收口核对）。**记录口径**：暂停实例一项未验证时，**链路 5 不得记「全部通过」**，如实保留为未完成项（评审修订）。**归属与门槛（2026-10-15 裁决）**：小探针是 **US-1 交付后的独立待办项**——**不得整体后移**（链路 1 / 3 / 4 的验证随 US-4 执行，不带上它）；它**不阻断** US-2 / US-3 开工，但完成前**必须**如实留在 US-1 的剩余项清单里 |
| CI 与分支保护 | 首次推送后 `.github/workflows/ci.yml` 全绿——双 job + test job 内 **tzdata 隔离步骤（必跑）**（前置含四工具固定版本安装与 module 路径复核，见 §3.3）；GitHub 侧把两 job 设为**必需检查**（由用户设置、设置后**回读一次**——编程指南 §8.3 第 6 项） |
| 凭证复核 | 范围收窄为**运行对象**：`basalt.yaml.example`、测试夹具与**捕获的三路输出**——错误文案（`TestConfig_RejectsPlaintextCredential`）、logger 缓冲与 stdout/stderr（`TestStatus_NoCredentialLeakInOutputs`）均带 `NotContains` 断言（评审修订三稿补充：补 err.Error() 之外的实测面）；实例配置按 Operations §6 原式核（`grep -rnE "sk-\|api_key: [^$]" <实例目录>/basalt.yaml` 零命中）。**全仓 grep 会命中文档示例，不作为判据** |
| 装配确认（人工走查） | `go.mod` 无框架 / ORM 坐标（宪法 I / IX）；业务代码 `context.WithValue` 零使用（宪法 VII）；`app.Run` 装配面仅配置 / 日志 / 工作区 / 存储；`internal/storage/schema.sql` 与契约一致（由单测兜底）；入口包覆盖率达标（由用例兜底，无豁免） |
| 静态构建平台验证 | Linux 产物 `file` / `ldd` 断言静态；Windows 产物无额外依赖直接启动（宪法 VIII 复核方式） |

## 已决事项（2026-10-15 用户裁决带入本 US；出处：起步手册 §4「口径丙」+ 本次评审修订）

- **五链路原型分阶段执行**（口径丙）：本 US 交付后，用**不入库**小探针先验链路 5（崩溃恢复算法 + 实例锁语义，内存 SQLite，不碰 provider / 循环）；链路 1 / 3 / 4 在 US-4 交付后立即验证；链路 2 的 HTTP 准入面（409 / 429）到 US-9 补齐。**记录口径**：暂停实例（SIGSTOP / Windows 挂起）一项若未验证，链路 5 **不得**记「全部通过」——如实保留为未完成项。**归属口径**：小探针是 **US-1 交付后的独立待办项**（**不得**整体后移；**不阻断** US-2 / US-3 开工），崩溃恢复与实例锁的**正式实现**归 **US-4**。
- **契约状态**：运行时契约四件套在对应链路验证通过前为**待验证设计草案**——**不构成** US-2 / US-3 开工阻断；最终冻结条件 = 五链路全部通过 + 真实模型调用往返，实测偏差回写 `contracts/` 并更新状态头。
- **实例锁与恢复的实现归属**：US 序列中尚无明确归属，**建议归 US-4**（US-4 颗粒度文档定案；本 US 不带实现）。
- **探针期的锁实现手段**：Windows 锁手段在**探针启动前单独确认**（标准库 `syscall` vs `golang.org/x/sys`——含新增坐标则先请示）；Unix 侧 `syscall.Flock` 无争议。
- **退出码口径**：成功 0 / 失败 1，具体原因由文案区分（不采用分档非零）。
- **`basalt init` 参数面**：仅作用于**当前目录**（需求 §5.1 字面），不加 `--dir` / 位置参数；需要时另议。
- **`basalt status` 输出形态**：首版仅人类可读文本；机器可读 `--json` 随管理台（US-11）需要时另议。
- **无配置诊断模式**：`basalt status` 在配置文件**缺失**时照常给出报告（节 2 报「未发现配置（路径明示）」、不计失败）——验收 Demo（init 后直接 status）因此无需配置准备步骤；配置**存在但无效**计失败。（评审修订 P2-5 的二选一：取「允许无配置的诊断模式」。）
- **依赖方向裁决（D1，2026-10-15）**：`cli` 直接 import `storage` / `workspace` **不因「无编译环」而合规**——依赖倒置要求**由装配层 `app` 向 `cli` 注入接口或函数**；「没有环」只是最粗的形态，直接引用实现包同样让消费方绑死在具体实现上，装配层随之失去替换与测试注入的接缝。落地：`internal/cli/ports.go` 定义 `Store` / `SchemaView` / `Workspace` 三个端口（**接口定义在消费方**，返回类型刻意收成 cli 侧接口 / 裸切片），`internal/app/ports.go` 提供适配器（含 `*storage.DB` 装箱前的 typed-nil 判空）；可执行判据 = `internal/cli/ports_test.go` 的 `TestDependencyDirection_CliDoesNotImportImplementations`（`go list` 断言 `internal/cli` 的**非测试导入集**不含 `internal/storage` / `internal/workspace`）。同口径适用于后续消费方包（`channel` / `web` 等）——本裁决是对技术方案 §10.1「接口定义在消费方」的**具体化**，未改其原文。

## 待决事项（交用户裁决；附默认建议）

1. **配置是否提供「工作区路径」覆盖键**（DA §5.13 把「工作区路径」列为服务配置项；Operations §1.1 / §5.3 为 CWD 固定口径）：本 US 不引入、按 CWD 落（§2.4 取数澄清）。默认建议：维持 CWD 口径，该键随 US-9 装配面复核时再议（引入需同步改 init / status / 运维手册的路径语义）。
2. **探针启动前的 Windows 锁手段确认**（见「已决事项」）：默认建议优先评估标准库侧可行性（`syscall` 包的 `LockFileEx` 调用能力，写前核实），不可行则 `golang.org/x/sys/windows`（**新增坐标需用户确认**）。

## 附录 A：易漏维度核对（US-1 适用性）

| 维度 | US-1 处理 |
| --- | --- |
| 审计关联标识传导 | **不适用**（本 US 无审计写入路径；关联列 `run_id` / `tool_call_id` 随契约建齐，写入自 US-2 / US-4 起——宪法 V 时序） |
| 上下文对象传递机制 | **适用**：显式类型化参数（`Config` / `DB` / `Deps`，含 `WorkDir` 与目标版本）；context 只承载取消 / 追踪（宪法 VII）；本 US `context.WithValue` 零使用，§6 装配确认走查 |
| 前序改造点清单 | **无前序**——首个 US，§3.5 显式登记「无」 |
| 后续模块能力占位 | **适用**：`MetricsRegistry`（观测）/ `testutil`（US-2 假 Provider、US-7 MCP 测试服务）/ 配置段解析保留（US-2/4/6/12）/ 11 表（US-2 起）/ 迁移机制与 `migrations/`（US-4+ 增量）/ `core.SchemaIntrospector`（依赖倒置样例）/ tzdata（US-3 / US-10 的 zone 前提） |
| 缓存语义 | **适用**：`basalt.yaml` 每次进程启动读一次（修改下一次进程启动生效；运行内快照属 US-4，不预置）；`init` 每次全量扫描（幂等）；WAL 库级持久一次设置 |
| 边界行为返回语义（技术方案未规定项） | **适用**：旧版本 / 损坏 / 库缺失的拒绝语义已定（§2.3 状态矩阵 + 约束 16）；剩余 → 「待决事项」#1 工作区路径键 / #2 Windows 锁手段 |
| 并发泄漏类回归 | **适用（初始化竞争）**：`TestBootstrap_ConcurrentInitRace` 兜底「先判态后写」竞态（约束 16）；`make test` 全量 `-race`；多连接 PRAGMA 逐连接生效由 `TestOpenWritable_PragmasOnEveryConnection` 反「只设第一条连接」经典坑 |
| 留痕顺序 | **不适用**（无审计两阶段写；表建齐、写入自 US-2 起） |
| 配置占位符解析语义 | **适用**：§2.2 约束 7 + §2.3 流程 C——变量**不存在**（lookupEnv 判存在性，空串替换绕不过）点名键路径与变量名；凭证键解析为空即拒；错误与日志不回显凭证值；由 `TestConfig_RejectsMissingEnvVar` / `_RejectsEmptyCredential` / `_RejectsPlaintextCredential` 判卷 |
| LLM 响应字段可空性 | **不适用**（无 LLM 调用，US-2 起） |
| 失败信息回填闭环 | **不适用**（无工具循环，US-4 起） |
| 依赖初始化副作用 | **适用**：`import _` 两处（modernc 驱动注册 / tzdata 内嵌）见 §3.5，兜底生效性由 §4.3 隔离组 + 对照用例（CI 必跑步骤）判卷；坐标先核实纪律见 §2.2 约束 5 |
| 可观测性双轨 | **适用**（单轨落地）：日志轨本 US 落地（dev / prod 双档；失败路径 ERROR / WARN + exit 1）；审计轨 = 表就位、写入未启用 |
