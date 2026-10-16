# Phase 1 验证指南：US-4 ReAct 循环与首个工具闭环

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-16

本文件是**可复制执行的验证指南**：命令、期望结果、失败时的排查入口。设计面事实见 [data-model.md](./data-model.md) 与 [research.md](./research.md)；固定字面量以 [docs/contracts/](../../docs/contracts/) 与颗粒度文档 [docs/us/us4-react-loop.md](../../docs/us/us4-react-loop.md) §6.2 为准——本文件**不复制**这些字面量。

## 前置

| 项 | 判据 |
| --- | --- |
| 工具链 | `go version` → `go1.27.2 windows/amd64`（与 `go.mod` 的 `toolchain go1.27.2` 一致） |
| 模块代理 | 每个 `go` 命令显式带 `GOPROXY=https://goproxy.cn`（本机直连不可达） |
| 工作区 | 已有 `.basalt/`（`basalt init` 产物）：`agents/` / `skills/` / `basalt.lock` / `basalt.db` |
| 配置文件 | `.basalt/basalt.yaml` 的 provider 段已就位；凭证一律 `${ENV_VAR}` 占位（**不得**明文落盘） |

## 一、自动化判卷（无需凭证、可离线重复）

```bash
# ① 全量门禁（七目标聚合；实现完成的定义）
make check

# ② 只跑本模块新增的 21 个测试文件所属包（快速迭代）
GOPROXY=https://goproxy.cn go test -race -count=1 ./internal/core/... ./internal/tool/... \
  ./internal/channel/... ./internal/storage/... ./internal/workspace/... \
  ./internal/provider/... ./internal/cli/... ./internal/app/...

# ③ 用例条数核对（期望 185 条新增：21 个文件的逐文件条数见颗粒度文档 §3.2）
grep -rc '^func Test' internal/core/loop_test.go internal/core/tool_exec_test.go \
  internal/core/prompt_test.go internal/core/service_test.go internal/core/session_test.go

# ④ 覆盖率判卷（core / tool / storage ≥70%、其余 ≥50%——本 US 起三个包首次判卷）
make cover
```

**期望**：① 退出码 0，且 `cover` 输出含 `internal/core` / `internal/tool` / `internal/storage` **首次** ≥70% 的读数行；② 全绿（**不访问真实网络与真实模型**——fake `ProviderClient` 注入，无 `net.Listen`）；③ 条数与 §3.2 逐文件一致；④ 无 `[no statements]` 之外的跳过。

**格式类判卷纪律**：`gofmt -l` / `gofumpt -l` 发现差异**不会自动失败**——**先查退出码、再判输出**；`-race` 与 `CGO_ENABLED=0` **分属不同命令**（`make check` 已分离）。

## 二、评测层（默认 fake，可重复）

```bash
GOPROXY=https://goproxy.cn go run ./evals --cases evals/cases
```

**期望**：逐例 `PASS`；结果文件落在 `evals/results/<时间戳>.json`，每例含**用例 id / RunID / 状态 / 工具序列 / 资源用量（token 用量 + 调用次数）/ 计量完整性标记**。**不换算金额**——usage 缺失或行仍 `pending` / `unknown` 时标「计量不完整」，**不得**把库中默认零读成零成本。

真模型档**显式门控**（人工执行、凭证走环境变量）：

```bash
GOPROXY=https://goproxy.cn go run ./evals --cases evals/cases --provider <provider-name>
```

## 三、端到端人工验证（真模型 + 真控制台）

> 与颗粒度文档 §6.1「剩余人工项」逐条对应；每项须附**本轮当场**命令与输出。

### 场景 A：多轮对话 + 首个工具闭环

```bash
./basalt chat              # 真控制台交互（Windows 需真控制台，非重定向）
```

1. 发问「读一下工作区里的 notes.txt 并总结」→ 期望：界面出现一行 `⚙ 调用工具 read_file…`，随后答复**用上了文件内容**；
2. AGENT.md 身份生效（自称与正文一致）；
3. `Ctrl-C` → 该次运行标记为**已取消** + 打印取消提示，**进程退出码 0**（取消是预期动作）。

单条模式与管道两侧 UTF-8：

