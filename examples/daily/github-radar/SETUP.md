# SETUP —— 把 github-radar 接到你自己的 Basalt 工作区

这份清单按顺序做完，Agent 就能被调度触发。每一步都写了「改成什么」和「怎么确认」。
**示例不会自动导入，也不会替你放宽任何白名单**。

## 1. 复制 Agent 目录

```sh
cp -r examples/daily/github-radar <workspace>/.basalt/agents/github-radar
```

目录名必须仍是 `github-radar`（= `AGENT.md` 里的 `name`；不一致整天不注册）。

本 Demo **没有技能**：不需要往 `.basalt/skills/` 复制任何东西。

## 2. 配置 GitHub MCP server

编辑 `<workspace>/.basalt/mcp_servers.yaml`，加入一条（字段与 MCP 目录条目 `github` 一致）：

```yaml
servers:
  - name: github
    transport: stdio
    command: github-mcp-server
    args:
      - "stdio"
    env:
      GITHUB_PERSONAL_ACCESS_TOKEN: "${GITHUB_PERSONAL_ACCESS_TOKEN}"
```

- `name` 必须与 `AGENT.md` 的 `mcp_servers: [github]` 逐字一致。
- `github-mcp-server` 的安装方式与版本**未实测**（目录 `verified: false`）。它是 Go 二进制，
  需要自己在 PATH 上放好；装好后先跑 `github-mcp-server --help` 确认参数形态。
- `env` 的值只写 `${VAR}` 占位：保存 / 发布时若变量缺失或为空，Basalt 会 **fail-closed**
  拒绝写入并返回 503 + `operation_id`（不会把占位符写成字面量）。

## 3. 环境变量

| 变量 | 值 |
| --- | --- |
| `GITHUB_PERSONAL_ACCESS_TOKEN` | 只读细粒度 PAT（能读仓库元数据与文件内容即可） |

**不要**把它写进 `AGENT.md`、`basalt.yaml`、脚本或提交记录。用进程环境或 `.env` 注入。

## 4. 模型绑定

`AGENT.md` 里 `provider.name: local-model`、`model: 请在此填写模型名` 都是占位：

1. 在管理台新建（或选用已有）**模型连接**，名字与 `provider.name` 一致（或把两边一起改）。
2. 进「Agents → github-radar → 配置」，在模型连接下拉里**显式选择**该连接并保存——
   服务端会把 `resource_bindings.model_connection[].uid` 写进 frontmatter。
   **不要手填 UID**：绑定必须指向当前发布指针，手填的值过不了准入。
3. 用表单把 `provider.model` 改成真实模型名（空白即拒）。

保存后确认：`GET /api/v1/agents/github-radar/config` 的 `resource_bindings` 里恰好一条
模型连接，且 UID 与该连接的当前发布版本一致。

## 5. 通知（基线零通知，按需再开）

基线是 `notify_mode: none`（**不发任何通知**，`tools:` 里也没有 `notify`）。要开启：

1. 先建通知渠道（管理台「通知渠道」页），记下它的渠道名与 `resource_uid`。
2. 在 Agent 配置表单里把 `notify_mode` 改成 `allowlist`，勾选该渠道——保存时服务端会
   同时写入 `notify_channels` 与 `resource_bindings.notify_channel`。
3. 保存响应里的 `changes.tools_added` 应包含 `notify`：装配由保存路径完成，不要手改
   `tools:` 列表。

D1 用**模型 `notify`**（Run 内调用），不是 `deliver_to` 系统代投；两者不要同时开。

## 6. 放开 shell（脚本通道）

`shell` 工具只执行白名单里的可执行名，解释器还必须走受控通道。编辑 `basalt.yaml`：

```yaml
shell:
  allowed_commands:
    - python                # 与 interpreters / AGENT.md 用同一串（Windows 见下方提示）
    - github-mcp-server     # MCP server 的 command 也必须放行
  interpreters:
    - python                # 解释器：argv 必须是 [脚本路径, 参数…]，脚本须在本 Agent scripts/ 内
  env_passthrough: []       # 脚本不需要额外环境变量
```

