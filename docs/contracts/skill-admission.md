# 技能学习准入契约（US-21）

状态：已批准范围的实施契约，人工收口 review ② 待完成。准入是**人在环上**的技能入库通道：本机 CLI 提议并冻结候选、确定性评估、可信本机批准，再复用既有技能发布事务落版本。技能库对 Agent 只读；`.basalt/admissions/` 不在任何 Agent 读写白名单；管理面没有写入口。真实模型、外部网络与候选脚本执行均未授权。

## 状态机

数据库只存七个状态；`expired` 是**派生展示态**，永不落库——`approved` 且活跃批准满足 `now >= expires_at` 时对外显示 `expired`，发布由应用层拒绝。判定只有一条口径（存储态 `approved` + 决策为 `approve` + 未被 supersede + 有效期非零 + `now >= expires_at`，含相等）：编排层实现为 `admissionEffectiveState`，只读面实现为 `admissionDisplayState`——`internal/web` 不得 import `internal/app`，故两侧各持一份同口径谓词，不共享代码；两侧各有派生态用例钉住（`TestUS21_State_ExpiredIsDerived` / `TestUS21_AdmissionList_DerivedExpired`），改单边而不改另一侧即用例红。

```text
propose ──▶ proposed
proposed   ──evaluate──▶ evaluated
evaluated  ──approve───▶ approved       evaluated / approved ──reject──▶ rejected
approved   ──publish───▶ published      published ──withdraw──▶ withdrawn
proposed / evaluated / approved ──cancel──▶ canceled
approved 且 now >= expires_at ──（派生，不落库）──▶ expired
evaluated / approved ──evaluate──▶ evaluated（新评估行；approved 上同一事务把旧批准置 superseded）
aggregate = failed ──▶ approve 拒绝（状态不变）
```

| 规则 | 约束 |
| --- | --- |
| 可评估状态 | `proposed` / `evaluated` / `approved`；每次评估新增一行并 `revision + 1`，旧评估保留 |
| `reject` | 只从 `evaluated` / `approved` 发起；决策行必须绑定评估行（`evaluation_id NOT NULL`）。`approved` 上的 reject 是"撤回批准"：同一事务把旧批准置 `superseded_at`，候选进入终态 `rejected` |
| `cancel` | 只从 `proposed` / `evaluated` / `approved`；同一事务 supersede 活跃批准 |
| 终态 | `rejected` / `canceled` / `withdrawn` 不再接受任何推进（含 `evaluate`） |
| 唯一性 | 同名活跃候选（`proposed` / `evaluated` / `approved`）至多一条；一个候选至多一次成功发布；一个候选至多一条活跃批准（`decision='approve' AND superseded_at IS NULL`） |
| 证据保留 | 撤回 / 拒绝 / 取消都不删评估、决策或发布记录；`withdraw_version_id = 0` 表示本次撤回没有移动发布指针 |

`list --state approved` 排除派生为 `expired` 的行——要的是"还能发布"的批准；过期行只在 `--state expired` 里出现。两者互斥，库内状态不变。

## 来源声明严格 JSON

`basalt admission propose --source-file` 读一份 ≤64 KiB 的严格 JSON：

| 键 | 必填 | 取值 / 上限 |
| --- | --- | --- |
| `kind` | 是 | `manual` / `run` / `import` / `model_proposal` |
| `ref` | 是 | 非空，≤500 字符 |
| `task_outcome` | 是 | `completed` / `failed` / `unknown` / `not_applicable` |
| `confirmation` | 是 | `direct` / `user_confirmed` / `model_inferred` / `relayed` |
| `authorization` | 是 | `declared` / `not_required` / `unknown` |
| `redaction` | 是 | `declared` / `not_needed` / `unknown` |
| `applicable` | 是 | 布尔；缺失不等于 `false` |
| `retention_until` | 否 | 空串或 RFC3339 |
| `counterexamples` / `evidence` / `limitations` | 否 | 各 ≤20 项，单项 ≤500 字符；缺省为 `[]` |
| `notes` | 否 | ≤2000 字符；详情出站时置空 |

严格解码只有 `decodeAdmissionJSONValue` 一个入口：非空、≤64 KiB、合法 UTF-8、顶层必须是 `{`、未知键即拒（`DisallowUnknownFields`）、尾随内容即拒。**不做** `${ENV_VAR}` 替换——占位符按字面保存，凭证扫描放行 `${...}`；正文命中凭证模式即拒绝，错误与日志不回显原文，只回路径。

## 决策文件严格 JSON

`approve` / `reject` 都读一份 ≤64 KiB 的严格 JSON（同一解码器；全部字段用指针，缺省与显式零值都判）。`show --decision-template` 输出预填模板，`reason` / `expires_at` / 审查证据故意留白，不填必然被拒。

