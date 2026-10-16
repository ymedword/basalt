# Quickstart：US-3 Agent 目录与 Profile 加载——验证步骤

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-16

本文件是**验证 / 运行指南**：前置条件 → 可复制执行的验证场景（含预期）→ 参考。**不包含**实现代码与方法体（那些属 [tasks.md](./tasks.md) 与实现阶段）；实体与校验规则的细节见 [data-model.md](./data-model.md)，固定字面量一律以颗粒度文档 [docs/us/us3-agent-profile.md](../../docs/us/us3-agent-profile.md) §6 与 [docs/contracts/agent-md.md](../../docs/contracts/agent-md.md) 为准。

## 前置条件

| 项 | 要求 / 读数 |
| --- | --- |
| 工具链 | `go version` → `go1.27.2 windows/amd64`（≥ `toolchain go1.27.2`） |
| 模块代理 | `GOPROXY=https://goproxy.cn`（`robfig/cron/v3@v3.0.1` 与 `x/sys@v0.48.0` 已在本地缓存时无需网络） |
| 平台 | Windows 11 + Git Bash（锁分支运行读数在本机登记）；Ubuntu CI 只覆盖 `lock_unix.go` |
| 工作目录口径 | 命令以**当前目录**为工作区（`app.Run` 取 `os.Getwd()`）；下列命令都在验证目录内执行 |
| 清理 | 验证目录建在仓库外（如 `/tmp/basalt-qs`），用后即删 |

## 场景 1：门禁全绿（实现的定义）

```bash
# 在仓库根执行
make check
```

- **预期**：七目标 `fmt lint sec test test-integration cover build` 全绿，退出码 0。
- **判卷纪律**（宪法·开发工作流三条实测）：格式判卷**先查退出码、再判输出**；`-race` 与 `CGO_ENABLED=0` 分属不同目标（已由 Makefile 天然分离）；本 US **无集成层新增**，`test-integration` 跑既有集成用例。
- **覆盖面判卷**：`cover` 输出 `internal/workspace` ≥ 50%（自本 US 起纳入）；`internal/core` 记 `[no statements]` 跳过（纯声明）。

## 场景 2：装载与检视（注册成功路径）

```bash
# ① 构建二进制到验证目录（仓库根执行一次）
mkdir -p /tmp/basalt-qs
go build -o /tmp/basalt-qs/basalt ./cmd/basalt
cd /tmp/basalt-qs

# ② 初始化工作区（幂等）
./basalt init

# ③ 准备配置文件与一份最小 AGENT.md
#    basalt.yaml 需含非空 providers 段（字段表见起步手册；示例假定其首个 provider 名为 demo）
mkdir -p .basalt/agents/compat-check
cat > .basalt/agents/compat-check/AGENT.md <<'EOF'
---
name: compat-check
description: quickstart 冒烟 Agent
provider:
  name: demo
  model: placeholder-model
---
你是 quickstart 冒烟用的 Agent。
EOF

# ④ 检视
./basalt profile list
./basalt profile show compat-check
```

- **预期**（渲染形态见颗粒度文档 §6 固定口径 3）：
  - `list`：一行 `compat-check：registered=true（provider demo / 模型 placeholder-model）`，末尾汇总行 `汇总：1 个 Agent 目录（已注册 1）`；
  - `show`：同一声明面（provider / 模型）；
  - 退出码 0。
- **轻路径边界**：`list` / `show` 只读文件系统——不开数据库、不取实例锁、不做恢复。
- Compatibility §5.1 共享验收样例（六条判据）的逐字比对属**人工项**（见场景 7）。

## 场景 3：坏例六形态（整只不注册 + 点名原因）

在场景 2 的工作区继续，逐个放入坏例目录（每次一枚，便于对号；同一目录名改内容亦可）：

| 坏例 | 构造要点 | 预期（退出码 0；该行 `registered=false`） |
| --- | --- | --- |
| 未注册 provider | `provider.name` 不在 `basalt.yaml` 的 providers 段 | 原因 = 契约 §2.1 字面量原样（点名 provider 名） |
| 库里没有的技能 | `skills: [ghost-skill]`（`.basalt/skills/` 下无实体） | 原因点名技能名 |
| 5 段 cron | `schedules: [{cron: "*/5 * * * *", message: hi}]` | **必须含固定句**：`invalid schedule: Cron expression must consist of 6 fields` |
| 日周双限定 | `cron: "0 0 0 1 * 1"`（日与周都非裸 `*`） | 原因 = `invalid schedule` + detail（字面量见 §6 固定口径 5） |
| `zone: Local` | `schedules[].zone: Local` | 原因 = `invalid schedule` + `Local` 禁用文案 |
| 空白 model | `provider.model` 缺失或空白 | 原因点名 model 必填 |

- **判据**：坏例**不中断整批**——其余 Agent 照常注册（场景 2 的 `compat-check` 仍在且仍 `registered=true`）；每只失败恰一条点名 WARN（固定键 `agent` / `reason` / `detail`），可在日志中核对（`detail` 不含 YAML 源码片段、未知键 WARN 不含键值）。

## 场景 4：写模式命令（create / delete 三态）

