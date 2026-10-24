# Phase 0 Research: US-12 Agent 与公共资源动态管理

**Date**: 2026-10-22 | **Spec**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md)

本文件记录实施前的技术决策、依据与已放弃的替代方案。输入为已人审模块卡 R3.1（§2 约束、§2.3 图 A–H 与伪代码）、`docs/TechnicalDesign.md`、`docs/contracts/`；凡与仓库现状有关的判断均以 2026-10-22 实测为准（读数见 [plan.md](./plan.md) 的 Technical Context）。

## 决策 1｜定义文件与数据库的权威分工

**Decision**: 定义文件（`agents/*/AGENT.md`、`basalt.yaml` 的 `providers:` / `mcp_servers:` 条目、技能工作副本）承载「期望内容」；`resource_versions` / `resource_pointers` 承载「已发布事实」；运行对象（Provider 目录、MCP 集合、技能绑定）**只**由已发布版本构建。手工改文件不直接生效，由启动对账归类为「文件新 → 待发布」而非静默覆盖。

**Rationale**: 单一运行权威让「发布了什么」可审计、可回滚；文件仍是人可读、可进 Git 的部署形态。两者分离后，发布动作天然成为「文件 → 版本 → 指针」的单向流水，失败保旧的语义才可判定。

**Alternatives considered**:

- 文件直接就是权威（每次运行读文件）：无法回答「上次发布的是什么」，版本账与撤回语义无从落地；与 US-11 已交付的 `resource_versions` 表冲突。
- 数据库为唯一权威（文件仅为导出物）：违背本仓「定义在文件」的部署习惯，也让离线编辑与 Git 审查失去意义。

## 决策 2｜YAML 块编辑按文本切片，不做整文件重序列化

**Decision**: 对 `basalt.yaml` 的 `providers:` / `mcp_servers:` 顶层列表，按条目起始位置定位块边界，**只替换目标条目字节**，块外逐字保留；前置条件不足（缩进不一致、锚点别名、流式写法、重复键等无法安全保留的情形）→ 400 `EDIT_UNSUPPORTED`，磁盘逐字节不变。注释丢失必须记录在操作详情里。

**Rationale**: `goccy/go-yaml` 的重序列化会丢注释、重排键、改写引号风格，导致每次发布产生与业务无关的巨量 diff；文本切片把「改动面」限制在一个块内，人审 diff 可读。拒绝不可安全保留的写法，比「尽量保留」更可判定。

**Alternatives considered**:

- 整文件解析后重序列化：注释与格式全丢，且无法满足「块外逐字保留」的验收。
- 用 AST 注释保留库：本仓依赖清单内无此能力，且新增坐标违反零新增依赖。

## 决策 3｜ETag 的字节定义

**Decision**: 资源 ETag = 当前发布版本 `content_hash`（十六进制小写 sha256）的强 ETag `"sha256:<hash>"`；技能的 `content_hash` 是完整文件清单（`[{path, sha256, size}]` 按 path 排序后规整序列化）的 sha256；Agent 多文件提交使用**受管文件集合** ETag（同样的清单规整哈希）。一律对**存储的字节**求值，不重新序列化后求值。

**Rationale**: 发布版本不可变，其哈希天然是稳定版本标识；重新序列化会因序列化器版本不同产生不同值，让 412 判定失去确定性。技能是目录不是单文件，清单哈希是唯一能覆盖全部文件的定义。

**Alternatives considered**:

- 用 `version_id` 作 ETag：可读性差且无法表达「内容相同但版本号不同」（撤回会制造新版本号，客户端期望的是内容比较语义）。
- 技能用工作副本目录 mtime：不安全且跨平台不可靠。

## 决策 4｜条件写与操作行的先后顺序

**Decision**: 顺序固定为 ① 路径 / 请求体 / `If-Match` 语法等纯请求校验 → ② 存在性与候选解析 → ③ 写 `pending` 操作行 → ④ 锁内重读并比较真实 ETag → ⑤ 执行 → ⑥ 同事务写终态。412 / 404 / 400 在 ③ 之前返回，**零操作行、零写入**；一旦 ③ 落库，后续任何失败都必须把该行置 `failed` / `unknown` 并回传操作标识。