| 键 | 必填 | 取值 / 约束 |
| --- | --- | --- |
| `candidate_id` | 是 | `adm_` + 26 位 Crockford |
| `candidate_revision` | 是 | 正整数；必须等于当前候选修订号 |
| `candidate_hash` | 是 | 64 位小写十六进制；必须等于重读磁盘复算值 |
| `evaluation_id` | 是 | `eval_` + 26 位 Crockford；必须是该候选最新评估 |
| `evaluation_hash` | 是 | 64 位小写十六进制；必须与最新评估行逐字相等 |
| `decision` | 是 | `approve` / `reject` |
| `reason` | 是 | 非空，≤2000 字符 |
| `acknowledged_dimensions` | 否 | 只允许七个维度名、不得重复；approve 时须**精确等于**可确认集合 |
| `approved_scope` | 仅 approve | `skill` 非空；`risk_tier` 为 `normal` / `high`；`files[]` 每项 `path` 非空、`sha256` 64 位小写十六进制、`size` 非负、路径不重复；`referrers[]` 规整后比对 |
| `script_review` | 仅 approve | `path` 非空且不重复，`evidence` 非空 ≤2000 字符；路径集合须与评估脚本清单**逐条相等**，不得少也不得多 |
| `baseline` | 仅 approve | `skill_state` 为 `present` / `deleted` / `absent`；`resource_uid` 空或 `rs_` 标识；`published_version_id` / `version_no` / `pointer_revision` 非负；`content_hash` / `referrers_hash` 空或 64 位小写十六进制；`referrers[]` 规整后比对。`canonical_json` **不是**输入——由 `AdmissionBaselineCanonical` 复算，防止决策文件自带"证据" |
| `expires_at` | 仅 approve | RFC3339，必须落在 `(now, now+24h]`；上限 24 小时，可更短 |

reject 携带 `approved_scope` / `baseline` / `script_review` 任一即拒绝。批准边界按固定顺序判定：确认集合精确匹配 → 阻止维度为空 → 脚本静态审查齐全。确认集合 = 可确认集合 `{effect, cost, regression}` 与本次 `not_evaluable` 集合的交集，必须逐项相等——少确认、多确认、确认一个 `passed` 维度都被拒。阻止维度 = 全部 `failed` + 不在可确认集合里的 `not_evaluable`；`permissions = not_evaluable`（候选含脚本）只能由脚本静态审查解锁。**`not_evaluable` 不等于通过**。高风险（含脚本，或更新绑定中的既有技能）要求 `approver_subject != proposer_subject`。

脚本判定只看路径、不读内容、不执行：扩展名命中固定集合（shell / 解释器 / 可执行 / 打包产物）按脚本处理；**没有扩展名的文件无法被证明不是脚本，一律按脚本处理**——同样进入 `scripts`、触发 `permissions = not_evaluable`、高风险与逐条静态审查。**文件名以点结尾（如 `scripts/collect.`）视同无扩展名**——实现里按扩展名分类必须以"去掉末尾点后是否仍有点"为准，不得依赖 `path.Ext` 的点号判定被末尾点短路。**路径段首尾空白一律拒绝**：给 `run.sh` 加一个尾随空格后，扩展名变成带空格的 `".sh "` 而不是 `".sh"`，不拒绝即可改名绕过审查与提出者 / 批准者分离；Win32 还会静默剥离尾随空格，使冻结清单与实际落盘名不一致。分类前再剥一次末尾空白是纵深防御，不替代路径拒绝。改文件名不能绕过审查或提出者 / 批准者分离。

## 动作与目标字面量

七个写动作的 `target_kind` 恒为 `skill_admission`，`target_id` 恒为 `candidate_id`，`detail_json` 恒为 `{}`。每个动作先写 `pending` 再变更；预写失败不得发出变更。

| action | 允许的起始状态 | `request_hash` 输入 |
| --- | --- | --- |
| `skill.admission.propose` | —（新建） | `skill_name`, `candidate_hash` |
| `skill.admission.evaluate` | proposed / evaluated / approved | `candidate_id`, `candidate_hash` |
| `skill.admission.approve` / `skill.admission.reject` | evaluated / approved | 决策文件原始字节的 SHA-256 |
| `skill.admission.cancel` | proposed / evaluated / approved | `candidate_id`, 修订号（十进制）, `reason` |
| `skill.admission.publish` | approved 且未过期 | `candidate_id`, `decision_id` |
| `skill.admission.withdraw` | published | `candidate_id`, 版本号 `version_id`（十进制）, `reason` |

`AdmissionRequestHash(parts...)` = 对 `json.Marshal([]string)` 取 SHA-256 小写十六进制。失败原文截断 ≤4 KB，且不得含凭证、候选正文或来源全文。管理操作审计与模型 / 工具调用审计互不混写：准入不产生 Run。

