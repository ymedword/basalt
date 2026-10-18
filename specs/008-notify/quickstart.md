# Quickstart: US-8 通知工具（notify）验证指南

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Data Model**: [data-model.md](./data-model.md) | **Date**: 2026-10-18

**用途**：按本文件可复现地验证 `notify` 的自动化判据与人工验收项。**前置**：实现与测试已按 [tasks.md](./tasks.md) 落地（本文件不重复实现步骤）。所有 Go 命令**必须**带 `GOPROXY=https://goproxy.cn`；凭证一律经环境变量，**不得**写入任何文件或命令行历史。

## 1. 环境与基线核对

```sh
# 分支与工作区（应为 008-notify；开工基线改动按 run-state 判定，不重排无关文件）
git branch --show-current
git status --short

# 零依赖：本 US 不得产生 diff（预期：无输出）
git diff -- go.mod go.sum

# 零迁移：本 US 不得产生 diff（预期：无输出）
git diff -- internal/storage/schema.sql docs/contracts/schema.sql
ls internal/storage/migrations/ 2>/dev/null
```

**预期**：分支为 `008-notify`；两条 `git diff` 均为空；启动后 `schema_version` 与基线一致（迁移文件零新增）。

## 2. 自动化验证

### 2.1 本 US 全部测试（默认层，无外呼）

```sh
GOPROXY=https://goproxy.cn go test -race -count=1 ./internal/core ./internal/storage ./internal/tool ./internal/app
```

**预期**：四个包全绿；42 条断言（41 新增 + 1 修改）全部实际执行，无 `SKIP`。集成层本 US **无**：真实外呼归 §3 人工项（`test-integration` 空跑 + 依据，见颗粒度文档 §4.1）。

### 2.2 专属 check（逐条，与 stage 的 checks 数组一一对应）

| check ID | 命令 | 预期 |
| --- | --- | --- |
| `008-envref` | `GOPROXY=https://goproxy.cn go test -json -count=1 ./internal/core -run '^TestEnvPlaceholderName_ExactFormTable$'` | `TestEnvPlaceholderName_ExactFormTable` PASS |
| `008-executor-audit` | `GOPROXY=https://goproxy.cn go test -json -count=1 ./internal/core -run '^(TestToolExecutor_PreWriteFailureRefusesExecution\|TestToolExecutor_TerminalWriteFailureKeepsPendingRow)$'` | 两个既有用例 PASS（**必跑不重造**） |
| `008-storage-notify` | `GOPROXY=https://goproxy.cn go test -json -count=1 ./internal/storage -run '^TestNotifyChannelRepo_'` | S1~S4 PASS |
| `008-tool-notify` | `GOPROXY=https://goproxy.cn go test -json -count=1 ./internal/tool -run '^TestNotifyTool_'` | N1~N21 PASS |
| `008-tool-notify-adapters` | `GOPROXY=https://goproxy.cn go test -json -count=1 ./internal/tool -run '^(TestWebhookNotifyAdapter_\|TestFeishuNotifyAdapter_\|TestNotifyAdapters_)'` | A1~A13 PASS |
| `008-app-notify` | `GOPROXY=https://goproxy.cn go test -json -count=1 ./internal/app -run '^(TestNewNotifyAdapters_\|TestAgentsPort_LightPathAcceptsNotifyDeclaration$)'` | P1~P2 PASS |
| `008-ports-nine` | `GOPROXY=https://goproxy.cn go test -json -count=1 ./internal/app -run '^TestToolListerPort_HeavyAssemblyRegistersNineTools$'` | 恰好九件（判据面变更，人工 review ① 已批准）PASS |

判定纪律：`required_tests` 必须出现 `Action: "pass"`；空匹配 / `skip` / `fail` 均判该项未通过。

### 2.3 阶段验收与全量门禁

```sh
# 任务级（stage 由步骤 6 从 harness/stage.example.json 派生，路径以实际文件为准）
.\scripts\check-acceptance.ps1 --stage harness/stages/us8-notify-r1.json --task <task-id>

# 最终验收：无 --task（公共门禁 + 全部阶段断言并集）
.\scripts\check-acceptance.ps1
```

```sh
# 完成的定义（七目标；-race 与 CGO_ENABLED=0 分属不同目标，不得合并）
make check
```

**预期**：`make check` 七目标全绿且逐目标留读数；`check-acceptance` 无 `--task` 通过。公共全绿**不替代** 2.2 的专属断言，专属子集也**不得**替代公共门禁。

### 2.4 覆盖率

```sh
GOPROXY=https://goproxy.cn make cover
```

**预期**：`core` / `tool` / `storage` ≥ 70%，`app` ≥ 50%；`internal/core/notify.go` 无可执行语句不产生独立判卷项，`envref.go` 计入 `core` 包覆盖率。

### 2.5 依赖方向与红线自查

```sh
# core 只依赖标准库（预期：输出中无 basalt/internal/...）
GOPROXY=https://goproxy.cn go list -f '{{.ImportPath}}: {{join .Imports " "}}' ./internal/core

# tool 不得 import app / cli / workspace / storage（预期：无输出）
GOPROXY=https://goproxy.cn go list -f '{{join .Imports "\n"}}' ./internal/tool | grep -E 'basalt/internal/(app|cli|workspace|storage)'

# 框架坐标（预期：无输出）
grep -nE "eino|langchaingo|adk-go|trpc-agent-go" go.mod

# context.WithValue（预期：仅既有追踪标识用法，逐处人工确认）
grep -rn "context.WithValue" --include='*.go' cmd internal

# 抑制注释（本 US 零使用；预期：无输出）
grep -rnE "//nolint|#nosec|//lint:ignore" --include='*.go' internal/core internal/storage internal/tool internal/app
```

