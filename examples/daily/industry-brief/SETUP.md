# SETUP —— 把 industry-brief 接到你自己的 Basalt 工作区

这份清单按顺序做完，Agent 就能被调度触发。每一步都写了「改成什么」和「怎么确认」。
**示例不会自动导入，也不会替你放宽任何白名单**。

## 1. 复制 Agent 目录

```sh
cp -r examples/daily/industry-brief <workspace>/.basalt/agents/industry-brief
```

目录名必须仍是 `industry-brief`（= `AGENT.md` 里的 `name`；不一致整天不注册）。
复制过来的 `runs/` 若存在（本地跑过留下的产物），**不要**带进新工作区——它们是上一轮
的产物，不是输入。

## 2. 技能（本 Demo 不适用）

D5 不用技能：`AGENT.md` 没有 `skills:` 声明，`.basalt/skills/` 下也不需要建任何目录。
简报口径直接写在 `AGENT.md` 的步骤与模板里。不要为了「凑齐结构」复制别的 Demo 的
技能——声明名无实体时整只 Agent 不注册。

## 3. 配置 Fetch MCP server

搜索**不再是 MCP**：`web_search` 是内置工具（第 11 件），随二进制提供，不用安装、
不用在 `mcp_servers.yaml` 里登记。只需要配 Fetch：

编辑 `<workspace>/.basalt/mcp_servers.yaml`，加入一条（字段与 MCP 目录条目 `fetch`
一致，`verified: false`）：

```yaml
servers:
  - name: fetch
    transport: stdio
    command: python
    args:
      - "-m"
      - "mcp_server_fetch"
```

- `name` 必须与 `AGENT.md` 的 `mcp_servers: [fetch]` 逐字一致；
- **安装**（本仓库未实测，请自己装好）：给 `command` 指向的那个解释器装
  `mcp-server-fetch`（如 `python -m pip install mcp-server-fetch`），
  `python -m mcp_server_fetch` 能启动即可；
- `python` 必须**逐字**出现在 `shell.allowed_commands` 里：stdio MCP 启动同样要过命令
  授权（出现 `mcp server skipped: fetch` 就是没放行）。若把 `command` 写成绝对路径，
  `allowed_commands` 也要写同一个绝对路径（整串比对）；
- `fetch` 的 `-m` 是**服务端自己的入口方式**，不走 `shell` 工具的解释器通道——
  `shell.interpreters` 管不到 MCP 启动，`shell.allowed_commands` 才管；
- Windows 本机提示：Microsoft Store 的 `python` 是占位程序。要么把 python.org 装的
  Python 放在 PATH 前面（`command` 仍是 `python`），要么把 `command` 改成实际名字
  （如 `py`）并让 `shell.allowed_commands` 同步——两处必须同一串；
- 连接后核对一次工具名：目录里写的 `fetch` 是候选名，若加载报 `tool not registered`，
  按实际 `tools/list` 改 `AGENT.md` 的 `tools:`，**不要**改脚本。

### 放行 `web_search` 的出网域名（严格档必做）

`web_search` 由 **Basalt 自己**发请求到 `https://api.deepseek.com/v1/tools/search`，
受当前执行档位约束：

- **标准档**（新实例缺省）：`api.deepseek.com` 未命中禁止域名表 → 默认放行，无需配置；
- **严格档**（存量实例升级后一律严格）：管理台「执行环境 → 网络域名白名单」加入
  `api.deepseek.com` 并保存（**保存即生效，无需重启**）。漏了这一条时，每次搜索都被判
  `sandbox_denied`，简报只会写「检索不可达 / 数据缺失」——那是本地策略拒绝，不是
  DeepSeek 服务故障。

这次调用与 Fetch 子进程无关：`fetch` 抓哪些 URL 是它自己的网络策略，Basalt 不管。

## 4. 环境变量与搜索凭据

**本 Demo 不需要任何专用环境变量**（没有 `BRAVE_API_KEY` 这一项了）。

`web_search` 的搜索 key 取自**该 Agent 绑定的模型连接**：env 模式的连接读它的
`${ENV_VAR}`，托管模式的连接读实例加密的凭证。也就是说：

