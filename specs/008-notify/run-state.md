# run-state — US-8 通知工具（feature 008-notify）

> **性质声明（三声明，本文件常驻头部）**
>
> 1. 本文件是**过程状态，非设计工件**——speckit 命令**不得**把它当设计输入；
> 2. 失败台账**只追加**：一行一次尝试，不改旧行（可 diff 审计）；
> 3. 唯一允许原地翻转的是「未验证清单」的「未验证 → 已验证」，翻转必须注日期与证据。
>
> 本文件**入库 git、收口不删**——它是长任务续跑与验收可信度的过程载体，**不是**探针 / 诊断临时物（「临时物收口即删」惯例**不适用**）。

## 1. 基线

**写入时点**：步骤 1（2026-10-18；步骤 0 续跑分流 = glob `specs/008-*` 零命中 → 正常走步骤 1~7）。

| 项 | 值 |
| --- | --- |
| US | US-8（通知工具）——台账 `docs/us/REGISTRY.md` 第 8 行，类型：开发类 → 走完整流程步骤 1~7 |
| feature 目录 | `specs/008-notify/`（步骤 0 前 glob `specs/008-*` 零命中；`001-foundation` ~ `007-mcp` 为历史只读设计） |
| 分支 | `008-notify`（2026-10-18 步骤 1.3 手动建；仓库未登记 `before_specify` hook） |
| 起始 commit SHA | `4c6f05643c9f07535f6d946c577a8605b652544b`（短：`4c6f056`，`chore: restore specs and .specify to public tracking`） |
| worktree | **无**——单工作区 `D:/code/basalt`，无并行 US 交错 |

### 1.1 开跑时已有未提交改动（**保留，不得 stash / checkout 覆盖**）

`git status --short` 为空（工作树干净）。以下**私有路径**不在 git 跟踪 / 排除面内（见 `.git/info/exclude`），属本 US 开工前的既有改动，按文件快照核对；收口「变更总结」不纳入本轮判据：

| # | 路径 | 状态 | 归属 |
| --- | --- | --- | --- |
| 1 | `docs/us/us8-notify.md` | 新增（未入库，`docs/us/` 在排除列表） | US-8 颗粒度文档 R2 定稿 |
| 2 | `docs/us/REGISTRY.md` | 修改（未入库） | 第 8 行登记（`/us-register 8`，2026-10-18；只追加） |
| 3 | `docs/TechnicalDesign.md` | 修改（未入库）→ §6.4 webhook 表述 | 人工 review ① H1 裁决的外部文档同步（R2） |
| 4 | `docs/Requirements.md` | 修改（未入库）→ §5.1 webhook 表述 | 同上 |

### 1.2 本 US 允许触碰的文件范围（颗粒度文档 §3 交付物清单）

#### 代码（新增 6 + 修改 3，共 9 文件）

| 类 | 路径 |
| --- | --- |
| 新增 | `internal/core/envref.go`、`internal/core/notify.go`、`internal/storage/notify_store.go`、`internal/tool/notify.go`、`internal/tool/notify_adapters.go`、`internal/app/notify.go` |
| 修改 | `internal/app/wire.go`、`internal/workspace/mcp.go`、`internal/cli/config.go` |

#### 测试（新增 5 + 修改 1，共 6 文件；42 条 = 41 新增 + 1 修改）

| 类 | 路径（条数） |
| --- | --- |
| 新增（5 文件 41 条） | `internal/core/envref_test.go`（1）、`internal/storage/notify_store_test.go`（4）、`internal/tool/notify_test.go`（21）、`internal/tool/notify_adapters_test.go`（13）、`internal/app/notify_test.go`（2） |
| 修改（1 文件 1 条） | `internal/app/ports_test.go`（恰好八件 → 恰好九件，**判据面变更**，人工 review ① 已批准） |

#### 流程工件（允许，非产品交付）

- `specs/008-notify/**`（`spec.md` / `plan.md` / `research.md` / `data-model.md` / `quickstart.md` / `tasks.md` / `checklists/` / 本文件）
- `harness/stages/us8-notify-r1.json`（stage 配置，从 `harness/stage.example.json` 派生）
- `docs/us/REGISTRY.md`（本 US 行已登记；不再改历史行）

#### 零改动核对面（列出为核对任务，非改动任务）

`go.mod` / `go.sum`、`internal/storage/schema.sql` / `docs/contracts/schema.sql` / 迁移脚本、`basalt.yaml` / `basalt.yaml.example`、`Makefile`、`scripts/**`、`.github/**`、`docs/contracts/**` 正文、审计写入路径（`internal/core/tool_exec.go` 语义零改动）。

#### 范围外

- 四份权威文档正文（R2 已完成的 webhook 表述同步除外——人工 review ① 直接裁决、已发生，不在本轮增量内）
- `.specify/memory/constitution.md`、`docs/us/*` 其他 US 文档、`specs/007-*` 及更早 feature 目录

### 1.3 已知判据面变更（开工前登记清单，来源：颗粒度文档 §3.5 前序改造点）

| # | 文件 | 变更 | 归属 |
| --- | --- | --- | --- |
| 1 | `internal/app/ports_test.go` | 恰好八件 → **恰好九件**；用例改名 `TestToolListerPort_HeavyAssemblyRegistersNineTools`；名集与可解析循环加入 `notify` | **判据面变更**——人工 review ① 于 2026-10-18 批准（保留精确集合比较与实体可解析检查；集合语义不变，只随第 9 件更新） |
| 2 | `internal/core/envref.go`（新增）+ `internal/workspace/mcp.go` + `internal/cli/config.go` | `${VAR}` 整串判定收敛为 `core.EnvPlaceholderName` **唯一实现**，两处既有调用点改调它 | **等价重构，判据面零变更**；既有 `workspace/mcp_test.go` 边界表与 `cli/config_test.go` 凭证键用例**零改动**，新边界表落 `core/envref_test.go` |
| 3 | `internal/app/wire.go` | 注册第 9 件 `tool.NewNotifyTool`（门禁之前）+ `declaredToolNames` 九名 + 三处注释八件 → 九件 | 生产装配变更；由 R11 与 `ports_test.go` 覆盖 |

