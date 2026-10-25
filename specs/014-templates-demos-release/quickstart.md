# Quickstart: US-14 验证向导

**Date**: 2026-10-25 | **Plan**: [plan.md](./plan.md) | **Spec**: [spec.md](./spec.md)

本文件是**实现完成后**的验证向导：按 14A → 14B → 14C 顺序可复制执行，给出每步的预期读数与失败语义。字段与判据的权威定义见 [data-model.md](./data-model.md)、模块卡 §3.2（D-01–D-10）与 [docs/contracts/openapi.yaml](../../docs/contracts/openapi.yaml)；本文件不重复字段表。

## 0. 前置条件

| 项 | 要求 | 本机读数（2026-10-25） |
| --- | --- | --- |
| 分支 | `014-templates-demos-release`，工作区干净（`specs/**` 除外） | 分支已建，起始 commit `e2e285f` |
| Go | 匹配 `toolchain` 声明（≥ `go 1.27`） | 1.27.3 |
| 模块代理 | `GOPROXY=https://goproxy.cn`（本机直连不可达） | 已设置 |
| Node | 24.22.0（控制台 / 站点构建期；`make check` 不调用） | 便携安装于 `%TEMP%`，PATH 无 `node` |
| Python | 3.x（示例脚本；Windows 上 `python` 可能是商店存根，用 `py`） | 3.12 |
| 候选二进制 | `make build` 产物（14C） | 实施后生成 |

凡是**未执行**的步骤不得写成通过；缺少解释器 / Node / 网络时按失败处理，并在 run-state 未验证清单登记。

## 1. 14A：目录 / 闸门 / 诊断 / 测试记录 / 工具来源

### 1.1 包级测试（由 stage 逐 check 调用）

```sh
go test -count=1 ./internal/catalog
go test -count=1 ./internal/app -run '^TestMCPEnvGate'
go test -count=1 ./internal/app -run '^TestMCPDiagnostics'
go test -count=1 ./internal/app -run '^TestMCPTest'
go test -count=1 ./internal/app ./internal/tool -run 'TestMCPToolDisposition\|TestRegistry_Duplicate'
go test -count=1 ./internal/web -run 'TestMCP'
go test -count=1 ./internal/tool -run 'TestMCPPool_ConnectDetailed\|TestNewMCPCommand'
```

**预期**：全部 `ok`，0 FAIL、0 SKIP（除已写明理由的 `t.Skip`）。空匹配（`no tests to run`）**不算通过**——stage 用 `required_tests` 复核。

**关键红态**（实现改错必须变红）：

| 逆改点 | 会红的用例 |
| --- | --- |
| 闸门移到 `Commit` 之后 | `TestMCPEnvGate_CreateMissingEnvNoWrite`（文件内容变化） |
| 幂等发布也校验 / 或跳过却宣称已满足 | `TestMCPEnvGate_IdempotentPublishSkips` |
| 诊断改调 `Start` 或改用单独 `exec.LookPath` | `TestMCPDiagnostics_*`（进程计数 / 三态不符） |
| 诊断 `unconfirmed` 时测试直接放弃 | `TestMCPTest_UnconfirmedEntryStillAttempts` |
| 空工具清单改写成 `tools_list_failed` | `TestMCPTest_EmptyToolsOK` |
| 连接失败统一记 `failed` | `TestMCPTest_FailureClassification`（状态矩阵） |
| 改 `Registry` 注册语义 | `TestRegistry_DuplicateSemanticsUnchanged` |

### 1.2 契约测试

```sh
go test -count=1 ./internal/web -run 'TestMCP'
```

**预期**：`GET /api/v1/catalog/mcp` 与 `GET /api/v1/mcp-servers/{name}/diagnostics` 的响应字段逐字符合 D-01 / D-02；测试端点 503 / 504 均带 `AdminError.Data.operation_id` 且不返回结果对象；`McpServerView.tool_disposition` 满足 D-06 且 `offered = accepted + len(skipped)`。

### 1.3 全量门禁（14A 收口）

```sh
make check
```

**预期**：`fmt lint sec test test-integration cover build` 七目标全绿；覆盖率按包达标；`go.mod` / `go.sum` / `schema.sql` 无 diff。

### 1.4 控制台

```sh
npm --prefix assets/console test
npm --prefix assets/console run build
```

**预期**：Vitest 全绿；`build` 重建 `assets/console/dist`，`integrity.json` 与 dist 文件集合逐个 sha256 一致，`git status --porcelain -- assets/console/dist` 为空。

### 1.5 管理台人工走查（中文界面）

1. 打开管理台 → MCP 目录页：六条配方按 id 升序；可筛选；`verified=false` 显示"配方未实测"，**不得**出现"已验证"徽章。
2. 选一条"添加到配置"→ 表单被预填、**未自动保存**；直接保存（不设环境变量）→ 503 提示"未配置环境变量"+ 操作详情入口；`mcp_servers.yaml` 哈希不变。
3. 对已发布 server 点"诊断"→ 页面分轴显示命令状态 / env 布尔 / 是否待重启；**不产生进程、不新增操作行**（`management_operations` 行数不变）。
4. 点"测试"→ 页面显示 `operation_id`、被测版本、阶段、失败分类、工具名单；503 / 504 时用 `operation_id` 打开操作详情读到 `stage` / `failure_class`。

## 2. 14B：示例 / 模板 / 只读 MCP

### 2.1 docs-readonly 单测

```sh
go test -count=1 ./examples/shared/mcp/docs-readonly
```

