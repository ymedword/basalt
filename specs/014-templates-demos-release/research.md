# Research: US-14 MCP 目录与模板 + 五个每日业务 Demo + 发布收尾

**Date**: 2026-10-25 | **Plan**: [plan.md](./plan.md) | **Spec**: [spec.md](./spec.md)

本文件记录 Phase 0 的关键技术决策，来源限于已人审模块卡（R3）、[docs/TechnicalDesign.md](../../docs/TechnicalDesign.md)、[docs/contracts/](../../docs/contracts/) 与现有代码事实；不引入模块卡之外的第三方资料或版本号。

## R-01 MCP 目录的载体与读取时机

- **Decision**: 目录数据是 `internal/catalog/data/mcp_catalog.yaml`，经 `//go:embed` 编进二进制；`Load(raw []byte)` 是纯解析 + 校验函数；`Builtin()` 在启动装配时解析一次，`web.Options.MCPCatalog` 注入只读读者；请求期不重读文件、不访问网络。
- **Rationale**: 目录必须"随二进制嵌入、只读、可追溯"（模块卡 §1.1 第 4 条），且读取零进程 / 零安装 / 零配置写。编译期快照让目录与制品版本天然绑定，启动一次解析让请求路径无 I/O 与解析失败分支膨胀；纯函数 `Load` 让校验可单测。
- **Alternatives considered**:
  - Go 字面量结构体：省一次解析，但配方字段多、编辑与审阅远不如 YAML，且校验逻辑仍要写；
  - 运行时下载 / 读远端：引入网络依赖与"目录本身不可用"的失败面，与只读、可复现冲突；
  - 每次请求重读 YAML：多一次 I/O 与解析失败分支，收益为零。

## R-02 目录校验规则与单一 schema

- **Decision**: `Load` 校验：`version > 0`；`id` 唯一且 kebab-case；`transport` 只允许 `stdio` / `http`；stdio 必须有 `command`，http 必须有绝对 `url`；`env[].placeholder` 恰为 `${KEY}` 且 `KEY == env[].key`；`env[].key` 不重复；`source_url` 必须 `https://`；`verified=false` 时 `verification_note` 非空，`verified=true` 时 `verified_at` / `verified_platforms` 非空，平台只允许 `windows` / `linux` / `darwin`。
- **Rationale**: 目录是"配方"不是运行配置，拼接出的每条配方必须能被既有 `workspace.ParseMCPServerBlock` 装载；`TestCatalog_RecipesParse` 直接把 YAML 条目转成 `mcp_servers.yaml` 片段再走既有解析器，避免"目录自洽但与真实配置语法不一致"的两套规则。
- **Alternatives considered**:
  - 只做 YAML schema 校验、不验证可装载：会放过"目录能读、配置不能存"的配方；
  - 目录另写一套配置语法：违反"同类语义不得出现第二套实现"（宪法架构约束）。

## R-03 诊断复用启动构造（不启动进程）

- **Decision**: 新增 `internal/tool.NewMCPCommand(spec, root, envPassthrough) (*exec.Cmd, error)`，`dialCommand` 改为调用它；诊断 `diagnoseMCPCommand` 也调用它但**不 `Start`**，从构造结果映射三态（`located` / `not_found` / `unconfirmed`）与原因（`lookpath` / `absolute` / `not_found` / `relative_path` / `script` / `permission` / `unknown`）。
- **Rationale**: 诊断必须回答"按实际启动方式能否定位"，任何"单独的 `exec.LookPath`"或"另一套路径规则"都会与真实启动漂移；构造与启动同源，但构造不产生进程，满足"诊断不得启动进程"的硬约束（模块卡 14A 停止条件 3）。
- **Alternatives considered**:
  - 只调 `exec.LookPath`：漏掉 `.cmd`、相对路径、脚本等真实分支，且与 `dialCommand` 规则分叉；
  - 诊断时真实启动一次再关闭：违反"零进程"与"只读"约束，还会产生副作用；
  - 复制一份构造逻辑到诊断：第二套实现，规则漂移。

## R-04 env 闸门的位置、状态与幂等语义

