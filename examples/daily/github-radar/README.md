# D1 github-radar —— GitHub 开源雷达

每天扫一遍 GitHub 上最近有推送的仓库，给出一份「今天值得看」的项目推荐简报。
跨次去重、星差、窗口裁剪**全部由脚本计算**，模型只负责挑主题、解释和写报告；
首跑没有基线就明说没有基线，不编造增长率。

> 档位：Demo（不是模板）。**夹具可复跑**已交付；**真实 GitHub MCP 实跑未验证**——
> 本轮没有授权 GitHub PAT。证据档位见文末。

## 业务目标

回答「最近有哪些新的相关仓库值得看，比上次多了多少星」。要点：

- 已经推荐过的仓库在窗口内不再重复推荐；
- 星差只在**有基线**时出现（`baseline_stars` → `star_delta`），首跑只有 `baseline_missing`；
- 每次运行的候选数、去重数、裁剪数都要能对上账（`candidates = deduped + trimmed + recommended`）。

## 工具清单与 `tools:` 声明

| 工具 | 来源 | 用途 |
| --- | --- | --- |
| `search_repositories` | GitHub MCP（`github`） | 按主题与推送时间检索仓库 |
| `get_file_contents` | GitHub MCP（`github`） | 取 README 等文件的 `download_url` |
| `http_get` | 内置 | 按 `download_url` 读 README 正文 |
| `write_file` / `read_file` | 内置 | 落盘搜索结果、读脚本产物 |
| `shell` | 内置 | 运行 `scripts/github_radar.py`（解释器受 `shell.interpreters` 约束） |
| `notify` | 内置（受控装配） | **基线不装配**：`notify_mode: none`。SETUP 切到 `allowlist` 后由保存路径自动补上 |

`AGENT.md` 里的声明与上表一致。`shell` 不在最初的候选清单里，但没有它脚本跑不起来——
图 2-8 的主链写的就是 `shell: python scripts/github_radar.py`，这里按主链落地。

**MCP 工具名未实测**：`search_repositories` / `get_file_contents` 按官方
`github-mcp-server` 的公开工具名填写（MCP 目录条目 `github` 的 `verified: false`）。
连接后若加载报 `tool not registered: <name>`，按实际 `tools/list` 改 `tools:` 声明，
**不要**改脚本。

## 内置域名白名单

`http_get` 只访问 `basalt.yaml` / 管理台「执行环境」页里的网络域名白名单。本 Demo 需要：

| 域名 | 用途 |
| --- | --- |
| `github.com` | 仓库页 |
| `raw.githubusercontent.com` | README 原始内容（`download_url` 指向这里） |

这两个是**候选种子**，只在新实例首次启动时写库；已有实例不覆盖，需管理员显式添加。
搜索结果**不会**自动扩充白名单。

## MCP 自身出网范围

`github-mcp-server` 是独立进程，访问 `api.github.com` 等由它自己决定，**不受** Basalt
的域名白名单约束。Basalt 只约束自己发出的 `http_get`，不代管子进程的网络策略。

## 启动程序与版本

目录条目 `github` 的配方：`github-mcp-server stdio`（Go 二进制，在 PATH 上），
环境变量 `GITHUB_PERSONAL_ACCESS_TOKEN`。命令、参数、版本、只读工具名与代表性调用
**均未实测**（`verified: false`）。

## 凭据

| 变量 | 说明 |
| --- | --- |
| `GITHUB_PERSONAL_ACCESS_TOKEN` | 只读细粒度 PAT。配置里只写 `${GITHUB_PERSONAL_ACCESS_TOKEN}` 占位，**不得**落明文 |

## 依赖与固定版本

| 依赖 | 版本 | 用途 |
| --- | --- | --- |
| Python | 3.12（标准库即可） | 运行 `scripts/github_radar.py`（`shell.interpreters`） |
| `github-mcp-server` | 待实测 | GitHub 只读访问 |
| Basalt | v0.1.0 | 运行底座 |

脚本零第三方依赖：只用 `argparse` / `json` / `hashlib` / `tempfile` / `datetime` / `pathlib`。

## 读写文件范围

