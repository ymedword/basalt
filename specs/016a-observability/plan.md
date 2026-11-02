# Implementation Plan: US-16A Agent 可观测与执行诊断

**Branch**: `016a-observability` | **Date**: 2026-11-01 | **Spec**: [spec.md](spec.md)

## Summary

在既有执行事实与诊断上增加管理只读投影、上下文证据、指标、告警、持久导出账本、OTLP 发送和安全清理。保持业务/审计权威与 fail-closed，诊断可丢弃。所有实现与测试在工作区保留，最终交用户 review ②，不提交、不推送。

## Technical Context

- Language/Version：Go 最低要求由 go.mod 的 go 1.27 声明，准确构建采用 toolchain go1.27.3；单 Go module。
- Primary Dependencies：复用现有 cobra、goccy/go-yaml、pgx/v5 database/sql、testify；Vue 3/Vite/TypeScript/Vitest 为构建期。OTLP 按 T003 spike 决定 B 官方 SDK 或 C 官方 protobuf 类型+有界发送，禁止猜版本、解析错误字符串计数和隐式全局初始化。
- Storage：PostgreSQL + pgvector 唯一运行权威；0001–0005 与 schema.pg.sql v1 冻结；追加 0006。SQLite 仅前序迁移来源。
- Testing：具名 go test -json -count=1、race、真实 PG integration、前端测试/build、真实回环 Collector、六独立场景、变异反例、完整公共验收与 e2e。
- Target Platform：Windows 与 Linux/WSL；CGO_ENABLED=0 发布构建与 race 分开。
- Performance Goals：有界查询/Top-N/分页/批次/租约/年龄/重试；容量与延迟只报告实测规模，不作生产承诺。
- Configuration：复用现有 YAML 加载与环境占位规则；新增键仅限已审 observability 块；首版不热加载，清理/导出默认关闭，告警确定性规则默认启用。
- Module proxy：显式 `GOPROXY=https://goproxy.cn`；版本先 go list -m -versions 核实。

## Constitution Check

1. 无 Agent 框架、无自动工具循环，现有统一执行链不变。
2. core 只依赖标准库；observability 依赖 core/diagnostics/标准库；PG 适配实现领域窄端口，app 唯一装配层。
3. 遥测 worker 仅做诊断/账本/受控发送，不调用业务执行、恢复或重放；父 context 取消可退出。
4. 审计 pending 预写失败零外部动作，诊断失败不传播到业务错误链。
5. 密钥不落盘/回显；新增网络只在已授权回环测试与官方下载范围，生产出口不开启。
6. 只追加迁移，不改历史 stage、报告、断言语义或阈值。
7. 新依赖由用户本次自主执行授权覆盖工作区验证；版本、许可、体积和依赖树留 review ② 追认。
8. 范围、验收和人工收口语义不缩减；未验证和跳过单独呈现。

## Project Structure

```text
specs/016a-observability/{spec,plan,research,data-model,quickstart,tasks,run-state}.md
specs/016a-observability/contracts/README.md
internal/observability/       anchor/evidence/facts/ports/timeline/context/overview
                             metrics/alerts/export/mapping/retention/redact
internal/storage/postgres/   observability 读适配、账本、扫描、清理、告警与 0006
internal/core/               request 关联、工具关联、装配候选决定与检索摘要类型
internal/knowledge/          候选排名/预算决定、真实检索摘要、摄取/切换证据
internal/diagnostics/        chunk_invalidated 与 tool_call_id allowlist
internal/app/                生产端补齐、装配、worker 生命周期
internal/cli/                observability YAML 解析、默认值和校验
internal/web/                管理读路由、handler、view、错误映射
assets/console/              API、只读可观测页面、测试、构建 dist
harness/stages/us16a.json     私有输入 local-only，禁止并入 current.json/CI
```

## Design Decisions

