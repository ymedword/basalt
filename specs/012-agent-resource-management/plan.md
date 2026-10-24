# Implementation Plan: US-12 Agent 与公共资源动态管理

**Branch**: `012-agent-resource-management` | **Date**: 2026-10-22 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/012-agent-resource-management/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 12` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us12-agent-resource-management.md](../../docs/us/us12-agent-resource-management.md)（R3.1，人工 review ① 2026-10-21 通过、2026-10-22 按用户裁决修订，sha256 `ccca73fa762baa6f457d9ea3e2ba2a30638db141b5cb0154b6bb0f43a31acfd1`）§2 / §3 / §4 为准，冲突时以 [docs/TechnicalDesign.md](../../docs/TechnicalDesign.md) 为准；固定字面量以 [docs/contracts/](../../docs/contracts/) 为准（`openapi.yaml` 的 Agent / 资源 / 通知 / 工作区端点与错误码、`run-and-errors.md` §9.4 恢复判据、`agent-md.md`、`schema.sql` 的 v2 表结构）。设计面已冻结；本 plan 只做落位与验证映射，不引入文档外交付物。

## Summary

把 US-11 建立的管理面底座接上真正的产品动作：Agent 创建 / 作者草稿 / 多文件提交 / 复制 / 归档 / 恢复，模型连接与 MCP server 的登记 / 修改 / 测试 / 发布 / 删除，技能的创建 / 导入 / 编辑 / 校验 / 发布 / 删除，公共资源的版本、发布指针、引用检查与受控删除，以及只读工作区浏览和通知渠道 CRUD。五条价值主线：

**① 写操作先留痕再动目标**——任何写端点先在 `pending` 管理操作落库，预写失败零写入；纯请求校验（路径、`If-Match`、存在性、候选解析）在操作行之前完成，412 / 404 / 400 不产生操作行；`generate-files` 例外，只写模型调用审计。**② 定义在文件、发布事实在数据库**——运行对象只由已发布版本构建；手工编辑走启动对账；发布失败保留旧文件与旧指针；版本只追加，撤回用旧内容创建新版本。**③ 条件写与分歧闸门**——资源 ETag 是当前发布版本内容的强哈希，技能是完整文件清单的规整哈希，Agent 多文件提交用受管文件集合 ETag；锁内重读并比较，陈旧值 412 零写入、零操作行。**④ 生命周期只有一条写路径**——创建 / 复制 / 恢复 / 归档都经既有 `LifecyclePort`；归档按固定顺序执行并有界等待在途 Run，超时 409 `AGENT_BUSY` 且完整回滚。**⑤ 管理台是交付形态的一半**——四条新路由只使用既有静态壳与自实现路由，写后重新读取，不做乐观更新，不引入任何前端运行时依赖。

技术路线的十个要点：

1. **审计与校验分层**：`ManagementStore` 预写 `pending` → 服务层执行 → 同事务写终态；动作字面量逐字取契约；操作标识 `op-<millis>-<6 位>`。
2. **定义文件块编辑**：`workspace` 按顶层列表条目定位、只替换目标块字节，块外逐字保留；无法安全保留注释 → 400 `EDIT_UNSUPPORTED`。
3. **发布编排**：取资源锁 → 取定义文件锁 → 重读文件 → 分歧闸门 → 块编辑 / 原子写 → `ResourceGate` 写锁 → 同事务写版本 / 指针 / 操作终态 → 运行时生效；失败逆序补偿并做文件身份检查。
4. **显式 Provider 映射**：`newProviderCatalogFromPublished` 只从已发布版本构建不可变映射；评测注入为每个已发布名建替身，不构造、不请求真实客户端；构建失败保留旧表。
5. **Agent 级写锁**：创建 / 提交 / 复制 / 归档 / 恢复 / 启停 / 准入共用同一把 Agent 读写锁；归档等待 Run 退出时不持有阻塞 Run 终态的锁。
6. **多文件提交**：锁内重读受管文件集合并比较集合 ETag，通过后构建完整副本（未列出文件保留），两次 rename 交换；崩溃残留由启动恢复前滚 / 回滚。
7. **归档与恢复**：注销定时 → 移出注册 → 有界等待（30 秒）→ 状态迁移 `archived` → 目录移入归档区 → 写归档记录；三档回滚矩阵；原名恢复保留 generation 并条件迁移，异名才 Bootstrap。
8. **MCP supervisor 与命令执行分离**：stdio 走命令授权与进程树回收，HTTP 走请求授权与域名白名单、零进程；应用版本只在成功连接后写入，待重启由发布 / 应用版本差异派生。
9. **引用视图与删除闸门**：Agent 引用来自注册表与归档扫描（解析失败按可能引用处理），活动运行引用来自未释放 `run_resource_refs`；公共资源不提供 force，通知渠道只接受 `?force=true` 字面值。
10. **CLI 与 HTTP 共用同一管理服务**：`profile create` / `profile delete` 取一次实例锁、打开并迁移数据库、经端口调用 `AgentAdmin`，写真实操作行与归档行；`internal/cli` 不 import `internal/app`。

## Technical Context

**Language/Version**: Go 1.27.3 构建版本（`go.mod` 的 `go` 行写最低版本要求 `go 1.27`；准确构建版本由 `toolchain go1.27.3` 与 CI 的 `actions/setup-go` 固定——**不得**表述为「锁定 1.27.3」）。模块卡固定技术栈句仍写 1.27.2；用户 2026-10-21 已裁决升至 1.27.3，仓库 `go.mod` 与 US-11 交付均已是 1.27.3，本轮以 `go.mod` 为唯一事实源，不改文档版本句。前端为构建期工具链：Vue 3 + Vite + TypeScript + Vitest（US-11 已批准），版本以 `assets/console/package-lock.json` 为准；本机无 Node，前端测试与构建只在 CI 的 Node job 执行。

**Primary Dependencies**: **运行时零新增坐标**——`git diff go.mod go.sum` 必须为空。既有直接依赖：`spf13/cobra v1.10.2`、`goccy/go-yaml v1.19.2`、`sashabaranov/go-openai v1.43.0`（只做协议调用）、`modernc.org/sqlite v1.60.1`、`robfig/cron/v3 v3.0.1`、`modelcontextprotocol/go-sdk v1.8.0`、`fsnotify v1.10.1`、`golang.org/x/sys v0.48.0`、`stretchr/testify v1.12.1`；测试专用 `santhosh-tekuri/jsonschema/v6 v6.0.3`。MCP stdio / HTTP 两种传输都复用现有 SDK 能力，不引入新坐标。前端零新增运行时依赖、零新增构建依赖。

**Storage**: **R4 范围修订后（2026-10-23）本条由文末「R4 范围修订的技术计划」取代**：schema v3 / 26 张表 / 迁移 `0003_credentials_scope.sql`。以下为 R3 原文记录——沿用 US-11 的 **schema v2，零新增迁移**——`internal/storage/migrations/` 保持只有 `0002_us11_us12_resources.sql`，`SupportedSchemaVersion` 保持 2（[migrate.go:21](../../internal/storage/migrate.go#L21)）。复用七张表：`management_operations`（扩展动作组）、`resource_versions`（只追加）、`resource_pointers`（发布 / 应用指针）、`run_resource_manifests`、`run_resource_refs`（活动运行引用）、`agent_lifecycle`（可重建索引）、`agent_archives`（归档身份）。通知渠道沿用 US-8 的 `notify_channels`。写边界：版本 / 指针 / 操作终态同事务；归档行与操作终态同事务；生命周期索引不进事务；不跨介质事务。

**Testing**: **R4 增量（2026-10-23）**：另加 14 条 check（合计 32 条）与 26 个新增测试文件、26 个修改测试文件（见文末「R4 范围修订的技术计划」），R3 的十八个 check 名全部保留。以下为 R3 原文记录——默认层 `go test -race ./...`——**新增 28 个测试文件 / 315 条用例** + **修改 20 个测试文件 / 新增 94 条用例**（逐文件见颗粒度文档 §3.2.1 / §3.2.2）。层次按 §4.1：L1 单元（YAML 块、ETag、路径 / 大小边界、整词匹配）、L2 契约（端点 / 字段 / 错误码 / 动作字面量 / 表结构）、L3 集成（临时目录 + 临时库：SQLite 事务、原子写、两次 rename、supervisor 夹具、启动对账）、L4 九条人工 Demo（§6.2）。**默认层不访问外部网络、不依赖真实模型**；HTTP 用 `httptest`，stdio MCP 用夹具进程，HTTP MCP 用本地测试服务器。前端 Vitest 只在 CI Node job 跑；`make check` 与 `scripts/check-acceptance.*` **不得**调用 Node / npm。十八个专属 check（`012-core-author-agent` / `012-core-resource-types` / `012-workspace-yaml-provider` / `012-workspace-skill` / `012-workspace-agent` / `012-storage-admin` / `012-app-resource-admin` / `012-app-agent-admin` / `012-app-mcp-supervisor` / `012-app-reconcile` / `012-app-notify-author` / `012-web-admin-agents` / `012-web-admin-resources` / `012-web-admin-notify-workspace` / `012-web-etag-routes` / `012-cli-author` / `012-e2e` / `012-race`）按 §4.4 的精确测试函数名展开落 stage；`required_tests` 族名**不得**写成通配符。覆盖率：`core` / `tool` / `memory` / `storage` ≥ 70%，其余 ≥ 50%，由既有 `make cover` 判卷（**不改脚本与阈值**）。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布产物 `CGO_ENABLED=0` 构建；`-race` 与发布构建**分属不同命令 / job**。MCP stdio 进程树回收按平台分片（Windows `taskkill /T`、Unix 进程组）；HTTP MCP 零进程。管理台面向现代常青浏览器；浏览器 E2E 不在自动门禁内。

**Project Type**: CLI + 服务单二进制，沿用 `assets/console/` 内嵌前端资产树（`go:embed`）。不新增 Go 顶层包；`internal/core` 增领域类型，`internal/workspace` 增文件与目录操作，`internal/storage` 增 SQL，`internal/app` 增管理编排与 supervisor，`internal/web` 增写面端点，`internal/cli` 增 `author:` 解析。

**Performance Goals**: 无吞吐 / 延迟指标（管理面类 feature）；判据是**行为预算**——归档等待上限 30 秒；模型连接与 MCP 测试各固定 15 秒；单次请求体受 `http.max_request_bytes` 限制；技能导入单文件 256 KiB、总量 `min(2 MiB, http.max_request_bytes)`、文件数 200；工作区浏览单文件 256 KiB（超限拒绝）；`write-after-read` 的页面读回不设缓存。

**Constraints**: **先审计后动作**——预写失败零写入，测试端点也不得先调用外部再补审计；**一道闸门**——发布写锁 / 准入读锁只认 `core.ResourceGate`，不得第二套互斥；**一处生命周期权威**——状态文件只经 `LifecyclePort` 写，DB 索引可重建；**固定锁顺序**——Agent 锁 → 资源锁 → 定义文件锁 → `ResourceGate` → supervisor 锁，不得反向；**零新增 Go 依赖**；**迁移**按 R4 取代为 1 个受控前向迁移（0003 → schema v3）；**契约**按已批准三项修订 + R4 列出的 12 个错误码 / 动作字面量与 schema v3（见 R4 节）；**脱敏 fail-closed**——凭证只回 `${ENV_VAR}` 引用名、错误只含固定短语与 scheme + host；**同步执行**——`context.WithValue` 零新增；**静态门禁**——抑制注释零使用；测试函数名英文、中文验收点以注释保留；格式只许 `gofumpt -w <本轮改动的文件>`。

**Scale/Scope**: **97 个 Go 文件**——新增生产 28 + 修改生产 21 = 49（模块卡 §3.1.1 列 20 个修改；`internal/app/baseline.go` 为约束 44 / FR-009「恢复扩展到新动作组」的派生新增修改，见差异表 B-3）；新增测试 28 文件 / 315 用例 + 修改测试 20 文件 / 新增 94 用例 = 48 文件 / 409 用例（实际条数以实施后实测为准）。另加前端增量 13 个源文件（4 views + 3 components + `api-admin.ts` + `router.ts` + 4 tests）与 4 条新路由、配置与文档 3 项（`basalt.yaml.example` / `docs/Operations.md` / `docs/GettingStarted.md`）、harness 工件 `harness/stages/us12-agent-resource-management.json`。R4 取代为零迁移 → 1 个受控前向迁移（0003），零新增 Go 坐标；R3 十八个 + R4 十四个专属 check（见 R4 节）；人工项为 §6.2 的九条 Demo 与 R4 的 Demo L–O。判据面变更见颗粒度文档 §3.5 与 run-state 的记录，既有断言零删除、零跳过、零放宽。

**环境实测读数（2026-10-22，本轮当场命令）**: 分支 `012-agent-resource-management`；起始 commit `383d89eba1a74c988bd0203fec7adef96df246ed`；`git rev-list --left-right --count main...HEAD` = `0 0`；工作区只有 `?? specs/012-agent-resource-management/`（本 US 工件），无用户未提交改动。`go version go1.27.3 windows/amd64`；本机 `node` 不存在（前端构建只走 CI）。`go.mod` 无任何待新增坐标。已核对：`SupportedSchemaVersion = 2` 在 [migrate.go:21](../../internal/storage/migrate.go#L21)；`MCPServerConfig` 只有 `Name` / `Command` / `Env` 在 [core/mcp.go:15](../../internal/core/mcp.go#L15)；`strings.Fields(spec.Command)` 在 [tool/mcp.go:205](../../internal/tool/mcp.go#L205)；`newProviderCatalog` 在 [app/providers.go:42](../../internal/app/providers.go#L42)、`newProviderCatalogWithOverride` 在 [app/providers.go:69](../../internal/app/providers.go#L69)，唯一生产调用方 [app/wire.go:445](../../internal/app/wire.go#L445)；`agentsPort` 在 [app/ports.go:82](../../internal/app/ports.go#L82)，其 `Create` / `Delete` 仍转调 [workspace/agent_files.go:52](../../internal/workspace/agent_files.go#L52) / [:120](../../internal/workspace/agent_files.go#L120)；`web.Options` 在 [web/server.go:26](../../internal/web/server.go#L26)（已有 `AdminAuth` / `Registry` / `Lifecycle` / `AgentAdmin` / `Operations` / `Resources` 等 US-11 端口）；`IMPORT_REJECTED` 尚未定义，其余错误码在 [web/envelope.go:36-51](../../internal/web/envelope.go#L36) 已有。`assets/console/src/router.ts` 现为十二条路径 + 404，无 `/console/workspace`。**本轮未跑** `make check`、覆盖率读数与真实进程 Demo；MCP HTTP、Windows 进程树、30 秒归档等待、对账幂等性与前端构建仍属 run-state 的未验证项 U1–U6。

### 文档 ↔ 仓库事实差异（实测发现）

**A. 与文档一致、作为落地基点的仓库事实**（逐条已核，不构成差异）：

| # | 文档写法 | 仓库实测 | 落地口径 |
| --- | --- | --- | --- |
| 1 | §3.1：零新增 Go 坐标 | `go.mod` 已含全部所需依赖，无待加项 | 本 US 不改 `go.mod` / `go.sum` |
| 2 | §3.4：零新增迁移，schema v2 | `migrations/` 只有 `0002_us11_us12_resources.sql`；`SupportedSchemaVersion = 2` | 不新增迁移；表结构零改动 |
| 3 | §3.1.1：`core.MCPServerConfig` 扩展 `Transport` / `Args` / `URL` | 当前仅 `Name` / `Command` / `Env` | 追加字段；既有字段语义不变 |
| 4 | §3.1.1：`tool/mcp.go` 删除 `strings.Fields` | 当前在 [mcp.go:205](../../internal/tool/mcp.go#L205) 按空白拆分 | 改为 `exec.Command(spec.Command, spec.Args...)` |
| 5 | §3.1.1：错误码只新增 `IMPORT_REJECTED` | [envelope.go:36-51](../../internal/web/envelope.go#L36) 已有其余全部错误码 | 只加一个常量；其余复用 |
| 6 | §3.1.1：`newProviderCatalogWithOverride` 替换为发布版本目录 | 函数在 [providers.go:69](../../internal/app/providers.go#L69)，唯一生产调用方 [wire.go:445](../../internal/app/wire.go#L445) | 替换并删除旧函数；保留评测注入语义 |
| 7 | §3.1.1：删除 `workspace.CreateAgent` / `DeleteAgent` | 两者在 [agent_files.go:52](../../internal/workspace/agent_files.go#L52) / [:120](../../internal/workspace/agent_files.go#L120)，调用方只有 `agentsPort` | 由 staged / archive 取代；CLI 改走 `AgentAdmin` |
| 8 | §3.1.10：新增前端视图 `views/workspace.ts` | [views/workspace.ts](../../assets/console/src/views/workspace.ts) 已被 US-11 的「工作区资源」页占用 | 新页面落 `views/workspace-browser.ts`；路由仍为 `/console/workspace`，文件计数不变 |
| 9 | §3.1.10：四条新路由 | `router.ts` 十二条路径 + 404，无冲突路径 | 追加 `agents/new`、`resources/:kind/:id`、`archived`、`workspace`，静态段先于动态段 |

**B. 描述性文本差异（不改文档，按人审输入实施）**：

| # | 差异 | 处理 |
| --- | --- | --- |
| 1 | 归档顺序：`openapi.yaml` `/agents/{name}` DELETE、`run-and-errors.md` 第 203 行与 `TechnicalDesign.md` §3.4.2 / §6.2.4 / §9 写「标记 `archived` → 移出注册 → 取消并等待」；R3.1 约束 32 写「移出注册 → 等待 → 迁移 `archived`」 | 按 R3.1（人工 review ① 的 B4 / D-08）实施；两者满足 `run-and-errors.md` §9.4 的 applied 判据、409 语义与最终状态一致。已记入 [spec.md](./spec.md) 的 `## Clarifications`，不在本 US 修改契约或技术方案 |
| 2 | 模块卡固定技术栈句写 Go 1.27.2；`go.mod` 是 `toolchain go1.27.3` | 以 `go.mod` 为唯一事实源；本 US 不改版本句（US-11 已按 2026-10-21 用户裁决登记） |
| 3 | 模块卡 §3.1.1 修改生产文件清单 20 个，未列 `internal/app/baseline.go`；约束 44 / FR-009 要求启动恢复覆盖新动作组 | 派生新增第 21 个修改生产文件（`verifyOperation` 分派扩展）；不改变已审范围语义，按已审约束实施并在 run-state 登记差异 |

