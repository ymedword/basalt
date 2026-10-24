# US-12 验收报告（Agent 与公共资源动态管理）

本文件是 US-12 的**验收报告**，随实施过程追加；自动验收读数引用 `.loop/runs/` 下的机器报告，人工项按四态口径标注。当前状态：最终全量门禁与 stage 全绿（均在复核修复后的同一份源码上重跑），Demo 可脚本部分已执行；收口分析 CRITICAL 0，独立复核的阻断项已清零并逐条处置。R3.3 判据修订（§4.3.2 发布失败用例场景强化）已用独立目录复跑全量与 `make check`，读数见 §4；人工 review ② 待收口。

## 1. 自动门禁（T069 / T070）

| 门禁 | 四态 | 读数 |
| --- | --- | --- |
| `make check` 七目标 | 通过 | 最终轮（第四轮，复核修复后）exit=0；fmt / lint / sec / test（`-race`）/ test-integration（`-tags=integration` 单列）/ cover（按包判卷，全部达标）/ build（`CGO_ENABLED=0`）全绿。覆盖率关键包：core 87.70% / storage 81.81% / tool 85.65% / memory 91.3% / app 75.31% / web 90.90% / workspace 78.45%。第三轮（修复前）同样 exit=0（`run-state.md` §1.70；日志 `/tmp/us12-make-check4.log`） |
| 阶段全量验收（28 check） | 通过 | 最终全量 run `scope=full/us12-agent-resource-management-r1`，28/28 `passed`、exit=0；18 个 US-12 专属 check 的 `required_tests` **615/615 命中（0 缺失）**，实际执行并通过的测试函数合计 3673，平台跳过 14 条（`012-race` 10 + 四处软链接子例各 1，均为非锚点、已登记 U8/U10/U12/U13）。报告 `.loop/runs/us12-stage-final-full/report.json`（`go1.27.3`，`base_commit=383d89e`，`started_at 2026-10-22T09:40:36Z`；`run-state.md` §1.73） |

口径说明：第三轮全量报告里的「3621」是**执行并通过的测试函数总数**（`passed_tests` 求和），不是 `required_tests` 命中数；本表已按最终全量报告分别列出「required 命中 615」与「passed 合计 3673」。`/tmp/*.log` 为当轮临时日志，收口时随临时物删除，读数以本报告与 `.loop/runs/` 机器报告为准。

## 2. Demo A–I 执行记录（T071）

`quickstart.md` §5 的九条 Demo 是模块卡定义的人工演示；本轮执行其中**可脚本化**的部分：在临时工作区用真实二进制（`internal/e2e` 的 `TestMain` 构建，`-tags=e2e`）经 HTTP 驱动，或执行与该 Demo 同判据的自动化用例。页面交互、真实模型、真实 webhook、真实 MCP server、真实 30 秒长跑等需要人工 / 环境的步骤一律标**未验证**，未用自动化读数冒充。

证据命令与日志：

```text
GOPROXY=https://goproxy.cn go test -tags=e2e -count=1 -v -run 'TestAdminConsoleE2E_|TestUpgradeNotes_Documented' ./internal/e2e
  → /tmp/us12-demo-e2e.log（6 例 PASS，EXIT=0）

GOPROXY=https://goproxy.cn go test -count=1 -v -run '<与各 Demo 同判据的用例>' ./internal/app ./internal/workspace
  → /tmp/us12-demo-unit.log（25 例 PASS）
  → /tmp/us12-demo-unit2.log（8 例 PASS）
  → /tmp/us12-demo-unit3.log（5 例 PASS）
```