- 你为模型调用准备的那份 DeepSeek key，就是搜索用的 key——不需要第二份；
- key 的落法按模型连接的既有规则（进程环境变量 / 管理台托管凭据），不要把值写进
  `AGENT.md`、`basalt.yaml`、脚本或提交记录；
- 搜索 key 缺失或解析为空时，`web_search` **失败关闭**（不发出请求），工具结果是可读
  错误——简报照写、来源表为空，不编造。

## 5. 模型绑定

`AGENT.md` 里 `provider.name: local-model`、`model: 请在此填写模型名` 都是占位：

1. 在管理台新建（或选用已有）**DeepSeek 模型连接**，名字与 `provider.name` 一致
   （或把两边一起改）。搜索端点固定为 `api.deepseek.com`，所以这条连接必须是
   DeepSeek（或与官方接口兼容、且凭据可用的）连接——换成别家时搜索会按 401 失败，
   不会改用别的凭据。
2. 进「Agents → industry-brief → 配置」，在模型连接下拉里**显式选择**该连接并保存——
   服务端会把 `resource_bindings.model_connection[].uid` 写进 frontmatter。
   **不要手填 UID**：绑定必须指向当前发布指针，手填的值过不了准入。
3. 用表单把 `provider.model` 改成真实模型名（空白即拒）。

## 6. 通知渠道与系统代投（`deliver_to`）

基线是 `notify_mode: none`、任务上没有 `deliver_to`——**这一轮零通知**。要让简报在
Run 成功后自动发到渠道，按下面三步（顺序不能换）：

1. **建渠道**：管理台「通知渠道」→ 新建（webhook / 飞书），写入地址与凭据并确认
   `availability` 是 `ready`。记下渠道名——后面用下拉选，**不要手抄 UID**。
2. **声明渠道**：进「Agents → industry-brief → 配置 → 通知」，把 `notify_mode` 改成
   `allowlist` 并勾选该渠道后保存。服务端会同时写入 `notify_channels` 与
   `resource_bindings.notify_channel`，保存响应里 `changes.tools_added` 应含 `notify`。
   声明是权限边界：**没声明的渠道不会出现在代投下拉里**。
3. **给任务配代投**：「定时任务」→ 修改 `daily-industry-brief` → 在「运行成功后由系统
   投递」下拉里选该渠道 → 保存。页面文案应变成「运行成功后由系统投递到 <渠道名>」。

配好之后这一轮 Run 的行为（契约固定，不随页面变化）：

- 准入阶段把该轮 `notify_mode` **固定为 `none`**，并从工具面剔除 `notify`——模型
  只产出正文，不会自己发；
- 只有 Run **成功结束**才获得代投资格：系统先写 `schedule_deliveries` 的 `pending`
  行再发送，终态 `sent` / `failed` / `unknown`；非成功 Run 记 `skipped`；
- `failed` / `unknown` **不算送达**，也**不自动补发**；`AGENT.md` 不会被运行时改写。

不想代投就保持基线：不建渠道、不改 `notify_mode`、不配 `deliver_to`，简报只留在
运行记录与文件里。

## 7. 放开 shell（脚本通道）

`shell` 工具只放行白名单里的可执行名，解释器还必须走受控通道。编辑 `basalt.yaml`：

```yaml
shell:
  allowed_commands:
    - python               # fetch MCP 的 command（与 mcp_servers.yaml 逐字一致）
  interpreters:
    - python               # 解释器整串；Windows 写绝对路径，见下方提示
  env_passthrough: []      # 脚本不需要额外环境变量
```

- 解释器通道的 argv 必须是 `[<脚本路径>, <参数…>]`，脚本真实路径必须落在该 Agent 的
  `scripts/` 内——`-c` / `-m` 这类内联代码一律拒；`scripts/` 对模型只读。