实现期若出现新的签名漂移或第三处描述性冲突，按颗粒度文档「停止边界」口径停下登记，不静默自决。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 / 约束 | 本模块落地判据 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct 循环 | `go.mod` 零 Agent 框架坐标；本模块只加管理写面、资源发布与 supervisor，循环体不动 | 通过 |
| II LLM 客户端只做协议调用 / 单一消息仓储 | 作者草稿与模型连接测试都只调 `ProviderService.Chat` 协议入口；不自动执行工具；不新增消息追加路径 | 通过 |
| III Provider 显式映射 | `newProviderCatalogFromPublished` 每次受控发布构建不可变映射并原子替换；新 Run 准入固定；失败保旧 | 通过 |
| IV 一个目录 = 一个 Agent / 声明式绑定 | Agent 创建 / 复制 / 恢复都写 `AGENT.md` 且 `name` 等于目录名；技能只按 `skills:` 声明绑定；不引入软连接、不新增 `use_skill` | 通过 |
| V 审计 day-one 且 fail-closed | 写端点先 `pending`，预写失败零写入；测试端点先审计后调用；管理恢复只查证不重做 | 通过 |
| VI 授权与执行分离 | MCP stdio 经 `Policy.Check` + `CommandRunner`；HTTP 经请求授权；supervisor 不绕过两个端口 | 通过 |
| VII 同步执行 + context 纪律 | 归档等待与测试调用都是同步有界；`context.WithValue` 零新增；actor / 操作标识显式传参 | 通过 |
| VIII 单二进制纪律 | 前端资产 `go:embed`；`CGO_ENABLED=0` 与 `-race` 分列；零新增 Go 坐标 | 通过 |
| IX 不用 ORM / 只追加迁移 | 手写 SQL 集中在 `storage`；R4 新增 0003 前向迁移到 v3（受控、单事务、已发布 `0002_us11_us12_resources.sql` 零改写） | 通过（R4 取代「零迁移」） |
| X 范围卡紧 | 交付物与 §3.1 / §3.2 清单逐一对应；14 项边界不进入实现 | 通过 |
| 架构约束：依赖方向 | `core` 只依赖标准库；`workspace` / `storage` / `web` / `tool` → `core`；`app` 唯一装配层；`internal/cli` 只经 `cli.Deps.Agents` 端口，不 import `internal/app` | 通过 |
| 架构约束：接口定义在消费方 | `AgentAdminService` / `ResourceAdminService` / `NotifyAdminService` / `WorkspaceService` 声明在 `web`；`AgentAdmin` / `ResourceAdmin` 端口声明在 `core`；实现放 `app` / `workspace` / `storage` | 通过 |
| 架构约束：第三方实现放消费方包内或 `adapters` | MCP SDK 封装仍在 `tool` 与 `app` 的 supervisor；SQLite 仍在 `storage`；不新增 `adapters` 包 | 通过 |

