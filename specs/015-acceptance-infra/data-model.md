# Data Model — US-15 工程验收基础设施

**事实源**：模块卡 `docs/us/us15-acceptance-infra.md` §3.3（记录规范）、§3.2（stage 与台账）、§4.2（check 与超时）。本文是设计视图；字段字面量冲突时以模块卡 §3.3 与 `docs/contracts/` 外的固定文本为准，不另造第二套定义。

数据全部是文件，无数据库变更：运行目录 `.loop/runs/<run>/`、发布面 `harness/acceptance/`、stage 文件 `harness/stages/*.json`。

## 实体关系

```text
check 定义（acceptance.json / stage.checks）
     │ 选择并执行
     ▼
acceptanceReport（运行目录 report.json）
     │ buildRecord
     ▼
record（运行目录 record.json）
     │ 成对发布（先绑定校验）
     ├──────────────► records/<record_id>.json
     ├──────────────► records/reports/<record_id>.json（同次报告原样拷贝）
     └──────────────► ledger.md 追加一行

catalogItem（backfillCatalog）◄── item / check / test / level ──► stage.required_tests
     │ computeRecordStatus（读取时计算，不落盘）
     ▼
statusKey(item, source_fingerprint, os) → statusEntry(record_ids, verdict, conflict)
```

## E1. record（运行记录）

| 字段 | 类型 | 规则 |
| --- | --- | --- |
| `version` | int | 固定 `1` |
| `record_id` | string | 运行记录 = 运行目录名；人工项记录 = `<运行目录名>-<item>` 或 `backfill-<UTC 时间戳>`；全库唯一 |
| `run_id` | string | 证据所属运行目录名；运行记录必须等于 `record_id`；人工项记录必须等于其报告中的 `run_id` |
| `run_kind` | enum | `original` / `rerun` / `backfill` |
| `run_status` | enum | `passed` / `failed` / `source_changed`；运行记录必填；`error`（含取消）不生成记录 |
| `executor` | enum | `ai` / `user` / `ci` |
| `started_at` / `finished_at` | RFC3339 UTC | 必填 |
| `subject` | object | 见 E1.1 |
| `criteria` | object | 见 E1.2 |
| `code` | object | 见 E1.3 |
| `environment` | object | `os` / `arch` / `go_version` / `ci`；不采集其他环境变量 |
| `authorization` | object/null | `subject.level = external` 时必填（`ref` + `scope`）；其他层级必须为空 |
| `verdict` | enum | `passed` / `failed` / `skipped` / `not_verified` / `not_applicable` |
| `na_basis` | string | `not_applicable` 时必填 |
| `checks` | array | 运行记录必填；见 E3 |
| `evidence` | object | 运行记录必填；见 E1.4 |
| `red_state` | object/null | 非空时 `path` / `sha256` / `applies_to` 必填 |
| `notes` | string | 可选；`not_verified` 时必须写明缺失条件 |

### E1.1 subject

| 字段 | 规则 |
| --- | --- |
| `kind` | `run` / `manual_item` |
| `level` | `source` / `process` / `external`；运行记录 = 所选 check 声明层级的最大值；人工项记录 = `check_id` 的声明层级 |
| `stage_id` / `stage_path` / `profile` | 有 stage 时填写 |
| `items` | 人工项 `passed` / `failed` 时必填；每项必须在 `backfillCatalog` 中映射到同一 `(check, test)`；跨 check 拆记录 |
| `check_id` / `test` | 人工项 `passed` / `failed` 时必填；`test` 为 `go test -json` 全名（子测试写 `Parent/sub`） |

### E1.2 criteria

`acceptance_sha256`（`harness/acceptance.json` 内容哈希）、`stage_sha256`（有 stage 时）、`inputs`（路径 + sha256）、`check_ids`（运行记录必填）。

### E1.3 code

`base_commit`（`git rev-parse HEAD`）、`source_fingerprint`（现行 `sourceFingerprint`）、`dirty`（工作区相对 HEAD 是否有改动）、`tested_binary_sha256`（`process` / `external` 必填；从日志 `binary_sha256=` 行提取；`source` 可为 `null`）。

### E1.4 evidence

`report_path` + `report_sha256`（运行记录指向运行目录 `report.json`；人工项指向已发布 `records/reports/<record_id>.json`）、`logs`（逐 check 相对路径 + 哈希）、`artifacts`（按需）、`excerpt`（脱敏摘要）。

## E2. acceptanceReport（机器报告）

| 字段 | 规则 |
| --- | --- |
| `version` / `scope` / `fingerprint` | 必填 |
| `run_id` | 运行目录名 |
| `base_commit` / `dirty` / `environment` / `tested_binary_sha256` / `authorization` | 供记录绑定与发布校验 |
| `started_at` / `status` / `exit_code` | `status` ∈ `passed` / `failed` / `source_changed`（工具无顶层 `skipped`） |
| `checks` | 逐 check：`id` / `level` / `status` / `exit_code` / `duration_ms` / `passed_tests` / `skipped_tests` / `log` |

