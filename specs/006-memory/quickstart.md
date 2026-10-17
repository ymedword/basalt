# Quickstart 验证指南: US-6 Memory 长期记忆

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Data Model**: [data-model.md](./data-model.md) | **Date**: 2026-10-17

本文件是**可复制执行的验证步骤**（不搬运契约字面量——实体与字段见 [data-model.md](./data-model.md)，固定口径见 [docs/contracts/](../../docs/contracts/) 与颗粒度文档 [docs/us/us6-memory.md](../../docs/us/us6-memory.md) §6.2）。

## 前置条件

- Go ≥ `toolchain` 声明的 `go1.27.2`（`go version` 应输出 `go1.27.2 windows/amd64`）；
- 每条 `go` / `make` 命令显式带 `GOPROXY=https://goproxy.cn`（本机直连 proxy.golang.org 不可达）；
- 默认层测试**不访问真实网络、不打真实模型、不起真实子进程**；真实文件系统（`t.TempDir()`）与真实本地 SQLite 库文件**允许**——场景 A/B/C/D 全部离线可跑；只有场景 E 中标注「人工」的步骤才需要真模型。

## 场景 A：本 US 测试面（优先跑，失败最先暴露）

```bash
# A1 新增测试文件（memory 包 4 个 + storage 1 + tool 1 + workspace 1 = 7 文件 43 条）
GOPROXY=https://goproxy.cn go test -race ./internal/memory/ ./internal/storage/ -count=1
GOPROXY=https://goproxy.cn go test -race ./internal/tool/ ./internal/workspace/ -count=1

# A2 改动文件追加（core 2 + workspace 1 + cli 1 + app 2 = 6 文件追加 15 条 + 夹具扩展）
GOPROXY=https://goproxy.cn go test -race ./internal/core/ ./internal/cli/ ./internal/app/ -count=1

# A3 与颗粒度文档 §3.2 逐文件对号（条数核对：新增 43 + 追加 15 = 41 + 17，差异只在文件归属分类）
for f in internal/memory/memory_test.go internal/memory/markdown_test.go internal/memory/carrier_contract_test.go \
         internal/memory/import_test.go internal/storage/memory_store_test.go internal/tool/memorytools_test.go \
         internal/workspace/templates_test.go; do
  printf '%s %s\n' "$(grep -c '^func Test' "$f")" "$f"; done
```

**预期**：A1 / A2 全绿；A3 计数依次为 **10** / 8 / 7 / **1** / 6 / 9 / 2（`memory_test.go` 由 9 → 10：R3 裁决 P2-b 的 `TestService_AppendLocksAreKeyedByScope`；`import_test.go` 的 1 条 = analyze C1 处置的**包级架构测试** `TestDependencyDirection_MemoryImportsOnlyCoreWorkspaceStdlib`）。

## 场景 B：关键回归 R1–R9 点跑（守点最尖，先于全量门禁）

