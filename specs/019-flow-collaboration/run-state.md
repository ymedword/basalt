# US-19 运行状态

1. 本文件是**过程状态，非设计工件**——speckit 命令**不得**把它当设计输入；
2. 失败台账**只追加**：一行一次尝试，不改旧行（可 diff 审计）；
3. 唯一允许原地翻转的是未验证清单的「未验证 → 已验证」，翻转必须注日期与证据。

## 1. 基线

- 起始 main `ca9a0d29f185a548b14b595e6068c52df1c3cdf4`；US18用户确认已收口／main代码CI通过，不重新打开历史review。已有019分支原指2017211，本轮切换并FF至main，无重写。
- worktree `D:/code/basalt`，分支 `019-flow-collaboration`。起跑 Git 干净；私有 CLAUDE.local.md 的US18记录由本轮先写，保留并追加当前批准。
- 用户原文（2026-11-03）：「确认 R3 及 §6.6.0 全文，自动推进到最终人工验收」。所审模块卡 `docs/us/us19-flow-collaboration.md` SHA-256 `514ce6c9f6a8bb1e86499c80a281403e858c007a92138f3b8771429c13a37ef5`，原字节不改。批准包括模块卡review①、正式修宪／配套、登记、派生与实施；最终review②由用户决定。
- 宪法正式v6.0.0，VII从模块卡精确提取整体替换；其余原则不变。模板resolver成功，无constitution hook；本阶段仅写宪法，完成后才回外层流程同步文档。
- 允许路径：模块卡P01–P21范围的internal/flow/core/tool/storage/postgres/app/web/cli/observability/diagnostics/callers、assets/console、testdata/flow、evals/flow、harness/stages/us19-*、正式契约／治理镜像／配置／公开图源与运维；旧迁移及历史specs不改，用户8080 demo不用于故障测试。只读review不代替Claude G-FT。
- 增量登记：零新增坐标／版本；jsonschema/v6既有依赖转生产适配器；零公共profile／构建步骤新增，复用公共门禁；US19专属不变量放本stage，不复制全局清单。台账仅追加19行，18历史保留。
- spec SHA-256 `d4a8a3a8cb43c061d326b0d2c50a57a03b33ead82386503c3f32cbfdb847075a`；plan `96b7f40c04aee88941473b6de216808e3eb60942cf76b15a2c7099555b760626`；tasks归一勾选指纹 `ed8950689aaf388a13e7eb6b6bf85a8c577b73f2d1e3262e2460dc6746eee65d`。后续派生修正追加记录，不重生成已勾选正文。

## 2. 失败台账

| 日期 | 问题／症状 | 假设／处理 | 当场结果 | 同题计数 |
| --- | --- | --- | --- | --- |
| 2026-11-03 | git switch -c 019-flow-collaboration：branch already exists | 只读查分支／worktree，019仅原US18tip，无实现 | 切已有019并FF main成功；未覆盖用户代码 | 1 |
| 2026-11-03 | setup-tasks 在spec生成前调用 | 先生成spec再依赖顺序调用 | 初次明确拒绝spec不存在；补齐后setup-tasks JSON成功 | 1 |
| 2026-11-03 | npx markdownlint 无可执行入口 | 明确使用 markdownlint-cli 已有工具包 | 工具可执行，随后发现 MD012；非产品门禁结论 | 1 |
| 2026-11-03 | 新文档五处空行及 flow.md 尾部 MD012 | 归一新块与文件尾空行 | 最后完整指定文档 lint exit 0；未删断言或改阈值 | 2 |
| 2026-11-03 | Schema 先行红态：NestedSchemaViolationZeroDispatch 等三类 | 临时空实现应被断言识别 | .loop/us19-schema-red.log 命中 invalid nested input must be rejected before dispatch／unsupported schema must be rejected／missing compiled schema must fail closed；非编译失败 | TDD 1 |
| 2026-11-03 | 定义先行红态：unsupported／unbounded／required validator／trusted branch | 临时空校验应被断言识别 | .loop/us19-definition-red.log 四条具名断言红，落实后绿 | TDD 1 |
| 2026-11-03 | T003 首次 Harness fmt exit 2 | Windows PATH 未含 Git sh；不是格式断言失败 | 读取 fmt.log 确认 CreateProcess sh 失败；加载既有 Enter-DevEnv 后 fmt 与 schema 均通过，旧报告保留 | 1 |
| 2026-11-03 | 独立复核：FLOW 内嵌 contract 的 version=0 可与缺项 TASK 合并 | 用真实 Loader 构造反例；禁止文件间控制字段合并 | .loop/us19-contract-merge-red.log 命中 contract files must not merge controls from FLOW.yaml；修复待后续记录 | 1 |
| 2026-11-03 | 独立复核：blocked未join取消返回非法cancelling | 保留blocked与取消位等待真实退出，不违反封闭转移矩阵 | .loop/us19-blocked-close-red.log 命中 blocked cancellation must wait in a legal state until join；修复后复验另记 | 1 |
| 2026-11-03 | 新 PG 仓储编译：decoder 缺 ctx、DB 无 logger、Principal 无 AuthorizingKind 字段 | 对源码核实签名，显式 ctx／slog，按合法 Kind 映射原授权二分 | 对应 build1/2 日志保留，修正后编译通过；未改变公共身份协议 | 1 |
| 2026-11-03 | 旧迁移版本测试仍断言7 | 保留旧迁移／v1 hash断言，当前版本改为已批准8，增加新契约与0008逐字比较 | .loop/us19-definition-store-build3.log 通过，不删旧断言 | 1 |
| 2026-11-03 | PG首跑 Claim拒绝：JSONB将RawMessage加空格，原hash不匹配 | 加载检查点时恢复精确语义规范字节并核对hash | .loop/us19-pg-foundation-first.log 红，second.log 真实PG六项绿 | 1 |
| 2026-11-03 | 独立复核根关闭后子任务仍无关闭意图 | 真实PG反例先红；同根锁传播子树意图并在领取／提交／修订／恢复逐次核对祖先 | .loop/us19-root-close-red.log 命中具名断言，us19-pg-root-close-green.log 全部新增PG回归通过 | 1 |
| 2026-11-03 | PROG-01：完成审计成功、观察窗口写失败后旧窗口可能漏放第三次重复 | BeforeCall 对齐该节点原终态 audit 与 LastAuditID；不一致拒绝，不靠本次 Run 返回失败充当重启保护 | .loop/us19-progress-fault-green.log 两项真实PG通过；独立复核确认闭合 | 1 |
| 2026-11-03 | Worker 首跑不能从带scope的admin-builtin构造原身份 | 沿原 core.OwnedPrincipal 管理身份必须无caller scopes；Flow上限仍保留于契约 | us19-worker-pg-first.log 真实红，green.log 两项通过，未默认补管理员 | 1 |
| 2026-11-03 | 子流程容量续跑ActiveAttempt把text直接Scan到json.RawMessage失败 | 先读text，规范化并核对原input hash | us19-subflow-pg-first.log 红；green.log 直接与容量续跑两分支通过 | 1 |
| 2026-11-03 | 模型请求Snapshot解到原payload导致slice／map复用 | 解到全新payload，UseNumber保留大整数，与原消息彻底隔离 | us19-model-pg-first.log hash不等真实红；green.log 全部模型接缝回归绿 | 1 |
| 2026-11-03 | 独立复核异常usage：分项100／total1可能低额结算 | Flow严格拒负数／矛盾／溢出，动作和祖先预算unknown，返回已发出错误使caller保留pending；原始审计数值保留 | us19-model-frozen-usage-verified.log 真实HTTP／PG负例及三种回调换参通过；独立复验闭合 | 1 |
| 2026-11-03 | 异常usage测试把未结算NULL直接Scan到int64 | 改用sql.NullInt64并断言NULL；不把未知伪装成0 | 首日志保留；verified.log 全绿，判据更严格 | 1 |
| 2026-11-03 | app授权适配首编译使用了不存在的AdminPosture.Usable | 查既有姿态闭集，只放行token／local_trust | us19-authority-build.log 失败；build-green.log app Flow定点通过 | 1 |

## 3. 停止预算与计数

按harness/project：同题3次无效修复走debug，3假设失败设计讨论，每US最多2轮debug；修订不清预算。当前产品修复／debug 0，以上仅准备阶段错误。局部G-FT前置不使其他独立任务停工。无commit／push／部署／tag授权，不自动执行。

## 4. 未验证清单

| ID | 状态 | 内容／恢复条件 |
| --- | --- | --- |
| U19-01 | 已验证（2026-11-04） | 产品实现及F01–F19机器必验项：final5-public 11/11、base 11/11、process 1/1，同指纹b50912c5；不含后加业务质量及最终人工review② |
| U19-02 | 已验证（2026-11-03） | 实际 Claude 设计条件回执由用户转贴，GFT-01–06／P1／P3／P6／P7 已按回执完成隔离证据并经独立定点复验；.loop/runs/us19-foundation-4/report.json、gft-receipt.md。仅设计关口闭合，不等于行为或最终验收；生产写分支仍关闭 |
| U19-03 | 已验证（2026-11-04） | G-FT行为关口：final5-base与process具名必验项通过，原工具执行器／审计与真实进程崩溃矩阵、写动作不重放；设计前置仍以原Claude条件回执及闭合记录为准 |
| U19-04 | 未验证 | 真模型质量Q5：尚无US19服务／模型／资料／次数／金额授权，不外发；mock不证明质量优势 |
| U19-05 | 未验证 | 真实浏览器、PG／进程故障、race、容量及完整公共门禁尚未执行 |

## 5. 收口结论

未完成；review①已实际通过，最终review②未开始。已正式修宪、同步原则镜像／D07、追加US19登记及派生规格计划；T001/T002等待静态验证与analyze后勾选，不把文件生成当产品完成。下一动作：lint／输入覆盖和只读analyze→基础判据与实现；G-FT材料独立准备。

### 2026-11-03 实施前检查记录

文档 lint exit 0；模板及 prerequisites JSON 成功；只读 analyze 见 analysis.md，CRITICAL=0。FR25／任务28唯一／P21／F19 结构检查通过。原 R3 hash 不变，私有快照 `career/.backups/20261103T014316783886Z-after-us19-review1-governance-spec/manifest.json` 543文件、恢复核对通过；该快照早于最后尾空行订正，最终私有指纹需再取。

独立 Codex 定点预审识别 GFT-D01–D05（结构化结局、发出计数、固定节点不作 prompt 检索／压缩／Chat、Flow 缺 pinned 实例不 fallback、共享总计数内原子交互预留），均纳入研究与实现任务。它不是实际 Claude G-FT 签字；新八组探针／PG／真进程尚未执行，不计通过。T001 的治理／登记／静态核对已完成；T002 的分析／映射已完成，stage 具名用例存在性需随源码建立，不提前勾选该任务。继续 T003。

准备期间 plan 空行等价格式订正后的冻结 hash：`3297177f679766e9b167028d943bf0f4bc1105ecae9cd3644d4138d10547f4f7`；spec 与 tasks 正文未改。旧 plan hash 为格式前快照，不抹旧记录。

### 2026-11-03 Claude 回执及领域修正

实际 Claude 回执／原输入 hash／未覆盖面登记在 gft-receipt.md；这是带前置的设计通过，T007 不提前勾选。用户转贴即真实评审输入，不需要 CLI。模块卡只改过期状态行，批准设计原 hash 仍以上述 514ce6… 为准；四份镜像实在本地忽略文件中，已核对 CLAUDE 原则 VII、TechnicalDesign §2.8、Requirements §8、Compatibility §9 存在，不由 Git 未显示误认缺失。

Loader 合并、blocked 未 join 两条反例修复复绿（.loop/us19-review-fixes-green.jsonl）；另外已知预算一维超支须阻断其它维度分配、来源可读不等于支持目标两条真实红态分别在 .loop/us19-overage-red.log 与 .loop/us19-support-red.log，落实跨维阻断和可信支持判据后复绿。完整三包回归见 .loop/us19-canonical-summary.log；这些只是领域证据，PG／执行接线仍待做。

迁移 0008_flow.sql 及对应正式补充契约 flow.pg.sql 已写，支持版本增为 8；真实临时 PG 的 MigrateTakesAdvisoryLock 首跑通过（.loop/us19-migrate8-first.log）。T006 字面路径派生修正：既有 schema.pg.sql 是冻结 v1／0001 的逐字镜像，故不改旧文件，新增 flow.pg.sql 与 0008 逐字对应；行为范围不变，任务原文保留。本次尚无 Flow 仓储／CAS／恢复通过证据。

### 2026-11-03 G-FT 接缝与真实 PG 增量

共享助手／串行组已提取，普通 Run 原预算拒绝计数语义保留，实际尝试在结果消息追加之前计数。严格 Flow 执行接缝复用唯一 Tool.Execute 与 begin／finish，持久许可端口在外发前接收原 int64 审计ID，输入字节复制并复核schema／hash，无 pin 不 fallback；结构化结局保留实际发出及审计未知，RecordOutcome 失败不调用 AfterCall。真实 ToolExecutor＋本机HTTP接收端的动作／pending／许可失败、嵌套Schema、pin、readonly、换参／换schema及审计未知探针通过。原始 .loop/us19-executor-probes-first.log、us19-gft-shared-regression.log、us19-gft-core-prerequisites.log。固定 assistant helper 单组探针通过，但还没有 FixedTool Service 分支／计费／PG动作绑定／进程故障证据，不将 helper 当完整P4。

PG 已实现定义不可变发布／退役指针、管理审计同事务、受理幂等／容量、单次领取CAS、条件取消／关闭传播、检查点／契约完整性、结果图与转移原子提交、有限修订基础。真实PG新增具名回归在 .loop/us19-pg-root-close-green.log：发布旧版保留、12路同键接受一个任务、切版回读旧事实、8路推进只一个attempt、真实CHECK非法关闭位／JSON缺键、取消不重排、坏检查点跨caller隐藏、契约hash错阻断、末尾转移故障导致产物整组回滚、旧版本提交零产物、根关闭子领取拒绝、未知定义版本404。中途红日志与修正保留。T006仍不勾选：动作／预算持久账本、完整恢复与引用保护尚待实现，完整公共门禁未跑。

独立Codex本次只读复核六项具体发现已落实或正复验；它不是实际Claude追加签字。GFT设计仍带前置，写分支关闭；正式契约端口按用户转贴Claude回执同步精化（BeforeDispatch auditID、AfterCall ActionOutcome），原 R3 设计正文不回改。

### 2026-11-03 定义任务验收与下一阶段

T004 已验收：.loop/runs/us19-foundation-3/report.json 的 019-flow-domain 32 个具名顶层用例全部通过，另有 4 Schema／11 core／20 PG／3 callers／2 app 的局部检查全部通过、0跳过。本 stage 是局部实施证据，不代表全部 F01–F19、公共七目标或 G-FT 行为通过。新增子流程父重试／返工乘数反例先红（.loop/us19-subflow-multiplier-red.log），修复后 .loop/us19-definition-bounds-green.log 通过；可信评分器登记、必需来源下限、保证前驱、发布预算上限、目录／文件独立性均有负向与正对照。T005继续，T006/T007/T008/T009/T016仍未提前勾选。

真实动作／预算 PG 增量：.loop/us19-pg-action-probes-first.log 通过两层预写＋祖先预算故障零外发（PG sequence 非事务标记证明 fault-fired），接收端查到 action→原 pending 审计绑定，正常调用只消耗1祖先工具额度；终态写失败后的 action/budget unknown 在取消后仍保留、重试接收次数不增长；真实进程kill仍未跑。共享实例／caller计数与 app.defer 释放增量见 .loop/us19-shared-admission-regression.log、us19-app-shared-admission.log。当前已加入 Flow 显式类型字段，但 AgentService 对尚未接线的 Flow 请求明确拒绝，避免落入普通可写工具路径；尚无 FixedTool 执行分支。

Service.Start 与真实 PG 受理增量见 .loop/us19-start-service-pg.log：checkpoint故障无接受事实、旧键按冻结契约校验、切版竞争败者用胜者模板重算而回读原任务；无真实业务模型／服务调用。来源槽位也不能用Schema规则绕过来源数量／支持验证：.loop/us19-slot-source-red.log 反例红、green.log 领域全包通过。Config 排空秒数在 duration 乘法前拒绝越界，避免溢出。较早局部四包 race .loop/us19-foundation-race.log 通过；此后源码仍有增量，最终需按稳定树复跑。

下一动作：共享工具组接入严格 Flow 绑定并补组完整性探针，闭合 G-FT 设计回执的剩余前置；继续领域修订／进展窗口与服务／Worker，不等待每任务人工确认。最终人工review②仍保留。

### 2026-11-03 T007／T009 验收

实际Claude条件回执的六前置已落实。独立复验补出了共享组 binder 换参缺口：仅校验 binder 自己覆盖后的值不足。现 core.CanonicalFlowJSON 是标准库唯一规范算法，flow 委托；共享组先比较原参数的精确语义，才采用规范字节。逆改自身比较确实命中非空洞断言（us19-binder-input-inverse-red.log），随后逐字恢复并全包复绿。原设计材料 hash 保持 53c556… 未改，后续全记 gft-receipt.md。

