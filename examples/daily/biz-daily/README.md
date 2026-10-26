# D3 biz-daily —— 经营日报（只读 SQLite）

每天从一份**只读** SQLite 样例库汇总销售订单：先报数据质量（缺失 / 重复 / 非法值
逐类计数），再报经营指标（毛额 / 退款 / 挂起 / 净额 / 分区 / 客单价）。数字全部由
脚本算，模型只负责取数、解释与写日报。

这个 Demo 的看点是：**坏数据不被算法悄悄吞掉**——三类异常互斥计数、逐条点名，
指标只统计有效行；同时**零通知**，日报只落在文件与运行记录里。

> 档位：Demo（不是模板）。**夹具与脚本可复跑已验证**；**样例库生成已验证**；
> **真实 DBHub + Node 实跑未验证**（本轮无外部授权）；**DBHub 工具名未实测**；
> **自动触发 / 人工补跑未在该实例取证**。证据档位见文末。

## 业务目标

回答「这张订单表今天能不能用、生意做得怎么样」。要点：

- 24 行固定输入里刻意埋了 3 缺失 / 2 重复 / 4 非法值 / 15 有效，四类之和恒等于
  总行数（互斥，不重复计数）；
- 坏行**不进**指标：缺金额的行不被当成 0 计入，重复单号以**首次出现**为准，
  非法值整行剔除并给出原因码；
- 一条有效行都没有时，金额字段是 `null` 而不是 `0.00`——「没算出来」和「零收入」
  是两件事；
- 明细示例超过上限时打 `truncated: true` 并给出 `{"shown", "total"}`；**计数永远
  覆盖全部行**，截断只影响展示。

## 工具清单与 `tools:` 声明

| 工具 | 来源 | 用途 |
| --- | --- | --- |
| `execute_sql` | DBHub MCP（`dbhub`，`@bytebase/dbhub` 1.5.0） | 连库自检与固定 SQL，取 `orders` 明细 |
| `read_file` | 内置 | 读脚本产物 `daily.json` |
| `write_file` | 内置 | 落盘查询结果 `orders.json`、写 `daily.md` |
| `shell` | 内置 | 运行 `scripts/biz_daily.py`（解释器是 `AGENT.md` 里已替换的绝对路径，须在 `shell.allowed_commands` 里整串登记） |

`AGENT.md` 的声明与上表一致；`notify` **不在**声明里——基线 `notify_mode: none`，
工具面也不会补。`shell` 同样是必需项：脚本跑不起来就没有可信数字。

**工具面按实测对齐（2026-10-26，`@bytebase/dbhub` 1.5.0，Windows）**：

- `dbhub.toml` 的 `[[tools]]` 只列 `execute_sql` 时，`tools/list` **只报这一个工具**；
  `search_objects` / `list_tables` 均不存在。入参只有一个键：`{"sql": "..."}`。
- 调用成功时结果是 MCP 文本块，内容为
  `{"success": true, "data": {"statements": [{"sql": "...", "rows": [...], "count": N}],
  "source_id": "default"}}`；脚本按这个结构解包（也接受裸数组与 `{"rows": [...]}`）。
- 只读由配置强制：`UPDATE` / `DELETE` 返回
  `{"success": false, "code": "READONLY_VIOLATION"}`，不写库。
- 目录条目 `dbhub` 的 `verified` 仍为 `false`：验证时间 / 平台字段在本 US 未登记，
  真实证据以本节实测记录与 `SETUP.md` 为准。

## 内置域名白名单

**无**。本 Demo 不声明 `http_get`，Basalt 自身不发任何出站 HTTP 请求；数据经
DBHub 子进程从**本地文件**读取，不需要在「执行环境」页添加任何域名。

## MCP 自身出网范围

DBHub 是独立的 Node 子进程，理论上它自己可以出网（本配方没给它任何远程 DSN，
只指向本地 `.db` 文件），这**不受** Basalt 的域名白名单约束。Basalt 只约束自己
发出的 HTTP 请求，不代管子进程的网络策略——给 MCP server 授权时要自己心里有数。

## 启动程序与版本

目录条目 `dbhub` 的配方（`verified: false`，warnings `package_entry_unverified` /
`sqlite_sample_only`）：

```text
node <absolute-path-to-dbhub-entry> --transport stdio \
  --config <absolute-path-to-dbhub.toml>
```

