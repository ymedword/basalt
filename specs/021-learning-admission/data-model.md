# US21 数据模型

四张表（`migrations/0010_skill_admission.sql`），全部以 `management_operations(operation_id)` 为审计外键：

- `skill_admission_candidates`：候选主键 `adm_…`；状态 `proposed/evaluated/approved/rejected/canceled/published/withdrawn` + 修订号 CAS；内容 / 来源哈希 64 hex；清单 / 来源 / 基线 JSON；提议主体固定 `cli:<user>`（CHECK 拒绝空、`cli:unknown` 与非 cli）；风险档；终态原因与操作号；活动同名候选的部分唯一索引。
- `skill_admission_evaluations`：追加行（seq identity），候选 + 修订 + 候选哈希绑定；评分器版本、策略哈希、七维 JSON、脚本清单、聚合、风险档、引用、基线、时间、操作号。
- `skill_admission_decisions`：决策行（seq identity）；批准要求批准范围 / 脚本审查 / 基线 / 有效期非空，拒绝要求全 NULL；部分唯一索引保证一个候选最多一条活跃批准；`superseded_at/by` 记录被替换。
- `skill_admission_publications`：候选主键一对一；版本行外键到 `resource_versions(id)`；状态 `active/withdrawn`；撤回时间、撤回操作号、原因分类与撤回版本；状态与原因一致性 CHECK。

写入纪律：每条写都在单事务里做领域行 + 状态 CAS + `finishOperationTx`；`CreateCandidate` 唯一冲突映射 `ErrAdmissionActiveCandidateExists`；`PublishAdmission` 只在候选 `state='approved' AND revision=?` 时推进；`CancelCandidate` 同时 supersede 活跃批准。

状态机：`proposed → evaluated → approved|rejected`；`approved → published|withdrawn(取消)|canceled`；`evaluated → canceled`；`rejected` 终态。过期不写数据库，由列表 / 详情按「approved 且活跃批准 expires_at <= now」派生展示。撤回分类：指针删除 → `deleted`；同一 UID 且指针仍指向本次版本 → `restored_previous`（上一版 = 同 UID 中 `version_no` 小者最大）；否则 `superseded`。

`.basalt/admissions/` 是工作区文件侧模型：`candidates/<id>/manifest.json`（含候选哈希、文件摘要）、`source.json`、`files/`（逐字冻结），目录与文件只读；读取时重新 `ValidateSkillDraft` 并复算哈希。