- **Windows 本机必须写解释器的绝对路径**（已实测）：受控执行器把 `interpreters` 里的
  整串**原样**交给 `CreateProcess`（`lpApplicationName`），既不查 PATH、也不补
  `.exe` / `.cmd` 后缀——写 `python`、`python.exe`、`py` 都只会得到
  `The system cannot find the file specified`。请写 python.org 安装的绝对路径
  （如 `C:/Users/<you>/AppData/Local/Programs/Python/Python312/python.exe`），
  并把 `allowed_commands` 与 `AGENT.md` 第 4 步的可执行名同步成**同一串**（白名单按
  整串逐字比对）。Linux / macOS 上执行器走 `exec.Command`（查 PATH），裸名可用。
- 同一份 `basalt.yaml` 的 `budget:` 段也要动：`budget.max_iterations` 缺省 **10**，
  而本 Demo 的 `AGENT.md` 声明 `settings.max_iterations: 12`——Profile 只能**收紧**
  运维上限、**不得**放大（契约 `run-and-errors.md` §6），不调这一项 Run 会在第 10 轮
  落 `limit_reached`。在该段把 `max_iterations` 调到不小于 12。

## 7. 重启

`mcp_servers.yaml` 与 `shell.*` 都**不热加载**。改完重启 Basalt：

```sh
basalt serve
```

确认两件事：启动日志里 `github` 连接成功（六段证据第 1 段），管理面「MCP server」页
没有「待发布 / 待重启」标记。

## 8. 手动跑一次

管理台「Agents → github-radar → 手动运行」，或：

```sh
curl -X POST -H "Authorization: Bearer $BASALT_ADMIN_TOKEN" \
  http://127.0.0.1:8080/api/v1/schedules/daily-radar/run
```

审计里这条 Run 的 `trigger` 是 `manual`。**手动跑成功不等于定时触发已验过**——到点触发
的证据要另取一次（等一个 `0 30 8 * * *`，或改 cron 到最近一分钟再改回来）。

## 9. 核对六段证据

把 `<run_id>` 换成上一步返回的 Run id，`<workspace>/.basalt/basalt.db` 换成你的库路径。

```sh
# 1 连接成功：启动日志
grep -i "mcp" <日志文件> | grep github

# 2 tools/list 实名单：MCP server 页的 tools（契约字段 McpServerView.tools）
curl -s -H "Authorization: Bearer $BASALT_ADMIN_TOKEN" \
  http://127.0.0.1:8080/api/v1/mcp-servers | python -m json.tool

# 3 实际调用 + 5 审计行
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT tool_name, status, error_kind FROM tool_invocations WHERE run_id='<run_id>' ORDER BY id;"
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT status, trigger, iterations, tool_calls FROM runs WHERE run_id='<run_id>';"
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT round, status FROM llm_calls WHERE run_id='<run_id>' ORDER BY round;"

# 4 结果进入回答 / 报告：打开 runs/<时间戳>/report.md，数字应与 report.json 一致
# 6 业务产物：报告与状态的哈希（脚本 stdout 已打印），以及通知次数
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT COUNT(*) FROM tool_invocations WHERE run_id='<run_id>' AND tool_name='notify';"
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT task_id, status, trigger FROM task_executions WHERE run_id='<run_id>';"
```

对照 `README.md` 的「期望业务结果」逐项核对：`baseline_missing`、五个推荐、被裁剪的
仓库、每项来源 URL。数字对不上就是**不通过**，不要用「Run completed」代替业务断言。

## 10. 失败时先看哪里

| 现象 | 先查 |
| --- | --- |
| Agent 没注册 | 启动 WARN：`name mismatch` / `provider version missing` / `tool not registered` / `mcp server skipped: github` |
| 保存 MCP 配置返回 503 | `GITHUB_PERSONAL_ACCESS_TOKEN` 未设置或为空；用响应里的 `operation_id` 查操作详情 |
| `shell` 返回 `sandbox_denied` | `shell.allowed_commands` / `shell.interpreters` 未放行，或脚本不在 `scripts/` 下 |
| 脚本非零退出 | `stderr` 原文会被回填给模型；按 `README.md` 的「失败样例」判定，不要改写脚本绕过 |
| `http_get` 返回 `sandbox_denied` | 域名白名单缺 `raw.githubusercontent.com` / `github.com`；在管理台「执行环境」页添加 |