GFT-04 已用受控只读的**完整 AgentService.Run**闭合：无模型映射仍执行1只读工具，Provider lookup／Chat／Memory／Knowledge／压缩／ChatQuota 为0，原RunQuota1、Run行1、user/assistant/tool各1、Iterations0、ToolCalls1、祖先工具1/token0；额度拒绝或写适配器零调用。生产app未设置FlowRunsReady，默认关；普通Flow Agent仍拒绝，固定写适配器仍由组无条件readonly拒绝。该原型不声称生产接线、真实caller额度或进程故障已完成。

T007／T009的控制侧验收证据为 .loop/runs/us19-foundation-4/report.json（六局部check全通过，0跳过）、us19-gft04-full-readonly-first.log、独立复跑core全包和真实PG20具名用例。独立Codex不是另一次Claude签字；它核对的是实际Claude条件的实现证据，未代行人工review②。T010可开始受控实现，写节点交付仍须行为门禁。

此后RunRecord已增加显式Flow关联，由原RunStore唯一INSERT同事务校验planned Run／node／session／原caller与key／revision／epoch，PG动作探针移除了手动UPDATE关联；.loop/us19-original-run-flow-link.log通过（20 PG用例）。该增量晚于foundation-4指纹，不把旧报告称为最终树证明。下一动作：该原始Run写路受影响回归／真实负例，再推进T010／T011及持久进展和Worker；其余任务仍未完成。

### 2026-11-03 T003／基础阶段进展

T003 完成：core 标准库 Flow／Schema 窄端口与严格 JSON 解析、tool 生产校验适配、数值精度／重复键／子集／取消／并发缓存测试。任务 Harness `.loop/runs/us19-t003-schema-2/report.json`：fmt＋019-flow-schema均通过、4顶层具名用例、0跳过；源码指纹与完整命令在report/record。`.loop/runs/us19-foundation-1/report.json` 的两个基础检查通过（4 Schema＋18领域顶层），只是局部证据，不能代替F01–F19或完整公共门禁。

core／tool 全包回归 `.loop/us19-schema-regression.log`、后续加领域与验证回归 `.loop/us19-verification.log` 均exit0；go vet -tags=integration 三包通过。T004/T005 仍在实施，不提前勾选：尚需目录／契约独立性修复、子流程发布完整边界、可信判断器登记、修订／摘要及PG接线。实际Claude G-FT与真模型仍未完成。下一动作：修 Loader 反例并具名复验、完善领域，再进入真实PG。

### 2026-11-03 持久循环窗口／有界读面增量

原 RunStore 的 Flow 原子关联负例与受影响 Run／manifest 回归通过，core／flow／tool 全包通过。可信工具分类仅识别代码拥有的具体内置类型，不信工具名或 MCP readonlyHint；生产写门仍关，write_file 查证与模型预算、app 绑定尚未交付。

新增 core 标准库 ProgressWindow（16项hash、3重复／6交替候选前拦、6次完成无进展后拦下一次）、FlowProgress 根检查点持久观察、Commit 合并调用后的新窗口、Revise 禁清零。真实PG跨revision负例及观察写故障测试通过 `.loop/us19-progress-fault-green.log`；第二次调用终态成功后checkpoint UPDATE故障确有sequence标记，重建Store／Guard后第三次零外发。独立Codex先报PROG-01（旧窗口会漏放），现以每节点原始终态audit和LastAuditID对齐阻断，独立复验确认闭合；不冒充Claude或T017完整验收。合法分页的可信事实入口目前只有组件测试，生产接线／子树／真实进程恢复仍待做。

新增有界归属任务／尝试分页、原输出attempt／Run身份核对与祖先剩余预算读面。容量拒绝YieldUnadmitted只有在原Run／action／allocation均证明未准入时才能退还尝试计数，保留interrupted历史和下一可执行时间；已准入Run拒退款。真实PG两项通过 `.loop/us19-query-yield-first.log`。这些是T006／T016的局部证据，不把Worker或服务推进记为已完成。下一动作：受控节点推进、模型逐调用祖先预算和原授权接线、Worker取消／恢复；所有未勾任务与U19-01／03／05保持未完成。

### 2026-11-03 服务／子流程／Worker与模型接缝增量

Service已增加单根顺序Advance、输出schema与独立验证缓冲、结果／引用／验收一次CAS、先持久意图后实际join的Cancel／关闭核对、独立管理Abandon基础；固定Worker池构造不启动，要求app先完成原Run恢复，退出前等实际join。真实PG＋mock叶子的成功／坏shape／坏JSON、容量无Run退款、取消竞争、固定2 worker处理5任务、shutdown非用户取消见 us19-advance-pg-first.log、us19-worker-pg-green.log。仅叶子为替身，不冒充生产AgentService接线。Revise、完整恢复及app启动／停机装配仍待完成。

静态子流程使用持久child＋固定定义／原授权／继承截止，父不建虚假Run；容量不足时保存同一父attempt和child并公平延后。真实PG直接／容量续跑两分支通过 us19-subflow-pg-green.log：两任务只一叶子Run，child只创建一次，root展开计数包含子任务；根256总展开预算与子退款在同根短事务维护。关闭完成核对同步向子树完成关闭，不遗留取消中的子任务。完整多Agent生产身份／来源／会话验证仍待做。

原ProviderService的唯一BeginCall／Chat／FinishCall新增Flow可选接缝（P07/P11及实际模型预算的必要内部改造，未新增模型调用或审计写路）：动作＋祖先预留、原pending、原llm_call_id许可后才外发；只保存请求hash元数据，不复制模型prompt到账本。模型请求深拷贝、精确JSON、消息／工具／来源回调换参零发送；四处预写故障有sequence证明且零HTTP；缺usage保持预留，审计未知停止，矛盾usage不低额结算。us19-model-pg-green.log、us19-model-frozen-usage-verified.log及独立Codex定点复验通过，使用localhost OpenAI协议端点，无付费模型。caller目前只证明返回错误不会被分为未发出，尚非完整caller PG配额接线证据。

app已开始flowAuthority适配，复用原注册／生命周期／资源快照／原Key资格检查；尚未生产接线。AgentRef.Version的内部取数使用既有生命周期generation，配置／资源仍分别核对hash，避免同名重建重绑；这只是固定版本字段的来源细化，不改批准卡正文。生产FlowRunsReady仍false，固定写在共享组仍被无条件readonly阻断；不声称G-FT行为或T010／T011／T015／T017／T019完成。下一动作：授权与工具／来源适配的真实接线、模型与caller双预算、受控写查证、修订与恢复，然后API／CLI／UI与全部必验门禁；最终review②仍由用户。

### 2026-11-03 生产节点适配与检索边界增量（尚未服务启用）

Authority 两项及 binder 两项定点已过：原 Key 不重绑、UID/generation/config/manifest 改变均拒绝；实际固定工具适配器才进入白名单，Agent 只读且模型 call ID 不能改逻辑槽。权威夹具失败记录追加：fakeDeliveryLifecycle 属外部测试包导致编译失败（authority-guards-first）；cmpwResources 的 typed-nil Runtime 导致 Release panic（authority-guards-green）；改为本包生命周期和纯元数据资源夹具后 authority-guards-verified 通过，未改产品判据。

flowNodeRunner 只调用同一 runAdmission／AgentService，配同一原消息／审计／RunQuota；Core 新增 app-owned FlowWritesReady 单独默认关闭，Agent prompt 工具表只保留实际 readonly pin，固定节点跳过无模型操作，Flow 摘要进入原 StructuredState、user 消息携原来源引用；普通 core 全包回归 flow-state-core-regression 已过。没有新增工具／模型审计写路。

真实 PG＋重路径资源／生命周期／快照＋原 AgentService 生产节点两分支通过 production-node-audit-final：Fixed read_file 一 Run、三消息、一工具审计、零模型；Agent 一 Run、两消息、一模型审计、祖先实际 tokens=8，write_file 不在请求工具表。仅模型协议为脚本替身，无付费模型；尚未证明 caller 周期配额全链或受控写行为。

生产节点夹具失败历史保留：production-node-build-full 为 Usage 字段名误用；production-node-run 为隔离 Fixed 探针未开发布 gate；production-node-verified／green 为夹具误把原 toolResultJSON 当 core.Result，且错误要求 omitempty 的 false 标志必有；按既有 synthesis.go 原审计契约修正为 content 必填、布尔可选并新增精确 content 断言，未修改既有公共契约。production-node-audit-verified 为断言插错到 finished 声明之前；移动后通过。均为新增夹具纠正，不删除或放宽既有测试、阈值。

检索接缝新增冻结集合与当前原 Key scope 核对；空冻结范围不退回 Agent 默认全声明集合、零向量化；外部 query embedding 前走祖先 tokens 预留并复核，真实 usage 已知结算、未知保留，不造 llm_calls。内部 KnowledgeRunRequest 仅增加不可由 DTO 填写的 Flow 上下文，普通检索路径不变。仍须补真实 PG 检索与恢复／受控写／生命周期装配、API／CLI／UI及完整门禁，所有未勾任务保持未完成。

### 2026-11-03 检索审计／修订／完成复用／生产装配增量

只读 Codex 定点复核发现并修复 query embedding 回调后原 Key 撤销的读取窗口、后续轮内／答案复核未传 Flow、预算票据复用没有持久外发许可、原 auditedEmbedder 终态 fail-open 不能作 Flow 审计确认、公开 HasUnknownActions 漏 embedding 状态。三条来源读取现都逐项重查当前原 Key 和冻结集合；BeforeEmbedding 在原 pending 之后、外发之前经同 root 短事务检查 task/revision/epoch/祖先关闭／当前 Key／pending预算，绑定原 embedding_calls.run_id；同一 Run 不接受第二个原 embedding 审计绑定。普通 Embed 的既有终态 fail-open 不改，Flow 可选 EmbedFlow 要求确认，未知保留预算并停。持久取消还通过 CheckActiveBoundary 禁止 ctx 尚活时继续读来源；根 unknown 统一包含绑定 embedding pending/unknown。

证据：retrieval-revoked-callback、query-audit-guards、query-permit-pg、query-strict-real-pg、query-unknown-projection、active-boundary-app／pg 全通过。真 PG permit 四例含 bound／reserve后取消／unknown票据／绑定UPDATE故障且 sequence fault-fired；重建 Store 后 HasUnknownActions=true，先完成原 Run 后 ObserveNode.Unknown 仍true。真实审计终态故障试验使用原 EmbeddingCallRepo＋PG触发器，证明外发1、pending、预算端口 KeepUnknown、后续检索／装配0；该试验的预算和permit仍为替身，不冒充整条持久预算一体故障链。原 FlowBudget unknown 的真PG回归另有分层证据。无业务或付费模型外发。

Service.Revise 已实现封闭 proposal（reason／node_id／可选input），拒绝改图、权限、约束和预算；真正 current 原Key／join／无unknown后，当前来源复核、完整反向依赖失效和新 checkpoint 一份CAS，不重置attempt／progress／deadline。原操作 ID 回读接受事实，不随当前来源或新头重解释。真PG revision-service-first 包含参数／未知键／重复JSON／预算与计数不重置、旧revision不变、幂等冲突、2次修订上限、INSERT故障 fault-fired且无部分新头；通过。每个当前验证只消费实际传入当前attempt的来源，不用历史 stale refs 证明当前新版本；历史图仍完整保留。

完成结果复用：真实原 Run 已 completed、Flow 产物事务未提交时，重建 Flow 对象后只读原消息／审计／资源钉住／当前来源，再 ReclaimCompleted＋原子接受；不新建 Run、session、attempt、模型或工具调用，不重新扣 RunQuota。旧 attempt 的 epoch 仅在 Recovered 内部路径允许，SQL 再核对原completed Run、同caller/task/node、当前固定config／manifest、原最终正文或工具审计输出hash，以及根无unknown。completed-run-reuse-green 四分支通过，checkpoint分支产物INSERT fault-fired，完成后每项仍只有1 Run／1 attempt及原调用条数。只是重建内存对象，不冒充真实二进制kill；write_file查证和真进程F组仍未完成。

flows 配置接入 CLI 严格闭集解析：默认禁用、有界缺省、显式0的预留语义、null／未知键／非法类型拒绝且不回显值；cli全包 flow-config-full通过。重路径把同一 resolver／snapshot／admission／core service 接到 flowRuntime，默认构造不启动；serve 显式先核对，Worker 在实际监听的有效管理认证姿态就绪后才推进，其他命令不隐启 Worker。startup 中断根先blocked，原权限核对后可resume；Close 实际join先于PG关闭。live-runtime-startup-recovery 用实际重路径／原core／PG／固定Worker，只有协议替身，已通过构造0调用、认证就绪前0调用、中断planned attempt不造Run、resume后共2真实Run、join后PG可读。固定只读 Tool 发布使用已验证共享接缝；发布与执行的 writeReady 均仍false，真实写 G-FT行为未通过，不宣称US19交付。

本增量失败历史（只追加）：

| 尝试 | 症状 | 处理／原始结果 |
| --- | --- | --- |
| query-audit-build | 新严格permit单测夹具未补permit，known/unknown失败 | 补测试许可替身，普通／Flow定点复绿；不作为PG许可证据 |
| completed-run-reuse-first | SQL误读runs.manifest_hash（实际在run_resource_manifests） | 改为既有表JOIN及真实hash核对，四分支复绿 |
| live-runtime-worker | 60s真实测试超时，堆栈为Start goroutine send nil chan | Close清r.done后goroutine再读该字段；改为启动前捕获local done，原超时日志保留；fixed及startup-recovery复绿，未放宽超时 |

公共增量回归：domain-config-unit-regression 七包全量unit通过（core／flow／provider／tool／cli／callers／app）；pg-flow-expanded-regression 真PG所有 TestFlow_通过；history-flow-protection 真PG本US两例＋既有HistoryRetention组通过：无关40天Run删除，Flow引用Run活跃与closed均保留。runtime-auth-order 定点装配／Web／原释放顺序回归通过。上述是各自当时的树，后续新增仍须绑定最终源码完整重跑；尚未全 make check、全部US19 stages、race/e2e/browser。

下一动作：完整生产caller双预算与受控写／查证G-FT行为、补claim／可信支持验证与分页进展、API／CLI／UI与OBS投影、12任务／200次容量／真实进程／浏览器，完成全部门禁后再交用户review②。未提交／推送／合并，未碰8080既有demo或发布密钥；tasks已勾正文不重写，其余未全验继续空白。

### 2026-11-03 生命周期／最终接受复核补强与HTTP投影开工

只读独立Codex在上述增量发现三处窗口，均修并独立复验：并发Close曾在真实join前清空字段，另一次Close可早退并允许Start；现使用保留的共享close-only done／runErr，停止起始标closed，所有Close等同一实际join，关闭后拒Start。Commit原有调用前Current不能封验证期间撤Key窗口，现幂等接受事实回读之后在同root提交事务内检查原Key、当前flows:write及叶子Agentinvoke；branch/subflow控制节点不伪造Agent名。prepare原用当前MaxOpenTasks作为扫描limit，降限可漏老running根；现用持久硬上界32扫全部旧根。

新证据：concurrent-close-join 真实重路径＋原core阻塞协议，两个Close不能早退、stopping／closed不重启、释放叶子后两者都join且等待期间PG可读；commit-current-key 真PG最后第三次schema回调撤销Key／移除flows:write后零产物与未接受输出，valid正对照成功；startup-lowered-cap 把两根已存在后的部署open限额降到1，较晚running根仍先blocked，手动核对后才续。独立Codex实际重跑指定app／PG测试全绿，未替代Claude设计关口或最终review②。runtime-commit-race 两包相应真实PG定点-race通过（app6.587s／postgres3.803s）。

服务构造、worker authReady、默认关闭均保持；关闭后台Worker不应妨碍旧任务停用后取消，serve即使flows.enabled=false也只做启动核对并建立旧进程已退出的join证明，不起后台池、不推进节点。原新建任务仍要求enabled。HTTP仅开始加入消费端安全投影（ports_flow.go），尚无路由／handler／app适配／OpenAPI与UI；不得宣称接口交付。Start增加仅本次调用的NewlyAccepted内存标记（json忽略，不进检查点／账本），用于后续HTTP区分新接受202与幂等事实200，依据服务端生成TaskID与接受事实比较，避免用时间推定。

当前仍未完成项不变：完整caller周期双预算、写工具G-FT行为／查证、可信claim／support／分页进展、全部API／CLI／UI与OBS、安全正文投影、真实进程、12任务／200次容量／浏览器与最终全门禁。当前新增文件只git add -N以纳入树指纹，没有实际暂存提交、推送、合并或部署。

### 2026-11-03 HTTP真实链路与CLI轻路径增量（尚未收口）