**Post-Design Re-check**: Phase 1 设计（research / data-model / quickstart）完成后复核，结论不变；无违反项需要 Complexity Tracking。

## Project Structure

### Documentation (this feature)

```text
specs/012-agent-resource-management/
├── plan.md              # 本文件
├── spec.md              # 需求规格（含 2026-10-22 澄清会）
├── research.md          # Phase 0 输出
├── data-model.md        # Phase 1 输出
├── quickstart.md        # Phase 1 输出
├── checklists/
│   └── requirements.md  # 规格质量清单（16/16）
├── run-state.md         # 过程状态（非设计输入）
└── tasks.md             # Phase 2 输出（/speckit-tasks，不由本命令创建）
```

**不新建 `specs/012-agent-resource-management/contracts/`**：固定字面量的唯一来源是 [docs/contracts/](../../docs/contracts/)（`openapi.yaml` / `run-and-errors.md` / `agent-md.md` / `schema.sql`），复制一份必然漂移。

### Source Code (repository root)

```text
internal/
├── core/                                  # 领域类型与端口（只依赖标准库）
│   ├── author.go                          # 新增：作者配置 / 提示词 / 草稿解析
│   ├── agent_admin.go                     # 新增：Agent 写面类型与归档记录
│   ├── resource_admin.go                  # 新增：资源种类 / 版本 / 指针 / 发布 / 引用 / 测试结果
│   ├── errors.go                          # 修改：ErrConflict / ErrBusy / ErrImportRejected 与 data 载体
│   └── mcp.go                             # 修改：MCPServerConfig 扩展 Transport / Args / URL + PublishedMCPServer
├── workspace/                             # 文件系统实现
│   ├── yaml_block.go                      # 新增：顶层列表条目块定位 / 替换 / 追加 / 移除
│   ├── provider_file.go                   # 新增：providers 条目解析 / 渲染 / 原子写
│   ├── skill_admin.go                     # 新增：技能导入边界 / 工作副本 / 快照 / 归档
│   ├── agent_admin.go                     # 新增：staging 创建 / 复制 / 恢复 / 归档清单
│   ├── agent_draft.go                     # 新增：多文件候选树 / 完整副本 / 两次 rename / 集合 ETag
│   ├── notify_scan.go                     # 新增：活动与归档 AGENT.md 整词扫描
│   ├── agent.go                           # 修改：frontmatter name / identity 读写
│   ├── agent_files.go                     # 修改：名称规则收紧；删除 CreateAgent / DeleteAgent
│   ├── templates.go                       # 修改：模板接受 description / model
│   ├── skill.go                           # 修改：工作副本与不可变版本的路径映射
│   └── mcp.go                             # 修改：严格候选 + 旧多词 command 兼容拆分
├── storage/                               # 手写 SQL（沿用 v2 七表，零迁移）
│   ├── agent_admin_store.go               # 新增：归档插入 / 恢复标记 / 查询 / 列表
│   ├── notify_admin_store.go              # 新增：通知渠道 CRUD
│   └── resource_store.go                  # 修改：MarkDeleted；其余版本 / 指针 / 引用方法复用
├── app/                                   # 唯一装配层
│   ├── resource_admin.go                  # 新增：三类资源的写 / 发布 / 删除 / 测试编排
│   ├── agent_admin.go                     # 新增：创建 / 草稿 / 提交 / 复制 / 归档 / 恢复编排
│   ├── mcp_supervisor.go                  # 新增：MCP 连接生命周期 / 授权 / 应用版本 / 测试
│   ├── definition_reconcile.go            # 新增：启动定义对账
│   ├── notify_admin.go                    # 新增：通知渠道管理（占位 / 脱敏 / 引用扫描）
│   ├── author.go                          # 新增：作者配置校验与 Draft 调用
│   ├── workspace_browse.go                # 新增：只读工作区浏览
│   ├── baseline.go                        # 修改：verifyOperation 扩展到 US-12 动作组（约束 44 / FR-009）
│   ├── providers.go                       # 修改：newProviderCatalogFromPublished 替换 overrides 入口
│   ├── mcp.go                             # 修改：由 supervisor 接管连接
│   ├── notify.go                          # 修改：发送读取 DB 渠道
│   ├── run.go                             # 修改：启动顺序接入对账与 supervisor
│   ├── schedule.go                        # 修改：归档注销 / 恢复不自动启用
│   ├── wire.go                            # 修改：装配写面服务 / supervisor / 对账
│   ├── web.go                             # 修改：Server Options 增写面端口
│   └── ports.go                           # 修改：CLI 写路径的实例锁 / DB / 组件生命周期所有者
├── web/                                   # HTTP 与管理台
│   ├── handlers_admin_agents.go           # 新增：创建 / 草稿 / 提交 / GET files
│   ├── handlers_admin_archived.go         # 新增：归档 / 归档列表 / 恢复
│   ├── handlers_admin_resources.go        # 新增：资源写面 / 版本 / 发布 / 引用 / 版本详情
│   ├── handlers_admin_notify.go           # 新增：通知渠道写面
│   ├── handlers_admin_workspace.go        # 新增：工作区树 / 文件
│   ├── ports_admin.go                     # 新增：写面服务端口接口
│   ├── views_admin.go                     # 新增：写面视图与错误 data 投影
│   ├── etag.go                            # 新增：If-Match 解析与强 ETag 校验
│   ├── admin_routes.go                    # 新增：写面路由表与条件写中间件
│   ├── envelope.go                        # 修改：只新增 IMPORT_REJECTED
│   └── server.go                          # 修改：挂载写面路由与中间件
├── cli/
│   ├── author.go                          # 新增：author: 段解析
│   └── config.go                          # 修改：接入 author 段类型化解析
└── tool/
    └── mcp.go                             # 修改：Command + Args 分离，删除 strings.Fields

assets/console/src/
├── views/
│   ├── agents-new.ts                      # 新增：创建 + 草稿编辑 + 提交
│   ├── resource-detail.ts                 # 新增：版本 / 发布 / 撤回 / 删除 / 引用 / 测试
│   ├── archived-agents.ts                 # 新增：归档列表与恢复
│   └── workspace-browser.ts               # 新增：Agent 目录只读树与预览（文件名为适配 US-11 既有 workspace.ts）
├── components/
│   ├── agent-files.ts                     # 新增：多文件编辑与提交
│   ├── resource-form.ts                   # 新增：三类资源新建表单
│   └── notify-channels.ts                 # 新增：渠道 CRUD
├── api-admin.ts                           # 新增：写面端点 / If-Match / 错误映射
├── router.ts                              # 修改：追加 4 条路由（静态先于动态）
└── tests/                                 # 新增 4 个 Vitest 文件

basalt.yaml.example                       # 修改：author: 示例 + shell.allowed_commands 注释
docs/Operations.md                        # 修改：MCP 白名单 / 资源管理 / 归档恢复 / 工作区边界
docs/GettingStarted.md                    # 修改：管理 API 示例补 If-Match
harness/stages/us12-agent-resource-management.json  # 新增：本 US 专属 stage（含私有输入，只在本机执行）
```

