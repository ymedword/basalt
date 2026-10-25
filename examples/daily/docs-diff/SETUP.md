# SETUP —— 把 docs-diff 接到你自己的 Basalt 工作区

这份清单按顺序做完，Agent 就能被调度触发。每一步都写了「改成什么」和「怎么确认」。
**示例不会自动导入，也不会替你放宽任何白名单**。

## 1. 复制 Agent 目录

```sh
cp -r examples/daily/docs-diff <workspace>/.basalt/agents/docs-diff
```

目录名必须仍是 `docs-diff`（= `AGENT.md` 里的 `name`；不一致整天不注册）。复制过来
的 `state/` 若存在（本地跑过留下的基线），**不要**带进新工作区——首跑应当从
`baseline_missing=true` 开始。`runs/` 同理：它们是上一轮的产物，不是输入。

## 2. 技能（本 Demo 不适用）

D4 不用技能：`AGENT.md` 没有 `skills:` 声明，`.basalt/skills/` 下也不需要建任何
目录。差异口径直接写在 `AGENT.md` 的步骤与报告模板里。不要为了「凑齐结构」复制别的
Demo 的技能——声明名无实体时整只 Agent 不注册。

## 3. 构建并放置只读 MCP server

`docs-readonly` 是本仓库自研素材（独立嵌套 module，纯标准库、零依赖），不在 MCP
目录里，需要现场构建：

```sh
go -C examples/shared/mcp/docs-readonly build -o D:/basalt-samples/docs-readonly.exe .
```

再准备只读根（用共享夹具复制一份，**不要**让 Agent 直接指向仓库里的目录）：

```sh
cp -r examples/shared/fixtures/docs D:/basalt-samples/docs
```

- 二进制的绝对路径后面要原样写进 `mcp_servers.yaml` 与 `shell.allowed_commands`；
- 复制只改 `modified_at`、不改内容哈希——脚本判定只看哈希，结论不受影响；
- 想用自己公司的文档：把 Markdown 放进只读根即可（`list_documents` 只列 `.md`，
  最多 1000 份，单文件上限 1 MiB，必须是 UTF-8）。**先脱敏**。

## 4. 配置只读 MCP server

编辑 `<workspace>/.basalt/mcp_servers.yaml`，加入一条：

```yaml
servers:
  - name: docs-readonly
    transport: stdio
    command: "D:/basalt-samples/docs-readonly.exe"
    args:
      - "-root"
      - "D:/basalt-samples/docs"
```

- `name` 必须与 `AGENT.md` 的 `mcp_servers: [docs-readonly]` 逐字一致；
- `command` 必须是**绝对路径**，且**逐字**出现在 `shell.allowed_commands` 里——
  stdio MCP 启动同样要过命令授权（按整串比对，`D:/…` 与 `D:\…` 不算同一个）。
  出现 `mcp server skipped: docs-readonly` 就是没放行；
- `args` 里的 `-root` 用**绝对路径**。不传时 server 取 `$DOCS_READONLY_ROOT`，再缺省
  为进程当前目录——缺省会指到 Basalt 的工作目录，`list_documents` 只会返回空数组
  或无关文件；
- 根目录不可用（不存在 / 不是目录）时 server 直接 `exit 2`，启动日志会有明确一行。

## 5. 环境变量

**无**。只读库是本地目录，不需要令牌；`shell.env_passthrough` 保持空数组。不要为了
「占位完整」往 `mcp_servers.yaml` 的 `env` 里塞任何假凭据——env 闸门会对缺失 / 空值
fail-closed（503 + `operation_id`），假占位只会让保存直接失败。

## 6. 模型绑定

`AGENT.md` 里 `provider.name: local-model`、`model: 请在此填写模型名` 都是占位：

1. 在管理台新建（或选用已有）**模型连接**，名字与 `provider.name` 一致（或把两边一起改）。
2. 进「Agents → docs-diff → 配置」，在模型连接下拉里**显式选择**该连接并保存——
   服务端会把 `resource_bindings.model_connection[].uid` 写进 frontmatter。
   **不要手填 UID**：绑定必须指向当前发布指针，手填的值过不了准入。
3. 用表单把 `provider.model` 改成真实模型名（空白即拒）。

## 7. 通知渠道（基线不发通知，按需开启）

基线是 `notify_mode: none`（**不发任何通知**，`tools:` 里也没有 `notify`）。D4 的验收
口径是「每次运行一条 `notify`」，需要按下面开启：

