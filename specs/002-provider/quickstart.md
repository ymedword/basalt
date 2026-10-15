# 快速验证指南：US-2 Provider 对接（能力一）

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-16

本文件给出**可复制执行**的验证步骤，用于证明本 feature 已交付且可工作。实现细节见 [tasks.md](./tasks.md) 与颗粒度文档 [docs/us/us2-provider.md](../../docs/us/us2-provider.md) §3；实体与状态机见 [data-model.md](./data-model.md)；固定字面量以 [docs/contracts/](../../docs/contracts/) 为准。

## 前置条件

| 项 | 判据 | 复核命令 |
| --- | --- | --- |
| Go 工具链 | 与 `toolchain` 行一致（1.27.2） | `go version` |
| GNU Make | 就位 | `make --version` |
| 新增坐标 | 已进 `go.mod` / `go.sum`（唯一新增） | `grep go-openai go.mod` |
| 模块代理 | 本机直连上游不可达 | 命令显式带 `GOPROXY=https://goproxy.cn` |
| 真实调用（可选） | 凭证经环境变量注入 + 显式执行开关 | 见场景 4——缺一即 SKIP，**不算通过** |

## 场景 1：门禁全绿（P1 的完成定义）

```bash
make check
```

**预期**：七目标（fmt / lint / sec / test / test-integration / cover / build）依次通过，退出码 0；`cover` 目标的读数里 `internal/provider` **不再**记「无可执行语句」，而是给出百分比并判 `>= 50%`。

只跑本 feature 涉及的五个包（默认层；本 feature 新增 **54** 条，命令同时回归包内前序用例）：

```bash
go test -race -count=1 ./internal/provider/ ./internal/storage/ ./internal/app/ ./internal/cli/ ./internal/testutil/
```

**预期**：全部 PASS；`-race` 无告警（并发与取消用例在这里被覆盖）。

## 场景 2：`basalt provider list` 的四形态（P2）

在临时工作区搭一实例：

```bash
mkdir -p /tmp/basalt-provider-demo && cd /tmp/basalt-provider-demo
basalt init
cat > basalt.yaml <<'YAML'
providers:
  - name: alpha
    base_url: https://api.example.com/v1
    api_key: ${DEMO_API_KEY_ALPHA}
    default_model: alpha-chat
  - name: beta
    base_url: http://127.0.0.1:8000
    api_key: ${DEMO_API_KEY_BETA}
    default_model: beta-chat
YAML
DEMO_API_KEY_ALPHA=stub DEMO_API_KEY_BETA=stub basalt provider list
```

**预期**：逐条列出 `alpha` / `beta` 的**名称 / 收窄地址 / 缺省模型** + 凭证状态文案、有汇总行、退出码 0；输出**不含**任何凭证值（示例里的 `stub` 即探针值，用它 `grep` 断言零命中）。

异常态对照（各自点名原因、退出码 1）：

```bash
cd /tmp && mkdir -p basalt-provider-empty && cd basalt-provider-empty && basalt init
basalt provider list                          # 缺 basalt.yaml → 点名配置文件路径（不沿用 status 的容忍语义）
printf 'providers: []\n' > basalt.yaml
basalt provider list                          # providers 段为空 → 点名段名
```

负向一例（不需要凭证）——地址含不支持成分：

```bash
cd /tmp/basalt-provider-demo
printf 'providers:\n  - name: gamma\n    base_url: https://u:p@api.example.com/v1?route=x#frag\n    api_key: ${DEMO_API_KEY_ALPHA}\n' > basalt.yaml
DEMO_API_KEY_ALPHA=stub basalt provider list   # 预期：装载期拒绝；文案点名 providers[0].base_url 与成分，
                                               #       且**不含**该 URL、不含 u:p（userinfo 里可能就是凭证）
```

## 场景 3：默认层判卷关键回归（P1 的守卫点）

