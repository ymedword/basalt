# Phase 0 Research: US-8 通知工具（notify）

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-18

**研究口径**：本 US 的设计面已在颗粒度文档 [docs/us/us8-notify.md](../../docs/us/us8-notify.md)（R2，经人工 review ①）冻结，本文件**不做新一轮选型**，只把已冻结决定整理成「决定 / 理由 / 备选与取舍」，并记录本轮在仓库中核实过的落地事实。每条决定都标注权威来源；出现「未在文档中规定」的，一律列在文末「未决事项」，不在此自决。

## D1 `${VAR}` 解析时机：发送时解析，且只有一份判定实现

- **决定**：渠道 `url` 只在**每次发送时**经进程环境解析；整串 `${VAR}` 判定收敛为 `core.EnvPlaceholderName(s) (name string, ok bool)` 唯一实现，`workspace/mcp.go`（原 `isEnvPlaceholder`）与 `cli/config.go`（原 `wholePlaceholder` 正则）改调它。
- **理由**：① 宪法「同类语义不得出现第二套实现」——三处调用点若各留一份判定，字符集或边界最先漂移；② 发送时解析使库内只存引用、进程环境是唯一真值（凭证零明文落盘，宪法架构约束「凭证纪律」）；③ 解析放装配时会与 US-12 的动态渠道管理冲突（改环境值需重启，改渠道行下一发生效）。
- **备选与取舍**：加载 / 装配时解析（`docs/TechnicalDesign.md` §3.7 的通用配置口径）——被 §2.4 裁决显式覆盖：通知渠道地址是**运行期数据**，不是启动配置；驳回。`tool` 包内复制一份判定——被独立复核 #1 判 CRITICAL（第二套实现），驳回。
- **来源**：颗粒度文档 §2.4（裁决 1）、§2.2 约束 4/5、§3.5 第 4 项；既有实现核实见 `internal/workspace/mcp.go:137-157`、`internal/cli/config.go:155`。

## D2 渠道名匹配：TrimSpace 后大小写敏感精确匹配

- **决定**：`channel` 先 `strings.TrimSpace`，再与注册表 `name` 做**大小写敏感的精确**比较；纯空白视同未提供（不猜默认渠道）。
- **理由**：与渠道名的存储语义（主键、区分大小写的 TEXT）一致；大小写折叠会让 `OPS` 命中 `ops`，掩盖登记错误，且 SQLite 默认 `=` 本身大小写敏感——两边口径必须一致。
- **备选**：`strings.EqualFold`（驳回：与 SQL 主键查询语义不一致）；纯空白按空串查表（驳回：会绕过「列出可用渠道名」的诊断路径）。
- **来源**：颗粒度文档 §2.3 图 A ④、§6.2 #3；spec.md Clarifications 收敛项 ①。

## D3 类型 → 适配器映射：装配层唯一真值

- **决定**：`type → 适配器` 只在 `internal/app/notify.go` 的 `newNotifyAdapters()` 构建一次（键恰好 `webhook` / `feishu`），以 `map[string]tool.NotifyAdapter` 注入 `NotifyTool`；工具内核只 `adapters[ch.Type]` 查表，不出现类型字符串分支。
- **理由**：① 依赖方向——`tool` 不得 import `app`，装配知识必须从 `app` 注入；② 宪法第 3 条的同一纪律（显式映射、不遍历推断）适用于所有「名字 → 实现」面；③ 加类型只改装配处一处，工具内核零改动。
- **备选**：`switch ch.Type` 写在工具里（驳回：新增类型要改工具内核，且「支持集」出现第二份真值）；适配器自注册（`init()` 全局注册表，驳回：隐式副作用、测试顺序敏感，违反显式装配）。
- **来源**：颗粒度文档 §2.2 约束 7/8、§6.2 #7/#8；宪法第 3 条；`internal/tool/registry.go` 拒绝重复注册的既有纪律（同构）。

## D4 成功判据：webhook = 2xx；feishu = 2xx 且回执 `code == 0`

