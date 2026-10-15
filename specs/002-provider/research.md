# Phase 0 研究：US-2 Provider 对接（能力一）

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-16

**结论先行**：Technical Context 无 `NEEDS CLARIFICATION`——设计面已冻结（颗粒度文档 [docs/us/us2-provider.md](../../docs/us/us2-provider.md)「待决事项」为空：R1 评审 + R2 复核裁决全部落案，含 SDK 角色收窄与 `base_url` 装载期拒绝两条用户裁决）。本文件的职责是**把「怎么做」的关键决策与「写前必须实测核实」的项逐条固化**，供 implement 阶段逐项消化（module-dev 步骤 6 的 H3 门禁：第三方 API 在本地依赖核实不到 → 软门禁停下）。

## 决策记录

### D1 SDK 角色收窄：协议类型 + 已知字段编解码，HTTP 往返自建

- **Decision**: `github.com/sashabaranov/go-openai` v1.43.0 只提供协议类型与已知字段编解码（请求信封基座 / 消息 / `ToolCall` / `Usage` / 错误体）；HTTP 往返由 `internal/provider` 自建薄传输（`net/http` + `encoding/json`）：**一次** POST、不重试、不 fallback、不跟随重定向（`CheckRedirect` 返回 `http.ErrUseLastResponse`，3xx 按 `external_error` 记审计）。
- **Rationale**: 源码核实（v1.43.0，本地模块缓存）：`ChatCompletionMessage` 的 `MarshalJSON` / `UnmarshalJSON` 只处理已知键、全仓 `ExtraFields` **零命中**、chat 无导出的原始 JSON 通道——技术方案 §4.4 约束 3「供应商扩展字段两端保真」在库的 chat API 上**不可实现**。用户 2026-10-16 已裁决（颗粒度文档 §2.4 注 7）：换的是实现路线、不是承诺本身；「唯一路径」措辞已改为「本 US 选定路线」。
- **Alternatives considered**: 自定义 `RoundTripper` 注入 / 截获未知字段（与库内部编解码耦合——要按 `messages` 下标配对并吞掉库自身的解析与错误语义，复杂度更高、收益相同）；放弃扩展字段透传承诺（直接违背 §4.4 约束 3 与协议往返保真 FR-008）。
- **代价如实登记**: 库的传输层便利（代理环境变量、连接池调参）改由标准库缺省承担——`http.Transport` 同样读 `HTTP_PROXY` / `NO_PROXY`。

### D2 请求信封的构造方式：map 覆盖，不靠字段仲裁

- **Decision**: `buildRequestBody` 先把 SDK 请求类型的已知字段 marshal 成 `map[string]json.RawMessage`（如 `{"model": …}`），再**显式覆盖** `"messages"` 键为 `encodeMessages` 构造的消息数组，最后 marshal 整个 map。
- **Rationale**: 「出站体里 `"messages":` 只出现一次」这件事必须是**构造出来的**——`TestOpenAIClient_RequestEnvelopeKeepsSingleMessagesKey` 直接断言；若靠「外层结构体嵌入 SDK 类型 + 同名字段遮蔽」，正确性就依赖 Go 的字段仲裁规则，且 SDK 升级可能改变行为。
- **Alternatives considered**: 结构体嵌入 + 同名字段遮蔽（依赖仲裁规则）；纯手写 map（丢掉 SDK 的已知字段基座——US-4 扩展 `tools` / `temperature` 时要重建，且协议字段拼写失去单点）。

### D3 审计两处写入共用同一把解耦上下文

- **Decision**: `auditContext(ctx) = context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)`（`auditWriteTimeout` 缺省 5 秒）；**预写与终态回填共用**（`service.go` 内单点定义）。
- **Rationale**: 契约 §4.2 的主语是「审计写入」**整体**，不止终态回填；预写解耦消掉一个歧义——**预写失败必须意味着「审计真的不可用」，而不是「ctx 恰好已取消」**（否则取消会伪装成 fail-closed 拒发）。可直接观察的后果：调用前 ctx 已取消时**仍先落 `pending` 行**，再发出（客户端立刻以 `context.Canceled` 失败），终态 `failed` + `timeout`。
- **Alternatives considered**: 只有终态用解耦上下文（取消被伪装成审计不可用）；把 5 秒提为配置键（契约已给缺省值，本 US 不新增配置面——颗粒度文档 §2.4 注 8）。

