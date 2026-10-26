# D2 repo-digest —— 代码仓库日报

每天汇总一个仓库的 PR / Issue / Release 变化：给出计数、待跟进与待确认清单，
用游标只处理「上次之后的新条目」，成功后由系统代投到通知渠道。

这个 Demo 的看点是三件事叠在一起：**技能三级披露**（L1 卡片进提示词、L2 正文由模型
`read_file` 取、L3 参考按需再取）、**缺字段标「待确认」而不是猜**、**游标只前进到
已处理项**。

> 档位：Demo（不是模板）。**夹具可复跑已验证**；**真实 GitHub MCP + PAT 实跑已验证**
> （2026-10-27 本机，github-mcp-server 2.0.3）；**`deliver_to` 实际送达未验证**
> （无真实接收方）。证据档位见文末。

## 业务目标

回答「这个仓库昨天到今天有哪些值得跟的事」。要点：

- 三源（PR / Issue / Release）各有分桶计数，`counts` 之和等于 `processed`；
- 待跟进按固定规则筛（过期未合并 PR、长期未答 / 长期未关 Issue），原因码唯一；
- 字段读不出来就写「待确认」，**不得**用相邻桶或默认值凑整；
- 同一天重跑不会把同一批条目再报一遍——游标只前进到已处理项。

## 工具清单与 `tools:` 声明

| 工具 | 来源 | 用途 |
| --- | --- | --- |
| `list_pull_requests` | GitHub MCP（`github`） | 取 PR 列表 |
| `list_issues` | GitHub MCP（`github`） | 取 Issue 列表 |
| `list_releases` | GitHub MCP（`github`） | 取 Release 列表 |
| `read_file` | 内置 | 读技能 L2/L3、读脚本产物 |
| `write_file` | 内置 | 落盘三份原始响应、写 `digest.md` |
| `shell` | 内置 | 运行 `scripts/repo_digest.py`（解释器是 `AGENT.md` 里已替换的绝对路径，须在 `shell.allowed_commands` 里整串登记） |

`AGENT.md` 的声明与上表一致；`notify` **不在**声明里——基线 `notify_mode: none`，
装配面也不会补。`shell` 同样是必需项：脚本跑不起来就没有可信数字（模块卡 §2.3）。

**MCP 工具名已实测**：三个名字与本机 `github-mcp-server 2.0.3` 的 `tools/list` 一致
（2026-10-27 实测；MCP 目录条目 `github` 的 `verified` 字段保持目录口径，不随单次实跑改写）。
若换版本后加载报 `tool not registered: <name>`，按实际 `tools/list` 改 `tools:` 声明，
**不要**改脚本。

## 技能：L1 / L2 / L3

`AGENT.md` 用 `skills: [repo-digest]` 声明绑定——**声明即绑定**，不存在别的绑定方式。

| 层 | 文件 | 谁读 | 内容 |
| --- | --- | --- | --- |
| L1 | 系统提示词里的一行卡片 | 每轮自动注入 | `- repo-digest：<description>（.basalt/versions/skills/repo-digest/<资源 UID>/<版本号>/SKILL.md）` |
| L2 | 发布目录里的 `SKILL.md`（路径见 L1 卡片） | 模型按需 `read_file` | 口径、分桶、日报排版 |
| L3 | 同一发布目录里的 `REFERENCE.md` | 模型按需再取 | 字段定义、游标不变量、期望读数 |

卡片给的是**已发布版本**的真路径（`.basalt/versions/skills/repo-digest/<uid>/<no>/`），
不是工作副本 `skills/repo-digest/SKILL.md`：绑定发布版本的运行只读发布目录，改工作
副本要重新发布版本才对下一次运行生效。

要点：

- **不预载正文**：L1 只有名字、描述与路径；正文由模型自己按需读（渐进披露）。
- **没有 `use_skill` 工具**：取用技能就是普通的 `read_file`，路径就在卡片里。
- 技能目录名、`SKILL.md` 的 `name`、`AGENT.md` 的声明名三者一致；技能库里没有
  实体、或没有**已发布版本**时，整只 Agent 不注册（WARN 点名）。