1. 先在「通知渠道」页建一个渠道，记下它的 UID；
2. 在 Agent 配置表单里把 `notify_mode` 改成 `allowlist`，勾选该渠道——保存时服务端会
   同时写入 `notify_channels` 与 `resource_bindings.notify_channel`；
3. 保存响应里的 `changes.tools_added` 应包含 `notify`：装配由保存路径完成，不要手改
   `AGENT.md` 的 `tools:`；
4. D4 用**模型 `notify`**（Run 内调用），不是 `deliver_to` 系统代投；任务上不要配
   `deliver_to`，两者不要同时开。

开启后每次运行（**含无变化**）恰好一条通知；`schedule_deliveries` 仍应为 0 行。

## 8. 放开 shell（脚本通道）

`shell` 工具只放行白名单里的可执行名，解释器还必须走受控通道。编辑 `basalt.yaml`：

```yaml
shell:
  allowed_commands:
    - "D:/basalt-samples/docs-readonly.exe"   # 与 mcp_servers.yaml 的 command 逐字一致
  interpreters:
    - python               # 解释器整串；Windows 写绝对路径，见下方提示
  env_passthrough: []      # 脚本不需要额外环境变量
```

- 解释器通道的 argv 必须是 `[<脚本路径>, <参数…>]`，脚本真实路径必须落在该 Agent 的
  `scripts/` 内——`-c` / `-m` 这类内联代码一律拒；`scripts/` 对模型只读；
- `bash` / `sh` **不得**放行；
- **Windows 本机必须写解释器的绝对路径**（已实测）：受控执行器把 `interpreters` 里的
  整串**原样**交给 `CreateProcess`（`lpApplicationName`），既不查 PATH、也不补
  `.exe` / `.cmd` 后缀——写 `python`、`python.exe`、`py` 都只会得到
  `The system cannot find the file specified`。请写 python.org 安装的绝对路径
  （如 `C:/Users/<you>/AppData/Local/Programs/Python/Python312/python.exe`），
  并把 `AGENT.md` 第 5 步的可执行名同步成**同一串**（白名单按整串逐字比对）。
  Linux / macOS 上执行器走 `exec.Command`（查 PATH），裸名可用。
- 同一份 `basalt.yaml` 的 `budget:` 段也要动：`budget.max_iterations` 缺省 **10**，
  而本 Demo 的 `AGENT.md` 声明 `settings.max_iterations: 20`——Profile 只能**收紧**
  运维上限、**不得**放大（契约 `run-and-errors.md` §6），不调这一项 Run 会在第 10 轮
  落 `limit_reached`。在该段把 `max_iterations` 调到不小于 20。

## 9. 重启

`mcp_servers.yaml` 与 `shell.*` 都**不热加载**。改完重启 Basalt：

```sh
basalt serve
```

确认三件事：启动日志里 `docs-readonly` 连接成功（或给出可读的失败分类）、没有
`mcp server skipped: docs-readonly`、管理面「MCP server」页没有「待发布 / 待重启」标记。
连不上时先点该行的「诊断」——它不启动进程，只看命令定位与否、env 是否就绪；诊断
`unconfirmed` 不代表失败，点「测试」真实连接一次。

## 10. 手动跑一次

管理台「Agents → docs-diff → 手动运行」，或：

```sh
curl -X POST -H "Authorization: Bearer $BASALT_ADMIN_TOKEN" \
  http://127.0.0.1:8080/api/v1/schedules/daily-docs-diff/run
```

**首跑预期**：报告结论是「无历史基线：本次记录 N 份文档的内容哈希作为基线，不判定
新增 / 修改 / 删除」；`changes.json` 里每份文档的 `status` 是 `baseline`，**没有**
`changes_detected` 字段。第二轮同内容再跑，结论应是「本次无变化」。

审计里这条 Run 的 `trigger` 是 `manual`。**手动跑成功不等于定时触发已验过**——到点
触发的证据要另取一次（等一个 `0 15 9 * * *`，或改 cron 到最近一分钟再改回来）。

## 11. 核对六段证据

把 `<run_id>` 换成上一步返回的 Run id，`<workspace>/.basalt/basalt.db` 换成你的库路径。

