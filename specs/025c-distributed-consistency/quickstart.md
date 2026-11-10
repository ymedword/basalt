# Quickstart: 已发布资产自动分发与跨进程一致性（US-25C）

**用途**：定向判据的可执行入口与人工链路索引。实现细节见 [plan.md](plan.md)、[data-model.md](data-model.md)；运行时序见模块卡 §2.3。

## 1. 前置

- Go 工具链按 `go.mod` `toolchain` 固定版本（`go version` 报 go1.27.3）；`GOPROXY=https://goproxy.cn`。
- 本机 PostgreSQL 夹具：`dev-env/scripts/with-postgres.sh`（127.0.0.1:55432）。
- 管理台测试：`assets/console` 下 `npm ci` / `npm test`（版本以 `package-lock.json` 为准）。
- 站点测试：`website` 下 Node 24.22.0（`.nvmrc`）与 `npm ci`；本机不可用时登记未验证，不降引擎声明。
- 自动验证**不得**调用真实模型；本地协议桩与冻结夹具。

## 2. 定向判据（先稳定，后冻结代码）

按顺序执行四个专属 stage；每次只跑当前 stage，全绿后再进入下一个：

```sh
sh scripts/check-acceptance.sh --stage us25c-base
sh scripts/check-acceptance.sh --stage us25c-sync
sh scripts/check-acceptance.sh --stage us25c-fault
sh scripts/check-acceptance.sh --stage us25c-docs
```

Windows 等价：

```powershell
.\scripts\check-acceptance.ps1 --stage us25c-base
.\scripts\check-acceptance.ps1 --stage us25c-sync
.\scripts\check-acceptance.ps1 --stage us25c-fault
.\scripts\check-acceptance.ps1 --stage us25c-docs
```

单包定向（示例）：

```sh
GOPROXY=https://goproxy.cn go test ./internal/assets -run 'TestEncodeBundle|TestDecodeBundle' -count=1
GOPROXY=https://goproxy.cn go test ./internal/app -run 'TestAssetSync|TestPolicyRefresh|TestMCPPrepare|TestScheduler_' -count=1
GOPROXY=https://goproxy.cn go test -tags=integration ./internal/storage/postgres -run 'TestMigration0013|TestWorkerOverview|TestStartRun_Pinned' -count=1
```

## 3. 关键行为的手工小验

1. **先载荷后指针**：在测试夹具中令载荷写入失败，发布返回失败；`resource_pointers` 行不变、`agent_bundle_payloads` 无新行。
2. **同哈希补载荷**：删除载荷行后重发同哈希，载荷恢复、指针 `updated_at` 不变。
3. **就绪门**：未同步完成时触发到期 → 不产生执行行、`next_fire_at` 不推进；完成后下一轮正常领取。
4. **钉住版本**：领取后发布并同步新版本 → 该 Run 记录旧哈希；下一触发记录新哈希。
5. **扫描公平**：前 50 条持续失败时第 51 条在有限轮内被处理；持续每秒到期仍能结束一圈。
6. **概览三态**：造同名不同实例、心跳超时与已停止行，核对计数全库、明细上限 200、截断文案、读取失败显示不可用。

## 4. A+B 真实链路（人工，未授权项不执行）

按模块卡 §4.5 九步执行；其中真实模型调用与真实 webhook 未授权，只能执行不触发模型的步骤，其余登记为未验证。核心断言：

- 管理台修改并发布 → 两端 `assets status` 均显示「已安装哈希 = 指针哈希」；
- 两端分别执行后续定时任务 → Run 资源清单记录同一版本哈希，实际配置值与发布一致；
- 在途 Run 保持旧版本；
- 载荷故障与数据库中断分别验证，恢复不靠重导种子；
- 未确认副作用的业务调用计数不翻倍。

## 5. 冻结与全量门禁

四个 stage 全绿后**冻结代码**，只运行一次全量：

```sh
sh scripts/check-acceptance.sh
```

完成定义仍是 `make check` 七目标（`fmt lint sec test test-integration cover build`）全绿；全量入口是超集，不得用子集冒充。

## 6. 预期读数与失败处置

- 每个 stage 报告必须给出具名测试的四态（通过 / 失败 / 跳过 / 未验证）与本轮读数；SKIP 与未验证不得计为通过。
- 失败先定位到 `file:line` 与失败断言，按 `docs/DevelopmentLoop.md` §7 陷阱表核对；同一问题 3 次不收敛转 `/debug`。
- 容器 / 平台相关用例在 Windows 原生不可用时 `t.Skip` 并写明理由，登记未验证清单，不冒充通过。
