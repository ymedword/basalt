# US22 契约材料

本目录只做指针：权威契约在 `docs/contracts/`，实施时逐条对照；契约与颗粒度文档冲突时以契约固定字面量为准，并在颗粒度文档记录差异裁决。

- **新增** `docs/contracts/memory-learning.md`：只写本 US 的状态机、固定字面量、窗口与证据规则、确认处置、恢复矩阵与只读接口；不复制 US-16 / US-21 的既有契约。
- **追加** `docs/contracts/openapi.yaml`：`MemoryEntryView` / `MemoryEntryReceipt` 追加可空字段（`candidate` 元数据、`superseded_id`）；新增 `/api/v1/learning/*` 五个只读路径与其 schema；确认请求体改为可选；**不删改**任何既有字段。
- **修订 + 追加** `docs/contracts/run-and-errors.md`：
  - 修订 §2 正常收尾条文（现行第 189 行）：「终态与引用释放同事务」→「终态先落库 → 抽取 → 释放后置且可重入；释放失败不回滚终态，由启动 `releaseTerminalRefs` 补齐」；崩溃恢复关闭 `running` 行**仍保持同事务**。
  - 追加 `memory.confirm` 的 `detail_json` 可选键（`resolution` / `supersedes_id`，**不含正文**）与恢复表「学习抽取」一行。
  - **不新增动作字面量**；`memory.confirm` 沿用既有动作。
- **冻结不改** `docs/contracts/schema.pg.sql`：它是 v1 基线；v11 只由迁移表达。
- **新增** `internal/storage/postgres/migrations/0011_memory_learning.sql`：两条 CHECK 扩展 + 五张新表，只追加；`SupportedSchemaVersion` 10 → 11。
- **错误语义**：确认门禁失败全部映射既有 `409` 分类；自动候选遇到未装配 resolver 返回 `503`（**不得**退化到旧 `Confirm`）；学习只读端点不存在返回 `404`；学习路径的 `POST` / `PUT` / `PATCH` / `DELETE` 一律非成功。不新增公开错误码，专有哨兵（`ErrLearningNotFound` / `ErrLearningDraftNotFound` / `ErrLearningBudgetExhausted` / `ErrLearningExtractorUnavailable`）只在进程内使用。
- **配置契约**：只新增 `learning.auto_extract`（默认 `false`）与 `learning.agents`（默认空）；无环境变量、无热加载；启用但名单为空 / 重复 / 非法名 → 启动失败；名单内未知 Agent → WARN + 状态页、不抽取。

实现时逐条对照上列文件；`specs/022-*` 的 `data-model.md` 与 `quickstart.md` 只作设计说明与验证指南，不构成第二份契约。
