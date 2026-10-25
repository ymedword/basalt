# D5 industry-brief —— 行业 / 竞品简报（双 MCP + 来源表）

每天做一份行业简报，能力拆成两段：**Brave Search MCP 只负责检索**（给候选 URL），
**Fetch MCP 只负责抓正文**（给可引用依据）。Python 脚本把两段结果合成一张来源表，
把来源分成 **已抓正文（`citable=true`，可作结论来源）** 与 **仅线索（`citable=false`，
只能列在「线索」表）** 两类。

这个 Demo 的看点是：**未抓正文的 URL 不得作为结论来源**——不是靠提示词自觉，而是
`--verify-report` 按 `brief.json` 的可引用集合逐条检查简报里的 `[来源: <url>]`：线索
被当成结论引用就非零退出，改到合格为止。

> 档位：Demo（不是模板）。**业务脚本 24 个单测 / 夹具 → 脚本全链路（本机 Windows）
> 已验证**；**真实 Brave / Fetch MCP 实跑未验证**（Brave key 与外部抓取均未授权）；
> **真实模型驱动的完整 Agent Run 未验证**；**`deliver_to` 实际送达未验证**（无真实
> 接收方）。证据档位见文末。

## 业务目标

回答「这个主题这两天有哪些新动态，哪些能作为结论依据」。要点：

- **双 MCP 各管一段**：检索结果只是候选 URL；只有 Fetch 抓到正文的页面才算依据。
  没抓、抓失败、正文为空的 URL 进「线索」表，**不得**写进结论；
- **来源去重**：同一 URL 出现在多个查询里只算一个来源，`queries` 合并；去重数 =
  命中条数 - 去重后来源数，两个数字都进 `counts`，可复算；
- **日期只认 `page_age` 的 ISO 前缀**：取日期部分；缺失或不可解析一律「未确认」，
  不解析 `"3 days ago"` 这类自然语言，也不拿访问时间冒充发布日期；
- **访问时间取本轮运行时钟**（`--now`）：Fetch 结果不回传逐页时间戳，逐页时间只能
  靠模型口述、不可核对，所以统一记 Run 时钟；
- **零来源不编造**：`counts.sources = 0` 时简报只写「本次未获取到来源：不产出任何
  结论（不编造）」。

## 工具清单与 `tools:` 声明

| 工具 | 来源 | 用途 |
| --- | --- | --- |
| `brave_web_search` | `brave-search` MCP（外部，**候选名**） | 三条查询的网页检索，返回结果 JSON |
| `fetch` | `fetch` MCP（外部，**候选名**） | 逐 URL 抓正文（成功给正文，失败给报错） |
| `read_file` | 内置 | 读脚本产物 `brief.json` |
| `write_file` | 内置 | 落盘 `search.json` / `pages.json` / `brief.md` |
| `shell` | 内置 | 运行 `scripts/industry_brief.py`（解释器受 `shell.interpreters` 约束） |

`AGENT.md` 的声明与上表一致；`notify` **不在**声明里——基线 `notify_mode: none`，
投递由 `deliver_to` 系统代投承担（见「通知路径」）。`shell` 是必需项：脚本跑不起来
就没有可信来源表。

**MCP 工具名未实测**：两个名字按各自公开文档填写（MCP 目录条目 `brave-search` 与
`fetch` 都是 `verified: false`）。连接后若加载报 `tool not registered: <name>`，按
实际 `tools/list` 改 `AGENT.md` 的 `tools:`，**不要**改脚本。

## 内置域名白名单

**无**。本 Demo 不声明 `http_get`，Basalt 自身不发任何出站 HTTP 请求；检索与抓取都由
MCP 子进程完成。因此不需要在「执行环境」页添加任何域名。

## MCP 自身出网范围

- `brave-search` 子进程访问 `api.search.brave.com`；
- `fetch` 子进程按命令抓取**任意** URL。

两者**都不受** Basalt 的域名白名单约束：Basalt 只约束自己发出的 HTTP 请求，不代管
子进程的网络策略。对 stdio 启动程序，Basalt 能约束的只有 `shell.allowed_commands`
（启动命令按整串比对）。所以这个 Demo 把「抓哪些 URL」写进 `AGENT.md` 的步骤：
抓取面在子进程，不在 Basalt——给 MCP 授权时请自己心里有数。

