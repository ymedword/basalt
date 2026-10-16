# Quickstart 验证指南: US-5 Shell 与 HTTP 工具

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Data Model**: [data-model.md](./data-model.md) | **Date**: 2026-10-17

本文件是**可复制执行的验证步骤**（不搬运契约字面量——实体与字段见 [data-model.md](./data-model.md)，固定口径见 [docs/contracts/](../../docs/contracts/) 与颗粒度文档 [docs/us/us5-shell-http-tools.md](../../docs/us/us5-shell-http-tools.md) §6.2）。

## 前置条件

- Go ≥ `toolchain` 声明的 `go1.27.2`（`go version` 应输出 `go1.27.2 windows/amd64`）；
- 每条 `go` / `make` 命令显式带 `GOPROXY=https://goproxy.cn`（本机直连 proxy.golang.org 不可达）；
- 默认层测试**不访问真实外网、不打真实模型、不起真实子进程**（本地回环 `httptest` 允许）——场景 A/B/C 全部离线可跑；场景 E/F 中标注"人工"的步骤才需要真网络 / 真模型。

## 场景 A：本 US 测试面（优先跑，失败最先暴露）

```bash
# A1 新增测试文件（3 个，51 条）
GOPROXY=https://goproxy.cn go test -race ./internal/tool/ ./internal/cli/ -run 'Shell|Http|ToolList' -v

# A2 改动文件追加与判据扩面（7 文件 + ports_test）
GOPROXY=https://goproxy.cn go test -race ./internal/core/ ./internal/tool/ ./internal/cli/ ./internal/app/ -count=1

# A3 与颗粒度文档 §3.2 逐文件对号（条数核对）
for f in $(git ls-files 'internal/**/*_test.go'); do printf '%s %s\n' "$(grep -c '^func Test' "$f")" "$f"; done
```

**预期**：A1 全绿；A2 全绿；A3 中本 US 的 3 个新增文件计数 = 17 / 28 / 6，7 个改动文件与其追加条数一致（收口时与 §3.2 逐项核对）。

## 场景 B：关键回归 R1–R8 点跑（守点最尖，先于全量门禁）

```bash
GOPROXY=https://goproxy.cn go test -race ./internal/tool/ -v -run \
  'TestHttpGet_PerHopCheckPrecedesEverySend|TestHttpGet_RedirectHopLimitIsFive|TestHTTPChain_ExecutorCapsFinalContent|TestHTTPGet_CancelDuringBodyReadReturnsPromptly|TestHTTPWithRealClient_DrivesHopsAndIgnoresEnvProxy|TestHttpGet_RedirectToNonDefaultPortIsRefusedBeforeSend|TestShellTool_TimeoutSecondsOnlyNarrows|TestShellTool_NonZeroExitIsNotAnError'
```

> R1~R8 **全部**落在 `internal/tool` 包内（`httptools_test.go` 六条 + `shelltools_test.go` 两条）——R3 经包内接缝 `newHTTPTool` / `NewRegistry` 组装，不跨包跑。

**预期**：8 条全 PASS。逐条守点：

| 回归 | 守什么 | 判可见的读数 |
| --- | --- | --- |
| R1 `TestHttpGet_PerHopCheckPrecedesEverySend` | 每一跳**发送前**都过判定（两侧记账：发了几次 / 判了几次判的哪个 target） | 越界那跳**不被发送** |
| R2 `TestHttpGet_RedirectHopLimitIsFive` | 跳数上限 5、**初始不计**（假 doer 恰收 6 次请求） | 超限 → `sandbox_denied`（非 `external_error`） |
| R3 `TestHTTPChain_ExecutorCapsFinalContent` | 工具 → **真实 `ToolExecutor`** → 落库链路：预算口径 = 最终内容、截断标记**恰一次** | 审计行 `result_json` 与 `truncated` 值内一致 |
| R4 | 取消传播：**等「请求已发出」信号再取消**、限时返回 | 归 `timeout`、无后台读取残留 |
| R5 `TestShellTool_TimeoutSecondsOnlyNarrows` | `timeout_seconds` 四态：缺省 / 更短 / 更长不生效 / 非正值按缺省 | 生效值 ≤ `env.Timeout` |
| R6 `TestShellTool_NonZeroExitIsNotAnError` | 非零退出码不是错误（首行「退出码：N」） | `err == nil`、Content 首行核对 |
| R7 `TestHTTPWithRealClient_DrivesHopsAndIgnoresEnvProxy` | 真链路：跳转由**工具**驱动（客户端不跟随）；`Transport.Proxy` 为 nil、**不读** `HTTP_PROXY` | 摆上 `HTTP_PROXY` 环境变量后仍直连 |
| R8 `TestHttpGet_RedirectToNonDefaultPortIsRefusedBeforeSend` | 白名单域名重定向到非默认端口 → **目标请求零发送** | 拒绝先于发送、归 `sandbox_denied` |