### D4 `base_url` 支持面：两层拒绝 + 展示层收窄

- **Decision**: 只支持 scheme（`http` / `https`）+ host + path 三部分并**原样保留**（path 可为空）；含 userinfo / query / fragment 的取值在**装载期拒绝**（`cli.validateConfig` 第 8 类 fail-closed）；缺 scheme **不代补** `https`；`base_url` 缺失提为必填；构造期 `provider.parseBaseURL` 按同一口径**再判一次**；展示层 `redactedBaseURL` 按收窄规则渲染。两层文案都只点名 `providers[i].base_url` 与不支持的成分名，**不回显原 URL、也不回显其中的值**（`url.Parse` 的原始错误自带 URL，**不得**直传）。
- **Rationale**: 用户 2026-10-16 裁决（颗粒度文档 §2.4 注 9）：**不采用**「接受后静默丢弃」——静默删改会**改写用户配置的含义**，把认证 / 路由错误推迟成难定位的运行时故障（query 里可能带着网关路由参数）。构造期再判是因为客户端可被直接构造（测试、后续 US 的装配），不依赖装载层已拦；展示层再收窄是纵深防御（不依赖上一层已拦干净）。
- **Alternatives considered**: 接受后静默裁剪（用户明确不采用）；支持 query（须先显式定义语义，将来再放开）；让 `cli` import `provider` 复用同一段校验（**依赖方向红线**——`ports_test.go` 判卷；两层各一小段实现、同一口径）。
- **将来放开的口径（写在此处防漂移）**: 若需支持 query，**先显式定义支持范围与语义**再放开，不做「白名单之外一律丢弃」的宽松解析。

### D5 失败类别：本 US 三类取值 + 兜底 + 归并口径

- **Decision**: 本 US 产生 `external_error` / `timeout` / `unknown` 三类（七类字面量见契约 §7.3）；`context.Canceled` 与 `DeadlineExceeded` **归并**记 `timeout`，但**错误链区分保留**（`errors.Is` 仍可判具体原因）；`unknown` 是**兜底**（既非客户端产出的安全错误、错误链上也无 ctx 错误的异常错误），**不是恢复路径专属**。
- **Rationale**: 契约 §7.3（失败必须有类别）；用户 2026-10-16 裁决（已决事项 #4：暂归 `timeout`，明示这是分类归并）。恢复扫描（US-4）把孤儿 `pending` 行标 `unknown`，与本条兜底**同值不同因**——要分开这两类时按 `status` 判（`failed` vs `unknown`），不靠 `error_kind` 单独判。
- **Alternatives considered**: 取消类记 `unknown`（与恢复行混淆——恢复行以 `status=unknown` 判定，`error_kind` 再记 `unknown` 则两者彻底不可分）；新增「取消」类别（越出七类固定字面量）。

### D6 `core` 只放声明（零可执行语句）与覆盖率后果

- **Decision**: 类型 / 接口 / 常量 / 哨兵进 `internal/core/provider.go`（**纯声明，零可执行语句**——覆盖率判卷跳过）；一切逻辑（分类函数、协议翻译、URL 处理、SQL）进 `provider` / `storage`。
- **Rationale**: **首先**是职责划分——`core` 是契约层（各包共用的类型 / 接口 / 常量 / 哨兵），且包纪律规定它**不得** import 其他 internal 包与第三方（放得下实现就没有倒置可言）。覆盖率门槛是同一件事的后果：`core` 一旦出现可执行语句即纳入 ≥ 70% 判卷，而「只做转发的 `core` 实现」会**同时**违背依赖方向与判卷口径。本机实测（2026-10-16）：`internal/core` 与 `internal/provider` 当前均记 `[no test files]` / 无可执行语句。
- **Alternatives considered**: 把 `classifyError` 放 `core`（违反包纪律 + 触发 70% 门槛）；为「凑覆盖率」给 `core` 写空转测试（不符合「测试要判真实行为」的纪律）。

