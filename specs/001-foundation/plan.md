# Implementation Plan: US-1 工程地基

**Branch**: `001-foundation` | **Date**: 2026-10-15 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/001-foundation/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 1` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us1-foundation.md](../../docs/us/us1-foundation.md) §2 / §3 为准，冲突时以 [docs/TechnicalSolution.md](../../docs/TechnicalSolution.md) 为准。

## Summary

一次性装好 Basalt 的工程地基：单 Go module 骨架（`cmd/basalt` + `internal/` 十包）+ 一条聚合门禁 `make check`（七目标）+ `basalt init`（幂等工作区骨架 + 幂等引导建库）+ `basalt status`（无需配置的三节只读诊断）+ 持久化基座（写模式打开能力 / 只读校验打开 / 受控前向迁移机制，11 张表幂等建齐）+ 全仓样板（日志装配、配置装载 `${ENV_VAR}` 解析、测试基础设施、覆盖率判卷脚本与脚本自检）。

技术路线的两个要点：**写能力与命令准入分离**（`storage.OpenWritable` 是能力层，`Bootstrap` 是 init 的准入层，重路径持锁顺序归 US-4），以及**判态不早于写权**（创建 / 迁移的判定与写入在同一 `BEGIN IMMEDIATE` 事务内完成，并发竞争方在写锁释放后重判为 Kept）。

## Technical Context

**Language/Version**: Go 1.27.2 构建版本（`go.mod` 的 `go` 行写最低版本要求 `go 1.27`；准确构建版本由 `toolchain go1.27.2` 与 CI 的 `actions/setup-go` 共同固定——**不得**表述为「锁定 1.27.2」）

**Primary Dependencies**: `spf13/cobra` v1.10.2（CLI）/ `goccy/go-yaml` v1.19.2（配置与契约 YAML）/ `modernc.org/sqlite` v1.60.1（纯 Go SQLite 驱动，`import _` 注册）/ `stretchr/testify` v1.12.1（测试断言）；标准库 `log/slog`、`net/http` + `httptest`、`go/parser`、`fs`、`embed`。本 US **仅此四个新增坐标**——go-openai / cron / MCP SDK / fsnotify 随消费 US 引入，本 US 不预占。

**Storage**: SQLite 单文件库（`.basalt/basalt.db`）；WAL（库级持久）；读写连接分工（写池 max 1、读池若干）；`foreign_keys` 与 `busy_timeout(5000)` 逐物理连接经 DSN；11 张表由 `schema.sql` 幂等建齐，版本记录 `schema_version`（本 US 写入首行 = 1）。列 / 状态字面量的唯一事实源是 [docs/contracts/schema.sql](../../docs/contracts/schema.sql)。

**Testing**: `go test -race -cover ./...`（默认层）+ 独立 `go test -tags=integration ./...`（本 US 空跑通过即绿）+ `//go:build tzisolated` 隔离组（不入 `make check`，CI 必跑容器步骤）；testify 断言；`internal/testutil` 提供临时工作区与本地回环 HTTP 服务；`scripts/check-coverage.sh` 自检 6 类判据。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布产物四平台交叉编译（`CGO_ENABLED=0`）；`-race` 与发布构建分属不同命令 / job。

**Project Type**: CLI + 服务单二进制（核心阶段 CLI 面；web 面归 US-9）

**Performance Goals**: 无显式吞吐 / 延迟指标（地基类 feature）；判据是门禁一次全绿与三件可演示物就绪

**Constraints**: 三条包纪律（依赖方向、接口定义在消费方、第三方实现放消费方包内）+ `core` 只依赖标准库；覆盖率底线按包可执行判卷（`core` / `tool` / `memory` / `storage` ≥ 70%，其余 ≥ 50%，有语句无测试包计 0% 不豁免）；三条实测纪律（格式判卷先退出码后输出 / `-race` 与 `CGO_ENABLED=0` 分离 / build tag 单列）；凭证 `${ENV_VAR}` 占位、拒绝路径不回显值

**Scale/Scope**: 约 25~30 个文件（多数样板类）；11 张表；2 条命令 + 1 个全局选项；新增坐标 4 个

**环境实测读数（2026-10-15，本轮当场命令）**: `go version` → `go1.27.2 windows/amd64`；`make --version` → `GNU Make 4.4.1`；`gcc --version` → `gcc.exe (Rev5, MSYS2) 16.1.0`；`gofumpt --version` → `v0.12.0 (go1.27.2)`；`staticcheck -version` → `0.8.2`；`gosec -version` → `Version: dev`（该子命令**不打印版本号**，属已接受的兼容性噪声；实际版本本轮用 `go version -m` 回读确认为 `mod github.com/securego/gosec/v2 **v2.29.0**`——与 CI 钉的 `gosec@v2.29.0` 一致，"待复核"由此清除）；`govulncheck` 已就位；`go env GOPROXY` → `https://goproxy.cn,direct`。工具链齐备 → 本 feature 的 `go` / `make` 命令**可以实跑**，不登记为「未验证」。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