```bash
./basalt chat --message "读一下工作区里的 notes.txt，用一句话概括"
echo "你好" | ./basalt chat --message "用中文回答：收到"
```

### 场景 B：越权拒绝（四段式 + 审计读数）

诱导模型读工作区外路径（如 `../../etc/passwd` 或绝对路径）。**期望**：四段式拒绝文案可见（违规说明 / 具体目标 / 规则出处 / 放行指引）；**运行本身照常继续**（工具失败**不**判运行失败）。查库读数：

```bash
sqlite3 .basalt/basalt.db \
  "SELECT tool_name, status, error_kind FROM tool_invocations ORDER BY id DESC LIMIT 5;"
```

**期望**：`status` = `failed`、`error_kind` = `sandbox_denied`；**且 `tool_invocations` 无 `round` 列**（列清单断言由 `storage/run_store_test.go` K4 / `tool_invocations_test.go` J8 判卷）。

### 场景 C：重启续会与崩溃恢复

1. 正常多轮后退出 → 再次 `./basalt chat`（同一会话标识）→ 历史仍在、可继续；
2. **强杀**进程（工具执行中途）→ 重启 → 期望：被中断的调用在历史里显示「结果未知（进程中断，无法确定是否已产生外部副作用）」或「未执行（进程中断）」（取决于有无审计行）；半截组已补齐；陈旧运行被收尾。

```bash
./basalt session list      # 只读：不取锁、不恢复、不清锁文件
```

**期望**：一行一会话 `<id>：<消息数> 条（最后活动 <时间>）` + 末尾 `共 N 个会话`；空库 → `未发现会话`。

### 场景 D：进程树强杀（Windows Job Object）

起一个会派生**子进程**的命令并等超时，前后对比进程表：

```bash
tasklist | grep -i <进程名>   # 超时前后各一次
```

**期望**：超时后**父子进程都消失**（整树终止而非单杀）；关联失败路径的清理顺序由单测判卷（`tool/runner_test.go` G9）。

## 四、平台与装配读数

```bash
# 三平台编译（人工项，不进 make check）
CGO_ENABLED=0 GOOS=linux   go build ./...
CGO_ENABLED=0 GOOS=darwin  go build ./...
CGO_ENABLED=0 GOOS=windows go build ./...

# 装配确认：命令已注册、重路径顺序在日志里可见
./basalt chat --help
./basalt session list --help
```

**期望**：三条编译退出码全 0（Linux `TCGETS` / Darwin `TIOCGETA` / Windows 控制台与 Job Object 分支各被编译到一次）；日志可见顺序 = 取实例锁 → 写模式打开 → 显式迁移 → 崩溃恢复。

## 五、人工项与未验证项

- **剩余人工项清单**（真模型 Demo 七项、`reasoning_content` 清空后复现供应商 400 的负例、Windows 控制台编码读数、token 估算参考读数、变动范围核对、凭证复核等）见颗粒度文档 §6.1 十一项——**harness 已判卷的部分不等**替代人工项。
- **平台读数（2026-10-16）**：三平台编译（上面三条 `CGO_ENABLED=0 GOOS=… go build ./...`）**已通过**（本轮复跑三条退出码全 0）；**运行验证仅 Windows**——Linux / Darwin 分支的运行读数**不属本机**（CI 绿**不得**据此宣称 Windows 分支已验，反之亦然；本机与 CI 都跳过的项**仍未验证**）。
- **门禁读数（2026-10-16）**：`make check` **已执行**——首次整跑 **exit 2**，失败于 `sec`（gosec 5 条命中 + 导出数据解码错误；根因 = `go install` 版 gosec 自带 `x/tools v0.49.0` 读不了 Go 1.27 导出数据，扫描静默降级）；按[起步手册 §2.5](../../docs/GettingStarted.md)配方重建 gosec 并完成五条真实修复后**复跑 exit 0**（七目标全绿，`govulncheck` 报 `No vulnerabilities found.`）。两次读数并列保留——"已执行"不得改写为"已通过"。
- **本阶段未验证**：markdownlint 本机不可用（借 VSCode markdown 诊断自查）；真模型 / 真控制台类人工项见上行清单。
