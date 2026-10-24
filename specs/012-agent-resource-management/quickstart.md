# Quickstart: US-12 验证向导

**Date**: 2026-10-22 | **Plan**: [plan.md](./plan.md) | **Data model**: [data-model.md](./data-model.md)

本文件是**验证 / 运行向导**，不是实现说明。固定字面量以 [docs/contracts/](../../docs/contracts/) 为准，测试清单与 check 定义以模块卡 §4.4 与 `harness/stages/us12-agent-resource-management.json` 为准。

## 1. 前置条件

| 项 | 要求 |
| --- | --- |
| 工作区 | 分支 `012-agent-resource-management`；工作树无基线外改动 |
| Go | 本机版本 ≥ `go.mod` 的 `toolchain` 声明（实测 `go1.27.3`） |
| 代理 | 每个 Go 命令前显式 `GOPROXY=https://goproxy.cn` |
| Node | **不需要**——Go 门禁不调用 Node / npm；前端测试与构建只在 CI 的 Node job 执行 |
| 私有输入 | `docs/us/us12-agent-resource-management.md` 在本机存在；stage 含该输入，因此**只在本机跑**，不得进 CI |

## 2. 门禁（完成的定义）

```sh
# 全量七目标（fmt lint sec test test-integration cover build）
GOPROXY=https://goproxy.cn make check
```

判卷纪律（三条，逐条可复核）：

```sh
# ① 格式：先查退出码，再判输出（两者都非空 / 非零才失败）
gofmt -l . ; echo "gofmt exit=$?"
gofumpt -l . ; echo "gofumpt exit=$?"

# ② 竞态与发布构建分属不同命令
go test -race ./...
CGO_ENABLED=0 go build ./...

# ③ build tag 测试默认不跑，必须单列
go test -tags=integration ./...
```

覆盖率由 `make cover` 判失败：`core` / `tool` / `memory` / `storage` ≥ 70%，其余 ≥ 50%；有可执行语句但无测试文件的包计 0%。

## 3. 本 US 专属验收（三十二个 check：R3 十八 + R4 十四）

```sh
# 全量阶段验收（Linux / Git Bash）
sh scripts/check-acceptance.sh --stage harness/stages/us12-agent-resource-management.json

# Windows PowerShell
.\scripts\check-acceptance.ps1 --stage harness/stages/us12-agent-resource-management.json

# 单任务 / 单 check（实现期逐任务）
sh scripts/check-acceptance.sh --stage harness/stages/us12-agent-resource-management.json --task T0xx
```

期望：**三十二条** check ID 全部出现且 PASS（R3 十八条 + R4 十四条；不带 `--stage` 只跑公共清单，R4 的 14 条不会执行）；每条 `required_tests` 按精确函数名真跑（`--- PASS:` 逐条比对），空匹配或 SKIP 视为失败。

| # | check ID | 包 | 超时 |
| --- | --- | --- | --- |
| 1 | `012-core-author-agent` | `internal/core` | 180s |
| 2 | `012-core-resource-types` | `internal/core` | 180s |
| 3 | `012-workspace-yaml-provider` | `internal/workspace` | 180s |
| 4 | `012-workspace-skill` | `internal/workspace` | 180s |
| 5 | `012-workspace-agent` | `internal/workspace` | 180s |
| 6 | `012-storage-admin` | `internal/storage` | 180s |
| 7 | `012-app-resource-admin` | `internal/app` | 240s |
| 8 | `012-app-agent-admin` | `internal/app` | 240s |
| 9 | `012-app-mcp-supervisor` | `internal/app` | 240s |
| 10 | `012-app-reconcile` | `internal/app` | 180s |
| 11 | `012-app-notify-author` | `internal/app` | 180s |
| 12 | `012-web-admin-agents` | `internal/web` | 180s |
| 13 | `012-web-admin-resources` | `internal/web` | 180s |
| 14 | `012-web-admin-notify-workspace` | `internal/web` | 180s |
| 15 | `012-web-etag-routes` | `internal/web` | 180s |
| 16 | `012-cli-author` | `internal/cli` | 60s |
| 17 | `012-e2e` | `internal/e2e`（`-tags=integration`） | 300s |
| 18 | `012-race` | core / storage / workspace / tool / web / app | 900s |
| 19 | `012-core-identity` | `internal/core` | 180s |
| 20 | `012-core-scope` | `internal/core` | 180s |
| 21 | `012-workspace-bindings` | `internal/workspace` | 180s |
| 22 | `012-storage-identity` | `internal/storage` | 300s |
| 23 | `012-migration-v3` | `internal/storage` + `internal/app` | 300s |
| 24 | `012-app-binding` | `internal/app` | 180s |
| 25 | `012-app-scope` | `internal/app` | 180s |
| 26 | `012-app-notify-mode` | `internal/app` | 180s |
| 27 | `012-credential-managed` | `internal/app` + `internal/cli` | 300s |
| 28 | `012-credential-leak-sentinel` | `internal/app` | 300s |
| 29 | `012-web-cfg-structured` | `internal/web` | 180s |
| 30 | `012-web-credential` | `internal/web` | 180s |
| 31 | `012-console-config` | `internal/web` | 180s |
| 32 | `012-e2e-v3` | `internal/e2e`（`-tags=e2e`） | 300s |

