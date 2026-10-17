# Phase 0 Research: US-5 Shell 与 HTTP 工具

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-17

**背景**：本 feature 的设计面已冻结（颗粒度文档 [docs/us/us5-shell-http-tools.md](../../docs/us/us5-shell-http-tools.md)「待决事项」已清空；评审回合十三项裁决 + 2026-10-17 用户端口终裁全部落案）。Technical Context **无 NEEDS CLARIFICATION**。本文件不重新设计，而是把已冻结的裁决整理为可执行的**决策记录**（Decision / Rationale / Alternatives considered）+ **写前核实清单**，供 `/speckit-tasks` 与 implement 逐步引用。

## D-01：HTTP 客户端构造——不自行跟随重定向、不读环境代理

- **Decision**：新增包内接缝 `newHTTPClient() *http.Client`——`Transport.Proxy` **恒 `nil`**、`CheckRedirect` 恒返回 `http.ErrUseLastResponse`；两件 HTTP 工具经 `httpDoer` 接口（包内接缝 `newHTTPTool(policy, doer)`）持有它。
- **Rationale**：逐跳判定（FR-009）要求**每一跳在发送之前**过 `policy.Check`——客户端若自行跟随重定向，越界的那一跳就没人拦。`ErrUseLastResponse` 的语义是**返回最近响应本体 + nil error**（本轮 `go doc net/http.ErrUseLastResponse` 实测：*"the next request is not sent and the most recent response is returned with its body **unclosed**"*）；`Proxy` 为 nil 的语义是**不走代理**（本轮 `go doc net/http.Transport.Proxy` 实测：*"If Proxy is nil or returns a nil *URL, no proxy is used"*）。
- **Alternatives considered**：
  - *默认客户端自动跟随*：拒——判定点无法插入重定向链；`HTTP_PROXY`/`HTTPS_PROXY` 一设就换出口，域名档形同虚设（FR-017）。
  - *`CheckRedirect` 返回普通 error*：拒——`go doc` 实测其语义是"returns **both** the previous Response (with its Body **closed**) **and** CheckRedirect's error (wrapped in a url.Error)"；错误包装会把状态码与 `Location` 头挡在工具之外，反而判不了 FR-011 的跳转语义。
  - *只用 `http.Transport{Proxy: http.ProxyFromEnvironment}`*：拒——同环境代理问题。
- **落地注意（实测新增）**：因 `ErrUseLastResponse` 返回的 body **unclosed**，逐跳循环**每一跳都必须 `defer` / 显式 `Close` 响应体**（含被判定拒绝的那一跳不会发生——拒绝先于发送，不存在残留）。

## D-02：逐跳判定唯一处——工具不预判、策略不信任"已检查"标记

- **Decision**：scheme / 主机 / 端口 / 域名白名单**只在** `WhitelistPolicy.checkHTTPRequest` 一处判；工具在**每次发送前**调 `policy.Check(ctx, Action{Type: ActionHTTPRequest, Target: 本跳 URL, …})`，不做任何本地预判、不改写目标串。
- **Rationale**：单一判定点是「判定与执行分离」（宪法 VI）的直接推论；两处判必漂移（例：工具放行 `:80`、策略拦 `:8080`，谁对？）。
- **Alternatives considered**：*工具本地快速预判（省一次策略调用）*：拒——第二处判定即第二份事实，安全面不允许。

## D-03：端口策略——`defaultPortFor(scheme) (int, bool)` + 两条固定指引常量

- **Decision**：`http` + 省略端口或 `:80`、`https` + 省略端口或 `:443` 放行；交叉档（`http:443` / `https:80`）与非默认档（`:8080` / `:8443`…）一律拒、归 `sandbox_denied`；指引固定为「当前版本不支持非默认端口」（常量 `guidanceNonDefaultPort`），**不指向**域名白名单（`ruleNonDefaultPort` 点明白名单管 host、不管端口）；初始请求与每一次重定向**同判**、拒绝**先于发送**。
- **Rationale**：2026-10-17 用户终裁（冻结）。端口是 DNS 之外的第二条出网面——只按 host 放行会让白名单域名的任意端口（含内网侧端口扫描面）可达。交叉档拒是因为"http 走 443"与"https 走 80"都不是默认组合、无法用"协议默认端口"一句话解释。
- **Alternatives considered**：*按 `url.Port()` 为空即放行*：拒——`http://host:443` 的 `Port()` 非空但仍是"非本 scheme 默认"，会放行交叉档；*放行通道（新配置键）*：拒——归扩展阶段，本模块如实登记「本地 8080 类服务不可达」。
- **回归点名**：R8——白名单域名重定向到非默认端口 → **目标请求零发送**（两侧记账：发了几次 + 判了几次判的哪个 target）。