| 路径 | 谁写 | 说明 |
| --- | --- | --- |
| `github_search.json` | 模型（`write_file`） | MCP 搜索结果的原始落盘，**不得**改数字 |
| `runs/<时间戳>/report.json` | 脚本 | 机器可读结果；目录由脚本创建 |
| `runs/<时间戳>/report.md` | 模型（`write_file`） | 简报正文，数字全部取自 `report.json` |
| `state/seen.json` | 脚本（原子替换） | 观测与推荐历史，模型**不得**手改 |
| `scripts/` | 只读区 | 模型改不动脚本；这是「数字由脚本算」的前提 |

## 通知路径

- 基线 `notify_mode: none` → **零通知**，Run 内不装配 `notify`。
- 按 SETUP 切到 `allowlist` 后：模型在报告写完后调用**一次** `notify`；发送失败保留
  报告与 `run_id`，**不自动补发**（补发由人决定）。
- 通知成功不等于端到端恰好一次；判据看投递记录的终态。

## 生效方式

改 `AGENT.md`、`mcp_servers.yaml` 或 `shell.*` 白名单后**必须重启** Basalt——这些
配置不热加载。管理台会显示「待发布 / 待重启」，那不等于已生效。

## 固定输入

`fixtures/search.json`：六条**虚构**仓库（不含真实项目、账号或凭据），形态与 GitHub
MCP 搜索结果一致（`{"total_count", "incomplete_results", "items": [...]}`）。

两条刻意的设计：

1. 星数最低的 `lumen-labs/tiny-loader` 在 `top 5` 下**会被裁剪**，但它仍然记进
   `observed`——第二次运行才轮得到它出星差。
2. `orbitworks/fleet-scheduler` 带 `readme_url`，且分支是 `trunk` 而不是 `main`：
   提醒你不要假设默认分支，URL 一律取 MCP 响应的 `download_url`。

固定输入哈希（sha256）：`7117da7bc7ebf3c2a286399e93ce1e4c79bc069d1730df65648580b15a1455b2`。

## 脚本输入 / 输出契约

```sh
python scripts/github_radar.py --input github_search.json --state state/seen.json
```

| 参数 | 必填 | 缺省 | 说明 |
| --- | --- | --- | --- |
| `--input` | 是 | — | 模型落盘的搜索结果 JSON |
| `--state` | 是 | — | `state/seen.json`；存在则读，报告写成功后原子替换 |
| `--output` | 否 | `runs/<时间戳>/report.json` | 报告路径；父目录自动创建 |
| `--now` | 否 | 本机当前时间 | 运行时钟 t0，**必须带时区偏移**（如 `2026-10-25T08:30:00+08:00`）；显式给出可复跑 |
| `--top` | 否 | `5` | 每个主题保留条数 |
| `--window-days` | 否 | `30` | 去重窗口；窗口内已推荐的不再推荐。`0` = 不抑制 |

**输入**（模型写）：裸数组，或 `{"items": [...]}` 形态。每条必填 `full_name`
（`owner/repo`）、`stars`（或 `stargazers_count`）、`pushed_at`（ISO 8601）；
可选 `html_url`（缺省按 `https://github.com/<full_name>` 推导）、`description`、
`readme_url`（必须是 http(s) 地址，取自 MCP 的 `download_url`）。同一响应内重复的
`full_name` 合并为一条，保留星数较高的。任何字段不合规 → **非零退出**，一个字节都不写。

**输出 `report.json`**：

| 字段 | 说明 |
| --- | --- |
| `observed_at` | 本次观测时间（带偏移的 ISO 8601） |
| `source.status` / `source.candidates` | `ok` / `empty`；候选仓库数 |
| `baseline_date` / `baseline_missing` | 首次运行 `null` / `true` |
| `window.top` / `window.days` | 本次生效的裁剪与去重参数 |
| `counts` | `candidates` / `deduped` / `trimmed` / `recommended`，四者和恒等式成立 |
| `items[]` | 本次推荐：`full_name`、`stars`、`pushed_at`、`url`，可选 `description`、`readme_url`；**有基线时**才有 `baseline_stars` 与 `star_delta` |
| `message` | 人可读结论；空结果写「本次无新增」 |

