# Phase 0 研究：US-9 Web Service

**Feature**: `009-web-service` | **Date**: 2026-10-18 | **Plan**: [plan.md](./plan.md)

**研究立场**：设计面已由颗粒度文档 [docs/us/us9-web-service.md](../../docs/us/us9-web-service.md)（R4，人工 review ① 2026-10-18 通过，sha256 `5195dbee…`）§2 / §3 冻结，固定字面量以 [docs/contracts/](../../docs/contracts/) 为准。本文件不重新做设计取舍，只把「为什么这样落地」写成可复核的决策记录，并列出**写代码前必须在本地依赖 / 文档中核实**的清单。每条决策的事实来源只引已提交工件与仓库实测，不引记忆。

## D1 路由：标准库 `ServeMux` 方法模式 + 方法无关 catch-all

- **决定**：`routes()` 只在一处注册全部路径：`GET /api/v1/health`、`GET /api/v1/info`、`GET /api/v1/profiles`、`GET /api/v1/tools`、`GET /api/v1/memory`、`POST /api/v1/sessions`、`GET /api/v1/sessions/{id}`、`DELETE /api/v1/sessions/{id}`、`POST /api/v1/sessions/{id}/messages`、`POST /api/v1/agents/{name}/invoke`，外加方法无关的 `/api/v1/` 兜底 → `handleNotFound`（404 信封）。不注册 `GET /api/v1/sessions`（属 US-11）。
- **理由**：Go 1.22+ 的 `ServeMux` 原生支持「方法 + 路径通配符」，`r.PathValue("id")` 直接取路径参数，零新增坐标。catch-all 是**让错误方法也落统一信封**的关键：若只依赖 `ServeMux` 的自动 405，响应是纯文本 `Method Not Allowed`，不符合 FR1「未匹配的路径与方法统一 404 信封」。catch-all 作为完整匹配（路径 + 任意方法）存在时，`ServeMux` 不再走 405 分支。
- **备选与取舍**：① chi / gorilla/mux——新增第三方坐标、超出交付物清单，且本项目路由数量固定，收益不抵坐标成本；② 手写路径解析——重复造路由、通配符与转义要自己兜；③ 只靠 `ServeMux` 自动 405——响应体不是信封。
- **来源**：颗粒度文档 §2.3 伪代码 `routes` / `handleNotFound`、§2.4 注 5（405 → 404）；spec FR1 / FR15；`docs/contracts/openapi.yaml` 的端点集合。

## D2 信封：单一出口，DELETE 成功是唯一无载荷成功

- **决定**：`Envelope{Code, Message, Data any json:"data,omitempty", Timestamp}`；`writeOK`（200 + `code=OK` + `message=success`）、`writeResult`（200 但 code 非 OK，仅 LIMIT_REACHED）、`writeError`（失败状态码 + code + message，**永不设 Data**）三个方法之外不写 JSON。`handleArchiveSession` 调 `writeOK(w, nil)`——`nil` 接口被 `omitempty` 省略，得到唯一无载荷成功。`timestamp` 一律 `opts.Now().UTC().Format(time.RFC3339)`。
- **理由**：信封形态（成功含 `data` / 失败省略 `data` / 时间戳 UTC）要可判卷，唯一可靠做法是**只有一个出口**；handler 各自 `json.Marshal` 意味着 20 多个错误分支各写一次形态，必然漂移。`Data any` 的 `omitempty` 只在接口为 nil 时省略，因此「成功但数据为空串 / 空数组」仍会输出（空数组必须是非 nil 切片，见 D14 与 data-model 的 `[]` 约定）。
- **备选与取舍**：① 每 handler 自己 marshal——形态无法统一；② 泛型 `writeJSON[T]`——DELETE 无载荷场景没有类型实参，仍要特殊分支，不如两个出口直白；③ 用 `Data *T`——多一层指针，视图构造点全要取地址。
- **来源**：颗粒度文档 §3.1 `envelope.go` 全签名、§2.4 注 4 / 注 8；`docs/contracts/openapi.yaml` `components.schemas.Envelope`；`run-and-errors.md` §8。

