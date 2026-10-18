# Quickstart：US-9 Web Service 验证指南

**Feature**: `009-web-service` | **Date**: 2026-10-18 | **Plan**: [plan.md](./plan.md)

本文件是**可复制执行**的验证步骤，不重复设计细节。实体与字段见 [data-model.md](./data-model.md)，决策依据见 [research.md](./research.md)，完整验收口径见颗粒度文档 [docs/us/us9-web-service.md](../../docs/us/us9-web-service.md) §4 与 §6。

## 0. 前置

```sh
# 在仓库根目录；Go 工具链必须 ≥ toolchain 指令声明的 1.27.2
go version                                   # 期望 go1.27.2 windows/amd64
export GOPROXY=https://goproxy.cn            # 本机直连 proxy.golang.org 不可达
git branch --show-current                    # 期望 009-web-service
```

**基线核对**（三件输入，与 [run-state.md](./run-state.md) 基线一致）：

```sh
sha256sum docs/us/us9-web-service.md specs/009-web-service/spec.md specs/009-web-service/plan.md
# 期望：颗粒度文档 5195dbee…；spec 5261c0e2…
```

## 1. 全量测试（快速反馈，不含阶段判卷）

```sh
GOPROXY=https://goproxy.cn go test -race -count=1 ./...
```

期望：全绿。本 US 新增 144 条 + 修改 5 条 = 149 条测试（含收口复核修复轮新增 4 个函数、独立复核修复轮扩 8 例、review ② 小修轮再扩 4 例），分布在：

| 测试文件 | 条数 | 覆盖面 |
| --- | --- | --- |
| `internal/web/contract_test.go` | 15 | 信封形态、错误码 / 类型映射、视图字段、时间戳与 Content-Type |
| `internal/web/handlers_test.go` | 74 | 10 端点行为、校验、归档 / 准入互斥、脱敏、failed 内部诊断、预算传导、客户端断开 |
| `internal/web/server_test.go` | 23 | 缺省值、超时矩阵、五阶段停机（异常退出与收尾超时）、真实连接写预算、跨源拒绝、recover、请求日志与 panic 标识 |
| `internal/app/web_test.go` | 11 | 装配投影、provider 无凭证、工具来源分类、serve 初始化、`MaxConcurrent`、`runWithRuntime` |
| `internal/core/sessionid_test.go` | 5 | 固定向量、相邻元组不碰撞、路径 / 日志安全、空组件编码、恶意前缀回落 |
| `internal/storage/session_api_test.go` | 11 | 四条 SQL：状态读取、幂等创建、幂等归档、窗口顺序 |
| `internal/cli/serve_test.go` | 5 | serve / gateway 命令、信号阻塞、根命令注册 |

修改：`internal/app/run_test.go`（+3）、`internal/cli/config_test.go`（+1）、`internal/memory/memory_test.go`（+1）。

## 2. 九个专属 check（逐条可复跑）

每条命令都必须含 `-json` 与 `-count=1`（stage 装载期会拒绝缺失者）。

| check ID | 命令 | 期望 |
| --- | --- | --- |
| `009-web-contract` | `GOPROXY=https://goproxy.cn go test -json -count=1 -run 'TestContract_' ./internal/web` | 5 个具名测试全 PASS |
| `009-web-handlers` | `GOPROXY=https://goproxy.cn go test -json -count=1 -run 'TestSessions_\|TestMessages_\|TestInvoke_\|TestMeta_' ./internal/web` | 16 个具名测试全 PASS |
| `009-web-server` | `GOPROXY=https://goproxy.cn go test -json -count=1 -run 'TestServer_' ./internal/web` | 11 个具名测试全 PASS |
| `009-web-race` | `GOPROXY=https://goproxy.cn go test -race -json -count=1 ./internal/web` | 32 个具名测试全 PASS（上三行并集） |
| `009-storage-session-api` | `GOPROXY=https://goproxy.cn go test -json -count=1 -run 'TestSessionStore_' ./internal/storage` | 4 个具名测试全 PASS |
| `009-core-sessionid` | `GOPROXY=https://goproxy.cn go test -json -count=1 -run 'TestDeriveSessionID_' ./internal/core` | 5 个具名测试全 PASS |
| `009-cli-serve` | `GOPROXY=https://goproxy.cn go test -json -count=1 -run 'TestServeCommand_\|TestGatewayCommand_\|TestRootCommand_RegistersServeAndGateway' ./internal/cli` | 4 个具名测试全 PASS |
| `009-app-web` | `GOPROXY=https://goproxy.cn go test -json -count=1 -run 'TestNewWebServer_\|TestWebProviders_\|TestNewToolCatalogPort_\|TestEnsureWorkspaceForServe\|TestNeedsServer\|TestNewRuntime_MaxConcurrentRuns\|TestRun_HeavyCommandSplit\|TestRunWithRuntime_' ./internal/app` | 8 个具名测试全 PASS |
| `009-cli-config` | `GOPROXY=https://goproxy.cn go test -json -count=1 -run '^TestConfig_RejectsNegativeMaxConcurrentRuns$' ./internal/cli` | 1 个具名测试 PASS |

