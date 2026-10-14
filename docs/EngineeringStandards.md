# 工程规范（Basalt）

本文件是 Basalt 的工程规范：**Go 编程 / 测试 / 数据库 / 日志与错误 / 提交与分支**五块的日常操作口径，适用对象是每一行进入本仓库的提交（人写的与 AI 写的同一标准）。

**裁决口径**（与 CLAUDE.md 同一份口径）：

- 文档冲突按三层裁决（与 CLAUDE.md 同一口径）：**原则**以[宪法](../.specify/memory/constitution.md)为准；**实现设计**以 [TechnicalSolution.md](TechnicalSolution.md) 为准；**固定字面量**（表结构 / 端点 / 错误码 / 状态字面量）以 [contracts/](contracts/) 为准——本规范**不复制**任何字面量，只写"怎么做"
- 能由门禁判的一律以 `make check` 的当场输出为准（Makefile 由 US-1 交付）；本规范管机器管不到的部分。反向同理：**门禁实现必须与本节条款一致**——某条规范未被门禁覆盖（漏检）时**必须修门禁**（补检查或补钩子），**不得**把"这次绿灯"当成该条已满足
- 条款来源随文标注（技术方案 / contracts / 编程指南 / 起步手册引用、NivroOS 语言无关条款移植）；**§1 以 Go Code Review Comments（CRC）为具名基线**——条款标注 CRC 条目名，机器可判子集由 staticcheck 判卷；**2026-10-15 新定的条款**在节点内标「新增」

## 1. Go 编程规范

本章 Go 语言级条款以 **Go Code Review Comments**（CRC，<https://go.dev/wiki/CodeReviewComments>，2026-10-15 核实仍在维护，31 个条目）为具名基线：条款尾部标注对应 CRC 条目名；CRC 未覆盖处的本仓细则单独标注。个别深层分叉参照 Google Go Style Guide 并注明（CRC 页面自身指向它）。能由工具有效判卷的映射见 §1.1。

### 1.1 格式与静态检查（机器强制，无裁量）

- 格式唯一裁判是 `gofumpt`（gofmt 严格超集）：修复只许 `gofumpt -w <本轮改动的文件>`，**不得**手改格式；全仓检查 `gofumpt -l .` **只读**——**不得**顺手重排无关文件（唯一例外：`/init-foundation` 的全仓初始格式化）
- 判卷顺序先查退出码、再判输出（格式差异时退出码为 0 且输出非空）——陷阱表 §7 第 1 条，[AiProgrammingGuide.md](AiProgrammingGuide.md) §7
- `go vet` / `staticcheck` / `gosec` / `govulncheck` 必须零命中；`//nolint` / `#nosec` 抑制**默认不得使用**——确有理由须逐条写明、经用户确认、进验收报告（门禁豁免纪律，编程指南 §8.1）
- staticcheck 判定集**显式指定**（2026-10-15 定）：`staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...`——`inherit` 取 v0.8.2 默认集（已含 ST1005 错误字符串 / ST1006 接收者命名 / ST1008 / ST1012 等），追加子集把 §1.2 命名与 §1.5 注释的若干条款变成门禁判卷（US-1 的 Makefile `lint` 目标落地）。本机探针实测：`userId` → `userID` 只在追加 ST1003 后命中（默认集不含 ST1003）。两种配置形式——`staticcheck.conf` 文件与命令行旗标——官方等价、**都有效**；**本仓统一取命令行旗标**（Makefile 一处钉死，AI 与 CI 同一入口）。⚠ 探针实测更正（2026-10-15）：源码位于用户缓存目录范围（`%TEMP%` / `AppData\Local\Temp`）时，staticcheck **跳过配置文件定位**——在该类位置探针会得到配置"不生效"的假象（本仓曾据此误判并写进文档，已更正）；论证工具行为**必须**在缓存目录之外建探针
- `-race` 全绿在门禁内，与 `CGO_ENABLED=0` 发布构建**分属不同命令 / job**（陷阱表 §7 第 2 条）

### 1.2 命名