- **决定**：webhook 以 HTTP 2xx 为成功（不读成功响应体）；feishu 必须 2xx **且** JSON 回执 `code` 为整数 0；3xx / 4xx / 5xx 一律 `external_error`。
- **理由**：飞书业务失败（关键词不匹配 `19024`、缺 content `19002`）同样返回 HTTP 200，只看状态码会把失败报成成功；webhook 面向自定义接收端，没有可依赖的业务回执格式，文档已把兼容承诺**收窄**为「接收端接受请求」（人工 review ① H1 修正了旧的过宽表述）。
- **备选**：webhook 读响应体并解析 `code`（驳回：发明文档外协议，且拒绝合法接收端的任意响应体）；webhook 只认 200（驳回：2xx 语义是 HTTP 契约，201/204 不应判失败）。
- **来源**：颗粒度文档 §2.2 约束 12/13、§6.2 #12/#13；`docs/contracts/agent-md.md` §5.5；真实飞书协议探针（2026-10-18：正确载荷 `code=0`；缺 content `code=19002`；空文本 `code=0`）。

## D5 飞书回执解析：不可解析即失败 + 显式 64 KiB 超限检测

- **决定**：回执读取用 `io.LimitReader(body, maxNotifyReceiptBytes+1)`，读回字节数 > 64 × 1024 即判「飞书回执无法解析」；未超限则整体 `json.Unmarshal` 到 `feishuNotifyReceipt{Code *int}`——空体、非 JSON、缺 `code`（`nil`）、`null`、非整数（含小数）、JSON 后尾随垃圾全部失败；回执可带 `StatusCode` / `msg` / `data` 等额外字段，**只判 `code`**。
- **理由**：① 「结果未知不冒充成功」——截断后解析可能恰好得到 `code:0`，把超限失败伪装成成功；多读 1 字节让超限成为**可判事实**而非推测；② `*int` 让「缺失」与「0」可区分；`json.Unmarshal` 对尾随非空白字节返回语法错误，天然满足「尾随垃圾 = 失败」；③ 额外字段容忍来自真实回执形态（探针观测 `StatusCode` / `msg` / `data`）。
- **备选**：`json.Decoder.Decode` 只解第一个值（驳回：会放过尾随垃圾）；`map[string]any` + 类型断言（驳回：`null` 与缺失区分靠两段判断，易漏）；读满 64 KiB 后截断解析（驳回：正是文档禁止的伪成功）。
- **来源**：颗粒度文档 §2.2 约束 14~16、§6.2 #14/#15；spec.md Clarifications 收敛项 ④；真实回执形态（2026-10-18 探针）。

## D6 HTTP 客户端：复用 `newHTTPClient()`——不跟随重定向、不读环境代理

- **决定**：两个适配器都构造一次 `newHTTPClient()`（`CheckRedirect` 返回 `http.ErrUseLastResponse`；`Transport.Proxy = nil`），每个适配器持有一个不可变的 `httpDoer`；测试经非导出构造器注入 `fakeDoer`。
- **理由**：① 3xx 必须作为**原始响应**交回判定（跟随重定向会改变最终目标，绕过发送前的策略判定对象）；② `Proxy = nil` 使行为不随环境变化、且避免把请求交给进程环境声明的代理（确定性 + 凭证不外泄）；③ 与 `http_get` / `http_post` 既有行为同构，运维只需理解一套 HTTP 语义。
- **备选**：`http.DefaultClient`（驳回：会读 `HTTP_PROXY`、默认跟随 10 次重定向）；每发新建客户端（驳回：连接池无法复用，且 `Transport.Clone` 成本白付）。
- **来源**：颗粒度文档 §2.2 约束 17、§6.2 #17；`internal/tool/httptools.go:170-186`。

## D7 ctx 身份优先：取消 / 截止返回裸哨兵