- 导入时把 `skills/repo-digest/` 整个复制到 `<workspace>/.basalt/skills/repo-digest/`
  （见 `SETUP.md`）。`skills/` 对模型只读。

## 内置域名白名单

**无**。本 Demo 不声明 `http_get`，Basalt 自身不发任何出站 HTTP 请求；全部数据经
GitHub MCP 子进程取得。因此不需要在「执行环境」页添加任何域名。

## MCP 自身出网范围

`github-mcp-server` 是独立进程，访问 `api.github.com` 等由它自己决定，**不受**
Basalt 的域名白名单约束。Basalt 只约束自己发出的 HTTP 请求，不代管子进程的网络
策略——这一点在给 MCP server 授权时要自己心里有数。

## 启动程序与版本

目录条目 `github` 的配方：`github-mcp-server stdio`（Go 二进制，在 PATH 上），
环境变量 `GITHUB_PERSONAL_ACCESS_TOKEN`。命令、参数、版本、只读工具名与代表性
调用**均未实测**（`verified: false`）。

## 凭据

| 变量 | 说明 |
| --- | --- |
| `GITHUB_PERSONAL_ACCESS_TOKEN` | 只读细粒度 PAT（能读仓库的 PR / Issue / Release 即可）。配置里只写 `${GITHUB_PERSONAL_ACCESS_TOKEN}` 占位，**不得**落明文 |

## 依赖与固定版本

| 依赖 | 版本 | 用途 |
| --- | --- | --- |
| Python | 3.12（标准库即可） | 运行 `scripts/repo_digest.py`（绝对路径登记在 `shell.allowed_commands`） |
| `github-mcp-server` | 待实测 | GitHub 只读访问 |
| Basalt | v0.1.0 | 运行底座（技能发布、调度、`deliver_to`） |

脚本零第三方依赖：只用 `argparse` / `json` / `hashlib` / `tempfile` / `datetime` /
`pathlib` / `os` / `sys`。

## 读写文件范围

| 路径 | 谁写 | 说明 |
| --- | --- | --- |
| `pull_requests.json` / `issues.json` / `releases.json`（Agent 根目录） | 模型（`write_file`） | MCP 响应**原样**落盘，**不得**改字段、改数字、改顺序 |
| `runs/<时间戳>/digest.json` | 脚本（原子替换） | 机器可读结果；父目录由脚本创建 |
| `runs/<时间戳>/digest.md` | 模型（`write_file`） | 日报正文，数字全部取自 `digest.json` |
| `state/cursor.json` | 脚本（原子替换） | 三源游标；模型**不得**手改 |
| `scripts/` | 只读区 | 模型改不动脚本；这是「数字由脚本算」的前提 |

`write_file` **不自动创建父目录**：根目录三份输入文件没有这个问题；`digest.md` 写在
脚本刚建好的 `runs/<时间戳>/` 里。

## 游标与「宁可重复，不可漏读」

游标（`state/cursor.json`）只做一件事：决定下一轮哪些条目算「新」。

- 只有 `updated_at > 上次游标` 的条目计入；首跑没有游标，全部算新。
- **游标只前进到已处理项**：编不了号或时间读不出来的条目**不处理**，游标不得越过它。
- 受阻项会挡住它之后已处理过的同源条目，下一次这些条目会被**重复计入一次**。
  这是有意的取舍：宁可重复，不可漏读；日报会在末尾如实写「本次有 N 条未计入，
  游标未越过它们」。
- 写序先 `digest.json` 后 `cursor.json`：两步之间崩溃只会导致下次重复，不会导致
  「没写成却记成已处理」。报告里不复制状态副本。

## 通知路径（`deliver_to` 系统代投）

- **便携基线不带 `deliver_to`**（也不带任何 UID）：`notify_mode: none`，这一轮零通知，
  工具面里也没有 `notify`。
- 按 `SETUP.md` 在任务上配置 `deliver_to`（值是**渠道 `resource_uid`**，`rs_` 前缀，
  不是渠道名）后：该轮 Run 准入时固定 `notify_mode = none` 并从工具面剔除 `notify`，
  模型只产出正文；**仅当 Run 成功结束**，系统才写 `pending` 行并发送本轮最终正文。