- 包名全小写、无下划线、不用复数；**不得**用 `util` / `common` / `misc` 类空泛名，也**不得**与标准库重名（CRC: Package Names；ST1003）
- 标识符一律 mixedCaps（导出与非导出同规）——未导出常量写 `maxLength`，**不得** `MaxLength` / `MAX_LENGTH`（CRC: Mixed Caps）
- 初略词整体同案大小写：`HTTPServer` / `urlPath` / `appID` / `ServeHTTP`——**不得** `Url` / `appId` / `ServeHttp`（CRC: Initialisms；ST1003）
- 变量名长短与作用域成比例：局部短名（`i` / `c`），全局与远距使用更描述性（CRC: Variable Names）
- 方法接收者 1–2 个小写字母、**不得** `this` / `self` / `me`，同一类型的方法间保持一致（CRC: Receiver Names；ST1006 默认判、ST1016 开启判）
- 不用 stutter：**重复**包名的标识符不用——`chubby.ChubbyFile` 应为 `chubby.File`；本仓反例是 `core.CoreService`（应叫 `core.Service`）——`core.Service` 本身**不属** stutter；若嫌 `Service` 语义太泛，按其职责另取名（如 `AgentService`），**不得**靠 `core.` 前缀补语义（CRC: Package Names）
- **已定字面量逐字保真**：类型名 / 方法签名 / 配置键 / 表列名 / 端点路径以技术方案与 contracts 为准，**不得**就地改写、自造缩写或同义命名。症状：实现与文档 / 契约两套叫法，契约解析单测与一致性核对对不上；复核：契约 YAML 解析单测（陷阱表 §7 第 32 条）+ 收口时的字面量核对
- 测试函数 `TestXxx_yyy`（英文）；文档 harness 的中文验收点以注释保留（本仓）

### 1.3 包与依赖方向（技术方案 §10.1 三条包纪律）

1. **依赖方向**：`channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`app` 是唯一允许"什么都知道"的装配层；`core` **不得** import 其他 internal 包（只依赖标准库）
2. **接口定义在消费方**：`core` 声明它需要的接口（如 `TaskStore` / `MemoryStore` / `Policy`），实现放各自的包（依赖倒置）
3. **第三方实现放消费方包内或 `internal/adapters/`**：新增一个第三方实现**不得**迫使其他包跟着改；容纳不下时允许按需新增包，前提是三条纪律仍成立

- 无环由 CI 用 `go list` 复核（技术方案 §10.2）
- import 分三组、空行分隔：标准库 / 第三方 / 本仓（CRC: Imports）；重命名导入只用于消歧；**不得** `import .`（CRC: Import Dot；ST1001 默认判）；仅副作用导入（`import _`）只允许出现在入口包与测试（CRC: Import Blank）
- **新增第三方坐标或版本变更**：先核实（`GOPROXY=https://goproxy.cn go list -m -versions <module>`）再问用户，**不得**先引入后解释；技术栈表与已批准计划内的坐标直接落地（宪法第 8 条、CLAUDE.md 依赖纪律）

### 1.4 错误处理

- **异常不吞**：每个 `err` 必须被处理（上抛 / 转审计落库 / 转日志），**不得** `_ =` 静默丢弃（CRC: Handle Errors）。症状：失败路径既无审计也无日志，排障黑洞；复核：grep `_ =` 与 `_, _ =` 命中处逐处确认
- 错误字符串不大写、不以标点或换行结尾——`fmt.Errorf("something bad")` 而非 `fmt.Errorf("Something bad.")`（CRC: Error Strings；ST1005 默认判）
- 错误分支先处理、正常路径保持最低缩进——**不得** `if err != nil { … } else { … }` 式组织（CRC: Indent Error Flow）
- 失败**必须**经最后一个返回值表达（`error` 或 `ok` 布尔），**不得**用 `-1` / `""` / `nil` 哨兵值；错误变量命名随导出性——非导出 `errFoo`、导出（属 API 一部分）**必须** `ErrFoo`（如 `ErrNotFound`），**不得**一律小写（CRC: In-Band Errors；ST1008、ST1012 默认判）
- 包装用 `fmt.Errorf("读取 Agent 配置 %s: %w", name, err)` 保留错误链；判定用 `errors.Is` / `errors.As`，**不得**比对错误文本字符串（本仓细则）
- 对外错误语义与文案以 [contracts/run-and-errors.md](contracts/run-and-errors.md) 为准（`error_kind` 分类、错误码表 §8）；错误文案点名"要改什么"，**不得**回显凭证、绝对路径、堆栈（技术方案 §7.2）
- `panic` 只用于不可恢复的编程错误（CRC: Don't Panic）；库代码对可预期错误**不得** `panic`；HTTP 侧由 `recover` 中间件兜底（panic → 500 信封 + ERROR 日志，技术方案 §7.2）

