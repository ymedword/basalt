# 仓库日报参考（L3）

字段级定义与核对口径。**判定值一律以 `digest.json` 为准**——本文件解释「为什么」，
不替代脚本输出。

## 1. 输入形态

三个输入文件是 MCP 工具返回的原始 JSON：裸数组，或 `{"items": [...]}`（GitHub 列表
响应的常见形态）。脚本只读以下字段：

| 来源 | 身份 | 必需 | 业务字段 |
| --- | --- | --- | --- |
| PR | `number` | `updated_at`、`state` | `created_at`、`draft`、`merged_at` |
| Issue | `number` | `updated_at`、`state` | `created_at`、`comments` |
| Release | `id`（无则 `number`） | `updated_at`（键缺失时用 `published_at`） | `draft`、`prerelease`、`published_at` |

- 时间字段**必须带时区偏移**（`...Z` 或 `+08:00`）。无时区的时间无法与游标
  比较，按「无法确认」处理。
- 真实 GitHub 的 release 对象没有 `updated_at`（MCP `list_releases` 的字段枚举里
  也没有），发布事件的唯一时间锚是 `published_at`。因此 Release 的游标时间 =
  `updated_at`（存在时）→ 键缺失则 `published_at`。**键存在但解析失败**仍是坏行
  （进 `held_back`，不拿发布时间掩盖脏值）；草稿且没有 `published_at` 没有可比较的
  时间，同样进 `held_back`。
- 身份或 `updated_at` 读不出来的条目：**不处理**，进 `held_back`，并且挡住游标。
- 业务字段缺失或类型不对：条目照常计入，同时进 `unconfirmed`，**不猜默认值**。
- 「键缺失」与「显式 `null`」是两件事：`merged_at` 键缺失 → 待确认；
  `merged_at: null` → 关闭未合并。
- 实测 github-mcp-server 2.0.3：Issue 列表的 `state` 是大写 GraphQL 枚举
  （`OPEN` / `CLOSED`），脚本按大小写不敏感归一化后再分桶；PR 列表本来就是小写。
- 同一版本实测：Issue 的 `comments` 在 0 条评论时**省略该键**。脚本不把缺失当 0，
  这类条目进 `unconfirmed`（`field: comments`），并且**不会**触发 `unanswered_open`。
  这是有意的保守口径——要恢复该信号需要换用能返回 0 的字段 / 工具。

## 2. 分桶

| 来源 | 桶 | 判定 |
| --- | --- | --- |
| PR | `open` | `state=open` 且非草稿 |
| PR | `draft_open` | `state=open` 且 `draft=true` |
| PR | `merged` | `state=closed` 且 `merged_at` 可解析 |
| PR | `closed_unmerged` | `state=closed` 且 `merged_at` 显式为 `null` |
| PR | `unknown` | `state` 非法，或 `merged_at` 缺失 / 不可解析 |
| Issue | `open` / `closed` / `unknown` | `state` 为 `open` / `closed` / 其他 |
| Release | `draft` | `draft=true`（不论是否已发布） |
| Release | `published` | 非草稿且 `published_at` 可解析 |
| Release | `prerelease` | 同上且 `prerelease=true` |
| Release | `unpublished` | 非草稿且 `published_at` 显式为 `null` |
| Release | `unknown` | 非草稿且 `published_at` 不可解析；键缺失且无 `updated_at` 时整条进 `held_back`，不进此桶 |

`counts.with_unconfirmed_fields` 统计「已计入但至少有 1 个待确认字段」的条目数，
它不参与分桶求和（`sum(桶) = processed` 恒成立，`with_unconfirmed_fields` 另算）。

## 3. 待跟进规则

| 原因码 | 条件（`age_days` = 运行时钟 − `created_at`） |
| --- | --- |
| `stale_open` | PR `state=open`、`draft=false`、`age_days ≥ stale_days` |
| `unanswered_open` | Issue `state=open`、`comments == 0`、`age_days ≥ stale_days` |
| `long_open` | Issue `state=open`、`age_days ≥ long_open_days` |

同一条按上表顺序只命中一个原因；`unanswered_open` 优先于 `long_open`。
阈值缺省 `stale_days=7`、`long_open_days=30`，报告里以 `window` 回显。

## 4. 游标不变量

状态文件 `state/cursor.json`：

```json
{"schema_version": 1, "sources": {"pull_requests": null, "issues": null, "releases": null}}
```

- 只有时间游标 `> 上次游标` 的条目算「新」（`is_new`）。时间游标对 PR / Issue 是
  `updated_at`，对 Release 是 `updated_at`（键缺失时用 `published_at`）。首跑游标为
  `null`，全部算新。
- **游标只前进到已处理项**：
  - 没有受阻项 → 取已处理条目的最大 `updated_at`；
  - 有受阻项但时间可读 → 取**严格小于**受阻项最早时间的最大已处理时间（相等不行：
    下一次比较是 `>`，相等会让受阻项被永久跳过）；
  - 受阻项连时间都读不出来 → 游标**原地不动**（fail-closed，不猜）。