- **决定**：适配器在把发送错误包装成 `ExternalError` 之前、以及在回执读取错误返回之前，先判 `ctx.Err()`；命中即返回裸 `ctx.Err()`（或 `ctxPreferred(ctx, err)` 的既有哨兵）。执行器据此把 `context.Canceled` / `DeadlineExceeded` 归 `timeout` 而非 `external_error`。
- **理由**：取消不是外部系统失败。若包装成 `ExternalError`，审计 `error_kind` 会把「用户 / 执行器取消」误记为「外部依赖故障」，排障方向被带偏；且 `ExternalError.Error()` 会渲染 `Reason`，可能拼进不该出现的上下文。
- **备选**：`errors.Is(err, context.Canceled)` 在包装里保链（部分可行，但 `ExternalError` 的分类优先级在 `classifyToolError` 中高于 ctx——包装后仍归 external，驳回）。
- **来源**：颗粒度文档 §2.2 约束 19、§6.2 #19；分类判序见 `internal/core/tool_exec.go:221-246`；既有工具 `ctxPreferred` 在 `internal/tool/httptools.go:538-553`。

## D8 失败分类：复用契约七值，不新增 error_kind

- **决定**：`invalid_input`（入参 / 超限）、`not_found`（渠道缺失 / 未知，经 `notifyNotFoundError.Unwrap() → fs.ErrNotExist`）、`sandbox_denied`（策略拒绝）、`external_error`（传输 / 回执 / 非 2xx）、`runtime_error`（注册表读取、环境解析、未知类型、配置装配失败，**以及执行器兜底**——`classifyToolError` 的 `default` 分支）、`timeout`（ctx）——`notify` 不新增任何错误类别；契约第七值 `unknown` **不在本工具产生**（它只由崩溃恢复扫描写给半截的 `pending` 行）。
- **理由**：七值是 `docs/contracts/run-and-errors.md` §7.3 的冻结字面量，审计 / 观测 / 排障都按它分组；新增类别是判据面变更，需要裁决，而现有七值已能覆盖全部失败面（人工 review ① 已裁决「配置装配失败归 `runtime_error`」）。
- **备选**：新增 `config_error` / `notify_error`（驳回：契约变更 + 无验收依据）；把未知类型归 `invalid_input`（驳回：错误源是登记数据不是模型入参，模型重试无意义）。
- **来源**：颗粒度文档 §2.3 失败判据表、§2.4 裁决 2、§6.2 #8；`docs/contracts/run-and-errors.md` §7.3。

## D9 凭证红线：只出 `scheme://host`，其余位置一律脱敏

- **决定**：`notifyDisplayURL(rawURL)` 解析后只返回 `scheme://host`（解析不出返回既有 `targetUnresolvable` 占位）；沙箱拒绝**保留** `Rule` / `Guidance` 原文、只重写 `Target`；传输错误 `Reason` = 固定类别短语 + `：` + `scheme://host`，原始错误只进 `Cause`；工具与适配器不持有 logger，任何路径都不打印 `url` 原值。
- **理由**：`url` 的 path / query / userinfo 都可能承载 token（飞书 webhook 的 path 就是 token）；日志、审计 `error_message`、模型可见结果三处任一泄露都等于凭证落盘。`scheme://host` 保留排障所需的「打到哪台主机」信息，同时不含可复用凭证。
- **备选**：全量脱敏为 `[redacted]`（驳回：排障失去主机维度）；对 path 做哈希（驳回：新概念、无验收依据）。
- **来源**：颗粒度文档 §2.2 约束 11、§6.2 #10/#11；宪法架构约束「凭证纪律」；spec.md FR-011 / FR-012。

## D10 审计与可观测性：完全复用既有执行器两阶段写

- **决定**：`NotifyTool` 与两个适配器**零审计、零消息、零日志**（构造函数不接 logger / 仓储）；调用前后的 `pending` 预写、终态回填、失败 WARN 全部由既有 `core.ToolExecutor` 承担。`notify` 只作为普通 `core.Tool` 注册进池。
- **理由**：① 宪法第 2 条 / 第 5 条——消息与审计的写入路径唯一，工具不是第二写入口；② 预写失败必须**拒绝发出**，这只有执行器在执行前能保证；工具自审会让「先发送后落库」在代码结构上成为可能。
- **备选**：工具内自审（驳回：双写路径 + fail-closed 无法保证）；适配器记 WARN（驳回：凭证红线 + 双轨职责归执行器）。
- **来源**：颗粒度文档 §2.2 约束 20~22、§6.2 #20~#22；`docs/TechnicalDesign.md` §4.4；R12 用例（`blockingNotifyAdapter` 阻塞期间观察 `pending` 行）。