### 1.5 注释与文档

- 注释与文档用中文；关键注释（包级 / 导出符号级 / 复杂逻辑段）**中英并列、中文在前**（本仓）
- 注释为完整句子、以句号收尾；导出符号的 doc comment 以符号名开头（CRC: Comment Sentences / Doc Comments；ST1020–ST1022 开启判）
- 所有顶层导出名**必须**有 doc comment，非平凡的未导出类型 / 函数同样；注释写"为什么"与约束（决策背景、易踩的坑），不逐行复述"是什么"（CRC: Doc Comments）
- 包注释紧邻 `package` 子句（中间无空行）并放 `doc.go`；`package main` 可用 `// Command seedgen …` 式变体（CRC: Package Comments；ST1000 开启判）

### 1.6 并发与时间

- **同步执行模型**（宪法第 7 条；CRC: Synchronous Functions）：**不得**引入异步编排 / 后台任务框架；启出的 goroutine 必须能说清"何时退出"，说不清就不起（CRC: Goroutine Lifetimes）
- `context.Context` **必须**是函数的第一个参数（`func F(ctx context.Context, …)`）；**不得**把 context 存进结构体字段——签名受接口（标准库 / 第三方）强制的除外（CRC: Contexts）
- `context` 只承载取消 / 截止 / 追踪标识（trace id）；Profile / Session 等业务状态**必须**显式类型化传递（`RunRequest` / `RunEnv`）；**不得**用包级变量模拟 ThreadLocal。复核：grep `context.WithValue` 命中处逐处确认为追踪标识（陷阱表 §7 第 24 条）
- **时区纪律**：库内时间一律 UTC（Unix 毫秒，[schema.sql](contracts/schema.sql) 迁移纪律 4）；代码**不得**依赖进程本地时区（禁 `time.Local` 做隐式换算），需要地方时区处按显式配置的 IANA 名（`zone` 字段）换算；时区数据库经 `time/tzdata` 内嵌（US-1）。症状：开发机 CST、容器 UTC，展示 / 定时触发差 8 小时；复核：grep `time.Local` 零命中（本仓已有纪律——`/module-dev` 收尾全局不变量第 7 条）。来源：本仓全局不变量 + NivroOS 同源条款（zone 默认 UTC、禁 `ZoneId.systemDefault()`）+ 时区内嵌口径（编程指南 §3.2）

### 1.7 依赖与版本

- 依赖坐标与版本以 `go.mod` / `go.sum` 为唯一事实源；`go` 行是**最低版本要求**，准确构建版本由 `toolchain` 指令与 CI 固定——**不得**写成"`go.mod` 锁定 x.y.z"（CLAUDE.md 版本语义）
- **不得**引入 Agent 框架坐标（宪法第 1 条）；LLM 客户端**不得**启用任何自动工具循环（宪法第 2 条）；工具调度唯一归属 `ToolExecutor`
- 发布产物 `CGO_ENABLED=0`（宪法第 8 条）；四平台交叉编译由 CI `build` job 承载（技术方案 §10.2）

### 1.8 其余 CRC 适用条款（汇编）