**路径规则**：序列化前 `checks[].log` 投影为仓库相对路径、`/` 分隔；报告写入后不再修改，发布时原样拷贝。

## E3. recordCheck / result

`status` ∈ `not_run` / `passed` / `failed` / `error`；`passed_tests` / `skipped_tests` 是 `go test -json` 事件解析结果；`required_tests` 中的名字必须是本轮具名 PASS，SKIP 或未执行判该 check 失败。

## E4. stage（阶段描述）

| 字段 | 规则 |
| --- | --- |
| `version` | 固定 `1` |
| `id` | 非空；`current-tree` / `us15-linux` / `us15-slow` / `us15-real` 等 |
| `approval` | 必须指向真实人审决定与所审版本；拒绝 `REPLACE` 占位 |
| `inputs` / `protect` | 仓库内相对路径且实际存在 |
| `checks` | check 定义：`id` / `args` / `timeout_seconds`（1..7200）/ `required_tests` / `level` / `eval_report` / `allowed_skips` |
| `tasks` | task → checks；不得引用未知 check |
| `superseded_checks` | 可选：旧 check ID → 新 check ID；目标必须存在且不自我替代 |

## E5. catalogItem（回填目录项）

| 字段 | 规则 |
| --- | --- |
| `Item` | 历史子判据编号（如 `us10-6.2-1b`），格式与唯一性由 `TestStages_BackfillCatalogMatchesRequiredTests` 校验 |
| `Check` | 现行 check ID：`015-backfill` / `015-signal-linux` / `015-slow-timeout` / `015-real-model` / `015-real-feishu` |
| `Test` | `go test -json` 全名；有子测试写 `Parent/sub` |
| `Level` | `source` / `process` / `external`；`requiredBackfillItems` 只含 `source` / `process`，`requiredRealItems` 只含 `external` |

`backfillCatalog` 与两个冻结全集双向相等；目录项与各 stage 的 `required_tests` 按 check 逐字一致。

## E6. ledger 行

固定列序：`record_id` | `date` (UTC) | `scope` | `run_kind` | `executor` | `verdict` | `record`。`record` 列链接 `records/{record_id}.json`；每发布一条记录追加一行，只追加、不改历史行；报告不单独建行。

## E7. statusKey / statusEntry（读取时计算）

```text
statusKey{Item, Fingerprint, OS}
      │
      ▼
statusEntry{RecordIDs []string, Verdict string, Conflict bool}
      │
      ├─ 键内取 finished_at 最新；同刻取 record_id 字典序最大
      ├─ 不同键并列保留（不跨平台淘汰）
      └─ 同键或跨键结论相反 → Conflict = true，保留两条记录 ID
```

三视图：

1. **历史曾执行**：该 item 是否存在任何 `passed` 回填记录。
2. **指定版本与平台**：按 `(item, source_fingerprint, os)` 键取结论；用户复跑进入同一视图。
3. **本次回归**：本轮 `current.json` / `us15-linux.json` 中承载该 item 的 check 结论。

## 校验规则（机器判卷）

1. 枚举：`run_kind` / `executor` / `subject.kind` / `subject.level` / `verdict` / `checks[].status` 只接受上表值。
2. `not_applicable` 必须有 `na_basis`；`not_verified` 必须 `subject.kind = manual_item`，且 `notes` 与 `subject.items` 非空。
3. 运行记录的 `verdict` 只允许 `passed` / `failed`；`skipped` / `not_verified` / `not_applicable` 只允许人工项记录。
4. `process` / `external` 记录必须有 `tested_binary_sha256`；缺失不得降级。
5. `external` 记录必须有 `authorization`，且与同次报告逐项相等；非 `external` 记录携带授权即失败。
6. 记录、报告与台账中所有路径为仓库相对路径、`/` 分隔；拒绝盘符、前导 `/`、反斜杠、Unix 家目录路径。
7. 序列化后的记录拒绝凭证模式（`sk-[A-Za-z0-9_-]{16,}`、`xox[baprs]-`、`ghp_[A-Za-z0-9]{20,}`、`AKIA[0-9A-Z]{16}`）；拒绝 webhook 的路径 / 查询串 / userinfo。
8. `red_state` 非空时 `path` / `sha256` / `applies_to` 均非空；`applies_to` 每项为仓库相对路径 + 64 位小写十六进制。
9. `run_status = source_changed` ⇒ `verdict = failed`；`passed` 要求每个 check 为 `passed`。
10. 执行绑定：人工项 `passed` / `failed` 的 `items` 必须在目录中映射到 `check_id` / `test`；报告存在且哈希相符；`run_id` / 指纹 / 基线提交 / 脏标记 / 平台 / 架构 / 二进制哈希逐项相等；`passed` 还要求报告对应 check 为 `passed` 且 `passed_tests` 含该测试。
11. 只追加：同名记录已存在即停止；运行记录碰撞只能换新目录重跑；人工项记录可另取新 ID。
