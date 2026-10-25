# arxiv-tracker —— 每日论文追踪模板

用内置 `http_get` 拉取 `export.arxiv.org` 的 Atom 响应，按上次运行时间（记忆）判断
新增条目，写出追踪简报。**无凭据、无 MCP、无脚本**。

档位：A（全量）

## 领域

学术研究——每天固定时间给出一份「昨天之后 arXiv 上 cs.AI 新增了什么」的清单。

## 能力

- 拉取 `cat:cs.AI` 按 `updatedDate` 倒序的最近 20 条；
- 以记忆里的「上次运行时间」为基线判定新增 / 已知；
- 首跑没有基线时如实写「首跑」，不计算新增率；
- 时间戳读不出来归「待确认」，不并入新增；
- 失败轮不推进基线，下次运行仍从旧基线重读。

## 工具与域名

| 项 | 值 |
| --- | --- |
| `tools:` 声明 | `http_get`、`read_file`、`write_file`、`save_memory`、`recall_memory` |
| MCP | 无（`mcp_servers: []`） |
| 出网域名 | `export.arxiv.org`（HTTPS 443） |
| 内置域名白名单 | **无**——`http_get` 本身不带域名清单 |

**出网边界**：导入后必须在管理台「执行环境」把 `export.arxiv.org` 加进域名白名单
（`basalt.yaml` 的 `http.allowed_domains` 只是首次种子），否则每次运行都是
`sandbox_denied`。白名单不因导入模板自动放宽。

## 文件范围

| 路径 | 读 / 写 |
| --- | --- |
| `arxiv-tracker.md`（Agent 根目录） | 写（整文件覆盖） |
| `fixtures/response.xml` | 只读（仅离线复跑用） |
| 记忆 | 读基线、成功后写新基线 |

## 凭据

无。arXiv API 不需要 key；模板里不出现任何 `${...}` 占位。

## 依赖

| 依赖 | 说明 |
| --- | --- |
| Basalt 实例 | 提供 `http_get`、文件工具、记忆工具与调度 |
| 模型连接 | `provider.name: local-model` 是占位，导入后在管理台绑定 |
| 网络 | 能访问 `export.arxiv.org` |

## 档位

**档位：A（全量）**——完整 Agent 目录 + 夹具 + 固定期望结果 + 本 README；可装载；
声明的内置工具全部是已注册内置工具；无凭据可复跑。

## 证据状态

| 证据 | 状态 |
| --- | --- |
| 可装载、工具可解析 | 仓内 `TestTemplates_LoadAndTools` 覆盖（e2e tag） |
| 夹具复跑（新增判定与期望结果） | 仓内夹具 + 人工按 `AGENT.md`「夹具复跑」走一遍 |
| 真实 arXiv API 实连 | **未验证**——本仓库验收不调用真实外部服务 |
| 真实模型 Run | **未验证**——需要你自己绑定模型连接 |

## 已知限制

- 只查一个分类（`cat:cs.AI`）与固定的 20 条窗口；换分类 / 扩窗口要改 URL；
- 只按 `updated` 判定，不按 `published`——修订也会算「新增」，这是有意选择（修订值
  得重看）；
- 摘要只取开头用于简报，不抓全文；
- arXiv 的 API 条款与频率限制未在本仓库核实，长期高频拉取前请自行确认；
- 记忆条目以固定前缀 `arxiv-tracker 上次运行` 开头：**手动写别的格式会让下一轮读
  不到基线而退回首跑**。

## 夹具与期望结果

`fixtures/response.xml` 共 6 条 `<entry>`；复跑基线固定为
`2026-10-22T07:00:00+08:00`（= `2026-10-21T23:00:00Z`）：

| # | 条目 | updated | 期望归类 |
| --- | --- | --- | --- |
| 1 | Tool-Call Trajectories as a First-Class Audit Unit | 2026-10-24T09:12:00Z | 新增 |
| 2 | Sandboxed Shell Access for Small Agent Runtimes | 2026-10-23T16:40:00Z | 新增 |
| 3 | Memory Baselines for Daily Diff Agents | 2026-10-22T23:05:00Z | 新增 |
| 4 | Prompt Caching for Scheduled Batch Agents | 2026-10-21T20:00:00Z | 已知 |
| 5 | Deterministic Fixtures for Agent Evaluation | 2026-10-21T11:30:00Z | 已知 |
| 6 | Unparseable Timestamp Example | `not-a-timestamp` | 待确认 |

结论读数：新增 3、已知 2、待确认 1，合计 6；「新增」段按 updated 从新到旧排列；
第 6 条**不得**出现在「新增」段。首跑复跑（清掉记忆再跑）应写「首跑，无基线」并把
前 5 条列为首跑可见、第 6 条列待确认。

## 通知路径

基线 `notify_mode: none`（零通知）。要每日推送：管理台把 `notify_mode` 改成
`allowlist` 并勾选渠道，再给 `daily-arxiv-tracker` 配 `deliver_to`；由系统在 Run
成功后发送，模型不调用 `notify`、失败不自动补发。

## 生效方式

改完 `AGENT.md` 后重启 Basalt（或走既有受控发布流程）；域名白名单在管理台保存即生效。
