# US24 验证与运行指南

本文件是**验证指南**，不是第二份设计或契约：行为、字面量与验收点以 [spec.md](spec.md)、[data-model.md](data-model.md) 与 `docs/contracts/` 为准。

## 前置条件

| 项 | 要求 |
| --- | --- |
| Go | 仓库 `toolchain go1.27.3`（`go 1.27` 仅为最低要求）；`GOTOOLCHAIN=auto` 会自动取用 |
| 模块代理 | `GOPROXY=https://goproxy.cn`（本机直连不可达） |
| PostgreSQL | 集成 / PG 用例与双进程验证需要真实实例（16/17 + `vector` 0.8.x + `pg_trgm`）；`BASALT_TEST_PG_DSN` 缺省即失败，**不跳过** |
| Node / npm | 管理台测试与构建需要 Node 24.22.0；本机无系统 Node 时用便携版并先加 `PATH`（构建期工具链，不进 Go module） |
| 模型 / webhook | **不需要真实模型**：所有用例使用本地协议桩与冻结夹具；webhook 用本地回环桩；不得注入真实凭据 |

## 一键验收

```sh
# 本 US 专属 stage（超集入口；us24-base 与 us24-fault 两套）
sh scripts/check-acceptance.sh --stage us24-base
sh scripts/check-acceptance.sh --stage us24-fault

# 逐任务验收（实现阶段）
sh scripts/check-acceptance.sh --stage us24-base --task T0xx

# 完成的定义：宪法口径的七目标全绿
make check
```

Windows PowerShell 等价入口：`.\scripts\check-acceptance.ps1 --stage us24-base [--task T0xx]`。
判读规则：退出码 0 **且**每个 check 的 `required_tests` 均具名 PASS；空匹配、SKIP、未设置 `BASALT_TEST_PG_DSN` 都**不算**通过。

## 分层验证

```sh
# 1) 单元：租约类型 / keeper 有界停止 / 到期扫描 / bundle
go test -count=1 -timeout=5m ./internal/core ./internal/assets

# 2) 迁移：v11 库回填、版本 12、幂等、resource_id 编码、约束名
go test -count=1 -tags=integration -timeout=10m ./internal/storage/postgres -run '^TestMigration0012_|^TestLeaseResourceIDEncoding$'

# 3) PG 集成：租约、会话锁、FencedTx、调度去重、回收、Flow 判据
go test -count=1 -tags=integration -timeout=20m ./internal/storage/postgres -run '^Test(Lease|Worker|Run|Scheduler|Reclaimer|Flow|Resource).*$'

# 4) 装配：双 App / 双 Scheduler 共享 schema、资产准入、停机、归档、学习恢复
go test -count=1 -timeout=15m ./internal/app -run '^Test(Distributed|Assets|Shutdown|Archive|Learning).*$'

# 5) 限额与 CLI
go test -count=1 -timeout=5m ./internal/callers ./internal/cli -run '^Test(Limiter|Assets).*$'

# 6) 竞态：与发布构建分属不同命令
go test -race -count=1 -timeout=20m ./internal/core ./internal/storage/postgres ./internal/app
```

## 本机双进程人工验证（G5，14 项）

启动两个 `serve` 进程（各自工作区、同一 `BASALT_TEST_PG_DSN` 指向的库），按模块卡 §6.1 逐项执行；每项记录命令、时间与关键查询输出。

| # | 场景 | 关键核对 |
| --- | --- | --- |
| 1 | 并发领取 | 同一 `*/5` 任务跑 2 分钟：每个 `planned_at` 恰一行；`lease_events` 的 `claimed` 数与行数一致 |
| 2 | 崩溃接管 | `kill -9` A：记录崩溃 / 到期 / B 首次回收 / 终态四段时间；B 只收尾 A 的 Run |
| 3 | 暂停恢复（**非重启**） | `SIGSTOP` → 到期 → B 接管 → `SIGCONT`：A 的提交全部 `ErrLeaseLost`、不覆盖结果、不重跑已判动作 |
| 4 | 同会话恢复期窗口 | 恢复中同 `session_id` 新消息 → `SESSION_BUSY` 且**不产生 Run 行**；收尾后放行 |
| 5 | 取消路由 | 非属主取消 → `cancel_requested=true`；属主在 ≤ 续约间隔内收为 `cancelled` |
| 6 | 投递未知 | 桩收到但阻断回写 → `schedule_deliveries.status=unknown`，桩计数仍为 1（不重发） |
| 7 | 资产不符 | B 安装旧 bundle → `ASSET_VERSION_MISMATCH`、无 Run 行；换新后放行 |
| 8 | Flow 接管 | `SIGSTOP` 节点 Run → B `TookOver=true` 先 `Recover`：已完成节点复用（Run 行数不增）、未知节点 blocked；`SIGCONT` 后 A 的 `Advance` 被拒 |
| 9 | 停机顺序 | `SIGTERM`：停止领取 → 继续续约 → join → 释放 → 关库；`pg_stat_activity` 连接在收尾后才消失；B 释放后即可接管 |
| 10 | 升级 | v11 库构造三类在途行 → 0012 → 新 Worker：三类回填租约被首轮回收领取并收尾 |
| 11 | 数据库中断 | `pg_terminate_backend` / 停 PG 5–10s：A 提交被拒、B 到期接管、A 不覆盖不重跑 |
| 12 | 时钟变化 | A 本机时钟 ±10 分钟：到期与接管时机**不随本机时钟变化**，A 旧提交仍被 fence |
| 13 | 响应丢失与事件重投 | 掐断回写：`unknown` 不重发、每个计划触发一条执行；**不检验**"重复补跑不增行"（补跑是新 Run） |
| 14 | 网络分区 | 阻断 B→PG 后在 A 上触发接管：B 提交被拒、A 完成接管、B 恢复后不自封主不重复执行 |

## 收口证据口径

- 自动化门禁：`make check` 七目标全绿 + 两个 stage 全绿；`test-pg` 与 `-race` 分开运行。
- 双进程记录与故障矩阵（十四项）：逐项"注入方式 / 实际结果 / 是否重放"。
- 审计与诊断对账：被回收 Run 的 `llm_calls` / `tool_invocations` 终态与 `lease_events`；确认无第二套追加路径。
- 契约与依赖：§3.5 文档 diff；`git diff --exit-code docs/contracts/schema.pg.sql` 为空；`go.mod` / `go.sum` 与起点 `138382f` **空 diff**。
- 未验证清单：真实双宿主、S3、DB HA、正式升级工具链、真实 webhook、真实模型，逐项标 `未测量` / `未授权`。
- 并发正确性在取得 PG 用例读数前保持 `未验证`；不以本地桩通过冒充。