**状态 `state/seen.json`**：`observed`（每个仓库的 `first_seen` / `last_seen` /
`last_stars`）、`recommended`（窗口内的推荐记录，出窗即淘汰）、`baseline_date`。

**写序**：先写 `report.json`，成功后才原子替换 `seen.json`。两步之间崩溃只会导致
「下次可能重复推荐」，不会导致「数据没写成却记成已推荐」。报告里**不**复制一份状态，
避免两处副本漂移。

`stdout` 输出一段 JSON 摘要（输入 / 报告 / 状态的路径与 SHA-256、计数、结论），
可直接作为六段证据里的「输入与产物哈希」。

## 期望业务结果（夹具复跑）

命令与固定时钟：

```sh
python scripts/github_radar.py --input fixtures/search.json \
  --state /tmp/seen.json --output /tmp/report.json --now 2026-10-25T08:30:00+08:00
```

| 断言 | 期望 |
| --- | --- |
| `baseline_missing` | `true`（首跑，无基线） |
| 任何条目出现 `star_delta` | **不出现** |
| `counts` | `candidates=6`、`deduped=0`、`trimmed=1`、`recommended=5` |
| `items` 顺序 | 按星数降序：`orbitworks/fleet-scheduler`(1240) … `kite-systems/otel-spans`(118) |
| 被裁剪 | `lumen-labs/tiny-loader`（47 星，排第 6）不在 `items` 里，但在 `state.observed` 里 |
| 每项 `url` | 非空，且与 `html_url` 一致 |
| `message` | `本次推荐 5 个仓库（候选 6，去重 0，裁剪 1）` |

第二次运行（把 `lumen-labs/tiny-loader` 星数改大、再加一个新仓库，状态沿用上一次的
`seen.json`）：`baseline_missing=false`、`baseline_date=2026-10-25`，已推荐的仓库被去重，
`lumen-labs/tiny-loader` 带着 `baseline_stars` 与 `star_delta` 出现在推荐里。

## 失败样例

| 场景 | 期望行为 |
| --- | --- |
| 输入不是数组 / 缺 `pushed_at` / `stars` 为负 | 脚本非零退出（`exit 1`），`stderr` 给可读原因；**不写**报告与状态 |
| 状态文件被手改成非法 JSON | 脚本非零退出；不写任何文件（不猜、不重建） |
| GitHub MCP 不可达（无输出可落盘） | **不运行脚本**；`report.md` 写「来源不可达 / 数据缺失」+ 观测时间，不补造条目 |
| 首跑 | 报告写「首次运行，无基线，不计算星差」，**不得**写「增长」 |
| 搜索结果为空 | `items: []`、`source.status: "empty"`、`message: 本次无新增…`，仍写出报告 |
| `shell` 工具拒绝脚本路径 / 解释器未放行 | Run 可能仍 `completed`（失败结果回填给模型）；业务断言因缺脚本产物判失败，不算通过 |

## 证据档位

| 档位 | 结论 | 依据 |
| --- | --- | --- |
| 夹具可复跑 | **已验证** | `scripts/test_github_radar.py` 9 个用例；`TestUS14Demos_Fixture/github` 用同一夹具复跑 |
| 真实 GitHub MCP + PAT 实跑 | **未验证** | 外部授权未提供；真实工具名、只读模式与代表性调用均未实测 |
| 通知送达 | **未验证**（基线零通知） | `notify_mode: none`；切到 `allowlist` 后的送达需真实接收方 |

## 已知限制

- **MCP 工具名未实测**：`tools:` 里两个 GitHub 工具按公开文档填写；首次连接后按
  `tools/list` 核对一次。
- **`observed` 不淘汰**：只裁剪 `recommended`。长期运行后状态文件随「见过的仓库数」
  增长；需要收敛时手工归档 `state/seen.json`（脚本会按新状态重新建立基线）。
- **不判断相关性**：脚本只算数，仓库是否真与主题相关取决于搜索查询串与模型解释。
- **不做补发**：通知失败只留证据，补发由人用保存的报告手动执行，并注明原 `run_id`。