### 1.4 三件输入的 sha256 与 tasks 指纹

| 输入 | 路径 | sha256（现行） | 行数 | 写入时点 |
| --- | --- | --- | --- | --- |
| 颗粒度文档 | `docs/us/us8-notify.md` | `5f5890081c81be5cc806bffc2d0b37303122397aede5842f0d62fe9042d764c5` | 1382 | 2026-10-18 步骤 1 写入（R2 定稿） |
| 规格 | `specs/008-notify/spec.md` | `6a5cd9d8fdbcb9044c135bbf38358d4513332e4edb5eb773c5756f92e7b7f0f2` | 173 | 2026-10-18 步骤 2 生成、步骤 3 clarify 后终稿（零提问 + 5 条收敛项） |
| 计划 | `specs/008-notify/plan.md` | `e29ad8781ad40bd336481485232bed944f1ed6fd5576c20a28b90363f10f0f7c` | 155 | 2026-10-18 步骤 4 生成（Phase 0/1 工件：research.md `fb6920e4…` / data-model.md `9a2c3933…` / quickstart.md `42ad3c5a…`；无 `contracts/`，固定字面量引用 docs/contracts） |
| **tasks 指纹** | `specs/008-notify/tasks.md` | `2adc868a02c775aa5ba8b40b79ce50370717810c41bb1fe0de672cce5903bd93` | 172 | 2026-10-18 步骤 5 冻结（26 任务、全未勾选；勾选位归一后同值） |

**人工 review ① 记录**（步骤 1.1 依据；不编造批准）：

| 项 | 记录 |
| --- | --- |
| 所审版本 | R1 定稿，SHA-256 `8d65502e8c2746d61572338073cc75a038344eebce9e27803455dae6d421063b`（1301 行） |
| 决定（2026-10-18） | 主体设计保留；H1~H8（1 HIGH / 4 MEDIUM / 2 判别力补强 / 1 裁决）全部受理并落入 R2；三项待决全部裁决为方案 A（空 `content` **放行** / `${VAR}` **仅精确引用** / 配置装配失败归 `runtime_error`）；唯一判据面变更（八件 → 九件）**获批** |
| 当前操作版本 | R2，SHA-256 `5f5890081c81be5cc806bffc2d0b37303122397aede5842f0d62fe9042d764c5`（1382 行）；逐条处置见文档「人工 review ①」表与「已决事项 #21~#23」 |
| 外部文档同步 | `docs/TechnicalDesign.md` §6.4 与 `docs/Requirements.md` §5.1 的 `webhook` 表述已按 H1 同步（不改变契约字面量） |

### 1.5 验收映射（步骤 5 冻结后绑定实际 task ID；check 来源：颗粒度文档 §4.4）

| 项 | 值 |
| --- | --- |
| stage 路径 | `harness/stages/us8-notify-r1.json`（从 `harness/stage.example.json` 派生） |
| 专属 check | `008-envref`（120s）/ `008-executor-audit`（180s，含两个既有用例）/ `008-storage-notify`（180s）/ `008-tool-notify`（300s）/ `008-tool-notify-adapters`（300s）/ `008-app-notify`（300s）/ `008-ports-nine`（300s） |
| 最终验收 | `.\scripts\check-acceptance.ps1`（**无 `--task`**）+ 上表全部专属断言；公共全绿不替代专属断言，专属子集也不得替代公共门禁 |
| protect 路径 | `internal/app/ports_test.go`（恰好九件）、`internal/workspace/mcp_test.go`、`internal/cli/config_test.go`（既有占位判据） |

### 1.6 AI 任务比对与冻结（步骤 5，2026-10-18）

**比对对象**：`tasks.md`（26 任务）↔ 颗粒度文档 §3.1/§3.2（15 文件）/ §4.2（42 条）/ §4.4（7 专属 check）/ §6.1（人工项）。

| 比对项 | 结论 | 读数 |
| --- | --- | --- |
| 交付物文件覆盖 | **齐** | 15/15 路径在 tasks 中逐一出现（新增生产 6 / 修改生产 3 / 新增测试 5 / 修改测试 1） |
| 测试条数 | **齐** | 42 = N1~N21（21）+ A1~A13（13）+ S1~S4（4）+ P1~P2（2）+ envref（1）+ ports 修改（1）；任务落点 3+10+7+1+13+2+1+4+1 = 42 |
| harness 先行 | **满足** | 每个故事的测试准备任务均先于对应实现任务；跨故事已落地行为用 `-overlay` 逆改取红态（T012 / T015 / T021 DoD 已注明） |
| 关键回归映射 | **齐** | R1=T015/T018(A5)、R2=T015、R3=T012、R4=T015、R5=T012、R6=T018(A1/A2)+T009(N17)、R7=T018(A7~A11)、R8=T018(A3)、R9=T018(A6/A12)、R10=T021、R11=T019+T020、R12=T009(N19)、A4=T018、A10=T018 |
| 专属 check 绑定 | **齐** | `008-envref`=T003/T004；`008-executor-audit`=T011（既有两用例必跑不重造）；`008-storage-notify`=T006/T007；`008-tool-notify`=T009/T012/T015/T021；`008-tool-notify-adapters`=T018/T022；`008-app-notify`=T019/T023；`008-ports-nine`=T020/T023 |
| 前置 / 边界依据 | **可追溯** | 任务中的验收点逐条指回 §2.2 约束 1~24 / §4.3 / §6.2；范围边界取自 §5 |
| 多出项 / 发明项 | **无** | tasks 未引入 §3 之外的交付物；stage 与 run-state 属流程工件，已在 §1.2 允许面 |
| 判据面 | **仅 1 项已批准变更** | T020（`ports_test.go` 八件 → 九件，人工 review ① 已批准）；T008 为等价重构、判据面零变更；其余既有测试零改动 |
| 人审依据引用 | **已引用** | 模块卡 R2 sha256 `5f589008…`（§1.4）+ 人工 review ① 决定（§1.4 记录）；AI 比对不写成用户批准 |