HTTP已接批准§6.6.4的9条业务＋7条管理路由，含管理任务列表／详情两条只读救援投影。业务沿原Host／Origin／身份／入口限流，scope闭集flows:read/write；管理变更只收actor。完整任务ETag绑定ID＋revision／state_version／epoch，缺失／非规范／跨任务412零写。新接受202与幂等事实200不靠时间推断；取消仍活202、终态200。严格canonical JSON＋闭集DTO拒重复键、尾随／额外控制字段。OpenAPI新增完整bundle／契约／节点／预算schema及16路由，修正深度／子数／revision允许0，输入不假定只能object。

接口增量单元 `us19-flow-http-guard-tests.log` 与全web `us19-web-contract-expanded.log`已绿。旧契约回归预期3处红（OpenAPI缺/flows、scope／ingress清单仍15项）；按已批接口加严枚举到24项业务路由，未删断言／放宽。真实链路首轮夹具编译错（CallerKindService／IssuedKey.Key），改原CallerKindExternal／Secret；第二轮揭示生产发布误用file-only decoder，完整bundle的contract遭400，改HTTP闭集bundle解析，文件仍禁合并。第三轮reader判据复用结构体导致omitempty正文残留在测试内存，换全新值并断言原始HTTP正文无sentinel；不是产品泄漏。失败原始 `us19-flow-http-real-chain-first/second/third.log` 保留，fourth及controls／admin-chain已绿。

`TestFlow_HTTPRealWorkerOwnershipArtifactAndDualQuota`走回环HTTP＋真鉴权／签Key／PG／Worker／原admission＋core＋审计消息，仅替身外部模型协议：非法schema400路径；同键202／200、异参409；仅1Task／Run／Chat；caller tasks used1、tokens used8、reserved0；Flow剩99992；同owner仅read可读状态但正文屏蔽；跨caller及管理身份冒用业务读取404；专门管理投影200仅元数据、caller403。第2根可受理，但原叶子caller周期限额阻断，零额外Run／Chat。`TestFlow_HTTPRealControlAndReadOnlyQueries`停止Worker后GET任务／产物／验证零Run、缺ETag412、cancelled、旧版本同操作200、终态resume409，零模型。不是二进制kill或最终G-FT行为证据。

管理救援不借原Key读正文；预算未提供省略而非假0，unknown查原账本。业务attempt的Input／Output本来json忽略，现进一步在消费端口前清空内存正文。独立Codex关于两字段网络泄漏的初始find在核对标签后撤回，其余HTTP复核仍进行。真实来源撤权须扩面，未据无来源样例宣称全部完成。

CLI已接list/start/status/cancel/resume同一服务API；validate目录只做同源安全Loader的结构检查，不证明发布依赖／权限。保持轻路径、不装配第二Run／PG／锁；只收受限token-file，不进argv／日志；显式server、远端HTTPS、禁止跳转携凭据；有界普通文件读与完整条件版本；新幂等键／操作ID在dispatch前显示，失败不自动换键。当前CLI只编译及既有受影响单元通过 `us19-flow-cli-first-build.log`，客户端／文件权限／离线异常／退出码具名测试尚待；未勾T021。G-FT行为、claim／support／进展、CLI／UI／OBS、真进程／12任务／容量／浏览器、最终全门禁仍待；未提交／推送／合并／tag，未触8080demo与发布私钥。

### 2026-11-03 HTTP独立复核闭合、CLI与管理台操作探针

只读Codex确认两处HTTP缺口（不是Claude行为收口）：新受理未检查caller有效Flow子容量0；resume同原操作／旧If-Match被上层状态前置拦截。现新受理幂等回读之后经原concurrencyLimit解析器的静态容量检查，不占票据，0子容量429 FLOW_INTERACTIVE_RESERVE；Current／既有任务不走该关口，降额度仍退避。Recover新增精确原请求回执查询，绑定task／caller／原Key／原Expected／reason，不把派生Status加入请求hash；全新控制仍拒终态／关闭／过期。真实HTTP新增单名额reserve1拒且零任务、reserve0接受、原resume二次仅1转移／epoch+1、异参409、取消后原回执200但不复活。`us19-flow-http-review-regressions.log`及独立复跑均绿。新增可选容量关口令旧组合夹具nil拒绝的4例回归红（reviewer实跑），给该夹具接生产同源方法值而非nil放行，已恢复并扩至7例。

CLI `TestFlow_CLIRoutesThroughService`／UncertainWriteRetainsIdentityAndRejectsUnsafeRequests／StatusDoesNotTreatStoppedAsBusinessSuccess／OfflineStructureHasNoRuntimeAuthority已通过：真实回环HTTP客户端、同源credential权限校验、禁止redirect外带凭据、重复／尾随／过深JSON零dispatch、原幂等头、轻路径零运行装配、stopped非成功、两文件离线结构与在线资格分离。首次夹具在Windows用了prepareSecretDirectory而宽松继承SID被正确拒，改用既有tightenMasterKeyACLForTest收紧合成凭据目录／文件；不改生产ACL。第二轮将一个合法精确大整数误列非法JSON导致额外一次本机请求，纠正新测试的非法样本为NaN并增加深度反例；大整数本身仍允许且规范化保精度。原始probes-first/second日志保留；`us19-flow-cli-http-reuse-probes.log`绿。

管理台新增流程／任务导航，定义元数据→导入JSON bundle→只读在线校验→版本差异→确认发布／退役，任务受理／详情／控制。API出口仍唯一ApiClient；新增rawBody使重复键／精确大整数原字节交服务端严格解析，避免JS parse/stringify先丢信息。unknown不给恢复／强制成功，关闭提示实际join；未知请求锁定输入和原ID，只GET核对控制；新受理复用原幂等键。定时2s单飞、隐藏／离开停止、终态停轮询；网络5xx保留最后成功数据，明确403/404清空旧私有正文。晚到响应在卸载后不再读来源或导航；视图Query变更重建，不跨管理／归属模式混数据。调用方Key签发选项加flows:read/write，不默认赋权。

前端首轮5例红源于新测试把含#私有字段的client/router放进Vue深代理（非生产h传props），夹具markRaw修复；第二轮发布成功文案被bundle watcher异步清空，修产品watcher，仅使旧校验文案失效；第三轮全绿。`us19-console-full-first.log`全量35文件412例通过（此后又加权限清屏1例，最终全量待再跑）；`us19-flow-console-auth-probes.log`9例及typecheck通过。具名覆盖原键查证／重复点击／完整ETag／确认取消／版本发布／私密清屏／终态与卸载／实际App路由，未代替真实浏览器。任务attempt端口改显式安全DTO，并查原Run账本给run_admitted；planned标识不生成伪运行链接。节点游标改(开始时间,ID)稳定顺序。`us19-flow-safe-projection-and-cli.log`PG／app／web定点绿；Schema编译失败另映射FLOW_SCHEMA_UNSUPPORTED。OpenAPI新增run_admitted尚待同步。

`us19-flow-expanded-after-http-cli.log`当时全部TestFlow_：app8.489s／web0.212s／flow0.217s／PG18.260s通过；仅绑定该轮源码，并非最终指纹。未勾选任务正文保持原样；API／CLI／UI还需最终完整stage／浏览器／文档。可信claim／support／分页进展、动作查证、真进程／12任务／200容量、OBS、最终七目标与review②仍待，未提交／推送／合并／发布。

### 2026-11-03 G-FT受控写文件行为增量（生产写入口仍关闭）

扩展原TestFlow_ProductionNodeUsesOriginalRunAndAudit，保留原tool／agent及各自checkpoint4例，增加隔离write／write-checkpoint／write-unknown3例。仅隔离夹具显式开authority／runner／core三层writeReady；生产wire仍false。真PG＋原ToolExecutor＋实际write_file写临时Agent自有output.txt：正常3消息／1Run／1审计／0Chat；Flow产物提交故障sequence证明触发后重建Flow内存，复用原完成Run，文件mtime不变，仍1attempt／Run／audit，未重写文件。审计success更新触发器故障后文件确已写、原审计未假终态、动作unknown、祖先工具预留unknown1；人工resume返回ErrActionUnknown且版本不增、不再调用。该探针也促使fresh Recover遇unknown直接拒绝，不再无意义写blocked／增epoch；worker关闭核对仍走独立repository语义。`us19-gft-write-behavior-first.log`7例通过4.004s。

这些不是进程kill、可信write_file查询核验或notify unknown的完整行为证明。P2／P5／P8、F04／F12／F17全部行为证据与独立复核齐前不得启用生产写路径；T010仍不勾，gft原Claude条件回执不改写。下一步继续补来源／claim与可信动作查证、实际进程窗口、UI浏览器及全门禁，不在当前局部绿态停工。

后续核对：OpenAPI已补run_admitted，管理台不把计划RunID渲染为真实运行链接；CLI增加只读 `validate <dir> --bundle` 标准输出，连接FLOW.yaml／TASK.json与管理台JSON导入，不写文件或自动发布。真实CLI命令输出bundle并回读名称／契约的断言已加入。`us19-api-cli-ui-vet.log`装载integration的6包vet通过；`us19-console-build-first.log`typecheck／Vite／integrity构建成功，3文件，JS约516kB（gzip169kB），Vite给500kB chunk体积提示但无错误，未通过提高阈值消提示。`us19-http-cli-gft-race.log`app10.821s／PG1.615s定向race绿，不替代最终全race。`us19-http-cli-ui-doc-lint.log`两文档lint退出0。

新PG节点时间／ID双键分页探针首轮错误夹具把同逻辑调用3条都置prepared，实际唯一活动索引正确拒绝；回归汇总在模型看到首轮结果前又执行，同题2次红，日志 `us19-chronological-bundle-regression.log`／full-probes保留。修为3次已结束failed历史，再判时间先于字典序、同时间ID断开、翻页无重叠与非法cursor拒绝；`us19-chronological-pagination-restored.log`绿0.508s。该修复只改新夹具，不改唯一活动约束；full-probes中的app8.200s同时验证CLI bundle、HTTP与7例原Run复用／写入。当前尚无新的完整stage／最终指纹报告，不冒充US完成。

### 2026-11-03 结论生产、来源支持与依赖闭包增量

补齐已批准 claim 目标绑定：acceptance target_kind=claim 的 target_id 引用既有叶子 ID，一节点一份 canonical 输出提议；必需生产节点须支配所有成功出口。结论／验证／证据边／产物在原 Commit CAS 一起接受，ID／状态／版本均由服务器产生。NodeOutput 内部检查点记录由实际输入映射派生的 ClaimRef 闭包；无 claim 规则的中间节点也保留依赖，可选提议不通过不能作为可信上游，未使用的可选提议不阻塞无关路线。PG 同时核对实际 producer 闭包与产物边，不能漏边后仅凭格式成功。正式 flow.md 同步，未新增 ClaimSpec、任意模型状态写接口或新 REST。

内置 basalt-source-value-v1 经原 SourceViewer／原身份读取固定资料版本，只证明完整 JSON 值相等（精确整数）或非JSON原文逐字相等；不做子串推断或通用业务裁判。作者须显式点名该语义。当前 Key、冻结集合、当前来源 ACL／版本／hash 在前后核对；旧版本、截断、未知位置不通过，独立资料按UID去重，来源可读本身不作支持证据。真实装配捕获 contract input_refs 在进入即时检查时缺 task 绑定，已在消费边界绑定当前 TaskID并拒跨任务引用，不放宽Sources校验。

只读独立 Codex 定点复核（非Claude行为回执）发现并修复四处组合：blocked验收错误新开Run／原accept操作ID冲突；无关上游跨revision被过度拒绝；可选未通过结论被无claim下游丢边；同节点claim已接受但产物尚需人工时的提交冲突。现验收待定保留原完成Run及interrupted Flow attempt；低信任输出Verified=false，已验结论仍保留Verified输出，OutputEvidence只允许当前blocked叶子的原完成Run重验。新验收操作绑定attempt＋revision／epoch／stateVersion，不覆盖旧事实；原Run／action／会话不变。旧claim绑定原output revision／attempt／hash和规则版本，保留上游在新revision只重查来源，不重新推理。人工能力缺失的记录由basalt-human-gate-v1报告inconclusive，不伪造人工批准。

真实返工还暴露模型logical slot仅按node＋round把新合法只读推理当旧成功动作：模型槽改绑NodeAttempt＋round，并在Model边界加全root unknown拒绝；业务工具稳定槽不变，未知动作不借新attempt绕过。调用与预算仍走原Provider审计及祖先分配。

证据：`us19-claims-and-source-restored.log`领域／PG六场景／来源值单元绿；`us19-real-source-claims-restored.log`真实app/SourceView四场景绿。扩展恢复场景前后红日志、跨revision缺槽红日志、组合缺ValidatorID红日志均保留，不抹失败。`us19-claim-dependency-provenance-restored.log` app7.675s／PG7.129s绿；`us19-claims-optional-and-revision.log` app11.009s／PG8.200s绿；`us19-claim-artifact-combination-restored.log`同节点claim通过＋人工产物缺失→blocked→原Run再验仍blocked且Chat只有1次，通过1.127s。此前`us19-claims-expanded-regression.log`六包全部TestFlow_绿（PG27.681s、app24.890s），只绑定当时源码，最新组合变动后仍须重跑完整门禁。测试用真实临时PG、生产装配和原审计／会话，外部模型协议仅脚本／回环，不外发付费请求。测试涵盖真实SourceView读后撤权、原完成输出重验、跨revision保留上游、未通过可选结论零下游调用、无claim中间节点传边；不冒充真实进程kill／最终行为关口。

T018仍待完整闭包／上下文验收，T010生产写入口仍关闭。下一步真实二进制故障矩阵、可信动作查证／分页进展、OBS／文档、12任务／容量／浏览器、全部stage与七目标。新出现的AGENTS.md和.agents目录按用户工作区内容只读遵循，不纳入本轮产品变更。未提交／推送／合并／tag／发布，未改变8080用户实例或读取发布私钥。

### 2026-11-03 有界返工、真实进程故障与 G-FT 行为复核增量

已发布 FailurePolicy 的固定返工目标由原 Worker 在 Advance 实际 join 后顺序推进；次数同时受节点、revision、attempt 与祖先预算限制，human_required 不自动返工，unknown 不借新 revision 绕过。特殊 bounded_rework_required 检查点允许重启继续原固定策略，其他 blocked 不自动推进。根输入变化保守失效全部旧输出／结论／产物；同输入保留不受影响上游。真实生产 Worker 三场景日志 `us19-production-bounded-rework.log` 绿3.848s；领域／PG／app汇总 `us19-bounded-rework-foundation.log` 绿。累积 Reworks 新约束同步本US原PG直接返工测试，保留其进展历史不清零断言。

真实候选二进制12窗口（accept／claim／dispatch事务前后、接收端ACK、原审计终态、Flow结果提交、revision事务前后、取消意图到join）完成；每窗真PG故障标记、真进程终止与重启、原动作／Run／预算核对。`us19-real-binary-crash-twelve-final.log` 12/12绿、整体23.661s，绑定当时普通二进制SHA256 `82ab45c812bc32f76883fe63076fd7b1ba8c83ffa5a197b19b8958b1f78f8e1b`。此前挂起日志 `us19-real-binary-crash-twelve.log` 保留：接收端未读完请求体且关闭等待已取消context，改为读完并显式释放；两处后续红态中revision前提交由原固定策略自动恢复，不再错误手动resume竞争；Flow取消后的原FinishRun仅在Flow路径使用5秒有界WithoutCancel清理，普通Run不变。专用单元与最终12窗验证通过。

真实写节点在隔离复制源码构建，只开wire三个写就绪位，无产品后门／新配置；二进制SHA256 `336675bc5921b06112d018f493a474c74be1db995b391a7d52911fc000c88ae3`，登记 `.loop/us19-gft-probe-build.json`。`us19-gft-real-binary-write-probes.log` 两窗绿3.451s：文件副作用发生但原工具审计未终态→unknown禁止重放且预算保留；原审计已成功但Flow结果未提交→复用原Run，mtime及Run/action/attempt/工具调用数均不增长。隔离探针不是发布候选证据。

独立只读 Codex G-FT行为复核（实际Claude设计回执仍独立保留）结论：可闭合行为关口，允许同三处生产开关开启进入普通候选全验收。复核者独立复跑四类预写故障零外发、普通core/tool/callers兼容、真实写节点两窗、普通二进制12窗21.436s与claim通过但人工产物缺失组合；未发现新阻断。2026-11-03据此打开wire三位；T010仍待正常候选完整stage，US review②未通过。

清理限制：中断挂起的自有测试后，已核对并停止自有e2e／子进程，精确回收仅匹配task `ft_4ac5ddaaf2822abc6a6b1d4bd1005b54` 的临时schema（输出own_interrupted_schema_removed=1）。自动审批拒绝递归删除两处自有遗留目录，仅返回“blocked by policy”：`C:\Users\76562\AppData\Local\Temp\TestFlow_CrashWindowMatrixdispatch-after-commit2132825100`、`C:\Users\76562\AppData\Local\Temp\basalt-e2e-build-3016997350`。未换等价命令／工具重试，目录保留；不影响其余实现。