## D3 错误映射按错误类型，不按文本

- **决定**：`writeRunError` 用 `errors.As` / `errors.Is` 判：`*core.SessionBusyError` → 409 `SESSION_BUSY`（冻结文案）；`*core.InstanceFullError` → 429 `TOO_MANY_RUNS`（message 点名当前上限）；`context.DeadlineExceeded` → 504 `TIMEOUT`；其余 → 500 `INTERNAL`（固定文案「服务器内部错误」，原始错误记 ERROR 日志）。`writeStoreError` 把 `fs.ErrNotExist` → 404，其余 → 500。
- **理由**：错误文本是给人读的，随文案调整而变；错误类型是 `core` 已导出的契约面（[internal/core/errors.go:95](../../internal/core/errors.go#L95) `SessionBusyError`、[:106](../../internal/core/errors.go#L106) `InstanceFullError`）。按文本匹配会让「文案改一个标点 → 状态码静默变 500」。
- **备选与取舍**：① 字符串前缀匹配——被 §2.3 的映射纪律排除；② HTTP handler 自己判 `res.Status`——`Run` 的函数级 error 才表达准入失败，`RunResult.Status=failed` 是正常 200 路径（见 D11），两者不可混；③ 让 `core` 返回 HTTP 状态码——依赖方向反转，`core` 不能知道 HTTP。
- **来源**：颗粒度文档 §2.3 `writeRunError` / `writeStoreError`、§2.2 约束「映射按错误类型」；`internal/core/errors.go`。

## D4 归档与运行准入：服务级一把锁 + 在途计数表

- **决定**：`Server` 持有 `admissionMu sync.Mutex` 与 `inflightRuns map[string]int`。`admitRun(ctx, id)` 在锁内完成「`GetSession` → 状态复核（archived → `errSessionArchived`）→ `inflightRuns[id]++`」；`releaseRun(id)` 在锁内递减、归零即 `delete`（防止 map 无界增长）；`archiveSession(ctx, id, at)` 在锁内完成「`GetSession` → `inflightRuns[id] > 0` → `errSessionRunning`，否则 `ArchiveSession`」。运行期间的 `releaseRun` 由 `defer` 覆盖成功 / 失败 / 取消全部路径。
- **理由**：这是 check-then-act 竞态的唯一收敛点——「会话是否在跑」与「现在允许归档吗」必须由同一把锁裁决，否则归档可能穿过「状态仍是 active、运行刚要登记」的窗口。SQLite 没有跨请求行锁，且运行生命周期跨越多次 DB 操作，事务无法覆盖；在途登记本质是**进程内服务实例状态**，与服务同生命周期才正确。
- **备选与取舍**：① DB 事务 + `SELECT ... FOR UPDATE`——SQLite 无行锁语义，且事务提交后运行仍在继续，无法表达「在途」；② `sync.Map` / 原子 CAS——两个字段（状态 + 计数）耦合，CAS 需要自己造重试循环；③ 单独 goroutine 串行化命令——引入异步编排，违反宪法 VII；④ 不做互斥、靠归档时再查一次——人工 review ① 指出的竞态窗口仍然存在。
- **来源**：颗粒度文档 §2.2 约束 30 / 36、§2.3 `admitRun` / `releaseRun` / `archiveSession` 伪代码、§2.4 注 13（线性化）、R4 同步屏障测试、D-01 裁决（忙时归档 409 零写入）。

## D5 会话标识：`core.DeriveSessionID` 唯一实现 + 安全前缀回落

- **决定**：把 CLI 现有派生逻辑迁入 `core.DeriveSessionID(channel, user, profile)`——域串 `basalt/session/v1` 做域分离、组件以 `0x1F` 分隔并带长度前缀、SHA-256 取前 16 hex、前缀 `channel + "-"`；`channel` 前缀先按 `^[a-z0-9][a-z0-9-]{0,31}$` 校验，不合法回落固定 `ch`。`cli.deriveSessionID` 保留原签名、函数体逐字委托。固定向量：`("cli","alice","demo") → cli-a2025c15b5cbc303`、`("web","anonymous","demo") → web-bb27c35eeb3cc34a`。
- **理由**：幂等建会话要求 `(channel, user, agent)` 的确定性映射；两份实现（CLI + web）必然漂移，且既有 CLI 实现没有前缀校验——恶意 `channel`（含 `/`、换行、超长串）会直接进入 `session_id`，进而进入 SQL 参数与日志。迁移到 `core` 同时解决「唯一实现」与「输入约束」。
- **备选与取舍**：① UUID v4——每次不同，幂等直接失效；② web 里另写一份派生——双实现漂移；③ 对非法 channel 直接 400 拒绝——会改变 CLI 既有行为（既有 channel 由调用方控制），回落更小侵入。
- **来源**：颗粒度文档 §3.1 `core/sessionid.go`、§3.5 #6（等价重构）、R13 / `core/sessionid_test.go`（5 条，含固定向量与恶意前缀回落）；[internal/cli/chat.go:222](../../internal/cli/chat.go#L222) 现状。

## D6 输入边界：`MaxBytesReader` + 二次 decode 判 EOF + 运行前字节判定

- **决定**：`decodeBody` 用 `http.MaxBytesReader(w, r.Body, opts.MaxRequestBytes)` 包体、`json.NewDecoder` 解码一次，再 `Decode(&struct{}{})` 要求返回 `io.EOF`（否则 400「请求体含多余内容」）；`*http.MaxBytesError` → 400 `BAD_REQUEST`（**不是** 413）；未知字段忽略（默认行为，不调 `DisallowUnknownFields`）。单条消息在**调用 `Run` 之前**按 `len(*body.Message)`（Go 字符串字节数，即 UTF-8 字节）判定：`> 32768` → 400。必填字段用指针区分「缺失 / null」（nil → 400「message 必填」）与「空串」（允许）。
- **理由**：契约写死「请求体超限 400（不是 413）」；`MaxBytesReader` 默认会让 `http.Server` 回 413 纯文本，必须由 handler 捕获 `*http.MaxBytesError` 自行落信封。「运行前判定」保证超限消息不产生任何 Run 行、不写任何消息。
- **备选与取舍**：① `http.MaxBytesHandler`——内部写死 413 文本；② `io.ReadAll` 全量读再判——超限前已经分配内存；③ 用 `utf8.RuneCountInString`——契约口径是 UTF-8 **字节**，不是字符数（32768 / 32769 的边界用例即按字节）；④ `DisallowUnknownFields`——契约要求未知字段忽略。
- **来源**：颗粒度文档 §2.3 `handlePostMessage`、§2.4 注 1 / 注 9、R6 / R7 / R8；`openapi.yaml` 请求体说明。

## D7 运行时限：60s → 504，运行由 core 记 `cancelled`

- **决定**：`runCtx, cancel := context.WithTimeout(r.Context(), opts.RunTimeout)`（缺省 60s）传给 `Runs.Run`；返回后先判「客户端是否已断开」（断开则不写响应，只记 INFO），再 `err != nil` 走错误映射；最后 `errors.Is(runCtx.Err(), context.DeadlineExceeded)` → 504 `TIMEOUT`「运行超过 60 秒时限」。运行账本的 `cancelled` 终态由 `core` 既有取消路径写，web 不碰。
- **理由**：HTTP 客户端需要一个确定的超时结果码；而「运行被取消」属于执行链的账本语义，只能由 `AgentService` / `ReActLoop` 写，web 层写就是第二套追加路径。
- **备选与取舍**：① 不设时限——客户端永久挂起；② `http.TimeoutHandler`——写死 503 纯文本，且无法区分客户端断开；③ 在 web 层自己改 Run 状态——违反宪法 II。
- **来源**：颗粒度文档 FR11 对应约束、§2.3 `handlePostMessage`、R9。

## D8 写超时：读 60 + 运行 60 + 余量 10 = 130s

- **决定**：`newHTTPServer` 构造 `http.Server`：`ReadHeaderTimeout=5s`、`ReadTimeout=60s`、`IdleTimeout=120s`、`WriteTimeout = opts.RunTimeout + readTimeout + writeMargin(10s)`（缺省 130s）；包内测试用非导出的 `writeTimeoutOverride` 设置更小值做真实连接测试（`R19`）。
- **理由**：写超时从「请求到达」开始计时（Go 的 `WriteTimeout` 语义含读阶段），必须覆盖「慢慢读完 body（≤60s）+ 跑满运行时限（60s）+ 序列化余量」，否则长运行会在服务器侧被半途切断，客户端拿到的是断连而不是 504。人工 review ① 明确指出过早期 90s 设计短于该组合。
- **备选与取舍**：① 固定 90s——短于读 + 跑；② 不设写超时——慢客户端可长期占用连接；③ 用 `http.ResponseController` 按 handler 延长——Go 1.20+ 可行但需要每个长运行 handler 手动调用，漏一处即回归。
- **来源**：颗粒度文档 §2.4 注 14、R19（慢 body + 长运行的真实回环 TCP 测试）、§3.1 `writeTimeout`。

## D9 停机：五阶段，基上下文与信号解耦

- **决定**：`Server.Run(ctx)`：监听（或用 `opts.Listener` 测试缝）→ 起 `srv.Serve` goroutine → `select` 等 `errCh` 或 `ctx.Done()`。信号路径：① `srv.Shutdown(70s grace)`（先关监听、再等在途 HTTP 请求）→ ② 宽限到期则进入统一收尾 `stopServing`：`enterDraining()` + `cancelBase()` + `srv.Close()`（取消在途请求上下文、断开活动连接）→ ③ `waitInflight(5s)` 有界等待 handler 收敛（`sync.WaitGroup` 由中间件计数）；到期只记 ERROR，**继续** `inflight.Wait()` 直到归零（等待超时不是完成点）→ ④ `Run` 返回（宽限耗尽时返回带 `%w` 的错误）→ ⑤ 调用方 `runWithRuntime` 的 `defer` 关闭 `heavyRuntime`（MCP → DB → 锁）。**`Serve` 错误路径**同样经 `stopServing`（③④⑤），不得直接返回。`BaseContext = context.WithCancel(context.WithoutCancel(ctx))`——信号只在宽限耗尽时经 `cancelBase()` 传导。draining 标志与「登记在途 handler」在同一把锁下互斥，等待组不会在 `Wait` 之后增长（收口复核修复轮）。
- **理由**：FR14 明确「宽限期不得被信号直接取消」：正常停机要等在途运行自然完成；`WithoutCancel` 把信号与请求上下文切开，`cancelBase` 是唯一的取消入口。资源释放必须在 `Run` 返回**之后**（否则运行中的请求会看到已关闭的 DB），这正是 `runWithRuntime` 抽取的动机。`Serve` 错误（监听器被外部关闭等）同样意味着「服务已不能接入」，但仍可能有在途 handler 触碰共享资源，因此必须走同一收尾路径；等待超时只说明收尾慢，不说明已退出——安全优先于活性。
- **备选与取舍**：① 直接把信号 ctx 作为 `BaseContext`——一收到信号所有运行立即取消，与宽限期矛盾；② `Shutdown` 之后不 `Close`——宽限到期后活动连接不释放，`Run` 无法返回；③ 不等待 handler——DB 在 handler 收尾前被关；④ `Serve` 错误直接返回 / 收尾超时视为退出——已由用户故障探针证伪（`Run` 返回时 handler 仍存活）；⑤ 等待超时后由外部 SIGKILL 兜底——那是放弃清理，不属于进程自身的收尾语义。
- **来源**：颗粒度文档 §2.3 `Server.Run` 伪代码、§3.5 #16、R16（监听先关、运行后收尾的事件序）。

## D10 跨源保护：标准库 `http.CrossOriginProtection`，403 信封，零 CORS 头

- **决定**：`NewServer` 构造 `http.NewCrossOriginProtection()`（零值语义即可用），`SetDenyHandler` 直接写 403 信封（`code=BAD_REQUEST`，message「跨源请求被拒绝」），**不**调用 `AddTrustedOrigin` / `AddInsecureBypassPattern`，不输出任何 `Access-Control-Allow-*` 头。
- **理由**：Go 1.25+ 标准库自带跨站写保护：安全方法（GET/HEAD/OPTIONS）恒放行；非安全方法按 `Sec-Fetch-Site` 或 `Origin` 与 `Host` 比对判定跨站；两个头都缺失时按「非浏览器同源」放行。这正好覆盖「无认证的本地 HTTP 面不能被任意网页跨站写」的威胁，而 CORS `*` 恰恰是把写权限开放给任意页面（人工 review ① 已否决）。
- **备选与取舍**：① 手写 Origin 校验——要自行覆盖 Fetch Metadata、缺头回退、`Origin: null` 等边角，漏检面大；② CORS 白名单配置——契约边界外（明确不做「CORS 白名单配置」「跨域读取开放」）；③ 不做保护——默认监听回环只防了网络面，防不了同机浏览器。
- **来源**：`go doc net/http.CrossOriginProtection`（本轮实测：`NewCrossOriginProtection` / `Check` / `SetDenyHandler` 存在；安全方法恒允许；缺头按同源放行）；颗粒度文档 §2.2 约束 12、R5（恶意 Origin 零副作用）；人工 review ① 决定。

## D11 对外原因脱敏：固定投影，原文只入库与日志

- **决定**：`publicRunReason(status, reason)`：`failed` → 「运行失败」；`cancelled` → 「运行已取消」；`limit_reached` → 仅 `core.ReasonIterationLimit` / `core.ReasonToolBudget` 原样保留，其余 → 「运行达到上限」。`limitReachedMessage(reason)` 对已知两种 reason 输出点名配置键的 message（`budget.max_iterations` / `budget.max_tool_calls`），未知 reason 只回「运行达到上限」。500 的 message 固定「服务器内部错误」；原始错误只进 ERROR 日志与 `runs.reason`。
- **理由**：`RunResult.Reason` 可能包含 provider 响应片段、文件路径或内部错误文本；契约要求「运行原因原文透出」属于明确不做项。已知的两种上限 reason 是**公开语义**（用户能据此调配置），可以原样出现；其余一律投影。
- **备选与取舍**：① 原样透传 `Reason`——泄露面；② 全部投影为「运行失败」——用户无从知道该调哪个配置键；③ 在 `core` 里就把 reason 改成公开文案——毁掉审计与排障所需原文。
- **来源**：颗粒度文档 §3.1 `envelope.go`、§2.4 注 15、R11（四种终态脱敏夹具）、`run-and-errors.md` §3.1 / §8。

## D12 运行预算：`app.webBudget(rt.cfg)` 按 `cli.chatBudget` 同口径映射

- **决定**：`webBudget(cfg *cli.Config) core.Budget` 只映射四个键：`MaxIterations`、`MaxToolCalls`、`ContextTokens` 直接取；`MaxOutputBytes`（配置面 `int64`）仅当 `0 < v <= math.MaxInt` 时搬运，超范围丢弃（留零值由运行入口补缺省）。`ToolTimeout` 不在配置面，保持零值。`cfg == nil` → 全零预算。`newWebServer` 把结果放进 `web.Options.RunBudget`；handler 构造 `core.RunRequest` 时原样带上。
- **理由**：与 CLI 的 `chatBudget`（[internal/cli/chat.go:323](../../internal/cli/chat.go#L323)）保持**同一映射语义**：缺省值不在适配层补（补了就成了第二套缺省来源），`int64 → int` 溢出时丢弃而不是截断。`mergeBudget` 内含 CLI 单次运行的硬编码缺省，REST 路径使用它会把配置值掩盖掉（FR20 明确禁止）。
- **备选与取舍**：① 复用 `mergeBudget`——带硬编码缺省，配置值失效；② handler 内读配置——`web` 不能依赖 `cli`；③ 在 web 层给 `ToolTimeout` 补 30s——第二个缺省来源。
- **来源**：颗粒度文档 §3.1 `app/web.go`、§2.2 约束「预算显式传导」、R12；[internal/cli/chat.go:323-348](../../internal/cli/chat.go#L323-L348) 现状实测；[internal/core/run.go:107-113](../../internal/core/run.go#L107-L113) `Budget` 五字段。

## D13 记忆读面：`Sections` + `renderSectionOrEmpty`，不动既有 `renderSection`

- **决定**：`memory.Service.Sections(ctx, scope) (coreText, archiveText string, err error)` 读取后分别经新抽出的 `renderSectionOrEmpty(title, entries)` 渲染——条目为空 → **空串**（`/memory` 的 `core` / `archive` 字段为空串）。既有 `renderSection`（标题恒出、正文可空）保持原样，继续服务 markdown 导出；`coreSectionOf` 改为调用 `renderSectionOrEmpty`，输出逐字不变。
- **理由**：两个调用方要的语义相反——markdown 文件要求标题常在（空分区也要有标题结构），HTTP 视图要求空分区为空串（契约口径）。把「标题恒出」改掉会破坏 US-7 已交付行为；在 web 层裁剪字符串则是在视图层重复解析语义。
- **备选与取舍**：① 直接复用 `renderSection`——空分区输出空标题，违反契约；② web 层对渲染结果做空标题裁剪——字符串级修补，脆弱；③ 另写一套 markdown 解析——重复实现。
- **来源**：颗粒度文档 §3.1 `memory/memory.go`、§3.5 #10（等价重构）、`memory_test.go` +1；[internal/memory/markdown.go:195](../../internal/memory/markdown.go#L195) / [:283](../../internal/memory/markdown.go#L283) 现状实测（两函数语义确实相反）。

## D14 `/info` 与 `/tools` 的数据来源

- **决定**：`/info`：`agents` = `AgentEntry.Registered == true` 的计数（**不是** `len(Profiles())`）；`mcp_servers` = `Options.MCPConnected`（`newWebServer` 传 `len(rt.mcpConns)`）；`providers` 按配置顺序，`models` 只含 `default_model`，`configured` 是布尔，**无** `api_key` / `base_url`。`/tools`：顺序 = `registry.Names()`（已排序），`source` 判定为 builtin 集（`app.declaredToolNames`）→ `builtin`、MCP 连接池 → `mcp`、其余 → `code`。`/profiles`：按装载顺序，未注册项 `registered=false` 且 `problems=[RejectReason]`；`tools` / `skills` / `problems` 必须是非 nil 切片（空则 `[]`，不是 `null`）。
- **理由**：`Profiles()` 返回**每个目录一个条目**（含未注册、含 `Unverified`），用它当 `agents` 会虚高；`Registered` 才是「可运行 Agent」的口径（[internal/core/profile.go:79-85](../../internal/core/profile.go#L79-L85) 实测注释：非 nil `Profile` 不等于已注册）。MCP 连接数必须来自真实连接池（`len(rt.mcpConns)`），不能数配置。`declaredToolNames` 是 US-8 交付的九件内置唯一声明，另写一份名单就会漂移。
- **备选与取舍**：① `len(Profiles())`——虚高；② `len(registry.Names())`——工具数不是 Agent 数；③ 按名称前缀猜 builtin——命名不是契约；④ 空切片留 nil——JSON 输出 `null`，违反视图契约。
- **来源**：颗粒度文档 §3.1 `handlers_meta.go` / `views.go` / `app/web.go`、§2.2 约束 25 / 27、R17；`openapi.yaml` 的 `InfoResponse` / `ToolInfo` / `ProfileInfo`；[internal/core/profile.go:79](../../internal/core/profile.go#L79)；[internal/tool/registry.go:60](../../internal/tool/registry.go#L60)。

## 写前核实清单

写代码前逐项核实；**核实不到的停下报告，不凭记忆写**。

| # | 待核实项 | 核实方式 | 状态 |
| --- | --- | --- | --- |
| 1 | `http.CrossOriginProtection` 的构造 / 判定 / deny handler 语义 | `go doc net/http.CrossOriginProtection` | **已核实**（2026-10-18，本机 Go 1.27.2） |
| 2 | `ServeMux` 的 catch-all 是否压过自动 405 | 仓库外最小 `httptest` 探针：注册 `GET /x` + `/` 兜底，发 `POST /x` | **待实现期核实**（探针用毕即删） |
| 3 | `http.MaxBytesReader` 的超限错误类型 `*http.MaxBytesError` | `go doc net/http.MaxBytesError` + 探针 | **待实现期核实** |
| 4 | `context.WithoutCancel` 可用性与语义 | `go doc context.WithoutCancel` | **待实现期核实**（Go 1.21+ 必有，仍按纪律查） |
| 5 | `json.Decoder` 第二次 `Decode` 在尾随空白 / 多余内容下的返回值 | 探针（`{} x`、`{}` 加尾随空白、`{}{}` 三种输入） | **待实现期核实** |
| 6 | `core.Budget` 五字段与 `cli.chatBudget` 映射口径 | 读 [internal/core/run.go:107](../../internal/core/run.go#L107) / [internal/cli/chat.go:323](../../internal/cli/chat.go#L323) | **已核实**（本轮） |
| 7 | `core.RunStatus` 五值与 `RunResult` 字段 | 读 [internal/core/run.go:49-76](../../internal/core/run.go#L49-L76) | **已核实**（本轮） |
| 8 | `core.AgentEntry.Registered` 语义（非 nil `Profile` ≠ 已注册） | 读 [internal/core/profile.go:75-85](../../internal/core/profile.go#L75-L85) | **已核实**（本轮） |
| 9 | `sessions` / `messages` / `runs` 的列名与可空性 | 读 [docs/contracts/schema.sql:40-96](../../docs/contracts/schema.sql#L40-L96) | **已核实**（本轮） |
| 10 | `memory.parseSections` / `renderSection` / `coreSectionOf` 的现有语义 | 读 [internal/memory/markdown.go:195](../../internal/memory/markdown.go#L195) / [:283](../../internal/memory/markdown.go#L283)、[memory.go:178](../../internal/memory/memory.go#L178) | **已核实**（前序会话，本轮按行号复核） |
| 11 | `storage.SessionStore` 的 `db.read` / `db.write` 约定与 `UpsertSession` 不改 `status` | 读 [internal/storage/session_store.go:116](../../internal/storage/session_store.go#L116) / [:139](../../internal/storage/session_store.go#L139) | **已核实**（前序会话，本轮按行号复核） |
| 12 | `http.Server` 四超时字段名与 `BaseContext` 行为 | `go doc net/http.Server` | **待实现期核实**（字段名标准，仍按纪律查） |
| 13 | `(*http.Server).Shutdown` 返回 `context.DeadlineExceeded` 的条件 | `go doc net/http.Server.Shutdown` | **待实现期核实** |
| 14 | `fs.ErrNotExist` 在 `ArchiveSession` 的 `RowsAffected()==0` 语义下与 `errors.Is(err, fs.ErrNotExist)` 的匹配 | 实现期以 `session_api_test.go` 的红态先行验证 | **待实现期核实** |
| 15 | `crypto/rand` 生成的 `requestID` 长度与日志字段 | 实现期按 `log/slog` 现有字段约定 | **待实现期核实** |

## 未决事项

无。设计面已由 R4 冻结并经人工 review ① 通过；D-01 的两个契约增补（DELETE 409 / POST /sessions 400）已获用户批准并写入 [docs/contracts/openapi.yaml](../../docs/contracts/openapi.yaml)。实现期若出现新的签名漂移或契约冲突，按颗粒度文档「停止边界」停下登记，不静默自决。
