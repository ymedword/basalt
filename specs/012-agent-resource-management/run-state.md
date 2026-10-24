# US-12 运行状态（run-state）

> 本文件是**过程状态，非设计工件**——speckit 命令不得把它当设计输入。
>
> 失败台账**只追加**：一行一次尝试，不改旧行（可 diff 审计）。
>
> 唯一允许原地翻转的是「未验证清单」的「未验证 → 已验证」，翻转必须注日期与证据。

## 1. 基线（2026-10-22 建立）

| 项 | 值 |
| --- | --- |
| US | US-12 Agent 与公共资源动态管理 |
| 类型 | 开发类（台账第 39 行） |
| 分支 | `012-agent-resource-management`（步骤 1.3 手动创建；`before_specify` hook 未登记） |
| 起始 commit | `383d89eba1a74c988bd0203fec7adef96df246ed`（`fix: bound linux sampler conversions flagged by gosec G115`） |
| 开跑时未提交改动 | 无（`git status --short` 为空）；不得 stash / checkout 覆盖 |
| worktree | `D:\code\basalt`（主工作区，无并行 worktree） |
| 前序交付 | US-11 已于 `50e66cb` 交付；US-1～US-11 全部完成 |
| 模块卡 | `docs/us/us12-agent-resource-management.md`（R3.2，2,214 行；未入库，以文件哈希为权威） |
| 模块卡 SHA-256 | `c59ca1d1dd365d06789ffedab8109905de358d77d5fda486ff7790ba99a716c4`（R3.2，2026-10-22；R3.1 为 `ccca73fa762baa6f457d9ea3e2ba2a30638db141b5cb0154b6bb0f43a31acfd1`） |
| 人审依据 | 人工 review ①：R3 于 2026-10-21 通过并登记；R3.1（2026-10-22）按用户裁决修订实施输入（H1–H4 / M1–M9 / L1–L3），不重新登记、不改台账、不改契约与宪法；R3.2（2026-10-22）经用户裁决把 §4.3.2 发布回归用例改为显式撤回分支（见 §1.21） |
| 台账状态 | 已登记（REGISTRY.md 第 39 行）；本 US 不重新登记 |

### 1.1 允许触碰范围（以模块卡 §3 为准）

- 生产代码：模块卡 §3.1.1 的 28 个新增 + 20 个修改 Go 文件，落在 `internal/core`、`internal/workspace`、`internal/storage`、`internal/app`、`internal/web`、`internal/cli`、`internal/tool`；另加派生修改 `internal/app/baseline.go`（约束 44 / FR-009，见 §1.5 C1）。
- 前端：`assets/console/` 增量 13 个源文件（含 4 个测试）、4 条新路由；构建产物 `assets/console/dist` 与 `integrity.json` 随源码一致更新。
- 测试：模块卡 §3.2 的 28 个新增测试文件 / 315 条用例 + 20 个修改测试文件 / 新增 94 条用例。
- Harness：新建 `harness/stages/us12-agent-resource-management.json`（含私有 `docs/us/*` 输入，只在本机执行，不进 CI）。
- 流程工件：`specs/012-agent-resource-management/`（spec / plan / tasks / run-state 等）。
- 评测接线：`evals/` 仅在已批准接口适配所必需时触碰（`Options.ProviderClient` 注入路径）；模块卡 §3.1.1 未列其修改。

**范围外（不得改）**：`docs/contracts/**`（契约已冻结）、`.specify/memory/constitution.md`、`docs/us/**`（私有输入，只读）、已发布的 `specs/0001–011` 历史工件。零新增 Go 坐标、零迁移 SQL（复用 US-11 的 v2 七表）。

### 1.2 三件输入指纹（步骤 2–5 后补录）

| 输入 | 路径 | SHA-256 | 状态 |
| --- | --- | --- | --- |
| 颗粒度文档 | `docs/us/us12-agent-resource-management.md` | `c59ca1d1dd365d06789ffedab8109905de358d77d5fda486ff7790ba99a716c4` | 已冻结（R3.2；R3.1 基线 `ccca73fa…` 经用户裁决于 2026-10-22 修订 §4.3.2，见 §1.21） |
| `spec.md` | `specs/012-agent-resource-management/spec.md` | `6bac0de2fe63269f223bd50deddafdebd898afc50c934a02870b7a524932520e` | 已澄清（2026-10-22，368 行；用户故事 P1–P9，FR-001–FR-061，SC-001–SC-015；澄清会 5 项见 spec `## Clarifications`） |
| `plan.md` | `specs/012-agent-resource-management/plan.md` | `0f5123cdc3fb8889ea0608d0b8d0592694df80da5d85e3f6f693b8309cb0c75c` | 已生成（2026-10-22，208 行；Phase 0 research / Phase 1 data-model / quickstart 同批产出；无 contracts/ 目录；analyze 后补差异表 B-3 `internal/app/baseline.go`） |
| `tasks.md` | `specs/012-agent-resource-management/tasks.md` | `2c331ae26982959067ed71044518f14113aa503d09c9033d2870d7a25a332f39` | 已生成并冻结（2026-10-22，306 行，74 个任务；AI 比对通过；analyze 后追加 T067A–T067C 并修正 T005 / T031 / T065 / T067，见 §1.4 / §1.5） |

### 1.3 依赖存在性核对（2026-10-22 实测）

| 依赖 | 实测位置 | 结论 |
| --- | --- | --- |
| `core.MCPServerConfig` | `internal/core/mcp.go:15` | 存在，按约束 8 扩展 `Transport` / `Args` / `URL` |
| `core.ResourceKind` / `ResourceVersion` / `ResourcePointer` | `internal/core/resources.go:104/124/141` | 存在（US-11），复用 |
| `core.ResourceGate` | `internal/core/resources.go:32` | 存在（US-11），复用 |
| `core.LifecyclePort` / `LifecycleIndex` | `internal/core/lifecycle.go:59/79` | 存在（US-11），复用 |
| `core.AgentAdmin` | `internal/core/management.go:283` | 存在（US-11），保留不动 |
| `workspace.ValidateAgentName` / `CreateAgent` / `DeleteAgent` | `internal/workspace/agent_files.go:34/52/120` | 存在；后两者按 D-15 删除并由 staged / archive 取代 |
| `workspace.ParseMCPServers` | `internal/workspace/mcp.go:28` | 存在，返回类型不变，增严格 / 兼容两路径 |
| `workspace.NewSkillLibrary` | `internal/workspace/skill_version.go:68` | 存在 |
| `workspace.LifecycleStore` | `internal/workspace/lifecycle.go:68` | 存在 |
| `newProviderCatalog` | `internal/app/providers.go:42` | 存在，保留 |
| `newProviderCatalogWithOverride` | `internal/app/providers.go:69`；唯一生产调用方 `internal/app/wire.go:445` | 存在，替换并删除 |
| `agentsPort` | `internal/app/ports.go:82` | 存在；`Create` / `Delete` 目前转调将删除的旧函数，按 H4 改为锁 / DB / 组件生命周期所有者 |
| `storage.ResourceStore.VersionByID` / `ReferencedBy` / `PublishWithVersion` | `internal/storage/resource_store.go:270/312/155` | 存在，复用 |
| `finishOperationTx` | `internal/storage/management_store.go:125` | 存在，包内复用 |
| `tool.MCPPool` / `strings.Fields(spec.Command)` | `internal/tool/mcp.go:59/205` | 存在；按 H1 改为 `exec.Command(spec.Command, spec.Args...)` |
| `app.agentConsole` | `internal/app/agent_console.go:75` | 存在（US-11），保留不动 |
| `core.PublishedMCPServer` / `core.ResourceVersionDetail` | 全仓零命中 | 确认本 US 新增 |

### 1.4 任务比对、映射与冻结（步骤 5，2026-10-22）

**AI 比对结论：齐。** 逐项核对模块卡 R3.1 §3.1 / §3.2 / §3.3 与 `tasks.md`：

| 交付面 | 模块卡要求 | tasks.md 覆盖 | 结论 |
| --- | --- | --- | --- |
| 新增生产 Go 文件 | 28 | 28（T001–T067 逐文件点名） | 齐，无缺无多 |
| 修改生产 Go 文件 | 20 | 21（20 + 派生 `internal/app/baseline.go`） | 齐；模块卡清单遗漏恢复扩展文件，按约束 44 / FR-009 补，见 plan 差异表 B-3 |
| 新增测试文件 / 用例 | 28 / 315 | 28 文件，用例伴随各实现任务 | 齐；实际条数以实施后实测为准 |
| 修改测试文件 / 新增用例 | 20 / 94 | 20 文件 | 齐；同上 |
| 前端源文件 / 路由 | 13（4 views + 3 components + api-admin.ts + router.ts + 4 tests）/ 4 | 13 / 4（`workspace-browser.ts` 适配，路由 `/console/workspace` 不变） | 齐 |
| 配置与文档 | 3（`basalt.yaml.example` / `docs/Operations.md` / `docs/GettingStarted.md`） | T003 / T066 | 齐 |
| Stage | 1（`harness/stages/us12-agent-resource-management.json`） | T068 | 齐 |
| 专属 check | 18 | 18 个 ID 全部在 T065–T071 落点（stage 任务含 `012-race` 12 个并发用例） | 齐，IDs 与 §4.4 逐字一致 |
| 迁移 / 新增坐标 | 0 / 0 | 无任务触碰 | 齐 |

**任务分段（文档序）**：Phase 1 Setup T001–T007；Phase 2 Foundational T008–T017；US1 T018–T024；US2 T025–T029；US3 T030–T032；US4 T033–T038；US5 T039–T043；US6 T044–T049；US7 T050–T053；US8 T054–T059；US9 T060–T064；Polish T065–T071 及 T067A–T067C。共 74 个任务；T067A–T067C 为 analyze 后按后缀追加（保持既有编号不重排），其余顺序编号无空洞。

**测试先行核对**：每个实现任务都同文件 / 同批次带测试任务或测试步骤（harness 先行）；关键回归测试逐条点名（如 `TestYAMLBlock_ReplacePreservesCommentsAndSurroundings`、`TestResourceAdmin_PublishFailureKeepsOldPointer`、`TestAgentArchive_TimeoutRollsBack`、`TestReconcile_InvalidEditKeepsPublished`、`TestMCPSupervisor_PolicyDenied`、`TestAdminResources_StaleETag412NoSnapshot`）。

**check ID → 任务映射**：`012-core-author-agent` / `012-core-resource-types` → Phase 1–2（T001–T017 的 core 测试，含 T005 的 `TestManagementActions_*`）；`012-workspace-yaml-provider` / `012-workspace-skill` / `012-workspace-agent` → T008–T010 / T050–T052 / T018–T019 + T033–T037；`012-storage-admin` → T011 / T039；`012-app-reconcile` → T030–T032 + T067A（`TestRecoverAdmin_*` / `TestRecoverTest_*`）；其余 `012-app-*` 四个 → US1–US8 各 app 任务（T031 同时覆盖 `012-app-resource-admin` 的 run 测试）；`012-web-*` 四个 → US1/US2/US5/US6/US8 的 web 任务；`012-cli-author` → T024 / T037；`012-e2e` → T065（含 `TestUpgradeNotes_*`）；`012-race` → T067B（12 个精确用例）与 T068（stage 展开）。

**保护路径**：`docs/contracts/**`、`.specify/memory/constitution.md`、`docs/us/**`（私有只读）、`specs/0001–011/**`；判据面允许适配仅模块卡 §3.5 九项。

**Stage 路径**：`harness/stages/us12-agent-resource-management.json`（含私有 `docs/us/*` 输入，只在本机执行，不进 CI）。

**模块卡人审决定引用**：R3 于 2026-10-21 人工 review ① 通过；R3.1 于 2026-10-22 按用户裁决修订（H1–H4 / M1–M9 / L1–L3），用户明确「主要是在落实已批准范围，目前看无需修改宪法或契约」；本 US 不重新登记，沿台账第 39 行继续。

### 1.5 实施前 analyze 与处置（步骤 6 阻断检查，2026-10-22）

只读分析（spec / plan / tasks + 宪法 v2.0.0 + 模块卡 R3.1）：CRITICAL 1、HIGH 3、MEDIUM 2、LOW 2。全部在已审范围内处置：

| ID | 严重度 | 发现 | 处置 |
| --- | --- | --- | --- |
| C1 | CRITICAL | FR-009 / 约束 44 要求启动恢复覆盖新动作组；`tasks.md` 无任务，`plan.md` 修改清单缺 `internal/app/baseline.go`（现有 `verifyOperation` 默认 indeterminate） | 追加 T067A（扩展分派 + `TestRecoverAdmin_ActionMatrix` / `TestRecoverTest_UnknownNotResent`）；plan 差异表 B-3 与文件清单登记派生修改；§1.1 范围增列；T068 的 protect 增列 |
| E1 | HIGH | `012-race` 的 12 个精确并发用例只在 T068 stage 描述中出现，无编写任务 | 追加 T067B，逐字列出 12 个测试名（分配在既有新增测试文件） |
| E2 | HIGH | `012-e2e` 的 `TestUpgradeNotes_*` 无任务；T067 未写旧多词 `command` 迁移示例 | T065 增第 6 例 `TestUpgradeNotes_Documented`；T067 补迁移示例 |
| E3 | HIGH | 前端 `assets/console/dist` 与 `integrity.json` 无重建任务（FR-053） | 追加 T067C，在 CI Node job 构建并校验产物一致；本机无 Node |
| I1 | MEDIUM | T005 未含 `TestManagementActions_*` 且漏标 `012-core-author-agent` | T005 增加动作 / 目标字面量用例并标两个 check |
| I2 | MEDIUM | T031 的 `TestRunAdmission_*` / `TestRunRuntime_*` 属 `012-app-resource-admin`，标注不全 | T031 补标该 check |
| D1 | LOW | FR-015 与 FR-034 语义重复，且位于 Agent 节 | 不改；无实现歧义，记录于此 |
| A1 | LOW | 任务未逐字写测试族名 | Notes 增「测试函数名逐字遵循模块卡 §4.4 族名」 |

复跑结论：CRITICAL 0；其余发现均有处置；允许进入 implement。未改变已审范围、公共契约、权限或验收语义。

### 1.6 Stage 派生与红态基线（步骤 6.1–6.2，2026-10-22）

| 项 | 值 |
| --- | --- |
| Stage 路径 | `harness/stages/us12-agent-resource-management.json` |
| Stage ID | `us12-agent-resource-management-r1` |
| 输入 | 5（模块卡 / spec / plan / tasks / `harness/acceptance.json`） |
| 保护路径（当前） | 60——21 个修改生产文件 + 20 个既有测试文件 + 4 份契约 + `go.mod` / `go.sum` / `Makefile` / `harness/acceptance.json` + 11 个验收入口脚本 |
| 保护路径（T068 目标） | 88——增补 28 个新增测试文件；`loadStage` 对每个 protect 路径做 `os.Lstat`，未落地文件不得提前列入 |
| 专属 check | 18，ID 与模块卡 §4.4 逐字一致；12 个 race 测试名逐字 |
| task 绑定 | 49 条（覆盖 T003–T067B 中产生 check 的任务） |
| 属性 | 本机专用（inputs 含私有 `docs/us/*`）；不进 `current.json`、不进 CI |
| `required_tests` 状态 | 79 个控制侧锚点；76 个尚未实现（3 个既有：`TestDeleteAgent_ArchivesWithoutTouchingData` / `TestSchemaV2_NewDatabaseHasV2Tables` / `TestRecoverPendingOperations_TerminalRowsUntouched`）；T068 在测试全部落地后按 `go test -list` 展开为完整精确集 |

红态判据（2026-10-22 实测）：

```text
sh scripts/check-acceptance.sh --lint-stages --stage harness/stages/us12-agent-resource-management.json
→ 76 条 ERROR（本 stage，fail-closed）+ 3 条 WARN（历史 stage）；exit=1

sh scripts/check-acceptance.sh --stage harness/stages/us12-agent-resource-management.json --stage-only --task T003
→ failed 012-core-author-agent (exit=0) required test did not execute and pass: TestAuthor_PromptStable
→ report=.loop/runs/accept-20261021T211510.876664500/report.json；exit=1
```

结论：stage JSON 可加载（`Lstat` 路径检查通过），空匹配被如实判失败；红态有效，允许开始 T001。

### 1.7 T010 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | T010 冻结正文要求**删除** `workspace.CreateAgent` / `DeleteAgent`，但替代函数（`CreateAgentStaged` / `ArchiveAgentDir`）原排 T020、调用方改线原排 T036 | 提前在新文件 `internal/workspace/agent_admin.go` 落地两个替代函数（行为与旧实现等价），并把 `internal/app/ports.go` 的 `Create` / `Delete` 改为**端口层取一次实例锁**后转调（H4：CLI 路径的唯一取锁者）；未改 T020 / T036 冻结正文，后两者落地时补 DB / 状态 / 操作行编排 |
| D2 | `012-workspace-agent` 的 `-run` 正则缺 `TestLifecycle_`，其 `required_tests` 里的 `TestLifecycle_SingleWriter` 永远不会执行 | 控制侧修正 stage argv（严格超集；`TestLifecycleStore_*` 不匹配 `TestLifecycle_`），不删锚点 |
| D3 | `content_edit_test.go` 断言 `../agents/writer` 的错误文案含「路径分隔符」；M9 新增的点前缀规则先命中，文案随之改变 | 判据面适配：改用 `agents/writer` 判分隔符规则，`../` 形态只判「被拒」；断言数量增加、语义未放宽 |
| D4 | T009 行尾曾追加「（实际 14 例…）」注记，违反模块-dev 修订自检 ②（翻转须正文逐字重现） | 撤销注记；实际例数改记本文件（`012-workspace-yaml-provider` T009 命中 14 个测试函数） |
| D5 | `012-workspace-agent` 的 `required_tests` 含 `TestAgentFiles_CollectionETag`（属 T018 / T020 的受管文件集合 ETag） | T010 的 check 在 T018 / T020 落地前保持红；T010 暂不勾选，待本 check 转绿后与 T018 / T020 一并翻转 |

### 1.8 T012 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 计划用例 ③ 原写「操作行缺失 → `finishOperationTx` 报 `ErrOperationNotFound` 并回滚」；实测 `agent_archives.operation_id` 外键在 INSERT 处**先于**终态写入拒绝，错误是 FK 约束失败 | 存储实现不动（外键已给出更强的 fail-closed：行根本插不进去）；用例改为断言「报错 + 归档行零落库 + 操作行未被置终态」，保留 8 例计划数 |
| D2 | `012-storage-admin` 的 `required_tests` 含 `TestResourceStore_MarkDeleted`（T039）与 `TestNotifyAdminStore_CRUD`（T055），两者尚未实现 | T012 的 check 在 T039 / T055 落地前保持红；T012 暂不勾选，待本 check 转绿后与 T039 / T055 一并翻转（与 D5 同口径） |

### 1.9 T013 实施记录（2026-10-22）

| 项 | 值 |
| --- | --- |
| 交付 | `internal/web/etag.go`（`parseIfMatch` / `formatETag`，逐字强 ETag）+ `internal/web/etag_test.go`（9 例） |
| 落点核对 | 与模块卡 §3.1.8 一致：形状判定只在 `parseIfMatch`，handler 不重复解析；弱 / 通配 / 逗号列表 / 裸摘要 / 大小写 / 长度偏差全部拒绝 |
| 实测 | `go test ./internal/web/ -run 'TestIfMatch_' -count=1` → 9/9 passed；staticcheck（项目 checks）零命中 |
| stage-only | 红（`accept-20261021T215236.291493300`）：`012-web-etag-routes` 报缺 `TestAdminViews_ErrorDataShape`——该锚点属 T014，尚未实现；本任务自身 9 例全绿 |
| 勾选 | 暂不勾选，待同一 check 的 T014–T016 锚点落地转绿后一并翻转（与 §1.7 D5 同口径） |

### 1.10 T014 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 模块卡 §3.1.7 伪代码把 `AgentFilesView` / `SkillDetail` 等写作 `internal/app` 返回类型，但 `internal/web` **不得** import `internal/app`（依赖方向，架构三条包纪律） | 端口按「接口定义在消费方」拆分：web 能独立投影的返回 `core` 类型（Agent 文件集合 / 归档 / 版本 / 引用 / 测试结果 / 发布结果），需要 app 拼装聚合的返回 `views_admin.go` 视图类型（模型连接 / MCP / 技能详情 / 工作区）。线上响应的字段集合不变，仅接口归属按包纪律落地 |
| D2 | 契约里 `POST /skills` 与 `POST /skills/import` 的响应是 `SkillView`，模块卡 §3.7.1 写作 `SkillDetail` | 固定字面量以 `docs/contracts/openapi.yaml` 为准；T015 的这两个 handler 把 `SkillDetailView` 只投到 `SkillView` 字段集（`PUT /skills/{name}` 才返回完整详情） |
| D3 | `ErrorDataView` 用 `omitempty` 时空数组被整体省略，与「携带即输出 `[]`」冲突 | 四个白名单数组改为 `*[]string`：nil = 未携带（字段省略），非 nil 空切片 = 携带空数组（输出 `[]`） |
| D4 | `writeAdminError` 在 T014 内无调用者，staticcheck U1000 判死代码 | 白名单投影留在 `views_admin.go`；带 data 的失败信封组装移到 T015 写面 handler（`errorDataView` + `writeEnvelope`），**不**新开第二个信封出口 |
| 交付 | `ports_admin.go` / `views_admin.go` / `views_admin_test.go` | 四个端口 + 请求 DTO、视图投影与错误 data 白名单、6 例判卷 |
| 实测 | `go test ./internal/web/ -count=1` | 全包 passed；`go vet` / staticcheck（项目 checks）/ gofumpt 零命中 |
| stage-only | `accept-20261021T220130.888941700` | 红：`012-web-etag-routes` 报缺 `TestAdminRoutes_RequireAuthAndMethod`——该锚点属 T015；本任务 6 例全绿 |
| 勾选 | 暂不勾选 | 待 T015 / T016 锚点落地后随同一 check 翻转 |

### 1.11 T015 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 30 条写面路由在 T015 就要进表，但真正的 handler 分散在 T022–T061，T015 不得提前实现它们 | 表内每行先指向显式占位 `handleAdminUnassembled`（认证与 If-Match 均已生效，进入处理函数才回固定 500 INTERNAL 并记 ERROR）；handler 任务逐行替换占位，收口前占位必须清零 |
| D2 | `strconv.ParseInt` 接受 `+1`，与「非数字 / 符号一律非法」的路径参数口径不符 | `parseVersionID` 先逐字节校验 ASCII 数字，再解析并拒绝 ≤0 与溢出；非法与不存在都由 handler 统一映射 404，不用 400 区分 |
| D3 | T015 时写面路由尚未并入 `routeTable()`（T016 的计划内挂载） | 测试用 `adminTableHandler` 从同一份 `adminRoutes()` 行 + 中央认证包装搭独立 mux，与生产 `s.routes` 同构；T016 后再由 `TestAuth_RouteMatrix` 从主路由表覆盖一遍 |
| D4 | 认证必须先于 If-Match：条件写中间件若包在 `s.admin` 外层，缺凭据会先得到 412 | `requireIfMatch` 在 `adminRoutes()` 装配时套在 `adminHandler` 上，仍由 `routeHandler` 统一经 `s.admin` 包裹；`TestAdminRoutes_AuthPrecedesIfMatch` 锁定顺序 |
| 交付 | `internal/web/admin_routes.go` / `admin_routes_test.go` | 30 行写面路由表 + 条件写中间件 + `parseVersionID`；7 个测试函数（路由面、版本号解析、401 与方法兜底、条件写形状、非条件写放行、认证顺序、闸门恰一次） |
| 实测 | `go test ./internal/web/ -count=1` | 全包 passed；`go vet` / staticcheck（项目 checks）/ gofumpt 零命中；`TestAdminRoutes_*` 7/7 passed |
| stage-only | `accept-20261021T220544.953916700` | 红：`012-web-etag-routes` 报缺 `TestContractAdmin_NewErrorCodes`——该锚点属 T016；本任务 7 例全绿 |
| 勾选 | 暂不勾选 | 待 T016 锚点落地、同一 check 转绿后与 T013–T016 一并翻转 |

### 1.12 T016 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 判据面适配：`TestContract_ErrorCodes` 原按 17 个错误码对账（并显式 `delete(contract, "IMPORT_REJECTED")` 排除契约表的 400 行）；T016 落地 `IMPORT_REJECTED` 后，该排除会让新增码逃过对账 | 期望集合由 17 扩到 18：补 `codeImportRejected = "IMPORT_REJECTED"` 一行、删掉排除语句、`require.Len(produced, 18)`；契约仍逐字比对，**未删除任何断言**，属「期望集合随已批准契约扩大」的适配（依据：`docs/contracts/run-and-errors.md` §8 的 400 行与模块卡 §6.3「仅新增一个码」） |
| D2 | 装配缝隙：`web.Options` 的四个写面端口需要从运行期句柄投影，但 `heavyRuntime` 在 T016 前没有对应槽位；若在 `webOptions` 里临时构造会违反「写服务只构造一次」 | 在 `internal/app/wire.go` 的 `heavyRuntime` 增 `writeServices writeServices` 字段（类型定义在 `web.go`）；`wireWritePorts` 逐字段投影、空槽保持 nil；装配任务（T022 起）逐字段填入，不在投影处构造 |
| D3 | `TestAuth_RouteMatrix` 的行为面随写面路由并入主表而扩大：缺凭据 → 401；条件写有凭据无 `If-Match` → 412；未装配 handler → 500；断言集合（不得出现 401 / 403）不变 | 既有断言原样通过，无需改判据；新增四条 `TestConsoleAdmin_*` 从主路由表复核 30 条写面行的挂载、认证先行、If-Match 强制与未装配失败关闭 |
| 交付 | `internal/web/envelope.go`（仅 `IMPORT_REJECTED`）/ `internal/web/server.go`（Options 四端口 + `adminRoutes()` 并入 `routeTable()`，兜底仍最后一行）/ `internal/web/server_test.go`（+4）/ `internal/web/contract_test.go`（+4）/ `internal/app/web.go`（`writeServices` + `wireWritePorts`）/ `internal/app/wire.go`（`heavyRuntime.writeServices`）/ `internal/app/web_test.go`（+3） | 用例数与冻结任务逐条吻合：contract +4、server +4、app +3 |
| 实测 | `gofumpt -w`（仅本轮改动文件）；`go vet ./internal/web/ ./internal/app/` exit=0；staticcheck（项目 checks）exit=0；`go test ./internal/web/ ./internal/app/ -count=1` → 两包 passed（web 4.16s / app 14.27s） | 无失败项；`context` / `core` / `web` / `cli` 导入在既有文件内已齐备 |
| stage-only | `accept-20261021T221349.468573200` | **绿**：`012-web-etag-routes` passed（exit=0），五个 `required_tests` 全部执行并通过——本 check 自 T013 起首次转绿 |
| 勾选 | T013 / T014 / T015 / T016 一并翻转 | 四个任务的交付物与用例数已逐项核对；同一 check 的锚点已全部落地（§1.9–§1.11 的待翻转条件满足） |

### 1.13 T017 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 已发布 `definition_yaml` 存在**两种落库形态**：US-11 基线写入剥离序列指示符的条目映射原文（`baseline.go` 的 `stripEntryIndicator`），本 US 管理写路径写入块式列表片段（`BlockEntries` 的 `Raw` 含 `-` 指示符）；`workspace.ValidateProviderEntries` 对映射形态返回 0 条且不报错 | 目录构建用 `publishedProviderEntries` 兼容两种形态：先按序列形态判卷，0 条时把映射原文在内存里包成单条目列表再交给同一校验函数（字段规则只有一份）；**不改写**存储字节 / 哈希，也不改 `workspace` 的已定接口 |
| D2 | 用例名与 stage 草稿 `required_tests` 对齐 | T1 落为 `TestProviderCatalog_PublishedNamesOnly`（stage 草稿已列该精确名）；T068 仍按 `go test -list` 重展开，不提前固化 |
| D3 | `newProviderCatalogWithOverride` 删除后，`newProviderCatalog(cfg)` 仍有轻路径使用者 | 生产装配唯一入口改为 `newProviderCatalogFromPublished(ctx, store, override, logger)`（`wire.go:445`）；`newProviderCatalog` 保留给配置文件解析路径与既有测试，不新增第三个目录构造器 |
| D4 | 约束 16 的「原子替换 + 构建失败保留旧表」在本 US 之前没有持有者 | 持有者落在 `providerCatalogStore`（`atomic.Pointer`，`Load` / `Store` / `rebuild`）；`rebuild` 只在构建成功后替换，失败返回错误并保留旧表（`TestProviderCatalog_BuildFailureKeepsOld`） |
| 交付 | `internal/app/providers.go`（`newProviderCatalogFromPublished` / `publishedProviderEntry` / `publishedProviderEntries` / `resolveProviderAPIKey` / `providerCatalogStore`）/ `internal/app/wire.go`（步骤 ⑦ 调用点）/ `internal/app/providers_test.go`（+8） | 用例数与冻结任务吻合（+8）；错误文案只点名资源与变量名，不回显 base_url 或凭证值 |
| 实测 | `gofumpt -w`（仅本轮改动文件）；`go vet ./internal/app/` exit=0；staticcheck（项目 checks）exit=0；`go test ./internal/app/ -count=1` → passed（14.6s）；`-run TestProviderCatalog_` 8/8 passed | 无失败项 |
| stage-only | 红（`accept-20261021T221951.317538900`）：`012-app-resource-admin` 报缺 `TestResourceAdmin_PublishFailureKeepsOldPointer`——该 check 的余下 7 个锚点属 T025 / T031 / T040 / T042 / T051，尚未实现；本轮 8 例全绿 | 记录待兄弟任务落地后复跑；不临时删锚点 |
| 勾选 | 暂不勾选 | 与 §1.9–§1.12 同口径：同一 check 的锚点全部落地转绿后翻转 |

### 1.14 T018 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 受管文件集合只含 **Agent 根目录的普通文件**（约束 30），而提交允许带段的路径（约束 41）；子目录文件会进入完整副本与最终目录，但**不进入** `ManagedAgentFiles` 的清单，也不改变集合 ETag | 读面按约束 30 只列根目录；`SubmitAgentFiles` 返回「落盘后重新读取的**受管文件**」清单与集合 ETag。测试据此断言：子目录文件落盘可见、清单不含它、集合 ETag 不变（`TestAgentFiles_NewFileSubdirectory`），不做超出定义的推断 |
| D2 | 约束 41 的路径规则已有 `core.normalizeAuthorPath`（作者草稿解析，未导出）；`workspace` 不能 import 未导出符号，导出它又属交付物清单之外的新对外概念 | 在 `agent_draft.go` 落 `normalizeAgentFilePath`（同一组规则：分隔符归一、绝对路径 / 盘符、空段、`.` / `..`、控制字符、保留后缀；**不做** `path.Clean`），注释标明两处必须同步；用 `TestAgentFiles_PathMatrix` 判卷 |
| D3 | 集合 ETag 的规整字节未在正文给到逐字节串形；全仓既有规整规则（`agentDirManifest` / 技能清单）是 `path\0sha256\0` 串接后求 sha256 | `FileSetETag` 复用同一规整规则并渲染为 `ContentETag` 的带引号强 ETag；测试侧用 `crypto/sha256` 独立重算（`TestAgentFiles_CollectionETag`），不调用被测实现 |
| D4 | `.backup` 删除失败发生在提交**已生效**之后；若把清理失败当提交失败，会把已落盘的写入谎报为 `failed` | 记 WARN 后返回成功（新树已生效）；残留正是约束 30 的「正式目录完整 + `.backup` 在 → 前滚清理」状态，交由启动恢复处置 |
| 交付 | `internal/workspace/agent_draft.go`（`ManagedAgentFiles` / `FileSetETag` / `SubmitAgentFiles` + 内部复制 / 写入 / 路径规整）/ `internal/workspace/agent_draft_test.go`（11 个测试函数 + 子例，含约束 30 四个精确锚点与集合 ETag 锚点） | 签名与模块卡 §3.1.5 逐字一致；未新增对外概念（`core.AuthorFile` / `core.AgentFileInfo` / `core.ErrPreconditionFailed` 均为既有类型） |
| 实测 | `gofumpt -w`（仅本轮改动文件）；`go vet ./internal/workspace/` exit=0；staticcheck（项目 checks）exit=0；`go test ./internal/workspace/ -count=1` → passed（13.5s）；`-run TestAgentFiles_` 11 个测试函数全绿 | 首轮 2 个用例因「返回清单是否含子目录文件」的期望与约束 30 的定义不符而红，属用例期望修正（见失败台账），非实现缺陷；`ErrPreconditionFailed` 经错误包装可 `errors.Is` |
| stage-only | **绿**（`accept-20261021T222433.134776500`）：`012-workspace-agent` passed（exit=0），五个 `required_tests` 全部执行并通过，含本任务补上的 `TestAgentFiles_CollectionETag` | 本 check 自 T010 起的留白锚点至此清零 |
| 勾选 | T010 / T018 一并翻转 | T010 的 D5 条件（本 check 转绿）已满足；T018 交付物与用例数已核对。T020 的复制 / 恢复仍未实现，保持未勾选，待其交付物落地后单独验收 |

### 1.15 T019 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | `ValidateAuthorConfig` 的判定时点：`author:` 段缺键不阻断启动（CLI 只做语法解析），"Provider 是否已发布"只能在**调用时**判定 | 校验函数只在 `Draft` 内调用：temperature 有限性与 `[0,2]` → 成对键（单边配置包 `core.ErrProviderNotConfigured`）→ Provider 逐字命中已发布快照；两键皆空返回 nil（功能关闭），由 `Draft` 转成同一 503 分类。所有配置类失败都包哨兵，草稿内容类失败**不**包，保证 web 层能分别映射 503 / 400 |
| D2 | `author.temperature` 已校验但**无法随请求发送**：`core.ChatRequest` 无 Temperature 字段，`provider/client.go` 的 `buildRequestBody(model, msgs, tools)` 也没有透传路径；两个文件都不在本 US 批准的 20 个修改生产文件清单内 | 本任务只落地校验（非法值 400 / 503 分类生效），传输缺口登记为未验证项 U7，待后续 US 走已批准接口变更补齐；不擅自改 `core` / `provider` 的公共接口（软门禁 4） |
| D3 | 草稿 AGENT.md 的 frontmatter 解析：`workspace.parseFrontmatter` / `splitFrontmatter` 未导出，`workspace.ValidateCandidate` 需要真实根目录与完整加载规则，草稿阶段不能用 | app 侧落**窄读取面** `authorDraftFrontmatter`（只取 `name` / `provider.name,model,temperature` / `skills` / `mcp_servers`），未知键容忍（与装载期只 WARN 同口径）；它不是第二套装载器——完整规则仍在提交时由 `workspace.ValidateCandidate` 判卷。`splitDraftFrontmatter` 容忍 UTF-8 BOM、要求单独一行 `---` 闭合 |
| D4 | stage 草稿的 `required_tests` 写了全仓不存在的 `TestAuthorService_DraftWritesModelAudit`；模块卡约束 29（第 168 行）与 §4.4 验收表第 11 行（第 1892 行）点名的精确名是 `TestAuthorService_OnlyLLMCallAudit` | 控制侧把 stage 草稿锚点对齐人审模块卡的精确名（派生工件修正，不删锚点、不改验收语义；同 T003 的 `TestManagementActions_LiteralSet` 先例）；T068 仍按 `go test -list` 重展开 |
| D5 | 草稿路径规则由 `core.ParseAuthorDraft` 承担（约束 41 全套，含大小写不敏感重复拒绝）；app 只补引用与结构校验 | 不在 app 重复实现路径规则；`Draft` 的校验顺序为 配置 → 调用 → 解析 → 引用 / 结构 → `http.max_request_bytes` 总量 |
| 交付 | `internal/app/author.go`（`ValidateAuthorConfig` / `validateAuthorTemperature` / `AuthorService.Draft` / `authorProviderNames` / `checkDraftRequestLimit` / `validateDraft` / `authorDraftFrontmatter` / `splitDraftFrontmatter` / `validateDraftTemperature` / `unknownAuthorNames`）/ `internal/app/author_test.go`（10 个测试函数 + 子例） | 签名与模块卡冻结行逐字一致（`ValidateAuthorConfig(core.AuthorConfig, []workspace.ProviderEntry) error`；`Draft(ctx, name, description) ([]core.AuthorFile, error)`）；`var _ authorChatter = (*provider.ProviderService)(nil)` 锁死审计路径，装配层无第二条绕过 |
| 实测 | `gofumpt -l` 两文件零命中；`go vet ./internal/app/` exit=0；staticcheck（项目 checks）exit=0；`go test ./internal/app/ -run '^(TestAuthorService_\|TestValidateAuthorConfig_)' -count=1` → passed（2.1s） | 审计用例走真库 + 真 `ProviderService`：`llm_calls` 恰一行（`run_id=agent-author-helper`、`session_id` NULL、`round=1`、provider/model 逐字），`runs=0`、`management_operations=0` |
| stage-only | 红（`accept-20261021T223314.898531200`）：`012-app-notify-author` 报缺 `TestNotifyAdmin_ReferenceGate`；本任务 24 个测试（含子例）全部 passed，含锚点 `TestAuthorService_OnlyLLMCallAudit` | `go test -list` 实测 `TestNotifyAdmin_ReferenceGate`（T056）与 `TestWorkspaceBrowse_RejectsEscape`（T060）均不存在——同 check 的兄弟锚点未落地，非实现缺陷 |
| 勾选 | 暂不勾选 | 与 §1.7 D5 同口径：同一 check 的 T056 / T060 锚点全部落地转绿后一并翻转 |

### 1.16 T020 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 复制 / 恢复的分层：`RestoreAgentDir(root, archiveDir, target)` 签名没有「期望哈希」参数，`agentDirManifest` 也未导出 | 目录层只保证「归档目录可复算清单」（含非普通文件 / 不可读 → **在任何写入之前**拒绝）；与归档行 `content_hash` 的比对、原名 `Transition`（保留 generation）/ 异名 `Bootstrap` 都归调用方 `app.AgentAdmin.Restore`（T033）——与 T020 自身「不改状态文件、不写归档行」的边界一致 |
| D2 | 模块卡 §4.4 的族归属段写明 `TestAgentCopy_*` / `TestAgentRestore_*` 在 workspace（目录层）与 app（编排层）**各有一套同名族**，但同节 `012-workspace-agent` 的族清单漏列这两族 | 控制侧在该 check 的 `-run` 正则补上两族（只扩大执行面，五个 `required_tests` 精确锚点一字不动；与既有 `TestLifecycle_` 的纳入同口径）；T068 仍按 `go test -list` 重展开 |
| D3 | T020 行括注的「原名恢复 generation、异名 Bootstrap」属状态迁移，workspace 按约束 49 不写状态文件 | 17 例 = 既有 `TestRewriteAgentName_*` 8 例 + 新增 9 例（复制 6 / 恢复 3）；generation 与 Bootstrap 由 `012-app-agent-admin` 的 `TestAgentRestore_OriginalNameGeneration` / `TestAgentCopy_DisabledBootstrap`（T033）判卷，本层断言「生命周期状态文件一个都没写」（`requireNoLifecycleFiles`） |
| D4 | `schedules` 清空要同时覆盖块式列表与流式值；键不存在时是否补写 | 块式 → 整块替换为 `schedules: []`（保留行尾注释与行尾符）；流式 → 只换值；键不存在 → 原样返回（契约 §2 缺省即 `[]`，不凭空加键）；不重新序列化，围栏外正文与其余键逐字保留 |
| D5 | 「复制源必须是活动区 Agent（active / disabled）」与目标初始 disabled 是生命周期判定 | 不在 workspace 重复实现：`CopyAgentDir` 只认 `agents/<source>` 目录存在且是目录；状态校验与 `Lifecycle.Bootstrap` 归调用方（T033） |
| 交付 | `internal/workspace/agent_admin.go`（`CopyAgentDir` / `RestoreAgentDir` / `normalizeArchiveRelDir` / `copyAgentDirInto` / `rewriteStagedAgentDefinition` / `clearAgentSchedules`）+ `internal/workspace/agent_admin_test.go`（8 + 9 = 17 例） | 签名与模块卡 §3.1.5 逐字一致；未新增对外概念（复用 `core.AgentFileInfo` / `RewriteAgentName` / `copyAgentTree` / `managedFilesIn`） |
| 实测 | gofumpt 仅本轮两文件；`go vet ./internal/workspace/` exit=0；staticcheck（项目 checks）exit=0；`go test ./internal/workspace/ -count=1` → passed（12.861s）；复制 / 恢复 / 改名 17 个测试函数全绿 | 「符号链接使归档清单不可复算」子断言在本机跳过（无 SeCreateSymbolicLinkPrivilege，与 `TestDeleteAgent_RefusesSymlinkEntry` 同处置），登记 U8；其余断言全部执行 |
| stage-only | **绿**（`accept-20261021T223924.872272100`）：`012-workspace-agent` passed（exit=0），五个 `required_tests` 全部执行并通过；报告含 9 个新测试名（6 个 `TestAgentCopy_*` + 3 个 `TestAgentRestore_*`） | 本 check 的最后一个任务落地；D2 的正则补充使新族真正进入执行面 |
| 勾选 | 翻转 T020 | 同一 check 已无未落地锚点；T019 仍待 T056 / T060（§1.15 口径不变） |

### 1.17 T021 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | stage 草稿的锚点写作 `TestAgentAdmin_CreateDefaultsDisabled`（复数），模块卡约束 27 的复核清单（第 164 行）与 `TestAgentAdmin_CreateNoResidue*` 同节点名的是单数 `TestAgentAdmin_CreateDefaultDisabled` | 按 T003 / T019 先例以人审模块卡精确名为准：测试函数改为单数，控制侧同步把 stage `required_tests` 该一项对齐（只改名字，八个锚点的断言与验收语义不动，不删锚点）；复跑 stage-only 不再报该名，改报 T033 锚点 |
| D2 | `infoEntries` 用结构体字面量把 `core.AgentFileInfo` 转成 `core.FileEntry`，staticcheck 报 S1016 | 两类型的字段名与底层类型逐一相同，改为直接类型转换 `core.FileEntry(info)`；未用任何抑制注释 |
| D3 | 约束 27 要求「Bootstrap 之后 rename 失败 → Discard 收回状态文件」必须有可复现判据，但两次 `ensureCreateTargetFree` 与 `CreateAgentStaged` 之间没有可注入点；预置 `agents/<name>.staging` 又会让「staging 不残留」的断言失去意义 | 给 `AgentAdminDeps` 增一个注入位 `CreateStaged`（缺省 `workspace.CreateAgentStaged`，生产装配只传缺省函数），只为该回滚判据服务；它不是第二套实现入口，也不新增对外契约概念（`AgentAdminDeps` 字段本就不在模块卡冻结行内，与同处可选的 `Registry` / `Scheduler` 依赖同口径） |
| D4 | 创建成功后的响应视图要重新读取受管文件集合；读取失败时若把整个创建判为失败，会把已落地的写入谎报成失败 | 成功路径不回退：重新读取失败记 ERROR 并返回降级视图（`baseNameEntries` + 空集合 ETag），操作终态仍为 success；提交路径不复用该降级——提交返回的清单来自落盘后的真实重读 |
| D5 | 412 必须在写操作行之前判出（约束 1 / 3），而 `workspace.SubmitAgentFiles` 内的 `strongETagMatches` 在预写之后才执行 | `SubmitFiles` 在锁内重读后用字面比较 `ifMatch != currentETag` 先判 412（缺失 / 弱 / `*` / 不匹配一律不等），预写前返回零操作行；workspace 侧的比较保留为第二道闸（覆盖预写与落盘之间的窗口），两层判定对强 ETag 等值语义一致 |
| 交付 | `internal/app/agent_admin.go`（`AgentAdmin` 的 `Create` / `GenerateFiles` / `Files` / `SubmitFiles` + 视图与详情类型 + `CreateStaged` 注入位）/ `internal/app/agent_admin_test.go`（21 个测试函数：创建 8、模板 1、读面 2、提交 7、草稿 3；归档 / 复制 / 恢复 4 例留给 T033 的 25 例总额） | 方法签名与模块卡 §3.1.7 冻结行逐字一致；先写 pending 再动目标、纯请求校验在预写之前、Agent 写锁经注入的共享 `*core.AgentGate` |
| 实测 | gofumpt 仅本轮三文件；`go vet ./internal/...` exit=0；staticcheck（项目 checks，`./internal/app` + `./internal/workspace`）exit=0；`go test ./internal/app/ ./internal/workspace/ -count=1` → passed（15.4s / 12.8s）；本轮 21 例 `-v` 全 PASS | 失败分类经真实返回值断言：预写失败 → 「拒绝执行」且零目录 / 零状态文件 / 零操作行；Bootstrap 后失败 → 状态文件经 `Discard` 收回；索引失败 → success + `index_write=deferred` + ERROR 日志；412 四种形态 → 零操作行零写入；草稿路径 → 零操作行零落盘 |
| 红态 | `-overlay` 逆改（探针副本建在仓库外，用毕即删）：① `Create` 吞掉预写失败 → `TestAgentAdmin_CreateNoResidueOnPreWriteFailure` 报 `An error is expected but got nil.`（agent_admin_test.go:289）；② 跳过锁内 ETag 比较 → `TestAgentAdmin_SubmitStaleETagNoOperation` 报 `Should be empty, but was [...]`（`Messages: 412 不得写操作行`，agent_admin_test.go:497） | 两处逆改均按预期变红且文案可辨识；移除覆盖层后复跑全绿，判别力成立 |
| stage-only | **红**（`accept-20261021T230354.100695700`）：`012-app-agent-admin` 报缺 `TestAgentArchive_TimeoutRollsBack`；本 check 的 `TestAgentAdmin_CreateDefaultDisabled` / `TestAgentAdmin_SubmitCollectionETag` / `TestAgentTemplate_DeterministicProvider` 三个锚点已执行并通过 | 同 check 其余锚点属 T031 / T033 / T035（`TestAgentCopy_DisabledBootstrap`、`TestAgentRestore_OriginalNameGeneration`、`TestAgentLoad_PublishedOnly`、`TestStartupOrder_ReconcileBeforeRegistry`）——组检查未齐，非实现缺陷 |
| 勾选 | 暂不勾选 | 与 §1.7 / §1.15 同口径：同一 check 的 T031 / T033 / T035 锚点全部落地转绿后一并翻转 |

### 1.18 T022 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 模块卡交付物清单没有「写面错误映射」的共享文件；US-11 的只读 handler 各自持有按类型分支的映射函数 | 在 `handlers_admin_agents.go` 内放 `writeAgentAdminError`（本任务可触达的分类：404 / 400 / 503 / 412 / 409 busy / 409 conflict / 504 / 500）；导入拒绝等资源写面分类留给 `handlers_admin_resources.go` 任务各自落地——不为「复用」新建模块卡外的共享文件 |
| D2 | 契约把 `description`（generate-files）与 `files`（多文件提交）列为 required，但 `encoding/json` 不提供「键是否存在」的元数据 | handler 判定：空描述 → 400 并点名 `description`；`files == nil`（缺键 / `null`）→ 400 并点名 `files`；`files: []` 放行给适配器，由适配器按「缺 AGENT.md」拒绝——本层不做内容级判断 |
| D3 | 契约只在 `GET` / `POST /agents/{name}/files` 定义 `ETag` 响应头；创建、草稿、复制都没有该响应头 | 只有两个 files 端点设置 `ETag` 响应头（与 `data.etag` 逐字一致）；创建与复制把集合 ETag 只放进 `data.etag`，不新增契约外响应头 |
| D4 | 失败信封的 `data` 只经 `errorDataView` 四键白名单投影，没有 `operation_id` 载体；模块卡 §3.2.3 的「可查的操作标识」在 US-11 已定为经 `GET /operations` 查证 | 失败响应按 US-11 口径投影（无白名单键时省略 `data`）；「412 零操作行」的判据落在适配器层（T021 的 `TestAgentAdmin_SubmitStaleETagNoOperation`），web 层判的是 412 映射、恰一次端口调用、无 data |
| D5 | 端口未装配时若返回空结果，会把装配错误伪装成成功 | `writeAdminPortMissing` 统一记 ERROR 并回固定 500 `INTERNAL`，与 US-11 只读面的缺端口口径一致；五个 handler 各自在解码前检查 |
| 交付 | 新建 `internal/web/handlers_admin_agents.go`（五个 handler + 固定文案 + `decodeAdminWriteBody` + 类型分支错误映射）；新建 `internal/web/handlers_admin_agents_test.go`（12 个测试函数：`CreateZeroResidue` / `CreateRejectsUnknownField` / `GenerateDraft` / `GenerateNotConfigured` / `GetFilesSetETag` / `GetFilesNotFound` / `SubmitProjectsFreshView` / `SubmitStaleETagNoOperation` / `SubmitRejectsMissingIfMatch` / `SubmitRequiresFiles` / `Copy` / `MissingPort`）；修改 `internal/web/admin_routes.go`（五行占位替换为真实 handler，其余 25 行保持 `handleAdminUnassembled`） | handler 只做「解码 → If-Match 形状 → 委托 → 投影」；`If-Match` 原样转发，比较留在适配器写锁内（约束 28） |
| 实测 | gofumpt 仅本轮三文件；`go vet ./internal/...` exit=0；staticcheck（项目 checks，`./internal/web`）exit=0；`go test ./internal/web/ -count=1` passed（4.196s）；本轮 12 个测试函数 `-v` 全 PASS | 解码路径同时校验请求体上限（`MaxBytesReader`）、未知字段（`DisallowUnknownFields`）与尾随内容 |
| 红态 | `-overlay` 逆改（探针副本建在仓库外，用毕即删）：① 去掉 GET files 的 `ETag` 响应头 → `TestAdminAgents_GetFilesSetETag` 报 `expected: "sha256:bbbb…"` / `actual: ""`（handlers_admin_agents_test.go:344）；② 把过期提交分支映射为 500 → `TestAdminAgents_SubmitStaleETagNoOperation` 报 `expected: 412` / `actual: 500`（handlers_admin_agents_test.go:426） | 两处逆改均按预期变红；移除覆盖层后复跑全绿（② 的首次探针因 Python 占位 shim 未执行变异而假绿，见 §2 同题记录） |
| stage-only | **红**（`accept-20261021T230930.828569100`）：`012-web-admin-agents` 报 `required test did not execute and pass: TestArchivedAgents_List`；本 check 的 12 个 `TestAdminAgents_*` 全部执行并通过 | 另一锚点属 T034（归档列表），组检查未齐，非实现缺陷 |
| 勾选 | 暂不勾选 | 与 §1.7 / §1.15 / §1.17 同口径：同 check 的 T034 锚点落地转绿后一并翻转 |

### 1.19 T023 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 模块卡 §3.1.2（第 1122 行）要求作者提示词携带「既有模板正文」，但 `agentTemplate` / `agentTemplateBody` 均为 `workspace` 包内非导出函数，交付物清单里也没有模板访问器 | 在 `internal/workspace/templates.go`（模块卡第 5 行改造点）新增 `AgentTemplateSkeleton()`：一行包装 `agentTemplate`，**不复制**模板文本（单一来源）。仅 internal 包内导出，不新增 REST / 配置键 / 数据表 / frontmatter 字段等对外概念，未改任何已定字面量或冻结签名 |
| D2 | 骨架若写入装配时刻的 Provider 名，就缓存了一份会过期的发布列表（`AgentAdminDeps` 注释明确「不缓存过期列表」） | 骨架以 `providers=nil` 渲染：名字写中性 `<name>`，provider / model 写可见占位符；已发布名单由 `core.AuthorReferences` 按**每次调用**的当前快照另行渲染。`TestWire_AgentAdminSharesAssemblyInstances` 断言骨架含占位符且不含装配时刻的 `probe` |
| D3 | `PublishedProviders` 的取数路径未在模块卡里点名 | 复用运行目录的同一解析器：`ListPointers(ResourceModelConnection)` + `publishedProviderEntry`（归属 / 形态 / 名字一致性只此一份），**不构造客户端、不解析环境变量**；重复条目名失败关闭（与 catalog 同口径） |
| D4 | `PublishedSkills` 需要稳定顺序与撤回复核 | 复用注册装配的 `publishedSkillNames`（自己复核 `DeletedAt`），取名后升序排序——提示词按传入顺序渲染，顺序稳定才可判卷 |
| D5 | 「已应用的 MCP」不等于「applied == published」 | 按 FR20 与 `runResources.resolveMCP` 的同一判据：`AppliedVersionID != nil` **且**在本进程已连接。待重启（applied 为旧版本）的 server 仍可被运行解析，因此**必须**留在白名单；不进名单的 server 随后由 `validateDraft` 按同一份名单拒绝 |
| D6 | 提交前的加载期校验必须与装载 / 热加载共用同一道规则 | `ValidateDefinition` 调 `loader.ValidateCandidate(root, agents.Entries(), …)`：调度 id 冲突基线取**当前**注册表快照（与 US-11 管理端口同一写法），不读装配时刻的 `entries` |
| D7 | `web.AgentAdminService` 要求 8 个方法，`*AgentAdmin` 目前只有 Create / Files / SubmitFiles / GenerateFiles；Copy / Archive / Restore / Archived 属 T033 | 端口适配器**不在本任务**构造：`writeServices.agentAdmin` 保持 nil，handler 按缺端口固定 500 失败关闭（web.go 槽位规则「空槽保持 nil，不得换成空实现冒充可用」）。T023 因此**暂不勾选**，与 T033 一并翻转；这是已审范围内的派生排序决策，不改范围、权限或验收语义 |
| 交付 | 修改 `internal/app/wire.go`（新增三个作者白名单快照函数、在装配末尾构造 `AgentAdmin`、`heavyRuntime` 增 `agentAdmin` 句柄）；修改 `internal/workspace/templates.go`（`AgentTemplateSkeleton`）；`internal/app/wire_test.go` +1 例 `TestWire_AgentAdminSharesAssemblyInstances` | 锁表 / 生命周期存储 / 资源仓储 / 管理操作仓储全部传共享实例；`Options.ProviderClient` 评测注入路径一字未动 |
| 实测 | gofumpt（仅本轮三文件）exit=0；`go vet ./internal/app/ ./internal/workspace/` exit=0；staticcheck（项目 checks）exit=0；`go test ./internal/app/ ./internal/workspace/ -count=1` 均 ok（15.036s / 13.371s）；新例 `-v` PASS | 全量 app 包回归覆盖既有的 wire / 准入 / 基线用例 |
| 红态 | `-overlay` 逆改（探针副本建在仓库外，`perl -0pi -e` 施加后 grep 核对生效，用毕即删）：把 `AgentAdminDeps.Gate` 改成 `nil`（退化为私有锁表）→ `require.Same` 在 `wire_test.go:1046` 报 `Not same`，expected / actual 是两个不同的 `*core.AgentGate` 地址，消息「写面必须与准入共用同一把 Agent 级锁表（私有锁表会让互斥静默消失）」；移除覆盖层复跑 PASS | 判别力成立：该例能识破"另建一份锁表"的装配错误 |
| stage-only | 未跑：本任务在 stage 中没有 `tasks` 绑定（无专属 check），其行为由 T033 / T035 的 `012-app-agent-admin` 与 T034 的 `012-web-admin-agents` 覆盖 | 与 tasks.md 一致；不为装配任务临时新增判据面 |

### 1.20 T024 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | `api-admin.ts` 必须复用唯一 HTTP 出口（约束 45：凭据注入 / 信封解包 / 401 策略只在 `api.ts`），但 `ApiClient` 只有私有 `#request` / `#call`，`api-admin.ts` 无法调用 | 在 `api.ts` 增加三个公开出口 `request` / `requestWithEtag` / `requestOK`（后两个分别暴露响应头 ETag 与"无 data 的成功"）。`api.ts` 不在 §3.1.10 的 13 文件前端增量内，属已审范围内的派生缝隙；未新增 REST 路径 / 配置键 / 数据表 / frontmatter 字段，既有端点行为不变（原有测试只断言 status / code） |
| D2 | 图 H 要求 400 展示 `problems`，但 `ApiError` 只保留 status / code / message / Retry-After，失败信封的 `data` 被丢弃 | 增加 `ApiErrorData`（契约已定义的四个字符串数组白名单）、`ApiError.data`（构造器第 5 参默认 `null`）、模块内 `errorData()` 四键投影与 `api-admin.ts` 的 `problemsOf()`。服务端对应物是 `web.errorDataView` 同一白名单；不新增错误码、不改失败语义 |
| D3 | `AgentFilesView.lifecycle` 只保证 `state` / `valid` / `lifecycle_error`；`agentFilesView` 有意把 `generation` / `updated_at` / `operation_id` 留零（openapi.yaml:2019 契约注），不能用来拼启停迁移 | `agents-new` 的启用步骤改读 `GET /profiles`，用 `ProfileView.lifecycle` 的完整 `generation` 发 `expected_state: 'disabled'` + `expected_generation`。只增加一次只读 GET，不换状态写路径（仍走 US-11 的 enable 端点） |
| D4 | 图 H 写「创建 200 → 进入 /console/agents/:name」，但草稿只在页面内存、未落盘；直接跳转会让刚生成的草稿无法提交，且 T024 冻结正文要求 `agents-new` 包含「创建 + 草稿编辑 + 提交」 | 创建成功后停留在本页：展示创建响应（含集合 ETag）、就地提交草稿、就地启用，并提供「打开 Agent 页面」链接进入 `/console/agents/:name`（后续编辑由该页的 `agent-files` 组件承担）。属已审范围内的交互排序决策，不改权限、契约或验收语义 |
| D5 | 412 / 409 / 成功三档的页面策略未逐字写死 | 412：保留用户编辑正文，仅把基线原文与集合 ETag 前移，并入服务端新增文件；服务端已删除的文件保留在编辑器，等待显式重建。409：只展示服务端固定文案，不重读、不移动基线、不重试。成功：清单与每份正文全部重新 GET（`agent-files`）；`agents-new` 按图 H「用返回的文件列表刷新」消费 POST 返回的服务端真值。正文读取失败（超限 / 二进制 / 消失）的文件标记为不可提交，**不以空正文回写** |
| D6 | 本机 Node / npm 均不可用（2026-10-22 实测：`node: command not found` / `npm: command not found`） | Vitest / `vue-tsc` / `vite build` 本机**未执行**；前端判据由 CI 前端 job（`npm ci` → `npm test` → `npm run build` + dist / integrity 对账）承担。未验证清单 U6 保持「未验证」，不宣称前端绿态 |
| 交付 | 新增 `assets/console/src/api-admin.ts`（写面端点、显式 `ifMatch`、`problemsOf`）、`assets/console/src/views/agents-new.ts`（描述 → 草稿 → 创建 → 提交 → 启用）、`assets/console/src/components/agent-files.ts`（多文件编辑与提交、写后重读、412 基线前移、409 不动基线）、`assets/console/tests/agent-files.test.ts`（4 例）；修改 `assets/console/src/api.ts`（D1 / D2 的四个增量扩展） | 全部只使用 US-11 已交付的 `api.ts` / `router.ts` / `render.ts` / 静态壳；未新增前端依赖、未新增构建产物类型；页面无危险 DOM、token 仍在内存 |
| 实测 | 本机不可执行：无 Node / npm，故 Vitest 与类型检查**未跑**。仅做静态自查：导出符号与 `shared.ts` / `ui.ts` / `api.ts` 逐项对照存在；`AdminApi` 每方法对应契约端点；视图与组件不直接调用 `fetch` | 如实登记为未验证；不以静态自查替代判卷 |
| 红态 | 未执行（无 Node；前端在 stage 中无专属 check） | 待 CI 前端 job 首次运行给出红 / 绿读数 |
| stage-only | 不适用：stage 的 `tasks` 表无 T024 绑定（前端判据不在 Go stage 内） | 与 tasks.md / stage 一致；不为前端任务临时新增 Go 判据面 |
| 勾选 | 暂不勾选 | 未全绿不得宣称完成；待 CI 前端 job 通过（含 4 个新测试）后由控制侧翻转，并按需把 U6 翻为已验证（注日期与 CI 证据） |

### 1.21 T025 前置裁决与模块卡 R3.2 修订（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| R1 | §4.3.2 关键回归 `TestResourceAdmin_PublishFailureKeepsOldPointer` 原稿在 v1 已发布后**不带版本号**调 `Publish`，并注入 `failNextVersionInsert` 期望失败；按约束 9 / 图 B / FR-024，不带版本号的 publish 在已发布时是**幂等成功、不插版本**，注入的插入失败没有插入可作用 | 判据面变更 → 停下提交用户裁决（AskUserQuestion，2026-10-22）。用户裁决：**修订用例为显式撤回** |
| R2 | 裁决落地 | 模块卡 §4.3.2 的 Publish 请求补 `VersionID: v1.ID`（走「带旧版本 → INSERT 新版本 → 指针 → 终态」），断言逐字保留；模块卡头部升 R3.2 并在文末追加 R3.2 裁决节；`harness/stages/us12-agent-resource-management.json` 的 `approval` 注记更新为新哈希 `c59ca1d1…` / 2,214 行；本文件基线与本表同步。错误语义、传输范围、权限边界、台账、契约、宪法均不变 |
| R3 | 存储层先决 | `ResourceStore.PublishWithVersion` 改为「总是 INSERT 新版本 + 指针 + `finishOperationTx` 同事务」；`EnsureVersion` 保持哈希幂等（基线 / 对账用）。`TestResourceStore_*` 全绿（`ok … 1.525s`）。顺序 INSERT → 指针 → 终态，使既有原子性用例仍在「更新发布指针」处失败并回滚版本行 |
| R4 | 测试注入点 | 存储层无 DB 故障钩子，关库又会破坏后续 `Pointer` 读；`ResourceAdminDeps` 增加可注入的 `PublishVersion func(ctx, v, operationID) (v, error)`，默认 `store.PublishWithVersion`，测试环境一次性注入 `disk full`（先例：`AgentAdminDeps.CreateStaged`） |

### 1.22 T025 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 模块卡用例写作 `core.Actor{Subject: "test"}`，但 `management_operations.auth_method` 有 CHECK 约束（bearer / local_trust / cli / migration），缺方法值的预写直接报 `CHECK constraint failed` | 夹具增加 `resourceActor()`：主体名保持卡片的 `"test"`（`actor_subject` 无约束），补 `Method: core.AuthMethodLocalTrust` 与回环 `Source`。属夹具适配，断言与判据面不变 |
| D2 | 卡片 R3.2 修订句写作 `VersionID: v1.ID`，而 `core.ResourcePublishRequest.VersionID` 是 `*int64` | 落为 `VersionID: &v1.ID`（R2 的语义不变：走「带旧版本 → INSERT 新版本 → 指针 → 终态」），断言逐字保留 |
| D3 | `TestModelConnection_TestTimeout504` 的处理函数只等 `r.Context().Done()`，在 Windows 上 `httptest.Server.Close` 会一直等处理函数返回（实测挂起 90s 后 panic） | 处理函数改为 `select`：优先等请求上下文，另有 `release` 通道由 `t.Cleanup` 在 `srv.Close` 之前关闭。判据（504 分类 + 操作 failed + `TIMEOUT`）不变 |
| D4 | 原样落地的关键回归带 `t.Parallel()`，与 `t.Setenv` 互斥；但模型连接发布要在**预写之前**解析 `${VAR}`，正常路径必须有一个已设置的变量 | `internal/app` 增加包级 `TestMain`：`os.Setenv(BASALT_RESOURCE_TEST_KEY, "resource-test-sentinel")`，设置失败退 1。缺变量用例用另一个未设置的变量名（并行例）或串行例里的 `t.Setenv(key, "")`——Go 只在全部串行例结束后恢复平行例，二者不重叠 |
| D5 | 重名创建按失败判据表落为「操作 failed + 409」：`BeginOperation` 已写 pending，随后判重并 `finishOperationTx(failed, CONFLICT)`，响应 `data.operation_id` 可查（实测 1 条 failed 行） | 与约束 1a 一致：重名不是纯请求校验（要读资源指针），故在预写之后。`blockedByPlan` 一类的纯参数校验才在预写之前 |
| D6 | 发布不带版本号且最新版本已是发布指针 → 幂等成功、不新增版本、文件不动，但仍留一条 success 操作行（实测 `TestPublish_Idempotent`） | 约束 9 / FR-009 的幂等分支；「每个写操作恰一行台账」不允许因为幂等而省行 |
| D7 | 显式撤回把**来源版本的 `definition_yaml` 字节**原样存进新版本（`content_hash` 等于来源哈希），再把定义文件改写为该字节；历史版本行不改写（实测 v3 的 `definition_yaml` 与 v1 逐字相等、v1 行仍为 version_no=1） | 约束 9 / 约束 10 的「撤回 = 用旧内容造新版本」；不做「重新渲染后写回」，避免重新序列化改变哈希 |
| D8 | 补写文件后重读校验的失败点全部落在预写之前：环境变量缺失 / 候选校验失败 / 文件分歧 / 412 都是零操作行（实测 `TestModelConnection_MissingEnvZeroWrite` / `TestResourceWrite_FileDivergedConflict` / `TestModelConnection_StaleETagZeroWrite`） | 约束 1a 的 read-only 前置段；只有「重名」需要读指针而落在预写之后（D5） |
| D9 | 提交段失败后的补偿：文件身份检查通过 → 恢复原字节 + 操作 failed + 响应 `operation_id` 可查（实测 `TestResourceAdmin_DBFailureRestoresFile`）；身份不通过才置 unknown | 约束 36；测试用注入的 `PublishVersion` 在事务内失败，不伪造第二套实现 |
| D10 | 模型测试的外部失败（连接 / 非 2xx）记 **success + `ok=false`**，只有超时记 failed + `TIMEOUT`、凭证缺失记 failed + `NOT_CONFIGURED`（实测三例） | 约束 50：操作记录的是「测试是否执行完毕」，结论由 `data.ok` 表达 |
| D11 | `TestModelConnection_TestWritesOperation` 在模块卡的测试族枚举里没有，但 stage 的 `012-app-resource-admin` 以它为锚点 | 按 stage 锚点在 `TestModelConnection_*` 族内交付该名（族归属与模块卡一致），未新增判据面 |
| D12 | stage 锚点 `TestResourceRecreate_VersionContinues` 与模块卡约束 24 的 `TestResourceRecreate_VersionNoContinues` 名称不一致 | 以模块卡精确名为准（同 T003 / T019 / T021 先例），由 T042 落地时一并把 stage 锚点对齐；本轮不删锚点、不改语义 |
| D13 | `editUnsupportedError` 在本文件覆盖率为 0%：模型连接写路径只有在块结构不可安全编辑（`BlockEntries` 失败 / `replaceLost`）时才走到它 | 属正常——该分支的触发条件需要构造不可编辑的既有块，留给 T044–T051（MCP / 技能写面）与 workspace 既有用例覆盖；登记为观察项，不临时造用例 |
| 交付 | 新增 `internal/app/resource_admin.go`（模型连接 `Upsert` / `Publish` / `TestModelConnection` 与提交段 / 补偿 / 分歧闸门 / 目录替换）；新增 `internal/app/resource_admin_test.go`（18 个测试函数，删 / 引用 / 版本详情在三组兄弟任务补足） | 只实现 §3.1.7 点名的对外概念；未新增 REST 路径 / 配置键 / 数据表 / frontmatter 字段；未新增 Go 坐标 |
| 实测 | gofumpt（仅本轮两文件）exit=0；`go vet ./internal/app/` exit=0；staticcheck（项目 checks）exit=0；定向 `go test -run '^(TestResourceAdmin_\|TestPublish_\|TestModelConnection_\|TestResourceWrite_)' ./internal/app/` → `ok … 2.115s`（18/18 PASS）；`-race` 同集 → `ok … 3.095s`；全包 `go test ./internal/app/` → `ok … 14.268s`（TestMain 不影响既有用例）；覆盖率全包 80.2%，`resource_admin.go` 内 `Publish 68.3%` / `UpsertModelConnection 75.8%` / `TestModelConnection 83.6%` / `commitModelConnection 86.7%` | 全绿读数当场取得；覆盖率供 T069 `make cover` 复核 |
| 红态 | `-overlay` 逆改两例（探针副本建在仓库外，`perl -0pi -e` 施加后 grep 核对生效，用毕即删）：① 关掉更新路径的分歧闸门 → `TestResourceWrite_FileDivergedConflict` 在 `resource_admin_test.go:622` 报 `Expected error with "conflict" in chain but got nil.`；② 提交段失败后不恢复文件 → `TestResourceAdmin_DBFailureRestoresFile` 报 `Not equal`，expected 为 `https://a.example` / 实际 `https://b.example`，消息「提交段失败必须把定义文件恢复为原字节」 | 判别力成立：两例分别识破「手工改动被静默覆盖」与「失败不回滚文件」 |
| stage-only | `failed 012-app-resource-admin (exit=0) required test did not execute and pass: TestResourceVersionDetail_Projection`（报告 `accept-20261022T003305.954509100`） | 非实现缺陷——该 check 的 9 个锚点里本任务交付 `TestResourceAdmin_PublishFailureKeepsOldPointer` / `TestModelConnection_TestWritesOperation` 两个，余下属 T031 / T034 / T040 / T042 / T051。记录待兄弟任务落地后复跑；不临时删锚点 |
| 勾选 | 暂不勾选 | 未全绿不得宣称完成：`012-app-resource-admin` 仍红在兄弟任务锚点上，勾选在整组 check 转绿后由控制侧翻转 |

### 1.23 T026 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 卡片原样回归 `TestAdminResources_StaleETag412NoSnapshot` 用 `httptest.NewRequest` 构造请求（默认 Host `example.com`，无端口），而真实认证闸门要求 `allowed_hosts` 每项带端口且 `normalizeHost` 不补默认端口——请求在认证层即 403，到不了 If-Match | 夹具改用透传 `resourceAuth` 替身注入固定 actor（与 `server_test.go` 的 `countingAuth` 同法）；认证矩阵由 `admin_routes_test.go` / `server_test.go` 判卷，本文件只判写语义，卡片用例正文逐字未改。中途把 `example.com` 加进 AllowedHosts 的尝试在装配期 panic（`allowed_hosts 第 4 项必须写成 host:port`），已放弃 |
| D2 | 约束 3 / run-and-errors §508 / `core/errors.go:242` 要求 412 的 `data.current_etag`，适配器已提供，但 `ErrorDataView` 白名单此前只有四个字符串数组——该键被静默丢弃 | 扩 `ErrorDataView` 增 `CurrentETag *string`（空串视为未携带）与 `errorDataView` 的 `current_etag` 分支；未知形状仍按"没有该键"丢弃。`TestAdminViews_ErrorDataShape` 不受影响（未知名丢弃语义不变）。属已审范围内的派生补齐，不改判据面 |
| D3 | `editUnsupportedMessage` 的拼装逻辑原本内联在 `writeAgentContentError`，资源写面（块编辑）需要同一出口 | 原样抽成 `handlers_agent_content.go` 的包级函数，两处共用；对外文案逐字不变（「原因；请改用整份原文保存」/ 无原因时用 `messageEditUnsupported`） |
| D4 | 版本列表端点同时出现在 T026 与 T041 的任务文字里（T041 写「版本列表」） | 按 T026 的显式点名（模型连接端点与版本 / 发布端点）本轮落地；T041 只做删除 / 引用，不重复实现版本列表 |
| D5 | 卡片写「测试文件共 19 例，其余在 T041 / T042 补全」 | 本轮落地 13 例（含三锚点）；删除 / 引用归 T041，版本详情归 T042 |
| 交付 | 新增 `internal/web/handlers_admin_resources.go` 与 `handlers_admin_resources_test.go`（13 例）；修改 `admin_routes.go`（五条路由换真 handler）、`views_admin.go`（`current_etag` 白名单）、`handlers_agent_content.go`（抽出 `editUnsupportedMessage`）、`handlers_admin_agents.go`（仅注释口径） | 只实现 §3.1.8 点名的端点；未新增契约外路径 / 配置键 / Go 坐标；错误码全部复用既有字面量 |
| 实测 | gofumpt（本轮六文件）exit=0；`go vet ./internal/web/` exit=0；staticcheck（项目 checks）exit=0；定向 `go test -run '^(TestAdminResources_\|TestModelConnection_\|TestAdminResourceVersions_\|TestAdminResourcePublish_)' ./internal/web` → `ok … 1.298s`（13/13 PASS）；全包 `go test ./internal/web` → `ok … 4.121s`；`go build ./...` exit=0 | 全绿读数当场取得 |
| 红态 | `-overlay` 逆改两例（探针副本建在仓库外，`perl -0pi -e` 施加后 grep 核对生效，用毕即删）：① 五条路由退回 `handleAdminUnassembled` → `TestAdminResources_StaleETag412NoSnapshot` 在 `handlers_admin_resources_test.go:389` 报 `status = 500, want 412`；② 去掉 `current_etag` 白名单分支 → `TestModelConnection_StaleETag412` 报 `unexpected end of JSON input`（响应 data 为空） | 判别力成立：分别识破「路由未接真 handler」与「412 不带当前 ETag」 |
| stage-only | `failed 012-web-admin-resources (exit=0) required test did not execute and pass: TestSkillValidate_NoSideEffects`（报告 `accept-20261022T004225.583689700`）；passed 列表含本任务三锚点与 `TestAdminResources_MissingPort500` 的五个子例 | 非实现缺陷——该 check 余下锚点 `TestSkillValidate_NoSideEffects` / `TestMCPViews_Redacted` / `TestMCP_StaleETag412` 属 T048 / T052；记录待兄弟任务落地后复跑，不临时删锚点 |
| 勾选 | 暂不勾选 | 未全绿不得宣称完成：`012-web-admin-resources` 仍红在兄弟任务锚点上，勾选在整组 check 转绿后由控制侧翻转 |

### 1.24 T027 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | `web.ResourceAdminService` 要求 13 个方法，`*ResourceAdmin` 目前只有 `UpsertModelConnection` / `Publish` / `TestModelConnection` 三个（且前两个的签名与端口形状不同：端口收线上 `ModelConnectionWrite` 并返回 `ModelConnectionView`）；其余方法属 T040–T053 | 端口适配器**不在本任务**构造：`writeServices.resourceAdmin` 保持 nil，handler 按缺端口固定 500 失败关闭（web.go 槽位规则「空槽保持 nil，不得换成空实现冒充可用」）。沿用 §1.19 D7 先例，T027 **暂不勾选**，待方法集补齐、整组 check 转绿后由控制侧翻转 |
| D2 | `providerCatalogStore` 在 T017 落地后仍**没有生产调用方**（`newProviderCatalogStore` 零引用），约束 16 的「原子持有者」在装配期不存在 | 在步骤 ⑦ 以 `newProviderCatalogFromPublished` 的首次构建结果初始化持有者，并把**同一实例**交给 `ResourceAdmin`；`heavyRuntime` 增 `catalogStore` 句柄。发布路径 `s.catalog.Store(s.catalog.Load().with(...))` 由此有了真实的共享目标 |
| D3 | 本任务完成后、T032 之前，`modelClient` 与 `NewRunResources` 仍持有装配时刻的目录快照；发布只更新持有者，新 Run 尚看不到新映射 | 已登记的**任务切分过渡态**，不是可交付语义：T032 把准入 / 路由读取点接到 `Load()`。装配注释与 heavyRuntime 字段注释都写明这一点，避免后续误读为设计完成 |
| D4 | 任务文字点名 `internal/app/web.go`，但写面端口的投影边已由 T016 落地（`wireWritePorts` 逐字段投影 `rt.writeServices.*`）；本任务无需改动该文件 | 保留 `web.go` 原样；槽位为 nil 时投影结果即 nil，`TestWritePorts_FailClosedWithoutAdapters` / `TestWritePorts_ProjectedIdentically` / `TestWritePorts_IndependentOfContentEditor` 继续判卷。属已审范围内的派生排序判断，不改范围、权限或验收语义 |
| 交付 | 修改 `internal/app/wire.go`（步骤 ⑦ 新增 `catalogStore`；步骤 ⑧ 构造 `NewResourceAdmin`；`heavyRuntime` 增 `resourceAdmin` / `catalogStore` 两个句柄并写入返回字面量）；修改 `internal/app/wire_test.go`（+1 例 `TestWire_ResourceAdminSharesAssemblyInstances`） | 版本 / 操作仓储、发布闸门、目录持有者全部传共享实例；定义文件路径取 `filepath.Join(a.workDir, configFileName)`；未新增 REST 路径 / 配置键 / 数据表 / Go 坐标 |
| 实测 | gofumpt（本轮两文件）exit=0；`go vet ./internal/app/` exit=0；staticcheck（项目 checks）exit=0；定向 `go test -run '^(TestWire_ResourceAdminSharesAssemblyInstances\|TestWire_AgentAdminSharesAssemblyInstances\|TestWritePorts)' ./internal/app/` → `ok … 1.908s`；全包 `go test ./internal/app/` → `ok … 13.621s`；`go build ./...` exit=0 | 全绿读数当场取得；全包回归覆盖既有 wire / 准入 / 基线 / 写面用例 |
| 红态 | `-overlay` 逆改两例（探针副本建在仓库外，`perl -0pi -e` 施加后 grep 核对生效，用毕即删）：① `Gate: resourceGate` 改为 `Gate: &core.ResourceGate{}` → `wire_test.go:1090` 报 `Not same`，expected / actual 是两个不同地址的 `*core.ResourceGate`，消息「发布闸门必须是准入持读锁的同一实例（私有闸门会让互斥静默消失）」；② `Catalog: catalogStore` 改为 `Catalog: nil`（退化为私有空目录）→ `wire_test.go:1096` 报 `Not same`，expected / actual 是两个不同的 `*providerCatalogStore`，消息「运行期目录持有者必须是同一实例（发布替换后新 Run 才读得到）」 | 判别力成立：分别识破「另建一份发布闸门」与「另建一份目录持有者」两种装配错误 |
| stage-only | 不适用：stage 的 `tasks` 表无 T027 绑定（任务无专属 check）；其行为由 T031 / T040 / T042 / T051 的 `012-app-resource-admin` 与 T031 的 `012-app-agent-admin` 覆盖 | 与 tasks.md 一致；不为装配任务临时新增判据面 |
| 勾选 | 暂不勾选 | 未全绿不得宣称完成：`writeServices.resourceAdmin` 仍为空槽，待 T040–T053 补齐 13 方法后随整组 check 翻转 |

### 1.25 T028 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 卡片要求 `resource-form.test.ts` 覆盖「新建 / 412 / 400 problems」，但新建走 `POST`，契约里 POST 端点**不带 If-Match**（条件写只覆盖修改 / 删除 / 发布），新建路径不可能产生 412 | 表单同时实现模型连接的**条件修改**（`GET /model-connections/{name}` 取响应头 ETag → `PUT` 带 If-Match）；412 用例走修改路径：固定文案 + 重读 ETag + 保留用户输入，第二次保存使用重读后的 ETag。新建与 400 两例仍走 POST，三例覆盖卡片点名的三类分支 |
| D2 | US-11 的 `ApiClient` 错误 data 白名单只有 `agents` / `active_runs` / `run_ids` / `problems`，412 契约可选的 `current_etag` 会被丢弃 | **有意不改** `api.ts`（US-11 已判面，§1.20 D2 已扩到该白名单边界）：412 分支按注 20 重新 `GET` 详情取 ETag，不消费 `current_etag`；本任务不扩大既有错误 data 形状 |
| D3 | 卡片 13 文件前端增量未列 `views/resources.ts`，但页面路由表把 `resource-form` 定义为 `/console/resources/:kind` 的**页内展开式表单**（不占新路由） | 派生接线：`views/resources.ts` 增加「新建」按钮、展开 / 取消状态、`created` 后重读列表；不新增路由、不改 `app.ts` VIEWS。属已审范围内的派生接线，不改范围 / 权限 / 验收语义；文件增量 +1 记入收口核对 |
| D4 | MCP / 技能修改不在本任务：`McpServerDetail` 只回 `env_keys`（值不可恢复），`McpServerUpsert.env` 在 update 时的合并语义契约未定义；技能更新是全量替换且归详情页 | 表单 `edit` 仅对 `model-connections` 生效；其它种类固定提示「该资源类型暂不支持在此表单修改：请在资源详情页按版本发布」。MCP env 更新语义随 T044–T048 定案，技能更新归 T029 详情页 |
| D5 | 本机无 Node（`node` / `npm` / `npx` 均不可用，同 §1.20 D6） | Vitest / `vue-tsc` 本机**未执行**；前端判据由 CI 前端 job 承担，未验证清单 U6 保持「未验证」，不宣称前端绿态 |
| 交付 | 新增 `assets/console/src/components/resource-form.ts`（三类资源创建；模型连接条件修改；412 重读 ETag 保留输入；400 渲染 `problems`；技能目录只读 UTF-8 文本）；新增 `assets/console/tests/resource-form.test.ts`（3 例）；修改 `assets/console/src/views/resources.ts`（页内挂载 + 写后重读） | 无新前端依赖、无危险 DOM（`v-html` / `innerHTML` / `dangerously` / `eval(` 三文件零命中）、token 仍在内存；写后由父页面重新 GET，不做乐观更新 |
| 实测 | 本机 `node --version` → `command not found`；危险 DOM grep 零命中（exit=1）；导入符号、事件名与 `AdminApi` 方法签名逐项静态对照通过 | 前端检查**未验证**（无本地工具链），CI 判卷；不以静态自查替代判卷 |
| stage-only | 不适用：stage 的 `tasks` 表无 T028 / T029 绑定（前端判据不在 Go stage 内） | 与 tasks.md / stage 一致；不为前端任务临时新增 Go 判据面 |
| 勾选 | 暂不勾选 | 未全绿不得宣称完成：前端用例本地无法执行，待 CI 前端 job 与收口证据后由控制侧翻转，并按需把 U6 翻为已验证（注日期与 CI 证据） |

### 1.26 T029 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 详情页的 412 只可能来自发布 / 删除（条件写）；版本详情是只读 GET | 发布与删除都带详情 GET 的响应头 ETag；412 固定提示 + `reloadAll()`（详情 / 版本 / 引用），已打开的版本详情与页面选择原样保留，不自动重试。编辑表单的 412 由 `resource-form` 自身处理（§1.25 D1） |
| D2 | 图 H 写「409 → 同名 / 定义文件已变化 / 资源忙；按 message 分支」 | 409 直接展示服务端固定文案（`errorMessage`），页面**不**按文案子串分叉、不自动重试；只有 412 重新读取 ETag。避免把 UI 行为耦合到文案措辞 |
| D3 | 「撤回」在契约里 = 用旧版本内容创建新版本再发布（`POST .../publish {version_id}`） | 版本列表每行给「撤回至此版本」（非当前发布版本）与「详情」；当前发布版本显示「当前发布」不可点。全局「发布最新版本」按不带 `version_id` 的幂等发布实现；成功后重读版本 / 指针 / 引用 |
| D4 | 技能没有测试端点：`POST /skills/{name}/validate` 校验的是**上传草稿**，不是详情页的已发布版本；图 H 只为模型连接 / MCP 画了测试按钮 | 测试按钮只在 `model-connections` 与 `mcp-servers` 渲染；技能详情不提供「测试」，也不拿 validate 冒充 |
| D5 | 详情页的「修改」入口 | 仅模型连接嵌入 `ResourceForm` 的修改模式（契约定义了 `api_key` 留空保留原值）；MCP / 技能的修改维持 §1.25 D4（值不可恢复 / 契约未定义合并语义），不造第二套写路径 |
| D6 | 资源列表页到详情页的跳转链接尚未接上 | 详情路由属 T064（`router.ts` 追加 4 条路由）；视图文件先落地，列表行链接随 T064 一并接上，避免提前产生 404 链接 |
| D7 | 本机无 Node（同 §1.20 D6 / §1.25 D5） | `vue-tsc` / Vitest 本机**未执行**；视图在 tsconfig include 面内，由 CI 前端 job 判卷；未验证清单 U6 保持「未验证」 |
| 交付 | 新增 `assets/console/src/views/resource-detail.ts`（版本列表 / 版本详情 / 引用 / 发布 / 撤回 / 删除 / 测试；412 重读 ETag 保留输入；删除二次确认复用既有 `ConfirmDialog`） | 无新前端依赖、无危险 DOM（`v-html` / `innerHTML` / `dangerously` / `eval(` / `new Function` / `document.write` 零命中）；写后重新 GET，无乐观更新；只使用 `AdminApi` 既有方法 |
| 实测 | 危险 DOM grep 零命中（exit=1）；导入面逐项对照 `api-admin.ts` / `api.ts` 存在；`ResourceForm` 事件名 `saved` 与其 `emits` 声明一致 | 静态核对通过；类型检查与用例**未验证**（无 Node），CI 判卷 |
| stage-only | 不适用：stage 的 `tasks` 表无 T029 绑定 | 与 tasks.md / stage 一致；前端判据不在 Go stage 内 |
| 勾选 | 暂不勾选 | 未全绿不得宣称完成：待 CI 前端 job（`vue-tsc` / `vitest` / `vite build`）与收口证据后由控制侧翻转 |

### 1.27 T030 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 约束 34 的四行决策要求对三种资源都覆盖，但本期唯一的事务性发布 API `ResourceStore.PublishWithVersion` 在 `publishTx(..., true, ...)` 处**硬编码 `applied=true`**；MCP 的待重启语义要求 published 前移、applied 保持旧值（约束 19 / 图 C） | 本任务的 MCP 分支**延后到 T044–T049**（MCP 发布存储路径同批交付），期间 `warnMCPReconcileDeferred` 对已发布 MCP 逐次 WARN 点名、已发布版本保持不变。否决的方案：① 照写 `applied=true`（会把待重启静默成本次生效）；② `EnsureVersion` + `Publish(applied=false)`（按哈希去重而非总建新版本，且把一次发布拆成两个事务）；③ 现在新增存储方法（超出交付物清单的对外概念，触发软门禁） |
| D2 | 分歧闸门判据原本只服务写面（固定 `providers:` 键），对账需要同一判据但按顶层键参数化 | 判据实现移到 `definition_reconcile.go` 的 `definitionBlockMatchesFor(doc, key, name, published)`，`resource_admin.go` 的 `definitionBlockMatches` 改为一行委托（`providersBlockKey`）。两条路径共用一份实现，避免第二套哈希口径 |
| D3 | `extractDefinitionBlocks` 在 `ParseComments` 下把**只有注释的文档**判成 `*ast.CommentGroupNode` 而报「顶层必须是映射」；对账因此把「运维把条目删到只剩注释」判成 Retained 而不是 Ignored | 该函数补一条分类：`CommentGroupNode` = 空文档（注释不是内容），与 `NullNode` 同口径返回零条目——它不是结构损坏。基线同受益（空配置不再报错）。实测：R4 首轮红（`Retained:1` / `want Ignored:1`），改动后 12 例全绿、`./internal/app` 全包 passed |
| D4 | 候选校验（整份文件校验、占位解析、客户端构建）与「预写操作系统操作行」的先后 | 全部候选校验在 `BeginOperation` **之前**完成：候选非法不产生操作行（§1.24 D 口径「纯校验不留操作行」）；一旦预写 pending，后续任何失败都回填 `failed` 并拒绝启动。`Gate` 写锁只覆盖提交事务，不覆盖文件读取与客户端构建 |
| D5 | 技能快照目录名需要版本号，而快照物化在数据库事务之前 | `nextNo = 最新版本号 + 1`（版本号只增不补）；`Materialize` 先于事务执行，事务失败留下的孤儿快照目录在下一次启动 `Materialize` 时按内容哈希命中复用（不覆盖、不重写） |
| D6 | R12 并发用例的断言面 | 生产启动由实例锁串行化（`TestWire_CloseReleasesInstanceLockLast` 钉住锁的持有期），R12 只证明实现本身无共享可变状态：两路调用均不误判为 Retained / Ignored、版本号不重复、发布指针的 `definition_yaml` 与文件条目原文逐字节一致。技能并发不测：共享 `nextNo` 可能让 `storage_path` 与库内版本号错位，属单线程启动假设（已写入用例注释） |
| 交付 | 新增 `internal/app/definition_reconcile.go`（四种决策 / 相同哈希跳过 / 非法规文件保留且 WARN / 操作幂等；provider + 技能；MCP 延后点名）与 `internal/app/definition_reconcile_test.go`（12 例，含模块卡 §4.3.4 原样回归 `TestReconcile_InvalidEditKeepsPublished` 与竞态锚点 `TestReconcile_ConcurrentStartup`）；修改 `internal/app/resource_admin.go`（判据委托）、`internal/app/baseline.go`（D3 空文档分类） | 零新增坐标、零迁移；发布内容一律取文件条目原文，不重新序列化 YAML；actor = `cli:<本机用户>`、`detail_json.source=startup_reconcile` |
| 实测 | `go test -count=1 -run '^TestReconcile_' ./internal/app` → ok（12 测试函数 / 13 个含子例）；`go test -count=1 ./internal/app` → ok；`gofumpt -w` 四个改动文件后 `gofmt -l` 无输出、exit=0 | stage 报告 `accept-20261022T010648.156475000`：`012-app-reconcile` 的 24 个已存在测试全部 passed，唯一未执行锚点为 T067A 的 `TestStartupReconcile_Idempotent` |
| stage-only | `012-app-reconcile` 报 `required test did not execute and pass: TestStartupReconcile_Idempotent` | 非实现缺陷——该锚点属 T067A（启动序列集成），另两个恢复锚点（`TestRecoverAdmin_ActionMatrix` / `TestRecoverTest_UnknownNotResent`）待兄弟任务落地；不临时删锚点（保留控制侧完整判据面）。已记入 §2 |
| 勾选 | 暂不勾选 | 未全绿不得宣称完成：`012-app-reconcile` 的锚点集到 T067A 才完整 |

### 1.28 T031 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | T031 的任务正文写「修改 `internal/app/run.go`：`serve` 启动顺序接入定义对账与 supervisor」，但重路径九步装配的实体在 `wire.go` 的 `newRuntime`；`run.go` 只是 `Run` 入口与委托（`serve` 走 `newRuntime` 之后才拿运行期） | 生产改动落在 `wire.go`（`run.go` 零改动）：`ReconcileDefinitions` 插在 `stepRecovery` 之后、目录与注册表之前，收尾对账读数进 `stepReconcile`；与任务正文的**落位差异**记录在本行，改动内容与次序判据不变（颗粒度文档 §3.1 的落位以包为粒度，不在文件粒度冻结） |
| D2 | 约束 18 要求「用已发布版本构建 Provider 目录」，而原装配把目录构建排在 `newAgentRegistry` **之后**（注册表先建立、目录后建），次序与卡片图 A 相反 | 目录构建提前到注册表之前，并新增 `stepCatalog` 读数；次序判据落为 `lock→open→migrate→recover→baseline→recovery→reconcile→catalog→registry` 九步（`TestWire_HeavyPathStepsAppearInOrder` 的期望列同步扩为九项） |
| D3 | 模块卡 §3.2.2 记 `run_test.go` **+6**，实际交付 **7** 例：`TestStartupOrder_PublishedBeforeRegistry`、`TestStartupReconcile_ExternalEditPublishesWithCLIActor` / `_InvalidFileKeepsPublished` / `_RemovalIgnored`、`TestRunAdmission_PinsProviderVersion` / `_PublishGateConsistent`、`TestRunRuntime_ReleaseAfterFinish` | 七例都是卡片点名或必需的判据面（约束 17 复核行明写 `TestRunAdmission_PublishGateConsistent` 与 `TestRunRuntime_ReleaseAfterFinish`）。多出的一例是启动语义的**装配层**用例：T030 的三例 `TestStartupReconcile_*` 判的是对账函数本身，本任务的三例判「真实重路径上对账确实先于目录 / 注册表且发布内容生效」——判据不同，不重复。按 M3 裁决以实测为证据，计划数 6 与实际 7 的差额记在本行 |
| D4 | 约束 17 复核行还点名 `TestRunAdmission_DeletedResourceRejectsNewRun`（已准入的 Run 不受删除影响、新 Run 立刻失败） | 本任务**不交付**：删除路径（`ResourceStore.MarkDeleted`）属 T039–T043，缺它无法构造"已删除资源"。T040 落地删除后补该例，并在 T068 的 stage 展开时加入 `012-app-resource-admin` 的精确锚点集；本行是该例的唯一挂账处 |
| D5 | 重启类用例要在**同一工作区**连续装配，而 `heavyRuntime.Close` 非幂等（`storage.DB.Close` 二次调用报错），夹具的 `assemble` 又把关闭挂在 `t.Cleanup` | 新增 `restartSequence`（本轮装配 → 显式关旧的 → `t.Cleanup` 关最后一个），每个运行期恰好关闭一次；装配动作与 `assemble` 逐句一致，仅关闭时机交给调用方 |
| D6 | 夹具 `writeAgent(t, name)`（零工具）写出的 frontmatter 缺收尾换行：`declared` 为 `"tools: []"` 时与 `---` 粘连成 `tools: []---`，装载器判 `invalid frontmatter`，Agent **整只不注册**，运行时报 `agent not found: runner` | 夹具补一个换行（既有 11 个调用点都带工具，行为不变）；这是夹具缺陷而非实现缺陷，记入 §2 |
| D7 | stage 草稿的两处锚点与模块卡不一致：`012-app-reconcile` 的 `TestStartupReconcile_Idempotent`（全仓与卡片均无此名）、`012-app-agent-admin` 的 `TestStartupOrder_ReconcileBeforeRegistry`（卡片约束 18 为 `TestStartupOrder_PublishedBeforeRegistry`） | 按 T003 / T019 / T021 先例以模块卡精确名为准，控制侧把 `required_tests` 对齐为卡片名（`012-app-reconcile` 换成三例 `TestStartupReconcile_*`；`012-app-agent-admin` 换成 `TestStartupOrder_PublishedBeforeRegistry`），不删锚点、不改断言；`012-app-resource-admin` 补 `TestRunAdmission_PinsProviderVersion` / `TestRunRuntime_ReleaseAfterFinish`。复跑后两条 check 的未执行锚点都变成兄弟任务用例 |
| 交付 | `internal/app/wire.go`：`ReconcileDefinitions` 接入 + 九步读数（新增 `stepReconcile` / `stepCatalog` / `stepRegistry`）+ 目录构建前移；`internal/app/run_test.go`：七例 + `restartSequence` / `writeProbeConfigModel` / `writeInvalidProbeConfig` / `publishProbeVersionForTest` / `probePointer` / `probeUpdateOperations` / `probeVersions` / `recordingRunResources` / `recordingRunRuntime` / `recordingRunStore` / `runEventRecorder`；`internal/app/wire_test.go`：九步期望 + 夹具换行修复；`harness/stages/us12-agent-resource-management.json`：三处锚点对齐 | 零新增坐标、零迁移；`run.go` 未改（见 D1）；`FinishRun` → `Release` 的次序由 `core.AgentService` 既有实现保证，本轮用记录包装在**同一份仓储**上复验 |
| 实测 | 七例逐个 passed（`go test -count=1 -run '^(TestStartupOrder_\|TestStartupReconcile_\|TestRunAdmission_\|TestRunRuntime_)'`）；`go test -count=1 ./internal/app` → ok（14.482s，全包无回归）；`gofumpt -w` 三个改动文件后 `gofmt -l` 无输出、exit=0；`go vet ./internal/app` exit=0；`staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./internal/app` exit=0 | 关键判据读数：A1 清单 `Provider.VersionID` = 装配后新发布的 v2；A2 写锁持有 100ms 内新 Run 未返回、释放后清单记 v2；A3 事件序 `["finish","runtime-release"]`、释放计数 1、释放后 `Provider("probe")` 为 false；S1 目录 `models["probe"]` = 文件新值；S2 操作行 `cli:<user>` / `cli` / `startup_reconcile` 且三次启动版本数 2、操作行 1；S3 / S4 各零写入 + WARN 点名 |
| stage-only | **红**（报告 `accept-20261022T011902.245015100`）：`012-app-agent-admin` 报缺 `TestAgentArchive_TimeoutRollsBack`（本任务 25 个匹配用例含 `TestStartupOrder_PublishedBeforeRegistry` 全部 passed）；`012-app-resource-admin` 报缺 `TestResourceVersionDetail_Projection`（本任务 `TestRunAdmission_*` 两例与 `TestRunRuntime_ReleaseAfterFinish` 全部 passed） | 非实现缺陷——前者属 T033 / T035，后者属 T040；两条 check 的锚点集到兄弟任务落地才齐。不临时删锚点（保留控制侧完整判据面），不复用旧报告（源码已变，证据按源码版本失效口径处理） |
| 勾选 | 暂不勾选 | 未全绿不得宣称完成：两条 check 仍红在兄弟任务锚点上，勾选在整组 check 转绿后由控制侧翻转 |

### 1.29 T032 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 约束 18 要求注册表按**已发布** Provider 名集装载，但重路径的因果次序是「基线需要 Agent 名字集 → 名字集来自装载器 → 装载器要判 Provider 成员 → 已发布名集要到对账 + 目录之后才存在」 | **两遍装载**：基线之前一遍只服务 `runBaseline` 的 `Agents` 名字集，Provider 名集取 `providerNameUnion`（配置文件 ∪ 未删除发布指针，去重升序）——首启并集退化为文件名，后续启动时「文件已删、指针保留」的 Provider 也不会把 Agent 误判成未注册而污染日志；目录之后第二遍用 `catalogProviderNames(catalog)` 重判，产出喂给注册表、调度器、管理台列表与 watcher。注册结论只采信第二遍。否决的方案：① 基线前就读目录（目录要已发布版本，而基线正是首启写指针的那一步）；② 单遍装载后置（基线拿不到 Agent 名，生命周期状态文件无从对齐） |
| D2 | 场景 2「新 Run 立即使用新映射」要求运行期路由跟随发布换表，而原来的运行入口是装配期冻结的 `*provider.ProviderService`；`internal/provider/` 不在模块卡 §3 交付物清单内，不能改 | 在 `internal/app/providers.go` 新增 `catalogProviderRouter`（持有 `providerCatalogStore`；`Pinned` / `Chat` 每次读当前快照；查不到名字时的错误包装与 WARN 键与 `ProviderService.Chat` 第 1 道拒绝逐字一致）；`runResources.LLM` 由 `*provider.ProviderService` 放宽为包内接口 `pinnedProviderService`，既有 `NewRunResources` 调用点与 `providerClient` 签名不变；在途 Run 仍拿准入时固定的 `Pinned` 视图（约束 16 的写时复制） |
| D3 | 约束 18 的 MCP 半边（「MCP 集合只由已发布版本构建」）本任务**未落地**：注册表的 `MCPServers` / `MCPConnected` 仍来自 `mcpRT.ConfiguredNames()`（文件解析结果） | **延后到 T044–T049**，属任务排序而非范围变更：T044 会把 `ParseMCPServers` 的返回形状改成 `core.MCPServerConfig`（args 与 command 分离），T046 / T047 / T049 把连接生命周期移进 supervisor 且 `applied` 只在成功接管后写入；此刻按已发布版本建 MCP 集需要 T044 尚未产出的配置形状与 T046 才有的连接判定。约束 18 现有锚点（`TestStartupOrder_PublishedBeforeRegistry` / `TestStartupReconcile_InvalidFileKeepsPublished`）本轮实测 passed。本行是该延后项的唯一挂账处，T049 收口时须核销 |
| D4 | stage 的 `tasks` 表原本没有 T032 绑定（§1.6 派生时按当时已存在的测试族登记） | 控制侧补 `{ "id": "T032", "checks": ["012-app-agent-admin", "012-app-resource-admin"] }`，并把本轮新增的 `TestRunAdmission_RoutingFollowsPublishedCatalog` 加进 `012-app-resource-admin.required_tests`（控制侧精确锚点）；不删锚点、不改任何断言语义 |
| D5 | `webProviders`（`/info` 的 provider 摘要）仍按 `cfg.Providers` 产出 | **不动**：它是信息视图不是运行目录（约束 18 约束的是目录 / 注册表 / 路由三处构建来源），模块卡 §3 未把它列入改造点（卡片只在主裁决 4 提 `/info` 的 `uptime_seconds`）。如实记录以免复核时误判为漏改 |
| 交付 | 修改 `internal/app/providers.go`（`catalogProviderNames` + `catalogProviderRouter` + `modelClient` 注释改口）、`internal/app/resources.go`（`pinnedProviderService` 接口 + `runResources.LLM` 放宽 + `providerClient` 失败关闭加固）、`internal/app/wire.go`（`versions` 上提到基线之前、两遍装载、`providerNameUnion`、运行入口改 `newCatalogProviderRouter`）；新增测试 helper `writeAgentContent` / `writeAgentWithProvider`（`wire_test.go`）、`publishModelConnectionForTest`（`run_test.go`）；`run_test.go` 新增 `TestAgentLoad_PublishedOnly` 与 `TestRunAdmission_RoutingFollowsPublishedCatalog` | 零新增坐标、零迁移；`providers[` 复核 grep（`grep -rn 'providers\[' internal/app \| grep -v published`）只剩错误标签与 map 存取 |
| 实测 | 两例 `go test -run '^(TestAgentLoad_PublishedOnly\|TestRunAdmission_RoutingFollowsPublishedCatalog)$'` → PASS；`go test -count=1 ./internal/app` → ok（15.253s）；`go test -count=1 ./...` → 全部 ok（含 web / e2e 外的全部包）；`go vet ./...` exit=0；`staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./internal/app/...` exit=0；`gofumpt -w` 五个改动文件后 `gofmt -l` 无输出 | 红态先行（`go test -overlay`，探针副本建在仓库外、用毕即删）：① 把注册表遍的 `Providers` 改回 `providerNames(cfg)` → `TestAgentLoad_PublishedOnly` 红在 `run_test.go:440`「Agent 目录里的条目必须进入注册表快照」（只存在于已发布集的 `extra-runner` 整只不注册）；② 把运行入口改回 `modelClient(catalog, …)` 冻结点 → `TestRunAdmission_RoutingFollowsPublishedCatalog` 红在 `run_test.go:549`「换表之后的新 Run 必须路由到新客户端」（`assemblyClient` 收到调用、新客户端零调用）。两处红态逐字符合预期，恢复后绿 |
| stage-only | **红**（报告 `accept-20261022T012705.685185300`）：`012-app-agent-admin` 报缺 `TestAgentArchive_TimeoutRollsBack`（本 check 里 `TestAgentLoad_PublishedOnly` 与 `TestStartupOrder_PublishedBeforeRegistry` 均 passed）；`012-app-resource-admin` 报缺 `TestResourceVersionDetail_Projection`（`TestRunAdmission_RoutingFollowsPublishedCatalog` / `TestRunAdmission_PinsProviderVersion` / `TestRunAdmission_PublishGateConsistent` / `TestProviderCatalog_*` 均 passed） | 非实现缺陷——前者属 T033 / T035，后者属 T040；两条 check 的锚点集到兄弟任务落地才齐。不临时删锚点，不复用旧报告 |
| 勾选 | 暂不勾选 | 未全绿不得宣称完成：两条 check 仍红在兄弟任务锚点上，勾选在整组 check 转绿后由控制侧翻转 |

### 1.30 T033 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 模块卡 §4.3.3 的原样回归用 `core.Actor{Subject: "test"}`（不带 Method / Source）；`management_operations.auth_method` 的 CHECK 只收 `bearer` / `local_trust` / `cli` / `migration`，首轮红在预写（`… CHECK constraint failed: auth_method IN (...)`，want `AgentBusyError`） | 夹具 `newAgentEnv` 在 `AgentAdminDeps.Operations` 外包 `defaultingActorStore`：只在操作写入边界把空 Method 补成 `local_trust`。生产代码与断言语义零改动——真实调用方（web / CLI）一律带完整 Actor；与 T025 的 `resourceActor()` 属同一类夹具修正（§2） |
| D2 | 归档的「源可归档」判定（404 / 状态非法 / 目录缺失）放在预写**之前**：`checkArchiveSource` 在 `beginAgentOperation` 前首次调用，锁内再调用一次防「检查后漂移」。图 F 第 441 行从 `DELETE` 起画、未标预写位置 | 按 FR1「纯请求校验（路径 / If-Match / 存在性）在写操作行之前」落地：未知 Agent 归 404 且**零操作行**；预写之后的任何失败才落 failed / unknown |
| D3 | 复制 / 恢复的候选校验（名称规则、源在活动区、目标目录与状态文件空闲、归档行存在且未恢复、`ArchiveDirContentHash` 复算）全部在预写前完成 | 锁内只重做并发相关复检（复制：源仍活动 + 目标仍空闲；恢复：目标仍空闲 / 原名状态文件仍为 archived）。纯校验失败零操作行；不得把校验失败伪装成已留痕的失败操作 |
| D4 | 归档与恢复的派生索引写失败**无法**回填 `index_write=deferred`：`InsertArchive` / `MarkRestored` 在同一事务里落定操作终态，索引同步发生在其后，操作行已不可改写 | 索引失败只记 ERROR 日志（状态文件与归档行为权威；索引可从二者重建，约束 27 / 36）。`index_write=deferred` 只用于创建 / 复制这两条「先 `finish` 终态、后同步索引」的路径 |
| D5 | 归档第 ⑧ 步失败的回滚顺序与恢复失败的统一回退 | 归档：`UnarchiveAgentDir`（反向 rename）→ 状态 `archived → 归档前状态`（generation 再 +1）→ 恢复 ①②；反向 rename 失败即「目录归属不确定」，不再回退状态 / 注册，整体标 unknown；补偿全部成功才落 failed。恢复：`rollbackRestore` 删目标目录 → 原名 `Transition(disabled → archived)` / 异名 `Discard`；补偿失败 → unknown。归档目录从不移动，归档行保持未恢复、可重试 |
| D6 | 归档目标已存在（`archive/<name>-<UTC 时间戳>` 已被占用） | `workspace.ArchiveAgentDir` 在 rename 前判目标存在并上抛 `ErrArchiveTargetExists`；`Archive` 映为 409 CONFLICT「归档目标已存在，未执行归档」，源目录 / 状态 / 注册零变化，**不**自动换名（FR32） |
| D7 | 模块卡 §3.2.1 记 `agent_admin_test.go` 25 例；实测 21 既有 + 4 新增 = 25 | 新增：`TestAgentCopy_DisabledBootstrap`（+ `failure_zero_residue` 子例）、`TestAgentArchive_TimeoutRollsBack`（§4.3.3 原样，逐字未改）、`TestAgentArchive_RollbackMatrix`（`target_exists` / `insert_failure`）、`TestAgentRestore_OriginalNameGeneration`（`original_name` / `renamed_bootstrap` / `failure_keeps_archive_retryable`）。代数链实测：Bootstrap(1) → active(2) → archived(3) → 原名恢复 disabled(4) → 失败回退 archived(5) → 重试 disabled(6)；异名恢复 Bootstrap(1) |
| 交付 | `internal/app/agent_admin_test.go`：夹具族 `fakeClock` / `archiveScheduler` / `archiveCanceller` / `archiveEnv` / `newAgentEnv` / `defaultingActorStore` / `failingInsertArchiveStore` / 快照三件（`everythingSnapshot` / `snapshotEverything` / `equalEverything` / `readAdminTree`）与四例；生产代码 `internal/app/agent_admin.go`、`internal/workspace/agent_admin.go` 在本任务前序窗口已落地（本轮零改动） | 零新增坐标、零迁移；夹具时钟 `baselineClock()` 固定，归档标识按 `20060102-150405`（UTC）复算 |
| 实测 | 4 个测试函数含子例全 PASS；`go test -count=1 ./internal/app` → ok（15.319s）；`go vet ./internal/app` exit=0；`gofmt -l` / `gofumpt -l` 对两个改动文件无输出、exit=0；staticcheck（项目 checks）`./internal/app` exit=0 | 关键读数：超时路径 `AgentBusyError.RunIDs` 含 `run-1` 且状态 / 注册 / 定时 / 目录树四项快照相等；`insert_failure` 归档目录零残留、零归档行、操作 failed / INTERNAL；恢复失败后归档行 `restored_at` 保持 NULL、清掉残留重试成功 |
| stage-only | **绿**（报告 `accept-20261022T014905.258472100`，exit=0）：`012-app-agent-admin` 的八锚点全部执行并 passed，含 T031 / T032 交付的 `TestAgentLoad_PublishedOnly`、`TestStartupOrder_PublishedBeforeRegistry` | 本 check 的兄弟锚点（`TestRecoverAdmin_ActionMatrix` / `TestResourceVersionDetail_Projection`）分属 T030 / T031 / T032 的另外两条 check，不前移到本任务 |
| 勾选 | **勾选**（`- [ ]` → `- [x]`） | 本任务 check 全绿、25 例齐；兄弟任务 T030 / T031 / T032 的 check 仍红在各自锚点，保持未勾选 |

### 1.31 T034 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 本包已有 `decodeOptionalAdminWriteBody`（T023 的 `handlers_admin_resources.go`，供 `POST …/publish` 的可选体使用）；首版草稿在归档文件里重复声明，`go vet` 报 `method Server.decodeOptionalAdminWriteBody already declared` | 删除重复定义并复用既有助手——解码口径（上限 / 未知字段 / 尾随内容 / 空体 = 零值）与归档恢复端点完全一致，不引入第二个解码出口（§2 失败台账） |
| D2 | 模块卡 §3.2.1 把「原名恢复保留 generation、异名 Bootstrap」列在本任务用例面，但这两条是**适配器**的编排（状态迁移与 generation 递增），handler 层拿不到状态文件 | handler 层判据：缺省请求体 → 端口收到空 `new_name`（原名路径的输入契约）、响应投影恢复后的文件视图且生命周期为停用；generation 代数链由 T033 的 `TestAgentRestore_OriginalNameGeneration` 判卷（§1.30 D7）。两条用例仍按卡片计划数落地为 10 个测试函数 |
| D3 | 三个占位行（`DELETE /agents/{name}`、`GET /archived-agents`、`POST /archived-agents/{archive_id}/restore`）接上真实 handler；其余未实现端点继续用 `handleAdminUnassembled` | 路由表仍是 30 条、顺序与契约 §3.7.1 一致；认证与 If-Match 包装不变（归档与恢复都**不**是条件写，`conditional` 保持 false——契约未给这两个端点 If-Match） |
| D4 | 恢复端点对归档缺失的 404 必须点名**归档标识**；`core.ErrResourceNotFound` 是与 Agent 名无关的资源级哨兵 | 恢复 handler 先判 `core.ErrResourceNotFound` → 404「未找到归档记录：<archive_id>」；`writeAgentAdminError` 同步补一条通用 `core.ErrResourceNotFound` → 404 分支（点名传入标识），保持错误映射集中在唯一出口 |
| D5 | 恢复请求体按契约 `required: false`：省略 = `new_name` 为空；给了就严格解码（未知字段 / 尾随内容 / 语法错误 → 400 固定文案、端口零调用） | 复用既有可选体助手；`TestArchivedAgents_RestoreInvalidBody` 三个子例（unknown / trailing / malformed）各断言零端口调用 |
| 交付 | 新增 `internal/web/handlers_admin_archived.go`（三 handler + 两个固定文案）与 `internal/web/handlers_admin_archived_test.go`（10 个测试函数：`TestArchivedAgents_List` 三子例 / `_ArchiveProjectsRecord` / `_ArchiveBusy` / `_ArchiveConflict` / `_ArchiveNotFound` / `_RestoreOriginalName` / `_RestoreRenamed` / `_RestoreMissingArchive` / `_RestoreInvalidBody` 三子例 / `_PortMissing`）；修改 `internal/web/admin_routes.go`（三行接线）与 `internal/web/handlers_admin_agents.go`（404 分支 + 注释） | 零新增坐标、零迁移；测试替身 `fakeArchivedAgentService` 在既有 `fakeAgentAdminService` 上只覆盖归档三方法，未注入即报「意外调用」 |
| 实测 | 10 个测试函数含子例全 PASS；`go test -count=1 ./internal/web/` → ok（4.211s）；`go vet ./internal/web/` exit=0；`gofmt -l` / `gofumpt -l` 对四个改动文件无输出、exit=0；staticcheck（项目 checks）`./internal/web` 无输出 | 关键读数：409 AGENT_BUSY 的 `data.run_ids` 为 `["run-1","run-2"]` 且自由文本不含标识；空列表 `data` 为 `[]`（非 null）；归档缺失 404 文案为 `未找到归档记录：gone-20260101-000000` |
| stage-only | **绿**（报告 `accept-20261022T015332.062137800`，exit=0）：`012-web-admin-agents` 两个 `required_tests`（`TestAdminAgents_CreateZeroResidue` / `TestArchivedAgents_List`）均执行并 passed | 本 check 其余锚点属 T035 / T037 的兄弟任务，不前移到本任务 |
| 勾选 | **勾选**（`- [ ]` → `- [x]`） | 本任务 check 全绿、10 例齐；兄弟任务 T030 / T031 / T032 的 check 仍红在各自锚点，保持未勾选 |

### 1.32 T035 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 适配器原先的 `SetRegistered` 在锁外委托调度器、随后才进 `adapter.mu` 删 claim；`ReconcileAgent` 也先解锁再委托。归档注销（`SetRegistered(false)` → 调度器读任务行并退休）期间，并发 `manage_schedule` 仍能拿到 `adapter.mu` 落盘到正在归档的目录 | 两处都把调度器委托并入同一临界区（锁序仍是 `adapter.mu` → `scheduler.stateMu` / `loader.writeMu`，无反向）；注销成功才删 claim——调度器失败时调用方按回滚矩阵恢复注册，旧基线保留到下一轮对齐 |
| D2 | 卡片 §3.2.1 把 4 例「注销 / 恢复联动」分给 `schedule_test.go`，而 §4.4 的族归属段写 `TestAgentArchive_*` → `agent_admin_test.go`；约束 32 的复核锚点又点名 `TestAgentArchive_SchedulesRetiredNotDeleted` | 按任务文本落到 `schedule_test.go`（真实调度器 + 真实任务仓储的夹具在本文件；stage 正则只看函数名、不看文件归属）。四个测试函数：`TestAgentArchive_SchedulesRetiredNotDeleted` / `TestAgentArchive_RollbackRestoresRetirement` / `TestAgentRestore_DoesNotAutoEnableSchedules` / `TestAgentArchive_UnregisterSerializedWithScheduleWrite`；不改族名、不改卡片正文 |
| D3 | 「恢复不自动启用」的判据：正常恢复（`AgentAdmin.Restore`）不触碰调度器；即使恢复后的 Agent 经热加载重新登记（`SetRegistered(true)`）并以恢复文件里的空定义对账，旧任务行也保持 `retired_at`。归档注销的「保留行、只退休」由约束 32 ②要求 | 适配器层两个方向都断言：注销只写 `retired_at`（cron / message / zone / enabled 原样保留）、回滚清 `retired_at` 并恢复 claim 基线、恢复后的重新登记不清退休。T033 的 `TestAgentRestore_OriginalNameGeneration` 已在编排层判「恢复清空 schedules、不回到注册表 / 调度器」（§1.30 D7） |
| D4 | 「写锁顺序」的判别性：只有把调度器委托放进 `adapter.mu`，注销期间并发写入才会被挡住 | `TestAgentArchive_UnregisterSerializedWithScheduleWrite` 用阻塞仓储（第一次 `ListTasks` 停在 `release` 上）+ 文件字节断言：注销临界区内写入不得返回、文件逐字节不变；注销结束后写入按序成功 |
| 交付 | 修改 `internal/app/schedule.go`（`mu` 注释 + `SetRegistered` / `ReconcileAgent` 临界区）；`internal/app/schedule_test.go` 新增 4 个测试函数与夹具 `archiveScheduleFixture` / `newArchiveScheduleFixture` / `rowsFor` / `gatedTaskStore` | 零新增坐标、零迁移；测试函数名英文、中文说明以注释保留 |
| 实测 | 4 例 PASS；`go test -count=1 ./internal/app/` → ok（15.553s）；`go vet ./internal/app/` exit=0；`gofmt -l` / `gofumpt -l` 对两个改动文件无输出、exit=0；staticcheck（项目 checks）`./internal/app` 无输出 | 红态先行（`go test -overlay`，探针副本建在仓库外、用毕即删）：把 `SetRegistered` 还原为锁外委托的旧实现 → `TestAgentArchive_UnregisterSerializedWithScheduleWrite` 红在「注销临界区内不得落盘」，并给出文件 diff（写入在注销期间落地了 `demo-2` 块）；恢复后绿 |
| stage-only | **绿**（报告 `accept-20261022T015912.061412000`，exit=0）：`012-app-agent-admin` 的八个 `required_tests` 全部执行并 passed；本轮四个新测试也在 `passed_tests` 中 | 本 check 其余锚点属 T036 / T037 的兄弟任务，不前移到本任务 |
| 勾选 | **勾选**（`- [ ]` → `- [x]`） | 本任务 check 全绿、4 例齐；兄弟任务 T030 / T031 / T032 的 check 仍红在各自锚点，保持未勾选 |

### 1.33 T036 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 卡片 §3.1.5 H4 的 CLI 写路径落点：`agentsPort.Create/Delete` 原先直接调 `workspace.CreateAgentStaged` / `workspace.ArchiveAgentDir`，不写操作行、不写生命周期状态文件、不写归档行——`profile create` / `profile delete` 在台账里查无痕迹 | 改为**完整生命周期所有者**：`withCLIAdmin` 一次实例锁内 → `storage.OpenWritable` + `Migrate` → 装配最小管理面（管理操作仓储 / 生命周期状态文件 / 资源存储索引 / AgentGate / 归档仓储，HTTP / MCP / 调度器 / 监视器**不**装配）→ `AgentAdmin.Create` / `Archive` → 关库（成功与失败都关）→ 释锁。命令面打印的路径仍相对工作区根，`profile delete` 的确定性归档标识仍由注入的 `now` 驱动 |
| D2 | 真实操作行 / 归档行锚点的**文件归属**：`internal/cli` 不得 import `internal/app`（用户既有裁决），且 `agentsPort` 未导出——cli 包无法构造端口去断言真实 SQLite 行 | 两条锚点（`TestCLIProfileCreate_WritesManagementOperation` / `TestCLIProfileDelete_WritesArchiveRow`）落在 `internal/app/ports_test.go`；`internal/cli/profile_test.go` 只保留命令层调用计数（`TestProfileListAndShow_SkipWriteModePort` / `TestProfileWriteCommands_TouchPortExactlyOnce`）。stage `012-cli-author` 因此同时跑 `./internal/cli` 与 `./internal/app`——`required_tests` 两锚点一字未动，判据面不变 |
| D3 | provider 名集的来源：`agentsPort.Create` 签名带 `providers []string`，H4 不要求 CLI 读已发布目录 | 命令层装载的配置列表原样传入，适配器把它映射成 `workspace.ProviderEntry` 交给模板的确定性选择（约束 28）；签名参数不是死参数，`profile create` 打印的「已选择 provider」与写入模板的 provider 同源 |
| D4 | `workspace.RestoreAgentDir` 是**复制**语义：归档目录保留，物理清除不在本模块范围 | 用例按此断言（`DirExists` 而非 `NoDirExists`，注释写明保留理由）；不为了"看起来干净"改成删除——那会越过卡片 §2 明确排除的「归档物理清除与保留策略」 |
| D5 | stage `012-cli-author` 的 `-run` 正则原先只匹配 `TestCLIProfile_` 前缀，实际用例名为 `TestProfileList_*` / `TestProfileDelete_*`，且 `TestCLIProfileDelete_WritesArchiveRow` 在 `./internal/app` 而正则未带该包 | 派生修复（同 T020 先例）：正则扩为 `^(TestParseAuthorSection_\|TestProfile(List\|Show\|Create\|Delete)_\|TestRenderProfileList_\|TestCLIProfileCreate_\|TestCLIProfileDelete_)`，包参数加 `./internal/app`，`timeout_seconds` 120 → 180；`required_tests` 不变 |
| 交付 | 修改 `internal/app/ports.go`（`Create` / `Delete` / 新增 `withCLIAdmin` / `cliAdminSession` / `openCLIAdmin` / `actor` / `userName` / `withInstanceLockErr`）、`internal/app/run.go`（`agentsPort` 补 `SupportedSchemaVersion` 与 `Logger`）；测试 `internal/app/ports_test.go`（+4）、`internal/cli/profile_test.go`（+2 与计数器） | 零新增坐标、零迁移；`server` / `web` / HTTP 面未碰 |
| 实测 | `go test -count=1 ./internal/app ./internal/cli` 全绿（15.8s / 2.4s）；`go vet ./internal/app ./internal/cli` exit=0；`gofumpt -w` 后 `gofumpt -l` 对四个改动文件无输出 | 每次改动的文件都跑了 gofumpt |
| 红态先行 | 探针（`go test -overlay`，探针副本建在仓库外、用毕即删）把 `Create` 逆改成 `workspace.CreateAgentStaged`、`Delete` 逆改成 `workspace.ArchiveAgentDir`：Create 用例**逐字红**在「生命周期状态文件缺失（control\agents\cli-agent.json）」；Delete 用例红在 `sql: no rows in result set`（归档行缺失）；锁释放用例红在操作行数 `expected: 1 actual: 0`（消息「同名再建的校验失败先于预写，只能有首次创建那一行」）；轻路径用例保持绿（不经过写路径，符合预期） | 三处绕开点（生命周期状态 / 归档行 / 操作行）都被兜住；探针目录已删除 |
| stage-only | **绿**（报告 `accept-20261022T020723.982113300`，exit=0）：`012-cli-author` 的两个 `required_tests` 全部执行并 passed | 兄弟任务 T030 / T031 / T032 的 check 仍红在各自锚点，保持未勾选 |
| 勾选 | **勾选**（`- [ ]` → `- [x]`） | 本任务 check 全绿、6 例齐 |

### 1.34 T021 / T022 勾选补记（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | §1.17 / §1.18 的「暂不勾选」条件是同 check 的兄弟锚点落地转绿（T021 待 T031 / T033 / T035，T022 待 T034）；T033–T035 已落地并各自勾选，条件已满足 | 逐项复跑：`-task T021` → `012-app-agent-admin` 八锚点全 passed（报告 `accept-20261022T020904.278258400`）；`-task T022` → `012-web-admin-agents` 两锚点全 passed（报告 `accept-20261022T020911.994062000`）。两条 check 均 exit=0 |
| 勾选 | T021 / T022 翻转（`- [ ]` → `- [x]`） | 与前序翻转同口径：check 全绿才勾选，未实现兄弟任务（T030 / T031 / T032）仍保持未勾选 |

### 1.35 T037 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | Agent 写面的 web 端口适配器落点：`web.AgentAdminService` 的 8 个方法必须由 `internal/app` 提供（`internal/web` 只定义消费方端口，不 import 实现包） | 新增 `internal/app/web.go` 的 `agentAdminPort`（含编译期断言 `var _ web.AgentAdminService = agentAdminPort{}`）：`Create` / `SubmitFiles` / `Copy` / `Restore` 经 `agentFileSet` 把 `AgentFilesView` 投影成 `web.AgentFileSet`（字段一一对应，不重算 ETag）；`GenerateFiles` 传装配期冻结的 `core.AuthorConfig`（端口不读配置、不补默认值）；`Archive` / `Archived` 原样转调记录；错误一律原样上抛，分类与文案映射仍归 web 层（约束 43） |
| D2 | 在途 Run 端口没有生产实现（T033 只定义了 `RunCanceller` 接口，测试用替身） | `internal/app/wire.go` 新增 `runCancellerAdapter`：`RunningRunIDs` 走 `storage.RunStore.ListRuns`（`Agent` + `Status=running`，`Limit=200` 与只读面同源）；`CancelRun` 只送信号——`AgentService.Cancel` 的等待被压到一个轮询间隔（`runCancellerPoll=100ms`），整段归档等待由 `WaitRuns` 的窗口统一计时，否则 N 个在途 Run 会把 30 秒有界窗口撑成 N×30 秒；`WaitRuns` 轮询 `GetRun`，**到期是结果不是错误**（返回剩余集合，调用方判 409），行已不存在按已退出计，窗口到期与读取竞争时期限断言优先；`Cancel` 不写终态，终态仍由 `ReActLoop` 写（宪法 2） |
| D3 | 归档协作者的装配面：`AgentAdminDeps` 的 `Archives` / `Registry` / `Scheduler` / `RunCanceller` 在重路径上一件都没接（约束 32 的 all-or-nothing 组） | 重路径一次补齐：`Archives: storage.NewAgentAdminStore(db)`、`Registry: agents`、`Scheduler: scheduleEditor`（与配置热加载同一实例，私有 claim 基线不分叉）、`RunCanceller: runCancellerAdapter{runs, service}`；`heavyRuntime.writeServices.agentAdmin` 填 `agentAdminPort{admin: agentAdmin, author: cfg.Author}`；三处「等 T033 / 后续任务」的过时注释同步改写。CLI 写路径保持整组省略（`openCLIAdmin` 只装归档行存储） |
| D4 | 锚点文件归属：卡片 §4.4 第 24 行把 `TestArchivedList_*` 映射到 `012-web-admin-agents`，但 `internal/web` 无法构造 CLI 写路径（不得 import `internal/app`，且无 storage 依赖） | 与 T036 同口径：`TestArchivedList_IncludesCLIArchive` 落在 `internal/app/ports_test.go`（CLI `agentsPort` 真实建库 → `agentAdminPort.Archived` 读到归档行，断言 `Len=1`、标识 / 名称 / `RestoredAt=nil`）；stage 该 check 的参数加 `./internal/app` |
| D5 | stage 派生面缺陷两处：① `tasks` 缺 T037 绑定（`-task T037` 报 `unknown stage task`）；② `012-web-admin-agents` 的 `required_tests` 未含卡片点名的 `TestArchivedList_IncludesCLIArchive`——只加包参数时该用例即使不执行 check 仍会绿 | 控制侧补 `{ "id": "T037", "checks": ["012-web-admin-agents"] }`；`required_tests` 增补该锚点（只收紧、不删不改既有锚点）；先跑 `accept-20261022T021432.844864200`（加包参数后绿）再收紧锚点复跑 |
| 交付 | 修改 `internal/app/web.go`（`agentAdminPort` / `agentFileSet`）、`internal/app/wire.go`（`runCancellerAdapter` / 两个常量 / `AgentAdminDeps` 四字段 / `writeServices` 槽）；测试 `internal/app/ports_test.go`（+1）；stage `harness/stages/us12-agent-resource-management.json`（T037 绑定、包参数、锚点收紧） | 零新增坐标、零迁移；`internal/web` 未改 |
| 实测 | `go test -count=1 ./internal/app ./internal/web` 全绿（15.4s / 3.6s）；`go vet ./internal/app` exit=0；`go build ./...` exit=0；三个改动文件 `gofumpt -w` 后无残留格式差异 | — |
| 红态先行 | 探针（`go test -overlay`，副本建在仓库外、用毕即删）把 `agentAdminPort.Archived` 逆改成 `return nil, nil`：`TestArchivedList_IncludesCLIArchive` **逐字红**在 `"[]" should have 1 item(s), but has 0`（消息「CLI 归档行必须出现在 /archived-agents 的数据源里」，ports_test.go:433） | 判别力成立：端口不转调时该用例必红；探针目录已删除 |
| stage-only | **绿**（报告 `accept-20261022T021507.993744600`，exit=0）：`012-web-admin-agents` 三个 `required_tests`（含 `TestArchivedList_IncludesCLIArchive`）全部执行并 passed，`passed_tests` 可见该锚点 | — |
| 勾选 | **勾选**（`- [ ]` → `- [x]`） | 本任务 check 全绿；兄弟任务 T030 / T031 / T032 的 check 仍红在各自锚点，保持未勾选 |

### 1.36 T038 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 卡片 §3.1.10：`archived-agents` 是 `/console/archived` 的整页视图（归档列表与恢复，含改名）；路由登记属 T064 | 新增 `assets/console/src/views/archived-agents.ts`：读取 `GET /archived-agents`；列 = 归档标识 / 原名称 / 归档时间 / 内容哈希 / 状态 / 操作；未恢复行提供「恢复」，已恢复行显示恢复目标与时间（`restored_at` 空串 = 未恢复，契约既有口径） |
| D2 | 恢复两条路径的界面文案必须与约束 31 一致 | 面板按目标名分支：留空（或等于原名）→「原名恢复：条件迁移归档态 → 停用，generation 递增，清空定时任务」；异名 →「Bootstrap 为停用、generation=1，清空定时任务」。成功后重新 `GET`（无乐观更新）；409 前缀「恢复被拒绝：目标名称已被占用，或该归档的状态已变化」并保留服务端固定文案；401 由 `ApiClient` 统一回登录并保留 `next` |
| D3 | 「含改名二次确认」的落点：`ConfirmDialog` 只能承载固定 message，无法容纳改名输入，且修改 `components/ui.ts` 不在 T038 交付面 | 行按钮只选中行并打开「恢复归档」面板；提交按钮逐字写出目标名（`确认恢复为「X」`），面板本身即第二次确认；取消关闭面板、不发请求 |
| D4 | 本机无 Node：`command -v node` exit=1、`command -v npm` exit=1、`C:\Program Files\nodejs\node.exe` 不存在；`node_modules` 存在但无运行时 | `vue-tsc` / Vitest / `vite build` **未执行**；静态自查：危险 DOM（`v-html` / `innerHTML` / `outerHTML` / `document.cookie` / `localStorage` / `sessionStorage` / `eval(` / `new Function`）grep 零命中（exit=1），视图不直接 `fetch`（exit=1）；导入符号逐个对照 `shared.ts` / `components/ui.ts` / `api.ts` / `api-admin.ts` 的导出，`listArchivedAgents` / `restoreArchivedAgent` 签名与返回 `AgentFilesView.agent` 匹配 |
| D5 | `api-admin.ts` 既有注释「原名恢复（保留 generation）」与约束 31 的「条件迁移 + generation 递增」不符 | 只改注释不改行为；`api-admin.ts` 属 US-12 交付面，按同一语义派生修正 |
| 交付 | 新增 `assets/console/src/views/archived-agents.ts`；修改 `assets/console/src/api-admin.ts`（1 行注释） | 前端零新增依赖、零新构建产物类型；`router.ts` 与 4 条路由仍归 T064（本任务不提前接线） |
| 实测 | 本机不可执行：无 Node / npm（同 §1.20 D6、§1.25 D5） | 如实登记为未验证，不以静态自查替代判卷 |
| 红态 | 未执行（无 Node；前端在 stage 中无专属 check） | 待 CI 前端 job 首次运行给出红 / 绿读数 |
| 勾选 | 暂不勾选 | 未全绿不得宣称完成：前端类型检查与用例本地无法执行，待 CI 前端 job（T067C 构建 dist 与 `router.test.ts`）通过后由控制侧翻转，并按需把 U6 翻为已验证（注日期与 CI 证据） |

### 1.37 T039 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 卡片 §3.1.6 冻结签名 `MarkDeleted(ctx, kind, id, opID string, at time.Time) error`；语义 = 只置 `deleted_at` / `operation_id`（行内 `updated_at` 同步刷新），不删指针行、不删版本；终态同事务回填 | `internal/storage/resource_store.go` 追加 `MarkDeleted`（放在 `checkVersionBelongsTo` 之后、只读段 `Pointer` 之前）：校验 kind / id / opID（`at` 零值取当前时刻）→ 开写事务 → `UPDATE resource_pointers`（0 行 → `core.ErrResourceNotFound`）→ 同事务 `finishOperationTx(...OperationSuccess...)` → 提交；错误链沿用「删除资源 %s/%s: <步骤>: %w」句式。重复删除按幂等处理：覆盖为最近一次的时间与操作号，行数不变 |
| D2 | 审计闸门在存储层不悬空：`resource_pointers.operation_id` 外键要求删除操作已预写 | 悬空操作号在指针 UPDATE 处就被外键拒绝（错误含「标记删除」），指针与终态都不落库；因此 `finishOperationTx` 的 `ErrOperationNotFound` 分支在 `MarkDeleted` 路径上不可达（外键先拒），恢复 / 归档等其它调用方不受影响 |
| D3 | 卡片 §4.4 第 46 行要求 `resource_store_test.go` +8，点名的锚点 `TestResourceStore_MarkDeleted` 必须原样落地 | 追加 8 个用例（均在 `TestResourceStore_` 家族）：`MarkDeleted`（标记 + 版本保留 + 同事务终态）、`MarkDeletedKeepsVersionsAppendOnly`（删除后按 id 可读旧版本、重建版本号继续递增、重新发布清标记）、`MarkDeletedIsIdempotent`（重复删除覆盖标记、指针 / 版本行数不变、两次操作各自 success）、`MarkDeletedMissingPointer`（not-found 且操作保持 pending）、`MarkDeletedMissingOperation`（外键拒绝、指针不变）、`MarkDeletedRollsBackWhenTerminalWriteFails`（触发器中止终态写 → 删除标记整笔回滚）、`MarkDeletedValidatesInput`（未知类型 / 空 id / 空操作号零写入）、`MarkDeletedLeavesOtherResources`（同类其他资源不受影响、已删行保留为证据） |
| D4 | 红态先行（新增审计语义的判别力证明） | 两轮 `go test -overlay` 逆改（探针副本建在仓库外，用毕即删）：① 删掉 `finishOperationTx` 调用 → `TestResourceStore_MarkDeleted` 逐字红在 `expected: "success" actual: "pending"`，`MarkDeletedIsIdempotent` 同文红，`MarkDeletedRollsBackWhenTerminalWriteFails` 红在 `An error is expected but got nil.`；② 把 UPDATE 的 `deleted_at` 参数逆改为 nil → `TestResourceStore_MarkDeleted` 红在 `Expected value not to be nil.`，`MarkDeletedLeavesOtherResources` 红在列表仍有 2 项。另有一次无效逆改（`COALESCE(deleted_at, ?)` 仍会写入首值，用例保持绿）按设计排除，不作为判别力证据 |
| 交付 | 修改 `internal/storage/resource_store.go`（+`MarkDeleted`）；测试 `internal/storage/resource_store_test.go`（+8） | 零新增坐标、零迁移；未触碰其他包 |
| 实测 | `go test -count=1 -run '^TestResourceStore_' ./internal/storage` 全绿（23 个用例）；`go test -count=1 ./internal/storage` 全绿（7.4s）；`go vet ./internal/storage` exit=0；`gofumpt -l` 两个改动文件 exit=0 且输出为空 | — |
| stage-only | **红**（报告 `accept-20261022T022050.466802300`，exit=1）：`012-storage-admin` 仅因 `TestNotifyAdminStore_CRUD`（T055 尚未实现）报 `required test did not execute and pass`；`passed_tests` 41 项含 `TestResourceStore_MarkDeleted` 与全部 7 个兄弟用例 | 非实现缺陷——同 §1.27 T012 口径：锚点按整组 check 配置，保留完整判据面，不临时删锚点 |
| 勾选 | 暂不勾选 | 本任务 check 仍红在 T055 的锚点；T055 落地后复跑 `-task T039` 转绿再由控制侧翻转 |

### 1.38 T040 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 引用闸门需要两类外部输入：Agent 装载快照与工作区根（归档扫描）；删除模型连接还需在目标凭证可能已撤掉的前提下重建候选目录 | `ResourceAdminDeps` 追加 `Root` / `Agents` / `ProviderOverride`（生产装配在 T043 接线）。`Agents` 取**装载快照**（`AgentLoader.LoadAll` 含未注册条目）而不是已注册注册表：未注册 Agent 的 `AGENT.md` 同样在引用资源，删掉会让下一次装载失败；`Profile == nil`（解析失败）按「可能引用」点名为 `<agent>（unparseable）`（卡片字面量，含全角括号） |
| D2 | 引用联合视图的语义 | `References` 用 `store.Pointer` 而**不是** `activePointer`：已删除资源仍可查询（历史版本 / 引用证据保留），404 只属于指针行不存在。`active_runs` 只来自 `ReferencedBy`（`released_at IS NULL`），**不**用"当前没有运行"推断。Agent 名单去重升序（`slices.Sort` + `Compact`），空结果返回非 nil 切片。归档扫描走 `os.OpenRoot`（不跟随符号链接），只读普通目录、缺 `AGENT.md` 的归档跳过，读取 / 解析失败按 `archive:<id>` 计入（fail-closed），单文件上限 1 MiB |
| D3 | 删除的分段与补偿 | 图 G 顺序：静态校验 → 资源锁 → 指针 404 → `If-Match` **必填**且必须等于已发布版本强 ETag（缺失按 412，零操作行）→ 文件锁 → 重读 → 分歧闸门 409（零操作行）→ 移除块 + 整份候选校验（全部在预写之前）→ `BeginOperation` → 引用闸门（任一命中 400 点名 `data.agents` / `data.active_runs`，操作 failed，零写入）→ 原子写 → gate 外重建候选目录 → gate 写锁内替换目录 + `MarkDeleted`（同事务回填 success，**不**二次 `finish`）→ 释放。重建失败 / 事务失败：身份检查恢复文件、恢复旧目录快照、操作 failed；补偿失败 → unknown |
| D4 | 删除时目标自身凭证缺失会挡住自己的删除 | `providers.go` 追加 `newProviderCatalogFromPublishedExcept(..., skipResourceID)`，与既有构造函数共用**同一条**路径；跳过发生在解析条目、环境变量与构造客户端之前，其余资源仍失败关闭。既有 `newProviderCatalogFromPublished` 改为 `skipResourceID == ""` 的薄包装（不新增第三个目录构造器，§3.1.7） |
| D5 | 本任务只接入模型连接删除 | MCP / 技能删除在 T044 / T051 接入同一签名；此前显式返回 `*core.InvalidInputError`（400），不静默走模型连接路径、不落操作行 |
| D6 | 红态先行（引用闸门与条件写的判别力） | 一轮 `go test -overlay` 双逆改（探针副本建在 `D:/code/.basalt-probe-t040`，仓库外，用毕即删）：① `profileReferencesResource` 的 Provider 分支改 `return false` → `TestResourceDelete_ReferencedByAgent` 逐字红在 `An error is expected but got nil.`；② 删除的 If-Match 判断改为放行空串 → `TestResourceDelete_StaleETag412` 逐字红在 `Expected error with "precondition failed" in chain but got nil.` |
| 交付 | 修改 `internal/app/resource_admin.go`（+`References` / `Delete` / 引用扫描与删除助手）、`internal/app/providers.go`（+`newProviderCatalogFromPublishedExcept`）、`internal/app/resource_admin_test.go`（夹具补 `Root` / `Agents`；+15 例） | 零新增坐标、零迁移；未触碰其他包 |
| 实测 | `go test -run 'TestReferences_\|TestResourceDelete_\|TestResourceRecreate_' ./internal/app` 全绿；`go test ./internal/app -count=1` 全绿（16.4s）；`go vet ./internal/app/` exit=0；`gofumpt -l` 三个改动文件 exit=0 且输出为空 | `TestResourceRecreate_VersionContinues` 按 stage 锚点落地，卡片原文名 `TestResourceRecreate_VersionNoContinues` 以注释保留（T025 D12 的挂账仍由 T042 复核） |
| stage-only | **红**（报告 `accept-20261022T024107.071785400`，exit=1）：`012-app-resource-admin` 仅因 `TestResourceVersionDetail_Projection`（T042 尚未实现）报 `required test did not execute and pass`；本任务新增 15 例全部出现在 `passed_tests` | 非实现缺陷——锚点按整组 check 配置，保留完整判据面，不临时删锚点 |
| 勾选 | 暂不勾选 | 本任务 check 仍红在 T042 的锚点；T042 落地后复跑 `-task T040` 转绿再由控制侧翻转 |

### 1.39 T041 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 删除端点的 handler 面只做四件事：路径名、`If-Match` 形状与逐字原值、委托端口、空 data | 三个包装（`handleAdminDeleteModelConnection` / `handleAdminDeleteMCPServer` / `handleAdminDeleteSkill`）共用 `handleAdminDeleteResource`；资源类型由路由行固定，不靠路径推断。引用闸门、块移除、候选目录重建与事务仍在适配器（图 G），本层**不**先查引用、**不**读盘 |
| D2 | 引用闸门的 400 必须带 `data.agents` / `data.active_runs` | `writeResourceAdminError` 的 `*core.InvalidInputError` 分支改用 `writeAdminDataError`：有白名单键时投影（空数组输出 `[]`），无键时自动退回原 `writeError`，其余错误码路径零变化。`handleAdminResourceReferences` 只做 kind 解析 → `References` → `resourceReferencesView`；未知 kind 与不存在同 404，已删除资源仍可查的分支在适配器（§1.38 D2） |
| D3 | 红态先行（删除映射与条件写的判别力） | 三轮 `go test -overlay` 逆改（探针副本建在 `D:/code/.basalt-probe-t041`，仓库外，用毕即删）：① InvalidInput 分支退回 `writeError` → `TestAdminResources_DeleteReferenced` 逐字红在 `Received unexpected error: unexpected end of JSON input`；② 技能删除路由 `conditional: true → false` → `TestAdminResources_DeleteMissingIfMatch412` 红在固定文案不符（期望中间件文案，实际 handler 文案）；③ 中间件与 handler 双重守卫同时移除 → 同用例红在 `expected: 412 actual: 200` |
| 交付 | 修改 `internal/web/handlers_admin_resources.go`（三个删除包装 + 共用出口 + 引用 handler + InvalidInput 分支带 data）、`internal/web/admin_routes.go`（替换 4 行占位）、`internal/web/handlers_admin_resources_test.go`（fake 补 `deleteFn` / `referencesFn`，+9 例，`MissingPort500` 增 4 个子例） | 零新增坐标、零迁移；未触碰其他包 |
| 实测 | `go test ./internal/web -count=1` 全绿（4.1s）；`go vet ./internal/web/` exit=0；`gofumpt -l` 三个改动文件 exit=0 且输出为空 | — |
| stage-only | **红**（报告 `accept-20261022T024530.120088100`，exit=1）：`012-web-admin-resources` 仅因 `TestSkillValidate_NoSideEffects`（T052 尚未实现；同 check 的 `TestMCP_*` 锚点属 T048）报 `required test did not execute and pass`；`passed_tests` 22 项含本任务 9 个测试函数与 `MissingPort500` 全部子例 | 非实现缺陷——锚点按整组 check 配置；T048 / T052 落地后复跑 `-task T041` 转绿再由控制侧翻转 |
| 勾选 | 暂不勾选 | 本任务 check 仍红在 T048 / T052 的锚点 |

### 1.40 T042 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 版本详情的三个派生字段只从既有读取面取，不新增存储列 | `published` / `applied` 取 `store.Pointer`（含 `deleted_at` 行——删除后历史版本仍可读，卡片 §3.2.1 / 约束 14）；`reverted_from_version_id` 取创建操作的 `RequestedVersionID`（>0 且不等于本版本才置位；操作行缺失保持 nil，不猜测）。`versionID ≤ 0` 与不存在统一 `core.ErrResourceNotFound`（handler 映射 404，不做 400 区分，H2）。只读：不写操作行、不解析环境变量、不构造客户端 |
| D2 | 脱敏扫描必须覆盖两种已落库定义形态且 fail-closed | `redactVersionDefinition` 把定义原文解析成通用树后递归：凭证键（`api_key` / `token` / `secret` / `password`，键名折小写）的值必须是精确 `${VAR}`（`core.EnvPlaceholderName` 唯一判定）；`env` 映射每个值同样必须精确占位；任何字符串值不得携带 URL userinfo；无法解析、凭证值非标量、未知节点类型一律拒绝。错误文案固定、不含命中的值；app 只记 ERROR（kind / resource / version_id），不记正文；失败时 `DefinitionYAML` 为空（不返回部分内容） |
| D3 | handler 只做四步，路由行替换占位 | `handleAdminResourceVersionDetail`：端口缺失 → 500；`parseVersionID` 失败 → 与不存在同一 404（文案 `未找到资源：<原文>`）；委托 `VersionDetail`；`resourceVersionDetailView` 投影。`admin_routes.go` 的占位行替换为本 handler，方法 / 路径 / 受保护属性不变；不写操作行 |
| D4 | 红态先行（脱敏、撤回来源、版本号映射的判别力） | 三轮 `go test -overlay` 逆改（探针副本建在 `D:/code/.basalt-probe-t042`，仓库外，用毕即删）：① 去掉 `redactVersionDefinition` 调用 → `TestResourceVersionDetail_RedactionFailClosed` 六个子例逐字红在 `An error is expected but got nil.`；② 撤回来源条件改为 `false` → `TestResourceVersionDetail_Projection` 红在 `Expected value not to be nil.`；③ handler 的 `if !ok` 改成 `if false && !ok` → `TestAdminResources_VersionDetailBadID404` 红在 `expected: 404 actual: 200`（raw=abc） |
| 交付 | 修改 `internal/app/resource_admin.go`（`VersionDetail` + 递归脱敏助手）、`internal/web/handlers_admin_resources.go`（版本详情 handler + 文件头）、`internal/web/admin_routes.go`（替换 1 行占位）、`internal/web/handlers_admin_resources_test.go`（fake 补 `versionDetailFn`，+4 例，`MissingPort500` 增 1 个子例）、`internal/app/resource_admin_test.go`（+5 例与两个造行助手） | 零新增坐标、零迁移；未触碰其他包；未改契约字段 |
| 实测 | `go test ./internal/app/ ./internal/web/ -count=1` 全绿（15.99s / 3.65s）；`go vet ./internal/app/ ./internal/web/` exit=0；`gofumpt -l` 五个改动文件 exit=0 且输出为空 | — |
| stage-only | **红**（报告 `accept-20261022T025212.330719500`，exit=1）：`012-app-resource-admin` passed（含锚点 `TestResourceVersionDetail_Projection` 与全部 5 个新测试函数 / 子例）；`012-web-admin-resources` 仅因 `TestSkillValidate_NoSideEffects`（T052 尚未实现；同 check 的 `TestMCP_*` 锚点属 T048）报 `required test did not execute and pass`；其余 `TestAdminResources_*`（含 4 个新测试函数与 `MissingPort500/version-detail`）全部 passed | 非实现缺陷——锚点按整组 check 配置；T048 / T052 落地后复跑 `-task T042` 转绿再由控制侧翻转 |
| 勾选 | 暂不勾选 | 本任务 check 仍红在 T048 / T052 的锚点 |

### 1.41 T043 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | T043 的交付面（删除 / 引用 / 版本列表 / 版本详情）要求端口已填槽可走通，而 `web.ResourceAdminService` 是 13 方法接口，MCP / 技能 6 方法属 T048 / T052 | 对 T027「空槽保持 nil」的**关系调整**：本次填槽，`resourceAdminPort` 7 方法真转调（`UpsertModelConnection` 经只读投影回读视图；`Delete` / `Publish` / `Versions` / `VersionDetail` / `References` / `TestModelConnection` 直转 `ResourceAdmin`），其余 6 方法返回 `errResourceAdminUnassembled`——web 侧落既有缺端口路径的 500 固定文案，且对应路由仍由 `handleAdminUnassembled` 占位，运行期到不了桩。Go 无部分实现接口，T043 的目的正是让这四个端点可达；桩是失败关闭，不是「空实现冒充可用」 |
| D2 | 端口的 `Versions` 需要 `[]core.ResourceVersionDetail`，而卡片点名的 `ResourceAdmin.Versions` 返回 `[]core.ResourceVersion` | 指针状态 / 撤回来源的派生抽成 `versionFlags`，由 `VersionDetail` 与新的未导出 `versionDetails` 共用（单一派生实现）；列表投影**不做**定义脱敏扫描（列表不携带定义）。导出面仍是卡片点名的 `Versions`，不新增对外概念 |
| D3 | 三项新依赖的来源 | `Root` = 装配期 `root`（`<workDir>/.basalt`，归档扫描边界）；`Agents` = `agents.Entries()`（只读快照，归档 / 恢复后随注册表更新）；`ProviderOverride` = `a.providerOverride`（与启动装配同一实例，评测注入路径不变） |
| 交付 | 修改 `internal/app/resource_admin.go`（`Versions` / `versionsAndPointer` / `versionDetails` / `versionFlags`；`VersionDetail` 改走共用派生）、`internal/app/web.go`（`errResourceAdminUnassembled` + 13 方法 `resourceAdminPort`）、`internal/app/wire.go`（补三项依赖 + 填 `writeServices.resourceAdmin`，删「空槽保持 nil」旧注释）、`internal/app/wire_test.go`（+1 例 `TestWire_ResourceAdminPortWiring`，补 `web` 导入）、`internal/app/resource_admin_test.go`（+4 例 V6–V9，文件头清单 V1–V9） | 零新增坐标、零迁移；未触碰接口字面量、路由表与契约；`TestWritePorts_FailClosedWithoutAdapters`（半截运行期仍全 nil）与 `TestWritePorts_IndependentOfContentEditor` 原样通过 |
| 红态 | 两轮 `-overlay` 逆改（探针副本 `D:/code/.basalt-probe-t043`，仓库外、`perl -0pi -e` 施加并 grep 核对生效，用毕即删）：① `web.go` 端口 `Versions` 的 `return p.admin.versionDetails(...)` → `return nil, nil` → `wire_test.go:1164` 逐字红 `Should NOT be empty, but was []`；② `wire.go` 的 `Agents` 返回值改为 `nil, nil` → `wire_test.go:1153` 红 `Not equal`（expected 1 条 `reader`，actual `[]core.AgentEntry(nil)`） | 判别力成立：分别识破「端口没接到编排实例」与「注册表快照漏传」 |
| 实测 | `go test ./internal/app ./internal/web -count=1` → `ok app 15.598s` / `ok web 3.655s`；定向 5 例 `-v` 全 PASS（`TestResourceAdmin_Versions_*` 4 + `TestWire_ResourceAdminPortWiring` 1，2.043s）；`go vet ./internal/app/ ./internal/web/` exit=0；gofumpt 本轮四文件 exit=0 | — |
| stage-only | 不适用：stage 的 `tasks` 表无 T043 绑定（任务无专属 check）；其行为由 `012-app-resource-admin`（T040 / T042 锚点）与 `012-web-admin-resources`（T041 端点）覆盖 | 与 tasks.md 一致；不为装配任务临时新增判据面 |
| 勾选 | 暂不勾选 | 本任务无专属 check 可翻转；按 T027 先例归入整组 check 转绿后的控制侧勾选批次（桩由 T048 / T052 替换） |

### 1.42 T044 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 两条读取路径共用一个语义核心，`allowLegacy` 是唯一差异 | `ParseMCPServers(root)` 保持既有文件入口（`agent.go` 两个调用点零改动，约束 47）；新增导出 `ValidateMCPServerCandidate(cfg)` 走严格路径。二者都进 `normalizeMCPServer(cfg, index, prefix, allowLegacy)`：传输分支必填 / 互斥、command 单可执行名、args 凭证、env 精确占位只写一份。兼容路径的多词 `command` 用 `strings.Fields` 拆分（与旧 `tool/mcp.go` 逐字等价），严格路径报 `单个可执行名——请把参数移入 args 字段`。YAML 条目先由 `mcpConfigFromEntry` 做必填 / 类型检查，再进同一核心 |
| D2 | 兼容拆分只记可执行名与参数个数 | `command` 可能夹带明文凭证，WARN 只带 `file` / `server` / `command`（argv[0]）/ `args`（个数），**不**整体回显旧 command。解析层无 logger 注入，用 `slog.Default()`；测试 `slog.SetDefault` + `t.Cleanup` 还原（与 `core/session_test.go` 同法） |
| D3 | 脱敏纪律落在错误文案上 | URL 错误固定短语 `必须是不含 userinfo 的 http / https 绝对地址`，不回显 URL；args 错误只点名 `args[i]` 与开关名，不回显取值；env 错误只点名 `env.<KEY>`。既有用例 `TestParseMCPServers_RejectsPlaintextEnvWithoutEchoingValue` 继续通过 |
| D4 | 判据面调整（已审范围内） | 既有 `TestParseMCPServers_RejectsUnsupportedTransport` 的固定文案断言 `v1 只支持 stdio` → `只支持 stdio / http`：约束 8 已批准新增 http 传输，旧文案不可能保留；断言强度不变（仍逐字匹配固定短语）。不删断言、不放宽阈值 |
| D5 | 重名判定在传输分支之前 | 一个 stdio 与一个 http 同名同样拒载（名字是 server 唯一标识），错误文案 `server 名重复: <name>` 保持 US-7 原文；新用例 `TestParseMCPServers_DuplicateNamesAcrossTransports` 固定该语义 |
| 交付 | 修改 `internal/workspace/mcp.go`（重写：共有核心 + 候选入口 + URL / args / env 校验）、`internal/workspace/mcp_test.go`（+8 例，补 `bytes` / `log/slog` / `strings` / `core` 导入）；修改 `harness/stages/us12-agent-resource-management.json`（见 stage-only 行） | 零新增坐标、零迁移；未触碰 `agent.go` 调用点、`core.MCPServerConfig` 字面量与契约 |
| 红态 | 两轮 `go test -overlay` 逆改（探针副本 `D:/code/.basalt-probe-t044a` 与 `...-t044b`，仓库外、`perl -0pi -e` 施加并 grep 核对生效，用毕即删）：① `validateMCPURL` 去掉 `u.User != nil` → `TestParseMCPServers_RejectsUserinfoAndPlaintextTokenArgs` 逐字红在 `mcp_test.go:568 An error is expected but got nil.`；② `validateMCPArgs` 调用改为 `false && err != nil` → 同用例逐字红在 `mcp_test.go:575 An error is expected but got nil.` | 判别力成立：分别识破「userinfo 放行」与「明文凭证参数放行」 |
| stage-only | 首跑**红**（报告 `accept-20261022T030556.672017200`，exit=1）：`012-workspace-yaml-provider` 报 `required test did not execute and pass: TestParseMCPServers_LegacyCommandSplit`——stage 的 `required_tests` 写的是截短名。按卡片 §4.4（required_tests 必须按 `go test -list` 展开为精确函数名）与约束 8 点名的四个复核用例修正：`TestParseMCPServers_LegacyCommandSplit` → `TestParseMCPServers_LegacyCommandSplitWarns`，并补齐 `TestParseMCPServers_CandidateMatrix` / `TestParseMCPServers_RejectsUserinfoAndPlaintextTokenArgs` / `TestParseMCPServers_LegacyAmbiguousRejected` 三个锚点。复跑**绿**（报告 `accept-20261022T030614.348647800`，exit=0，fmt + `012-workspace-yaml-provider` 双 passed） | 非实现缺陷；锚点修正是控制侧按已审卡片对齐，不新增验收语义（四个测试均为卡片点名用例且已绿） |
| 实测 | `gofumpt -w` 两文件 exit=0；`go vet ./internal/workspace/` exit=0；`go test ./internal/workspace -count=1` → `ok 13.011s`；定向 `-run '^TestParseMCPServers_' -v` 22 个测试函数（含子例）全 PASS，8 个新函数逐个 PASS | — |
| 勾选 | **已勾选** | 专属 check `012-workspace-yaml-provider` 本轮通过 |

### 1.43 T045 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | `dialCommand` 全量改用 `exec.Command(spec.Command, spec.Args...)`，`strings.Fields` 已从 `internal/tool` 删除 | `Command` 只做 `TrimSpace` 后作为单个 argv[0]，参数逐项来自 `Args`（约束 8：命令与参数在解析层已分开，工具层再拆会绕过严格校验）。空白 command 构造期 fail-closed（`command 为空`）。`exec.Command` / `Cmd.Dir` / `mergeChildEnv` / `Cancel == nil` 的既有语义不变（约束 17 / 18 / 27） |
| D2 | 判据面调整（已审范围内） | 既有 `TestMCPPool_DialCommandEnvAndDir` 的前提「command 按空白切分」被约束 8 取代：夹具改为 `Command: "gh-mcp", Args: ["--stdio","extra"]`，期望 argv 与断言强度不变（仍逐项等值）。不删断言、不放宽阈值 |
| D3 | stage 的 `012-app-mcp-supervisor` 只跑 `./internal/app`，而卡片 §4.4 的族 `TestMCP*` 显然包含 `internal/tool` 的 `TestMCPPool_*` / `TestMCPToolAdapter_*`，T045 的 5 个用例无处执行 | 派生工件缺陷（同 T020 / T036 先例）：控制侧把 argv 扩为 `./internal/tool ./internal/app`，并补两个工具层锚点 `TestMCPPool_DialCommandArgsVerbatim` / `TestMCPPool_DialCommandNoWhitespaceSplitting`；既有锚点一字未动。不改卡片正文 |
| 交付 | 修改 `internal/tool/mcp.go`（`dialCommand` + 两处注释）、`internal/tool/mcp_test.go`（改 1 例夹具 / 注释，+5 例：`ArgsVerbatim` / `NoWhitespaceSplitting` / `EmptyCommand` / `CapturesSnapshotAtCallTime` / `ConnectToolsFollowSnapshot`）；修改 stage 的 `012-app-mcp-supervisor` argv 与锚点 | 零新增坐标、零迁移；未改 `core.MCPServerConfig`、未改 `MCPPool` 导出面 |
| 红态 | 两轮 `go test -overlay` 逆改（探针副本 `D:/code/.basalt-probe-t045a` 与 `...-t045b`，仓库外、`perl -0pi -e` 施加并 grep 核对生效，用毕即删）：① 恢复 `strings.Fields(command)` → `TestMCPPool_DialCommandNoWhitespaceSplitting` 逐字红在 `mcp_test.go:906`（expected `["npx -y @scope/server"]`，actual `["npx","-y","@scope/server"]`）；② 把 Args 用空格拼接再 `Fields` → `TestMCPPool_DialCommandArgsVerbatim` 逐字红在 `mcp_test.go:884`（含空格的参数被拆成多个元素、末尾空串丢失） | 判别力成立：分别识破「工具层重新拆分 command」与「参数不以逐项边界传递」 |
| 实测 | `gofumpt -w` 两文件 exit=0；`go vet ./internal/tool/` exit=0；`go test ./internal/tool -count=1` → `ok 2.820s`；回归 `go test ./internal/app ./internal/workspace -count=1` → `ok 16.384s` / `ok 11.807s` | — |
| stage-only | **红**（报告 `accept-20261022T030827.807848500`，exit=1）：两个工具层新锚点均 passed；`012-app-mcp-supervisor` 仅因 `TestMCPSupervisor_PolicyDenied`（T047 尚未实现）报 `required test did not execute and pass` | 非实现缺陷——锚点按整组 check 配置；T046 / T047 落地后复跑 `-task T045` 转绿再由控制侧翻转 |
| 勾选 | 暂不勾选 | 本任务 check 仍红在 T046 / T047 的锚点 |

### 1.44 T046 / T047 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 逐字用例 `TestMCPSupervisor_PolicyDenied`（卡片 §4.3.5）要求 `errors.Is(err, core.ErrPolicyDenied)`，而 `internal/core/errors.go` 没有该哨兵 | 新增哨兵 `ErrPolicyDenied`（US-12 supervisor 判定锚点，独立于管理写面分类），`Start` / `Test` 以 `fmt.Errorf("%w: %w", core.ErrPolicyDenied, denied)` 包装——`errors.Is` 对哨兵与 `*SandboxViolationError` 同时成立，四段式固定文案原样保留（不带凭证） |
| D2 | supervisor 连接成功后要把 `applied_version_id` 写入数据库，但 `internal/app` 不应让 supervisor 直接依赖具体存储实现，且启动时 applied 追平 published 需要一条路径 | deps 注入 `MarkApplied func(ctx, name, versionID) error`：装配层用 `store.Publish(kind, name, versionID, applied=true, pointer.OperationID, now)`（启动时 published == 目标版本，FK 由指针的发布操作满足）；`Test` / 单元测试传 nil → 只更新内存状态。**调用点固定在 `Gate.Lock()` 内**（约束 17 / 42） |
| D3 | 卡片 §2.3 错误矩阵对 MCP 策略拒绝写「审计行」，但 `run-and-errors.md` §9.3 的动作字面量是闭集（无启动 / 应用动作），§9.3 且禁止把管理审计与调用审计混写 | 启动 / 测试授权落**结构化审计事件**（固定键 `event` / `server` / `transport` / `version_id` / `decision` / `reason` / `at`）经 logger + 可选 `Audit func(MCPAuditEvent)` 回调；**不**补伪操作行、**不**写 `llm_calls`。记入 T049 的 D 项，不改契约 |
| D4 | 约束 19 / 22 要求 stdio 停止与测试收尾「清理进程树」，而真实进程由 SDK `CommandTransport` 在 `Connect` 内启动，池层只持有 transport | 进程树回收落在 `internal/tool` 的 `MCPPool`：`dialCommand` 记录 `*exec.Cmd`、Unix `Setpgid`、Close / 连接失败路径按平台杀整树（Windows `taskkill /T /F /PID`，Unix `kill(-pgid)`）；新增两个 build tag 文件 `mcp_process_windows.go` / `mcp_process_unix.go`。先杀树后 `session.Close`——Windows 上父进程一死子关系即消失，顺序反过来就清不到孙进程；主动终止后 Close 的退出错误是预期噪声，单列注释说明 |
| D5 | 并发与锁顺序（约束 42）要求「不得持有 supervisor 锁等待 gate」 | 每 server 一个 `entry.mu`（启停串行）＋ 全局 `s.mu`（仅护 map）；`Start` 在 `entry.mu` 内过策略与连接，**释放后**再 `Gate.Lock()` → `entry.mu`（顺序 gate → supervisor）→ `MarkApplied` + 状态写入；`Stop` 只取 `entry.mu`，幂等；`epoch` 防「连接期间被 Stop / 新 Start 顶替」后仍写 applied |
| D6 | T046 要保留 `mcpConnector` / `newMCPRuntime` 既有测试夹具（`wire_mcp_test.go` / `schedule_test.go` / `resources_test.go` 均引用），而 supervisor 接管后运行时来源变了 | 修订 `mcp.go` 时保留 `mcpConnector` 接口与 `newMCPRuntime` 形态，新增 `newMCPRuntimeFromSupervisor` 与 applied / pending 访问器；4 条新用例只加在 `mcp_test.go`，既有夹具不改 |
| D7 | stage `012-app-mcp-supervisor` 的 `required_tests` 引用了 `TestCommandRunner_SingleCommandOnly`，仓库任何位置都没有该用例；卡片约束 19 的复核名是 `TestCommandRunner_NotUsedForMCP` | 判定为 stage 派生笔误：按卡片名落地静态断言用例 `TestCommandRunner_NotUsedForMCP`，`required_tests` 同步改正；同一 check 的正则 `^(TestMCP\|TestCommandRunner_)` 不变、仍要求逐条执行并通过，判据强度不变。测试端点四例同步对齐卡片约束 22 的复核名（`TestMCPTest_NoRunNoLLMCall` / `TestMCPTest_StdioCleansProcessTree` / `TestMCPTest_HTTPNoProcess` / `TestMCPTest_Timeout`），目标名全部原样保留 |
| 交付 | 见本条目下方的实施行 | — |
| 交付（T045–T047） | 新增 `internal/app/mcp_supervisor.go` / `mcp_supervisor_test.go`（17 例）与 `internal/tool/mcp_process_windows.go` / `mcp_process_unix.go`；`internal/app/mcp.go` 增 supervisor 句柄与 `newMCPRuntimeFromSupervisor` / `Tools` / `AppliedVersion` / `PendingRestart`（`mcp_test.go` +4）；`internal/tool/mcp.go` 记录 `*exec.Cmd`、失败路径与 Close 先杀树、`mcp_test.go` +1（`TestMCPPool_KillProcessTree`）；`internal/core/errors.go` 增 `ErrPolicyDenied` | stage `012-app-mcp-supervisor` 对 T045 / T046 / T047 均 passed（`accept-20261022T032721` / `T032712` / `T032702`） |
| 红态（T047 策略先行） | `-overlay` 探针（仓库外）把 `Start` 的 authorize 调用改为 `error(nil)`：`TestMCPSupervisor_PolicyDenied` 报 `err = <nil>, want ErrPolicyDenied`；`TestMCPSupervisor_PolicyDeniedDoesNotStart` 报 `Expected error with "mcp policy denied" in chain but got nil` | 判别力成立；探针用毕已删 |
| 红态（T045 进程树） | `-overlay` 探针跳过 `taskkill` 调用：`TestMCPPool_KillProcessTree` 报 `expected: 41 / actual: 79`，消息 `孙进程仍在写心跳——进程树未被整棵回收` | 判别力成立；探针用毕已删 |

### 1.45 T048 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 上一窗口把 `TestResourceDelete_UnsupportedKind` 的用例类型从 `core.ResourceMCPServer` 改为 `core.ResourceSkill`：MCP 删除已由 T047 / T048 接入，不再是「不支持的类型」 | 已审范围内的派生适配——`Delete` 当前接受 `model_connection` / `mcp_server`，对技能显式拒绝（`resource_admin.go:1421-1428` 注释写明 T051 接入）；断言与错误分类未变。**前向注记（T051 落地时必须同步修订）**：T051 接入技能删除后三类字面量全部可删，该负例需改用合成类型 `core.ResourceKind("agent")`（仍先命 `ParseResourceKind` → `*core.InvalidInputError`，断言不变）；届时若 `T051` 复核本行未落实，按「判据面变更」停下裁决 |
| D2 | `resourceEnv.setApplied` 用直连 SQLite 的 UPDATE 模拟「已生效版本」（`resource_admin_test.go:1695-1698`），因为 supervisor 的 applied 回写尚未由装配层接线 | 夹具层权宜：T049 在 `wire.go` 注入 `MarkApplied` 并做启动追平后，该 helper 仍保留用于构造「待重启（applied ≠ published）」场景；生产路径不得出现第二套写入（约束 17 / 42），接线后复核一次 |
| D3 | `internal/app/wire_test.go` 的 `TestWire_ResourceAdminPortWiring` 原先断言 MCP 端口方法返回 `errResourceAdminUnassembled`；端口改为真实转发后断言会假红 | 按已批准接口适配：改用空标识触发 `*core.InvalidInputError`（校验先于任何写入与 supervisor 交互），并补 `NotErrorIs(errResourceAdminUnassembled)` 证明真实适配器被触达；技能族（4 方法）仍断言失败关闭，待 T052 替换 |
| D4 | stage 草稿锚点名 `TestMCPViews_Redacted` 在全仓与模块卡中均不存在；模块卡约束 23 点名的脱敏回归是 `TestMCPViews_NoEnvValues`（web）与 `TestMCPError_NoSecrets`（app），后者此前未落地 | 按 T003 / T044 先例以模块卡精确名为准：web 用例改名（断言不删不改），stage 的 `012-web-admin-resources` 同步改名；新增 `TestMCPError_NoSecrets` 并把其加入 `012-app-mcp-supervisor` 的 `required_tests`（只收紧，既有锚点一字未动） |
| 交付（T048） | `internal/web/handlers_admin_resources.go` 新增 `handleAdminCreateMCPServer` / `handleAdminUpdateMCPServer` / `handleAdminTestMCPServer` 与 `messageMCPServerBody`；`internal/web/admin_routes.go` 挂四条 MCP 路由（更新 / 删除为条件写）；`internal/app/web.go` 的 `UpsertMCPServer` / `TestMCPServer` 改为真实转发（九法已实现，技能族仍失败关闭）；`handlers_admin_resources_test.go` +6 例（含 `TestMCPViews_NoEnvValues` / `TestMCP_StaleETag412`）；`resource_admin_test.go` +1 例（`TestMCPError_NoSecrets`）；`wire_test.go` 适配（D3） | stage `012-web-admin-resources` 复跑（`accept-20261022T035645.158334800`）：`fmt` passed；本任务锚点 `TestMCPViews_NoEnvValues` / `TestMCP_StaleETag412` / `TestAdminResources_*` / `TestModelConnection_*` 逐条 passed，唯一红为 T052 锚点 `TestSkillValidate_NoSideEffects`——按既有先例不翻勾选 |
| 红态（T048 秘密回显） | `-overlay` 探针（仓库外）把 connect 失败分支的 `ErrorMessage` 从固定文案改为 `err.Error()`：`TestMCPError_NoSecrets` 报 `expected: "连接失败" / actual: "connect \"https://svc:s3cret@mcp.example/mcp?token=t0ken\" failed"`（`resource_admin_test.go:2007`） | 判别力成立（逐字泄露文案）；探针用毕已删 |
| 红态（T048 过期 ETag 映射） | 同一探针把 MCP 更新失败分支的 `writeResourceAdminError` 改为 `writeOK`：`TestMCP_StaleETag412` 报 `expected: 412 / actual: 200`（`handlers_admin_resources_test.go:1147`） | 判别力成立；探针用毕已删 |

### 1.46 T049 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 图 A **标题**、T049 任务文本与 FR-48 写「注册表 → supervisor」，图 A **正文**把 ⑧ supervisor 排在 ⑨ registry 之前；两序不可能同时成立 | 按正文实现（`wire.go:newRuntime`：目录 → registry → supervisor → OnConnected → 服务开放）：约束 21 要求"Agent 加载期引用的 MCP 必须已应用且已连接"，连接必须发生在装载判定之前。**不改** FR-48 正文；本行即裁决记录，供收口复核 |
| D2 | 第一遍装载发生在 supervisor 之前，此时没有任何连接结果 | `MCPServers` / `MCPConnected` 都置为文件序名集（乐观占位）——否则 `TestWire_AgentDeclaringFailedServerOnlyIsNotRegistered` 会退化成第三态「Agent 注册待确认」（被该用例明令禁止）；第二遍装载按 `mcpRT.ConfiguredNames()` / `ConnectedNames()` 重判，注册结论只采信第二遍 |
| D3 | T049 任务文本点名修改 `run.go`，但装配顺序不在 `run.go` | `run.go:122` 只调用 `a.newRuntime(ctx)`；顺序全部在 `wire.go:newRuntime`。`run.go` **零改动**——为"任务文本提及"而改它等于制造无关 diff |
| D4 | `heavyRuntime` 同时保留旧 `mcp`（`mcpConns`）与 `mcpSupervisor` | 旧路径只服务无 supervisor 的测试夹具与旧装配分支；生产装配只走 supervisor（`mcpRT` 由 `newMCPRuntimeFromSupervisor` 构造），`Close()` 先 supervisor 后旧池 |
| D5 | supervisor 连接成功后需要把工具送进注册池，但注册池在 supervisor 之前已建好 | `MCPSupervisorDeps.OnConnected func([]tool.MCPConnection)`：`Start` 在释放 `entry.mu` 之后、`markApplied` 之前回调装配层注册工具；回调失败不回滚连接（连接已成立，回滚反而留半开状态） |
| D6 | 连接期间可能发生新发布 | `markMCPApplied` 只在 `pointer.PublishedVersionID == versionID` 时写 applied；已被更新发布取代 / 已删除时跳过，**不得**把指针写回旧版本（倒退发布） |
| D7 | 测试夹具按旧"一次 Connect 连接全部 server"写，supervisor 改为每 server 一次 Connect | `fakeMCPConnector.Connect` 改为按请求 server 名过滤罐装连接 + 累积（覆盖只会留最后一台）；`TestWire_MCPConfigChangesNeedRestart` 断言改为 `[beta, alpha]`（新文件条目 ∪ 指针保留项，FR17 已批准语义，不是热加载）；`writeMCPServersYAML` 用 `unused-mcp`，探针用例补真实连接 |
| D8 | 启动对账要求 `PublishVersion` 能按资源种类决定是否推进 applied | `ReconcileDeps.PublishVersion` 增加 `applied bool`（Provider / 技能传 `true`，MCP 传 `false`），零值走 `Store.PublishWithVersionState`；新增必填 `MCPServersPath`——缺失会静默跳过整类资源，正是对账最该避免的漏判 |
| D9 | 旧代码把 MCP 对账推迟到"T044–T049 引入事务性发布 API"，`warnMCPReconcileDeferred` 只记 WARN | 本任务删除该占位并落地 `reconcileMCPServers`（约束 34 的 MCP 行）：与 Provider 同一张决策表（一致 / 文件新 / 非法 / 条目被删 / 文件缺失），唯一差别是发布 `applied=false`（约束 21：supervisor 连接成功才追平）。占位解析（`resolveMCPEnv`）在发布前完成，失败只点名键与变量名 |
| D10 | **已知设计张力（未改）**：US-11 的 `baseline.go:scanResource` 对"已发布但未 applied"直接补 applied（含 MCP），发生在 supervisor 连接之前 | 重启场景下，一次"对账发布 v2 → 连接失败"之后，下次启动的基线会把 applied 补成 v2，使 `pending_restart` 在从未连接成功时显示 false——与约束 21"应用版本只在成功连接并接管后写入"相左。`baseline.go` 不在本卡 20 个修改文件清单内，**不**静默修改；已进未验证清单 U9，交收口裁决 |
| 交付（T049） | `internal/app/wire.go`：`MCPSupervisorDeps` 全字段接线 + `NewConnector` 测试缝 + `ApplyPublished` + `newMCPRuntimeFromSupervisor` + `OnConnected` 注册工具 + `ResourceAdmin.Supervisor` + `heavyRuntime.mcpSupervisor`；`internal/app/mcp.go` / `mcp_supervisor.go`（前序窗口）补齐 `MarkApplied` / 状态快照；`internal/app/definition_reconcile.go` 接入 MCP 分支（D9）+ `MCPServersPath`；`definition_reconcile_test.go` +6 例（R13–R18，MCP 分支五种决策 + 拒绝启动）与夹具 5 参 `PublishVersion`；`wire_mcp_test.go` 夹具按 D7 适配；stage 补 T049 绑定（同 T037 先例：无绑定会 `unknown stage task`，只加绑定不改判据） | 见下行实测 |
| 实测（T049） | `gofumpt -w` 四文件 exit=0；`go vet ./internal/app/` exit=0；`go test ./internal/app -count=1` → `ok 16.228s`；`TestReconcile_*` 18 例逐条 PASS（含 R13–R18）；`TestWire_*` / `TestStartupOrder_*` 25 例逐条 PASS；stage `012-app-mcp-supervisor`（`accept-20261022T041950.774089600`）passed | `012-app-reconcile` 仍红在 T030 / T067A 尚未落地的 `TestStartupReconcile_*` / `TestRecoverAdmin_*` 锚点（既有台账记录），不是本任务缺陷 |
| 红态（MCP 发布推进 applied） | `-overlay` 探针（仓库外）把 `publishMCPVersion` 的 `applied` 从 `false` 改为 `true`：`TestReconcile_MCPExternalEditPublishesPendingRestart` 报 `expected: 1 / actual: 2`，消息 `MCP 对账发布不得推进 applied——新内容要等 supervisor 连接成功才生效`（`definition_reconcile_test.go:691`） | 判别力成立；探针用毕已删（`/tmp/tmp.hSL3OLtoxh`） |
| 未验证 | 约束 21 与基线的张力（D10）无用例覆盖：判据应落在"重启 + 连接失败后 `pending_restart` 仍为 true"，需改 `baseline.go`（当前不在批准修改面） | 进 §4 未验证清单 U9，交收口裁决，不自行改基线 |
| 勾选 | **翻转** T049 | 本任务无专属 check；判据 = 全包测试 + `012-app-mcp-supervisor` 绿 + 红态证据 |

### 1.47 T050 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 卡片 §3.1.4 把 `ValidateSkillDraft` 的形参写成 `[]AuthorFile`，而该类型在 `internal/core`（`agent_draft.go` 全部调用点为 `[]core.AuthorFile`） | 按仓内类型落地为 `[]core.AuthorFile`；签名语义（路径 + 内容字面量）不变，属已审范围内的派生适配 |
| D2 | 卡片未规定"技能名非法"归哪类错误，而写入口需要可映射的 400 | 名字规则复用 M9 裁决后的 `ValidateAgentName`（前导点 / `.staging` / `.backup` 后缀 / 分隔符 / NUL）；`ValidateSkillDraft` / `WriteSkillWorkCopy` 归入 `ErrImportRejected`（400），`ArchiveSkillWorkCopy` 返回普通错误（app 层先校验名字） |
| D3 | `SnapshotSkill` 的字节落盘走既有 `SkillLibrary.Materialize`（要求工作副本哈希与候选一致、已存在版本按哈希幂等复用）；只读化不由它负责 | 只读化（文件 0o444、目录 0o555 自底向上）留在 `SnapshotSkill`；`skill_version.go` 零改动。manifest 序列化规则在 `buildSkillCandidate` 本地镜像，并以 `sha256Hex(ManifestJSON) == ContentHash` 断言钉住 |
| D4 | 归档根与时间戳沿用 Agent 归档口径（`archiveDirName` / `archiveStampLayout`，UTC）；约束 12 要求同名重建继续版本号 | 技能归档落 `archive/skills/<name>-<stamp>`；目标已存在 → `*core.AdminError{Kind: core.ErrConflict}`（409 语义），源目录原样保留，不自动换名 |
| D5 | 约束 24 的隐藏段规则（M9 裁决）与 Agent 写面路径规则不同：Agent 允许普通文件带点扩展名，技能导入连隐藏文件也拒绝 | `normalizeSkillFilePath` = `normalizeAgentFilePath` + 逐段拒绝以点开头的段；不复制 Agent 的路径实现（共用一份判据，只加一层） |
| 交付（T050） | 新增 `internal/workspace/skill_admin.go`（`SkillFile` / `SkillCandidate` / `ValidateSkillDraft` / `WriteSkillWorkCopy` / `SnapshotSkill` / `ArchiveSkillWorkCopy`；导入边界、staging 全量替换、只读版本快照、删除归档）与 `skill_admin_test.go`（20 例，含 `TestSkillImport_TextOnly` 与文档点名的 `TestSkillUpdate_FullReplacement` / `TestSkillUpdate_CrashResidue` / `TestSkillVersion_Immutable` / `TestSkillValidate_SameGateAsImport`） | 文档计划 16 例，按实测补足到 20 例（M3 裁决：以实际发现和执行的测试为准） |
| 实测（T050） | `gofumpt -w` 两文件 exit=0、`gofumpt -l` 零输出；`go vet ./internal/workspace/` exit=0；`staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./internal/workspace/` exit=0 零输出；`go test ./internal/workspace -count=1` → `ok 13.148s`，新族 20 例逐条 PASS | 另外 `TestSkillPaths_*` / `TestSkillVersion_*` / `TestSkillLibrary_*` 等既有 17 例同批 passed（无回归） |
| stage-only | **红**（报告 `accept-20261022T042640.610984800`，exit=1）：`012-workspace-skill` 的 `TestSkill*` 全部执行并通过，唯一失败为 `required test did not execute and pass: TestNotifyScan_WordBoundary` | 非实现缺陷——该锚点属 T054（`harness/stages/us12-agent-resource-management.json` 任务行 T054）；按 T011（§失败台账 2026-10-22 行）先例记录待兄弟任务落地后复跑 `-task T050`，不临时删锚点、不勾选 |
| 红态（导入文本闸门） | `-overlay` 探针（仓库外 `%TEMP%\us12-t050-probe`）把 NUL 判定改为恒假：`TestSkillImport_TextOnly` 报 `An error is expected but got nil.`（`skill_admin_test.go:154`，经 `requireImportRejected`） | 判别力成立；探针用毕已删（目录清点 0） |
| 红态（版本只读） | 同一手法把 `skillVersionFilePerm` / `skillVersionDirPerm` 改为 0o644 / 0o755：`TestSkillVersion_Immutable` 报 `An error is expected but got nil.` + 消息 `已发布版本文件必须只读`（`skill_admin_test.go:463`） | 判别力成立；探针用毕已删 |
| 未验证 | `TestSkillSnapshot_RejectsSymlink` 在本机按平台约定跳过：`os.Symlink` 报 `A required privilege is not held by the client`（与 U8 同源，Windows 非开发者模式） | 进 §4 未验证清单 U10；用例在 CI（Linux）或开发者模式环境执行 |
| 勾选 | 暂不勾选 | 本任务 check 仍红在 T054 的 `TestNotifyScan_WordBoundary`；T054 落地后复跑 `-task T050` 转绿再由控制侧翻转 |

### 1.48 T051 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 图 D 把「同名创建 409 / 工作副本分歧 409」画在 `BeginOperation` **之前**，且卡片失败表（L883）写「不写操作行」；而模型连接的同类失败在预写之后必须留 failed 行并返回 `operation_id` | 技能侧按图 D 落地：两类 409 都在 `BeginOperation` 之前返回，零操作行；预写之后的任何失败（校验回落、快照 / 事务）一律 `FinishOperation(failed)` 并带 `operation_id`。用例逐条断言操作行计数不变 |
| D2 | 卡片未写「重建 / 撤回 / 幂等发布」时 `nextNo` 的来源 | 全程持 `resourceMu("skill", name)`：`skillVersions` 按 `version_no DESC, id DESC` 读最新，`nextNo = versions[0].VersionNo + 1`；删除后重建复用被删指针行（`publishTx` 清 `deleted_at`），版本号继续递增（约束 12） |
| D3 | `publishSkillVersion` 需要在「发布最新」与「发布旧版本」两条路径上共用同一事务 | 统一走 `publishVersion(..., applied=true, ...)`：无 `VersionID` 时若最新已发布则幂等成功（仍写一条 success 操作行、不新建快照）；带旧 `VersionID` 时从版本目录字节经 `readSkillFiles` 重建候选，要求 `ContentHash` / `ManifestJSON` 与目标版本一致，按 `nextNo` 快照后发布，`record.RequestedVersionID` 记来源 |
| D4 | 删除需要「引用闸门 → 归档 → 事务」且事务失败要回滚归档 | 顺序：If-Match（缺 → 412）→ 分歧 409 → `BeginOperation` → 引用闸门（含归档 Agent 与未释放运行引用，400 + failed 行 + `detail_json.agents`）→ `ArchiveSkillWorkCopy` → `gate.Lock()` + `store.MarkDeleted`；事务失败用 `restoreSkillArchive`（身份校验后 rename 回）恢复，恢复不了标 `OperationUnknown` |
| 交付（T051） | `internal/app/resource_admin.go`：`CreateSkill` / `ImportSkill` / `UpdateSkill` / `ValidateSkill`、`writeSkill`（staging `.basalt/skills/<name>.staging/` + 两次 rename）、`skillWorkCopyState`、`abortSkillWrite`、`skillDetail`、`publishSkillVersion`、`deleteSkill`、`restoreSkillArchive`、`removeSkillWorkCopy` / `removeSkillSnapshot`（Windows 只读清理）；`resource_admin_test.go` 追加 13 例（K1–K13）并改写 `TestResourceDelete_UnsupportedKind`（D1 台账项：改用 `core.ResourceKind("agent")`） | 断言面全部落在真实 SQLite 行 + 真实磁盘字节（工作副本 / 只读版本目录 / 归档目录）；固定时钟 `baselineClock()` 使归档名可判 |
| 实测（T051） | `gofumpt -l` 零输出；`go vet ./internal/app` exit=0；项目 staticcheck `-checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./internal/app` exit=0；`go test ./internal/app -count=1` → `ok 17.146s`；13 例新增与改写例逐条 PASS | 技能用例覆盖：创建四方一致（工作副本 / 快照 / 指针 / ETag，含只读位 `perm&0o200 == 0`）、导入排序清单、全量替换、过期 / 空 / 错误 If-Match 零写入、分歧 409 零行、同名创建 409 零行、提交失败回滚（旧工作副本恢复 + 快照目录移除 + failed 行）、删除归档、重建续号、被引用 400 点名、撤回建新版本（v3 = v1 内容哈希）、幂等发布、校验零写入 |
| stage（T051） | `sh scripts/check-acceptance.sh --stage … --stage-only --task T051` → passed，报告 `accept-20261022T044932.999407100`；`012-app-resource-admin` exit=0，`passed_tests` 74 个函数含本任务 K1–K13 中匹配族前缀的 11 个 | 该 check `required_tests` 十锚点全部命中 |
| 红态 ①（同名创建零操作行） | `-overlay` 探针在 `BeginOperation` 前注入一条 pending 行：`TestResourceAdmin_SkillDuplicateCreateZeroOperations` 报 `同名创建是预写之前的 409，零操作行` | 判别力成立（逐字预期文案）；探针目录用毕已删 |
| 红态 ②（分歧闸门零操作行） | 探针把分歧比较改为恒假并注入 pending 行：`TestSkillWrite_WorkCopyDivergedConflict` 报 `分歧闸门在预写之前，不得写操作行` | 判别力成立；探针目录用毕已删 |
| 红态 ③（提交失败回滚） | 探针把 `abortSkillWrite` 的恢复调用替换为不恢复：`TestResourceAdmin_SkillCommitFailureRollsBack` 报 `提交失败必须恢复旧工作副本` | 判别力成立（探针先修好未用变量才可编译，红态不是编译错误）；探针目录用毕已删 |
| stage 覆盖面 | 卡片约束 3 / 35 点名的 `TestSkill_StaleETagZeroWrite` / `TestSkillWrite_WorkCopyDivergedConflict` 是 app 层用例，但二者不在 `012-app-resource-admin` 的族列表（§4.4 第 7 行）也不在 `012-workspace-skill` 的包面（`./internal/workspace`）内 | 函数名按卡片字面量保留，**不改** stage 判据面；两者在 `make check`（`go test ./...`）内执行。已进 §4 未验证清单 U11，由 T068 按 `go test -list` 重展开时处置 |
| 静态门禁修复 | staticcheck S1016：`resource_admin.go:1934` 用结构体字面量把 `core.FileHash` 转为 `core.ResourceManifestFile`（T042 既有代码，本包此前未跑全量 staticcheck） | 两类型字段集与顺序一致，改为直接转换；未用抑制注释 |
| 勾选 | **翻转** T051 | check `012-app-resource-admin` 绿 + 全包测试绿 + 三条红态证据齐 |

### 1.49 T052 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 卡片 §3.7.1 把 `POST /skills` 与 `POST /skills/import` 的成功响应写成 `SkillDetail`，而契约 `openapi.yaml`（固定字面量权威）规定这两个端点只回 `SkillView`（无 `files`）；`PUT /skills/{name}` 才回 `SkillDetail` | 按契约落地：创建 / 导入只回 `SkillView`；`handleAdminImportSkill` 错误路径改用 `body.Name`（成功视图在错误时为占位零值，不能拿来当目标名）；更新写 `ETag` 响应头。`admin_routes.go` 既有注释「POST 创建 / 导入按契约只回 SkillView」即此口径，未改契约 |
| D2 | 校验端点的「不落盘 / 不写操作行」必须在 web 层可见地成立，且不能靠「handler 没调用写端口」的自证 | `handleAdminValidateSkill` 只调 `ValidateSkill`（只读端口），不要求 If-Match、不回 ETag；用例以 `operationLedger` 零行 + `snapshotWorkspace` 逐字节比对 + 写端口调用计数三重断言 |
| D3 | `012-web-admin-resources` 的草稿锚点 `TestSkillValidate_NoSideEffects` 在全仓与模块卡中均不存在；卡片约束 26 点名为 `TestSkillValidate_NoWrites` / `TestSkillValidate_SameGateAsImport` | 按 T044 / T003 / T019 先例对齐模块卡精确名，并把第二个卡片用例补进 `required_tests`——**只收紧**判据面（既有锚点一字未动、零删除） |
| 交付（T052） | `internal/web/handlers_admin_resources.go`：`handleAdminCreateSkill` / `handleAdminImportSkill` / `handleAdminUpdateSkill` / `handleAdminValidateSkill` 与三条固定请求体文案；`admin_routes.go` 五行技能占位替换为真实 handler（更新 / 删除保持 `conditional: true`）；`handlers_admin_resources_test.go`：fake 增加四个技能端口字段，追加 8 例（`CreateSkillProjectsView` / `CreateSkillDuplicate409` / `ImportSkillProjectsView` / `ImportSkillRejectsBoundary` / `UpdateSkillRequiresStrongIfMatch` / `UpdateSkillProjectsDetail` / `TestSkillValidate_NoWrites` / `TestSkillValidate_SameGateAsImport`），`MissingPort500` 扩到技能四端点 | 用例夹具 `skillDetailFixture` 按 `SkillView` + 两个文件投影；422/400/409/412 逐端点断言状态码、错误码与零副作用 |
| 实测（T052） | `gofumpt -l` 零输出；`go vet ./internal/web/` exit=0；项目 staticcheck `-checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./internal/web/` exit=0；8 例聚焦 PASS；`go test ./internal/web -count=1` → `ok 4.086s` | 无新增依赖、无迁移 |
| stage（T052） | `sh scripts/check-acceptance.sh --stage … --stage-only --task T052` → passed，报告 `accept-20261022T045304.796708000`；`012-web-admin-resources` exit=0，命中含 `TestSkillValidate_NoWrites` / `_SameGateAsImport` / `CreateSkill*` / `ImportSkill*` / `UpdateSkill*` / `MissingPort500/*-skill` | 七锚点全部命中（D3 修正后） |
| 红态 ①（创建只回 SkillView） | `-overlay` 探针（仓库外 `%TEMP%\us12-t052-probe-a.1XDGmh`）让创建响应改回 `SkillDetail`：`TestAdminResources_CreateSkillProjectsView` 报 `should not contain "files"` + 消息 `创建响应只回 SkillView`（`handlers_admin_resources_test.go:1344`） | 判别力成立（逐字预期文案）；探针目录用毕已删 |
| 红态 ②（校验端点零写） | 探针在 `handleAdminValidateSkill` 注入一次写端口调用：`TestSkillValidate_NoWrites` 报 `expected: 200 / actual: 500` + 消息 `校验端点不得要求 If-Match，也不得触达写端口`（同文件 1511 行） | 判别力成立；探针目录用毕已删 |
| 红态 ③（条件写形状闸门） | 探针移除 `PUT /skills/{name}` 行的 `conditional: true` 与 handler 的 412 分支：`TestAdminResources_UpdateSkillRequiresStrongIfMatch` 报 `条件写形状不合法时不得触达端口`（同文件 1443 行） | 判别力成立（端口被触达才 `t.Fatal`）；探针目录用毕已删 |
| 勾选 | **翻转** T052 | check `012-web-admin-resources` 绿 + 全包测试绿 + 三条红态证据齐 |

### 1.50 勾选对账（2026-10-22）

T052 收口后复核「已实现但当时因兄弟任务锚点缺位未翻转」的勾选，按本轮实测一次性对账：

| 任务 | 依据 |
| --- | --- |
| T017 / T025 / T031 / T040 / T042 / T048 / T052 | 所属 stage check 本轮复跑全绿：`--task T031` → `012-app-agent-admin` + `012-app-resource-admin` passed（`accept-20261022T045528.723605400`）；`--task T042` → `012-app-resource-admin` + `012-web-admin-resources` passed（`accept-20261022T045542.241063200`），两 check 的 `required_tests` 锚点全部命中 |
| T026 / T041 | 同上 `012-web-admin-resources` 绿（此前仅缺 T052 技能锚点） |
| T023 / T027 / T032 / T043 | 接线任务无独立 stage 绑定（T068 重展开时补）；本轮证据：`internal/app` 全包测试绿（§1.48 实测）、编译期端口断言 `var _ web.ResourceAdminService = resourceAdminPort{}` / `var _ web.AgentAdminService = agentAdminPort{}` 成立、`run_test.go` 的构建来源断言（目录由对账后的已发布版本构建）同批通过 |
| 未翻转 | T011 / T012 / T019 / T030 / T050 仍被兄弟任务锚点阻塞（T054 / T039 / T055 / T056 / T060 / T067A）；T023 等接线任务若 T068 加绑定后复跑失败，回退勾选并落失败台账 |

### 1.51 T053 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 端口签名直接收线上草稿类型（`web.SkillCreateWrite` / `SkillImportWrite` / `SkillUpdateWrite`），而编排层要 `core.AuthorFile`；卡片未规定投影助手的位置 | 在 `internal/app/web.go` 加 `skillFilesFromDrafts`（路径 / 正文逐字，边界仍由 `workspace.ValidateSkillDraft` 唯一裁决）与 `skillDetailView`（`file_count` 取清单长度、`files` 逐项给 path / size / sha256 / 正文，**不**重算哈希或 ETag）；四个方法改为转调，删除 `errResourceAdminUnassembled` 与失败关闭桩 |
| D2 | `TestWire_ResourceAdminPortWiring` 原断言技能四方法返回桩错误（T048 时代的临时红态锚点，注释即写「技能方法族由 T052 替换：在此之前必须失败关闭」） | 按任务计划更新为真实转调断言：创建走完整写路径（工作副本 `skills/<name>/SKILL.md` 与首版快照 `versions/skills/<name>/1/SKILL.md` 落盘）、同名导入 409、更新缺 If-Match 412、校验只读不改字节。**判据面变更 = 收紧**（桩错误断言退场，换为可观察落盘与错误分类断言）；无删除断言、无阈值放宽 |
| 交付（T053） | `internal/app/web.go`：`skillFilesFromDrafts` / `skillDetailView` + 四方法转调；`internal/app/wire.go`：写面端口注释更新（13 方法全转调，无失败关闭桩）；`internal/app/wire_test.go`：`TestWire_ResourceAdminPortWiring` 更新 | 工作副本 / 快照目录经 `ResourceAdminDeps.Root` 与 `NewSkillLibrary(root)` 共用同一工作区根（wire 既有传值，无需新增依赖位） |
| 实测（T053） | `gofumpt -l` 零输出；`go vet ./internal/app/` exit=0；项目 staticcheck exit=0；聚焦 `TestWire_ResourceAdminPortWiring` PASS；`go test ./internal/app -count=1` → `ok 16.573s` | 无新增依赖、无迁移 |
| stage（T053） | `--task T031` → `012-app-agent-admin` + `012-app-resource-admin` passed（`accept-20261022T045856.799959100`） | T053 在暂定 stage 中无独立任务绑定（T068 重展开时补）；本轮以 app 全包测试 + 两个 app check 绿为判据 |
| 红态（假成功桩） | `-overlay` 探针（仓库外 `%TEMP%\us12-t053-probe.qBI6DZ`）把 `CreateSkill` 换成「返回填充好的 `SkillDetailView` 但不落盘」：`TestWire_ResourceAdminPortWiring` 报 `unable to find file …\.basalt\skills\wire-skill\SKILL.md` + 消息 `工作副本必须落在工作区根的 skills/<name>/ 下`（`wire_test.go:1219`） | 判别力成立（逐字预期文案：端口返回成功值与真实落盘可区分）；探针目录用毕已删 |
| 勾选 | **翻转** T053 | 接线转调 + 落盘 / 错误分类断言 + 探针证据齐 |

### 1.52 T054 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 卡片 §3.1.4 只冻结三个符号（`AgentDocument` / `ScanAgentDocuments` / `WholeWordMatch`）与整词边界，**未规定**扫描面的 fail-closed 细节：非普通文件定义、读取失败、超限、`archive/` 缺失、`agents/` 缺失、`.staging` 残留是否入扫描 | 按约束 38「删除前扫描」与约束 11 的 fail-closed 口径落定：①缺 `AGENT.md` 的目录跳过（无从引用）；②读取失败 / 非普通文件（含软链）/ 超过 1 MiB（与 app `maxArchivedAgentBytes` 同口径）→ **整体报错**，调用方据此拒绝删除（证明不了"没人引用"就不放行）；③归档 frontmatter 解析失败**仍返回文档**（正文才是判定面），`Name` 回退归档标识；④`archive/` 缺失按空集、`agents/` 缺失报错（工作区未初始化不得当成零只 Agent）；⑤不按名字过滤 `.staging` 残留——残留树仍可能引用渠道，扫描它才是 fail-closed（清理归恢复路径） |
| D2 | 卡片 §3.2.1 计划 `notify_scan_test.go` **8 例**（整词、大小写、归档目录、解析失败） | 实际落地 **10 个测试函数（含 17 个表驱动 / 条件子例）**：计划四类全部覆盖，另加平台无关的非普通文件用例（本机建不出软链接时仍能判卷同一 Lstat 分支）与工作区初始化三子例。按 M3 裁决「计划用例数实施完成后以实际发现和执行的测试为准」登记差异，不删计划覆盖点 |
| 交付（T054） | `internal/workspace/notify_scan.go`（`AgentDocument` / `ScanAgentDocuments` / `WholeWordMatch` + 私有读取与词符判定）；`internal/workspace/notify_scan_test.go`（10 个测试函数） | 只读面：不开锁、不开 DB、不写文件；不新增依赖（`goccy/go-yaml` 为既有坐标） |
| 实测（T054） | `gofumpt -l` 零输出；`go vet ./internal/workspace/` exit=0；项目 staticcheck exit=0；`go test ./internal/workspace -run '^TestNotifyScan_'` PASS；全包 `ok 12.835s`；`go build ./...` exit=0 | 无迁移、无 go.mod 变更 |
| stage（T054） | `--stage-only --task T054` → `012-workspace-skill` passed（`accept-20261022T050245.969412300`） | 该 check 的 `-run ^(TestSkill\|TestNotifyScan_)` 同时执行 `TestSkillImport_TextOnly` 与本任务 10 例；`required_tests` 两锚点均命中 |
| 红态（三条探针） | ①词符集去掉 `_` → `TestNotifyScan_WordBoundary/下划线算词内字符` 报 `expected: false / actual: true`（`notify_scan_test.go:45`）；②解析失败时丢弃归档文档 → `TestNotifyScan_ParseFailureKeepsDocument/缺围栏` 报 `"[]" should have 1 item(s), but has 0`（`notify_scan_test.go:123`）；③非普通文件改判 `fs.ErrNotExist`（静默跳过）→ `TestNotifyScan_RejectsNonRegularDefinition` 报 `An error is expected but got nil.`（`notify_scan_test.go:186`） | 三条都在仓库外 `%TEMP%\basalt-t054-probe` 用 `-overlay` 逆改、`perl -0pi -e` 施加，探针目录用毕已删；逐字预期文案成立 |
| 勾选 | **翻转** T054；**对账翻转** T011 / T050 | T011（`SkillWorkCopyRelDir` / `SkillVersionRelDir` + `TestSkillPaths_*` 3 例）与 T050（`skill_admin.go` + 16 例含 `TestSkillImport_TextOnly`）的交付与 check 均在 `012-workspace-skill` 绿前已存在，此前仅因缺 `TestNotifyScan_WordBoundary` 被阻塞；本轮 check 绿后一并翻转（证据同上报告） |

### 1.53 T055 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 卡片 §3.1.6 冻结四个签名（`Create` / `Update(keepURL)` / `Delete` / `Get`），未规定：同名冲突在存储层的错误形态、`description` 的可空落法、`created_at` 由谁写 | ①同名 → `core.ErrConflict`（不新增 `CONFLICT` 错误码；约束 38 的 400 映射由 T056 完成——驱动文案随版本变化，判定改为插入失败后**重查行是否存在**，不做字符串匹配）；②`description` 按参数原样落库，读面 `sql.NullString` 折成空串（沿用 US-8 读面口径）；③`created_at` 由仓储取写入时刻（签名无时钟参数）；④空 `url` 照存空串（列 NOT NULL），占位符语法 / 域名白名单 / 类型表 / 引用扫描 / 操作行全部留 T056 |
| D2 | `TestNotifyAdminStore_CRUD` 是 stage 的精确 `required_tests` 锚点（`012-storage-admin`），卡片 §3.2.1 计划 7 例（CRUD、保留 URL、唯一名） | 落地 **7 个测试函数**与计划数量一致：`CRUD`（含发送读面同行的断言）、`CreateEmptyURL`、`DuplicateNameIsConflict`、`UpdateKeepsURLWhenRequested`、`UpdateReplacesURL`、`DeleteMissingIsNotFound`、`RejectsInvalidWrites` |
| 交付（T055） | `internal/storage/notify_admin_store.go`（四方法 + 形状判据 + 唯一键冲突重查）；`internal/storage/notify_admin_store_test.go`（7 例） | 只新增一个存储文件；句柄面复用 `db.go` 的 `queryer`，不另造同形接口；无迁移、无 go.mod 变更 |
| 实测（T055） | `gofumpt -l` 零输出；`go vet ./internal/storage/` exit=0；项目 staticcheck exit=0；聚焦 7 例 PASS；全包 `ok 7.676s`；`go build ./...` exit=0 | — |
| stage（T055） | `--stage-only --task T055` → `012-storage-admin` passed（`accept-20261022T050548.667476600`） | 此前 `--task T012` 的当前红态 `required test did not execute and pass: TestNotifyAdminStore_CRUD`（`accept-20261022T050356.228622600`）已消除；`required_tests` 四锚点全部命中 |
| 红态（三条探针） | ①唯一键冲突映射短路 → `TestNotifyAdminStore_DuplicateNameIsConflict` 报 `Target error should be in err chain: expected: "conflict" / in chain: "创建通知渠道 dup-channel: constraint failed: UNIQUE constraint failed: notify_channels.name (1555)"`（`notify_admin_store_test.go:127`）；②`Update` 忽略 `keepURL` → `TestNotifyAdminStore_UpdateKeepsURLWhenRequested` 报 `expected: "${BASALT_TEST_HOOK}" / actual: ""`（`:160`）；③空 URL 写成 NULL → `TestNotifyAdminStore_CreateEmptyURL` 报 `NOT NULL constraint failed: notify_channels.url (1299)`（`:101`） | 三条都在仓库外 `%TEMP%\basalt-t055-probe` 用 `-overlay` 逆改、`perl -0pi -e` 施加，探针目录用毕已删；逐字预期文案成立 |
| 勾选 | **翻转** T055；**对账翻转** T012 / T039 | T012（`agent_admin_store.go` 8 例）与 T039（`ResourceStore.MarkDeleted` 8 例）的交付与断言早已存在，仅因同一 check 缺 `TestNotifyAdminStore_CRUD` 被阻塞；本轮 check 绿后一并翻转（证据同上报告） |

### 1.54 T056 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 卡片 §3.1.7 冻结 app 层四签名，产出类型是 `NotifyChannelSummary`；而 US-11 已冻结的 web 端口 `NotifyAdminService` 返回 `core.NotifyChannel`，响应视图 `notifyChannelView` 只投影 `notifyURLHost(ch.URL)`。卡片未规定摘要是否携带库里的占位 | 摘要**原样携带库里的 URL 值**（精确 `${VAR}` 占位或空串），不携带解析后的地址；T058 的端口适配器把摘要映射成 `core.NotifyChannel`，host 投影继续由 web 层既有 `notifyChannelView` 完成——不另造第二套投影。`TestNotifyAdmin_OnlyHost` 在 app 侧钉住"解析值不进摘要 / 操作摘要 / 日志" |
| D2 | 卡片 §2 写"发送时环境变量缺失 → 发送失败并记 ERROR"；US-8 冻结的 `NotifyTool` 不带 logger（约束 20），发送失败由执行器统一记 **WARN**（`core/tool_exec.go:122`，`detail` 里含渠道名与变量名） | **保留冻结的 WARN 路径**：给工具加 logger 面属修改前序模块公共接口（软门禁 4），且无类型化错误可区分"环境变量缺失"与普通输入错误，包一层"所有失败记 ERROR"会把模型选错渠道名等普通错误也抬成 ERROR。内容要求（点名渠道与变量）已满足，级别差异作为**已知偏差**交收口 review ② |
| D3 | 卡片未规定三条写路径的校验与预写次序细则 | 创建：纯校验（名称 / 类型表 / 占位 / 环境变量 / 白名单）→ 预写 → 写入；同名由存储层 `ErrConflict` 在预写**之后**判定 → 映射 `*core.InvalidInputError`（400，契约只列 200/400）并回填 failed `BAD_REQUEST`（符合卡片失败表"操作 failed；零写入"）。更新：名称 / 请求体名一致 → `Get` 404（零操作行）→ 纯校验 400 → 预写 → 更新。删除：名称 → `Get` 404 → 预写 → 扫描 → 命中闸门 → 删除 |
| D4 | `force=true` 的边界：卡片只写"才强删"，未写扫描失败时的取舍 | 扫描失败 = 证明不了"没人引用"，**force 也不放行**（failed `INTERNAL` + `operationFailedError`，行保留）；force 只跳过"命中"这一档。强删成功后 `detail_json` 记 `{"forced": true}`，非强删不写该键 |
| D5 | 卡片 §3.2.1 计划 app 侧 `notify_admin_test.go` 10 例；实际落地 12 个测试函数（M3 口径：以实际发现与执行为准） | 新增拆分 `TestNotifyAdmin_ForceDelete`（卡片约束 38 复核名单名）与 `TestNotifyAdmin_UpdateGuards`（更新 / 删除的 404 与名称不一致）。卡片复核名单中的 `TestNotifyAdmin_ListHostOnly` 归 T057（host 投影在 web 的 `notifyChannelView`；卡片行 1041 明确 GET 读面沿用 US-11 文件），T057 落地时以该名建用例 |
| 交付（T056） | `internal/app/notify_admin.go`（新增：四签名编排、校验、脱敏、引用闸门、操作行）；`internal/app/notify_admin_test.go`（新增：12 函数）；`internal/app/notify.go`（+`newNotifyTool` 装配：DB 读面 + 与类型端点共用的适配器映射）；`internal/app/notify_test.go`（+5 发送面用例：读库、缺变量、策略拒绝脱敏、未知渠道 / 未装配类型、入参上限） | 发送面的 DB 读取与发送时解析由 US-8 已交付；本任务只新增装配函数与判据。无迁移、无 `go.mod` 变更 |
| 实测（T056） | `gofumpt -l` 四文件零输出；`go vet ./internal/app/` exit=0；项目 staticcheck exit=0；聚焦 `TestNotifyAdmin_` / `TestNewNotifyTool_` PASS；`go test ./internal/app -count=1` → `ok 17.428s`；`go build ./internal/app/` exit=0 | 全部在仓库内实测，未跳过 |
| stage（T056） | `--stage-only --task T056` → **failed**：`012-app-notify-author` 报 `required test did not execute and pass: TestWorkspaceBrowse_RejectsEscape`（`accept-20261022T051754.208257600`）；本 check 的另两个锚点 `TestNotifyAdmin_ReferenceGate` / `TestAuthorService_OnlyLLMCallAudit` 与全部 `TestNotify*` 已执行通过 | 非实现缺陷——该锚点属 T060（工作区只读浏览），尚未实现。按 T048 先例**不临时删锚点、本任务勾选暂不翻转**；T060 落地后复跑 T056 并同时翻转 T019 / T056 / T060 |
| 红态（五条探针） | `-overlay` 探针（仓库外 `%TEMP%\us12-t056-probe2.*`，`perl -0pi -e` 变异）：①预写短路 → `TestNotifyAdmin_EveryWriteHasOperation/预写失败零写入` 报 `An error is expected but got nil.`，且 `CreateAndUpdate` 报 `"[]" should have 1 item(s), but has 0`；②引用扫描返回空集 → `TestNotifyAdmin_DeleteReferenced` 与 `ReferenceGate/归档文档点名` 同报 `An error is expected but got nil.`；③扫描错误被吞 → `ReferenceGate/扫描失败不放行` 报 `An error is expected but got nil.`；④占位判定换常量 → `PlaceholderOnly/明文地址` 报 `"环境变量 ${PROBE} 未设置或为空，拒绝登记该渠道" does not contain "占位"`；⑤强删不写 detail → `TestNotifyAdmin_ForceDelete` 报 `Should be true` | 五条逐字预期文案成立；探针目录用毕已删 |
| 勾选 | **不翻转** T056 | 自身判据全绿，但绑定 check 因兄弟任务锚点未落地仍红；待 T060 落地复跑后与 T019 / T060 一并翻转 |

### 1.55 T057 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 卡片 §3.7.1 行 1474 写"响应 `NotifyChannelView`（由 `NotifyChannelSummary` 投影）"，而 T014 已落地的 web 端口 `NotifyAdminService` 返回 `core.NotifyChannel`（web 包不得 import `internal/app`） | 投影链分两段：T058 端口适配器把 `NotifyChannelSummary` 形状映射成 `core.NotifyChannel`（字段一一对应，不改语义），web handler 继续用既有 `notifyChannelView` 出站——**不**改 T014 端口签名、**不**在 web 造第二套投影。摘要携带库里的原始值（D1 of §1.54），host 投影仍只由 `notifyURLHost` 完成 |
| D2 | 契约 `DELETE /notify-channels/{name}` 的 `force` 是 `enum: ['true']`；卡片只写"字面 `true` 才强删"，未规定其他取值的处置 | `parseNotifyForce` 只接受缺省 / 空串（false）与字面 `true`（true）；其余一律 400 `BAD_REQUEST` + 固定文案 `force 只接受字面 true`，**不触达端口**（无操作行、无磁盘动作）。大小写变体与 `1` / `false` 都按非法枚举拒绝 |
| D3 | 卡片未规定 web 层的通知错误映射 | 复用既有 `writeResourceAdminError`：`ErrResourceNotFound` → 404（`未找到资源：<name>`）、`*InvalidInputError` → 400（经 `writeAdminDataError` 携带白名单 `data.agents`）、其余 → 固定 500。**不**新建通知专用映射器，避免第二套错误分类 |
| D4 | 纯请求校验与写端口的次序 | 三条写端点都先 `decodeAdminWriteBody`（`MaxBytesReader` + 未知字段拒绝 + 尾随内容拒绝）→ 更新再判请求体 `name` 与路径一致 → 删除先解析 `force` 字面量 → 之后才调用端口。校验失败零端口调用，因此不可能写操作行或动磁盘 |
| D5 | 卡片 §3.2.1 计划 web 侧 9 例；实际落地 9 个测试函数 | 覆盖：同名 400（含空 URL / 类型原样透传）、请求体拒绝、更新请求形状（留空保留 / name 不一致）、更新错误映射（404 / 400 / 500 不回显）、`force` 字面量、引用命中 400 + `data.agents`、写后读只回 host、缺端口 500、创建回包对历史绝对地址也只回 host。卡片约束 38 复核名单中的 `TestNotifyAdmin_ListHostOnly` 按 §1.54 D5 在此落地 |
| 交付（T057） | `internal/web/handlers_admin_notify.go`（新增：POST / PUT / DELETE 三 handler、`parseNotifyForce`、三条固定文案）；`internal/web/handlers_admin_notify_test.go`（新增：9 函数）；`internal/web/admin_routes.go`（通知三行由 `handleAdminUnassembled` 替换为真实 handler；工作区两行仍为 T061 占位） | GET 列表 / 类型仍由 US-11 `handlers_notify_read.go` 提供，未重复注册；无迁移、无 `go.mod` 变更 |
| 实测（T057） | `gofumpt -l` 三文件零输出；`go vet ./internal/web/` exit=0；项目 staticcheck exit=0；聚焦 `^(TestAdminNotify_)` 9 函数 PASS；`go test ./internal/web -count=1` → `ok 4.133s` | 全部在仓库内实测，未跳过 |
| stage（T057） | `--stage-only --task T057` → **failed**：`012-web-admin-notify-workspace` 报 `required test did not execute and pass: TestAdminWorkspace_TreeSkipsStaging`（`accept-20261022T052142.967261300`）；本任务锚点 `TestAdminNotify_CreateDuplicate400` 与全部 9 个 `TestAdminNotify_*` 已执行通过 | 非实现缺陷——该锚点属 T061（工作区只读浏览），尚未实现。按 T048 先例**不临时删锚点、本任务勾选暂不翻转**；T061 落地后复跑 T057 / T061 并一并翻转 |
| 红态（三条探针） | `-overlay` 探针（仓库外 `%TEMP%\t057probe.ik12lG`，`perl -0pi -e` 变异）：①`parseNotifyForce` 其余取值当成 true → `TestAdminNotify_DeleteForceLiteral` 报 `expected: 400 actual: 200`（Messages: `query=?force=1`）；②`notifyChannelView` 直接回 `ch.URL` → `TestAdminNotify_CreateViewOnlyHost` 报 `expected: "https://open.feishu.cn" actual: "https://open.feishu.cn/open-apis/bot/v2/hook/abc?token=super-secret"`，`TestAdminNotify_ListHostOnly` 报 `expected: "" actual: "${OPS_HOOK}"`；③关闭更新 name 一致性判定 → `TestAdminNotify_UpdateRequestShape` 报 `expected: 400 actual: 200` | 三条逐字预期文案成立；探针目录用毕已删 |
| 勾选 | **不翻转** T057 | 自身判据全绿，但绑定 check 因兄弟任务锚点（T061）未落地仍红；待 T061 落地复跑后与 T060 / T061 一并翻转；T056 / T019 仍待 T060 |

### 1.56 T058 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | §1.55 D1 定下的两段投影由哪一层落地，卡片未点名 | 端口适配器 `notifyAdminPort` 放在 `internal/app/web.go`——与 `agentAdminPort` / `resourceAdminPort` 同一宿主（卡片修改清单第 14 行），三方法只做形状投影：`web.NotifyChannelWrite → NotifyChannelRequest`、`NotifyChannelSummary → core.NotifyChannel`。URL 原样携带（占位或空串），不解析、不补全；**不**改 T014 端口签名 |
| D2 | 发送路径 `tool.NewNotifyTool(..., newNotifyAdapters())` 直调与 `newNotifyTool` 装配函数并存 | `wire.go` 改调 `newNotifyTool`（唯一装配函数）：发送面行为不变（每次发送读 DB、发送时解析 `${VAR}`），并使发送面、类型端点、写面类型表三处共用 `newNotifyAdapters` 唯一真值（约束 38） |
| D3 | 卡片 §3.1.7 未规定 `NotifyAdmin` 构造点的依赖清单 | 装配共享同一批实例：`Root`（工作区根，删除扫描）、`storage.NewNotifyAdminStore(db)`（与发送读面同表）、`ops`（管理操作台账，与资源写面同一实例）、`notifyTypeNames()`（与类型端点同一有序切片）、`policy`（出网白名单预检）、`logger` / `now`；`LookupEnv` 不注入（编排层 nil → `os.LookupEnv`） |
| D4 | 卡片的 app 测试清单无 wire 专属文件，且 T058 无专属 check | 判据落 `internal/app/wire_test.go` 的 `TestWire_NotifyAdminPortWiring`（与 T043 / T053 接线用例同文件）：端口填槽、root / 类型表 / 操作台账依赖、空地址创建、`${VAR}` 占位原样回传、留空更新保留原值、两次删除落库与操作行计数 |
| 交付（T058） | `internal/app/web.go`（+`notifyAdminPort`、两个投影助手、编译期断言）；`internal/app/wire.go`（工具注册改 `newNotifyTool`；`notifyAdmin` 构造；`heavyRuntime.notifyAdmin` 字段与 `writeServices.notifyAdmin` 填槽；装配注释补第 3 个端口）；`internal/app/wire_test.go`（+`TestWire_NotifyAdminPortWiring`） | 无迁移、无 `go.mod` 变更 |
| 实测（T058） | `gofumpt -l` 三文件零输出；`go vet ./internal/app/` exit=0；项目 staticcheck exit=0；`go build ./internal/app/` exit=0；聚焦 `TestWire_NotifyAdminPortWiring` PASS；`go test ./internal/app -count=1` → `ok 17.545s` | 全部在仓库内实测，未跳过 |
| stage（T058） | 不适用：stage 的 `tasks` 表无 T058 绑定（任务无专属 check）；其行为由 T056 / T057 的 `012-app-notify-author` / `012-web-admin-notify-workspace` 覆盖 | 与 tasks.md 一致；不为装配任务临时新增判据面 |
| 红态（两条探针） | `-overlay` 探针（仓库外 `%TEMP%\t058probe.wD6Hki`，`perl -0pi -e` 变异）：①删掉 `writeServices.notifyAdmin` 填槽 → `wire_test.go:1279` 报 `Expected value not to be nil.`（Messages: `T058 之后通知写面端口必须已填槽`）；②端口请求投影把 `URL` 置空 → 报 `expected: "${WIRE_HOOK_URL}" actual: ""`（Messages: `响应必须回库里原样占位，不得回解析后的地址`） | 两条逐字预期文案成立；探针目录用毕已删 |
| 勾选 | **翻转** T058 | 本任务无专属 check；判据 = 全包测试 + 红态证据（T049 先例） |

### 1.57 T059 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 卡片 §3.1.8 页树要求「类型下拉来自 `GET /notify-channels/types`（有序）」，但 `api.ts` / `api-admin.ts` 均无该方法，T059 任务正文也只点名组件与用例 | 在 `api-admin.ts`（卡片 §3.1.10 明列「上述端点的函数」）追加 `listNotifyTypes(): Promise<string[]>`，经同一 `ApiClient.request` 出口；**不**改 `api.ts`、**不**新增第二套客户端。组件据此渲染 `<select>`，类型名与顺序完全取自服务端（约束 39），读取失败时退化为文本输入，仍由服务端校验 |
| D2 | 任务写「挂 `/console/settings/notifications` 页内」，而该页是 US-11 的 `views/workspace.ts` 中的 `NotificationsView`（只读表格） | 只把该页的表格替换为页内组件：`useLoader` 与错误 / 加载提示保留，组件收到 `channels` 列表；写成功后 `onChanged` → `channels.reload()`（注 20：不做乐观更新）。路由、`app.ts` 与静态壳未动 |
| D3 | 「URL 占位符」与「更新留空 = 保留原值」在前端的落点 | 非空地址必须精确匹配 `/^\$\{[A-Za-z_][A-Za-z0-9_]*\}$/`，不匹配在本地就拒绝（零请求）；空白地址在创建时不提交 `url` 字段（服务端存空串 = 暂不可发送），在修改时同样不提交（服务端保留原值）。服务端仍做同口径二次校验，本地拒绝只为保证「零请求发出」 |
| D4 | 「force 二次确认」的交互定义卡片只写原则 | 行内删除先是一次确认（「删除」→「确认删除」，不带 force）；服务端 400 且 `data.agents` 非空时进入强删通道，点名列表逐条展示；第一次点「强制删除」只解锁按钮（文案变为「再次确认强制删除」），第二次才发 `?force=true`。其它失败（404 / 无点名的 400 / 500）只展示信封文案，不进入强删通道 |
| D5 | 列表只显示 host 的落点在组件而非父页面 | 组件自行渲染渠道表：第 4 列只取服务端 `NotifyChannelView.url_host`（空串显示「暂不可发送（地址为空）」），原始地址在组件与页面都没有渲染载体；响应类型本身也不含 URL 字段 |
| 交付（T059） | `assets/console/src/components/notify-channels.ts`（新增：CRUD、占位校验、host-only 列表、两级删除确认）；`assets/console/tests/notify-channels.test.ts`（新增：6 例）；`assets/console/src/api-admin.ts`（+`listNotifyTypes`）；`assets/console/src/views/workspace.ts`（`NotificationsView` 换挂页内组件） | 零新增前端依赖、零 Go 改动、无迁移 |
| 实测（T059） | 本机 `node --version` / `npm --version` → `command not found`（同 §1.20 D6）：Vitest / `vue-tsc` **未执行**；危险 DOM `grep -rnE 'v-html\|innerHTML' assets/console/src` → exit=1、零命中；组件 / `api-admin.ts` / 视图内 `fetch(` 零命中（exit=1，唯一 HTTP 出口仍是 `ApiClient`）；导入符号与 `api.ts` / `api-admin.ts` / `ui.ts` / `shared.ts` 逐项静态对照存在 | 前端判据由 CI 前端 job（`npm ci` → `npm test` → `npm run build` + dist / integrity 对账）承担；U6 保持「未验证」，**不**宣称前端绿态 |
| stage（T059） | 不适用：`harness/stages/us12-agent-resource-management.json` 无 T059 绑定（前端判据不在 Go stage 内） | 与 tasks.md / stage 一致；不为前端任务临时新增 Go 判据面 |
| 红态（T059） | 不适用：本机无 Node，用例无法执行，无法取得逐字失败文案；不以静态推演冒充红态 | 红态判据随 CI 前端 job 一并执行；如实登记未验证 |
| 勾选 | **暂不勾选** T059 | 未全绿不得宣称完成：类型检查与 6 例前端用例本地无法执行，待 CI 前端 job 与 T067C 证据后由控制侧翻转，并按需把 U6 翻为已验证（注日期与 CI 证据） |

### 1.58 T060 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 浏览根与装配输入：卡片约束 40 / 注 19 冻结「根固定 `.basalt/agents/`」，而 app 依赖惯例的 `Root` 是工作区根（`.basalt`） | `WorkspaceBrowseDeps.Root` 取工作区根，`openRoot` 用 `os.OpenRoot(filepath.Join(root, agentsDirName))` 打开浏览根（`workspace_browse.go:242-252`）；`Root` 为空按装配错误返回，**不**回落到进程工作目录下同名目录 |
| D2 | 路径规则只在 app 层实现一次（web 只做 query 解码与投影）：拒绝绝对路径、盘符 / UNC、反斜杠（Windows 等价分隔符会让 `..\` 绕过逐段判定）、NUL / 控制字符、空段 / 尾随 `/`、`..`；`.` 段按规范省略（`workspace_browse.go:259-303`） | 全部映射为 `*core.InvalidInputError{Tool: "workspace.browse"}` → 400；盘符判定不依赖平台（非 Windows 上 `C:` 也是非法形状）。`.staging` / `.backup` 残骸不是入参错误，按 404 返回（`ErrResourceNotFound`），与「残骸在浏览语义里不存在」同口径 |
| D3 | 符号链接策略：逐段 `os.Root.Lstat`（`workspace_browse.go:306-338`），任一段是 `ModeSymlink` 即按不存在处理（404），不进入目标；目录列表仍以 `type=symlink`、`size=0` 列出（`workspace_browse.go:93-160`） | 404（而非 400）取自模块卡 §2.3 错误表「工作区路径越界 / 指向符号链接 → 400 / 404」的对应顺序；契约 `/workspace/file` 的「经软连接越出根 → 400」由路径形状拒绝与 `os.Root` 双重保证。**Windows 目录联接**（junction）不是 `os.ModeSymlink`：探针实测它不出现在列表里，穿行请求返回 400「不是目录」，同样零内容泄露（见「实测（T060）·探针」行） |
| D4 | 256 KiB 上限的三道判卷：`Lstat` 判大小后直接拒绝（不打开正文，`workspace_browse.go:200`）；打开后 `Stat` 复核（判卷与打开之间文件可能被替换，`:222`）；`io.LimitReader(cap+1)` 兜住「判卷后变大」的读取，超 1 字节即拒绝（`:228-232`） | 超限一律 400，绝不截断；app 层结果类型没有 `truncated` 字段，web 视图的 `truncated` 由 T061 按契约冻结为 false |
| D5 | 文本判定用 UTF-8 合法性 + NUL 字节（`workspace_browse.go:233-236`），不做扩展名猜测、不 base64；合法 UTF-8（含中文）按原字节返回，`Size` 取实际读取字节数 | 与技能导入的文本判定同口径（约束 24）；二进制 / 含 NUL → 400 |
| D6 | 残骸隐藏大小写不敏感（`workspace_browse.go:355-360`）：Windows 上 `a.STAGING` 与 `a.staging` 是同一目录，列表与直接读取都必须按残骸处理 | 根列表过滤条目；请求路径含任一残骸段 → 404；合法 Agent 不受影响 |
| 交付（T060） | `internal/app/workspace_browse.go`（新增：`WorkspaceBrowse.Tree` / `.File`、路径规范、逐段投影、大小与文本判卷）；`internal/app/workspace_browse_test.go`（新增：8 例） | 零新增依赖、零迁移、零 web / 前端改动 |
| 实测（T060） | `gofumpt -w` 两文件 → 无输出；`go vet ./internal/app` exit=0；`staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./internal/app`（无管道直取退出码）exit=0、零输出；`go test -run '^(TestWorkspaceBrowse_)' ./internal/app` → `ok … 1.862s`；`go test ./internal/app` 全包 → `ok … 17.269s`；stage `-task T060` → passed（报告 `accept-20261022T053503.013585200`，`required_tests` 三例全在 `passed_tests`）；`-task T056` → passed（`accept-20261022T053731.906830600`）；`-task T019` → passed（`accept-20261022T053737.247396100`） | `TestWorkspaceBrowse_NoSymlinkEscape` 在本机 SKIP（逐字：`跳过软链接用例：当前账号无法创建符号链接（… A required privilege is not held by the client.）`）；该例不在 `required_tests`，stage 判卷不受影响，登记 U13；CI Linux job 覆盖 |
| 实测（T060）·探针 | Windows 目录联接探针（overlay 临时测试、用毕即删）：根外目标建 junction 后，列表不出现该条（`entries=[]`）；`File("writer/leakdir/secret.txt")` → `路径 writer/leakdir 不是目录`（400），零内容返回 | junction 不是 `ModeSymlink`，安全保证由「非目录即拒绝」承担；不改实现去猜 reparse 点类型，行为已如实登记 |
| 实测（T060）·gosec | `gosec -quiet ./internal/app`（无管道）exit=1：6 命中全部在本任务文件之外——`resource_admin.go:598/1247/2149/2807` G304 ×4、`mcp.go:278/282` G602 ×2；`workspace_browse.go` 零命中 | `make check` 的 `sec` 目标因此仍未绿（既有 US-12 文件，非本任务引入）；留给对应任务 / T069–T070 修实现，**不**加抑制注释、不顺手改无关文件；已入失败台账 |
| 红态（T060） | 四个 overlay 探针（探针副本建在仓库外、用毕即删）：① 删 `..` 拒绝分支 → `Error: Should be in error chain: expected: *core.InvalidInputError in chain: "查看工作区路径 ..: statat ..: path escapes from parent"`；② 关三道大小判卷 → `Error: An error is expected but got nil. expected: *core.InvalidInputError`；③ `isWorkspaceResidueName` 恒 false → `Error: Not equal: … actual: []app.WorkspaceEntry{ ghost.STAGING, writer, writer.backup, writer.staging }`；④ UTF-8 判定改 `&&` → `Error: An error is expected but got nil. expected: *core.InvalidInputError` | 逆改后四例均逐字变红、恢复后绿；软链接不跟随的红态本机无法取得（无符号链接权限，用例 SKIP），如实登记未验证，不静态推演冒充 |
| 勾选 | **翻转 T019 / T056 / T060** | 三个任务同绑 `012-app-notify-author`，其 `required_tests`（`TestNotifyAdmin_ReferenceGate` / `TestAuthorService_OnlyLLMCallAudit` / `TestWorkspaceBrowse_RejectsEscape`）与各自交付物均已执行通过（T056 / T019 各自 task 报告） |

### 1.59 T061 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 路由表最后两行（`/workspace/tree`、`/workspace/file`）原先指向 `handleAdminUnassembled` 占位 | T061 把两行换成 `handleAdminWorkspaceTree` / `handleAdminWorkspaceFile`；30 行全部指向真实 handler 后，占位函数失去调用方（staticcheck U1000），按文件头「不得在占位状态下宣布本 US 完成」删除并同步注释——没有"装配未完成"的中间态残留 |
| D2 | `GET /workspace/tree` 的 `path` 可省略（契约未标 required），`GET /workspace/file` 的 `path` 必填 | tree 把 `r.URL.Query().Get("path")` 原样转交（空串 = 浏览根，归一化在适配器）；file 缺失 / 空串在触达端口之前 400 `BAD_REQUEST` + 固定文案 `messageWorkspacePathRequired`，端口零调用（不可能产生文件系统读取）。web 层不复制第二份路径规则（`handlers_admin_workspace.go:31-77`） |
| D3 | 错误映射只认类型、不比对文本：`*core.InvalidInputError` → 400（Reason 原样，覆盖越界 / 超限 / 二进制 / 目录与文件误用）；`core.ErrResourceNotFound` / `fs.ErrNotExist` → 404 `NOT_FOUND`，文案 `工作区路径不存在：<path>`；其余 → 500 + ERROR 日志（`:79-100`） | 软链接、残骸与"不存在"共用 404：状态码不能成为"有链接 / 有残骸"的探测通道；失败信封不带 data（`writeError` 永不设置载荷） |
| D4 | 视图投影：tree 经 `nonNilWorkspaceTree`（nil / 空 → `[]`，页面不拿 `null`）；file 原样投影 `WorkspaceFileView`，`truncated` 由适配器恒 false | 本层**不**强制改写 `truncated`，避免掩盖适配器违反契约；超限是 400，400 响应没有 content、也没有 `truncated:true` |
| D5 | 只读语义：两条端点不写管理操作行、不写审计、不写任何文件；handler 不消费 actor | 与卡片 §2.3 注 19 一致；无写路径可绕过管理操作台账 |
| 交付（T061） | `internal/web/handlers_admin_workspace.go`（新增：两 handler + `writeAdminWorkspaceError` + 必填文案）；`internal/web/handlers_admin_workspace_test.go`（新增：6 例，含 stage 锚点 `TestAdminWorkspace_TreeSkipsStaging` 与卡片 §6.1 #15 的 `TestAdminWorkspace_SizeCap` / `TestAdminWorkspace_SymlinkNotFollowed`）；`internal/web/admin_routes.go`（两行换真实 handler、删除占位函数、文件头同步） | 零新增依赖、零迁移、零前端改动 |
| 实测（T061） | `gofumpt -w` 三文件 → 无输出、`gofumpt -l` 零命中；`go vet ./internal/web` exit=0；staticcheck 项目 checks（无管道直取退出码）exit=0、零输出；`gosec -quiet ./internal/web` exit=0、零输出；`go test -run '^(TestAdminNotify_\|TestAdminWorkspace_)' ./internal/web` → 15 例全 pass（9 notify + 6 workspace）；`go test ./internal/web` 全包 → `ok … 4.173s`；stage `-task T061` → passed（报告 `accept-20261022T054259.343361900`，`passed_tests` 15 条含锚点、`skipped_tests` 为 null）；`-task T057` → passed（`accept-20261022T054302.855097200`） | 本机 Windows；前端任务不在本 stage |
| 实测（T061）·gosec | `gosec -quiet ./internal/web`（无管道）exit=0、零输出 | web 包无安全命中；`internal/app` 的 6 条既有命中仍挂在失败台账（非 T061 引入） |
| 红态（T061） | 三个 overlay 探针（副本在仓库外、用毕即删）：① 屏蔽必填判卷（`if false && path == ""`）→ `Error: Not equal: expected: 400 actual: 500` + `Messages: /api/v1/workspace/file`；② 404 分支改成 400 → `Error: Not equal: expected: 404 actual: 400` + `Messages: /api/v1/workspace/tree?path=ghost`；③ tree 投影去掉 `nonNilWorkspaceTree` → `Error: "{\"code\":\"OK\",\"message\":\"success\",\"data\":null,…}" does not contain "\"data\":[]"` | 逆改后三例逐字变红、恢复后绿。空树用例首版让假件返回非 nil 空切片，探针 ③ 未变红——改为返回 nil 后才具备判别力（测试加强，非放宽） |
| 勾选 | **翻转 T061 / T057** | T057 的 9 例通知写面用例早已落地并通过，其 check 只差本任务锚点 `TestAdminWorkspace_TreeSkipsStaging`；两任务同绑 `012-web-admin-notify-workspace`，本轮 `-task T057` / `-task T061` 均 passed（T056 / T019 一并在 §1.58 翻转） |

### 1.60 T062 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 重路径装配在 `notifyAdmin` 之后构造 `workspaceBrowse := NewWorkspaceBrowse(WorkspaceBrowseDeps{Root: root, Logger: logger})`（[wire.go:689-694](../../internal/app/wire.go#L689-L694)），`heavyRuntime` 保留同一实例（`:1070-1074`），`writeServices.workspace` 填 `workspaceBrowsePort{browse: workspaceBrowse}`（`:737`） | 装配一次、全进程共享；浏览根由服务自己拼 `<工作区根>/agents/`，装配层只给工作区根。纯读、不持写锁、不写操作台账、不缓存条目——没有可与写面共享的实例 |
| D2 | 新适配器命名 `workspaceBrowsePort`（[web.go:639-682](../../internal/app/web.go#L639-L682)）：`workspacePort` 已被 [ports.go:55](../../internal/app/ports.go#L55) 的 `cli.Workspace` 适配器占用，重名会导致重复声明 | 两个方法只做形状投影：`Tree` 把 `[]WorkspaceEntry` 投影成 `web.WorkspaceTreeView`（`make` 保证非 nil）；`File` 投影四字段且 `truncated` 恒 false（超限是错误不是截断）；错误原样上抛，分类与文案映射归 web 层（约束 43）。编译期断言 `var _ web.WorkspaceService = workspaceBrowsePort{}` |
| D3 | T062 原先没有装配级判据；本轮在 `wire_test.go` 新增 `TestWire_WorkspaceBrowsePortWiring`（新测试，**允许新增**、不改任何既有断言） | 走真实重路径装配：断言 `rt.workspaceBrowse` 与 `writeServices.workspace` 非 nil、根目录树逐项相等且 `reader.staging` 不可见、`reader/AGENT.md` 的路径 / 类型 / 大小、文件预览正文与 `truncated=false`。stage 的 `012-app-notify-author` 检查同步加该测试到 `-run` 与 `required_tests`，并登记 `T062 → 012-app-notify-author` 任务绑定（只增判据，不放宽） |
| 交付（T062） | `internal/app/wire.go`（构造浏览服务、`heavyRuntime` 字段与字面量、`writeServices.workspace` 填槽）；`internal/app/web.go`（`workspaceBrowsePort` 适配器 + 编译期断言）；`internal/app/wire_test.go`（+1 例）；`harness/stages/us12-agent-resource-management.json`（check 正则 / 锚点 / T062 绑定） | 零新增依赖、零迁移、零前端改动 |
| 实测（T062） | `gofumpt -w` 三文件 → `gofumpt -l` 零命中；`go vet ./internal/app` exit=0；staticcheck 项目 checks exit=0、零输出；`go build ./...` exit=0；`go test ./internal/app` → `ok … 17.371s`；新例单跑 → pass；stage `-task T062` → passed（报告 `accept-20261022T054719.271617500`：`passed_tests` 含 `TestWire_WorkspaceBrowsePortWiring`；`skipped_tests` 仅既有 `TestWorkspaceBrowse_NoSymlinkEscape`，见 U13 平台跳过依据） | 本机 Windows；T062 无前端 / 迁移面 |
| 实测（T062）·gosec | `gosec -quiet ./internal/app`（无管道）exit=1：仍是台账在案的 6 条既有命中（`resource_admin.go:598/1247/2149/2807` G304 ×4、`mcp.go:278/282` G602 ×2）；`web.go` / `wire.go` / `wire_test.go` 零新增命中 | 非 T062 引入；`make check` 的 `sec` 目标在修完前保持红，**不**加抑制注释 |
| 红态（T062） | 两个 overlay 探针（副本在仓库外、用毕即删）：① 删掉 `writeServices.workspace` 填槽行 → `Error: Expected value not to be nil.` + `Messages: T062 之后浏览端口必须已填槽`；② 构造时把 `Root: root` 改成 `Root: ""` → `Received unexpected error: 工作区浏览：工作区根未配置` | 逆改后逐字变红、恢复后绿；端口填槽与根传递各自有判别力 |
| 勾选 | **翻转 T062** | 装配、端口投影与装配级判据全部落地并通过；`-task T062` stage 报告已留证 |

### 1.61 T063 / T064 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | T063 的视图文件名落 `assets/console/src/views/workspace-browser.ts`：`views/workspace.ts` 已被 US-11 的 tools / memory / notifications / sandbox 四视图占用（tasks 适配说明已登记），路由仍为 `/console/workspace` | 视图只发相对路径（`/` 分隔），浏览根文案固定 `.basalt/agents/`，不拼宿主路径；软链接行只显示「不跟随」、无进入入口；目录 / 文件操作走 `DataTable` + 面包屑；预览用 `previewTicket` 丢弃晚到的旧响应，失败清空面板并展示信封文案；页面没有任何写入口（只读约束 40 / 注 19） |
| D2 | T064 在 `router.ts` 追加四条路由（`agents/new`、`resources/:kind/:id`、`archived`、`workspace`）：静态段 `agents/new` 排在动态段 `agents/:name` 之前；`resource-detail` 与 `resources` 共用固定三值 kind 校验；`app.ts` 的 `VIEWS` / `TITLES` 同步四个视图，Agents 侧栏激活名扩展到 `agents-new` / `archived`；`/workspace` 未新增侧栏项（模块卡未要求导航） | `router.test.ts` 从 12 条路径扩到 16 条，另加「静态先于动态」「kind 校验 + id 解码」两例；不存在第三方路由库、无新增运行时依赖 |
| D3 | 实施后跑前端全量用例，12 例全红：`agent-files` 4 例（`.files-etag` 为 `—`、找不到 `AGENT.md` 编辑器）、`notify-channels` 5 例、`resource-form` 3 例（`找不到创建请求`） | 根因经临时探针定位（副本在 `tests/`，用毕即删）：`vue-test-utils` 的 `mount` 把 props 包进 `Vue.reactive({})`（`vue-test-utils.cjs.js:8277`），类实例被深代理成 `Proxy` 后过不了 `ApiClient` 私有字段的品牌检查，逐字报 `TypeError: Receiver must be an instance of class ApiClient`（`api.ts:485`）。三个测试夹具的 `mount` 统一改为 `api: markRaw(new ApiClient(...))`；**不改任何断言**，生产代码零改动（真实应用经 `ctx` 浅层 props 传入，不会被深代理）。 |
| D4 | `notify-channels` 修好夹具后仍有 1 例：`.col-host` 取到 `['目标服务', 'hooks.example.com', '暂不可发送（地址为空）']` | `DataTable` 的 `columnClasses` 同时作用于 `th` 与 `td`（`components/ui.ts:73`，US-11 既有行为）；用例选择器收紧为 `tbody .col-host`，断言的目标与期望值一字未改 |
| 交付（T063/T064） | `assets/console/src/views/workspace-browser.ts`（新增）；`assets/console/src/router.ts`（+4 路由、RouteName 扩展）；`assets/console/src/app.ts`（VIEWS / TITLES / 导航激活）；`assets/console/tests/router.test.ts`（16 路径 + 2 例）；`assets/console/tests/agent-files.test.ts` / `notify-channels.test.ts` / `resource-form.test.ts`（夹具 `markRaw` + 一处 tbody 选择器） | 零新增前端依赖、零 Go 改动、零迁移 |
| 实测（T063/T064） | 便携 Node v22.20.0：`vitest run` → **8 文件 / 75 例全 pass**（含 `agent-files` 4、`notify-channels` 6、`resource-form` 6、`router` 22）；`vue-tsc --noEmit` → exit=0 | 本机 Node 版本低于 `.nvmrc` 固定的 24.22.0，读数为**参考值**；前端判据仍以 CI 前端 job（Node 24）为准，U6 保持「未验证」。临时探针文件已删除 |
| stage（T063/T064） | 不适用：前端任务无 Go stage 绑定 | 与 tasks.md / stage 一致；不为前端任务临时新增 Go 判据面 |
| 勾选 | **暂不勾选 T063 / T064**（与 T024 / T028 / T059 同口径） | 本地 75/75 与类型检查全绿仅是参考读数；待 CI 前端 job 与 T067C 的 dist / integrity 对账证据后由控制侧翻转 |

### 1.62 T065 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | `internal/e2e/harness_test.go` 的 US-12 写面夹具（+232 行：`adminWrite` / `requireWriteOK` / `requireLifecycle` / `readProfile` / `waitProfile` / `waitAgentRegistered` / `waitAgentGone` / `enableAgentRequest` / `waitAgentEnabled` / `adminAgentFiles` / `readLifecycleStateFile` 与 7 个判卷副本结构体）随 T065 落地；既有 e2e 辅助函数零改动 | 夹具只做视图字段的判卷投影（字段子集，`encoding/json` 忽略多余键）；创建 / 启用 / 归档 / 恢复的等待都走注册表与生命周期读数的既有端口，不引入第二套状态源 |
| D2 | `012-e2e` stage 草稿用 `-tags=integration` 跑 `./internal/e2e`，而该目录每个文件都是 `//go:build e2e` → 零测试执行、exit=0，`required_tests` 永远无法匹配 | 派生工件缺陷（不是实现缺陷）。控制侧改为 `-tags=e2e`，`-run` 与 `required_tests` 一字未动（判据面不变）；`harness/stages/current.json` 的 `015-backfill` 用 `-tags=e2e`、`015-e2e-vet` 用 `-tags=e2e,slow,real` 为同仓先例 |
| D3 | 首轮 `TestAdminConsoleE2E_ModelConnectionPublishRevert` 真实红：[admin_console_test.go:507](../../internal/e2e/admin_console_test.go#L507) 逐字 `创建模型连接：HTTP = 500，期望 200（信封 {"code":"INTERNAL","message":"服务器内部错误",...}）`；探针复现的服务日志逐字 `模型连接 aux 的定义原文无法解析: [1:3] sequence was used where mapping is expected` | 实现缺陷：`parseProviderDefinition` / `parseMCPDefinition` 只解基线存储的「剥离 `-` 指示符的映射」形态，而管理写路径按约束 4 存「块列表」原文；读视图因此 500（`GET /api/v1/model-connections` 同病）。修 [resources.go](../../internal/app/resources.go)：映射解码失败时回退按单条目序列解码，两函数对称；存储字节与哈希不变（只改读取输入形态） |
| D4 | 新增 app 层单测 `TestRunResources_ResourceViewsBlockDefinition`（[resources_test.go:655-693](../../internal/app/resources_test.go#L655-L693)）：以块列表形态种 `definition_yaml`，断言 Provider / MCP 两视图逐字段投影 | 新测试、不改既有断言；overlay 红态（仓库外探针，用毕即删）：还原单形态解析 → [resources_test.go:683](../../internal/app/resources_test.go#L683) 逐字 `expected: "https://g.example" / actual: ""`；恢复后 pass。该例随 `make check` 与 `012-race`（整包无 `-run` 过滤）执行；stage 判据面由卡片点名的 e2e 用例承担，不为它单加锚点 |
| 交付（T065） | `internal/e2e/admin_console_test.go`（新增 6 例：创建 → 启用 → 归档 → 恢复全流程、创建 / 复制 / 归档默认值、模型连接发布与显式撤回、技能引用闸门、工作区浏览与通知 CRUD、升级说明文档断言）；`internal/e2e/harness_test.go`（+232 行写面夹具）；`internal/app/resources.go`（两解析函数接受两种落库形态）；`internal/app/resources_test.go`（+1 例）；`harness/stages/us12-agent-resource-management.json`（`012-e2e` 的 `-tags` 修正） | 零新增依赖、零迁移、零前端改动；stage 文件为本地专用派生工件（`??` 未跟踪，不进 `current.json` / CI） |
| 实测（T065） | `gofumpt -l` 三文件零命中、exit=0；`go vet ./internal/app` 与 `go vet -tags=e2e ./internal/e2e` exit=0；`go test ./internal/app` → `ok … 17.501s`；stage `-task T065` → passed（报告 `accept-20261022T065851.229564100`，`passed_tests` 六例全量、`skipped_tests` 为空） | 本机 Windows；仓库外探针目录已删净（遗留的探针 serve 进程已终止） |
| 勾选 | **翻转 T065** | 六例落地、stage 判卷通过（`012-e2e` exit=0） |

### 1.63 T066 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 三例的落点：① `TestOperations_ListProjectsNewActionGroups`（11 条 US-12 动作组记录逐字投影、保持仓储顺序、四个新 target_kind 同表出现、未结束时间为空串）；② `TestOperations_FilterAcceptsNewTargetKinds`（四个新 kind × 新动作自由文本原样交给仓储、单次查询）；③ `TestOperations_DetailNewActionGroupsRedacted`（白名单闭集：版本号 / 哈希 / 路径照常投影；`command` / `env` / `transport` / 绝对路径与 `forced` / `before_state` / `archive_id` / `index_write` 一律不出站） | 模块卡 §3.2 只给「+3：新动作组的操作视图」，不点名函数名；三例分别钉住列表投影、过滤闭集与详情白名单。视图白名单属 US-11 的 [views.go:750-798](../../internal/web/views.go#L750-L798)（不在本 US 修改清单），扩展白名单是有意的契约变更——用例注释写明「先改白名单再改用例」，不反向放宽 |
| D2 | `012-web-etag-routes` 的 `-run` 正则原为 `^(TestIfMatch_\|TestAdminViews_\|TestAdminRoutes_\|TestContractAdmin_\|TestConsoleAdmin_)`，不含 `TestOperations_`——新例不会被该 check 执行，`-task T066` 会在「新例没跑」的情况下变绿 | 派生工件缺陷。控制侧只扩大 `-run`（追加 `TestOperations_`），`required_tests` 五锚点一字未动（判据面不变；先例 T020 / T036 / T045）。报告 `passed_tests` 实测含三例与七个子例 |
| 交付（T066） | `internal/web/handlers_operations_test.go`（+3 测试函数 / 7 个子例）；`harness/stages/us12-agent-resource-management.json`（`012-web-etag-routes` 的 `-run` 扩大） | 测试专用任务，生产代码零改动；无新依赖、无迁移 |
| 红态（T066） | `go test -overlay` 探针（仓库外副本，用毕即删）三处逆改：① `operationView` 的 `TargetKind` 改 `strings.ToUpper` → `TestOperations_ListProjectsNewActionGroups` 逐字 `expected: "agent" / actual: "AGENT"`；② `validOperationTargetKind` 删掉四个新 kind → `TestOperations_FilterAcceptsNewTargetKinds` 四个子例 `expected: 200 / actual: 400`；③ 详情白名单 `default` 改为放行 → 子例报 `should not contain "command"` 与 `Should be empty, but was map[forced:true …]` | 逐字预期失败文案成立；恢复后全绿 |
| 实测（T066） | `gofumpt -l` exit=0 零命中；`go vet ./internal/web` exit=0；`go test ./internal/web` → `ok … 4.189s`；stage `-task T066` → passed（报告 `accept-20261022T070140.554191300`，`passed_tests` 含三例全部子例） | 本机 Windows |
| 勾选 | **翻转 T066** | 三例落地、stage 判卷通过 |

### 1.64 T067 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | `docs/Operations.md` 六项内容已齐：§5.4「升级说明：MCP 命令白名单与旧格式迁移」（stdio → `shell.allowed_commands`、http → `http.allowed_domains`、未通过即拒绝连接且 WARN 只记 scheme + host；旧多词 `command` 读取兼容 + 管理台编辑被拒 + `command` + `args` 的 YAML 迁移示例）；§6 凭证（通知渠道只存 `${ENV_VAR}`、红线段）；§9「管理面写操作要点」（资源版本只追加 / 发布保旧 / MCP 待重启 / 归档恢复两条路径 / CLI `profile delete` 归档行可见 / 通知占位 / 工作区只读边界 / If-Match 412） | 与模块卡 §3.2 的 6 项文档交付逐项对应；不复制流程，只写运维边界与迁移动作 |
| D2 | `docs/GettingStarted.md` §9 末段给出 US-12 六个写面入口（`/console/agents`、`/console/resources/{kind}` 与详情、`/console/archived`、`/console/workspace`、`/console/settings/notifications`），并链接 `Operations.md` §5.4 / §9 | 只给入口与链接，不复制流程（T067 原文要求） |
| 实测（T067） | 官方便携 Node v24.22.0 + `markdownlint-cli2@0.23.3 --config .markdownlint.json docs/Operations.md docs/GettingStarted.md` → `Linting: 2 files` / `Summary: 0 issues in 0 files`、exit=0（临时目录用毕即删） | 本机无系统 Node；便携判卷法与 2026-10-17 实测口径一致。文档断言 `TestUpgradeNotes_Documented` 在 T065 的 `012-e2e` 报告 `accept-20261022T065851.229564100` 中通过（`shell.allowed_commands` / `http.allowed_domains` / `旧格式的多词` / `command: npx` / `args:` / 两条列表项 / ``改用 `args` 字段``） |
| 勾选 | **翻转 T067** | 六项内容 + 入口链接落地；markdownlint 零问题；文档断言已由 e2e 判卷 |

### 1.65 T067A 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | `verifyOperation` 分派扩到本 US 全部动作组：`agent.create` / `agent.copy` → 状态文件 + 目录树；`agent.files.update` → 受管文件集合 ETag；`agent.archive` → 状态 + 归档行 + 清单哈希；`agent.restore` → 归档行 + 状态 + 目录；`model_connection.*` / `mcp_server.*` / `skill.*` 写与发布 → 版本行 + 指针 + 定义文件；三类删除 → 删除标记 + 文件；两类测试 → 预写 detail；`notify_channel.*` → 行字段脱敏哈希（[baseline.go:809-839](../../internal/app/baseline.go#L809-L839)） | `agent.generate_files` 不写管理操作行（契约 §9.3 注 3），不进入分派；`system.baseline_v2` 等未列举动作保持 indeterminate（→ unknown），**不**猜 |
| D2 | 三态判据按契约 §9.4 逐组落地；单事务不变量决定了部分 applied 分支**只能**是防御分支：版本 + 指针 + 终态同事务、归档行 + 终态同事务（`InsertArchive`）、`MarkRestored` + 终态同事务、`MarkDeleted` + 终态同事务、测试 / 通知终态 detail 由 finish 写入——pending 行本不该命中，故这些分支用「行已存在且 op 匹配」做最强判据，测试只对归档行用裸 SQL 种行验证（与既有判卷先例同一手法） | 防御分支不是冗余：恢复必须在"别人手工补过行"时也能给出正确结论，且不能把 defense 分支当正常路径；`verifyResourceTest` 的 applied 分支同理，正常 pending 恒 `{}` |
| D3 | 原名恢复崩溃窗口（约束 44 点名）由 `compensateOriginalRestore` 处置：在 `Gate.Lock(target)` 内**重读**状态，要求 `disabled` 且 `operation_id` 是本次；`Transition(disabled, gen, archived, op.OperationID)` 只补偿状态、不复制归档内容（**不是重做**）；索引 `PutLifecycleIndex` best-effort（失败 ERROR，状态文件为准）；补偿失败 → indeterminate → `unknown`（[baseline.go:1122-1155](../../internal/app/baseline.go#L1122-L1155)） | 补偿后归档行保持 `restored_at IS NULL`、可重试；`Transition` 递增 generation（archived g → disabled g+1 → archived g+2），**不回退**代数；WARN 文案「状态已补偿回 archived 并置 failed」由用例逐字断言 |
| D4 | 通知渠道的预写 detail 必须先于目标写入：`notify_channel.*` 的记录改为 `notifyChannelOperationDetail{BeforeSHA256, AfterSHA256}`，哈希材料是 `channelRequestHash` 的四字段投影（name / type / url / description），不存原始地址；`Update` 保留原先被丢弃的 `store.Get` 行以按 `keepURL` 计算"生效后"哈希；`Delete` 记 before（[notify_admin.go](../../internal/app/notify_admin.go)） | 没有预写哈希就无法在崩溃后区分"仍是旧值"与"被第三方改写"；列表 / 恢复日志均不回显地址，脱敏口径与视图一致 |
| D5 | 恢复的 `RecoveryDeps` 新增六项可选依赖（`Root` / `ConfigPath` / `Skills` / `Gate` / `Archives` / `Notify`），生产装配在 [wire.go:392-413](../../internal/app/wire.go#L392-L413) 一次给全；某组查证缺依赖时该组只能 indeterminate（→ unknown），**不得**因装配少一项把操作猜成成功或失败 | 依赖显式传入而非包级变量（原则 7）；`newRecoveryEnv` 同步补齐同一工作区上的依赖，恢复夹具与生产同源 |
| 交付（T067A） | `internal/app/baseline.go`（8 个查证函数 + 9 个助手 + 分派扩展 + `path` 导入）；`internal/app/notify_admin.go`（`notifyChannelOperationDetail` 与三条写路径的预写 detail）；`internal/app/wire.go`（恢复装配六项）；`internal/app/recovery_operations_test.go`（夹具补齐）；`internal/app/agent_admin_test.go`（`TestRecoverAdmin_ActionMatrix` 11 子例 + 两个助手，含裸 SQL 归档行防御分支与原名恢复补偿）；`internal/app/resource_admin_test.go`（`TestRecoverAdmin_ResourceActionMatrix` 6 子例、`TestRecoverTest_UnknownNotResent`；夹具补 `db` / `logger`） | 零迁移、零新增依赖；不改任何既有断言；新增测试函数名英文，文档语义以中文注释保留 |
| 实测（T067A） | `gofumpt -w` 六文件 → 无输出；`go vet ./internal/app` exit=0；staticcheck 项目 checks（无管道直取退出码）exit=0、零输出；`go test -run '^(TestRecoverAdmin_\|TestRecoverTest_\|TestRecoverPendingOperations_)' ./internal/app` → `ok … 3.178s`；`go test ./internal/app` 全包 → `ok … 18.531s`；stage `-task T067A` → passed（报告 `accept-20261022T072426.646642000`，`012-app-reconcile` exit=0，`required_tests` 含两锚点） | 本机 Windows；`TestRecoverAdmin_ActionMatrix` 的上游既有 `TestRecoverPendingOperations_*` 八例同轮全绿 |
| 红态（T067A） | `go test -overlay` 探针（副本在仓库外、用毕即删）两处逆改：① `compensateOriginalRestore` 末行改 `VerificationIndeterminate` → `restore_original_crash_window_compensated` 逐字 `expected: "failed" / actual: "unknown"`（[agent_admin_test.go:1610](../../internal/app/agent_admin_test.go#L1610)）；② `verifyResourceTest` 末行改 `VerificationApplied` → `TestRecoverTest_UnknownNotResent` 逐字 `expected: "unknown" / actual: "success"`（[resource_admin_test.go:2788](../../internal/app/resource_admin_test.go#L2788)） | 两条断言对补偿与"测试不重发"各自有判别力；恢复后全绿，探针目录已删净 |
| 勾选 | **翻转 T067A** | 分派、三态判据、崩溃窗口补偿与两条锚点测试全部落地并通过；`-task T067A` stage 报告已留证 |

### 1.66 T067B 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 12 个精确并发用例的落点：10 个新增于既有测试文件——`internal/app/agent_admin_test.go` 三条、`internal/app/resource_admin_test.go` 两条、`internal/app/notify_admin_test.go` 一条、`internal/storage/resource_store_test.go` 一条、`internal/workspace/yaml_block_test.go` 一条、`internal/app/workspace_browse_test.go` 一条、`internal/web/handlers_admin_resources_test.go` 一条；另 2 个为既有用例（`TestReconcile_ConcurrentStartup` [definition_reconcile_test.go:632](../../internal/app/definition_reconcile_test.go#L632)、`TestMCPSupervisor_ConcurrentRestart` [mcp_supervisor_test.go:587](../../internal/app/mcp_supervisor_test.go#L587)），未新增文件、未改动两个既有用例正文 | 满足任务「分散在既有新增测试文件，不新增文件」；既有用例的判别力归属其原始任务，本任务不复测其红态 |
| D2 | 竞态洁净夹具三处要点：① web 用例覆盖默认假件（其 `srv.currentETag` 无锁改写），改用互斥量保护的局部 ETag 与假件；② 归档假件 `archiveExitCanceller` 用 `sync.Once` + channel 发取消信号，`WaitRuns` 只在 `exit` 关闭或 ctx 到期时返回；③ `require` / `json.Unmarshal` 只出现在测试 goroutine，goroutine 内只落 `httptest.ResponseRecorder` | 默认假件只服务单线程用例、本身并发不安全；`require` 在子 goroutine 调用会误用 testify 的测试协程 |
| 交付（T067B） | `internal/app/agent_admin_test.go`：`archiveExitCanceller` + `TestAgentAdmin_ConcurrentCreate` / `TestAgentArchive_ConcurrentRun` / `TestAgentRestore_ConcurrentCopy`；`internal/app/resource_admin_test.go`：`TestResourceAdmin_ConcurrentPublish` / `TestResourceAdmin_DeleteDuringPublish`；`internal/app/notify_admin_test.go`：`TestNotifyAdmin_ConcurrentDelete`；`internal/storage/resource_store_test.go`：`TestResourceStore_ConcurrentVersionNo`；`internal/workspace/yaml_block_test.go`：`TestYAMLBlock_ConcurrentReaders`；`internal/app/workspace_browse_test.go`：`TestWorkspaceBrowse_ConcurrentRead`；`internal/web/handlers_admin_resources_test.go`：`TestAdminResources_ConcurrentIfMatch` | 10 个新增函数名英文、中文语义以注释保留；每个用例在并发结束后断言完整状态（版本号集合 / 指针 / 文件字节 / 终态行数），不只断言「无错误」 |
| 实测（T067B） | `gofumpt -w` 七文件 → 无输出；`go vet ./internal/app ./internal/storage ./internal/workspace ./internal/web` exit=0；十个新增用例 `go test -race -run '^(…)$' -count=1` 四包 → `ok`（app 3.324s / storage 1.903s / workspace 1.393s / web 1.580s）；stage `--stage-only --task T067B` → `passed 012-race (exit=0)`（报告 `accept-20261022T073429.614652100`，41.348s），`required_tests` 12 个名称逐一命中 `passed_tests` | 本机 Windows；`012-race` 整包 `-race` 运行，12 个锚点均为非 SKIP 实跑 |
| 红态（T067B） | 三个 `go test -overlay` 探针（副本在仓库外、`perl -0pi -e` 变异、用毕已删）：① 删除 `Create` 两处 `ensureCreateTargetFree` → `TestAgentAdmin_ConcurrentCreate` 逐字 `expected: *core.InvalidInputError / in chain: "同名目录已存在，拒绝覆盖：agents/racer…"`（[agent_admin_test.go:1768](../../internal/app/agent_admin_test.go#L1768)）；② `awaitArchiveRuns` 的 `len(runIDs)==0` 改 `if true`（不发取消信号）→ `TestAgentArchive_ConcurrentRun` 逐字 `归档未在等待窗口内发出取消信号`（[:1824](../../internal/app/agent_admin_test.go#L1824)）；③ 更新 handler 把 If-Match 置空后传端口 → `TestAdminResources_ConcurrentIfMatch` 逐字 `Should not be: "sha256:f6dc…"` + `412 必须回传写后的当前 ETag`（[handlers_admin_resources_test.go:1641](../../internal/web/handlers_admin_resources_test.go#L1641)） | 三条分别锁定「写锁内目标复核」「归档取消与等待」「条件写头逐字透传」；其余 9 个未逐条逆改，如实登记未覆盖：2 个为既有用例；`TestYAMLBlock_ConcurrentReaders` / `TestWorkspaceBrowse_ConcurrentRead` 为纯读路径，当前无共享可变状态可逆改（人为加包级状态属制造缺陷，不是逆改）；`TestResourceStore_ConcurrentVersionNo` 的串行由单写连接 + UNIQUE 约束 + 事务共同保证，无单行逆改点；另外 4 个的资源锁 / RowsAffected 串行移除后结果依赖时序，无法取得逐字确定红态。不以静态推演冒充红态 |
| 勾选 | **翻转 T067B** | 12 个用例全部落地，stage `012-race` 全绿且 12 个 `required_tests` 命中；三条逆改红态逐字成立 |

### 1.67 T067C 实施派生记录（2026-10-22）

| # | 事实 | 处置与依据 |
| --- | --- | --- |
| D1 | 本机 PATH 仍无 Node（复测 `node` / `npm` → `command not found`）；改用官方便携包 Node **24.22.0**（与 `assets/console/.nvmrc` 完全一致）：`nodejs.org` zip 37,733,703 字节，按同目录 `SHASUMS256.txt` 校验 `f9683a19…f321f80c3` 相符 | 只在仓库外临时目录解压使用，用毕即删；不以低版本读数冒充 CI 版本（对照 §1.61 的 Node 22 参考读数） |
| D2 | `npm ci` → `added 118 packages in 14s`（npm 11.19.1）；`npm test` → **8 文件 / 75 例全 pass**（1.40s）；`npm run build` → `vue-tsc --noEmit` 通过 + Vite 36 模块 + `integrity.json: 3 个文件` | 任务要求的三步全部实跑；依赖安装走既有 `package-lock.json`（resolved 指向 registry.npmmirror.com，未改任何 npm 配置） |
| D3 | 重建产物：`index.html` 与 `integrity.json` 修改、旧 `assets/index-IrlgPqCY.js` 删除、新 `assets/index-1O0-hbj4.js` 新增；**连续两次 build 的 dist 文件 sha256 完全一致** | 可复现性以「二次构建字节相同」实测；提交后 CI 的 `git status --porcelain -- assets/console/dist` 为空是同一事实的提交后断言，待用户提交闭环 |
| D4 | 按 `ci.yml` console job 的内联脚本逐字复跑完整性判据 → `ok: 3 个文件与 integrity.json 一致`（path 集合 + 逐文件 sha256）；`go test -run '^TestConsoleDist_' ./internal/web` → `ok`（嵌入产物集合 / 哈希 / 目录白名单） | 与 CI 判据同一逻辑；Go 侧用例验证 `go:embed` 的 dist 与清单自洽 |
| 实测（T067C） | `Makefile` 与 `scripts/` 对 `node` / `npm` 的调用零命中（唯一 grep 命中是 `scripts/stmtcount/count.go` 的 `ast.Node` 类型，不是外部工具）；`git status --porcelain -- assets/console/dist` → 四行：删除 `assets/index-IrlgPqCY.js`、修改 `index.html`、修改 `integrity.json`、未跟踪 `assets/index-1O0-hbj4.js` | Go 门禁不依赖 Node（约束 42）；dist 与 integrity 更新留在工作区，随本轮改动由用户提交 |
| 勾选 | **翻转 T067C** | 三步实跑全绿、dist / integrity 已更新且二次构建字节一致；提交后 CI 的干净 worktree 断言由提交动作完成 |

### 1.68 勾选补记（2026-10-22）

按 §1.67 的 Node 24.22.0 证据一次性对账前端与 reconcile 任务的「暂不勾选」条件：

| 任务 | 依据 |
| --- | --- |
| T024 / T028 / T029 / T038 / T059 / T063 / T064 | 各节「暂不勾选」的唯一条件是 CI 前端 job（Node 24）判卷。§1.67 D2 已用 `.nvmrc` 同版 Node 24.22.0 实跑 `npm ci` → `vitest` 8 文件 75 例 → `vue-tsc --noEmit` + `vite build`，覆盖这些任务新增的文件与用例（agent-files 4 / notify-channels 6 / resource-form 6 / router 22 等）；U6 同步翻为已验证 |
| T030 | §1.27 的唯一条件是 `012-app-reconcile` 锚点集到 T067A 才完整；T067A stage 报告 `accept-20261022T072426.646642000` 的 `012-app-reconcile` exit=0，条件满足 |

### 1.69 T068 stage 派生记录（2026-10-22）

| 项 | 内容 |
| --- | --- |
| D1 十八条 check | check ID 与模块卡 §4.4 逐字一致（18/18）。`args` 按实现落位修正：`012-app-mcp-supervisor` 增 `./internal/tool`（`TestMCPPool_*` 所在）；`012-cli-author` 增 `./internal/app`（`TestCLIProfileCreate_/Delete_`）；`012-web-admin-agents` 增 `./internal/app`（`TestArchivedList_IncludesCLIArchive`）；`012-e2e` 用 `-tags=e2e`（源码全为 `//go:build e2e`，`integration` 零测试）。另把 U11 的 `TestSkill_*` / `TestSkillWrite_*` 并入 `012-app-resource-admin`，`TestValidateAuthorConfig_*` 并入 `012-app-notify-author`，web 的 `TestModelConnection_*` / `TestAdminResourceVersions_*` / `TestAdminResourcePublish_*` 并入 `012-web-admin-resources`；断言与选择语义未弱化。 |
| D2 required_tests | 17 个 check 按 `go test -list` 精确展开，`012-race` 保留卡片固定 12 例；合计 602 个名字。四个平台跳过用例不入 required_tests：`TestParseMCPServers_ReadsThroughRoot`（跳过子例）、`TestNotifyScan_DoesNotFollowSymlinks`、`TestDeleteAgent_RefusesSymlinkEntry`、`TestWorkspaceBrowse_NoSymlinkEscape`（Windows 无 `SeCreateSymbolicLinkPrivilege`；测试仍在交付面，CI Linux 可执行）。卡片四族在实现中迁移 / 合并：workspace `TestAgentAdmin_*` → app（`012-app-agent-admin`）、workspace `TestAgentDraft_*` → core `TestAuthor_Draft*` + workspace `TestAgentFiles_*`、workspace `TestValidateAgentName_*` → `TestCreateAgent_RejectsInvalidName` / `TestAgentFiles_PathMatrix`、storage `TestResourceVersions_*` → `TestResourceStore_*`；无交付用例丢失（stage approval 同记录）。 |
| D3 protect | 95 路径逐文件枚举（`loadStage` 对每路径 `Lstat` 实测通过）：23 修改生产（stage 原 21 + `internal/app/resources.go` + `internal/web/handlers_agent_content.go`）+ 25 修改既有测试（卡片 20 + `recovery_operations_test.go` / `resources_test.go` / `wire_mcp_test.go` / `wire_test.go` / `content_edit_test.go`）+ 28 新增测试 + 4 契约 + `go.mod` / `go.sum` / `Makefile` / `harness/acceptance.json` + 11 脚本。 |
| D4 可见性 | `harness/stages/current.json` 零命中；`.github/workflows/` 零命中；`inputs` 含私有 `docs/us/**`，仅本机执行。 |
| D5 实测读数 | `--stage-only` 四次：`T067A` 绿（`012-app-reconcile`，34 required）；`T019` 绿（`012-app-notify-author`，32 required，3 个新名字在 passed_tests）；`T026` 绿（`012-web-admin-resources`，41 required，9 个新名字在 passed_tests）；`T040` 绿（`012-app-resource-admin`，71 required，`TestSkill_*` / `TestSkillWrite_*` 在 passed_tests）。报告：`.loop/runs/accept-20261022T075048.304900000` / `…075247.632259900` / `…075252.988704900` / `…075256.462729800`。 |
| 勾选 | T068 → `[x]`。 |

### 1.70 T069 七目标门禁记录（2026-10-22）

三轮 `GOPROXY=https://goproxy.cn make check`：首轮停在 `sec`（gosec 11 处）、二轮停在 `test-integration`（US-7 集成层旧 command 形态），均记 §2；修复只改实现 / 随已批准接口适配（命令与参数分离），未用任何抑制注释、未放宽判据。第三轮 exit=0，七目标读数：

| 目标 | 四态 | 本轮读数（`/tmp/us12-make-check3.log`） |
| --- | --- | --- |
| fmt | 通过 | `check-format: gofmt / gofumpt 均干净`（先查退出码再判输出） |
| lint | 通过 | `go vet ./... && staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...` exit=0，零诊断 |
| sec | 通过 | `gosec -quiet ./...` 零命中；`govulncheck` 输出 `No vulnerabilities found.` |
| test | 通过 | `go test -race -cover ./...`：18 个包 `ok`，零 FAIL |
| test-integration | 通过 | `go test -tags=integration ./...`：18 个包 `ok`（含 `internal/tool` 四例真实子进程集成） |
| cover | 通过 | `scripts/check-coverage.sh` exit=0；`internal/e2e` 记 `[no test files]` / `无可执行语句` → 按判据第 3 类跳过（有依据） |
| build | 通过 | `CGO_ENABLED=0 go build -trimpath -ldflags … -o bin/basalt ./cmd/basalt` exit=0；产物 `bin/basalt` 18,765,824 字节、PE32+ x86-64（16:10） |

附：`internal/tool/mcp_integration_test.go` 随 FR8 接口适配补入 stage `protect`（95 → 96），approval 已留注；断言逐字未改，原「路径含空白跳过」条件因切分语义消失而失效（覆盖只增不减）。勾选：T069 → `[x]`。

### 1.71 T070 stage 全量验收记录（2026-10-22）

命令：`GOPROXY=https://goproxy.cn sh scripts/check-acceptance.sh --stage harness/stages/us12-agent-resource-management.json`，日志 `/tmp/us12-stage-full.log`，`EXIT=0`。报告：`.loop/runs/accept-20261022T081108.421611900/report.json`（`scope=full/us12-agent-resource-management-r1`，`status=passed`，`go1.27.3`，`base_commit=383d89e`，`dirty=true` 属预期——本 US 改动尚未提交）。

| # | 检查 | 四态 | 本轮读数（required 命中 / 跳过） |
| --- | --- | --- | --- |
| 1 | fmt | 通过 | exit=0，零诊断 |
| 2 | lint | 通过 | exit=0，零诊断 |
| 3 | sec | 通过 | exit=0（gosec 零命中、govulncheck 无漏洞） |
| 4 | test | 通过 | exit=0，18 包 ok |
| 5 | test-integration | 通过 | exit=0，18 包 ok |
| 6 | cover | 通过 | exit=0（判据脚本按包判卷） |
| 7 | build | 通过 | exit=0 |
| 8 | eval-build | 通过 | exit=0 |
| 9 | behavior | 通过 | exit=0 |
| 10 | injection | 通过 | exit=0 |
| 11 | 012-core-author-agent | 通过 | required 37 / skipped 0 |
| 12 | 012-core-resource-types | 通过 | required 6 / skipped 0 |
| 13 | 012-workspace-yaml-provider | 通过 | required 150 / skipped 1（`TestParseMCPServers_ReadsThroughRoot/软连接越界被挡下`，平台跳过、不入 required） |
| 14 | 012-workspace-skill | 通过 | required 78 / skipped 1（`TestNotifyScan_DoesNotFollowSymlinks`，同上） |
| 15 | 012-workspace-agent | 通过 | required 74 / skipped 1（`TestDeleteAgent_RefusesSymlinkEntry`，同上） |
| 16 | 012-storage-admin | 通过 | required 50 / skipped 0 |
| 17 | 012-app-resource-admin | 通过 | required 77 / skipped 0 |
| 18 | 012-app-agent-admin | 通过 | required 45 / skipped 0 |
| 19 | 012-app-mcp-supervisor | 通过 | required 59 / skipped 0 |
| 20 | 012-app-reconcile | 通过 | required 53 / skipped 0 |
| 21 | 012-app-notify-author | 通过 | required 88 / skipped 1（`TestWorkspaceBrowse_NoSymlinkEscape`，同上） |
| 22 | 012-web-admin-agents | 通过 | required 38 / skipped 0 |
| 23 | 012-web-admin-resources | 通过 | required 55 / skipped 0 |
| 24 | 012-web-admin-notify-workspace | 通过 | required 15 / skipped 0 |
| 25 | 012-web-etag-routes | 通过 | required 70 / skipped 0 |
| 26 | 012-cli-author | 通过 | required 37 / skipped 0 |
| 27 | 012-e2e | 通过 | required 6 / skipped 0（`TestAdminConsoleE2E_*` 五例 + `TestUpgradeNotes_Documented`） |
| 28 | 012-race | 通过 | required 2683 / skipped 10（全部为 Windows 无软链接权限的平台跳过子例，逐条列于报告的 `skipped_tests`；`required_tests` 未包含它们） |

汇总：28/28 `passed`，`required_tests` 命中合计 3621，退出码 0。四条平台跳过用例在 §4 U8 / U12 / U13 已登记（软链接权限），本次读数为同一事实的复核。勾选：T070 → `[x]`。

### 1.72 T071 Demo A–I 执行记录（2026-10-22）

执行面：`quickstart.md` §5 的九条 Demo 中可脚本化的部分——真实二进制经 HTTP 驱动（`-tags=e2e`），以及与各 Demo 同判据的自动化用例；页面交互、真实模型 / webhook / MCP server、真实 30 秒长跑标未验证。结果写入 `specs/012-agent-resource-management/acceptance-report.md` §2（含逐 Demo 四态表与未验证清单）。

| 证据 | 命令 / 日志 | 本轮读数 |
| --- | --- | --- |
| e2e 六例 | `go test -tags=e2e -count=1 -v -run 'TestAdminConsoleE2E_\|TestUpgradeNotes_Documented' ./internal/e2e`（`/tmp/us12-demo-e2e.log`） | 6/6 PASS，EXIT=0（FullFlow 1.49s / CreateCopyArchivedDefaults 0.18s / ModelConnectionPublishRevert 0.16s / SkillReferenceGate 0.80s / WorkspaceAndNotify 0.15s / UpgradeNotes 0.02s） |
| Demo 判据单测（三批） | `go test -count=1 -v -run '<各 Demo 用例>' ./internal/app ./internal/workspace`（`/tmp/us12-demo-unit*.log`） | 25 + 8 + 5 例全 PASS，EXIT=0；含 `TestAgentArchive_TimeoutRollsBack`（注入时钟推进 31s）、`TestMCPCreate_PublishesPendingRestart`、`TestSkillImport_BoundaryMatrix`、`TestNotifyAdmin_DeleteReferenced`、`TestReferences_ActiveRunRefs`、`TestAgentAdmin_GenerateFiles*` |
| 未验证项 | — | 页面交互全部步骤；真实 MCP server 连接与重启追平；真实长跑 30 秒等待；真实 webhook 投递；软链接子断言（平台，沿用 §4 U8 / U10 / U12 / U13）；只读文件系统真机注入 |

勾选：T071 → `[x]`。

### 1.73 收口修复轮与最终验收记录（2026-10-22）

独立复核（独立上下文子代理，不读本文件与验收报告）总结论**阻断**：F-01（阻断）、F-02（高）、F-03–F-06（中）、F-07–F-08（低）。逐条处置与 `file:line` 证据写入验收报告 §3.2；本节记录修复与最终读数。

| id | 处置 | 落地位置与复验 |
| --- | --- | --- |
| F-01 | 修：按传输分支拨号，新增真实 HTTP 传输（零子进程、拒绝重定向） | `internal/tool/mcp.go:321/338`；`TestMCPHTTP_*` 4 例 |
| F-02 | 修：删除 / 发布崩溃窗口按已落盘内容幂等补齐 | `internal/app/resource_admin.go:2196/1259`；3 例重试用例 |
| F-03 | 修：`markApplied` 写库失败时失效连接并回落未连接 | `internal/app/mcp_supervisor.go:254` `abandonApply` |
| F-04 | 修：测试端点先解析 `${VAR}` 再连接 | `internal/app/resource_admin.go:1756` |
| F-05 | 修：形状级 412 在可解析时带 `data.current_etag` | `internal/web/admin_routes.go:165` |
| F-06 | 裁决接受：`RestoreAgentDir` 成功 → `MarkRestored` 前崩溃按 `unknown` 收口、不自动补做（FR43 / 约束 44） | `internal/app/baseline.go:1095`；U14 |
| F-07 | 裁决按约束 1a 收口：`pending` 之后失败置 failed + 可查 `operation_id`；预写前 412 仍零操作行 | `agent_admin.go:1371` `postPendingError`；`handlers_admin_agents.go:311` |
| F-08 | 修：`recovery.go` 注释改为活动分支；卡片点名 → 实际测试映射表入验收报告 §3.2 | `internal/workspace/recovery.go:19` |

控制侧自查同轮修复：`postPendingError` 覆盖 Agent 创建 / 提交 / 复制 / 归档 / 恢复与通知写面（`writeAdminDataError` + `ErrorDataView.operation_id`）；`archiveTargetConflictError` 把归档目标冲突提升为 409；`RemoveEntry` 调用处 `:=` 变量遮蔽改为显式赋值（`resource_admin.go:2202`）。stage 锚点审计：12 个已交付测试补入 `required_tests`，`go test -list` 逐一验证被 `-run` 命中，`--lint-stages` 返回 `stage lint: ok`；stage 最终 sha256 `62c637c0b42d8766b39e611aeae55024da8bccc891f4f1eaeec7781c57739e90`。

| 最终门禁 | 读数 |
| --- | --- |
| `make check` 第四轮 | exit=0；fmt / lint / sec（govulncheck 无漏洞）/ test（`-race`）/ test-integration（`-tags=integration`）/ cover（全部包达标）/ build（`CGO_ENABLED=0`）全绿；日志 `/tmp/us12-make-check4.log` |
| 交叉编译 | `CGO_ENABLED=0 go build ./...` 与 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...` 均 exit=0（修复轮后重跑） |
| stage-only 全量 | 18/18 passed、exit=0，`required_tests` 615/615 命中、0 缺失，passed_tests 3673、skipped 14；报告 `.loop/runs/us12-stage-final/report.json` |
| 阶段全量（28 check） | 28/28 passed、exit=0；同源码最终证据；报告 `.loop/runs/us12-stage-final-full/report.json`（`scope=full/us12-agent-resource-management-r1`，`go1.27.3`，`base_commit=383d89e`，`started_at 2026-10-22T09:40:36Z`） |
| 判据面 | 只收紧：补 12 个 `required_tests` 锚点；无删除、无放宽、无阈值变更 |

> 计数口径：第三轮全量报告写的「3621」是 `passed_tests` 合计，不是 `required_tests` 命中数；最终全量报告按两类分别列出（required 615 / passed 3673）。

### 1.74 R3.3 判据修订记录（2026-10-22）

**触发**：收口评审发现 §4.3.2 `TestResourceAdmin_PublishFailureKeepsOldPointer` 在 R3.2 补 `VersionID` 后，前置条件仍是「只发布过 v1 且 v1 即当前指针」——撤回目标与被替换版本是同一份内容，定义文件改写是空操作，「切换失败后保留当前版本」没有被真实验证。对照发布契约（`docs/contracts/openapi.yaml:831`），不带版本号且最新版本已发布时是幂等返回、不插入版本，模块卡原示例的前置条件写错。

**裁决**（用户，2026-10-22）：按第一项修订，**不修改约束 9、FR-024 与发布契约**；第二项（改发布契约）会破坏产品语义，第三项（不采用该用例）没有必要。五点要求：前置改为已发布 v1、v2 且指针 / 运行时在 v2；显式指定 v1 的版本 ID、使用当前 v2 的有效 ETag、注入新版本插入失败；确认注入实际触发并断言错误、文件恢复、指针 / 运行时仍为 v2、无残留新版本、操作 failed；单独保留幂等测试；记录依据、同步模块卡 / 任务 / stage 指纹、保留历史失败记录后再复跑。

| 项 | 落地 |
| --- | --- |
| 用例 | `internal/app/resource_admin_test.go` 的 `TestResourceAdmin_PublishFailureKeepsOldPointer`：v1（a.example / m1）→ v2（b.example / m2）→ 显式撤回 v1（`ExpectedETag` = v2 内容哈希）+ `failNextVersionInsert("disk full")` |
| 断言 | 日志含注入文案（证明提交段真失败）；`AdminError` 分类 + `operation_id`；文件字节等于操作前；published / applied 仍为 v2；运行时同一客户端且 `models["main"] == "m2"`；版本数不变且最新仍为 v2；操作行 failed / `lifecycleResultInternal` / `发布事务失败` / ExpectedETag / RequestedVersionID |
| 幂等覆盖 | 保留 `TestPublish_Idempotent`（模型连接）与 `TestPublish_SkillIdempotent`（技能），不改断言 |
| 红态 | `go test -overlay` 短路 `commitModelConnection` 的文件恢复调用 → 本用例逐字失败「失败后定义文件必须恢复为操作前字节（v2 内容）」，实际文件留在 v1 字节；旧断言（指针 + 非 nil）按断言内容与失败分支写序推断在该逆改下仍会通过（属推断，未单独执行旧版用例）。探针在仓库外，用毕即删 |
| 文档同步 | 模块卡 §4.3.2 与文末 R3.3 节、tasks.md T072、stage approval 指纹（模块卡 `daccf82d…` 2287 行；tasks `4acc1d14…` 307 行 / 75 任务；最终 stage sha256 `70b9b9ce…`） |
| 边界 | 约束 9、FR-024、`docs/contracts/openapi.yaml` 均未改动；无生产代码改动 |

**复跑读数**：见验收报告 R3.3 节与本节下方「R3.3 复跑」记录；新增失败台账行只追加，不改历史行。

### 1.75 R3.4 页面操作闭环与浏览器走查记录（2026-10-22）

**触发与裁决**：用户按交付页面验收时指出「后端能力存在，但用户从页面无法完成操作，产品交付仍然有缺口」，裁决补齐新建 / 复制 / 归档 / 恢复的入口与操作路径；不新增路由、API 或依赖，不改正式契约。范围与逐字文案见模块卡 R3.4（`docs/us/us12-agent-resource-management.md`）。

**落地文件（US-12 增量，US-11 历史记录不改写）**：

| 文件 | 内容 |
| --- | --- |
| `assets/console/src/views/agents.ts` | 页头「新建 Agent」/「查看已归档」；空状态同一入口；`?archived=1` 到达说明 |
| `assets/console/src/views/agent-detail.ts` | 「复制配置」对话框（默认停用 / 清空定时提示，成功进入 `?copied=1`）；危险操作区「归档 Agent」（逐字确认文案、409 点名 Run、不自动重试、重复提交保护）；复制 / 恢复到达页约 5 秒有界重试与「正在确认…」状态 |
| `assets/console/src/views/archived-agents.ts` | 恢复成功进入 `?restored=1` 详情；失败留在原页显示服务端文案 |
| `assets/console/src/styles.css` | `.page-actions` / `.card.danger-zone` / `.empty button` 最小样式 |
| `assets/console/tests/agent-actions.test.ts` | 9 条操作路径用例（含注册表滞后重试），全部经真实视图与路由 |

**验证读数（2026-10-22 本轮当场）**：

| 项 | 读数 |
| --- | --- |
| 前端测试 | `npm test`：9 文件 / 87 例全 pass（`agent-actions.test.ts` 9 条） |
| 类型与构建 | `vue-tsc --noEmit` + `vite build` exit=0；dist 三个文件与 `dist/integrity.json` 逐项 sha256 一致：`index.html` `f88b16f8…`、`index-UrjTscUs.js` `fb5e4491…`、`index-BrfLMlNY.css` `8c500bd3…` |
| 二进制 | `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=383d89e"`；18,861,056 字节，sha256 `ee3867cf…`，内嵌新 JS（`UrjTscUs` 命中 3 次）；复制到演示目录后 8080 实际返回 JS `fb5e4491…`、index `f88b16f8…` |
| 浏览器走查 | 无头 Edge CDP：8/8 步通过、零页面错误；列表新建 → 复制 → 归档（逐字文案）→ 恢复，截图与 `result.json` 在 `.loop/demo-us12-r34b/`；脚本在仓库外 |
| stage-only 复跑 | 18/18 check passed、exit=0；`required_tests` 615/615、passed_tests 3673、skipped 14；`record.criteria.stage_sha256=9529c975…`、`source_fingerprint=6d0a4119…`；报告 `.loop/runs/accept-20261022T144544.554587100/report.json` |
| 冻结输入 | 模块卡 `ebfc9772…`（2326 行）；tasks `40f4ff23…`（325 行 / 83 任务，运行时 T079 未勾选、T073–T078 与 T080 已勾选）；stage 文件 sha256 `9529c975…`；T079 翻转后最终 tasks sha256 `cd27be4f…`（仅勾选位变化） |

**走查发现与处置**：

- 注册表滞后：复制响应返回时注册表尚未刷新（`updated_at` 14:40:06 / 14:41:41，名称时间戳 14:40:04 / 14:41:39），详情页只读一次 profiles 会把新 Agent 误报「未找到」。已加有界重试（20 × 250ms）与重试期间「正在确认新 Agent 的注册状态…」，并补回归用例；窗口结束仍无结果才显示未找到。
- 走查第 5 轮仍失败的原因是判读时机：脚本在 URL 变化后立即读 body，落在「首次读取完成、首次重试未发起」的间隙；页面当时显示未找到。修正后脚本等待详情就绪再判读，8/8 通过。过程读数保留在失败台账。
- 演示服务曾以旧二进制运行（PID 40836），页面加载旧产物；替换为新二进制并重启后，JS / index 哈希与 `integrity.json` 一致。旧的 CSS「缺失」告警是探针文件名大小写笔误（`BrflMlNY` vs `BrfLMlNY`），非产品缺陷。
- 创建页 `refreshLifecycle` 仍可能在注册表刷新前显示「已创建的 Agent 尚未出现在注册表中…」并暂不显示「打开 Agent 页面」；属既有行为、自行恢复，本次未改。Demo J 走查改经 Agents 列表进入，不影响操作闭环。

**限制**：无头浏览器走查覆盖功能路径与截图，不等同于人工视觉验收；真实模型 / webhook / 30 秒长跑仍未验证（U3、U5 等维持原状态）；演示库保留本次与早前走查创建的 Agent，未清理。人工 review ② 仍待用户决定。

### 1.76 R3.6 Agent 通知渠道声明与渠道状态记录（2026-10-23）

**触发**：用户人工验收三问——(1) 自建 Agent「github新闻」创建会话与定时任务运行都失败；(2) Agent 页面不能配置该 Agent 可用哪些通知渠道；(3) 已填飞书 webhook 的渠道仍显示「暂不可发送（地址为空）」。范围与逐字文案见模块卡 R3.6（`docs/us/us12-agent-resource-management.md`）。

**根因证据（已验证）**：

- 第 1 问：演示库两次失败运行 `web-1792683909208-t8fcbp` / `scheduler-1792683992622-gz86zz` 的 `provider.model` 是模板占位符「请在此填写模型名」，DeepSeek 返回 400，逐字「The supported API model names are deepseek-flash, deepseek-v4-pro, but you passed 请在此填写模型名」；同一 Agent 把 model 改成 `deepseek-chat` 后，`scheduler-1792684084265-jjfjrj` / `web-1792684127339-j9kl8f` 均 completed。占位符能过校验、能注册，只在每次模型调用处失败——页面此前没有提前告警。约束 28 明确禁止从 provider 默认模型自动回填，故本 US 只做**提前可见**（详情页告警 + 新建页文案），不改运行语义。
- 第 3 问：渠道地址存的是 `${FEISHU_WEBHOOK_URL}` 引用，存储侧非空；只读面只投影 `url_host`，引用解析不出 host → 页面把空 host 一律当「地址为空」。属**投影语义缺失**，不是地址没配置。

**裁决**（用户，2026-10-23）：先做「可用通知渠道多选」，保留调用时显式指定渠道，**不引入默认渠道 / 自动发送**；全局页反向展示「文档引用的 Agent」并注明是扫描线索、精确范围看 `notify_channels`；页面统一叫「通知渠道」（入站路由归后续模块）；后端必须给出明确的配置 / 解析状态。

**落地文件（R3.6 增量）**：

| 层 | 文件 | 内容 |
| --- | --- | --- |
| workspace | `internal/workspace/agent.go` / `agent_test.go` | 解析 `notify_channels`（空 / 缺省 = 不限制）；空白元素 WARN；未登记名保持可加载 |
| workspace | `internal/workspace/content_edit.go` / `content_edit_test.go` | `changes.notify_channels` 补丁：缺省不改、`[]` 清空、非空全量替换；改工具 / 技能时不重写该块 |
| core | `internal/core/profile.go` / `run.go` / `service.go` / `service_test.go` | 声明随 Run 准入冻结进 `RunEnv`，`ToolEnv` 携带同一份 |
| tool | `internal/tool/notify.go` / `notify_test.go` | 发送前按冻结声明收紧：声明外渠道 `not_found` 并点名限制；全未登记给专门文案；空声明不限制 |
| web | `internal/web/views.go` / `handlers_notify_read.go` / `handlers_notify_read_test.go` | `NotifyChannelView` 增 `url_configured` 与 `referenced_by`；列表读取失败固定 500（失败关闭） |
| web | `internal/web/handlers_admin_notify.go` / `handlers_admin_notify_test.go` | 写成功回包与列表同源；写已落盘后扫描失败只降级空引用并 WARN（不回 500 诱发重试） |
| app | `internal/app/ports.go` / `web.go` | `NotifyReferences` 端口接线，与删除闸门共用 `ScanAgentDocuments` / `notifyReferenceHits`，不建第二套扫描 |
| console | `assets/console/src/api.ts` / `components/notify-channels.ts` / `views/agent-detail.ts` / `views/agents-new.ts` | 类型补字段；三态显示（host / 环境变量引用 / 未配置）；详情页多选绑定 + 「管理全局渠道」链接；占位模型名告警 + 新建页文案 |
| console 测试 | `assets/console/tests/notify-channels.test.ts` / `agent-operations.test.ts` | 三态与文档引用、渠道绑定与占位告警（+2 例） |
| 契约 / 文档 | `docs/contracts/openapi.yaml` / `agent-md.md` / `run-and-errors.md` / `docs/Operations.md` | `url_configured` / `referenced_by` / 补丁字段 / 声明校验第 9 条；运维手册写清三态与「引用 ≠ 绑定」 |
| stage | `harness/stages/us12-agent-resource-management.json` | 四个相关 check 扩大 `-run` 并补 13 个锚点（635 条 required）；`protect` 96 → 107；approval 追加 R3.6 段与指纹 |

**验证读数（2026-10-23 本轮当场）**：

| 项 | 读数 |
| --- | --- |
| Go 新增 13 例 | workspace 4（`TestAgentLoader_ParsesAllFrontmatterFields` / `_NotifyChannelsBlankRejected` / `_NotifyChannelsUnknownNameStaysLoadable` / `TestApplyChanges_NotifyChannelsPatchOnly`）、core 1（`TestAgentService_Resources_ToolEnvCarriesFrozenNotifyChannels`）、tool 3（`TestNotifyTool_DeclaredAllowlistGatesChannels` / `_EmptyDeclarationKeepsBackwardCompatibility` / `_DeclaredButUnregisteredExplainsEmptyIntersection`）、web 5（`TestNotifyRead_UnparseableURLsYieldEmptyHost` / `_ListCarriesDocumentReferences` / `_ReferenceScanFailureFailsClosed`、`TestAdminNotify_WriteViewCarriesDocumentReferences` / `_WriteReferenceScanFailureKeepsWrite`）；13 条在 `012-workspace-agent` / `012-core-resource-types` / `012-app-notify-author` / `012-web-admin-notify-workspace` 与 `012-race` 的 passed_tests 中逐条命中 |
| `make check` | EXIT=0，七目标全绿：fmt「gofmt / gofumpt 均干净」、lint 零输出、sec 零命中 + `govulncheck` No vulnerabilities、test（-race）全包 ok、test-integration 全包 ok、cover「全部包达标」（app 75.1% / core 87.6% / web 91.0% / workspace 78.5%）、build `CGO_ENABLED=0` 通过 |
| 阶段全量 | 28/28 check passed、exit=0；`required_tests` 635/635（0 缺失）、passed_tests 3850、skipped 14（全部为 Windows 无法创建符号链接的平台跳过，`t.Skip` 理由写在用例内，且各有平台无关对照用例）；stage sha256 `6957c9ce…`（57,948 字节、18 check、protect 107）；`source_fingerprint=e7c035cb…`、`base_commit=383d89e`、`dirty=true`；报告 `.loop/runs/accept-20261022T162912.617609500/report.json` |
| 前端 | `npm test` 10 文件 / 96 例全绿（R3.5 为 94 例，+2）；`npm run typecheck` 通过；`dist` 独立核对 files 4 / entries 3 / bad 0 / extra 0，`index.html` `9a77a733…`、`index-0jOt1y1V.js` `2d943308…`、`index-BeiEd25x.css` `7076b6a5…` |
| 文档判卷 | markdownlint（`markdownlint/sync` 的 `lint`）对 `docs/Operations.md`（511 行）/ 模块卡（2,393 行）/ `tasks.md`（365 行）/ `docs/contracts/agent-md.md`（432 行）零错误，逐文件返回 `linesInFile` 证明已解析；负对照（重复 H1 + 无语言 fence）报 MD025/MD031/MD040 |
| 二进制与演示 | `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=383d89e"` → 18,897,408 字节、sha256 `b9df387f…`，内嵌 `index-0jOt1y1V.js` / `index-BeiEd25x.css` 各 3 处命中；停止 8080 上旧二进制进程（PID 41408）、替换演示目录 `basalt.exe`，端口交还用户用 `start-demo.ps1` 启动 |

**判据面**：只做**追加**——四个相关 check 扩大 `-run`、补 13 个锚点（required_tests 615 → 635），`protect` 96 → 107；无删除、无放宽、无断言改写。approval 段追加 R3.6 说明与冻结指纹（模块卡 `ff798e17…` 2,393 行；tasks `e5f9a6f6…` 365 行 / 105 任务）。

**已知限制**（同模块卡）：`referenced_by` 是文本扫描（注释 / 示例里的渠道名也会命中），页面已明示不是绑定；白名单只收紧「发送前可选渠道」，不提供默认渠道、自动发送或入站路由；未登记渠道名可保存但发送时失败。

**未验证**：真实飞书 webhook 的发送成功 / 失败与「声明白名单下工具拒绝」的真实端到端仍归 U16 / 新增 U17；占位模型告警只是页面提示，不改变「占位符运行必失败」的语义。

### 1.77 T134 装配与评测夹具适配记录（2026-10-23）

**触发**：T134 实施到「装配层接线」时跑 `go test ./...`，`evals` 红（`TestMainRun_IsolatesResults` 的 `mainRun` 返回 1）。逐字首错：`运行入口拒绝执行：resource unavailable: isolated-cli (agent resource bindings are missing, invalid or ineligible; fix resource_bindings in AGENT.md and republish the referenced resources)`；同批 WARN：`启动对账：模型连接定义未通过候选校验 … edit unsupported: 目标块首项不是列表项（-）`。两条独立根因见 §2 台账 2026-10-23 两行。

**T134 落地（装配层）**：

| 文件 | 内容 |
| --- | --- |
| `internal/app/wire.go` | 存储实例（8 个读/写面）提到迁移**之前**构造——v3 编排的中间步骤要写操作台账 / 状态文件 / 绑定投影，必须与运行期消费面是同一批实例；`db.Migrate` 换成六步 `Migrator.MigrateV3`（新库=目标版本 → 零报告、不写操作行）；凭据五件（解析器 / 守卫 / Run 检查点 / 写面 / secrets 服务）在迁移之后、工具注册之前装配；`resolver` 接入 notify 工具、Provider 目录、`AdmissionDeps`（`Credentials` / `CredentialPins` / `Checkpoint`）与 `ResourceAdminDeps.CredentialSource`；`resolver.VerifyStartup` 在投影重建之后、目录构建之前；`heavyRuntime` 增 `resolver` / `secrets`，新增包外访问器 `Runtime.Secrets()` |
| `internal/app/ports.go` | `openCLIAdmin` 与重路径共用同一迁移编排（不再直调 `db.Migrate`），版本校验语义随 `Migrator` |
| `internal/app/credential_crypto.go` | 生产侧助手 `writeMasterKeyFile` 改名 `writeNewMasterKeyFile`——与既有测试助手（`credential_crypto_test.go:53`）同名，`go vet` 报 redeclared；测试助手与其调用点未动 |
| `internal/app/wire_test.go` | 新增 `TestWire_CredentialComponentsAssembled`（同一批实例、未配置主密钥是合法形态、拒绝路径不落盘）与 `TestWire_StartupRunsMigrationOrchestration`（新库不写 `system.baseline_v3` 操作行；缺 Agent 目录在迁移步失败并归因 `迁移数据库`） |

**评测夹具适配（evals）**：v3 绑定契约要求 `AGENT.md` 的 `resource_bindings` 与定义里的引用逐一对应，而资源 UID 只有**发布**时才生成——夹具在装配前写不出它。`runCase` 改为两遍装配：第一遍让基线发布配置条目（真模型档凭证缺失仍按跳过处理），只读打开夹具库取已发布身份（模型连接取 `resource_pointers` 指针 UID，通知渠道取 `NotifyAdminStore.Get` 的 `ResourceUID`），用 `workspace.BackfillAgentBindings`（与 v3 迁移第 4 步同一写面、同一条块编辑纪律）写回定义文件，第二遍才正式装配并启用。夹具不自己拼 UID（生产 UID 随机，按名抄一个等于伪造身份）；无引用的用例不动文件。新增常量 `databaseFileName`（与装配层同值、按字面量引用）。

**块编辑缺陷（`internal/workspace/yaml_block.go`，本轮新文件）**：`lineOf` 原先按 `token.Position.Offset` 换算行号。实测 goccy v1.19.2 的偏移在键前**两行以上连续注释**时少算（两行少 1、三行少 2，纯 ASCII 与中文同病；单行注释恰好正确），键行被定位到上一行，`scanEntries` 首项判定随即失败。改为按 `Position.Line` 定位并对 CRLF 的行号虚增做定点校正（虚增量 = 目标行之前的 CRLF 条数，按「报出行号 − 该行行号 = 该行之前的 CRLF 条数」求解）；删除 `byteOffsetOfRunes`。补回归 `TestYAMLBlock_FindBlockAfterConsecutiveComments`（两行 / 三行 ASCII、三行中文、评测夹具形状、CRLF、混合行尾六种定位 + 一次替换逐字节保留）。

**验证读数（本轮当场）**：

| 项 | 读数 |
| --- | --- |
| 构建 / 静态 | `go build ./...` exit=0；`go vet ./...` exit=0；`gofumpt -l` 对本轮三文件零输出 |
| 包测试 | `go test ./internal/workspace ./internal/app ./evals -count=1` → ok（20.760s / 38.431s / 1.212s） |
| evals 复绿 | `TestMainRun_IsolatesResults` passed（`Totals.Passed=1`、`Skipped=0`）；此前同命令 exit=1 |
| 块定位 | 八种形状（含评测夹具两行中文注释、CRLF、混合行尾）`FindBlock` 起始行逐例命中键行 |

**判据面**：无删除、无放宽、无改写既有断言；新增 1 个回归用例与夹具适配。`internal/web` 的六个契约用例仍红且属预期（T138–T142 的 harness 先行），不在本轮处理。

**未验证**：完整 `make check` 与 stage 全量待 T150–T152；`-race` 全量、覆盖率、`staticcheck` 与前端构建同批待跑；真实模型档的绑定回填只有 fake 档实测（真模型档需凭证，仍按门控跳过）。

### 1.78 T135 secrets 命令面与判据面适配记录（2026-10-23）

**T135 落地（CLI 命令组）**：

| 文件 | 内容 |
| --- | --- |
| `internal/cli/ports.go` | 新增 `Secrets` 端口（`Init` / `Status` / `Verify` / `RevokeManaged`）与投影结构 `SecretsStatus` / `SecretsVerifyReport`——只含公开标识（key_id、指纹）与计数，不含任何密钥材料 |
| `internal/cli/deps.go` | `Deps.Secrets`：只有重路径装配才产出该服务，轻路径拿到 nil 即报缺件 |
| `internal/cli/secrets.go` | `secrets` 命令组：可运行父命令 + 显式未知命令分支；`init`（生成主密钥，拒绝覆盖 / 拒绝孤儿由服务面判定）、`status`（未初始化只影响文案、不影响退出码）、`verify`（失败条数 > 0 即非零退出，明细在 WARN 日志）、`revoke-managed`（**必须** `--confirm`，缺确认时零副作用零调用） |
| `internal/cli/root.go` | 注册第十条子命令 `secrets` |
| `internal/app/ports.go` | `secretsPort` 适配 `SecretsService` → `cli.Secrets`；同时把 `agentsPort.actor()` 与撤销共用的主体构造抽成 `cliWriteActor` / `cliUserName`（避免第二套 CLI 主体口径） |
| `internal/app/run.go` | `heavyCommands` 增 `secrets`——四条子命令都建立在真库与真主密钥上（`init` 统计托管依赖、`revoke-managed` 写撤销行与操作行），与 `chat` / `tool` 同口径取实例锁、走迁移；部署类命令与服务实例不并存，锁冲突即该语义 |
| `internal/app/credential_ops_test.go` | 新增四个 `TestSecrets_*`：密钥已存在拒绝且文件逐字节不变、存在托管依赖拒绝且不落盘、指纹稳定且 verify 只回计数（key_id 不匹配必计失败）、`revoke-managed` 撤销两条（密文清空 / 纪元 +1 / 两条操作行）且重复执行幂等 |
| `internal/cli/secrets_test.go` | 六个 `TestSecrets_Command*`：init 只打印公开标识、status 未初始化零退出码、verify 失败即非零、`revoke-managed` 缺确认零调用、端口缺件报「未装配」、未知子命令非零退出 |

**判据面适配（两项，均为本机实测发现，断言未删未放宽）**：

1. **平台条件子测试拆出**：按 stage 原样跑 `012-credential-managed` 的 argv，`go test` 退出 0，但验收执行器对 required_tests 的**任何子测试 SKIP** 判红——`TestCredentialCrypto_MasterKeyOutsideWorkspace/symlink_rejected`（Windows 无符号链接特权）与 `TestCredentialCrypto_KeyPermissions/mode_bits`（Windows 权限位为合成值）会把它判红。按「整例按平台跳过」的既有形态拆成 `TestCredentialCrypto_MasterKeySymlinkRejected` 与 `TestCredentialCrypto_KeyPermissionBits`（非 required，整体跳过并记入 `skipped_tests`），其余断言原样留在 required 用例里；`TestCredentialCrypto_KeyPermissions` 保持形态校验全集。原因与依据见 §2 台账。
2. **静态检查全量清零**：`staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...` 首轮 5 条（`U1000` 未用常量、`ST1005` ×2、`ST1020` 重复注释、`SA9004` 常量组类型）→ 逐条修：删未用常量（注释就地说明 `definition_missing` 归迁移侧）、两条错误文案改为小写起头、删误贴在 `CurrentETag` 上的 `activePointer` 旧注释、常量组补显式类型；全量复跑 exit=0。

**验证读数（本轮当场）**：

| 项 | 读数 |
| --- | --- |
| 构建 / 静态 | `go build ./...` exit=0；`go vet ./...` exit=0；`staticcheck … ./...` exit=0（首轮 5 条已清零） |
| 包测试 | `go test ./internal/app ./internal/cli ./evals -count=1` → ok（39.849s / 7.292s / 5.640s）；`./internal/storage` → ok（15.241s） |
| stage argv（`012-credential-managed` 原样） | exit=0；PASS 69、FAIL 0；SKIP 2 且均为整例（`TestCredentialCrypto_MasterKeySymlinkRejected` / `TestCredentialCrypto_KeyPermissionBits`，非 required） |
| required_tests 核对 | 17 个锚点中 14 个已存在并 PASS、无子测试 SKIP；`TestCredentialCrypto_NonceUniquePerKey` / `TestResources_EtagKeyOnlyChange` / `TestResources_KeyChangeDoesNotBreakOtherAgentSave` 属 **T155 / T157 尚未交付**（stage 为前向锚点），该检查的完整通过待两任务落地后复跑 |
| markdownlint（本文件 + `tasks.md`） | 便携 Node v24.22.0 + markdownlint-cli2@0.23.3（内含 markdownlint 0.41.1），配置同 `.markdownlint.json`（`MD013` 关、`MD033` 放行 `a`）：两文件 **0 告警**。该版本**无 `linesInFile` 字段**（包内 grep 零命中），解析证明改走阳性对照：同一内容副本注入行尾 3 空格 → `MD009@1419`、去末尾换行 → `MD047@1417`；不存在路径直接 `ENOENT` 抛错。即"0 告警"是解析后无违规，不是没检查 |

**判据面**：无删除断言、无跳过代替断言、无放宽阈值。本轮改动：新增 10 个 `TestSecrets_*`；两个平台条件用例拆出为独立测试（语义不变）；静态检查文案 / 注释 / 常量修正（无行为变化，`migrate_v3.go` 与 `resources.go` 的错误文案更动只影响措辞，仓内无断言匹配旧文案）。`secrets` 进入重路径属命令面语义（§1.78 表内说明），不改任何既有命令的分流。

**未验证**：`secrets` 四命令的真实二进制手工冒烟（需真实工作区 + 主密钥文件）；`status` / `verify` 在服务运行中执行会因实例锁被拒（已按设计接受，部署类命令与实例不并存）；完整 stage 全量与 `make check` 待 T150–T152；T155 / T157 的三个锚点用例尚未交付。

### 1.79 T136 secrets 段装载校验记录（2026-10-23）

**T136 落地**：

| 文件 | 内容 |
| --- | --- |
| `internal/cli/config.go` | `SecretsConfig` 文档写明装载期两条规则；`LoadConfig` 在解码后调用 `validateSecretsSection`（用原始树区分「整段缺失」与「写了段但没给路径」——解码后两者都是空串）；新增 `pathWithin`（词法包含判定，跨卷按不包含处理） |
| `internal/cli/config_test.go` | 新增 `demoEnv` / `secretsConfig` 两个夹具助手；`TestConfig_MasterKeyFileRequired` 覆盖整段省略、空小节 / 仅注释 / 空映射 / 路径空串四种「写了段缺路径」与工作区外绝对路径正例；`TestConfig_MasterKeyInsideWorkspaceRejected` 覆盖绝对路径、相对路径、路径即工作区根三形态，并以工作区外同级目录作反证 |

**口径说明**：

- 「缺失」= 写了 `secrets:` 段（含空小节与仅注释行）就必须给 `master_key_file`；整段省略合法（纯 env 实例不需要主密钥，Operations §6.1）。理由：写了段等于声明托管部署，空路径会把故障推迟到首次托管写入才暴露。
- 「越界」= 词法判定，工作区根取「配置文件同级 `.basalt/`」（与 status / init / profile 的工作区口径一致），相对路径按配置文件所在目录换算。装载层不碰文件系统——密钥文件由 `secrets init` 另行生成，装载期允许尚不存在；符号链接 / junction、权限与真实工作区根仍由 `LoadMasterKey` 每次使用前复核（T129），装载期检查**不**替代运行期判定。
- 未新增配置键、未改 `credentialKey` 规则、未改既有段解析。

**验证读数（本轮当场）**：

| 项 | 读数 |
| --- | --- |
| 格式 / 构建 / 静态 | `gofumpt -w` 两文件后零输出；`go build ./...` exit=0；`go vet ./internal/cli` exit=0；`staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./internal/cli` exit=0 |
| 定向用例 | `TestConfig_MasterKeyFileRequired`（7 个子用例）与 `TestConfig_MasterKeyInsideWorkspaceRejected`（4 个子用例）全 PASS |
| 包回归 | `go test ./internal/cli` ok（4.884s）；`go test ./internal/app` ok（33.555s——`LoadConfig` 的两个调用方全量回归） |

**判据面**：无删除断言、无放宽阈值；新增 11 个子用例。装载期拒绝只提前暴露配置错误，不改任何既有键的语义。

**未验证**：真实二进制「`basalt.yaml` 配绝对路径 → `basalt secrets init`」手工冒烟待 T150 前后统一做；Windows 跨卷路径只按 `filepath.Rel` 语义推演，本机未造第二卷实测。

### 1.80 T137 泄漏哨兵 golden 扫描记录（2026-10-23）

**T137 落地**：

| 文件 | 内容 |
| --- | --- |
| `internal/app/credential_leak_test.go`（新增） | `TestCredentialLeak_NoPlaintextInSurfaces`：哨兵 Key / 哨兵完整地址经**三条生产写路径**（`ResourceAdmin.UpsertModelConnection` 落 `basalt.yaml`、`CredentialOps.Write` 落托管 Key、`CredentialOps.Write` 落托管渠道地址）写入，再逐面扫描；`TestCredentialLeak_GoldenFixtures`：10 条真实凭据形态正则扫契约 / 内部包 / 前端源码与产物，带阳性与阴性对照 |
| 助手 | `assertNoLeak`（报出面 + 行号）、`scanDatabaseForLeaks`（`sqlite_master` 现取表名、`SELECT *` 现取列，逐值扫描并回计数）、`walkTextFiles`、`newLeakAdminFixture`、`leakCredentialPatterns` / `leakCorpusRoots`、`finishFailingOperations` |

**八个扫描面（每面都有阳性对照，"零命中"必须同时证明"扫描器真的读过语料"）**：

| # | 面 | 阳性对照 |
| --- | --- | --- |
| ① | 三条写路径都真的写下东西 | `written.VersionNo == 1`、`sealed.Changed`、`envConn.ResourceID` |
| ② | 响应体（模型连接列表 / 详情、MCP、技能、版本详情、通知渠道行） | `channelRow.CredentialID == sealed.CredentialID`、`URLHost` 只留 scheme + host、`Last4 == "cafe"` |
| ③ | manifest v2 序列化 + `runs` / `run_resource_manifests` / `run_credential_refs` / `run_notify_refs` 落库 | 清单含 `written.CredentialID`；`run_credential_refs` 计数 = 1 |
| ④ | `management_operations.detail_json` | 预写详情含 `credential_id` |
| ⑤ | `basalt.yaml` | 文件里必须是 `${VAR}` 占位，且已解析值不回流 |
| ⑥ | 错误文案（userinfo 地址被拒 / env 定义拒绝托管写入） | `userinfoErr` 命中 userinfo 分支、`missingErr` 点名资源 |
| ⑦ | 撤销面 | `RevocationEpoch == 1`、`OperationID` 非空 |
| ⑧ | 库全表全列 + 日志 + 前端 `src` / `dist` | 库扫描值 > 100；回填失败路径必记 ERROR；每个前端目录必扫到文件 |

**日志面的阳性对照怎么来的**：写路径**只在失败时**记 ERROR，全部成功时缓冲为空——直接断言"日志里没有明文"等于断言"日志里没有东西"。为此新增 `finishFailingOperations`（内嵌 `core.OperationStore`，只让 `FinishOperation` 返回错误），用它在同一夹具上真实走一次托管写入：写入成功、终态回填失败、写面记一条 ERROR，缓冲非空后才做扫描。该路径同时复现了崩溃窗口的残局（操作行停在 pending），库扫描顺带覆盖 pending 行形态。成功路径与资源写面的缓冲按现状可能为空，只扫描不再要求非空（阳性对照已由上面那条给出）。

**本轮发现并修复的产品缺陷（`internal/storage/management_store.go`，US-11 期文件）**：④ 的阳性对照首轮红，`detail_json` 实读 `{}`。根因是 `FinishOperation` / `RecoverOperation` 把空入参经 `normalizeDetailJSON` 规整成 `{}` 再交给 `COALESCE(?, detail_json)`——`COALESCE` 只见 SQL NULL 才保留旧值，于是**每条终态行都丢掉预写阶段写下的投影**（资源名 / `credential_id` / 版本号 / 模式），违反 `docs/contracts/run-and-errors.md:524`。修复：新增 `optionalDetailJSON`（空入参回 SQL NULL，非空才规整），`FinishOperation` / `RecoverOperation` 改用它；`normalizeDetailJSON` 的 `{}` 缺省只留给 `BeginOperation`（列 NOT NULL）。回归用例 `TestManagementStore_TerminalWriteKeepsPendingDetail`（终态不带 detail 保留 / 带 detail 覆盖 / 恢复不带 detail 保留）。

**遗留缺口（本任务无归属，交 T141 承接）**：app 层托管**保存路径**尚未实现——`ModelConnectionRequest` / `NotifyChannelRequest` 没有 `CredentialMode` 字段，`UpsertModelConnection` 仍拒绝非 `${VAR}` 的 Key，`NotifyAdmin.Create/Update` 只校验 `${VAR}` 占位；而模块卡 R4.9 要求保存端点承载 write-only `api_key` / `url` + `credential_mode`。本任务的托管写入走的是 `CredentialOps.Write`（服务层），所以扫描仍然覆盖真实加密路径，但 **HTTP 保存面**要到 T141（可能连带 T139）扩展 `internal/app/resource_admin.go` / `internal/app/notify_admin.go` 与 web 层之后才算闭合。

**验证读数（本轮当场）**：

| 项 | 读数 |
| --- | --- |
| 格式 / 构建 / 静态 | `gofumpt -w` 三文件后 `gofumpt -l` 零输出、exit=0；`go build ./...` OK；`go vet ./internal/app ./internal/storage` OK；`staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./internal/app ./internal/storage` exit=0 |
| 包测试 | `go test ./internal/storage ./internal/app -count=1` → ok（16.352s / 35.432s） |
| stage argv（`012-credential-leak-sentinel` 原样 `-json`） | `go test -json -count=1 -run "^(TestCredentialLeak_)" ./internal/app` → 两例全 PASS、无 SKIP、`ok 3.326s` |
| 真实 harness | `sh scripts/check-acceptance.sh --stage harness/stages/us12-agent-resource-management.json --task T137` → `passed fmt`、`passed 012-credential-leak-sentinel`，`acceptance task/us12-agent-resource-management-r1/T137: passed exit=0`，报告 `.loop/runs/accept-20261023T022233.855778400/report.json` |
| 同批旁证 | `go test ./internal/web -count=1` 仍红 6 例（`TestContract_ErrorCodes` / `TestOpenAPI_ProfileViewPropertiesMatchContract` / `TestOpenAPI_ManifestViewPropertiesMatchContract` / `TestContractAdmin_NewErrorCodes` / `TestNotifyRead_ViewShapeMatchesContract` / `TestResources_ViewShapesMatchContract`），与本轮改动无关，属 §1.77 已登记的 T138–T142 harness 先行红 |

**扫描范围口径**：`leakCorpusRoots` 只收 `docs/contracts`、`internal`、`assets/console/{src,dist,tests}`、`basalt.yaml.example`——这五处是"示例与夹具"的产品侧载体。后台全仓探针（大小写敏感的**前缀**级匹配，非本测试判据）另在 `docs/us/`、`scripts/acceptance/`、`harness/acceptance/records/`、`.loop/go-cache/` 命中 `sk-ant-` / `ghp_` / `open.feishu.cn/...` 字样；这些是**描述判据本身的文档与记录**（含本测试的形态说明），把它们纳入等于让判据文档自己的例子判红，故明确排除。严格形态（长度 + 字符集）在前述五处零命中。

**判据面**：新增 2 个测试函数与 1 个回归测试，未删断言、未放宽阈值、未加 `t.Skip`。`internal/storage/management_store.go` 是**行为修复**（终态回填不再抹掉预写投影），既有用例全绿，`resource_admin_test.go:3125` 对 `{}` 的期望仍成立（该例的预写详情本就是 `{}`）。

**未验证**：真实外部凭据泄露路径（托管凭据在真实 DeepSeek / 真实 webhook 上的密文形态与失败文案）需真实凭证，本机未跑；前端产物面只按 UTF-8 文本扫描，二进制资源（字体 / 图片）未解码；库扫描按 `SELECT *` 的驱动映射取值，SQLite BLOB 若为压缩二进制不会被文本匹配捕获（本期无此类列）；HTTP 保存面的托管写入要等 T141 补齐后复跑本测试才闭合。

### 1.81 T152 R4 统一验收记录（2026-10-23）

> 任务原文指定记 §1.77，但 §1.77–§1.80 已被 T134–T137 占用，按"只追加、不重编号"落到下一个空号 §1.81。

**七目标**：`GOPROXY=https://goproxy.cn make check`，日志 `/tmp/makecheck3.log`，`EXIT=0`。

| 目标 | 读数 |
| --- | --- |
| fmt | `gofmt -l .` / `gofumpt -l .` 零输出（先查退出码再判输出） |
| lint | `go vet` + `staticcheck`（inherit + ST1000/ST1003/ST1016/ST1020/ST1021/ST1022）零诊断；期间删未用方法 `publishedProviderNames`、把 `resourceEnv` 方法 receiver 统一为 `e` |
| sec | `gosec -quiet` 零命中——16 条首轮命中均按"变量名 / 目录权限"误报逐条 `// #nosec` + 中文理由（13×G101 常量名含 credential、2×G304 部署者配置路径、1×G302 目录 0700）；`govulncheck` 无漏洞 |
| test | `go test -race -cover ./...` 19 包全 ok（`internal/e2e` 无测试文件） |
| test-integration | `go test -tags=integration ./...` 19 包全 ok |
| cover | 16 包达标、2 包无可执行语句跳过、0 FAIL：app 73.88% / storage 72.40% / core 86.70% / memory 91.32% / tool 85.91% / workspace 75.67% / web 90.40% |
| build | `CGO_ENABLED=0` → `bin/basalt` 19,872,256 字节（`git describe` = `383d89e`） |

**阶段全量（两次尝试）**：

| 尝试 | 命令 / 日志 | 结果 |
| --- | --- | --- |
| 第 1 次 | `sh scripts/check-acceptance.sh --stage harness/stages/us12-agent-resource-management.json`，`/tmp/us12-stage-r4.log` | 42/42 check passed、退出码 0，但收尾判 `source_changed exit=2`——运行期间我在仓库内改了 `research.md`（MD032 补空行）。报告 `.loop/runs/accept-20261023T060204.152025900/report.json` 保留为失败证据（不删） |
| 第 2 次 | 同一命令，树冻结（运行期零写入），`/tmp/us12-stage-r4b.log` | **passed、exit_code=0**：42/42 check（profile 10 + 专属 32 = R3 18 + R4 14）；`required_tests` 719/719 命中、0 缺失、无空匹配；`passed_tests` 4577 条（含子测试）、`skipped_tests` 18 条（全部为 Windows 无符号链接特权的整例跳过 + `012-race` 的平台子例）；`started 06:18:27Z → finished 06:31:54Z`（13m27s） |

| 指纹 | 值 |
| --- | --- |
| 报告 | `.loop/runs/accept-20261023T061826.909255200/report.json` |
| `stage_sha256` | `e0a3d40f7599e2046d1b729de0f8a2e0ca4403ee1b5ea94b8ffd0ab4d671ae16` |
| `source_fingerprint` | `6b9b370444116f92ad509431a913631b6495d34701a77f27d6360d4dcfe4378c` |
| `acceptance_sha256` | `686fd775aed10db4a803c1b2e22a6ed74f5657ed6ec51d17afd73baf510716ce` |
| `base_commit` / `dirty` | `383d89e…` / `true`（本轮改动未提交，预期） |
| 运行内 inputs | 卡 `39a4afec…`、spec `c1fa3e1a…`、plan `5d155011…`、tasks `7b7315b7…`（T151 已勾选、T152 未勾选的快照；T152 勾选后正文未改，仅勾选位变化，按执行器口径勾选位归一化、指纹不受影响） |

**R4 十四条新 check 读数**（`required / passed / skipped`）：`012-core-identity` 9/9/0、`012-core-scope` 3/3/0、`012-workspace-bindings` 6/22/0、`012-storage-identity` 11/13/0、`012-migration-v3` 6/10/0、`012-app-binding` 3/17/0、`012-app-scope` 5/13/0、`012-app-notify-mode` 5/16/0、`012-credential-managed` 17/70/2、`012-credential-leak-sentinel` 2/2/0、`012-web-cfg-structured` 3/5/0、`012-web-credential` 4/32/0、`012-console-config` 4/6/0、`012-e2e-v3` 3/3/0。R3 十八条的 argv 与 required_tests 未改；`012-race` 追加的 3 个并发锚点全部命中（该 check 本轮 passed 3096 / skipped 12，R3.6 为 2728 / 10——增量来自本轮新增用例与 2 个平台跳过子例）。

**判据面变更清单**（R4 review 已批准，逐条列在收口报告）：

| # | 变更 | 位置 | 依据 |
| --- | --- | --- | --- |
| 1 | `TableNames` 18 → 26 张 | `internal/storage/schema_test.go` 等 | R4.15 |
| 2 | `schema_version` 2 → 3 | 迁移 `0003_credentials_scope.sql` | R4.10 |
| 3 | manifest `format_version` 1 → 2（增 `agent_uid`） | `internal/app/run.go` / 契约 | R4.15 |
| 4 | `012-race` `required_tests` 追加 3 条 | stage JSON | R4.12 |
| 5 | 新增端口（编译期接口断言） | `internal/core` / `internal/app` | R4.11 |

**Demo L–O**：模块卡 B8 与 §4.4 只写「Demo L–O 全链路；迁移 0003 → 启动 → 准入 → 发送」，§6.2 只定义 Demo A–K——**正文没有 L–O 的逐条定义**（见收口报告发现 (o)）。按 B8 的描述映射到三条 v3 e2e，本轮全部 passed：

| Demo | 用例 | 覆盖 |
| --- | --- | --- |
| L | `TestV3E2E_MigrateAdmitNotify` | v2 → v3 迁移、启动、准入、通知渠道声明与发送闸门（含策略拒绝文案只带 scheme + host） |
| M | `TestV3E2E_CredentialLifecycle` | 托管凭据写入 / 使用 / 撤销 / 同资源重录，失败回滚、旧版本不复活、在途 Run 固定旧版本 |
| N | `TestV3E2E_ScopeAndCopyMapping` | 共享 / 专属范围、复制后的 UID 与映射语义 |
| O | 上述三条同批 exit=0 | 视为 L–O 全链路通过；L–O 的逐条文字定义待人工 review ② 补认 |

**文档判卷**：markdownlint-cli2@0.23.3（便携 Node 24.22.0，`{"MD013": false}`）对 6 份改动文档（Operations / DevelopmentLoop / 卡 / agent-md / run-and-errors / 宪法）与 9 份 specs 文档判零诊断；`research.md` 的 14 处 MD032 补空行属纯格式修订（不改语义、不进 stage inputs）。

**本轮实测观察**（不属实现缺陷）：

1. `scripts/check-coverage.sh` 的 profile 逐行校验为每行起一个 `grep` 进程：本轮 profile 17,245 行，实测约 32 ms/行（≈9 分钟），是 `make check` 与 stage 的主要耗时来源；属既有实现，本轮未改判据，优化（单趟 awk 校验）留作后续候选。
2. 验收运行期间**不得**改动仓库内任何文件：源码指纹覆盖整树（除 `.git` / `.loop` / `bin` / `evals/results` / `harness/acceptance/records` 与 `run-state.md` 等），不止 protect 名单——第 1 次尝试即因改 `research.md` 被拒。
3. `go vet`/`staticcheck`/`gosec` 的修复只改本轮触及文件；`gofumpt -w` 只作用于本轮改动文件（全仓 `gofumpt -l .` 只读）。

### 1.82 R4 独立复核修复轮记录（2026-10-23）

收口前对 R4 范围做独立复核，findings 8 条（B-1 / H-1 / H-2 / M-1 / M-2 / M-3 / M-4 / L-1）逐条处置如下。处置只改本轮触及文件，未改 R3 已批准的 argv，未删除或放宽任何断言。

| ID | 发现 | 处置（位置） | 覆盖用例 |
| --- | --- | --- | --- |
| B-1 | 通知发送不写 `notify_deliveries` 派生行，发送明细只有表象没有落库 | 装配 `notifyDeliveryRecorder`（`internal/app/notify.go:43`，`internal/app/wire.go:355`）：发送成功后按工具调用写派生行；派生写失败只记 WARN，**不**把已发生的发送翻成失败（否则触发重复投递） | `TestNotifyDelivery_ExecutorWritesDerivedRow`、`TestNotifyTool_RecordsDeliveryInsideRun`、`TestNotifyTool_NoDeliveryRecordWithoutInvocationOrAfterFailure` |
| H-1 | 范围收缩只看绑定投影，投影缺失 / 过期时漏判引用者 | 共享 → 专属转换**无条件**扫描活动与归档 `AGENT.md` 整词（`internal/app/scope.go:334`，`scopeDocumentRefs` 于 `:346`），与绑定投影求并；工作区根缺失或扫描失败 fail-closed，409 带引用名单、不 force | `TestScopeConversion_BlockedByDefinitionScan`（`012-app-scope` 族全绿） |
| H-2 | 已退役（retired）凭据被准入拒绝，固定该版本的在途 Run 无法继续 | `internal/app/credential_guard.go` 区分「已撤销」与「已退役」：固定旧版本的在途 Run 继续可用，新准入拒绝；按用户凭据重录裁决把「凭据身份永不恢复」收窄为「被撤销版本永不恢复」 | `TestCredentialGuard_RetiredKeepsInFlightRunButBlocksAdmission`、`TestCredentialGuard_RejectsBlankCiphertext`（原 `_RejectsRetiredAndBlankCiphertext` 改名拆例，断言只增不减） |
| M-1 | 资源删除只删指针，凭据身份与活动版本仍留在库里 | 删除在同一事务里退役凭据（`internal/storage/credential_store.go:267` 的 `retireCredentialByOwnerTx`；资源删除 `resource_store.go:496`、通知渠道删除 `notify_admin_store.go:244`）；已撤销保持 revoked，env 模式删除是 no-op | `TestCredentialStore_ResourceDeleteRetiresCredential`（4 子例） |
| M-2 | 崩溃恢复只释放 `run_resource_refs`，另两张引用表留残留 | `internal/storage/recovery.go`：`closeAndReleaseRun` 与 `releaseTerminalRefs`（`:493`）统一释放 `run_resource_refs` / `run_credential_refs` / `run_notify_refs`，与 `FinishRun` 同口径 | `TestRecovery_ClosesRunningRunsAndReleasesRefs`（成功 / 释放失败两子例）、`TestRecovery_BackfillsTerminalRunResidualRefs` |
| M-3 | 范围切换的「受影响 Agent」只出现在响应里，操作记录查不到 | 名单在写前算一次（`internal/app/resource_state.go:271`），同时进响应（`:282`）与成功操作明细（`:337`，投影函数 `scopeActionDetail:480` 经 `credentialActionDetail` 增 `AffectedEligibleAgents`）；失败保留 pending 投影供恢复核对 | `TestResourceState_NotifyChannelUsesChannelETagAndImpactHint` |
| M-4 | `docs/Operations.md` 声称 Windows 也由进程校验主密钥权限，实际只校验 Linux 权限位 | 文档改为部署职责：Windows 用 `icacls` 收 DACL，并写明进程内不读取安全描述符（读取需新增 `x/sys/windows`，与零新增依赖冲突）；产品代码零改动 | 文档判卷（markdownlint 0 诊断）；无代码用例 |
| L-1 | `run_resource_refs.resource_uid` 回填口径比卡片 R4.10 更保守 | 接受偏差并在迁移注释写明：v2 的 `resource_pointers` 无历史，同名重建后「名称 + 创建时间」无法区分旧实例，只信活动指针；删除过的指针留 NULL（即逐行 `legacy_unrecorded` 标记）；不改契约 schema | `012-migration-v3` 既有回填用例全绿 |

**判据面变化（只增）**：`required_tests` 719 → 726（+7）；`-run` 仅在 `012-storage-identity`（+`TestRecovery_`）与 `012-app-scope`（+`TestResourceState_`）扩大；无锚点、无 argv、无断言被删。

**七目标**：`GOPROXY=https://goproxy.cn make check`，日志 `/tmp/makecheck-r4fix.log`，`EXIT=0`。fmt / lint / sec / test / test-integration / cover / build 七目标全绿；cover 16 包达标（core 86.72%、storage 72.86%、memory 91.32%、tool 86.02%），`CGO_ENABLED=0` 构建成功。

**阶段全量**：`sh scripts/check-acceptance.sh --stage harness/stages/us12-agent-resource-management.json --out .loop/runs/us12-stage-r4fix-full`，`/tmp/us12-r4fix-stage.log`，`passed`、`exit_code=0`，42/42 check（profile 10 + 专属 32）；`required_tests` 726/726 命中、0 缺失；`passed_tests` 4645、`skipped_tests` 18（Windows 符号链接平台整例跳过；`012-credential-managed` 的 2 条为非 required 的平台用例）；`started 07:52:18Z → finished 08:05:51Z`（13m33s）。

| 指纹 | 值 |
| --- | --- |
| 报告 / 记录 | `.loop/runs/us12-stage-r4fix-full/report.json` / `record.json` |
| `stage_sha256` | `055a6d4e…` |
| `source_fingerprint` | `453bffbc…` |
| `acceptance_sha256` | `686fd775…` |
| `base_commit` / `dirty` | `383d89e…` / `true`（本轮改动未提交） |
| 运行内 inputs | 卡 `39a4afec…`、spec `c1fa3e1a…`、plan `5d155011…`、tasks `b59d6cd9…`、`harness/acceptance.json` `686fd775…` |
| `run_kind` | `original` |

**新锚点执行读数**（`required / passed / skipped`）：`012-storage-identity` 14 / 35 / 0、`012-app-scope` 7 / 32 / 0、`012-app-notify-author` 36 / 178 / 1、`012-credential-managed` 18 / 69 / 2。7 个新锚点全部实际执行并通过：`TestCredentialStore_ResourceDeleteRetiresCredential` 5 条、`TestRecovery_ClosesRunningRunsAndReleasesRefs` 3 条、`TestRecovery_BackfillsTerminalRunResidualRefs` 1 条、`TestScopeConversion_BlockedByDefinitionScan` 6 条、`TestResourceState_NotifyChannelUsesChannelETagAndImpactHint` 1 条、`TestNotifyDelivery_ExecutorWritesDerivedRow` 1 条、`TestCredentialGuard_RetiredKeepsInFlightRunButBlocksAdmission` 1 条。与 06:18 轮相比 `012-credential-managed` 的 passed 70 → 69 来自 H-2 用例改名拆例（原 1 例 + 2 子例 → 改名 1 例 + 新锚点 1 例），不是用例丢失。

**历史证据**：`.loop/runs/accept-20261023T060204.152025900`（source_changed 失败轮）与 `.loop/runs/accept-20261023T061826.909255200`（R4 首次通过轮）原样保留；本轮用新目录，未覆盖。

**文档判卷**：markdownlint-cli2@0.23.3（便携 Node 24.22.0，`{"MD013": false}`，入口 `markdownlint-cli2-bin.mjs`）对 `docs/Operations.md` 判 0 诊断；先用故意非法的 `bad.md` 做负对照（报 `MD025/single-title`、EXIT=1），证明判卷真实生效（见 §2 同轮失败行）。

## 2. 失败台账

| 日期 | 问题（测试名 / 首错位置 + 症状） | 假设 | 结果 | 同题计数 |
| --- | --- | --- | --- | --- |
| 2026-10-22 | T003 stage-only：`012-core-author-agent` 报 `required test did not execute and pass: TestAgentSpec_RequiredFields`；该 check 的控制侧锚点同时覆盖 T004 / T005 | 非实现缺陷——锚点按整组 check 配置，T004 / T005 尚未实现 | 完成 T004 / T005 后复跑 T003 / T005 均 passed（`accept-20261021T211959.663430900` / `accept-20261021T212002.947343600`）；同时把 `TestManagementActions_*` 锚点对齐模块卡约束 2 的精确名 `TestManagementActions_LiteralSet` | 1 |
| 2026-10-22 | T006 stage-only：`012-cli-author` 报 `required test did not execute and pass: TestCLIProfileDelete_WritesArchiveRow`；`TestParseAuthorSection_*` 全部 passed | 非实现缺陷——该 check 的另一锚点属 T036（CLI `profile create` / `delete` 写真实操作行与归档行），T036 尚未实现 | 记录待 T036 落地后复跑；不临时删锚点（保留控制侧完整判据面） | 1 |
| 2026-10-22 | T008 stage-only：`012-workspace-yaml-provider` 报 `required test did not execute and pass: TestProviderDef_ParseAndRender`；`TestYAMLBlock_*` / `TestBlockEditor_*` 16 个测试函数全部 passed（`go test` exit=0） | 非实现缺陷——该 check 余下三个锚点属 T009（`TestProviderDef_ParseAndRender` / `TestProviderFile_AtomicWrite`）与 T044（`TestParseMCPServers_LegacyCommandSplit`），均未实现 | 记录待 T009 / T044 落地后复跑；不临时删锚点（保留控制侧完整判据面） | 1 |
| 2026-10-22 | T009 stage-only：`012-workspace-yaml-provider` 报 `required test did not execute and pass: TestParseMCPServers_LegacyCommandSplit`；`TestProviderDef_*` 5 个与 `TestProviderFile_*` 9 个测试函数（共 14 个）全部 passed（`go test` exit=0；报告 `accept-20261021T213745.009362000`） | 非实现缺陷——该锚点属 T044（MCP 旧多词 `command` 拆分与迁移提示），T044 尚未实现；本 check 的 yaml_block / provider_file 锚点本轮已全绿 | 记录待 T044 落地后复跑；不临时删锚点（保留控制侧完整判据面） | 1 |
| 2026-10-22 | T010 stage-only：`012-workspace-agent` 报 `required test did not execute and pass: TestAgentFiles_CollectionETag`；其余 32 个测试函数（`TestCreateAgent_*` / `TestDeleteAgent_*` / `TestLifecycle_SingleWriter` / `TestRewriteAgentName_*`）全部 passed（报告 `accept-20261021T214745.958932200`） | 非实现缺陷——该锚点是受管文件集合 ETag（T018 / T020），尚未实现；同 check 的其他锚点本轮已全绿 | 记录待 T018 / T020 落地后复跑；不临时删锚点 | 1 |
| 2026-10-22 | T011 stage-only：`012-workspace-skill` 报 `required test did not execute and pass: TestSkillImport_TextOnly`；`TestSkillPaths_*` 3 个与既有 `TestSkillVersion_*` / `TestSkillLibrary_*` 17 个全部 passed（报告 `accept-20261021T214851.624439300`） | 非实现缺陷——该锚点属 T050–T052（技能导入边界 / 工作副本 / 版本快照），尚未实现 | 记录待 T050 落地后复跑；不临时删锚点 | 1 |
| 2026-10-22 | T012 首轮 `go test` 红：`scanArchive` 对 NULL `restored_agent_name` 报 `converting NULL to string is unsupported`；`InsertArchive` 缺操作行时首错为 FK 约束而非 `ErrOperationNotFound` | 实现缺陷 + 用例预期偏差：前者改用 `sql.NullString`；后者确认外键先于终态写入生效，用例改判「报错 + 零落库」 | 一轮修复后 8 例全绿（`go test ./internal/storage/` exit=0） | 1 |
| 2026-10-22 | T012 stage-only：`012-storage-admin` 报 `required test did not execute and pass: TestResourceStore_MarkDeleted`；`TestAgentAdminStore_*` 8 个与既有 `TestSchemaV2_*` / `TestResourceStore_*` 24 个全部 passed（报告 `accept-20261021T215115.249837900`） | 非实现缺陷——`TestResourceStore_MarkDeleted` 属 T039、`TestNotifyAdminStore_CRUD` 属 T055，均未实现 | 记录待 T039 / T055 落地后复跑；不临时删锚点 | 1 |
| 2026-10-22 | T013 stage-only：`012-web-etag-routes` 报 `required test did not execute and pass: TestAdminViews_ErrorDataShape`；`TestIfMatch_*` 9 个测试函数全部 passed（报告 `accept-20261021T215236.291493300`） | 非实现缺陷——该锚点属 T014（写面视图与错误 data 投影），尚未实现 | 记录待 T014 落地后复跑；T014 已补齐该锚点 6 例全绿 | 1 |
| 2026-10-22 | T014 stage-only：`012-web-etag-routes` 报 `required test did not execute and pass: TestAdminRoutes_RequireAuthAndMethod`；`TestAdminViews_*` 6 个测试函数全部 passed（报告 `accept-20261021T220130.888941700`） | 非实现缺陷——该锚点属 T015（写面路由表与鉴权装配），尚未实现 | 记录待 T015 / T016 落地后复跑；不临时删锚点 | 1 |
| 2026-10-22 | T015 stage-only：`012-web-etag-routes` 报 `required test did not execute and pass: TestContractAdmin_NewErrorCodes`；`TestAdminRoutes_*` 7 个测试函数（含 4 个子例）全部 passed（报告 `accept-20261021T220544.953916700`） | 非实现缺陷——该锚点属 T016（错误码契约与路由挂载），尚未实现；T015 的写面表锚点已转绿 | 记录待 T016 落地后复跑；不临时删锚点 | 1 |
| 2026-10-22 | T017 首轮 `go test ./internal/app/` 红：wire 用例报「模型连接 probe 的发布版本 1 必须恰含一个条目（实际 0）」；`publishedProviderEntry` 按序列形态解析，而 US-11 基线存储的是剥离 `-` 指示符的映射原文 | 实现缺陷（形态兼容缺口），非用例错误 | 增 `publishedProviderEntries`：序列形态 0 条时把映射原文包成单条目列表再走同一校验；8 例全绿、全包 passed | 1 |
| 2026-10-22 | T017 stage-only：`012-app-resource-admin` 报 `required test did not execute and pass: TestResourceAdmin_PublishFailureKeepsOldPointer`；`TestProviderCatalog_*` 8 个测试函数全部 passed（报告 `accept-20261021T221951.317538900`） | 非实现缺陷——该 check 余下 7 个锚点属 T025 / T031 / T040 / T042 / T051，均未实现 | 记录待兄弟任务落地后复跑；不临时删锚点 | 1 |
| 2026-10-22 | T018 首轮 `go test ./internal/workspace/` 红两例：`TestAgentFiles_FullCopyAndTwoRenames` / `TestAgentFiles_NewFileSubdirectory` 期望返回清单含子目录文件 `docs/guide.md` | 用例预期偏差，非实现缺陷——约束 30 的受管集合只含 Agent 根目录普通文件，子目录文件进入完整副本与最终目录但**不进入**清单 / 集合 ETag | 按契约收紧期望：子目录文件断言「落盘存在、清单不含、集合 ETag 不变」；11 例全绿 | 1 |
| 2026-10-22 | T018 staticcheck（项目 checks）报 ST1005 四处：错误文案以大写 `Agent` 开头 | 实现层文案缺陷 | 改为中文开头（「目标 Agent 目录不存在…」等），未用任何抑制注释；staticcheck exit=0 | 1 |
| 2026-10-22 | T019 首轮 `go vet ./internal/app/` 报 `author.go:295: dto.MCPServers undefined` | 实现笔误——结构体字段名为 `MCP`，引用写成 `MCPServers` | 改为 `dto.MCP`；vet exit=0 | 1 |
| 2026-10-22 | T019 首轮 `go vet ./internal/app/` 报 `author_test.go:24: "errors" imported and not used` | 测试侧残留未用导入 | 删除 `errors` 导入；vet exit=0 | 1 |
| 2026-10-22 | T019 首轮 `go test` 红 9 个子例与 `TestAuthorService_DraftNotPersisted`：均报「作者草稿第 1 项缺少 path 字段」 | 测试 helper 缺陷——用 `json.Marshal(core.AuthorFile)` 编码，而该类型无 json tag（输出 `Path`/`Content` 大写键），与草稿输出契约的小写 `path`/`content` 不符 | helper 改为显式投影小写键的本地结构体；10 个测试函数全绿（`go test ./internal/app/` exit=0） | 1 |
| 2026-10-22 | T019 stage-only 首轮：`012-app-notify-author` 报缺 `TestAuthorService_DraftWritesModelAudit` | 派生工件缺陷——stage 草稿锚点名在全仓与模块卡中均不存在；模块卡约束 29 / §4.4 第 11 行点名 `TestAuthorService_OnlyLLMCallAudit` | 控制侧把 stage 草稿锚点对齐模块卡精确名（不删锚点、不改语义，同 T003 先例）；复跑改报缺 T056 锚点 | 1 |
| 2026-10-22 | T019 stage-only 复跑：`012-app-notify-author` 报缺 `TestNotifyAdmin_ReferenceGate`；`TestAuthorService_OnlyLLMCallAudit` 等 24 个测试全部 passed（报告 `accept-20261021T223314.898531200`） | 非实现缺陷——`TestNotifyAdmin_ReferenceGate` 属 T056、`TestWorkspaceBrowse_RejectsEscape` 属 T060，`go test -list` 实测均不存在 | 记录待 T056 / T060 落地后复跑；不临时删锚点 | 1 |
| 2026-10-22 | T020 首轮编译：`internal/workspace/agent_admin.go:327` 报 `illegal byte order mark`（`clearAgentSchedules` 中写入了字面量 U+FEFF；Edit 以转义形式写入被 harness 还原为字面量） | 实现笔误——BOM 以码点字面量进了源码，而 Go 扫描器禁止源文件出现 BOM 以外的 U+FEFF | 引入包级常量 `utf8BOMPrefix = "\xef\xbb\xbf"`（字节转义不会被还原成语义字符），两处改用常量；gofumpt / `go vet` 通过 | 1 |
| 2026-10-22 | T020 stage 派生面自查：`012-workspace-agent` 的 `-run` 正则未含 `TestAgentCopy_*` / `TestAgentRestore_*`，新族测试不会进入执行面（模块卡 §4.4 族归属段明写 workspace 与 app 各有一套同名族） | 派生工件缺陷（stage 草稿在 T020 前生成，新族名当时尚未落地；非 `required_tests` 缺失） | 控制侧只扩大该 check 的 `-run` 正则（`required_tests` 五锚点一字不动），复跑 T020 stage-only 绿；T068 仍按 `go test -list` 重展开 | 1 |
| 2026-10-22 | T021 staticcheck（项目 checks）报 S1016 一处：`internal/app/agent_admin.go:669` 用结构体字面量把 `core.AgentFileInfo` 转成 `core.FileEntry` | 实现层写法缺陷，非判据问题 | 两类型字段与底层类型相同，改为直接转换 `core.FileEntry(info)`；staticcheck exit=0，未用抑制注释 | 1 |
| 2026-10-22 | T021 stage-only 首轮：`012-app-agent-admin` 能匹配到 `TestAgentAdmin_CreateDefaultsDisabled` 以外的本任务用例，但锚点清单与模块卡不一致——stage 草稿写复数 `TestAgentAdmin_CreateDefaultsDisabled`，模块卡约束 27 点名单数 `TestAgentAdmin_CreateDefaultDisabled` | 派生工件缺陷（stage 由控制侧从模块卡派生，落盘时多写了一个 s；非实现缺陷） | 按 T003 / T019 先例以模块卡精确名为准：测试函数改单数 + stage `required_tests` 对齐（只改名不改断言），复跑改报缺 T033 锚点 `TestAgentArchive_TimeoutRollsBack` | 1 |
| 2026-10-22 | T022 红态探针 ②（过期 ETag 映射 500）首轮误判为绿：`go test -overlay` 通过，但探针副本实际未被变异 | 本机 PATH 上的 `python` 解析到 WindowsApps Store 占位 shim，heredoc 变异从未执行；`go test -overlay` 因此跑了原实现 | grep 核对探针文件确认仍为原文（第 262 行未变）；改用 `perl -0pi -e` 施加同一多行替换，复跑得到预期红态（expected 412 / actual 500，handlers_admin_agents_test.go:426）；探针目录用毕即删 | 1 |
| 2026-10-22 | T025 首轮 17 例全红：`预写管理操作失败: … CHECK constraint failed: auth_method IN (...)` | 用例夹具的 `core.Actor{Subject: "test"}` 缺 `auth_method`，而该列有 CHECK 约束 | 增加 `resourceActor()`（主体名不变，补 `local_trust` + 回环 source）；复跑 18/18 绿（§1.22 D1） | 1 |
| 2026-10-22 | T025 `TestModelConnection_TestTimeout504` 挂起：`panic: test timed out`，goroutine 转储显示 `httptest.(*Server).Close` 等处理函数，处理函数停在 `<-r.Context().Done()` | Windows 上客户端超时后服务端未及时感知连接断开，Close 无限等 | 处理函数加 `release` 兜底通道，由 `t.Cleanup` 在 `srv.Close` 前关闭；判据不变，复跑绿（§1.22 D3） | 1 |
| 2026-10-22 | T026 首轮编译失败：`undefined: jsonDecoder / s.adminDecodeError / editUnsupportedMessage` | 首版 handler 按草稿写助手名，未核对本包既有出口 | 按 `handlers_admin_agents.go` 的 `decodeAdminWriteBody` 重写可选体解码；把 `editUnsupportedMessage` 从 `writeAgentContentError` 抽出为共享函数（§1.23 D3）；复跑编译通过 | 1 |
| 2026-10-22 | T026 首次运行原样回归 `TestAdminResources_StaleETag412NoSnapshot` 失败：`status = 403, want 412`；把 `example.com` 加进 AllowedHosts 的尝试又在装配期 panic（`allowed_hosts 第 4 项必须写成 host:port`） | 真实认证闸门要求 Host 带端口，而 `httptest.NewRequest` 默认 Host 无端口；认证先于 If-Match | 夹具改用透传 `resourceAuth` 注入固定 actor（认证矩阵由既有路由 / 认证用例判卷），原样用例逐字未改；复跑 13/13 绿（§1.23 D1） | 1 |
| 2026-10-22 | T030 首轮 `TestReconcile_IgnoresRemoval` 红：`result.Retained=1` / `want Ignored:1`；探针实测 `extractDefinitionBlocks("# 注释\n", "providers")` 返 `顶层必须是映射，实际是 *ast.CommentGroupNode` | 实现缺陷——只有注释的文档被当成结构损坏，落进「文件无效 → 保留」而不是「条目被删 → 忽略」 | `extractDefinitionBlocks` 把 `CommentGroupNode` 与 `NullNode` 同判为空文档（注释不是内容）；复跑 12 例全绿、`./internal/app` 全包 passed（§1.27 D3） | 1 |
| 2026-10-22 | T030 stage-only：`012-app-reconcile` 报 `required test did not execute and pass: TestStartupReconcile_Idempotent`；`TestReconcile_*` 12 个测试函数与既有 `TestRecoverPendingOperations_*` 全部 passed（报告 `accept-20261022T010648.156475000`） | 非实现缺陷——该锚点属 T067A（启动序列集成）；同 check 的 `TestRecoverAdmin_ActionMatrix` / `TestRecoverTest_UnknownNotResent` 待兄弟任务 | 记录待 T067A 落地后复跑；不临时删锚点 | 1 |
| 2026-10-22 | T031 stage-only 首轮：`012-app-reconcile` 报缺 `TestStartupReconcile_Idempotent`——该名在全仓与模块卡中均不存在（卡片约束 5 / 18 / 34 点为 `TestStartupReconcile_ExternalEditPublishesWithCLIActor` / `_InvalidFileKeepsPublished` / `_RemovalIgnored`）；`012-app-agent-admin` 的 `TestStartupOrder_ReconcileBeforeRegistry` 同样是草稿名（卡片为 `TestStartupOrder_PublishedBeforeRegistry`） | 派生工件缺陷（stage 草稿在 T030 派生时按语义自拟了函数名；非实现缺陷） | 控制侧把 `required_tests` 对齐模块卡精确名（三条 `TestStartupReconcile_*` + `PublishedBeforeRegistry`），并给 `012-app-resource-admin` 补卡片约束 17 点名的两例；不删锚点、不改断言（§1.28 D7） | 1 |
| 2026-10-22 | T031 首轮运行三例红：`TestRunAdmission_PinsProviderVersion` / `TestRunAdmission_PublishGateConsistent` / `TestRunRuntime_ReleaseAfterFinish` 均报 `agent not found: runner`，而同批 `TestStartupOrder_*` 绿 | 夹具缺陷——`writeAgent(t, name)` 零工具时 `declared` 无收尾换行，`tools: []---` 被装载器判 `invalid frontmatter`，Agent 整只不注册（既有调用点都带工具，从未暴露） | 夹具 `declared` 补换行；复跑七例全绿、`./internal/app` 全包 passed（§1.28 D6） | 1 |
| 2026-10-22 | T031 二轮 `TestStartupReconcile_InvalidFileKeepsPublished` 红：日志里确有「启动对账：候选定义不可用，保留已发布版本」，断言 `resource=probe` 却未命中 | 用例笔误——夹具 logger 是 JSON handler，字段形态为 `"resource":"probe"`，而断言沿用了 `newCapturedLogger`（Text handler）的 `k=v` 写法 | 断言改为 `"resource":"probe"`；复跑绿（§1.28 实测） | 1 |
| 2026-10-22 | T032 首轮编译三处：`internal/app/providers.go` 报 `undefined: slices`；`internal/app/wire.go` 报 `undefined: providerNameUnion` / `undefined: versions`；随后报 `no new variables on left side of :=` | 实现笔误——新增的路由用了 `slices.Sort` 却没加导入；第一遍装载调用写在了 `versions := storage.NewResourceStore(db)` 之前；上提声明后又没删原处的重复声明 | 补 `slices` 导入、把 `versions` 声明上提到并集调用之前并删除原处重复行；`go build ./internal/app/` 与 `go vet ./internal/app/` 均 exit=0（§1.29） | 1 |
| 2026-10-22 | T033 首轮 `TestAgentArchive_TimeoutRollsBack` 红：`err = agent.archive 预写操作记录失败… CHECK constraint failed: auth_method IN (...)`，`want AgentBusyError` | 模块卡 §4.3.3 的原样用例传 `core.Actor{Subject: "test"}`（不带 Method），而 `management_operations.auth_method` 的 CHECK 只收四个固定字面量（与 T025 同源现象）——夹具直接连真 SQLite，预写在断言之前就失败了 | 夹具在操作写入边界包一层 `defaultingActorStore`（空 Method → `local_trust`），生产代码与断言语义零改动；四例复跑全 PASS、`./internal/app` 全包 passed（§1.30 D1） | 1 |
| 2026-10-22 | T034 首轮 `go vet ./internal/web/` 报 `method Server.decodeOptionalAdminWriteBody already declared at internal/web/handlers_admin_archived.go:107`（与 `handlers_admin_resources.go:217` 冲突） | 实现笔误——写归档 handler 时未先核对本包既有出口，重复声明了 T023 已落地的可选体解码助手 | 删除归档文件里的重复定义，复用 `handlers_admin_resources.go` 中的同一助手；`go vet` exit=0、10 例全绿（§1.31 D1） | 1 |
| 2026-10-22 | T036 首轮 `TestCLIProfileDelete_WritesArchiveRow` 红：`require.NoDirExists(archive/cli-agent-20261016-093000)` 失败，`Error: Should not exist, but does exist` | 用例预期偏差——`workspace.RestoreAgentDir` 是复制语义，归档目录保留；物理清除在卡片 §2 明确排除 | 断言改为 `DirExists` 并注释保留理由；不为了断言好看去删除归档（越过已排除范围）；复跑绿（§1.33 D4） | 1 |
| 2026-10-22 | T036 stage-only 自查：`012-cli-author` 的 `-run` 正则不匹配实际用例名 `TestProfileList_*` / `TestProfileDelete_*`，且不含 `TestCLIProfileDelete_WritesArchiveRow` 所在的 `./internal/app`（原任务 T006 首轮即已记录该锚点缺失） | 派生工件缺陷（stage 草稿生成时的正则写错前缀，非实现缺陷） | 控制侧扩大正则、加 `./internal/app`、`timeout_seconds` 120 → 180；`required_tests` 一字未动（判据面不变）；复跑 T036 stage-only 绿（§1.33 D5） | 1 |
| 2026-10-22 | T037 stage-only 首轮报 `unknown stage task`；补绑定后自查发现 `012-web-admin-agents` 只加了 `./internal/app` 参数却未把卡片 §4.4 第 24 行点名的 `TestArchivedList_IncludesCLIArchive` 放进 `required_tests`——该用例不执行 check 仍绿 | 派生工件缺陷（stage 草稿的 tasks 表漏了 T037，锚点清单也只抄了前两行；非实现缺陷） | 控制侧补 T037 绑定与第三个锚点（只收紧；既有锚点一字未动）；复跑 T037 stage-only 绿（§1.35 D5） | 1 |
| 2026-10-22 | T040 stage-only：`012-app-resource-admin` 报 `required test did not execute and pass: TestResourceVersionDetail_Projection`；本任务 15 例全部 passed（报告 `accept-20261022T024107.071785400`） | 非实现缺陷——该锚点属 T042（版本详情投影），尚未实现 | 记录待 T042 落地后复跑；不临时删锚点 | 1 |
| 2026-10-22 | T041 stage-only：`012-web-admin-resources` 报 `required test did not execute and pass: TestSkillValidate_NoSideEffects`；本任务 9 个测试函数与 `MissingPort500` 全部子例 passed（报告 `accept-20261022T024530.120088100`） | 非实现缺陷——该锚点属 T052；同 check 的 `TestMCP_*` 锚点属 T048，均未实现 | 记录待 T048 / T052 落地后复跑；不临时删锚点 | 1 |
| 2026-10-22 | T042 stage-only：`012-web-admin-resources` 报 `required test did not execute and pass: TestSkillValidate_NoSideEffects`；`012-app-resource-admin` passed（含锚点 `TestResourceVersionDetail_Projection`），web 侧本任务 4 个新测试函数与 `MissingPort500/version-detail` 全部 passed（报告 `accept-20261022T025212.330719500`） | 非实现缺陷——该锚点属 T052；同 check 的 `TestMCP_*` 锚点属 T048，均未实现 | 记录待 T048 / T052 落地后复跑 `-task T042`；不临时删锚点 | 1 |
| 2026-10-22 | T044 stage-only 首轮：`012-workspace-yaml-provider` 报 `required test did not execute and pass: TestParseMCPServers_LegacyCommandSplit`；本任务 8 个新测试函数全部 passed（报告 `accept-20261022T030556.672017200`） | 派生工件缺陷——stage 草稿写成截短名，模块卡约束 8 点名的是 `TestParseMCPServers_LegacyCommandSplitWarns`；非实现缺陷 | 按模块卡精确名修正锚点，并把约束 8 点名的另三个用例（`CandidateMatrix` / `RejectsUserinfoAndPlaintextTokenArgs` / `LegacyAmbiguousRejected`）补进 `required_tests`；复跑绿（`accept-20261022T030614.348647800`） | 1 |
| 2026-10-22 | T045 stage-only：`012-app-mcp-supervisor` 报 `required test did not execute and pass: TestMCPSupervisor_PolicyDenied`；两个工具层新锚点 passed（报告 `accept-20261022T030827.807848500`） | 非实现缺陷——`TestMCPSupervisor_PolicyDenied` / `TestMCPSupervisor_ConcurrentRestart` 属 T047、`TestCommandRunner_SingleCommandOnly` 待其实现；该 check 的 argv 原先漏了 `./internal/tool`（卡片族 `TestMCP*` 含工具层） | 控制侧把 argv 扩为 `./internal/tool ./internal/app` 并补两个工具层锚点；记录待 T046 / T047 落地后复跑 `-task T045`；不临时删锚点 | 1 |
| 2026-10-22 | T048 stage-only：`012-web-admin-resources` 报 `required test did not execute and pass: TestSkillValidate_NoSideEffects`；本任务锚点 `TestMCPViews_NoEnvValues` / `TestMCP_StaleETag412` 与 `TestAdminResources_TestMCP*` 同族、全部 `TestAdminResources_*` / `TestModelConnection_*` 逐条 passed（报告 `accept-20261022T035645.158334800`） | 非实现缺陷——该锚点属 T052（技能校验端点），尚未实现 | 记录待 T052 落地后复跑 `-task T048` / `-task T042` / `-task T041`；不临时删锚点，本任务勾选暂不翻转 | 1 |
| 2026-10-22 | T049 首轮 `go vet` 报 `wire.go:485 expected statement, found ')'` | 实现笔误——`MCPSupervisorDeps` 字面量以 `{` 开、却按函数调用的 `})` 收尾 | 改为 `}` 并补齐 `NewMCPSupervisor(mcpDeps)` 赋值；vet exit=0 | 1 |
| 2026-10-22 | T049 首轮 `go test ./internal/app` 七例 `TestWire_*` 红：`"MCP server 未连接——已跳过" … "可执行文件不存在"` | 实现缺口——supervisor 的 `NewConnector` 未从 `App.mcpConnect` 注入，测试替身被绕过、生产连接器真的去启动进程 | 在 `mcpDeps` 上按 `a.mcpConnect != nil` 条件设 `NewConnector`（nil 会让 `newConnector` 回退零值空指针）；七例转绿 | 1 |
| 2026-10-22 | T049 `TestWire_MCPConfigChangesNeedRestart` 红：`expected [beta] actual [alpha]` | 夹具缺陷——supervisor 每 server 单独 `Connect` 一次，夹具覆盖式记录只留下最后一次（指针保留的 alpha） | 夹具改为累积 `specs`，断言改为 `[beta, alpha]` 并注明 FR17 语义（新文件条目 ∪ 指针保留项）；转绿 | 1 |
| 2026-10-22 | T050 stage-only：`012-workspace-skill` 报 `required test did not execute and pass: TestNotifyScan_WordBoundary`；`TestSkill*` 全部执行并通过（报告 `accept-20261022T042640.610984800`） | 非实现缺陷——该锚点属 T054（通知类型词边界扫描），尚未实现 | 记录待 T054 落地后复跑 `-task T050`；不临时删锚点、本任务勾选暂不翻转 | 1 |
| 2026-10-22 | T051 staticcheck（项目 checks）报 S1016 一处：`internal/app/resource_admin.go:1934` 用结构体字面量把 `core.FileHash` 转成 `core.ResourceManifestFile` | 实现层写法缺陷（T042 既有代码，本包首次跑全量 staticcheck 才暴露），非判据问题 | 两类型字段集 / 顺序一致，改为直接转换 `core.ResourceManifestFile(file)`；staticcheck exit=0，未用抑制注释 | 1 |
| 2026-10-22 | T052 stage 派生面自查：`012-web-admin-resources` 的草稿锚点 `TestSkillValidate_NoSideEffects` 在全仓与模块卡中均不存在（该名自 T041 / T042 / T048 起一直被报为缺失）；卡片约束 26 点名为 `TestSkillValidate_NoWrites` / `TestSkillValidate_SameGateAsImport` | 派生工件缺陷（stage 草稿按语义自拟函数名；非实现缺陷） | 控制侧对齐模块卡精确名，并把第二个卡片用例补进 `required_tests`——只收紧、不删锚点、不改断言；复跑 `-task T052` 绿（`accept-20261022T045304.796708000`）；T068 仍按 `go test -list` 重展开 | 1 |

| 2026-10-22 | T056 stage-only：`012-app-notify-author` 报 `required test did not execute and pass: TestWorkspaceBrowse_RejectsEscape`；本任务 12 个测试函数与 `TestNotifyAdmin_ReferenceGate` / `TestAuthorService_OnlyLLMCallAudit` 全部 passed（报告 `accept-20261022T051754.208257600`） | 非实现缺陷——该锚点属 T060（工作区只读浏览），尚未实现 | 记录待 T060 落地后复跑 `-task T056` 并翻转 T019 / T056 / T060；不临时删锚点、本任务勾选暂不翻转 | 1 |
| 2026-10-22 | T057 stage-only：`012-web-admin-notify-workspace` 报 `required test did not execute and pass: TestAdminWorkspace_TreeSkipsStaging`；本任务锚点 `TestAdminNotify_CreateDuplicate400` 与全部 9 个 `TestAdminNotify_*` 已执行通过（报告 `accept-20261022T052142.967261300`） | 非实现缺陷——该锚点属 T061（工作区只读浏览），尚未实现 | 记录待 T061 落地后复跑 `-task T057` 并翻转 T057 / T061；不临时删锚点、本任务勾选暂不翻转 | 1 |
| 2026-10-22 | T058 新建用例首两轮红：`TestWire_NotifyAdminPortWiring` 先报 `expected: 2 actual: 3`（wire-hook 操作行），后报 `expected: 1 actual: 2`（wire-empty 操作行） | 用例期望计数笔误——删除同样留成功操作行（wire-hook = 创建 + 更新 + 删除 3 行，wire-empty = 创建 + 删除 2 行），实现行为正确 | 修正期望与消息；复跑绿，实现零改动 | 1 |
| 2026-10-22 | T060 全量静态门禁自查：`gosec -quiet ./internal/app`（无管道直取退出码）exit=1，6 命中——`resource_admin.go:598/1247/2149/2807` G304 ×4、`mcp.go:278/282` G602 ×2；新增的 `workspace_browse.go` 零命中 | 非本任务缺陷——命中均在既有 US-12 文件；task 级 stage 只跑 `go test`，此前未在包级跑 gosec，故未暴露 | 如实登记；由对应文件所属任务 / T069–T070 按语义修实现，**不**加 `//nolint` / `#nosec`、不顺手改无关文件；`make check` 的 `sec` 目标在修完前保持红 | 1 |
| 2026-10-22 | T061 staticcheck（项目 checks）报 U1000：`internal/web/admin_routes.go:136 func (*Server).handleAdminUnassembled is unused` | 非实现缺陷——最后一条 handler 任务（工作区浏览）落地后 30 行路由全部指向真实 handler，占位函数失去调用方；文件头已写明"不得在占位状态下宣布本 US 完成" | 删除占位函数并同步文件头说明；staticcheck exit=0，未用抑制注释 | 1 |
| 2026-10-22 | T063/T064 后前端全量 12 例红：`agent-files` 4 例（`.files-etag` 为 `—`）+ `notify-channels` 5 例 + `resource-form` 3 例；探针逐字：`TypeError: Receiver must be an instance of class ApiClient`，请求数为 0 | 夹具缺陷，非实现缺陷——`vue-test-utils` 把 mount props 深代理（`reactive({})`），类实例成为 `Proxy` 后私有字段品牌检查失败 | 三个测试夹具统一 `markRaw(new ApiClient(...))`；断言与生产代码零改动；复跑 12 例中 11 例转绿 | 1 |
| 2026-10-22 | 夹具修复后 `notify-channels` 剩 1 例红：`.col-host` 期望 2 个 host，实际收到 3 个（多出表头「目标服务」） | 用例选择器缺陷——`DataTable` 的 `columnClasses` 也作用于表头 `th`（US-11 既有行为） | 选择器收紧为 `tbody .col-host`，期望值与断言目标不变；全量 75/75 绿 | 1 |
| 2026-10-22 | T065 stage 派生面自查：`012-e2e` 的 args 用 `-tags=integration`，而 `internal/e2e` 下每个文件都是 `//go:build e2e` → 该 check 零测试执行、exit=0 却「通过」，`required_tests` 永远无法命中 | 派生工件缺陷（stage 草稿抄了集成测试标签；`current.json` 的 `015-backfill` / `015-e2e-vet` 用 `-tags=e2e` 为先例） | 控制侧改为 `-tags=e2e`；`-run` 与 `required_tests` 一字未动（判据面不变）；复跑 `-task T065` 绿（§1.62 D2） | 1 |
| 2026-10-22 | T065 首轮 e2e 真实红：`TestAdminConsoleE2E_ModelConnectionPublishRevert` 报 `创建模型连接：HTTP = 500，期望 200`（`admin_console_test.go:507`）；探针服务日志 `模型连接 aux 的定义原文无法解析: [1:3] sequence was used where mapping is expected`；`GET /api/v1/model-connections` 同病 | 实现缺陷：`parseProviderDefinition` / `parseMCPDefinition` 只解基线「剥离 `-` 的映射」形态，管理写路径按约束 4 存块列表原文 → 读视图 500 | 两函数改为映射解码失败时回退单条目序列解码（存储字节 / 哈希不变）；新增 `TestRunResources_ResourceViewsBlockDefinition`；overlay 逆改红态逐字 `expected: "https://g.example" actual: ""`（resources_test.go:683）；六例 e2e 全绿（§1.62 D3 / D4） | 1 |
| 2026-10-22 | T066 stage 派生面自查：`012-web-etag-routes` 的 `-run` 正则不含 `TestOperations_`，T066 新增的三例不会被该 check 执行——`-task T066` 会在「新例未跑」时变绿 | 派生工件缺陷（stage 草稿按「写面视图」族生成正则，Operations 读面属 US-11 既有族；非实现缺陷） | 控制侧只扩大 `-run`（追加 `TestOperations_`），`required_tests` 一字未动；报告 `passed_tests` 含三例 7 子例；复跑 `-task T066` 绿（§1.63 D2） | 1 |
| 2026-10-22 | T067A 首轮追加测试块：`cat >> internal/app/agent_admin_test.go <<'GOEOF'` 报 `unexpected EOF while looking for matching '`，零内容写入 | 工具用法缺陷——Git Bash heredoc 在本机 harness 下不可靠（同 T022 的 `python` shim 教训：先确认改写手段真的生效） | 改用 Edit 工具以文件末尾既有断言为锚追加；追加后 `database/sql` 导入即刻有使用者 | 1 |
| 2026-10-22 | T067A 追加后 `go vet ./internal/app` 报 `agent_admin_test.go:1626: undefined: archiveDirName` | 测试笔误——引用了 workspace 包的未导出字面量；app 包对同一归档区字面量的出口是 `agentArchiveDirName` | 改用 `agentArchiveDirName`（同值 `archive`）；vet exit=0，未新增第二套字面量 | 1 |
| 2026-10-22 | T069 首轮 `make check` 停在 `sec`：gosec 11 处——`workspace/provider_file.go:60/189/228` G304、`app/resource_admin.go:598/1247/2149/2807` G304、`app/mcp.go:278/282` G602、`tool/mcp.go:117/133` G104（fmt / lint 已通过；test / test-integration / cover / build 未执行） | 实现缺陷而非判据问题（与 T060 预登记同源）：G304 改用 `os.Root` 作用域读（workspace 新增 `readProviderFile`，app 复用既有 `readFileUnder`）；G602 改为构造新切片（顺带让入参不被就地改写）；G104 捕获清理错误并 WARN 留痕 | 修复后 `gosec -quiet ./...` exit=0、零命中；受影响三包 `go test -count=1` 全 passed（workspace / app / tool）；全程未用 `#nosec` / `//nolint` | 1 |
| 2026-10-22 | T069 二轮 `make check` 停在 `test-integration`：`internal/tool` 四例 `TestMCPIntegration_*` 红，均报 helper 子进程连不上，WARN `reason=初始化失败 detail=可执行文件不存在`；`fmt` / `lint` / `sec` / `test` 已通过，`cover` / `build` 未执行 | 测试装置与已批准接口脱节（非产品实现缺陷）——US-7 集成层把 `os.Args[0] + " -test.run=..."` 整串塞进 `Command`，而 US-12 约束 8 起 `Command` 是单可执行名、参数走 `Args`，执行器不再按空白切分；`-tags=integration` 在本 US 此前从未执行，故首次暴露 | 按「随已批准接口适配」改装置：`helperCommand()` 返回 `(os.Args[0], []string{"-test.run=^TestMCPHelperProcess$"})`，三处调用改填 `Command` + `Args`；原「路径含空白则跳过」条件随切分语义消失而失效（覆盖只增不减，断言逐字未改）。`go test -tags=integration -count=1 ./internal/tool` passed；该文件补入 stage `protect`（95 → 96），approval 留注 | 1 |
| 2026-10-22 | 独立复核 F-01（阻断）：生产 `MCPPool` 只有 stdio 的 `CommandTransport`，`Transport=http` 的配置 `Command` 恒为空 → HTTP MCP 在真实装配下无法连接 / 测试 / 应用；阶段里的 HTTP 用例全走假连接器 | 实现缺口——`dialTransport` 从未按传输分支，测试替身掩盖了生产路径缺失 | 新增 `dialTransport` 分支与 `dialHTTP`（streamable HTTP、零子进程、`CheckRedirect` 拒绝跟随，`internal/tool/mcp.go:321/338`）；新增 `TestMCPHTTP_*` 四例（真实 SDK `NewStreamableHTTPHandler` + httptest）并补入 `012-app-mcp-supervisor` 的 `required_tests`；最终全量 run 中该 check passed | 1 |
| 2026-10-22 | 独立复核 F-02（高）：删除 / 发布的崩溃窗口——定义文件块已改、数据库事务未提交时，重试删除与重新发布都被分歧闸门 409 阻断，约束 13 承诺的两条恢复路径实际不可用 | 实现缺口——分歧闸门只接受「与发布版本一致」，未承认「上次写入已落盘」的中间态 | 删除按 `blockGone` 幂等补齐继续（`resource_admin.go:2196`）；发布按 `fileMatchesTarget` 接受上次改写后的目标字节（`:1259`）；技能删除重试同口径。`TestPublish_RetryAfterFileRewriteCompletes` / `TestResourceDelete_RetryAfterBlockRemovedCompletes` / `TestResourceDelete_SkillRetryAfterArchiveCompletes` 均 passed 并补入锚点 | 1 |
| 2026-10-22 | 独立复核 F-03（中）：`markApplied` 写库失败后仍 `connected=true`，新版本工具已进注册池，与卡片图「失败 → connected=false」不符 | 实现缺口——应用失败只记错误码，未撤销连接状态 | 新增 `abandonApply`（`mcp_supervisor.go:254`）：失效关闭本次连接、`Connected=false`、清空工具名、保留旧 applied 与固定错误短语；`TestMCPSupervisor_ApplyFailureFailsClosed` / `_MarkAppliedSuperseded` passed 并补入锚点 | 1 |
| 2026-10-22 | 独立复核 F-04（中）：MCP 测试端点把 `${VAR}` 字面量直送子进程；启动路径解析、测试路径不解析，同一配置两条路径行为不一致 | 实现缺口——测试端点绕过了启动侧的 `resolveMCPEnv` | 测试端点先 `resolveMCPEnv` 再连接（`resource_admin.go:1756`）；`TestMCPTest_ResolvesEnvBeforeConnect` / `_MissingEnvNotConfigured` passed 并补入锚点 | 1 |
| 2026-10-22 | 独立复核 F-05（中）：形状级 `If-Match` 拒绝（缺失 / 弱 / `*` / 逗号列表 / 格式非法）返回 412 但不带 `data.current_etag`，页面无法按同一字段重试 | 实现缺口——形状校验先于 handler，未读当前版本哈希 | `requireIfMatch` 在可解析当前值时把 `current_etag` 放进 `AdminError.Data`（`admin_routes.go:165`）；`TestAdminRoutes_ShapeRejectionCarriesCurrentETag` passed 并补入锚点 | 1 |
| 2026-10-22 | 独立复核 F-06（中）：恢复在 `RestoreAgentDir` 成功、`MarkRestored` 之前崩溃 → 重启标 `unknown`，两条重试路径均 409 | 规格边界问题，非实现缺陷——FR43 / 约束 44 把该窗口定义为 `indeterminate → unknown` 且不自动补做；卡片只承诺 rename 之前的补偿窗口 | 裁决**接受**：保留 `unknown` + 「不得盲目重做」提示，代码注释写明窗口；登记 U14，交人工 review ② 确认 | 1 |
| 2026-10-22 | 独立复核 F-07（低）：首次 ETag 比较通过、`pending` 已写后，锁内重读发现外部改动 → 412 但留下 failed 操作行 | 规格边界问题——锁内重读依赖竞态状态，不属「纯请求校验」；按约束 1a，`pending` 之后的失败必须置 failed 并返回可查 `operation_id` | 裁决按约束 1a 收口：保留 failed 操作行，响应补 `data.operation_id`（`postPendingError`）；普通陈旧 ETag 仍是预写前 412 零操作行（`TestAgentAdmin_SubmitStaleETagNoOperation` 不变） | 1 |
| 2026-10-22 | 独立复核 F-08（低）：卡片点名的 `TestRecoverDelete_FilePointerMismatchUnknown` 不存在；`internal/workspace/recovery.go` 注释仍写「本 US 无 staged 写入方」 | 文档与注释过期——卡片用语义名、实际落地为表驱动子用例；注释写于 US-11 时期 | 注释改为 US-12 起为活动分支（`recovery.go:19`）；卡片点名 → 实际测试映射表写入验收报告 §3.2（`TestRecoverAdmin_ResourceActionMatrix/delete_file_removed_indeterminate` 等），全部在 stage 锚点内 | 1 |
| 2026-10-22 | 控制侧自查（修复轮）：`pending` 写入后的 Agent 创建 / 提交 / 复制 / 归档 / 恢复与通知渠道写面失败返回裸错误，web 层只发固定文案，客户端拿不到 `operation_id` | 实现缺口——约束 1a 的「后续失败必须可查」只落在部分写面 | 新增 `postPendingError`（`agent_admin.go:1371`）统一补 `operation_id`；`writeAgentAdminError` / `writeResourceAdminError` 改走 `writeAdminDataError`，`ErrorDataView` 白名单增 `operation_id`；`agent_console`（US-11）不在本 US 改动范围，登记为范围外观察（验收报告 §3.8） | 1 |
| 2026-10-22 | 控制侧自查（修复轮）：归档目标已存在未映射为 409；资源删除路径 `candidate` 被 `:=` 遮蔽，后续校验读旧值 | 实现笔误 | `archiveTargetConflictError`（`agent_admin.go:1397`）把 `workspace.ErrArchiveTargetExists` 提升为 409 CONFLICT 并保留哨兵；遮蔽改为 `var commentsLost bool` + 赋值（`resource_admin.go:2202`）；重试与回滚用例全绿 | 1 |
| 2026-10-22 | 判据面审计：12 个已交付测试未被任何 check 的 `required_tests` 点名（含 `TestMCPHTTP_*`、`TestMCPSupervisor_ApplyFailureFailsClosed`、`TestPublish_RetryAfterFileRewriteCompletes` 等） | 派生工件缺陷——stage 在 T068 生成时这些用例尚未落地，后续补测未回头补锚 | 按所属 check 补入 `required_tests`（只收紧），逐一用 `go test -list` 验证被 `-run` 正则命中；`--lint-stages` 返回 `stage lint: ok`；stage sha256 `62c637c0…` | 1 |
| 2026-10-22 | 全量 stage 重跑首次（`bcccquxqh`）：全部检查实际 passed，但最终报 `source_changed exit=2` | 运行期间编辑了 stage JSON，前后源码指纹不一致；机制按设计拒绝把「边跑边改」当通过 | 停止并发编辑，在工作区定稿后重跑：stage-only 18/18 passed、full 28/28 passed（`.loop/runs/us12-stage-final` / `us12-stage-final-full`） | 1 |
| 2026-10-22 | 判据修订（R3.3）：`TestResourceAdmin_PublishFailureKeepsOldPointer` 经 R3.2 补 `VersionID` 后仍只发布过 v1，撤回目标与被替换版本内容相同，文件改写是空操作，「失败保旧」的文件维度未被覆盖；发布契约（`openapi.yaml:831`）明确不带版本号且已发布时幂等、不插版本，模块卡原示例前置条件写错 | 判据缺陷（场景不覆盖契约语义），非实现缺陷；不降低「失败保旧」要求 | 按用户裁决改为已发布 v1、v2 且指针 / 运行时在 v2，显式撤回 v1（v2 ETag）并注入插入失败；补强断言并保留幂等用例；约束 9 / FR-024 / 发布契约不变；模块卡 R3.3、T072、stage 指纹同步；overlay 逆改恢复调用得到逐字红态 | 1 |

| 2026-10-22 | R3.4 浏览器走查第 1 轮：创建页在注册表刷新前不出现「打开 Agent 页面」，走查停在提交后 | 创建页 `refreshLifecycle` 与注册表异步；非本次新增缺陷 | 走查改为经 Agents 列表进入；页面行为保留并记入 §1.75 观察项 | 1 |
| 2026-10-22 | R3.4 走查第 2 轮：列表在 Agent 注册前读取，只显示旧 Agent | 脚本先导航列表、后等注册 | 使用运行唯一名称，并先轮询 `/api/v1/profiles` 确认注册再导航；走查通过该步 | 1 |
| 2026-10-22 | R3.4 走查第 3 轮：复制后到达页显示「未找到 Agent」，但 API 随后可见该副本（`updated_at` 14:40:06，名称时间戳 14:40:04） | 注册表刷新滞后于复制写响应，详情页只读取一次 profiles | 详情页加有界重试（初版 2 秒）并补回归用例；第 5 轮暴露重试期间仍渲染未找到，见下一行 | 1 |
| 2026-10-22 | R3.4 走查第 5 轮：同一断言仍红，失败截图显示「未找到 Agent」 | ① 重试间隙仍按未找到渲染；② 走查在 URL 变化后立即判读，早于详情就绪 | 重试期间改显「正在确认…」、窗口改为 5 秒；走查等待详情就绪再判读；第 6 轮 8/8 通过、零页面错误 | 2 |
| 2026-10-22 | 核对二进制 / 8080 的 CSS 时报「缺少 `index-BrfLMlNY.css`」 | 探针搜索串把大写 `L` 写成小写 `l`（`BrflMlNY`），文件实际存在 | 按 `integrity.json` 文件名复核：二进制命中 3 次，8080 返回 200 `text/css`、sha256 `8c500bd3…`；无产品缺陷 | 1 |
| 2026-10-22 | 演示服务页面仍加载旧版 JS | 8080 由演示目录的旧 `basalt.exe` 副本（PID 40836）提供，未随 `bin/basalt.exe` 更新 | 停止旧进程、替换二进制并重启；服务返回 JS `fb5e4491…`、index `f88b16f8…`，与 `integrity.json` 一致 | 1 |
| 2026-10-22 | 全量验收在 `make cover` 挂起（报告目录 `accept-20261022T141054`），长时间无输出 | 覆盖率目标在 Windows 上挂起，非断言失败；进程树残留 `.loop/lock`（pid 44268 已死） | 结束进程树，确认 pid 已死后移除陈旧锁；改跑 stage-only 18/18 通过；挂起根因未复现，保留观察 | 1 |
| 2026-10-22 | `test-integration` 在 `chat_test.go:636/640` 偶发红，单独复跑通过 | 时序偶发；本轮 Go 代码未改动 | 记为已知偶发；stage-only 不执行 integration，若最终再跑全量需复验 | 1 |
| 2026-10-23 | 用户报告「github新闻」创建会话与定时任务都失败：两次运行在模型调用处报错（`web-1792683909208-t8fcbp` / `scheduler-1792683992622-gz86zz`） | 疑似产品缺陷（运行链路） | 非代码缺陷——两次运行的 `provider.model` 都是模板占位符「请在此填写模型名」，DeepSeek 返回 400「The supported API model names are deepseek-flash, deepseek-v4-pro, but you passed 请在此填写模型名」；改 `deepseek-chat` 后两次运行 completed。约束 28 禁止自动回填默认模型 → 处置为页面提前告警（R3.6），不改运行语义 | 1 |
| 2026-10-23 | R3.6 首轮 `make check` 停在 `fmt`：6 个本轮改动文件因结构体字段对齐被判格式差异；首读为「exit 0」 | 新增字段后 gofumpt 对齐要求变化；且判卷命令尾部 `echo "EXIT=$?"` 取到的是 echo 自身状态，掩盖了 make 的失败码 | 6 个文件先看 diff 确认只有对齐 → `gofumpt -w` 本轮改动文件；重跑把 `EXIT=$?` 写进日志文件，读到 `EXIT=0` | 1 |
| 2026-10-23 | markdownlint 判卷两次失败：`markdownlint-cli2` 退出 0 但无输出（不能证明解析过）；程序化调 `md.lintSync` 报 `not a function` | 本机装的是 `markdownlint` v0.41.1，同步入口为 `markdownlint/sync` 且导出名是 `lint`；cli2 需正确配置才打印 | 改用 `import { lint } from 'markdownlint/sync'`，逐文件打印 `linesInFile` 与错误数（4 文件 0 错误）；负对照（重复 H1 + 无语言 fence）报 MD025/MD031/MD040，证明判卷真实生效 | 1 |
| 2026-10-23 | T134 首轮 `go test ./...` 中 `evals` 红：`TestMainRun_IsolatesResults` 的 `mainRun` 返回 1，首错 `运行入口拒绝执行：resource unavailable: isolated-cli (agent resource bindings are missing, invalid or ineligible…)` | v3 绑定契约生效后，夹具 `AGENT.md` 没有 `resource_bindings` 块；而 UID 只在发布时生成，夹具在装配前无法手写 | 判定契约行为正确、夹具需适配（产品侧「按名补绑」被 FR 禁止）：`runCase` 改为两遍装配，先发布再经 `workspace.BackfillAgentBindings` 写回已发布 UID；`TestMainRun_IsolatesResults` 复绿（`Totals.Passed=1`、`Skipped=0`） | 1 |
| 2026-10-23 | 同一轮 `evals` 另有启动对账 WARN：`模型连接定义未通过候选校验 … edit unsupported: 目标块首项不是列表项（-）` | 新块编辑器 `lineOf` 用 goccy `Position.Offset` 换算行号，在键前连续注释行后少算偏移，键行被定位到上一行 | 临时探针实测八种文档形状后确认偏移不可用（混合字节 / 字符单位，两行注释少 1、三行少 2）：改用 `Position.Line` + CRLF 定点校正，删 `byteOffsetOfRunes`，补 `TestYAMLBlock_FindBlockAfterConsecutiveComments`；对账 WARN 消失 | 1 |
| 2026-10-23 | 按 stage 原样跑 `012-credential-managed` 的 argv：`go test` exit=0，但验收执行器规则（required_tests 的任何子测试 SKIP 判红）会判该检查失败 | 两个平台条件子测试（Windows 无符号链接特权 / 权限位为合成值）位于 required 用例内部；runner 无法表达「required 用例内按平台跳过」 | 拆为独立测试 `TestCredentialCrypto_MasterKeySymlinkRejected` / `TestCredentialCrypto_KeyPermissionBits`（非 required，整例跳过并如实记入 `skipped_tests`），required 用例保留其余平台无关断言；复跑 exit=0、PASS 69、FAIL 0、SKIP 2（均为整例） | 1 |
| 2026-10-23 | T135 收尾静态检查全量红：5 条（`bindings.go` U1000 未用常量、`migrate_v3.go` / `resources.go` ST1005 大写错误文案、`resource_admin.go` ST1020 注释归属错位、`storage/run_store_test.go` SA9004 常量组缺显式类型） | 本 US R4 新增 / 改写代码时留下：未用常量提前登记、两条错误文案以「Agent」起头、`activePointer` 旧注释误贴在 `CurrentETag` 上（无空行分隔被归为同一段）、清单 v2 夹具常量组只有首个带类型 | 删未用常量并在原处说明 `definition_missing` 归迁移侧；两条文案改为小写起头并保留主语位置；删误贴注释（真正的 `activePointer` 注释在定义处）；常量组补显式类型。全量复跑 exit=0 | 1 |
| 2026-10-23 | T137 首轮 ④ 阳性对照红：`require.Contains(detailJSON, written.CredentialID)` 报 `"{}" does not contain "cred_8Z5Q…"`；探针 dump 显示基线与凭据写行的 `detail_json` 都是 `{}` | 实现缺陷——`FinishOperation` 把空入参经 `normalizeDetailJSON` 规整成 `{}` 再 `COALESCE(?, detail_json)`，预写阶段写下的投影被覆盖；契约 `run-and-errors.md:524` 要求终态保留资源名 / `credential_id` / 版本号 / 模式 | 新增 `optionalDetailJSON`（空入参回 SQL NULL、非空才规整），`FinishOperation` / `RecoverOperation` 改用它；`normalizeDetailJSON` 的 `{}` 缺省只留给 `BeginOperation`；补 `TestManagementStore_TerminalWriteKeepsPendingDetail`；`go test ./internal/storage ./internal/app` 全绿 | 1 |
| 2026-10-23 | T137 首轮 `TestCredentialLeak_NoPlaintextInSurfaces` 第 ⑧ 步红：`阳性对照：日志（凭据写面） 必须真的写过日志`，缓冲为空 | 测试设计缺陷——写路径只在失败时记 ERROR，全部成功必然空缓冲，「日志里没有明文」退化为空断言 | 新增 `finishFailingOperations`（只让 `FinishOperation` 失败）真实走一次写入把 ERROR 逼出来，阳性对照改压在该路径上；成功路径缓冲改为只扫不要求非空 | 1 |
| 2026-10-23 | R4 首轮 `make check` 停在 `sec`：gosec 16 处（13×G101、2×G304、1×G302）；同轮 `lint` 另有 U1000 / ST1016 各 1 | 判据误报为主——G101 命中的常量名含 `credential`（值是固定文案 / 错误码 / SQL 列序，不是凭据）；G302 目标是**目录**（0700 已是最小权限）；G304 路径来自部署者配置且调用方已拒绝符号链接 / 不在工作区。U1000 与 ST1016 是真实实现卫生问题 | 误报逐条加 `// #nosec Gxxx -- 中文理由`（不删除、不降级）；删未用方法 `publishedProviderNames`、统一 receiver 名；`gosec -quiet` 零输出、全量复跑 `EXIT=0`（§1.81） | 1 |
| 2026-10-23 | R4 首次全量 stage：42/42 检查实际 passed，收尾报 `source_changed exit=2`（报告 `accept-20261023T060204`） | 运行期间我在仓库内改了 `specs/012-agent-resource-management/research.md`（markdownlint MD032 补空行）；执行器的源码指纹覆盖整树，不止 `protect` 名单 | 失败报告保留不删；工作区定稿并冻结后重跑：passed exit=0、`required_tests` 719/719（`.loop/runs/accept-20261023T061826.909255200`，§1.81） | 1 |
| 2026-10-23 | 复核修复轮 M-3 首轮 `go vet ./internal/app` 报 `undefined: stateRevisionAfter` | 用例笔误——引用不存在的助手函数；实现侧写后版本以既有读回值 `channel.Revision` 为准 | 改用读回值，断言语义不变；`TestResourceState_` 族全绿 | 1 |
| 2026-10-23 | 文档判卷首轮静默通过：`markdownlint-cli2.mjs`（非 bin 入口）对 `docs/Operations.md` 返回 0 issues，对故意非法的 `bad.md` 也返回 0 | 入口点错误——该文件不是 CLI；`package.json` 的 bin 是 `markdownlint-cli2-bin.mjs`，"零诊断"实为"没检查" | 负对照改用正确入口后报 `MD025/single-title`、EXIT=1；同一入口复判 `docs/Operations.md` 得 0 诊断 | 1 |
| 2026-10-24 | R8R 收口 stage 首跑：`012-web-errors-zh` 报 `required test did not execute and pass: TestProblemMessages_ProjectsEachEntryAndNeverNil`；其余 53/54 check passed，`status=failed`、`exit=1`（报告 `.loop/runs/us12-stage-r8-closeout-full-2/report.json`） | 非实现缺陷——R8R-01 重写 `internal/web/errors_zh_test.go` 时删掉了该同名用例与两个 `TestProblemText_*` 前缀表用例；类型化断言已覆盖同一语义 | 恢复 `TestProblemMessages_ProjectsEachEntryAndNeverNil`（逐条目投影、空切片非 nil、中文文案），把两个因前缀表移除而失效的锚点替换为 `TestAgentRejectMessage_TypedKindsBecomeChinese` / `TestAgentRejectMessage_CoversEveryDefinedKind` / `TestEntryRejectMessage_IgnoresEnglishReason`（18 例表、15 类覆盖、英文不外泄）；stage approval 记录判据面变更；复跑 54/54 passed（§6） | 1 |
| 2026-10-24 | 首跑被控制侧 `TaskStop` 终止后，acceptance 进程（pid 9776）仍存活并持 `.loop/lock`；第二跑 `exit=0` 报 `exclusive harness lock unavailable`，现场目录 `us12-stage-r8-closeout-lockconflict` 保留 | `TaskStop` 只停 shell 包装，`acceptance.114` 及其 `go.exe` 子进程未随 shell 退出 | 确认 9776 / 25424 / 6284 / 39388 / 41048 属同一进程树后逐个终止；`acceptance` / `go.exe` 清零、确认 pid 已死后删除陈旧锁；stage protect 追加 4 条 R8R 触及的既有文件（`stage_sha256` 更新为 `53a29314…`），在干净目录重跑 | 1 |
| 2026-10-24 | US-12 收口后 CI（run 40071215899，`main` @ `84064ba`）`test` job 红：`test` / `test-integration` / `cover` 三目标同因失败——`TestWire_ResourceAdminPortWiring` 报 `TempDir RemoveAll cleanup: unlinkat …/versions/skills/wire-skill/<uid>/1/SKILL.md: permission denied`（Linux 专属；Windows 本地全绿，断言本身全过） | 非产品缺陷：技能已发布版本目录按约束 25 是 0o444 / 0o555，Linux 下只读目录（0o555）不允许 unlink 子项；wire 夹具未在清理阶段解除只读位（`resource_admin_test.go` 的既有 `unlockTreeOnCleanup` 只被资源写面用例调用） | `newWireFixture` 统一登记 `unlockTreeOnCleanup(t, workDir)`（只动测试清理，不改断言 / 阈值 / 判据面），并修正助手注释对两平台机制的描述；本地 `make check` EXIT=0，全量 stage 复跑 54/54（§7） | 1 |

## 3. 停止预算与计数

- 同一问题修复 3 次不收敛 → 停下走 `/debug`（不得第 4 次盲修）。
- `/debug` 内同一问题累计 3 个假设不成立 → 停下讨论设计 / 架构问题。
- 每个 US 的 `/debug` 轮次预算 ≤ 2（超出停下请裁）。
- 无进展（相邻两次尝试无新读数 / 新假设 / 新定位）→ 立即停。
- 当前计数：同题修复 0 次；`/debug` 0 轮。

## 4. 未验证清单

初始项来自模块卡 R3.1 的未验证登记，实施后按实测翻转（注日期与证据）：

| # | 项 | 状态 |
| --- | --- | --- |
| U1 | MCP `http` 传输在 `modelcontextprotocol/go-sdk v1.8.0` 的实际连接行为 | 已验证（2026-10-22：`TestMCPHTTP_ConnectListAndCall` 用真实 SDK `NewStreamableHTTPHandler` + `httptest` 起服务端，连接 → 初始化 → 列工具 → 调用全链路通过且零子进程；`TestMCPHTTP_URLShapeRejected` / `_UnknownTransportRejected` / `_RedirectNotFollowed` 同批通过并进 `required_tests`。真实外部 MCP server 进程仍在验收报告 §3.8 未验证） |
| U2 | Windows 进程树回收（`taskkill /T`）与 Unix 进程组清理 | 未验证 |
| U3 | 30 秒归档等待在真实长 Run 上的耗时 | 未验证 |
| U4 | `ResourceGate` 与定义文件锁的竞争 / 延迟 | 未验证 |
| U5 | 启动对账在既有工作区（带历史版本）上的幂等性与耗时 | 未验证 |
| U6 | 前端构建与 Vitest（本机 Node 可用性待实测；CI job 为准） | 已验证（2026-10-22：官方 Node 24.22.0，与 `.nvmrc` 同版；`npm ci` / Vitest 8 文件 75 例 / `vue-tsc --noEmit` + `vite build` + integrity 全绿，二次构建字节一致，§1.67 D1–D4；R3.4 复核：9 文件 87 例全绿、`agent-actions.test.ts` 9 条、构建与 integrity 一致，§1.75；CI Node job 仍将在 T070 复核） |
| U7 | `author.temperature` 只做了配置校验，**未随模型请求发送**：`core.ChatRequest` 无 Temperature 字段，`provider/client.go` 的请求体构造也没有透传路径；两文件在本 US 修改清单之外（T019 派生记录 §1.15 D2） | 未验证（API 缺口，待后续 US 走已批准接口变更补齐） |
| U8 | `TestAgentRestore_RejectsUnrestorableArchive` 的「归档目录含符号链接 → 清单无法复算，拒绝恢复」子断言在本机未执行：Windows 账户无 `SeCreateSymbolicLinkPrivilege`（`os.Symlink` 报 `A required privilege is not held by the client`）；按仓内先例（`TestDeleteAgent_RefusesSymlinkEntry`）先用 `t.Logf` 标注再跳过该子断言，其余子断言（缺目录 / 源不是 `archive/<id>` / 缺 AGENT.md）全部执行通过 | 未验证（平台跳过；CI 或开发者模式环境可复核；T020 §1.16 实测行） |
| U9 | 约束 21 与 US-11 基线的张力：`baseline.go:scanResource` 在 supervisor 连接之前把「已发布未 applied」的 MCP 指针补成 applied；「对账发布 v2 → 连接失败 → 重启」后 `pending_restart` 会显示 false（§1.46 D10） | 未验证（无对应用例；需改 `baseline.go`，不在本卡 20 个修改文件清单内，交收口裁决） |
| U10 | `TestSkillSnapshot_RejectsSymlink` 的「工作副本含软连接 → 冻结拒绝」子断言在本机未执行：Windows 账户无 `SeCreateSymbolicLinkPrivilege`（`os.Symlink` 报 `A required privilege is not held by the client`）；按仓内先例先用 `t.Logf` 标注再跳过整个用例 | 未验证（平台跳过；CI 或开发者模式环境可复核；T050 §1.47 实测行） |
| U11 | 卡片约束 3 / 35 点名的 app 层用例 `TestSkill_StaleETagZeroWrite` / `TestSkillWrite_WorkCopyDivergedConflict` 不在任何 stage check 的执行面（`012-app-resource-admin` 族列表无 `TestSkill`，`012-workspace-skill` 只跑 `./internal/workspace`）；两例在 `make check` 内执行并通过 | 已验证（2026-10-22：T068 把两例并入 `012-app-resource-admin` 的 `-run` 与 required_tests；`T040` 报告 passed_tests 含两例，§1.69 D1/D5） |
| U12 | `TestNotifyScan_DoesNotFollowSymlinks` 的「软链接目录跳过 + 软链接定义 fail-closed」子断言在本机未执行：Windows 账户无 `SeCreateSymbolicLinkPrivilege`（`os.Symlink` 报 `A required privilege is not held by the client`），按仓内先例 `t.Skip` 并写明理由；同一 Lstat 判定由平台无关的 `TestNotifyScan_RejectsNonRegularDefinition`（目录冒充 `AGENT.md`）在本机执行通过 | 未验证（平台跳过；CI 或开发者模式环境可复核；T054 §1.52 实测行） |
| U13 | `TestWorkspaceBrowse_NoSymlinkEscape` 的「软链接只报类型、不跟随、不借链接目录读目标」断言在本机未执行：Windows 账户无 `SeCreateSymbolicLinkPrivilege`，用例 `t.Skip` 并写明理由；目录联接（junction）探针在本机确认不列出、穿行被 400 拒绝、零内容返回（§1.58 探针行） | 未验证（平台跳过；CI Linux job 或开发者模式环境复核；T060 §1.58 实测行） |
| U14 | 恢复在 `RestoreAgentDir` 成功、`MarkRestored` 之前崩溃的窗口：重启后操作标 `unknown`，两条重试路径均 409；按 FR43 / 约束 44 不自动补做，需人工按 `unknown` 处置（复核 F-06 裁决接受，`baseline.go:1095`） | 未验证（接受残留风险：无自动化用例覆盖该窗口；交人工 review ② 确认接受） |
| U15 | Demo J 的人工视觉 / 交互复核（无头 Edge CDP 已走通列表新建 → 复制 → 归档 → 恢复并留截图，`.loop/demo-us12-r34b/`） | 已验证（2026-10-24：用户人工 review ② 明确「人工验证没有问题了」，接受 Demo J 的浏览器人工复核；自动化截图证据见 `.loop/demo-us12-r34b/`。这是用户人工判定，不是新增的自动化读数） |
| U16 | Demo K 的人工视觉 / 交互复核：会话新建 / 发送 / 归档、能力绑定、定时 CRUD、技能改版、MCP env 闸门在本机浏览器与 demo-agent 上的表现 | 已验证（2026-10-24：用户人工 review ② 明确「人工验证没有问题了」，接受 Demo K 的浏览器人工复核；Vitest 只覆盖页面操作路径，本条依据是用户人工判定） |
| U17 | R3.6 渠道声明的真实端到端：真实飞书 webhook 下「声明白名单内渠道发送成功 / 声明外渠道被拒绝」，以及页面三态在真实 `${ENV_VAR}` 引用与空地址两种渠道上的表现（本机只跑工具层与视图层用例，未真实投递 webhook） | 未验证（需用户自带 webhook 在演示实例上执行；与 U16 一并人工复核；§1.76。R6 已用确定性 mock 返回 `notify` 工具调用验证**执行链**：`notify-none-blocks` / `notify-allowlist-delivers` 两步 PASS，调用记录 `.loop/demo-us12-r6/mock.log` 02:07:19 / 02:07:22；真实外发仍未执行） |
| U18 | Windows 主密钥文件 DACL 由部署侧 `icacls` 保证：进程内不读取安全描述符（读取需新增 `x/sys/windows`，与零新增依赖冲突，复核 M-4） | 已验证（2026-10-24：`TestCredentialCrypto_WindowsDACL` 六个子例全 PASS——紧 ACL 放行、Everyone 读文件拒绝、Builtin Users 写目录拒绝、继承宽松 ACE 拒绝、用前放宽下次读取拒绝、安全描述符不可读拒绝；`internal/app/credential_crypto_windows.go` 每次 `LoadMasterKey` 重读文件与父目录 DACL，用的是 HEAD 已有的 `golang.org/x/sys v0.48.0` 直接依赖（`go.mod` / `go.sum` 本轮零改动，未新增坐标）；`docs/Operations.md` 的部署侧 `icacls` 仍是最低权限起点。§1.82 M-4 历史行不改写，原「本机构造不出负向用例」判断由本轮夹具推翻） |
| U19 | `run_resource_refs.resource_uid` 迁移回填比卡片 R4.10 更保守：删除过的指针留 NULL（复核 L-1 接受） | 已验证（口径核对，2026-10-24：同名重建反例成立——删除后同名重建生成**新** `resource_uid`、版本号从 1 重新计，`TestResourceRecreate_VersionContinues` PASS（`second.ResourceUID != first.ResourceUID`、两代版本号均为 1、旧身份历史版本仍可查），证明遗留引用无法按名字归属到新身份；迁移对删除过的指针留 NULL 与身份语义一致，不再作为门禁缺口，交 review ② 备案） |
| U20 | R8 白名单编辑器与可见页签自动刷新在真实 8080 浏览器上的走查（保存 / 412 重读 / 409 保留输入 / 15–30s 刷新 / 可见性去重） | 未验证（R8 证据只到前端单测、typecheck 与构建；未在真实浏览器点击走查。analyze A-4 的登录页说明同属此缺口。用户 2026-10-24 的人工接受是人工判断，不冒充自动化读数） |
| U21 | 真实 live MCP `http` 连接上的「收紧白名单即撤销、放宽不自动重连」端到端表现（analyze A-2） | 未验证（现有证据为 supervisor 单测与 app 服务层用例；未连真实外部 MCP server 实测） |

## 5. 收口结论

| 项 | 结论 |
| --- | --- |
| AI 自动验收 | `make check` 最终轮（复核修复后）七目标全绿 exit=0；阶段全量 28/28 check passed、exit=0，`required_tests` 615/615 命中（0 缺失）、passed_tests 合计 3673；报告 `.loop/runs/us12-stage-final-full/report.json`（第三轮报告 `accept-20261022T081108` 为修复前读数，保留备查） |
| Demo A–I | 可脚本部分 6 e2e + 38 单测 passed；页面交互、真实模型 / MCP / webhook / 30 秒长跑、软链接子断言等按四态登记为未验证（验收报告 §2） |
| 收口分析 | `/speckit-analyze` 复跑 CRITICAL 0（验收报告 §3.1；spec / plan sha256 与 tasks 勾选态在修复轮后复核未变） |
| 独立复核 | 第一轮**阻断**（F-01 阻断 / F-02 高 / F-03–F-06 中 / F-07–F-08 低）→ 6 条修复 + 2 条按契约裁决，阻断清零；另有控制侧自查 4 项同轮修复；逐条证据见验收报告 §3.2 与上方 §1.73 |
| 未验证清单 | U2–U5、U7–U10、U12–U15 保持未验证（U14 为复核 F-06 接受的残留风险；U15 为 Demo J 人工视觉复核）；U1 / U6 / U11 已验证（2026-10-22） |
| 待人工裁决 | U9（baseline applied 与 supervisor 连接顺序）、U14（接受残留风险确认）、U15（Demo J 人工视觉复核）、新增 `#nosec G204` 的逐条确认、U7 / T054 已知偏差、US-11 写面 `operation_id` 范围外观察 |
| 人工 review ② | **待人工收口 review**（未收到决定，不写成接受） |
| 四态明细 | 验收报告 §1–§3 |

**R3.3 复跑补记（2026-10-22）**：判据修订后门禁与 stage 已复跑——stage lint `ok`（exit=0）；stage-only 18/18 passed、`required_tests` 615/615、passed_tests 3673、skipped 14，报告 `.loop/runs/us12-stage-r33/report.json`；阶段全量 28/28 passed（profile 10 + 专属 18），报告 `.loop/runs/us12-stage-r33-full/report.json`；`make check` exit=0、cover 全部包达标。模块卡最终哈希（`daccf82d…`）同步进 stage approval 后，stage 指纹变化，再复跑 stage-only 18/18、全量 28/28（`record.criteria.stage_sha256=70b9b9ce…`，报告 `.loop/runs/us12-stage-r33b` / `us12-stage-r33b-full`，均 `run-kind=rerun`）。历史轮报告 `us12-stage-final` / `us12-stage-final-full` 保留不动；读数明细见验收报告 §4。

**管理台凭据表达修订（2026-10-22，用户裁决）**：用户指出“密钥不能进入普通配置与版本历史”不等于“页面永远不能输入密钥”；当前 `${VAR}` + 进程环境解析是**实现边界**，不是设计法则。US-12 保留现有边界，只改进页面表达：模型连接表单的输入从“凭据引用 / `${OPENAI_API_KEY}`”改为“密钥来源：部署环境 + 环境变量名”，页面把裸变量名归一成 `${VAR}` 后提交（粘贴完整占位也接受），编辑时展示当前引用、留空保留；明文或非法变量名在本地拒绝，不发出请求、不在错误文案回显输入。资源列表 / 详情的“凭据引用”改为“环境变量名”，详情增加“密钥来源”行与“部署者配置、修改后重启”说明；连接测试结果把“引用解析”（503 = 变量未设置或为空，测试未发出）与“连接结果”（200 + `ok`）分开显示，并明确“已解析”不等于“密钥有效”。**不改**服务端契约、`apiKeyEnv` 投影、阶段判据或 503 / 504 映射；不改模块卡与 stage 指纹。G 阶段的凭据托管 / 网页录入 / 目标地址与凭据使用权限约束 / 分布式逐节点轮换核验已按用户裁决补入 `career/Phase2IntegratedPlan.md` §13.3，不在 US-12 实施。

**修订复跑（2026-10-22）**：前端 `npm ci` → Vitest 8 文件 78 例全绿（原 75 例，新增 3 例：明文本地拒绝零请求、创建必须填环境变量名、粘贴完整占位并展示当前引用）→ `vue-tsc --noEmit` + `vite build` + `integrity.json` 一致；Go 侧 `make check` exit=0（cover 全部包达标、`internal/web` 的嵌入产物完整性 / 无游离文件用例通过）。stage-only 18/18、全量 28/28、exit=0，`required_tests` 615/615、passed_tests 3673、skipped 14，`record.criteria.stage_sha256=70b9b9ce…`、`source_fingerprint=cef878ef…`，报告 `.loop/runs/accept-20261022T123257.127923900`（stage-only）与 `.loop/runs/accept-20261022T123455.725159900`（full，`run-kind=original`）。U6 的旧读数“75 例”由本次 78 例更新，状态仍为已验证；其余未验证项不变。人工 review ② 仍为待人工收口 review。

**R3.4 复跑补记（2026-10-22）**：页面操作闭环落地后，前端 `npm test` 9 文件 87 例全绿（`agent-actions.test.ts` 9 条），`vue-tsc --noEmit` + `vite build` + `dist/integrity.json` 对账一致，`CGO_ENABLED=0` 二进制重建并确认嵌入新产物；无头 Edge CDP 走查 8/8（列表新建 → 复制 → 归档 → 恢复、零页面错误，证据 `.loop/demo-us12-r34b/`）。受影响 stage 复跑：stage-only 18/18 passed、exit=0，`required_tests` 615/615、passed_tests 3673、skipped 14，`record.criteria.stage_sha256=9529c975…`、`source_fingerprint=6d0a4119…`，报告 `.loop/runs/accept-20261022T144544.554587100/report.json`。运行时冻结输入为模块卡 `ebfc9772…`、tasks `40f4ff23…`（T079 未勾选）；复跑后仅翻转 T079 勾选位，正文未改，Go 代码零改动，未再重跑全量。U6 读数更新为 87 例；新增 U15（Demo J 人工视觉复核）保持未验证。人工 review ② 仍为待人工收口 review。

**R3.5 补记（2026-10-22，用户实测六项页面缺口）**：前端 `npm test` 10 文件 94 例全绿（新增 `agent-operations.test.ts` 7 条：会话新建 / 发送 / 归档、归档后换用户标识重开、能力绑定、定时 CRUD、技能改版、MCP env 闸门）；`npm run typecheck` 通过；`npm run build` 产物 `index.html` / `index-BeiEd25x.css` / `index-DBQbbGkX.js`，独立核对 `files 4 / entries 3 / bad 0 / extra 0`；`CGO_ENABLED=0` 二进制重建 18,882,560 字节；`go test ./internal/web -run 'Console|Integrity'` 通过。本机 demo 实例实测：`GET /agents/demo-agent/memory` 返回 `scope=agent`；新会话 `web-aea8a708479f7c16` 2 条消息、运行 `completed`、`final_text` 为可读三行简报；`/console/` 返回 `index-DBQbbGkX.js`。演示 mock 已区分作者草稿与普通对话（仅 demo 目录，不属仓库）。旧 8080 / 8099 进程已停止，端口交还用户用 `start-demo.ps1` 启动。**本轮未复跑 `make check` 与 stage**：Go 生产代码零改动，仅前端源码 / dist 与仓库外 demo 资产变化；`internal/web` 嵌入完整性用例已通过。stage 复跑在收口前仍需执行一次并记读数。

**R3.6 补记（2026-10-23，用户人工验收三问）**：`make check` 复跑 EXIT=0（七目标全绿，cover 全部包达标）；阶段全量 28/28 passed、exit=0，`required_tests` 635/635 命中、passed_tests 3850、skipped 14（Windows 符号链接平台跳过），`record.criteria.stage_sha256=6957c9ce…`、`source_fingerprint=e7c035cb…`，报告 `.loop/runs/accept-20261022T162912.617609500/report.json`；前端 `npm test` 10 文件 96 例、`npm run typecheck` 通过、`dist` 独立核对 files 4 / entries 3 / bad 0 / extra 0；改动文档 markdownlint 零错误（负对照有效）。`dist` 与二进制内嵌产物保持一致（`index-0jOt1y1V.js` / `index-BeiEd25x.css` 各命中 3 处），演示目录 `basalt.exe` 已替换为内嵌新产物的 `CGO_ENABLED=0` 构建（sha256 `b9df387f…`）。未验证清单新增 U17（真实 webhook 端到端），U2–U5、U7–U10、U12–U17 维持未验证，U1 / U6 / U11 已验证。人工 review ② 仍为待人工收口 review（未收到决定，不写成接受）。

**R4 修订基线（2026-10-23，用户批准范围修订，实施前登记）**：用户裁决原文「按上述 Q1–Q5 和三处修正推进，继续现有 US-12 范围修订流程；完成正式修订与颗粒度评审后实施，最终统一验收。旧证据保留，新范围重新形成完整证据，人工 review ② 继续待定。」本轮只做正式修订与派生工件，**未开始编码**，R3 的 105 个任务勾选与历史证据（`.loop/runs/us12-stage-*`、验收报告 §1–§4）一律不动。

修订内容与落点：

| 工件 | 本轮变更 | 指纹 |
| --- | --- | --- |
| 宪法 | v2.1.0：架构约束「凭证纪律」改 env / managed 两模式，新增 15 条凭据与绑定约束 + 2 条架构约束（资源使用范围；资源身份与绑定权威）；原则 I–X 不增删 | 走 `/speckit-constitution`，用户 2026-10-23 批准方向 |
| 模块卡 | R4.1–R4.17（范围 / 页面流程 / 字段与写语义 / 范围模型 / 身份与墓碑 / 绑定权威 / 通知三态 / 凭据 / API 与错误码 / 迁移 0003 / 交付物与 B1–B8 / 验收 / 八项影响清单 / 旧新语义映射 / 改造点 / 固定字面量 / 验证状态） | sha256 `23eedf3d…`，3018 行（R4 起于 2397 行） |
| spec.md | Clarifications 2026-10-23（Q1–Q5 + 凭据写入端点澄清）、FR-062–FR-088、Key Entities 增量、SC-016–SC-025（SC-017 / SC-018 显式取代 SC-005 / SC-013 对应句）、Out of Scope、Assumptions | 随 spec 正文；未改 R3 条目 |
| plan.md | 「R4 范围修订的技术计划」：技术上下文增量、B1–B8 阶段、七条关键实现决策、验证计划增量、风险与未测量项 | 追加于 Phase 0/1 产物之后 |
| tasks.md | Phase 16：T103–T152（50 个任务），按 B1–B8 分组；测试与实现同批 | sha256 `0678c278…`，445 行，155 任务（105 已勾选 + 50 未勾选） |
| stage JSON | approval 追加 R4 段落；新增 14 条 check；`012-race` 追加 3 个并发锚点；protect 追加 17 个已存在的本轮触及文件；既有 18 条 argv / required_tests 不改 | 32 checks、103 tasks、124 protect 路径 |

派生口径（写进 stage approval，不另行解释）：R4 新文件尚未创建，**不**进 protect（`loadStage` 要求路径存在），由 T151 在实施后补齐；`012-console-config` 只做 Go 侧产物与源码校验，前端 `npm test` / `typecheck` / `build` 由 CI console job 与统一验收单独执行（约束 42：Go 门禁不调用 Node / npm，本机无 Node 时 `make check` 照常判卷）。

待办（按序）：独立 AI 复核 findings 处置 → `/speckit-analyze`（CRITICAL 必须清零）→ B1–B8 实施 → 统一验收 → 收口三件套。**人工 review ② 仍为待人工收口 review**；R4 不复用 R3 的验收结论。

**R4 实施与统一验收（2026-10-23）**：T103–T157 全部完成，tasks.md 定稿 sha256 `b59d6cd96aa67f86b7a1a7095a7e91deb18a52a703f7264a2690e1f88ba955f0`，455 行、160/160 已勾选（T151 / T152 在验收后勾选；勾选位按执行器口径归一化，不影响源码指纹）。上表"R4 修订基线"里的卡 `23eedf3d…` / tasks `0678c278…` 是实施前快照，定稿为卡 `39a4afec…`、spec `c1fa3e1a…`、plan `5d155011…`；历史行不改写。

- **七目标**：`GOPROXY=https://goproxy.cn make check` → `EXIT=0`；cover 16 包达标、0 FAIL（app 73.88% / storage 72.40% / core 86.70% / memory 91.32% / tool 85.91%）；`CGO_ENABLED=0` 构建 19,872,256 字节。
- **阶段全量**：`passed`、`exit_code=0`，42/42 check（profile 10 + 专属 32），`required_tests` **719/719 命中、0 缺失**，`passed_tests` 4577 条、`skipped_tests` 18 条（Windows 符号链接平台的整例跳过 + `012-race` 平台子例）；报告 `.loop/runs/accept-20261023T061826.909255200/report.json`，`stage_sha256=e0a3d40f…`、`source_fingerprint=6b9b3704…`。首次尝试 42/42 也全 passed，但因运行期改 `research.md` 被判 `source_changed`（`accept-20261023T060204` 保留不删）。
- **判据面变更**（清单见 §1.81 与收口报告）：`TableNames` 18→26、`schema_version` 2→3、manifest `format_version` 1→2、`012-race` required +3、新增端口编译期断言。R3 十八条既有的 argv / required_tests 未改。
- **Demo L–O**：模块卡正文只有"迁移 0003 → 启动 → 准入 → 发送"一句，没有逐条定义（§6.2 只到 Demo K）；按该描述由 `TestV3E2E_MigrateAdmitNotify` / `_CredentialLifecycle` / `_ScopeAndCopyMapping` 三条承载，全部 passed，逐条映射见 §1.81，L–O 的文字定义待人工 review ② 补认。
- **文档判卷**：markdownlint 0 诊断（6 份改动文档 + 9 份 specs；`research.md` 只补 14 处 MD032 空行）。
- **已知边界（交人工 review ②）**：① Demo L–O 无正文定义；② 运行时发布的新 provider **名**对 `AgentLoader` 不可见，需重启后才可用于 Agent 绑定（provider-name freeze）；③ `/info` 的 `configured` 仍按 `base_url && api_key` 判，`credential_mode: managed` 的已托管连接会显示"未配置 / 凭证未上送"（页面表达与实际可用性不一致）；④ `scripts/check-coverage.sh` 逐行起进程，profile 1.7 万行约 9 分钟（既有实现，本轮未改）。
- **未验证清单**：U2–U5、U7–U10、U12–U17 维持原状态；R4 未新增需翻转项。**人工 review ② 仍为待人工收口 review**，本轮不写成接受。

**R4 独立复核修复轮（2026-10-23）**：8 条 findings 全部处置并留证（B-1 派生发送明细接线、H-1 范围收缩扫描定义文件、H-2 retired 凭据在途可用、M-1 删除同事务退役凭据、M-2 恢复释放三张引用表、M-3 影响名单落成功操作记录、M-4 Windows DACL 降为部署职责、L-1 接受保守回填；逐条见 §1.82）。`make check` 七目标全绿 `EXIT=0`；阶段全量 `passed` / `exit_code=0`，42/42 check、`required_tests` 726/726、`passed_tests` 4645、`skipped_tests` 18，报告 `.loop/runs/us12-stage-r4fix-full/record.json`（`stage_sha256=055a6d4e…`、`source_fingerprint=453bffbc…`、`run_kind=original`）。`required_tests` 只增 7 条、`-run` 只扩大两个 check，无锚点 / argv / 断言被删；R4 首次通过轮与失败轮报告原样保留。未验证清单新增 U18（Windows DACL 部署职责）与 U19（保守回填偏差）。**人工 review ② 仍为待人工收口 review**，本轮不写成接受。

**R4 review ② 前置走查（2026-10-23，用户六项页面缺口）**：按用户指定的顺序完成「补 Windows 权限校验 → 同名重建反例核对 → 受影响证据与门禁复跑 → 页面交互与真实通知 Demo → 提交 review ②」中的前四项；真实通知发送未执行，见下表。

| 项 | 读数 / 证据 |
| --- | --- |
| provider 名运行期可见性缺陷（known boundary ② 关闭） | 复现：Agent 建好后运行期发布模型连接，再经 `/files` 绑定 → 400 `provider not registered`，需重启才可用；与 `docs/contracts/run-and-errors.md` §9.6「Provider / 技能发布后对新准入 Run 立即生效」不符（US-11 约束 29 只管 `basalt.yaml` 的 Provider 段，不覆盖管理资源）。修复：`AgentLoaderOptions.ProvidersFunc` 每次校验读当前运行时目录（`catalogProviderNames(catalogStore.Load())`），目录在 `ResourceGate` 写锁内原子替换、失败保旧。回归：`TestAgentLoader_DynamicProviderSet`（运行期名可绑定 / 撤回名拒绝 / 动态源覆盖构建快照）+ `TestV3E2E_RuntimePublishThenBind`（建 Agent → 运行期发布 → 绑定 → 启用 → HTTP 运行 `completed`），修复前该 e2e 断言失败、修复后通过 |
| e2e Windows 主密钥夹具 | `t.TempDir()` 继承宽松 ACE（未知账户 SID、AppContainer 包 SID），`secrets init` 的 DACL 校验（正确）拒绝；新增 `internal/e2e/masterkey_acl_windows_test.go`（`icacls /inheritance:r /grant:r` 当前用户 + SYSTEM + Administrators）与 Unix 空实现，`TestV3E2E_CredentialLifecycle` 与新增用例在本机首次可跑；产品侧 DACL 校验未改，不属判据放松 |
| 同名重建口径核对（用户指出「版本号继续递增」已过时） | 核对结论：**我的说明错误，实现无回归**。R4.5、契约 §9.6（`run-and-errors.md:566`）与实现 / 断言一致为「同名重建 = 新 UID、版本号从 1 重新计」；`TestResourceRecreate_VersionContinues` / `_SkillVersionContinues` 断言即如此，函数名沿用 stage 固定名未改；`resource_admin_test.go` 的 D6 / K9 头注释已同步为 R4 语义 |
| 真实浏览器走查（无头 Edge CDP，T164 页面部分） | 9/9 步骤 PASS、0 控制台错误；证据 `shots/`（01–10 截图、`result.json`、`config-readback-select.json`、`config-readback-inline.json`、`connection-inline.json`、`invoke-result.json`）。读数：资源页新建并发布托管连接 `demo-managed`（页面无明文、尾号 `-key`）；Agent 页 `#config-provider` 选到该连接，保存后服务端读回 `provider.name=demo-managed`、绑定 `uid=rs_RJP4TF218HVMQY25KC7X2F8BZH` 与引用 `resource_uid` 一致；Agent 页就地新建 `demo-inline`（默认 managed / exclusive / owner=demo-agent，`uid=rs_RJSYJBCPF649Y3GDC0B2AMEVFR`、`credential_ref=cred_J9NMRPM36QXYVNZGH8VRH763ZH`、`credential_state=active`、`last4=-key`、`revision=3`）；停用 → 启用往返成功；HTTP 运行 `run_id=web-1792759916614-l92zgp`、`status=completed`、`final_text` 为 mock 固定回复。注：两把演示 Key 的 `last4` 恰好同为 `-key`，尾号不足以区分，仅证明非明文回显 |
| 真实通知发送 Demo（T164 剩余项） | **未执行**。`feishu-ops` 为 env 模式（`${FEISHU_WEBHOOK_URL}`，`address_status=env`），demo 启动脚本交互式询问该变量；本地 mock 模型只回文本、不会调用 `notify`；出站策略拒绝本地接收方。需用户提供真实 webhook 并同意外发后另跑 |
| `make check`（最终状态） | `GOPROXY=https://goproxy.cn make check` → `EXIT=0`；七目标 `fmt / lint / sec / test / test-integration / cover / build` 全绿，`check-coverage: 全部包达标`（日志 `make-check-2.log`；首次运行 `make-check.log` 同 `EXIT=0`，两轮均在最终代码状态附近，差异仅测试注释） |
| stage 全量统一验收 | `sh scripts/check-acceptance.sh --stage harness/stages/us12-agent-resource-management.json --out .loop/runs/us12-stage-r4-review2-full` → `passed`、`exit_code=0`，**42/42 check**（profile 10 + 专属 32），含 `012-e2e-v3` 的 `TestV3E2E_RuntimePublishThenBind`；报告 / 记录 `report.json` / `record.json`（`verdict=passed`、`run_kind=original`、`stage_sha256=055a6d4e…`、`source_fingerprint=cedafb99…`、`base_commit=383d89e`、`dirty=true` 属预期——本轮改动未提交）；`started_at=2026-10-23T13:17:38Z`、报告落盘 `13:34:23Z`（约 16m45s），未判 `source_changed` |
| 文档判卷 | markdownlint-cli2 v0.23.3（markdownlint v0.41.1，便携 Node v24.22.0）判 `docs/us/us12-agent-resource-management.md`、`specs/012-agent-resource-management/tasks.md`、`specs/012-agent-resource-management/run-state.md` → `Linting: 3 files`、`0 issues`、exit=0；负对照（双 H1）报 MD025、exit=1，证明判卷真实执行。便携 Node 与 lint 包只用于本次判卷，随后删除 |

**R5 补记（2026-10-23/24，管理台体验统一与页面闭环）**：R5.2 六组既有范围补齐与 T166–T177 已落地，统一验收与文档判卷完成；新增语义（`display_name`、问答式补全、定时一句话、记忆自动抽取）只登记提案，未实施。

- **两个用户报告**：① 通知未外发不是发送链路缺陷——`github新闻` 与 `中国股市近3日涨得最快得5支股票` 的 AGENT.md 均声明 `notify_channels:["feishu-ops"]`，但 `tools` 只有 `save_memory,recall_memory`，运行中没有任何 `notify` 调用；本轮在列表卡片、配置页通知区、定时页签三处加「已声明渠道但未绑定 notify」提示，不自动补工具；真实 webhook 外发仍未验证（U17），T164 保持未勾选。② 历史运行详情 500 是缺陷——旧 v1 运行清单被重新解析进 v2 结构体后重算哈希导致不匹配；改为 `verifyManifestBytes` 对**已存储字节**校验后，`scheduler-1792693801458-cyt8m5` 返回 200，浏览器 `legacy-run-detail` 通过。
- **前端门禁**：`npm test` **18 文件 / 184 条全绿**；`npm run typecheck` 通过；`npm run build` + `emit-integrity.mjs` 生成 `index-Fuk748Ra.js`（260,879 B）/ `index-CDmxpTG4.css`（33,559 B）/ `index.html`，sha256 分别 `0237829a…` / `f0363b32…` / `a35d37dc…`。
- **浏览器走查**：无头 Edge CDP（`.loop/demo-us12-r5/run.mjs`）**11/11 PASS**、0 控制台错误，截图 `shots/01`–`12`；一句话创建 → 预览 → 提交 → 启用 → 试运行闭环，创建 `agent-1ybks1a`，运行 `web-1792769711513-yqlz27` 为 `completed`；归档与恢复往返成功。
- **`make check`**：`EXIT=0`，七目标全绿；cover 全部包达标（core 86.79% / memory 91.32% / storage 72.98% / tool 86.02% ≥70；app 74.04% / web 90.44% / workspace 75.68% 等 ≥50），日志 `.loop/r5-make-check.log`。
- **stage 全量**：`passed`、`exit_code=0`，**42/42 check**，`required_tests` **726/726 命中、0 缺失**，`passed_tests` 4679、`skipped_tests` 18；`record.criteria.stage_sha256=c2406148…`、`source_fingerprint=1717f87f…`、`acceptance_sha256=686fd775…`；运行期输入为模块卡 `c1527b1d…`、spec `c1fa3e1a…`、plan `5d155011…`、tasks `a0fc51e1…`；`started_at=2026-10-23T16:02:18Z`、`finished_at=16:27:13Z`；报告 / 记录 `.loop/runs/us12-stage-r5-full/`。
- **收尾更正（stage 之后，文档与注释级）**：模块卡 R5.2 第 1 行与 T168 原写「列表卡片与详情页头各有一条配置入口」，实际详情页头只有复制 / 启停 / 更多操作，配置入口是「配置」页签；已改为「列表卡片有配置链接；详情页以『配置』页签为唯一入口」，`agents.ts` 同名注释同步更正。更正后卡 `701747d0…`（3132 行）、tasks `2f6a023e…`（487 行；T178 已勾选，唯一未勾选仍为 T164；B11 标题由误写的 T166–T180 改为 T166–T179）。stage 证据对应更正前快照（卡 `c1527b1d…` / tasks `a0fc51e1…`），更正只动文案与一行注释：前端 184 条、typecheck 与 build 复跑全绿，`dist` 三文件字节与哈希不变；stage 后 Go 代码零改动。
- **构建产物与运行实例**：`make check` / stage 构建 `bin/basalt` 19,960,320 B（sha256 `cdcb1e70…`），嵌入 `index-Fuk748Ra` / `index-CDmxpTG4`；用户 8080 进程（PID 37864，22:41:41 启动）仍服务旧包 `index-BNgiywrG` / `index-BeiEd25x`，**未重启**，需用户自行重启后看到新管理台。
- **文档判卷**：markdownlint-cli2 v0.23.3（markdownlint v0.41.1，便携 Node v24.22.0）判模块卡、tasks、run-state → 首跑报上一轮证据表末行缺尾管道（MD055），补尾管道 `|` 后复跑 `Linting: 3 files`、`0 issues`、exit=0（仅格式，不改内容）；负对照（双 H1）报 MD025、exit=1 已在前一轮证明判卷真实执行。
- **遗留**：R5.3 三项新增语义与记忆自动抽取待 review ①；矩阵第 2 / 3 / 4 / 7 行仍有浏览器缺口、第 6 行未开始；U17 / U18 / U19 状态不变；US-12 人工 review ② 仍为待人工收口 review（未收到决定）；未 commit / push / tag / 部署。 |

**R6 补记（2026-10-24，新增范围实施与统一验收）**：review ① 批准的三项新增语义（`display_name`、作者问答补全、定时一句话）已实施；统一验收在**冻结输入**上只跑一次，运行前后输入零漂移。

- **判据与范围**：stage 以新增断言并入既有 check（`required_tests` 726 → **756**，`protect` 245 → **247**，新增 `internal/adapters/cron/cron.go` / `cron_test.go`），既有 anchor / argv / 断言零删除；`--lint-stages` → `stage lint: ok`、exit=0。
- **`make check`（第五轮）**：`EXIT=0`，七目标 `fmt / lint / sec / test / test-integration / cover / build` 全绿，`check-coverage: 全部包达标`；日志 `.loop/demo-us12-r6/make-check.log`。前四轮各暴露一个真实问题并修复：① `gofumpt` 三文件字段对齐；② staticcheck S1016（`internal/app/author.go` 改 `core.Schedule(candidate)` 显式转换）；③ `TestAuthorService_InvalidDraftRejected` 旧断言（双形态解析错误文案改为「文件数组或含 questions」，负向语义未变）；④ `TestChat_GivesEveryRunItsOwnDeadline` 高负载下超时一次——单跑 `ok 1.757s`、第五轮全量通过，`internal/cli/chat*.go` 本 US 零改动，判为时序抖动非代码缺陷。
- **阶段全量（一次）**：`.loop/runs/us12-stage-r6-full/` → `verdict=passed`、`exit_code=0`，**42/42 check**（profile 10 + 专属 32），`passed_tests` **4884**、`skipped_tests` **18**（Windows 符号链接平台跳过），`required_tests` **756/756**；`stage_sha256=2bd1b26c…`、`source_fingerprint=c4739fd4…`、`acceptance_sha256=686fd775…`；输入卡 `9312456…`（3220 行）、spec `bd3af2ca…`、plan `bfbd77af…`、tasks `1ce871f…`（508 行展示；勾选位由 fingerprint 归一化）；`started_at=2026-10-23T18:49:41Z`、`finished_at=19:15:44Z`（约 26 分钟）。
- **冻结证明**：`freeze-before.txt` 与 `freeze-after.txt` 的 `git status --porcelain` / `git diff HEAD` sha256 完全一致（`bc23172a…` / `93b939e8…`，292 项），stage 未在运行期改动任何输入。
- **浏览器闭环**：无头 Edge CDP **16/16 PASS**、0 控制台错误、33 张截图（`.loop/demo-us12-r6/result.json` / `shots/`），覆盖 13 步既有矩阵 + `display-name` / `author-questions` / `schedule-candidate`（含服务端三次触发）。mock 调用记录证明生效在服务端而非页面显示：连接 A 运行 `model="demo-a-model"`、切到 B 后新运行 `model="demo-b-model"`（`mock.log` 02:06:45 / 02:06:48）；就地新建专属连接运行 `model="demo-private-model"`；工具 / MCP / 技能勾选呈现 `tools=7→6→7`、`8→7`、`skill_card=true→false`；温度 0.7 分支命中；离开提示三个 confirm；陈旧 ETag 412 与部分成功恢复由服务端读回核对。
- **通知执行链**：确定性 mock 返回 `notify` 工具调用，`notify-none-blocks` / `notify-allowlist-delivers` 两步 PASS（`mock.log` 02:07:19 / 02:07:22）；真实飞书外发未执行，U17 保持未验证。
- **服务资源哈希**：`.loop/demo-us12-r6/served-hash.json` 判 `pass`——8083 新构建服务的 `/console/`、`index-C-erDfq1.js`（266,360 B）、`index-DdIz_TTu.css`（33,889 B）与本地 `dist` 及 `integrity.json` 逐字节一致；旧包名 `index-IrlgPqCY.js` / `index-BfzM2k6Y.css` 返回 SPA fallback 的 `text/html`，证明旧包未被服务。用户 8080 旧实例（PID 37864）未重启、未用于本轮评审。
- **前端门禁**：`npm test` **18 文件 / 191 条全绿**（R5 184 + R6 7），`npm run typecheck` 通过；`dist` 为本轮 `npm run build` 产物。
- **U 状态**：U18 / U19 翻为已验证（依据见上表：DACL 六子例含负向、同名重建反例）；U17 保持未验证并补 mock 执行链读数；其余 U 项状态不变；T164 仍未勾选（真实 webhook 外发）。
- **文档判卷**：markdownlint 判 7 份改动文档（模块卡 3,221 行、tasks、run-state、宪法、Operations、agent-md、run-and-errors）→ 全部 `errors=0`；正对照（三尾空格 / 去尾换行）分别报 MD009 / MD047，判卷真实执行。
- **未做 / 未验证**：真实飞书 webhook 外发（U17 / T164）；8080 旧实例未重启（用户自行重启后可见新管理台）；未 commit / push / tag / 部署；**人工 review ② 仍为待人工收口 review**（未收到决定，不写成接受）。

**R7 补记（2026-10-24，九项人工验证问题修复与系统代投）**：按用户裁决 A–E、三项修正与固定顺序「修订与实现 → 隔离验证 → 8080 真实配置验证和浏览器走查 → 修完所有发现 → 冻结输入 → 最终统一验收」推进。本节只追加，不改历史行；T164 保持未勾选（真实 webhook 外发）。

- **范围修订**：宪法 v3.0.0（架构约束第 11 条「显式通知模式与受控装配」，仅 `notify` 允许自动增删）；模块卡 R7（九项问题、A–E、三项修正、交付物、八项影响清单）；`docs/contracts/agent-md.md` 增 `schedules[].deliver_to`、§2.6 受控装配、§3.2 代投；`openapi.yaml` / `schema.sql` / `run-and-errors.md` 增投递与测试字段；迁移 `0004_schedule_delivery.sql`（schema v4，只追加）。
- **实现**：core 代投端口与五值、调度器准入预写 / 成功结算 / `SuppressNotify`；app `deliveryService` 与 `notify_assembly`；storage `schedule_deliveries` 与 `resource_connection_tests`；tool 发送适配；web 投递投影、`deliver_to`、`last_test`、作者失败分类；console 会话深链、连接列表精简、记忆说明、渠道测试与执行 / 投递分列。
- **状态**：代码与本轮新增文档已落盘；隔离验证、8080 真实配置验收、浏览器走查、演示对象清理与最终统一验收按后续追加记录登记；人工 review ② 仍为待人工收口 review。

**R7 隔离验证与演示对象清理（2026-10-24）**：本轮固定顺序的第二、四步前半（隔离验证 + 可证明的自动化残留清理），均在**不触碰 8080 进程**的前提下完成；8080 真实配置验收与最终统一验收仍待凭据导入后执行。

| 项 | 读数 / 证据 |
| --- | --- |
| 格式 | `gofumpt -l` 首查 7 个文件（`core/delivery.go`、`core/notify.go`、`core/scheduler.go`、`tool/delivery_test.go`、`app/bindings_test.go`、`app/resource_state_test.go`、`workspace/content_edit_test.go`，均为字段对齐 / 空行），`gofumpt -w` 后复查 0；未动无关文件 |
| 定向套件 | core / app / storage / tool / web / workspace / cli / e2e 相关包逐包通过 |
| 全量 Go（首次） | `go test ./...` → `FAIL`、`EXIT=1`：`TestStatus_ReadyWithoutConfig` 仍按 v3 断言「26 张表」，实际输出「schema v4，28 张表」；日志 `.loop/r7-go-full.log` |
| 全量 Go（修复后） | 同步 `internal/cli/status_test.go`（28 张、v1 11 + v2 7 + v3 8 + v4 2）、`internal/e2e/bootstrap_test.go`（schema v4 / 28 张）、`internal/e2e/v3_test.go`（`requireFreshV3` → `requireFreshSchema`，断言 4 / 28）→ `go test ./...` 全包 `ok`、`EXIT=0`；日志 `.loop/r7-go-full-2.log` |
| e2e build tag | `-tags=integration` 命中 0 个用例（包内真实 tag 是 `e2e`）；改用 `-tags=e2e` 后 `internal/e2e` 三个受影响用例（bootstrap / v3 基线 / 迁移）通过 |
| 前端 | `npm test` **18 文件 / 208 条全绿**、`EXIT=0`（日志 `.loop/r7-console-test.log`；R6 191 + R7 17）；`npm run typecheck` 通过；`npm run build` + `emit-integrity.mjs` 产出 `index-DfHVIkiC.js`（276.82 kB）/ `index-BZZL5gRG.css`（34.07 kB）/ `index.html`，sha256 为 `a21c262c…` / `1be4ce9a…` / `bd1d1ce8…`，bundle 内含 `deliver_to` 与 `schedule-draft` |
| 脚本解析与守卫 | `scripts/import-credentials.ps1` / `scripts/redeploy-local.ps1` 均为 UTF-8 BOM、`parseErrors=0`；导入脚本对「模板位于仓库内」直接 `throw`，对仓库外模板通过路径检查；两脚本均未对 8080 发出任何请求（PID 37864 未变） |
| 演示对象审计 | `demo-partial-mvl8luzu` 是 **R6 走查在隔离实例（8083）上创建的模型连接名**（`.loop/demo-us12-r6/run.mjs:733` 的 `demo-partial-${STAMP}`），不是 Agent、不在 8080 的 `.basalt` 与 API 里；`server-triage2.log:111` / `:117` 两处命中都是本轮审计脚本自己的 `GET /api/v1/runs/demo-partial-mvl8luzu` → 404（05:56:59 / 05:59:46），**无任何活引用**，无需清理 |
| 清理执行（用户裁决 B / C） | 归档 8 个走查 Agent：`demo-walkthrough-20261023-220053`、`-143914-…`、`-144004-…`、`-144004-copy-…`、`-144139-…`、`-144139-copy-…`、`-144412-20261023-220054`、`-144412-copy-…`（`DELETE /agents/{name}` 均 200）；停用测试任务 `hourly-check` 与 `daily-brief`（`PUT /schedules/{id}` → enabled=false） |
| 清理读回 | `/profiles` 剩 3 个活动 Agent：`demo-agent`、`github新闻`、`中国股市近3日涨得最快得5支股票`（名字以服务端读回为准）；`/schedules` 中 `daily-3`（github新闻）与 `daily-stock`（中国股市）仍启用、未被改动；用户创建的 Agent / 连接 / 会话 / 记忆零删除 |
| 待凭据阻塞 | `deepseek`（env，UID `rs_00000000000000000000000021`）与 `feishu-ops`（env，UID `rs_00000000000000000000000012`）需先经 `scripts/import-credentials.ps1` 转为 managed 并实测连通，才能重部署 8080、删除 `demo` mock 连接、把 `demo-agent` 切到 DeepSeek、跑真实一句话定时创建与真实飞书外发 |

**R7 重部署演练（2026-10-24）**：在仓库外的临时工作区（`D:\code\basalt-redeploy-rehearsal`，8084，随机管理员令牌只存在进程环境）用 `scripts/redeploy-local.ps1` 走完整「识别 → 构建 → 优雅停止 → 换件 → 启动 → 校验 → 再优雅停止」；不触碰用户 8080（PID 37864 未变）。

| 项 | 读数 / 证据 |
| --- | --- |
| 首轮失败（真实缺陷） | 演练实例经 `cmd /c` 启动 `bin\basalt`，`Win32_Process.ExecutablePath` 实为 `D:\code\basalt\bin\basalt.exe`（CreateProcess 追加 `.exe` 的解析规则被同目录旧件抢先）：实际跑的是旧构建（schema 2、前端 0/3）。脚本原实现同样经 cmd 启动，**会把旧二进制当成新构建部署并报成功** |
| 修复 | 启动改为 `Start-Process -FilePath <完整路径> -ArgumentList 'serve' -RedirectStandardOutput/-RedirectStandardError -WindowStyle Hidden`，不经 cmd 再解析；`docs/Operations.md` §5.5 增「Windows 启动解析陷阱」与 `ExecutablePath` 复核要求 |
| 复跑结果 | 旧实例（R6 构建，PID 40568，schema 3）→ 构建 `version=383d89e` → `Stop=graceful`（CTRL_BREAK 成功，未走 `taskkill /F`）→ `Build=383d89e Schema=4 Admin=auth-ok Assets=3/3`、新 PID 5840；演练侧独立复核 `health=ok`、`schema=4`、资源 3/3、PID 已变 |
| 二次停止 | 新进程同样 `graceful` 停止；临时工作区与日志已删除 |
| 仓库副作用 | `bin/basalt` 现为本次重构建的 R7 二进制（`schedule_deliveries` 命中 17 处、内嵌 `index-DfHVIkiC`），`bin/basalt.old` 为 R6 构建；`bin/basalt.exe`（10-22）与 `bin/basalt~`（10-23，8080 正在用的镜像）仍是旧件，待真实重部署后清理 |
| 未验证 | 用户真实实例的停止 / 迁移（待凭据导入后执行）；`-Frontend` 分支未在演练中执行（本轮前端已单独构建过）；`taskkill /F` 兜底分支未触发 |

**R7 凭据导入与停止助手演练（2026-10-24）**：临时工作区 8085 + 工作区外主密钥（DACL 收紧到当前用户 / SYSTEM / Administrators），用**假值**演练 `scripts/import-credentials.ps1` 的创建 / 幂等 / 冲突三条路径；不调用真实 DeepSeek 与飞书。

| 项 | 读数 / 证据 |
| --- | --- |
| 演练发现 1（脚本缺陷） | 通知渠道**没有单条 GET**（契约只有列表 + 写端点）；脚本按 `/notify-channels/{name}` 读取得到 404，误判“渠道不存在”后走创建，返回 400「已存在」——首跑在创建后必失败，重跑必失败。修复：改读 `/notify-channels` 列表按名匹配 |
| 演练发现 2（环境前置） | 托管通知地址写入前要过 `http.allowed_domains`；空白名单 → 400「托管地址未通过出网白名单校验」（点名 host `open.feishu.cn`）。用户实例的 `basalt.yaml` 已含该 host（读回第 5–6 行），不阻塞正式导入；`docs/Operations.md` §6.3 已补此前置 |
| 演练发现 3（主密钥 DACL） | 工作区外目录默认继承 Authenticated Users，`secrets init` 按 fail-closed 拒绝；`icacls /inheritance:r` 收紧到当前用户 + SYSTEM + Administrators 后通过 |
| 修复后复跑 | 首次：`[deepseek] 已创建`、`[feishu-ops] 已创建`，exit=0；读回 `mode=managed last4=aaaa availability=ready uid=rs_E5VNB7…`；二次：两资源均「已是最新」，`uid stable=True`，exit=0；换尾号模板：`[deepseek] …未覆盖` + `conflict exit=3`，读回 `last4=aaaa unchanged=True` |
| 停止助手加固 | `redeploy-local.ps1` 的 CTRL_BREAK 改为两段：先发给目标自己的进程组（组号 = PID），2 秒后仍存活再等 5 秒，才退回整控制台（组 0），避免误伤同控制台的部署终端 / 并行任务；复跑重部署演练 `Stop=graceful`、`Schema=4`、`Assets=3/3`、exit=0 |
| 环境清理 | 8084 / 8085 均空，演练目录与进程已删除；只剩用户 8080（PID 37864） |
| 未验证 | 真实 DeepSeek 最小调用、真实飞书 `Basalt 连通性测试`、真实 `-Rotate` 轮换；演练只证明脚本路径与状态机 |

**R7 8080 真实配置验收与浏览器走查（2026-10-24）**：完成固定顺序第三步（8080 真实配置验证与浏览器走查）及第四步（修完所有发现）；凭据由用户经仓库外初始化模板导入，AI 未读取、未打印明文。最终统一验收（第五步）尚未执行，本节不写通过。

| 项 | 读数 / 证据 |
| --- | --- |
| 凭据导入（真实） | `deepseek`（UID `rs_00000000000000000000000021`）与 `feishu-ops`（UID `rs_00000000000000000000000012`）均为 managed、`availability=ready`；`demo` mock 连接已移除；API 与页面均不回明文 |
| 真实 DeepSeek 最小调用 | `POST /api/v1/model-connections/deepseek/test` → `ok=true`、`latency_ms=687`、`model=deepseek-chat`；结果脱敏并落 `resource_connection_tests` / `management_operations` |
| 真实飞书连通性测试 | API 调用 419 ms、页面调用 405 ms，固定文案 `Basalt 连通性测试`，飞书返回 `code=0`；页面「最近实测 07:46:53」由服务端投影刷新；失败分支未在真实渠道制造（用隔离实例的坏地址验证过页面三态） |
| 真实定时投递（系统代投） | 任务 `r7-delivery-verify`、`deliver_to=rs_00000000000000000000000012`；手动触发运行 `completed`、1 次迭代、0 次工具调用、`final_text=定时投递验证通过。`；`schedule_deliveries` 预写 `pending`（`created_at=1792798835615`）→ `sent`（`attempted_at=1792798836225`，预写早于发送）、`http_status=200`、`receipt_code=0`、`detail` 脱敏；同轮 `notify_deliveries=0`、`tool_invocations=0`、`llm_calls=1`；执行视图 `completed` 与投递 `sent` 分列。验证后任务删除，执行与投递历史保留 |
| 真实模型一句话创建（API） | `POST /api/v1/agents/demo-agent/schedule-draft` 指令「每天上午10点汇总昨天的演示数据，用三行简报发给我」→ 200、0.82 s；候选 `demo-agent-daily-report`、cron `0 0 10 * * *`、`Asia/Shanghai`、`next_runs` 2026-10-24/25/26 02:00Z；内容条件写（If-Match）→ 200；`/schedules` 与 AGENT.md 读回一致，无半保存任务 |
| 真实模型一句话创建（页面） | 页面生成候选 `demo-agent-weekly-workspace-digest`（`0 0 8 * * 1`、`Asia/Shanghai`），投递默认选中 `rs_…12`；确认保存 → 服务端读回 → 页面删除提示与行移除；两条本轮任务均已退休，历史执行保留 |
| 浏览器走查（问题 2–5） | 会话深链 `/console/agents/demo-agent/sessions/web-aea8a708479f7c16` 可刷新 / 返回 / 继续对话；连接列表只显示名称 / 默认模型 / 范围 / 可用性，并注明「本地配置就绪 ≠ 实测连通」；定时页分区显示执行状态与投递状态、投递列与旧任务未配置投递提示；记忆页注明「会话历史自动保存、`save_memory` 才进长期记忆、无对话后自动抽取」；配置页自动装配提示「移除 notify」「加入 notify」两次保存后内容逐字节复原（ETag `sha256:cecc3936…` 不变） |
| 本轮新发现并修复 1 | 托管凭据发布 / 撤回按条目声明模式取 Key（`catalogAPIKey`），不再把 managed 条目误判为缺 `${VAR}` 占位；启动定义对账用 `PinModelCredential` 同一判据，失败保旧并点名；新增 `TestModelConnection_ManagedSaveBuildsRuntimeClient`、`TestPublish_ManagedCredentialPublishByVersion`、`TestReconcile_ManagedEditPublishes`、`TestReconcile_ManagedRevokedKeepsPublished` 等用例 |
| 本轮新发现并修复 2 | `/schedules/{id}/executions` 的 `delivery_channel` 原取 `channel_name`，与契约「目标渠道资源 UID」不符；改为 `channel_uid` 并增 `delivery_join_reports_channel_uid` 用例；8080 读回 `delivery_channel=rs_00000000000000000000000012` |
| 重部署（真实实例） | `scripts/redeploy-local.ps1` 两次实跑：`Stop=graceful`（CTRL_BREAK，未走 `taskkill /F`）、`Build=383d89e`（工作树未提交）、`Schema=4`、`Admin=auth-ok`、`Assets=3/3`；新实例 PID 10864、`http://127.0.0.1:8080`，`/api/v1/health` 200；工作区 / 数据库 / 配置 / 主密钥保留；日志 `D:\code\basalt-demo-us12\logs\basalt-20261024-074304.*` |
| 启动脚本与隔离清理 | `start-demo.ps1` 不再写死管理员令牌、不再启动 8099 mock；令牌改由 Windows **用户**环境变量 `BASALT_ADMIN_TOKEN` 提供（脚本与 `.loop` 助手均只读环境，不落盘、不打印）；8099 / R6 mock / MCP 演示进程均已停止，8080 仍为唯一监听；`.loop/dbq` 等只读助手待收尾删除 |
| 未执行（待用户处置） | `D:\code\basalt-demo-us12\credentials.init` 仍在原处；AI 未读取内容，`rm` 请求被权限策略拒绝（不可逆删除用户提供的凭据文件需用户确认）。按 `Operations` §6.3 导入后应删除或转入密钥保管处 |
| 观察（不在本轮范围） | `/api/v1/info` 对托管连接仍按 `base_url && api_key` 判 `configured=false`（既有 `/info` 语义，属 US-11 接口），页面可用性另有 `availability` 与实测读数；不改已发布接口字段语义 |
| U 状态 / T164 | U17 由「未验证」推进为**部分验证**：真实 webhook 下白名单渠道发送成功、页面托管渠道实测已有证据（本节上表）；「声明外渠道在真实 webhook 下被拒绝」仍为工具层用例（R3.6），真实端到端未造。U17 行不改写，最终统一验收时按此口径向 review ② 报告。T164 勾选：连接选择 → 绑定 → 启用 → HTTP 运行（R4 浏览器 9/9）与真实通知发送（本轮系统代投 HTTP 200 / `code=0`）两部分均有当前证据；R5.3 / R6 / R7 节里「T164 保持未勾选」是当时事实，不改写 |
| 判据面增量（本轮，只增不减） | `012-app-resource-admin` required_tests +2（`TestModelConnection_ManagedSaveBuildsRuntimeClient`、`TestPublish_ManagedCredentialPublishByVersion`）；`012-app-reconcile` +3（`TestReconcile_ManagedEditPublishes` / `TestReconcile_ManagedRevokedKeepsPublished` / `TestReconcile_ManagedWithoutGuardKeepsPublished`）；`012-storage-delivery-records` 的 `-run` 与 required_tests +`TestTaskStore_ListExecutions`（执行历史投递列按 UID join）。既有 check / argv / required_tests / 断言零删除；stage 文件 sha256 更新为 `7aae2f6d…` |
| 最终统一验收 | **进行中**：首跑 `make check` 在 `lint` 目标失败——`internal/app/delivery.go` 四条错误文案以 `Agent` 开头触发 staticcheck ST1005（error strings 不得大写开头）；改为中文前置（`装配错误：Agent …` / `代投渠道声明复核失败：Agent …` / `生命周期状态缺失：Agent …`），测试断言子串（`注册表` / `不在注册表` / `生命周期`）保持不变，`TestDeliveryService*` 通过；`go vet` + `staticcheck` 复核 `EXIT=0`。二跑在 `sec` 目标失败——4 条 gosec 命中全部来自临时只读助手 `.loop/dbq` / `.loop/querydb`（`gosec ./...` 会扫到 `.loop` 下的 Go 文件），删除助手后重跑；产品代码零命中。首跑 / 二跑日志保留 `.loop/r7-make-check.log` / `.loop/r7-make-check-2.log`；三跑结果另段追加。人工 review ② 仍为待人工收口 review |

**R7 最终统一验收（2026-10-24）**：冻结输入后一次跑 `make check` 与 US-12 stage；失败轮报告与日志原样保留，不用旧快照代替当前证据。

| 项 | 读数 / 证据 |
| --- | --- |
| `make check`（第三轮） | `EXIT=0`，七目标 `fmt / lint / sec / test / test-integration / cover / build` 全绿；`check-coverage: 全部包达标`——core 87.45% / memory 91.32% / storage 70.78% / tool 85.00%（≥70），app 74.71% / web 90.65% / workspace 75.79% / cli 87.11% 等（≥50）；日志 `.loop/r7-make-check-3.log` |
| 输入冻结 | 冻结清单 = `git status --porcelain` + `git diff HEAD` + 185 个未跟踪文件逐文件 sha256 + 私有模块卡 sha256（`docs/us/` 在 `.git/info/exclude`，不在 git 指纹内，单独计入）；冻结哈希 `5b408d49…`（脚本 `.loop/freeze_manifest.py`，Unicode 路径安全）；stage 运行后复算并比对 |
| stage 文件 | `harness/stages/us12-agent-resource-management.json` sha256 `7aae2f6d…`，39 个 check；本轮只加锚点（见上表「判据面增量」），既有 check / argv / required_tests / 断言零删除 |
| 文档判卷 | markdownlint-cli2 v0.23.3（markdownlint v0.41.1，便携 Node v24.22.0）判 7 份改动文档（宪法、Operations、agent-md、run-and-errors、模块卡、tasks、run-state）→ `Linting: 7 files`、`0 issues in 0 files`、exit=0；日志 `.loop/markdownlint-2.log` |
| 未验证 / 待人工 | `-Rotate` 轮换未执行；U17 的「真实 webhook 下声明外渠道被拒绝」仍只有工具层证据；`/api/v1/info` 的 `configured` 语义未改（已登记观察）；`credentials.init` 删除被权限策略拒绝，待用户处置；人工 review ② 仍为待人工收口 review |

**R7 最终统一验收结论（2026-10-24）**：冻结输入后 `make check` 与 US-12 stage 均通过；失败轮与通过轮日志原样保留，不用旧快照代替当前证据。自动通过不等于人工 review ② 已接受——收口仍待人工。

| 项 | 读数 / 证据 |
| --- | --- |
| `make check`（第三轮，最终） | `EXIT=0`，`fmt / lint / sec / test / test-integration / cover / build` 七目标全绿；`check-coverage: 全部包达标`（core 87.45 / memory 91.32 / storage 70.78 / tool 85.00 ≥70；app 74.71 / web 90.65 / workspace 75.79 / cli 87.11 ≥50）；日志 `.loop/r7-make-check-3.log` |
| stage 结论 | `scope=full/us12-agent-resource-management-r1`、`run_id=us12-stage-r7-full`、`verdict=passed`、`exit_code=0`；49 个 check 全部 `passed`（10 公共 + 39 专属），5156 条测试 `passed` / 18 `skipped`；`started_at=2026-10-24T00:13:17Z`、`finished_at=00:28:51Z`；报告 `.loop/runs/us12-stage-r7-full/report.json`（sha256 `b71de65f…`）、记录 `record.json`（sha256 `21eb25c2…`）、日志同目录 |
| 指纹与环境 | `stage_sha256=7aae2f6d…`、`acceptance_sha256=686fd775…`、`source fingerprint=f08b094a…`、`base_commit=383d89eba1a74c988bd0203fec7adef96df246ed`、`dirty=true`；Go `go1.27.3`、`windows/amd64`、`ci=false` |
| 输入一致性 | stage 的 5 个输入（模块卡 `caa3f976…`、`spec.md` `43739d0c…`、`plan.md` `2f357826…`、`tasks.md` `0ba69499…`、`harness/acceptance.json` `686fd775…`）在运行结束时逐一复算一致 |
| 冻结前后差异 | 冻结前 `5b408d49…`（08:13:11）；stage 运行后复算 `f4fee2dc…`，唯一差异是 `run-state.md`（08:13:22 追加本验收节；流程状态，不是 stage 输入）；设计输入 5 文件零变化，stage 运行期间输入保持冻结；验收后复算 `497e825c…`（快照时点：本句写入前），与 `f4fee2dc…` 相比仅模块卡与本文件两处文档变化，tracked 状态 / diff 零变化 |
| 验收后文档更新 | R7.10「最终统一验收」行由「待执行」改为「已完成」，「8080 真实配置」改为三次实跑；模块卡当前 sha256 `c1d18820…`（pre-verdict `caa3f976…` 见 `record.json` 的 `criteria.inputs`）；markdownlint 复判模块卡 + 本文件 `2 files / 0 issues`（日志 `.loop/markdownlint-4.log`） |
| 最终重部署（问题 7） | 08:30 以 `make check` 新构建再部署 8080：`Stop=graceful`（CTRL_BREAK，未走 `taskkill /F`）、`Build=383d89e`、`Schema=4`、`Admin=auth-ok`、`Assets=3/3`；新 PID 19288、`http://127.0.0.1:8080`；日志 `D:\code\basalt-demo-us12\logs\basalt-20261024-083031.out.log` / `.err.log`；脚本未读取 / 打印 / 落盘凭据 |
| 重部署后独立复核 | `/api/v1/health` 200；三个前端资源服务端字节 sha256 与 `integrity.json` 全部一致；带令牌 `/api/v1/system/overview` 200（`schema=4`、`version=383d89e`），无令牌与错误令牌均 401；资源读回 `deepseek/managed/ready`、`kimi/managed/ready`、`feishu-ops/managed/ready`；3 个活动 Agent（`demo-agent`、`github新闻`、`中国股市近3日涨得最快得5支股票`）均 `registered/runnable`；工作区 / 数据库 / 配置 / 主密钥未改 |
| 未验证 / 待人工 | `-Rotate` 轮换未执行；U17「真实 webhook 下声明外渠道被拒绝」仍只有工具层证据；`/api/v1/info` 的 `configured` 旧语义未改（观察项）；`credentials.init` 仍待用户处置；人工 review ② 待人工收口 |

**R7 收尾清理与证据补记（2026-10-24）**：临时物清理与误删补救如实登记；复跑只补当前证据，不冒充原轮日志。

| 项 | 读数 / 证据 |
| --- | --- |
| 临时物清理 | 删除 143 个 R7 走查临时文件（CDP 脚本 / Chrome profile 日志 / HTML 快照 / JSON / 探针脚本）与便携 Node 工具链 `.loop/lint-tools`；保留 `.loop/runs/us12-stage-r7-full/`、`r7-make-check*.log`、`r7-stage.log`、`r7-redeploy-final.log`、`markdownlint-2/4/5.log`、`r7-final-freeze/`、`freeze_manifest.py`、`r7-redeploy-real.ps1`、`r7-import-real.ps1` 与两份演练日志 |
| 误删与补救 | 清理时误删三份曾被引用的日志：`r7-console-test.log`、`r7-go-full.log`、`r7-go-full-2.log`（不可恢复）。Go 全量的等价当前证据在 stage 的 `test.log` / `test-integration.log`（同冻结源码、全包 `ok`）；前端用当前源码复跑补证（见下行） |
| 前端复跑（补证） | `npm test` → 18 文件 / 208 条全部通过、`EXIT=0`（08:45，20.87 s）；`npm run typecheck` → `EXIT=0`；日志 `.loop/r7-console-test-rerun.log` / `.loop/r7-console-typecheck-rerun.log`。复跑不替代原轮日志 |
| 前端产物版本 | 当前 `assets/console/dist` 为 07:03 构建：`index-D8KybqCr.js`（sha256 `a6bb67c8…`，276,890 B）、`index-BZZL5gRG.css`（`1be4ce9a…`）、`index.html`（`24e5383a…`），与 `integrity.json` 一致，最终重部署服务端字节 3/3 匹配；更早行里的 `index-DfHVIkiC.js`（`a21c262c…`）是当时读数，已被该构建替换（替换命令未单独留存，**待核实**） |
| 案例库 | `docs/interview/engineering-cases.md` 新增 C-039（托管凭据发布链路按声明模式取 Key）与 C-040（执行历史投递渠道 join 用 UID），索引同步；markdownlint 1 文件 0 issues（`.loop/markdownlint-5.log`）；`interview-prep.md` 本轮未派生新题 |

**R8 六项裁决修复与范围修订（2026-10-24）**：按用户 2026-10-24 六项正式裁决完成代码、契约、文档、测试与阶段更新；自动验收通过不等于人工 review ② 已接受，收口仍待人工。

| 项 | 读数 / 证据 |
| --- | --- |
| 范围来源 | 六项裁决写入 [Requirements.md](../../docs/Requirements.md) 的「US-12 R8 修复与范围修订」段、[spec.md](spec.md) 的 R8 节、[plan.md](plan.md) 的 R8 技术计划与 [tasks.md](tasks.md) B14（T211–T220）；R8 仅对 `http.allowed_domains` 的运行期热加载撤销原边界，并新增迁移 0005 到 schema v5 |
| 权威源与热生效 | 新增 `internal/core/network_policy.go` / `internal/storage/network_policy_store.go` / `internal/storage/migrations/0005_network_domain_policy.sql` / `internal/app/network_allowlist.go`：SQLite `network_domain_policy` 单行是唯一权威源，`basalt.yaml` 只在无行时作首次种子；保存走 ETag / 修订号 CAS、预写 `pending`、落库后原子替换同一 `*tool.WhitelistPolicy` 实例，下一次出站判定即生效；收紧时撤销不再允许的 MCP http 连接（放宽不自动重连）；恢复按 `before_revision` / `after_revision` / `candidate_sha256` 三态查证，不重放 |
| 契约 | [openapi.yaml](../../docs/contracts/openapi.yaml) 新增 `GET` 视图字段与 `PUT /sandbox/network-domains`；[run-and-errors.md](../../docs/contracts/run-and-errors.md) 新增 `network_allowlist.update` / `network_allowlist` 字面量与 §9.4 恢复行；[schema.sql](../../docs/contracts/schema.sql) 同步 v5；[agent-md.md](../../docs/contracts/agent-md.md) 与既有接口字段语义未改；零新增 Go 坐标 |
| 错误本地化 | 新增 `internal/web/errors_zh.go`：类型化 reason → 中文映射，执行历史走安全中文投影、不回显原始错误；新增 Go 日志模板为英文；存量中文日志清单 [us12-r8-chinese-log-inventory.md](../../docs/us/us12-r8-chinese-log-inventory.md) 记录 **441 条 / 72 个文件**，**不**声称日志已合规，存量就地转换不在本轮 |
| 管理台 | `assets/console/src/views/workspace.ts` / `workspace.css` 增加执行环境白名单编辑器（实际生效状态、ETag 条件保存、412 重读、409 / 失败保留输入）；`shared.ts` 的 `useLiveLoader` 提供 15–30s 可见页签刷新、visibility / focus 去重、2× 退避、单飞与卸载清理；`agent-detail.ts` / `session-detail.ts` 使用「最近使用」列（无运行时显示创建时间）与「定时任务」来源标记，后台刷新保留数据与滚动位置；不引入 WebSocket，不自动重发运行请求 |
| 前端验证 | `npm test` → 20 文件 / 220 条全部通过；`npm run typecheck` → `EXIT=0`；`npm run build` → `dist/assets/index-CYANyTxQ.js`（285,806 B）、`index-BZZL5gRG.css`（34,076 B）、`index.html`，`integrity.json` 3 个文件一致 |
| Go 验证 | `go test -count=1 ./internal/...` 全部 `ok`；`go test -race -count=1 ./internal/web ./internal/app ./internal/tool ./internal/storage` 全部 `ok`；`make check` `EXIT=0`，七目标全绿，`check-coverage: 全部包达标`（core 85.18 / memory 91.32 / storage 71.02 / tool 85.22 ≥70；app 75.01 / web 90.74 / workspace 75.79 / cli 87.11 ≥50）；日志 `.loop/r8-make-check.log` |
| stage 首跑（失败保留） | 44 个专属 check 首跑 **52 passed / 2 failed**：`012-e2e-v3` 因 `requireFreshSchema` 仍断言 schema v4 / 28 张表（R8 已到 v5 / 29 张表）；`012-app-network-allowlist` 的 `-run` 前缀漏掉 `TestNetworkAllowlistRecovery_*`。失败报告 `.loop/runs/us12-stage-r8-full/report.json` 原样保留，未用旧快照替代 |
| 修复后复跑 | 更新 e2e 的新鲜库断言到 v5 / 29 张表（`internal/e2e/v3_test.go`）、stage 正则改为 `^(TestNetworkAllowlist_\|TestNetworkAllowlistRecovery_)`；其余断言、argv、required_tests 与 protect 未删 |
| stage 最终 | `run_id=us12-stage-r8-full-2`、`scope=full/us12-agent-resource-management-r1`、`verdict=passed`、`exit_code=0`；**54 个 check 全部 passed**（10 公共 + 44 专属）；`passed_tests` 条目 5324、`skipped_tests` 21；`started_at=2026-10-24T03:47:24Z`、`finished_at=04:03:23Z`；报告 `.loop/runs/us12-stage-r8-full-2/report.json`（sha256 `7f0c0aaf…`）、记录 `record.json`；`stage_sha256=df452853…`、`acceptance_sha256=686fd775…`、`source fingerprint=7b5b7136…`、`base_commit=383d89e…`、`dirty=true`、Go `go1.27.3` / `windows/amd64` |
| 输入一致性 | stage 的 5 个输入（模块卡、spec、plan、tasks、`harness/acceptance.json`）在运行期固定；tasks.md 运行后仅翻 T219 / T220 复选框（验收指纹对复选框做归一化，R7 已登记同一口径） |
| 判据面增量（只增不减） | 新增 5 个 check：`012-storage-network-policy`（8 条）、`012-app-network-allowlist`（14 条）、`012-tool-allowlist-hot`（6 条）、`012-web-errors-zh`（6 条）、`012-web-sandbox-write`（10 条）；`012-race` 追加 `TestWhitelistPolicy_ConcurrentSwapAndCheck`；`protect` 增至 275 路径；既有 check / argv / required_tests / 断言零删除 |
| 文档 | 技术方案 §3.6.1 / §3.6.3 / §3.6.5 / §6.2.2 / §6.2.6、需求 §4.4 与 R8 段、Operations §2.1 / §2.2 / §5 / §6、EngineeringStandards §4、DevelopmentLoop §7（陷阱 69 / 70）、`basalt.yaml.example` 同步；markdownlint 11 文件 0 issues |
| 未验证 / 待人工 | 管理台白名单编辑器与自动刷新未在真实 8080 浏览器走查（当前证据为 220 条前端单测、typecheck 与构建）；未在 live MCP http 连接上实测“收紧即撤销”（工具 / app 测试覆盖）；R8 二进制已由 `make check` 重建但未重部署 8080；441 条存量中文日志未就地转换；`credentials.init` 仍待用户处置；人工 review ② 仍为待人工收口 review |

**R8 收口复跑 · `/speckit-analyze`（2026-10-24）**：只读复核 `spec.md` / `plan.md` / `tasks.md` / 模块卡 R8 与宪法 3.0.0。**CRITICAL 0**，可进入独立复核；R8 的 SC-033–SC-038 均有任务锚点，历史 FR-001–FR-104 未在本轮改动。指标：104 条 FR、38 条 SC、223 条任务（T210 收口时翻为已勾选后全部完成）、54 个 stage check。

| ID | 级别 | 锚点 | 发现 | 处置 |
| --- | --- | --- | --- | --- |
| A-1 | MEDIUM | `spec.md:545`（SC-033） | 「管理台与用户可见文案 100% 中文且由类型化 reason 映射产生」把静态文案与错误文案合并成一句，字面上可读成所有界面文案都必须由 reason 映射产生；模块卡 R8.3 的分层是“静态文案中文、错误文案由映射产生”。属验收措辞歧义，不是实现缺陷 | 按模块卡 R8.3 执行，行为不变；不改冻结输入，记录为下一轮文档措辞修正项 |
| A-2 | MEDIUM | `spec.md:546`（SC-034）、T212 | “收紧后不再允许的 MCP http 连接断开且不自动重连”有 4 条 supervisor 单元测试（fake link），但没有 live MCP http 连接实测；`run-state` 已列为未验证项 | 保持未验证；不作为 CRITICAL，不伪造 live 证据；收口报告明示 |
| A-3 | LOW | `spec.md:548`（SC-036） | “旧术语出现次数为 0”没有全局自动扫描，只有目标视图测试；本轮补跑源码 grep：除 `api.ts:36` 的代码注释与 `agent-config.ts:715` 的否定句外，旧术语零命中 | 记录 grep 命令与结果；代码注释不属用户可见文案，否定句符合裁决；不新增断言 |
| A-4 | LOW | `spec.md:550`（SC-038）、`login.ts:106` | 登录页五项说明只有源码与人工查看，没有前端测试断言；未发现越界实现 SSO / RBAC | 记录为测试覆盖缺口；用户已人工查看登录页；不为此改冻结输入 |
| A-5 | LOW | 模块卡 `3502` | R8.8 写 `tests/session-refresh.test.ts`，实际交付与 T216 是 `tests/live-refresh.test.ts`；交付物清单文件名不一致 | 以实际文件为准；记录为文档事实修正项，不改 stage 冻结输入 |
| A-6 | LOW | 模块卡 `3` | 文档状态仍写 R3.6（2026-10-23），R7 / R8 正文已追加到文末；头部状态滞后 | 以文末 R8 正文与本 run-state 为准；记录为文档状态修正项，不改 stage 冻结输入 |

**未发现**：R8 语义与宪法 I–X 的 MUST 冲突、无任务覆盖的 SC、重复/冲突的验收条目、未解析占位符（TODO / TKTK）。**覆盖口径**：历史 FR-001–FR-104 沿用上一轮 analyze 与 R3–R7 证据，本轮只复核 R8 增量；不把“未重审全部历史 FR”写成“全部重新验证”。

## 6. R8R 收口 · 独立复核处置与最终统一验收（2026-10-24）

**人工 review ②：已接受（2026-10-24）**——用户明确「人工验证没有问题了；us-12 收尾吧」。自动化通过是收口前置条件，人工接受是用户的判定；两者分列记录，不互相冒充。

**独立复核**在 R8 增量上给出 5 条发现（1 CRITICAL / 2 MEDIUM / 2 LOW），逐条处置如下；复核方明示**未覆盖**：管理台前端（另有评审轮）、R3–R7 hunk、部分文档、harness / scripts / evals / 日志清单、live server 与浏览器。

| ID | 级别 | 复核发现（要点） | 处置 | 证据 |
| --- | --- | --- | --- | --- |
| R8R-01 | CRITICAL | `internal/web/errors_zh.go:51-91` 用英文前缀表映射用户文案（`strings.HasPrefix` 在 :80），`handlers_lifecycle.go:97,110` 拼接结果；`workspace/agent.go:322,327` 两类拒绝未映射、`:1208` cron 英文 detail；测试固化旧行为，SC-033 字面为假 | 在 `core` 定义 `AgentRejectKind`（15 个常量，字面量即契约英文原因）与 `AgentDefinitionRejectedError`；`workspace` 只产出类型化拒绝并保留历史日志形状（`logReason()`：provider / tool 合并进 `reason`，其余分类进 `detail`）；`content_edit.go` 类型化拒绝直通，不再包成 `InvalidInputError`；`web` 改为 switch-only 中文映射，可能含英文 / 绝对路径的 detail 收为「详情见服务端日志」 | `internal/core/profile.go`、`internal/workspace/agent.go`、`internal/workspace/content_edit.go`、`internal/web/errors_zh.go`；`TestAgentRejectMessage_TypedKindsBecomeChinese`（18 例）、`TestAgentRejectMessage_CoversEveryDefinedKind`、`TestEntryRejectMessage_IgnoresEnglishReason`、`TestProblemMessages_ProjectsEachEntryAndNeverNil` |
| R8R-02 | MEDIUM | `handlers_lifecycle.go:236` 新日志模板为中文「Agent 启停冲突」，与 R8「日志 / 审计英文」口径不符 | 日志改 `agent lifecycle transition conflict`；用户可见错误文案不变 | `internal/web/handlers_lifecycle.go:244` |
| R8R-03 | MEDIUM | `network_allowlist` 读侧缺失：`handlers_operations.go:35,89-95` 非法取值提示与过滤白名单、`openapi.yaml:1005,3132` 枚举、`contract_test.go:836-841` 候选清单、console `OperationTargetKind` | 五处同步接受 `network_allowlist`；console 过滤表加「网络白名单」标签 | `internal/web/handlers_operations.go`、`docs/contracts/openapi.yaml`、`internal/web/contract_test.go`、`assets/console/src/api.ts` / `views/operations.ts` |
| R8R-04 | LOW | `views.go:1098-1140` 白名单 detail 白名单丢弃 `before_revision` / `after_revision`；`handlers_operations_test.go:532` 未覆盖 | 详情白名单加两字段，用例断言两字段存在 | `internal/web/views.go`、`internal/web/handlers_operations_test.go` |
| R8R-05 | LOW | `handlers_sandbox.go:89-94,112-120` 的 409 返回陈旧内存快照的 `current_etag`，重试不收敛 | 409 只回 `operation_id`（契约只对 412 给 `current_etag`）；`network_allowlist.go` 在修订号 CAS 落败后重读权威行、应用到策略实例并撤销不再允许的 MCP http 连接 | `internal/web/handlers_sandbox.go`、`internal/app/network_allowlist.go`；`TestNetworkAllowlist_RevisionConflictReloadsAuthoritativeRow` |

### 6.1 七项证据（四态口径）

| # | 证据项 | 四态 | 本轮当场读数 |
| --- | --- | --- | --- |
| 1 | 门禁通过 | 通过 | `make check` `EXIT=0`，`fmt / lint / sec / test / test-integration / cover / build` 七目标全绿；`check-coverage: 全部包达标`（core 84.98 / memory 91.32 / storage 71.02 / tool 85.22 ≥70；app 75.03 / web 90.72 / workspace 75.86 / cli 87.11 ≥50）；`CGO_ENABLED=0` 构建成功 |
| 2 | 测试映射 | 通过 | US-12 stage `full-3`：54/54 check passed（10 公共 + 44 专属），`required_tests` 全部命中；`passed_tests` 5330、`skipped_tests` 21（均为平台条件整例跳过）；`run_id=us12-stage-r8-closeout-full-3`、`verdict=passed`、`exit=0` |
| 3 | 交付物核对 | 通过 | R8R 触及的生产 / 测试文件均在 stage `protect`（279 路径）；前端文件仍不入 stage check（前端证据为 `npm test` 20 文件 / 221 条、typecheck、构建与 `integrity.json` 3/3 匹配）；`scripts/import-credentials.ps1` / `scripts/redeploy-local.ps1` 按 R7 交付物入库 |
| 4 | 回归 | 通过 | 前序 US 的 44 个专属 check 在本轮 stage 全部 passed；`make check` 全包 `ok`（含 `internal/app` / `web` / `workspace` / `storage`） |
| 5 | 全局不变量 | 通过 | 12 条逐条复核：Agent 框架坐标 / ORM / `math/rand` / 点导入 / 明文凭证 / 审计 SQL 外置 / `BeginCall` 越界全部零命中；`context.WithValue` 2 处均为请求追踪标识（`requestIDKey`）；时区禁用词命中 1 处，为 `scripts/acceptance/invariants_test.go:137` 的判卷注释，生产零命中；`core` 导入清单只含标准库；`Makefile` / 验收脚本零 `node` / `npm` |
| 6 | 验收 Demo | 部分通过 | R7 九条 Demo 与 8080 真实投递证据保留（§1 R7 节）；R8 新增白名单编辑器与自动刷新**未**真实浏览器走查（U20）；live MCP http 收紧撤销未实测（U21）；R8R 后二进制**未**重部署 8080。用户 2026-10-24 的人工接受覆盖其人工复核范围，不替代上述自动化缺口 |
| 7 | 变更导读 | 通过 | 见 §6.3；提交信息按 EngineeringStandards §5 四段双语承载 |

### 6.2 最终 stage 读数与判据面

| 项 | 读数 / 证据 |
| --- | --- |
| stage 结论 | `run_id=us12-stage-r8-closeout-full-3`、`scope=full/us12-agent-resource-management-r1`、`verdict=passed`、`exit_code=0`；54 个 check 全部 `passed`（10 公共 + 44 专属）；`passed_tests` 5330、`skipped_tests` 21；`started_at=2026-10-24T06:18:45Z`、`finished_at=06:38:48Z` |
| 报告 / 记录 | `.loop/runs/us12-stage-r8-closeout-full-3/report.json`（sha256 `4d237dcf…`）/ `record.json`（sha256 `bb7f703d…`）；日志同目录 |
| 指纹与环境 | `stage_sha256=53a29314…`、`acceptance_sha256=686fd775…`、`source_fingerprint=80bea570…`、`base_commit=383d89eba1a74c988bd0203fec7adef96df246ed`、`dirty=true`；Go `go1.27.3`、`windows/amd64`、`ci=false` |
| 输入一致性 | 5 个输入运行后逐一复算一致：模块卡 `18839b03…`、`spec.md` `d3f5bec0…`、`plan.md` `2738b6c8…`、`tasks.md` `4e466ab1…`、`harness/acceptance.json` `686fd775…` |
| 判据面变更 | `protect` 275 → 279（追加 R8R 触及的 `internal/app/registry.go`、`internal/web/handlers_lifecycle.go`、`handlers_lifecycle_test.go`、`handlers_operations.go`）；`012-web-errors-zh` 把两个因前缀表移除而失效的 `TestProblemText_*` 锚点替换为三个类型化映射用例（18 例表 / 15 类覆盖 / 英文不外泄），`-run` 同步扩到 `TestAgentRejectMessage_` / `TestEntryRejectMessage_`；其余 check / argv / required_tests / 断言零变更。依据：R8R-01 CRITICAL 要求删除前缀表映射，属随已批准接口适配，锚点强度只增不减 |
| 失败轮保留 | `us12-stage-r8-closeout-full-2`（53/54，`012-web-errors-zh` 缺锚点，`failed exit=1`）与 `us12-stage-r8-closeout-lockconflict`（进程持锁，`exclusive harness lock unavailable`）原样保留，未覆盖、未删除 |
| 文档判卷 | 本轮改动文档用 markdownlint（markdownlint v0.41.1，便携 Node v24.22.0）复判：`docs/interview/engineering-cases.md` 813 行 / `specs/012-agent-resource-management/run-state.md` 1848 行，均 0 诊断；负对照（重复 H1 + 无语言 fence）报 `MD024` / `MD025` / `MD040`，证明判卷真实生效；日志 `.loop/markdownlint-r8r-final.log` |

### 6.3 改动导读（按包）

- `internal/core`：新增 `AgentRejectKind`（15 个常量）与 `AgentDefinitionRejectedError`；`AgentEntry` 增 `RejectKind` / `RejectDetail`，保留英文 `RejectReason` 仅供日志与 CLI。
- `internal/workspace`：装载期只产出类型化拒绝，`logReason()` 复刻历史 WARN 形状；`content_edit.go` 对类型化拒绝直通。
- `internal/app`：`registry.go` 的技能版本降级改用类型化分类；`network_allowlist.go` 在修订号 CAS 落败后重读权威行并应用。
- `internal/web`：`errors_zh.go` 改为 switch-only；`handlers_lifecycle.go` 日志英文化；`handlers_operations.go` 读侧接受 `network_allowlist`；`handlers_sandbox.go` 409 不带 `current_etag`；`views.go` 详情保留 `before_revision` / `after_revision`。
- `docs/contracts`：`openapi.yaml` 目标类型枚举补 `network_allowlist`；其余契约字面量未改。
- `assets/console`：`api.ts` / `views/operations.ts` 补目标类型与中文标签；`dist` 重建，`integrity.json` 3/3 匹配。
- `harness/stages`：`protect` 279 路径、`012-web-errors-zh` 锚点替换、approval 追加 R8R 处置说明。

**重点 review 清单（按风险）**：① `errors_zh.go` 的 switch-only 映射与 `profileView` 的 detail 抑制；② `content_edit.go` 的类型化直通（防回归为 `InvalidInputError`）；③ `network_allowlist.go` 的 CAS 落败重读路径；④ 409 不承诺 `current_etag` 的契约对齐；⑤ `logReason()` 对既有日志断言的兼容；⑥ stage 锚点替换是否等价或更强。

**如何验证**（可复制）：

```text
GOPROXY=https://goproxy.cn make check
sh scripts/check-acceptance.sh --stage harness/stages/us12-agent-resource-management.json --out .loop/runs/<new-dir>
cd assets/console && npm test && npm run typecheck && npm run build
```

### 6.4 未验证 / 待人工

- U20（R8 白名单编辑器 / 自动刷新真实浏览器走查）、U21（live MCP http 收紧撤销）保持未验证；U15 / U16 由用户人工 review ② 接受，已在 §4 注明依据是人工判定。
- 441 条存量中文日志未就地转换，清单在 `docs/us/us12-r8-chinese-log-inventory.md`；不声称日志已全英文。
- R8R 后二进制未重部署 8080（当前实例仍为 R7 的构建，schema 4）；`credentials.init` 仍待用户处置。
- analyze 表的 A-1（SC-033 措辞）、A-5（模块卡测试文件名）、A-6（模块卡头部状态）记录为文档修正项，本轮未改冻结输入。

## 7. 收口后 CI 修复轮（2026-10-24）

### 7.1 现象与定位

- CI run `40071215899`（push `main` @ `84064ba`）：`build（CGO_ENABLED=0 四平台交叉编译）` 与 `console` 绿，`test（默认 CGO…）` job 红。
- 三个红目标同因：`test` / `test-integration` / `cover`；唯一失败用例 `TestWire_ResourceAdminPortWiring`。
- 报错原文：`testing.go:1619: TempDir RemoveAll cleanup: unlinkat /tmp/TestWire_ResourceAdminPortWiring…/001/.basalt/versions/skills/wire-skill/<uid>/1/SKILL.md: permission denied`。
- 用例断言全部通过；失败发生在 `t.TempDir()` 清理阶段，Linux 专属（Windows 本地与 `make check` 均绿）。
- 现场证据：artifact `acceptance-evidence` → `ci-40071215899-1/{test,test-integration,cover}.log`。

### 7.2 根因

- 技能已发布版本目录按约束 25 置为只读：文件 0o444、目录 0o555（`internal/workspace/skill_admin.go`）。
- Linux 的 unlink 需要**父目录**写权限，0o555 目录内的文件删不掉；Windows 则是只读属性挡住 `RemoveAll`。
- 既有助手 `unlockTreeOnCleanup` 只被 `resource_admin_test.go` 的资源写面用例调用；wire 夹具（`newWireFixture`）从未登记，而 `TestWire_ResourceAdminPortWiring` 会经真实写路径发布技能版本。

### 7.3 修复

- `internal/app/wire_test.go`：`newWireFixture` 在 `workspace.Init` 后登记 `unlockTreeOnCleanup(t, workDir)`——cleanup 后注册先执行，早于 `t.TempDir` 的 RemoveAll，覆盖全部 wire 用例。
- `internal/app/resource_admin_test.go`：修正助手注释，写明 Linux（0o555 目录阻断 unlink）与 Windows（只读属性）两平台机制。
- 产品代码零变更；断言 / `required_tests` / 阈值 / stage / acceptance 一字未动（判据面无变更）。

### 7.4 验证读数

- 受影响用例：`go test -count=1 -run 'TestWire_ResourceAdminPortWiring|TestWire_NotifyAdminPortWiring' ./internal/app` → `ok`。
- `make check` EXIT=0，七目标全绿，覆盖率判据全部达标（`internal/app` 75.01%，`check-coverage: 全部包达标`）。
- 全量 stage 复跑 `us12-stage-ci-fix-1`：54/54 check passed，`passed_tests` 5330、`skipped_tests` 21、`status=passed`、`exit=0`；源码指纹 `887ef8d3…`、base `84064ba`、`dirty=true`；`stage_sha256 53a29314…` 与 `acceptance_sha256 686fd775…` 与 §6 一致（stage 文件未改）。
- report sha256 `b32ea1fe…`；record sha256 `02471099…`。
- **未验证**：Linux 侧行为只能由修复后的 CI run 复核（本机只有 Windows）；修复提交后以新 run 结论为准，若再红按同一口径继续定位，不调判据。