### 2026-11-03 真实来源进展判定增量

生产进展 Proof 只接受原action＋原工具审计已确认成功且输出hash一致的调用，额外来源核验在短事务外进行；提交同一根检查点。内置真实read_file通过原policy再读取当前内容并取稳定文件身份；内容／时间变化、同文件硬链接别名不生成新来源，未知／失败／不支持适配器不生成进展。原scope／Key／冻结资源在核验前重查。新文件身份测试及真实ToolExecutor＋PG的7独立来源读取／同文件反复改写测试已执行。

首轮 `us19-source-progress-production.log` 红：原ActionOutcome为success而非Flow持久态succeeded，导致合法新来源均未计入。修正该接线字面量，未改熔断阈值；`us19-source-progress-production-restored.log` tool0.243s／app3.431s绿：7真实独立来源可完成、8次模型；同文件改写第三次相同调用发出前拒绝，仅2次实际读取且来源仍1。先前 `us19-trusted-file-progress-first.log` 五包定点绿亦保留。尚未声称所有合法分页适配器、负载、浏览器或最终全门禁通过。

### 2026-11-03 观测、公共门禁与交付文档增量

P15新增可选只读FlowFactsReader。flow_task_id是独立于调度task_id的真实查询锚点，只展开原Run／attempt／caller三者互证的已准入运行；Timeline元数据附task／root／node／attempt／revision／epoch及原工具审计关联的action，不含任务正文。会话／Run查询自然带同一元数据，不伪造诊断事件、Run或派生执行。新增两个固定数据库窗口指标，身份不得作label。OpenAPI、管理台类型／入口／技术详情同步。真实生产HTTP＋PG查询三次不新增Run／Chat、不改变版本；caller key访问管理观测403。`us19-flow-observation-first.log`仅新指标测试夹具漏MetricGauge红；修正夹具并按label断言真实样本，未放松标签闭集。`us19-flow-observation-and-dependency-restored.log`验收脚本1.728s／OBS1.137s／web0.378s／app11.889s绿，包含实例密钥历史回归。

公共make check诊断四轮：第1轮格式扫描命中本轮隔离副本及新PG分页测试；本轮副本和清理Go探针已移动至仓库外 `D:\code\basalt-us19-test-probes`（拒绝删除的那两处临时目录仍原样保留），探针JSON只更新路径，hash不变；分页测试仅gofumpt本轮文件。第2轮staticcheck指出本US并发测试channel range中defer，改t.Cleanup保持名额判据；第3轮gosec21项为本US未处理Rows.Close与动态WHERE拼接，逐项合并关闭错误，改固定SQL＋布尔参数；未加抑制／未缩扫描面。第4轮格式／lint／安全绿、race全包中只有公共依赖断言红，未进入集成／cover／build，不称make check通过。

依赖判据按已批准plan P08／TechnicalDesign §1.3的jsonschema/v6生产适配器扩展同步：原测试名保留，唯一允许位置internal/tool/schema.go，其他生产导入仍红、仅准v6且必须真实链接；额外断言testify不得链接。新加严发现旧 `instance_secrets_perm_test_windows.go`／`_unix.go`不以_test.go结尾而进入生产图，已等价重命名为 `instance_secrets_perm_windows_test.go`／`_unix_test.go`，测试正文不改。首轮新加严红日志 `us19-flow-observation-restored.log`保留，修后上述dependency-restored全绿。不改已批准模块范围，不将测试框架留在发布面。

配置样例／Docker模板默认关闭Flow，Operations追加迁移v8、容量与交互预留、定义导入发布、CLI同实例、取消／恢复、unknown及保留引用说明，README和官网中英文边界同步。十二份SVG和十二个drawio页面增加同义Flow边界，修旧SQLite运行期文案；TechnicalDesign的SVG／PNG也同步。图元provenance指向真实main基线ca9a0d2，data-working-tree-feature明确US19尚未提交，未谎称基线commit已有Flow。首次脚本经PowerShell管道丢中文编码，渲染发现问号后以UTF8重写并复看；PNG由临时工具缓存resvg生成，未加产品依赖／未冒充drawio导出。官网20测试全绿；图／链接／浏览器最终复验待办。

### 2026-11-03 容量排障（debug轮次1，尚未通过）

固定真实二进制测试保持200独立任务、20节点、4active、32open、50ms回环模型；4000叶子必须全完成，受理p95≤2s／读p95≤1s／单独4根空队列cohort的80节点ready→start p95≤1s不改。收集分位、排队、RSS、共享测试库连接与本schema锁；单独cohort避免把有容量排队的等待算执行慢。原测试Key的周期配额由1000调整为10000供4000叶子使用，不改产品缺省／指标门槛。

调查日志全部保留：`us19-capacity-200-first.log`45.78s读401；`us19-capacity-200-diagnostic.log`48.33s意外blocked；`us19-capacity-200-debug-boundaries.log`92.28s一任务advance_unavailable；隔离stack overlay `us19-capacity-debug-stack.log`58.29s两任务leaf_execution_unconfirmed。未经根因确认不改执行或认证的失败关闭规则。线程栈显示重复资源／权限检查、PG新连接与查询等待，非单凭最终401推定Key被撤销。临时overlay在仓库外、仅诊断构建，不作为正式容量证据，不进产品源码。

假设1：20节点相同固定Agent在同一权限边界内被放大核对20次，导致无必要PG往返／建连。确定性红测试 `us19-capacity-authority-multiplication-red.log`逐字命中“should have 1 item(s), but has 20”；CheckStart按完整AgentRef＋tool＋write在本次调用内去重，下一次仍重新查权，撤权必拒绝。首段绿后真实200负载 `us19-capacity-200-authority-dedup.log`仍红（105.33s、accepted39／读401），证明仅该段不足。进一步红测试 `us19-capacity-current-key-multiplication-red.log`证明Current仍按同名Agent重复21次；本次Current按Agent名局部去重，下一边界不缓存。`us19-capacity-current-key-multiplication-green.log`真实app授权／进展／原Run组绿7.015s。此前隔离错误overlay `us19-capacity-debug-errors.log`197.60s推进到64任务（32 succeeded＋32 running）读500，当前正在通过HTTP错误overlay查第一失败位置，尚不声明完整假设成立或容量通过。当前无第2／第3个已被推翻假设，不以测试次数冒充修复次数，也不重置已有失败记录。

### 2026-11-03 进展原子替换复核处置

独立只读Codex复核指出物理ID会因原子替换变化，且审计后重开可能归入新身份；已确认major并修。同唯一Bind前捕获原pin／AgentUID／规范input对应路径hash＋物理IDhash＋内容hash，原审计确认后前后相同且真实Result内容相符才出证明。原Result继续文本投影，无新执行路径。文件proof持久按路径或物理ID任一已见都不重置；无进展的别名观察也记住，避免“硬链接B读过后换inode”绕过。Windows规范路径大小写归一。原工具权限失败仍由唯一ToolExecutor记录原拒绝审计，缺可选proof不会伪装新来源。

新增真实同字节原子替换（断言物理ID确已改变）、审计后替换不出proof、core路径／物理双去重及别名后替换反例；`us19-source-progress-replacement.log`tool0.257s／core0.233s／app2.983s绿。独立复核认可修法方向，指出前后快照不是同句柄的形式化ABA保证，但在累计双去重下未发现可转成该no-progress绕过的新反例；不把该限定写成消除所有文件并发竞争。P15本轮无新明确阻断，非PG定点测试绿，不替代最终PG／容量／US验收。

### 2026-11-03 容量根因闭合与32任务启动核对

容量debug轮次1假设2获得真实系统错误：`us19-capacity-debug-http-error.log`（413s，144受理／112完成）中的PG连接失败为Windows临时端口耗尽，随后原Key查询失败关闭为401、状态读500，并非Key真的被撤销。原PG池默认不限制最大连接、只保留2空闲连接，四Worker大量短查询造成反复建连。仅Flow enabled且未显式配置时，复用原池设置max=32、idle=max；显式配置与Flow关闭保持原语义、不预热、不另开池。单元`us19-flow-pool-unit.log`绿。精确自有临时schema清理因同一端口耗尽失败，端口回落后以原task身份核验并回收，`us19-capacity-schema-cleanup.log`输出own_capacity_schema_removed=1。

正常源码、无overlay候选SHA256 `12ea2283f1b45baa3204a395f808a9a98cbd9b0066d9a5c508473fe121e92930`，`us19-capacity-200-pooled-normal.log`通过：200任务×20节点、4active、32open，另4根80节点空队列cohort；失败率0，受理p50/p95/p99=117.966/159.454/164.445ms，读15.520/21.848/26.884ms，空队列ready→start41/48/53ms，有容量排队首节点2722/3251/4136ms；峰值RSS51154944字节，共享测试库峰值10连接，本schema关系锁185，负载399.454s。仅为隔离回环基准，不声称生产SLO／模型质量；源码随后变化须最终候选重跑。假设2由故障证据和同维度正常通过支持，本题未进入第二debug轮次。

补轮询查询回归：32相同定义任务的真实PG语句原65次红（us19-root-poll-query-red.log），改为一次完整任务批量读取、关闭游标后按UID/version局部读取不可变定义；仍逐条校验契约／输入输出hash／完整checkpoint，不缓存权限。单连接池回归与Worker组`us19-root-poll-query-green.log`绿1.797s。新夹具最初比较发布前非canonical RawMessage与PG解码表示红，改为对同一持久定义逐字比较，不放宽语义或查询数。首次错写-run导致no tests to run的输出不作通过证据。独立只读Codex核对池默认、完整checkpoint及先关游标，无新明确阻断。

正常候选真实写两窗再次通过。32-open重启首轮（us19-normal-write-and-restart.log）发现原审计unknown后Flow动作仍dispatching，虽任务安全blocked但unknown指标少计；在原恢复之后的Park短根事务传播原unknown审计到同Run/attempt动作及预算，只改unknown事实、不释放预留、不把成功审计猜unknown。修后`us19-normal-write-and-restart-restored.log`普通候选SHA256 `cd8c4bd6825343a4800ad7889bea13e77f0b611a63d3ed74a5eff61dcfca3142`绿10.698s；32open在1637.526ms核对完成，28安全完成、4unknown阻断，33次模型=1warmup+4中断+28安全、零重放；两写窗仍各一Run/action/attempt/工具审计，mtime不变。unknown阻断只算核对完成，不算业务恢复成功。最终SC004／全部stage／完整七目标／浏览器／12组真实对照仍待完成。

### 2026-11-03 默认时钟、固定对照与游标机制增量

SC004真实默认时钟：原Chat协议收到取消但测试接收端刻意不返回；30秒Cancel返回cancelling，35秒Worker日志报告超时，Close仍不返回、真实runner.Joined=false且PG可用；显式释放后才join。首轮用例主体正确但公共夹具清理仍要求Close无错误，导致预期超时错误在cleanup判红。给夹具增加仅该探针显式设置的expectedCloseError，其他用例保持NoError；日志Handler用通道观察超时而非并发读bytes.Buffer。`us19-default-lifecycle-clocks-restored.log`绿36.537s：取消30009.722ms、排空超时35070.507ms。原取消终态写独立5秒deadline由core专用测试核对，不把30/35秒测量说成实际花满5秒审计。

F19冻结12任务资料、预期、模型脚本与单Agent基线。真实生产装配/唯一Run与审计/真实PG共24条对照；真实模型未授权，不外发。本轮初始编译错误使用不存在RunResult.Text，修FinalText；分支夹具先缺MaxAttempts、再缺闭集Schema、再使用非枚举自由文本条件，均被实际发布验证器拒绝。最终使用作者固定budget.remaining_tokens数值分支，未改验证器。预算耗尽探针发现模型发出前拒绝被原loop统一投影failed；新增标准库core.ErrFlowBudget同义接缝，只在Flow且确定未发出时投影limit_reached→Task stopped，已外发/unknown不改。工具预扣拒绝同理。第一版脚本对未知写未遵从repeat=false造成普通基线10次写，修脚本只发一次后结束；该行为不声称普通Run已拥有Flow未知动作防重放机制。

`us19-frozen-comparison-and-alias-chain.log`绿18.220s；独立只读复核指出illegal_calls=0固定话术、仅计数未对节点/参数、NULL token被误作零成本。已补逐条原tool审计名称/规范参数/次数与实际原Run节点身份，未知写两臂各一次、排他分支只decide/chosen、交接两Agent和独立会话；报告从断言结果计算。成本拆已知token小计及未知用量调用数，未知/矛盾用量后验不通过；加载真实baseline上限，但明确普通Run沿既有配额，不拥有Flow祖先预扣。`us19-frozen-comparison-callset-reviewed.log`24条绿48.552s，12/12预期命中、错误成功0/非法调用0；取消两臂各1次未知用量，不再说零成本。仓库外overlay逆改为重复写，`us19-eval-duplicate-write-red.log`逐字命中“actual original audit must match frozen tool name, parameters and bounded dispatch set”，判别力成立；该overlay不进候选。

生产文件进展补真实硬链接/原子替换链a,b,b,c,c,d,d,e：7次实际读取后NoProgress=6，第8次发出前拒绝；累计别名不产生新来源。采集前已进一步改为仅metadata不读取正文，审计后才核对完整真实Result，旧记录中的“前置内容hash”不再是当前实现。`us19-trusted-cursor-pg.log`绿3.292s：原ToolExecutor/原两段审计/真实回环页及PG，对逐页重建guard的单调游标、重复、倒退、伪造来源和伪造审计身份判卷，跨revision/观察写失败另外两项同时绿。该注册协议只在测试中提供；不新增公开分页工具，也不宣称生产支持任意MCP分页，未注册协议没有proof。

七目标再次执行：格式短函数因gofmt/gofumpt对相邻长单行函数来回对齐产生非幂等，改测试Description为两语句函数消除格式冲突，未抑制扫描。`us19-make-check-after-format-cycle.log`fmt/lint/sec/普通race绿；全量integration红4项，都是已批准v8与17新表未同步的旧v7/60表/版本链断言（app536.451s、PG340.530s），cover/build未执行。随已批迁移适配：保留旧54普通表和6OBS表断言，新增17Flow表确切名字及v8脚本链；SQLite仍6。`us19-v8-legacy-assertions-restored.log`app1.472s/PG0.561s绿。当前完整七目标重跑中，不冒充已通过。

Stage已派生us19-base（11checks/279具名测试）与us19-process（真二进制5顶层，含12故障窗/两写窗/200容量/32重启），公共full不可替代。前端最新构建JS index-Bkm23XMx.js，官网build/check通过；12SVG渲染联系图已人工查看，修部署文字中的嵌入PG/旧basalt.db误述，同步drawio。临时Python无PIL时改本机System.Drawing生成联系图，无产品依赖变化。浏览器工具实际inventory为空且iab创建失败；已异步请求启用Chrome或明确允许测试无头浏览器，继续其余独立工作。未提交/推送/合并/tag/发布，不动8080用户demo和发布密钥。

### 2026-11-03 最后独立复核处置与验收入口冻结

补齐12条完整作者定义模板，独立严格解码definition/contract；初轮错误把内联contract送作者decoder被拒，另旧CRLF原字节与归一LF pin不符。两处夹具接线修正，不改严格decoder或原冻结业务判据；`us19-frozen-definitions-comparison-restored.log`24条绿65.155s，材料冻结亦绿。此前完整make因当时旧夹具/旧pin判红（app600.639s），日志`us19-make-check-v8-adapted.log`保留，未称七目标通过。

公平预留新增真实二进制的实例2/同caller2两条，3个Flow根等待时普通消息仍获预留，随后全部排空；原yield历史保留，当前checkpoint尝试不消耗。`us19-interactive-reserve-fairness-restored.log`绿11.416s。真实CLI持同一服务工作区锁完成list/start/status和同键重试，只一Task/FlowRun；首次广ACL token文件被正确拒绝，改隔离夹具目录严格ACL不放宽产品权限，`us19-cli-real-binary-restored.log`绿4.415s。正常二进制SHA2f0989bd259cfac9b5122efb8c11b7679adb38e251a370843f94a581e8ad048f。纯领域外部包真实PG/原Run公开生命周期、rework/subflow/Worker探针，`us19-flow-domain-public-coordinator-final.log`绿7.025s，领域包覆盖61.4%；早期夹具Next/retire参数/Retryable/列名/context取消错误已保留失败，未改产品判据。