check 的精确 `argv` 与 `required_tests` 见模块卡 R4.12 与 stage JSON；族名只是生成 `-run` 正则的简写，落盘必须是 `go test -list` 的精确函数名。R4 检查的真实 `argv` / 包与超时以 stage JSON 为准（本表是导航摘要）。

## 4. 前端（CI job，本机无 Node）

```sh
cd assets/console
npm ci
npm test
npm run build
```

构建后校验由 CI 执行：`assets/console/dist` 的文件集合与 `integrity.json` 的 path 集合一致、逐文件 sha256 相等，且 `git status --porcelain -- assets/console/dist` 为空（`??` 也算不一致）。

## 5. 人工 Demo（九条，按模块卡 §6.2）

启动服务（示例，凭证一律 `${ENV_VAR}`）：

```sh
export BASALT_ADMIN_TOKEN='${BASALT_ADMIN_TOKEN}'
GOPROXY=https://goproxy.cn go run ./cmd/basalt serve --config basalt.yaml
```

| Demo | 操作 | 期望 |
| --- | --- | --- |
| A 全流程 | `/console/agents/new` 输入名字与描述 → 编辑草稿 → 提交 → 启用 → HTTP 调一次 | 草稿可见；提交后磁盘出现 `AGENT.md`，`disabled` / generation=1；启用后 Run 成功；同名 / 非法名 400 |
| B 禁用一致性 | 复制 `disabled` Agent；对 `disabled` Agent 发 HTTP 运行 | 副本 `disabled`、schedules 空；运行 409 `AGENT_UNAVAILABLE`；管理写仍可进行 |
| C 发布失败保旧 | 改模型连接 `base_url` 发布；注入 DB 失败或只读文件系统再发布；发布旧版本 | 首次新版本生效；失败时列表与运行仍用旧版本；撤回生成**新**版本号，`reverted_from_version_id` 指向旧版 |
| D 删除闸门 | 被 `skills:` 引用的技能删除；有活动 Run 引用时删除；解除后删除并重建 | 400 且 `data.agents` / `data.active_runs` 点名；解除后 200；重建后 `version_no` 继续递增 |
| E MCP 待重启与授权 | 新建 stdio MCP → 重启 → 测试；新建 http MCP → 测试；填本地 IP | 首启 `pending_restart=true`、`connected=false`；重启后追平；测试返回 `ok` / `latency_ms` / `tools`，零 Run、零 `llm_calls`；IP 字面量按既有规则拒绝 |
| F 技能导入边界 | 导入 `../escape`、软链接、>256 KiB、>2 MiB、200+ 文件；合法导入；同名导入 | 越界全部 400；合法导入工作副本 + 只读快照；同名 409；校验端点同口径 |
| G 通知引用 | 新建渠道（`${ALERT_WEBHOOK}` / 明文 URL）；Agent 正文整词提到渠道名后删除；更新留空 url | 列表只回 `url_host`；明文 400；被引用删除 400 点名，`?force=true` 才删；留空保留旧值；环境变量缺失发送失败记 ERROR |
| H 归档与恢复 | 归档空闲 Agent；归档长跑 Agent；原名恢复；异名恢复；注入 rename 失败 | 目录进 `archive/<agent>-<stamp>/`，状态 `archived`；长跑 30 秒后 409 `AGENT_BUSY` 且状态文件 / 目录未动；原名恢复 `disabled`、generation +1；异名恢复 `Bootstrap` 且原归档保持 `archived`；rename 失败回退 `archived` |
| I 工作区与资源页 | `/console/workspace` 浏览；资源详情写操作 | 符号链接不跟随；>256 KiB 返回 400 不截断；`.staging` / `.backup` 不可见；412 后重读 ETag 并保留输入；写后重新读取 |

## 6. 判据面与收口提醒

- 允许的既有测试适配仅限模块卡 §3.5 的九项；其余删除断言 / `t.Skip` / 放宽阈值 / 改 `-run` 选择面属**判据面变更**，必须停下确认。
- 自动验收全绿 ≠ 人工收口通过：仍需独立复核与人工 review ②。
- 未验证项（MCP HTTP、Windows 进程树、30 秒归档等待、对账幂等、前端构建）在 [run-state.md](./run-state.md) §4 登记，实施后按实测翻转并注日期与证据。
