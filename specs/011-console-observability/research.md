# Research: US-11 管理台与运行观测

**Branch**: `011-console-observability` | **Date**: 2026-10-21 | **Sources**: 颗粒度文档 R3 §2 / §3 / §4、`docs/contracts/`、`docs/TechnicalDesign.md` §3.4 / §3.6 / §5.2 / §6.2 / §7.1.2、宪法 2.0.0

本文件只记录**已裁决的技术决策**及其替代方案；不引入新交付物、不改固定字面量。所有决策都能指回模块卡约束编号或契约段落。

## D-01 文件监听：fsnotify v1.10.1（已批准）

- **决定**：热加载用 `github.com/fsnotify/fsnotify v1.10.1`，封装在 `internal/workspace/watcher.go`（消费方包内）；只非递归监听已添加目录，新建目录补监听。
- **理由**：模块卡约束 28 与 TechnicalDesign §1.3 已列入；2026-10-21 经 `GOPROXY=https://goproxy.cn go list -m -versions github.com/fsnotify/fsnotify` 实测存在。纯 Go、`CGO_ENABLED=0` 可用。
- **替代方案**：定时轮询（实现简单但延迟不可控、扫描成本随目录增长）；`fsnotify` 递归监听（跨平台语义不一致，且会触碰受保护控制目录）。均被否决。
- **限制**：监听事件可能重复或合并——用防抖 + 全量 rescan 吸收；目录被删除后重建需重建监听（记为失败路径，保留旧注册表并 WARN）。

## D-02 发布与准入的唯一闸门

- **决定**：进程内一个 `core.ResourceGate`（`sync.RWMutex`），由 `app` 创建并同时交给 Run 准入（读锁）与 US-12 的资源发布（写锁）。
- **理由**：模块卡约束 46；避免"清单记录新版本、实际客户端仍是旧版本"的裂缝；单实例由工作区实例锁保证，无需分布式协调。
- **替代方案**：每资源一把锁（发布跨资源组合时出现部分可见）；数据库行锁（SQLite 单写者，无法覆盖内存运行时快照替换）。均被否决。

## D-03 生命周期：状态文件权威 + 数据库可重建索引

- **决定**：`.basalt/control/agents/<agent>.json` 是唯一权威；`agent_lifecycle` 只是可重建索引。准入在 Agent 级读锁内重读状态文件并校验校验和；缺失 / 损坏 → 409 `AGENT_UNAVAILABLE`。
- **理由**：模块卡约束 13–17 与契约 `schema.sql` 注释；文件可被外部审计，数据库损坏时可重建；fail-closed 满足企业场景。
- **替代方案**：数据库为权威（重启 / 备份 / 迁移时与工作区不同步，运维无法直接核对）；内存状态（重启丢失）。均被否决。
- **限制**：写入必须原子（临时文件 + rename），并同步索引；状态文件被手动改坏时拒绝新运行而非自动修复。

## D-04 内容条件写：强 ETag + If-Match + 行级补丁

- **决定**：ETag 为原始 UTF-8 字节的 `"sha256:<64 位小写十六进制>"`；`PUT` 必须带 `If-Match`，缺失 / 弱比较 / `*` / 不匹配 → 412 且磁盘零写入；补丁只在 frontmatter 的已知字段上做行级替换，未改行逐字保留；无法安全保留 → 400 `EDIT_UNSUPPORTED`。
- **理由**：模块卡约束 25–27 与 `openapi.yaml` `IfMatch` pattern；保留注释与行尾风格是运维手写配置的基本预期。写入与热加载共用同一道校验与发布闸门。
- **替代方案**：mtime / 版本号（精度与时钟依赖不可靠）；整份 YAML 重新序列化（丢注释与顺序）。均被否决。

## D-05 运行资源账：单事务写入 + 终态同事务释放

- **决定**：准入固定资源后，`runs` + `run_resource_manifests` + `run_resource_refs` 在一个事务内写入；终态 `FinishRun` 在同一事务内写 `released_at`，且只更新仍为 `running` 的行。
- **理由**：模块卡约束 18–20；崩溃窗口不留半写；引用计数由未释放行天然表达。历史运行不补造，API 标 `legacy_unrecorded`。
- **替代方案**：先写 Run、后补清单（崩溃留下无账运行）；引用表用计数列（并发与恢复难以判定）。均被否决。

## D-06 取消：信号 + 有界等待，不写终态