最后只读独立复核发现F18实际缺陷：API关闭意图none字符串被前端当truthy，造成恢复入口消失/误报关闭；真实形态反例`us19-real-close-intent-red.log`1红12绿，改闭集类型与显式比较，并修夹具不再空字符串；全前端`us19-real-close-intent-restored.log`417绿，build新JS index-CW0IF1MW.js，integrity3文件。F13/F17是验收缺口：新增生产Worker/Run实际ChatRequest断言完整硬约束/未解facts/revision0→1，损坏摘要及不可裁上下文超限原模型/llm审计均0，`us19-hard-context-boundaries.log`3.603s绿；首次错误角色常量编译红保留。新增真实已填数据v7→8，原sessions/messages/runs/llm_calls/tool_invocations全值JSON等价，原Run两新列NULL、pending/unknown不改、Flow任务0且重跑幂等；`us19-v7-execution-upgrade.log`0.702s绿。两项已进入F13/F17及base/race具名清单。独立复核关闭三项，无剩余明确blocker/major；它未运行完整PG/容量/浏览器，不作为最终门禁通过。

新增acceptance-map.json十九组实际名称，并以真实tasks.md核对T001–T028都绑定本stage执行项；`us19-stage-mapping-final.log`绿。CI新增只读flow job：隔离PG/扩展、本地脚本模型、两exact stages、同源普通候选、失败证据上传；无密钥/模型费用/推送/部署。尚未推送，GitHub Actions未运行。全局stage lint第二轮编译时碰到尚未修正RoleSystem的夹具编译错误，记录于us19-process-stage-lint.log；修复后须重验，不改历史stage WARN的真实性。

容量探针补OS/架构/CPU核数/PG版本/配置hash、CPU/RSS/goroutine/连接的10秒实测序列，以及本schema锁等待连接/最长观测等待，不用峰值替代资源趋势；宿主型号/总内存另在us19-host-metadata.json，无凭证。官网最终20单测/build/check绿，最新12SVG重渲染。当前准备稳定源码最终公共full、两stage、完整e2e；浏览器环境仍待异步回答，不伪称通过。未提交/推送/合并/tag/发布。

### 2026-11-03 七目标通过、公共复跑发现的排空错误及浏览器实测

`us19-stable-make-check-restored.log`：真实make check七目标exit0，原阈值全部达标，Flow61.53%／PG75.63%／tool83.17%；真实integration app498.658s。Linux限定Flow core/tool/flow/cli的-race亦绿（us19-linux-flow-race.log）。最终stage lint exit0（us19-stable-stage-lint.log），历史us8/us9缺失候选名称仍WARN，不修改历史消警告。spec/plan/tasks归一正文hash与原冻结值完全相同。

随后公共full复跑在真实SC004发现新产品缺陷（.loop/runs/us19-stable-public-full/test-integration.log，app466.172s）：已等待真实join且35秒超时有日志，但worker的errors.Join(context.Canceled, drainError)被flowRuntime.Close的errors.Is(context.Canceled)吞成nil。新增错误链三组合/重复Close反例，us19-drain-cancel-joined-red.log精确红；新增ErrDrainTimeout并包装排空错误，Close只在不含此哨兵时抑制取消；us19-drain-cancel-joined-restored.log：race＋真实30/35秒＋并发Close绿38.034s。独立只读复核亦独立运行无库三组合绿并认可修复。未放宽30/35秒或join判据。该full父进程PID36960按精确命令匹配后停止，已编译的PG子测试允许自行清理；session40527退出-1，不声称完整full通过，不用其后混合源码输出作最终证据。需在新源码上完整复跑。

用户安装扩展后Chrome真实可用（profile shun，browser id1），单独标签1558513127、隔离候选2ef4182c…／57951端口；原8080实例未动。实际登录、导航、启动成功任务ft_5465fe…，展示10.838秒、真实原Run链接、已记录产物与shape验证；缺证据任务ft_0c472e…真实blocked、facts未通过、恢复按钮出现（none修复有效）。因初始测试契约仅60秒，后续超时不当成恢复通过；完整UI旅程仍须新夹具复验。测试专用UI overlay在仓库外，仅普通候选，未作产品修改/故障后门。结束释放标记后fixture正常cleanup，us19-ui-review-fixture.log exit0，仅为环境记录，不冒充UI全部通过。

浏览器发现F18文案歧义：完成且空node的任务列表误标尚未开始；节点未接受/已记录产物混同执行中断/整任务验收；artifact witnessed=false一律误报权限。新增真实消费边界反例（us19-artifact-unconfirmed-reason-red.log），区分有授权但依据未确认artifact_basis_unconfirmed与current_source_access_required，两者仍省略正文；三组来源/HTTP回归us19-artifact-unconfirmed-reason-restored.log绿13.367s。前端反例us19-route-artifact-wording-red.log保留，首次修订漏import/未替换record变量的编译红保留，修后全前端us19-browser-wording-console-final.log绿418项/build成功，阈值和权限边界不改。API/契约同步解释产物版本记录不等于整个任务通过，版本hash计数不当作独立来源数。准备新普通候选及更长本地UI验收契约，随后重新冻结公共full及两exact stages。

### 2026-11-03 Chrome闭环及页内确认（最终门禁前）

普通候选762fc73d…／55728的实际Chrome闭环：发布v2、成功任务ft_17284e…；缺证据ft_a3b372…恢复后仍原Run flow-e6de40…、模型1次、剩余99992 tokens；取消后状态7／epoch2为cancelled，原产物与两轮验证保留。流程任务ID观测关联显示原Run10.1秒／唯一模型10秒，不凭查询推进。截图us19-browser-{success,blocked,cancelled,flow-observability}.jpg。原生confirm使扩展连续超时，用户协助确认发布／恢复／取消；用户问为何弹窗后，核对R3 §6.6.2及FR024只要求二次确认，未限定原生形式，等价改为页内影响与理由确认，保留权限、审计、原版本条件、单飞及不确定写锁。未知结果离页保护仍保留，不自动绕过确认。

前端正常在途unknown预留不再误报人工对账；节点耗时改为“节点跨度”并明确含暂停核对，原Run实际耗时不变。us19-inflight-wording-red.log保留，419项恢复绿。页内确认对发布／退役冻结原bundle及预期版本，对任务操作冻结理由和完整ETag；轮询改变版本则拒绝确认，返回／Escape零写。us19-inline-confirm-console.log全421绿、build/integrity成功；独立只读复核定点17/17与typecheck绿，无blocker，并补管理放弃独立两步／单飞／隐私用例，us19-inline-confirm-admin-final.log18/18绿（最后全量将在base stage重跑）。

新普通候选SHA8b550fdf55cb90a6339abe5c0ff2e8beb0752fdb9d21a2acf8195707c085990e／59842真实走查：在线校验→返回不发布→页内确认产生v2；成功任务ft_72e8fd…验收通过、产物可读；缺证据ft_ccf9ee…暂停→返回不恢复→确认恢复仍原Run flow-d3e18b…、1次模型／10秒、原Run10.11秒→页内确认取消后cancelled；退役缺证据定义后显示历史保留。全部无需新的原生确认框。截图us19-browser-{inline-confirm,task-inline-confirm,final-success,final-retired}.jpg已实际查看。原生文件选择器受Chrome扩展本地文件权限限制，未验证真实文件导入；JSON文本导入与完整发布已验证。仅本地脚本模型，不声称Q5通过；8080未动。两次fixture均正常退出cleanup。接下来冻结源码，完整公共full＋base＋process＋受影响e2e，仍未提交／推送／合并。

### 2026-11-03 R3范围最终验收；用户授权US19体验补强（不收口）

R3稳定树指纹24fb892cd7a423fd0d28010f76641af7b2174e640c74b20e777502b2c41d6e39、普通候选SHA8b550fdf55cb90a6339abe5c0ff2e8beb0752fdb9d21a2acf8195707c085990e。公共full 11/11 exit0（.loop/runs/us19-final-public-full/report.json）；原七目标包含在内，app74.71%／core78.19%／flow61.44%／PG75.63%／tool83.17%，逐包阈值未放宽。base 11/11 exit0，290个必需测试条目命中，Go通过集合含子例624条、零跳过；前端35文件422例通过，官网links／diagrams／build通过。process 1/1 exit0，7个必需顶层／23个含子例通过、零跳过。三份报告同指纹；F01–F19全部实际锚点复核无缺失（.loop/us19-final-behavior-coverage.json）。

SC002：200×20节点、4 active／32 open／50ms mock，330.214s；受理p50／95／99=94.813／123.667／154.525ms，查询12.439／16.504／21.767ms；空调度四任务80节点队列外启动28／36／120ms，负载首节点排队2035／2959／3595ms；失败率0。RSS峰值59,064,320字节；33个趋势采样RSS约50.2–56.3MiB，goroutine22–26，资源采样连接9、频繁读取最大共享测试库连接10；本schema关系锁峰值192、等待连接峰值3。56.172ms是观测到等待锁的查询从query_start起的已运行时间，不是精准锁等待时长。CPU为进程累计CPU差／墙钟差×100，按单核为100%，首样本NULL，不冒充整机占用；此短测不证明长期无泄漏。原始曲线、PG17.11、Windows amd64／20逻辑核、配置hash见us19-final-capacity.json及host-metadata。SC003：32 open，28安全完成／4未知阻断，重放0，核对2146.021ms；instance与same-caller两种交互公平检查均通过。SC004真实30／35秒及先join后关库在公共integration和base-race均通过。12项冻结对照两路径共24条结果，预期12/12、错误成功0／非法调用0；真模型效果未测试（us19-final-comparison.json）。

完整make e2e首跑失败：204通过／30跳过，11失败事件含父例，均为原US18演示占8080导致固定默认端口用例拒绝或误读旧服务；源码未改，日志us19-final-full-e2e.log保留。用户明确批准临时停应用后保留PG／卷／身份并恢复；仅docker stop应用容器，不停PG、不down、不删卷。重跑us19-final-full-e2e-8080.log：exit0，951.592s，216通过／29跳过／0失败；全部7个US19顶层再次通过、零Flow跳过。29项逐名保留于summary：1个仅子进程helper、1个Linux宿主、10个US17制品／升级专用、17个US18容器项（Windows PATH无Docker；WSL Docker不等于Windows工具可用），均不冒充本轮通过。既有US18 CI证据不覆写；本轮容器全回归仍未验证。finally恢复原baf2929a…容器及basalt:0.0.4-linux-amd64，原卷引用不变，healthy，/api/v1/health 200。首次inspect模板对bind mount取Name失败，改json Mounts读取，未改容器；首次误查/health返回404，实际健康端点200。Linux amd64／arm64、darwin arm64 CGO0静态构建通过，不冒充这些宿主运行通过。远程CI未推送未运行。

据此仅勾选原R3 T014／T026／T027；T028保持未完成。用户随后明确新要求：“管理台页面‘工作流’的优化、可视化编排、演示的agent和工作流这些要在us19完成”。因此旧R3通过不等于本US可收口：新增体验／业务演示纳入US19补充设计，用户保留最后人工review②；未提交／推送／合并／tag／发布。用户确认演示长任务为十几至30分钟，尚无内部资料。候选演示改从公开真实材料出发，当前推荐招标机会评估与应答材料准备；资料及候选比较见.loop/us19-workflow-scene-review.md，尚未实施。Q5真实模型效果仍待明确模型／资料／次数／预算授权，不能用本地脚本结果宣称专业分工优于单Agent。公共报告之后仅追加证据与原任务勾选，产品候选hash未变。

### 2026-11-03 R4 补充设计与真实模型授权

用户明确要求体验／编排／业务演示在本 US 完成，并询问是否需要提前授权以便休息；已明确回复“授权，按上述模型与上限执行”。授权为复用本地百炼凭据，qwen-plus，最多90次对话、累计输入100万／输出8万tokens、含向量化总费用30元、单任务30分钟，仅选定公开招标资料／可公开Basalt资料／明确合成案例；不购买、充值或切换收费服务。调用前核对实际计价，预算无法核实或即将超限停止。已通过仅输出元数据的本机检查发现 text-embedding-v4 的受保护凭据存在，8080 US18 demo当前对话连接为空；未发送真实模型请求，未输出或修改凭据，未修改US18实例。

控制侧保存 .loop/us19-r4-input-baseline.json 与 tasks-pre-r4 快照，追加 experience-design、FR026–031、T029–036、plan与模块卡§6.11。无原已勾正文改写；R3原指纹／报告保留。当前拟采用两个只读作者接口、原发布写链、Vue/SVG顺序编辑及用户主动本机草稿，不新增表／迁移／依赖／执行模型。下一动作：派生正式契约／stage并独立只读设计复核，analyze无CRITICAL后实现。最终review②仍由用户，无提交／推送／合并／tag。

R4独立只读设计复核中间发现两项major，均在实现前处置到设计／正式契约：完整定义改FlowAuthoringVersion.bundle_json字符串载体，不经response.json数值舍入；明确已发布规范化语义不承诺原排版。真实模型各组隔离工作区／新session／限定collection／合成核心记忆，冻结全部实际外发上下文，出口canary校验。memory_enabled歧义改memory_scope三值，说明仅层选择配置；Flow核心记忆原注入可用，不宣称recall_memory归档召回已适配。尚未真实调用模型，仍待最终设计回执后实施。

R4设计复核最终回执：obs_input_review 对 experience-design SHA6a17f61d3378bb35756cd553bc9a9bec051d5cfd1529a3b5d9604a3c41a9389a 确认 blocker0／open major0，未运行实现或外部请求。另一只读复核确认原核心记忆和知识检索注入可复用，但归档 recall_memory 不在可信 Flow adapter 中；演示必须用隔离的 Propose→Confirm 记忆及回执，逐页覆盖账本不能用几次检索代替，三组对照共享总费用账本并披露各组上下文差别。后续演示落实，不宣称已通过。

T030 两个作者只读接口已实现：原注册表／生命周期／资源快照固定 Agent 引用，缺失与停用不准入，不调用模型或业务工具；版本正文以字符串保留大数。us19-authoring-red.log 保留缺功能404红态；夹具禁用操作ID缺op前缀、web测试未接testAdminAuth造成500均修正测试装配，不放宽产品守卫。us19-authoring-green-2.log exit0，四个具名顶层及鉴权子例通过；完整门禁尚未复跑。

T031先补编辑器边界反例。首次npm调用被PowerShell的ErrorActionPreference截断，改Continue保留真实退出码；us19-editor-model-red.log 六项明确红态：非JSON空白、四类损坏输入映射、required引用缺失字段。修复假设为严格JSON空白、转换前检查映射结构、简单字段模式拒绝损坏Schema，保留原始高级内容与已有断言。下一步复验纯模型并实现可视化界面。

T031初次纯模型18例恢复绿；新增画布、字段映射、顺序调整、单工作流导航、原文下载／导入、精确版本编辑副本、标准任务输入表单；us19-editor-journey-first.log 三文件42例绿，typecheck/build通过。独立复核定位major：最小空缺step-N会复用已删除节点ID，新增不同Agent后重排使旧断链隐式接通。us19-editor-reused-id-red.log 精确1红18绿。修复采用随机稳定ID并核对当前／被引用ID不冲突，导入ID不改，新增删→加→重排反例；旧测试改读取实际生成ID，不改变引用保真和上游限制判据。当前尚在复验，未声称R4完整通过。

独立复核定位另一major（原R3共享前端逻辑）：后端ErrCommitUnknown以409/FLOW_STATE_CONFLICT返回，前端按所有4xx清未知写锁，会允许更换操作ID。us19-unknown-409-red.log 发布／退役反例两红22绿。新增共享明确拒绝码判断，状态类别不作为提交确定依据；发布／退役／受理／cancel、resume、abandon保留未知原键，受理仅复用原请求查证。新增六种动作的409路径断言，不改后端枚举／审计／重放契约；正在复验。

### R4 体验续跑（2026-11-03 21:46）

- 独立只读代码复核发现旧节点 ID 被删除后复用、未知提交 HTTP409 被当确定拒绝两项 major；各自真实红态保留 `.loop/us19-editor-reused-id-red.log`／`.loop/us19-unknown-409-red.log`。现随机稳定 ID 不重用；发布／退役／受理／控制按显式确定拒绝代码判断，unknown 保持原请求锁。身份失效另清除定义和草稿。
- `.loop/us19-editor-identity.log`：三个前端文件 51 tests PASS；`.loop/us19-editor-build-final-ui.log`：vue-tsc＋Vite＋integrity PASS。候选正常二进制 SHA256 `9dc225c5dff2aae2c6e20a5105bb8808485c9c880f92973d0f7e80bcbc3c7add`，未覆盖 bin/basalt.exe。
- Chrome 在隔离正常二进制 `127.0.0.1:50302` 完成无 JSON 新建两节点、连接输出、反向重排提示失效且清除发布校验、移回在线校验、页内确认发布、结构化任务输入、原两次 Run 和产物验收。task `ft_40d523c3e869b805fe71eacbedefb88a`；截图 `.loop/us19-r4-browser-task-passed.png`。模型为明确的本地协议夹具（各 10 秒），不是业务真模型时长或效果证据。
- 原公开 PDF 物理 p38 §4.9.4 限制第三方转发；独立来源复核后排除模型输入。尚无真实模型／向量外发。改用独立合成企业采购需求＋main 公共产品资料＋OGL v3 参考，未降低事实、覆盖或对照判据。细节见 experience-design §4。
- 真模型预算网关初版非联网单元 6/6；并发全局计数、预占持久化、未知用量停发、模型/来源限制已测，独立复核进行中；尚不声称已验证实际计费。

