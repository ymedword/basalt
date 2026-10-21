# Implementation Plan: US-11 管理台与运行观测

**Branch**: `011-console-observability` | **Date**: 2026-10-21 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/011-console-observability/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 11` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us11-console-observability.md](../../docs/us/us11-console-observability.md)（R3，人工 review ① 2026-10-21 通过，sha256 `f0c802a0…`）§2 / §3 / §4 为准，冲突时以 [docs/TechnicalDesign.md](../../docs/TechnicalDesign.md) 为准（§1.1 / §1.3 / §3.4.1 / §3.4.2 / §3.6.1–§3.6.4 / §4.5 / §5.2 / §6.2.4–§6.2.6 / §7.1.1 / §7.1.2 / §8.2）；固定字面量以 [docs/contracts/](../../docs/contracts/) 为准（`openapi.yaml` 的 `AdminAuth` / 内容写 / 运行查询 / 只读观测端点与新增错误码、`run-and-errors.md` §4 / §8、`agent-md.md`、`schema.sql` 的七张 v2 表）。设计面已冻结；本 plan 只做落位与验证映射，不引入文档外交付物。

## Summary

给 Basalt 装上**可运营、可核对的管理面**：单二进制内嵌的管理台 + 最小认证闸门 + 运行资源账 + 硬性生命周期准入 + 内容条件写 + 有边界的热加载。七件可判卷的价值：

**① 非回环必须有人证**——开放 HTTP 的命令启动即校验 `http.admin_auth`；token 只从 `${ENV_VAR}` 解析，`local_trust` 仅限直接回环并需本地信任头；Host / Origin 守卫先于认证；失败计数只统计 401 与 `local_trust` 的 403，第 10 次起在凭据校验前 429。**② 管理变更先留痕**——`agent.content.update` / `agent.disable` / `agent.enable` / `system.baseline_v2` 一律先写 `pending` 操作行，启动恢复只查证不重做。**③ 资源基线可重入**——Provider / MCP / 技能首版与指针、技能不可变快照、既有 Agent 生命周期状态文件在开放服务前建好；重复启动幂等。**④ 生命周期状态文件是唯一权威**——准入在 Agent 级读锁内重读并校验校验和，`disabled` / 缺失 / 损坏一律 409 `AGENT_UNAVAILABLE`，停用不取消在途。**⑤ 运行资源账同事务**——准入固定 Provider / MCP / 技能版本并构造清单哈希，Run + 清单 + 全部引用单事务写入，终态同事务释放。**⑥ 内容条件写**——强 ETag + `If-Match`，旧值 412 且磁盘零变化；补丁保留注释与行尾，无法保留则 400 `EDIT_UNSUPPORTED`。**⑦ 前端同仓可复现**——Vue 3 + Vite 产物随源码提交、`go:embed` 进单二进制，Go 层校验集合与逐文件哈希，常规门禁零 Node。

技术路线的七个要点：**① 唯一的进程级闸门** `core.ResourceGate`（RWMutex）同时交给准入（读锁）与发布（写锁，US-12 写面复用），准入在 Agent 级读锁内只完成"重读并校验状态"判定，释放后由 `ResourceGate` 读租约覆盖"资源解析 → 固定资源 → `StartRun` 成功"，失败路径归还租约并释放已固定运行时。**② 状态文件与索引分离**——`workspace.LifecycleStore` 是 `.basalt/control/agents/<agent>.json` 的唯一写者，`storage` 只存可重建索引；`core.AgentGate` 提供每 Agent 读写锁。**③ 资源清单是持久事实**——`core.RunResourceManifest` 规整序列化后哈希，`storage` 在同一事务写 `run_resource_manifests` 与 `run_resource_refs`；HTTP 视图是独立投影，历史运行返回 `legacy_unrecorded`。**④ 内容写的单道闸门**——`workspace.ContentEditor` 复用加载期校验：解码 → ETag 比较 → 候选生成 → 校验 → 原子写 → 快照替换；补丁只动 frontmatter 已知字段的原始行。**⑤ watcher 非递归 + 全量 rescan**——`workspace.Watcher` 只监听已添加目录，新建目录补监听；校验失败保留旧快照并 WARN；需重启的变更只在页面与文档标注。**⑥ 前端零第三方运行时**——History API 自实现十二路由（UI-R2 追加扩展）、安全子集渲染器、token 只在内存；`assets/console/dist` + `integrity.json` 由 `go:embed` 读取并做集合 / 哈希校验。**⑦ 观测面只读**——会话 / 记忆 / 沙箱 / 通知 / 资源列表全部只读投影，凭证只回引用名，采样按平台文件实现且无基线返回 `null` + `unavailable`。

## Technical Context

**Language/Version**: Go 1.27.3 构建版本（`go.mod` 的 `go` 行写最低版本要求 `go 1.27`；准确构建版本由 `toolchain go1.27.3` 与 CI 的 `actions/setup-go`（两处 job 均为 `1.27.3`）共同固定——**不得**表述为「锁定 1.27.3」）。用户 2026-10-21 裁决由 1.27.2 升至 1.27.3；`docs/TechnicalDesign.md` §1.3 等正文的版本句修订按该裁决另行处理，不在本 US 实施面内。本轮实测 `go version go1.27.3 windows/amd64`。前端为构建期工具链：Vue 3 + Vite + TypeScript + Vitest（U-9 已批准），Node 版本由新增 `.nvmrc` 与 CI job 固定，**不依赖开发机全局版本**；本机当前无 Node。

**Primary Dependencies**: **运行时新增恰 1 个已批准坐标** `github.com/fsnotify/fsnotify v1.10.1`——2026-10-21 经 `GOPROXY=https://goproxy.cn go list -m -versions github.com/fsnotify/fsnotify` 实测存在（版本列表末项 `v1.10.1`），与 TechnicalDesign §1.3 一致。`golang.org/x/sys v0.48.0` 已是 `go.mod` 直接依赖（Windows 采样 `GetProcessTimes` / `psapi` 复用）。**不新增**其他 Go 坐标。前端依赖（Vue / Vite / TypeScript / Vitest）只进 `assets/console/package.json` 与 `package-lock.json`，不進 Go module、不進发布二进制；版本在实施首日按 npm registry 实测锁定，结果逐项写入实施报告。判据：`git diff go.mod` 的 `+` 行只含 `github.com/fsnotify/fsnotify`（及其必要的 `// indirect` 变化），`-` 行零命中；直接导入经 `go mod tidy` 后落在直接 `require` 块（`go mod tidy -diff` 零输出）；`go list -deps ./cmd/basalt | grep -c '^github.com/fsnotify/fsnotify$'` 为 1（v1.10.1 另有 `fsnotify/internal` 子包，按整包名精确计数）；`assets/console/package-lock.json` 存在且被 §3.1.25 清单点名。

