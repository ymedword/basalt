# US21 实施研究

- Decision：候选目录 `.basalt/admissions/candidates/<id>/`，只读冻结。Rationale：与技能版本目录同仓工作区、可备份、不新增外部存储；Alternative：数据库存全文拒绝（无法逐字复算工作副本）。
- Decision：`publishTx` 增加可选指针期望（版本 + 修订）。Rationale：准入发布必须与批准时基线 CAS 绑定，重复实现一段 INSERT/UPDATE 等于第二套指针写路径；Alternative：发布前读后写无 CAS 拒绝（并发双赢）。
- Decision：策略哈希只覆盖生效面（跨 Agent 读、shell 三表、`allow_run_code`、隔离后端 / 工具 / 档位 / 限额、DB 两表行）。Rationale：绝对路径与代理会随主机漂移，纳入等于同一策略在不同主机哈希不同；Alternative：全量配置哈希拒绝。
- Decision：域名策略以 DB 行（scope / revision / 排序后域名）为哈希输入，YAML 仅作种子。Rationale：管理台改表后旧评估必须失效；Alternative：只哈希 YAML 拒绝。
- Decision：撤回以上一版内容新建版本，不移动指针到旧版本行。Rationale：指针只能前进；重放旧版本号会让版本号与时间倒退；Alternative：直接 CAS 回旧版本 ID 拒绝。
- Decision：`not_evaluable` 永不计为通过，聚合只有在全部 passed 时为 passed。Rationale：宪法 V 的 fail-closed 口径与「模型说完成只是提案」一致；Alternative：按权重折算拒绝。
- Decision：批准与发布都重读磁盘复算内容 / 来源哈希与保留期。Rationale：评估与批准之间可能被外部改写；Alternative：信任落库哈希拒绝。
- Decision：七个写动作在任何状态读取前校验 `cli:<OS user>` + `Method=cli` + `Source=cli`。Rationale：身份校验放后面会让不可信主体先拿到候选存在性；Alternative：读取后再拒拒绝。
- Decision：管理恢复对准入只查证 `operation_id` 关联行与状态 CAS 前后值。Rationale：重放发布会产生第二个版本；Alternative：按「最后一步重做」拒绝。
- Decision：管理台只读。Rationale：HTTP 面没有可信本机主体，写端点会绕开 CLI 身份约束；Alternative：管理台代执行拒绝。

实际 PostgreSQL 迁移前向、并发发布与恢复三态在实现阶段以集成用例取证，不把设计写成已实测。