- ADR-01：时间线来源九种（七类权威执行事实、装配、诊断）；导出来源九种（八扫描源+metric_snapshot）。SourceKind 全集合十种，两个子集分别验证；不能用错误的“统一九种”计数删来源。
- ADR-02：先加载完整有界前缀，排序并分配显示号，再应用 sources/from/to/分页；会话逐运行编号，不能提前过滤改变编号。
- ADR-03：新 retrieval JSONB 可空，历史不回填。非 chat 的 budget 按已审空值语义写 NULL，因此 0006 对该列 DROP NOT NULL；不改 v1。读 DTO 保留缺失键的 presence，不能把未记录渲染为零。
- ADR-04：所有来源查询错误作为来源级 query_failed；Context 不吞诊断错误；Cleanup 失败返回实际错误并记录，不使用伪代码的忽略错误写法。
- ADR-05：OTLP 类型/传输实现放 app 内适配文件，保持 observability 无第三方导入；它只接受 SpanData/LogData/MetricData。B 必须有稳定逐批计数公开 API、三信号和禁内部重试；否则采用 C。
- ADR-06：导出扫描按终态时间/来源 ID 键集分页，页入队独立事务；所有页成功后 CAS 覆盖。重叠与对账不构成普遍至少一次承诺。
- ADR-07：发送前重读/重建并核对 state_hash，BeginBatch 先写 unknown/NULL，响应与 item 状态同事务落账；部分成功不可逐条归因，不自动重试已落账批次。
- ADR-08：指标快照为派生可丢弃遥测，但已入队快照不可重算冒充同哈希。持久化最小脱敏快照 payload，复用 export_items 的内部 JSONB 载体；此为已有 metrics 账本能力必需的内部实现补齐，列入最终追认。执行事实不复制正文，发送前仍重读事实。
- ADR-09：清理仅允许四类已审表组；只有 `exporting && policy.ExportSource` 时要求覆盖与行级 sent、无未决项；更早历史/未知/失败均保留。导出关闭时按保留策略删除并明示未导出丢失。DELETE 与 cleanup_runs 同事务。
- ADR-10：告警当前状态 inactive/pending/firing/unknown；resolved 只写转换。旧 firing 经 unknown 后不能伪称新 pending 或恢复；LoadStates从已有转换查询最近非unknown状态/时刻，作为不序列化的内部恢复元数据，规则类型与原因在读取层解释；不新增告警表或API字段。
- ADR-11：总览/指标/状态只接受固定窗口和有界 limit；时间线/上下文保持明确锚点；不新增无界事件列表。
- ADR-12：回交文件的完整文件哈希写入 run-state 与交付说明；文件自身若写哈希则明确覆盖范围排除该字段，避免自引用伪哈希。

## Contract Surface

所有路径挂现有 /api/v1 前缀与管理读 guard：

| 方法/相对路径 | 输入 | 输出 |
| --- | --- | --- |
| GET /observability/overview | from/to、agent、limit（有界） | 总览、异常组、可用性 |
| GET /observability/timeline | 六类主锚点恰一、from/to、sources、limit | Entries/Gaps/OrderScope/Truncated |
| GET /observability/context | run_id | 检索、轮次、证据档位、缺口 |
| GET /observability/metrics | window（5m/15m/1h/24h） | 指标样本与可用性 |
| GET /observability/alerts | 有界窗口 | 当前状态、转换与缺失态 |
| GET /observability/export | 无敏感参数 | 启用/队列/未知尝试/批次状态 |
| GET /observability/cleanup | 有界窗口 | 策略与清理账本 |

已审 handler/client 指定同名路径；不修改 67 条管理写路由。RunView.request_id 为可选加法；旧 /diagnostics 排序和 missing/sampled_out 互斥语义保持。

## Phases and Validation

T001–T003 完成输入、判据与 OTLP spike；T004–T009 完成关联/迁移/证据生产；T010–T014 完成读取/指标/告警；T015–T020 完成扫描/账本/发送/清理；T021–T025 完成装配/API/管理台/六场景与运维；T026–T028 完成 Collector/独立复核/全量验收与走查。

测试先于或伴随实现。有效行为红态、绿态和报告进入 run-state；空匹配/SKIP 不算通过。公共 make check 七目标与验收入口引用现有清单，不复制判据、不改阈值。最终执行期间源码稳定，报告绑定最后版本。