**冻结**：`tasks.md` sha256 `2adc868a02c775aa5ba8b40b79ce50370717810c41bb1fe0de672cce5903bd93`（172 行，26 任务全未勾选，勾选位归一后同值）。通过后自动进入步骤 6（`/speckit-analyze`），不设 tasks 人工确认。

### 1.7 实施前 analyze 与派生工件修订（步骤 6，2026-10-18）

| 项 | 记录 |
| --- | --- |
| analyze 首轮 | CRITICAL 0 / HIGH 0 / MEDIUM 1 / LOW 4；31 项需求（FR-001~024、SC-001~007）全覆盖；十原则与三条架构约束无冲突；tasks ↔ 15 文件 / 42 条齐 |
| 修订 1 | `research.md` D8：执行器兜底 `internal_error` → `runtime_error`；补注 `unknown` 只由崩溃恢复写给半截 `pending` 行（依据：`docs/contracts/run-and-errors.md` §7.3 七值、`internal/core/tool_exec.go:221-246`） |
| 修订 2 | `data-model.md` §7 审计映射末行：`internal_error` → `runtime_error`，并注明契约无 `internal_error`；顺带修 MD032 两处（列表前补空行） |
| 修订后哈希 | `research.md` `687d5fc0e02445faa1e7444b4dd3bb7d3ae6fa4155df9f636aacaaa008cabb7b`（修订前 `fb6920e4…`）；`data-model.md` `523588dcd26a8c26daf0e65f5032d52d6b4f7404f1e9dc72dfbaac364f44d2d2`（修订前 `9a2c3933…`） |
| 三件输入 | 复核哈希未变：颗粒度文档 `5f589008…` / spec.md `6a5cd9d8…` / plan.md `e29ad878…`；tasks.md `2adc868a…` 未动——**未触碰冻结输入** |
| 保留项 | plan.md:155 中英混排（LOW，冻结输入不改）；tasks.md T011 的 GOPROXY 前缀、T018 的红态符号名（LOW，执行期按实际读数登记，不改冻结正文）；契约「20 KB」与 20 KiB 的对应已由颗粒度文档 §6.2 第 2 条裁决 |
| 复跑结论 | 修订后复跑 analyze：CRITICAL 0 / MEDIUM 0；I2/S1/S2/S3 保留为执行期说明，不阻断 implement |
| stage | `harness/stages/us8-notify-r1.json`（步骤 6.1 派生：7 专属 check + 具名 required_tests + inputs / protect；20 个任务具名绑定；未绑定 T001/T002/T005/T008/T025/T026——环境、零变更基线、`core` 值对象与收尾任务无专属断言，按 DoD 读数 + 完整 stage 验收）；用未知任务探针跑加载器：校验通过后报 `unknown stage task`、exit 2（符合预期，不启动任何检查） |

### 1.8 stage 任务绑定修订（实施期，2026-10-18）

| 项 | 记录 |
| --- | --- |
| 修订 | 删除 T009 / T010 / T012~T020 的任务绑定；保留 T004 / T007 / T011 / T021 / T022 / T023 / T024。7 个 check 的 ID、命令、`required_tests` 与 protect / inputs 均未变 |
| 依据 | 任务级 gate 只有在「该任务完成时 check 可整体通过」才有判别力；`008-tool-notify` 要求 N1~N21 全部具名通过，而 T009/T010 只落 3 条、T012/T013 增至 13 条、T015/T016 后 20 条、N14 在 T021 才落地。测试准备任务的证据形态是「逐字红态 + DoD」，不是整类绿 |
| 首跑记录 | T009 / T010 首跑 `008-tool-notify` 失败（`required test did not execute and pass: TestNotifyTool_MissingChannelIsNotFoundWithAvailableNames`）——控制侧绑定口径问题，非代码缺陷；台账已追加一行 |
| 覆盖保证 | 被解绑任务由逐任务 DoD 读数 + T021/T022/T023/T024 的整类 check + 最终完整 stage 覆盖 |

### 1.9 实施期进度与红态读数（2026-10-18，只追加）

#### 任务进度