**Structure Decision**: 保持单 Go module + `internal/` 分包。`core` 只放领域类型与端口（不 import 其他内部包）；`workspace` / `storage` 提供文件与 SQL 实现；`web` 只消费端口；`app` 唯一装配。前端继续独立成 `assets/console/`（构建期资产，非 Go 包），由既有 `assets/embed.go` 嵌入，不新增顶层包、不新增 `internal/adapters/` 包。`handlers_notify_read.go` 与既有通知读面保持原文件不动，写面另起 `handlers_admin_notify.go`。

## Complexity Tracking

无 Constitution Check 违反项，无需登记。

## Phase 0 / Phase 1 产物

- [research.md](./research.md)：关键技术决策与替代方案（文件 / 数据库权威、块编辑、ETag 定义、锁顺序、归档顺序、MCP supervisor、技能快照、CLI 共用服务、版本详情端点、零依赖零迁移）。
- [data-model.md](./data-model.md)：v2 七表 + 通知表的复用边界、领域实体、状态迁移与约束。
- [quickstart.md](./quickstart.md)：可执行的验证向导（门禁、十八个专属 check + R4 追加十四条、九条人工 Demo + R4 的 Demo L–O）。

## R4 范围修订的技术计划（2026-10-23 追加）

**触发与边界**：用户 2026-10-23 批准的三处修正与 Q1–Q5 把「结构化配置、资源范围、稳定身份、绑定权威、通知三态、托管凭据、迁移 0003」并入本 US。本节只追加计划，不改变上述 Phase 0 / Phase 1 产物对 R3 范围的描述；R3 已交付代码与证据不动。