### D7 失败原文两处截断（客户端出口 + 落库前）

- **Decision**: 客户端 `sanitizeMessage(text, apiKey)`（抹 key 回显 + 4 KB **UTF-8 安全**截断）在出口前做一次；`storage.LLMCallRepo.FinishCall` 落库前**再截一次**。两处各自独立成立，**不得**只留一处。
- **Rationale**: 契约 §7.4（失败原文截断到 4 KB）+ 颗粒度文档 §2.2 约束 12（截断责任在 `storage`——列的家，单点、可单测、**对任何写入方都成立**）。两处不是冗余：客户端那一次保证「错误文案离开唯一持有 key 的地方之前已安全」；storage 那一次保证「任何写入方都不会把超长文本写进列」。
- **Alternatives considered**: 只留客户端（其它写入方——如 US-4 的调用路径——不受约束）；只留 storage（做不了 key 抹除，且错误文案在离开客户端后已经泄露）。

### D8 `provider list` 轻路径与失败语义（刻意与 `status` 不同）

- **Decision**: 只读配置文件——**不开库、不取实例锁、不做恢复、不构造客户端**；缺配置文件 / `providers` 段为空 → 失败（exit 1）并点名路径 / 段名；配置无效 → 原错误上抛；正常 → 逐条列出 name / 收窄地址 / default_model / 凭证状态文案 + 汇总行，exit 0。
- **Rationale**: 技术方案 §8.7 轻命令口径（不涉及注册表或数据库）；颗粒度文档 §2.4 注 2——与 `status` 的容忍语义**有意的不同**：`status` 是诊断报告，必须在裸工作区出结果；`provider list` 的语义是「列出**已配置**的 Provider」，无配置即命令目的未达成。
- **Alternatives considered**: 沿用 `status` 的容忍语义（空输出 + exit 0，运维无法区分「没配」与「配了但没读到」）。

### D9 测试基础设施两件套：FakeProvider（含同步基元）+ FakeOpenAIServer

- **Decision**: `testutil.FakeProvider`（`FakeStep{Response,Err,Block}` 按脚本回放 / `Requests()` / `CallCount()` / `Started(n)` 返回第 n 次调用**进入时**关闭的信号通道）+ `testutil.FakeOpenAIServer`（`httptest` 回环，按脚本回放 HTTP 状态与响应体、记录收到的请求体）。
- **Rationale**: 编程指南 §8.3 第 2 项（假 Provider 随 US-2 接口落位、后续 US 复用）；取消用例**不得**用 `time.Sleep` 猜时序——取消可能落在预写完成**之前**，用例会走成 fail-closed 拒发那条分支（判的就不是取消路径），且时紧时松；`Started(n)` 自己先被判卷（它若坏掉，表现为用例挂起或时紧时松，属最难查的一类）。
- **Alternatives considered**: 只交付假 Provider（协议面只剩真网络或库内部构造——两跳透传与信封断言无法判）；`time.Sleep` 同步（时序脆弱，且会判错分支）。
- **两跳口径（防"假验收"）**: 仅对 `core.Message` 做一次 JSON 往返**检不出**库丢未知字段——验收必须走「假服务返回未知字段 → 解析进透传位 → **下一次请求体**仍含该字段」。

### D10 真实调用验收的执行形态：双门控 + 独立临时库

