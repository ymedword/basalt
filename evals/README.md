# Basalt 行为评测基座（evaluations）

开发用**固定用例集**回答一个具体问题：**这次改动有没有让 Agent 变笨**。用例以 YAML 承载（输入 / 夹具 / 预算 / **可执行断言**），逐例判 PASS / FAIL，读数落 `evals/results/<时间戳>.json`。

评测**不另建执行路径**：每条用例都经**唯一运行入口**（`AgentService.Run`）跑一次真实运行——路由查表、两阶段模型调用审计、工具调用审计、会话落库、运行账本全在生产那条链上。评测与生产是同一份代码，读数才谈得上是"生产行为"的读数。

## 1. 两条后端分档

| 档位 | 后端 | 何时跑 | 可重复性 |
| --- | --- | --- | --- |
| **fake 档**（`provider:` 留空） | 进程内假模型（`scriptedClient`，按用例脚本逐轮回放） | **恒跑**，无外部依赖，进日常回归 | 完全可重复（脚本用尽即报错，不静默重放最后一条） |
| **真模型档**（`provider: <名>`） | 运维 `basalt.yaml` 里那个供应商 | **显式门控**，四件缺一不发 | 不保证——真模型的用词不是用例能钉死的 |

**跳过不计入通过**：真模型档未门控 / 门控四件不全时，该例记 `[SKIP]` 并进 `totals.skipped`，**不**进 `passed`。把"没跑"读成"过了"是评测最坏的失效形态。

## 2. 目录布局

```text
evals/
├── main.go      运行器（package main：装载 → 执行 → 判定 → 结果文件）
├── main_test.go 运行器自身的单元测试（判定逻辑 / 装载校验 / 报告形态）
├── cases/       默认用例集（四类读数，含一条真模型档门控例）
├── injection/   注入类安全用例集（越权尝试必须四段式拒绝 + 审计 failed）
├── results/     运行产物（每次运行一份；**不入库**，收口前删净）
└── README.md    本文档
```

`cases/` 与 `injection/` 是**两个独立的用例目录**，用同一个运行器分别跑（见 §3）。目录里除 `*.yaml` 外的文件一律忽略。

## 3. 运行方法

必须在**仓库根**执行（用例工作区、`basalt.yaml`、结果目录都是相对进程工作目录解析的）。

```bash
# 默认用例集（fake 档恒跑；真模型档那一条会显示 SKIP）
GOPROXY=https://goproxy.cn go run ./evals --cases evals/cases

# 注入类安全用例集
GOPROXY=https://goproxy.cn go run ./evals --cases evals/injection

# 真模型档（显式门控；凭证从环境变量来）
GOPROXY=https://goproxy.cn go run ./evals --cases evals/cases --provider deepseek

# 可选包装（等价于上面第 1 条，可透传额外参数）
scripts/run-evals.sh
```

参数有三个（没有位置参数）：`-cases`（用例目录，缺省 `evals/cases`）、`-provider`（门控的供应商名）、`-results`（结果目录，缺省 `evals/results`）。CI / 验收入口用独立结果目录，不从历史目录取最近一份报告。

项目自动化统一使用 `sh scripts/check-acceptance.sh --profile evals`（Windows：`.\scripts\check-acceptance.ps1 --profile evals`），同时跑两个目录并核对必跑 ID 和跳过规则；完整门禁用同入口的默认 full profile。CI 已接入相同入口并上传失败证据，详见 [Harness 说明](../harness/README.md)。下表是评测二进制的真实退出码；`go run` 会将非零子进程状态包装，自动化调用编译后的二进制。

**退出码**三档，供脚本判卷：

| 退出码 | 含义 |
| --- | --- |
| `0` | 没有 FAIL（**SKIP 不算失败**） |
| `1` | 有用例 FAIL |
| `2` | 运行器自身出错（用例装载失败 / 结果文件写不出 / 参数非法） |

## 4. 用例格式

一条用例 = 一个 YAML 文件（文件名即装载序，与 `id` 无关）。**未知键一律装载期拒绝**——写错的键名（如 `max_iteration`）如果只是被忽略，用例就会"看起来配了断言"而实际没判。