| 阶段 | 任务 | 读数 |
| --- | --- | --- |
| 共享前置 | T001 / T002 | `go1.27.2 windows/amd64`；EngineeringStandards §1~§4 已读；五条 `git diff` 全空（`go.mod`/`go.sum`、两份 `schema.sql`、两份配置、`Makefile`/`.github/`/`scripts/`）；`internal/storage/migrations/` 仅 `.gitkeep`。research.md 写前核实第 10 项：`*int` 区分 `null` 已由 N18 实测 PASS，尾随垃圾待 A 系列实测 |
| Foundational | T003~T008 | envref 1 条、storage 4 条、既有 mcp/config 用例全绿；两处调用点收敛为 `core.EnvPlaceholderName`（等价重构，判据面零变更） |
| US1 | T009~T011 | N1~N3 绿；执行器既有两用例绿；`tool`/`core`/`storage` race 绿 |
| US2 | T012~T014 | N2~N18 落 13 条全绿；R3 / R5 overlay 红→绿；`tool`/`storage` race 绿 |
| US3 | T015~T017 | 7 条落齐；N12 自然红→绿；R1 / R2 / R4 overlay 红→绿；`tool`/`core` race 绿 |
| US4 测试准备 | T018 | 13 条落齐；逐字构建错误已登记（见下表）；转绿前提 = T022 |
| US4 测试准备 | T019 / T020 / T021 | 2 + 1 + 1 条落齐；构建期红态（`undefined: newNotifyAdapters`）与判别性红态（见下表 T019 / T020 / T021 行）已登记；转绿前提 = T023 |
| US4 实现 | T022 | 适配器全量实现；13/13 PASS；`go test ./internal/tool` ok 3.967s；race `./internal/tool ./internal/core` 绿；gofumpt 空 |
| US4 装配 | T023 | `newNotifyAdapters()` + wire.go 注册与九名集；P1 / P2 / NineTools 全 PASS；`go test ./internal/app` ok 4.080s；race `./internal/app ./internal/workspace ./internal/cli` 绿；gofumpt 空 |
| US4 验证 | T024 | 八条逆改红态齐全（见下表）；复原后 `go test -count=1 -v ./internal/tool ./internal/app`：**199 PASS / 0 FAIL / 0 SKIP**（tool ok 3.916s，app ok 3.665s） |
| 全量门禁 | T025 | `make check` exit 0：fmt（gofmt / gofumpt 干净）· lint（vet + staticcheck）· sec（gosec + govulncheck `No vulnerabilities found.`）· test（`-race -cover` 全包 ok）· test-integration（`-tags=integration` 全绿）· cover（全部达标：core 85.62% / tool 85.91% / storage 79.69% / app 85.91%）· build（`CGO_ENABLED=0` 产出 `bin/basalt`）；四包 race exit 0（core 2.101s / storage 9.040s / tool 16.170s / app 5.723s）；42 条具名 PASS = 1+4+21+13+2+1，本 US 相关 SKIP = 0（tool 包另有 3 条既有 symlink 相关 SKIP，属前序 US，非本 US） |
| 零变更面 | T026 | 四条 `git diff --stat`（`go.mod`/`go.sum`、两份 `schema.sql`、两份配置、`Makefile`/`.github/`/`scripts/`）全空；`core` 仅标准库；`tool` 对 app/cli/workspace/storage 零 import；抑制注释 grep 仅命中前序三处既有 `#nosec`（`internal/tool/mcp.go` / `runner_unix.go` / `runner_windows.go`，均为前序 US 经用户裁决在库记录），本 US 改动面零新增 |
| 收口 analyze | 复跑 | CRITICAL 0 / HIGH 1 / MEDIUM 3 / LOW 5；逐条处置见 §1.10；修正后 `go test -count=1 ./internal/app ./internal/tool` exit 0（3.737s / 3.636s）、gofumpt 空 |
| 收口独立复核 | 15 文件 | 通过（0 CRITICAL / 0 HIGH / 0 MEDIUM / 3 LOW 注释锚点，已修正）；未覆盖面见 §1.10 |
| 公共验收 | full（无 `--stage`） | exit 0：10 共享 check 全 passed；report `accept-20261018T003208.639479200`；`"status":"fail"` 计数 0 |
| 公共验收 | full + stage（修正前树） | exit 0：10 共享 + 7 专属 check 全 passed；report `accept-20261018T003538.634191300` |
| 公共验收 | final tree + stage（收口修正后） | exit 0：17/17 全 passed；report `accept-20261018T004343.706971700` |
| stage 绑定探针 | T003 / T006 / T004 | T003 / T006 → `unknown stage task` exit 2（解绑生效，红态任务不再被定向 gate）；T004 → `008-envref` passed exit 0 |

#### 红态读数

逆改探针建在仓库外 `/d/code/.basalt-probes`，用毕即删；下表为逐字失败文案摘录。