`dbhub.toml` 同时给出 DSN 与只读工具面（完整片段见 `SETUP.md` 第 2 节）：

```toml
[[sources]]
id = "default"
dsn = "sqlite:///<absolute-path-to-sample.db>"

[[tools]]
name = "execute_sql"
source = "default"
readonly = true
max_rows = 500
```

- `node` 必须在 `shell.allowed_commands` 里——stdio MCP 的启动同样要过命令授权；
- 1.5.0 **没有** `--readonly` 参数：只读与限行只由 `dbhub.toml` 的 `[[tools]]` 表达，
  传旧参数会直接启动失败；
- 上面的命令、`dbhub.toml` 形态、`execute_sql` 入参与返回结构、以及
  `READONLY_VIOLATION` 拒绝都是 **2026-10-26 实测**（Windows + Node + DBHub 1.5.0）；
- Node.js 版本要求（20+）来自 dbhub 包说明；本机实测版本记录在 `SETUP.md`。

## 凭据

**无**。样例库是本地文件，DBHub 按 `dbhub.toml` 的只读工具面打开；不需要任何令牌或密码。
`SETUP.md` 里也不会出现明文凭据。

## 依赖与固定版本

| 依赖 | 版本 | 用途 |
| --- | --- | --- |
| Python | 3.12（标准库即可） | 运行 `scripts/biz_daily.py` 与 `examples/shared/scripts/make_sample_db.py`（绝对路径登记在 `shell.allowed_commands`） |
| Node.js + `dbhub` 包 | 20+ / 待实测 | 只读 SQLite 查询（stdio MCP） |
| SQLite | 由 Python 标准库 `sqlite3` 提供 | 样例库文件格式 |
| Basalt | v0.1.0 | 运行底座（调度、MCP、审计） |

脚本零第三方依赖：`biz_daily.py` 只用 `argparse` / `hashlib` / `json` / `os` /
`sys` / `tempfile` / `datetime` / `decimal` / `pathlib`；建库脚本只用 `sqlite3`。
金额用 `Decimal` 计算，落盘保留两位小数。

## 读写文件范围

| 路径 | 谁写 | 说明 |
| --- | --- | --- |
| `orders.json`（Agent 根目录） | 模型（`write_file`） | `execute_sql` 返回的**整段** JSON 原样落盘（含 `success` / `data.statements`），**不得**改字段、改数字、改顺序 |
| `runs/<时间戳>/daily.json` | 脚本（原子替换） | 机器可读结果；父目录由脚本创建 |
| `runs/<时间戳>/daily.md` | 模型（`write_file`） | 日报正文，数字全部取自 `daily.json` |
| `scripts/` | 只读区 | 模型改不动脚本；这是「数字由脚本算」的前提 |

`write_file` **不自动创建父目录**：根目录的 `orders.json` 没有这个问题；`daily.md`
写在脚本刚建好的 `runs/<时间戳>/` 里。

## 通知路径

- `notify_mode: none`：这一轮**零通知**，工具面里也没有 `notify`。
- 日报只留在 `runs/<时间戳>/daily.md` 与运行记录里；要看结果去管理台的运行详情或
  直接读文件。
- 六段证据里的「通知次数」对 D3 就是**必须为 0**：`schedule_deliveries` 没有行、
  `tool_invocations` 里 `notify` 计数为 0。**不得**为了让日报「发出去」改写
  `AGENT.md` 的 `notify_mode` 或绕过调度。

## 生效方式

改 `AGENT.md`、`mcp_servers.yaml` 或 `shell.*` 白名单后**必须重启** Basalt；后两类
不热加载。管理台显示「待发布 / 待重启」不等于已生效。

## 固定输入

`fixtures/` 两份文件都是**虚构**数据（不含真实客户、订单或凭据）：

| 文件 | 内容 | sha256 |
| --- | --- | --- |
| `fixtures/schema.sql` | `orders` 表 DDL（**故意不加** UNIQUE / CHECK / NOT NULL） | `2136d7c0589f4fcb9a3f39f6f4e3c583221ab520343b8836b9e02f64d590ef53` |
| `fixtures/rows.json` | 24 行订单，三类异常刻意埋入 | `d64d446df5c4ccf4044fc01999ab05e71de37a0f97cff817d439ac7d4f6cfaf6` |