**Storage**: **schema v1 → v2 前向迁移**。`internal/storage/migrate.go:21` 的 `SupportedSchemaVersion` 由 `1` 改为 `2`；新增 `internal/storage/migrations/0002_us11_us12_resources.sql`，创建七张表（`management_operations`、`resource_versions`、`resource_pointers`、`run_resource_manifests`、`run_resource_refs`、`agent_lifecycle`、`agent_archives`）与索引，全部 `CREATE ... IF NOT EXISTS`、幂等；已发布 `schema.sql` 与既有迁移**只读**。时间戳一律 INTEGER Unix ms。新库直接建到 v2；库内版本高于二进制支持版本拒绝启动。判据：`git diff -- docs/contracts/schema.sql internal/storage/schema.sql` 空输出；`internal/storage/migrations/` 只新增 `0002_*.sql`；`TestMigrate_*` / `TestSchemaV2_*` / `TestSchemaFile_MatchesContract` 全绿。

**Testing**: 默认层 `go test -race ./...`——**新增 29 个测试文件 376 条用例** + **修改 12 个测试文件约 +67 条**（逐文件见颗粒度文档 §3.2.1 / §3.2.2）。层次按 §4.1：L1 单元（ETag / 补丁 / 校验和 / 清单哈希 / 采样解析 / 限速窗口）、L2 契约（OpenAPI 属性集、错误码、视图字段、`schema.sql` 表结构）、L3 集成（临时目录 + 临时库：SQLite 事务、HTTP 认证链、原子写、watcher、基线）、L4 六条人工 Demo（§6.2）。**默认层不访问外部网络、不依赖真实模型**；HTTP 用 `httptest`，时钟用固定 `Now` 接缝，watcher 用临时目录 + 显式事件注入。前端 Vitest 只在 CI 的 Node job 跑；`make check` 与 `scripts/check-acceptance.*` **不得**调用 Node / npm。十七个专属 check（`011-core-lifecycle` / `011-core-resources` / `011-core-cancel` / `011-storage-management` / `011-storage-resources` / `011-storage-migration` / `011-workspace-lifecycle` / `011-workspace-content` / `011-workspace-watcher` / `011-web-auth` / `011-web-observability` / `011-web-content-lifecycle` / `011-app-baseline` / `011-app-registry` / `011-web-readonly-meta` / `011-app-runtime-admin` / `011-race`）按 §4.4 的精确测试函数名展开落 stage；`required_tests` 族名**不得**写成通配符。覆盖率：`core` / `tool` / `memory` / `storage` ≥ 70%，其余 ≥ 50%，由既有 `make cover` 判卷（**不改脚本与阈值**）。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布产物 `CGO_ENABLED=0` 构建；`-race` 与发布构建**分属不同命令 / job**。采样按 `sampler_windows.go` / `sampler_linux.go` / `sampler_other.go` 平台分片，不支持平台显式 `null` + `unavailable`。管理台面向现代常青浏览器；浏览器 E2E 不在自动门禁内。