- **密钥 / 令牌 / 随机标识必须用 `crypto/rand`**，**不得**用 `math/rand`（含"随手生成"的密钥）——安全红线（CRC: Crypto Rand；复核：gosec G404）
- 值拷贝只按需复制：带指针方法或含同步原语（`sync.Mutex` 等）的类型**不得**按值拷贝（CRC: Copying）；仅因省字节把 `string` / 接口值传指针——**不得**（CRC: Pass Values）
- 接收者类型：拿不准用指针；含互斥字段**必须**指针接收者；同一类型方法的接收者类型**不得**混用（CRC: Receiver Type）
- 空切片声明用 `var t []string`，不用 `t := []string{}`——例外：JSON 编码等需要 `[]` 而非 `null` 处（CRC: Declaring Empty Slices）
- 具名返回值只在"同型多返回值提升可读性"或 deferred 闭包需要时使用；裸 `return` 仅限几行的小函数（CRC: Named Result Parameters / Naked Returns）
- 无行宽硬限制（gofumpt 不设），但**不得**为凑宽度粗暴折行——先改名字与语义（CRC: Line Length）

### 1.9 资源释放责任（新增，2026-10-15 定）

谁申请谁释放；每条获取资源的路径都要说得出"在哪里释放"：

- **HTTP 响应体**：`resp.Body` 由发起方释放——先判 `err` 再 `defer resp.Body.Close()`（错误路径同样要关，防 nil 解引用）。症状：连接不归还、句柄泄漏，压测下 `too many open files`；复核：grep `client.Do` / `http.Get` 命中处逐个确认 Body 有闭合路径
- **`sql.Rows`**：必须 `Close()`——"读完自然关闭"是假象（中途 `break` / 报错返回不会走到）；`defer rows.Close()` 与循环后的 `rows.Err()` 检查成对出现（§3）
- **事务**：`sql.Tx` 每条路径以 `Commit` 或 `Rollback` 收口——惯用式 `defer tx.Rollback()`（提交后 Rollback 返回 `ErrTxDone` 属正常）；**不得**有既不提交也不回滚的路径——写池 1 下泄漏事务 = 全库写阻塞（§3 迁移纪律第 5 条）
- **取消 / 超时函数**：`context.WithCancel` / `WithTimeout` / `WithDeadline` 的 `cancel` **必须**调用（`defer` 立即挂上，即使函数正常走完）——否则整个派生树悬挂到父级取消为止。复核：grep `WithCancel` / `WithTimeout` 命中处逐个确认有 `defer cancel`
- **循环里不开 `defer`**：长循环内 `defer` 累积到函数返回才执行——循环体资源改显式释放或抽小函数（本仓细则）

## 2. 测试规范

- 框架：标准库 `testing` + `github.com/stretchr/testify` v1.12.1（技术方案 §1.2）；多组用例默认表驱动（Go 惯例）
- 命名：`TestXxx_yyy` 英文；`t.Run` 子测试可用中文描述（文档 harness 的中文验收点以注释保留）
- **测试纪律**（编程指南 §8.2 第 1 条）：**不得**删断言、**不得**跳过测试（`t.Skip` 必须写明理由）、**不得**放宽阈值让测试变绿——实现错修实现；认为测试错 → 停下报告，**不得**自行改判
- **测试隔离纪律**（新增，2026-10-15 定）：测试不污染工作区、不依赖外部环境、不靠时序侥幸——配套设施（临时工作区 / 本地 HTTP 测试服务）见编程指南 §8.3 第 2 项：
  - 临时目录一律 `t.TempDir()`（自动清理）——**不得**往仓库工作区写测试产物；"固定工作区"类用例（如 `basalt init` 幂等）也建在 `t.TempDir()` 之下
  - 数据库**每个测试独立**：`t.TempDir()` 内的临时 SQLite 文件——**不得**共享样例库互相踩状态；`t.Parallel` 的用例各自持独立库
  - 外部依赖用**桩**：HTTP 用 `net/http/httptest`、假服务监听 `127.0.0.1:0`（端口由内核分配，避免冲突）——单测**不得**访问真实外网（"是否访问真实网络"是测试分层标准，写进颗粒度文档 §4）
  - 同步**不得**用固定 `sleep` 赌时序——用通道 / `sync.WaitGroup` / "轮询 + 截止时间"；确需轮询时到点即失败，不无限等