**约束故意缺席**：约束会把坏数据挡在入库之前，质量检查就永远只能报 0。SQLite 的
动态类型让 `amount REAL` 列也能存下文本 `"N/A"`——这正是一类要抓的异常。`.db` 是
生成物，**不进版本库**；用生成脚本现做：

```sh
python examples/shared/scripts/make_sample_db.py \
  --schema examples/daily/biz-daily/fixtures/schema.sql \
  --rows   examples/daily/biz-daily/fixtures/rows.json \
  --out    <仓库外路径>/biz-sample.db --force
```

生成脚本会校验 `{"table","columns","rows"}` 形态、拒绝未声明列、验证入库行数，并
打印 db / schema / rows 的 SHA-256。实测：24 行全部入库，三类异常在库里逐一可见
（`region IS NULL` 1 行；`amount` 列同时存在 text / real / null 三种存储类；重复
单号 `SO-1002` / `SO-1003` 各 2 行；`status` 有空串；日期 `2026-13-40` 原样保留）。
同一机器两次生成逐字节一致（`de3a167d37c828ed12d3c79486853184c9901f289b4be360ebbb0f96795ba235`）；
**跨机器 / 跨 SQLite 版本未验证**——证据里引用的哈希以你自己生成的库为准。

## 数据质量三类与固定 SQL

### 固定 SQL（模型不得改写）

```sql
SELECT row_id, order_id, order_date, region, channel, amount, status, note
FROM orders ORDER BY row_id;
```

模型**不得**加 `WHERE`、**不得**只取正常行、**不得**在 SQL 里做聚合——漏掉的行就
是证据。聚合与判定全部在脚本里完成。

### 判定顺序与互斥（`missing > duplicate > invalid`）

| 类别 | 判定 | 说明 |
| --- | --- | --- |
| **缺失** | 任一必填字段为空（键不存在 / `null` / 空串） | 必填：`order_id` / `order_date` / `region` / `amount` / `status`；**不补零、不猜值** |
| **重复** | `order_id` 此前已出现过 | 以**首次出现**为准，后续副本整行算重复；缺字段的行不参与占号 |
| **非法值** | 字段都在，内容不合约定 | 原因码：`amount_not_numeric` / `amount_not_finite` / `amount_negative` / `order_date_unparseable` / `status_unknown` |
| **有效** | 以上都不命中 | 唯一进入指标的行 |

一行只归一类（`missing + duplicate + invalid + valid == total` 恒成立）。优先级固定：
既缺字段又重复的行记「缺失」；既重复又金额非法的行记「重复」。

### 夹具里的三类异常（逐条对应一行）

| 类别 | 行 | 原因 |
| --- | --- | --- |
| 缺失 | r7 / r12 / r18 | `region: null` / `amount` 键缺失 / `status: ""` |
| 重复 | r9 / r20 | `SO-1003`（首次 r3，金额 2450 vs 2600）、`SO-1002`（首次 r2，800.5 vs 950） |
| 非法值 | r5 / r11 / r16 / r22 | `amount: "N/A"` / `amount: -300.0` / `status: "cancelled"` / `order_date: "2026-13-40"` |

## 脚本输入 / 输出契约

```sh
python scripts/biz_daily.py --orders orders.json
```

| 参数 | 必填 | 缺省 | 说明 |
| --- | --- | --- | --- |
| `--orders` | 是 | — | `execute_sql` 响应的落盘 JSON |
| `--output` | 否 | `runs/<YYYYMMDDTHHMMSS>/daily.json` | 报告路径；父目录自动创建 |
| `--now` | 否 | 本机当前时间 | 运行时钟，**必须带时区偏移**；显式给出可复跑 |
| `--max-examples` | 否 | `5` | 每类异常最多展示的明细条数（`0` = 只计数不列明细） |

**输入**：DBHub `execute_sql` 的真实返回
`{"success": true, "data": {"statements": [{"rows": [...], ...}]}}`，或更朴素的裸数组、
`{"rows": [...]}` / `{"items": [...]}`。`{"success": false, ...}`（如 `READONLY_VIOLATION`）
与多语句结果都按**错误**处理，不折算成「零行」。任何输入不合规 → **非零退出，一个
字节都不写**（连输出父目录都不创建）。

**输出 `daily.json` 字段**：