## D11 测试策略：默认层全覆盖，真实外呼不进 CI

- **决定**：42 条断言全部落在默认层（`go test -race ./...`），适配器经 `fakeDoer` / 回环 `httptest` 驱动；**无集成层**，真实 webhook / 飞书发送归 §6.1 人工项（四态登记）。
- **理由**：真实渠道需要凭证与外部账号，CI 不可复现；把「接收端真实收到」写进自动化会引入网络不确定性，并让凭证进入 CI 环境。默认层仍能覆盖全部判定面：载荷字节、状态码分支、回执矩阵、超限、重定向、代理、ctx、脱敏文案。
- **备选**：`-tags=integration` 下打真实飞书（驳回：凭证管理 + 外部抖动 + 与「不得明文落盘」冲突）；录制回放（驳回：新依赖、新工件，本 US 零新增）。
- **来源**：颗粒度文档 §4.1（测试分层）；`harness/acceptance.json`；陷阱 55（跳过须给依据）。

## D12 注册表读取：每次发送一次、只读池、`ORDER BY name`、零缓存

- **决定**：`NotifyChannelRepo.ListNotifyChannels` 每次调用都执行 `SELECT name, type, url FROM notify_channels ORDER BY name`，走 `db.read`；空表返回空切片（非 nil 或 nil 均可，但错误文案须能区分「注册表为空」）。
- **理由**：① US-12 的手工 / 管理端改动必须在**下一发**生效，缓存会引入失效策略与配置键（本 US 零新增配置键）；② 只读查询走只读池与既有先例一致（`tool_invocations` 的读取路径）；③ `ORDER BY name` 让「可用渠道名」文案稳定，测试可判。
- **备选**：进程内缓存 + 定时刷新（驳回：新配置键 + 失效语义）；在装配时预热一次（驳回：与「构造后新增行可见」的 FR 冲突）。
- **来源**：颗粒度文档 §2.2 约束 6、§6.2 #6；`internal/storage/db.go:24-26`、`internal/storage/tool_invocations.go:124`。

## 写前核实清单（本轮已核实 / 待实现期复核）

| # | 核实项 | 结论 | 证据 |
| --- | --- | --- | --- |
| 1 | `notify_channels` 表已存在且两处契约一致 | 已核实 | `internal/storage/schema.sql:216-222` 与 `docs/contracts/schema.sql:216-222` 逐字相同 |
| 2 | `${VAR}` 两处既有实现语义等价 | 已核实 | `internal/workspace/mcp.go:142-157` 逐字符判定；`internal/cli/config.go:155` 正则；两者接受集合相同 |
| 3 | `newHTTPClient` 不跟随重定向、不读代理 | 已核实 | `internal/tool/httptools.go:170-186` |
| 4 | 分类判序：沙箱 → 入参 → ctx → external → `fs.ErrNotExist` → `runtime_error` | 已核实 | `internal/core/tool_exec.go:221-246` |
| 5 | `ToolEnv.AgentName` 可用于策略判定 | 已核实 | `internal/core/run.go:119-126` |
| 6 | `fakeDoer` 与回环 `httptest` 夹具存在 | 已核实 | `internal/tool/httptools_test.go:68`、`internal/testutil/httpserver.go:14` |
| 7 | 注册循环 / 门禁 / `declaredToolNames` 行号 | 已核实 | `internal/app/wire.go:232-246` / `:253-257` / `:418-421` |
| 8 | `ports_test.go` 当前八件断言 | 已核实 | `internal/app/ports_test.go:301-327` |
| 9 | 飞书回执真实形态（`StatusCode` / `code` / `msg` / `data`） | 已核实（2026-10-18 探针） | 正确载荷 `code=0`；缺 content `code=19002`；空文本 `code=0` |
| 10 | 编译期依赖（`encoding/json` 对尾随垃圾报错、`*int` 区分 `null`） | 标准库行为，按 Go 文档；**实现期以测试实测为准** | 写前核实纪律（H3） |

## 未决事项

无。设计面全部由颗粒度文档 R2 与人工 review ① 的裁决覆盖；实现期若出现文档未规定的边界，按颗粒度文档「待决事项」口径停下登记，不在此自决。
