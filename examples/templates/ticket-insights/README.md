# ticket-insights —— 每日客服工单洞察模板

通过 DBHub（只读 SQLite）查询样例库里的客服工单，按固定关键词表分类计数，输出分类
分布与未关闭清单。分类口径由 `ticket-insights` 技能规定，**无凭据、无脚本**。

档位：A（全量）

## 领域

客户服务——每天固定时间回答「昨天到今天，工单集中在哪几类、还有多少没关」。

## 能力

- 只读查询样例库 `tickets` 表，取全量明细（不筛选、不限行）；
- 按固定关键词表 + 固定顺序（登录 → 支付 → 配送）**首命中**归类，其余归「未分类」；
- 四个分类的工单数与 `status = open` 条数；
- `priority` / `status` 出现约定外的值 → 单列「待确认」，不静默吞掉；
- 查询失败或 0 行时如实写失败 / 空结果，不补造数据。

## 工具与域名

| 项 | 值 |
| --- | --- |
| 内置 `tools:` 声明 | `read_file`、`write_file` |
| MCP 工具名 | `execute_sql`（按 `@bytebase/dbhub` 1.5.0 实测面对齐：只读配置下只暴露这一个工具） |
| `skills:` 声明 | `ticket-insights`（实体在本目录 `skills/ticket-insights/SKILL.md`） |
| `mcp_servers:` 声明 | `dbhub`（stdio，配置见下） |
| 出网域名 | 无——只读本地 SQLite，不访问网络 |

**MCP 自身出网范围**：DBHub 是本地 stdio 进程，只读打开的样例库文件；它自己的出网
不受 Basalt 域名白名单约束（白名单只管内置 `http_get` / `http_post`）。

**dbhub 配置要点**（`mcp_servers.yaml` 里一条，命令以你本机实际入口为准）：

```yaml
servers:
  - name: dbhub
    transport: stdio
    command: node
    args:
      - "<DBHub 入口绝对路径>"
      - "--transport"
      - "stdio"
      - "--config"
      - "<dbhub.toml 绝对路径>"
```

```toml
# dbhub.toml（@bytebase/dbhub 1.5.0：--readonly 参数已被移除，只读写在这里）
[[sources]]
id = "default"
dsn = "sqlite:///<样例库绝对路径>"

[[tools]]
name = "execute_sql"
source = "default"
readonly = true
max_rows = 500
```

`[[tools]]` 的 `readonly = true` 是**档位 A 的前提**：没有它，server 侧仍有写能力，
只读只靠 Agent `tools:` 声明保证，证据不足。实测（2026-10-26，1.5.0）`UPDATE` /
`DELETE` 返回 `{"success": false, "code": "READONLY_VIOLATION"}`，不写库。

## 文件范围

| 路径 | 读 / 写 |
| --- | --- |
| `ticket-insights.md`（Agent 根目录） | 写（整文件覆盖） |
| `tickets.json`（Agent 根目录） | 写（本轮查询结果原样落盘，整文件覆盖） |
| `fixtures/schema.sql` | 只读（建样例库用） |
| `fixtures/rows.json` | 只读（建样例库 / 离线复跑用） |
| `skills/ticket-insights/SKILL.md` | 复制到 `.basalt/skills/ticket-insights/SKILL.md`；对模型只读 |

两个输出文件都放 Agent 根目录：`write_file` **不建父目录**，写到子目录会直接失败。

## 凭据

无。样例库不含真实客户数据；模板里不出现任何 `${...}` 占位。

## 依赖

| 依赖 | 说明 |
| --- | --- |
| Basalt 实例 | 提供文件工具、MCP 客户端池与调度 |
| DBHub | Node 20+ + `@bytebase/dbhub@1.5.0`（本模板按该版本实测面对齐）；由它提供 `execute_sql` |
| 样例库 | `py -3 examples/shared/scripts/make_sample_db.py --schema …/fixtures/schema.sql --rows …/fixtures/rows.json --out <路径>/tickets.db --force`；`.db` 不提交 |
| 技能实体 | 复制 `skills/ticket-insights/` 到 `.basalt/skills/ticket-insights/`，否则声明名无实体、整只 Agent 不注册 |
| 模型连接 | `provider.name: local-model` 是占位，导入后绑定 |