**发布与决策的绑定义务归属应用层**：存储层 `PublishAdmission` 在唯一事务内校验——候选状态 CAS（`state='approved' AND revision=?`）、候选无既有发布行、`decision_id` 属于同一候选、`req.Baseline` 非空、版本种类为技能且技能名与候选一致、版本号分配与 `storage_path` 约定一致；并沿用既有发布原语的基线指针 CAS 与资源种类 / 技能名一致性检查。它**不**校验该决策是 `approve`、未被 supersede、未过期，也**不**校验 `req.Baseline` 与决策行 `baseline_json` 一致——编排层 `Publish` 在预写审计前完成其中三项：`LatestDecision` 为 `approve`、决策哈希与评估哈希一致、未过期（`now >= expires_at` 即拒绝）。编排层**不单独校验 `SupersededAt`**：当前状态机保证 `state='approved'` 且最新决策为 `approve` 时该决策必未被 supersede——`evaluate` / `reject` / `cancel` 会推进候选状态并在同一事务把旧批准置 superseded，重新批准写入更新的决策行。基线方向：编排层按**当前库状态**重算基线并与决策行 `baseline_json` 比较（不一致 → `STALE_BASELINE`），把当前基线作为 `req.Baseline` 传入；存储的 `StaleBaseline` 来自指针 CAS 期望（`admissionBaselineExpectation`），**不是**与决策行比较。新增第二个发布调用方时**必须**自行补齐四项（决策为 `approve`、未被 supersede、未过期、基线绑定），否则可用 reject、已失效的批准或漂移的基线绑定发布。

## 哈希输入

哈希一律对 UTF-8 JSON 序列化结果取 SHA-256 小写十六进制；数组先排序或规整，同一输入两次读数逐字相同。

| 哈希 | 输入 |
| --- | --- |
| `candidate_hash` | 候选文件清单 `[{path, sha256, size}]` 按 `path` 升序序列化 |
| `source_hash` | `source.json` 原始字节的 SHA-256 |
| `policy_hash` | `cross_agent_read`、`allowed_commands`（配置允许命令 + 安装清单可执行名，合并排序去重）、`interpreters`、`allow_run_code`、`isolation_backend`、`isolation_tools`（map）、`isolation_profiles[{name,runtime,image,cpus,memory_mb,pids,tmpfs_mb,output_tmpfs_mb,max_output_bytes,timeout_seconds}]`（按 `name` 升序；同名档位的任一执行边界变化即改哈希）、`config_profile`、`execution_profile`、`execution_revision`、`domain_policies[{scope,revision,domains}]`、`network_exceptions[{kind,value}]` |
| `baseline` | `skill_state`、`resource_uid`、`published_version_id`、`version_no`、`content_hash`、`pointer_revision`、`referrers`（排序）、`referrers_hash` |
| `evaluation_hash` | `evaluation_id`、`candidate_id`、`candidate_revision`、`candidate_hash`、`grader_version`、`policy_hash`、`dimensions`（content / source / permissions / safety / regression / effect / cost 固定序）、`scripts`（排序）、`aggregate`、`risk_tier`、`referrers`（排序）、`baseline_json`、`evaluated_at_ms`（UTC Unix 毫秒）、`operation_id` |
| `referrers_hash` | 引用者名单规整后的 SHA-256 |

域名白名单**只**认数据库两行 `network_domain_policy`（`http.allowed_domains` / `http.denied_domains`）与 `network_exceptions`；`basalt.yaml` 同名字段只是首次种子，读它会让哈希漏掉管理台之后的修改。缺行即报错，不静默取空。评分器版本固定 `admission-eval-v1`；新评分器必须新增版本号，旧评估不复用。

`evaluate` / `approve` / `publish` 重读磁盘复算内容哈希、来源哈希与保留期；`approve` / `publish` 还重算基线与策略哈希。任一漂移即拒绝，不静默采用新状态。

## 恢复

启动时持工作区实例锁、开放服务前扫描 `status='pending'`；**只查证，不重放**。`applied` → `success`；`not_applied` → `failed`；其余 → `unknown`，写 `recovered_at` 与 `verification`。查证入口先验 `target_kind == skill_admission` 且恢复依赖已装配，否则直接 `unknown`——防目标类型冒用。

| action | 查证对象 | `applied` | `not_applied` | 仍不确定 |
| --- | --- | --- | --- | --- |
| `propose` | 候选行与候选目录 | 候选行存在 | 行不存在且目录不存在 | 行不存在但目录存在（孤儿目录交人工检查） |
| `evaluate` | 按 `operation_id` 查评估行 | 存在 | 不存在 | 查询本身失败 |
| `approve` / `reject` | 按 `operation_id` 查决策行 | 存在 | 不存在 | 查询本身失败 |
| `cancel` | 候选状态与 `terminal_operation_id` | `canceled` 且终态操作匹配本操作 | 起始状态仍成立 | 其他状态 |
| `publish` | 按 `operation_id` 查发布记录 | 存在 | 不存在 | 查询本身失败 |
| `withdraw` | 按 `withdraw_operation_id` 查发布记录 | 存在 | 不存在 | 查询本身失败 |

