# SETUP —— 把 biz-daily 接到你自己的 Basalt 工作区

这份清单按顺序做完，Agent 就能被调度触发。每一步都写了「改成什么」和「怎么确认」。
**示例不会自动导入，也不会替你放宽任何白名单**。

## 1. 复制 Agent 目录

```sh
cp -r examples/daily/biz-daily <workspace>/.basalt/agents/biz-daily
```

目录名必须仍是 `biz-daily`（= `AGENT.md` 里的 `name`；不一致整天不注册）。

## 2. 技能（本 Demo 不适用）

D3 不用技能：`AGENT.md` 没有 `skills:` 声明，`.basalt/skills/` 下也不需要建任何
目录。日报口径直接写在 `AGENT.md` 的步骤与模板里。不要为了「凑齐结构」复制别的
Demo 的技能——声明名无实体时整只 Agent 不注册。

## 3. 生成样例库

样例库是生成物，仓库里只有它的源（`fixtures/schema.sql` + `fixtures/rows.json`）：

```sh
python examples/shared/scripts/make_sample_db.py \
  --schema examples/daily/biz-daily/fixtures/schema.sql \
  --rows   examples/daily/biz-daily/fixtures/rows.json \
  --out    D:/basalt-samples/biz-sample.db --force
```

- 输出路径建议放在仓库外、Agent 目录之外；DBHub 只要读权限。
- 脚本会打印 JSON 摘要（db / schema / rows 的 SHA-256、入库行数 24）；记下 `db.sha256`，
  它是证据里的样例库哈希。
- 已在目标机上跑过一次、不想覆盖时去掉 `--force`——脚本会拒绝覆盖已存在的库。
- 确认：24 行入库，且三类异常都在（`region IS NULL` 1 行、重复单号 2 组、非法金额
  与非法日期各 1 行）。没有这些行，后面的业务断言就失去意义。

## 4. 配置 DBHub MCP server

先装固定版本：`npm install @bytebase/dbhub@1.5.0`（本 Demo 按 **1.5.0 实测**对齐）。

`dbhub` 1.5.0 已**移除** `--readonly` 参数（传了会直接启动失败）；只读与限行必须
写在配置文件里。新建 `dbhub.toml`（示例库路径换成你本机的绝对路径）：

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

再编辑 `<workspace>/.basalt/mcp_servers.yaml`，加入一条：

```yaml
servers:
  - name: dbhub
    transport: stdio
    command: node
    args:
      - "<absolute-path-to-dbhub-entry>"
      - "--transport"
      - "stdio"
      - "--config"
      - "<absolute-path-to-dbhub.toml>"
```

- `name` 必须与 `AGENT.md` 的 `mcp_servers: [dbhub]` 逐字一致。
- `node` 必须是 `shell.allowed_commands` 里的字面量：stdio MCP 启动前同样要过命令
  授权（`mcp server skipped: dbhub` 就是没放行）。若把 `command` 写成 node 的绝对
  路径，`allowed_commands` 也要写同一个绝对路径（白名单按整串逐字比对）。
- `<absolute-path-to-dbhub-entry>` 是包内入口文件：`npm install @bytebase/dbhub@1.5.0`
  之后是 `<node_modules>/@bytebase/dbhub/dist/index.js`。
- `<absolute-path-to-dbhub.toml>` 是上一步的配置文件**绝对路径**。
- `[[tools]]` 里只列 `execute_sql`，服务端就**只暴露这一个工具**（实测 `tools/list`
  = `["execute_sql"]`）——`AGENT.md` 的 `tools:` 不要写 `list_tables` /
  `search_objects`，否则会以 `tool not registered` 拒绝注册。
- 只读是**服务端强制**：`UPDATE` / `DELETE` 会被拒绝并返回
  `{"success": false, "code": "READONLY_VIOLATION"}`（2026-10-26 实测），不是靠提示词
  约束。换库或改配置后可以手工验一次。
- DSN 用 `sqlite:///` 前缀加**绝对路径**（Windows 常见写法
  `sqlite:///D:/basalt-samples/biz-sample.db`）。

## 5. 环境变量

**无**。DBHub 指向本地文件，不需要令牌；`shell.env_passthrough` 保持空数组。
不要为了「占位完整」往 `mcp_servers.yaml` 的 `env` 里塞任何假凭据——env 闸门会对
缺失 / 空值 fail-closed（503 + `operation_id`），假占位只会让保存直接失败。

## 6. 模型绑定

`AGENT.md` 里 `provider.name: local-model`、`model: 请在此填写模型名` 都是占位：

1. 在管理台新建（或选用已有）**模型连接**，名字与 `provider.name` 一致（或把两边一起改）。
2. 进「Agents → biz-daily → 配置」，在模型连接下拉里**显式选择**该连接并保存——
   服务端会把 `resource_bindings.model_connection[].uid` 写进 frontmatter。
   **不要手填 UID**：绑定必须指向当前发布指针，手填的值过不了准入。
3. 用表单把 `provider.model` 改成真实模型名（空白即拒）。

## 7. 通知渠道（本 Demo 不适用）