## 启动程序与版本

两条配方来自 MCP 目录（`internal/catalog/data/mcp_catalog.yaml`），`verified: false`：

| server | 命令 | 参数 | env |
| --- | --- | --- | --- |
| `brave-search` | `node` | `["<absolute-path-to-brave-search-mcp-server-entry>"]` | `BRAVE_API_KEY`（必需） |
| `fetch` | `python` | `["-m", "mcp_server_fetch"]` | 无 |

- 包版本、入口路径、工具名与代表性调用**均未实测**；上游分别是
  <https://github.com/brave/brave-search-mcp-server> 与
  <https://github.com/modelcontextprotocol/servers/tree/main/src/fetch>；
- `fetch` 的 `-m` 是**服务端自己的入口方式**，与 `shell` 工具的「解释器通道拒绝
  `-c` / `-m`」不是同一条通道：MCP stdio 启动只检查 `command`（这里是 `python`）
  是否在 `shell.allowed_commands` 里；
- Windows 本机提示：Microsoft Store 的 `python` 是占位程序。要么把 python.org 装的
  Python 放在 PATH 前面（命令仍是 `python`），要么把 `command` 改成实际名字（如 `py`）
  并让 `shell.allowed_commands` 同步——两处必须逐字一致。

## 凭据

| 变量 | 说明 |
| --- | --- |
| `BRAVE_API_KEY` | Brave Search API key（检索权限即可）。`mcp_servers.yaml` 里**只写** `${BRAVE_API_KEY}` 占位，装配期从 Basalt 进程环境取值；缺失或为空直接拒绝装配（fail-closed），错误不回显值 |

Fetch 无凭据。**不得**把 key 写进 `AGENT.md`、`basalt.yaml`、脚本或提交记录。

## 依赖与固定版本

| 依赖 | 版本 | 用途 |
| --- | --- | --- |
| Python | 3.12（标准库即可） | 运行 `scripts/industry_brief.py`；也是 `fetch` MCP 的运行环境（`mcp-server-fetch`） |
| Node | 24.22.0 | 运行 `brave-search-mcp-server` |
| `brave-search-mcp-server` | 待实测 | Brave 检索（入口绝对路径，包版本未核实） |
| `mcp-server-fetch` | 待实测 | 网页正文抓取（包版本未核实） |
| Basalt | v0.1.0 | 运行底座（调度、MCP、`deliver_to`） |

脚本零第三方依赖：`industry_brief.py` 只用 `argparse` / `hashlib` / `json` / `os` /
`re` / `sys` / `tempfile` / `datetime` / `pathlib` / `urllib.parse`。

## 读写文件范围

| 路径 | 谁写 | 说明 |
| --- | --- | --- |
| `search.json`（Agent 根目录） | 模型（`write_file`） | 三条查询的结果**原样**按查询分组落盘；不得删 / 补结果、不得改 URL、不得补 `page_age` |
| `pages.json`（Agent 根目录） | 模型（`write_file`） | 逐 URL 的抓取结果：成功 `status: 200` + 正文原文；失败 `status` + `error` 原文、`content` 留空 |
| `runs/<t0>/brief.json` | 脚本（原子替换） | 来源表、可引用集合、计数、失败清单；父目录由脚本创建 |
| `runs/<t0>/brief.md` | 模型（`write_file`） | 简报正文，数字与 URL 全部取自 `brief.json` |
| `scripts/` | 只读区 | 模型改不动脚本；这是「来源表由脚本算」的前提 |
| `runs/` | 产物 | **不提交**，已在 `.gitignore` |

`write_file` **不自动创建父目录**：根目录两份输入没有这个问题；`brief.md` 写在脚本
刚建好的 `runs/<t0>/` 里（目录名取自 `stdout.report.path` 的父目录）。

## 脚本输入 / 输出契约

```sh
python scripts/industry_brief.py --search search.json --pages pages.json \
  --now 2026-10-25T09:30:00+08:00 --topic "企业级 AI Agent 平台"
```