> 具名测试的完整清单见颗粒度文档 §4.4；**空匹配 / SKIP 一律不算通过**（`harness/README.md` 的机器判据）。

## 3. 阶段验收（完整判卷）

```sh
# Git Bash / Linux / macOS
sh scripts/check-acceptance.sh --stage harness/stages/us9-web.json

# Windows PowerShell
.\scripts\check-acceptance.ps1 --stage harness/stages/us9-web.json
```

期望：机器报告中**九个 check ID 全部出现且为 PASS**；报告写入 `run-state.md` 的收口证据。不带 `--stage` 的 `make acceptance` 只覆盖公共层，**不构成**本 US 的完整验收。

**完成的宪法口径**仍是七目标全绿：

```sh
make check                                   # fmt lint sec test test-integration cover build
```

三条判卷纪律（先退出码、再判输出；`-race` 与 `CGO_ENABLED=0` 分离；集成层独立命令）见 [docs/DevelopmentLoop.md](../../docs/DevelopmentLoop.md) §2.2。

## 4. 覆盖率

```sh
GOPROXY=https://goproxy.cn make cover
```

阈值：`core` / `storage` / `memory` ≥ 70%，`internal/web` / `app` / `cli` ≥ 50%。有可执行语句但无测试文件的包计 0%、未达标即失败；无可执行语句的包跳过。本 US **不改** `Makefile` 与阈值。

## 5. 依赖方向与红线 grep（收尾逐条执行）

```sh
# ① web 只依赖 core + 标准库（除 internal/web 自身外，只允许出现 internal/core）
GOPROXY=https://goproxy.cn go list -deps ./internal/web | grep 'basalt/internal' | grep -v 'internal/web$'

# ② 单一写入路径：web 不写消息 / 审计 / SQL
grep -rnE 'AppendMessage|INSERT INTO|NewReActLoop|ToolExecutor' internal/web/ || echo "零命中（期望）"

# ③ context 无业务态：只允许追踪标识（宪法 VII）——命中项逐处人工确认
grep -rn 'context.WithValue' --include='*.go' cmd internal
# 期望：仅 internal/web/middleware.go 的 requestIDKey{}（追踪标识，允许）；
# 业务状态（Profile / Session 等）一律显式传参，出现即不通过

# ④ 零新增依赖
git diff --stat -- go.mod go.sum                         # 期望空输出
grep -nE 'eino|langchaingo|adk-go|trpc-agent-go' go.mod || echo "零命中（期望）"

# ⑤ 零迁移
git diff --stat -- internal/storage/schema.sql docs/contracts/schema.sql   # 期望空输出
ls internal/storage/migrations/ 2>/dev/null | wc -l                        # 期望与基线一致

# ⑥ 凭证零明文（只看响应结构标签）
grep -R 'json:"\(api_key\|base_url\)"' internal/web || echo "零命中（期望）"

# ⑦ 抑制注释零使用
grep -rnE '//nolint|#nosec|//lint:ignore' --include='*.go' internal/web internal/app/web.go internal/cli/serve.go || echo "零命中（期望）"

# ⑧ 时区禁用词
grep -rnE 'time\.Local|[.]Local[(][)]|time\.Now[(][)]\.Location[(][)]' --include='*.go' internal/web || echo "零命中（期望）"

# ⑨ 流程任务里 serve / gateway 已进入重命令集
grep -n 'heavyCommands' internal/app/run.go               # 期望含 serve、gateway
```