## D-04：失败分类的承载——`core` 新增两个平级错误类型，判类型不比文本

- **Decision**：`core` 新增 `ExternalError{Target, Reason, Cause}`（`Error()` 只出**自产安全摘要** + `Unwrap`）与 `InvalidInputError{Tool, Reason}`，配 `IsExternalError` / `IsInvalidInput`（`errors.As` 形态）；`classifyToolError` 判序：**沙箱 → 入参非法 → 取消/截止 → 外部系统 → 文件不存在 → 兜底**。
- **Rationale**：`core` 只依赖标准库（宪法架构约束、机器判据 `go list`）——**不得**复用 `provider.CallError`（方向反了：provider → core 才是合法方向）。判序里 **ctx 判定必须先于外部系统分支**：HTTP 工具把 DNS / 连接 / TLS / 读取失败包装成 `ExternalError` 时**先查 `ctx.Err()`**，取消 / 截止原样上抛——否则超时会被 `external_error` 截胡（契约 §7.3：`timeout` 是独立一档）。
- **Alternatives considered**：*复用 `provider.CallError`*：拒——反向依赖；*统一一个 `ToolError` 带类别字段*：拒——七值里的每一档若都走同一类型，`errors.As` 判据退化为字段比对，分类正确性失去编译期帮助；*哨兵错误 + `errors.Is`*：拒——类别需携带 Target/Reason 上下文，哨兵承载不了；且伪代码（流程 C）已按两类型冻结。
- **文案纪律**：`ExternalError` 的文案**只出自产类别 + host**，**回显 `url.Error` 原文**（其原文含整条 URL——query 可能带凭证，FR-016 / FR-020）。

## D-05：既有三件文件工具的解码错误同批换载体——文案不变

- **Decision**：`tool/filetools.go` 的 `decodeFileToolInput` / 取值校验错误由原载体改为 `*core.InvalidInputError`——**文案逐字不变**、只换类型；执行器入口判「合法 JSON 对象 + 必填键」维持既有路径。
- **Rationale**：FR-019 —— 入参非法的分层（执行器入口 / 工具解码点）两处产出**同一类别**，否则同一类错误会出现「有的进 invalid_input、有的进 runtime_error」的分类漂移。文案不变是为了既有测试断言（fixture 文本）零改动——载体变化不应连带测试改写。
- **Alternatives considered**：*只对新工具用新载体、旧工具不动*：拒——同一类别两种产出方，分类面撕裂。

## D-06：`shell` 执行——argv 直执、经 `core.CommandRunner`，包内零 `os/exec`

- **Decision**：`ShellTool` 持 `core.Policy` + `core.CommandRunner`；`executable` **整串**（不拆分）做 `Action.Target` 判命令档，`arguments` 逐项原样进 `core.Command`；`command(env, in)` 填 `Dir: env.AgentDir`（目录存在才填）、`Env: nil`、`MaxOutputBytes: env.MaxOutputBytes`；不经任何 shell。
- **Rationale**：「可执行名整串逐字比对、工具不得本地拆分 executable」是 FR-001 的**反绕过**设计——拆了 `executable` 会把 `bash -c "…"` 从"命令档硬拒"变成"参数面"，即标准绕过姿势。执行必须走白名单执行器（`LocalRunner`）——那里有环境白名单、整树强杀、读取限流三条保证；包内直接 `exec.CommandContext` 就是第二套执行路径（宪法 VI、包纪律）。
- **Alternatives considered**：*在 `tool` 包内直接 `exec.CommandContext`*：拒——绕开策略/执行分离；*工具内部 `sh -c` 组装命令串*：拒——shell 注入面。
- **编译期判据**：`shelltools.go` 只 import `core` 与标准库；收尾 grep 无 `exec.CommandContext`、无 `WhitelistPolicy` / `LocalRunner` 类型断言。

## D-07：预算下传——`Command.MaxOutputBytes` 字段 + 读取侧取更小者并夹 256 KiB