- **Decision**: `validateMCPEntryEnv(entryRaw, lookup)` 在 MCP 配置的创建 / 修改 / 发布 / 回滚四个入口、第一次写文件前调用；失败时先写 `pending` 管理操作再拒绝（503 + `operation_id`），文件 / 版本 / 指针不变，错误只点名 server / 键 / 变量名。幂等发布（无变更）在闸门之前短路：不重新校验、不新建版本、不宣称环境已满足。手工改 `mcp_servers.yaml` 的启动期对账保持既有 fail-closed。
- **Rationale**: 把失败暴露在写入之前是最小惊讶路径，避免"占位符字面量写进配置后连接失败"；四个入口覆盖所有管理面写路径（模块卡 §2.9），启动期对账覆盖手工编辑。幂等发布短路防止"没变更却因瞬时环境缺失而拒绝"，同时不得把"跳过校验"写成"校验通过"。
- **Alternatives considered**:
  - 只在发布时校验：创建 / 修改仍会写入不可用配置，失败延迟到发布；
  - 只在连接时解析：错误发生在 Run 里，无法预写管理操作、无法给操作详情；
  - 幂等发布也校验：无变更请求会因当前 shell 环境缺失而被拒，语义错误。

## R-05 测试记录的字段扩展与失败分类来源

- **Decision**: `ResourceTestResult` 追加 `OperationID` / `TestedVersionID` / `Stage` / `FailureClass`；`internal/tool` 新增 `ConnectDetailed` 返回 `MCPConnectFailure{Server, Stage, Class, Err}`；`Connect` 内部调用 `ConnectDetailed` 并保持"吸收单 server 失败"的旧行为；`MCPSupervisor.Test` 在连接器实现 `ConnectDetailed` 时使用详细结果，否则回退 `Connect` 并标 `dependency_unconfirmed`。分类只来自结构化失败，不解析 stderr / 日志。
- **Rationale**: 兼容既有 fake 与启动路径（`mcpConnector` 接口不变），同时拿到阶段与分类；`Connect` 旧语义不变是回归底线。解析 stderr 会把"第三方 server 的措辞"写成契约，且中英文输出不稳定。
- **Alternatives considered**:
  - 新表存测试结果：需要 schema 迁移，违反零迁移；
  - 让 `Connect` 直接返回详细失败：破坏既有调用方与启动路径语义；
  - 从 stderr 文本归类：不可判定、不可复现。

## R-06 工具来源记录在装配层

- **Decision**: `internal/app/mcp_disposition.go` 维护 `mcpDispositionTable`：内置工具注册时 `recordBuiltin`，MCP `OnConnected` 注册时 `recordMCP`；视图读 `mcpRuntime.ToolDisposition(name)`；`internal/tool/registry.go` 的注册语义、重名拒绝与 WARN 不变；`McpServerView.Tools` 保持 offered 语义。
- **Rationale**: 工具注册池是"最终谁赢"的既有权威，改它会把展示需求塞进注册语义；装配层同时看得到内置与 MCP 两侧注册结果，是唯一能记录"offer / accept / skip + 赢家"的位置。表只读投影，不参与执行。
- **Alternatives considered**:
  - 改 `Registry` 暴露拒绝原因：污染注册语义且属前序公共接口变更；
  - 从注册快照反推：无法区分"内置赢家"与"同名重复"的具体来源与原因；
  - 只在前端计算：前端拿不到注册过程，只能猜。

## R-07 两套 `warnings` 保持独立枚举

- **Decision**: 诊断与目录各有一套固定 code 集合（D-10），后端只返回 code，前端做文案映射；两套不合并、不共用常量表；新增 code 必须同时更新契约、前端文案与测试。
- **Rationale**: 两个集合回答不同问题（"这条运行配置有什么边界"vs"这条配方有什么前提"），合并会让任一集合新增 code 时另一侧被迫接受；Q-04 已裁决不合并。
- **Alternatives considered**:
  - 单枚举 + 场景子集：看似复用，实际每次新增都要论证"另一侧为何不报"；
  - 后端拼自由文本：把未验证推断写进 UI，且不可测。

## R-08 示例采用便携基线 + SETUP 覆盖