### 技术上下文增量

- **数据**：`schema_version` 2 → 3，26 张表（18 既有 + 8 新增）；新增受控前向迁移 `internal/storage/migrations/0003_credentials_scope.sql`。
- **依赖**：零新增 Go 坐标（`crypto/aes` / `crypto/cipher` / `encoding/base64` 均属标准库）。
- **配置**：`basalt.yaml` 新增 `secrets.master_key_file`；`providers[].credential_mode` 显式。
- **职责落位**：密码学与按需解析在 `internal/app`；SQL 与状态字面量在 `internal/storage`；绑定块与状态文件 v2 在 `internal/workspace`；领域类型与端口在 `internal/core`；HTTP 与错误映射在 `internal/web`；`secrets` 子命令在 `internal/cli`；`internal/app` 仍是唯一装配层。

### 阶段划分（与模块卡 R4.11 的 B1–B8 一一对应）

1. **B1 领域类型**：`agent_uid` / `resource_uid` 生成与校验、范围与资格、绑定模型、凭据模式与状态机（纯函数，无 I/O）。
2. **B2 文件层**：`resource_bindings` 块解析 / 序列化 / 块编辑；生命周期状态文件 v1 → v2 升级（备份 + 原子写 + 重算 checksum）。
3. **B3 存储层**：8 张新表 SQL、`resource_uid` 回填、`resource_versions` / `notify_channels` 重建、`uid_source`；迁移编排（操作行 → 状态文件 → SQL → 绑定回填 → 守卫）。
4. **B4 应用层（无凭据）**：绑定投影重建与准入校验、范围转换、通知三态与冻结集合、通知发送明细。
5. **B5 应用层（凭据）**：AES-256-GCM 与 AAD、主密钥校验、按需解析、`CredentialGuard` 检查点、四证恢复、`secrets` CLI。
6. **B6 Web 层**：`GET /agents/{name}/config`、凭据撤销、范围端点、12 个错误码与保存 400 / 准入 409 分段。
7. **B7 前端**：结构化配置页、资源详情页改造、3 个组件、**1 条新路由**（`/console/agents/:name/config`；资源详情复用既有 `/console/resources/:kind/:id`，不并存同形 `:name` 路由）、write-only 凭据输入。
8. **B8 收口**：Demo L–O、Operations / Compatibility 增量（README 归 US-13，本 US 不改）、`make check` 七目标与 stage 全量。

