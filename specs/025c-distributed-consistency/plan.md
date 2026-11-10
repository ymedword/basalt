# Implementation Plan: 已发布资产自动分发与跨进程一致性（US-25C）

**Branch**: `025c-distributed-consistency` | **Date**: 2026-11-09 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/025c-distributed-consistency/spec.md`；模块卡 `docs/us/us25c-distributed-consistency.md`（1488 行，SHA-256 `9fb665e1…c3f1c`）。

## Summary

把 Compose 多 Worker 的「数据库权威」补成可用的一致性闭环。数据库新增内容寻址载荷表（迁移 0013）；发布改为先写载荷再移指针；各 Worker 的有界后台同步循环拉取、校验、安装已发布 bundle，物化技能到本机发布版本目录，按固定顺序刷新注册表与调度集合；调度到期扫描加就绪门、公平键集游标与每圈冻结边界；触发在领取事务内钉住 bundle 版本，Run 准入按领取记录与执行租约复核；策略按「实际应用的同一次快照」刷新并单独判定过期；技能发布联动重发依赖 Agent，控制面技能修复器有界自愈；资源准备先于 Entry 构建与注册，MCP 同版本同凭据零副作用复用；概览改为按进程实例的只读三态读数；Worker 只跳过共享初始化写、保留知识只读装配。最后更新门面并产出 v0.2.0 收口证据。

## Technical Context

**Language/Version**: Go 构建版本由 `go.mod` toolchain 指令与 CI setup-go 固定（当前 go1.27.3；`go` 行 `go 1.27` 仅为最低要求，本机基础安装 1.27.2 不是构建版本，2026-10-27 实测）。

**Primary Dependencies**: 单 Go module（`cmd/basalt` + `internal/` 分包）+ spf13/cobra v1.10.2 + goccy/go-yaml v1.19.2 + sashabaranov/go-openai v1.43.0（只做协议调用，禁自动工具循环）+ modernc.org/sqlite v1.60.1（仅迁移来源）+ `github.com/jackc/pgx/v5` v5.11.0（`database/sql` stdlib 模式，运行权威）+ robfig/cron/v3 v3.0.1（6 段制式）+ modelcontextprotocol/go-sdk v1.8.0 + fsnotify v1.10.1 + 标准库 net/http / ServeMux / log/slog + testify v1.12.1 + [测试专用] jsonschema v6.0.3。**本 US 零新增 Go / npm 坐标，`go.mod` / `go.sum` 与起点必须空 diff。**

**Storage**: PostgreSQL 16/17（运行权威，含 pgvector）；本 US 追加迁移 `0013_agent_bundle_payloads.sql`，`SupportedSchemaVersion = 13`；SQLite 源库保留 v6，不参与调度，不实现钉住领取；`schema.pg.sql` 不改写（D-25C-02）。

**Testing**: 四层——单元（内存夹具）、集成（本机 PostgreSQL，`dev-env/scripts/with-postgres.sh`，127.0.0.1:55432）、端到端（两个 `bin/basalt.exe` 进程 + 一个 PostgreSQL + 本地协议桩）、A+B 人工验收（真实实例，自动 CI 不调用真实模型）。公共清单引用 `harness/acceptance.json`，专属为 `us25c-base` / `us25c-sync` / `us25c-fault` / `us25c-docs` 四个 stage。

**Target Platform**: Linux 服务器（Compose 多 Worker）与 Windows 开发机（Git Bash）；发布产物 `CGO_ENABLED=0` 单二进制；`-race` 与发布构建分属不同命令 / job。

**Project Type**: 单 Go module 的服务器 / CLI 混合项目 + Vue 3 管理台（构建期工具链）+ VitePress 站点。

**Performance Goals**: 同步是**有界后台循环**——`assets.sync_interval`（默认 30s，`0` 仅在启动时同步一次）为发现检查频率，单轮总超时 20s，单轮尝试上限 50，载荷上限 16 MiB，全量兜底每 20 轮发起；调度扫描每圈查询次数上界 `ceil(N/n) + ceil(S/n) + 1`（正常时钟下）；概览单条 SQL、明细上限 200；策略刷新周期 30s、过期阈值 300s。上述均为**有界性**承诺，不是就绪时限承诺。

**Constraints**: 零新增依赖坐标；已发布迁移只追加；`context.WithValue` 只允许追踪标识；备份 / 分发介质为数据库载荷表，不引入对象存储；载荷不自动清理（D-25C-09），文档不得宣称存储有界；真实模型调用与真实 webhook 未授权。

**Scale/Scope**: 首发参考拓扑为 A（控制面 + PostgreSQL + Worker 1）与 B（Worker 2），9 个 Agent、44 个工具；验收夹具造 250 行登记、201 行同毫秒指针、2×`dispatchBatch` 每秒到期任务等界内规模。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 / 约束 | 本计划的落法 | 判定 |
| --- | --- | --- |
| I 自实现 ReAct 循环 | 不新增 Agent 框架坐标；同步循环不执行模型或工具 | 通过 |
| II LLM 客户端只做协议调用 | 本 US 不触碰 LLM 客户端与消息追加路径 | 通过 |
| III Provider 显式映射 | 资源准备只构建「已发布连接」候选目录与 Entry 判定快照，不改 Run 准入时固定的映射；发布失败保旧 | 通过 |
| IV 一目录一 Agent；技能声明式绑定 | 技能物化只写发布版本目录，绑定仍只由 `AGENT.md` `skills:` 声明；不新增 `use_skill`，不预载正文 | 通过 |
| V 审计 day-one 且 fail-closed | 发布沿用既有管理审计预写；同步循环不新建审计 / 消息 / 执行路径；概览只读；回填不伪造指针 | 通过 |
| VI 授权与执行分离 | 本 US 不触碰 `Policy.Check` / `CommandRunner.Run` 调用方 | 通过 |
| VII 默认同步 + 有界后台例外 | 新增的是**服务生命周期内的有界同步 / 刷新循环**（单轮超时、批量上限、停机 join、先 join 再关 PG），不派生第二套模型 / 工具执行、消息追加或审计路径；普通 Run 仍同步；副作用未知不自动重放；无 `context.WithValue` 业务态 | 通过 |
| VIII 单二进制与显式运行期依赖 | `CGO_ENABLED=0` 不变；PostgreSQL fail-closed 不变；零新增依赖 | 通过 |
| IX 不用 ORM，手写 SQL | 迁移 0013 只追加、幂等；仓储手写 SQL；无 ORM 坐标 | 通过 |
| X 五大能力优先，范围卡紧 | 全部交付物在已登记 US-25C 台账行与已审模块卡范围内；扩展阶段项不进入 | 通过 |
| 架构约束 · 依赖方向 | 端口在 `core`（消费方声明），实现在 `storage/postgres`，装配在 `app`；`core` 不反向依赖 | 通过 |
| 架构约束 · 凭证纪律 | 同步不读取明文凭据；MCP 只对 applied 连接做只读凭据解析与准备；凭据代次参与复用判定 | 通过 |
| 架构约束 · 资源范围与绑定权威 | 资源准备按既有资格过滤，不按同名补绑；Entry 只读本轮 prepared 快照 | 通过 |

无违例，Complexity Tracking 为空。Phase 1 设计完成后复核结论不变。

## Project Structure

### Documentation (this feature)

```text
specs/025c-distributed-consistency/
├── plan.md              # 本文件
├── research.md          # Phase 0：决策与替代方案
├── data-model.md        # Phase 1：实体、迁移 0013、配置键、状态机
├── quickstart.md        # Phase 1：定向验证与人工链路入口
├── contracts/README.md  # Phase 1：契约增量清单（固定字面量）
├── checklists/          # specify 阶段质量清单
├── spec.md
├── tasks.md             # Phase 2（/speckit-tasks）
└── run-state.md         # 过程状态（非设计工件）
```

### Source Code (repository root)

```text
internal/assets/            # EncodeBundle / DecodeBundle（确定性 tar）
internal/core/              # 端口与记录：载荷、发布游标、技能版本、SweepBounds / DueScanCursor、
                            # ClaimedBundlePin / ClaimedPinRecord / ErrClaimPinInvalid、
                            # WorkerOverview*、Scheduler.ReplaceEntries、DefinitionAuthority
