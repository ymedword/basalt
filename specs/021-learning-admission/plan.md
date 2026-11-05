# Implementation Plan: 学习准入

**Branch**: `021-learning-admission` | **Date**: 2026-11-04 | **Spec**: [spec.md](spec.md)

## Summary

在既有技能发布通道之前加一段**冻结 → 评估 → 本机批准 → 受控发布 → 可撤回**的准入步骤：
候选内容与来源以只读目录冻结，七维确定性评估给出可复算证据，本机 CLI 主体以严格 JSON
决策批准（高风险要求提出者 ≠ 批准者、有效期 ≤ 24 小时），发布复用 US-12 的技能版本 /
指针 / 操作终态同一事务，撤回以上一版内容作为新版本重新发布。全部写动作先写 `pending`
管理审计；崩溃恢复只查证不重放。管理台只提供只读列表 / 详情，不新增第二条写路径。

## Technical Context

**Language/Version**: Go 1.27.3（toolchain 与 CI；`go 1.27` 为最低要求），单 module，发布 `CGO_ENABLED=0`。

**Primary Dependencies**: 沿用 `go.mod` / `go.sum`；标准库 `crypto/sha256`、`encoding/json`、`path/filepath`、`regexp` 与既有 cobra / goccy/go-yaml / pgx。零新增 Go 坐标、零新增 npm 坐标、零新增配置键。

**Storage**: PostgreSQL v9 → v10，只追加 `0010_skill_admission.sql`；`schema.pg.sql` 不改，SQLite 谱系冻结。

**Testing**: Go source / `-race` / PG integration（临时 schema）/ web 契约（POST 写方法非成功）/ console vitest；公共 Harness 引用 `harness/acceptance.json`，US 专属 stage `harness/stages/us21-base.json`。

**Target Platform**: Windows 11 + Git Bash 开发机；Linux/amd64 发布目标；不要求容器或外部网络。

**Performance Goals**: 无新增延迟 SLO；评估为纯内存计算，令牌化哈希与文件扫描按既有上限（200 文件 / 256 KiB 单文件 / 2 MiB 总量）有界。

**Constraints**: 不调真实模型、不发外部网络请求、不执行候选脚本、不做自动蒸馏、不做后台过期扫描或 GC、不做企业 SSO / RBAC、不新增 `use_skill` 或第二套发布 / 审计路径。

**Scale/Scope**: 单实例工作区；准入表按候选行增长（无自动清理）；列表默认上限沿用仓储 `Limit` 语义。

## Constitution Check

| 原则 | 本次落实 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct | 不触碰循环与工具调度；准入是管理面通道 | 通过 |
| II 客户端只做协议调用 | 不新增客户端；评估不调模型 | 通过 |
| III Provider 显式映射 | 不触碰映射表；准入只在发布事务后由既有基线绑定 | 通过 |
| IV 一目录一 Agent；技能声明式绑定 | 不改变 `AGENT.md` frontmatter 语义与渐进披露；准入只产出技能版本 | 通过 |
| V 审计 day-one fail-closed | 七个写动作全部 `BeginOperation` 先写 `pending`；失败补偿记 `failed` / `unknown` | 通过 |
| VI 授权与执行分离 | 复用 `core.Policy` / `CommandRunner`；评估不调用任何 runner | 通过 |
| VII 同步 + 有界后台例外 | 全部同步本机命令；不派生 Worker、不新增持久任务类型 | 通过 |
| VIII 单二进制 | 零新增 Go / npm 坐标；`CGO_ENABLED=0` 发布；console 仅新增只读页 | 通过 |
| IX 手写 SQL | 追加 `0010_skill_admission.sql`；已发布迁移不改写 | 通过 |
| X 五大能力优先 | 仅交付 US-21 已批范围；管理台写、企业治理、自动蒸馏明确不做 | 通过 |

依赖方向：`core` 只加纯类型与 JSON 解析；`workspace` 提供候选冻结读写；`postgres` 实现仓储与迁移；`app` 编排七动作、评估器与恢复；`cli` 定义 `AdmissionPort` 接口（装配层注入）；`web` 只依赖 core 与标准库。无新增包。

## Project Structure