### 关键实现决策

| 决策 | 选择 | 理由（替代方案与代价） |
| --- | --- | --- |
| 绑定权威 | `AGENT.md` 为源，DB 投影可重建 | 绑定属于 Agent 配置，必须与正文同一条件写；投影单独权威会出现两份真相（替代方案：DB 为源 → 手改文件无法表达） |
| 资源身份 | UID 不可变，名称是标签 | 同名重建若复用名称标识，旧引用会被静默接管（替代方案：名称 + 时间戳 → 无法稳定引用） |
| 通知模式 | 三态显式，空数组 = `all_eligible` | 空数组承担「禁用」与「不限」两义必然误读（替代方案：布尔开关 → 无法表达白名单） |
| 主密钥加载 | 按需读取，启动只判当前托管模型连接 | 启动期全量解密会把「只用了通知托管」的实例挡在门外；常驻缓存会放大泄漏面 |
| 撤销语义 | 按版本状态判定，不以身份是否 active 推断 | 撤销后重新录入会产生新版本；只看身份会把旧版本误判为可用 |
| 迁移顺序 | 状态文件 → SQL → 绑定回填 | 状态文件是 Agent 身份权威；SQL 事务可回滚，文件写入不可，必须先落定再回填 |
| ETag 组成 | 内容 + UID + revision + scope + 凭据状态 | 只换 Key 时内容字节不变，纯内容哈希无法发现并发替换 |

### 验证计划增量

- 新增 14 条专属 check（见模块卡 R4.12），既有 18 条不改判据；
- 迁移用合成 v2 库实测前向路径（真跑 0002 → 0003），并注入中断验证 `unknown` 拒绝启动；
- 泄漏哨兵用 golden 扫描 + 测试夹具，覆盖响应体 / 操作 detail / 日志 / 前端产物；
- 前端 `npm test` + 构建 + `integrity.json` 校验纳入 B7 判据。

### 风险与未验证

- 每次模型调用读取主密钥的性能影响**未测量**；
- SQLite `resource_versions` / `notify_channels` 重建在真实规模库上的耗时**未测量**（本地库规模小）；
- Windows DACL 的最小权限设置在 CI（Linux）无法覆盖，只在 Windows 本机实测登记；
- 迁移绑定回填依赖「定义最后保存时间」与操作台账的先后关系，历史台账缺失时一律不写绑定（偏保守）。

## R6 范围修订的技术计划（2026-10-24 追加）

### R6 技术上下文增量