| 参数 | 必填 | 缺省 | 说明 |
| --- | --- | --- | --- |
| `--search` | 是 | — | Brave 检索结果（`{"queries": [...]}`） |
| `--pages` | 是 | — | Fetch 抓取结果（`{"pages": [...]}`） |
| `--output` | 否 | `runs/<t0>/brief.json` | 产物路径；父目录自动创建 |
| `--now` | 否 | 本机当前时间 | 运行时钟，**必须带时区偏移**；显式给出可复跑 |
| `--topic` | 否 | 空 | 写进 `brief.json`：`topic` |
| `--verify-report` | 否 | — | 校验这份简报正文的引用只指向已抓正文的来源；不合格非零退出 |

**输入**：`search.json` 的每条结果必须有不空的 `title` 与 http(s) 绝对 `url`
（不带用户名 / 密码）；`page_age` 可省略。`pages.json` 的 URL 必须出现在检索结果里，
同一 URL 只出现一次；`status` 缺省 `200`。任何输入不合规 → **非零退出，一个字节都
不写**（连输出父目录都不创建）。

**输出 `brief.json`**：`schema_version`、`agent`、`observed_at`、`topic`、`counts`
（`queries` / `results` / `sources` / `duplicates` / `fetched` / `leads` /
`fetch_failures` / `missing_dates`）、`sources[]`、`citable_urls`、`lead_urls`、
`failures[]`、`message`。每个来源记录：`url` / `title` / `published_at` / `accessed_at` /
`status` / `citable` / `queries` / `content_sha256` / `content_bytes` / `excerpt` / `note`。

`stdout` 是一段 JSON 摘要：输入与报告的路径与 SHA-256、`counts`、`citable_urls`、
`lead_urls`、`message`，可直接作为证据里的「输入快照哈希 / 候选产物哈希」。

**可复现**：`brief.json` 只装业务事实——不含输入文件路径、不含输入文件哈希、不含
`modified_at`。**同一份输入 + 同一个 `--now` ⇒ 逐字节相同的 `brief.json`**；`--now`
不同则 `observed_at` 不同，产物自然不同。要核对输入哈希就抄 `stdout`，不要从报告里找。

## 通知路径（`deliver_to` 系统代投）

- **便携基线不带 `deliver_to`**（也不带任何 UID）：`notify_mode: none`，这一轮零通知，
  工具面里也没有 `notify`。
- 按 `SETUP.md` 在任务上配置 `deliver_to`（值是**渠道 `resource_uid`**，`rs_` 前缀，
  不是渠道名）后：该轮 Run 准入时固定 `notify_mode = none` 并从工具面剔除 `notify`，
  模型只产出正文；**仅当 Run 成功结束**，系统才写 `pending` 行并发送本轮最终正文
  （即 `brief.md`）。
- 投递终态 `sent` / `failed` / `unknown`；非成功 Run 记 `skipped`（按规则未发送）——
  `failed` / `unknown` 都**不算送达**，也**不自动补发**。
- 系统代投与模型 `notify` 互斥，不得通过改写 `AGENT.md` 实现接管；送达也不等于
  端到端恰好一次。

## 生效方式

改 `AGENT.md` 走受控发布后再重启；改 `mcp_servers.yaml` 或 `shell.*` 白名单后
**必须重启** Basalt——后两类不热加载。管理台显示「待发布 / 待重启」不等于已生效。

## 固定输入

`fixtures/` 两份**虚构**数据，URL 全部是 `example.com`，正文为脱敏的行业文本
（不含真实站点、账号或凭据）：

| 文件 | 字节 | sha256 |
| --- | --- | --- |
| `fixtures/search.json` | 3522 | `277a7a8b2e68fad7adfcd6ddb950b90df355a6ece8db86c9923a2b5726b4f039` |
| `fixtures/pages.json` | 5027 | `e4196b4b350834064609a04f8abeb936d85d727aca8cc6684409c8bf2bbed7c7` |

刻意的设计（每条对应一条判据）：

1. **跨查询重复**：报告页出现在查询 1 与查询 2，自托管运行时文章出现在查询 1 与
   查询 3 → 10 条结果去重为 8 个来源，`duplicates = 2`，两处 `queries` 合并。