- **Decision**: 每个 Demo / 模板的 `AGENT.md` 不含实例 UID、不含真实凭据，基线 `notify_mode: none`、无 `deliver_to`、无 `resource_bindings`；导入后按 `SETUP.md` 编辑模型绑定、服务名、渠道 UID、`deliver_to`、解释器路径；固定输入放 `fixtures/`，`state/` 不提交；不自动导入、不自动放宽白名单。
- **Rationale**: 示例要"能被复制"就不能绑定某个实例的身份；SETUP 显式列出编辑项让覆盖面可审计（覆盖 diff 是 14B 证据）；自动导入会静默改写实例配置，违反既有写入门与审计约束。
- **Alternatives considered**:
  - 示例里写真实 UID / 渠道：泄露且不可移植；
  - 提供导入脚本自动合并：绕过 `schedule_write` / 内容 ETag 等写入门；
  - 把示例做成"只能看"的文档：无法证明可复跑。

## R-09 `docs-readonly` 用纯标准库 + 主 module 内 `package main`（2026-10-26 改裁）

- **Decision**: `examples/shared/mcp/docs-readonly/` 是主 module 内的 `package main`，只实现 `initialize` / `tools/list` / `tools/call` 三件只读工具，拒绝越界、绝对路径、软链接与写操作；不被产品二进制导入，随根 `go test ./...` 测试；安装器把源码导出到 `<ws>/demos/src/docs-readonly` 并生成最小 `go.mod`（`module basalt-docs-readonly` + `go 1.27`），导出目录可独立构建。
- **Rationale**: 发布契约是单二进制，源码必须能经 `go:embed` 随二进制发行——实测 `go:embed` **跨不过嵌套 module**（2026-10-25 读数：`go list -f '{{range .EmbedFiles}}{{println .}}{{end}}' ./examples` 共 42 个文件、`docs-readonly` 占 0 个；并回主 module 后同一命令为 44 个，含 `shared/mcp/docs-readonly/main.go` 与 `main_test.go`），2026-10-26 用户裁决并回主 module；纯标准库保证零第三方依赖、可离线构建；不被产品导入保证依赖方向不倒挂。官方 Filesystem MCP 进程具备写能力，只有"工具面只读"，不能支撑 D4 的只读断言（模块卡 §2.5.2）。
- **Alternatives considered**:
  - 独立嵌套 module（2026-10-25 初版决定，2026-10-26 撤回）：`go:embed` 不含它，安装器只能打印一个无人创建的路径；
  - 维护第二份源码副本供扩展：两处源码会漂移，违反单一事实源；
  - 用官方 Filesystem MCP：写能力无法在本项目内证明被禁用；
  - Python 实现：需要解释器依赖，且与"最小素材"不符；
  - 做进产品 `internal/`：把示例升格为产品内建 server，超出范围。

## R-10 示例测试逐目录计数

- **Decision**: `scripts/run-example-tests.py` 按固定目录清单（五个 `examples/daily/*/scripts`）逐目录执行 `unittest discover`，解析 `Ran N tests` 并断言每目录 N ≥3；0 发现 / 低于下限 / 非零退出 = 失败；缺 Python = 失败。
- **Rationale**: `unittest discover` 在路径写错或文件命名不符时返回 0 且"零测试通过"，是典型漏测；逐目录解析数量把"没跑"与"跑了但少"都变成红态。固定清单避免新增目录被静默漏跑。
- **Alternatives considered**:
  - 只跑 `python -m unittest discover`：零发现仍绿；
  - 引入 pytest：新增依赖，违反零新增；
  - 在根 `go test` 里调用 Python：把解释器变成 Go 门禁依赖。

## R-11 发布 e2e 绑定候选二进制

- **Decision**: `make e2e` = `build` + `example-tests` + `BASALT_E2E_BINARY=$(CURDIR)/bin/basalt go test -tags=e2e -count=1 ./internal/e2e`；`internal/e2e/harness_test.go` 的 `TestMain` 读取该变量，导出绝对路径、重算 SHA-256、打印 `binary_source=candidate`；变量缺失时保留 temp 构建回退并打印 `binary_source=temp`（供 `demo-fixtures` / `legacy-d1` / `template-quality` 直接 `go test -tags=e2e` 使用）；仅 `make e2e` / stage 断言 `candidate`，缺失或 `temp` 不计发布证据。
- **Rationale**: 发布闸门必须验证"要分发的那一份文件"，而不是测试里临时构建的另一份；同一份文件 + SHA-256 让证据可逐字节核对。`TestMain` 集中处理，避免每个用例各自找二进制。
- **Alternatives considered**:
  - 测试内部 `go build`：产物与分发件不是同一份，哈希无法对齐；
  - `go run`：多一层编译缓存与进程包裹，路径不稳定；
  - 用 PATH 里的 `basalt`：无法证明是本候选。

