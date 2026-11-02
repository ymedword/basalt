# US-16A 执行状态

1. 本文件是**过程状态，非设计工件**——speckit 命令**不得**把它当设计输入。
2. 失败台账**只追加**：一行一次尝试，不改旧行。
3. 唯一允许原地翻转的是未验证清单的「未验证 → 已验证」，须注日期与证据。

## 1. 基线

- 工作区 D:/code/basalt；起始HEAD `a2746ede6b1fad7b1426770dc7b3b085b8c91aac`，代码基线092e1cd；原工作区干净。
- review ①：用户2026-11-01通过，所审模块卡2154行/289be456…；后续四项裁决与交接v3已读。执行起点原模块卡SHA-256 `043c87b1c25876b14dd40378f61f7c891e037bafc6bbfbabbb67ccde3be096c6`，原交接单 `6c4b53565a59ef06d42c9a0ff312b9f6c15586f3e4253d571c0fb71fe3275448`。
- 用户本轮要求：理解两文档后如可开工，全自动执行，最后仅人工走查；随后明确“台账要us16A呀”“us17已经有了”，以及“后面要我确认yes的，也是你自动yes”。编号固定US-16A，按推荐方案自主裁决，保留US-17。
- 原交接整数顺延说明误导本次先登记17，已只更正自己的新增登记和分支，无提交、无产品改动；当前分支`016a-observability`，目录`specs/016a-observability`，stage`harness/stages/us16a.json`。
- 私有文件仍被ignore，不强制入库。首次快照`career/.backups/20261101T130517738088Z-obs-before-codex`，121文件SHA恢复核对通过；登记前后快照与哈希 `.loop/obs-bootstrap.json` / `.loop/obs-registry-before.md`。
- 允许范围：模块卡§3代码/表/配置/文档与§2.19前序改造；历史迁移、schema.pg.sql、历史stage、既有报告、宪法/安全/审计边界不改。无commit/push/merge/tag/发布。
- Go native/WSL为1.27.3；Node24.22.0；本地PG 55432合成测试库SELECT 1通过。DSN只由既有脚本注入，不打印。
- 2026-11-01当场复核GitHub run41305923229：headSha092e1cd、completed/success、5 jobs success；仅基线证据，不代表本US通过。
- 三件输入及tasks归一指纹：待T001冻结记录追加。

## 2. 失败台账与过程记录

| 日期 | 任务/问题 | 假设/动作 | 原始结果 | 同题计数 |
| --- | --- | --- | --- | --- |
| 2026-11-01 | P000 WSL预检引号 | PowerShell内嵌bash/SQL括号导致转义错误，改为本轮拥有的.loop脚本 | 首跑syntax error；脚本启动Go与psql成功 | 1 |
| 2026-11-01 | P001 PG预检连接 | psql不读取BASALT_TEST_PG_DSN，需显式测试host/port/user/db；密码仍从PGPASSWORD | 首跑错误连接本地5432 socket；显式55432后SELECT 1输出1 | 1 |
| 2026-11-01 | P002 编号 | 先按交接顺延17；用户明确要求16A后修正新登记/文档/分支 | 当前16A唯一行，工作区此前仍无tracked改动；未触及既有US17内容 | 1 |
| 2026-11-01 | T001 输入派生 | 完整范围→spec/plan/tasks；解决来源集合计数、过滤前编号、缺失presence、budget可空与错误传播等派生歧义 | 工件已创建；analyze/冻结待完成 | 0 |

## 3. 停止预算与计数

同题修复3次转debug；同题debug3假设失败停止盲修；本US最多2轮debug。跨会话保留计数；当前debug=0。局部阻断先推进无依赖任务。审批工具拒绝不能绕过；普通实现选择无需再问。

## 4. 未验证清单

| 项 | 状态 | 证据/原因 |
| --- | --- | --- |
| 本US规格分析与输入冻结 | 已验证 | 2026-11-02：R001/R055，40/40映射与冻结指纹一致 |
| 新迁移/事实关联/上下文/生产端 | 已验证 | 2026-11-02：T004–012/024/027实际PG/入口与反例，最终树再经T028 |
| 指标/告警/账本/导出/清理 | 已验证 | 2026-11-02：T013–022及真实故障/恢复/原子性，最终树再经T028 |
| OTLP固定版本spike与Collector三信号 | 已验证 | 2026-11-02：R054官方Collector两编码/三信号与测试后端层1–3/5；外部后端未验 |
| 管理台与视觉交互 | 部分已验证 | 2026-11-02：机器DOM/类型/构建已验；视觉与人工收口仍待走查 |
| 全量工程/阶段/e2e与US16回归 | 已验证（明确上下文） | 2026-11-02：R071，v4七目标/full11/stage40/原生受影响及release_order候选e2e/Linux-root20m完整e2e/非root五项均通过；默认10m与非root全套不作通过声明 |
| 真实外部模型/真实外部后端 | 未验证 | 非本US必需，不调用、不冒充 |
| 生产容量与生产清理启用 | 未验证 | 默认关闭；只做隔离实测，不作生产承诺 |

## 5. 收口结论

**进行中，未到达review ②。** 已具备输入/授权/基础环境，开始自动推进；仅最后走查需要用户。机器证据不足不宣称完成。最终必须给出走查包与私有Claude回交包，依赖与必要内部实现选择列追认。

### R001 输入分析与冻结（2026-11-01）

- 独立只读输入复核 IR-01–10 全部按plan ADR落位：来源集合、budget可空、缺失presence、指标快照、错误传播、全前缀编号、导出关闭清理、适配依赖、integration构建面、非自引用哈希。
- 实施前analyze：34 FR + 6 SC，40项任务覆盖100%，28任务无孤项，CRITICAL=0；来源集合/数据可空性/恢复边界为强语义，修正伪代码不降低断言。stage实际具名发现与integration标签在T002继续核验。
- T001初次markdownlint有4条文档格式诊断（新增块引号间隔、两处多空行、裸URL），按格式规则修正；首次prerequisite命令多写空格未执行，随后用正确命令复验。
- 已审输入/任务归一SHA-256冻结：

  - `docs/us/us16a-observability.md`：`f9dfb6e1ca618954c6e68911a9cf0d079a8936cf46045556f077326e3adc627d`。
  - `specs/016a-observability/spec.md`：`efb273c261a88013dde84e4c7e0653dbf5af9c9d12632413150b344d69978862`。
  - `specs/016a-observability/plan.md`：`f8d545f39a09865148dce1a76bff4142d481daf869f7930ad6b31679b0bcb634`。
  - `tasks_normalized`：`1a5cd1f4f4ae393d36225d7d28df9d8b3c2e7b78c5c08dac05d47fbddb6c4006`。

- 后续仅控制侧按范围内修正追加新基线，任务勾选不改正文；失败预算不重置。

### R002 T001/T002 骨架、有效红态与定向验收（2026-11-01）

- prerequisite脚本正确解析`specs/016a-observability`；独立analyze为CRITICAL 0、HIGH 0、实质FR/SC覆盖40/40。U1已明确从既有转换恢复unknown前状态，T014/020须保留当前unknown所需的最近证据，不能被历史清理删掉。
- `harness/stages/us16a.json`：8 check、98 required_tests、27 task映射，私有输入local-only；未并入current/CI。Collector check待T003实际固定命令后追加，不提前冒充已验。完整stage多数测试尚未创建，预期红，不等于本US已通过。
- 有效红态 `.loop/obs-t002-red.jsonl`：`TestObservability_TimelineLocalOrder`逐字`observability timeline not implemented`，Test失败，原始go test exit=1；AnchorValidation和Example通过。T011必须用完整同毫秒事实夹具加强排序断言，不把错误消失当排序已验。
- 定向验收 `--stage harness/stages/us16a.json --task T002`：fmt与obs-bootstrap具名AnchorValidation均passed，exit=0；报告`.loop/runs/accept-20261101T131308.070475500/report.json`。
- T001/T002勾选仅代表派生与骨架验收，不代表时间线/其他功能完成；下一个T003固定版本spike正在独立临时目录执行。

### R003 T003选型、T004/T005/T006/T009进度（2026-11-01）

- T003：固定版本spike已完成，B无受支持的公开结构化拒绝计数接口，按已批推荐退C；三信号/六协议子例通过，真实回环Collector v0.164.0分别接收1 trace/1 log/1 metric；源码、校验、许可、依赖树与探针体积见research追加记录。产品Collector/T026仍未验证，不拿spike替代。
- T004红态`.loop/obs-t004-red.jsonl`逐字“缺少 v6 迁移文件”；追加0006后，升级、幂等、六新表、枚举拒绝、时间列/v1字节锁均PASS（`.loop/obs-t004-green.jsonl`）。定向stage `obs-schema`带race通过，报告`.loop/runs/accept-20261101T132003.129530000/report.json`，exit=0。
- T004适配既有FreshSchema测试：仅追加版本6映射，当前表数从52变58（v1仍由原52表集合锁定）；新增历史1–5 SHA-256字节锁。没有删除旧表、旧断言或历史stage；这是已审追加迁移的等价适配。
- T005首个夹具用不完整Principal被安全门拒绝，`.loop/obs-t005-red.jsonl`不当作目标红态；改用既有LocalAdminCLIPrincipal后，`.loop/obs-t005-red-valid.jsonl`逐字“real request identity must reach the run ledger”。贯穿核心/PG/视图后请求桥与运行仓储回归exit=0（`.loop/obs-t005-green.jsonl`）。新增可选RunView.request_id后，既有全字段样例需填实际请求ID才能维持“字段集恰等契约”断言；补样例后同名契约测试PASS，不削弱断言。
- T006：新工具ID脱敏回归有效红态“actual tool correlation must survive redaction”（`.loop/obs-t006-red.jsonl`）；补真实call.ID/adapter/按kind allowlist、chunk_invalidated代码与OpenAPI后，diagnostics与相关core回归PASS。app/三向数据库枚举和旧事件完整验证待本任务后续。
- T009：新增真实PG恢复红态“recovery time is needed for terminal-time scans”（`.loop/obs-t009-red.jsonl`）；同UPDATE写recovered_at后，pending归unknown、正常终态不动、工具结束不伪造、二次恢复不重写全部PASS（`.loop/obs-t009-green.jsonl`）。
- T004/T005/T009窄check追加不删除全局obs-pg既有required_tests；未完成功能不拿窄check冒充整包通过。全stage存在性预检尚有未落地测试，预期红；T006/009其余绑定待完成再勾选。

### R004 T005验收与T007/T010/T011进展（2026-11-01）

- T005定向stage fmt/obs-request通过：`.loop/runs/accept-20261101T132144.561537400/report.json`；请求写入、历史NULL/调度不补造、读回与RunView契约回归均通过，勾选T005。
- T007真实检索摘要/候选决定红态`.loop/obs-t007-red.jsonl`：summary未记录；补采集与持久化后知识单元及Assembly真实PG旧回归通过。新夹具最初指定candidate_k=3，原检索规则实际至少为默认topK=8；将输入设9以验证真实生效参数，未改旧规则/断言。四个Run级过滤场景尚待完成，不勾选。
- T010首次误用diagnostics.Reader构造/方法造成编译失败，查源码后改为NewReader(store,cfg,nil).List；编译及迁移回归通过。真实全来源读取测试待补，不勾选。
- T011六条行为红态`.loop/obs-t011-red.jsonl`；实现完整前缀排序/编号后筛选及来源缺口后，`.loop/obs-t011-green.jsonl`全部具名PASS，race通过。下一步扩大多锚点边界并运行定向stage，尚未勾选。
- T006补adapter真实ToolCallID/旧事件无ID与三方枚举、旧未知kind可读测试。首轮stage使用不存在的profile taskcheck，启动前退出1；应使用profile task。首个记录脚本在加载环境前命中WindowsApps的python别名，未执行也未写文件；改用环境加载后的真实Python，源文件未损坏。

### R005 T006通过与过滤夹具修正（2026-11-01）

- T006定向stage fmt/obs-correlation均PASS，报告`.loop/runs/accept-20261101T133131.523951700/report.json`，exit=0；含真实adapter关联、脱敏allowlist、代码/迁移/OpenAPI三方枚举及旧未知kind读取/新写拒绝。勾选T006。
- T007新增四个真实AgentService.Run+PG检索场景。首次项目不匹配期望2实为0、混合期望5实为3；软删/inactive已PASS（`.loop/obs-t007-run-first.jsonl`）。定位：原调用方project为空代表不限项目，故新增project集合实际可见。修正新夹具为调用方及正常集合均obs-project，异项目集合other-project；不改产品规则、不调整期望。该问题修复计数1，下一步原四场景复验。

### R006 T007过滤已验与T008生产端（2026-11-01）

- 四个Run级过滤场景与检索摘要/候选决定race复验PASS，`.loop/obs-t007-run-green.jsonl`，exit=0；项目/软删各2、inactive为1、混合5，计数来自真实授权及两路SQL，不手填结果。下一步补PG预算/摘要presence验证和定向stage。
- T008采用app存储/审计装饰器：真实作业成功开立/写块/写向量审计/索引完成/终态之后才发stage；真实收窄时chunk_invalidated只带count；代际切换无真实请求/操作锚点时不发。启动没有锚点是明确限制，不用代际号伪造operation_id。
- T008首编译`.loop/obs-t008-first.jsonl`失败，原因新增reason映射补丁误插到同文件quota.Release的同名switch；定位到84行，移到knowledgeReasonCode内，修复计数1。下一步原三条生产端测试复验。

### R007 关联基础定向验收准备（2026-11-01）

- T008三条真实生产路径race测试PASS（`.loop/obs-t008-green.jsonl`）；摄取五阶段、轮内撤权真实收窄、真实GenerationManager切换/失败/无锚点均有断言。后续T024补变异反例，最终复核不得把无锚点启动切换称为已关联。
- T007/T010新PG测试PASS（`.loop/obs-t010-first.jsonl`）：非chat预算NULL、检索真零/缺键保持、候选决定持久化；请求/会话/agent范围、LLM/tool、缺失分类、SQL注入字面量、原始错误不在投影中及其他空端口读。
- T010将不存在的操作/摄取分别归已有领域ErrOperationNotFound/fs.ErrNotExist，避免领域依赖数据库错误；T011已区分not_recorded/query_failed，未接线来源显式not_supported；达到旧诊断单页上限保守标截断。
- T011六锚点、独立会话编号及原排序/过滤/缺口race均PASS（`.loop/obs-t011-anchors.jsonl`）。下一步统一obs-foundation定向stage（T007–011），全局98个原required_tests保留，未完成范围仍红。

### R008 基础验收与独立中途复核（2026-11-01）

- obs-foundation定向stage含18个必跑名全部PASS，fmt通过，报告`.loop/runs/accept-20261101T133758.820896000/report.json`，exit=0。证据覆盖T007–011已实现部分，后续改动须重验。
- 独立只读基础review发现B1 HIGH：跨Run合并在排序前break会漏掉稍晚开始但早于首Run后续调用的Run；补重叠Run反例，改为每次保留最早有界集合但继续检查后续Run。B2 HIGH：统一原因白名单抹掉合法摄取错误和固定中文终止原因；改为来源专用转换，新增真实PG断言待跑。
- B3 MEDIUM：chunk_invalidated漏decision=trim，已补生产字段及真实Run断言；B4 MEDIUM：缺装配记录未查装配清理窗口，已复用窗口归因并补测试。只读review为中途限定范围，不冒充收尾独立review；所有发现保持原证据并待原场景复验。
- T012有效红态`.loop/obs-t012-red.jsonl`四具名测试均因context未实现而失败；实现presence JSON空值、允许候选过滤、混合计数逐项档位、装配/模型使用区分、诊断与清理错误保留。预算/段ID的JSON展示来自既有记录，未新增业务结论。下一步上下文/总览race及基础回归。

### R009 基础review修复与上下文通过（2026-11-01）

- 独立review四项修复后，obs-foundation（增加跨Run重叠前缀、装配清理、合法失败原因必跑名）和obs-context六项均PASS，fmt通过；报告`.loop/runs/accept-20261101T134440.982270300/report.json`，exit=0；针对回归`.loop/obs-t012-and-review-green.jsonl`三包race通过。勾选T007/T008/T010/T011/T012。
- T009恢复记录部分已验，但任务正文还包含导出未知边界，需随T016/T017实现后才能勾选；当前窄check不代表全部任务完成。
- 新API展示只序列化来源已有预算/段ID/证据档位；retrieval历史缺键null，真实0保持0，rerank无生产端保持null；上下文主查询失败返回错误，辅助诊断/清理失败保留query_failed。
- 检测到工作期间Claude新增既有US16文档提交`d46cc40d952d1e3f60f2474f68df00a3ffa4d965`（21:24:03），仅`specs/016-knowledge-memory-history/run-state.md`追加两行CI记录。本Agent未提交。起始HEAD记录仍保留，后续报告基线以实际d46cc40为准；US16A三输入及tasks归一指纹未变，工作区实现保留，不重置或改写该提交。

### R010 指标实现红态与设计落地（2026-11-01）

- T013四条核心有效红态`.loop/obs-t013-red.jsonl`：metrics未实现；固定目录/标签、进程累计计数与直方图、来源不可用、重启起点、DB窗口Gauge的核心实现后，`.loop/obs-t013-core-green.jsonl`四具名race均PASS。
- 进程直方图同时保留总计和固定result分类；原ObserveDuration签名不变，增加内部装配所需ObserveExportDuration辅助方法，避免把未知结果伪称ok。RecorderOptions新增无错误返回OnResult(kind,result)回调供采样/写失败/已记录指标，仍不依赖observability包、不进入业务错误链；后续装配须复用同一collector。
- 新增PG完整窗口SQL聚合：Run/调用/工具/过滤空/装配缺失、队列、固定桶和分位；恢复unknown不计零耗时、窗口桶为Gauge。新增真实PG及诊断指标生产端验证待跑。T013审计失败钩子和最终app接线尚未完成，不勾选；metrics导出账本/重叠后端语义随T019完成，不冒充已验。

### R011 T013数据库夹具参数类型（2026-11-01）

- `.loop/obs-t013-pg-first.jsonl`核心4项及真实诊断结果指标PASS；PG夹具在进入被测查询前失败：同一`$1`同时用作bigint时间和未定型`$1+1`，PG报42P08 inconsistent types。显式`$1::bigint`后复验，夹具问题修复计数1；不是产品目标红态。原命令的diagnostics正则未命中任何测试，不将其计为诊断回归通过；下一步另跑该包全量。

### R012 指标/告警核心与真实PG验证（2026-11-01）