- `bash` / `sh` **不得**放行。
- **Windows 本机必须写解释器的绝对路径**（已实测）：受控执行器把 `interpreters` 里的
  整串**原样**交给 `CreateProcess`（`lpApplicationName`），既不查 PATH、也不补
  `.exe` / `.cmd` 后缀——写 `python`、`python.exe`、`py` 都只会得到
  `The system cannot find the file specified`。请写 python.org 安装的绝对路径
  （如 `C:/Users/<you>/AppData/Local/Programs/Python/Python312/python.exe`），
  并把 `AGENT.md` 第 6 步的可执行名同步成**同一串**（白名单按整串逐字比对）。
  上面 `allowed_commands` 里的 `python` 是 **fetch MCP 的 `command`**（走
  `exec.Command` 查 PATH，可用裸名），与解释器那一串各判各的；若把 `command` 换成
  绝对路径，`allowed_commands` 也要同步。
  Linux / macOS 上执行器同样走 `exec.Command`，裸名可用。
- 同一份 `basalt.yaml` 的 `budget:` 段也要动：`budget.max_iterations` 缺省 **10**，
  而本 Demo 的 `AGENT.md` 声明 `settings.max_iterations: 30`——Profile 只能**收紧**
  运维上限、**不得**放大（契约 `run-and-errors.md` §6），不调这一项 Run 会在第 10 轮
  落 `limit_reached`。在该段把 `max_iterations` 调到不小于 30。
- `budget.context_tokens` 同样要调大：真实检索一轮（三条查询 × 10 条结果 + 抓正文 +
  写 `pages.json`）的工具结果可到 **100KB 量级**，缺省 **32768** 会在第二轮或「写
  `pages.json`」那一轮判「上下文超限」（2026-10-27 实测，96K 仍不够）。按模型窗口
  调到能容纳整轮（如 deepseek-chat 128K → `131072`）；报错原文会点名
  `budget.context_tokens`，见到它先调这一项，不要删结果或改脚本。

## 8. 重启

`mcp_servers.yaml` 与 `shell.*` 都**不热加载**。改完重启 Basalt：

```sh
basalt serve
```

确认三件事：启动日志里 `fetch` **连接成功**（或给出可读的失败分类）、没有
`mcp server skipped: fetch`、管理面「MCP server」页没有「待发布 / 待重启」标记。
连不上时先点该行的「诊断」——它不启动进程，只看命令定位与否、env 是否就绪；诊断
`unconfirmed` 不代表失败，点「测试」真实连接一次。

网络域名白名单不在此列：严格档加完 `api.deepseek.com` 保存即生效，不需要重启。

## 9. 手动跑一次

管理台「Agents → industry-brief → 手动运行」，或：

```sh
curl -X POST -H "Authorization: Bearer $BASALT_ADMIN_TOKEN" \
  http://127.0.0.1:8080/api/v1/schedules/daily-industry-brief/run
```

**预期**：`runs/<t0>/brief.md` 的结论段是脚本 `message` 原文，来源表 5 行、线索表 3 行
（数字随真实检索结果变化，守恒关系不变：`duplicates = results - sources`、
`fetched + leads = sources`）；每条结论都带 `[来源: <url>]`，且 URL 都在
`brief.json` 的 `citable_urls` 里。

审计里这条 Run 的 `trigger` 是 `manual`。**手动跑成功不等于定时触发已验过**——到点
触发的证据要另取一次（等一个 `0 30 9 * * *`，或改 cron 到最近一分钟再改回来）。

定时任务共用**同一个会话**（`scheduler-<hash>`，按 Agent 派生）：下一轮的提示词里能看到
上一轮的检索与产物。`AGENT.md` 已明确「每一轮都从第 1 步重来、不得复用历史里的
`search.json` / `pages.json` / `--now`」；若某轮 `runs` 显示 `completed` 却只有一两次
`write_file` / `shell`、`tool_invocations` 里没有本轮的 `web_search`，先查这条——那是模型
偷懒续写，不是检索失败。

## 10. 核对六段证据

把 `<run_id>` 换成上一步返回的 Run id，`<workspace>/.basalt/basalt.db` 换成你的库路径。