| 回归 | 测试 | 逆改 | 逐字失败（摘录） |
| --- | --- | --- | --- |
| R3 | N2 / N3 / N4 | 渠道缺失 / 未知静默回退首行（默认渠道） | N2 `Target error should be in err chain:`；N3 / N4 `Expected error with "file does not exist" in chain but got nil.` |
| R5 | N5 / N7 | 上限检查失效（`if false &&`） | `Should be true`（`core.IsInvalidInput` 不成立） |
| R5 边界 | N6 | `>` 改 `>=` | `Received unexpected error:`（限值被误判超限） |
| 注册表失败 | N15 | 去掉 `读取通知渠道注册表失败` 包装 | `"database is locked" does not contain "读取通知渠道注册表失败"` |
| 每次读表 | N16 | 首次读取后缓存 | `Not equal: … actual  : 1`（readCount 期望 2） |
| content 必填 | N18 | `null` 当空串 | 子测试 `content_为_null`：`Should be true` |
| 空串放行 | N21 | 空串拒绝 | `Received unexpected error:` |
| R4 | N10 / N11 / N20 | 环境未设置 / 空值判定整体移除（`if false`） | `"测试夹具只放行名单内目标：（目标无法解析）。…" does not contain "${OPS_HOOK}"`（三条同文案） |
| R1 | N12 | 脱敏改回透传（`return err`） | `"测试夹具只放行名单内目标：https://user:pass@hooks.example.com:8443/hook-token-must-not-leak?sign=hook-token-must-not-leak。…" should not contain "hook-token-must-not-leak"` |
| R2 | N13 | 策略判定移到 Send 之后 | `Not equal: expected: 1 actual  : 0`（发送当刻判定次数） |
| R4 | N9（N8 不触达该变异） | 占位判定改前缀匹配（`relaxedPlaceholderName`） | `${A}${B}` / `${A}x` / `${}` / `${1A}` 四个子测试 `… should not contain "<原值>"`；`$A` 与 N8 明文不含 `${` 前缀，该变异下保持绿 |
| US4 测试准备 | T018 A1~A13 | 适配器尚未实现（T022 前） | 构建失败：`internal\tool\notify_adapters_test.go:27:21: undefined: newWebhookNotifyAdapter`、`:70:9: undefined: NewWebhookNotifyAdapter`、`:86:14: undefined: NewFeishuNotifyAdapter`（共 11 处后 `too many errors`）；转绿前提 = T022 |
| T019 构建红 | P1 / P2 | T023 前 `newNotifyAdapters` 未实现 | `internal\app\notify_test.go:19:14: undefined: newNotifyAdapters`；转绿前提 = T023 |
| T019 判别 | P2 | `declaredToolNames` 移除 `notify` | `Should be true`；`轻路径与重路径共用同一份声明名集：notify 必须可注册：tool not registered: notify` |
| T020 判别 | NineTools | 删除 `NewNotifyTool(...)` 注册行 | `Received unexpected error: …`（wire_test.go:114）；`重路径装配必须成功（失败时先看日志里的 step 停在哪一步）` |
| R10 | N14 | 先解析 env 再查类型（T021 / T024 共用） | `Not equal: expected: 0 actual  : 1`；`类型未装配不得触达环境` |
| R6 | A2 | webhook 认一切状态码（`resp.StatusCode < 0`） | `Error Trace: notify_adapters_test.go:45`；`Messages: status=302` |
| R7 | A8 | 删除 `code != 0` 判定块 | `Error Trace: notify_adapters_test.go:153`（期望 `external_error`，实得 nil） |
| A10 | A10 | 删除 `len(raw) > maxNotifyReceiptBytes` 超限判定 | `Error Trace: notify_adapters_test.go:192`（超限回执被判成功） |
| R9 | A6 | 删除 Do 错误前的 `ctxPreferred` 判定 | `expected: *errors.errorString(…context canceled…)` / `actual: *core.ExternalError(…连接失败…)`；`裸哨兵：不得包装（ErrorIs 对包装错误仍为真，不具区分力）` |
| R8 | A3 | 构造器改用 `http.DefaultClient` | `Error Trace: notify_adapters_test.go:71`（重定向被跟随） |
| A4 | A4 | 构造器改用 `&http.Client{Transport: http.DefaultTransport}` | `Error Trace: notify_adapters_test.go:93`；`webhook: 不读 HTTP(S)_PROXY` |
| R11 | P1 | 映射插入第三键 `"sms"` | `Error Trace: internal/app/notify_test.go:20`；`类型 → 适配器映射恰好两键` |
| R11 值类型 | P1 | `internal/app/notify.go` 的 `webhook` 值改为 `tool.NewFeishuNotifyAdapter()` | `Object expected to be of type *tool.WebhookNotifyAdapter, but was *tool.FeishuNotifyAdapter`；`两键的值类型不得互换`（复原 sha `d254f762…`） |
| R10 文案 | N14 | `notifyTypeUnregisteredError.Error()` 去掉渠道名与类型 | `"通知渠道类型未装配（已装配类型：webhook）" does not contain "dingtalk"`（复原 sha `6bee4dfb…`） |

**T012 DoD 偏差（如实登记）**：T012 DoD 预期 N15 自然红；实际 T010 已按颗粒度文档评审发现 10 落地注册表读取失败包装，N15 首跑即绿。红态改由 overlay 逆改（去掉包装）取得，逐字文案见上表；判据面未变更。

**T021 / T024 逆改方法偏差（如实登记，2026-10-18）**：本机 `go test -overlay` 对**命中的替换项**不可用（环境缺陷，非本仓代码问题）。实测链路：盘符大小写不一致——Go 命令以 `D:\…` 读目录、加载器以 `d:\…` 打开同一文件；大写键快速报 `Open …: deleted in overlay`（`fsys.stat` 的前缀判定对卷名大小写不敏感），小写键精确命中后 `fsys.children()` 在 `ReadDir D:\code\basalt\internal\tool` 处无限产出同一条目（`GODEBUG=gofsystrace=1` 轨迹的最后一行为该 ReadDir；最小复现模块在盘符大小写一致时可正常工作，两条键并存则必挂）。据此改用**等价逆改**：快照三文件到仓库外 → 生成整文件突变体 → `cp` 覆盖 → 跑单个具名测试取逐字红态 → `cp` 快照还原 → sha256 与快照逐一相等（本轮 9 次变异 + 2 次 wire 变异全部 `restore OK`；wire.go 复原哈希 `8abb073c…`）。`-overlay` 路径本身不再使用。探针目录 `/d/code/.basalt-probes` 与仓库内 `.probe-tmp` 已于当日删除（`git status --short` 无残留）。

**T023 DoD grep 偏差（如实登记）**：DoD 写 `grep -n '"notify"' internal/app/wire.go` 应命中注册与名集两处；实际注册行是 `tool.NewNotifyTool(` 调用（无引号字面量），该 grep 只命中名集一处（line 420）。代码无缺陷，判定口径改为两条命令合并：`grep -n 'NewNotifyTool' internal/app/wire.go`（line 242 注册）+ 上述名集命中；stage 与收口报告按此口径记录。

