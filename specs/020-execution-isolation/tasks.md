# Tasks: US20 执行隔离升级

**Input**: spec.md / plan.md / research.md / data-model.md / contracts。

**Tests**: 已审 Harness 的必跑测试与关键反例必须落地，安全/并发/审计红态先行；不删除断言或无理由跳过。

## Phase 1: Setup

- [x] T001 核对 US19 接缝、审批与 WSL 环境，冻结输入到 specs/020-execution-isolation/run-state.md；DoD：注册唯一、基线及授权可追溯，analyze 无 CRITICAL。
- [x] T002 从具名判据派生 harness/stages/us20-base.json、us20-oci.json、us20-gvisor.json；DoD：required_tests 与命令匹配，缺环境/授权非通过，stage lint 通过。

## Phase 2: Foundational

- [x] T003 在 internal/core/isolation.go、action.go、store.go 及 isolation_test.go 增加已审纯类型、账本端口和错误映射测试；DoD：Command 零值兼容、Reason 不污染出网闭集。
- [x] T004 在 internal/storage/postgres/migrations/0009_sandbox_execution.sql、connect.go、isolation.go 与 docs/contracts/isolation.pg.sql 实现账本及同源 DDL；DoD：既有迁移零改写、预写唯一/终态/恢复/计数实现，integration 新测试通过。
- [x] T005 在 internal/storage/postgres/history_retention.go、isolation_test.go 增加未清理保护与 cleaned 级联回归；DoD：待清理行不丢指针，清理完可回收。

## Phase 3: User Story 1 - 显式隔离 (P1)

独立判据：旧配置兼容；非法绑定、运行时缺失和不可信镜像拒绝，受控命令不降级。

- [x] T006 [US1] 在 internal/cli/config_isolation_test.go 写配置/默认/未知键/预算区间反例，随后在 config_isolation.go、config.go 严格解析校验；DoD：us20-base-config 配置用例通过，正值/溢出/授权绑定互验。
- [x] T007 [US1] 在 internal/isolation/trust_test.go 写有效及篡改/缺签名/错公钥/非法内容判据，实现 trust.go、release_pubkey.go；DoD：已有公钥验签，签名有效不跳过内容校验。
- [x] T008 [US1] 在 scripts/releasekit/imagelock.go、image.go、release.go、verify.go 与测试及 release/image-lock.json 支持 schema2 合并和签名清单；DoD：镜像构建后合并、不回写源锁、清单签名进入校验链、旧基础字段兼容。
- [x] T009 [US1] 在 internal/tool/command_guard.go、runner.go、shelltools_test.go、isolation_runner_test.go 抽取解释器守卫与脚本映射；DoD：LocalRunner 拒绝 Isolation、基名/内联/越界拒绝，既有回归通过。
- [x] T010 [US1] 在 internal/tool/isolation_runner.go 及必要内部拆分实现 Docker CLI 有界调用和共用安全 argv；DoD：任务/探针/清理均 --pull=never、无网只读非 root、限额、标签、路径边界及参数测试通过。
- [x] T011 [US1] 在 internal/tool/isolation_runner.go、isolation_runner_test.go 实现 create/start/wait/logs/remove 状态与取消、未知不重试；DoD：预执行拒绝 cleaned、未知 failed、输出有界、清理不继承取消。
- [x] T012 [US1] 在 internal/tool/isolation_runner.go、isolation_runner_test.go 实现 Probe 与 RemoveOutputDir 清理 helper；DoD：000/隐藏目录清理、自身删除复查、命名隔离及缺失目录幂等 source 用例通过。

## Phase 4: User Story 2 - 代码执行与产物 (P1)

独立判据：授权后完整执行并回收文件，拒绝零执行，审计一行，容量与产物并发有界。

- [x] T013 [US2] 在 internal/tool/isolation_ledger.go、isolation_ledger_test.go 实现 SandboxAdmission 和 SandboxLedger；DoD：顺序、unknown、写失败 WARN、共享槽位、不重复计数、pin/release、查询失败拒绝用例通过。
- [x] T014 [US2] 在 internal/tool/policy.go、runcode.go、runcode_test.go 实现 ActionRunCode 默认拒绝、严格入参、超时收窄、受控输入与 staging；DoD：真实 Policy 正反例、未配置拒绝、输入累计含脚本且路径受限。
- [x] T015 [US2] 在 internal/tool/runcode.go 及内部产物文件实现扫描、哈希、实例锁内总量/发布/回滚与 releaseStage；DoD：类型/链接/数量/字节/跨 Agent 总量/发布并发/清理四态测试通过。
- [x] T016 [US2] 在 internal/tool/shelltools.go、runcode.go 接入共享准入、账本与结果呈现，在 internal/core/tool_exec_test.go 验证单行审计；DoD：两档共享实例、预写失败零执行、产物拒绝仍清理、Flow 白名单不扩权。

## Phase 5: User Story 3 - 恢复、自检与只读投影 (P2)

独立判据：启动三类恢复正确，遗留占用可释放或保留，页面如实只读。