- 分层与命令：
  - 单测默认执行：`go test ./...`
  - 集成测试带 `//go:build integration`：**必须**单列命令 `go test -tags=integration ./...`（陷阱表 §7 第 3 条：默认命令不跑带 tag 的测试）
  - `-race` 需 CGO 开启，与发布构建分属不同命令 / job（陷阱表 §7 第 2 条）
- **覆盖率**（可执行判据，`make cover` 按包判失败）：`core` / `tool`（沙箱）/ `memory` / `storage` ≥ 70%，其余 ≥ 50%；有可执行语句但无测试文件的包计 0%；`[no statements]` 跳过（CLAUDE.md 覆盖率底线）
- 覆盖率是底线不是替代：安全关键路径必须有专项用例——沙箱三档拒绝（`error_kind=sandbox_denied`）、凭证不回显 / 不落日志、审计 fail-closed（预写失败即拒绝发出）
- 失败信息写清实际值与期望值并附输入，顺序 actual 在前（`Foo(%q) = %d; want %d`）——排障的人可能不是你（CRC: Useful Test Failures）
- 新包**必须**配可运行的 `Example`（或演示完整调用序列的测试）（CRC: Examples）
- 契约单测：`docs/contracts/*.yaml` 用项目已有 YAML 依赖（`goccy/go-yaml`）解析，隐性错误当场红（陷阱表 §7 第 32 条）；生成物必须有"下游解析器读回"的断言
- 颗粒度文档 harness 写出的测试：**用例 / 输入 / 预期 / 关键断言必须保留**——允许格式化改动与不改变验收含义的接口适配（对齐实际接口落位）；改变验收语义即"变更"（停下确认），**不得**借"适配"删断言或弱化预期

## 3. 数据库规范

- **字面量唯一源**：[contracts/schema.sql](contracts/schema.sql)——表 / 列 / 状态字面量以它为准（其头部已声明契约地位）；本规范**不另立第二份定义**，技术方案 §9 与它冲突时以它为准
- **迁移纪律**（schema.sql 头部第 1–7 条，**必须**逐条遵守；此处不复制原文，只逐条点名）：
  1. 建表一律 `IF NOT EXISTS`（幂等）；结构变更 = 新增受控前向 SQL + 递增 `schema_version`；**禁止** ORM 自动 DDL（宪法第 9 条）
  2. 新旧库以 `schema_version` 表是否存在判定；库版本高于二进制支持 → **拒绝启动**
  3. 已发布迁移**只追加、不得改写**
  4. 时间存 INTEGER（Unix 毫秒，UTC）；布尔存 INTEGER（0/1）
  5. 连接池：写连接 max 1（SQLite 单写者），读连接若干
  6. PRAGMA **逐物理连接**设置（modernc 经 DSN `?_pragma=...`）；`journal_mode=WAL` 是库级属性
  7. 事务**不得**跨 LLM 调用与工具执行
- 手写 SQL（宪法第 9 条），**不得**引入 ORM；查询一律参数化占位符，**不得**字符串拼 SQL
- 新增对象命名（新表 / 新列必须走 schema.sql 迁移，命名沿用现状风格）：表名复数 snake_case；时间列 `<动作>_at` 风格（`created_at` / `last_active_at`）；列名 snake_case
- SQL 集中在 `internal/storage`；**审计写入 SQL 只允许出现在 storage**，其余包只经接口（`/module-dev` 收尾全局不变量第 3 条）。复核：grep `INSERT INTO` / `UPDATE` 的命中文件
- **审计两阶段写**（宪法第 5 条，fail-closed）：先写 `pending` 再发调用，返回后更新终态；**失败分支按契约三分**（[run-and-errors.md](contracts/run-and-errors.md) §7.2 / §4.2），**不得**笼统理解为"审计写失败 = 一律拒绝动作"：
  - **预写失败**（动作尚未发出）→ **拒绝发出**该次调用 / 工具执行（模型调用侧 Run 落 `failed`；工具侧回填"审计不可用"说明；§7.2 第 1 步）
  - **终态更新失败**（动作已发生）→ 记 ERROR、行**保留 `pending`**、**不得**伪造终态——下次启动恢复扫描标 `unknown`（§7.2 残余窗口 / §4.3.2）
  - **取消收尾写失败** → 记 ERROR、**保留 `cancelled` 语义**（审计写入用 `context.WithoutCancel` + 独立短超时，**不得**反向覆盖运行状态；§4.2）
  - 审计库整体不可用时，"失败必定落库"**无法**保证——按上述分支降级；fail-closed 守住的确定性边界是"**预写失败就没有已发出的动作**"；"记了日志"**不等于**已满足审计

