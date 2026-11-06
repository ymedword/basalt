# US22 验证与运行指南

本文件是**验证指南**，不是第二份设计或契约：行为、字面量与验收点以 [spec.md](spec.md)、[data-model.md](data-model.md) 与 `docs/contracts/` 为准。

## 前置条件

| 项 | 要求 |
| --- | --- |
| Go | 仓库 `toolchain go1.27.3`（`go 1.27` 仅为最低要求）；`GOTOOLCHAIN=auto` 会自动取用 |
| 模块代理 | `GOPROXY=https://goproxy.cn`（本机直连不可达） |
| PostgreSQL | 集成 / PG 用例与 E2E 需要真实实例；`BASALT_TEST_PG_DSN` 缺省即失败，**不跳过** |
| Node / npm | 管理台测试与构建需要 Node 24.22.0；本机无系统 Node 时用便携版 `/tmp/node24/node-v24.22.0-win-x64`（先加 `PATH`） |
| 模型 | **不需要真实模型**：所有用例使用本地协议桩与冻结夹具；不得注入真实凭据 |

## 一键验收

```sh
# 本 US 专属 stage（12 个 check，超集入口）
sh scripts/check-acceptance.sh --stage us22-base

# 逐任务验收（实现阶段）
sh scripts/check-acceptance.sh --stage us22-base --task T0xx

# 完成的定义：宪法口径的七目标全绿
make check
```

Windows PowerShell 等价入口：`.\scripts\check-acceptance.ps1 --stage us22-base [--task T0xx]`。
判读规则：退出码 0 **且**每个 check 的 `required_tests` 均具名 PASS；空匹配、SKIP、未设置 `BASALT_TEST_PG_DSN` 都**不算**通过。

## 分层验证

```sh
# 1) 默认关闭零副作用（E2E）
go test -count=1 -tags=e2e -timeout=10m ./internal/e2e -run ^TestUS22_E2E_DisabledNoSideEffects$

# 2) 抽取 / 校验 / 预算 / 重试（memory 包）
go test -count=1 -timeout=5m ./internal/memory -run ^TestUS22_

# 3) 两阶段审计与时延分段（provider 包）
go test -count=1 -timeout=5m ./internal/provider -run ^TestUS22_

# 4) 装配、确认门禁、恢复（app 包，storage 面用例需 -tags=integration）
go test -count=1 -tags=integration -timeout=10m ./internal/app -run ^TestUS22_

# 5) 迁移、召回三入口、确认事务（PG 包）
go test -count=1 -tags=integration -timeout=10m ./internal/storage/postgres \
  -run '^(TestUS22_|TestPostgres_|TestObservability_)'

# 6) 只读面与契约（web / cli）
go test -count=1 -timeout=5m ./internal/web -run ^TestUS22_
go test -count=1 -timeout=5m ./internal/cli -run ^TestUS22_

# 7) 管理台三页签（console）
npm --prefix assets/console test
npm --prefix assets/console run build

# 8) 竞态（与发布构建分属不同命令）
go test -count=1 -race -tags=integration -timeout=10m ./internal/app \
  -run '^TestUS22_(Confirm_ConcurrentOneWinner|Confirm_ConcurrentRevokeFailsClosed|Hook_OncePerRun|Hook_BoundedAndNoGoroutineLeak|Recovery_UnknownAdvancesNoReplay|Unknown_NoRestartNextRunNoReplay)$'
go test -count=1 -race -timeout=5m ./internal/memory -run '^TestUS22_Concurrent_SameAgentSerialized$'
```

## 预期结果

- 默认关闭：完成一次 Run 后 `memory_extractions` 0 行、无 `purpose='memory_extract'` 的 `llm_calls`、`.basalt/learning/drafts/` 不存在。
- 启用后：合格条目以 `candidate / auto_extract / archive` 落库，`memory_candidate_meta` 与 `memory_candidate_sources` 各至少 1 行；确认前关键词与语义召回均不命中。
- 确认后：条目 `active`，关键词与语义召回命中；`keep_both` 回执 `superseded_id=0`，`supersede` 回执带旧条目 id 且旧目标转 `superseded`。
- 来源失效：删除 / 到期来源后，`RecallByKeyword`、关键词回退、向量路径与 `recall_memory(mode=keyword)` 真实工具入口都不返回派生条目；显式条目不受影响。
- 恢复：`pending` 启动后转 `success` 或 `unknown`（`verified_at` 非空），游标推进，**不重放**；同来源新 Run 恢复正常抽取。
- 时延：钩子端到端 ≤20s；慢模型（8s）仍写 `success`；工作段耗尽仍在总预算内落 `failed` / `timeout`。
- 迁移：v10 → v11 前向升级成功，`source='auto_extract'` 与 `purpose='memory_extract'` 可写，五张表存在，表数 82 → 87。

## 人工验证（review ② / 收尾走查）

以下不替代 harness 判卷，等用户人工执行：

1. 空配置启动，确认「记忆」页无「待确认 / 学习记录」新增内容，`basalt learning status` 显示未启用。
2. 在 `basalt.yaml` 打开 `learning.auto_extract: true` 并列出目标 Agent，重启；完成一次 Web / CLI 会话。
3. 管理台「记忆 → 待确认」查看逐字证据（消息主键、角色、引文、时刻）与来源抽取；确认「保留双方」，新 Run 能召回。
4. 另造一条错误候选，不确认或直接删除；后续 Run 不召回。
5. 删除 / 让来源条目到期后，派生条目的召回停止。
6. 制造一次提交未知并重启，核对恢复只查证、不重放；重启后新 Run 正常。
7. 查看 `.basalt/learning/drafts/` 是否有只读草案；确认 Agent 文件工具无法读取，`basalt learning handoff` 只在取锁 + `cli:<OS user>` + `user_confirmed` 下工作。
8. 核对左侧导航无「学习」入口、无 `/console/learning*` 路由、学习记录页签无写按钮。

## 效果口径

真实模型抽取质量、任务收益、召回增益与技能绑定收益一律 `未测量`。本指南中的确定性验收**不得**转写为效果提升结论；相关实验归 D-2（拟），需另行立项并解除真实模型禁令。