2. **没有 `page_age`**：厂商对比页缺发布日期键 → `published_at = null`，简报写
   「未确认」，`missing_dates = 1`；**不得**用检索顺序或访问时间补一个日期。
3. **从未抓取正文**：沙箱逃逸复盘页（`/news/sandbox-incident-review`）在检索结果里，
   但本 Run 没抓 → 只能进线索表。
4. **抓取失败**：采用率调查页（`/news/enterprise-agent-adoption`）抓取返回 HTTP 404
   → 上线失败清单、仍是线索；它的发布日期照常从 `page_age` 取。
5. **抓到正文的 5 个页面**：每份记录 `content_sha256`、`content_bytes` 与 200 字
   单行 `excerpt`；访问时间是本轮 `--now`。

## 期望业务结果

本机 Windows 实测命令（`--output` 指向仓库外临时目录，仓库里不留产物）：

```sh
python scripts/industry_brief.py \
  --search fixtures/search.json --pages fixtures/pages.json \
  --output <tmp>/brief.json --now 2026-10-25T09:30:00+08:00 \
  --topic "企业级 AI Agent 平台"
```

| 断言 | 期望值 |
| --- | --- |
| `counts` | `queries=3`、`results=10`、`sources=8`、`duplicates=2`、`fetched=5`、`leads=3`、`fetch_failures=1`、`missing_dates=1` |
| 守恒 | `duplicates = results - sources`；`fetched + leads = sources` |
| `message` | `本次检索 3 个查询，命中 10 条结果（去重 2 条），来源 8 个：已抓正文 5 个、仅线索 3 个` |
| `citable_urls`（5 个，URL 升序） | `/analysis/audit-trail-design`、`/blog/runtime-cost-controls`、`/blog/self-hosted-agent-runtime`、`/guides/tool-sandbox-patterns`、`/reports/agent-platform-observability-2026` |
| `lead_urls`（3 个） | `/news/enterprise-agent-adoption`（404）、`/news/sandbox-incident-review`（未抓）、`/vendor/agent-os-comparison`（未抓、无日期） |
| `failures` | 1 条：`enterprise-agent-adoption` / `HTTP 404 Not Found` |
| `brief.json` sha256 | `86dbe1dd26960353a2b890aab0d4ec585d30ef3d59349923081c96847f2db170` |

同一输入、同一 `--now` 连跑两次，`brief.json` **逐字节相同**（实测）；把 `--now`
改成 `09:31` 后产物不同（`observed_at` 与 `accessed_at` 都变）。

**引用自检的三次实测**（同一份 `brief.json`）：

| 简报内容 | 退出码 | `stderr` |
| --- | --- | --- |
| 引用已抓来源（`/reports/agent-platform-observability-2026`） | 0 | 空 |
| 引用线索 URL（`/news/enterprise-agent-adoption`） | 1 | `industry_brief: 引用了未抓正文的 URL（只能作线索）：https://example.com/news/enterprise-agent-adoption` |
| 有来源但全文没有任何 `[来源: <url>]` | 1 | `industry_brief: 简报没有任何「[来源: <url>]」标注：结论必须逐条可追溯到已抓正文的来源` |

## 失败样例

**业务脚本的输入拒收**（`rc=1`，`stderr` 原文实测；全部零写入、不建父目录）：

```text
industry_brief: Brave 检索结果必须是 {"queries": [...]} 形态的对象
industry_brief: queries 第 1 项的 query 必须是非空字符串
industry_brief: 查询 'q' 缺少 results 数组
industry_brief: 查询 'q' 的第 1 条结果缺少 title
industry_brief: 查询 'q' 的第 1 条结果的 url 必须是 http(s) 绝对地址：'ftp://example.com/a'
industry_brief: 查询 'q' 的第 1 条结果的 url 不得包含用户名或密码：'https://u:p@example.com/a'
industry_brief: pages 第 1 项的 URL 不在检索结果里：https://example.com/other（混入了别的 Run？）
industry_brief: --now 必须是带时区偏移的 ISO 8601 时间：'2026-10-25T09:30:00'
```

其余拒收（`缺少 queries 数组` / `缺少 pages 数组` / `同一 URL 出现多次` /
`status 必须是整数` / `content 必须是字符串`）由单测逐条断言，文案同源。