## 4. 日志与错误规范

- 结构化日志（`slog`），双档：**dev 文本 handler / prod JSON handler**（编程指南 §3.2、US-1 步骤 6）；**不得** `fmt.Println` 打日志、**不得**手拼 JSON 字符串
- 字段纪律：固定键名、上下文进结构化字段；错误进字段（`error` / `error_kind`），**不得**把错误文本拼进消息主体
- 请求级追踪标识（trace id）经 `context` 携带——这是 `context.WithValue` 唯一允许的场景（宪法第 7 条）；HTTP 请求日志带 trace id（技术方案 §7.2 中间件链）
- **级别约定**（新增，2026-10-15 定）：
  - `ERROR`：需要人介入的失败（审计预写失败、审计终态更新 / 取消收尾写失败、panic 兜底、重试耗尽）
  - `WARN`：失败路径与降级（工具被拒、渠道发送失败——与审计落库构成双轨）
  - `INFO`：生命周期关键点（启动 / 关闭 / 迁移应用 / run 起止）
  - `DEBUG`：排障细节，prod 默认关闭
- **禁止**（症状 → 复核）：
  - 日志输出凭证 / token / 密钥——复核：日志语句里打印配置对象或 `%v` 全量回显的命中处逐处确认（编程指南 §8.2 第 3 条凭证纪律）
  - 外部地址写全量 URL（含查询参数）——只回 scheme + host（技术方案 §7.4）
  - 参数未剥离 CR / LF 就进日志——防日志注入（NivroOS 移植）
- **双轨可观测性**：失败路径**同时**要审计落库与 WARN 日志——只落其一即排障黑洞（起步手册 §5.4 硬约束）；审计写入**自身**失败按 §3 的三分支降级（记 ERROR、保留运行状态）——"双轨"要求审计可用时两轨都在，**不**顺势放宽为"审计库不可用时仍保证有审计记录"
- 对外错误：统一信封 `code` / `message` / `data` / `timestamp`（技术方案 §7.2）；错误码表与 message 要求以 [contracts/run-and-errors.md](contracts/run-and-errors.md) 为准；500 响应体只有话术，**不带**异常细节

## 5. 提交与分支规范

### 5.1 提交信息（双语四段式）

**规范原文以本节为准**；`.gitmessage` 是模板落地，CLAUDE.md / 编程指南 §8.2 / 起步手册的提法都是指向本节的指针。

- **标题**：`<type>(<scope>): <subject>`（Conventional Commits）；**英文**、祈使句、动词开头、≤ 72 字符、句尾无句号——标题只用英文，不写中文
- **正文四段**：Problem / Design / Testing / Impact；**逐段对照**——每段先英文段落、空行、再对应中文段落（2026-10-15 定案：标题仅英文 + 正文英中逐段对照）
- type 取值：

  | type | 用途 |
  | --- | --- |
  | `feat` | 新功能（US 交付物） |
  | `fix` | 修复（含陷阱表修复） |
  | `docs` | 文档（权威文档 / 契约 / 指南 / 台账） |
  | `refactor` | 重构（行为不变的实现调整） |
  | `test` | 测试补齐或修正（**不得**借改名 / 改断言来"修绿"） |
  | `chore` | 杂项（脚本 / 模板 / 仓库配置） |
  | `ci` | CI / 门禁（`.github` / Makefile） |
  | `build` | 构建与发布（交叉编译 / 打包） |
  | `perf` | 性能 |
  | `revert` | 回滚 |