- **依赖**：零新增 Go 坐标、零新增前端运行时依赖、零迁移；`cronadapter.Engine.Occurrences` 与 `workspace.CronSpec` 已在仓内，直接复用，不复制 cron 解析。
- **契约**：`docs/contracts/agent-md.md` 与 `openapi.yaml` 增量见模块卡 R6.5；`schema.sql` 与已发布迁移不变。
- **职责落位**：作者提示词 / 解析与候选结构在 `internal/core`；装载规则复用与显示名校验在 `internal/workspace`；模型调用、候选校验与触发点计算在 `internal/app`；HTTP 解码 / 视图与错误映射在 `internal/web`；页面在 `assets/console`。

### 阶段划分

1. **R6-a 显示名**：`Profile.DisplayName`、frontmatter DTO / 已知键 / 映射、`changes.display_name` 补丁与移除、配置字段与 `ProfileView` 投影、契约与前端展示。
2. **R6-b 问答式补全**：`core.AuthorQuestion` / `AuthorAnswer`、提示词分支、草稿解析双形态、`Draft` 透传答案、`answers` 解码与响应问题数组、页面问答卡。
3. **R6-c 定时候选**：`core.BuildScheduleDraftPrompt` / `ParseScheduleCandidate`、`workspace.ValidateScheduleCandidate`、`AuthorService.DraftSchedule`（`Occurrences`）、`schedule-draft` 端点与视图、定时页候选卡与确认保存。
4. **R6-d 走查与验收**：mock 分支校准、浏览器新增三步、服务端资源哈希核对、`make check` 与 stage 全量。

### R6 关键实现决策

| 决策 | 选择 | 理由（替代方案与代价） |
| --- | --- | --- |
| 显示名载体 | frontmatter 顶层 `display_name` | 与 `name` / `identity.agent_name` 语义分开；`changes` 可按字段条件写（替代方案：复用 `identity.agent_name` → 改显示名会改提示词自我称谓） |
| 显示名清空 | 空串 = 移除该行 | 与 `provider.temperature` 的三态同口径，避免「省略 = 清空」误读 |
| 问答形态 | 扩展既有 `generate-files` | 复用同一审计与错误映射；不新增第二套作者端点（替代方案：新端点 → 两套解码与错误面） |
| 问题位置 | 响应 `questions[]` 与 `files` 互斥 | 页面一次只处理一种结果；不存在「半份文件 + 问题」 |
| 候选计算 | app 内 `cronadapter.Engine.Occurrences` + `workspace.CronSpec` | 与调度器同一解析器与 `CRON_TZ=` 规则；不复制 cron 解析，也不让浏览器或模型推算 |
| 候选保存 | 复用 `schedules` 整块条件写 | 与页面编辑器、`manage_schedule` 同一道写入门 |

### R6 验证计划增量

- **Go**：workspace（显示名解析 / 补丁 / 候选校验）、core（提示词与解析上限）、app（问答透传、候选计算与错误分类）、web（请求解码、视图字段、契约字段集）。
- **前端**：Vitest 覆盖显示名回退、问答卡（必填 / 继续生成 / 两轮上限）、定时候选展示与确认保存、id 冲突阻止。
- **浏览器**：显示名写入与回退、问答两轮生成、定时候选 → 保存 → 读回 `schedules`。
- **门禁**：`make check` 七目标与 stage 18 条；R6 行为作为**新增断言**并入既有 check（只增不减，不改既有判据）。
- **未验证项**：真实飞书外发仍需用户注入进程环境变量；模型生成候选的质量不属自动判据（只判结构与解析）。

## R7 范围修订的技术计划（2026-10-24 追加）

### R7 技术上下文增量

- **依赖**：零新增 Go 坐标、零新增前端运行时依赖；新增迁移 `0004_schedule_delivery.sql` 到 schema v4（只追加列与表，不改已发布迁移）。
- **契约**：`docs/contracts/agent-md.md`（`schedules[].deliver_to`、§2.6 受控装配、§3.2 代投）、`openapi.yaml`（`deliver_to`、`delivery_*`、`last_test`、渠道测试、`changes`）、`schema.sql`、`run-and-errors.md`；宪法 v3.0.0 架构约束第 11 条。
- **职责落位**：投递端口与三值语义在 `internal/core`；代投服务、受控装配与保存时校验在 `internal/app`；权威投递记录与连接测试投影在 `internal/storage`；发送适配复用 `internal/tool`；视图与错误映射在 `internal/web`；会话详情与投递展示在 `assets/console`；初始化导入与重部署为 `scripts/*.ps1`。

### R7 关键实现决策

| 决策 | 选择 | 理由（替代方案与代价） |
| --- | --- | --- |
| 投递目标 | 渠道 `resource_uid`，非名称 | 同名重建不自动接管；保存与运行时都按身份校验（替代方案：名称 → 重建后错投给新资源） |
| 代投归属 | 调度器在运行成功后由系统发送 | 模型不调用 `notify` 也有结果；避免「模型是否记得调用」成为可靠性变量 |
| 防重复 | 配置目标即剔除模型 `notify` | 系统与模型各发一次是最危险的重复路径；授权层仍兜底 |
| 权威记录 | 先写 `schedule_deliveries` 再发送 | 崩溃后能回答「发没发」；未知不重发，避免重复外发 |
| 状态值 | `pending` / `sent` / `failed` / `unknown` / `skipped` | `unknown` 与 `failed` 分开；空 `delivery_status` 表示无记录，不冒充成功 |
| 受控装配 | 只增删 `notify`，变更回报 `changes` | 宪法唯一例外；其余工具名保持显式声明，页面可见 |
| 凭据导入 | 仓库外模板 + 现有管理 API | 复用加密与审计；保留 UID；AI 不接触明文 |
| 重部署 | 识别 → 优雅停止 → 换二进制 → 启动 → 校验 | 不手工重启；保留数据与主密钥；输出不含凭据 |

### R7 阶段划分