**Rationale**: 「先留痕再动目标」是宪法原则 V 的本仓落地；把 412 放在操作行之后会产生大量「未发生写操作」的审计噪声，也让「操作行 = 一次执行尝试」的语义失真。

**Alternatives considered**:

- 所有响应都留操作行：审计表被徒劳请求灌满，且无法区分「尝试执行」与「请求非法」。
- 先比较 ETag 再写 pending：锁外比较存在 TOCTOU，必须在锁内重读，故真正判定只能发生在 ④；③ 之前只能做语法级校验。

## 决策 5｜并发控制：每资源锁 + 每定义文件提交锁 + 固定锁序

**Decision**: 两类锁——`per-resource` 写锁（同一 `kind+id` 串行）与 `per-definition-file` 提交锁（同一 YAML 文件串行）；Agent 侧另有 Agent 级读写锁。全局锁序固定为 **Agent 锁 → 资源锁 → 定义文件锁 → `core.ResourceGate` → supervisor 锁**，任何路径不得反向获取。

**Rationale**: 不同资源写不同文件时天然可并行（不同文件并行是明确的验收点）；同文件多资源必须串行才能保证块编辑与写入的一致性。固定锁序是避免死锁唯一可机器复核的手段（`012-race` 覆盖）。

**Alternatives considered**:

- 单一全局写锁：实现简单但扼杀不同文件并行，且把只读观测也拖进临界区。
- 无文件锁，靠重试：块编辑是读-改-写，无锁必然丢更新。

## 决策 6｜发布事务与失败补偿顺序

**Decision**: 发布 = 取资源锁 → 取文件锁 → 重读文件 → 分歧闸门 → 块编辑 / 原子写 → `ResourceGate` 写锁内**同事务**写版本行、指针、操作终态 → 释放后让运行时生效。任一步失败：先补偿文件（回写旧字节并校验身份），再写操作失败终态；**绝不允许先换指针后校验**。`pending_restart` 由已发布版本 ≠ 应用版本派生。

**Rationale**: 文件与数据库不可能同事务，唯一正确的顺序是「先把可回滚的外部介质改对，再提交数据库」。数据库事务是唯一的原子点；此前的失败用文件补偿，此后的失败只剩运行时刷新，可重试。

**Alternatives considered**:

- 先写数据库再改文件：数据库已提交但文件失败时，运行权威与文件永久分歧，且无法回滚已提交事务。
- 两阶段提交式临时表：SQLite 单库内事务已足够，额外机制属过度设计。

## 决策 7｜归档顺序采用 R3.1 口径（与契约描述性文本的差异）

**Decision**: 归档顺序 = 注销定时 → 移出注册索引 → 有界等待在途 Run（30 秒）→ 迁移生命周期状态到 `archived` → 目录移入归档区 → 写归档行 + 操作终态。超时 → 409 `AGENT_BUSY` 并回滚注册与定时注册，**状态文件与目录均未动**。补偿失败标 `unknown`。

**Rationale**: 契约 `openapi.yaml` / `run-and-errors.md` 第 203 行 / `TechnicalDesign.md` 的段落顺序写作「标记 archived → 移出注册 → 等待」，与 R3.1 的顺序描述不同；但 `run-and-errors.md` §9.4 的 applied 判据只要求「归档目录存在 + 清单哈希匹配 + 状态为 `archived`」，两种顺序在成功终态、409 语义与回滚结果上完全一致。R3.1 的顺序把「等待失败」置于状态迁移之前，可保证回滚只需撤销注册与定时两项，不存在「状态已 archived 但目录未移」的中间态需要回滚——更少的状态面，因此按人审输入实施。

**Alternatives considered**:

- 按契约字面顺序：会在等待超时时产生「状态已 archived 却仍在活动区」的中间态，回滚需同时撤销状态迁移，失败窗口更大。
- 修改契约使其与 R3.1 一致：契约已冻结，用户 2026-10-22 裁决「无需为落实已批准范围修改契约」，且 §9.4 判据不冲突。已在 [spec.md](./spec.md) `## Clarifications` 记录。

## 决策 8｜MCP supervisor 与命令执行器分离，授权按传输分支