```sh
# 1 输入快照哈希：脚本 stdout 的 input.documents.sha256 / input.metadata.sha256
#   （报告里不含输入路径与输入哈希——这是刻意的，报告必须可复现）
# 2 时区与计划：AGENT.md 的 schedules 段（6 段 + Asia/Shanghai）
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT task_id, trigger, status FROM task_executions WHERE run_id='<run_id>';"

# 3 候选产物哈希：脚本 stdout 的 report.sha256 / state.sha256
# 4 run_id 与工具调用：三样缺一不可——只读 MCP、记忆、脚本
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT tool_name, status, error_kind FROM tool_invocations WHERE run_id='<run_id>' ORDER BY id;"
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT status, trigger, iterations, tool_calls FROM runs WHERE run_id='<run_id>';"

# 5 业务断言：打开 runs/<时间戳>/changes.json 与 changes.md，按 README「期望业务结果」核对
# 6 通知次数：基线 none 时应为 0；开启 allowlist 后应为 1
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT COUNT(*) FROM tool_invocations WHERE run_id='<run_id>' AND tool_name='notify';"
sqlite3 <workspace>/.basalt/basalt.db \
  "SELECT COUNT(*) FROM schedule_deliveries WHERE run_id='<run_id>';"
```

判据：

- `tool_invocations` 里必须同时有 `list_documents`、`document_metadata`（**每份文档
  一条**）、`recall_memory`、`save_memory`、`shell` 的成功行——缺任何一类即判不通过；
- `changes.json` 的计数必须守恒：新增 + 修改 + 未变 = 本次文档数；删除 + 修改 + 未变 =
  基线文档数。对不上就是**不通过**，不要用「Run completed」代替业务断言；
- 未变文档的 `sha256` 必须等于 `previous_sha256`；变更文档必须不等；
- `schedule_deliveries` 必须为 **0**（D4 不用系统代投）。

**零历史会话核对**：D4 的跨运行基线来自记忆，不靠对话历史。要验证这一条，先把该
Agent 的会话消息清掉（保留 `memory_entries` 与 `state/baseline.json`）：

```sh
sqlite3 <workspace>/.basalt/basalt.db \
  "DELETE FROM messages WHERE session_id IN (SELECT DISTINCT session_id FROM runs WHERE agent='docs-diff' AND session_id IS NOT NULL);"
```

再跑一轮：`recall_memory` 必须仍有成功行，报告的「记忆基线」节必须引用上一轮
`save_memory` 写入的原文（时间、计数、`state.sha256`）。报告写「无历史记忆（首次运行）」
而库里明明有 `docs-diff` 记忆条目 → 不通过（先查 query 与 scope，见第 12 节）。

## 12. 失败时先看哪里

| 现象 | 先查 |
| --- | --- |
| Agent 没注册 | 启动 WARN：`tool not registered` / `mcp server skipped: docs-readonly` |
| MCP 启动即退出 | `-root` 目录不存在或不是目录（server `exit 2`）；`command` 是否与 `allowed_commands` **逐字**相同 |
| `list_documents` 返回空数组 | 根目录指错了：检查 `-root` 绝对路径；只列 `.md`，非 Markdown 不算 |
| 诊断说 `unconfirmed` | 不代表一定失败：点「测试」真实连接一次，结论只看测试结果与失败分类 |
| `shell` 返回 `sandbox_denied` | `shell.interpreters` 未放行，或脚本不在 `scripts/` 下，或 `arguments[0]` 以 `-` 开头 |
| 脚本报 `metadata 缺少文档的元信息` | 没对每份文档调用 `document_metadata`，或聚合时漏项 → 整轮重取后重跑 |
| 脚本报 `size` / `modified_at` 不一致 | 两次读取之间文档变了（有人正在改库）→ 整轮重取；不要把两次快照拼在一起 |
| 报告写「无历史记忆（首次运行）」但上一轮跑过 | `recall_memory` 的 `query` 不是 `docs-diff`；或上一轮 `save_memory` 写成了 `scope: CORE`（核心区**不参与检索**） |
| 首跑里出现「新增」 | 基线记录被当成了变更；首跑只应有 `status=baseline` |
| 通知条数不是 1 | `notify_mode` 还是 `none`，或保存后 `changes.tools_added` 没带 `notify`；每次运行恰好一条，含无变化 |
| `state/baseline.json` 出现在 `git status` | 不应提交：`.gitignore` 已忽略 `examples/daily/docs-diff/state/`；本地工作区里别把它当产物交付 |