| 字段 | 内容 |
| --- | --- |
| `quality.total` / `quality.counts` | 总行数；四类互斥计数 |
| `quality.missing` | `by_field`（逐字段条数）+ `examples[]` |
| `quality.duplicate` | `order_ids`（排序）+ `examples[]`（含 `first_row`） |
| `quality.invalid` | `by_field` / `by_reason` / `examples[]` |
| `quality.truncated` / `quality.truncation` | 是否截断；各类 `{"shown", "total"}` |
| `metrics` | `computed` / `valid_orders` / `status_counts` / `gross_amount` / `refunded_amount` / `pending_amount` / `net_amount` / `average_paid_amount` / `currency`（固定 `CNY`）/ `by_region` |
| `message` | 一句话结论（日报结论段照抄） |
| `input` | `path` + `sha256`（输入快照哈希） |

`stdout` 输出一段摘要 JSON（输入 / 报告路径与 SHA-256、计数、净额、结论），可直接
作为证据里的「输入快照哈希 / 候选产物哈希」。

## 输出预算与截断标记

有两个不同的上限，都有标记，**不得**混写：

1. **工具输出预算**：Basalt 对单次工具输出有硬上限（`budget.max_output_bytes`，
   缺省 256 KB），超限时在读取中截断并在工具结果上标 `truncated: true`。本 Demo 的
   取数结果与脚本输出都远小于该上限，正常不应触发。
2. **业务明细上限**：`--max-examples` 只限制每类**示例条数**。超限时 `daily.json`
   的 `quality.truncated=true`，`quality.truncation` 给出每类 `{"shown", "total"}`；
   日报对应段写「仅列前 N 条，共 M 条（其余见 `daily.json`）」。

**计数不受任何上限影响**：`counts` 永远覆盖全部行。截断只影响展示。

## 期望业务结果（夹具复跑）

命令与固定时钟：

```sh
cd examples/daily/biz-daily
python scripts/biz_daily.py --orders fixtures/rows.json \
  --output /tmp/biz/daily.json --now 2026-10-25T09:00:00+08:00
```

实测读数：

| 断言 | 期望 |
| --- | --- |
| `quality.total` / `counts` | `24`；`valid=15`、`missing=3`、`duplicate=2`、`invalid=4` |
| `quality.missing.by_field` | `region=1`、`amount=1`、`status=1` |
| `quality.duplicate.order_ids` | `["SO-1002", "SO-1003"]`；首次分别在第 2 / 3 行 |
| `quality.invalid.by_reason` | `amount_not_numeric=1`、`amount_negative=1`、`status_unknown=1`、`order_date_unparseable=1` |
| `quality.truncated` | `false`（缺省上限 5 下三类示例都只有 2~4 条） |
| `metrics.status_counts` | `paid=9`、`refunded=3`、`pending=3` |
| `metrics` 金额（CNY） | 毛额 `11890.50`、退款 `1190.00`、挂起 `2170.00`、净额 `10700.50`、客单价 `1321.17` |
| `metrics.by_region` | 华东 6 单 `3740.00`、华北 5 单 `4290.50`、华南 2 单 `1990.00`、西南 2 单 `680.00`（分区净额之和 == 总净额） |
| `message` | `本次 24 行中有效 15 行（缺失 3 / 重复 2 / 非法 4），净额 10700.50 CNY` |

同一命令、同一时钟两次运行，`daily.json` 逐字节一致：

| 产物 | sha256 |
| --- | --- |
| `daily.json` | `67814bc79325d095ae812c74fa8788c209b664cdf82b8baebba226fc4a61c876` |

> 报告哈希包含 `input.path`（`fixtures/rows.json`），复现时保持同一相对路径与
> 运行目录；换路径会改哈希，这不是结果漂移。

`--max-examples 1` 时：`quality.truncated=true`，`quality.truncation` 为
`{"missing": {"shown": 1, "total": 3}, "duplicate": {"shown": 1, "total": 2},
"invalid": {"shown": 1, "total": 4}}`，而 `counts` 不变。

## 六段证据怎么取

| 段 | 看什么 |
| --- | --- |
| 输入快照哈希 | 脚本 `stdout` 的 `input.sha256`（`orders.json`）；样例库另有生成脚本给的 DB 哈希 |
| 时区与计划 | `AGENT.md` 的 `schedules`（6 段 + `zone: Asia/Shanghai`）；`task_executions` 的触发时间 |
| 候选产物哈希 | 脚本 `stdout` 的 `report.sha256` |
| `run_id` | 触发接口或运行列表返回值；`task_executions.run_id` 对得上 |
| 业务断言 | `daily.json` 的四类计数、原因码、指标（照「期望业务结果」核对） |
| 通知次数与故障读数 | `schedule_deliveries` 对该任务 **0 行**、`notify` 调用 **0 次**；`tool_invocations` 里的失败行 |