对照 [.specify/memory/constitution.md](../../.specify/memory/constitution.md) 十原则 + 三条架构约束 + 开发工作流（GATE 判定：通过 / 不适用 / 需说明）：

| # | 宪法条目 | 本 feature 关系 | GATE |
| --- | --- | --- | --- |
| I | 自实现 ReAct 循环 | 不适用——本 US 无循环体；`go.mod` 无 Agent 框架坐标（收尾 grep 复核） | 通过 |
| II | LLM 客户端只做协议调用 | 不适用——本 US 无 LLM 调用；消息仓储未落地（US-2 起） | 通过 |
| III | Provider 必须显式映射 | 不适用——provider 包本 US 仅 `doc.go` 占位（无语句，覆盖率跳过） | 通过 |
| IV | 一个目录 = 一个 Agent；技能声明式绑定 | 不适用——Agent 加载归 US-3；`workspace.Init` 只建空骨架，不建 `memory/agents/` | 通过 |
| V | 审计 day-one 且 fail-closed | **部分适用**：11 张表（含 `llm_calls` / `tool_invocations` / `runs`）随 `schema.sql` 建齐；本 US **不写审计**（写入自 US-2 / US-4 起）——表在、写入路径不在属预期 | 通过 |
| VI | 授权与执行分离（两接口） | 不适用——沙箱 / 工具归 US-4 / US-5 | 通过 |
| VII | 同步执行 + context 只承载取消 / 截止 / 追踪 | **适用**：`Config` / `DB` / `Deps`（含 `WorkDir`、`SupportedSchemaVersion`、`Logger`）一律显式类型化传递；本 US `context.WithValue` 零使用（收尾 grep 复核） | 通过 |
| VIII | 单二进制纪律 + 依赖最小化 | **适用**：发布构建 `CGO_ENABLED=0`；`-race` 与发布构建分属不同 job；新增坐标仅四个且全部在宪法架构约束技术栈表内（直接落地，不重复询问） | 通过 |
| IX | 不用 ORM，手写 SQL | **适用**：`go.mod` 零 ORM 坐标；建表与迁移只在 `schema.sql` + `schema_version`；首次全量建表与版本行同一事务、每版增量同事务；`schema_version` 零行 = 损坏态拒绝；目标版本缺迁移文件拒绝并点名 | 通过 |
| X | 五大能力优先，范围卡紧 | 通过——本 US 仅交付地基与两条命令，Out of Scope 十项归属明确（spec.md Out of Scope） | 通过 |

**架构约束**：依赖方向 `channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`app` 唯一装配层；`core` 只依赖标准库——本 US 建立首个依赖倒置样例（`core.SchemaIntrospector` 由 `cli` 消费、`storage` 实现），无环由 CI 用 `go list` 复核；技术栈表内坐标直接落地；凭证一律 `${ENV_VAR}` 占位、样例文件零明文。

**开发工作流**：门禁一条命令 `make check`（七目标）；覆盖率可执行判据由 `scripts/check-coverage.sh` 按包判失败（脚本自检 6 类判据逐条覆盖）；三条实测纪律写进 Makefile 判卷顺序与 CI job 分离；提交纪律（AI 不自动 commit / push）。

**plan review 必查（宪法 §开发工作流）逐条**：

| 必查项 | 本 plan 的答复 |
| --- | --- |
| 依赖方向无环 | `core` 只依赖标准库（哨兵错误 + 两接口）；`storage` / `workspace` 依赖 `core`；`cli` 依赖 `core` / `workspace` / `storage`；`app` 装配全部；`cmd/basalt` 只调 `app.Run`。无反向依赖（收尾以 `go list -deps` 复核） |
| 审计写入路径唯一 | 本 US 无审计写入；审计表随 `schema.sql` 建齐，写入路径唯一性（`core` 仓储接口 → `storage` 实现）在 US-2 / US-4 落地时判卷 |
| 无框架坐标 | `go.mod` 四个坐标零 Agent 框架（收尾 `grep -nE "eino\|langchaingo\|adk-go\|trpc-agent-go" go.mod` 零命中） |
| 无 `context.WithValue` 业务态 | 本 US `context.WithValue` 零使用（收尾 grep 零命中）；`Deps` 显式传递 |
| 单二进制构建通过 | `build` 目标 `CGO_ENABLED=0 go build -trimpath -ldflags ...`；`-race` 只在 `test` 目标 |

**Phase 1 后复评（post-design）**：设计产物（data-model / contracts / quickstart）完成后重跑本表——结论同前，无新增违规、无 Complexity Tracking 条目（见文末）。

## Project Structure

### Documentation (this feature)

```text
specs/001-foundation/
├── spec.md              # /speckit-specify 输出（需求规格）
├── plan.md              # 本文件（/speckit-plan 输出）
├── research.md          # Phase 0 输出（决策 + 待实测核实项）
├── data-model.md        # Phase 1 输出（实体与状态机）
├── quickstart.md        # Phase 1 输出（可执行验收步骤）
├── checklists/
│   └── requirements.md  # 需求质量清单（/speckit-specify 输出）
├── contracts/
│   ├── cli-contract.md  # 命令行面契约（两条命令 + --version、退出码、报告分节）
│   └── storage-api.md   # 存储能力面契约（导出签名 + 哨兵错误 + 不变量）
└── tasks.md             # /speckit-tasks 输出（下一步生成）
```

### Source Code (repository root)

```text
cmd/basalt/
├── main.go                  # 唯一 main：os.Exit(run(os.Args[1:])) + import _ "time/tzdata"
├── run.go                   # run(args []string) int 薄入口包装（可测）
├── run_test.go              # TestEntryRun_VersionFlag / _UnknownCommand
├── tzdata_test.go           # 默认层可移植加载检查
└── tzdata_isolated_test.go  # //go:build tzisolated（CI 容器步骤执行）