#### 收口后 §6.1 AI 执行项读数（2026-10-18，只追加）

验证工作区建在仓库外 `D:\code\.basalt-verify-us8`（假模型 mock + `notify_channels` 一行 `ops`）；数值用例结束后行已还原为 `webhook`，工作区与 mock 用毕删除。

| # | 项 | 四态 | 本轮读数 |
| --- | --- | --- | --- |
| 8 | `tool list` | 通过 | 输出 `共 9 件工具`，含 `notify` 且描述非空（「…channel 必须是注册表里的渠道名，没有默认渠道」）；重路径装配日志同列九名（排序）。schema 非空由 N1 `TestNotifyTool_InputSchemaRequiresContentOnly` PASS 佐证 |
| 9 | Agent 声明 / 轻路径 | 通过 | `profile list` → `ops-agent：registered=true（provider local / 模型 stub）`；重路径 `chat` 对着 mock 连续两轮均完成「模型 → `notify` 工具 → 模型收尾」（`⚙ 调用工具 notify…`，mock 计数递增）——声明注册与可聊同证 |
| 10 | 域名白名单拒绝 | 通过 | `allowed_domains: [example.com]` + 新进程：`reason=sandbox_denied`，`detail="域名不在白名单：http://localhost。这是安全策略…加入配置键 http.allowed_domains 并重启服务。"`；mock 计数 4→4（零外呼）；文案只有 `scheme://host`，无端口 / userinfo / path / query。验证后配置还原 `[localhost]` |
| 11 | 环境负向 | 通过 | 不带 `BASALT_NOTIFY_OPS` 的新进程：`reason=runtime_error`，`detail="通知渠道 ops 引用的环境变量 ${BASALT_NOTIFY_OPS} 未设置或为空"`；mock 计数 4→4（零收到） |
| 12 | 四目标编译 + race | 通过 | `CGO_ENABLED=0` 四目标全部构建成功：linux/amd64 15,286,432 B、linux/arm64 14,483,616 B、windows/amd64 15,522,816 B、darwin/arm64 14,601,010 B；`-race` 单独命令 `go test -race -count=1 ./internal/tool ./internal/app` exit 0（16.276s / 5.584s） |
| 13 | 生效时机 | 通过（两半均实测） | ① 渠道行：单进程长会话内，第一轮 webhook 发送 mock 计数 3→4；`UPDATE notify_channels SET type='sms' WHERE name='ops'` 后同进程第二轮：计数 4→4，`reason=runtime_error`、`detail="通知渠道 ops 的类型 sms 未装配（已装配类型：feishu、webhook）"`——下一发即生效，无需重启。② 环境值：进程 A `BASALT_NOTIFY_OPS=http://localhost/hook` → 成功 4→5；新进程 B 改为 `http://localhost/hook2` → `reason=external_error`、`detail="HTTP 404：http://localhost"`，计数 5→5——新值随重启生效（同进程内进程环境不能被外部改写属 OS 语义，非代码缓存）。行已还原 `webhook` |
| 14 | 变更范围核对 | 通过 | 以开工基线（§1.1）为参照：`git status --short` 恰为 15 个交付文件（4 修改 + 11 新增）+ `harness/stages/` + `specs/008-notify/`；零变更面 `git diff --stat -- go.mod go.sum internal/storage/schema.sql docs/contracts/schema.sql basalt.yaml basalt.yaml.example Makefile scripts .github` 为空。§1.1 所列开工前私有 docs 改动不纳入本轮判卷 |

### 1.10 收口前 analyze 复跑与独立复核（2026-10-18，只追加）

**analyze 复跑**：独立上下文只读复跑，结论 CRITICAL 0 / HIGH 1 / MEDIUM 3 / LOW 5；交付物 15/15、测试 42/42、R 点齐、判据面仅 `ports_test.go` 八件→九件（已批准）。发现逐条处置（均在本轮完成，未扩大已审范围）：

| ID | 严重度 | 摘要 | 处置 |
| --- | --- | --- | --- |
| A-01 | HIGH | stage approval 把「人工 review ① 审阅 R1」写成「批准 R2」 | 已改 approval：写明所审 R1 sha `8d65502e…`、决定 H1~H8 落入 R2、三项裁决；注明 R2 `5f589008…` 为修订后操作版本、未另称经人审。实质裁决未改 |
| A-02 | MEDIUM | run-state §1.7 记 20 个任务绑定，最终 stage 经 §1.8 修订后为 9（本轮再收敛为 7）；§1.5 为冻结时点映射 | 以 §1.8 + 本节为最终口径：当前绑定 7 项（T004/T007/T011/T021/T022/T023/T024）；§1.5 / §1.7 保留为历史记录，不改写。复核命令：`grep -c '"id": "T0' harness/stages/us8-notify-r1.json` 与 stage `tasks` 数组逐项比对 |
| A-03 | MEDIUM | T003 / T006 是测试准备（红态）任务，绑到要求 PASS 的 check，`--task T003` 必然 exit 1 | 已从 stage `tasks` 移除 T003 / T006；保留转绿的 T004 / T007。**不采纳**「补绑 T013 / T016」：二者完成时 `008-tool-notify` 仍不整类通过（N8~N21 未落齐），绑定会重犯 §1.8 已裁决的绑定口径问题；其证据由逐任务 DoD 红态读数 + 最终整类 check 承担 |
| A-04 | MEDIUM | P1 只断键集，未落地冻结 R11 的两条值类型断言 | 已在 P1 补 `require.IsType` 两条（webhook / feishu 值类型不得互换）；未新增用例，42 条口径不变 |
| A-05 | LOW | A10 正文用常量构造，未像 N5 那样钉 `64*1024` | 不修改：冻结 A10 代码本身即为该形态；常量值已由 analyze 逐字核对为 `64 * 1024`，登记为可接受残余风险 |
| A-06 | LOW | N14 未像冻结 R10 那样断言文案含渠道名与类型 | 已把夹具改为 `ops` / `dingtalk` 并补两条 `Contains`，与冻结 R10 断言对齐 |
| A-07 | LOW | 模块卡 §4.2 小节标题写「20 条」，实为 N1~N21 共 21 条 | 属冻结输入正文笔误；**不改写冻结输入**（会改变 R2 sha 与已审依据）。以 21 条与总数 42 为准，此差异在人工 review ② 收口包中如实列出，待用户裁决是否修订 |
| A-08 | LOW | run-state 把 tasks 指纹行数记为 172 行，实际 222 行 | 勘误：指纹 sha `2adc868a…` 本身经勾选位归一复核一致；行数读数以 `wc -l specs/008-notify/tasks.md` = 222（非空 141）为准 |
| A-09 | LOW | P1 / P2 / N14 注释约束锚点错 | 已按模块卡 §3.2 修正为 P1=约束 7 / P2=约束 23 / N14=约束 8 |

