# funding-watch —— 每日融资线索模板（B 档配方）

用 Brave Search 检索融资新闻，用 Fetch 抓取前几条正文，产出带来源链接的线索清单。

档位：B（配方）

> **未验证，不要当可运行示例**：本模板是**配方**，不是可运行示例。没有夹具、没有
> 固定期望结果、没有跑通过一次真实检索；工具名、参数与输出形态全部是**候选**。
> 直接复制去跑之前，请先按「未验证」一节逐条核对。

## 领域

行业 / 创业——每天扫一遍行业里的融资事件，攒成线索清单。

## 能力

- 两组固定查询词检索融资新闻，按 `url` 去重；
- 对前 5 条抓正文，只抽正文里明确写出的「公司 / 轮次 / 金额 / 投资方 / 日期」；
- 缺字段写「未提及」，不跨条推断；
- 抓取失败的条目降级为「仅线索」，不丢弃、不编内容；
- 重复报道保留在报告末尾，不展开。

## 工具与域名

| 项 | 值 |
| --- | --- |
| 内置 `tools:` 声明 | `read_file`、`write_file` |
| MCP 工具名 | `brave_web_search`（Brave Search，**候选**）、`fetch`（Fetch，**候选**） |
| `mcp_servers:` 声明 | `brave-search`、`fetch` |
| 出网域名 | 由两个 MCP server 自己决定——**不受** Basalt 域名白名单约束 |
| 内置域名白名单 | **无**（本模板的内置工具不出网） |

**出网边界**：Brave 与 Fetch 都是本地 stdio 进程，检索与抓取由它们发起，Basalt 的
`http.allowed_domains` 管不到它们。谁能被抓取，取决于你给 Fetch 的参数与它自身的
策略——这是 B 档**必须人工确认**的边界，别把它当成「已经有白名单」。

## 文件范围

| 路径 | 读 / 写 |
| --- | --- |
| `funding-watch.md`（Agent 根目录） | 写（整文件覆盖） |

本模板不落中间文件、不维护基线——没有夹具也没有跨运行对比。

## 凭据变量

| 变量 | 用途 | 说明 |
| --- | --- | --- |
| `BRAVE_API_KEY` | Brave Search 的 API key | 由 `mcp_servers.yaml` 的 `env` 引用 `${BRAVE_API_KEY}`；**本仓库不含任何真实 key** |

Fetch 不需要凭据。

## 依赖

| 依赖 | 说明 |
| --- | --- |
| Basalt 实例 | 提供文件工具、MCP 客户端池与调度 |
| Brave Search MCP | Node 24.22.0 + Brave 发布包（**包名与入口本仓库未核实**） |
| Fetch MCP | Python 3.12 + `mcp-server-fetch`（**包名未核实**）；也可用 Node 版实现，以官方文档为准 |
| 模型连接 | `provider.name: local-model` 是占位，导入后绑定 |

## 档位

**档位：B（配方）**——两份文件（`AGENT.md` + 本 README）就是全部交付：说明怎么配、
需要什么、边界在哪。没有夹具、没有固定期望结果、没有通过任何一次真实检索，因此不
满足 A 档的装载、工具子集与夹具复跑证据。

## 证据状态

| 证据 | 状态 |
| --- | --- |
| 可装载、内置工具可解析 | 仓内 `TestTemplates_LoadAndTools` 覆盖（e2e tag） |
| MCP 工具 ⊆ 目标 server `tools/list` | **未验证**——两个 server 都未实连，工具名是候选 |
| 夹具复跑 | **无夹具**——B 档没有业务断言 |
| 真实检索与抓取 | **未验证**——本仓库验收不调用真实外部服务 |
| 真实模型 Run | **未验证**——需要你自己绑定模型连接 |

## 未验证（首次接入逐条核对）

1. `brave_web_search` 的名字与参数：以 Brave server 的 `tools/list` 为准；名字不同
   就改 `AGENT.md` 的 `tools:` 与第 1 步；
2. `fetch` 的参数（`url` / `max_length` 等）：以 Fetch server 的 `tools/list` 为准；
3. 两个包的入口绝对路径与版本：按官方文档安装后回填 `mcp_servers.yaml`；
4. `BRAVE_API_KEY` 的申请方式与配额：按 Brave 官方文档确认，本仓库未核实；
5. 结果数量与费用：Brave 有免费额度与速率限制，本仓库未实测；
6. 抓取失败率与超时：真实站点差异很大，本仓库无实测数据。

## 已知限制

- 只有两组查询词，覆盖面窄；换行业要改 `AGENT.md` 第 1 步的表；
- 只抓前 5 条正文，其余只留标题与链接；
- 不判断新闻真伪、不交叉验证金额——只做「有来源的摘录」；
- 不做跨日对比：没有基线文件，也没有「新出现」的概念；
- 需要登录或反爬的站点抓不到，只会留在「仅线索」里；
- 本 Agent 声明了两个 MCP server：`brave-search` / `fetch` 任一没配好或连不上时，
  **整只 Agent 不注册**（加载期校验第 6 条，WARN `mcp server skipped: <name>`）。

## 配置配方

`mcp_servers.yaml` 加两条（命令与入口以你本机实际安装结果为准）：

```yaml
brave-search:
  transport: stdio
  command: node
  args:
    - "<Brave MCP 入口绝对路径>"
  env:
    BRAVE_API_KEY: ${BRAVE_API_KEY}

fetch:
  transport: stdio
  command: python
  args:
    - "-m"
    - "mcp_server_fetch"
```

`${BRAVE_API_KEY}` 是**引用**，不是字面量：真实值放环境变量，别写进这个文件。

## 通知路径

基线 `notify_mode: none`（零通知）。要每天推送：管理台把 `notify_mode` 改成
`allowlist` 并勾选渠道，再给 `daily-funding-watch` 配 `deliver_to`；由系统在 Run
成功后发送，模型不调用 `notify`、失败不自动补发。

## 生效方式

`mcp_servers.yaml` 与 `AGENT.md` 的变更都要重启 Basalt（或走既有受控发布流程）后
生效；环境变量要在启动 Basalt 的那个环境里存在。本模板不自动导入、不自动放宽任何
白名单。