- **Decision**：`core.Command` 增 `MaxOutputBytes int`（`<=0` = 用装配值；只许更短）；`LocalRunner.effectiveMaxOutputBytes(cmd)` = `min(装配值, cmd 正值, 256 KB)`，两者都非正 → 256 KB 缺省；装配接线 `MaxOutputBytes ← cfg.Budget.MaxOutputBytes`（既有运维配置键，int64→int 照 `cli/chat.go:340-345` 先例）。
- **Rationale**：FR-023——逐运行更小的读取上限要"**真正接到执行侧读取端**"；US-4 只交付了旋钮与逐 Run 生效上限的 `min` 语义，`LocalRunner` 零生产调用点。取值走**字段**而非 context（宪法 VII：context 只承载取消 / 截止 / 追踪）。夹 256 KB 是硬上限——配置写大了也不放行。
- **Alternatives considered**：*经 `context.WithValue` 下传*：拒——宪法 VII；*包级变量*：拒——模拟 ThreadLocal；*改 RunnerOptions 语义（让逐 Run 值可放大）*：拒——"只许更短"是 FR-003 同族的收窄原则。
- **落地注意（本轮实测）**：`runner.go:78-81` 现 clamp **只兜非正值、不夹上限**（§3.5 第 10 项待补的收窄 + 夹取属实）；`runner.go:56` 注释键名 `shell.max_output_bytes` **在配置面不存在**（注 3 登记）——随本项改动该文件时按「注释是事实登记面」对齐为 `budget.max_output_bytes`。

## D-08：`tool list`——端口在消费方、适配器在装配层、重路径

- **Decision**：`cli` 声明 `ToolLister interface { Tools() []core.ToolSpec }`；`Deps` 增 `Tools ToolLister`；`app.toolListerPort{registry}` 转调 `Specs(Names())` 保序；`heavyCommands` 增 `"tool"`；`cli/ports_test.go` 禁 import 清单**增** `internal/tool`。
- **Rationale**：接口定义在消费方（包纪律 2）——`cli` 不得 import `internal/tool`；顺序稳定由实现侧给出、命令面不得重排（FR-021）；命令属**重路径**（答案来自装配期构建的内存注册表，「只读」= 清单读取本身不修改状态，**不是**"整命令零日志"）。
- **Alternatives considered**：*cli 直接 import tool 拿注册表*：拒——依赖方向；*命令面自行排序*：拒——FR-021 明禁。

## D-09：出站请求体上限 256 KiB——新增裁决、零发送、归 `invalid_input`

- **Decision**：`http_post` 的 body 按 UTF-8 字节计上限 256 KiB（`maxRequestBodyBytes = 256*1024`），超限 → `invalid_input`、**零发送**。
- **Rationale**：沿用契约 §6 的 256 KB 预算量级作为出站对称上限；如实登记为**本模块新增裁决**、不冒称契约已有。归 `invalid_input` 而非沙箱：这是"入参本身超界"，不是"越权"。
- **Alternatives considered**：*不设上限（交给超时兜住）*：拒——大 body 在慢链路上占满超时窗口；*归 sandbox_denied*：拒——语义错位（没有规则被违反，是参数太大）。

## D-10：测试造错手段——假 doer 为主 + 少量 httptest 真链路

- **Decision**：`tool/httptools_test.go` 以**进程内假 doer**（拼 `*http.Response`，零 socket）为主判逐跳语义与记账；**真链路**（`httptest`）只保留三条假 doer 判不了的：跳转真由工具驱动（R7）、取消传播（R4，等「请求已发出」信号再取消）、`Proxy=nil` 不读代理变量（R7 同案）。默认层允许本地回环（工程规范 §2）。
- **Rationale**：DNS / TLS / 读失败等分类判别用假 doer 可精确构造（真链路造不出稳定的 TLS 失败）；但"不跟随重定向"与"不读代理"是**客户端构造**行为——必须由真 `net/http` 客户端验证，否则测的是假 doer 自己。
- **Alternatives considered**：*全假 doer*：拒——R7 名点真链路，客户端构造正确性无人判；*全 httptest*：拒——失败分类难构造、端口绑定慢、`Proxy` 读取要摆环境变量。
- **纪律**：取消用例**等信号再取消 + 限时返回**（不许 `time.Sleep` 赌时序）；共享夹具带锁保证 `-race` 干净；链路截断用例经**真实 `ToolExecutor`**（不造第二执行器）。