- **Decision**: `//go:build integration` + **显式执行开关**（`BASALT_REAL_CALL=1`）+ 凭证环境变量**双门控**（缺一即 `t.Skip` 写明理由，CI 上跳过属预期）；入口 `context.WithTimeout` **显式设截止**（测试常量，不是配置键）；用**独立临时库**（不复用开发库）；不可达端点反例**不需要凭证**（故不因缺凭证跳过——它是 fail-closed 的正面判据）；`t.Skip` **不算通过**，收口报告必须区分 `--- PASS` 与 `--- SKIP`。
- **Rationale**: 用户 2026-10-16 裁决（已决事项 #6 / #7）；契约 §3.3 承诺边界（运行时不会自动重试）；编程指南 §4.2 验收 Demo 口径。
- **Alternatives considered**: 默认层跑真实调用（违背「默认层零真实网络」的分层口径）；用配置键当开关（污染产品配置面——开关只属测试环境变量，**不落配置、不入 `basalt.yaml`**）。

## 写前必须实测核实清单（implement 阶段 H3 门禁）

| # | 待核实项 | 复核手段 | 核实不到时的动作 |
| --- | --- | --- | --- |
| 1 | `ChatCompletionRequest` 的已知字段与 JSON tag（尤其 `stream` 的形态） | 读本地模块缓存 + `go doc`——**本轮已核实**：`chat.go:279` → `Stream bool \`json:"stream"\``（**无 `omitempty`**，缺省序列化出 `"stream": false`） | 软门禁：停下报告（判据已写成「`stream` **不得为 true**」，两种 tag 形态都成立） |
| 2 | 错误信封类型可用（`ErrorResponse` / `APIError`） | 同上——**本轮已核实**：`error.go:35` `type ErrorResponse struct` | 软门禁：停下报告（改为自建错误体解析类型） |
| 3 | 通用扩展字段**不存在**（`ExtraFields`） | 同上——**本轮已核实**：全仓 grep **0 命中** | 软门禁：停下报告（若存在，透传路线可简化——属路线变更，须先报告） |
| 4 | `http.Client.CheckRedirect` 返回 `http.ErrUseLastResponse` 的返回语义（拿到响应、`err` 为 `nil`、body 可读、**未**跟随） | 标准库文档 + 仓库外最小探针（假服务回 302 → 断言状态 302 可读、Location 未被请求）——**本轮已核实**（读数见下） | 软门禁：停下报告 |
| 5 | `url.Parse` 对异常输入的宽容度（成分判定字段：`u.User != nil` / `u.RawQuery != ""` / `u.Fragment != ""`；畸形输入与 `u.Opaque`） | 仓库外最小探针逐例读回——**本轮已核实**（读数见下） | 判据是**两层都拒绝**（装载期 + 构造期）——宽容度只决定要不要补显式校验，不影响判据本身；记录读数 |
| 6 | 驱动 `UPDATE ... WHERE id = ? AND status = 'pending'` 的 `RowsAffected` 可判（终态只写一次） | 实现内单测直接判卷（`TestLLMCallRepo_FinishCallRejectsNonPendingRow`） | 软门禁：停下报告（改判定姿势须同步改颗粒度文档 §3.1） |

> 探针纪律：核实清单 #4 / #5 的探针**一律建在仓库外**、用后即删（项目既有约定）；#1~#3 为读源码，无需探针。

### 探针实测读数（2026-10-16 implement 阶段，T001）

探针建在仓库外临时目录、读数回填后已删除；#1~#3 由本地模块缓存源码读回（`go version` = `go1.27.2`）。

**#1~#3 源码读数（`github.com/sashabaranov/go-openai@v1.43.0`，本地模块缓存）**

| 项 | 位置 | 读数 |
| --- | --- | --- |
| `Stream` 字段 | `chat.go:279` | `Stream` 为 `bool` 字段、JSON tag 是 `stream`——**无 `omitempty`**，零值序列化出 `"stream": false`（判据「`stream` 不得为 true」两种 tag 形态都成立） |
| 错误信封 | `error.go:35` / `error.go:11` | `type ErrorResponse struct` 含单个 `Error *APIError` 字段（tag `error`）；`APIError` 含 `Code any` / `Message string` / `Type string`——自建错误体解析可用 SDK 类型 |
| 通用扩展字段 | 全仓 grep | `ExtraFields` **0 命中**——chat API 无扩展字段通道，透传必须自建（D1 的收窄结论成立） |