## R-12 `make e2e` / `example-tests` 与门禁分离

- **Decision**: 两者不加入 `make check`、`harness/acceptance.json` 的 `full` / `ci`，也不进 CI；只由 `harness/stages/us14-release.json` 与发布流程调用。
- **Rationale**: e2e 依赖候选二进制与 Python，耗时长且属发布前人工触发；加入日常门禁会让门禁依赖 Node / Python / 构建产物，违反"`make check` 不调用 Node / npm"与单二进制纪律的边界。
- **Alternatives considered**:
  - 加入 `acceptance.json`：改变既有门禁语义，超出本 US 授权；
  - 只在 CI 跑：与"发布候选由维护者本地/目标机验证"的证据口径不符。

## R-13 干净检出用 `git archive`

- **Decision**: `scripts/check-release-clean.sh` 先要求 `git status --porcelain` 为空，再 `git archive HEAD` 到临时目录，在临时目录构建、跑门禁与 e2e；缺候选提交 / 脏工作区 = 失败。
- **Rationale**: `git archive` 只导出已提交内容，天然排除未跟踪物与本地私有文件；比复制工作树更能证明"候选提交可复现"。脏工作区直接失败，避免把本地改动混进证据。
- **Alternatives considered**:
  - 复制工作树：包含未提交 / 私有文件，证据不可复现；
  - `git clone`：需要远端与网络，且会带上非候选分支状态；
  - 只在当前目录跑：无法证明"干净检出"。

## R-14 公开材料增量与数据模块

- **Decision**: 官网只在既有 Docs 索引页加 Templates & demos / MCP catalog 区块，数据集中在 `website/.vitepress/data/templates.mjs`（双语字段：五 Demo + 八模板 + 六目录），由 `TemplateTable.vue` 渲染；README 英文单语加四节；`website/tests/content.test.mjs` 与 `check-links.mjs` 增加模板表一致性与 `examples/` 链接允许 / 私有路径拒绝。
- **Rationale**: US-13 已把站点信息架构与视觉冻结，新增一级路由或复制一套卡片会重开设计；集中数据模块让 README 与站点可逐条比对（内容同源原则的延续）。`examples/` 是公开目录，链接允许清单必须显式放行而不是放宽私有路径规则。
- **Alternatives considered**:
  - 新增独立页面 / 一级路由：重开 US-13 设计，且不在本 US 授权内；
  - README 与站点各写一份清单：必然漂移；
  - 直接从 `examples/` 运行时读取生成页面：站点是静态构建，构建期依赖示例文件会耦合发布。

## R-15 `v0.1.0` tag 与外部实跑的人工边界

- **Decision**: 14C 只产出发布证据；tag、push、部署由用户手动执行。所有真实外部服务（模型、GitHub、Brave、Fetch、Playwright、DBHub、真实通知接收方）保持未授权，只用本地夹具并在证据中标注"未验证"。
- **Rationale**: 宪法与项目约定禁止 AI 自动 commit / push / tag / 部署；把外部实跑写成通过会在 review ② 与对外表述中失真。夹具能验证"接线与判定逻辑"，不能验证第三方兼容性。
- **Alternatives considered**:
  - 用公共演示端点替代真实服务：仍是外部调用，且未获授权；
  - 把夹具结果写成"真实实跑"：证据造假，明确禁止；
  - 让 AI 代打 tag：违反硬性禁止。

## 未决与后续

- 真实外部实跑、双平台走查、依赖与许可表、`v0.1.0` tag 均为人工 / 授权项，进入 run-state 未验证清单；不在本文件预写通过。
- 目录配方的命令、参数、入口路径与工具名在实测前保持 `verified=false`；14B 实测后按实际结果更新，不提前写"已验证"。
