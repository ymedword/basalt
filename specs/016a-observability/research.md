# US-16A Research and Decisions

## 2026-11-01 输入复核

- 基线 a2746ed；代码基线 092e1cd；用户明确固定编号 US-16A，US-17 保留。
- Go native/WSL 均 1.27.3；本地 PG 55432/测试库 SELECT 1 通过；凭据由既有脚本注入，不打印。
- main CI 41305923229 的 headSha 为 092e1cd，结论 success，五个 job success；这不是本 US 新源码的证据。
- 文档备份：career/.backups/20261101T130517738088Z-obs-before-codex，121 文件逐字节恢复核对通过。
- ADR-01–12 见 plan；派生修正保持已审强语义，不删除来源、不吞错误、不补造缺失值。

## OTLP spike（T003 待执行）

三方案：A 手写 JSON、B 官方 SDK/exporter、C 官方 protobuf 类型+标准库有界 sender。B 优先，须公开 API 稳定返回三类信号批次拒绝数和 warning、内部重试可关闭；不可解析错误字符串取计数。不满足采用 C。版本、许可、来源校验、依赖树、三信号实测、二进制体积待记录；未执行前不得写已验证。

官方资料核对只作设计依据，不替代本地固定版本 spike：OTLP 规范要求部分成功不自动重试，只提供信号记录计数；每个 signal 分别处理。具体版本 API 必须从所选源码复核。

## T003 固定版本 spike 结果（2026-11-01）

- 自动选择 **C：官方生成protobuf类型 + 标准库有界HTTP sender**。B候选traces/metrics v1.47.0、logs v0.23.0的公开Export/Upload接口仅返回error；批次拒绝计数/warning封装于各模块internal.PartialSuccess，无法经受支持的公开类型提取。不能解析Error字符串或依赖反射/internal导入，所以不满足账本公开结构化计数准入；不是协议不兼容。
- C坐标：`go.opentelemetry.io/proto/otlp v1.11.1`（Apache-2.0，官方生成类型）；`google.golang.org/protobuf v1.36.12`（BSD-3-Clause）。均先go list -m -versions，再固定go mod download；模块zip/GoMod校验与LICENSE SHA-256、传递依赖树保存在隔离spike记录。
- 临时目录：`%TEMP%/basalt-otlp-spike-20261101`；仓库go.mod/go.sum未因spike改变。B/C全部模块下载在既有代理scope，Collector只回环，不接外部后端。
- C三信号六子例：trace/log/metric分别验证rejected=0 warning与rejected=1部分成功，稳定解析字段且每次只一HTTP请求；`go test -v -count=1 ./...`通过。
- 官方Collector v0.164.0：archive SHA-256 `ff9a3d79687cca7863bfbea1198542ca0c1800841289541913211d75ab6fea38`，与官方.sha256一致；binary SHA-256 `20aa55b613207c37f6b65df555b16001ec3f965d7218bf619742cb8579f0daa1`；LICENSE来自同版本官方仓库并留摘要。
- 真实回环Collector三信号：每类独立Accepted=1/Rejected=0、Requests=1，debug输出分别包含synthetic-task、synthetic-event、basalt.obs.window.synthetic；本轮拥有的Collector进程已退出。此为spike载荷，不冒充产品映射、canary或最终Collector验收；T026仍必须跑最终候选。
- 隔离探针`CGO_ENABLED=0`构建：A 6,071,296 bytes，B 14,959,104 bytes，C 13,028,864 bytes。入口不同，只是探针绝对体积，不是生产二进制依赖增量，不是容量承诺。最终候选体积在T028实测。
- 原始证据：临时目录modules.json、各candidate的go.mod/go.sum/modules-tree.json、main_test.go与test-output.txt、size-comparison.json、collector-verified.json、collector-debug.log、collector-three-signals.jsonl、result.json。最终保留脱敏索引和摘要到.loop，临时资源按归属清理。