## 6. 手工冒烟（临时工作区，用毕即删）

> 需要真实模型配置；没有配置时跳过并记「未验证」，**不得**记为通过。

```sh
# 临时工作区（仓库外，用毕删除）
WORK=$(mktemp -d)
cd "$WORK"
basalt init .
basalt serve &                                # 期望日志出现监听 127.0.0.1:8080
BASE=http://127.0.0.1:8080/api/v1

# Demo 四：建会话 → 发消息 → 取历史 → 归档
curl -s -X POST "$BASE/sessions" -H 'Content-Type: application/json' \
     -d '{"agent":"demo"}'                     # data.session_id；重复执行 id 不变
SID=<上一步的 session_id>
curl -s -X POST "$BASE/sessions/$SID/messages" -H 'Content-Type: application/json' \
     -d '{"message":"你好"}'                    # data.status=completed
curl -s "$BASE/sessions/$SID"                   # messages 含本轮 user / assistant
curl -s -X DELETE "$BASE/sessions/$SID"         # code=OK，无 data
curl -s "$BASE/sessions/$SID"                   # 归档后仍可读，status=archived

# Demo 五：无状态 invoke
curl -s -X POST "$BASE/agents/demo/invoke" -H 'Content-Type: application/json' \
     -d '{"message":"你好"}'                    # data.session_id 为空串

# 负向：归档后追加 → 400
curl -s -X POST "$BASE/sessions/$SID/messages" -H 'Content-Type: application/json' \
     -d '{"message":"再来一次"}'

# 负向：跨源写 → 403（GET 不被拦）
curl -s -X POST "$BASE/sessions" -H 'Origin: https://evil.example' \
     -H 'Content-Type: application/json' -d '{"agent":"demo"}'
curl -s "$BASE/health" -H 'Origin: https://evil.example'   # 200，无 ACAO 头

# 停止并核对监听
kill -TERM %1                                   # Linux / WSL：正常退出；Windows 记未验证
```

完整人工验收清单（Demo 四 / 五、真实模型两轮工具、崩溃重启、gateway 启停、`chat` 与 `serve` 互斥、预算传导等）见颗粒度文档 §6.2，共 11 项，由用户执行。

## 7. 负向矩阵（必须全部见到预期状态码）

| 输入 | 期望 |
| --- | --- |
| 未知路径 / 错误方法 | 404 信封（不是 405 纯文本） |
| `POST /sessions` 缺 `agent` / `agent: null` | 400「agent 必填」，零写入 |
| `POST /sessions` 未知或未注册 agent | 404 |
| 同元组重复建会话 | 200，同 `session_id`，`created_at` 不变 |
| `message` 缺失 / null | 400「message 必填」 |
| `message` = 空串 | 允许（进入运行） |
| `message` 32769 字节 | 400，零 Run 行 |
| 请求体 > `http.max_request_bytes` | 400（不是 413） |
| 请求体含尾随内容 | 400 |
| 同会话第二个并发运行 | 409 + 冻结文案 |
| 运行中归档 | 409，`status` / `archived_at` / 消息不变 |
| 归档后追加消息 | 400「会话已归档，不可追加消息」 |
| 归档不存在的会话 | 404 |
| 重复归档 | 200，首次 `archived_at` 不变 |
| 实例并发超限（`server.max_concurrent_runs: 1`） | 429，message 点名上限 |
| 运行超过 60s | 504 `TIMEOUT`，运行记 `cancelled` |
| 达迭代上限 | 200 `LIMIT_REACHED`，message 点名 `budget.max_iterations` |
| 跨源 POST / DELETE | 403 `BAD_REQUEST`「跨源请求被拒绝」，零副作用 |
| 跨源 GET | 放行（无 ACAO 头） |
| 内部错误 | 500，message 固定「服务器内部错误」 |
| `server.max_concurrent_runs: -1` | 配置装载期拒绝 |

## 8. 清理

- 临时工作区（`$WORK`）、探针二进制、`go test -overlay` 的仓库外副本：用毕即删，收口报告写明已清理。
- `specs/009-web-service/run-state.md` **不删**——它是过程状态，随 `specs/` 入库。
- 运行中产生的 `basalt serve` 进程必须确认已退出（`ss -tlnp` / `netstat -ano` 无 8080 监听）。
