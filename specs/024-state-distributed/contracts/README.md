# US24 契约材料

本目录只做指针：权威契约在 `docs/contracts/`，实施时逐条对照；契约与颗粒度文档冲突时以契约固定字面量为准，并在颗粒度文档记录差异裁决。

- **修订 + 追加** `docs/contracts/run-and-errors.md`：§3.2 / §3.4（归档顺序：先立准入屏障再发取消、再有界等待、超时才回滚）/ §4.3.1 / §4.3.2 表述更新；追加取消标志语义（`runs.cancel_requested` 由属主观察，非属主如实返回当前状态）与新增错误码（`AUTH_RATE_LIMITED` 之外的既有分类不变；新增 `SESSION_BUSY` 的跨 Worker 权威判定注记、`FLOW_ROOT_BUSY`、`ADMISSION_OUTCOME_UNKNOWN` 的进程内回查语义）；**只增不改既有分类与状态码**（Q18）。
- **修订 + 追加** `docs/contracts/flow.md`：新增两个判据 `Isolated`（根子树无仍有效 run 租约，只放行 `Recover`）与 `Joined`（根子树无 `prepared` / `running` attempt，只放行 `Advance`），写明 `interrupted` / `unknown` 属复用候选、**不**计入 `Joined`；新增第三个判据未决动作（子树无 `dispatching` / `unknown` 动作才放行 `Advance`，命中则 `Abandon` 逼出 `TookOver=true`）；领取 `TookOver` 分路与"接管先 `Recover` 再 `Advance`"；`ReconcileTask.Isolated` / `CancelTask.Isolated` 的收敛职责；`CreateRunForNode` 的父根归属与 `execution_epoch` / `task_revision` 校验。
- **修订 + 追加** `docs/contracts/agent-md.md`：生命周期索引代次单调守卫与准入拒绝规则（本地低于索引拒绝；高于索引不自动放行；同代次仅接受逐项内容一致的幂等写）。
- **追加** `docs/contracts/openapi.yaml`：运行视图只读字段（`worker_instance_id` / `ownership_epoch` / `asset_bundle_hash`）；6 个新业务码（`ASSET_UNAVAILABLE` 503、`ASSET_VERSION_MISMATCH` 409、`AGENT_LIFECYCLE_STALE` / `AGENT_LIFECYCLE_MISMATCH` 409、`FLOW_ROOT_BUSY` 409、`RESOURCE_VERSION_MISMATCH` 409 / 503 两形态）；**不删改**任何既有字段。
- **冻结不改** `docs/contracts/schema.pg.sql`：它是 v1 基线；v12 只由迁移 `0012` 表达；`0001`–`0011` 已发布迁移不改写。
- **新增** `internal/storage/postgres/migrations/0012_distributed_execution.sql`：三张新表 + 四组列 + 两个 `resource_kind` CHECK 扩展 + 三类回填，只追加；`SupportedSchemaVersion` 11 → 12。
- **错误语义**：`ErrLeaseHeld` → 409 `FLOW_ROOT_BUSY`（管理面 resume / revise 撞上执行者持有根租约，不重试、不降级）；`ErrLeaseLost` 在进程内终止执行，不映射为可重试；`ErrCallerConcurrencyLimited` → 429 `CALLER_CONCURRENCY_LIMITED`（哨兵别名，既有测试不变）；`ErrAdmissionOutcomeUnknown` 只在进程内使用，调用方按 `runID` 回查。不新增公开的"注入租约故障"管理 API。
- **配置契约**：新增 `worker`（`lease_ttl_seconds` 30 / `renew_interval_seconds` 10 / `reclaim_interval_seconds` 5 / `reclaim_batch` 32 / `heartbeat_interval_seconds` 10 / `max_inflight` 64 / `shutdown_grace_seconds` 45）、`scheduler`（`poll_interval_ms` 500 / `dispatch_batch` 32 / `missed_grace_seconds` 300）、`assets`（`bundle_root` 默认 `.basalt/assets`，不得为空）；装载期校验失败即启动失败，不静默取缺省；`assets.require_bundle` **已删除**（资产校验是无条件不变量，不提供绕过开关）。

实现时逐条对照上列文件；`specs/024-*` 的 `data-model.md` 与 `quickstart.md` 只作设计说明与验证指南，不构成第二份契约。