```bash
go test -race -count=1 -run 'TestProviderService_AuditPendingBeforeCall|TestProviderService_RefusesCallWhenAuditPreWriteFails|TestProviderService_CancelDuringCallRecordsTimeoutKind|TestProviderService_PreCanceledContextStillAuditsFailedCall|TestOpenAIClient_RoundTripsUnknownFieldsTwoHops' ./internal/provider/
go test -race -count=1 -run 'TestLLMCallRepo_FinishCallTruncatesErrorMessageAt4KBWithoutBreakingRunes|TestLLMCallRepo_FinishCallRejectsNonPendingRow' ./internal/storage/
go test -race -count=1 -run 'TestConfig_RejectsProviderBaseURLWithUserinfoQueryFragment|TestProviderList_RedactsBaseURLUserinfoAndQuery' ./internal/cli/
```

**预期**：全部 PASS——留痕顺序（假客户端在调用内部当场查库）、审计不可用零发出、取消路径错误链保留、调用前已取消仍先留痕、未知字段两跳往返、4 KB 截断不切断 UTF-8、终态只写一次、两层地址拒绝与展示层脱敏。

治理关键词的 grep 判据（用例存在性自查，逐条应有命中）：

```bash
grep -rn "Cancel" internal/provider/*_test.go
grep -rn "Concurrent" internal/provider/*_test.go internal/storage/llm_calls_test.go
grep -rn "CrashWindow" internal/storage/llm_calls_test.go
grep -rn "AuditPreWriteFails\|TerminalWriteFailure" internal/provider/*_test.go
grep -rn "TwoHops" internal/provider/*_test.go
grep -rn "EchoingKey\|RedactsBaseURL" internal/provider/*_test.go internal/cli/*_test.go
grep -rn "RejectsProviderBaseURL\|RejectsBaseURLWithUnsupportedParts" internal/provider/*_test.go internal/cli/*_test.go
grep -rn "ResponseRead\|OverLimit\|EncodedHashInPath\|BigIntegers" internal/provider/*_test.go internal/cli/*_test.go   # 实现评审回合（P1×2 / P2×2 / 优化×2）的回归
```

## 场景 4：真实调用验收（**人执行**；双门控）

前置：① 独立临时工作区（**不复用开发库**）；② `basalt.yaml` 配**两家** provider（`api_key` 一律 `${VAR}` 占位）；③ 真实凭证经**环境变量**注入（不落盘 / 不落日志 / 不进对话）；④ **显式执行开关**与凭证 env **同时**满足，缺一即 `t.Skip` 并写明理由。

正例：

```bash
BASALT_REAL_CALL=1 BASALT_DEMO_DIR=<演示工作区> go test -tags=integration -count=1 -run TestProviderService ./internal/app/
```

**预期**：`--- PASS`（**不是** `--- SKIP`——跳过不构成验收通过）；用例入口显式设截止时间；随后查库：

```bash
sqlite3 <演示工作区>/.basalt/basalt.db "SELECT provider, model, status, prompt_tokens, completion_tokens, total_tokens, error_kind FROM llm_calls;"
```

**判据**：`status=success`；token 三列**与响应一致**——未返回 usage 则三列 0 且**仍判成功**（不得把 `token > 0` 写成无条件判据，也**不得**由 `total_tokens > 0` 反推"供应商返回了 usage"）；`error_kind` 为空。

反例（**不需要凭证**，故不因缺凭证跳过）：

```bash
BASALT_REAL_CALL=1 BASALT_DEMO_DIR=<演示工作区> go test -tags=integration -count=1 -run TestProviderService ./internal/app/
```

**判据**：把某家 `base_url` 指向不可达端点重跑 → 失败同样落审计（`failed` + `error_kind` 非空，预期 `external_error`）。

凭证复核（范围限定为**运行对象**——全仓 grep 会命中文档示例，不作为判据）：

```bash
grep -rnE "sk-|api_key: [^$]" <演示工作区>/basalt.yaml   # 预期：零命中
```

> 缺凭证（或未开执行开关）时用例会 SKIP——收口报告必须区分 `--- PASS` 与 `--- SKIP`，跳过的那次**不构成**「真实调用验收通过」。

## 参考

- 需求与验收场景：[spec.md](./spec.md)
- 实体与状态机：[data-model.md](./data-model.md)
- 决策与写前核实清单：[research.md](./research.md)
- 模块级细节（Harness 映射表 / 人工项）：[docs/us/us2-provider.md](../../docs/us/us2-provider.md) §4 / §6
- 固定字面量：[docs/contracts/schema.sql](../../docs/contracts/schema.sql)、[docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md)