| Demo | 本轮执行 | 读数 | 四态 |
| --- | --- | --- | --- |
| A 全流程 | 真实二进制：创建 Agent → 停用态调用 409 → 启用 → HTTP 调用 `completed` → 同名 / 非法名 400；作者模型草稿端点三例 | `TestAdminConsoleE2E_FullFlow`（1.49s）、`TestAdminConsoleE2E_CreateCopyArchivedDefaults`（0.18s）、`TestAgentAdmin_GenerateFiles{NoOperationRow,NotConfigured,CallErrorPassthrough}` 全 PASS；磁盘出现 `AGENT.md`、`disabled` / generation=1、无 staging 残骸均有断言 | 通过（脚本面）；页面内草稿编辑未验证 |
| B 禁用一致性 | 停用态调用 409 `AGENT_UNAVAILABLE`（e2e）；复制 `disabled` Agent | `TestAgentCopy_DisabledBootstrap`（0.15s）断言副本 `disabled` / generation=1 / `schedules: []` / 不携带运行数据；e2e 覆盖停用态 409 与停用期文件提交 | 通过（脚本面）；页面交互未验证 |
| C 发布失败保旧 | 发布 / 撤回 / 非法候选零写入（e2e）；发布指针与磁盘在失败下保持旧值 | `TestAdminConsoleE2E_ModelConnectionPublishRevert`（0.16s）；`TestResourceAdmin_PublishFailureKeepsOldPointer`、`PublishBuildFailureKeepsOld`、`DBFailureRestoresFile`、`NoFileWriteOnPreWriteFailure`、`TestPublish_RevertCreatesNewVersion` 全 PASS | 通过（脚本面）；真实只读文件系统注入未验证 |
| D 删除闸门 | 被技能引用的删除 400 点名、解除后 200、同名重建版本号续增（e2e）；活动 Run 引用 400 点名 `data.active_runs` | `TestAdminConsoleE2E_SkillReferenceGate`（0.80s）；`TestReferences_ActiveRunRefs`、`TestResourceDelete_ActiveRunRef` 全 PASS | 通过（脚本面） |
| E MCP 待重启与授权 | 新建即发布 `pending_restart`、应用版本只在连接后写入、HTTP 传输拒绝 IP 字面量与非默认端口、测试端点零 Run / 零 `llm_calls` / 超时 | `TestMCPCreate_PublishesPendingRestart`、`TestMCPPublish_KeepsApplied`、`TestMCPSupervisor_PendingRestartFlag`、`AppliedOnlyAfterConnect`、`HTTPRejectsIPLiteralAndNonDefaultPort`、`TestMCPTest_NoRunNoLLMCall`、`TestMCPTest_Timeout` 全 PASS（进程层用测试注入传输） | 部分通过；真实 MCP server 连接与真实重启未验证 |
| F 技能导入边界 | 越界 / 隐藏目录 / 大小写重复 / 非 UTF-8 / 非普通文件 / 超限大小 / 文件数全部拒绝；校验端点同口径零写入；合法导入发布 | `TestSkillImport_BoundaryMatrix`、`SizeLimits`、`CaseFoldDuplicate`、`NoExecution`、`TestSkillValidate_SameGateAsImport`、`NoWrites`、`TestResourceAdmin_SkillImportPublishes` 全 PASS | 部分通过；软链接导入拒绝子断言本机平台跳过（`SeCreateSymbolicLinkPrivilege` 缺失，见 U10 / U8） |
| G 通知引用 | 占位入库、明文 400、同名 400、列表只回 host、`force` 字面量、被引用删除 400 点名、更新空地址保留旧值、环境变量缺失发送失败 | e2e `TestAdminConsoleE2E_WorkspaceAndNotify`（0.15s）；`TestNotifyAdmin_{PlaceholderOnly,CreateAndUpdate,UpdateGuards,DeleteReferenced,ForceDelete,OnlyHost}`、`TestNewNotifyTool_MissingEnvFailsSend` 全 PASS | 通过（脚本面）；真实 webhook 投递未验证 |
| H 归档与恢复 | 归档 / 原路恢复 / 异名恢复（e2e：目录进 `archive/<name>-<stamp>`、恢复 `disabled`、generation 递增 / Bootstrap）；超时回滚与冲突回滚矩阵 | `TestAdminConsoleE2E_FullFlow`、`CreateCopyArchivedDefaults`；`TestAgentArchive_TimeoutRollsBack`、`RollbackMatrix`、`TestAgentRestore_OriginalNameGeneration` 全 PASS（超时用注入时钟推进 31 秒） | 通过（脚本面）；真实长跑 30 秒等待未验证（U3） |
| I 工作区与资源页 | 工作区浏览：残骸不可见、`../` 400、>256 KiB 400 不截断、不跟随软链接口径；412 条件写零写入 | `TestAdminConsoleE2E_WorkspaceAndNotify`；`TestAgentAdmin_SubmitStaleETagNoOperation`（0.05s） | 部分通过；软链接断言平台跳过（U13）；页面 412 重取 ETag / 409 / 503 / 401 交互未验证 |

### 未验证项（按 T071 要求显式保留）

- 管理台页面的全部交互步骤（草稿编辑、412 后重取 ETag 保留输入、409 / 503 / 401 提示、资源详情写操作）——需人工在浏览器执行。
- Demo E 的真实 MCP server 连接与「重启后追平」——本机无可用 MCP server，进程层只用测试注入传输。
- Demo H 的真实长跑 Agent 30 秒等待——自动化以注入时钟推进，未在真实长 Run 上计时。
- Demo G 的真实 webhook 投递（环境变量缺失发送失败仅断言到错误与日志）。
- Demo F / I 的软链接子断言——Windows 账户无 `SeCreateSymbolicLinkPrivilege`，已在 `run-state.md` §4（U8 / U10 / U12 / U13）登记，CI Linux 可复核。
- Demo C 的「只读文件系统」真机注入——自动化用 DB 失败与构建失败等价覆盖。

## 3. 收口证据（步骤 7）

### 3.1 `/speckit-analyze` 复跑（2026-10-22）

| 项 | 读数 |
| --- | --- |
| 输入基线 | `spec.md` sha256 `6bac0de2fe63269f223bd50deddafdebd898afc50c934a02870b7a524932520e`、`plan.md` sha256 `0f5123cdc3fb8889ea0608d0b8d0592694df80da5d85e3f6f693b8309cb0c75c`，与步骤 6 analyze 基线逐字一致；`tasks.md` 74/74 勾选、零未勾选，文档序无空洞 |
| 任务增量 | 归一化指纹 `5d3604ed44fcde49b251ff5a07c56508160887c1247b9e15bfae43e8fa5f12db` 与冻结基线 `2c331ae2…` 的差异仅限 analyze 处置追加的 T067A–T067C 与勾选翻转（`run-state.md` §1.2 / §1.5） |
| 步骤 6 findings 复核 | C1（CRITICAL）：T067A 已落地——`internal/app/baseline.go` 已修改，`TestRecoverAdmin_ActionMatrix`（`internal/app/agent_admin_test.go:1462`）与 `TestRecoverTest_UnknownNotResent`（`internal/app/resource_admin_test.go:2766`）存在并随 `012-app-reconcile` passed；E1：T067B 的 12 个 race 用例随 `012-race` 执行（2683 passed / 10 skipped）；E2：`TestUpgradeNotes_Documented` 随 `012-e2e` 执行（6 passed）；E3：T067C 的三步实跑与 dist / integrity 对账见 §3.3；I1 / I2 / D1 / A1 的处置仍在 `tasks.md` |
| 结论 | **CRITICAL 0**；无新增 CRITICAL / HIGH；三件输入未发生已审范围外的语义变更，收口分析通过 |

### 3.2 独立复核（2026-10-22）

独立上下文（只读子代理）复核冻结模块卡 R3.2 / `spec.md` / `plan.md` / `tasks.md` / 契约与当前工作区，**不读** run-state 与验收报告；总结论**阻断**，8 条 findings 全部逐条处置如下（无静默丢弃）。