### R4 真模型首轮与预算停止（2026-11-03 22:28）

实际官方计价已核对，授权网关按保守区域最高费率预占、落盘及 usage 结算；12 个离线测试覆盖越权、全局并发预算、未知用量停发、来源哈希、密钥/canary 与导入未知禁止重放。前置失败保留：启动 Python launcher PID 不符（无外发）、AGENT CRLF 装配失败（无外发）、刷新目录短暂不可见、embedding 批次超过 10（第三批本地拒绝）。仅重建隔离夹具，全部付费用量账本保留，不清零；第一批 2 次 embedding 333 tokens 亦在累计中。

隔离实例 60481，11 份资料 active、5 份合成核心记忆经原 Propose→Confirm。scope-A 原 Run web-1793715222919-7hkbee，15.353 秒、2 次对话、1 次 read_file，原结果保留。长任务 v1 在第 7 章发现交接仅 result，遗漏 sources/open_questions；显式取消 ft_c098996ae5600df12c5892c3c6b6b077，保留全部消费和原 Run，未恢复重放。v2 改完整 report 对象、四组完整矩阵附件；首个定义超过全 JSON 1024 属性上限被拒，删冗余输入映射后发布，未放宽守卫。

v2 ft_8cc01dd0fc8222b83fb60359e66e0f33 在 technical_08 停为 run_limit_reached：16 个节点中前 15 成功，240.542 秒，根 200000 tokens 剩余 7120，不足下一调用的保守预留；并非模型或工作进程崩溃。源任务状态与失败 Run 保留于 us19-business-runs.json／us19-business-long-r2-failed-run.json。只读检查发现 task_summary 的权威 basis_refs 随阶段积累，重复文件读取也重复计入上下文。下一假设：将相邻章节按 12 项一组交接，冻结产品全文传给专业节点，保留完整矩阵/权限/全局90次与根200000上限，减少重复处理；不改产品硬状态与预算语义。

此时账本 45 次 chat、42 次 embedding 均 settled，无 pending/unknown；累计输入250382、输出10017、保守费用1549170微元（1.54917元），不是供应商账单。失败消费含在总授权。长任务及商业正确性尚未通过；短夹具与结构通过不冒充长任务目标。F20–F24 stage 派生及36任务映射已新增，定点两个商业静态测试与StageMapping通过，最终公共门禁待稳定树执行。

### R4 复核与真实结果（2026-11-03 22:48）

长任务 v3 `ft_6a3f76551f8fd9296a24bdff64b2737f` 在 final_pack 前停止：350.484 秒、20 次 Chat，Chat 输入103692／输出10187，加17次查询向量化83569，总实际 usage197448，根200000只剩2552。最终摘要为空；四份复核矩阵只有43个不同编号，遗漏R11/R12/R28/R30/R32，且有错组／重复、R19并发单位、R25可选认证、R38尾款条件错误。独立复核据原请求与输出确认，不能称长任务或商业结果通过。原Run、原文和结构检查见 `.loop/us19-business-output/long-r3/`；summary basis_refs 从0增至98，摘要从244增至7307字符，全量JSON同时用于检索查询，后续向量化亦大量消耗预算。三次试跑及消费均保留；停止盲试，进入只读根因／设计讨论，未放宽预算或判据。

九组配对均已完成运行，尚待逐条事实评分。全局账本87次Chat、79次embedding全部settled，无pending/unknown；输入576339（含embedding203727）、输出37666，保守估算4.301083元，非供应商账单。原90次授权只剩3次，不够重跑长任务，不自行加额；按时间边界归集的读数见 `.loop/us19-business-usage-summary.json`。

独立复核另发现R44五种状态被演示提示错写四种，已修提示与收集器共用原枚举；没有改原要求。收集器只作结构覆盖，校验完整result/sources/open_questions。普通无会话invoke回执有final_text，GET Run投影为空，不能据后者判空答；增加同RunID绑定原同步回执，保留GET原投影并另存invoke回执，异ID拒绝，不重调用。`.loop/us19-collector-invoke-assertion-red.log` 精确复现空答误判，修后16离线测试绿（`.loop/us19-collector-invoke-green.log`），三份A原文重新只读收集。

HTTP浏览器兼容缺陷：非安全上下文没有crypto.randomUUID，原2项真实反例失败；改用crypto.getRandomValues生成标准v4 ID，不用Math.random，456项前端测试绿（us19-insecure-http-id-red/green.log）。Chrome发现新增Agent的aria-label被脚本写成问号，已用UTF8补丁修正；画布和面板展示步骤／Agent名，内部稳定ID留tooltip，输入连接显示人类标签。最新全前端456例与build验证另见us19-r4-console-copy-final/build.log。

最终公共检查前一轮因最后提示修正而中止（PID21204，us19-r4-final-public）；下一轮启动遇旧独占锁，确认原PID不存在后仅移除其锁，未并发占用。public-2七目标在unit发现openapi.yaml被此前写为CRLF，历史Docs14D固定路径断言5处失败；路径实际存在，恢复该已改文件LF，不改断言（us19-openapi-lf-green.log）。该轮期间又有本节所记源码修正，不作为最终指纹通过；需要稳定树重跑。最终人审仍未就绪，未提交／推送／合并／tag／发布。

### R4 真实页面与独立评分（2026-11-03 23:02）

普通候选 SHA256 `5c4c798808dbfcb50c752e0b1f2cf69452062dd5dd661655d1b79a7701ab660a`，隔离实例60729，Chrome无JSON完成两节点新建、上游result→facts连接、反向重排拒绝无效引用、恢复顺序、在线校验、页内确认发布和表单启动。任务 `ft_56740828a64e5d30bd3e1098717c9fab` 以两个原Run结束并通过固定格式验收，产物原文可读；截图 `.loop/us19-r4-final-ui-task.png`。协议模型是本地夹具，不计作商业或时长证据。节点卡片、标题与来源标签实际显示已核对；任务详情仍保留原节点ID以便追踪。

独立reviewer逐条核对九组原最终结果、RunID／hash／bytes以及A组原invoke身份，按冻结fact/source/boundary各1分评分：A 67/72、B 60/72、C 59/72；B/C各至少一条明确伪引文。scope为24/22/21，delivery为21/22/23，commercial为22/16/15。逐项评分及扣分依据存 `.loop/us19-r4-independent-quality.json`。C仅在delivery略高，不能宣称多Agent总体优于单Agent；完成运行或schema绿不等于业务通过。

根因讨论后仅在.loop准备修正候选：17节点不改执行语义，4组复核固定R编号的result/statuses双映射，原五状态、sources/open_questions完整保留；需求／技术仍真实KB，交付／复核拟用完整冻结输入减少重复查询。尚未修改已发布Agent／定义或调用模型。候选996属性、139831字节，原产品只读validate通过（us19-long-r4-validate.json）；真实SchemaCompiler四组共32个正负例通过，覆盖缺项／错组／额外项／重复键／非法状态／空分析／缺交接（us19-long-r4-schema-probe.log）。只是结构可执行证据，不是业务通过；候选与预算仍由独立上下文复核。

全局90调用上限仍有效，当前87。已一次性请求用户裁决累计115调用（其他tokens／费用／模型／30分钟上限不变）及是否按工作量而非最低十几分钟验收；尚未收到答复，不推断同意。public-2最终exit2/source_changed：fmt/lint/sec、test-pg、cover、build、eval-build、behavior、injection通过，unit/integration因前述LF问题失败；完整新树仍需重跑，不将该轮写成make check通过。

### R4 阶段通过与修正候选复核（2026-11-03 续跑）

experience-final 3/3、base-final 11/11 exit0，同指纹 `7d5b23c8186c5b76a0b7252175b95ae3c9e88ba4eaeb976ab95f206dde910bf1`；base包含635条Go通过事件（含子例）、零跳过，前端456项与官网检查/构建通过。结合已有独立设计/实现复核与真实Chrome闭环，仅勾选T029–T032；T033–T036与T028保持未完成。编辑器画面补存 `.loop/us19-r4-final-editor.png` 并实际查看。未修改US18实例。

process-final 的真实进程检查exit0、所有必需用例通过，但最终总判为source_changed/exit2。定位到本轮勾选T029–T032时apply_patch同时把这四行原CRLF变为LF；正文/Go代码未变，任务勾选本身本来已被指纹归一化。仅恢复四行原换行后，以原fingerprint.go复制到.loop的只读工具重新计算，恢复为上述同一指纹。旧失败报告保留，不改判卷器、不回填通过；重新启动process-final-2，成功后串行执行final-public-3。

独立预算/资料复核发现首版候选的review仍缺产品和企业原文，不能把上游模型报告当独立证据。离线修正版四个review现内联完整product-main.md及commercial-synthetic.md和各自hash，delivery内联企业登记；仍17节点/996属性，198799字节。首个只读validate探针误少definition信封而400（控制侧调用错误，无外发），按原demo API形状修正后通过，见 `.loop/us19-long-r4-grounded-validate.json`；真实SchemaCompiler32例仍通过。实际Agent配置、已发布定义与调用上限未改。

新增离线格式提案 `.loop/us19_matrix_format_proposal.py`：trim拒空、精确编号/五状态、四段依据/缺口/责任/行动、重复JSON键拒绝；六组测试通过，原long-r3仍43项且不通过，非空虚构明确仍需独立语义审核。该提案尚未接入生产collector，不把格式检查当商业正确性。预算逐节点敏感性分析 `.loop/us19-long-r4-budget-projection.json`：原197448，拟去除九次embedding56758；换指令/Schema、补原文和最终调用后，在其他输出不变假设下为167946。估算器不是tokenizer/上界，新输出传播、检索片段及引用数量仍可能变化，不能保证任务成功。90次总调用仍只剩3次，未新增付费请求。

### R4 修正版接线与阶段复验（2026-11-03 23:40）

process-final-2以7d5b23c8指纹通过1/1检查、23条Go通过事件（含7项必需顶层），零跳过。
public-3首项fmt失败，原因是本轮只读指纹辅助文件`.loop/us19_fingerprint_main.go`
未格式化；该目录虽不进源码指纹，仍被全仓格式检查扫描。已仅对该辅助文件执行
gofumpt；原失败报告保留。随后本节演示脚本与说明有实际修正，该轮也不作为稳定树
通过，完成后须以新树再跑。Chrome实测拖动第二张卡到第一张后，引用失效阻止发布，
恢复顺序后原引用恢复；8080原US18实例23:18健康HTTP200，未变更。

将离线矩阵接入examples/workflows/bid-readiness：matrix_definition.py固定17节点，
matrix_report.py严格解析与原文分栏；collect.py按回执显式格式分流，原响应/hash
原样保留，旧Markdown回执不重判。源码清单新增三个控制文件，11份来源hash未变，
15份上下文重新冻结；旧清单保存`.loop/us19-source-manifest-before-matrix.json`，
旧setup／运行回执不改。28项离线用例全绿（us19-matrix-integration-tests-2.log），
覆盖重复键／错组／空白／缺字段、原文、旧格式、原响应hash、未知发布不重发和来源
修订不符零写；追加检查进入experience stage，原3项保留。

独立reviewer两项major为任务启动未绑定精确版本、配置回执来源hash未核对。已修，
独立复跑相关3项假API用例通过并确认关闭；不扩展为全业务通过。prepare-matrix
仅收窄隔离60481实例bid-delivery／bid-reviewer的自动检索，完整原文改由节点传入，
保留原文件快照、只读工具、禁止全局记忆和原五份确认记忆。重新取得pins后只读
校验通过，定义996属性／198700字节，SHA256为
`37e746d09a45f6e0b5f33b4581d8a86d5e3bcbbb9ecc0fb77b151dd3ac80b746`。
真实SchemaCompiler32项通过，见us19-long-fixed-v1-schema.log。

新定义单独发布为`long-fixed-v1`，UID `fd_70fb7e142d7ee636cf6d6694956152c9`；
未启动任务，不覆盖旧长任务或B/C对照。配置／发布前后付费账本hash一致，证据为
us19-matrix-config-no-paid.json及us19-matrix-publication-no-paid.json。调用仍87、
成本估算仍4.301083元，额外调用和时长变更仍待明确答复。业务演示与人工review②
均未通过。运维及quickstart已同步统一导航、无JSON编排、表单执行和页内确认。

### R4 稳定树验收启动（2026-11-03 23:47）

最终源码指纹`588c2346204e585c9464675b7d3bccf9010581ae2259539fccdb3e0b8e2347af`。
stable-experience 4/4通过，包含新增28项Python离线回归；base／process／public正在
串行复验，报告未结束前不判绿。此前public-3最终source_changed/exit2，fmt为上述
辅助文件问题，cover另有一次PG连接失败：
TestObservabilityStore_AtomicManagementAuditFailureObserved在建立55432 TCP连接时
返回“Only one usage of each socket address … is normally permitted”。其他10项中
除这两项外均通过；不改断言、不将连接失败视作测试通过。失败发生于23:39:20，
23:46检查该目标连接为Established10／TimeWait844，动态端口范围49152–65535；
后续快照不能证明失败瞬间的占用量，不能据此断言具体根因已解决，保留环境偶发风险。

隔离演示原五份memory entry经只读API核对仍为active且匹配旧proposal／confirmation
回执，见us19-matrix-memory-preserved.json。第一次只读脚本误用items而非entries
字段，本地KeyError后按真实响应字段修正；无配置或模型副作用。修正定义尚未启动。

本节计数订正：public-3总共11项，9项通过，fmt与cover两项失败；上段“其他10项”
为记录笔误，以原report.json逐项结果为准。

### R4 CI 接线补齐与最终复验（2026-11-04 00:04）

588c2346 树的 stable-base 11/11、stable-process 1/1 已通过，后者包含23条Go通过
事件、7项必需顶层、零跳过。随后只读核对CI发现R4体验阶段未进入flow job，已追加
us19-experience及独立输出目录，原base/process未删除；CI输入Git可见性、输出目录
唯一性、36任务阶段映射三个定点检查通过（us19-r4-ci-wiring.log）。新交付文件仅
作git intent-to-add以供既有可见性检查；未提交、推送或修改用户AGENTS/.agents。

此CI改动发生于stable-public运行期间，因此该轮即使逐项通过也只能作过程证据。
最终源码指纹为`3a07a15d1030f8f80d89c1e0a30f1a9f5cd85b772e582bfc5d19ffb3f8902a52`。
已串行排队ci-final-experience/base/process/public，沿用独占锁，不并发运行PG整套
验收，不覆盖旧报告。当前未判最终绿；远端CI尚未执行。TCP只读采样保存于
us19-r4-tcp-samples.jsonl，用于若连接错误再次发生时核对当时读数，不能回证旧失败。

原90次Chat上限、30分钟任务上限及最低时长要求保持原样；累计87次，无新增付费
请求。追加调用与最低时长两项待答复，用户“继续”未被解释为修改这些边界。

stable-public的test-integration于00:08结束，失败仅为TestStages_CIInputsGitVisible：
CI新增引用后、intent-to-add完成前读取到experience-design.md不在Git跟踪面。
不是产品集成用例失败。原失败日志保留；00:03三个CI/阶段定点检查已在登记后通过。
最终排队整体验收仍将再跑该检查，不以定点绿代替完整报告。

00:20 stable-public结束：10项通过，唯一失败为前述CI输入可见性；所有包覆盖率达标，
本轮未再出现55432连接错误。总判source_changed/exit2，报告不改写为通过。
ci-final系列于00:20:44开始，最终指纹3a07a15d保持冻结。00:15只读HTTP核对8080、
60729、60481三个管理台均200；未停止或替换实例。

最终3a07a15d树：ci-final-experience 4/4（00:20:56）、ci-final-base 11/11
（00:25:49）、ci-final-process 1/1（00:31:46）全部exit0。process为23条Go通过
事件、7项必需顶层、零跳过，真实容量／故障窗口测试完成。最后公共full于00:31:46
开始，未结束前不声称七目标通过。新一段TCP只读采样位于
us19-r4-ci-final-tcp-samples.jsonl，旧采样保留。

### R4 工程复验完成，业务验收仍未完成（2026-11-04 00:57）

ci-final-public于00:56:36以exit0完成，公共full 11/11，七目标与固定行为／注入
检查全部通过，所有包覆盖率达标。之前CI输入可见性失败已在本轮完整integration
和cover中通过；未再次出现55432连接错误，不据此改写旧失败或宣称已证明旧错误根因。

四份最终报告均绑定`3a07a15d1030f8f80d89c1e0a30f1a9f5cd85b772e582bfc5d19ffb3f8902a52`：

| 报告（.loop/runs/） | 结果 |
| --- | --- |
| us19-r4-ci-final-experience/report.json | 4/4 PASS |
| us19-r4-ci-final-base/report.json | 11/11 PASS |
| us19-r4-ci-final-process/report.json | 1/1 PASS；23条Go通过事件，7项必需顶层，0跳过 |
| us19-r4-ci-final-public/report.json | 11/11 PASS；exit0 |