- **决定**：取消只对进程内在跑的运行发 `context` 取消并最多等待 30 秒；终态由运行自身写入；响应 200 + 当前运行视图。
- **理由**：`run-and-errors.md` §4（取消优先于预算判定；审计与取消解耦）；强行写终态会与循环收尾竞争并破坏审计。
- **替代方案**：管理员直接 UPDATE 终态（破坏单一写入路径）；无限等待（请求挂死）。均被否决。

## D-07 管理台：Vue 3 + Vite，产物入库并嵌入

- **决定**：`assets/console/` 为独立前端工程；`dist/` 与 `integrity.json` 随源码提交；`assets/embed.go` 用 `go:embed` 嵌入；Go 测试校验集合与逐文件哈希；CI 单独 Node job 跑 `npm ci` / `npm test` / `npm run build` 并校验 `git status --porcelain -- assets/console/dist` 为空。
- **理由**：模块卡约束 40–43；单二进制交付要求运行时不读磁盘前端文件；`make check` 不得依赖 Node，因此把构建一致性拆到 CI job，Go 侧用哈希守住"提交的产物即被嵌入的产物"。
- **替代方案**：服务端模板渲染（交互能力不足）；CDN 托管（违背自托管与离线要求）；`go generate` 在构建时跑 Node（污染 Go 门禁）。均被否决。

## D-08 前端路由与渲染：零第三方运行时

- **决定**：History API 自实现十二条固定路由（2026-10-22 UI-R2 追加扩展，原八条的审阅记录保留于 run-state）；模型文本用固定标签子集渲染器（不用 `v-html` / `innerHTML`）；不引入路由库与 Markdown 库；token 只在内存。
- **理由**：模块卡约束 40–41；攻击面最小化（XSS / 凭证持久化）；深链接刷新由 Go 侧 SPA 回退兜底。
- **替代方案**：`vue-router` + `marked`（额外运行时、需审计供应链）；`v-html` + 消毒库（依赖第三方消毒器）。均被否决。

## D-09 采样：平台分片 + 显式不可用

- **决定**：`sampler.go` 定义接口与 5 秒缓存；Windows 用 `GetProcessTimes` / `psapi` 工作集，Linux 读 `/proc/self/stat` 与 `/proc/self/statm`，其他平台返回 `null` 并把字段列入 `unavailable`；首次 CPU 无差值基线同样为 `null`。
- **理由**：模块卡约束 33–34；`golang.org/x/sys v0.48.0` 已是直接依赖，不新增坐标；不伪造 0 读数。
- **替代方案**：统一用第三方 `gopsutil`（新增运行时坐标，超出批准范围）；返回 0 占位（误导运维）。均被否决。
- **待核实**：Windows API 符号与结构体在实施首日按 H3 规则在本地 `x/sys/windows` 与 `psapi` 绑定源码中核实；核实不到则按「停止边界」登记并改用 `null` + `unavailable`（不改变对外契约）。

## D-10 管理操作审计与运行审计分离

- **决定**：`management_operations` 独立于模型 / 工具调用审计；管理面高权限变更先写 `pending`；启动恢复按动作查证并写 `verification`，无法确定标 `unknown`。
- **理由**：宪法原则 V 与模块卡约束 10–12；两类审计的重试与恢复语义不同（管理操作是"查证结果"，调用审计是"补齐半截工具组"）。
- **替代方案**：复用运行审计表（字段语义冲突、查询混杂）；只写日志（不可核对、不可恢复）。均被否决。

## D-11 资源基线可重入

- **决定**：迁移后、开放服务前执行基线：先写 `system.baseline_v2` 的 `pending`；按 `content_hash` 识别已完成资源并跳过；既有 Agent 建状态文件为 `disabled` / `generation=1`；任一失败拒绝启动。
- **理由**：契约 `schema.sql` 增量迁移 v2 段；跨文件 / 数据库两介质无法单事务，只能"查证后写"；重复启动收敛到同一结果。
- **替代方案**：首次启动不建基线、按需惰性建（首个运行前的窗口不可控）；把基线写进 SQL 迁移（无法创建文件与快照）。均被否决。

## D-12 前端依赖版本锁定

- **决定**：实施首日按 npm registry 实测确定 Vue / Vite / TypeScript / Vitest 版本并写入 `package-lock.json`；`.nvmrc` 固定 Node 主版本；CI 用 `npm ci` 而非 `npm install`。
- **理由**：本机无 Node，无法预锁；模块卡 §1 已要求"实施首日按 npm registry 实测锁定并逐项写入实施报告"。`git status --porcelain -- assets/console/dist` 确保产物与源码同版本。
- **替代方案**：`package.json` 用 `latest`（不可复现）；把版本写死在本文件（未经 registry 核实，违反事实核实纪律）。均被否决。
