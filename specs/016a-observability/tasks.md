# Tasks: US-16A Agent 可观测与执行诊断

输入：spec.md、plan.md、data-model.md；真实授权见run-state。勾选只表示该任务已取得具名验收证据，不代表人工收口通过。所有关键测试保留已审输入、动作、断言与逆改点；新增测试不改变历史判据。

## P0 输入与判据

- [x] T001 登记US-16A、基线/私有快照、spec/clarify/plan/research/data-model/contracts/quickstart，任务对比、analyze与输入指纹冻结；FR-001–034、SC-001–006。
- [x] T002 建立 `internal/observability` 类型/消费端口与具名单元验收骨架；从模块卡提取全部必跑名与有效行为红态，建立 local-only `harness/stages/us16a.json` 的inputs/protect/tasks映射；FR-002–009、SC-001/002。
- [x] T003 在独立临时目录执行OTLP A/B/C固定版本spike，验证三信号批次结果/拒绝数/warning/禁内部重试、许可/依赖树/体积；自动裁决并留追认清单；FR-016/021/023、SC-004。

## P1 关联与生产端证据

- [x] T004 新增 `0006_observability.sql` 六表、可空关联/恢复/检索字段与扫描索引，budget空值兼容、版本6，先测v5升级/幂等/历史字节/枚举；FR-001/017/020/026/033、SC-003。
- [x] T005 贯穿RunRecord/AgentService/PG读写/RunView的request_id，历史NULL与调度无请求不补造；FR-001/033。
- [x] T006 工具ToolDiagnostic.ToolCallID、attrs写读allowlist、旧事件兼容、chunk_invalidated枚举一致性；FR-010/030/033。
- [x] T007 检索真实候选kept/rank_cut/budget_drop与retrieval摘要持久化、非chat budget NULL，运行真实过滤四场景；FR-007–009、SC-001/003。
- [x] T008 撤权收窄、摄取阶段和索引切换真实生产端，不复制assembly/embedding事件、不伪造锚点；FR-010/030/031。
- [x] T009 恢复pending审计同UPDATE写recovered_at，正常终态不变；导出恢复未知边界测试；FR-024/033、SC-003。

## P2 读取与上下文

- [x] T010 `ObservabilityStore` 实现所有只读事实/关联端口，SQL只读允许列，真实PG锚点与隔离测试；FR-002/003/029/030。
- [x] T011 `Projector.Timeline` 局部稳定排序、显示号、过滤后编号不变、session编号、截断与来源级缺口；FR-002–006。
- [x] T012 `Projector.Context/Overview` 逐项证据档位、缺失presence、混合计数不泄露、Top-N异常组；FR-007–009/013。
- [x] T013 `MetricsCollector` 有界标签、进程计数/直方图、PG窗口样本/Gauge、缺失态与审计失败采集；FR-011/012/025/031。
- [x] T014 `AlertEngine` 八内置规则、状态/转换持久化、持续/恢复/unknown与工具拒绝排除检索；FR-014/015/013。

## P3 持久导出与清理

- [x] T015 PostgreSQL导出账本Enqueue/Claim/MarkStale/BeginBatch/ApplySendResult、租约与重试、状态查询，真实并发/NULL计数测试；FR-019–023、SC-003。
- [x] T016 终态窗口扫描八来源、键集分页/覆盖CAS/重叠/有界对账，长任务、恢复、晚提交、同毫秒、页失败测试；FR-017/018/024。
- [x] T017 `Mapper` 与三信号中间结构、稳定ID、真实/派生结束、恢复log、导出allowlist与canary；FR-016/024/030。
- [x] T018 `ExportWorker` 发送前重读哈希、信号分批、尝试先写、确认事务、部分成功/未知崩溃窗口、业务零副作用；FR-019–024/031/032。
- [x] T019 OTLP有界sender与 `MetricsExporter` 快照持久化/去重，三信号协议分支、Gauge不相加与重启可解释；FR-016/021/023/025。
- [x] T020 `RetentionCleaner` 四策略默认关闭、覆盖+行级确认、分批删除/账本原子性、重启/导出冲突与缺口；FR-026–028、SC-003。

## P4 装配与管理台

- [x] T021 `internal/cli`配置解析校验与 `internal/app/observability.go` 装配/单生命周期worker、取消、关闭和诊断失败隔离；FR-031/032/034。
- [x] T022 七个OBS管理读API、参数/空错/来源重脱敏、所有路径callerKey拒绝、OpenAPI/枚举一致性；FR-002/029/030/033/034。
- [x] T023 管理台API客户端与总览/任务组/Run时间线/上下文/查询/告警/导出/清理页面，空缺错拒绝状态、技术详情、测试与dist/integrity构建；FR-034、SC-002/006。
- [x] T024 六独立场景贯通实际入口与真实检索/审计/证据，关键安全/恢复/排序/部分成功变异反例；SC-001/002/003。
- [x] T025 技术方案诊断语义、Operations、示例配置、八项影响清单、隔离清理容量/查询实测与限制；FR-026/028/034、SC-006。

## P5 全量验收与人工走查

- [x] T026 授权范围内真实回环Collector验证三信号发送/接收/字段脱敏与测试后端Gauge语义，容量只记实测、真实外部后端保持可选未验证；SC-004。
- [x] T027 analyze复跑、独立上下文review原始工件与diff，高风险五类逐条处置并重验；SC-002/005。
- [x] T028 稳定最终树运行make check七目标、make acceptance、完整US stage、make e2e及US16受影响回归；逐项存在性/13不变量/四态证据/候选SHA，准备走查与Claude回交包；SC-005/006。

## Dependencies and Check Mapping

T001→T002；T003独立spike先行；T004→T005/006/007/008/009/010/015；T010→T011/012/013/014；T015→T016/017/018/019/020；T003→T019；T021整合读取与worker；T022→T023；T024–028整合验收。由控制侧顺序执行，独立复核使用独立只读上下文。

stage：obs-unit（T002/011/012/013/014/017/018/019/020）、obs-pg（T004/005/007/009/010/015/016/020）、obs-producers（T006/007/008/013）、obs-web（T021/022）、obs-console（T023）、obs-scenarios（T024）、obs-collector（T003/026）；T025/027/028绑定完整受影响检查。T001证据是文档校验、冻结和分析；不以虚构产品测试证明文档已实现。