```yaml
id: tool-selection-read-file     # 用例标识：会话标识由它派生（eval-<id>）、结果条目键、排序键
kind: 工具选择                    # 四类读数之一（见 §5）
provider: ""                      # 留空 = fake 档；非空 = 真模型档（值须与 --provider 逐字相等）
description: 一句话说明这一例在读什么
message: 读一下 config.ini，告诉我里面的端口号是多少。   # 一次运行一条用户消息
fixture:                          # 临时工作区夹具（每例一份独立的 .basalt/，跨例不共享）
  agent: selector-agent           # Agent 目录名（= AGENT.md 的 name）
  agent_md: |                     # AGENT.md 全文（frontmatter + 正文，契约见 docs/contracts/agent-md.md）
    ---
    name: selector-agent
    provider:
      name: probe                 # fake 档固定 probe；真模型档写门控的那个供应商名
      model: eval-probe-model
    tools:
      - read_file
    ---

    正文……
  files:                          # Agent 目录内的文件（相对 Agent 目录）
    config.ini: |
      port=8080
  workspace_files:                # 工作区（.basalt/）内的其他文件（相对工作区根）
    notes.txt: |
      工作区根的备注。
budget:                           # 三项都可省；省 = 核心缺省（10 / 50 / 32768）
  max_iterations: 3               # 只能收紧：Profile 的值更大也压不到上面去
  max_tool_calls: 5
  context_tokens: 32768
script:                           # **只** fake 档可用：假模型的逐轮回放
  - content: 我先读一下配置文件。   # 这一轮的回答正文
    usage:                        # 这一轮回的 token 计量（三项都可省，全零 = 供应商未回用量）
      prompt: 120
      completion: 18
      total: 138
    tool_calls:                   # 这一轮发起的工具调用（可多个；项目内**串行**执行）
      - id: call-config-1         # 审计关联键：expect.audit 按它定位审计行
        name: read_file
        arguments: '{"path": "config.ini"}'
  - content: 配置文件里的端口号是 8080。
expect:
  status: completed               # 必填，四条终止态之一（running 不是终止态，装载期拒绝）
  tool_sequence:                  # 期望工具名序列（逐位相等）；空列表 = 期望一次工具都没调
    - read_file
  text_contains:                  # 最终回答里必须出现的片段（只判 FinalText）
    - 8080
  tool_results_contain:           # **工具结果**里必须出现的片段（只判 role=tool 消息正文）
    - port=8080
  audit:                          # 工具审计行断言（按 tool_call_id 定位；行必须存在）
    - tool_call_id: call-config-1
      status: success             # 审计状态四值之一
      error_kind: sandbox_denied  # 留空即不判；失败分类七值之一
  llm_calls: 2                    # 期望的模型调用次数（= 审计行数）
  min_total_tokens: 372           # token 总用量下界（≥）
  usage_complete: true            # 期望的计量完整性
```

装载期硬校验（不通过则**整批**拒绝，退出码 2）：

- `id` / `fixture.agent` 只许小写字母、数字与连字符（两者会进入会话标识与文件路径）
- `kind` 必须是四类读数之一；`message` / `fixture.agent_md` / `expect.status` 必填
- fake 档**必须**给 `script`（否则第一次模型调用就只能报"脚本已用尽"）；真模型档**不得**给 `script`
- `expect.audit` 的 `tool_call_id` 必须真出现在 `script` 里（断言一条不会发生的调用等于让它空过）
- 夹具路径一律**干净的相对路径**——绝对路径与 `..` 逃逸会让夹具写到临时工作区之外
- 预算三项不得为负；`id` 重复点名两条路径

## 5. 判定口径

| 断言 | 判据 |
| --- | --- |
| `status` | 运行返回的终止状态：`completed` / `limit_reached` / `cancelled` / `failed` |
| `tool_sequence` | 会话里**助手消息**携带的工具名序列（含被拒的工具——发起了就算） |
| `text_contains` | **只**判 `RunResult.FinalText`（达上限与失败时该字段为空） |
| `tool_results_contain` | **只**判 `role=tool` 的消息正文（四段式拒绝文案的判据落在这里） |
| `audit` | 按 `tool_call_id` 逐条定位审计行；行不存在即 FAIL |
| `llm_calls` | 本次运行的模型调用审计行数 |
| `min_total_tokens` | `usage.total` ≥ 给定下界 |
| `usage_complete` | 计量完整性（见 §6） |

**一条用例零条断言不算通过**（兜底与装载期校验两层都有）。`kind` 只影响汇总读数，不影响判定强度。

四类读数（`kind` 的四个取值，首批每类至少一例）：