```bash
cd /tmp/basalt-qs

# ① create：生成可装载模板
./basalt profile create demo-agent
./basalt profile list          # demo-agent 应 registered=true
./basalt profile show demo-agent

# ② delete：归档（不触碰会话 / 记忆 / 审计数据）
./basalt profile delete demo-agent
ls .basalt/archive             # 出现 demo-agent-<YYYYMMDD-HHMMSS>/
ls .basalt/agents              # 源目录消失
```

- **预期**：
  - `create` 输出**明示选中** `basalt.yaml` providers 段的**首项**；无 providers 配置时写占位文本并说明「模板尚未注册」；模板 `tools: []`、其余列表字段为空或省略；
  - `delete` 成功后源目录整体进入 `archive/<name>-<时间戳>/`（单次 rename，同文件系统原子）；
  - 已存在目录再 `create` → 拒绝且**不覆盖**（零写入）；同秒内二次 `delete` 同名字 → 拒绝且源目录保留；
  - 非法 Agent 名（空 / 含路径分隔符 / `.` / `..` / 含 NUL）→ create 与 delete 同判据拒绝。
- **锁拒绝态**（第三态）：运行中的实例持锁时 `create` / `delete` 拒绝启动并点名占用者 pid 与开始时间（指引改用 REST / 管理台），`list` / `show` 照常可用。本 US 尚无长驻服务（serve 归 US-9），该态以单测判卷：

```bash
go test -race -v ./internal/workspace/ -run 'TestCreateAgent_RefusesWhenInstanceLockHeld|TestInstanceLock'
```

## 场景 5：关键回归单测（可复制）

```bash
# 在仓库根执行
go test -race -v ./internal/workspace/ -run 'TestAgentLoader|TestCronSpec|TestContextLoader|TestCreateAgent|TestDeleteAgent|TestInstanceLock'
go test -race -v ./internal/cli/ -run 'TestProfile'
go test -race -v ./internal/app/ -run 'TestAgentsPort'
```

- **预期**：全 `PASS`；**恰一处 `SKIP`**（R2 追加的 `TestDeleteAgent_RefusesSymlinkEntry`——Windows 建符号链接需开发者模式 / 特权，本机建不出来时跳过并在用例里写明理由；Ubuntu CI 上实际执行）；跨进程锁两例（`TestInstanceLock_HelperProcess*`）以 helper-process 形态运行（exec 自身测试二进制 + 子进程 stdout 行同步，无睡眠等待）。
- 关键回归守点（与颗粒度文档 §4.3 逐条对号）：共享验收样例加载六判据 / 八条校验坏例 / 段一四要素顺序与两次调用重读 / 跨进程锁互斥与强杀重取 / 拒锁零写入 / 归档撞名不覆盖 / 模板名字经 YAML 编码 / 未知键 WARN 只记键路径不打印值 / 解析错误文案不回显源码片段。

## 场景 6：依赖方向与禁用词（grep 判据）

```bash
# 在仓库根执行
go list -f '{{.Imports}}' ./internal/core                  # 预期 [context errors time]（仅标准库；不含任何第三方或 github.com 路径）
grep -rn 'time.Local' --include='*.go' cmd internal        # 预期零命中（禁用词）
grep -rn 'use_skill' --include='*.go' cmd internal         # 预期零命中（宪法 IV）
grep -rn 'context.WithValue' --include='*.go' cmd internal # 预期零命中（宪法 VII）
grep -nE 'eino|langchaingo|adk-go|trpc-agent-go' go.mod    # 预期零命中（无框架坐标）
```

- `cli` 不 import `workspace` 由既有 `internal/cli/ports_test.go` 的禁止清单**自动判卷**（`profile` 命令走端口即自动受判）。
- 门禁收尾另须 `gosec -quiet ./...` 与 `govulncheck ./...` 零命中（随 `make check` 的 `sec` 目标）。

## 场景 7：剩余人工项（harness 之外）

逐条见颗粒度文档 §6「验证与验收」；执行人 = 用户：

1. 门禁七目标全绿读数（场景 1）；
2. 验收 Demo 三条人工执行（运维装载 / create-delete 往返 / 运行中拒锁——拒锁态本 US 以单测形态判卷，人工复核文案）；
3. Compatibility §5.1 共享验收样例的**人工对拍**（判据 3·6——技能卡片路径与未来十次触发序列）；
4. 写模式拒绝文案人工复核（可读性口径）；
5. Windows 锁分支**运行读数**本机登记（CI 绿**不**覆盖 `lock_windows.go`）；
6. 凭证 grep 复核（本 US 无凭证面，沿用全局不变量 4）；
7. 装配确认（`profile` 命令组注册、端口注入、`--help` 文案）；
8. 变动范围核对：`git status --short` 与交付物清单逐项对照、**无探针残留**。

## 参考

- 实体与校验规则：[data-model.md](./data-model.md)
- 决策与写前核实清单：[research.md](./research.md)
- 固定字面量（渲染模板 / 日志键 / schedules detail 表）：[docs/us/us3-agent-profile.md](../../docs/us/us3-agent-profile.md) §6
- AGENT.md 字段表与八条校验：契约 [docs/contracts/agent-md.md](../../docs/contracts/agent-md.md) §2
- 锁语义：契约 [docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md) §4.3.1
- 兼容判据：[docs/Compatibility.md](../../docs/Compatibility.md) §5.1
- 起步手册（工具链 / `GOPROXY` / `basalt init`）：[docs/GettingStarted.md](../../docs/GettingStarted.md)