```sh
# 1 输入快照哈希：脚本 stdout 的 input.search.sha256 / input.pages.sha256
# 2 时区与计划：AGENT.md 的 schedules 段（6 段 + Asia/Shanghai）
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT task_id, trigger, status FROM task_executions WHERE run_id='<run_id>';"

# 3 候选产物哈希：脚本 stdout 的 report.sha256
# 4 run_id 与工具调用：web_search 与 fetch 各至少一条成功行，缺一即不通过
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT tool_name, status, error_kind FROM tool_invocations WHERE run_id='<run_id>' ORDER BY id;"
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT status, trigger, iterations, tool_calls FROM runs WHERE run_id='<run_id>';"

# 5 业务断言：打开 runs/<t0>/brief.json 与 brief.md，按 README「期望业务结果」核对
# 6 通知次数（系统代投，不写 tool_invocations）
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT status, channel_uid, created_at FROM schedule_deliveries WHERE run_id='<run_id>';"
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT COUNT(*) FROM tool_invocations WHERE run_id='<run_id>' AND tool_name='notify';"
```

判据：

- `tool_invocations` 里必须**同时**出现 `web_search` 与 `fetch`，各至少一条成功行
  （`fetch` 的工具名以服务端 `tools/list` 为准；只看全局名字不够，同名冲突时以装配期
  的 `tool_disposition` 归属为准）——**缺任何一个通道即判不通过**；
- 三条查询应当对应**三条** `web_search` 审计行；一次搜索内部的 408 / 429 / 5xx 重发
  **不**另计行（一条工具调用 = 一条审计行）；
- `brief.json` 的计数必须守恒：`duplicates = results - sources`、
  `fetched + leads = sources`、`fetched = citable_urls` 的条数；对不上就是**不通过**，
  不要用「Run completed」代替业务断言；
- `brief.md` 里每条 `[来源: <url>]` 必须指向 `citable_urls`；任何线索 URL 出现在结论里
  都是**不通过**（`--verify-report` 应当已经拦下，拦下还留着就是没改）；
- `schedule_deliveries.status` 必须是 `sent`（`failed` / `unknown` 不算送达）；
- `tool_invocations` 里 `notify` 计数必须是 **0**（代投轮不向模型提供 `notify`）。

## 11. 失败时先看哪里

| 现象 | 先查 |
| --- | --- |
| Agent 没注册 | 启动 WARN：`mcp server skipped: fetch` / `tool not registered` |
| `web_search` 报 `sandbox_denied` | 严格档没把 `api.deepseek.com` 加进网络域名白名单；页面保存即生效，不用重启 |
| `web_search` 报 401（`搜索凭据无效或已撤销`） | 模型连接不是 DeepSeek / 兼容连接，或该连接的 key 已撤销；到「模型连接」页检查并重录 |
| `web_search` 报 402（`账户余额不足`） | DeepSeek 账户欠费；充值后再跑。工具不重试这一档 |
| `web_search` 报 `请求被官方安全策略拒绝`（403） | 官方侧拒了这条查询；换查询词，不要反复重试 |
| `web_search` 报 `已按上限重试仍失败` | 408 / 429 / 5xx 重发两次仍失败；稍后再跑，不要手工补来源 |
| `fetch` 启动即退出 | `mcp_server_fetch` 没装到该解释器；或 Windows Store 的 `python` 是占位程序——换 `py` 并同步 `allowed_commands` |
| 某个 URL 抓取失败 | 目标站点拒绝 / 超时：该 URL 应进线索表，**不得**把检索摘要当正文 |
| 自检报「引用了未抓正文的 URL」 | 结论引用了线索；改成已抓来源的事实或删掉，再跑一次自检 |
| 自检报「没有任何 `[来源: <url>]` 标注」 | 结论没标来源；只在确实无来源时才允许全篇无引用 |
| 脚本报「不在检索结果里」 | `pages.json` 混进了别的 Run 的页面；只抓本轮 `search.json` 里的 URL |
| `shell` 返回 `sandbox_denied` | `shell.interpreters` 未放行，或脚本不在 `scripts/` 下，或 `arguments[0]` 以 `-` 开头 |
| 简报里日期全是「未确认」 | `web_search` 结果的 `date` 缺失；**不得**手补日期，只能如实写「未确认」 |
| `schedule_deliveries` 没有行 | 任务没配 `deliver_to`，或该轮 Run 未成功（应记 `skipped`）；先看 `runs` 表的终态 |
| `runs/` 出现在 `git status` | 不应提交：`.gitignore` 已忽略 `examples/daily/industry-brief/runs/` |
