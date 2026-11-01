# Implementation Plan: US-16 知识库、语义记忆与历史使用（B）

**Branch**: `016-knowledge-memory-history` | **Date**: 2026-10-28 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/016-knowledge-memory-history/spec.md`；设计事实源 `docs/us/us16-knowledge-memory-history.md`（review ① 2026-10-27 通过）

## Summary

把 Basalt 的运行期存储统一切换到 PostgreSQL + pgvector，并在其上交付四块业务能力（知识库、语义记忆、历史回查、会话压缩）与两块接入 / 观测能力（调用方身份与配额、诊断事件），同时提供既有 SQLite v6 实例的迁移与实例身份 / 主密钥交接路径。技术路径：新增 `internal/storage/postgres`（手写 SQL + 幂等建表 + 受控前向迁移）、`internal/dbmigrate`（导出 / 导入 / 校验 / 切换）、`internal/knowledge` / `internal/history` / `internal/compaction` / `internal/callers` / `internal/diagnostics` 五个领域包；`internal/app` 装配改为只构造 PG 仓储，`serve` 不再打开 SQLite；所有新增持久化经消费方窄端口，`core` 保持只依赖标准库。

## Technical Context

**Language/Version**: Go；构建版本由 `go.mod` 的 `toolchain go1.27.3` 与 CI `setup-go` 固定（`go` 行 `go 1.27` 只是最低要求；本机基础安装 1.27.2 **不是**构建版本，2026-10-27 实测 `bin/basalt.exe` stamp 为 `go1.27.3`）

**Primary Dependencies**: 既有栈（cobra / go-yaml / go-openai / modernc sqlite / cron / mcp go-sdk / fsnotify / slog / testify）+ **新增** `github.com/jackc/pgx/v5` v5.11.0（`database/sql` stdlib 模式，MIT，宪法架构约束·技术栈表已列）；`github.com/pgvector/pgvector-go` v0.4.1 虽在同表内，但**经 A6 决定不引入**——向量以字面量字符串经 `$1::vector` 传入（`internal/storage/postgres/knowledge_store.go` 的 `vectorLiteral`），`go.mod` 无该坐标

**Storage**: PostgreSQL 16/17 + `vector` 0.8.x（+ `pg_trgm`）为**唯一运行期后端**；SQLite 谱系冻结在 v6（33 张表），仅作迁移来源与回退快照；PostgreSQL 谱系自 v1（新增 19 张表 + 既有表演进）

**Testing**: `go test`（单元）/ `go test -tags=integration`（真 PG，独立 stage）/ `-tags=e2e`（进程级 Demo）/ `-race`（独立命令）；门禁 `make check` 七目标 + US 专属 stage `harness/stages/us16*.json`

**Target Platform**: Linux / Windows / macOS；单二进制 `CGO_ENABLED=0` 发布构建

**Project Type**: 单 Go module（`cmd/basalt` + `internal/` 分包）+ Vue 3 管理台（`assets/console`，构建产物入库）

**Performance Goals**: 以实验冻结——D-05（embedding 模型 / 维度 / recall@k / nDCG@10 / 延迟 / 费用）、D-06 / D-07（索引与中文关键词路径）、D-24（压缩参数）、D-27（迁移单事务规模）、D-29（等待上限）、D-30（三类入口 QPS / burst）；本计划**不预填数值**

**Constraints**: 启动 fail-closed（连接 / schema 版本 / 扩展能力任一不满足即拒绝启动）；审计两阶段（`pending` 预写）不可绕过；`context` 只承载取消 / 截止 / 追踪；手写 SQL 无 ORM；已发布迁移只追加；凭证 `${ENV_VAR}` 占位、DSN 不落盘不回显；时间一律 UTC

**Scale/Scope**: 7 个新增包、19 张新表（§3.4.1）、33 张既有表的迁移映射、管理台 5 个视图、**14 个 stage check**（`us16-unit` / `us16-compaction` / `us16-wiring` / `us16-pg` / `us16-migrate` / `us16-handoff` / `us16-assets` / `us16-migrate-drill` / `us16-handoff-drill` / `us16-legacy-upgrade-drill` / `us16-eval` / `us16-e2e` / `us16-console-build` / `us16-console-test`）+ 外部层级单列的 `us16-real`（`us16-external`，缺授权旗标退出 2）

> **计划期基线说明（2026-10-30 收口核对补记，不改上文）**：上文 14 个 check 是**计划期具名基线**，不是最终清单。实施期按任务逐个增补定向 check（每新增一个 check 必须带具名 `required_tests`、只增不删），至 2026-10-30 实测 `harness/stages/us16.json` 共 **78 个 check / 905 个唯一 `required_tests` 名称**；收口复核又追加窄 check `us16-workspace`（`tasks.md` T149），最终为 **79 个 check / 911 个唯一名称 / 23 个 inputs / 122 个 task 绑定**（2026-10-30 复核，附录 R143.4 披露）。check 的增删与理由逐轮记在 run-state（如 R20 的 `us16-compaction` 38→40、R25/R26 的 `us16-callers` 名单扩张）。**以 stage 文件与 run-state 为准**，本条计数不得用于判断覆盖面。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 / 架构约束 | 本计划的落实 | 判定 |
| --- | --- | --- |
| I 自实现 ReAct 循环 | 不引入 Agent 框架；压缩调用复用既有 Provider 路径，不自建循环 | 通过 |
| II LLM 客户端只做协议调用 | 摘要调用经 `ProviderService`；消息追加路径仍唯一（压缩摘要**不写回**消息表） | 通过 |
| III Provider 显式映射 | 摘要 / embedding 调用在 Run 准入时固化已发布映射，Run 内只查固定表 | 通过 |
| IV 一目录 = 一 Agent | 不新增 Agent 目录机制；技能绑定不变 | 通过 |
| V 审计 day-one 且 fail-closed | 摄取 / 检索 / Key 生命周期 / 迁移 / 压缩均先写 `pending`；诊断可丢弃但审计不可 | 通过 |
| VI 授权与执行分离 | 新增能力不绕过 `Policy` / `CommandRunner`；知识库不新增命令执行面 | 通过 |
| VII 同步执行 + context 只承载取消 / 截止 / 追踪 | `Principal`、Session、配额快照**显式传参**；不引入异步编排 | 通过 |
| VIII 单二进制 + 显式运行期依赖（v5.0.0） | `CGO_ENABLED=0`；启动 fail-closed 校验 PG；SQLite 仅迁移源；新增坐标在技术栈表内 | 通过 |
| IX 不用 ORM + 两谱系 | 手写 SQL；SQLite v6 冻结、PG 自 v1；迁移前向且 advisory lock 串行 | 通过 |
| X 五大能力优先 / 范围卡紧 | 本 US 属已登记第二阶段范围；企业 SSO / 多租户 / RBAC 不在范围内 | 通过 |
| 架构约束·技术栈 | 仅新增 `pgx/v5`（表内）；`pgvector-go` 表内但经 A6 不引入（`$1::vector` 字面量） | 通过 |
| 架构约束·依赖方向与包结构 | `core` 只依赖标准库；领域包只依赖 `core`；`storage/postgres` 实现消费方端口；`app` 唯一装配层 | 通过 |
| 架构约束·凭证与资源绑定 | 凭据只走 `${ENV_VAR}`；Key 只存指纹；主密钥不进备份包 / 日志 | 通过 |
| 架构约束·资源使用范围 | 配额与预算显式传递；后台任务绑定授权 Key | 通过 |
| 架构约束·资源身份与绑定权威 | 名册 / 实例身份为唯一权威；缺名册拒绝受控操作 | 通过 |

**Gate 结论**：无违反项，无需 Complexity Tracking 豁免。Phase 1 设计后复核同一张表（见文末）。

## Project Structure

### Documentation (this feature)

```text
specs/016-knowledge-memory-history/
├── plan.md              # 本文件
├── research.md          # Phase 0 输出（实验与冻结方法）
├── data-model.md        # Phase 1 输出（实体与关系）
├── quickstart.md        # Phase 1 输出（可执行验证指南）
├── contracts/           # Phase 1 输出（契约增量与权威指针）
├── checklists/          # 规格质量清单
└── tasks.md             # Phase 2 输出（/speckit-tasks，本命令不创建）
```

### Source Code (repository root)

```text
cmd/basalt/                     # 入口（不变）
internal/
├── core/                       # [扩展] Principal、Run 请求与冻结快照纯类型、窄端口声明
├── storage/
│   ├── (既有 SQLite 实现)       # [冻结] v6，仅迁移源
│   └── postgres/               # [新增] v1 幂等建表、受控迁移、仓储实现
├── dbmigrate/                  # [新增] 导出 / 导入 / 校验 / 切换 / 回退判据
├── knowledge/                  # [新增] 摄取 / 切分 / 代际 / 混合检索 / 引用 / 来源视图
├── history/                    # [新增] 关键词检索 / 精确读取 / 预算 / 权限
├── compaction/                 # [新增] 滚动摘要 / 切点 / 链 / 降级
├── callers/                    # [新增] 身份 / Key 生命周期 / QPS / 并发 / 配额
├── diagnostics/                # [新增] 诊断契约 v0 / 脱敏 / 记录
├── memory/                     # [扩展] 语义召回 / 六态 / 冻结 / 配额
├── web/                        # [扩展] 知识 / 历史 / 调用方 / 诊断端点与视图
├── app/                        # [扩展] PG 装配；serve 不打开 SQLite
├── cli/                        # [扩展] db / instance / secrets 子命令
├── e2e/                        # [扩展] us16_demo_test.go
└── (其余包不变)
assets/console/src/             # [扩展] 知识 / 调用方 / 历史 / 压缩 / 诊断视图；重建 dist
docs/contracts/                 # [修订] schema.pg.sql（新增）、openapi.yaml、run-and-errors.md
harness/stages/                 # [新增] us16*.json
```

**Structure Decision**: 单 Go module + `internal/` 分包（与 US-1～US-15 一致）。新增 7 个包按**有界上下文**划分，各自声明消费方窄端口，`storage/postgres` 统一实现；`dbmigrate` 只由 CLI 使用，不进入 `serve` 装配；`app` 仍是唯一装配层。

## Phase 0：研究与实验（详见 [research.md](research.md)）

| 事项 | 方法 | 产出 |
| --- | --- | --- |
| D-05 embedding 模型与维度 | ADR-06 固定查询集 + 候选 `text-embedding-v4`；记录数据集哈希 / recall@k / nDCG@10 / 延迟 / 费用 | 冻结模型与 `vector(n)` |
| D-06 索引选择 | 精确扫描基线 → HNSW；过滤召回不达标不启用 | 索引类型与参数 |
| D-07 中文关键词路径 | `simple` + `pg_trgm` 对照 | 分词 / 权重方案 |
| D-04 PG / pgvector 版本 | 已实测 PostgreSQL 17.11 / vector 0.8.7 / pg_trgm 1.6（HNSW + 迭代扫描可用） | 冻结最低版本与 CI 镜像 |
| pgx stdlib 用法 | 连接池 / 参数化 / 事务 / advisory lock / 时间与大整型映射 | 编码约定 |
| D-24 压缩参数 | 先定任务集与判据，再测触发比例 / 保留组数 / 摘要预算 | 参数表 |
| D-27 迁移规模 | 逐级放大演练，测行数 / dump 字节 / WAL 峰值 | 单事务上限 |
| D-29 等待上限 | 对齐一次 Run 的准入超时；实测 `pg_xact_status` | 秒数 |

**未实验前不得预填数值**；实验读数写入 `research.md` 与实施报告，缺测量就写「未测量」。

## Phase 1：设计产物

- [data-model.md](data-model.md)——19 张新表与既有表演进的实体、关系、状态机与校验规则
- [contracts/README.md](contracts/README.md)——契约增量清单与权威指针（权威仍是 `docs/contracts/`）
- [quickstart.md](quickstart.md)——可执行验证指南（本地 PG、迁移演练、Demo）

## Post-Design Constitution Re-check

Phase 1 设计完成后逐条复核上表：新增表与端口未突破「`core` 只依赖标准库」「领域包不反向依赖适配层」；契约增量未引入表外依赖；迁移脚本只追加。**结论：仍无违反项。**

## Complexity Tracking

无宪法违反项，无需豁免记录。以下为**已批准的设计复杂度**（非豁免）：

| 复杂点 | 为什么需要 | 更简单的替代为何不可行 |
| --- | --- | --- |
| 两套 schema 谱系 | 既有实例必须先导出再切换，且已发布迁移不得改写 | 单谱系就地升级会改写已发布迁移、破坏回退快照 |
| `dbmigrate` 独立包 | 导出 / 导入 / 校验 / 切换需要与运行期装配分离，且 `serve` 不得打开 SQLite | 放进 `app` 会让 `serve` 依赖 SQLite 路径，违反原则 VIII |
| 消费方窄端口 | 领域包不反向依赖适配层（原则 IX 澄清） | 直接依赖 `storage/postgres` 会造成领域 ↔ 适配层双向依赖 |