- 代价：受阻项会挡住它之后已处理过的同源条目，下一次它们会被**重复计入**。
  这是有意选择——**宁可重复，不可漏读**；日报里如实写「本次有 N 条未计入，
  游标未越过它们」。
- 状态里的时间是规范化后的 ISO 8601：输入写 `Z`，落盘是 `+00:00`。
- `schema_version` 不是 `1`、或出现未知来源名 → 脚本拒绝运行（不猜、不迁移）。

## 5. 输出 `digest.json`

| 字段 | 说明 |
| --- | --- |
| `observed_at` | 本次运行时钟（带偏移的 ISO 8601） |
| `window.stale_days` / `window.long_open_days` | 本次生效的阈值 |
| `sources.<来源>.total` | 列表条目总数 |
| `sources.<来源>.new` | 晚于游标的条目数（含受阻项） |
| `sources.<来源>.processed` | 本次计入统计的条目数 |
| `sources.<来源>.held_back[]` | 未计入的条目：`number` / `field` / `reason` / `updated_at` |
| `sources.<来源>.counts` | 分桶计数 + `with_unconfirmed_fields` |
| `sources.<来源>.cursor_held` / `cursor_limit` | 仅有受阻项时出现：阻挡原因与上限时间 |
| `followups[]` | `source` / `number` / `title` / `url` / `reason` / `age_days` |
| `unconfirmed[]` | `source` / `number` / `title` / `field` / `reason` |
| `cursor.previous` / `cursor.next` / `cursor.advanced` | 三源游标的前后值与是否推进 |
| `cursor.held_by` | 被阻挡的来源 → 阻挡原因（`unprocessed_item` / `unparseable_updated_at`） |
| `message` | 唯一结论句 |

写序：先写 `digest.json`，成功后才原子替换 `cursor.json`。两步之间崩溃只会导致
「下次重复处理」，不会导致「没写成却记成已处理」。报告里**不**复制状态，避免两处
副本漂移。

## 6. 命令行

```sh
python scripts/repo_digest.py --pull-requests pull_requests.json \
  --issues issues.json --releases releases.json \
  --state state/cursor.json [--output runs/<时间戳>/digest.json] [--now <ISO8601+偏移>]
```

| 参数 | 必填 | 缺省 | 说明 |
| --- | --- | --- | --- |
| `--pull-requests` | 是 | — | PR 列表原始响应 |
| `--issues` | 是 | — | Issue 列表原始响应 |
| `--releases` | 是 | — | Release 列表原始响应 |
| `--state` | 是 | — | 游标文件；存在则读，报告写成功后原子替换 |
| `--output` | 否 | `runs/<YYYYMMDDTHHMMSS>/digest.json` | 报告路径；父目录自动创建 |
| `--now` | 否 | 本机当前时间 | 运行时钟，**必须带时区偏移**；显式给出可复跑 |
| `--stale-days` | 否 | `7` | PR / Issue 待跟进阈值 |
| `--long-open-days` | 否 | `30` | Issue 长期未关阈值 |

任何输入或状态不合规 → `stderr` 一行 `repo_digest: <原因>`、退出码 1、**一个字节
都不写**。`stdout` 只输出 JSON 摘要（三份输入与报告、状态的路径与 SHA-256、计数、
结论），可直接作为证据里的哈希。

## 7. 固定夹具的期望读数

夹具与断言在 Demo 目录的 `fixtures/` 与 `scripts/test_repo_digest.py`；下面是
2026-10-25 的实测读数（固定时钟 `2026-10-25T08:45:00+08:00`，首跑无游标）：

| 项 | 期望 |
| --- | --- |
| PR 计数 | `open=2`、`draft_open=1`、`merged=1`、`closed_unmerged=1`、`unknown=1`、`with_unconfirmed_fields=1` |
| Issue 计数 | `open=4`、`closed=1`、`with_unconfirmed_fields=1` |
| Release 计数 | `published=1`、`prerelease=1`、`draft=1`、`unknown=1`、`with_unconfirmed_fields=1` |
| 待跟进 | PR #2 `stale_open`（15 天）、Issue #13 `long_open`（54 天）、Issue #12 `unanswered_open`（10 天） |
| 待确认 | PR #6 `merged_at` missing、Issue #15 `comments` missing、Release #1004 `published_at` missing |
| `message` | `本次处理 PR 6 / Issue 5 / Release 4 项，待跟进 3 项，待确认 3 项` |
| 第二次同输入运行 | 三源 `processed=0`，`message` 为「本次无新增…」，游标文件字节不变 |

## 8. 六段证据的投递查询

`deliver_to` 由系统代投，不在 `tool_invocations` 里留 `notify` 行。核对投递看
`schedule_deliveries`：

```sql
SELECT run_id, status, channel_uid, created_at
FROM schedule_deliveries WHERE run_id = '<run_id>';
```

- `sent` 才算送达；`failed` / `unknown` 不算通过，`skipped` 表示「按规则未发送」
  （Run 未成功）；
- 投递失败**不自动补发**，也不回滚游标——日报照常产出，补发由人用保存的报告手动做。