| id | 严重度 | 复核结论（摘要） | 处置 | 复验证据 |
| --- | --- | --- | --- | --- |
| F-01 | 阻断 | 生产连接器只有 `mcp.CommandTransport`，HTTP 配置 `Command` 为空 → HTTP MCP 无法启动 / 测试 / 应用；阶段 HTTP 用例全走假连接器 | **已修**：`dialTransport` 按 `Transport` 分支（[mcp.go:321](../../internal/tool/mcp.go#L321)），新增 `dialHTTP` 构造 streamable HTTP 传输、零子进程、拒绝重定向（[mcp.go:338](../../internal/tool/mcp.go#L338)）；新增真实 HTTP 连接用例 4 例并纳入 `012-app-mcp-supervisor` 的 `required_tests` | `TestMCPHTTP_ConnectListAndCall`、`TestMCPHTTP_URLShapeRejected`、`TestMCPHTTP_UnknownTransportRejected`、`TestMCPHTTP_RedirectNotFollowed`（`internal/tool/mcp_http_test.go:62/102/136/152`）passed |
| F-02 | 高 | 删除 / 发布崩溃窗口：块已移除但事务未提交时，重试删除与重新发布都被分歧闸门 409 阻断，约束 13 承诺的两条恢复路径均不可用 | **已修**：删除按 `blockGone` 幂等补齐继续（[resource_admin.go:2196](../../internal/app/resource_admin.go#L2196)）；发布按 `fileMatchesTarget` 接受上次改写后的目标字节（[resource_admin.go:1259](../../internal/app/resource_admin.go#L1259)）；技能删除重试同口径 | `TestPublish_RetryAfterFileRewriteCompletes`（`:831`）、`TestResourceDelete_RetryAfterBlockRemovedCompletes`（`:869`）、`TestResourceDelete_SkillRetryAfterArchiveCompletes`（`:899`）（`internal/app/resource_admin_test.go`）passed |
| F-03 | 中 | `markApplied` 写库失败后仍 `connected=true`、新版本工具已进注册池，与图「失败 → connected=false」不符 | **已修**：新增 `abandonApply`，失效关闭本次连接、`Connected=false`、清空工具名、保留旧 applied 与固定错误短语（[mcp_supervisor.go:254](../../internal/app/mcp_supervisor.go#L254)） | `TestMCPSupervisor_ApplyFailureFailsClosed`（`mcp_supervisor_test.go:525`）、`TestMCPSupervisor_MarkAppliedSuperseded`（`:554`）passed |
| F-04 | 中 | 测试端点未解析 `${VAR}`，字面量直送子进程；启动路径会解析、测试路径不会 | **已修**：测试端点先 `resolveMCPEnv` 再连接（[resource_admin.go:1756](../../internal/app/resource_admin.go#L1756)） | `TestMCPTest_ResolvesEnvBeforeConnect`（`resource_admin_test.go:2124`）、`TestMCPTest_MissingEnvNotConfigured`（`:2142`）passed |
| F-05 | 中 | 形状级 `If-Match` 拒绝（缺失 / 弱 / `*` / 逗号列表 / 格式非法）返回 412 但不带 `data.current_etag` | **已修**：`requireIfMatch` 在可解析当前值时把 `current_etag` 放进 `AdminError.Data`（[admin_routes.go:165](../../internal/web/admin_routes.go#L165)），形状错误与陈旧值给出同一组字段 | `TestAdminRoutes_ShapeRejectionCarriesCurrentETag`（`internal/web/admin_routes_test.go:291`）passed |
| F-06 | 中 | 恢复在 `RestoreAgentDir` 成功、`MarkRestored` 之前崩溃：重启标 `unknown`，重试两条路径都 409 | **裁决接受**：FR43 / 约束 44 把该窗口定义为 `indeterminate → unknown` 且**不自动补做**；卡片只点名 rename 之前的补偿窗口，未承诺此窗口可重试。恢复路径保留 `unknown` + 提示「不得盲目重做」，由人工处置（[baseline.go:1095](../../internal/app/baseline.go#L1095)）；代码注释同步写明该窗口 | `TestRecoverAdmin_ActionMatrix` 的 restore 子用例随 `012-app-reconcile` passed；残留在 run-state §4 未验证清单 |
| F-07 | 低 | 首次 ETag 比较通过、`pending` 已写后，锁内重读发现外部改动 → 412 但留下 failed 操作行 | **裁决按约束 1a 收口**：锁内重读依赖竞态状态，不属「纯请求校验」；`pending` 之后的失败必须置 failed 并返回可查 `operation_id`。已补 `operation_id`（见下「修复轮补充」），普通陈旧 ETag 仍是预写前 412 零操作行 | `TestAgentAdmin_SubmitStaleETagNoOperation`（预写前路径）与新增 `data.operation_id` 断言（post-pending 路径）passed |
| F-08 | 低 | 卡片点名的 `TestRecoverDelete_FilePointerMismatchUnknown` 不存在；`internal/workspace/recovery.go` 注释仍写「本 US 无 staged 写入方」 | **已修 / 记录**：注释改为 US-12 起为活动分支（[recovery.go:19](../../internal/workspace/recovery.go#L19)）；点名映射见下表（stage 早已按实际精确名展开） | `gofumpt -l` 干净；映射表逐条入 `required_tests` |

复核点名的文档名 → 实际测试映射（全部在 stage `required_tests` 内，无家族空缺）：

| 卡片点名 | 实际落地 |
| --- | --- |
| `TestRecoverDelete_FilePointerMismatchUnknown` | `TestRecoverAdmin_ResourceActionMatrix`（`internal/app/resource_admin_test.go:2820`）子用例 `delete_file_removed_indeterminate`（`:2890`） |
| workspace `TestAgentAdmin_*` | 已在 `internal/app/agent_admin_test.go` 同名族（`012-app-agent-admin`） |
| workspace `TestAgentDraft_*` | `core.TestAuthor_Draft*` + `workspace.TestAgentFiles_*` |
| workspace `TestValidateAgentName_*` | `TestCreateAgent_RejectsInvalidName`、`TestAgentFiles_PathMatrix` |
| storage `TestResourceVersions_*` | `TestResourceStore_*` 的版本用例（`012-storage-admin`） |

**修复轮补充（控制侧自查，与上述 findings 同轮修复）**：

- **post-pending 失败必须可追查（约束 1a / 43）**：`pending` 已写入后，Agent 创建 / 提交 / 复制 / 归档 / 恢复与通知渠道写面的失败原先返回裸错误，web 层也只发固定文案；现由 `postPendingError`（[agent_admin.go:1371](../../internal/app/agent_admin.go#L1371)）统一补 `operation_id`，`writeAgentAdminError` / `writeResourceAdminError` 全部改走 `writeAdminDataError`（[handlers_admin_agents.go:311](../../internal/web/handlers_admin_agents.go#L311)），`ErrorDataView` 白名单增 `operation_id`（[views_admin.go:359](../../internal/web/views_admin.go#L359)）。分类哨兵经 `AdminError.Unwrap` 保持 `errors.Is` / `errors.As` 穿透；`AgentBusyError` 同时带 `run_ids` 与 `operation_id`。
- **归档目标冲突分类（约束 33）**：`workspace.ErrArchiveTargetExists` 经 `archiveTargetConflictError` 提升为 409 CONFLICT（[agent_admin.go:1397](../../internal/app/agent_admin.go#L1397)），同时保留哨兵身份。
- **块编辑变量遮蔽**：资源删除路径 `candidate` 被 `:=` 遮蔽导致后续校验读旧值；改为 `var commentsLost bool` + 赋值（[resource_admin.go:2202](../../internal/app/resource_admin.go#L2202)），执行重试与回滚用例全绿。
- **判据面锚点补全**：复核后审计发现 12 个已交付测试未进 `required_tests`（`TestAdminRoutes_ShapeRejectionCarriesCurrentETag`、`TestMCPHTTP_*` 4 例、`TestMCPSupervisor_ApplyFailureFailsClosed` / `_MarkAppliedSuperseded`、`TestMCPTest_ResolvesEnvBeforeConnect` / `_MissingEnvNotConfigured`、`TestPublish_RetryAfterFileRewriteCompletes`、`TestResourceDelete_RetryAfterBlockRemovedCompletes`、`TestResourceDelete_SkillRetryAfterArchiveCompletes`），已按所属 check 补入并逐一用 `go test -list` 验证被选择器命中。

**未覆盖面清单处置**：复核者声明的未覆盖面（其本人未亲自执行测试与前端构建、未验证真实外部依赖等）在修复轮由本报告 §1、§3.3、§3.6 的**当场运行读数**补足——全部测试、阶段套件与门禁都在最终源码上重跑；仍属环境限制的真实模型、真实 webhook、真实 MCP server、真实长跑等待保留在 §3.8 未验证清单，未用自动化读数冒充。

### 3.3 证据 1：门禁命令全绿

| 目标 | 四态 | 读数 |
| --- | --- | --- |
| `make check` 七目标（fmt / lint / sec / test / test-integration / cover / build） | 通过 | 最终轮（复核修复后）exit=0；`sec` 零命中（govulncheck `No vulnerabilities found.`），`test` 为 `go test -race -cover ./...`，`test-integration` 单列 `-tags=integration`，`cover` 按包判卷「全部包达标」（`run-state.md` §1.73） |
| Stage 固定目标（eval-build / behavior / injection） | 通过 | 最终全量 run 中三者均 passed、exit=0（修复轮后重跑，不再沿用修复前读数） |
| 阶段全量（28 check） | 通过 | 最终全量 run 28/28 passed、exit=0；18 专属 check 的 `required_tests` 615/615 命中、0 缺失，passed_tests 合计 3673；报告 `.loop/runs/us12-stage-final-full/report.json` |
| 跳过 / 未验证 | 记录 | 阶段内 4 个 check 各 1 条平台软链接跳过、`012-race` 10 条平台跳过；均非 `required_tests` 锚点，逐条登记在 `run-state.md` §4（U8 / U10 / U12 / U13） |

### 3.4 证据 2：Harness 映射与关键回归对号

十八条专属 check（`012-*`）全部存在、非空且 passed；check ID 与模块卡 §4.4 逐字一致，逐 check 的 passed / skipped 读数见 §1 的 28 行表。关键回归点与代表用例：

| 回归点 | 代表用例（本轮 passed） |
| --- | --- |
| 管理写先留痕（预写失败零写入） | `TestAgentAdmin_CreateNoResidueOnPreWriteFailure`、`TestResourceAdmin_NoFileWriteOnPreWriteFailure` |
| 条件写 412 零写入 | `TestAgentAdmin_SubmitStaleETagNoOperation`、`TestAgentFiles_StaleETag412NoWrite` |
| 发布失败保旧 | `TestResourceAdmin_PublishFailureKeepsOldPointer`、`TestPublish_RevertCreatesNewVersion` |
| 版本只追加 | `TestPublish_RevertCreatesNewVersion`（`012-storage-admin` 50 passed） |
| 删除引用闸门 | `TestReferences_ActiveRunRefs`、`TestResourceDelete_ActiveRunRef` |
| MCP 待重启与按传输授权 | `TestMCPSupervisor_StdioOwnsProcessTree`、`TestMCPSupervisor_HTTPNoProcess`、`TestMCPTest_NoRunNoLLMCall` |
| 技能导入边界 | `TestSkillImport_BoundaryMatrix`、`TestSkillValidate_SameGateAsImport` |
| 归档超时回滚与恢复两条路径 | `TestAgentArchive_TimeoutRollsBack`、`TestAgentRestore_OriginalNameGeneration`、`TestAgentRestore_RewritesNameIdentityAndClearsSchedules` |
| 启动对账 | `TestReconcile_InvalidEditKeepsPublished`、`TestReconcile_ConcurrentStartup` |
| 每定义文件提交锁与分歧闸门 | `TestResourceWrite_FileDivergedConflict`、`TestResourceWrite_DivergenceCheckedInsideFileLock` |
| CLI `profile create` / `delete` | `TestCLIProfileCreate_WritesManagementOperation`、`TestCLIProfileDelete_WritesArchiveRow` |
| 工作区越界拒绝 | `TestWorkspaceBrowse_NoSymlinkEscape`（平台跳过子断言见 U13） |
| 前端无危险 DOM | `v-html` / `innerHTML` 等 grep 零命中（§1.67）；Vitest 75 例全 pass |
| 零新增依赖零迁移 | `go.mod` / `go.sum` 与 `docs/contracts/` 工作区零变更；无新增 SQL |

**复核修复轮补锚（12 个已交付测试原先未被 `required_tests` 点名）**：`TestAdminRoutes_ShapeRejectionCarriesCurrentETag`、`TestMCPHTTP_ConnectListAndCall` / `_URLShapeRejected` / `_UnknownTransportRejected` / `_RedirectNotFollowed`、`TestMCPSupervisor_ApplyFailureFailsClosed` / `_MarkAppliedSuperseded`、`TestMCPTest_ResolvesEnvBeforeConnect` / `_MissingEnvNotConfigured`、`TestPublish_RetryAfterFileRewriteCompletes`、`TestResourceDelete_RetryAfterBlockRemovedCompletes`、`TestResourceDelete_SkillRetryAfterArchiveCompletes`——已分别补入 `012-web-etag-routes`、`012-app-mcp-supervisor`、`012-app-resource-admin` 的 `required_tests`，并用 `go test -list` 逐一验证被所属 check 的 `-run` 正则命中（判据只收紧、未放宽）。

### 3.5 证据 3：交付物清单逐项核对

| 类别 | 计划 | 实测 |
| --- | --- | --- |
| 新增生产 Go | 28 | 28/28 存在且未跟踪 |
| 派生新增生产 Go | — | 2：`internal/tool/mcp_process_unix.go` / `mcp_process_windows.go`。模块卡约束 19 / 22 与 §3.1「`mcp_supervisor.go`」明确要求 stdio 停止 / 取消清理进程树（Windows `taskkill /T`、Unix 进程组）；平台调用必须按 build tag 拆分，属已审机制的实现细节，无新接口 / 依赖 / 配置键。Linux / Darwin 交叉编译 exit=0；Windows 随 `make check` 的 build 通过 |
| 修改生产 Go | 20 | 20/20 已修改；派生 3：`internal/app/baseline.go`（analyze C1）、`internal/app/resources.go`（约束 16 / 17 的运行期路由与两种落库形态，`run-state.md` D2 / T032 / T065）、`internal/web/handlers_agent_content.go`（抽出共用 `editUnsupportedMessage`，文案与行为不变） |
| 新增测试 | 28 | 28/28 存在且非空 |
| 修改测试 | 20 | 20/20 已修改；另有 6 个既有测试随已批准接口适配（`recovery_operations_test.go` / `resources_test.go` / `wire_mcp_test.go` / `wire_test.go` / `content_edit_test.go` / `mcp_integration_test.go`——最后一个按 T069 的 FR8 command/args 拆分适配），均在 stage `protect` 内；两处期望按新架构扩展（启动步骤集、MCP 指针保留项），无删除断言或放宽阈值 |
| 前端源文件 | 13 | 13/13 存在（4 views + 3 components + `api-admin.ts` + `router.ts` + 4 tests）；视图文件名适配：工作区视图落 `views/workspace-browser.ts`（`views/workspace.ts` 已被 US-11 占用），路由仍为 `/console/workspace`（tasks 输入已登记该适配） |
| 前序 US-11 前端壳被本模块触碰 | — | 4 个修改文件：`app.ts`（注册 4 条新路由视图与导航分组）、`api.ts`（`ApiErrorData` 失败数组白名单，卡 §3.1.8）、`views/resources.ts`（列表页内嵌 `resource-form`，卡 §3.1.10 页面—路由表）、`views/workspace.ts`（通知页内嵌 `notify-channels`，同表）；`dist/` 为构建产物（旧 hash 文件删除、新 hash 新增、`index.html` / `integrity.json` 更新） |
| 前端构建 | `npm ci` → `npm test` → `npm run build` | Node 24.22.0（与 `.nvmrc` 同版）：`npm ci` 118 packages；Vitest 8 文件 / 75 例全 pass；`vue-tsc --noEmit` + Vite 36 模块；`integrity.json` 3 文件；二次构建字节一致（`run-state.md` §1.67） |
| 配置 / 文档 | `basalt.yaml.example` / `docs/Operations.md` / `docs/GettingStarted.md` | 前两者工作区 `M`；`docs/GettingStarted.md` 为私有文件（`.git/info/exclude`），快照 sha256 `fa137e7f6c91ca2217055288450963617cfc872abd2833fef77d421c5f774f13` |
| Stage | `harness/stages/us12-agent-resource-management.json` | 存在，sha256 `62c637c0b42d8766b39e611aeae55024da8bccc891f4f1eaeec7781c57739e90`（复核修复轮补 12 个锚点后的版本；`--lint-stages` 返回 `stage lint: ok`）；本机专用，未进 `current.json` / CI |
| 迁移 | 0 | 无新增 / 修改 SQL，`schema.sql` 零变更 |
| Go 坐标 | 0 | `go.mod` / `go.sum` 零变更 |
| 前端运行时依赖 | 0 | `package.json` / `package-lock.json` 零变更 |
| 契约 | 0（仅复用已批准修订） | `docs/contracts/` 工作区零变更 |

### 3.6 证据 4：前序模块回归

| 项 | 四态 | 读数 |
| --- | --- | --- |
| 全包单测（含前序模块） | 通过 | `make check` 的 `test` 目标为 `go test -race -cover ./...`，exit=0 |
| 集成层 | 通过 | `go test -tags=integration ./...` exit=0；`internal/tool` 四例在 T069 二轮修复后转绿 |
| 基线 gate | 通过 | 最终全量 run 的 fmt / lint / sec / test / test-integration / cover / build / eval-build / behavior / injection 10 项全部 passed（修复轮后同一源码上重跑） |
| 并发回归 | 通过 | `012-race` 2709 passed / 10 skipped（平台软链接；较修复前 +26，含复核修复新增用例） |
| 跨模块契约 | 通过 | `012-web-etag-routes` 80 passed、`012-cli-author` 37 passed、`012-e2e` 6 passed |

### 3.7 证据 5：全局不变量逐条自查

| # | 不变量 | 四态 | 本轮读数 |
| --- | --- | --- | --- |
| 1 | 坐标零 Agent 框架 | 通过 | `grep -nE "eino\|langchaingo\|adk-go\|trpc-agent-go" go.mod` 零命中 |
| 2 | `context.WithValue` 只承载追踪标识 | 通过 | 2 处命中，均为 `internal/web/middleware.go:41/79` 的 `requestIDKey{}` |
| 3 | 审计两表写路径唯一 | 通过 | 审计 INSERT 只在 `internal/storage`（`llm_calls.go:85`、`management_store.go:76`）；其余包经接口 |
| 4 | 凭证零明文 | 通过 | `sk-…` 模式零命中；配置文件凭证均为 `${ENV_VAR}` |
| 5 | 无 ORM / 无自动迁移 | 通过 | `grep -nE "gorm\|entgo\|ent\.io\|xorm" go.mod` 零命中 |
| 6 | 单二进制 | 通过 | 修复轮后重跑：`CGO_ENABLED=0 go build ./...` 与 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...` 均 exit=0（2026-10-22） |
| 7 | 时区禁用词 | 通过 | 唯一命中是 `scripts/acceptance/invariants_test.go:137` 的逆改点注释，生产代码零命中 |
| 8 | 随机源红线 | 通过 | `"math/rand` 零命中 |
| 9 | 点导入零命中 | 通过 | 零命中 |
| 10 | `core` 只依赖标准库 | 通过 | `go list -f '{{.Imports}}' ./internal/core` = 纯标准库列表（`cmp context crypto/rand crypto/sha256 encoding/hex encoding/json errors fmt io/fs log/slog slices sort strconv strings sync time unicode unicode/utf8`） |
| 11 | LLM 审计入口唯一 | 通过 | `internal/core` 内 `.BeginCall(` 零命中 |
| 12 | 前端构建不进入 Go 门禁 | 通过 | `Makefile` / `scripts/check-acceptance.sh` / `scripts/check-acceptance.ps1` 对 `node` / `npm` 零命中 |

### 3.8 证据 6：剩余人工项与已知偏差

**harness 已判卷；以下项目等用户人工执行或裁决：**

| 项 | 状态 | 依据 |
| --- | --- | --- |
| 管理台全部页面交互（草稿编辑、412 重取 ETag、409 / 503 / 401 提示、资源详情写操作、归档恢复） | 未验证 | 需浏览器人工执行 |
| Demo A 第 1 步真实作者模型草稿 | 未验证 | 本机未接真实模型；自动化覆盖未配置 503 与假客户端路径 |
| Demo E 真实外部 MCP server 连接与重启追平 | 未验证 | 本机无可用外部 MCP server；SDK 的 HTTP 传输本身已由 `TestMCPHTTP_*`（真实 SDK server，进程内 httptest）覆盖，U1 已据此翻转为已验证 |
| Demo H 真实长跑 Agent 的 30 秒等待 | 未验证 | U3；自动化用注入时钟推进 |
| Demo G 真实 webhook 投递 | 未验证 | 只断言到环境变量缺失时的失败与日志 |
| Demo C 只读文件系统真机注入 | 未验证 | 自动化用 DB 失败与构建失败等价覆盖 |
| Demo F / I 软链接子断言 | 未验证 | U8 / U10 / U12 / U13；Windows 无 `SeCreateSymbolicLinkPrivilege`，CI Linux 可复核 |
| Windows 进程树回收 / Unix 进程组运行时行为 | 未验证 | U2；两平台均已编译通过，运行时回收未在真机断言 |
| `ResourceGate` 与定义文件锁的竞争 / 延迟 | 未验证 | U4 |
| 既有工作区的启动对账幂等性与耗时 | 未验证 | U5 |
| `author.temperature` 未随请求发送 | 已知偏差 | U7；`core.ChatRequest` 无字段，属后续 US 的已批准接口变更 |
| 通知发送失败日志级别 WARN（卡片写 ERROR） | 已知偏差 | `run-state.md` §1.53 D2；冻结的 `NotifyTool` 无 logger 面，执行器统一记 WARN，点名渠道与变量名 |
| 新增 `#nosec G204`（`internal/tool/mcp_process_windows.go:34`） | **待确认** | 抑制的是 `exec.Command("taskkill", "/T", "/F", "/PID", pid)`：可执行名常量、唯一变量是 `os.Process` 的 int PID、不经 shell；`gosec -nosec ./internal/tool/...` 证明该行确会命中 G204，移除注释需改走 `os.StartProcess` 或手工拼 `exec.Cmd`（更差）。按 module-dev 规则，抑制需用户逐条确认，故列入收口裁决 |
| U9：baseline 在 supervisor 连接前把「已发布未 applied」的 MCP 指针补成 applied | **待裁决** | `run-state.md` §4 U9；连接失败时 `pending_restart` 可能显示 false，与 FR20「连接失败保留旧应用版本」张力，需人工决定修复或接受 |
| US-11 管理面 `AgentConsole.Transition` / `UpdateContent` 的 post-pending 失败不带 `operation_id`（[agent_console.go:219-222](../../internal/app/agent_console.go#L219-L222) 等处返裸错误，web 写面不投影） | **范围外观察（未修改）** | 与该规则在 US-12 写面的缺陷同源，但 US-11 判据面已交付验收、不属本 US 的已审改动范围；本 US 未触碰其断言或实现，登记给后续 US 裁决 |
| 恢复在 `RestoreAgentDir` 成功、`MarkRestored` 之前崩溃的窗口 | **接受残留风险** | 复核 F-06 裁决：按 FR43 / 约束 44 标 `unknown`、不自动重做，由人工处置（`run-state.md` §4 U14）；卡片只承诺 rename 之前的补偿窗口 |

**人工 review ② 状态：待人工收口 review（未收到决定，不写成接受）。**

**临时物说明**：正文引用的 `/tmp/us12-*.log` / `/tmp/us12-*.txt` 是当轮临时日志，已按本机临时物纪律在收口时删除；可回溯的证据保留在 `.loop/runs/` 的机器报告与本报告逐条读数中。

## 4. R3.3 判据修订与复跑（2026-10-22）

**依据**：用户裁决（2026-10-22）修订模块卡 §4.3.2 的测试场景，**不修改约束 9、FR-024 与发布契约**。R3.2 补上 `VersionID` 后，用例仍只发布过 v1，撤回目标与被替换版本内容相同，文件改写是空操作；发布契约（`docs/contracts/openapi.yaml:831`）明确「不带版本号且最新版本已发布 → 幂等返回、不插入版本」，原示例前置条件写错。修订与裁决全文见模块卡文末 R3.3 节，过程记录见 `run-state.md` §1.74 与失败台账新增行（历史行未改）。

| 项 | 落地 |
| --- | --- |
| 用例 | `internal/app/resource_admin_test.go`：已发布 v1（a.example / m1）与 v2（b.example / m2），指针 / 运行时在 v2；显式撤回 v1，`ExpectedETag` 用 v2 内容哈希，注入 `failNextVersionInsert("disk full")` |
| 断言 | 注入确已触发（日志含 `disk full`）；`AdminError` 分类 + 可查 `operation_id`；定义文件恢复为操作前字节；published 与 applied 仍为 v2；运行时为同一 v2 客户端且默认模型 `m2`；版本数不变、无残留新版本；操作行 `failed` / `lifecycleResultInternal` / `发布事务失败` / ExpectedETag / RequestedVersionID |
| 幂等覆盖 | 保留 `TestPublish_Idempotent` 与 `TestPublish_SkillIdempotent`，断言未改（用户裁决第 4 点） |
| 判据面性质 | 修正测试场景以验证既定契约，**未降低**「失败保旧」要求；无生产代码改动；约束 9 / FR-024 / 发布契约零变更 |
| 指纹同步 | 模块卡 sha256 `daccf82d…`（2287 行）；tasks sha256 `4acc1d14…`（307 行 / 75 任务，T072 追加、T025 未改）；stage approval 文本补 R3.3 说明并记录最终卡哈希（最终 stage sha256 `70b9b9ce…`） |

**红态证据**：`go test -overlay` 把 `commitModelConnection` 的文件恢复调用短路后，本用例逐字失败：`失败后定义文件必须恢复为操作前字节（v2 内容）`，实际文件停在 v1 字节（`https://a.example` / `m1`）。旧断言（指针 + 运行时非 nil）不检查文件字节，按断言内容与失败分支的写序推断在同一逆改下仍会通过（属推断，未单独执行旧版用例）。探针目录建在仓库外、用毕即删。

| 复跑项 | 四态 | 本轮读数 |
| --- | --- | --- |
| 定向用例 | 通过 | `go test -count=1 -run 'TestResourceAdmin_PublishFailureKeepsOldPointer\|TestPublish_Idempotent\|TestPublish_RevertCreatesNewVersion' ./internal/app` → `ok … 2.044s` |
| stage lint | 通过 | `stage lint: ok`，exit=0；仅 `us15-linux.json` / `us8-notify-r1.json` 的历史 WARN，US-12 stage 零发现 |
| stage-only 复跑 | 通过 | 18/18 `passed`、exit=0；`required_tests` 615/615 命中（0 缺失），`passed_tests` 3673、跳过 14（非锚点，U8/U10/U12/U13 同前）；`012-app-resource-admin` 实际执行清单含 `TestResourceAdmin_PublishFailureKeepsOldPointer`。报告 `.loop/runs/us12-stage-r33/report.json`（`started_at 2026-10-22T11:14:17Z`，`go1.27.3`，`base_commit=383d89e`） |
| 阶段全量复跑 | 通过 | profile 10 项（fmt / lint / sec / test / test-integration / cover / build / eval-build / behavior / injection）+ 18 专属 check，28/28 `passed`、exit=0；报告 `.loop/runs/us12-stage-r33-full/report.json`（`started_at 2026-10-22T11:16:30Z`） |
| 卡哈希最终同步后 stage-only | 通过 | stage approval 更新为最终卡哈希 `daccf82d…` 后复跑；18/18 `passed`、exit=0；`required_tests` 615/615 命中（0 缺失）、`passed_tests` 3673、跳过 14；`record.criteria.stage_sha256=70b9b9ce…`、`source_fingerprint=a686401d…`；报告 `.loop/runs/us12-stage-r33b/report.json`（`started_at 2026-10-22T11:43:28Z`） |
| 卡哈希最终同步后全量 | 通过 | profile 10 项 + 18 专属 check，28/28 `passed`、exit=0；报告 `.loop/runs/us12-stage-r33b-full/report.json`（`started_at 2026-10-22T11:46:34Z`，`run-kind=rerun`） |
| `make check` 复核 | 通过 | exit=0；cover 判卷「全部包达标」——core 87.59% / storage 81.81% / tool 85.65% / memory 91.32% / app 75.27% / web 90.90% / workspace 78.45%；build 为 `CGO_ENABLED=0` 构建 |

**历史读数保留**：收口轮的 `.loop/runs/us12-stage-final` / `us12-stage-final-full` 与报告 §1 的读数不因本节重写；R3.3 复跑用独立目录 `us12-stage-r33` / `us12-stage-r33-full`，卡哈希最终同步后再用 `us12-stage-r33b` / `us12-stage-r33b-full`，均为 `run-kind=rerun`。

**未验证**：无新增未验证项；U2–U5、U7–U10、U12–U14 等既有平台 / 范围外项状态不变（见 §3.8）。人工 review ② 仍为**待人工收口 review**。

## 5. 管理台凭据表达修订与复跑（2026-10-22）

**依据**：用户裁决（2026-10-22）——「密钥不能进入普通配置与版本历史」不等于「页面永远不能输入密钥」；当前只接受 `${VAR}` 并由服务进程解析环境变量是**实现边界**，不是设计法则。US-12 保留现有服务端边界，先把页面表达和使用引导做对；凭据托管、网页录入与目标地址约束由后续 G 阶段承接（已补入 `career/Phase2IntegratedPlan.md` §13.3，不在本 US 实施）。本修订不改变服务端契约、发布语义、`apiKeyEnv` 投影或 stage 判据。

| 位置 | 修订 | 目的 |
| --- | --- | --- |
| [resource-form.ts](../../assets/console/src/components/resource-form.ts) | 输入改为「环境变量名」（如 `DEEPSEEK_API_KEY`），提交时归一成契约要求的 `${VAR}`，也接受粘贴完整占位；创建必填，编辑留空 = 保留当前引用；编辑时展示当前引用 | 用户不再误以为要填写真实密钥，也不会把明文送进请求 |
| [resource-form.ts](../../assets/console/src/components/resource-form.ts) | 明文 / 非法变量名在本地拒绝，零请求、错误文案不回显输入；表单展示「密钥来源：部署环境」「真实密钥由部署者配置，本页面只保存引用。修改部署环境后，需要重启服务」 | 失败关闭，且不把真实密钥写进错误、日志或终端 |
| [resources.ts](../../assets/console/src/views/resources.ts) | 列表列「凭据引用」改为「环境变量名」，页头说明密钥由部署环境提供、这里只显示变量名 | 与详情、表单用词一致 |
| [resource-detail.ts](../../assets/console/src/views/resource-detail.ts) | 详情增加「密钥来源」「环境变量名」两行与部署 / 重启说明；连接测试结果拆成「引用解析」与「结果」：503 = 不可解析（测试未发出），200 + `ok` = 已解析后的连接结论，504 = 已解析但超时 | 不把“环境变量存在”显示成“密钥有效” |
| [resource-form.test.ts](../../assets/console/tests/resource-form.test.ts) | 创建用例改为填写裸变量名并断言请求体为 `${MAIN_KEY}`；新增明文本地拒绝（零请求、不回显）、创建必填变量名、粘贴占位并展示当前引用三例；原 400 用例改为 base_url 服务端校验，继续覆盖 problems 展示与输入保留 | 把新交互变成可复跑判据，原有断言未删除 |
| [Operations.md](../../docs/Operations.md) §6 | 增加「管理台创建的模型连接」行：只填环境变量名、真实值由部署者注入、修改后重启、503 = 引用不可解析而非密钥无效 | 运维手册与页面口径一致 |

**复跑读数（四态：通过 / 失败 / 跳过 / 未验证）**：

| 复跑项 | 四态 | 本轮读数 |
| --- | --- | --- |
| 前端依赖安装 | 通过 | 官方 Node 24.22.0（与 `assets/console/.nvmrc` 同版）；`npm ci` 完成，118 packages |
| 前端单测 | 通过 | `npm test`：8 文件 78 例全部 passed（原 75 例 + 新增 3 例） |
| 前端构建与完整性 | 通过 | `vue-tsc --noEmit` + `vite build` + `emit-integrity.mjs` 通过；`dist/integrity.json` 3 个文件，`internal/web` 的 `TestConsoleDist_IntegrityMatchesFiles` / `TestConsoleDist_NoStrayFiles` / `TestConsoleDist_NoExternalReferences` passed |
| `make check` | 通过 | exit=0；fmt / lint / sec / test / test-integration / cover / build 七目标全绿，cover 判卷「全部包达标」 |
| stage-only | 通过 | 18/18 `passed`、exit=0；`required_tests` 615/615 命中，passed_tests 3673、skipped 14；报告 `.loop/runs/accept-20261022T123257.127923900/report.json`（`started_at 2026-10-22T12:32:57Z`） |
| 阶段全量 | 通过 | profile 10 项 + 18 专属 check，28/28 `passed`、exit=0；passed_tests 3673、skipped 14；`record.criteria.stage_sha256=70b9b9ce…`、`source_fingerprint=cef878ef…`、`base_commit=383d89e`；报告 `.loop/runs/accept-20261022T123455.725159900/report.json`（`started_at 2026-10-22T12:34:55Z`，`run-kind=original`） |
| 页面人工走查 | 未验证 | 表单标签 / 当前引用 / 503 与 200 的分开展示需在浏览器确认；自动化只覆盖表单请求体与本地拒绝，详情页引用解析分支无独立前端用例 |

**未变更项**：服务端 `ModelConnectionUpsert` / `ModelConnectionView` / 503 / 504 映射、模块卡、`tasks.md`、stage JSON 与 `record.criteria.inputs` 均未修改；stage sha256 保持 `70b9b9ce…`。人工 review ② 仍为**待人工收口 review**。
