# G-FT 实际 Claude 设计回执与前置跟踪

2026-11-03，由用户转贴当前 Claude 会话的只读评审结论，无需安装 CLI。审阅材料 `gft-design.md` 为 48 行，SHA-256 `53c556d0c687e5c5d0e494c98131f98981476b9851632fee5c844d246708aa15`；保留该文件原字节作为此次输入，后续实现证据追加于本文件。此回执不是行为关口或 US 最终收口批准。

## Claude 回执

**结论：带具体前置通过。** 设计方向能守住 I／II／V／VII，但写分支在下列前置闭合前不得开启；这不是 G-FT 行为关口通过，也不代替 US 收口。

实际审阅：core 的 flow／schema／tool_exec／loop／service、tool 的 schema 与测试、app/run_admission、flow 的 ports／state、模块卡 §6.6.2、宪法 v6.0.0 与 REGISTRY 第 19 行。实际运行 `GOPROXY=https://goproxy.cn go test ./internal/flow ./internal/tool -run 'TestFlow_' -count=1`，两包 ok。未跑真实 PG／进程／ToolExecutor P1–P8、core 全包回归、25 FR／28 tasks 逐行重审、真模型 Q5。

| ID | 严重度／问题 | 必须处置 | 状态 |
| --- | --- | --- | --- |
| GFT-01 | 前置·高：pending 后没有 BeforeDispatch 插入点，只有文本 Result | Flow 专用执行方法复用原 begin／finish，在唯一 Tool.Execute 前持久许可，返回结构化结局；P3 零外发注入 | 待实现／探针 |
| GFT-02 | 前置·高：动作尝试无法在外发前绑定原审计 ID | BeforeDispatch 接收并持久 auditID，崩溃后两行可互证，不重放 unknown | 待实现／探针 |
| GFT-03 | 前置·中：AfterCall 只有文本，失败／未知可能误算进展 | 接收 Dispatched／AuditConfirmed 等结构化事实，Result 只作文本投影；P7／P11 负向 | 待实现／探针 |
| GFT-04 | 前置·中：FixedTool 先经 pinnedClient，没有模型映射即失败 | 固定分支在 pinnedClient 前分流，保留资源／身份固定；真实零模型探针 | 设计已接纳，写分支未开启 |
| GFT-05 | 前置·中：Flow pinned 缺失仍 fallback 全局池 | 专用方法严格拒绝，P6 全局有同名但 pin 缺失零外发 | 待实现／探针 |
| GFT-06 | 前置·中：固定 assistant(tool_calls) 来源未定义 | 同一共享 helper 明确接收固定调用来源标记；P1／P4 单组历史且零模型 | 待实现／探针 |
| GFT-07 | 记录·低：Schema 本地计数器不是真执行器 | 保留组件证据标注，P6 用真实 ToolExecutor＋回环接收端计数 | 待探针 |

Claude 指定：写分支前必须 GFT-01–06＋P1（共享 helper 普通 Run 回归）／P3（两层预写失败零外发）／P6（真实执行器严格 pin／Schema）／P7（输入冻结）；P2／P5／P8 属行为关口，写节点交付前跑。未跑不得写绿。I 的标准库边界成立；II／V 方向成立但 01／02／06 未闭合不能证明；VII 显式 context 与普通同步成立，类级例外已获宪法批准。无新增案例。

非 G-FT 指出模块卡状态及四份镜像未同步；实际本工作树已存在 TechnicalDesign §2.8／技术栈、Requirements §8、Compatibility §9、CLAUDE 原则 VII 修改（私有忽略文件，不出现在 Git diff）。本次再次逐字核对存在；模块卡状态行已据用户真实批准同步，历史批准 hash 保留。

## 实现与探针证据（只追加）

收到回执时写分支仍关闭，定义验证器 `WriteGate=false`；尚无 FixedTool 执行入口。T007 保持未勾选，待前置实际闭合。无需安装 CLI 或新增模型凭据。

### 2026-11-03 前置闭合证据

以下是收到条件回执后的实现与隔离探针，不改写原 Claude 回执，不声称 Claude 再次运行或批准了 US 收口。独立 Codex 两次只读复验未发现定点前置仍有阻断；实际 Claude 的条件结论仍为授权依据。

| 前置 | 实现／实际证据 | 界限 |
| --- | --- | --- |
| GFT-01／02、P3 | 唯一 ToolExecutor 的 pending→持久绑定原审计ID→dispatching→唯一外发；真实PG＋本机HTTP接收端查询绑定，四种预写故障由非事务sequence证明触发且0外发；.loop/us19-pg-action-probes-first.log | 原审计唯一归属未变；不是进程kill证据 |
| GFT-03 | ActionOutcome含int64审计ID／Dispatched／AuditConfirmed；动作观察或预算结算失败不更新AfterCall；.loop/us19-pg-and-gft-guards2.log、us19-gft-core-prerequisites.log | 完整持久进展算法待T017 |
| GFT-04 | 受控只读的完整AgentService.Run通过，缺模型映射时Provider lookup／Chat／Memory／Knowledge／压缩／ChatQuota均0；原RunQuota1、Run行1、三类消息各1、Iterations0／ToolCalls1、祖先工具1／Token0；拒RunQuota／写适配器均零外发；.loop/us19-gft04-full-readonly-first.log，独立复跑core全包通过 | FlowRunsReady默认false，生产app未设置；只读原型不是正式写节点／PG完整Run接线交付 |
| GFT-05、P6 | Flow严格pin且无全局fallback；真实执行器的嵌套Schema、身份、分类、hash负例及接收端正对照；.loop/us19-executor-probes-first.log | 不以Schema本地计数器替代真实执行器 |
| GFT-06、P1 | 固定assistant显式来源、共享组唯一追加；普通Run全回归和Flow预写失败补齐工具消息通过；.loop/us19-flow-group-prerequisites.log | 所有写适配器仍由共享组无条件readonly拒绝 |
| P7 | 当前已验证上游／hash／权限复核、调用前冻结及换schema拒绝；发现并修复binder替换参数的共享组反例，现先比较共享规范语义再采用规范字节；1→2拒绝、1.00→1接受；逆改自身比较后断言红且已逐字恢复；.loop/us19-input-freezing.log、us19-binder-input-inverse-red.log、us19-binder-input-restored-green.log | 输入规范算法归core标准库一份，flow委托；来源实际适配与完整app接线仍待后续 |

结论：实际 Claude 所列设计／隔离前置已逐项闭合，可进入受控写分支实现；生产写节点仍保持关闭，必须经P2／P5／P8及F04／F12／F17行为证据后才可交付。T007控制侧在本轮Harness与普通回归记录核对后勾选。人工review②、commit／push／merge权限均未改变；无新增厂商凭据、无付费模型请求。