- T013夹具类型修正后`.loop/obs-t013-pg-green.jsonl`三包指标核心/真实窗口/写入结果生产端全部PASS；`.loop/obs-t013-diag-regression.jsonl`诊断全包race PASS，弥补前次空匹配，不把空匹配算通过。
- 审计计数采用同DB句柄的原子只读回调注册（ObserveAuditWriteFailures），四类pending写失败均计数，LLM/tool/embedding终态SQL错误与管理面独立终态事务错误也记录；输入验证、既有终态幂等不冒充SQL写失败。新增日志只含固定source/phase/error_kind，不含错误原文。绑定同collector的最终生产接线在T021，启动前的日志仍以既有启动日志为准。
- `.loop/obs-t013-audit-first.jsonl`真实PG四类触发器拒绝计数=4、非法JSON验证不增长；真实AgentService.Run的LLM预写故障计数=1且provider调用数=0，race全部PASS。未改变原始错误与fail-closed。
- T014有效红态`.loop/obs-t014-red.jsonl`为alerts未实现；实现后四条核心race PASS（`.loop/obs-t014-core-green.jsonl`）。真实PG跨重启unknown保留firing/恢复计时、resolved仅历史一次、注入历史写失败回滚当前状态，以及真实分类不把检索混合计数当工具拒绝均PASS（`.loop/obs-t014-pg-first.jsonl`）。
- ADR-10落地：最近转换的已有evidence_ref以obs-meta前缀保存固定原因与不序列化的恢复元数据，保留原表结构；兼容旧引用时从最近非unknown转换恢复，证据损坏/缺失不假定正常。ApplyAlertEvaluation为PG原子端口扩展，状态与转换同事务且检查旧评估；T020必须保护每个当前状态所需的最新转换（含恢复等待），不能按时间一刀删。
- 下一步obs-health统一定向stage；最终T021生命周期/同collector接线、T019指标外发与测试后端、全量门禁仍未验，不把当前独立核心/端口测试当整US完成。

### R013 指标/告警中途独立review处置（2026-11-01）

- 原obs-health 14必跑项与fmt PASS，报告`.loop/runs/accept-20261101T140201.407001700/report.json`；仅为当时版本定向证据，不替代本次修复后的复验。
- 第二轮限定范围独立review新增3项MEDIUM、无CRITICAL/HIGH：M1指标回调panic可能覆盖审计错误/中断业务；仅回调边界recover并记固定metric_callback_failed，不记录panic值，补真实SQL故障和provider返回值保持测试。M2窗口桶/计数Gauge单位误为ms；修为1，sum/分位仍ms，collector不再覆盖数量单位。M3原子业务事务直调finishOperationTx绕过审计失败观测；计数移至唯一管理审计SQL实现，所有六个调用点传同DB句柄，包装器不双计。
- 新M2/M3反例`.loop/obs-health-review-red.jsonl`两项均有效红：数量单位ms而非1、原子归档审计success UPDATE被拒却计数0。产品修复不改原断言。下一步原场景及health带race复验，补仅拒绝success、failed补偿成功且计数恰1的证明。
- 复核确认前次B1–B4关闭；obs-meta与PG状态/转换事务/CAS未见新高风险。T020必须保护每个当前scope最新转换，及unknown态兼容旧引用所需最近非unknown转换。此要求保持后续验收。

### R014 中途review修复复验通过，进入导出（2026-11-01）

- M1–M3针对回归`.loop/obs-health-review-green.jsonl`全部race PASS：回调panic不覆盖SQL错误/供应商成功响应、桶单位1、原子归档的审计UPDATE失败计数1且补偿成功不清零/不双计。
- 最新定向stage fmt/obs-health（16必跑名）/obs-audit-regression全部PASS，报告`.loop/runs/accept-20261101T140831.935469600/report.json`，exit=0；原管理/资源/归档仓储回归具名PASS。T013/T014核心与端口已验，勾选；最终生产接线/生命周期仍由未勾选T021完成。
- 下一步T015持久导出账本；新增来源定位/状态/信号/批次结果类型，批次级partial结果集中分类，非法计数保持NULL，warning只保留固定分类；指标payload只保存派生快照，执行事实只保存定位/哈希并发送前重读。不修改任何业务状态。

### R015 持久导出账本（2026-11-01）

- T015有效红态`.loop/obs-t015-red.jsonl`四具名测试因ledger未实现失败；实现后首次四项真实PG/race PASS（`.loop/obs-t015-first.jsonl`）。后续增加过期持有者不得确认新claim、确认失败整笔回滚、状态查询保留NULL和统一协议分类，`.loop/obs-t015-green.jsonl`七具名全部race PASS。
- Enqueue事务+唯一键、Claim SKIP LOCKED/attempts递增/租约、BeginBatch先记录unknown/NULL并绑定当前claim；Apply在同事务中确认批次与成员，拒绝旧attempt/last_batch_id。partial整批unconfirmed且不再claim，warning零拒绝为sent，非法计数保持NULL；错误与warning只保存固定分类。
- 传输可重试按指数抖动/RetryAfter有界至5m；单项attempt/年龄上限作用于传输失败分支，不把部分成功拆成逐条结果。额外批次策略字段只在内部传递，不新增YAML键；执行事实不持久化payload，只有派生metric_snapshot可持久化快照。
- ExportStatus保留last accepted/rejected可空计数与单位，unknown attempt仅证明已记录准备发送；不证明网络已发出或接收端已收。下一步obs-ledger定向stage；T016扫描/覆盖CAS、T017映射、T018 worker、T019网络、T020清理仍未实现，不宣称端到端外发通过。

### R016 账本定向通过，扫描控制侧准备（2026-11-01）

- obs-ledger七必跑名及fmt均PASS，报告`.loop/runs/accept-20261101T141732.219609200/report.json`，exit=0。勾选T015；尚未建立网络/清理/扫描端到端结论。
- T016采用observability.Scanner的消费侧窄端口+PG适配；按终态时间/来源ID键集、每页独立入队，完整增量和有界对账成功后才CAS推进该来源覆盖。将Enqueue的内部实现返回实际插入行数，保持原公开签名，避免把重复扫描尝试数当新增入队数。
- 新消费侧支撑类型/方法仅服务已审扫描、覆盖、重读能力，不新增REST路径/配置键/业务写口。扫描构造不启动线程；SQL固定来源注册表、所有值走参数。下一步行为红态与PG分页/晚提交/CAS验证。

### R017 终态扫描与覆盖CAS（2026-11-01）

- T016五个边界有效红态`.loop/obs-t016-red.jsonl`均因Scanner未实现失败；实现后全部race PASS。PG六项`.loop/obs-t016-pg-first.jsonl`、其余来源`.loop/obs-t016-pg-sources.jsonl`均PASS，无SKIP。
- 八固定来源按终态时间/来源ID的C排序键集分页，每页独立事务入队；完整增量及有界对账成功后CAS两侧覆盖边界。起点不扩张，时钟后退不倒退覆盖，重读后删除/移动的行不改变原查询游标与完成判定。
- 真实PG验证：早建晚结束、旧历史排除、覆盖CAS冲突、同毫秒跨页、第二页触发器失败后前页已提交/覆盖仍NULL、较小ID先分配但后提交且超出overlap时被对账捕获；恢复未知log/正常零时长trace与pending排除。执行事实payload仍不复制到账本。
- T015内部实际插入数重构后ExportLedger race PASS，见`.loop/obs-t015-count-refactor.jsonl`。新增obs-scan窄门禁12必跑名保留原全US要求；下一步统一门禁，再推进映射/worker。T009仍待映射端恢复unknown证明后勾选。

### R018 扫描通过，映射实现与边界修复（2026-11-01）

- T016最新统一obs-scan十二必跑名与fmt PASS，报告`.loop/runs/accept-20261101T142912.622261300/report.json`，exit=0，勾选T016。
- T017六个有效红态`.loop/obs-t017-red.jsonl`均因纯Mapper骨架拒绝映射失败；当前六项`.loop/obs-t017-first.jsonl`race PASS。仍需完整映射/指标与八源边界统一验收，不提前勾选。
- 自检发现正常unknown工具有实际finished_at时应为Error span（只有无结束恢复才为log）；新增`.loop/obs-t017-tool-unknown-red.jsonl`有效失败。假设定位：MapToolSpan排除了unknown，PG signal也未检查finished_at；修复两处且保留恢复log断言，无盲试。
- 自检收窄state_hash为实际外发字段的规范投影：Session/agent/model等不外发字段变动不应引入新状态；映射失败只保存固定错误投影而不让坏行阻止扫描。指标快照仍按原规范payload哈希并保留原process_started_at。下一步补字段哈希/直方图/所有映射反例并统一复验。

### R019 映射复验与独立复核（2026-11-01）

- `.loop/obs-t017-green.jsonl`只有新增直方图夹具预期错误：collector预置5种结果序列，测试却假定只返回两条非零序列（实际零序列count=0）；没有产品错误读数。控制侧把该夹具明确选择aggregate/ok，原两条count=2/sum=170/桶差分断言不变，并新增独立五条预置零序列完整保留测试；未删用例/弱化原US判据。
- `.loop/obs-t017-green2.jsonl`导出映射及扫描相关全部race PASS，无SKIP。unknown工具有finished_at的Error span分支已修复，恢复无结束仍log。哈希按脱敏外发中间结构规范化，不含private session/agent等；坏记录哈希固定mapping_error投影以便隔离。
- Metrics映射将进程累计桶差分为Histogram各桶，保留原start/count/sum；窗口桶/计数Gauge单位1，窗口sum/分位ms，无StartTime，按实际数据点计RecordCount。独立只读review已交原reviewer，限定账本/扫描/映射，不混入未实现的worker/sender缺口。
- 下一步obs-map+obs-scan统一门禁，随后T018 worker。T009需同门禁中的真实恢复UPDATE→扫描重读→映射log证据通过再勾选。

### R020 独立复核协议冲突NULL计数（2026-11-01）

- T017最新obs-map十四必跑名、obs-scan十二必跑名与fmt全部PASS，报告`.loop/runs/accept-20261101T143409.067434800/report.json`。尚待本轮独立发现处置后确认勾选，不把旧证据覆盖新改动。
- 独立review新增MEDIUM：ResolveSendResult在partial+transport冲突时先填计数指针，违反模块§2.13.2(4)“sendErr==nil且校验通过才填计数”；原控制侧表驱动partial_transport预期错误为known=true。作者进一步定位无partial却有Rejected>0分支也会把protocol_invalid计数落库。
- 新真实PG反例`.loop/obs-export-review-null-red.jsonl`两个子例均有效红，accepted.Valid实际true，且要求仍unconfirmed/不重试。修复假设：计数赋值必须同时满足有效计数、sendErr==nil、无协议冲突；更新原partial_transport预期为false是恢复已审NULL契约的更严格判据，不削弱US断言。下一步修复并连同旧账本/扫描/映射重验。

### R021 独立复核批次成员完整性（2026-11-01）

- 协议冲突NULL修复及账本旧用例`.loop/obs-export-review-null-green.jsonl`全部race PASS，两个冲突均NULL且unconfirmed不再claim。
- 独立review再定位MEDIUM：ApplySendResult未核对持久item_count，调用方若仅传原批次子集仍能确认该批次，遗漏项随后可能重发。新真实PG反例`.loop/obs-export-review-members-red.jsonl`subset有效红（实际无错误），replacement原逻辑已拒绝；两者都要求整笔unknown/NULL、所有项保持inflight。
- 修复假设：锁住批次时取item_count并要求等于已校验唯一的Items数量；逐项既有last_batch_id/attempts/inflight校验保证每个成员都属于该批次，数量+唯一集合+成员fencing共同证明全体匹配。失败仍整笔回滚，不引入第二套成员副本或新业务写路径。

### R022 独立复核摄取失败枚举（2026-11-01）

- 批次成员修复后`.loop/obs-export-review-members-green.jsonl`成员子集/替换、确认原子回滚、旧租约fencing与NULL计数全部race PASS。
- 独立review新增MEDIUM：摄取span借用上下文reason白名单，将path_rejected/decode_failed/parser_failed/verify_failed降成unclassified，导致真实失败分类及其状态哈希丢失差异。六冻结枚举+raw canary反例`.loop/obs-export-review-ingest-red.jsonl`有效红。
- 修复假设：仅为摄取映射定义六个固定错误分类白名单（与既有knowledge.ErrorKinds字面量核对），未知原文仍unclassified，保持observability不依赖knowledge适配层；原私有/敏感字段约束不变。下一步复验并吸收剩余独立发现。

### R023 导出中途review处置完成（2026-11-01）

- 限定独立review最终三项MEDIUM、无CRITICAL/HIGH；E1协议NULL与E2批次成员静态复核修复已确认，E3六类摄取错误映射已修并定向race PASS。review仅覆盖账本/扫描/映射，不冒充尚未实现worker/sender或最终收口复核。
- 新白名单保持六冻结摄取分类，六类与raw canary的映射/哈希逐项断言通过；未知错误仍unclassified。累计无效盲修0；本次为三项新证据的各一次定点修复。
- 将E1/E2 PG反例纳入obs-ledger、E3纳入obs-map，protect新增协议反例；T009/T017绑定map/scan/ledger的当前版本联合复验。下一步通过后勾选相应任务并进入T018。

### R024 映射/恢复当前门禁通过，进入worker（2026-11-01）

- 当前obs-map十五必跑名、obs-scan十二必跑名、obs-ledger九必跑名及fmt全部PASS，报告`.loop/runs/accept-20261101T143733.697868200/report.json`，exit=0。勾选T009/T017；恢复UPDATE→扫描重读→真实Mapper仅log的组合已验，T016证据同步覆盖哈希调整。
- 进入T018：ExportOnce依赖扫描/重读、账本与Sender，构造不启动；单执行互斥，发送前保留claim租约/attempts重读事实、按信号分组。Begin失败不发；Apply失败返回错误并保留unknown/NULL与inflight，下一次过租约可能重复；仅落账成功计入本轮聚合。
- 映射错误/重试预算隔离走窄FailExportItems扩展（PG已有），不制造零记录网络批次；读取暂时失败不标永久业务失败。先建立具名行为红态，再实现worker与真实PG崩溃窗口组合回归。

### R025 worker故障窗口与真实PG组合（2026-11-01）

- T018有效红态`.loop/obs-t018-red.jsonl`的发送/确认/部分成功/哈希/业务只读具名用例均因worker未实现失败；`.loop/obs-t018-first.jsonl`及扩展`.loop/obs-t018-more.jsonl`全部race PASS。
- Worker不自动启动，单次执行互斥，重读只替换临时Fact，保留claim身份/attempts/租约；旧hash标stale，新hash下一扫；失败映射与预算独立隔离，Begin失败零发送，确认失败聚合不报成功。部分成功整批unconfirmed不再claim，异常计数NULL，warning零拒绝为sent。
- 真实PG组合`.loop/obs-t018-pg.jsonl`两项race PASS：确认批次UPDATE触发器失败使先前item更新一起回滚；unknown/NULL保留，租约内不重发、6m后重发，第二次partial确认后停止重试；8张业务/审计/任务表完整JSON指纹前后相等（包括崩溃pending未被恢复）。三信号真实扫描/原指标payload重读→Mapper→持久批次分别确认，第二轮不重复发送。
- Scanner增加已知pending/running/started事实的消费侧防线；PG仍由固定终态谓词筛选，重读pending不入队且不改变原页游标。补具名单元反例pending排除/后续完成可入队，与真实PG pending排除相互补充。
- 新obs-worker门禁17必跑名，保留既有全US要求；下一步worker/ledger/map/scan联合验收，随后T019真实OTLP sender与指标快照exporter。

### R026 worker联合通过，正式协议适配（2026-11-01）