**Project Type**: CLI + 服务单二进制，新增内嵌前端资产树 `assets/`（`go:embed`）。不新增 Go 顶层包；`internal/web` 增认证 / 观测 / 管理台文件，`internal/core` 增领域类型，`internal/workspace` 增文件与 watcher，`internal/storage` 增 SQL，`internal/app` 增装配。

**Performance Goals**: 无吞吐 / 延迟指标（管理面类 feature）；判据是**行为预算**——认证失败窗口 60 秒、第 10 次起 429、`Retry-After` 向上取整；取消等待上限 30 秒；采样缓存 5 秒；运行列表缺省 50 / 上限 200；热加载防抖窗口按颗粒度文档 §3.1.9 实测值；启动基线在开放服务前完成。

**Constraints**: **一道闸门**——进程内只有 `core.ResourceGate`，准入读锁 / 发布写锁，不得第二套互斥；**一处权威**——生命周期只认状态文件，不得第二份缓存；**一次事务**——Run + 清单 + 全部引用同事务，终态同事务释放；**一条审计路径**——管理操作先写 `pending`，与运行 / 模型 / 工具审计互不混写；**actor 显式参数**——不写入 `context`，`context.WithValue` 零新增；**零 Agent 框架**——`go.mod` 不出现框架坐标；**单二进制**——前端资产嵌入，运行时不读磁盘前端文件，不加载任何 CDN / 第三方运行时资源；**判据面守住**——`make check` 七目标不变、覆盖率阈值不变、不得调用 Node；**静态门禁**——抑制注释零使用；测试函数名英文、中文验收点以注释保留；格式只许 `gofumpt -w <本轮改动的文件>`。

**Scale/Scope**: **103 个 Go 文件**（62 生产 + 41 测试）——新增生产 34、修改生产 28、新增测试 29（376 条用例）、修改测试 12（约 +67 条）；另加迁移 SQL 1 个、前端工程 1 棵（`assets/console/`）、配置与文档 4 项（`basalt.yaml.example` / `docs/Operations.md` / `docs/GettingStarted.md` / `.github/workflows/ci.yml`）、依赖文件 `go.mod` / `go.sum`、harness 工件 `harness/stages/us11-console-observability.json`。十七个专属 check 见 §4.4；人工项为 §6.2 的六条 Demo。前序改造点 28 项（§3.1.24）；判据面变更见颗粒度文档 §3.5 与 run-state 的记录，既有断言零删除、零跳过、零放宽。