**#4 `CheckRedirect` → `http.ErrUseLastResponse`（假服务对 `/redirect` 回 302 + `Location: /target`）**

```text
#4 err=<nil> errIsNil=true status=302 followed=false
#4 bodyRead=0 body="" location="/target"
```

判定：**err 为 `nil`、状态 302 可读、重定向目标未被请求**（`followed=false`，目标 handler 从未进入）——与 D1 的「3xx 按 `external_error` 记审计」直接兼容（拿到响应、不跟随、按非 2xx 归类）。

**#5 `url.Parse` 宽容度（逐例读回）**

| 输入 | 读数 | 判定 |
| --- | --- | --- |
| `https://user-x9:secret-7f@api.example.com/v1` | `user!=nil:true`，解析**无错** | 宽容——成分判定必须靠 `u.User != nil`，解析不报错 |
| `https://api.example.com/v1?route=9f3a` | `rawquery="route=9f3a"`，无错 | 同上，靠 `u.RawQuery != ""` |
| `https://api.example.com/v1#alt-2b` | `fragment="alt-2b"`，无错 | 同上，靠 `u.Fragment != ""` |
| `api.example.com/v1`（缺 scheme） | scheme=""、host=""、path="api.example.com/v1"，**无错** | 宽容——缺 scheme **不报错**，必须显式判 `u.Scheme` |
| `ftp://api.example.com/v1` | scheme="ftp"，无错 | 宽容——非 http(s) 必须显式拒 |
| `http:api.example.com/v1`（opaque 形态） | host=""、opaque="api.example.com/v1" | 宽容——host 为空即可拒（无需单判 `u.Opaque`） |
| `https:/api.example.com` / `http:///v1` | host="" | 同上 |
| `HTTPS://API.EXAMPLE.COM/V1` | scheme="https"（库已小写化） | scheme 比较可安全用等于 |
| `http://[::1]:8080/v1` | host="[::1]:8080" | IPv6 字面量正常 |
| `""` | 全零 URL，**无错** | 空值必须显式拒（与「`base_url` 必填」同判） |
| `://bad` | **err**：`parse "://bad": missing protocol scheme` | 错误文本**自带原 URL**——确证「`url.Parse` 原始错误不得直传」 |
| `http://%zz` | **err**：`parse "http://%zz": invalid URL escape "%zz"` | 同上 |

结论：判定字段（`u.Scheme` / `u.Host` / `u.User` / `u.RawQuery` / `u.Fragment`）逐例可用；**宽容度决定「必须显式补校验」**——`url.Parse` 对缺 scheme、非 http(s)、空值、opaque 全部静默通过，两层拒绝（装载期 + 构造期）均**不得**依赖解析报错；解析失败路径的原始错误带 URL，一律替换为自产文案。

## 环境读数（2026-10-16 本轮实测）

| 项 | 读数 | 判定 |
| --- | --- | --- |
| `go version` | `go1.27.2 windows/amd64` | 与 `toolchain go1.27.2` 一致 |
| `GOPROXY=https://goproxy.cn go list -m -versions github.com/sashabaranov/go-openai` | 末位 `v1.43.0`，退出码 0 | 坐标存在且与固定技术栈句一致——直接落地 |
| 本地模块缓存 | `github.com/sashabaranov/go-openai@v1.43.0` 在位 | H3 门禁可就地核实（无需网络） |
| `go test -cover ./internal/core/ ./internal/provider/` | 两包均 `[no test files]` | 与设计约束（`core` 零语句）一致；`provider` 落地后纳入 ≥ 50% 判卷 |
| 覆盖率判卷（US-1 交付的 `scripts/check-coverage.sh`） | 最近一次全绿读数见颗粒度文档 §6（`internal/storage 76.26%` / `internal/cli 77.06%` / `internal/app 81.13%`；`core` / `provider` 记「无可执行语句」跳过） | 本 US 收口必须**重测**——若 `core` 出现语句而无测试文件，判卷会以 0% 判失败 |