- 投递终态 `sent` / `failed` / `unknown`；非成功 Run 记 `skipped`（按规则未发送）——
  `failed` / `unknown` 都**不算送达**，也**不自动补发**。
- 系统代投与模型 `notify` 互斥，不得通过改写 `AGENT.md` 实现接管。

## 生效方式

改 `AGENT.md`、技能内容、`mcp_servers.yaml` 或 `shell.*` 白名单后**必须重启** Basalt——
前两类走受控发布后还需重新发布，后两类不热加载。管理台显示「待发布 / 待重启」
不等于已生效。

## 固定输入

`fixtures/` 三份**虚构**数据（仓库 `northwind-labs/mcp-bridge`，不含真实项目、账号或
凭据），形态与 GitHub 列表响应一致（`{"total_count", "items": [...]}`）。夹具**永远**
是虚构仓库：单测的期望计数、哈希与它们是绑定的，换成真实仓库就不可复跑。

**实际跑的时候默认目标是真实公开仓库 `github/github-mcp-server`**（`AGENT.md` 的
`daily-digest` 触发消息），因为 MCP 的 `list_pull_requests` 只认真实 `owner/repo`——
夹具里的虚构仓库在真实 GitHub 上不存在，查了只会是 404 / 空列表。换成自己的仓库：
改 `schedules[0].message` 里的 `owner/repo`，重启后生效；不要把真实仓库写回夹具。

真实列表响应远大于夹具：`AGENT.md` 第 2 步要求每个列表工具传 `perPage: 10` 与
`fields`（只取脚本要用的字段），否则整段响应写进 `write_file` 会被入参上限截断
（报「入参不是合法 JSON」）。另外，真实 release 对象**没有** `updated_at`，脚本的
时间锚是 `published_at`；夹具为固定读数显式带了 `updated_at`，两种形态脚本都接受。

刻意的设计（每条对应一条判据）：

1. **PR #6 的 `merged_at` 键缺失**（closed 但没有该字段）→ 进 `unknown` + 待确认；
   **PR #5 写的是 `"merged_at": null`** → 进 `closed_unmerged`。缺键 ≠ 显式 null。
2. **PR #3 是草稿**（`draft: true`）→ 进 `draft_open`，即使很旧也不触发 `stale_open`。
3. **Issue #12 零评论且 10 天**→ `unanswered_open`；**Issue #13 54 天**→ `long_open`。
   两条同时开着，原因码不同，不能合并成一句「老 Issue」。
4. **Issue #15 没有 `comments` 键** → 待确认，**不得**按 0 评论处理（否则会误判
   `unanswered_open`）。
5. **Release #1003 是草稿且 `published_at: null`** → 进 `draft`（不是待确认）；
   **Release #1004 非草稿但没有 `published_at` 键** → 进 `unknown` + 待确认。夹具显式
   带 `updated_at`（真实 release 对象没有，脚本用 `published_at` 兜底），所以这两条
   的判定由 `updated_at` 定；草稿且两个时间都没时，整条进 `held_back` 并挡住游标。

固定输入哈希（sha256）：

| 文件 | sha256 |
| --- | --- |
| `fixtures/pull_requests.json` | `b17ad886830f66a6fcad2e43f1eb8d7900a0585828c82dbea300bad3f68e51a1` |
| `fixtures/issues.json` | `c1688f9cfed8ab0d94ed7e360ef7c98d3ed02457834fde9b2de8eb7f486127cf` |
| `fixtures/releases.json` | `c2b012efbb0778b61380e7cf7206de106f1c84c737a3ab8ce0fe5fd83061fbbb` |

## 脚本输入 / 输出契约

```sh
python scripts/repo_digest.py --pull-requests pull_requests.json \
  --issues issues.json --releases releases.json --state state/cursor.json
```

| 参数 | 必填 | 缺省 | 说明 |
| --- | --- | --- | --- |
| `--pull-requests` / `--issues` / `--releases` | 是 | — | 三个来源的原始响应 |
| `--state` | 是 | — | 游标文件；存在则读，报告写成功后原子替换 |
| `--output` | 否 | `runs/<YYYYMMDDTHHMMSS>/digest.json` | 报告路径；父目录自动创建 |
| `--now` | 否 | 本机当前时间 | 运行时钟，**必须带时区偏移**（如 `2026-10-25T08:45:00+08:00`）；显式给出可复跑 |
| `--stale-days` | 否 | `7` | PR / Issue 待跟进阈值 |
| `--long-open-days` | 否 | `30` | Issue 长期未关阈值 |