- scope：`internal/` 包名（`app` / `core` / `provider` / `memory` / `tool` / `channel` / `web` / `storage` / `cli` / `workspace`）或 `build` / `docs`；归属不明或跨多包可省略
- 四段写什么：
  - **Problem**：为什么需要这次改动——现象 / 触发场景 / 根因（纯新增可写背景与目标）
  - **Design**：怎么做——关键决策、结构、引用的原则 / 文档章节；越界或与文档冲突处的裁决记录
  - **Testing**：怎么验证——**本轮当场**跑过的命令与结果；没跑 / 没覆盖的部分如实写明（编程指南 §8.2 第 6 条完成证据纪律）
  - **Impact**：影响范围——涉及包 / 契约 / 依赖 / 兼容性、后续项
- 破坏性变更：type/scope 后加 `!`，正文末尾加 `BREAKING CHANGE: <说明>`
- **项目发布版本**只由 `git tag` 表达（构建注入 `-ldflags` 是其派生），**不得**在提交信息里自称项目版本（起步手册 §6；NivroOS 移植）；**依赖升级 / 安全修复涉及的版本、数据库迁移版本**是背景事实，**应当**写进正文（Design / Impact 段）——没有它们，审计追溯会断线
- 小改动可只留标题（如 `docs: fix typo in GettingStarted`），删掉不需要的小节
- 粒度：**每个 US 收口一个 commit**（起步手册 §5.5 / §6）；AI **不得**自动 commit / push / tag——提交时机由用户决定（宪法提交纪律）
- 模板：[.gitmessage](../.gitmessage) 已入库并挂在仓库级 `commit.template`；新克隆后执行 `git config commit.template .gitmessage` 恢复

**版式示例**（内容虚构，仅示格式；填写说明见 `.gitmessage` 内的 `#` 注释）：

```text
docs: add engineering standards

**Problem:**
Coding, testing, database, logging and commit conventions were spread
across several documents; no single normative reference existed.

工程规范散落在多份文档里，没有单一权威文本可对照。

**Design:**
Add docs/EngineeringStandards.md consolidating the five areas, citing
existing sources and marking newly fixed clauses. Commit format is
canonical in §5; other documents point here.

新增 docs/EngineeringStandards.md 汇集五块规范，标注来源与新定条款；
提交格式以 §5 为准，其余文档改为指针。

**Testing:**
Structural self-check passed; markdownlint unavailable on this machine —
format judging NOT run, stated as such in the report.

结构自查通过；本机无 markdownlint，格式判卷未跑，已在报告如实写明。

**Impact:**
docs/ only; CLAUDE.md and GettingStarted get pointer edits; no code or
contract change.

仅 docs/；CLAUDE.md 与起步手册改为指针；无代码 / 契约变化。
```

### 5.2 分支与行尾

- `main` 为主线；每个 US 一个分支 `{NNN}-<slug>`（如 `001-foundation`）——本仓库无 `before_specify` hook，分支**手动建**（`git checkout -b`），操作口径见 `/module-dev`
- 合回 main 的时机与方式由用户定（建议快进合回、保持线性历史）；AI **不得**自动 commit / merge / push / tag
- 行尾纪律：`.gitattributes` 钉死入库 / 检出统一 LF（gofmt 的前提）——**不得**擅自改 `.gitattributes` 或放开 CRLF 例外

## 修订口径

- 本规范的增补 / 修订沿用项目惯例：AI 起草、**用户确认后**落地；与权威文档冲突时先裁决再改规范，**不得**反向"修正"权威文档
- 每条规范在 US 实施中第一次被违反并修复的教训，按惯例拟一条进陷阱表（编程指南 §7，带「怎么发现」与「怎么复核」），经用户确认后入库