- [x] T017 [US3] 在 internal/app/isolation.go、isolation_test.go 实现探针、可信清单、tmpfs 实测与三类启动恢复；DoD：handled 含清理容器、账本失败 WARN 保留、孤儿/枚举/目录失败阻断。
- [x] T018 [US3] 在 internal/app/wire.go、web.go、run.go 与测试完成共享 Policy/Admission/PublishMu、实例身份与轻重注册接线；DoD：十二件内置一致、旧配置不变、MCP 边界不变、schema 与审计恢复后接单前清理。
- [x] T019 [US3] 在 internal/cli/isolation.go、ports.go、deps.go、root.go 与 internal/app 接线自检；DoD：CLI 只读探针不触库、未配置/失败/成功输出与退出码正确。
- [x] T020 [US3] 在 internal/web/views.go、server.go、handlers_sandbox.go、handlers_runs.go 与 isolation_view_test.go 增加只读投影；DoD：reader 缺失拒绝、错误500、空数组、无新写入口。
- [x] T021 [US3] 在 assets/console/src/api.ts、views/workspace.ts、views/run-detail.ts、styles.css、tests/isolation.test.ts 显示真实状态；DoD：禁用/空/长文本/清理失败与重启文案，console-test/build 及 dist/integrity 验证。

## Phase 6: 真实验收与收口

- [x] T022 在 docs/contracts/isolation.md、openapi.yaml、run-and-errors.md 同步配置/字段/闭集/状态，补契约测试；DoD：正式契约与实现一致、既有八类和四类不变。
- [x] T023 在 docker/Dockerfile 预建挂载点，docker/basalt.yaml、compose.yaml 补最小可选签名清单接线；DoD：默认服务部署兼容、不默认挂 Docker socket，候选镜像可用于验收。
- [x] T024 在 internal/e2e/us20_fixture_test.go 建真实非 root、两档不同入口、独立 PG/工作区/tmpfs 的夹具与有界控制组；DoD：镜像与 UID 可追溯，清理只触专属资源，无真实模型。
- [x] T025 在 internal/e2e/us20_oci_test.go 落全生命周期/取消/无网/无秘密/只读/限额/孤儿/恢复/篡改用例；DoD：us20-oci 全部 required PASS，N 同时占位后拒 N+1。
- [x] T026 在 internal/e2e/us20_gvisor_test.go 落全生成代码/边界/限额/崩溃/产物用例；DoD：us20-gvisor 全部 required PASS，跨 UID roundtrip、000 与 ..cache 清理真实通过。
- [x] T027 对安全/并发/审计与规定十一条关键回归实施仓库外 overlay 变异，证据记 specs/020-execution-isolation/acceptance.md；DoD：预期断言逐字红，原版绿，发布并发 count=10。
- [x] T028 同步 docs/TechnicalDesign.md、Requirements.md、Operations.md、Compatibility.md、README.md、README.zh-CN.md、basalt.yaml.example、website 与公开图源适用面；DoD：真实范围/容量互扰/运维恢复明确，不提前写人工收口。
- [x] T029 运行公共 full、七目标、PG、前序回归、固定评测、console/website 检查，记录 specs/020-execution-isolation/acceptance.md；DoD：全部必验通过，覆盖率不降、无新 Go 坐标、静态发布构建通过。
- [x] T030 复跑 analyze、独立 AI 复核并逐条处置，完成七项证据至 specs/020-execution-isolation/acceptance.md、run-state.md；DoD：无未处置阻断，提交人工 review ②，禁止代签/自动提交。

## Dependencies & execution order

T001 → T002 → T003 → T004 → T005；US1 为 T006–T012；US2 为 T013–T016；US3 为 T017–T021；最后 T022–T030。取文档序首个依赖满足的未完成任务；局部环境阻断先做无依赖工作，不停等“继续”。测试与对应实现同任务交付，先建立判据再实现，关键路径补可编译变异红态。

可并行检查：配置与签名 source 检查互不依赖；web/console 的只读展示检查可在接口冻结后独立跑；真实 OCI/gVisor 使用不同临时实例。当前单一工作区顺序实现，不交错多 US；独立复核另开只读上下文。

## Requirement / check coverage

| FR | Tasks | Checks |
| --- | --- | --- |
| 01 | T006,T018,T019 | us20-base-config/app |
| 02 | T010–T012,T017,T019 | us20-base-core/app, us20-oci-cleanup |
| 03 | T013–T016,T024,T026 | us20-base-core/race, us20-gvisor |
| 04 | T009–T011,T016,T025 | us20-base-core, us20-oci-lifecycle |
| 05 | T010,T024–T027 | us20-oci-security, us20-gvisor |
| 06 | T006,T010,T015,T025,T026 | us20-base-config/core, us20-oci-security, us20-gvisor |
| 07 | T004,T011–T016,T025 | us20-base-core, test-pg, us20-oci-lifecycle |
| 08 | T004,T005,T017,T025,T026 | us20-base-app, test-pg, us20-oci-cleanup, us20-gvisor |
| 09 | T012,T014,T015,T026 | us20-base-core/race, us20-gvisor |
| 10 | T003,T022 | us20-base-core/web |
| 11 | T020,T021,T022 | us20-base-web, console |
| 12 | T002,T024–T027,T029 | 全部三 stages 与公共 full |
| 13 | T018,T026 | us20-base-app, us20-gvisor |
| 14 | T013,T016,T017,T025 | us20-base-race/app, us20-oci-cleanup |
| 15 | T007,T008,T010,T017,T023,T026 | us20-base-core/config/app, releasekit, us20-oci-cleanup, us20-gvisor |

SC001 对应 T029/T030；SC002 对应 T013/T016/T027；SC003 对应 T015/T026/T027；SC004 对应 T013/T015/T025/T027；SC005 对应 T027/T029。

## Implementation strategy

先形成受控 OCI 的可测后端，再完成生成代码与恢复读面，持续验证至真实两档和 full。阶段完成只登记证据并继续，最终仅人工 review ② 是预定停点。