internal/
├── app/
│   ├── doc.go               # 包注释（中英并列；唯一装配层）
│   ├── run.go               # Run(ctx, args) int + 装配骨架（注入 cli 端口适配器）
│   ├── ports.go             # cli 端口的适配器：storageStore / workspacePort（含 typed-nil 判空）
│   ├── logger.go            # NewLogger(cfg) *slog.Logger（dev 文本 / prod JSON）
│   └── run_test.go
├── cli/
│   ├── doc.go               # 包注释（中英并列）
│   ├── root.go              # NewRootCommand(deps Deps) *cobra.Command；--version 全局选项
│   ├── init.go              # init 子命令（工作区骨架 + Bootstrap，经端口）
│   ├── status.go            # status 子命令（三节只读报告，经端口）
│   ├── config.go            # LoadConfig(path, lookupEnv) + Config + 凭证红线（类型关 → 形态关）
│   ├── deps.go              # Deps（WorkDir / SupportedSchemaVersion / Logger / Store / Workspace）
│   ├── ports.go             # Store / SchemaView / Workspace 三个端口（接口定义在消费方）
│   ├── ports_test.go        # 依赖方向可执行判据：go list 断言 cli 不 import storage / workspace
│   ├── config_test.go
│   ├── contracts_test.go    # 契约 YAML 解析
│   └── status_test.go
├── workspace/
│   ├── doc.go               # 包注释（中英并列）
│   ├── workspace.go         # Init(root) (InitReport, error)
│   ├── inspect.go           # Inspect(root) (Report, error)
│   ├── templates.go         # 5 个模板文件的字面量
│   ├── init_test.go
│   └── inspect_test.go
├── storage/
│   ├── doc.go               # 包注释（中英并列）
│   ├── db.go                # DB 类型 + OpenWritable / OpenReadOnly + SchemaVersion / TableNames / Close
│   ├── bootstrap.go         # Bootstrap(ctx, path, target) (created bool, err error)
│   ├── migrate.go           # Migrate(ctx, db, fsys, target) error
│   ├── pragmas.go           # DSN 装配（逐连接 PRAGMA）+ WAL（含回读判卷）
│   ├── schema.sql           # 契约文件的嵌入副本（go:embed；逐字节一致由单测强制）
│   ├── migrations/.gitkeep  # 目录就位（本 US 无增量文件）
│   ├── migrate_test.go
│   ├── openwritable_test.go
│   ├── bootstrap_test.go
│   ├── openreadonly_test.go
│   ├── pragmas_test.go      # 逐连接 PRAGMA 与 WAL 回读判据
│   ├── example_test.go      # 只读打开的 Example（可执行文档）
│   └── schema_contract_test.go
├── core/
│   ├── doc.go               # 包注释（中英并列）
│   ├── core.go              # SchemaIntrospector / MetricsRegistry 接口 + NoopMetrics
│   └── errors.go            # 五个哨兵错误
├── provider/doc.go          # 空包占位（无语句）
├── memory/doc.go            # 空包占位（无语句）
├── tool/doc.go              # 空包占位（无语句）
├── channel/doc.go           # 空包占位
├── web/doc.go               # 空包占位
└── testutil/
    ├── workspace.go         # TempWorkspace(t) string
    ├── httpserver.go        # NewHTTPServer(t, h) *httptest.Server
    └── testutil_test.go