**预期**：`ok`；越界 / 绝对路径 / 软链接 / 写操作被拒绝，`initialize` / `tools/list` / `tools/call` 三件工具行为符合 README。它在主 module 内，根 `go test ./...` 同样覆盖；stage 另用 `-run ^TestDocsReadOnly_` 固定命名用例。

### 2.2 示例脚本测试

```sh
python scripts/run-example-tests.py
```

**预期**：五个 `examples/daily/*/scripts` 逐目录输出 `Ran N tests`，每目录 N ≥3，退出 0。0 发现 / 低于下限 / 非零退出 / 缺解释器 = 失败。

### 2.3 五 Demo 与八模板（e2e 夹具层）

```sh
go test -json -tags=e2e -count=1 ./internal/e2e -run '^TestUS14Demos_Fixture'
go test -json -tags=e2e -count=1 ./internal/e2e -run '^TestLegacyD1_HttpEndpoints'
go test -json -tags=e2e -count=1 ./internal/e2e -run '^TestTemplates_'
```

**预期**：

- 以上三条直接 `go test -tags=e2e` **不带** `BASALT_E2E_BINARY`，`TestMain` 走 temp 构建回退并打印 `binary_source=temp`；它们只作功能验收，**不得**作为发布证据（模块卡 §6.11 第 7 条）。
- `TestUS14Demos_Fixture`：五 Demo 各自的业务断言 + 六段证据（见 data-model §5）；D1 两次运行（首跑无基线、第二次去重算差）；D3 零通知；D4 内容哈希；D5 双 MCP 调用。
- `TestLegacyD1_HttpEndpoints`：R0 三端点 200 / 500 / 不可达分类 + 一次通知汇总；不计入五 Demo。
- `TestTemplates_*`：八模板可装载、内置工具可解析、所选 MCP 工具 ⊆ 目标 server `tools/list`；A / A\* / B 档位证据与 README 文案一致；A 不通过**不得**降 B。

### 2.4 公开增量（US-13 review ② 通过后）

```sh
npm --prefix website test
npm --prefix website run build
npm --prefix website run check
```

**预期**：模板表与 `templates.mjs` 数据一致；`check-links` 接受 `examples/` 链接、拒绝私有路径；站点构建成功。README 英文四节与站点数据同源。US-13 review ② 未完成时**跳过本节并登记未验证**，不得改其交付物。

## 3. 14C：发布收尾

### 3.1 发布专用目标

```sh
make example-tests
make e2e
```

**预期**：

- `example-tests` = docs-readonly 单测 + `scripts/run-example-tests.py`，全绿。
- `e2e` = `build` + `example-tests` + `BASALT_E2E_BINARY=$(CURDIR)/bin/basalt go test -json -tags=e2e -count=1 ./internal/e2e`；测试输出含 `binary_source=candidate` 与 `binary_sha256`，且哈希与 `bin/basalt` 一致。`make e2e` **必须**导出该变量；标记缺失或为 `temp` = 发布证据失败（直接 `go test -tags=e2e` 允许 temp 回退，但不得作为发布证据，见 §2.3）。
- 七场景逐项绿：工具闭环 / 越权 / 取消 / 工具超时 / 崩溃恢复 / 409 / 热加载 / Web·调度·通知（取消与工具超时是**两个**用例：取消 → Run `cancelled` + 无子进程残留；超时 → 工具审计 `failed` + `timeout` + Run `completed` 且失败结果回填）。

### 3.2 stage 与干净检出

```powershell
.\scripts\check-acceptance.ps1 --stage harness/stages/us14-release.json
```

```sh
sh scripts/check-release-clean.sh
```

**预期**：stage 全 check 通过（含 `mcp-*`、`demo-fixtures`、`template-quality`、`docs-readonly-tests`、`script-tests`、`e2e`、`console-*`、`website-*`、`acceptance`、`clean-checkout`）；`check-release-clean.sh` 要求 `git status --porcelain` 为空，`git archive HEAD` 到临时目录后构建 / 门禁 / e2e 全绿。全量验收另有 `make acceptance`（`make check` 的超集 + 固定评测）。

### 3.3 发布证据

按 [data-model.md](./data-model.md) §7 汇总：版本（`v0.1.0`，tag 由**用户**手动）/ commit / 候选路径 + SHA-256 / 七场景逐项 / Windows + Linux 双平台走查 / 依赖与许可表（Go / Node / Python / MCP 包）/ 升级·恢复演练。**双平台缺一 = 未完成**。

## 4. 边界与失败语义

| 情况 | 处置 |
| --- | --- |
| 真实外部服务（模型 / GitHub PAT / Brave / Fetch / Playwright / DBHub / 通知接收方） | **全部未授权**：只用夹具，证据标"未验证"；不得声称通过 |
| Node / Python 缺失 | 对应 check = 失败；不得改成跳过 |
| `make check` 之外的 `make e2e` / `example-tests` | 不进 `acceptance.json` / CI；不得为让门禁变绿而加入 |
| 测试空匹配 / `SKIP` | 不算通过；`required_tests` 复核 |
| 需要改 D-01–D-10 字面量或判据面 | 停下返回人工 review ①，不得就地放宽 |
| `v0.1.0` tag / push / 部署 | 用户手动执行，AI 不代打 |

## 5. 未验证项（实施前）

- 全部真实外部实跑、双平台走查、依赖与许可表、`v0.1.0` tag 属人工 / 授权项，写入 run-state 未验证清单。
- 本向导中的命令在实现完成前**不可执行通过**；届时按实际签名与文件路径校正 `required_tests` 名称。