1. **R7-a 代投**：`core.ScheduleDelivery` 端口、调度器准入预写与成功结算、`app.deliveryService`、`storage` 记录与恢复、`tool` 发送适配、视图 `delivery_*`。
2. **R7-b 接管与装配**：`RunRequest.SuppressNotify`、`core/service.go` 兜底、`app.notify_assembly`、`changes.tools_added / tools_removed`、保存时 `deliver_to` 校验。
3. **R7-c 页面与文案**：会话详情路由、连接列表精简、记忆说明、渠道测试三值、定时任务投递列与执行 / 投递分列。
4. **R7-d 运维**：初始化导入脚本、重部署脚本、Operations 文档、8080 真实配置验收与浏览器走查。

### R7 验证计划增量

- **Go**：core（预写 / 冻结 / 接管 / 三值结算 / 未知不重发）、app（保存时目标校验、装配增删、连接测试投影）、storage（记录与恢复）、tool（发送适配）、web（视图字段与错误映射）。
- **前端**：Vitest 覆盖会话深链、连接列表列、记忆说明、渠道测试结论、投递状态分列。
- **浏览器**：会话详情返回 / 刷新 / 深链；定时任务配置投递 → 真实运行 → 执行与投递分列；连接测试成功与失败；一句话创建定时任务。
- **运维**：初始化导入幂等（重复导入不重复、尾号不同不覆盖）、重部署脚本（PID 识别、优雅停止、健康与资源哈希）。
- **门禁**：`make check` 七目标与 stage；R7 行为作为**新增断言**并入既有 check（只增不减）。
- **未验证项**：真实飞书外发、真实 DeepSeek 一句话创建依赖用户提供凭据；Windows 控制台优雅停止的行为边界按实测登记。

## R8 修复与范围修订的技术计划（2026-10-24 追加）

### R8 技术上下文增量

- **依赖**：零新增 Go 坐标、零新增前端运行时依赖；新增迁移 `0005_network_domain_policy.sql` 到 schema v5（只追加单行表，不改已发布迁移与既有表）。
- **契约**：`docs/contracts/run-and-errors.md`（§8 语言分工、§9.3 `network_allowlist.update` 动作 / `network_allowlist` 目标类型、§9.4 恢复行）、`openapi.yaml`（`GET /sandbox` 的 `network_allowlist` 元数据与 `PUT /sandbox/network-domains` 已在 R8 落盘）、`schema.sql`（`network_domain_policy`）。
- **职责落位**：策略类型、规整、ETag 与动作字面量在 `internal/core/network_policy.go`；SQLite 单行存储在 `internal/storage/network_policy_store.go`；热替换、预写审计、修订号 CAS、MCP 撤销与启动恢复在 `internal/app/network_allowlist.go` / `baseline.go`；策略实例与消费者在 `internal/tool/policy.go`、`httptools.go`、MCP http 传输；端点与生效视图在 `internal/web/handlers_sandbox.go` / `views.go`；中文映射在 `internal/web/errors_zh.go`；页面与轮询在 `assets/console`。

### R8 关键实现决策

| 决策 | 选择 | 理由（替代方案与代价） |
| --- | --- | --- |
| 白名单权威源 | SQLite `network_domain_policy` 单行 | 页面保存可持久化、可审计、可恢复；配置只在空表时作种子（替代方案：改 YAML + 重启，无法满足网页编辑与热生效） |
| 生效方式 | 同一 `Policy` 实例原子替换 + 生效快照原子替换 | 全部消费者共用实例，页面与判定不分叉；失败保留旧规则 |
| 并发修改 | 管理面锁 + DB `revision` CAS + `If-Match` ETag | 两道闸；412 零写入、409 零写入；页面重取后重试 |
| 恢复 | `before_revision` / `after_revision` / `candidate_sha256` 三态查证 | 只查证不重放；与内容写的恢复口径一致 |
| 中文文案 | 类型化 reason → 中文映射，日志模板英文 | 避免英文措辞变化导致文案失配；日志可检索；执行历史不回显原始错误 |
| 会话刷新 | 既有 `useLiveLoader` 轮询 + 可见性 / 焦点事件 | 不引入 WebSocket；去重、退避、单飞、定时器清理；未知结果只查询 |
| 时间列 | 新增「最近使用」展示字段（无运行 = 创建时间） | 不改 `last_active_at` 语义与身份规则；定时任务有来源标记 |
| 术语归一 | 仅展示层字符串 | 字段名、绑定、权限语义不变；temperature 0 不作确定性承诺 |

### R8 阶段划分

1. **R8-a 错误本地化**：`internal/web/errors_zh.go` 类型化映射、执行历史中文投影、既有中文日志清单；新增日志一律英文。
2. **R8-b 白名单**：core 策略类型 / 规整 / ETag、storage 单行存储与迁移 0005、app 预写审计与 CAS 热替换、MCP http 撤销与恢复、web 端点与生效视图。
3. **R8-c 页面**：执行环境白名单编辑器（412 重读与草稿保留）、会话自动刷新与「最近使用」列、术语归一、记忆说明、登录页身份说明。
4. **R8-d 文档与证据**：契约 / 技术方案 / 需求 / 运维 / 工程规范 / 示例配置 / 陷阱表；stage 增量断言；统一验收。

### R8 验证计划增量

- **Go**：core（规整 / ETag / 动作字面量）、storage（种子幂等 / CAS / 事务回读）、app（412 / 409 / 预写失败零写入 / 热替换 / MCP 撤销 / 三态恢复）、web（GET 生效视图 / PUT 认证与错误映射 / 中文映射）、tool（策略实例原子替换与消费者一致）。
- **前端**：Vitest 覆盖白名单编辑（保存、412 重读、失败保留输入、实际生效状态）、会话自动刷新（去重、退避、定时器清理、后台保留滚动与最后更新时间）、术语与记忆文案。
- **门禁**：`make check` 七目标与 stage；R8 行为作为**新增断言**并入既有 check（只增不减）。
- **未验证项**：真实浏览器长时间停留下的刷新体验；真实 MCP http 连接收紧后的断连观察；既有中文日志的收敛进度。