**环境实测读数（2026-10-21，本轮当场命令）**: 分支 `011-console-observability`；起始 commit `2188aa27949dce80253480fd82418bd80db8dc22`（`main` tip，US-15 收口后，`git rev-list --left-right --count main...HEAD` = `0 0`，未推送）。工作区已有 7 个 `M` 文件（宪法 2.0.0、契约 v2、`docs/Operations.md`、US-15 run-state），已按 run-state 基线保留，**不得** stash / checkout 覆盖。核对属实：`web.Options` 在 [internal/web/server.go:25](../../internal/web/server.go#L25)（无 `AdminAuth` / `Registry` / `Lifecycle` / `Console` / `Sampler`）；`core.ServiceOptions` 在 [internal/core/service.go:67](../../internal/core/service.go#L67)（无 `Lifecycle` / `Gate` / `Resources` / `RunSnapshots`）；`SupportedSchemaVersion = 1` 在 [internal/storage/migrate.go:21](../../internal/storage/migrate.go#L21)；`NewAgentLoader` 在 [internal/workspace/agent.go:130](../../internal/workspace/agent.go#L130)；`App.newRuntime` 在 [internal/app/wire.go:140](../../internal/app/wire.go#L140)；`cli.HTTPConfig` 在 [internal/cli/config.go:44](../../internal/cli/config.go#L44)；错误码常量在 [internal/web/envelope.go:30](../../internal/web/envelope.go#L30)（现有 10 个，缺 8 个新码，`IMPORT_REJECTED` 属 US-12）。`go.mod` 当前无 `fsnotify`（首任务加入）。`internal/storage/migrations/` 仅有 `.gitkeep`。**fsnotify v1.10.1 已实测存在于模块代理**（见 Primary Dependencies）。**本轮未跑** `make check`、覆盖率读数与真实进程 Demo。

### 文档 ↔ 仓库事实差异（实测发现）

**A. 与文档一致、作为落地基点的仓库事实**（逐条已核，不构成差异）：

| # | 文档写法 | 仓库实测 | 落地口径 |
| --- | --- | --- | --- |
| 1 | §3.1：`go.mod` 新增 `fsnotify v1.10.1` | `go.mod` 无 fsnotify；代理实测版本存在 | 首任务 `go get` + `go mod tidy`；只此一个运行时坐标 |
| 2 | §3.1：`SupportedSchemaVersion` 1 → 2 | [migrate.go:21](../../internal/storage/migrate.go#L21) 为 1 | 改为 2；新增迁移文件；`schema.sql` 与已发布迁移零改写 |
| 3 | §3.1：`web.Options` 追加 8 个字段 | [server.go:25](../../internal/web/server.go#L25) 无 | 追加 `AdminAuth` / `Registry` / `Lifecycle` / `AgentAdmin` / `Runs` / `Operations` / `Resources` / `Console` / `Sampler`（测试接缝保持包内） |
| 4 | §3.1：`core.ServiceOptions` 追加 4 个端口 | [service.go:67](../../internal/core/service.go#L67) 无 | 追加 `Lifecycle` / `Gate` / `Resources` / `RunSnapshots`；`nil` 时保持既有行为或显式报错 |
| 5 | §3.1：`cli.HTTPConfig` 追加 `AdminAuth` | [config.go:44](../../internal/cli/config.go#L44) 无 | 追加该段；`serve` / `gateway` 启动时校验，`chat` / `tool` 不要求 |
| 6 | §3.1：`web/envelope.go` 新增 8 个错误码 | [envelope.go:30](../../internal/web/envelope.go#L30) 现有 10 个 | 新增 `UNAUTHORIZED` / `FORBIDDEN` / `EDIT_UNSUPPORTED` / `AGENT_BUSY` / `AGENT_UNAVAILABLE` / `CONFLICT` / `PRECONDITION_FAILED` / `AUTH_RATE_LIMITED`；`IMPORT_REJECTED` 归 US-12 |
| 7 | §3.1.25：`assets/console/` 前端工程 | 目录不存在 | 新建；`dist/` 与 `integrity.json` 随源码提交；`.nvmrc` 固定 Node |
| 8 | §3.1.24：`internal/app/providers.go` / `mcp.go` 引入 `providerCatalog` / `mcpRuntime` | 两类型在 HEAD 不存在 | 按修改清单引入，计数以 §3.1 为准 |
| 9 | §3.4：`harness/stages/us11-console-observability.json` | `harness/stages/` 有 US-9 / US-10 历史文件 | 派生本 US stage；`inputs` 含私有 `docs/us/*`，**不進** `current.json`、不進 CI |

**B. 实测差异**：本轮逐条核对 §3.1 的签名、行号与被修改文件，未发现新的文档与仓库事实差异；实现期若出现新的签名漂移，按颗粒度文档「停止边界」口径停下登记，不静默自决。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 / 约束 | 本模块落地判据 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct 循环 | `go.mod` 零 Agent 框架坐标；本模块只改准入、观测与管理台，循环体不动 | 通过 |
| II LLM 客户端只做协议调用 / 单一消息仓储 | 运行资源账不改写消息；`final_text` 只读消息，不追加；模型调用仍由既有入口唯一写入 | 通过 |
| III Provider 显式映射 | 资源指针 → 准入固定映射；发布失败保旧；旧映射最后引用释放后回收；运行内只查固定表 | 通过 |
| IV 一个目录 = 一个 Agent / 声明式绑定 | 技能目录与发布版本冻结；不引入软连接；不新增 `use_skill`；声明名无实体整只不注册 | 通过 |
| V 审计 day-one 且 fail-closed | 管理面高权限变更先写 `pending`，预写失败零执行；启动恢复只查证不重做；管理审计与运行审计分离 | 通过 |
| VI 授权与执行分离 | 沙箱策略仍走 `Policy` / `CommandRunner` 端口；本模块只把控制目录排除加进策略白名单校验 | 通过 |
| VII 同步执行 + context 纪律 | 取消是同步信号 + 有界等待；`context.WithValue` 零新增；actor / 资源 / 会话显式传参 | 通过 |
| VIII 单二进制纪律 | 前端资产 `go:embed`；`CGO_ENABLED=0` 与 `-race` 分列；运行时只新增 fsnotify（纯 Go） | 通过 |
| IX 不用 ORM | 七张表手写 SQL 集中在 `storage`；迁移只追加；`schema.sql` 零改写 | 通过 |
| X 范围卡紧 | 交付物与 §3.1 / §3.2 清单逐一对应；15 项边界不進实现 | 通过 |
| 架构约束：依赖方向 | `core` 只依赖标准库；`web` / `workspace` / `storage` → `core`；`app` 唯一装配层；`assets/embed.go` 只被 `app` 导入，前端资产以 `Options.Console`（`fs.FS`）交给 `web`，运行期唯一消费点是 `web` | 通过 |
| 架构约束：接口定义在消费方 | `LifecyclePort` / `ResourceStore` / `AgentAdmin` / `RunService` 等端口声明在消费方（`core` / `web` / `app`），实现放 `workspace` / `storage` / `app` | 通过 |
| 架构约束：第三方实现放消费方包内或 `adapters` | fsnotify 封装在 `workspace/watcher.go`（消费方包内）；SQLite 仍在 `storage` | 通过 |

**Post-Design Re-check**: Phase 1 设计（research / data-model / quickstart）完成后复核，结论不变；无违反项需要 Complexity Tracking。

## Project Structure

### Documentation (this feature)

```text
specs/011-console-observability/
├── plan.md              # 本文件
├── spec.md              # 需求规格
├── research.md          # Phase 0 输出
├── data-model.md        # Phase 1 输出
├── quickstart.md        # Phase 1 输出
├── checklists/
│   └── requirements.md  # 规格质量清单
├── run-state.md         # 过程状态（非设计输入）
└── tasks.md             # Phase 2 输出（/speckit-tasks，不由本命令创建）
```

**不新建 `specs/011-console-observability/contracts/`**：固定字面量的唯一来源是 [docs/contracts/](../../docs/contracts/)（`openapi.yaml` / `run-and-errors.md` / `agent-md.md` / `schema.sql`），复制一份必然漂移。

### Source Code (repository root)

```text
internal/
├── core/                          # 领域类型与端口（只依赖标准库）
│   ├── management.go              # 新增：ManagementOperation / 状态与恢复查证类型 / 端口
│   ├── lifecycle.go               # 新增：LifecycleState / AgentGate / LifecyclePort
│   ├── resources.go               # 新增：ResourceGate / RunResourceManifest / 清单哈希 / 视图分离类型
│   ├── service.go                 # 修改：ServiceOptions 追加 Lifecycle / Gate / Resources / RunSnapshots；Cancel
│   ├── run.go                     # 修改：准入固定资源与清单传递
│   ├── store.go                   # 修改：运行记录与资源引用的仓储端口
│   ├── snapshot.go                # 修改：快照与清单的衔接
│   └── scheduler.go               # 修改：注册表替换后的再注册
├── workspace/                     # 文件系统实现
│   ├── lifecycle.go               # 新增：状态文件读写（路径 / 字段顺序 / 校验和 / 原子写）+ 索引同步
│   ├── content_edit.go            # 新增：ETag / 补丁应用 / 原子写 / 快照替换
│   ├── watcher.go                 # 新增：fsnotify 非递归监听 + 防抖 + 全量 rescan
│   ├── skill_version.go           # 新增：技能发布版本读取与哈希定义
│   ├── agent.go                   # 修改：注册不变量 / 声明名无实体整只不注册 / 控制目录排除
│   ├── context_loader.go          # 修改：技能版本固定
│   └── skill.go                   # 修改：发布版本目录
├── storage/                       # 手写 SQL
│   ├── management_store.go        # 新增：管理操作预写 / 回填 / 恢复查证 SQL
│   ├── resource_store.go          # 新增：资源版本 / 指针 / 清单 / 引用 / 生命周期索引 SQL
│   ├── run_query.go               # 新增：运行列表 / 详情查询 + final_text 取消息
│   ├── migrations/0002_us11_us12_resources.sql  # 新增：七张 v2 表与索引
│   ├── migrate.go                 # 修改：SupportedSchemaVersion 1 → 2
│   ├── run_store.go               # 修改：StartRun / FinishRun 接入清单与引用同事务
│   ├── recovery.go                # 修改：启动恢复顺序
│   ├── bootstrap.go               # 修改：v2 建库路径
│   ├── db.go                      # 修改：连接与版本校验
│   └── session_store.go           # 修改：会话列表投影
├── web/                           # HTTP 与管理台
│   ├── auth.go                    # 新增：Host / Origin / token / local_trust / actor 解析（认证链本体）
│   ├── auth_limit.go              # 新增：失败计数与 429
│   ├── console.go                 # 新增：静态壳与 SPA 深链接回退
│   ├── handlers_system.go         # 新增：概览与采样
│   ├── handlers_runs.go           # 新增：运行列表 / 详情 / 取消
│   ├── handlers_operations.go     # 新增：管理操作读面
│   ├── handlers_agent_content.go  # 新增：ETag 读 / 条件写
│   ├── handlers_lifecycle.go      # 新增：停用 / 启用
│   ├── handlers_resources.go      # 新增：技能 / MCP / 模型连接只读面
│   ├── handlers_sessions_query.go # 新增：会话列表查询
│   ├── handlers_memory.go         # 新增：Agent 记忆
│   ├── handlers_sandbox.go        # 新增：沙箱白名单
│   ├── handlers_notify_read.go    # 新增：通知渠道与类型
│   ├── sampler.go                 # 新增：采样接口 + 5 秒缓存 + unavailable
│   ├── sampler_windows.go         # 新增：GetProcessTimes / psapi 工作集
│   ├── sampler_linux.go           # 新增：/proc/self/stat + statm
│   ├── sampler_other.go           # 新增：显式不支持
│   ├── server.go                  # 修改：Options 追加字段 + routeTable()
│   ├── envelope.go                # 修改：8 个新错误码 + 固定文案
│   ├── middleware.go              # 保持不变（US-9 的通用中间件链）；认证链在 auth.go，路由挂载在 server.go
│   ├── views.go                   # 修改：运行 / 操作 / 资源视图投影
│   └── handlers_sessions.go       # 修改：既有端点的认证后行为
├── app/                           # 唯一装配层
│   ├── baseline.go                # 新增：资源基线 + 生命周期状态文件 + 恢复协调
│   ├── registry.go                # 新增：不可变注册表快照与原子替换
│   ├── watcher.go                 # 新增：watcher 装配与注册表 / 调度同步
│   ├── agent_console.go           # 新增：AgentAdmin 端口适配器
│   ├── run_service.go             # 新增：web.RunService 适配器（Run / Cancel / Detail）
│   ├── resources.go               # 新增：Resources 端口适配器
│   ├── providers.go               # 修改：providerCatalog
│   ├── mcp.go                     # 修改：mcpRuntime
│   ├── run.go                     # 修改：启动顺序
│   ├── wire.go                    # 修改：ServiceOptions / web.Options / heavyRuntime.Close
│   ├── web.go                     # 修改：服务装配
│   └── schedule.go                # 修改：注册表替换后的调度同步
├── cli/
│   └── config.go                  # 修改：HTTPConfig.AdminAuth + 启动校验
└── tool/
    ├── policy.go                  # 修改：控制目录排除
    └── filetools.go               # 修改：控制目录拒绝

assets/
├── embed.go                       # 新增：go:embed console/dist（integrity.json 位于 dist 内）
└── console/                       # 新增：Vue 3 + Vite + TS + Vitest 工程（§3.1.25）
    ├── .nvmrc / package.json / package-lock.json / tsconfig.json / vite.config.ts
    ├── index.html
    ├── scripts/emit-integrity.mjs
    ├── src/{main,app,router,api,auth,render,components,views}
    ├── tests/
    └── dist/（含 integrity.json）# 构建产物随源码提交

basalt.yaml.example               # 修改：http.admin_auth 示例（${ENV_VAR} 占位）
.github/workflows/ci.yml          # 修改：新增前端 Node job（npm ci / test / build + 一致性）
docs/Operations.md                # 修改：认证配置 / 热加载边界 / 需重启清单
docs/GettingStarted.md            # 修改：管理台入口与首个 token 步骤
harness/stages/us11-console-observability.json  # 新增：本 US 专属 stage（本地）
```

**Structure Decision**: 保持单 Go module + `internal/` 分包。`core` 只放领域类型与端口（不 import 其他内部包）；`workspace` / `storage` 提供文件与 SQL 实现；`web` 只消费端口；`app` 唯一装配。前端独立成 `assets/console/`（构建期资产，非 Go 包），由 `assets/embed.go` 嵌入。不新增顶层包、不建 `internal/adapters/` 新包（fsnotify 封装在 `workspace` 消费方包内）。

## Complexity Tracking

无 Constitution Check 违反项，无需登记。

## Phase 0 / Phase 1 产物

- [research.md](./research.md)：关键技术决策与替代方案（fsnotify、单闸门、状态文件权威、ETag 条件写、同事务清单、嵌入前端、采样分片）。
- [data-model.md](./data-model.md)：七张 v2 表与领域实体、状态迁移、索引与约束。
- [quickstart.md](./quickstart.md)：可执行的验证向导（门禁、专属 check、六条人工 Demo）。

## UI-R2 追加修订（2026-10-22）

选择 A：在原八条路由上增加工具、全局记忆、消息渠道、执行环境四条只读页面入口；现在为十二条 + 404。端点、认证、资源发布 kind 与写面保持原契约。约束 41 / spec FR-043 为当前路径清单；旧审阅哈希与读数仅作历史。

前端补充文件：`src/workspace.css`、`src/components/brand.ts`、`src/views/shared.ts`、`src/views/workspace.ts`、`tests/workspace.test.ts`。统一运行列表 / 运行详情、资源、操作与 404 的页眉和区块，修正侧栏高亮、MCP 二级导航、长标识与窄屏最近执行。共享表现组件不创建第二套业务数据源。

验证：T047–T050 由 tasks 的 UI-R2 补充任务重新执行，前端测试与生产构建独立留证；Go stage 不调用 Node。重新运行完整 stage，不能使用改版前的 `us11-visual-r1` 替代当前源码 / 制品证据。