**输入**：裸数组，或 `{"items": [...]}` 形态。身份与时间游标（PR / Issue 用
`updated_at`，Release 用 `updated_at`、键缺失时用 `published_at`；都必须带时区）
读不出来的条目不计入、挡住游标；业务字段（`merged_at` / `comments` / `published_at`
等）缺失或不可解析 → 条目照算，同时进 `unconfirmed`。任何输入或状态不合规 →
**非零退出，一个字节都不写**。

**输出 `digest.json`**：`observed_at`、`window`、`sources.<源>`（`total` / `new` /
`processed` / `held_back` / `counts`）、`followups[]`、`unconfirmed[]`、`cursor`
（`previous` / `next` / `advanced` / `held_by`）、`message`。字段级说明见技能 L3
（`skills/repo-digest/REFERENCE.md`）。

**状态 `state/cursor.json`**：`{"schema_version": 1, "sources": {"pull_requests": …,
"issues": …, "releases": …}}`；时间落盘为带偏移的 ISO 8601（输入写 `Z`，落盘是
`+00:00`）。

`stdout` 输出一段 JSON 摘要（三份输入、报告与状态的路径与 SHA-256、计数、结论），
可直接作为证据里的「输入快照哈希 / 候选产物哈希」。

## 期望业务结果（夹具复跑）

命令与固定时钟：

```sh
python scripts/repo_digest.py --pull-requests fixtures/pull_requests.json \
  --issues fixtures/issues.json --releases fixtures/releases.json \
  --state /tmp/cursor.json --output /tmp/digest.json --now 2026-10-25T08:45:00+08:00
```

首跑（无游标）的实测读数：

| 断言 | 期望 |
| --- | --- |
| PR `counts` | `open=2`、`draft_open=1`、`merged=1`、`closed_unmerged=1`、`unknown=1`、`with_unconfirmed_fields=1` |
| Issue `counts` | `open=4`、`closed=1`、`with_unconfirmed_fields=1` |
| Release `counts` | `draft=1`、`prerelease=1`、`published=1`、`unknown=1`、`with_unconfirmed_fields=1` |
| 三源 `processed` | `6` / `5` / `4`；`held_back` 全空 |
| `followups` | PR #2 `stale_open`（15 天）→ Issue #13 `long_open`（54 天）→ Issue #12 `unanswered_open`（10 天） |
| `unconfirmed` | PR #6 `merged_at` missing、Issue #15 `comments` missing、Release #1004 `published_at` missing |
| `cursor.next` | PR `2026-10-24T14:00:00+00:00`、Issue `2026-10-24T13:30:00+00:00`、Release `2026-10-24T11:00:00+00:00` |
| `message` | `本次处理 PR 6 / Issue 5 / Release 4 项，待跟进 3 项，待确认 3 项` |

同一次首跑的产物哈希（同一时钟、同一输入，重跑两次一致）：

| 产物 | sha256 |
| --- | --- |
| `digest.json` | `9773ebaf15701e6fe94036631aaa283330c6f4aa31c6b7468b2e0a67a6c8b836` |
| `state/cursor.json` | `8218ba5e176e4deaa7aba44e262d60289770972296dcc201fc640cdc92ca5072` |

第二次运行（同一输入、沿用上一次的 `cursor.json`）：三源 `processed=0`，
`message` 为 `本次无新增：三个来源都没有晚于游标的新条目`，游标文件字节与上次一致
（`advanced` 全 `false`）。

## 失败样例