```bash
# R1 段 2 注入与空记忆逐字节回归
GOPROXY=https://goproxy.cn go test -race ./internal/core/ -run 'InjectsCoreMemorySegment|EmptyMemoryIsByteIdenticalToSectionOne|Build_InjectsMemorySegment' -v

# R2 运行内冻结（整次运行只读一次；第 2 轮不得看到改后文本）
GOPROXY=https://goproxy.cn go test -race ./internal/core/ -run 'MemoryReadOncePerRun' -v

# R3 两层回退且不叠加（两档同一断言，含「Agent 层只有归档条目」档）
GOPROXY=https://goproxy.cn go test -race ./internal/memory/ -run 'TwoLayerFallbackWithoutStacking|ScopeIsolationAgentVsGlobal' -v

# R4a 手工编辑超配额：注入侧不截断 + WARN（逐字节等于完整期望文本 + 尾部哨兵；日志不打正文）
GOPROXY=https://goproxy.cn go test -race ./internal/memory/ -run 'OversizedManualMemory_WarnsAndInjectsFully' -v

# R4b 超上下文预算 → 拒绝启动、Reason 点名 budget.context_tokens、零 LLM（含同预算无记忆对照组）
GOPROXY=https://goproxy.cn go test -race ./internal/core/ -run 'OversizedMemoryFailsRunBeforeModelCall|MemoryCountsTowardTokenBudget' -v

# R5 并发写同一作用域一条不丢（两档，多重集比较，-race）+ 锁按完整 Scope 分档（同 Scope 串行、不同 Agent 不互相阻塞）
GOPROXY=https://goproxy.cn go test -race ./internal/memory/ -run 'ConcurrentAppendSameScopeSerialized|ConcurrentAppendKeepsEveryEntry|AppendLocksAreKeyedByScope' -v

# R6 sqlite 档配额检查与追加同事务（并发 12 笔恰 8 笔通过）
GOPROXY=https://goproxy.cn go test -race ./internal/storage/ -run 'ConcurrentAppendNeverExceedsCoreQuota' -v

# R7 载体隔离负向（文件工具拒写 memory/ + 规则名与指引逐字 + 原文件逐字节不变 + 记忆工具正向对照）
GOPROXY=https://goproxy.cn go test -race ./internal/tool/ -run 'MemoryDirIsolation' -v

# R8 空 query / 空 content 在工具层即拒——四态：`""` 与纯空白（`"   \n\t "` / `" "`）同档（规范化后为空；零写入 / 零检索）
GOPROXY=https://goproxy.cn go test -race ./internal/tool/ -run 'RejectEmptyInputs' -v

# R9 读取面三档不得塌成一档
GOPROXY=https://goproxy.cn go test -race ./internal/memory/ -run 'ErrorTiersDoNotCollapse|RefusesCorruptedFileKeepsOriginalBytes' -v
```

**预期**：每组全绿；R5 / R6 在 `-race` 下零告警（无数据竞争、无 goroutine 泄漏读数）。

## 场景 C：门禁全量（完成定义）

```bash
GOPROXY=https://goproxy.cn make check          # = fmt lint sec test test-integration cover build
```

**三条实测纪律**（判卷顺序写错会让门禁"报绿而实际没检"）：

```bash
# C1 格式先查退出码、再判输出（两个工具各查一遍；差异时退出码 0 而输出非空）
gofumpt -l . ; echo "gofumpt exit=$?"
gofmt -l .   ; echo "gofmt exit=$?"

# C2 -race（需 CGO）与发布构建（CGO_ENABLED=0）分属不同命令
GOPROXY=https://goproxy.cn go test -race ./...
CGO_ENABLED=0 GOPROXY=https://goproxy.cn go build ./...

# C3 集成层必须单列（build tag 隔离的测试默认不执行；本 US 无新增，跑既有冒烟）
GOPROXY=https://goproxy.cn go test -tags=integration ./...
```

**预期**：`make check` 七目标逐项四态登记（本 US 无跳过 / 不适用项——集成层跑既有即绿，覆盖率已含 `memory` 包）。

## 场景 D：覆盖率（按包判失败）

```bash
GOPROXY=https://goproxy.cn make cover
```

**预期**：`core` / `tool` / `memory` / `storage` ≥ 70%，其余 ≥ 50%；`memory` 包本 US 从骨架变主体，**必须** ≥ 70%（判定口径：有可执行语句但无测试文件的包计 0%、未达标即失败；无可执行语句的包跳过）。

## 场景 E：人工项（harness 之外，需真模型 / 手工操作）