**独立复核**（独立上下文，未读 run-state；输入 = 冻结规格 + 判据面清单）：结论**通过**，0 CRITICAL / 0 HIGH / 0 MEDIUM / 3 LOW，全部为注释锚点错误（与 A-09 同源），已修正；未覆盖面清单（真凭证人工项 / 全量门禁未由复核者复跑）已在报告中列出，由本轮公共验收承担。

**修正后复验**：`go test -count=1 ./internal/app ./internal/tool` exit 0（3.737s / 3.636s）；`gofumpt -l` 两文件空输出；stage JSON 经 `ConvertFrom-Json` 解析通过。

### 1.11 人工 review ② 反馈处置（2026-10-18，只追加）

**人工 review ② 结论**：代码与自动化部分建议通过；完整 US 收口仍需四项真实凭证验收；未发现阻断性生产代码问题。逐项处置：

| # | 反馈 | 处置 | 证据 |
| --- | --- | --- | --- |
| 1 | 收口包装配顺序表述写反 | **仅表述勘误，代码正确**：实际顺序 = `notify` 注册（等值门禁之前）→ 等值门禁 → MCP 注册（门禁之后）。生产代码零改动 | `internal/app/wire.go` 注册行 242、门禁在注册块之后、MCP 块在门禁之后 |
| 2 | A-05 建议补字面量断言 | A10 用例首行新增 `require.Equal(t, 64*1024, maxNotifyReceiptBytes, …)`；red state 实测：常量逆改为 `32*1024` 时 `expected: 65536 actual: 32768`（`notify_adapters_test.go:189`），复原 sha `f699c4d2…` | 单测绿；`internal/tool` 全包绿 |
| 3 | A-07 按纯文档勘误处理 | 模块卡 §4.2「20 条」→「21 条」；修订前 sha `5f589008…`（R2 原批准版本，保留于 §1.4 与 stage approval），修订后 sha `a96ef287…`；不重走 review ①，不覆盖历史哈希 | `docs/us/us8-notify.md:504`；`sha256sum` 前后值 |
| 4 | 补 `channel:null` 边界用例 | 保持既有语义（null → 空串 → 缺失渠道 `not_found`），**不改行为**；`decodeNotifyInput` 注释明确 null 例外；新增 `TestNotifyTool_NullChannelTreatedAsMissing`（not_found + 可用名 + 零判定零发送） | `go test -run '^TestNotifyTool_'`：22 PASS / 0 SKIP / 0 FAIL |
| 5 | 真实 webhook 验证 | 用用户提供的真实飞书机器人地址（仅经 `BASALT_NOTIFY_REAL` 进程环境传递，未落盘）跑通真实投递：audit 行 `tool=notify` / `status=success` / `error_kind=NULL`，result `{"content":"已推送到通知渠道：ops"}`；DB 与工作区文本文件检索 token 片段零命中 | 2026-10-18 实测；工作区与 mock 用毕删除 |

**复验**：`make check` exit 0（tool 覆盖率 86.01%，其余包读数同 §1.9 T025）；完整验收 `full/us8-notify-r1` 17/17 passed，report `accept-20261018T014330.046164000`；`TestNotifyTool_` 22 条、适配器 13 条全 PASS、零 SKIP。

**计数口径**：本 US 用例由 42 增至 **43**（42 新增 + 1 修改），加两个既有执行器回归共 **45** 个具名测试；§1.9 T025 的「42 条」为当时读数，保留不改写。stage `required_tests` 不列新用例——它仍在 `^TestNotifyTool_` 过滤面内被实际执行并 PASS，stage 无需改动。

**人工项状态**：review ② 处置时 §4 第 11~14 行按指示保持「未验证」（mock / AI 实测不得替代人工确认）。**后续更新（2026-10-18）**：用户已在飞书群确认收到真实投递，§4 第 12 行（§6.1 第 5 项）翻转为**已验证**；第 11 / 13 / 14 行保持未验证。

## 2. 失败台账（只追加）

| 日期 | 问题（测试名 / 首错位置 + 症状） | 假设 | 结果 | 同题计数 |
| --- | --- | --- | --- | --- |
| 2026-10-18 | T009/T010 任务验收：`008-tool-notify` 报 `required test did not execute and pass: TestNotifyTool_MissingChannelIsNotFoundWithAvailableNames` | stage 把增量测试任务绑到整类 check（N1~N21），中间时点必然不整 | stage 绑定修订（§1.8）；check 与 required_tests 零改动；T009 3/3 测试已绿并登记 | 1 |