## 3. 人工验收场景（§6.1 剩余项）

### 3.1 手工登记渠道（人工执行）

在 **sqlite3 交互提示符内**执行（避免 shell 展开 `${`）：

```text
sqlite3 .basalt/basalt.db
INSERT INTO notify_channels(name,type,url,description,created_at)
VALUES('ops','webhook','${BASALT_NOTIFY_OPS}','演示渠道', cast(strftime('%s','now') as integer)*1000);
SELECT name, type, length(url), url LIKE '${%}' AS is_ref FROM notify_channels;
.quit
```

**预期**：`is_ref = 1`；**不得**执行会打印 `url` 原值的查询。非交互场景必须用带引号的 heredoc（`<<'SQL'`）防止 shell 展开。

### 3.2 真 webhook 发一条（人工执行）

```sh
# 凭证只存在于当前进程环境；不要写入文件、不要 echo
export BASALT_NOTIFY_OPS='<真实 webhook>'
./basalt chat    # 让 Agent 调用 notify：channel=ops, content=巡检摘要
unset BASALT_NOTIFY_OPS
```

**预期**：接收端确实收到消息；工具结果逐字为 `已推送到通知渠道：ops`；审计行 `success`。

### 3.3 飞书业务失败（人工执行）

将渠道 `type` 改为 `feishu`，配置一个**错误关键词**（接收端校验关键词）：

**预期**：HTTP 200 且回执 `code=19024`；工具判 `external_error`；审计 `error_kind=external_error`；错误文案含 `code=19024`，**不含**回执正文。

### 3.4 域名白名单拒绝（AI 执行、人审证据）

把渠道域名从 `http.allowed_domains` 移除（或换成未登记域名）→ **重启进程**（配置不热加载）→ 再次调用。

**预期**：`sandbox_denied`；四段式错误段 2 只有 `scheme://host`，无端口 / userinfo / path / query；接收端零收到。

### 3.5 环境变量负向（AI 执行、人审证据）

```sh
unset BASALT_NOTIFY_OPS
# 重启进程后调用 notify(channel=ops, content=...)
```

**预期**：`runtime_error`，错误点名 `${BASALT_NOTIFY_OPS}`；不回显任何环境值；接收端零收到。

### 3.6 凭证面复核（人工执行，凭证在手）

对以下三处用真实 token 前缀执行 `grep`（token 从环境变量读取，**不要**把 token 写进命令参数）：

```sh
grep -F "$BASALT_NOTIFY_OPS" .basalt/logs/* 2>/dev/null
sqlite3 .basalt/basalt.db "SELECT error_message FROM tool_invocations ORDER BY id DESC LIMIT 20;"
```

**预期**：零命中；所有对外文案只出现 `scheme://host`。

### 3.7 其余 AI 执行项

| 项 | 命令 / 操作 | 预期 |
| --- | --- | --- |
| `tool list` | `./basalt tool list` | 恰好九件，含 `notify`，描述与 schema 非空 |
| Agent 声明 | 在 `AGENT.md` 的 `tools:` 声明 `notify` 后启动 + `./basalt profile list` | 注册成功、可聊；轻路径同结论 |
| 四目标编译 | `CGO_ENABLED=0 GOOS=<os> GOARCH=<arch> go build ./...`（四目标逐一） | 全部通过；`-race` 单独一条命令跑 `internal/tool` / `internal/app` |
| 生效时机 | 改环境值 → 重启生效；改渠道行 → 下一发生效（无需重启） | 与 §2.2 约束 6 一致 |

## 4. 预期失败形态（负向验证清单）

| 操作 | 预期失败 | 不得出现 |
| --- | --- | --- |
| `channel` 缺失 / 纯空白 / 错名 | `not_found` + 可用渠道名；零发送 | 默认渠道猜测、静默成功 |
| `content` 超 20 × 1024 字节 | `invalid_input`；零查表、零发送 | 截断后发送 |
| 渠道 `url` 为明文网址 | `runtime_error`；文案不回显地址 | 地址出现在错误 / 日志 / 审计 |
| `${VAR}` 未设置 / 空串 | `runtime_error`；点名变量；零发送 | 把 `${VAR}` 当 URL 发送 |
| 渠道 `type` 未注册 | `runtime_error`；零 env / 零 policy / 零发送 | 落到默认适配器 |
| 飞书回执不可解析 / 超 64 KiB | `external_error`「飞书回执无法解析」 | 截断解析后报成功 |
| webhook 返回 3xx | `external_error`；不跟随重定向 | 跟随跳转后报成功 |

## 5. 清理

- 人工演示结束后 `unset BASALT_NOTIFY_OPS`；**不得**把真实地址写入 `.env` / 脚本 / 文档。
- 临时探针（如有）建在**仓库外**，用毕即删；`specs/008-notify/run-state.md` 与阶段配置**不**属临时物（收口保留）。