| # | 步骤 | 预期 |
| --- | --- | --- |
| E1 | Demo 二（**markdown 档**）：`basalt init` → `basalt agent create` → CLI 会话里说「以后回答简洁点」→ 进程重启 → 新会话 | 新会话仍按该偏好输出；`memory/MEMORY.md` 核心区可见该条目 |
| E2 | Demo 二（**sqlite 档**）：改 `memory.backend: sqlite` → 重启 → 同上 | 断言语义与 E1 相同；`memory/MEMORY.md` **保持模板原文**（sqlite 档不碰文件） |
| E3 | 手工编辑生效时机：会话运行中手改 `MEMORY.md` | **下一次运行**才生效（运行内冻结）；进程不重启 |
| E4 | 手工编辑超配额：堆核心区过 32 KB → 启动运行 | 注入期记 **WARN** 并**完整注入**（不截断）；若因此超上下文预算 → 拒绝启动、Reason 点名 `budget.context_tokens`、**零 LLM 调用** |
| E5 | 两层作用域现场：同一份 Agent 资产建两个 Agent，各自 `save_memory` | 互相看不到对方条目（不叠加）；全局层偏好对两者都生效（回退） |
| E6 | `basalt tool list` | 读数**八件**（原六件 + `save_memory` / `recall_memory`），描述与 schema 非空 |
| E7 | 三平台编译矩阵 | `CGO_ENABLED=0 GOOS=linux/darwin/windows go build ./...` 全通过（人工项，不进 `make check`） |
| E8 | 损坏文件现场：删掉 `## 归档记忆` 标题行 → 运行 | 拒绝启动、错误点名文件与症状、**原文件逐字节不变**；手工恢复标题后照常可用 |

## 场景 F：独立核对命令（机器可判的不变量与零改动判据）

```bash
# F1 依赖方向：core 只依赖标准库（预期输出只有标准库包名）
GOPROXY=https://goproxy.cn go list -f '{{.Imports}}' ./internal/core

# F2 零新增坐标 / 零迁移 / 零新增配置键（预期三条都为空输出）
git diff --stat go.mod go.sum
git diff --stat docs/contracts/schema.sql
git diff --stat basalt.yaml.example

# F3 无框架坐标 / 无 ORM（预期零命中）
grep -nE "eino|langchaingo|adk-go|trpc-agent-go|gorm|entgo|ent\.io|xorm" go.mod

# F4 业务状态不进 context（预期零命中或仅追踪标识的既有命中）
grep -rn "context.WithValue" --include='*.go' cmd internal

# F5 抑制注释零使用（预期零命中）
grep -rnE "//nolint|#nosec|//lint:ignore" --include='*.go' cmd internal

# F6 记忆符号的交付面（既有零命中 → 本 US 落地后有命中，且只在预期文件）
grep -rn "LongTermMemoryStore\|MemoryReader\|MemoryFilePath\|MemoryConfig\|LoadEffective" --include='*.go' cmd internal

# F7 两条「文档 ↔ 仓库事实差异」的承载面核对（见 plan.md 同名小节）
grep -n "Profile.Tools\|Tools:" internal/workspace/agent_files_test.go | head
sed -n '304,324p' internal/app/ports_test.go

# F8 记忆包生产导入面（持久判据 = internal/memory/import_test.go，analyze C1 处置；本条是同一判据的一次性读数）
GOPROXY=https://goproxy.cn go list -f '{{join .Imports " "}}' ./internal/memory
```

**预期**：F1 只出标准库；F2 三条空；F3 / F4 / F5 零命中；F6 命中集与 §3.1 交付物清单逐项对上；F7 印证 `agent_files_test.go` 两处装载器断言与 `ports_test.go` 的「恰好六件」名集是本 US 判据面变更的真实承载点；F8 只出三类——标准库 + `github.com/ymedword/basalt/internal/core` + `.../internal/workspace`（`internal/storage` **不得**出现：`{{.Imports}}` 是**非测试**导入集，`carrier_contract_test.go` 的 test-only 引用不进它）。

## 收口提示

- 剩余人工项（E1–E8 中标「人工」者）**不在 harness 判卷内**——harness 全绿后由用户逐条执行，读数登记进验收报告与 `run-state.md` 的未验证清单；
- 未验证项**不得**写成「已验证」：本文件所有自动化场景的读数须以本轮实跑输出为准（不得引用历史输出）。