## 档位

**档位：A（全量）**——完整 Agent 目录 + 技能 + 夹具 + 固定期望结果 + 本 README；
可装载；声明的内置工具全部是已注册内置工具；技能声明有实体；声明的 MCP 工具是目标
server `tools/list` 的子集（以 DBHub 实际清单为准，未实测）；无凭据可复跑。

## 证据状态

| 证据 | 状态 |
| --- | --- |
| 可装载、内置工具可解析、技能有实体 | 仓内 `TestTemplates_LoadAndTools` 覆盖（e2e tag） |
| MCP 工具名是 DBHub `tools/list` 的子集 | **未实测**——验收用记录下来的 `tools/list` 夹具判定；真实 DBHub 未实连 |
| 夹具复跑（分类计数与期望结果） | 仓内夹具 + 人工按 `AGENT.md`「夹具复跑」走一遍 |
| 真实 DBHub 实连 | **未验证**——需要你本机装 DBHub 并生成样例库 |
| 真实模型 Run | **未验证**——需要你自己绑定模型连接 |

## 已知限制

- 分类只按 `subject` 关键词、只认中文写法：英文工单、口语化描述会大量落进
  「未分类」——这是有意选择（宁可少分，不要错分）；
- 关键词表是固定小表（登录 / 支付 / 配送），新业务类目要改 `SKILL.md` 的表并重跑
  夹具，属于判据变更；
- 只统计计数与未关闭清单，不算时长、不算 SLA、不跟历史趋势——没有历史库可支撑；
- 单表固定结构：换库（例如换成 MySQL）要同时改 `dbhub.toml` 的 `dsn` 与 `AGENT.md` 的固定 SQL；
- DBHub 的包入口路径与工具名都在本仓库**未实测**，首次接入请以实际 `tools/list`
  为准并回填 `AGENT.md` 与本节；
- 本 Agent 声明了 MCP server：`dbhub` 没配好或连不上时，**整只 Agent 不注册**
  （加载期校验第 6 条，WARN `mcp server skipped: dbhub`），不是「少了几个工具还能跑」。

## 夹具与期望结果

`fixtures/rows.json` 共 10 行（`fixtures/schema.sql` 建表）。复跑时用 `read_file`
读它代替查询，按技能的关键词表分类，期望：

| 分类 | 工单数 | 未关闭 | 命中依据 |
| --- | --- | --- | --- |
| 登录 | 4 | 2 | TK-2001 / 2002 / 2005 / 2009（TK-2009 同时含「支付」，按顺序取登录） |
| 支付 | 3 | 3 | TK-2003 / 2006 / 2010 |
| 配送 | 2 | 1 | TK-2004 / 2008 |
| 未分类 | 1 | 0 | TK-2007（关键词表全不命中） |
| 合计 | 10 | 6 | 四类相加必须等于明细行数 |

未关闭清单应为 6 条，按 `row_id` 升序：TK-2001、TK-2003、TK-2005、TK-2006、
TK-2008、TK-2010。「待确认」段应为 1 条：TK-2010 的 `priority: urgent`（不在
`low / normal / high` 内）——它**仍计入**支付类，只是字段待人工确认。

## 通知路径

基线 `notify_mode: none`（零通知）。要每日推送：管理台把 `notify_mode` 改成
`allowlist` 并勾选渠道，再给 `daily-ticket-insights` 配 `deliver_to`；由系统在 Run
成功后发送，模型不调用 `notify`、失败不自动补发。

## 生效方式

复制目录后：技能复制到 `.basalt/skills/ticket-insights/`，`mcp_servers.yaml` 加
`dbhub` 条目并生成样例库，绑定模型连接，重启 Basalt（或走既有受控发布流程）。
`mcp_servers.yaml`、技能与 `AGENT.md` 的变更都要重启后生效。