```text
internal/core/admission.go, admission_json.go
internal/workspace/admission_store.go
internal/storage/postgres/admission_store.go, resource_store.go, connect.go, migrations/0010_skill_admission.sql
internal/app/admission_admin.go, admission_eval.go, admission_port.go, admission_secret.go, admission_refs.go
internal/app/baseline.go, wire.go, run.go, web.go, resource_admin.go
internal/cli/admission.go, ports.go, deps.go, root.go
internal/web/handlers_admissions.go, views.go, server.go, handlers_operations.go
assets/console/src/api.ts, router.ts, app.ts, views/operations.ts, views/admissions.ts, views/admission-detail.ts, tests/admissions.test.ts, dist/
internal/e2e/us21_admission_test.go
harness/stages/us21-base.json
docs/contracts/skill-admission.md, run-and-errors.md, openapi.yaml
docs/TechnicalDesign.md, docs/Requirements.md, docs/Operations.md, docs/Compatibility.md
```

收口注（2026-11-05，analyze F-02/F-03 处置）：上列文件清单按实现实测更新——只读路由落在
`server.go` 路由表而非 `admin_routes.go`；`internal/app/ports.go` 未改动，CLI 端口装配经
`internal/app/admission_port.go` 与 `run.go` / `web.go` 接线。T021 正文中的 `admin_routes.go`
属已勾选任务描述，不改写，差异在 acceptance.md 登记。

## Implementation sequence

1. **存储与契约先行**：`0010_skill_admission.sql` + `SupportedSchemaVersion = 10`；`core` 类型 / 哨兵 / 严格 JSON；`workspace` 候选目录读写。
2. **评估器**：`BuildAdmissionPolicy`（读 `basalt.yaml` 安全面含 `isolation.profiles.*` 十个执行边界字段——按 review ② R-03 裁决纳入，外加 DB `execution_policy` / `network_domain_policy`）、七维判定、`BuildAdmissionBaseline`、风险档、凭据扫描与引用扫描。
3. **编排**：`AdmissionAdmin` 七动作；发布路径复用 `ResourceAdmin.LockResource`、`workspace.WriteSkillWorkCopy` / `SnapshotSkill`、`publishTx` 扩展期望参数；撤回分类；恢复查证。
4. **CLI**：`basalt admission propose|list|show|evaluate|approve|reject|cancel|publish|withdraw`；写命令取实例锁；写前拒绝 `cli:unknown`。
5. **只读观测**：web 两个 GET 端点 + `target_kind=skill_admission`；console 两个页面与 dist 重建。
6. **文档与 Harness**：契约四件追加、技术方案 / 需求 / 运维 / 兼容增量、`harness/stages/us21-base.json`、`TestUS21_*` 测试。
7. **收尾**：`make check` + `make acceptance --stage us21-base`；analyze 复跑；独立复核；七项证据。

## Safety and lifecycle

- 候选冻结：`ValidateSkillDraft` → 只读目录 `candidates/<id>/{manifest,source,files/}`；读取时复算哈希，缺失 / 不符即拒绝，不静默重建。
- 评估：固定 `admission-eval-v1`；只产出状态与理由，不执行脚本、不发网络、不调模型；相同输入与时钟必须同结果。
- 批准：严格 JSON、64 KiB 上限、未知键拒绝、凭证扫描；只接受 `{effect,cost,regression}` 的未知确认；阻止维度、来源未确认、安全失败不可确认。
- 发布：单事务写版本行、指针、发布记录、候选状态与操作终态；`publishTx` 增加可选指针期望（版本 + 修订），任一步失败整体回滚并补偿工作副本 / 快照。
- 撤回：分类 `restored_previous` / `superseded` / `deleted`；只有指针仍指向本次发布且存在上一版时才移动指针（以上一版内容建新版本）；首版拒绝；不删除证据。
- 恢复：`VerifyOperation` 按动作查证三态，从不重放；依赖缺失判 `unknown`。
- 只读面：web 无写端点；console 无写控件、无 token 存储；`.basalt/admissions` 不在任何 Agent 白名单。

## Internal clarifications

以下由已审颗粒度文档确定，无需用户裁决；记录取舍依据：

1. **批准有效期**：只允许 `(now, now+24h]`，不引入「永久批准」。
2. **不新增配置键**：策略读取既有 `sandbox.cross_agent_read`、`shell.allowed_commands`、`shell.interpreters`、`execution.allow_run_code`、隔离装配与 DB 两表；哈希只覆盖生效面。
3. **发布期望参数**：`publishTx` 增加 `*pointerExpectation` 可选参数（既有调用传 nil），不另建指针写入 SQL。
4. **CI 无真实模型**：评估器与恢复测试全部使用固定时钟与本地夹具；不做效果 / 成本测量。
5. **高风险双 OS 用户**：单用户主机无法人工执行，保留 U3 为人工未验证项，由集成测试覆盖分离判定。
