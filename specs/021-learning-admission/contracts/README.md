# US21 契约材料

- 准入动作与目标类型的固定字面量：`docs/contracts/run-and-errors.md` §9.3（本次追加七条 `skill.admission.*` 与 `target_kind=skill_admission`）；恢复矩阵见同节 §9.4。
- 候选 / 评估 / 决策 / 发布记录的数据形状与端点：`docs/contracts/skill-admission.md`（本次新建）。
- 只读管理面端点：`docs/contracts/openapi.yaml` 的 `/admissions`、`/admissions/{id}` 与操作查询的 `target_kind` 枚举追加。
- 表结构：`docs/contracts/schema.pg.sql` 不改；本次新增表只在 `internal/storage/postgres/migrations/0010_skill_admission.sql` 追加，`docs/Compatibility.md` 记录 v10。
- 错误码与退出语义：沿用 `docs/contracts/run-and-errors.md` §2 / §9.5，不新增公开错误码；准入专有哨兵只在进程内使用，CLI 文本映射到既有 BAD_REQUEST / CONFLICT / NOT_FOUND 分类。

实现时逐条对照上列文件；契约与颗粒度文档冲突时以契约固定字面量为准，并在颗粒度文档记录差异裁决。