| 读数 | 在读什么 | 首批用例 |
| --- | --- | --- |
| 工具选择 | 面对多个可用工具时模型选了哪个 | `tool-selection-read-file` |
| 业务结果 | 多步工具链跑完后最终回答是否用上了工具结果 | `outcome-list-and-read` / `outcome-iteration-limit` / `outcome-real-model-answer` |
| 越权尝试 | 越界目标是否被四段式拒绝且运行继续 | `injection-read-outside` + `injection/` 四件 |
| 资源用量 | token 用量、调用次数与计量完整性 | `usage-token-accounting` |

## 6. 资源用量字段说明

读数一律称**资源用量**：**token 用量 + 调用次数**。**不换算金额**——金额不在本项目的数据模型里，结果文件里**零金额字段**（`cost` / `money` / `price` / `usd` / `amount` / `金额` 六词零命中，由运行器自身的单元测试钉住）。

`usage` 三列平铺（与模型调用审计表同构）：`prompt` / `completion` / `total`。三列是**所有审计行的求和**——包括失败行（失败行的三列正常为零，那是事实而不是缺失）。

**计量完整性**（`usage_complete` + `usage_note`）判不完整的三支：

| 分支 | 症状 |
| --- | --- |
| 无行 | 本次运行一条模型调用审计行都没有（读数无从谈起） |
| 悬空 / 未知 | 任一行仍是 `pending`（预写后没等到终态回填）或 `unknown`（恢复扫描归一），`usage_note` 点名轮号 |
| 成功行三列全零 | 调用成功但供应商未回用量——**不得**把库中默认的零读成零成本 |

失败行三列为零**不**判不完整：调用没成功就没有用量，那是事实而不是缺失。

## 7. 真模型档门控（缺一不发）

四件**全齐**才跑，缺任何一件该例按 SKIP 处理：

1. 命令行给了 `--provider <名>`，且 `<名>` 与该例的 `provider:` **逐字相等**（门控的是别的供应商时不跑）
2. 仓库根有 `basalt.yaml`——真模型档把它**原样复制**进用例工作区。真模型读的就是运维写的那份配置，评测不替它改编
3. `basalt.yaml` 里有同名供应商条目（缺 → SKIP 并点名）
4. 该条目引用的**凭证环境变量已导出**（缺 → 配置装载 fail-closed 拒绝 → 该例按 SKIP 处理）

**凭证红线**：一切凭证只允许 `${ENV_VAR}` 占位，**不得**明文写进用例、配置或结果文件。

```bash
# 凭证走环境变量；命令历史里不落明文
export DEEPSEEK_API_KEY="..."   # 值从密钥管理/凭证面板取，不写进任何文件
GOPROXY=https://goproxy.cn go run ./evals --cases evals/cases --provider deepseek
```

真模型例的断言只判**结构读数**（终止状态 / 调用次数 / 用量 / 计量完整性）——真模型的用词不是用例能钉死的。

## 8. 结果文件

每次运行落一份 `evals/results/<YYYYMMDD-HHMMSS>.json`（同一秒内连跑不覆盖，顺延 `-2` / `-3`）——"连跑两次读数一致"这条复核依赖两份文件都在。

顶层字段：`generated_at` / `cases_dir` / `provider_gate` / `estimate_version`（token 估算器版本，当前 `v1`）/ `reserve_tokens`（估算预留量，当前 `1024`）/ `totals`（`total` / `passed` / `failed` / `skipped`）/ `kinds`（四类读数计数）/ `cases`。

每例字段：`id` / `kind` / `backend`（`fake` 或 `provider:<名>`）/ `session_id` / **`run_id`**（运行标识，据此可回到运行账本与会话） / `status` / `iterations` / `tool_calls` / `tool_sequence` / `llm_calls` / `usage` / `usage_complete` / `usage_note` / **`audit`**（`tool_call_id` + `status` + `error_kind`，与工具审计表的关联） / `checks`（逐条断言的判定与两侧读数） / `passed` / `skipped` / `skip_reason` / `error`。

`evals/results/` 是**运行产物**：不入库、收口前删净（与"探针与临时物不进仓库"是同一条纪律）。

## 9. 明确的边界

- **不新增评测表**：读数全部取自既有表（会话消息 / 模型调用审计 / 工具调用审计 / 运行账本），评测没有自己的库
- **不换算金额**：只读 token 用量与调用次数
- **不跑并行**：一轮内多工具是**串行**执行（与生产一致）
- **不替真模型编写答案**：真模型档禁 `script`
- **验收入口**：原 `make check` 保持七项目标，新增 `make acceptance` / `sh scripts/check-acceptance.sh` 将固定评测作为必验项，CI 调用统一入口；可选真模型 SKIP 不代表真实能力已验证