**Decision**: `MCPSupervisor` 独占连接的建立、接管、回收与工具发现；`CommandRunner` 只负责单次命令执行。stdio 连接前经 `Policy.Check(ctx, ActionCommand)`，进程树回收按平台分片；http 连接前经 `Policy.Check(ctx, ActionHTTPRequest)` 与域名白名单，**零进程**。连接失败保留旧应用版本并标记未连接；`applied_version_id` 只在成功接管后写入。

**Rationale**: 契约原则 VI 要求授权与执行分离；把生命周期塞进执行器会让「一次失败的命令」与「长期持有的连接」共用一条资源路径，回收与审计都说不清。按传输分支授权是因为 stdio 的风险面是本地进程，http 的风险面是出站网络，用同一策略判定会要么过宽要么过窄。

**Alternatives considered**:

- 全部走 `CommandRunner`（http 包装成子进程）：多一次进程且绕过域名白名单语义。
- 全部走 HTTP 请求授权（stdio 视为本地请求）：丢失命令白名单这道最关键的防线。

## 决策 9｜技能：工作副本 + 不可变版本快照，全量替换

**Decision**: 技能工作副本位于可写区；每次发布把**完整文件清单**复制到不可变版本目录（`storage_path` 指向它），发布 = 全量构建 staging 目录 → 两次 rename 交换。更新是全量替换而非增量补丁。导入边界（越界路径、绝对路径、隐藏目录、软链接、非普通文件、非 UTF-8、大小 / 文件数、大小写重复）在进入工作副本**之前**拒绝；校验端点复用同一函数且不落盘、不写操作行。

**Rationale**: 「不可变版本」是撤回与历史可查的前提；全量替换让磁盘状态与清单哈希一一对应，避免增量更新留下陈旧文件导致哈希漂移。两次 rename 提供崩溃安全（要么旧目录要么新目录，不存在半套）。

**Alternatives considered**:

- 版本目录存增量补丁：回放复杂且哈希无法验证。
- 原地更新工作副本并另存快照：更新中途崩溃会污染工作副本，且发布失败无法退回。

## 决策 10｜CLI `profile create` / `profile delete` 共用同一管理服务

**Decision**: `profile create` / `profile delete` 走与 HTTP 相同的一条路径：取一次实例锁 → 打开并迁移数据库 → 装配必要管理组件 → 调用 `AgentAdmin`（创建 / 归档）→ 关闭组件并释放锁。操作人记 `cli:<user>`、`auth_method=cli`；写真实 `management_operations` 与 `agent_archives` 行；不产生 `cli-op-*` 伪标识，不提供无数据库路径，删除不做纯目录删除。`internal/cli` 只经 `cli.Deps.Agents` 端口调用，**不得** import `internal/app`。

**Rationale**: 目录操作必须与状态文件、审计、归档记录一致，否则 CLI 创建的 Agent 会缺少生命周期状态、CLI 删除的 Agent 会缺少归档行（Demo H 与 SC-014 直接判此）。实例锁保证 CLI 与服务进程不会同时改同一工作区。

**Alternatives considered**:

- CLI 直接调 `workspace.CreateAgent` / `DeleteAgent`：绕过审计与归档（模块卡 D-15 明确删除这两个函数）。
- CLI 走本地 HTTP 调用服务：要求服务在跑，违背 CLI 独立可用；且需要处理认证令牌分发。

## 决策 11｜交付 `GET /resource-versions/{version_id}`（H2）

**Decision**: 该端点由本 US 交付：路由、认证、服务调用、视图投影、脱敏与 HTTP 测试都在本 US 落地；存储层 `VersionByID` 复用 US-11 的实现，但**不以存储测试替代端点交付**。响应只回可公开字段：定义内容对模型连接 / MCP 脱敏（凭证只回 `${ENV_VAR}` 引用名），技能回文件清单与内容（受大小上限约束）。

**Rationale**: 契约列出该端点但 US-11 未交付（实测确认）；版本详情是「撤回」交互的必要读面，缺它则前端无法展示历史版本内容。脱敏 fail-closed 是宪法 V 的要求。

**Alternatives considered**:

- 由前端从版本列表推断：列表不含内容，无法支撑撤回预览。
- 后置到其他 US：契约端点已存在而无人交付，属交付缺口。