internal/storage/postgres/  # migrations/0013、载荷仓储、AssetDistributionStore、
                            # ClaimDueTriggerPinned、StartRun 准入复核、ReadWorkerOverview、
                            # WorkerStore.Register 心跳间隔、SupportedSchemaVersion=13
internal/app/               # runtimeRole、AssetSync、materializeBundleSkills、PreparedResources、
                            # PolicyRefresher、SkillRepublisher、SkillRepairer、
                            # PinnedAgentSource / RunResources.ResolvePinned、BackfillBundlePayloads、
                            # 概览读取装配、publishAs / writePayload、wire.go 角色门
internal/cli/               # AssetsConfig / Config.Policy 解析、assets status
internal/web/               # POLICY_STALE、发布 publish 块、概览 Worker 字段与视图
assets/console/             # 发布状态与节点状态视图、overview-worker-labels.ts、dist 重建
internal/e2e/               # us25c_*.go（//go:build integration）双实例链路
harness/stages/             # us25c-base / us25c-sync / us25c-fault / us25c-docs
website/                    # 能力、范围注、导航、演示动画、双语图示
docs/                       # Operations、TechnicalDesign、Compatibility、contracts
```

**Structure Decision**: 沿用单 Go module + `internal/` 分包；同步编排放 `app`（装配层），端口在 `core`，SQL 在 `storage/postgres`，不新增包。管理台沿用 `assets/console` 既有视图与测试；站点沿用 `website/` 既有数据文件与检查器。

## Phase 0: Research

见 [research.md](research.md)：R1–R10 覆盖分发介质、发布顺序、同步工作集、钉住领取、扫描公平、策略刷新、技能联动、资源准备、概览读数与角色门。全部决策来自已审模块卡，不引入新选型。

## Phase 1: Design & Contracts

- 实体、迁移 0013、配置键与状态机见 [data-model.md](data-model.md)。
- 契约增量（错误码、OpenAPI 字段、迁移口径）见 [contracts/README.md](contracts/README.md)。
- 可执行验证入口见 [quickstart.md](quickstart.md)。

## Implementation Phases

| 阶段 | 内容 | 对应 FR |
| --- | --- | --- |
| A 分发底座 | 载荷编解码、迁移 0013、发布链、回填、AssetSync、技能物化、注册表 / 调度刷新、就绪门与钉住领取、钉住版本贯穿准入 | FR-25C-01～11 |
| B 一致性修复 | 角色门、合法协调写保留、生命周期索引、四类策略刷新、技能联动重发与修复、资源准备、MCP PrepareAtApplied | FR-25C-12～20 |
| C 真实链路 | 双实例 e2e、载荷 / 数据库故障恢复、未知调用不重放 | FR-25C-21～23 |
| D 门面 | 网站、README、演示动画、图示、导航、运维文档 | FR-25C-24～28 |
| E 门禁与证据 | 四个专属 stage、配置解析、v0.2.0 收口证据 | FR-25C-29 |
| F 节点可见性 | 概览三态读数、心跳间隔登记 | FR-25C-30～31 |

**实施顺序（用户裁决）**：先完成定向判据并稳定，冻结代码后再运行一次全量门禁；定向 stage 顺序 `us25c-base → us25c-sync → us25c-fault → us25c-docs`。

## Post-Design Constitution Re-check

设计与 Phase 1 工件未引入新依赖、新执行路径、新审计路径或新业务 `context.WithValue`；迁移只追加。结论与上表一致：**无违例**。