结束后重新计算实际工作树指纹，与四份报告一致，保存us19-r4-ci-final-verified-fingerprint.txt。
只读汇总us19-r4-current-readiness.json的engineering_passed_same_tree=true；
business_demo_passed=false、final_human_review_ready=false。只写过程状态，未勾选
T033–T036／T028，未将R3历史E2E或本地协议模型冒充本次真实业务效果。远端CI未触发。

费用账本仍87 Chat、79 embedding、输入576339、输出37666、估算4.301083元，166项
全部settled，无unknown。修正版long-fixed-v1仍未启动。剩余独立工程工作已完成；
按用户原90次调用授权只余3次，不够约21次的完整复验，且最低十几分钟的原时长判据
尚未获准调整。因此等待累计115次／其他上限不变及按真实工作量验收时长两项明确
裁决，不能把“继续”或没有回复当作同意。即使获准，仍须真实执行和48项语义复核，
不能预先承诺多Agent优胜或review②通过。

分支仍019-flow-collaboration；未提交、推送、合并、tag或发布。00:57管理台8080／
60729／60481均HTTP200，验收锁已自动释放，已通知TCP采样程序停止，走查实例保留。
两处早前递归清理被自动审批以“blocked by policy”拒绝的临时目录，经本次只读核对
仍存在（TestFlow_CrashWindowMatrixdispatch-after-commit2132825100、
basalt-e2e-build-3016997350）；未换命令重试删除。

### R4 追加授权与恢复计划（2026-11-04）

用户已明确答复：允许累计最多115次对话（其余100万输入／8万输出／30元上限不变），
并改按多阶段业务结果验收、单任务最长30分钟、不要求最低十几分钟。原87 Chat＋79
embedding、166 settled、576339输入／37666输出和4.301083元保守估算全部保留。
本次只完成一轮修正后的完整验证；不重跑已记录任务。官方北京区价格已重新核对，
qwen-plus非思考短输入档0.8元／百万输入、2元／百万输出，embedding为0.5元／百万；
费用保护仍按既有更高费率保守预留，非账单实际扣费。

下一步：追加显式115上限及保留累计记录的离线回归；原网关无动态配置且只持有进程内
令牌，故只替换隔离网关并通过原管理API更新两个演示模型连接，真实上游密钥仍仅在
网关进程内。业务实例、旧任务、知识库和确认记忆不重建。修改来源清单时保存旧清单，
逐文件确认资料／persona未变，只登记本次授权及控制脚本修订；完成预检后才调用。

预算网关追加的30项离线回归通过，独立复核追加／未知用量／默认90并发／崩溃四项通过。
首次尝试在线把两个演示模型连接切换为托管凭据，接口明确503 CREDENTIAL_UNAVAILABLE；
原夹具没有可用托管主密钥。只读核对仍为原env模式及版本1／2，未重发该写请求。
随后仅重启空闲60481隔离应用及网关，保留原工作区、schema、全部9条任务、KB、记忆；
同一网关端口58469、随机令牌只在进程环境，真实密钥只给网关。恢复扫描0条，未重放。
旧网关停止后锁按原PID查证解除；所有166条settled消费逐项相等，未清零。新上限115。

来源清单只改scenario／evaluation-protocol授权附录、gateway和显式启动参数；11份
资料与5个角色原文／已确认配置／原定义完全不变。旧清单与配置回执另存，追加迁移记录
us19-authorized-source-amendment.json，现清单SHA256为
`f639a8d30a8f8b19a1ce20a05037130b767746eab9f168e7d73c320bc11fe3c6`。
预算预检us19-long-fixed-v1-final-preflight.json：原87 Chat，余28，正常路径估计21；
根token条件估计167953不是最坏上界，原硬预算不改。已启动原发布v1的唯一新任务
`ft_6fae2f9c702c3c13e9f96425179c4f2f`，运行中，不预判业务通过。

用户新增裁决：只有本次US19收尾继续获批真实模型验证；后续CI、基础／回归测试和其他
US自动验收禁止真实模型。已同步CLAUDE.md、AGENTS.md、EngineeringStandards及Harness
适配。独立复核发现历史external stage+旧授权、继承GOFLAGS可绕默认选择；已加执行前
拦截、已知模型凭据剥离和旧真实模型helper凭据读取前拒绝。PG、Docker、故障进程及
`go vet -tags=e2e,slow,real`只读装载保留。追加离线反例及experience stage映射；
不删历史断言、不改旧验证报告，不把本次US19授权传递到后续US。

### R4 商务叶子空知识绑定失败（2026-11-04 05:00）

long-fixed-v1 原任务 ft_6fae2f9c702c3c13e9f96425179c4f2f 在154.890秒后失败，
前8节点完成，delivery_1在382ms内、零轮次／工具调用时失败。只读原Run库行确认原因
为“请求的0个集合均不可见”：Flow把根集合上限传给没有knowledge声明的商务叶子，
真实Authz把显式非空请求与空声明的交集判为拒绝；不能改成吞掉所有权限错误。
预算186项全部settled、99 Chat，无unknown；原任务终态不可复活，原始输出与消费保留。
剩余16次不足预计21次完整重验，已向用户请求125累计上限，未答复前不再发真实调用。

本题第1次修复假设：AgentService从已固定快照判定Flow叶子没有知识绑定时，跳过
自动检索；带绑定节点仍经过原范围／权限／预算检查。真实PG＋两个注册Agent＋原
Worker/Run/审计的离线回归TestFlow_NoKnowledgeLeafKeepsSharedRunChain已复现
expected succeeded / actual failed（us19-empty-knowledge-red.log），随后才修实现。
此外acceptance整包第一次复验因新增model_policy_test.go尚未intent-to-add而失败，
保留us19-offline-policy-acceptance-tests.log；补Git可见性后再复验，不记为产品失败。

空知识绑定修复后的真实 PG 定点回归退出0，app 2.435s（us19-empty-knowledge-green.log），
同时覆盖检索／向量化回调／后续来源核对／终态审计失败预算；新增双叶子用例有两个 completed
Run、两个 llm_calls，使用本地协议脚本，零真实模型请求。独立 reviewer 静态核对为0
blocker／major，未自称运行PG或真模型。新用例追加experience required_tests及T033／T036。

自动验收拒绝策略补充 --tags／--overlay／go -C 形式和真实子进程环境断言；补充反例先红
（us19-model-policy-flags-red.log）后 acceptance 整包通过14.074s
（us19-offline-policy-acceptance-final.log）。本轮再做定点独立复核与新树全量验收，
旧报告保留；125次追加问题仍待答复，未新增付费请求。

### R4 Worker 停机查询竞态（2026-11-04 05:18）

新树 experience 5/5 通过；base 10/11，唯一失败是原有真实 PG 测试
TestFlow_WorkerUsesBoundedPoolAndDurableQueue 在正常 cancel 后收到 context canceled。
报告 us19-r4-offline-final-base、原日志保留，不用重跑侥幸转绿。第1次假设：父取消
发生于循环检查之后、OpenRoots／ReadyRoots 查询期间，查询错误被当作运行失败返回。
同时独立 reviewer 指出 runtime.Close 的 errors.Is(cancel) 会吞掉 cancel＋数据库
错误。先加查询内确定性取消与复合错误反例，再仅归一已取消父 context 的单链取消
包装；复合错误保守上抛，实际 join 和 ErrDrainTimeout 不变。旧 PG 断言不改。

假设已证实：us19-worker-cancel-red.log 四个查询内取消反例报 context canceled，
Close 复合错误反例报 expected database error / got nil。最小修复后
us19-worker-cancel-green.log 的 flow／app／真实 PG 全通过，原 Worker 队列与实际
join 测试仍保留。独立 reviewer 实跑16个查询场景与 Close 组合通过，0 blocker／major；
未自称复跑 PG 或全门禁。两个具名测试加入 base／race required_tests，T026／T027
映射不变，未增加公共接口。下一轮使用新的 evidence 路径，旧失败报告保留。

### R4 页面与演示现场核对（2026-11-04 05:25）

Chrome 扩展 inventory 可见，重置 CUA 会话后仍在绑定本任务空白页时返回
Cannot read properties of undefined (reading 'timeoutMs')；没有执行新的页面动作，
不将 API 读回当成新的浏览器走查。旧60729实例的三个前端文件与本轮 dist/integrity
逐字节哈希全部一致（us19-ui-asset-continuity.json），此前真实截图仍对应当前前端；
旧已走通任务 ft_56740828a64e5d30bd3e1098717c9fab 原API回读仍 succeeded。
8080／60729／60481 均HTTP200；60481十条任务全为终态，未复活失败任务或增加调用。

独立业务 reviewer 已交付 us19-business-semantic-review-checklist.md：48个编号唯一
完整，11份来源哈希匹配。逐项核对数值、付款触发条件、引用、能力证明和材料缺口；
不改采购原文／旧评分／旧输出，不把中间节点意见或结构通过当最终业务通过。

2026-11-04 05:34：用户明确允许累计最多 125 次，离线通过后仅复验一次完整演示；其余 1M 输入／80K 输出／30 元／任务 30 分钟不变。原账本 99 chat + 87 embedding 均 settled，保留历史。experience 5/5、base 11/11、process 1/1 已通过（e306ebd）；public 于 fmt/lint 后由控制侧主动中断，以纳入本次授权控制改动后再跑，不记通过。新增额度追加及 v2 固定原发布版本回归，先取红态。

125 追加已取得有效红态（unsupported authorization；v2误选legacy格式），最小修改后Python离线33/33通过。独立reviewer亦实跑33/33，0 blocker/major，未操作真账本或网关。来源清单新hash c69d32af3d428e1970626eb2e3fe1b0bb627c59fd3bf02d52e8ec295ab7dbf45，仅五个授权/控制文件变更，11来源、5 persona及当前pins不变；原清单/配置快照和两次修订回执都保留。最终offline-125系列已启动，尚未新增付费调用。Chrome库存可读，但绑定标签和documented tabs接口均仍报timeoutMs内部错误；不将本轮API核对冒充新浏览器操作。

### R4 人工真实模型验收接管（2026-11-04 05:43）

用户明确改为“真实模型我最后人工验收也行”，要求准备 Agent、工作流、配置、服务与操作步骤。自此不再自动启动 long-fixed-v2，不把未运行的完整业务复验记为通过。原 99 Chat／87 embedding 消费、原失败任务和全部上限保留；125 上限仅为本次人工触发保留。离线套件继续，交接时将 T034／T035 中需人工核对的内容和最终 review② 明示，不伪造完成。

浏览器重启后 Chrome 库存与新标签页元数据能读取，但导航仍返回 timeoutMs 内部错误；这次也未完成新的页面操作。已告知用户无需反复重启。后续以已有真实走查及资产一致性证据为已验范围，新候选业务页由用户实际核对。

独立定点 analyze 为 CRITICAL=0、HIGH=0、MEDIUM=1：F24 静态映射漏列已实际执行的两个离线检查。当前运行中不修改输入；套件完成后补齐派生映射，并明确文档差异与既有代码证据的对应关系，不覆盖旧报告。

### 2026-11-04 默认工作流随产品交付（用户追加）

用户要求四类 Agent、17 个顺序节点、48 条采购需求像每日 Demo 一样作为默认配置后续携带。
沿已批准 US19 增量落地：净化素材嵌入二进制、一次性安装且保留用户修改/归档、管理台绑定当前已就绪 Agent 生成草稿；安装不启动任务、不调用模型、不摄取知识、不自动确认记忆。
独立设计复核给出半复制防护、生命周期防复活、当前引用与素材隔离四类要求，纳入实现。
为修改源码，主动停止 05:43 开始的 offline-125-public（当时 fmt/lint/sec/test 已过，integration 仍运行），此报告保持中断/未完成，不作为最终公共通过。
同树 offline-125-experience 5/5、base 11/11、process 1/1 已通过并保留；新增后重跑受影响门禁及最终稳定树。
用户另裁决真模型最终轮改由人工启动；AI 不再发起 paid 请求，累计账本继续保存 99 对话/87 向量化记录。

默认素材初次校验发现：前端旧解析器把数组元素也计入对象属性，导致17节点模板被拒；
修正为契约原定的属性总数和单数组长度各1024，并补两类1025反例，未提高上限。
模板改延迟加载后测试须等待异步模块完成，保留分页/草稿保护/零POST全部断言，34项通过。
素材11份来源哈希、17节点服务端结构、四角色无模型/知识默认绑定及排除实验控制器均验证。

独立代码复核发现1项major：CLI安装后尚未首次启动时，删除角色会被启动补回。
共享安装器在完整复制后写一次性回执，启动与CLI共用；不覆盖旧开箱状态，不接管legacy。
逆改忽略回执时fresh/managed/损坏回执三个场景均复活并失败（us19-defaults-receipt-red.log），
恢复后六项定点测试通过（us19-defaults-green.log）。该项继续交独立复核，未提前判闭合。

独立 reviewer 已复核共享回执并独立运行六项默认工作流测试，三包退出0，major关闭。另补全部目录已存在但无control的安装回执边界，builtin测试通过。Markdownlint与diff检查通过。06:04开始冻结新树，执行defaults-final系列；人工T034/T035和review②保持待验。

2026-11-04 06:13：defaults-final-experience 6/6；base 10/11，唯一失败为
TestFlow_StageMappingCoversFrozenTasksAndBehaviorGroups 仍固定 36任务/24组，与用户
新增默认配置后的 T037/F25 不一致。原失败日志和报告保留。第1次定位即证实：
任务/规格/stage/acceptance-map 已为37/25；仅同步计数与遍历上界，保留原映射、
冻结语料哈希和全部断言，不减少测试或放宽范围。定点四项通过
（us19-defaults-mapping-green.log），独立复核另行回执。按新指纹启动final2系列。

2026-11-04 06:11:08：独立映射复核确认37/25适配忠实于已批准增量，T037/F25均映射实际执行项，未放宽验收语义。上一条手填06:13为记录时间笔误；实际新套件启动06:10:33，以报告机器时间为准。

2026-11-04 最终公共门禁追加：final2体验6/6、base11/11、process1/1同树通过，
public的fmt/lint/sec通过，但test失败。原架构断言明确拒绝CLI直接import workspace；
新增demo workflow install为取锁误越过消费端口。第1次定位确定，未改原架构断言。
控制侧中断尚在运行的公共integration，清除已终止PID26980的原锁；原报告保留未完成。
最小修复为cli.Deps注入原锁获取/释放函数，app.workspacePort适配及Run接线；
命令缺锁端口/空句柄即零写拒绝，释放错误上抛。原真实占锁拒绝断言保留；新增真实
app.Run持锁零写、释放后安装、再次取锁的接线回归。架构断言和新接线用例一并纳入
experience required_tests。定点cli/app通过（us19-defaults-cli-port-green.log）。
此为原依赖约束的接线修复，无产品API或执行语义变化、无新增依赖/模型请求。

2026-11-04 06:28:35：锁端口小改独立复核无新增阻断，reviewer实际运行cli/app定点测试退出0（0.557s/1.208s），未运行模型。final3先跑公共full再三个stage，源码冻结。此前stage-lint --stage current退出0，包含旧stage名称警告，未称零警告。

### 2026-11-04 final3 诊断用例的审计超时调查

final3-public 的 fmt/lint/sec/test 通过，test-integration 中原有
TestDiagnostics_WriteFailureDoesNotBlockRun 失败（6.92s）：预期 completed，实际 failed，
原因是 llm_calls pending INSERT 的独立 5 秒 context deadline exceeded。原日志和报告保留；
不把此项写绿。诊断缺表的 11 条 PG 错误于 22:31:58.980–22:31:59.048 UTC 返回，
随后审计超时；没有证据证明诊断 INSERT 本身阻塞或新 CLI 轻路径触碰 PG。
第 1 次调查保留池等待/连接建立、PG 锁或 I/O、宿主调度三种未区分原因，不修改生产 5 秒上限、
原场景、断言或失败行。三条原测试（CLI 接线、诊断可丢弃、审计 fail-closed）同进程 count=3
通过4.412s（us19-diagnostic-audit-timeout-focused.log），只记暂未复现，不记根因已修。
独立 reviewer 已确认上述边界，并指出 pgx 默认用连接 deadline 取消：没有 server cancel
日志不能证明 SQL 未到服务端。追加仓外临时 failure-only 日志 overlay 与 count=30 取证，
不写产品源码；final3 其余公共检查继续收集，随后按完整门禁结果决定复验。
30轮临时日志overlay探针在整组120秒上限退出，未出现原diagnostics断言失败；退出栈位于
第后续轮夹具装配的迁移advisory锁（该用例刚开始0s），不把整组超时当原5秒审计失败复现，
也不记30轮通过。探针与公共cover使用同一测试PG，后续不再并行追加PG探针。源码与阈值未改。
final4按run-kind=rerun排队，待原门禁释放锁后才完整复验；四组报告使用独立路径，失败历史保留。
此前7:05–7:15预估因本项未解释失败失效；完整复验后才允许切换人工实例，不提前宣布收口。
2026-11-04 06:58：final3-public 最终 9/11 检查通过，另有 cover 中
TestFlow_CoordinatorOriginalLedgerLifecycle/capacity 失败（实际 blocked，预期 succeeded）；
该断言未输出 task.Reason，因此不能把它直接归因于容量回退或数据库超时。静态核对发现
脚本叶子的 FinishRun 使用独立 1 秒截止，叶子错误会走 leaf_execution_unconfirmed；
这只是待验证路径，不是已证实根因。与原诊断审计超时分别登记，不混成同一故障。
final3 的 cover 中 app 全包通过；final4 保持同一源码指纹 b50912c5aa4e7055d4609ae9d2b924f9fa562ccaf3a1ebad7c902ea6610b55d0，
未更改断言、超时或测试场景，也不再并行运行额外 PG 探针。即使复验通过，历史失败及未证实根因仍保留。
时间更正：上一条取证记录实际写入为 2026-11-04 06:55（06:58 为手工时间笔误），以验收报告自身时间戳为准。