## 场景 C：`basalt tool list` 冒烟（可在已初始化工作区离线跑）

```bash
GOPROXY=https://goproxy.cn go build -o /tmp/basalt.exe ./cmd/basalt && /tmp/basalt.exe tool list
```

**预期**：六行「名称：描述」（含 `shell` / `http_get` / `http_post`），末尾空行 +「共 6 件工具」；顺序稳定（实现侧给出，命令面不重排）。未初始化工作区时按既有重路径报错提示先 `basalt init`。**离线、无需模型。**

## 场景 D：出网与命令链路的隔离验证（无外网可跑）

- **端口档**：单测已覆盖（R8 + `tool/policy_test.go` 4 条新增）——`http://example.com:8443/x` 与 `https://example.com:8443/x` 均拒、指引为「当前版本不支持非默认端口」、**不**提示加域名白名单；交叉档（`http:443` / `https:80`）同拒。
- **出站 body 超限**：`http_post` body > 256 KiB → `invalid_input`、**零发送**（假 doer 计数为 0）。
- **`shell` 真命令**：默认层用**假 runner**（零进程）判语义；真进程读数属**人工项**（场景 F）。

## 场景 E：全量门禁（"完成"的定义）

```bash
make check        # = fmt lint sec test test-integration cover build 七目标聚合
```

**预期**：退出码 0（本 US 对 `Makefile` / CI **零改动**——`tool` / `cli` / `core` / `app` 自 US-4 起已在判卷内）。格式判卷纪律：先查退出码、再判输出；`-race` 与 `CGO_ENABLED=0` 分属不同目标。覆盖率：`tool` ≥ 70%、`cli` 与其余 ≥ 50%（`make cover` 按包判失败）。

## 场景 F：人工项（harness 不判卷，等用户执行）

以下步骤需要**真外网 / 真模型 / 真控制台**，按颗粒度文档 §6.1 十二项逐条执行，本文件只给入口：

1. **Demo 一（出网取数）**：工作区配置 `allowed_domains` 加入真实天气 API 域（如 `wttr.in`，Demo 一指定）→ `basalt chat` 对话问「今天天气怎么样」→ 观察 `http_get` 调用、结果回填模型、给出穿衣建议；审计 `tool_invocations` 出现 `success` 行；
2. **沙箱拒绝样例**：对话中诱导模型试 `bash -c "…"`、工作区外路径、非默认端口 URL、非白名单域名 → 四段式拒绝文案 + 审计 `failed` / `sandbox_denied`，运行照常继续；
3. **`shell` 真命令与进程树强杀**：Agent 目录下跑白名单命令（非零退出码不算失败、stderr 合流）；长命令 Ctrl-C 后核对**整棵进程树**被杀（Windows Job Object / unix 进程组）；
4. **三平台编译**、**gosec 复核**、**凭证与敏感面复核**、**变动范围核对**（`git diff --stat` 与 §3 清单逐项）、**Windows 真控制台渲染**（`tool list` 中文与空行）、**端口与脱敏抽验**——逐条见颗粒度文档 §6.1。

> 敏感面提醒（人工核对时用）：日志与模型可见文案**不得**含 userinfo / query / headers / body；但审计 `input_json` 按契约**原样保存**模型给的 arguments——**不得**笼统承诺"凭证不落库"（边界见 [data-model.md](./data-model.md) §4）。