- 当前obs-worker十七必跑名、obs-ledger九名、obs-map十五名、obs-scan十二名及fmt全部PASS，报告`.loop/runs/accept-20261101T144107.761246500/report.json`，exit=0，勾选T018。全US仍有未实现任务，不能以局部通过宣布收尾。
- T019依已授权工作树新增依赖裁决引入C：go.opentelemetry.io/proto/otlp v1.11.1（Apache-2.0）与google.golang.org/protobuf v1.36.12（BSD-3-Clause）。先前spike固定版本/模块校验/许可/三信号回环证据保留，正式依赖树/许可与最终生产二进制差值仍需回填并留review②追认，不提交/发布。
- 正式代码前同入口stripped生产二进制已构建，大小/SHA见`.loop/obs-otlp-product-before.json`；不是原发布基线，不把spike不同入口体积差当产品开销。
- 再核[官方OTLP规范](https://opentelemetry.io/docs/specs/otlp/)（1.11.1）：HTTP成功200须解析typed partial response，可重试仅429/502/503/504；http/json需整数枚举及trace/span十六进制ID（不能直接采用protojson的base64）。三信号独立请求/typed计数；禁重定向/内部重试；固定1MiB响应上限/超时，原始响应/错误不出边界。
- 下一步建立MetricsExporter与真实HTTP sender协议反例，再引入所裁决依赖并实现；真实产品Collector五层验证仍由T026完成，本地假接收器不冒充Collector结论。

### R027 指标入队与sender协议（2026-11-01）

- MetricsExporter四个未实现行为红态与一项已有Gauge映射绿态见`.loop/obs-t019-metrics-red.jsonl`；实现后五项`.loop/obs-t019-metrics-green.jsonl`race PASS。固定15m外发窗口（不新增配置键），本地API仍四种窗口；进程累计起点跟随原快照。Enqueue失败保留原payload/hash再试，不重新计算同点；间隔内跳过，跨新实例相同snapshot唯一键去重。
- 正式依赖go get/tidy完成：除两直接坐标外传递引入grpc v1.84.0、grpc-gateway v2.31.0、x/net v0.59.0、genproto固定版本；原x/text .41→.42、x/oauth2 .35→.36由模块最小版本选择升级，完整树/许可/体积仍需保存，不能称零依赖或零体积开销。
- Sender初始有效红态`.loop/obs-t019-sender-red.jsonl`各信号/格式/结果均因未发送失败；首实现`.loop/obs-t019-sender-first.jsonl`三信号×两格式×四结果、HTTP分类、oversize/malformed已PASS，之后timeout夹具在httptest.Server.Close卡住，手动中断该自有测试会话exit=1，不计整组通过。
- 新定位不是sender超时失败：100ms请求已返回并进入defer Close，假服务未消费请求体就等待r.Context.Done，服务器无法及时检测断连。修复夹具读完body再等待，并设1s退出兜底（客户端超时断言仍100ms/timeout/一次请求、不放宽）；下一步定向重验后跑完整sender组。

### R028 正式OTLP与快照重放证据（2026-11-01）

- timeout夹具修复定向`.loop/obs-t019-timeout-green.jsonl`race PASS；整组`.loop/obs-t019-sender-green.jsonl`四父测试/全部子例race PASS，三信号×protobuf/json×success/warning/partial/invalid，9种HTTP错误分类，oversize/malformed/timeout/redirect与不安全配置均具名通过。无内部重试，真实请求数各1；错误/warning不携原始响应。
- 同名同类型同单位指标合并为Metric多数据点，RecordCount仍是点数；固定service.name与安全build版本，不带主机名/路径/身份。新增typed protobuf的Sum累计/monotonic/原StartTime、Histogram累计/桶/单位和DB Gauge无StartTime形状断言；`.loop/obs-t019-shapes-replay.jsonl`与PG旧快照重放均race PASS，重启前后start/value/time分别保持原值，旧payload不重算。
- 正式模块树132个模块（含项目既有树），9个本次新增/升级坐标的固定版本、校验和、LICENSE原件SHA已存`.loop/obs-otlp-product-dependencies.json`与`.loop/otlp-product-licenses/`；完整graph及raw模块清单保留。无OTel SDK exporter/global meter注册；生成protobuf类型描述初始化不是后台导出。
- 同入口trimpath/stripped：引入协议前28,842,496 bytes，引入协议imports后36,428,288 bytes，差7,585,792 bytes；SHA及命令见`.loop/obs-otlp-product-{before,imports-size}.json`。此时sender尚未生命周期接线，不能把该差值当最终启用功能全部开销；T028仍需最终制品大小。未访问真实外部Collector/backend/model。
- 下一步obs-otlp十一必跑名连worker/ledger/map/scan统一验收；真实官方Collector接收/脱敏/测试后端语义仍由T026完成。通过后进入T020清理，重点覆盖保护/行级确认/删除与账本同事务/告警恢复元数据保留。

### R029 T019联合通过，清理控制侧准备（2026-11-01）

- 当前obs-otlp十一必跑名及worker/ledger/map/scan、fmt全部PASS，报告`.loop/runs/accept-20261101T145419.048762900/report.json`，exit=0，勾选T019。真实Collector产品三信号仍未验，不把httptest协议通过升级为Collector兼容结论。
- T020只允许diagnostic_events/context_assembly_records/alert_history/export_history四表组，默认关闭。只有exporting且diagnostic导出源要求覆盖；行级当前hash/sent、终态时间与任意未决阻断在删除同事务核对；积压保护与既有30m口径一致。导出关闭按保留删除但不声称已导出。
- 为防受保护行反复占同一批、重复计算Skipped，采用仅本轮传递的CleanupCursor（时间/ID），CleanupRequest与CleanupRun内部扩展、不进API/YAML、不新增表或墓碑。每批独立事务、最大批数封顶，重启重扫源行；失败整批回滚，失败尝试另记deleted=0的failed账本，不伪造已删证明。
- 告警历史必须保护当前scope最新转换及unknown兼容旧引用所需最近非unknown转换；导出历史不得删除非终态项，且导出开启时保留仍存在diagnostic源行需要的确认/阻断账本，避免删掉保护事实后误删源行。所有业务/审计/会话表零清理入口。
- 下一步类型/行为红态、PG故障注入与四策略隔离验收；T021以后未实现任务继续保持未勾选。

### R030 清理核心与真实PG首验（2026-11-01）

- 核心缺实现红态`.loop/obs-t020-core-red.jsonl`已保存；覆盖读失败的拒绝用例本就通过，不虚称全部红。实现后七核心用例race PASS：默认不调用存储、批数/游标封顶、未覆盖/区间外跳过、覆盖读错不删、重启和只有导出源要求确认。
- PG两项`.loop/obs-t020-pg-red.jsonl`均因CleanupBatch未实现有效红；`.loop/obs-t020-pg-first.jsonl`两项race PASS：清理成功账本触发器失败使DELETE整批回滚，重试只删1次；sent且当前hash匹配只删1行，另外9类无账本/未覆盖历史/pending/retry/unconfirmed/failed/stale/dropped/wrong-hash全部保留；业务表名入口拒绝且业务指纹不变。
- PG固定表组注册；诊断/装配/告警按锁定候选键分页，导出历史按固定union候选；每组事务advisory锁+500ms锁等待上限；行级导出证明持锁读取，错误不当“没阻断”。失败回滚后仅另记deleted=0/failed尝试。Skip聚合原因按固定优先级，不能当逐条原因分类。
- CleanupRequest另有内部ExportEnabled布尔，只供export_history保留仍存在诊断行所需的确认/阻断证据；原Exporting/RequireConfirmedExport仍只在导出源为真。该补充不进REST/YAML，不新增持久表。RecordSkipped未枚举候选，0表示没有枚举数、不能当该窗口无候选；T022视图须明确“本轮未枚举候选”。
- 下一步补全四表组/告警恢复元数据/积压/并存sent与未决项/跳过游标/重启反例，再独立review与统一清理门禁；T020尚未勾选。

### R031 清理扩展反例与时间presence（2026-11-01）

- `.loop/obs-t020-pg-more.jsonl`六PG父项/全部子例race PASS：完整确认与七种未决状态并存仍保留、未扫描不删、同轮游标让protected行只计一次且后续sent可删、全局31m积压跳过、原子回滚与原分类保持。
- 新四组/告警保护测试`.loop/obs-t020-groups-first.jsonl`有两项夹具错误：参数化prepared statement含两条SQL（pgx不允许），装配夹具缺真实runs外键；尚未跑到产品断言。修复夹具拆SQL并补running Run，不变任何删除/保护断言。
- 同次真正有效红：AlertMetadataUnixEpoch显示obs-meta把合法Unix毫秒0用omitempty省略，重读变Go零时间。定位为旧obsAlertMeta int64无法表达presence，修复为可空*int64，缺失继续缺失、epoch0正常保留；既有现代时间持久化/清理保护回归保持。各问题一次定点修复，不计盲试或放宽判据。

### R032 独立清理review：跨轮前缀饥饿（2026-11-01）

- `.loop/obs-t020-groups-green.jsonl`告警恢复证据保留、其余组与epoch0/原告警持久化全部race PASS；`.loop/obs-t020-core-green.jsonl`核心清理/窗口缺口/确认与pending保护全部race PASS。
- 独立review新增一项MEDIUM、无CRITICAL/HIGH：若protected前缀≥BatchSize×MaxBatches，每轮cursor清空会永远重扫前缀，后面sent不可达。新真实PG反例`.loop/obs-cleanup-review-progress-red.jsonl`有效红，三受保护行>每轮预算二行，第二轮实际删除0而非1。
- 修复假设：RetentionCleaner内部固定四项progress map保存每组最后成功提交批次的游标，跨CleanupOnce续行；成功读到不足一页的末尾后回到起点，重新检查此前blocked后来sent的行；失败不推进未提交批次。TryLock防并发。cursor不是删除证明，所有行仍在每批事务重读/锁定/判断；不进入DB/API/YAML、无逐行墓碑；重启新实例从起点重新遍历并可继续跨轮推进。
- 这修订R029“只在本轮传递”的内部选择，未改模块冻结的有界批次/重启原子性/无墓碑要求。持续频繁重启或写入量超过清理预算不承诺及时清空，T025容量限制需明确。下一步反例与原清理回归重验，再独立核对该修复。

### R033 清理review修复通过，待联合门禁（2026-11-01）

- 跨轮progress修复及原核心/PG清理/告警持久化联合`.loop/obs-cleanup-review-progress-green.jsonl`全部race PASS，无SKIP。独立只读复核R1修复未见新明确CRITICAL/HIGH/MEDIUM，静态关闭；T021必须复用同一Cleaner实例，不能每tick重建，否则丢失跨轮进度。
- 新增父测试覆盖导出批次历史：终态item清理后才可清理其已确认batch；unknown batch及inflight项即使过期也保留。其余四组、源确认阻断、元数据保留、窗口缺口/未枚举说明均维持，不引入业务删除入口。
- obs-cleanup具名清单从已建立的核心与真实PG反例冻结，T020联合health/worker/ledger重验当前版本；通过后勾选并进入配置/生命周期/API。最终全US判据与人工收口仍未执行，T021–028保持未完成。

### R034 配置接线前代理纪律自检（2026-11-01）

- T020当前obs-cleanup二十四必跑名、health/worker/ledger与fmt全部PASS，报告`.loop/runs/accept-20261101T150814.240097700/report.json`，exit=0；R1动态反例与独立复核均关闭，下一步勾选T020并配置接线。
- 读取既有cli.HTTPConfig.Proxy说明发现“HTTP_PROXY/HTTPS_PROXY在任何情况下不读”；新OTLP sender误用了http.ProxyFromEnvironment。仅内部实现纪律调整，不新增代理配置或改变导出默认关闭。拟固定直连，明确配置endpoint仍可https/http。
- 两次新反例`.loop/obs-otlp-env-proxy-red{,2}.jsonl`都绿，不能作为有效红：Setenv发生在测试进程已初始化之后。核对本地Go1.27源码net/http/transport.go:1038确认envProxyFunc用sync.Once缓存环境；清空大小写no_proxy仍未改变缓存。无sender产品修复或盲改；下一步改为自有测试二进制子进程，在exec前注入合成代理环境，所有Dial只允许父进程两个回环监听地址，从进程启动前隔离缓存，再建立有效反例。
- 隔离启动反例`.loop/obs-otlp-env-proxy-isolated-red.jsonl`有效红：代理接收请求1次（期望0）；原sender误读环境确已复现，无真实外网调用。定位已验证，下一步移除Transport.Proxy环境函数并重跑所有协议组，保持请求与计数语义不变。

### R035 清理已验，配置/生命周期开始（2026-11-01）

- R1跨轮进度联合回归与独立只读复核均关闭；T020的24具名清理+health/worker/ledger/fmt当前门禁通过后勾选。T021装配必须生命周期复用同一Cleaner；默认四策略关闭，不动现有8080实例或真实库。
- OTLP环境代理修复`.loop/obs-otlp-env-proxy-green.jsonl`六父测试及全部子例race PASS，隔离子进程中合成代理收到0、显式Collector目标1；原协议/计数/超时/大小限制保持。新反例追加obs-otlp必跑名（现在12），原要求保留。
- T021配置采用cli独立严格段解析（与author段先例一致），显式0/非法范围拒绝，不把错误值当默认；headers只允许ENV引用，可空ENV表示无headers，错误和JSON视图不回显值；enabled默认true，export/cleanup默认false，alerts默认true；不热加载、不新增每规则DSL/阈值YAML键。
- 生产指标collector在诊断端口之前创建，同一collector接诊断结果/audit observer/OBS组件；单serve生命周期worker使用现有runCtx取消源，构造不启动，停机等待并在DB关闭之前停止，不接入任何业务执行/恢复服务。所有测试继续使用隔离schema/临时目录/回环端口。

### R036 严格配置解析实现（2026-11-01）

- `.loop/obs-t021-config-red.jsonl`默认段测试已绿，显式参数/非法范围/ENV形态/错误脱敏因stub忽略配置而有效红。独立段解析接decodeConfig先例，不让YAML解码器静默吞字段；credentialKey纳入observability.export.headers，仅该可选headers允许ENV解析为空，不放宽既有provider/admin凭证。
- 首实现`.loop/obs-t021-config-first.jsonl`编译失败，定位config_observability.go尾部残留stub多余闭括号（171行）；未执行测试。不改判据，删除该孤立括号后格式化并复验。约束具体范围在解析器固定表中，默认保留；lease_ttl>timeout、backfill>safety_lag、装配保留不小于诊断窗口。

### R037 配置canary夹具路径修正（2026-11-01）

- `.loop/obs-t021-config-green.jsonl`默认/显式参数/ENV和大部分非法用例PASS，仅3个canary断言失败；定位并非配置内容泄漏，而是子用例名直接采用含CANARY的配置文本，t.TempDir把测试名带进合法错误路径。错误正文只含固定字段/类别。
- 夹具改用数字子用例名，使路径不含测试机密哨兵；原错误不含CANARY断言与所有非法输入保持不变，不改LoadConfig既有点名路径契约。下一步整CLI回归与新配置具名复验。

### R038 生命周期核心与正式接线（2026-11-01）

- `.loop/obs-t021-cli-green.jsonl`整CLI race PASS，无SKIP；输出中的两条Error为既有命令失败分支的预期读数，不是测试失败。配置新增四具名/子例均通过，普通明文凭证规则未放宽。
- `.loop/obs-t021-wiring-red.jsonl`构造未实现行为有效红（纯AST上下文约束用例已绿）；实现后`.loop/obs-t021-wiring-first.jsonl`六生命周期/业务只读/诊断失败隔离/审计fail-closed均race PASS。后续正式newRuntime同collector接诊断结果与audit observer，heavyRuntime持同一OBS实例，Close在数据库前等待退出；`.loop/obs-t021-production-first.jsonl`新OBS组及重路径次序/端口/锁释放受影响回归PASS。
- 生命周期通过web的消费侧ObservationLifecycle窄端口接入：实际HTTP配置/认证已装配、监听已取得后，Server.Run将原ctx交Start；退出等待Close，再交app释放数据库。不另创建信号源，不在chat/tool命令启动后台；默认导出/清理关闭。
- 每轮按既有metrics_interval（默认1m）安排导出/扫描与快照，不新增轮询配置键；账本NextAttemptAt仍按1s–5m退避，实际尝试还受轮询频率约束。告警/清理各按独立既有间隔，组件构造后复用；单后台循环每项有1m取消/执行预算，异常只记固定分类，不使业务失败。
- 新HTTP生命周期反例`.loop/obs-t021-web-lifecycle-red.jsonl`的start_failure有效红：启动失败虽关监听，却未调用worker.Close（实际0，期望1）。cancel/close_failure分支已绿。修复假设：启动失败同样等待worker.Close并合并释放错误；不改Server其他停机/在途请求语义，随后新用例及既有停机回归重验。

### R039 T021中途独立review定位（2026-11-01）

- HTTP生命周期修复及原停机回归`.loop/obs-t021-web-lifecycle-green.jsonl`PASS；正式same collector/原配置header边界`.loop/obs-t021-production-ports.jsonl`race PASS。
- 独立review确认两项MEDIUM：W1配置bool指针按值复制仍别名共享，可无重启改变告警开关/产生race；W2Collector与OBS用注入Clock，但PG窗口仍db.clock（正常Open=time.Now），固定注入时会拿错窗口后标错采集时间。新`.loop/obs-wiring-review-red.jsonl`W1有效红；W2夹具先被现有trigger规范拒绝（web不接受显式user），尚非有效产品反例。
- W2夹具改真实CLI渠道且由仓储推导user trigger，不改窗口/计数断言。修复假设：W1构造时复制两bool到独立启动快照；W2仅给ObservabilityStore派生账本/窗口增加显式时钟绑定（同DB，不改变业务DB时钟），生产在collector创建前统一绑定，独立NewObservability也给其默认collector绑定相同时钟。先取得W2有效红，再改产品。

### R040 时间窗口/配置快照修复（2026-11-01）

- `.loop/obs-wiring-clock-red.jsonl`已获得有效W2红：真实Run在注入2023时间窗内，快照标2023但未读到该Run（实际PG使用当前2026窗口）。独立复核最终仅W1/W2两项MEDIUM、无CRITICAL/HIGH；其余指定生命周期/凭据无新确定问题。
- 将Snapshot实际CollectedAt通过可选窄WindowCountsAt端口交PG，保留原WindowCounts签名兼容；SQL同一轮聚合使用该固定时点，既修注入差异也不在存储层重新取一个time.Now。ObservabilityStore.WithClock只返回同DB的不可变观测适配副本，账本创建/年龄/清理时钟同源，不改业务DB.clock或业务审计时间。
- NewObservability复制两个bool指针；生产collector之前绑定Store时钟，再将同collector与相同显式Clock交各组件。下一步W1/W2、原组件及正式装配/计数联合race复验。

### R041 T021复核修复通过，联合门禁准备（2026-11-01）

- `.loop/obs-wiring-review-green.jsonl`W1/W2及原指标/清理/账本/正式接线联合race PASS，无SKIP；新逐次进钟测试MetricWindowUsesCollectedAt通过：clock仅构造+采样各取一次，窗口端口收到实际CollectedAt，不重新取时钟。独立复核已请求小改动核对。
- 启用清理时装配保留同时核对既有diagnostics.retention_days（当前可配置历史窗口），LoadConfig与重路径写入前都拒绝不足值；默认关闭时不误拒旧窗口。无其它已配置业务历史天数，未伪造新配置键。
- 新obs-wiring二十必跑名覆盖配置/生产collector/同runCtx生命周期/组件复核/既有重路径次序与释放。ExportConfigRedacted放在真实配置所属cli测试，obs-unit只增加cli包（保留全部原必跑名和断言）；不将配置DTO搬进领域包，也不删除原测试要求。
- 下一步wiring/health/otlp/cleanup联合验收，之后T022七管理只读端点及脱敏/契约/缺口。T021未取得联合报告前不勾选；全量公共门禁/管理台/六场景/产品Collector/回交仍未执行。

### R042 T021联合验收通过（2026-11-01）

- `.loop/runs/accept-20261101T153638.900594400/report.json` fmt/obs-wiring20/obs-health/obs-otlp12/obs-cleanup24全部PASS，无SKIP；T021勾选。独立只读复核确认L1/L2关闭。
- 进入T022：管理读取与后台生命周期分开，GET不评估或写告警；七端点admin-only，空证据与读取失败分开，补来源重脱敏和“其他”聚合。

### R043 T022读取/HTTP/契约实现与反例（2026-11-01）

- `.loop/obs-api-redaction-red.jsonl`复现：允许attrs键中的敏感错误值、Run/执行错误值可漏出，Top-N只丢弃组而无“其他”。修复为Projector再次收窄原因和值，导出/本地使用同一敏感值规则；其他组合真实求和并保留有界下钻列表。原有Top-N列表长度与排序不变。
- 首次试给DiagnosticEvent加HiddenFields被旧TestDiagnostics_ContractFields有效拒绝（`.loop/obs-api-redaction-green.jsonl`）；未改旧字段判据。移除该字段，新增OBS专用DiagnosticReadEvent与可选窄读取端口，Reader先脱敏并只传隐藏数量，Projector再脱敏，不双计，旧v0结构和API不变。测试夹具改走该真实新端口；原敏感值/隐藏数判据保留。
- 七路径保护请求测试`.loop/obs-api-http-red.jsonl`：新handler stub有效红（callerKey拒绝已绿）；实现后`.loop/obs-api-http-domain-green2.jsonl`整web/domain/diagnostics race PASS。管理认证、参数、重复/未知/空参数、6锚点、空/错/未装配/关闭分别表达；GET不调用Evaluate/清理/业务执行。
- 受影响既有RunDetail全字段契约夹具未填新增可选request_id而红；只给完整记录夹具补真实request_id、并新增历史未记录为空断言，不改变字段集合/schema验证。历史仍省略，不补造。`.loop/obs-api-contract-first.jsonl`七GET/管理security/七结果schema全部PASS；原路径与写面未改。
- `.loop/obs-api-status-red.jsonl`复现永久失败/丢弃/过期状态漏计；补全持久计数和八来源可空覆盖、未知确认计数/单位。接口仅显示endpoint/header配置有无，不回显值。
- 真实PG→生产Reader/Projector→真实管理认证/HTTP测试首轮缺Ingress装配，属于夹具错误；补与生产同一newIngressLimiter后通过。原正文/错误/配置canary断言未改变。初始绿`.loop/obs-api-pg-second.jsonl`。

### R044 T022独立复核闭环与全来源读取（2026-11-01）

- 独立只读复核确认两项MEDIUM：C1管理清理历史误用被清理数据窗口，且最老10001条前缀后倒排会丢最新；C2历史批次/清理自由error_kind未经读侧收窄。无其他确定C/H/M。
- C1 `.loop/obs-api-cleanup-history-red.jsonl`复现“今天清理7天前数据”漏行；latest子例先SQL参数类型冲突，不冒充有效红。保持原判据给fixture显式bigint后，`.loop/obs-api-history-canary-red.jsonl`最新10002条反例通过新实现。新增CleanupHistory按started_at DESC/cleanup_id DESC取limit+1，app不再重排；0006新增执行时间索引。旧CleanupRuns保持数据窗口交集供缺口证明。
- C2 `.loop/obs-api-history-canaries-red2.jsonl`export/cleanup各自子例在真实PG写合成敏感历史error_kind并有效红；固定分类收窄后`.loop/obs-api-read-review-green.jsonl`联合race PASS。两种清理读取都收窄；导出读取复用writer同一分类集合，未知为unclassified，不宣称传输失败证据。独立复核确认两项静态关闭。
- `.loop/obs-api-all-sources-canary-green.jsonl`真实PG+HTTP race PASS：Run身份/原因、消息正文、LLM/工具原错、操作详情、任务正文/原错、文档标题路径/原文、摄取/Embedding错误、装配候选标题/正文/无权身份、未知诊断属性/敏感值/URL与导出配置均无canary外发；合法行确实可见。全部GET前后业务/证据/OBS整行指纹一致；告警关闭unknown/时间null、配置仅有无标志。
- obs-unit原62必跑名现全部存在；只追加APIContract/CleanupHistory与PG终态计数名，不删除历史判据。T022联合门禁扩为fmt/obs-unit/obs-pg/obs-web；报告未取得前不勾选。

### R045 T022联合门禁暴露启动取消窗口（2026-11-02）

- harness拒绝把项目fmt写进任务check映射；移出任务映射后，fmt仍由原项目门禁自动运行，未删除。首正式报告`.loop/runs/accept-20261101T155913.559371200/report.json`：fmt/PG PASS；CLI文档测试因OpenAPI生成器在Windows写CRLF导致固定LF路径文本匹配失败，实际旧路径均存在。恢复仓库LF，生成器显式newline=LF，不改断言；第二报告`.loop/runs/accept-20261101T160021.944075900/report.json` fmt/obs-unit/obs-pg PASS。
- 两轮obs-web同一取消反例红：worker.Start通知后立即取消，Shutdown可能先于Serve登记监听器；Server.Run已返回但端口重绑失败。定位为真实资源退出窗口，不是等待不足，原有释放断言保持。Server.Run在取得监听器后即登记最终Close（忽略net.ErrClosed，其他释放错误合并），所有退出路径释放调用方监听器。
- `.loop/obs-api-listener-race-green.jsonl`原ServerOwnsLifecycle三分支race重复100轮全部PASS，无SKIP、未增加睡眠或放宽时间；下一步T022全组重验，未勾选。

### R046 T022联合验收完成（2026-11-02）

- `.loop/runs/accept-20261101T160145.034812600/report.json` fmt/obs-unit/obs-pg/obs-web全部PASS、无SKIP，T022勾选。独立复核亦确认监听器兜底关闭无新的确定C/H/M；仍先等待OBS退出，再交调用方释放共享数据库。
- T023继续既有Vue管理台与客户端，不引入前端依赖或写OBS入口；总览/组/Run诊断/查询/指标/告警/导出/清理遵循模块卡空缺错拒绝文案，技术字段放折叠区。

### R047 T023控制台实现与自查（2026-11-02）

- 首次npm test未启动（未安装依赖）；npm ci按既有锁文件安装，未增加依赖，package-lock前后SHA256均`dd9afd3e4df41e8d8dcacca94f70725c033b019d3be3b08d559d75da0494e35c`。PowerShell合并原生stderr会把npm info当ErrorAction Stop；改Git Bash仅负责日志重定向，不改变命令/退出码。
- 首夹具没有给登录GET /info成功载荷，认证未成立；补真实登录校验成功载荷、断言导航已显示，并直接next到无锚点查询页。`.loop/obs-console-red3.log`九个已登录UI场景对空壳组件全部有效红；`.loop/obs-console-first.log`九个PASS。客户端仅七GET；管理台新增八页签、运行详情深链，查询串刷新/分享/导航独立组件key。
- 自查`.loop/obs-console-self-review-red.log`两个有效红：缺装配记录却无条件写“已装配”；未提交锚点编辑仍留旧结果且刷新可能混新context/旧timeline。修复为只对有真实装配段的轮次表述已装配、其余未记录；读取锚点快照固定，编辑立即清空证据并禁刷新。晚到旧锚点成功响应不能覆盖新页。`.loop/obs-console-self-review-green.log`十二场景通过。
- 类型检查首次pairs的VNodeChild直传h重载失败，改数组children而非强制类型掩盖；后续通过。使用项目既有TypeScript格式器整理新增视图，不加格式化依赖。沿用既有表格、definitions、filters、tabs与文字转义，不插HTML/原始JSON/业务操作按钮。

### R048 T023独立复核四类修复（2026-11-02）

- 独立复核确定F1–F4四组MEDIUM，无C/H：检索permission_denied误作工具权限拒绝；指标无样本/未知告警/空批次的缺失与0混淆；旧403副作用绕过loader代际；启动无合法关联锚点的index_switch误宣称生产覆盖。
- F1 `.loop/obs-console-review-filter-red.log`真实形状retrieval与permission_decision同码反例；新增observation-copy.ts两页同源显示“可见性过滤（原因未细分）”与混合计数/单位边界，工具拒绝保持。旧诊断页同类反例亦红后修复。
- F2 `.loop/obs-console-review-red2.log`区分no_data/not_recorded/not_supported/query_failed/真实0；`.loop/obs-console-coverage-legacy-red.log`未知告警值占位0不显示为真实读数；`.loop/obs-console-empty-ledger-red.log`空账本不冒充0条批次。修复后未知读数“未确认”、无批次明确说明，确认未知NULL保持未确认，不补0。
- F3 `.loop/obs-console-stale-denial-red.log`两反例：旧403晚于新刷新200、编辑后旧403清空新草稿。readEpoch在刷新/编辑/卸载失效，403副作用须匹配当前epoch；`.loop/obs-console-stale-denial-green.log`PASS。保留跨导航晚到成功响应反例。
- F4核对正式wire_knowledge.go只给空correlation，启动代际切换确实不发事件；撤回本次误加的生产覆盖标记，保留无生产端说明，不补假锚点。ingest_stage有真实装配，旧覆盖测试据实由“暂无生产端”改为已覆盖，并加index_switch独立反例；不改变枚举/写入规则。
- 补锚点的时间/来源/1–1000条筛选，API保留后端展示号（S7不重编号）；编辑清空旧证据，非法条件不发请求。`.loop/obs-console-all-review-green.log`两页27项PASS。独立复核静态确认F1–F4全关闭，无新的确定C/H/M。
- `.loop/obs-console-all-final.log`31文件374项PASS；构建首先被新增测试findLast超出项目现有lib目标拒绝，等价reverse().find保留断言/不改目标，`.loop/obs-console-build-green.log`typecheck+Vite+integrity通过。下一步T023正式门禁；未通过前不勾选。

### R049 T023正式验收完成（2026-11-02）

- `.loop/runs/accept-20261101T161946.888891400/report.json` fmt/obs-console-test/obs-console-build全PASS、无SKIP；生成dist及integrity，T023勾选。前端新增18个OBS场景及2个旧诊断场景（两页总27项），全前端31文件374项通过。
- T024的六个独立场景将放在真实装配/准入/生产者/PG可组合的app集成层；领域包不能反向依赖app。原obs-scenarios保留领域包参数并追加app，不删除六个必跑名；不套壳调用旧测试函数。

### R050 T024六场景实际入口与逆改（2026-11-02）

- 共享Run夹具仅新增显式Observability开关；既有默认不变。开启时同一真实Recorder/Collector接模型装饰器、工具、准入、配额和知识检索；仍只替外部协议，不替Run/审计/授权/SQL检索逻辑。暴露既有registry/agents用于真实HTTP读取；OBS场景检索时钟与运行时同用time.Now，避免旧固定夹具时刻先于Run的假顺序。
- 首四场景`.loop/obs-scenarios-four-first.jsonl`超时/空检索已绿，正常HTTP因Key未给Agent调用scope被真实403拒绝，工具拒绝夹具用了根本未注册名称（实际应为not_found，不是权限判定）。修正夹具为真实所需scope、全局注册但Agent未声明；原拒绝/无审计/无执行断言不变，未改产品鉴权或分支。`.loop/obs-scenarios-four-second.jsonl`四场景race PASS。正常场景经真实HTTP认证/入口限流/准入/循环/知识/工具/PG：服务端请求ID贯穿，客户端伪造头未采用，3装配/2模型/1工具，无缺口；不把同毫秒展示次序当因果。
- `.loop/obs-scenario-fixture-regression.jsonl`原混合过滤/正式时钟/API canary回归PASS。新增PermissionAndBudget沿真实检索造2拒集合+3拒行、3允许候选，实测filtered_count=5、kept/budget_drop/rank_cut分开；20次真实检索permission_denied不计工具拒绝指标/规则。`.loop/obs-scenario-permission-first.jsonl`PASS。
- `.loop/obs-scenario-export-first.jsonl`实际PG扫描→Mapper→产品OTLP sender→自有回环HTTP五子例PASS：A503后重试200；B三项全unconfirmed、accepted2/rejected1、不自动重试；C普通成功；D部分响应后确认事务故障；E普通成功响应后确认事务故障。D/E首次unknown/计数NULL/三项inflight，租约到期第二次发送相同稳定span IDs、attempts=2/批次2；第二次结果按实响应，历史unknown尝试仍1。业务/审计/证据整行指纹不变，协议模型调用0。
- `.loop/obs-scenarios-six-baseline.jsonl`六独立场景及各子例race PASS。已有能力首跑为绿，不伪称它们先发现产品缺陷；独立有效红由下述受控逆改取得。
- `.loop/obs-scenario-mutations/report.json`六项valid_behavior_red=true：丢request_id、过滤计数手填1、超时不写诊断、拒绝事件误分类、空检索不写装配、部分成功误标sent。每次仅一个已定位产品错误，真实测试断言红而非编译/panic；脚本finally逐字恢复并校验原/恢复SHA256，六项全部恢复。各.before/红态日志/哈希在同目录；下一步正式原树全场景重验。
- 场景5同时补UI反例`.loop/obs-scenario-empty-copy-red.log`，已记录hits=[]原先缺“知识库无结果”明确文案；补该真实空结果说明，不伪造no_answer，`.loop/obs-scenario-empty-copy-green.log`通过。T024正式门禁一并重跑前端测试/构建以更新dist。

### R051 T024稳定产物重验（2026-11-02）

- `.loop/runs/accept-20261101T163559.630397900/report.json` fmt/六场景/生产者回归/前端测试/构建各自exit0，但整轮source_changed/exit2，**不算有效通过**。本轮新增场景5文案后未先构建dist，任务内构建更新了受源指纹保护的嵌入JS/index/integrity；不是验收期间人工改产品。该报告与失败状态保留。
- dist已由该构建生成；保持当前源码/产物稳定，下一轮完整重验相同checks。不放宽source_changed检查、不删构建、不借各项exit0勾选任务。

### R052 T024正式验收与独立复核完成（2026-11-02）

- `.loop/runs/accept-20261101T163711.669103500/report.json`稳定树fmt/六场景/生产者回归/前端测试/构建全PASS、无SKIP，T024勾选。独立只读复核确认非套壳、真实入口与生产端/PG、十表指纹；六逆改均为行为红且源码SHA与恢复记录一致，未发现确定C/H/M。
- 证据局限明确：SC6主要证明trace/HTTP JSON，不替代T026真实Collector三信号；既有embedding审计/配额假件不证明相应外部能力；无真实模型调用。T025继续容量/运维/语义文档，T026仍须产品三信号实接Collector，不借spike/场景读数替代。

### R053 T025运维、默认配置与隔离容量完成（2026-11-02）

- 技术方案§5.6.1追加US-16A事实投影语义；Operations§5.11追加八项升级影响、缺失/错误/覆盖/导出/清理排障、容量步骤和局限；示例配置导出/清理默认关闭。旧Session诊断实际经Run关联，覆盖文案与新增反例据实修正（`.loop/obs-doc-session-coverage-red.log`、`-green.log`）；index_switch启动无锚点仍不生产。
- `TestObservabilityStore_CapacityDrill`在独立PG17.11 schema、Go1.27.3 Windows amd64/race下种1000 Run、1000 LLM、各20000诊断/装配记录；真实Cleaner以40批删除40000行，保留Run1000。最终`.loop/obs-capacity-final.jsonl` PASS；提取`.loop/obs-capacity-measurement.json` SHA256 `b07e5cb22b4a4be4aff67de03a29ae0ad435739b8a4c33923bbb08b553bd87b5`，测试源SHA256 `850ffcb1bb232127334987e0639f9cc2360b4a102c14d3d52dc6b54bfa1f3047`。清理11284.4139ms；前/后timeline p95 5.5451/5.7480ms、context 3.2180/3.7908ms、metrics 25.2458/16.3976ms；无全部查询加速或生产容量承诺。普通VACUUM仅两张自有测试表，未测生产增长/RSS/并发/大正文/稳态导出吞吐。
- 实际默认示例测试首先RED（`.loop/obs-doc-example-config.jsonl`）：显式headers ENV引用不存在，原校验按要求拒绝。将可选headers示例改注释、endpoint默认空串；未放宽凭据解析。`.loop/obs-doc-example-config-green.jsonl` PASS。第一次正式报告`.loop/runs/accept-20261101T165306.696615800/report.json`仅obs-unit失败：公开Operations误链私有TechnicalDesign；删除该链接、保留公开OpenAPI，历史守卫不改。
- 正式报告`.loop/runs/accept-20261101T165433.509643900/report.json` fmt/obs-unit/obs-pg/obs-web/console测试/构建全部PASS、无SKIP，T025勾选。独立复核指出partial_unconfirmed还包含协议非法/矛盾，文案不能一概声称实际拒绝；现按error_kind区分合法部分拒绝与未知NULL。修正文案后`.loop/obs-docs-final.jsonl`公开文档/默认配置守卫PASS，三份文档markdownlint exit0。T026继续产品→官方Collector三信号与测试后端Gauge语义。

### R054 T026真实Collector与测试后端完成（2026-11-02）

- 独立标签`integration && obscollector`的`TestCollector_US16AProductThreeSignals`要求显式固定版本二进制/自有证据目录，缺失即失败、不SKIP；二进制SHA固定`20aa55b613207c37f6b65df555b16001ec3f965d7218bf619742cb8579f0daa1`（官方0.164.0）。实际NewObservability→PG源行/扫描/账本→Mapper→产品sender→官方Collector→typed protobuf回环测试后端，两种编码各自trace3/log3/metric多点、HTTP200/合法确认独立记录。debug与转发结果均无CANARY；Run/派生LLM/工具结束时刻、恢复未知log不伪造span、ID/父关系/状态/属性均明确核对。
- 首次测试文件有缺闭合括号的编译失败，`.loop/obs-product-collector-first.jsonl`保留，不算有效红；修正后second/final通过。独立复核指出累计Histogram及log字段断言不足，随后加强非零25/75ms样本、五结果序列的cumulative/ms/精确起点/时间/边界/非累计桶/count/sum，以及日志时间/severity/事件ID/typed属性。`.loop/obs-product-collector-strengthened.jsonl` PASS。
- `.loop/obs-collector-histogram-mutation/report.json`仅删产品累计Histogram映射时，两协议因“process histogram must survive the real Collector pipeline”有效行为红；源码finally逐字恢复，原/恢复SHA均`36bbea11b1a9b9d96b0bf51a02390e3f2c3ac98bb02934b55d4debebb3fe3987`。测试后端按(name,collected_at,attributes)幂等覆盖：原点重送不增点，两次重叠窗口各保留值1，进程counter由1重启为0并推进起点，Histogram由2归0。
- 阶段接线两次预检拒绝（`.loop`不能protect、wrapper不是直接go test -json/-count=1），均未运行测试/未生成正式报告；按既有Harness要求改为直接go test，运行时仅注入二进制与新证据目录ENV，不改判卷规则。早期正式`.loop/runs/accept-20261101T165949.909225900/report.json`是加强前通过，保留。最终加强后`.loop/runs/accept-20261101T170213.950321700/report.json` fmt/obs-unit/obs-collector全PASS、无SKIP，绑定实际Go测试二进制SHA（不是发布候选）；调试日志、五组typed输出、测试二进制在日志指向的`.loop/obs-product-collector-stage-*`保存。T026勾选；真实外部后端层4仍可选/未运行，无真实模型或外部观测流量。

### R055 T027一致性分析、独立复核与修复（2026-11-02）

- 冻结module/spec/plan/tasks正文归一SHA未变；`.loop/obs-final-analyze.json` SHA256 `7701e6ef828a7c00c9dae149e0a46b1f9b376dc0e8987d41895ed741831d3769`逐项映射34 FR+6 SC，40/40、28任务无孤项。独立只读复核原输入/diff/消费侧/证据，五类高风险（鉴权/敏感外发、账本未知确认/重试、覆盖/清理、真实生产端/关联/缺失、生命周期/业务语义）最终C0/H0/未关闭M0；机器通过不作人工②。
- 七组发现及处置：Collector Histogram/log断言见R054；本地attrs补模型/结果码、Run轮次/工具数、LLM tokens与恢复时刻，UI区分0/缺失且恢复仅观测；Export新增nullable retry_attempts/next_retry_at/last_http_status，明确当前retry项再次认领合计、非网络重发证明；退避改crypto/rand；历史诊断kind/reason/decision用领域Valid收窄、保留旧行；Other合并后按真实StartedAt+ID排序再截20；retrieval既有evidence映射增assembly direct/insufficient，仅真实装配空hits说无结果。没有扩大Mapper外发allowlist。
- 有效行为红：`.loop/obs-review-read-red.jsonl`模型丢失/历史kind透出；`obs-review-retry-red.jsonl`缺nullable字段；`obs-review-retry-ui-red.log`缺技术详情；`obs-review-overview-red.jsonl`漏最新溢出Run；`obs-review-presence-red.jsonl`恢复/装配presence缺失；`obs-review-presence-ui-red.log`fallback伪称空结果。新实际v5已发布SQL1–5→CANARY kind/reason/decision→生产assemble升v6→真实admin HTTP反例`obs-review-legacy-api-red.jsonl`确见自由decision透出，修复后保留旧DB原值、API占位/隐藏计数为3。
- 定向绿：`.loop/obs-review-domain-green.jsonl`、`obs-review-retry-green.jsonl`、`obs-review-legacy-api-green.jsonl`、`obs-review-ui-green.log`、`obs-review-ui-metadata-green.log`与prebuild均PASS。正式`.loop/runs/accept-20261101T170950.818062400/report.json` fmt/obs-unit508/obs-pg59/obs-web82/obs-scenarios11具名PASS及前端测试/构建全部通过、无SKIP，T027勾选。新增fixture按实际assembly evidence补真实空结果输入；重试测试表述由“已重发”纠正为再次认领，未放宽业务断言/历史判据。
- 13项静态审计额外发现HEAD既有诊断采样器仍math/rand；已在同一触及文件改crypto/rand的53位均匀采样，可注入roll接口和采样边界不变，熵失败丢诊断、不传播业务错误。`.loop/obs-final-sampler-green.jsonl`完整诊断race测试PASS，最终树须T028再验。静态审计`.loop/obs-final-invariants.json` SHA256 `7b6728870b974bf7aa827109a5d69bd70e12c473cb1d10b74bba90b57eb9a199`通过；单二进制/审计行为/不打开SQLite另依动态门禁。广扫SQLite仍命中六个未改历史迁移测试夹具，生产导入0，报告逐文件单列，未伪称全目录字面0。审计脚本最初Windows解码/引号错误及这两项诊断保留为工具/检查失败，不算产品绿态。

### R056 T028首轮门禁问题与范围内修正（2026-11-02）

- `.loop/obs-make-check-first.log` fmt通过、lint失败：未使用的旧私有newDiagnosticsPorts包装器及Other列表append简化；已删除无调用包装器（保留实际WithMetrics装配）并简化循环。`.loop/obs-make-check-second.log` fmt/lint通过、sec失败，其余目标未跑。未改变工具、阈值、跳过或历史判据。
- 独立补充复核指出Go1.27.3的crypto/rand.Read遇Reader错误会fatal，原“返回error”分支不成立；改sampleFractionFrom(io.Reader)用io.ReadFull，正常错误/短读丢诊断，底层自身fatal不声称可恢复。新增`TestRateSampler_EntropyFailureDropsDiagnostic`用ErrReader/短读/两边界，不改全局Reader；`.loop/obs-final-sampler-error-green.jsonl` PASS。独立复核关闭此M，recorder源SHA `7df28004022f60b0f9f28c35de619c8b6a4b1cad1804085ee9c58049fc1c9ff8`。
- gosec的两处G115改为FormatUint→ParseInt拒绝超int64，再按int可表示边界检查，无截断；`.loop/obs-final-config-overflow-green.jsonl` PASS。两处G202为固定八来源/四表switch的SQL片段、值全绑定，依既有diagnostic_store同一模式加精确#nosec与中英依据；`.loop/obs-final-sql-boundary-green.jsonl`真实PG恶意kind/table/id/cursor反例PASS，源行不变。独立复核确认无外部SQL片段、无新C/H/M。两处窄例外属于判据面说明，不能称“零变更”。
- `.loop/obs-make-sec-fixed.log` gosec通过，但govulncheck发现x/net v0.59.0五个可达漏洞GO-2026-6617/6612/6611/6610/6603，均指向v0.60.0修复。核对[官方Go公告](https://pkg.go.dev/vuln/GO-2026-6617)与[固定版本](https://pkg.go.dev/golang.org/x/net@v0.60.0)，按已授权依赖裁决仅升级x/net至v0.60.0并tidy，未改toolchain。`.loop/obs-make-sec-patched.log` exit0、影响代码漏洞0；另有2个require模块的不可达漏洞提示，不写成所有依赖漏洞零。最终132模块坐标/和/许可证/依赖树另存`.loop/obs-final-dependencies.json`、`obs-final-module-graph.txt`、`obs-final-licenses/`，不覆盖原spike读数。
- US-16A阶段追加采样故障具名检查，以及十项受影响US-16检查的派生副本，原required_tests保持；旧argv未带integration的只追加加载面。历史stage/记录未改。quickstart补实际Collector ENV与授权参数，外部后端/真实模型仍不运行。最终稳定树完整门禁继续；当前原生8080存在演示监听，后续e2e不得停止/修改该进程。

### R057 T028已批迁移与旧谱系判据同步（2026-11-02）

- `.loop/obs-make-check-third.log` fmt/lint/sec/全仓race单元通过，test-integration失败，仅app两条旧数值断言：`TestStorageStore_OpenReadOnlyPostgres`期望52、实际58；`TestApp_SupportedVersionInjectedPerBackend`期望PG5、实际6。PG包175.029s通过；期间只读测试库锁状态见迁移锁排队，未超时/强制解锁，不能把等待误报为死锁。cover/build未到达。
- 已审O-06/0006要求PG5→6，SQLite源谱系仍6，原“数值必须不等”前提失效。独立复核裁定按后端分别注入才是原语义，必要适配无需新增人审：只读盘点保留legacy52，并加精确六OBS集合与总58；实际init→SQLite、migrate→PG、status链路全部保留；两个无效NotEqual替换为SQLite无六OBS/PG有六OBS，数字分别按权威当前值检查。新增AST只定位实际cli.Deps复合字面量，按import路径验证两个版本字段分别来自各自后端，各恰一，防止数值同6掩盖串用。此为判据适配，不写“判据面零变更”。
- `.loop/obs-final-lineage-compat-green.jsonl`三测试race PASS；`.loop/obs-lineage-mutation/report.json` runtime→storage有效架构红，`.loop/obs-lineage-source-mutation-v2/report.json` source→postgres有效架构红，两者原/恢复run.go SHA均`2847229f9088a01fb0587deab83b1db3a7f493df0c593efb61560ad84ba7c80c`。source首轮unused import编译红的false报告保留，不计有效；v2仅加无行为引用使错注入能到断言，不改目标错误。阶段另追加三条谱系具名检查，历史定义不改。
- 原生8080演示PID15180保持；WSL Linux Go1.27.3的独立NAT回环8080可bind，后续完整make e2e可在该隔离网络运行，原生候选另跑不占默认端口的受影响e2e。两种候选SHA分别记录，不把Linux进程证据说成Windows全套已验；不改默认端口断言、不跳测试、不停止演示。当前第四轮make check继续。

### R058 T028本机资源限峰重验（2026-11-02）

- `.loop/obs-make-check-fourth.log` fmt/lint/sec/单元通过，integration仅`TestWire_CredentialComponentsAssembled`连接PG时Windows bind报WSAENOBUFS（缓冲不足或队列满），此前两条schema/谱系断言已通过；PG178.405s通过。故整轮仍失败，cover/build未到达，未冒称绿态。
- 故障后本机观测TimeWait18/Bound119、动态TCP端口范围49152起16384个，不能证明故障瞬间端口已满；根因仅确认资源耗尽类别，具体端口/缓冲/内存尚待确认。无OS参数修改、无进程/服务重启、无强制连接清理。后续只在验收子进程设`GOFLAGS=-p=1`降低包级同时运行峰值，包内并发/race/用例/阈值/超时不变；该环境项写入最终验证manifest。第五轮make check继续。

### R059 T028私有走查工具纳入扫描（2026-11-02）

- 第五轮`.loop/obs-make-check-fifth.log` fmt/lint通过，sec新增G701来自私有`.loop/obs-review-seed.go`，其余目标未到达。gosec仍扫描该私有Go文件，未移走文件或排除扫描。该文件只准备独立回环走查的合成事实，schema必须匹配固定前缀+12小写hex，再用pgx.Identifier.Sanitize引用；新增精确G701说明，生产两处G202不变。`.loop/obs-review-schema-validation-v2.json`用有效继承测试DSN验证4恶意schema全部拒绝、固定错误文案，无schema创建；第一版空DSN负例辨别力不足，保留而不单独作安全证明。
- `.loop/obs-make-sec-with-review.log` exit0、影响代码漏洞0，仍保留2个require模块不可达提示。原GOFLAGS为空已实测；`-p=1`仅子进程限峰，经独立复核不改变包内并行、race、用例与阈值。私有走查脚本的独立复核继续，正式启动等机器门禁结束；第六轮make check继续。

### R060 T028走查夹具完整性与正向脱敏加强（2026-11-02）

- 独立复核私有工具发现三项M：候选键形状错误、启动后异常遗留进程、只凭首Run存在就误判半套夹具完成。现候选用真实snake_case；完整集合校验3 Run/2 LLM/1工具/1装配/2诊断，再经真实Projector.Context要求唯一允许候选/hit且无CANARY；已有首Run也须校验。启动后的readiness/seed/API/metadata全在catch范围，按PID+路径+startTime核对本轮进程后清理自有树，失败不写preview ready；故障参数必须带独立TestCase。独立静态复核三M关闭，最终候选HTTP与启动故障证据仍待补。
- `.loop/obs-review-fixture-validation.json`两个新自有schema的退出序列为[0,1,1,0,0,0]：受控只写首Run后中断，再次seed明确失败；另一完整seed及重跑均通过、允许候选与命中真实存在。两个临时schema已精确自有清理exit0，保留验证与cleanup日志。
- 同一键形状问题也用于加强公开`TestObservability_APINoCanary`：原SQL夹具PascalCase导致允许候选也被空掉，旧通过不能证明该候选实际可见。保留全部原canary/GET无写断言，先追加真实允许候选/hit正向要求，`.loop/obs-final-canary-positive-red.jsonl`有效红为候选[]；只将fixture键改真实snake_case后，`obs-final-canary-positive-green.jsonl`全部PASS。未改产品读取或放宽断言。该测试增强发生在第六轮app integration已结束后、cover前；后续公共full验收将对最终稳定树整体再验，不能以先前app结果代替新断言。

### R061 T028七目标通过与最终稳定树验证启动（2026-11-02）

- `.loop/obs-make-check-sixth.log`整条`make check` exit0：fmt/lint/sec/test/test-integration/cover/build七目标均通过；原生Windows Go1.27.3，子进程`GOFLAGS=-p=1`。最终cover含加强后的正向canary断言，app75.2%、observability82.7%、postgres79.1%，全部包达既有阈值；无可执行语句包按原coverage规则不适用，未添加Skip或阈值豁免。构建为原生`bin/basalt.exe`，CGO0/trimpath/去符号/当前HEAD版本标记。
- 为对最终稳定源码及私有准备工具整体重新绑定，启动`.loop/obs-final-gates.py`，manifest `.loop/obs-final-validation-20261101T180453154402Z/manifest.json`；独立CGO0 `go build ./...`已exit0，公共make acceptance正在执行。其后完整US-16A stage-only（全部37检查、含十项US16回归/官方Collector）、原生候选受影响e2e、WSL独立NAT完整make e2e顺序执行，任一步失败即保留并停止后续依赖。实际有效GOFLAGS写manifest，未跑项仍未验证；T028暂不勾选。

### R062 T028证据归档与收口工具准备（2026-11-02）

- 独立只读复核核对最终范围、七目标日志、实际37阶段检查及原生/WSL制品传递，未发现新确定C/H/M；运行仍未结束，未冒称通过。独立构建确实由runner显式注入`CGO_ENABLED=0`；Windows受影响e2e、Linux完整e2e、Collector Go测试程序和官方Collector分别记录SHA，不合并平台结论。
- 固定版本spike的36份源码、模块树、许可、回环结果已按原字节归档`.loop/obs-spike-evidence-archive/spike-source-and-evidence.zip`，33041 bytes，SHA256 `c6fc010920394dd087b7a35da69aae95341fcb31bad95881b0f61a66c4ef6d21`；逐文件清单manifest SHA256 `1a4de4c8e1c23bab499d9414209441a657a0ef2c5fd9c8d76827c560adff4c96`。排除可执行文件/下载包，原临时目录保留供Collector复验；Go源只在zip内，不新增活动扫描源码。
- 新增私有`.loop/obs-review-startup-faults.ps1`并解析语法通过，准备实际验证AfterStart/PartialSeed两故障窗口、独立PID消失与不留ready元数据、8080原进程不变；尚未执行。`.loop/obs-validate-final-evidence.py`准备在完成后核对日志SHA、两报告全部检查、共同源码指纹、两候选实际marker与逐名通过/跳过。十二节回交包模板仅私有草稿，最终报告/HTTP/浏览器实测后才生成正式回交包。

### R063 T028公共最终树验收通过与交接预复核（2026-11-02）

- `.loop/runs/accept-20261101T180502.646785600/report.json` 公共full完整11/11检查PASS、exit0；源码指纹`548da13c0cb456eb746f3cdaf63fb35d181622f1cedf811217a43705901075c1`。行为评测为5通过/0失败/1跳过，唯一`outcome-real-model-answer`缺显式provider门控不发出；注入评测4通过/0失败/0跳过。报告Go具名skipped_tests为空不代表评测无跳过，最终交付分开列明。当前完整37项stage正在执行，之后两平台e2e仍待验证。
- 最终源码13项不变量另存`.loop/obs-t028-invariants.json`，静态PASS、SHA256 `7b6728870b974bf7aa827109a5d69bd70e12c473cb1d10b74bba90b57eb9a199`；与前次读数相同。动态CGO0构建已PASS，审计/SQLite运行边界由本轮full及stage具名测试另证；六个既有SQLite测试夹具例外清楚列明。
- 独立只读交接预复核指出并修正四类私有准备问题：timeline不存在Run应200空entries/not_recorded，context不存在Run才404、缺anchor400；旧通过工件不得混入新候选，render必须绑定Windows候选SHA与HTTP/preview/browser/faults及PID/startTime/schema；browser failed/skipped/not_run不得转成未验证；故障schema只有精确SQL执行exit0且查pg_namespace remaining=0后才可声称已清理，原8080 PID从实际before/after取。旧52表只称计数，六新表另精确名单。均未改产品或测试判据，实际HTTP/故障/浏览器仍未执行。
- 台账仅US16A行同步已选C与正式依赖版本及review②待追认，未登记US17。冻结模块卡/spec/plan/tasks正文未改；私有交接模板与控制脚本不计作已交付机器结果。

### R064 T028完整阶段通过与Linux普通用户失败对照（2026-11-02）

- 完整stage `.loop/runs/accept-20261101T183106.946351700/report.json` 37/37检查PASS，源码指纹与公共full均为`548da13c0cb456eb746f3cdaf63fb35d181622f1cedf811217a43705901075c1`；obs-unit509、obs-pg60、obs-web83、六场景11、Collector3具名含子例通过。stage内十次Go测试SKIP（跨check重复）：五个软链接/POSIX权限位测试受Windows条件限制，另一个真实provider测试未打开`BASALT_REAL_CALL=1`。原记录保留，拟在Linux非root补五项，真实模型不调用。
- 原生Windows受影响候选e2e exit0；发布候选37543424 bytes / SHA256 `a176c8b393e89b6b7c969883653775e4fc7b1f47bf0a21d5454d042a5406078f`。Linux普通用户完整`make e2e` exit2（Go test exit1），不是通过；五组Python示例102用例通过，但多个e2e失败。原manifest与日志保持`.loop/obs-final-validation-20261101T180453154402Z/`不改；`initial-candidates.json` SHA256 `d96de4397ca399a81e869476e61a5af380f2db813827be0497127133506384af`核对原始marker后保存失败候选36892832 bytes / `bc1bf2d8752521ee115e2b2cb143570ff775b8dfc0a068f3d72530926a833ad9`，后续候选不得覆盖。
- 明确失败：US14两组五Demo夹具和LegacyD1的11次回环80绑定permission denied；AdminConsole SkillReferenceGate等待引用清空且TempDir只读快照删除权限拒绝；V3 ScopeAndCopyMapping等待clone注册；FullPackage LinuxInstallSmoke缺biz-daily。端口失败根因已由错误证实，三项非端口失败的具体根因尚未确定，不能统称为端口或权限导致。
- 隔离临时detached worktree `d46cc40d952d1e3f60f2474f68df00a3ffa4d965`、同非root UID1000/Go1.27.3/-p1，原HEAD候选28307616 bytes / `572b623f65377858c14e7897b4a18e6e4df8acff48465995f98195e4b87115f0`，上述三非端口测试与LegacyD1全部复现失败，0通过/4失败/0跳过；测试exit1、69.6s。证据`baseline-nonroot/manifest.json`与targeted-e2e.log，**这是基线问题复现，不是通过验收**。独立复核先确认相关e2e/打包源码与索引字节相同，再要求此实际基线对照；不能仅凭测试未改断定非回归。
- 按`docs/DevelopmentLoop.md:1199`已有WSL `-u root`本地测试姿态重跑同一完整make e2e，BIN改用独立`.loop/obs-linux-root-candidate/basalt`，不改源/端口/阈值/系统权限配置；当前正在运行。只可报告Linux/root验证，不能声称修复Linux普通用户问题或普通权限边界已验。新结果写resolved-manifest，不改原失败manifest；T028仍未勾选。

### R065 T028 完整 e2e 红态与兼容性修复（2026-11-02）

- 原 Linux/root 完整 e2e 同样失败并触发 Go 默认 10 分钟超时；原 `resolved-manifest.json` 保持 failed，不能用 root 姿态代替修复。其候选与非 root 候选残留进程经精确 executable/PID/start-ticks 枚举均为零，记录 `.loop/obs-failed-linux-process-cleanup.json`。原非 root 权限失败与独立 HEAD 对照仍保留。
- `internal/e2e/us14_demos_test.go`：D2 既有脚本改显式固定 NOW、两轮独立 output，新增首轮报告保留及 hash 断言；原 followups=3 等业务判据不变。有效红 `.loop/obs-e2e-fixture-artifact-red-root.jsonl`，绿 `obs-e2e-fixture-artifact-green-root.jsonl`。D4 显式 keyword，符合 US16 无 embedding 时禁止默认 semantic 的既有契约，原空记忆/历史/通知断言不变。
- `internal/workspace/watcher.go`：Linux IN_MOVE_SELF 后旧 inode 缓存会漏掉目录替换后的真实文件修改；重建整个 fsnotify generation、先监听既有 agents/skills 根、再按原两层白名单扫描，并对旧/新名称排队刷新。重建失败使用可取消 backoff 持续重试，无须再来一个拓扑事件。真实 WriteFile 红态与两次失败后自动恢复红态分别在 `obs-watcher-swap-red-linux.jsonl`、`obs-watcher-retry-red-linux.jsonl`；最终 Win/Linux 完整 workspace race 绿在 `obs-watcher-final-green-{windows,linux}.jsonl`。中间测试代次识别错误的失败原件保留。
- `internal/app/instance_bootstrap.go`：严格 fresh 分支在 instance roster 前发布既有 SchemaVersion=1 的 incomplete open-box intent；同目录私有临时文件、写入/Sync/Close 检查、Root.Link 原子不覆盖。现有 marker（含损坏/未知）逐字保留，失败不发布 roster。既有 installer 自行恢复 incomplete intent，未扩展全局 Fresh、未修改 installer、未增加公共字段或配置。有效红 `obs-openbox-intent-red.jsonl`；原安装测试与新五项 Win/Linux race 绿 `obs-openbox-intent-green-{windows,linux}.jsonl`。
- `internal/e2e/us14_fullpackage_test.go`：中断首启的再次 serve 依旧 Fresh=false；按既有公开契约先验证 await_init，再经认证 POST confirm=true 初始化 secrets，保留原 Linux 0700/0600、token、锁及 shutdown 断言。第二 serve 使用 h.environ 注入同一隔离 DSN，避免在配置校验先失败而未到锁。`obs-openbox-explicit-init-green-root-v3.jsonl` 为原 LinuxInstallSmoke exit0；这不是自动首启密钥生成 e2e 证明。诊断只记录布尔状态、文件存在和 Agent 名称，不输出凭证。
- 上述兼容性修复在用户全自动授权内落地；台账仍只更新 16A。local-only stage 追加两项恢复回归，原 37 检查、历史 stage、阈值/超时均保留；当前共 39 检查。旧 Windows 候选及启动故障聚合证据先按原字节归档 `.loop/obs-pre-recovery-preservation/manifest.json`，新的完整门禁将绑定新稳定源码与新候选，不借旧绿态结案。

### R066 T028 跨用户恢复复验与新稳定树门禁（2026-11-02）

- `.loop/obs-recovery-e2e/manifest.json`：同一新 Linux 候选依次运行原三项，root 3/3 PASS；普通用户安装、clone 两项 PASS，SkillReferenceGate 的业务断言完成，仅 Go TempDir RemoveAll 删除只读版本快照时 permission denied，故该测试和该轮仍 FAIL，不冒称全绿。完整非 root e2e 未重跑；回环80权限边界未改。此结果细化 R064 的未定位问题，不能将 root 通过当作普通权限部署通过。
- 既有独立只读 reviewer 核对 watcher、roster 前 incomplete intent、日报隔离输出、显式 secrets 初始化及第二 serve 环境，未发现新的确定 C/H/M；原业务断言保留。其要求单列纯 OBS 接线外的生产兼容修复、首启中断/显式初始化范围和各平台局限，已纳入回交模板；完整机器证据尚待完成。
- `.loop/obs-private-final-comparison.json` PASS：模块卡仅原两行编号补充；台账其他行逐字未改；handoff 仅尾部历史接手/用户更正；TechnicalDesign 原 §5.6 两段同步 v1/迁移及诊断关联/脱敏事实，另增 §5.6.1。原 stage 通过移除本轮纯追加部分重建字节，SHA 与追加前记录一致，原37检查保留，现39项。
- 新 runner `.loop/obs-final-gates-v2.py` / `.loop/obs-final-current.json` 指向 `.loop/obs-final-v2-20261101T195449066713Z/manifest.json`，按 Linux/root 完整 make e2e、非 root 五权限/软链检查、原生 make check 七目标、CGO0 build、公共 full、39项 stage、原生受影响候选 e2e 顺序执行。任一步失败即记录后停止；不修改原失败汇总。当前仍运行，T028 未勾选。

### R067 T028 整包预算归因、源码绑定和私有工具故障（2026-11-02）

- v2 Linux/root 完整 `make e2e` exit2，605.828s；原始 Go JSON 只出现包级 fail，明确 `panic: test timed out after 10m0s` / 包600.306s，正在 `TestUS16Demo_OriginInjection/industry-brief`。此前所有已完成具名用例无 fail；既有70s停机耗尽实测约85s，五个固定+25s cron子例各约27s，均继续推进。v2后续六步未运行，源码窗口控制器随失败停止，原manifest保持failed。
- 独立只读复核核对已审OBS各10m check不等于完整e2e包期限；本包没有读取t.Deadline()/test.timeout，局部t.Context仍有原独立预算。按handoff§0.3(3/10)选择开发执行预算适配：Makefile新增 `E2E_TIMEOUT ?= 10m`，本轮明确 `make e2e E2E_TIMEOUT=20m`，不使用0关闭看门狗。原70s上下界、cron25s、HTTP/Run期限、全部断言、选择/count、OBS stage超时不改；不是原默认make e2e通过。此门禁配置适配及回退列review②追认。
- 证据控制独立复核另发现两M：v2首步未记录执行前源指纹、cleanup/partial-proof汇总未强校验四份SQL/log原件。前者不倒填旧指纹，废弃窗口补证的成功路径；新 `.loop/obs-final-gates-v3.py` 对每步实际记录原harness算法的执行前/后同一源码指纹，全部门禁重跑。后者在renderer增加四原件SHA核对、partial exit0与1Run/其余0精确事实，以及HTTP响应文件SHA/bytes校验。
- 新v3 `.loop/obs-final-v3-current.json` 指向 `.loop/obs-final-v3-20261101T200720574065Z/manifest.json`，初始源码指纹 `8dda8db96e6d82835c759af5be7816e3f96f0ceb1a3f0e931643982e6f41d0aa`。独立Linux/root20m全套与原生make check流水线可并行：不同平台产物、隔离schema、WSL独立8080；原生check→CGO0→公共full→39stage→原生候选e2e顺序不变，之后非root五权限/软链补测。新Linux候选独立目录，不覆盖旧候选；当前仍运行，T028不勾选。
- 旧Windows候选a176上实际完成AfterStart/PartialSeed启动故障、独立PID消失、原8080 PID15180不变、部分写入确为1completed Run且后续事实全0；五个精确自有schema实际清理后remaining=0。曾因taskkill尚未回收就立即检查而失败，补PID/路径/startTime核对及5s WaitForExit；另有聚合器误判被Go helper隐藏的原始错误文本，改用phase并以真实SQL事实证明。三次失败原件保留；最终旧聚合、SQL/log及清理结果已归档 `.loop/obs-pre-recovery-preservation/`，不借给新候选。新SHA将在build完毕后重新执行两故障/事实证明/清理，再启正常走查。
- 新旧失败候选残留产品进程均按精确exe/PID/start-ticks枚举，当前v2亦为零，记录 `.loop/obs-failed-v2-linux-process-cleanup.json`；没有广泛pkill、系统参数调整、演示实例重启或未知schema批量清理。

### R068 T028 纠正端口归因并修复真实停止完成屏障（2026-11-02）

- v3 Linux/root `make e2e E2E_TIMEOUT=20m` 实际运行至完成，exit2 / Go692.789s；唯一具名失败为 `TestBackfill_Web/release_order`，断言是 `exec 出错同样释放运行时：输出缺少 msg=stop`。该用例本就刻意占用端口，bind失败是预期输入。Codex最初只看bind行误判为随机占用、发起一次同源全套重跑；复读原测试后立即纠正用户口径并取消重跑，不以重跑替代缺陷修复，不把它写成环境偶发。
- 原生v3仅完成部分make check（此前fmt/lint/sec/unit/integration通过，cover尚在运行），后续公共full/stage/候选e2e未完成。自有控制进程按PID/路径/启动时刻核对后取消；Linux重跑子进程仅按精确候选环境标记或exe核对后停止，3个自有子进程消失。原始partial manifest/log不改，另存 `.loop/obs-v3-cancelled-controls.json` / `obs-v3-cancelled-linux-children.json`；8080仍PID15180。第一次取消脚本遇快照PID已退出而停止，修正为已退出即略过，后续取消成功；该错误不作产品失败。
- 独立只读定位确认：run.go早退确实调用rt.Close，scheduler.Stop也确实被调用；robfig v3.0.1返回的context仅等jobWaiter，可能早于调度循环最终timer.Stop/真实Info(stop)回调完成。已证完成屏障不足，不能据此宣称DB/锁/业务job实际泄漏，OBS尚未Start也不是直接原因。
- 仅修改现有 `internal/adapters/cron/cron.go`：Stop合并原jobs完成与本代真实stop回调转发后的完成信号；重复/并发Stop共享信号；Start在上一代循环结束前不发布下一代，不等待上一代jobs，避免旧stop误确认新代。依赖仍为原固定robfig3.0.1，明确适配其最终回调语义；没有假stop日志、产品sleep、新配置或核心接口/70s宽限变化。
- 新黑盒可控stop日志handler有效红 `.loop/obs-cron-loop-red-windows.jsonl`：`Stop completed before the real scheduler loop finished its stop log`。新增五项覆盖loop完成、未启动/重复、Info过滤、并发Stop与跨代重启；保留原真实在途job测试。完整cron race Win `obs-cron-final-green-windows.jsonl`、Linux `obs-cron-final-green-linux-v2.jsonl`均exit0；第一次Linux探测被PowerShell原生stderr警告提升异常中断，原件保留、不当有效Go失败。
- 原 `TestBackfill_Web/release_order` 未改一字，修复后真实进程级 Win/Linux 均PASS：`.loop/obs-cron-release-order-green-{windows,linux}.jsonl`。Win独立窄候选SHA `3f988c987a0b7c0e6a91ef588e983ec909bd74f2263588e917571415a52dd4a1`；它只是定向证据，最终候选仍需全门禁重新绑定。格式与cron go vet已过，独立代码复核继续。
- 专属stage再纯追加cron完成屏障回归，原37及先前追加两项保持不变，共40项；台账16A追加cron触点，US17不动。`.loop/obs-private-final-comparison.json`从原stage字节重建验证旧SHA，证明原检查未改。准备v4严格前后源码指纹全门禁，另加最终Windows候选release_order复验；T028仍未勾选。

### R069 T028 v4 完整 Linux、七目标与新候选故障工具链通过（2026-11-02）

- v4 `.loop/obs-final-v4-20261101T203026573300Z/manifest.json` 初始源码指纹 `36f85700ce0e0013615824a0ef8f4dbbeaa679136b33cf0c8ee8640261c6c0e5`。Linux/root完整 `make e2e E2E_TIMEOUT=20m` exit0（总665.156s，Go658.640s），五组Python示例102例通过，原release_order断言通过；前后同一源码指纹。默认10m未改且先前确实超时，不能写默认命令通过。
- 原生完整 `make check` exit0 /1190.844s，fmt/lint/sec/test/test-integration/cover/build七目标均完成、所有包覆盖率达原阈值；独立 `CGO_ENABLED=0 go build ./...` exit0 /7.750s。Windows新候选SHA `3f988c987a0b7c0e6a91ef588e983ec909bd74f2263588e917571415a52dd4a1`。公共full当前仍运行，40stage、最终Windows两组候选e2e和Linux非root五项补测未完成，故T028仍不勾选。
- 在新候选上实际重做AfterStart/PartialSeed两故障，精确PID/路径/启动时刻核对、终止与5s退出等待均通过；独立PID不存在且不留ready。真实SQL证实部分夹具只有1条completed Run、LLM/tool/assembly/diagnostic全0。原8080前后均PID15180。
- 第一轮新候选cleanup SQL实际exit0，before=2/remaining=0；WSL UTF16警告在首个ASCII marker前留下NUL，Python锚点解析失败，故整条工具链如实FAIL。六份共享原件按字节归档 `.loop/obs-runtime-attempt1-preservation/manifest.json`，不改初次失败汇总；未产生的新cleanup JSON不得借用旧a176结果。仅解析视图去NUL，raw log不改；再以两个新case完整重验全部故障/事实证明/清理。
- 第二轮 `.loop/obs-final-v4-20261101T203026573300Z/runtime-faults-attempt2/runtime-faults-summary.json` PASS，三步骤exit0。当前fault manifest SHA `b9295ecf6c0563f3b97cff73e9fd13bdc23a27b3eba7c1f3145bfc3e90cd19f3`；精确SQL针对新2+旧历史3个自有schema，实际before2、删除2、remaining0。旧3已清理不算本轮新删除。全部SQL/log SHA与候选SHA留独立证明，最终renderer仍须核对原件。
- 私有走查脚本在自有workspace预放既有格式agents_skipped marker，保留任何已有marker，避免安装可运行样例Agent及晨间定时任务；这是合成OBS夹具，不代表自动首次安装/登录烟测。脚本语法校验通过，新两故障已实际验证该设置。浏览器能力查询当前无可用surface；正常走查服务、HTTP与实际浏览器入口仍待准备。

### R070 T028 公共 full、显式本机信任与正常走查就绪（2026-11-02）

- `.loop/runs/accept-20261101T205027.667298900/report.json` 公共full 11/11检查PASS、exit0，源码指纹与v4初始同为 `36f85700ce0e0013615824a0ef8f4dbbeaa679136b33cf0c8ee8640261c6c0e5`。总1548.188s；专属40stage仍运行，已包含真实Collector通过；最终Windows两组候选e2e和非root五项尚未完成，T028不勾选。
- 正常走查首次失败在context_validation：私有PowerShell客户端漏发既有 `X-Basalt-Local-Trust: 1`，服务日志明确`trust_header_missing`、403，守卫行为正确。脚本按PID18512/路径/启动时刻核对并终止、5s等退出，不留ready。失败目录经绝对路径/.loop边界及非reparse校验后移动保留为 `.loop/us16a-review-failed-auth-*`（完整路径见preservation JSON）；唯一失败schema `basalt_obs_review_df159eb28e75` 精确SQL清理before1/remaining0，见 `.loop/obs-normal-auth-failure-{preservation,cleanup}.json` 与SQL/log。未改变产品鉴权。
- 仅在私有启动校验与HTTP客户端补齐原合同信任头；因launcher SHA改变，第二轮故障通过聚合及原件已归档 `.loop/obs-runtime-attempt2-preservation/`，又以两个新case完整重做三步骤。最终fault工具链为v4 `runtime-faults-attempt3/runtime-faults-summary.json` PASS，候选仍 `3f988c987a0b7c0e6a91ef588e983ec909bd74f2263588e917571415a52dd4a1`；旧第二次通过不冒领新脚本。
- 正常夹具重新创建干净workspace/state/schema，v4 `normal-review-attempt2/normal-review-summary.json` PASS：首启动、第二次启动复用及14项HTTP均exit0；两次preview文件SHA完全相同，未换进程/端口/schema。HTTP真实验证3 Run、1失败/1超时/1工具拒绝、正常四类来源、允许candidate/hit各1、无CANARY、空总览0且失败率NULL、缺失timeline200+not_recorded/context404、缺anchor400。实际无可运行Agent文件，安装marker agents_skipped=true，alerts/export/cleanup关闭。
- 当前预览PID41172、启动UTC `2026-11-01T21:20:02.3871218Z`，自有schema `basalt_obs_review_c9f56c4a06cc`，地址 `http://127.0.0.1:56775/console/observability?view=run&run_id=review-normal`；`.loop/us16a-review/preview.json`及HTTPmanifest绑定候选SHA/进程/启动时间/schema。浏览器登录页仍需点击“使用本机信任”，无需token，认证标记只在页面内存；不能称为免鉴权或免登录。
- CUA实际getState返回apps/browsers均空；随后对该真实URL尝试iab及chrome，分别返回`Browser is not available: iab/chrome`。`.loop/obs-review-browser-evidence.json`明确not_run，8视图均未自动视觉/交互、截图0；这是工具未连接，不是产品页面失败，不绕过原生控制限制或伪造截图。HTTP14和前端测试与视觉检查分列；人工review②负责最终界面走查。

### R071 T028 最终机器证据核对通过（2026-11-02）

- `.loop/obs-final-v4-20261101T203026573300Z/verified-evidence.json` PASS，SHA `3263b3e67f7be0be353cc47b654abeff658fa46162ff1e93903a3d7204ade774`。八步骤实际执行前/后源码指纹全部为 `36f85700ce0e0013615824a0ef8f4dbbeaa679136b33cf0c8ee8640261c6c0e5`；两份报告也为同指纹，日志SHA、原失败/取消记录SHA和实际候选marker逐项核对通过。
- acceptance：11/11 PASS，`.loop/runs/accept-20261101T205027.667298900/report.json` / SHA `3995755aa1717a4e70d8f3bb90d0960cbd2be3617f219acd70f9064228a9f7af`。
- us16a-stage：40/40 PASS，`.loop/runs/accept-20261101T211616.061130900/report.json` / SHA `ca39a3eb81cda79866e58526c34647454d2f1d65653d66a859bc022b9b370556`。
- linux-root-full-make-e2e：51顶层/194具名含子例PASS、skip2；候选 `.loop/obs-linux-v4-candidate/basalt`，36905120 bytes / SHA `38cb84becc95fb58c75df9afa5ffd47a525dbe9849d3fd143c8429863e6a8969`。
- windows-candidate-e2e：16顶层/52具名含子例PASS、skip0；候选 `bin/basalt.exe`，37558272 bytes / SHA `3f988c987a0b7c0e6a91ef588e983ec909bd74f2263588e917571415a52dd4a1`。
- windows-release-order-e2e：1顶层/2具名含子例PASS、skip0；候选 `bin/basalt.exe`，37558272 bytes / SHA `3f988c987a0b7c0e6a91ef588e983ec909bd74f2263588e917571415a52dd4a1`。
- Linux完整全套仅按已有root测试上下文、显式E2E_TIMEOUT=20m；默认10m原失败保留。其两个skip为MCP帮助子进程入口与Windows安装烟测，不伪称在Linux通过Windows安装。非root五项源级权限/软链补测5顶层PASS/0skip；默认WSL调用无root参数，完成后独立id-u观察为1000。非root全套未重跑，原只读快照TempDir清理失败仍保留。
- Stage原始具名SKIP 10次、去重6项：`TestCredentialCrypto_KeyPermissionBits`、`TestCredentialCrypto_MasterKeySymlinkRejected`、`TestProviderService_RealCallWritesAudit`、`TestRootReader_RejectsInRootSymlink`、`TestRootReader_RejectsLinkEscape`、`TestWorkspaceBrowse_NoSymlinkEscape`。五项Windows软链/权限位由Linux补测；RealCall门控未开，真实模型不调用。原Windows跳过仍是skip，不改写为PASS。
- behavior评测：pass5/fail0/skip1（total6）；仅outcome-real-model-answer按原provider门控跳过。
- injection评测：pass4/fail0/skip0（total4）；全部已执行。
- 官方Collector SHA `20aa55b613207c37f6b65df555b16001ec3f965d7218bf619742cb8579f0daa1`；本轮Go Collector测试程序 `.loop/obs-final-v4-20261101T203026573300Z/collector/collector-product.test.exe` / 64777216 bytes / SHA `38c3473d95ce8160505bbd0c35e23183bb566de0b8178003c8d0e74998feb140`。这两者与两个平台发布候选严格区分；外部生产后端未跑。
- 正常服务仍供人工走查：PID41172、schema `basalt_obs_review_c9f56c4a06cc`、14项HTTP与二次启动复用PASS，最终私有launcher的两故障/partial SQL/精确cleanup以runtime-faults-attempt3为准。浏览器证据明确 `not_run`，8视觉/交互未跑、截图0，入口工具不可用；不得写浏览器已验。
- 13不变量静态报告仍PASS；所有动态必需工程检查已齐。下一步只做回交包最终交叉复核、任务勾选、文档校验和交付清单封存；人工review②仍待用户，尚无Codex提交/推送/合并/tag/部署。§5开工时“进行中”为历史记录，最新状态以本条及后续收口记录为准。

### R072 已到 review ②，工程收口与最终回交封存（2026-11-02）

- **有条件完成：授权实现、兼容修复、必需机器门禁与走查准备全部完成，T001–T028已勾选；唯一人工收口review②仍待用户走查与追认，不能写人工已通过。** 默认10m完整e2e原失败保留，最终是Linux/root显式20m；非root全套不作通过声明；浏览器工具不可用，视觉/交互not_run；真实模型、真实外部后端、生产容量/启用和本US远端CI未跑。
- 独立只读最终交叉复核C0/H0/M0；原八步、full11/stage40、原37检查保留、候选/Collector分层、faults attempt3、HTTP14、进程身份与失败/skip/未跑说明一致。复核草案原字节保存在 `.loop/us16a-handback-reviewed-draft.md`（SHA `8020ddc44d52c03bd36fc433780e55dc4a8a57b397ca91e91cd0941482a152b2`）；最终库存/receipt和更新后正文校验由控制侧另行完成，不冒充AI复核已覆盖后续封存。
- 私有十二节回交包 `docs/us/us16a-codex-handback.md`：正文排除checksum footer的SHA `d625e3df034146587f5487d3ee616fa3f3580df1dce6b3df1dbc7a0866f4a7f3`；**完整文件SHA `cb3b8c4a2dfdbc0e9e31436c2dc1675e8fde7392e5f48ca581f1bc722dc1e89b`**。完整SHA只在本状态和receipt中引用，避免文档自身自引用。最终markdownlint、冻结/28任务校验和git diff检查通过后生成交付清单。
- 交付目录 `.loop/obs-delivery-us16a-20261102-final/`：`inventory.json`含完整工作区状态/逐文件SHA/私有文件与分组；`tracked-changes.patch`、`changed-files.zip`、完整status/stat均供复核。最后的`receipt.json`记录这些工件、最终回交/状态文件、候选、源码指纹和实时走查进程身份；本条只引用路径，避免库存与run-state循环哈希。
- 正常走查入口与精确PID/schema/启动时刻以 `.loop/us16a-review/preview.json` 为准；当前56775端口、3组合成事实、14项HTTP及二次启动复用通过。登录页点“使用本机信任”，不用token；指标选24h。脚本仅在自有随机端口/schema运行，样例Agent不安装，alerts/export/cleanup关闭，原8080仍PID15180。故障schema与首次正常失败schema均已按独立SQL精确清理，正常schema保留供走查。
- 早间走查包按handoff§0.5的十项，直接给在本次最终对话回复（≤20行）；回交包§9引用该回复，不复制第二份清单。用户只做最后人工走查；依赖坐标、最小契约补齐、启动index_switch边界、兼容修复与判据/整包预算适配留review②追认。
- 固定US-16A，不占既有US17；当前branch016a-observability、外部HEAD d46cc40保留。Codex未commit/push/merge/tag/部署/发布，也未启用生产导出或清理；不把旧main CI当本US证据。最新状态以本条及最终receipt为准，开工时§5“进行中”保留为历史。

### R073 人工走查发现诊断展示缺口，review ②尚未通过（2026-11-02）

- 用户明确要求在8080重启供人工走查；已将既有非生产演示实例 `D:\code\basalt-demo-us16-secure` 更新为最终Windows候选，SHA仍为 `3f988c987a0b7c0e6a91ef588e983ec909bd74f2263588e917571415a52dd4a1`。8080由旧PID15180切至PID33336；工作区、配置与现有密码保留，旧程序、配置和可列目录验证的PG备份位于该实例 `.setup/us16a-restart-20261102T002925Z/`。健康和七项HTTP检查通过，匿名OBS请求401。此前R072的8080进程身份为当时事实，不再代表当前身份。
- 用户亲自执行新运行 `web-1793579836373-wzhuhy`，并提供其页面及失败运行 `scheduler-1793496601659-9w7amd` 的截图。新Run的真实模型调用是用户人工操作，不倒填为此前机器门禁或Codex发起的真实模型验证。
- 对照已审颗粒度文档、四份实际受管理认证保护的timeline/context响应及READ ONLY事务内的原始事实，确认10项开放发现：P1六项、P2四项。证据目录 `.loop/obs-manual-review-audit-20261102/`，`manifest.json`绑定实际候选/进程、响应SHA和受查源码SHA；`findings.json`列逐项依据与修复验收条件，`database-sanitized.json`只含安全分类、计数及必要数值，不落凭据或原始错误正文。
- **本次失败的真实终止原因是上下文超限**：不可裁部分估算66171 tokens，超过配置65536（记录包含余量1024），Run与任务行存有相同原因；OBS将其降为 `unclassified`，任务投影又未提供安全原因。Run耗时180900ms；任务记录实测181118ms，端点相减181119ms，口径分别保留。此前fetch外部错误耗时2550ms、write_file参数错误记录耗时0ms，不能据此替代最终Run失败原因。
- 失败Run时间线有91条记录、21个非空结束时间、15条带耗时的模型审计（9条chat、6条compaction）、19条带开始/结束的工具审计、17条明确allow判定。页面的“未记录”并不代表这些事实都未采集；也不能把15条模型审计等同于15轮对话或自动推断重试。

| 发现 | 优先级 | 未达要求与证据 |
| --- | --- | --- |
| OBS-UI-01 | P1 | 安全失败分类覆盖不足：已存的上下文超限丢为“原因未细分”；调度失败缺安全原因投影。需补安全分类及关联说明，不得直接公开任意原始错误。 |
| OBS-UI-02 | P1 | 前后端原因枚举展示不齐：external_error、invalid_input等九个已接纳值没有中文映射；真实失败工具因而仍显示未细分。 |
| OBS-UI-03 | P1 | 时间线仅渲染status，忽略已有decision；本次17个allow被显示为“未记录”。 |
| OBS-UI-04 | P1 | 时间线缺已知结束时间/耗时展示；工具duration在读侧投影遗漏，详情也错误显示未记录；LLM已记录耗时只藏于折叠详情。 |
| OBS-UI-05 | P2 | 点事件、无错误原因的成功、真正缺证据混用“未记录”；调用账本与诊断事件的主视图角色也未区分，易误解为重复调用。 |
| OBS-UI-06 | P1 | 颗粒度文档§1及FR-009要求逐项证据档位；API有entry.evidence，页面没有逐项展示。 |
| OBS-UI-07 | P1 | 颗粒度文档§2.5要求权威/诊断冲突时明确提示；目前仅分行展示，没有冲突标记或“以审计为准”的实现。源码审查确认缺口，未向真实库制造冲突案例。 |
| OBS-UI-08 | P2 | §2.15.3要求负耗时显示“时间异常”及说明本机墙钟；当前负值分支显示未记录，缺墙钟说明。源码确认，未注入真实时钟故障。 |
| OBS-UI-09 | P2 | §2.6.3要求提示数据变化可使展示编号重排；当前只提示局部范围与非因果，未说明编号不是持久标识。 |
| OBS-UI-10 | P2 | 前端验收主要检查DOM文本存在，包括折叠详情；未证明默认可见性，也未覆盖本轮暴露的原因映射、判定、时长和冲突展示判据。 |

- 不将设计允许的边界误报为缺陷：LLM无独立finished_at时，按§2.6.4保持结束未知，不能用created_at+duration_ms伪造界面结束时间；但已有终态耗时必须可读。工具结束需标记录内派生。混合过滤计数不等于文档数，装配不证明模型使用，历史确实未记录的字段不得补造。
- 按用户要求实际尝试独立看页面：CUA inventory为apps[]/browsers[]，iab与chrome均明确返回Browser is not available。独立浏览器视觉验收仍为not_run、独立截图0；已检查的是用户给出的实际截图、真实接口、只读库事实和源码，不冒称已亲自操作浏览器。
- **当前结论：Run诊断页面未达到本轮人工验收要求，review ②尚未通过，需按上述发现整改再走查。** 原必需机器门禁PASS保留为其实际覆盖范围内的历史事实，不能作为这些界面判据通过的证据；原封存库存/receipt不覆盖本次新增复核工件。此次仅取证与追加状态，未修改产品代码、原测试、设计输入或原封存包；未重跑机器测试，未commit/push/merge/部署生产，不占US17。

### R074 根据人工截图整改排障路径，8080已更新为复查候选（2026-11-02）

- 用户补充17张真实截图，指出排版、文案、指标身份不明、默认长表及无法定位异常。按其持续授权完成整改；编号仍为US-16A，未改US17或冻结的spec/plan/tasks正文。整改前受改文件快照为 `.loop/obs-ux-before-20261102T011305Z/`；原R072封存包、R073取证文件保持历史原件，不以新结果覆盖。
- 页面按“执行结果→异常定位→执行时间线→上下文证据”组织。总览改为概况卡片和异常任务表，去掉默认全量指标请求；运行详情复用同一诊断摘要，资源哈希、压缩与资源版本收进可展开区域，观测与运行页扩展有效宽度。筛选使用本地日期时间控件，传输继续为RFC3339，原深链/认证/只读语义保留。
- 时间线可切换“关键执行／仅异常／全部记录”；异常链接展开并定位真实步骤，默认显示记录的开始/结束和易读耗时。判定decision显示放行/拒绝/裁剪/降级，瞬时事件说明不适用持续时间，成功无错误不再当缺证据；原始诊断事件与调用审计明确分角色。每条显示证据档位；仅通过真实audit_ref判断冲突，保留双方且提示以审计为准，不按相同轮次猜关联。缺失LLM结束时间仍保持未知，工具结束标记录内派生，负耗时标时间异常，说明本机墙钟和编号变化边界。
- 九个已接纳错误分类补中文映射。对core/prompt.go唯一固定上下文超限格式做全串、数字上限与数值关系校验，仅投影安全分类和三个数字；任意前后缀/原始错误仍为unclassified。Run和调度行的原始错误不经API出站；调度原因来自关联Run时明确来源。任务导出维持原source-only列和事实，不将关联Run原因引入其导出哈希；新增可选字段零值省略。没有修改业务预算、模型配置或历史行。
- 指标直接展示对象/状态、用途、次数/累计/P50/P95/P99口径及单位；格式化小数和耗时，累计桶明确不可相加。默认最多24项，其余全部可展开，不能冒称其余全是零。上下文候选与每轮细节可展开，主视图区分直接装配记录与模型使用待核对。导出/清理未启用时用简洁说明和候选策略，原明细与历史保留；没有启用导出、清理或改告警规则。
- 回归：前端32文件、385测试全PASS，typecheck与build通过；`go test -race -tags integration ./internal/observability ./internal/storage/postgres ./internal/web ./internal/app -run TestObservability -count=1 -json` 四包、267具名PASS（含子例）、0FAIL/0SKIP。另已通过observability/web完整普通单测、PostgreSQL/app观测集成、四包go vet、observability/postgres gosec、markdownlint及diff空白检查。新测试覆盖安全错误格式拒绝、真实零耗时/unknown区分、预算数字投影、调度关联原因不改变导出事实、默认可见结论、判定/点事件、精确审计冲突关联、步骤切换与指标口径。
- 失败尝试如实保留：首次前端6项因旧文案/列位/时间输入和新默认筛选失败；按实际交互迁移旧断言，保留其业务判据，并新增默认可见性检查。第二轮剩一项可见性过滤文案不一致，恢复“原因未细分”限定后385项通过。首次PG回归发现任务共享列增加破坏导出decoder，拆分只读解释列与原导出列后补独立回归并全绿；未把该失败当成环境问题。初次编译亦发现新测试缺integration标签及旧签名调用，修正后执行真实集成。原失败报告保存在 `.loop/obs-ux-vitest.json` 及会话工具结果中，不伪称首次即绿。
- 新发布形态为CGO_ENABLED=0、trimpath单二进制 `bin/basalt-us16a-ux.exe`，版本 `us16a-ux-review-20261102`，SHA `daf504be10dddbd35803f836f4280d692d9aec0c59f8982d67225218a79c4dfc`；原 `bin/basalt.exe` 保留原封存候选。前端dist/integrity已重建，新JS为 `index-DBa_b03r.js`。
- 更新的是既有非生产演示实例8080：停止前再次确认无运行中Run；精确核对旧PID33336/路径/启动时间后停止其进程树，新PID17736，配置与现有密码未变。旧程序、配置、PG备份及可列目录验证在 `D:\code\basalt-demo-us16-secure\.setup\us16a-ux-20261102T013209Z/`；没有恢复/删改业务数据。9项真实HTTP验证、匿名401、密码匹配、服务JS与当前dist逐字节SHA一致均PASS。既有失败Run已返回context_budget_exceeded、66171/65536/1024，调度原因来源associated_run，19条工具时长齐全，2550ms外部错误和0ms参数错误分别保留。
- 当前证据目录 `.loop/obs-ux-review-20261102/`：`checks.json`、`vitest.json`、`go-race.jsonl`、`go-race-summary.json`、`runtime-verification.json`、`restart-evidence.json`与最终manifest。PowerShell原始Go JSONL为UTF16LE，原字节保存为 `go-race.raw.utf16.jsonl`，UTF8副本只用于便携读取。先前full11/stage40/e2e是原候选历史结论，本轮未重跑，不冒领为新候选全量门禁。
- **当前状态：整改实现与上述机器/实际接口复验通过，已更新8080供用户再次走查；人工review②仍未通过。** CUA再次inventory仍为apps[]/browsers[]，独立浏览器视觉验收not_run，不能拿接口、DOM测试或用户旧截图冒充新版独立截图。没有commit/push/merge/tag/生产部署；最终画面、交互与后续是否提交仍待用户人工走查决定。

### R075 用户人工复查七项残留整改（2026-11-02）

- 用户说明前轮人工步骤已全部执行，除命名、历史时间范围、检索筛选文案、历史失败分类、调度与运行耗时关系、会话入口及管理操作“查看”这七类问题外，未发现其他问题；这不等同于本轮整改后的人工通过。编号继续US-16A，未占用US17；整改前源文件快照为 `.loop/obs-round2-before-20261102T020347Z/`。原R072封存与R073/R074证据保留。
- 统一入口为“运行排障”，逐条事件页为“诊断明细”，说明运行汇总与底层事件的关系；保留原URL及权限。Agent会话详情增加按真实session_id打开排障的入口，关联查询列出已加载的多次Run，再打开单次运行；对截断和1000条上限明确提示。
- 历史筛选提供最近1天（默认）、7天、30天及自定义，显示实际生效的本地起止，传输仍为RFC3339；锚点查询默认全部关联时间，避免旧Run被最近1天排除。清除“全部记录”深链残留日期，禁止显示全部却仍按旧日期过滤。**依用户本轮明确要求，将管理历史读取上限从24小时扩至30天**，更新对应API描述；保留行数/组数/来源扫描上限。指标窗口、采集、告警后台窗口、导出及保留策略不变；冻结spec/plan/tasks正文未改，本项新裁决在本记录承接，不能将旧24小时设计表述冒充新实现。
- 知识检索的permission_denied历史事件属于访问范围筛选统计，改为“已按访问范围筛选”，不再列为运行异常；混合计数4及口径局限保留在技术详情，既不换算为过滤文档数，也不补造逐项身份或原因。真正的工具拒绝仍列为异常。
- session `scheduler-156066457e7633c1` 首次失败Run为 `scheduler-1793450080217-0oywzs`。旧分类只判断正文是否大于总预算，漏掉预留量：实际7518+1024=8542，大于8192。改为校验正文+预留，仅从核心固定格式提取有界安全数字，新增边界相等及恶意后缀回归；原始错误不出站，任意未知错误仍不猜分类。旧66171/65536/1024案例应读为66171+1024=67195超过65536；未改业务预算、模型配置或历史数据。
- session `scheduler-aa592b885ce22dfe` 首Run `scheduler-1793450167810-1856ip` 的Run17227ms、调度17252ms，外层边界差25ms，8次模型调用记录合计15925ms；Run `scheduler-1793493000025-8gb54i` 总39652ms、调度39723ms，最长模型调用17437ms。调度包含Agent运行，二者重叠、不可相加。单Run新增调用合计及慢调用；每条返回真实run_id支持会话内准确归属。调用合计可能重叠，其余阶段没有分项计时，不能把差额推断成排队/网络。LLM独立结束时间缺失仍保持未知。
- 管理操作“查看”原实现将详情放在长列表下方，点击后当前画面看不到变化；改为当前画面内的详情对话框，立即显示加载/错误/结果，支持关闭、Escape、焦点限制与返回原按钮，关闭后的旧请求不能再打开详情。仅执行原只读GET，增加按operation_id打开排障的入口。
- 验证：最终前端396项PASS（含时间预设/自定义、过期深链、过滤与异常分离、嵌套计时、会话入口、详情对话框）；typecheck与生产构建PASS。observability/web完整普通测试、四包go vet、observability/postgres gosec及四包观测相关race集成269具名PASS、0FAIL/0SKIP。race在最终Go源上通过，之后只补前端深链清除及其测试并重建dist；未把历史full11/stage40/e2e当成本候选全量门禁。最终真实HTTP16项200、超过30天400、匿名401、原密码匹配及服务JS与当前dist逐字节SHA一致均PASS。
- 失败与中间尝试保留：首次新增Go用例夹具缺少必需来源端口，以及原48小时非法窗口断言不适用于新30天裁决，修正夹具和边界为31天后通过，未放宽无效参数/1000行约束。首次race与前端构建并行，embed读取的旧文件在构建中被替换，web/app编译失败；固定产物后顺序重跑四包通过。首次停服后的程序文件暂时仍被Windows占用，核对已停止目标后重试复制；一次HTTP验证早于服务就绪，等待健康后复验。验证脚本曾错误假定Run在attrs中带duration_ms，已按真实起止计算校验；没有为脚本补造产品字段。先行r2接口证据保存在 `.loop/obs-round2-intermediate-20261102/`，最终结果独立归档。
- 最终候选 `bin/basalt-us16a-ux-r2.exe`，CGO_ENABLED=0、trimpath，版本 `us16a-ux-r2-review-20261102`，SHA256 `3f15752711d444d3c5d96bfdc8a9f5c07f3731bd6ab4494f1c3daafaaf98a440`；原工程候选 `bin/basalt.exe` 未覆盖。最终dist为 `index-Cb_HUvzz.js` / `index-CuxHhRFU.css`。8080旧PID17736先更新为29408，前端深链补充后再次更新为18344；两次停服前均查无运行中Run并核对进程路径/启动时间，配置和原密码未变。两次PG备份及pg_restore目录验证在演示工作区 `.setup/us16a-ux-r2-20261102T021907Z/` 与 `.setup/us16a-ux-r2-20261102T022231Z/`，未恢复或删除业务数据。
- 最终证据 `.loop/obs-round2-review-20261102/` 包含前端、race原始UTF16LE及UTF8副本、静态检查、HTTP响应、运行身份、源文件/dist哈希和manifest。CUA复查仍apps[]/browsers[]，独立浏览器视觉验收not_run；已验证的是DOM交互测试和实际接口，不冒称亲自看过新版浏览器画面。**当前：七类整改完成并提供8080复查，人工review②待用户复查，未自动判通过。** 未commit/push/merge/tag/生产部署，未操作US17。

### R076 总时长、模型耗时与调用次数分别展示（2026-11-02）

- 用户要求将本次总时长、模型处理时长、模型调用次数分开说明。执行摘要改为四张卡片：本次总时长、模型调用累计耗时、模型调用次数、工具调用次数。有关联调度记录时，总时长采用调度起止，并另列其中的Agent运行时长；没有调度记录时明确按Agent起止。调度结束未知时保持未知，不拿较短的Agent跨度冒充调度总时长。
- 核对 `internal/provider/service.go` 的实际计量：围绕 `client.Chat` 记录调用耗时，不能将其命名为模型服务端的纯处理时间。界面解释请求发出至返回，包含等待和网络传输；模型次数按加载的LLM审计计数，包含失败、待终态与会话压缩，不把轮次数当调用次数，也不重复计算诊断事件。显示有耗时记录的覆盖数量；缺失、截断、来源/时间筛选不能冒充完整统计，已记录零耗时仍保留零。未新增不可验证的排队、检索或首字耗时。
- 前端34文件400项PASS，新增真实0/未知区分、外层总时长、非调度运行、部分模型记录、失败/压缩计次与诊断不重复计次回归；typecheck、生产build、CGO_ENABLED=0 trimpath Go构建、markdownlint及diff空白检查PASS。本轮只改前端展示/测试及运维说明，未改Go生产源、API或数据库结构；R075的269项后端race结果保留为历史验证，本轮未重跑。
- 新候选 `bin/basalt-us16a-timing.exe`，版本 `us16a-timing-review-20261102`，SHA256 `5c3b378ad7ac00be04f3546e79613f1255443b5ba2f945a284140df29e91d77c`；原工程及r2二进制均保留。8080停服前确认无在途Run并核对PID18344/路径/启动时间，备份程序/配置和PG并验证pg_restore目录后更新；新PID19532，配置与原密码不变，备份为演示工作区 `.setup/us16a-timing-20261102T023157Z/`。16项实际接口复核、匿名401、31天范围400及当前服务JS与dist哈希一致均PASS；dist为 `index-C1YUQQvi.js` / `index-CuxHhRFU.css`。
- 本次前置快照 `.loop/obs-timing-before-20261102T023031Z/`，验证证据 `.loop/obs-timing-review-20261102/`；保留旧封存不覆盖。用户又询问可进一步细分的时长，现有模型/工具审计可提供逐次、累计和慢调用统计；检索全过程、上下文准备及模型首字响应等须补充实际采集，未由相邻事件时间差补造。独立浏览器视觉验收仍not_run，人工review②待复查；未commit/push/merge/生产部署，编号US-16A，未操作US17。

### R077 实测检索、上下文与非流式响应阶段（2026-11-02）

- 用户要求按建议实现阶段细分，并明确选择“保留现有调用方式，展示真实首响应等待与响应接收耗时”。本轮优先落地知识检索、每轮上下文准备、非流式模型响应三部分，补上模型用途/工具的次数、累计、平均和最长统计；没有将首响应字节冒充首个token，没有切换流式协议、增加重试或改变工具/供应商扩展字段的传递方式。准入排队、结果持久化、通知投递等未在本轮补成独立阶段。
- 增加消费方 `core.PhaseRecorder`，显式经ServiceOptions/RunEnv/ChatRequest传递；实际执行边界记录起止墙钟及单调时钟耗时，不通过context暗传业务依赖、不从相邻事件间隔补算。检索细分为全过程、查询向量化、访问范围解析、关键词检索、向量检索、融合与预算筛选、命中内容精确读取；每轮上下文细分为父阶段、知识复核检查、压缩检查与处理、提示词装配。无向量或无命中时不制造对应查询/内容读取的耗时；合法范围拒绝标记跳过，实际路径故障和取消各自保留状态。
- 模型仍执行一次非流式HTTP请求：用httptrace捕获首响应字节，单独测量读取并关闭响应体；回调不做观测写入，调用审计耗时冻结、审计终态回填后才发布阶段证据，并以实际llm_calls主键关联。原始模型审计无独立结束时刻时仍保持未知，新阶段端点不用于回填它。阶段写入独立限时、可丢弃，观察器故障不能覆盖业务结果；未记录或时钟边界异常不伪装成零/正常。
- 阶段证据复用既有retrieval/assembly/llm_call诊断种类，不增加数据库表/迁移；依本轮用户授权增加成组标量白名单 `phase`、`phase_parent`、`phase_status`、`duration_ms`、`phase_end`。阶段名、父关系、状态、有限耗时和RFC3339端点均严格校验，落库/读取/导出复用同一脱敏入口；不含查询、正文、参数、凭据或原始错误。OpenAPI attrs说明及Operations口径已更新，冻结spec/plan/tasks未改。新事件会增加对应诊断种类的事件数量，不改变Run失败计量的权威来源。
- 页面“阶段耗时”先列父阶段和两项模型响应计时，检索/上下文子阶段默认折叠，保留时间线实际起止与耗时。“模型用途与工具耗时细分”可展开查看；次数只计审计，阶段与诊断不重复算调用。平均只除以有耗时的记录，父子耗时不相加，采样/筛选/截断只代表返回范围。历史运行不补算，旧瞬时事件仍为瞬时事件；墙钟结束早于开始时保留独立耗时并提示异常。
- 验证：前端34文件403项PASS，typecheck与生产构建PASS。后端完整普通七包2645项具名PASS、4项Windows权限/链接条件SKIP；最终核心五包完整race为969项PASS、2项Windows链接条件SKIP，PG/app/web观测与阶段race为155项PASS、0SKIP；随后仅补“空检索不记录内容读取”与测试清理边界，最终app/provider阶段race再5项PASS。该5项与前述存在重复，不作为额外唯一用例总数。七包go vet、六包gosec、格式/markdownlint/diff检查通过；旧full11/stage40/e2e仍为历史结果，本轮未冒领重跑。
- 失败尝试保留：首次新增检索测试误用假端口字段，编译失败，修正为既有fakeVectorPath.path.err后通过，未削弱断言。第一次演示验证脚本因Windows PowerShell对无BOM中文脚本的解析失败，尚未发出POST；改为ASCII测试消息后执行一次创建会话、一次发消息，未重试不确定的写请求。测试报告保留初始编译失败及后续通过记录。
- 最终候选 `bin/basalt-us16a-phases.exe`，CGO_ENABLED=0、trimpath，版本 `us16a-phases-review-20261102`，SHA256 `4b6e969787dc6ce5561d3b7332ed3cccb5122826a39712fd993825d68f246ae0`；原工程与前轮候选均保留。8080停止前确认无在途Run并核对PID19532/路径/启动时间，备份旧程序、配置与PG且验证可列目录后替换；新PID42696，配置/原密码不变。备份在演示工作区 `.setup/us16a-phases-20261102T030132Z/`，dist为 `index-goM2RQJV.js` / `index-CuxHhRFU.css`。
- 16项真实GET复核、匿名401、31天范围400与服务JS匹配当前dist均PASS。另在独立测试用户会话 `web-14f38724ddddff67` 完成真实Run `web-1793588515501-cvppg8`：1次模型调用、0工具、13条实测阶段、无截断。总2059ms，检索399.399ms、每轮上下文准备22.603ms；首响应365.879ms、响应接收1202.792ms，模型审计1569ms。父阶段已含子阶段，不把这些数相加为新总时长；这是一次现场读数，不是性能保证或服务端推理耗时。历史Run复查仍无新增补算阶段。
- 前置源快照 `.loop/obs-phases-before-20261102T024232Z/`；最终证据 `.loop/obs-phases-review-20261102/` 包含原始/便携测试日志、真实接口、新运行、候选身份与manifest。CUA再次为apps[]/browsers[]，独立浏览器画面验收not_run，不能用DOM/API冒充新版独立截图。**当前：本轮三类阶段细分已实现并更新8080，人工review②待用户复查。** 未commit/push/merge/tag/生产部署，编号仍US-16A，未操作US17。

### R078 历史记录30天保留与关联清理（2026-11-02）

- 用户对R077耗时细分反馈“我看了一下没啥问题”，随后要求先列清理/保护范围，并明确批准“可以。按你的建议来实施”。本轮据此将范围从原四类观测证据扩至关联业务历史与本实例管理的文件；保留期限默认30天，开关仍显式配置。该授权不等同于本轮清理页的人工复查通过。台账仍为US-16A，未占用US17，冻结spec/plan/tasks未改。
- 新增PostgreSQL前向迁移0007，未改0001至0006；SQLite来源谱系仍v6。独立`historyretention`消费方端口与PG实现承接五类关联历史，成功删除、组/行计数与清理报告同事务，失败回滚后只记录失败状态。按主键游标有界推进，避免被保护的前缀长期阻塞后续候选。
- 会话以最后活动时间起算；会话、消息、来源绑定、摘要链、运行、模型/工具/嵌入审计、已结束调度/投递、已结算配额与释放引用在完整组内处理。新维护门禁共享唯一AgentService准入的busy集合，不消耗Run并发名额；与新运行、归档互斥，事务内再锁定和复核。无会话运行单独成组；活跃会话内的旧运行随会话保留。
- 运行中、未知/未完成调用、未决配额、未释放或近期释放引用、新消息、近期证据、外部摘要/消息/装配/诊断审计引用、导出来源引用均阻止删除。启用外部导出时保守保留业务源；即使关闭导出，未确认/在途诊断来源仍保留。未知来源身份不解析文本猜归属。有效知识、长期记忆、当前定义/绑定/策略、身份墓碑、凭据密钥、版本及必要恢复/安全审计锚点不按天删除。
- 摄取只清理过期且有后续尝试的已结束历史，保留版本最新尝试、最近成功和未知调用/未决配额；不动知识文档、版本、片段与向量。临时管理操作采用明确动作白名单，保留同目标最新成功、所有资源/凭据/生命周期/连接测试来源和新诊断引用。当前用量周期及仍被Run或调用审计引用的配置快照保留；孤立未知审计不因年龄推断可删。连接测试表本来只存每资源最新结果，继续保留。
- 日志在取得工作区运行时后切换到`.basalt/retention/logs/`，默认日志器及底层审计共用输出；按天/32 MiB轮转，归档30天、总预算256 MiB，容量超限可提前删旧日志，保留当前文件。只清理本实例过期`.upload-<nonce>`普通临时文件。备份仅处理本实例`retention/backups/verified-*`中的恢复验证标记与SHA-256清单，再次核对所有文件、拒绝未知项/链接、不递归删除，至少保留最新两份；原有`.setup/`、未验证或损坏备份继续保留。保留期限不是对保护数据的最长寿命保证。
- 清理页/API增加独立业务历史、文件策略与执行状态；删除组数和删除行数分列，不混用原观测证据行数。页面仍只读，修改配置需重启。业务清理失败纳入`cleanup_failed`告警；旧报告可汇总计数，必要删除窗口、当前告警状态与恢复依据保留。运维手册与API契约同步更新。
- 验证：前端34文件404项PASS、类型检查与生产构建PASS。八个相关Go包完整race单元检查通过；最终日志接线后app/historyretention/web完整race再通过。真实PG的到期删除、活跃/未知/配额/门禁/跨范围引用保护、幂等、事务回滚、完整摘要链、摄取状态/操作锚点、导出关闭时仍保护未决来源、审计所引用快照保护均通过。相关观测PG/app集成及最终API契约检查通过；有界清理与告警相关race回归通过；最终vet/gosec、markdownlint与diff检查通过。各轮有重复，不汇总为唯一用例总数；旧full11/stage40/e2e未重跑，不借用历史通过。
- 初次验证的旧7天/PGv6断言、活跃装配原可删除断言及API新增字段/空值契约已随新裁决同步；一次跨范围夹具误把外部会话也设为过期，修正为仍保留的会话后验证。PowerShell管道默认编码曾使新中文变问号，已修复UTF-8并用真实中文DOM断言通过；全文件CRLF导致的既有文档字面量检查已统一LF通过。初次归档缺少压缩程序集，随后WSL普通stderr提示触发Stop；两次均在替换程序/开启策略之前自动恢复旧服务，失败尝试与未验证备份保留。后续完整归档、隔离数据库恢复、所有表行数比对及工作区逐文件校验均通过，才写入验证标记并切换。
- 最终候选`bin/basalt-us16a-retention-final.exe`，CGO_ENABLED=0、trimpath，版本`us16a-retention-review-20261102`，SHA256 `964bdb29efadb205bf842367cfdd62908cb97cc0810a1dd9ec7796ba275eb49a`；原工程与前轮候选均保留。最终8080进程`33456`，启动UTC `2026-11-02T04:18:21.3363839Z`；配置SHA256 `2d241e9ecc1e02e64eb154ae28e5777c45d0b0b42c5d27e6386d2ea892dd9c0a`。原密码及其它配置未改，诊断与四类证据、业务历史均30天，每小时有界执行。备份`D:\code\basalt-demo-us16-secure\.basalt\retention\backups\verified-20261102T041813Z`已完成真实隔离恢复与全表行数比对、工作区归档逐项校验，包含配置、程序及环境密钥来源密文；回退需恢复同一停机点整套数据，不能仅用旧程序打开v7数据库。
- 最终真实8项GET均OK、匿名401，生效策略和首轮五组报告/文件清理均成功；当前无到期数据，删除0。17类知识/记忆/资源/凭据/身份/任务定义表行数对比不变。真实Run `web-1793592573502-jhm7sl`（本轮先行保留候选）完成1次模型、0工具，最终候选再次读取运行/诊断/阶段证据正常；没有重发不确定写请求。dist为`index-DsWWsNwu.js` / `index-CuxHhRFU.css`，服务JS与dist SHA-256一致。CUA浏览器不可用的既有限制未变，本轮独立画面验收not_run，DOM/API不能替代人工走查。
- 最终证据`.loop/retention-review-20261102/`，修改前快照`.loop/retention-before-20261102T034907Z/`；包含失败与通过记录、原始/便携日志、配置生效、恢复验证、源码/产物清单与manifest。未commit/push/merge/tag/生产部署。**当前：30天清理已在8080启用，等待用户人工走查清理页。**

### R079 用户人工走查完成，转Claude收尾（2026-11-02）

- 用户本轮原话：“Ok，我人工走查完了，现在要交给claude收尾提交吗？”记录人工走查已完成，未提出新增整改项；建议交Claude完成最终复核、材料同步和提交准备。R078的“等待用户人工走查”是当时状态，由本条更新；不改写历史证据或将未执行门禁补记为通过。
- 转交前只读复核：分支`016a-observability`，HEAD `d46cc40d952d1e3f60f2474f68df00a3ffa4d965`；R078封存清单中的1179个源码/产物路径与当前逐项SHA-256一致，差异0。本轮仅追加本台账，不修改产品代码、不重新生成dist，不重启8080；原封存manifest保留，其源码指纹中的台账截止R078，本条属于封存后的用户确认。
- Claude应以本台账最新裁决及R073–R079为收尾依据；`docs/us/us16a-codex-handback.md`是早期回交快照，其迁移版本、资产、变更数量和“待人工走查”状态已被后续记录更新。最终实现含0006观测迁移、0007关联历史清理、排障界面整改、真实阶段耗时与30天策略，不能只按早期四类证据范围收口。
- 最终实现验证见R078及`.loop/retention-review-20261102/`；提交前按最终代码范围核对必需门禁，历史full11/stage40/e2e不能冒充最终候选已重跑。提交范围保持US-16A，保留US17；核对新增文件和dist完整性、私有文档的既有跟踪规则，排除本机密钥、实例配置、运行数据库、备份、临时证据和可执行程序。最终提交说明记录PostgreSQL v7与整套恢复要求。
- **当前：用户人工走查已完成，待Claude收尾复核与提交。** Codex本轮未commit/push/merge/tag，也未代为向Claude发送消息；推送、合并或发布不由本条自动扩大授权。

### R080 Claude 收尾复核：文档同步、最终树门禁与提交准备（2026-11-02）

- 依据R079接手收尾，范围仍为US-16A，未触碰US17。工作树差异：71个已跟踪文件修改、2个旧dist资产删除、87个新文件（应用/测试、0006与0007迁移、控制台dist、Operations、OpenAPI、示例配置、`harness/stages/us16a.json`、`specs/016a-observability`）。本机配置、密钥、备份、运行数据库、临时证据、可执行程序和私有路径均未纳入提交。
- 文档同步：`docs/Operations.md` §5.11改为收口口径（PostgreSQL 5→7、`0006_observability.sql`+`0007_history_retention.sql`、旧程序不能打开更高版本库、清理/导出默认关闭、默认保留30天、7天仅作压测参数）；`docs/contracts/openapi.yaml`数据库目标版本5→7（US-16A起）；`basalt.yaml.example`的`diagnostics.retention_days`=30并新增`observability`段；`harness/stages/us16a.json`的approval追加0007与60表口径。`docs/TechnicalDesign.md`本机私有且未跟踪，正文已有0006/0007表述，本次未改、未提交。
- **证据不一致与修复**：R078称PG版本断言已同步，但最终树首轮`make check`在`test-integration`红（退出码1）：`TestPostgres_FreshSchemaIsV1`期望迁移链1–5（实际1–7）；`TestStorageStore_OpenReadOnlyPostgres`期望v6/58表（实际v7/60）。前者连0006也未纳入，后者只补到0006。按已批准0007适配为迁移链1–7、总60表（v1 52 + OBS 6 + 保留账本2），并保留OBS六表精确集合断言；v6专属`TestObservabilityMigration_Applies`的58表断言保持不变。两处定向真库用例随后通过，完整`make check`复跑exit 0。未删断言、未放宽阈值；R078原文不改写，以本条为最新事实。
- 最终树门禁读数：`make check`七目标exit 0（fmt/lint/sec含`vet`/`staticcheck -tags=integration`、`gosec`、`govulncheck` 0个可调用漏洞；`test -race`；`test-integration`真实PostgreSQL；`cover`全部包达标；`CGO_ENABLED=0` build）。覆盖率：core 78.4%、memory 85.6%、tool 83.7%、storage 73.9%、storage/postgres 79.4%、historyretention 76.2%、observability 83.3%、app 75.1%。仅注释的版本链修正后，最终树`make fmt lint sec`再跑exit 0；`make check`的代码行为判据不因注释变化，未二次全量重跑，如实登记。
- US-16A stage-only在最终树40/40 PASS：`.loop/runs/accept-20261102T055202.322238200/report.json`，SHA-256 `228498fac0d0be292aac7a5f0d751d6e74f4ce1b93bafb2c2d4803298be07ff4`，源码指纹`f4bc1b2ae6b9ddb619a3335bfcdc9c33a0c87c80799822078138bd866d7fbe04`，run-kind rerun、executor ai。Collector仍为固定版SHA `20aa55b613207c37f6b65df555b16001ec3f965d7218bf619742cb8579f0daa1`；外部生产后端未跑。
- 前端`npm test` 34文件404项PASS，typecheck与生产构建通过；dist三文件SHA与`integrity.json`逐字一致（`index-DsWWsNwu.js` 493504 bytes、`index-CuxHhRFU.css`、`index.html`），构建可复现（重建后内容不变）。
- **E2E两起失败与处置**：首轮`TestBackfill_Web/listen_addr_override`红——本机8080被演示实例（pid 33456）占用并返回Basalt健康信封；停该实例（`.setup/serve.pid`核对一致）后复跑，该项转绿，属环境冲突非代码回归，e2e结束后已用`start-demo-secure.ps1`重启（新pid、`/api/v1/health`返回ok）。复跑又暴露`TestUS16Demo`：断言“llm_call事件数=模型调用数”，而R633已批准阶段证据复用`llm_call`种类，每次模型调用另有`model_first_response`/`model_response_read`两条阶段事件，实测3条事件对1次调用；按已批准设计适配用例——调用级事件仍与模型调用一一对应，阶段级单独计数并断言阶段名在白名单内、不得携带失败原因码；未删断言、未放宽阈值。单跑`TestUS16Demo` PASS后全量通过。该改动仅`-tags=e2e`测试文件，不进入`make check`与stage编译集，产品代码与候选二进制未变（SHA同上）；`gofumpt -w`与`go vet -tags=e2e`通过。
- `E2E_TIMEOUT=20m make e2e`最终树通过：608.356s，`internal/e2e` ok，`binary_source=candidate`、`binary_sha256=394c00e33904dde53e58e92068178f029b9305f09cc9bb206bdd9786fb6343ad`（日志`.loop/e2e-final-20261102-b.log`；首轮失败日志`.loop/e2e-final-20261102.log`保留）。
- `markdownlint`（Operations、specs/016a-observability全部md、run-state）0 issues；`git diff --check`干净。完整`--profile ci`验收（11项，与`full`同一清单）由本次提交后的CI执行并回填R081，不以历史full11冒充最终树结果。
- 尚未验证：远端CI结果（R081回填）、真实外部OTLP后端、真实模型调用、生产容量与启用后的长期运行；独立浏览器画面验收仍not_run，用户已完成人工走查，不以DOM/API顶替。提交、推送、快进合并与CI跟踪按用户授权继续。
