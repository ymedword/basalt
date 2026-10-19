# Quickstart — US-15 工程验收基础设施

本文件是**验证 / 运行指南**，不包含实现代码。字段与规则见 [data-model.md](data-model.md)，设计与取舍见 [research.md](research.md)，检查定义以模块卡 §4.2 为唯一事实源。

## 前置条件

- Go 1.27.2（`go version` 与 `toolchain` 行一致）
- 仓库根 `D:\code\basalt`（或对应 clone），分支 `015-acceptance-infra`
- `GOPROXY=https://goproxy.cn`（本机直连不可达）
- 真实项额外要求：执行 shell 中已注入 `BASALT_REAL_LLM_BASE_URL` / `BASALT_REAL_LLM_MODEL` / `BASALT_REAL_LLM_API_KEY` / `BASALT_REAL_FEISHU_URL`（值不写盘、不进对话）

所有运行目录必须是**尚不存在的新目录**；`accept` 拒绝复用已存在目录。

## 场景 1：stage 时效预检（lint）

```sh
sh scripts/check-acceptance.sh --lint-stages --stage harness/stages/current.json
```

- **预期**：退出码 0；`current.json` 的每个 `required_tests` 在 `go test -list` 中可见。
- **反例**：删掉任一 required_test 的测试函数 → 退出码非 0 并点名缺失项；历史 stage 的同类失配只打印 WARN。

## 场景 2：验收工具自身判据

```sh
go test -json -count=1 ./scripts/acceptance -run '^(TestRecord_|TestStageLint_|TestStages_|TestRecords_|TestLedger_|TestFingerprint_|TestAccept_ReportPathsRelative|TestAccept_ExternalRequiresAuthorizationFlags)'
go test -json -count=1 ./scripts/acceptance -run '^TestRepoInvariants_'
```

- **预期**：全部具名 PASS；无 SKIP。
- **反例**：
  - 把 process check 的 `binary_sha256=` 行全部删掉 → 应退出 2 且无记录（不得降级为 source）。
  - `--out` 指向仓库外 → 退出 2；报告不写、记录不生成。
  - external check 缺授权旗标，或未选 external 却给旗标 → 均退出 2。
  - 在 `go.mod` 加入 eino / gorm，或在 `cmd/` / `internal/` 写入 `time.Local` → 相应不变量变红。

## 场景 3：现行累计验收

```sh
RUN_PREFIX=".loop/runs/local-$(date -u +%Y%m%dT%H%M%S)"
sh scripts/check-acceptance.sh --stage harness/stages/current.json --stage-only --out "$RUN_PREFIX-current"
```

- **预期**：33 个 check 全部 PASS；运行目录有 `report.json` 与 `record.json`；`record_id` 等于运行目录名；`record.json` 的 `evidence.report_sha256` 等于同目录报告哈希。
- **检查报告路径**：`report.json` 中 `checks[].log` 均为仓库相对路径、`/` 分隔，可拼回真实日志。
- **注意**：`015-backfill` 在首批记录发布前可能因 `TestRecords_BackfillCoversCatalog` 处于预期失败（bootstrap）；按收口顺序先执行回填并发布记录，再跑本步骤。

## 场景 4：进程级回填（平台中立）

```sh
go test -tags=e2e -count=1 ./internal/e2e
```

- **预期**：`TestBackfill_*` 全部 PASS；构建真实二进制（`CGO_ENABLED=0`、`-trimpath`）并使用临时工作区与回环外部服务；不访问真实网络。
- **反例**：让 `init` 非幂等、越界读被放行、cron 不触发、409 文案变化等 → 对应子测试变红。

## 场景 5：tagged 代码静态检查

```sh
go vet -tags=e2e,slow,real ./internal/e2e
```

- **预期**：退出码 0。
- **边界**：`e2e` 代码不被无 tag 的 `make lint` 扫描；本命令只补 `go vet`，不宣称 staticcheck 已覆盖（见模块卡 §3.4 末段）。

## 场景 6：Linux 信号场景（CI Ubuntu / WSL）

```sh
sh scripts/check-acceptance.sh --stage harness/stages/us15-linux.json --stage-only --out "$RUN_PREFIX-linux"
```

- **预期**：`TestBackfill_SignalLinux/grace_exhausted` 与 `/sigterm_order` 具名 PASS。
- **Windows**：无法投递 SIGTERM / 真实 Ctrl+C，记未验证（平台限制），不得记为通过。

## 场景 7：慢速超时（收口执行）

```sh
sh scripts/check-acceptance.sh --stage harness/stages/us15-slow.json --stage-only --out "$RUN_PREFIX-slow"
```

- **预期**：`TestBackfill_SlowTimeout/run_timeout` PASS；约 5 分钟量级，不进入 CI 与 `current.json`。

## 场景 8：已授权真实模型 / 飞书外发（收口执行）

仅在用户明确授权且四个环境变量已注入时执行；授权只对本次运行有效：

```sh
sh scripts/check-acceptance.sh --stage harness/stages/us15-real.json --stage-only \
  --authorization-ref user-2026-10-19-real-llm-webhook \
  --authorization-scope "real-llm:openai-compatible; real-webhook:feishu; synthetic-data-only; model-calls<=30; cny<=10; sends<=5" \
  --out "$RUN_PREFIX-real"
```

- **预期**：`015-real-model`（900s）与 `015-real-feishu`（180s）PASS；记录与报告的 `authorization` 逐项相等；输出只出现 `scheme://host`；模型调用 ≤30、飞书发送 ≤5。
- **缺变量**：测试 FAIL（不是 SKIP），check 失败、记录如实失败。
- **禁止**：把授权写进 stage、把凭证写入任何文件、扩大到后续运行。

## 场景 9：七目标门禁

```sh
make check
```

- **预期**：`fmt` / `lint` / `sec` / `test` / `test-integration` / `cover` / `build` 全绿；`harness/acceptance.json` profile 与 `Makefile` 未变。

## 场景 10：发布面核对（收口）

```sh
go test -json -count=1 ./scripts/acceptance -run '^(TestRecords_|TestLedger_|TestStages_)'
```

- **预期**：每条发布记录与同次报告配对；台账与记录一一对应；三视图冲突显式；`records/` 与 `ledger.md` 不计入源码指纹。
- **反例**：改动已发布报告任一字节 → 绑定用例变红；删除台账行或留下悬空链接 → 台账用例变红。

## 预期失败（bootstrap）

首批回填记录发布前，`TestRecords_BackfillCoversCatalog` 处于预期失败状态。**不得**用 `t.Skip`、删除测试或放宽断言规避；按"实现 e2e 与记录工具 → 执行回填并发布记录 → `make check` 收口"的顺序推进。