## 失败样例

| 场景 | 期望行为（括号内为实测 `stderr`） |
| --- | --- |
| 输入文件不存在 | 非零退出，不写报告（`biz_daily: 读取查询结果失败: missing.json: [Errno 2] No such file or directory: 'missing.json'`） |
| 顶层不是数组 / 也不是 `rows` / `items` 形态 | 非零退出（`biz_daily: 查询结果必须是行数组，或 {"rows": [...]} / {"items": [...]} 形态的列表响应`） |
| 行不是对象 | 非零退出（`biz_daily: 第 1 行不是对象，实际 str`） |
| `--now` 不带时区偏移 | 非零退出，不写报告（`biz_daily: --now 必须是带时区偏移的 ISO 8601 时间戳: '2026-10-25T09:00:00'`） |
| `--max-examples` 为负 | 非零退出（`biz_daily: --max-examples 必须 ≥ 0，实际 -1`） |
| 输入为空数组 | 正常写报告：`counts` 全 0、`computed=false`、金额为 `null`，`message` 为「本次 0 行：输入为空，未计算任何指标」 |
| 全部行都是坏行 | 正常写报告：`computed=false`、金额为 `null`，`message` 说明「没有可用行（缺失 / 重复 / 非法）」 |
| DBHub 不可达（无输出可落盘） | **不运行脚本**；`daily.md` 写「数据库不可达 / 数据缺失」+ 观测时间，不补造行 |
| `shell` 拒绝脚本路径 / 解释器未放行 | Run 可能仍 `completed`（失败结果回填给模型）；业务断言因缺 `daily.json` 判失败，不算通过 |
| 调度成功但「通知期望」被写进验收 | 该任务 `schedule_deliveries` 应始终 0 行——D3 是零通知 Demo，出现 `sent` 即判定不通过 |

## 证据档位

| 档位 | 结论 | 依据 |
| --- | --- | --- |
| 夹具可复跑 | **已验证** | `scripts/test_biz_daily.py` 22 个用例全绿（`Ran 22 tests … OK`）；同一夹具两次重跑 `daily.json` 哈希一致 |
| 三类异常逐类计数 | **已验证** | 夹具断言 `{valid:15, missing:3, duplicate:2, invalid:4}` 与互斥不变量；三处逆改探针（缺失补 0 / 取消去重 / 静默截断）分别变红 |
| 样例库生成 | **已验证** | `make_sample_db.py` 生成 24 行；SQL 直查确认三类异常都在库里；重复生成逐字节一致（同一机器） |
| 真实 DBHub + Node 实跑 | **已验证（本机）** | 2026-10-26：`@bytebase/dbhub` 1.5.0 + 真实模型跑通一轮（24 行 → 15 有效 / 3 缺失 / 2 重复 / 4 非法）；工具面与只读拒绝见「工具清单」节实测记录 |
| 自动触发 / 人工补跑 | **未验证** | 需要真实实例；本轮只交夹具与脚本证据 |
| 零通知 | **配置固定，运行未取证** | `notify_mode: none` 且工具面无 `notify`；`schedule_deliveries` 为 0 的行为需按 `SETUP.md` 在实例上核对 |

## 已知限制

- **DBHub 版本被钉死**：只读与限行只由 `dbhub.toml` 的 `[[tools]]` 生效（1.5.0 已移除
  `--readonly` 参数）；换版本要先按 `SETUP.md` 重核 `tools/list` 与拒绝语义。
- **样例库是桩数据**：24 行固定数据用于复现三类异常；换成真实业务库时，SQL 与脚本
  字段约定要同步改，脚本的必填字段 / 状态枚举是固定的。
- **不做跨行修复建议**：重复单号只告警并保留首次出现，不判断哪一行「才是对的」；
  金额非法只报原因码，不做换算（如把「1,200 元」解析成 1200）。
- **不判断业务含义**：负数金额一律算非法；预售冲正、退款回冲等真会出现的负数场景
  需按业务改写规则，脚本不内置猜测。
- **`.db` 不进版本库**：样例库要在目标机现生成；跨机器的字节一致性不承诺，证据里
  引用自己生成的哈希。
