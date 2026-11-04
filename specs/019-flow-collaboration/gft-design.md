# G-FT 独立设计／探针复核材料

状态：2026-11-03，待**实际 Claude**独立复核。R3 模块卡与宪法 VII 已由用户全文批准；本文件不替代该批准，不代表 G-FT 设计／行为关口已通过。最终 US 收口仍由用户 review ②。

## 复核输入与前置

基线 main `ca9a0d29f185a548b14b595e6068c52df1c3cdf4`，当前 `019-flow-collaboration`。原审 R3 SHA-256 `514ce6c9f6a8bb1e86499c80a281403e858c007a92138f3b8771429c13a37ef5`；正式协议在 `docs/contracts/flow.md`，spec／plan／tasks 及实际进度在同目录。请直接读原始源码和判据，勿只依作者摘要；不批准 US 最终收口、不提交、不推送。

G-FT 分两关：设计／隔离探针复核在 FixedTool 写分支实施前；F04／F12／F17 行为及普通 Run 回归在写节点可交付前。允许先提取共享助手和做隔离探针；任何功能绿态不自动代替实际 reviewer 的结论。若方案需改变 Q2 范围，返回模块卡 review①，不能偷偷改成只读交付或旁路调用。

## 现有接缝及最小设计

| 接缝 | 已核对源码 | 提议落地与必须守住的边界 |
| --- | --- | --- |
| 真实工具结果 | core/tool_exec.go ExecuteStep／finishInvocation | core 内增加结构化内部结局：是否发出、原调用审计ID、工具结局、终态是否确认；原 Execute／ExecuteStep 继续投影 Result 文本。没有模型的固定节点不解析文本判断成功 |
| 计量与消息失败 | core/loop.go 串行工具组 | 提取同一个 assistant 追加／工具组 helper；真实调用及预算占用先记，不能因 AppendToolResult 失败免计；普通 Run 原纯文本／空content／多工具／取消语义回归保持 |
| FixedTool 的运行入口 | core/service.go Run；app/run_admission.go | 只由可信 app NodeRunner 构造、普通 HTTP DTO 没有 FixedTool 控制字段；保留原 Principal／Key、Agent UID／快照、会话与Run准入、RunQuota、账本、用户／assistant／tool历史；跳过 prompt专用检索、压缩、Chat配额及Chat调用，Iterations=0 |
| 动作与审计 | core/tool_exec.go beginInvocation →唯一Tool.Execute→finishInvocation | 固定规范参数／来源与schema版本→Prepare动作→原调用pending→动作尝试绑定原审计ID→BeforeDispatch以epoch／关闭意图／当前授权／预算／input hash再核对并持久dispatching→唯一外发→原审计终态→保留实际结局。任一预写失败零外发；终态未知不重放 |
| 钉住与只读 | core/tool_exec.go pinned tools fallback；core/store.go ToolPinner | 只在 Flow 可选路径缺 pinned实例／版本／schema／可信分类时拒绝，不走普通Run的全局resolver兼容回退；Agent节点只能用可信只读交集，名称／作者／模型不能自报只读 |
| 强Schema兼容 | tool既有 InputSchema；新 tool/schema.go | 部分既有内置schema没有显式additionalProperties，HTTP headers还用未批准的schema对象形式。Flow只接明确审计的内置适配规则，固定原schema hash＋Flow严格schema hash；不暗改旧非Flow工具schema，不忽略MCP或未知关键字，不支持的工具对Flow明确不可用 |
| 并发预留 | core/service.go实例计数；callers/limiter.go共享计数 | Flow类别在各自原总计数临界区原子核对total与Flow子上限，默认实例／caller各留1；同caller多Key共享，根框架不持叶子Run票据；失败／panic／取消仍共用原defer释放，实际退出前不释放 |
| 真实成功与验收 | flow/verification.go；flow/claims.go | 工具已知调用结局与业务验收分开；HTTP2xx不作通用业务生效证明。未知类工具默认不可安全重放。来源可读／hash／数量不等于支持目标，证据规则还须可信支持／冲突判据绑定目标／规则／来源版本；缺能力inconclusive，未知不换revision/action绕过 |

所有端口位于实际消费方 core／flow，适配器在 app／tool／postgres；core不import其它internal包或第三方库。FixedTool不写另一套模型／工具审计、不复制消息仓储；祖先预算与caller配额分别计量但同维度不重复扣。正常成功的固定工具Run计工具1次、caller Run1次；没有Chat不造Token。重试新Run仍收Run配额，同请求幂等不重复接受。

## 探针与当前真实证据

| 组 | 必须实测的判据 | 当前状态 |
| --- | --- | --- |
| P1 普通消息组兼容 | 纯文本／空content／多工具／工具失败／取消／消息回填失败，无重复消息、无孤儿tool，最终回答不丢 | 既有core全包回归通过；共享助手改造后的专门探针待跑 |
| P2 身份／版本拒绝 | 无身份、原Key撤销、scope缺失、UID／配置／manifest不符、伪造FixedTool零外发 | 新Flow真实PG／准入探针待跑 |
| P3 两层预写 | action prepared／dispatch许可／调用pending分别注入失败，fault-fired、零外发、组完整、无验收产物 | 既有pending fail-closed回归在core全包中通过；新动作账本探针待跑 |
| P4 固定调用计量 | 一个工具→一个Run配额／祖先工具占用；Chat／摘要／prompt检索为0；消息回填失败仍计真实调用 | 待共享助手／FixedTool分支探针，不能只断言Iteration=0 |
| P5 双名额 | Flow与普通Run竞争，实例／caller预留正确，拒绝／取消／panic释放，未退出不还票 | 既有callers定向回归通过；新Flow共享原子计数探针待跑 |
| P6 只读／强Schema | Agent请求写、未知MCP／伪造只读／缺pin／嵌套非法参数均零外发；可信只读正对照执行 | Schema组件4项具名Harness已通过；真实ToolExecutor零发出尚待接线，不以组件计数替身替代 |
| P7 输入固定 | 已验证上游映射正对照；dispatch前后换参／schema／同名实例拒绝，逻辑ID稳定 | 定义／Schema／上游前驱基础在实施；真实动作探针待跑 |
| P8 unknown故障 | 临时PG＋真实进程＋回环接收端，接收后审计终态前kill；重启／resume／revision调用次数不增长，关闭不清unknown／未知预留 | 待真实二进制与PG；不能用返回error替代kill |

原始回归日志：`.loop/us19-schema-regression.log`（core/tool全包）、`.loop/us19-verification.log`（三包）；局部Harness `.loop/runs/us19-t003-schema-2/`（fmt＋4 Schema具名）及 `.loop/runs/us19-foundation-1/`。独立Codex预审确认过现有接缝和原core/callers定点回归，但它不是实际Claude，不登记为G-FT通过。

## 请 Claude 给出的回执

1. 本最小共享边界是否守住 I／II／V／VII，以及哪些隔离探针必须先补，才能接受设计关口。
2. findings逐条给严重度、规格锚点、源码file:line、可复现反例；明确区分设计阻断与已批范围内实现细化。
3. 已实际审阅／运行和未覆盖面分别列出；不得凭作者摘要把P1–P8未跑项写绿。
4. 明确结论：G-FT设计关口通过／带具体前置／阻断。行为关口、US最终review②与提交权限不在此次回执内。

当前缺少可调用的实际Claude CLI：PATH、标准本地安装位置和WSL均未找到；若用户提供现有CLI路径可按只读review方式运行，不生成／读取模型厂商凭据，不另启动编码Agent承包实施。其余无依赖冲突任务继续。