| 场景 | 期望行为 |
| --- | --- |
| 输入文件不存在 | 脚本非零退出（`exit 1`），`stderr` 给可读原因；**不写**报告与游标（实测：`repo_digest: pull_requests 输入 不存在：…`） |
| 输入不是数组 / 不是 `{"items": …}` | 同样非零退出（实测：`repo_digest: pull_requests 必须是 JSON 数组，或 {"items": [...]} 形态的列表响应`） |
| 游标 `schema_version` 不是 1 | 非零退出（实测：`repo_digest: 状态文件 schema_version 必须是 1，实际 99`）；**不猜、不迁移** |
| 某条缺 `number` 或 `updated_at` 不可解析 | 该条进 `held_back`，游标退到它之前；日报写「本次有 N 条未计入」，下一轮重读 |
| closed PR 没有 `merged_at` 键 | 进 `unknown` + 待确认；**不得**写成 merged 或 closed_unmerged |
| GitHub MCP 不可达（无输出可落盘） | **不运行脚本**；`digest.md` 写「来源不可达 / 数据缺失」+ 观测时间，不补造条目 |
| 搜索结果为空（三份都是 `[]`） | `processed=0`、`message` 为「本次无新增…」，仍写出报告 |
| `shell` 工具拒绝脚本路径 / 解释器未放行 | Run 可能仍 `completed`（失败结果回填给模型）；业务断言因缺 `digest.json` 判失败，不算通过 |
| Run 成功但投递失败 | `schedule_deliveries` 记 `failed` / `unknown`，**不算送达**；不自动补发，日报文件仍可查 |

## 六段证据怎么取

| 段 | 看什么 |
| --- | --- |
| 输入快照哈希 | 脚本 `stdout` 的 `input.*.sha256`（三份原始响应） |
| 时区与计划 | `AGENT.md` 的 `schedules`（6 段 + `zone: Asia/Shanghai`）；`task_executions` 的触发时间 |
| 候选产物哈希 | 脚本 `stdout` 的 `report.sha256` / `state.sha256` |
| `run_id` | 触发接口或运行列表返回值；`task_executions.run_id` 对得上 |
| 业务断言 | `digest.json` 的计数、`followups`、`unconfirmed`（照「期望业务结果」核对） |
| 通知次数与故障读数 | `schedule_deliveries` 的 `status`（`sent` 才算送达）；`tool_invocations` 里的失败行 |

## 证据档位

| 档位 | 结论 | 依据 |
| --- | --- | --- |
| 夹具可复跑 | **已验证** | `scripts/test_repo_digest.py` 16 个用例；同一夹具两次重跑产物哈希一致 |
| 技能 L1/L2/L3 读取 | **夹具用例覆盖** | `digest.json` 与技能文档逐条对应；L1 卡片断言与 `read_file` 审计在 `TestUS14Demos_Fixture/repo_digest`（需要真实模型，本轮未跑） |
| 真实 GitHub MCP + PAT 实跑 | **已验证（2026-10-27 本机实跑，github-mcp-server 2.0.3）** | 三份列表落盘、脚本退出 0，PR 10 / Issue 10 / Release 10 计入；`comments` 省略语义与 `state` 大小写见「已知限制」 |
| `deliver_to` 实际送达 | **未验证** | 需要真实通知接收方；基线不带 `deliver_to`，`sent` 未取得 |

## 已知限制

- **实测的服务端形状**（github-mcp-server 2.0.3）：Issue 列表的 `state` 是大写
  GraphQL 枚举（`OPEN` / `CLOSED`），脚本已做大小写归一化；Issue 的 `comments` 在
  0 条评论时**省略该键**。脚本坚持「缺失 ≠ 0」，所以这些 Issue 会进「待确认」，
  `unanswered_open` 对它们不会触发——宁可少报，也不猜 0。要恢复这个信号，需要换用
  能返回 0 的字段或工具（本 Demo 未做）。
- **同一页数据**：脚本只处理模型落盘的那一页；GitHub 分页更早的条目不在本次范围，
  必要时在 `AGENT.md` 里改成多页拉取（脚本不负责翻页）。真实仓库一页可达数百 KB，
  所以 `AGENT.md` 用 `perPage: 10` + `fields` 控制落盘体量。
- **受阻项导致的重复**：见「游标与宁可重复」一节——同源更晚的条目可能被重复计入
  一次，日报会写明条数。
- **不判断优先级**：脚本只按固定规则筛待跟进，哪件更急仍由人读日报判断。
- **不自动补发**：投递失败只留证据，补发由人用保存的 `digest.md` 手动执行，并注明
  原 `run_id`。