`unknown` 操作在管理台单独列出，由人判断；不得自动删除、再次执行或把它补成 `success`。

**提交结果未知时禁止磁盘补偿**：发布 / 撤回事务的 `Commit` 返回错误只证明"结果未知"，不证明未生效。此时调用方不得恢复工作副本、不得删除快照（事务可能已提交，删除快照会毁掉已发布内容），操作立即记 `unknown` 交给恢复查证；撤回准备（快照）失败后的补偿本身失败（工作副本恢复或快照清理报错）同样记 `unknown`，不谎报 `failed`。**工作副本交换失败且回滚失败**（正式目录缺失、旧树留在 `.backup`、新树留在 `.staging`，由 `workspace.ErrWorkCopyUncertain` 标识）也是不确定状态：调用方记 `unknown`，不尝试复原或清理，留现场人工裁决；冻结候选目录在写入报错但目录已就位时同理（行不存在但目录存在 → `unknown`）。

## 只读接口

| 路径 | 语义 |
| --- | --- |
| `GET /api/v1/admissions` | 列表；`state`（8 字面量）、`skill`、`limit`（1–200，缺省 50）三个查询参数，其他参数忽略。返回数据库行投影，无清单摘要 |
| `GET /api/v1/admissions/{candidate_id}` | 详情；摊平列表行 + `source`（`notes` 置空）+ `files` 摘要 + 最新 `evaluation` / `decision` / `publication`。子记录缺失时对应字段为 `null`（不是 404，也不是空对象） |

候选不存在 → 404；`state` / `limit` 非法 → 400 固定文案；准入端口未装配 → 500（**不是**空列表）。固定字段以 [openapi.yaml](openapi.yaml) 的 `AdmissionListView` / `AdmissionDetailView` 为准。`GET /operations?target_kind=skill_admission` 可查七个动作的操作行。

管理面与 CLI 都没有批准、发布或撤回的 HTTP 端点；`POST` / `PUT` / `PATCH` / `DELETE` 落在准入路径上一律非成功。

## CLI

七个写命令都取工作区实例锁，**要求服务已停止**；`list` / `show` 不取锁，可在服务运行时执行。所有写动作在读取候选状态**之前**校验 actor：非空 `cli:<本机用户>` 主体、`Method=cli`、`Source=cli`；`cli:unknown` 在开库前拒绝。没有 `--approver` 之类旁路参数——批准者主体只来自本机身份。

```text
basalt admission propose --name <skill> --from-dir <dir> --source-file <json>
basalt admission list [--state <state>] [--skill <name>] [--limit <n>]
basalt admission show <candidate-id> [--files] [--diff] [--decision-template]
basalt admission evaluate <candidate-id>
basalt admission approve <candidate-id> --decision-file <path>
basalt admission reject  <candidate-id> --decision-file <path>
basalt admission cancel  <candidate-id> --reason <text>
basalt admission publish <candidate-id>
basalt admission withdraw <candidate-id> --reason <text>
```

`show --files` 输出候选文件正文，`--diff` 输出与当前发布版本的有界 unified diff（≤1 MiB，自实现 LCS，不引第三方 diff 库）；两者都只读。

## 明确不承诺

- **不自动蒸馏、不自动判定成功**：候选必须由人或上游 Agent 显式提议，再经本机人工批准。
- **不做真实模型 / Judge 行为评测**：`effect` / `cost` 恒为 `not_evaluable`（原因 `real_model_not_authorized` / `no_real_model_invocation`）；不测量效果与成本，也不因其"未测出问题"放行。
- **不执行候选脚本**：含脚本一律高风险，只接受逐条人工静态审查证据；不建隔离容器、不运行脚本。
- **不提供企业治理**：无 SSO / RBAC / 多租户 / 远端审批 / 法定人数；本机 CLI 是唯一批准通道。
- **不建第二套写入路径**：发布复用既有技能发布原语与事务；不新增发布 / 审计 / 消息 / 执行路径，不改技能运行时格式与渐进披露语义。
- **不做自动回滚、后台过期扫描或自动 GC**：过期只在读侧派生；孤儿目录只报告，不自动删除。
- **不做首版撤回到空**：撤回要求存在可恢复的上一版内容，否则拒绝。
- **不覆盖记忆候选（US-22）**：记忆准入必须有自己的状态机与确认语义，不共用本契约的表与通道。