scripts/
├── check-coverage.sh        # 按包阈值判失败（六类判据）
├── check-format.sh          # 格式判卷：先查退出码、再判输出（gofmt + gofumpt 各一遍）
├── check_coverage_test.go   # 脚本自检 6 用例（测试专用包，无语句 → 覆盖率跳过）
└── stmtcount/
    ├── main.go              # 薄 main
    ├── count.go             # go/parser 可执行语句计数
    └── count_test.go

Makefile                     # 七目标聚合（check = fmt lint sec test test-integration cover build）
go.mod / go.sum
.github/workflows/ci.yml     # 双 job（test / build）+ tzdata 隔离容器步骤 + Linux 产物静态断言（file / ldd）
basalt.yaml.example          # 样例配置（凭证一律 ${ENV_VAR}；仓库只含样例）
```

**Structure Decision**: 单 Go module + `internal/` 分包（无多模块、无 `src/` 层）；`cmd/basalt` 是唯一程序入口，`internal/app` 是唯一装配层，`testutil` 是测试专用包（被其它包在 `_test.go` 中导入）。与台账「模块落位（摘要）」列一致：根（`go.mod` / `Makefile` / CI / `scripts/`）、`cmd/basalt`、`internal/{app,cli,workspace,storage,core,testutil}`——另外五个包（`provider` / `memory` / `tool` / `channel` / `web`）本 US 仅 `doc.go` 占位（落位列未列，属骨架起点，非交付物清单外概念）。

**文件组织调整（相对本 plan 初稿的落地差异，逐条记理由）**：

| 调整 | 理由 |
| --- | --- |
| 新增 `internal/cli/ports.go` + `internal/app/ports.go`（+ `ports_test.go`） | **依赖方向裁决的落地**：`cli` 直接 import `storage` / `workspace` 尽管无编译环，仍属"依赖具体实现"，装配层失去替换与测试注入的接缝。改为**接口定义在消费方 `cli`、适配器放装配层 `app`**，并把"无编译环 ≠ 方向合规"写成可执行判据（`go list` 断言 cli 的导入集不含两个实现包）。初稿把 `storage` / `workspace` 直接列为 `cli` 的消费依赖，已被该裁决取代 |
| 每个 internal 包补 `doc.go` | 十包骨架（含 `app` / `cli` / `storage` / `workspace` / `core`）统一"包注释中英并列、中文在前"的落点；初稿只在空包列了 `doc.go` |
| `workspace.Inspect` 拆到 `inspect.go` | `Init` 与 `Inspect` 是两条独立路径（写 / 只读），分文件后各自测试同构（`init_test.go` / `inspect_test.go`） |
| `core` **无**测试文件（初稿列的 `core_test.go` / `errors_test.go` 未落地） | 本 US 的 `core` 只有接口声明 + 空实现方法体（零可执行语句），按覆盖率口径为 `[no statements]` 而跳过；为凑覆盖率写空转测试不符合"测试要判真实行为"的纪律。哨兵错误的**可判性**在消费侧实测：`storage` 用例以 `require.ErrorIs` 断言 `core.ErrSchemaOutdated` / `core.ErrDatabaseMissing` 等（`bootstrap_test.go` / `openreadonly_test.go`） |
| 新增 `scripts/check-format.sh`、`storage/pragmas_test.go`、`storage/example_test.go` | 格式判卷的"先查退出码、再判输出"逻辑落在脚本里（`Makefile` 目标薄调用，同一判据可被本地与 CI 共用）；PRAGMA 逐连接与 WAL 回读是实测暴露的回归面，单列测试文件；只读打开的 `Example` 兼作可执行文档 |

**标签口径（`[US1]` 不混淆）**：`tasks.md` 中的 `[US1]` / `[US2]` / `[US3]` 是 Spec-Kit 对**本 feature 内部**用户故事的编号（对应 [spec.md](./spec.md) 的三个 User Story），**不是**项目台账（[docs/us/REGISTRY.md](../../docs/us/REGISTRY.md)）里的 US-1 / US-2 / US-3——本 feature 的三个故事全部落在项目 **US-1（工程地基）** 一个模块内。标签按工具产出保留，不另造编号。

## Complexity Tracking

> 仅当 Constitution Check 存在需辩护的违规时填写。

无违规——本 feature 无 Constitution Check 不通过项，故无复杂度辩护条目。