## 决策 12｜零新增 Go 依赖、零迁移、前端零新增运行时依赖

**Decision**: `go.mod` / `go.sum` 零变更；复用 US-11 的 schema v2 七表（`management_operations`、`resource_versions`、`resource_pointers`、`run_resource_manifests`、`run_resource_refs`、`agent_lifecycle`、`agent_archives`）与既有 `notify_channels`；前端只用既有 Vue 3 + Vite + TypeScript + Vitest 与自实现路由，不新增任何运行时依赖。

**Rationale**: 宪法原则 VIII 的依赖最小化；v2 表结构在 US-11 已按本 US 需求设计并冻结（迁移文件名为 `0002_us11_us12_resources.sql`，本 US 只是填满写入方）。前端零新增依赖保持内嵌资产的构建可复现。

**Alternatives considered**:

- 引入前端状态库 / 表单库：写面只有四个页面，自实现读取与错误分支足够；新增依赖要过版本核实与批准流程。
- 新增管理专用表：v2 已覆盖全部所需语义，新增表属重复建模。

## 决策 13｜Provider 映射只从已发布版本构建

**Decision**: 以 `newProviderCatalogFromPublished` 取代 `newProviderCatalogWithOverride`（[providers.go:69](../../internal/app/providers.go#L69)，唯一生产调用方 [wire.go:445](../../internal/app/wire.go#L445)）：每次受控发布后构建**不可变**映射并原子替换；键取自已发布版本条目自身的 `name`，不遍历推断、不取唯一值当默认；构建失败保留旧表。评测注入保留 `Options.ProviderClient`：对**每个已发布名**建立替身，不构造、不请求真实客户端，替身调用仍走既有审计与 Run 资源固定。

**Rationale**: 宪法原则 III 要求显式映射，且 Go map 遍历顺序随机，任何「取第一个」都是不可复现的缺陷。发布后原子替换保证 Run 内路由稳定（准入时固定）。

**Alternatives considered**:

- 运行时按需构造客户端：无法保证「发布失败保旧」，也无法在准入时固定版本。
- 保留 override 入口叠加在发布目录之上：两条真值路径，评测与生产行为不一致。

## 决策 14｜启动对账：文件与已发布版本的四种决策

**Decision**: 启动时逐资源比较定义文件与已发布版本，归类为 一致 / 文件新 / 版本新 / 文件无效：一致 → 无动作；文件新 → 记录待发布（不自动发布）；版本新 → 以已发布版本回写文件（受块编辑前置条件约束，失败则记录并保留文件）；文件无效 → 保留已发布版本运行，记录可见告警。对账 actor 记 `local_trust` / `migration` 语义的操作人，操作行保证幂等（同一差异重复对账不重复落行）。

**Rationale**: 「不得静默覆盖」是模块卡约束 34；四种决策覆盖了全部比较结果且每种都有明确的可观察后果。回写文件让「文件=已发布」的自描述性恢复，但必须受块编辑安全前提约束。

**Alternatives considered**:

- 启动即按文件重新发布：把「手工编辑」变成隐式发布，违背审计语义。
- 文件新时直接拒绝启动：可用性代价过大，且与「发布失败保旧」的容错取向不一致。

## 待澄清项

无。所有 NEEDS CLARIFICATION 已由模块卡 R3.1、契约与技术方案裁决；与契约描述性文本的唯一差异（归档顺序）见决策 7。

## 文档内一致性提示（不改冻结输入）

- §4.2 第 R30 行的 check 列写作 `012-app-notify-author`、`012-workspace-notify`；§4.4 的十八条权威清单中**没有** `012-workspace-notify`，`TestNotifyScan_*` 归 `012-workspace-skill`。本 US 以 §4.4 为 check ID 唯一枚举，stage 只落十八条；R30 的第二个 ID 视为笔误，不影响验收语义。
- 模块卡 §3.1.10 的新增视图文件名 `views/workspace.ts` 与 US-11 既有文件同名（实测冲突）。本 US 落 `views/workspace-browser.ts`，路由仍为 `/console/workspace`，文件计数不变；见 [plan.md](./plan.md) 差异表 B-8。