| 场景 | 期望行为 |
| --- | --- |
| 检索结果为空（`{"queries": []}`） | 不报错：`sources=[]`、`message` 为「本次未获取到来源：不产出任何结论（不编造）」，仍写出 `brief.json` |
| 抓取失败 | 该 URL 进 `failures` 与线索表，`note` 为「抓取失败：<原因>」；**不得**标成 fetched |
| 抓取成功但正文为空 | 同抓取失败（`note` 为「抓取失败：正文为空」） |
| 简报引用了线索 URL | `--verify-report` 非零退出；**先改写简报再重跑**，不得手改 `brief.json` |
| Brave / Fetch 不可达 | 简报写「检索不可达 / 数据缺失」+ 观测时间，不补造来源；不反复重试 |
| `shell` 工具拒绝脚本路径 / 解释器未放行 | Run 可能仍 `completed`（失败结果回填给模型）；业务断言因缺 `brief.json` 判失败，不算通过 |
| Run 成功但投递失败 | `schedule_deliveries` 记 `failed` / `unknown`，**不算送达**；不自动补发，`brief.md` 仍可查 |

## 六段证据怎么取

| 段 | 看什么 |
| --- | --- |
| 输入快照哈希 | 脚本 `stdout` 的 `input.search.sha256` / `input.pages.sha256` |
| 时区与计划 | `AGENT.md` 的 `schedules`（6 段 + `zone: Asia/Shanghai`）；`task_executions` 的触发时间 |
| 候选产物哈希 | `stdout.report.sha256` |
| `run_id` | 触发接口或运行列表返回值；`task_executions.run_id` 对得上 |
| 业务断言 | `brief.json` 的 `counts` / `citable_urls` / `lead_urls` / `failures`（照「期望业务结果」核对） |
| 通知次数与故障读数 | `schedule_deliveries.status`（`sent` 才算送达）；`tool_invocations` 里 Brave 与 Fetch 各至少一条、失败行原文 |

## 证据档位

| 项 | 状态 | 证据 |
| --- | --- | --- |
| 脚本单测 | **已验证** | `py -3 -B -m unittest discover -s scripts`：`Ran 24 tests`，`OK` |
| 夹具 → 脚本全链路 | **已验证（本机 Windows）** | `brief.json` sha256 `86dbe1dd…`；同输入同 `--now` 两次逐字节相同 |
| 引用自检判别力 | **已验证** | 已抓来源 rc 0；引用线索 / 无引用均 rc 1（`stderr` 见上表） |
| 逆改点（红态） | **已验证** | ① 把线索也标成可引用 → 线索不可引用用例打红；② 去掉跨查询去重 → 去重计数打红；③ 让自检放过线索 → 引用校验用例打红（仓库外探针副本，用毕即删） |
| 真实 Brave / Fetch MCP 实跑 | **未验证** | 外部授权未提供；命令、入口、包版本、工具名与代表性调用均未实测 |
| 真实模型驱动的完整 Run | **未验证** | 本轮无模型授权；`AGENT.md` 的步骤顺序与简报模板未实跑 |
| `deliver_to` 实际送达 | **未验证** | 需要真实通知接收方；基线不带 `deliver_to`，`sent` 未取得 |
| 自动触发 / 人工补跑 | **未在该实例取证** | 需要用户环境按 `SETUP.md` 接一次 |

## 已知限制

- **访问时间是 Run 时钟**：Fetch 不回传逐页时间戳，`accessed_at` 是本轮 `--now`，
  不是逐页真实抓取瞬间。
- **日期只认 ISO 前缀**：`page_age` 缺失或不可解析一律「未确认」，不猜。
- **只处理落盘的那一页**：检索分页、查询改写不在脚本范围；要覆盖更多来源，扩
  `AGENT.md` 的查询或抓取范围。
- **抓取失败的页面仍保留标题与日期**（来自检索结果），但正文缺失，只能作线索。
- **不判断重要性**：脚本只按「有没有正文」分可引用 / 线索，哪条更重要由作者读简报判断。
- **不做预算控制**：Brave 计费与速率由外部服务决定。
- **不自动补发**：投递失败用保存的 `brief.md` 手动补发，并注明原 `run_id`。