D3 是**零通知** Demo：`notify_mode: none`、`notify_channels: []`、任务上没有
`deliver_to`，工具面里也没有 `notify`。不要给它建渠道、不要改 `notify_mode`——
出现 `schedule_deliveries` 行反而会让验收判不通过。

## 8. 放开 shell（脚本通道）

`shell` 工具只放行白名单里的可执行名，解释器还必须走受控通道。编辑 `basalt.yaml`：

```yaml
shell:
  allowed_commands:
    - node                 # MCP server 的 command 也要过命令授权
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
  并把 `AGENT.md` 第 4 步的可执行名同步成**同一串**（白名单按整串逐字比对）。
  MCP 的 `command: node`（§4）走的是另一条通道（`exec.Command` 查 PATH），不受此限。
  Linux / macOS 上执行器同样走 `exec.Command`，裸名可用。
- 同一份 `basalt.yaml` 的 `budget:` 段也要动：`budget.max_iterations` 缺省 **10**，
  而本 Demo 的 `AGENT.md` 声明 `settings.max_iterations: 12`——Profile 只能**收紧**
  运维上限、**不得**放大（契约 `run-and-errors.md` §6），不调这一项 Run 会在第 10 轮
  落 `limit_reached`。在该段把 `max_iterations` 调到不小于 12。

## 9. 重启

`mcp_servers.yaml` 与 `shell.*` 都**不热加载**。改完重启 Basalt：

```sh
basalt serve
```

确认三件事：启动日志里 `dbhub` 连接成功（或给出可读的失败分类）、没有
`mcp server skipped: dbhub`、管理面「MCP server」页没有「待发布 / 待重启」标记。
连不上时先点该行的「诊断」——它不启动进程，只看命令定位与否、env 是否就绪。

## 10. 手动跑一次

管理台「Agents → biz-daily → 手动运行」，或：

```sh
curl -X POST -H "Authorization: Bearer $BASALT_ADMIN_TOKEN" \
  http://127.0.0.1:8080/api/v1/schedules/daily-report/run
```

审计里这条 Run 的 `trigger` 是 `manual`。**手动跑成功不等于定时触发已验过**——到点
触发的证据要另取一次（等一个 `0 0 9 * * *`，或改 cron 到最近一分钟再改回来）。

## 11. 核对六段证据

把 `<run_id>` 换成上一步返回的 Run id，`<workspace>/.basalt/basalt.db` 换成你的库路径。

```sh
# 1 输入快照哈希：脚本 stdout 的 input.sha256（orders.json）；样例库另有生成脚本的 db.sha256
# 2 时区与计划：AGENT.md 的 schedules 段（6 段 + Asia/Shanghai）
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT task_id, trigger, status FROM task_executions WHERE run_id='<run_id>';"

# 3 候选产物哈希：脚本 stdout 的 report.sha256
# 4 run_id 与工具调用（必须看到 execute_sql 的成功行——「DBHub 未被实际调用」判不通过）
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT tool_name, status, error_kind FROM tool_invocations WHERE run_id='<run_id>' ORDER BY id;"
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT status, trigger, iterations, tool_calls FROM runs WHERE run_id='<run_id>';"

# 5 业务断言：打开 runs/<时间戳>/daily.md 与 daily.json，按 README「期望业务结果」核对
# 6 通知次数：D3 必须为 0
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT COUNT(*) FROM schedule_deliveries WHERE run_id='<run_id>';"
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT COUNT(*) FROM tool_invocations WHERE run_id='<run_id>' AND tool_name='notify';"
```

判据：

- `tool_invocations` 里必须有 `execute_sql` 的成功调用（这是「DBHub 实际调用」的证据）；
- 两条计数都必须为 **0**——D3 零通知，出现任何 `notify` 调用或代投行即判不通过；
- `daily.json` 的四类计数与指标必须能按 README 复算对上；数字对不上就是**不通过**，
  不要用「Run completed」代替业务断言。

## 12. 失败时先看哪里

| 现象 | 先查 |
| --- | --- |
| Agent 没注册 | 启动 WARN：`tool not registered` / `mcp server skipped: dbhub` |
| DBHub 连不上 | 「MCP server → 诊断」看命令定位与环境变量；再看 `--dsn` 三斜杠与 `.db` 绝对路径；`node` 是否在 `allowed_commands` |
| 诊断说 `unconfirmed` | 不代表一定失败：点「测试」真实连接一次，结论只看测试结果与失败分类 |
| `shell` 返回 `sandbox_denied` | `shell.interpreters` 未放行，或脚本不在 `scripts/` 下，或 `arguments[0]` 以 `-` 开头 |
| 脚本非零退出 | `stderr` 原文会被回填给模型；按 `README.md` 的「失败样例」判定，不要改写脚本绕过 |
| 日报写「数据库不可达 / 数据缺失」 | DBHub 调用失败或返回为空；查 `tool_invocations` 对应行的 `error_kind`，不要靠重跑刷结果 |
| 指标是空的但库里有行 | 看 `daily.json` 的 `quality.counts`：可能全部行都进了缺失 / 重复 / 非法值，`computed=false` 时金额本就该是 `null` |
| `schedule_deliveries` 出现行 | 有人改了 `notify_mode` 或配了 `deliver_to`——D3 应当是零通知，撤销改动后重跑 |