### 2026-11-04 07:24 final4 整包超时与隔离环境验证

final4-public 的 test-integration 全绿（app 511.469s，flow 22.804s）；cover 的 flow
也通过（23.497s），此前两个具体断言未复现。但 cover 的 app 在 Go 默认整包10分钟
上限终止（600.723s），仍有约20个刚开始的并行管理面测试和迁移 advisory 锁等待。
因此 full 仍失败，不进入服务切换，也不把这次不同的失败说成前两项根因已修。

环境假设第1次：长期复用测试库的系统目录和残留夹具增加了迁移/清理成本。只读实测
原库 pg_class 96MB、pg_attribute 139MB、pg_depend 157MB，含73个pgtest残留schema；
并非已证实逻辑死锁或全部失败的共同根因。保留原库和演示，新增专属容器
basalt-us19-final-pg（46edfb678f0282d645aff410512b1eb473c70383a93eb9d384c65f9601e3375b），
127.0.0.1:55433，仍用同一本地 pgvector 0.8.7/PG17 镜像、2 CPU/2GB、默认 fsync 等配置，
basalt_test 仍是非超级用户且同名测试库，vector 0.8.7/pg_trgm 1.6。新库 pg_class 232KB、
pg_attribute 680KB、pg_depend 400KB；实际 pgtest 全包 0.861s 通过。初始化曾发现bootstrap
角色不可降权，已在未执行测试前删除仅本次新建容器并按原postgres/bootstrap加普通测试角色重建；
SQL经stdin传入，未打印或写入凭据。原数据库和全部历史证据未动。

final5使用新库完整执行相同public/experience/base/process，源码指纹仍b50912c5，
未调整任何断言、超时、并发参数或覆盖率阈值。新库验证未完成，不提前记通过。
时间更正：上段隔离环境记录实际完成于 07:22，final5-public 于 07:22:54 启动；标题 07:24 为手工时间笔误，以各报告时间戳为准。

环境核对补充：旧容器记录的 image ID 为13b6ffe9771a，新容器为7d0a41464797（后者与当前
已批准 docker/compose.yaml 的锁定digest一致）。因此撤回“完全相同镜像ID”的可能解读；
两者 PostgreSQL 实报均17.11，postgres可执行文件SHA256均f7d05a9a444dc63d93f6b6e329d31eaebae809e85c8479b5072d8aff595303ed，
vector.so均98d8e195d63d582647c1038fe044a7e70077cd5f625aa1cfdf27d86976445a29。
当前验证是已批准镜像的新隔离环境复验，不能单靠通过就断言系统目录大小是所有失败的唯一根因。

### final5 最终自动验收与人工交接

记录时间：2026-11-04T08:03:57+08:00。源码从06:28修复后保持冻结，最终四报告共同指纹
`b50912c5aa4e7055d4609ae9d2b924f9fa562ccaf3a1ebad7c902ea6610b55d0`。

| 证据 | 本轮实测 | 边界 |
| --- | --- | --- |
| 公共full／make check七目标 | final5-public 11/11，exit0，覆盖率全部达标；app coverage 489.056s、flow 25.648s | 本地隔离PG；远端CI未触发 |
| 工作流基础与兼容 | final5-base 11/11；696条Go通过事件，0跳过；含race、前端测试/构建、官网检查/构建 | Go事件含子例及不同检查的重复执行，非696个独立顶层用例 |
| 体验与默认示例 | final5-experience 6/6；38条Go通过事件，0跳过，另有前端/Python实际命令 | 禁止基础测试真实模型、模板、默认安装与升级保留均已判卷 |
| 真实进程 | final5-process 1/1；7项required顶层、23条Go通过事件，0跳过 | 200任务容量、32原任务恢复、崩溃窗口、写动作身份及不重放 |
| 人工实例 | 60481健康ok、管理面无token401、4个已就绪角色、3个知识集合、确认记忆、17节点v1 | 页面资源4项hash匹配；不冒充最新浏览器视觉或业务效果通过 |
| 模型账本 | 原99 Chat／87 embedding共186条settled完整保留；上限125，余26；累计费用上界4.851198元 | 本轮离线验收和服务核对新增0模型调用，最后业务运行由用户触发 |

报告目录：`.loop/runs/us19-r4-defaults-final5-{public,experience,base,process}/report.json`。
汇总 `.loop/us19-final-handoff-evidence.json`；只读预检与服务核对见
`.loop/us19-long-fixed-v2-final-preflight.json`、`.loop/us19-manual-service-ready.json`。

收口七项证据：①公共与三个stage上表逐项通过；②acceptance-map的37任务/25行为组由
具名映射检查判卷，原required项保留；③Git完整路径/增删改快照为
us19-final-git-status.txt／us19-final-git-stat.txt，私有治理文件11项hash为
us19-governance-snapshots-final5.json；④公共full包含前序全包单元、集成、PG、覆盖率与
固定行为/注入，US19另有共享执行链/普通Run兼容；⑤原架构不变量检查、唯一审计链、
受控写动作及资源边界检查均通过；⑥下述人工项仍未验证；⑦reviewer导读随最终对话交付。
最近analyze增量结论CRITICAL=0/HIGH=0；默认安装、映射、CLI锁端口独立复核回执及
修复记录见此前各段，未重新扩大审查范围。源码未为本轮复验修改。

历史失败未撤销：final3诊断审计超时与capacity被blocked、final4整包超时都保留。
final5在批准镜像的专属干净PG上通过，未改任何超时、并发或断言；这证明本轮环境通过，
不证明历史偶发失败的共同根因已查清。环境事实和清理回执见us19-final-test-environment.json。

服务切换辅助脚本在停止旧应用/网关后，立即Get-Process检查一度仍见进程而保护退出；
随后CIM、端口和原账本hash核对确认二者已退出，按检查点续接，未重跑已完成动作。
原24小时fixture宿主55260被终止以保留人工验收数据，记为interrupted而非测试通过；
原工作区/schema保留，8080 US18健康ok。新应用39924、网关53840，均隐藏后台运行。
后续只读预检首次错误地直接比较省略字段的本地JSON与Go结构序列化；定位仅为
TaskContract的required_slots=null及零值scope补齐。现在按既有Go类型补齐这两个字段、
同时核对原发布回执hash后全量逐值比较，通过；原差异及失败留于
us19-published-bundle-diff.json和us19-long-fixed-v2-final-preflight-attempt2.log。
汇总器另修正JSON null空跳过列表的读取。以上均为.loop交接辅助，不改产品或官方门禁。

仅勾选T033／T036／T037。T034（最终完整真模型业务结果）、T035（最新页面人工视觉）
及T028（人工review②）保留未勾选；U19-04／05的初始“尚未执行/授权”是历史登记，
当前以本段为准：机器证据通过、授权已存在，真实业务/最新页面待用户。旧对照67/60/59
不能宣称多Agent更优。自动验收通过不等于US19已经人工收口。未commit/push/merge/tag/release。
人工操作单：`.loop/us19-manual-review-steps.md`；下一动作由用户登录60481并提交一次原v1任务。

### 人工验收期间追加的演示 Agent 收尾安排

记录时间：2026-11-04T08:25:01+08:00。用户提出：60481验收后，最后收尾时把本次5个采购Agent作为演示配置追加，
与原有定时演示Agent一并后续携带。按用户指定时点，当前不修改正在人工验收的实例或候选。
源码核对：内置日常Agent为5个；当前已实现的默认采购素材只含4个专业角色，
第5个bid-general是本次单Agent对照，尚未进入默认安装清单。可选examples/templates不等于自动安装。

后续收尾增量：保留原5个日常Agent，另带4个工作流专业角色及1个通用采购助手（单Agent对照），
共10个内置演示Agent；用户自建和可选模板另计。17节点主工作流继续只编排原4个专业角色，
不为凑数量插入对照Agent。新加示例默认禁用、不增加定时计划、不自动调用模型；原有配置、
定时计划及启停状态保留，不复制验收实例凭据、数据库身份或运行历史。
用户完成当前人工走查后再同步默认素材/说明/安装与保留回归、派生任务状态和新候选证据；
当前T037已通过仅代表原4角色范围，不把第五个默认安装记为已实现，不沿用旧指纹冒充新增已验。
本条不表示人工review②已通过，不授权commit/push/merge/tag/release。

### 演示工作流随默认示例包交付的补充要求

2026-11-04 用户追加：“同时这些用于演示的工作流也默认带上吧”。与上一条同在人工走查后的收尾增量完成。
示例包除5个采购Agent外，还携带可复用工作流模板、配套示例材料、任务说明样例与使用指引。
主推荐为当前 `bid-readiness-matrix-v1` 的17节点采购响应准备工作流；scope、delivery、commercial
三组短流程的单Agent／专业Agent对照模板归入可选对照示例，明确其用途，不宣称已有多Agent优势证据。
旧 `bid-readiness-full` 已被修正后的主流程替代，不把旧失败版本与调试任务当作推荐默认配置。

交付采用可移植模板，不复制验收库的Agent UID、发布记录、凭据或任务历史；在目标实例选择并绑定已配置的Agent，
通过校验后由用户发布、手动提交任务。默认携带不等于自动发布、启动任务或付费调用。
安装与升级保持现有自定义配置；当前已实现的主流程模板与待补齐的对照模板分别记录和验收，
新增范围完成后更新对应材料及自动验收证据，不改变正在60481人工走查的候选。

### 2026-11-04 人工任务定位与旧失败记录复核

用户截图中的 `ft_8cc01dd0fc8222b83fb60359e66e0f33` 是11月3日22:19创建的旧定义v2任务，
本轮GET仍为stopped/run_limit_reached，technical_08失败，前15节点成功；根预算200000、剩余7120。
与此前保留的下一次模型调用预算预留不足记录一致，不是今天新提交的任务，也不是30分钟截止触发。
只读证据：`.loop/us19-user-reported-old-task-detail.json`、`us19-user-reported-old-task-diagnostics.json`。

用户今天提交的原文“帮我看看这次采购项目我们能不能接，还有哪些材料需要补齐，有什么需要提前注意的风险。”
已用数据库只读事务核对冻结input，准确对应 `ft_dec546ac7a15a22202163e9d807f457c`，
定义为 `bid-readiness-matrix-v1` v1；北京时间08:19:25.428创建、08:25:34.670完成，369.242秒。
17节点全部succeeded，最终report已生成，report-shape/schema_valid通过；剩余31907 tokens、66工具次数。
这证明本次真实业务链执行完成及结构规则通过，内容准确性与最终review②仍由用户走查，不自动勾为人工通过。
证据：`.loop/us19-user-manual-task-succeeded.json`、`us19-user-task-input-confirmation.json`。
本轮仅GET与数据库只读取证，未提交新任务、未调用模型或重放旧任务。

已定位任务可发现性缺陷：列表第一列只有内部ID，没有用户任务说明；存储查询按task_id而非创建时间排序，
最新任务不一定在顶部，旧试跑容易被误认。本问题纳入人工反馈后的收尾：用户可读任务说明、
工作流名称及创建时间、最新任务入口与合理排序；涉及游标排序的修改须同步语义与回归，不以浏览器局部排序冒充全局排序。

### 2026-11-04 用户人工 review ② 与最后两处显示调整

用户明确：“我人工验证可以收尾了”，仅要求详情显示工作流编号和版本，列表按更新时间倒序、
增加工作流编号列和编号筛选；同时明确“不走全量验证，就改这两处展示，快速收尾，由Claude提交并跟踪CI”。
本条是实际人工验收回执；不由AI替代批准。此前T034末轮真实业务及T035人工页面走查由用户完成，
对应任务 `ft_dec546ac7a15a22202163e9d807f457c`，17/17节点通过，369.242秒；失败与对照历史保留。

本轮增量严格限于上述显示与其必要只读查询接线、派生契约／回归／打包工件。
后端取固定定义的name与version；SQL先按归属／工作流精确筛选，再按updated_at、task_id倒序分页；
管理投影仍不暴露任务目标／产物正文。前端可选择或输入工作流编号，筛选／刷新从最新第一页开始。
旧内部定义UID仍可核对，不改Run、Worker、审计、预算与模型调用路径。

定点检查初次新增PG用例误把CLI管理员与Web管理员当不同归属，断言失败；两者都是admin-builtin，
测试夹具修正为真实不同caller形态，保留原“不能跨归属读取”的断言；HTTP真实鉴权集成同时验证该隔离。
首次npm命令被PowerShell对npm info的stderr处理提前终止，重跑时显式处理shell错误策略，未改判据。
原失败日志 `us19-manual-ui-delta-go.log`／`us19-manual-ui-delta-web.log` 保留。

增量检查：Go五个定点测试（PG排序／分页／筛选／归属、HTTP链路、历史尝试分页、幂等受理）全绿；
前端 `flow-views.test.ts` 33/33通过，`npm run build`（含vue-tsc与integrity生成）exit 0。
证据：`.loop/runs/us19-manual-ui-delta-go-2.log`、`us19-manual-ui-delta-web-2.log`、`us19-manual-ui-delta-build.log`。
按本次用户明确要求，不重跑make check／full stage／全量e2e，不调用真实模型。
此前final5四报告的共同指纹 `b50912c5aa4e7055d4609ae9d2b924f9fa562ccaf3a1ebad7c902ea6610b55d0`
仅证明当时已验树，不冒充本轮新树全量通过；新提交的全量结果由Claude跟踪CI。

范围交接：此前默认包已经交付5个日常Agent＋4个采购专业Agent和17节点主模板；
第五个采购对照Agent bid-general及三组对照模板默认随包的追加要求仍待落实。
本次用户“只改两处显示、快速收尾”的指令下不展开该增量，不把它们登记为已实现／已验收；
Claude接手时保留该待办并告知用户，不以此前T037的四角色证据覆盖它。

60481 已更新为本轮构建 `bin/us19-manual-ui.exe`（PID 32452），沿用原工作区、schema 与网关。
更新前确认全部任务已终态；更新后只读 API 核对11条任务保留、全局更新时间倒序、
用户采购任务排首位；编号筛选返回2条匹配任务，不存在的编号返回空列表；详情编号与v1一致。
模型总账哈希前后一致，本轮零新增真实模型调用。运行交接 `.loop/us19-business-manual-ui.json`；
静态构建证据 `.loop/runs/us19-manual-ui-delta-binary.log`（CGO_ENABLED=0，exit 0）。
本轮浏览器新开页遇到扩展适配器 timeoutMs 错误，未将真实页面视觉复验记为通过；
组件渲染／交互测试及新实例只读 API 检查已通过。不阻断用户要求的定点修补交接。
提交与CI交接见 `claude-handoff.md`；本轮未commit、push、merge、tag或正式发布。

### 2026-11-04 合入 main 与 CI 收口

用户确认 US19 流水线成功后，明确要求“把us19合到主分支，并完全收尾”，随后开始 US20。本条只追加交接后的提交、合并与 CI 证据，不改写上面的人工验收、失败记录与任务勾选。

提交范围：a19637b（US19 实现）与 094a4ac／dcde51d／498bcd3／f87c81c／7d7968c（CI 失败修复）。分支 `019-flow-collaboration` 的 CI run 41768244811 在 7d7968c 上七个 job 全部 success（flow／postgres／build／test／console／website／container）；同轮 `test` job 11:37–12:22 约 45 分钟。
main 从 ca9a0d2 快进到 7d7968c 并推送 origin/main；推送触发同 SHA 的重复运行 41773179803。合并为纯快进，未改写历史、契约或已发布迁移。
仍未落实：`bid-general` 及 scope／delivery／commercial 三组对照模板的默认随包增量，保留为后续待办，不登记为已实现或已验收。
未打 tag、未发布、未部署；未读取或上传发布私钥；60481 人工实例与 8080 用户演示未改动；本轮无真实模型调用。
