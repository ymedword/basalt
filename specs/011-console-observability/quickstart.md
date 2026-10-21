# Quickstart: US-11 管理台与运行观测

**Branch**: `011-console-observability` | **Date**: 2026-10-21 | **Scope**: 验证向导（不含实现代码）；实现细节以 `tasks.md` 与代码为准。

本文件回答一个问题：**拿到这个分支后，怎样用可复制的命令证明 US-11 真的工作**。分三层——门禁、专属 check、人工 Demo。

## 0. 前置条件

| 项 | 要求 |
| --- | --- |
| Go | ≥ `toolchain go1.27.2`（`go version` 核对） |
| 模块代理 | 显式 `GOPROXY=https://goproxy.cn`（直连不可达） |
| 工作区 | 已 `basalt init`（含 `.basalt/` 与 `basalt.yaml`） |
| 凭据 | 管理 token 一律经 `${ENV_VAR}`——演示前 `export BASALT_ADMIN_TOKEN=...`，**不得**写入任何文件 |
| Node | **不需要**；前端构建只在 CI 的 Node job 执行。本机无 Node 不影响 Go 门禁 |
| 终端 | Windows 用 PowerShell / Git Bash；PowerShell 脚本路径 `.\scripts\check-acceptance.ps1` |

## 1. 全量门禁（完成的定义）

```bash
# 七目标：fmt / lint / sec / test / test-integration / cover / build
make check
```

预期：七目标全绿；覆盖率 `core` / `tool` / `memory` / `storage` ≥ 70%，其余 ≥ 50%；`CGO_ENABLED=0` 构建通过。**该命令不得调用 Node / npm。**

```bash
# 竞态（与发布构建分属不同命令）
go test -race ./internal/... 
```

```bash
# 集成标签（默认不执行，必须单列）
go test -tags=integration ./...
```

## 2. 本 US 专属 stage（十七个 check）

```bash
# Git Bash
sh scripts/check-acceptance.sh --stage harness/stages/us11-console-observability.json

# PowerShell
.\scripts\check-acceptance.ps1 --stage harness/stages/us11-console-observability.json
```

预期：十七个 check 全 PASS；`required_tests` 逐个精确匹配（空匹配或 SKIP 视为 FAIL）。check 清单见颗粒度文档 §4.4；stage 文件 `inputs` 含私有 `docs/us/*`，**只在本机使用**，不进 `current.json`、不进 CI。

按任务单独跑（实现期逐任务验收）：

```bash
.\scripts\check-acceptance.ps1 --stage harness/stages/us11-console-observability.json --task T012
```

## 3. 前端一致性（CI 语义，本机无 Node 时跳过）

CI 的 Node job 执行：

```bash
cd assets/console
npm ci
npm test              # Vitest
npm run build
cd ../..
git status --porcelain -- assets/console/dist   # 必须为空
```

Go 侧另有测试校验 `dist/` 与 `integrity.json` 的集合与逐文件 sha256 相等。本机无 Node 时，以 CI 结果作为该层证据，并在实施报告标注「本机未跑」。

## 4. 六条人工 Demo（对应 spec 的七个用户故事）

每条都需要**真实进程 + 临时工作区**，按颗粒度文档 §6.2 执行并留证（命令 / 输出 / 截图，不含凭证）。

### Demo A｜认证闸门（P1 / Story 1）

1. 非回环监听 + 缺 `http.admin_auth` → 启动失败，文案点名该配置键。
2. `mode=token` + 未设置 token 环境变量 → 启动失败；token 写成明文 → 启动失败。
3. 正确启动后：`/api/v1/health` 200；`/api/v1/profiles` 无凭据 401；错误 token 401；伪造 Host 403；连续 10 次错误凭据 → 429 + `Retry-After`。
4. `mode=local_trust` + 回环 + `X-Basalt-Local-Trust: 1` → 200；附加任意 `X-Forwarded-For` → 403。

### Demo B｜生命周期基线（P1 / Story 3）

1. 用 US-10 旧库启动 → 基线完成；`/profiles` 显示既有 Agent `state=disabled`、`runnable=false`。
2. 启用 → `generation` 1 → 2；HTTP 发消息与 `basalt chat` 均可运行。
3. 手改状态文件或删除 → 新运行 409 `AGENT_UNAVAILABLE`，`/profiles` 显示 `lifecycle_error`，进程不崩。

### Demo C｜内容条件写（P2 / Story 4）

1. `GET /agents/<name>/content` 取得 ETag；用旧 ETag `PUT` → 412，文件哈希不变。
2. 刷新后保存成功；补丁只改 frontmatter 字段，注释与正文逐字节保留。
3. 补丁触碰正文 → 400 `EDIT_UNSUPPORTED`，零写入。

### Demo D｜运行观测与取消（P1 / Story 2）

1. 发起运行并结束；运行详情显示准入清单（Provider / MCP / 技能版本与哈希）。
2. 历史运行显示 `legacy_unrecorded`，无清单。
3. 取消在跑运行 → 200 + 运行视图最终为 `cancelled`；重复取消返回当前状态。

### Demo E｜热加载边界（P2 / Story 5）

1. 修改 `AGENT.md` → 新运行用新配置，在途运行不受影响。
2. 改坏 → 旧快照继续服务，WARN 点名文件，`/profiles` 保留旧条目。
3. 删除 / 新建 Agent 目录 → 注册表与调度器同步增减。
4. 改 `mcp_servers.yaml` / 沙箱白名单 → 页面显示"需重启"，运行行为不变。

### Demo F｜管理台（P3 / Story 7）

1. 打开 `/console/` → 登录页；刷新后要求重新输入（token 只在内存）。
2. 登录后总览显示版本、运行时长、计数、DB 状态；首次采样 CPU 为 `null` + `unavailable`，5 秒后出现读数。
3. Agent 详情六个页签可用；配置页保存带 ETag。
4. 深链接刷新正常；未知路径 404 视图；未保存离开有确认；401 → 回登录并保留 `next`。

## 5. 常见失败与判读

| 现象 | 可能原因 | 处理 |
| --- | --- | --- |
| 启动失败且文案点名 `http.admin_auth` | `serve` / `gateway` 未配置认证 | 按提示补配置；token 用 `${ENV_VAR}` |
| 全部 API 401 | 缺 `Authorization: Bearer` | 带上 token；确认没有多余空格 |
| `local_trust` 403 | 非回环 / 有转发头 / 缺本地信任头 | 按三者逐一核对 |
| 429 且 `Retry-After` | 同来源失败达 10 次 | 等窗口过期；**不要**用重启规避 |
| 新运行 409 `AGENT_UNAVAILABLE` | Agent 为 `disabled` 或状态文件损坏 | 显式启用；损坏时按错误文案修复文件 |
| 保存 412 | ETag 过期 | 重新 `GET` 后重试 |
| 保存 400 `EDIT_UNSUPPORTED` | 补丁触碰正文 / 锚点不唯一 | 改用整份替换 |
| `/profiles` 缺 Agent | 注册不变量不满足（资源 / 技能问题） | 看日志 WARN 与资源只读面 |

## 6. 证据清单（收口时）

1. `make check` 摘要（七目标 + 覆盖率数字）。
2. stage 报告（十七个 check + `required_tests` 计数）。
3. 六条 Demo 的命令与输出（不含凭证）。
4. 契约对照表：每个端点 / 视图字段 → `openapi.yaml` 行号。
5. 判据面变更逐项说明（无则记"零变更"）。
6. 未验证项（如本机无 Node、Windows 采样）与依据。