## 3. 停止预算与计数

- 同一问题修复 **3 次**不收敛 → 停下走 `/debug`（**不得**第 4 次盲修）
- `/debug` 内同一问题累计 **3 个**假设不成立 → 停下讨论设计 / 架构
- 每个 US 的 `/debug` 轮次预算 **≤ 2**
- **无进展**（相邻两次尝试无新读数 / 新假设 / 新定位）→ 立即停，不等计数
- 预算到期 = **停下报告请裁**（继续 / 降档 / 换方案）——不自动放弃
- **当前计数**：同题修复 0 次；`/debug` 轮次 0；未验证项见 §4

## 4. 未验证清单（四态：通过 / 失败 / 跳过 / 未验证；翻转须注日期与证据）

| # | 项 | 状态 | 证据 / 备注 |
| --- | --- | --- | --- |
| 1 | 专属 check `008-envref` | 已验证（2026-10-18） | `TestEnvPlaceholderName_ExactFormTable` PASS（T025 verbose 取证）；`envref_test.go` 1 条 |
| 2 | 专属 check `008-executor-audit` | 已验证（2026-10-18） | `TestToolExecutor_PreWriteFailureRefusesExecution` / `TestToolExecutor_TerminalWriteFailureKeepsPendingRow` 均 PASS（不重造） |
| 3 | 专属 check `008-storage-notify` | 已验证（2026-10-18） | S1~S4 共 4 条 PASS |
| 4 | 专属 check `008-tool-notify` | 已验证（2026-10-18） | N1~N21 共 21 条 PASS |
| 5 | 专属 check `008-tool-notify-adapters` | 已验证（2026-10-18） | A1~A13 共 13 条 PASS |
| 6 | 专属 check `008-app-notify` | 已验证（2026-10-18） | P1~P2 共 2 条 PASS |
| 7 | 专属 check `008-ports-nine` | 已验证（2026-10-18） | NineTools PASS；判据面变更经人工 review ① 批准 |
| 8 | 公共门禁七目标（`make check`） | 已验证（2026-10-18） | `make check` exit 0，七目标逐条读数见 §1.9 T025 行 |
| 9 | 公共验收 full（无 `--task`） | 已验证（2026-10-18） | full + stage（`us8-notify-r1`，无 `--task`）exit 0：10 共享 + 7 专属 check 全 passed；report `accept-20261018T004343.706971700`；另两轮读数见 §1.9 |
| 10 | §6.1 第 1~3 项（门禁读数 / 零迁移 / 零依赖） | 已验证（2026-10-18） | 门禁读数见 §1.9 T025；零迁移 / 零依赖见 §1.9 T026；人审证据在 review ② 提交 |
| 11 | §6.1 第 4 项（手工登记渠道） | 未验证 | **人工执行**；sqlite3 提示符内插入，`${VAR}` 不经 shell 展开 |
| 12 | §6.1 第 5 项（真 webhook 发一条） | 已验证（2026-10-18） | 真实飞书机器人投递补充实测：审计行 `tool=notify` / `status=success` / `error_kind=NULL`，结果 `已推送到通知渠道：ops`；用户于 2026-10-18 确认飞书群收到内容「Basalt US-8 真实投递验收：通知工具已连通」；真实地址仅经进程环境传递，DB / 文本文件零命中 |
| 13 | §6.1 第 6 项（飞书错关键词 `code=19024`） | 未验证 | **人工执行**；2026-10-18 仅完成协议探针（正确载荷 `code=0`、缺 content `code=19002`、空文本 `code=0`），工具判定面待实现后验收 |
| 14 | §6.1 第 7 项（凭证面 grep） | 未验证 | **人工执行**（凭证在手） |
| 15 | §6.1 第 8~14 项（`tool list` / Agent 声明 / 白名单拒 / 环境负向 / 四目标编译 / 生效时机 / 变更范围） | 已验证（2026-10-18） | AI 执行、读数见 §1.9「收口后 §6.1 AI 执行项读数」；第 13 项两半（渠道行下一发生效 / 环境值随重启生效）均实测 |

## 5. 收口结论

- **状态**：**人工 review ② 通过（2026-10-18）**：代码与自动化部分通过；真实飞书投递已由用户在群内确认收到，用户授权提交与推送。§6.1 第 4 / 6 / 7 项仍按未验证登记，不冒充通过。
- **AI 验收**：review ② 处置后最终树复验：`make check` exit 0；完整验收 `full/us8-notify-r1` 17/17 passed，report `accept-20261018T014330.046164000`；本 US 用例 43 条（42 新增 + 1 修改）+ 两个既有执行器回归 = 45 个具名测试，零 SKIP。
- **独立复核**：独立上下文复核通过（0 CRITICAL / 0 HIGH / 0 MEDIUM / 3 LOW，均为注释锚点并已修正）；发现清单逐条处置见 §1.10。
- **七项证据**：门禁 / harness 逐条 / 交付物 15 文件 / 前序回归 / 全局不变量 / 人工项清单 / 变更总结，见收口包。
- **剩余未验证**：§4 第 11 / 13 / 14 行（§6.1 第 4 / 6 / 7 项，**人工执行**）；第 12 行（§6.1 第 5 项）已由用户群内确认翻转；A-07 已按 review ② 裁决作为纯文档勘误处理（§1.11），不再挂起。
- **实际人工决定**：2026-10-18 review ② = 通过（真实投递已在飞书群确认收到；授权提交与推送）；一处表述勘误与三项低风险建议均已处置（§1.11）。