## D-11：响应读取多读 1 字节——让执行器截断判据必定命中

- **Decision**：HTTP 工具读响应体上限 = `env.MaxOutputBytes + 1` 字节；shell 的输出限流同族（上游限流点追加截断标记、执行器按最终预算必要时再截一次——FR-007）。
- **Rationale**：执行器按 `len(content) > maxBytes` 判截断并加标记；读取端若恰读到 `maxBytes` 就停，`len` 与上限相等 → 判据**永不命中**、标记永不出现。多读 1 字节让"恰好超限"与"恰好等于上限"两种内容在判据上可区分（契约 §7.4：256 KB 截断并标记）。
- **Alternatives considered**：*读满上限后由工具自己判是否还有余量*：拒——等于工具自造第二套截断判定，与 FR-007「截断分工」冲突（会出现两个标记）。

## D-12：`classifyToolError` 的 ctx 先行——`timeout` 不被外部系统截胡

- **Decision**：判序固定「沙箱 → 入参非法 → 取消/截止 → 外部系统 → 文件不存在 → 兜底」；HTTP 工具在**包装**外部错误前先查 `ctx.Err()`——取消 / 截止**原样上抛**（不包装）。
- **Rationale**：`http.Client.Do` 在 ctx 取消时返回的 `*url.Error` 同时满足"是外部错误"与"源于截止"；若先判外部错误，`timeout` 档永远拿不到。FR-016 / 契约 §7.3 双双点名 `timeout` 为独立一档（MUST NOT 被截胡）。
- **Alternatives considered**：*靠 `errors.Is(err, context.DeadlineExceeded)` 事后区分*：不够——`url.Error` 会包装它，且工具层已丢失原始包装链的可能性高；先查 `ctx.Err()` 是最稳的（伪代码流程 C 已按此冻结）。
- **回归点名**：R4（取消传播，真链路 + 等信号 + 限时）。

## 写前核实清单（implement 期 H3）

| # | 待核实项 | 核实方式 | 状态 |
| --- | --- | --- | --- |
| 1 | `http.ErrUseLastResponse` 语义（返回最近响应 + nil error） | `go doc net/http.ErrUseLastResponse` | ✅ 2026-10-17 本轮实测（见 D-01） |
| 2 | `Transport.Proxy` 为 nil = 不走代理 | `go doc net/http.Transport.Proxy` | ✅ 同日实测（见 D-01） |
| 3 | `CheckRedirect` 返回普通 error 的语义（包 url.Error、body 被关） | `go doc net/http.Client.CheckRedirect` | ✅ 同日实测（拒用为构造方案的依据） |
| 4 | `httptest.NewServer` / `httptest.NewTLSServer` 可用性与 `Close` 约定 | `go doc net/http/httptest` | 待 implement 首日核实（标准库、零坐标） |
| 5 | `net/url` 的 `URL.Port()` / `Hostname()` 解析行为（省略端口 / 显式默认端口 / 交叉档） | 本地写一次性 `url.Parse` 打印（不改仓库文件） | 待 implement（D-03 的函数实现前） |
| 6 | Go 1.27 内建 `min`（用于 `effectiveMaxOutputBytes`） | `go doc builtin.min` | 待 implement（若不可用退回两两比较） |

**本 feature 零第三方 API**（唯一依赖面是标准库 `net/http` / `net/url` / `io` / `encoding/json` / `time`），H3「第三方 API 先在本地依赖核实方法存在」的适用面因此只落在上表 stdlib 条目（已按 `go doc` 逐条核）。

## 与本 feature 相关但**不**在 research 范围（已冻结，见颗粒度文档）

- FR-011 五种重定向状态码的逐码语义（301/302/303 转 GET 清正文头；307/308 保方法保 body；304 普通返回）——不设替代方案，照契约落地；
- FR-013 跨跳头同源规则与 FR-012 跳数上限 5（超限归 `sandbox_denied` 不归 `external_error`）；
- FR-005 环境白名单缺省（`PATH` / `HOME` / `LANG` / `TZ`）与 `Env: nil` 语义；
- FR-006 非零退出码不是错误（首行「退出码：N」+ 原样输出）。
