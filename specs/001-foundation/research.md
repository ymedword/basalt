# Phase 0 研究：US-1 工程地基

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-15

**结论先行**：Technical Context 无 `NEEDS CLARIFICATION`——全部决策已在四份权威文档与颗粒度文档第三稿中定案。本文件的职责是**把「怎么做」的关键决策与「写前必须实测核实」的项逐条固化**，供 implement 阶段逐项消化（module-dev 步骤 6 的 H3 门禁：第三方 API 在本地依赖核实不到 → 软门禁停下）。

## 决策记录

### D1 SQLite 驱动与其 DSN

- **Decision**: `modernc.org/sqlite` v1.60.1，`import _` 注册驱动名 `"sqlite"`；连接级 PRAGMA 经 DSN 参数 `?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)` 逐物理连接生效。
- **Rationale**: 纯 Go 驱动是 `CGO_ENABLED=0` 单二进制纪律（宪法 VIII）的硬前提；PRAGMA 的连接级语义决定了只能走 DSN（编程指南 §7 陷阱 38 / 契约迁移纪律 6）。
- **Alternatives considered**: `mattn/go-sqlite3`（需 CGO，与发布构建冲突）；启动后 `db.Exec("PRAGMA ...")`（只影响池中某一条连接——正是陷阱 38 的经典坑）。
- **待实测**：`_pragma` 参数语法与转义形态以本地依赖驱动源码 / `go doc` 实测为准（下「核实清单」#1）。

### D2 写权获取与判态顺序

- **Decision**: 打开后立即在**同一事务**内取得写权（`BEGIN IMMEDIATE`）并**重判**库状态，分支动作（创建 / 空事务提交 / 拒绝）在该事务内完成。
- **Rationale**: 约束 16——判态不得早于写权；文件存在性与 `IF NOT EXISTS` 都不能替代取得写权后的重判，否则两个 `init` 并发或「已持锁但初始化事务未提交」的启动窗口会出现双创建者。
- **Alternatives considered**: 先 `os.Stat` 判存在再决定（无写权保证，双创建者窗口）；只靠 `IF NOT EXISTS`（把「谁创建」的责任交给 SQL，`created` 语义无法判定）。
- **待实测**：`BEGIN IMMEDIATE` 的取得姿势——显式语句（专用 `*sql.Conn` / `Tx` 包装）或驱动 DSN 的事务锁参数；两法都要验证失败回滚后连接可用（下「核实清单」#2）。

### D3 `schema.sql` 的嵌入与执行

- **Decision**: 契约文件 [docs/contracts/schema.sql](../../docs/contracts/schema.sql) 的副本放 `internal/storage/schema.sql`，经 `go:embed` 载入；`TestSchemaFile_MatchesContract` 逐字节比对两文件。
- **Rationale**: 契约字面量唯一源（约束 12、编程指南 §8.3 第 5 项）——实现副本与契约漂移是「第二份字面量」的典型来源；逐字节单测把它变成可执行判据。
- **Alternatives considered**: 直接从 `docs/` 读文件（发布产物不携带 `docs/`，二进制自包含失效）；手抄列清单进 Go 代码（正是要防的漂移）。
- **待实测**：多语句脚本的执行与切分方式（下「核实清单」#3）；**回滚断言与切分实现无关**（夹具失败点由合成迁移集构造，不依赖契约文件本身）。

### D4 读写连接分工

- **Decision**: 两个 `*sql.DB` 句柄（同一 DSN）：写池 `SetMaxOpenConns(1)`（SQLite 单写者），读池默认并发；WAL 在事务外执行一次（库级持久）。
- **Rationale**: WAL 允许读写并发，但写仍须串行；写池上限 1 让「写事务排队」在 Go 侧就显式化，避免驱动层 `SQLITE_BUSY` 抖动。
- **Alternatives considered**: 单池 `MaxOpenConns(1)`（读也被串行化）；单池无上限（写并发直接撞锁）。

### D5 配置装载与 `${ENV_VAR}` 解析

- **Decision**: `cli.LoadConfig(path, lookupEnv func(string) (string, bool))`；缺省 `os.LookupEnv`；四类不安全输入 fail-closed 拒绝并点名配置键路径（占位变量不存在 / 凭证键解析为空 / 替换后仍含 `${` / 凭证明文），错误文案与日志不回显凭证值。
- **Rationale**: 需求 §5.13 + Operations §6 + 约束 7；`lookupEnv` 注入让「变量不存在」与「存在但为空」成为可测的两条路径，也让测试无需污染进程环境。
- **Alternatives considered**: `os.Getenv`（空串既表示不存在也表示空值——两条拒绝路径合并，测试不可分辨）；`os.ExpandEnv`（不报缺失）。
- **位置口径**: 实例配置 `basalt.yaml` 与工作区**同级**（不在 `.basalt/` 内）；`basalt init` **不生成**实例配置，仓库只带 `basalt.yaml.example`（§2.4 取数澄清）。

### D6 只读诊断的工作区路径

- **Decision**: `basalt status` 的唯一工作区路径 = **当前工作目录**的 `.basalt/`，不从配置取。
- **Rationale**: Operations §1.1 / §5.3 与需求 §5.1 同向；一份报告的三节必须出自**同一路径**，否则可能检查两个工作区。配置覆盖键属「待决事项」#1，随 US-9 装配面复核（已记入 spec Assumptions）。
- **Alternatives considered**: 从配置读工作区路径（引入未裁决配置键 + 报告内部路径不一致风险）。

### D7 迁移机制的注入面

- **Decision**: `Migrate(ctx, db *sql.DB, fsys fs.FS, target int) error`——文件集经 `fs.FS` 注入、目标版本显式传参；新库全量、旧库逐版前向（每版 SQL + 版本记录同一事务）；本 US 唯一生产调用方归 US-4 持锁重路径（本 US 交付能力与测试覆盖，不接通命令）。
- **Rationale**: 显式 `target` 让单版本期（`SupportedSchemaVersion = 1`）下不可达的分支（旧库 / 损坏态）可用合成值覆盖；`fs.FS` 注入让合成迁移集（1→2→3）进测试而不进仓库。
- **Alternatives considered**: 生产调用方本 US 接通（越过 US-4 的持锁顺序决策）；从磁盘固定目录读迁移（合成集无法注入）。

### D8 tzdata 内嵌与隔离分层

- **Decision**: `cmd/basalt` 经 `import _ "time/tzdata"` 内嵌 IANA 库；默认层只做可移植加载检查（不判隔离）；隔离组 `//go:build tzisolated` 由 CI 必跑容器步骤执行，对照探针**必须以「时区加载失败」（进程退出码 3 + `TZ_LOAD_FAILED` 标记）**失败。
- **Rationale**: 平台时区源（Linux `/usr/share/zoneinfo`、Windows 注册表）无法用环境变量遮蔽——把对照放默认层会让正确实现在装有系统 tzdata 的机器上判红（三稿 P2 修订）。对照的**失败原因**必须唯一，否则「启动失败 / 崩溃」会被误判成「隔离成立」。
- **Alternatives considered**: 默认层正反对照（本机必假红）；只跑主组不跑对照（无法证明内嵌真的在起作用）。
- **待实测**：各平台查找顺序（含 Windows 注册表是否为 `LoadLocation` 候选源）、探针现场构建与调用姿势、CI 镜像内路径（下「核实清单」#4 / #5 / #6）。

### D9 覆盖率判卷脚本的输入口径

- **Decision**: `scripts/check-coverage.sh` 解析 `go test -coverprofile` 的每包覆盖结果，配合 `scripts/stmtcount`（标准库 `go/parser` 统计可执行语句数）判定「有语句但无测试文件」的包；阈值按包名判定（`core` / `tool` / `memory` / `storage` ≥ 70%，其余 ≥ 50%）。
- **Rationale**: `go test -cover` 只打印百分比、不会因低于阈值失败——必须有可执行判据（宪法·开发工作流）；`no test files` 的包在 coverprofile 里没有记录，只有「有无可执行语句」的独立判定才能落实「计 0% 不得豁免」。
- **Alternatives considered**: 解析 `go test -cover ./...` 的文本输出（格式脆弱）；`go tool cover -func` 汇总（同样缺「无测试文件包」的信息）。

### D10 版本注入与退出码

- **Decision**: 版本经 `-X main.version=$(git describe --tags --always)` 注入（Makefile 内写 `$$(...)`）；进程退出码成功 0 / 失败 1，原因由文案区分；退出码唯一出口在 `app.Run`。
- **Rationale**: 技术方案 §10.2 构建命令 + 2026-10-15 裁决（0 / 1 两档）；`$$(...)` 是 Make 配方内 shell 展开的必需转义（否则被 Make 当变量展开为空）。
- **Alternatives considered**: 多档退出码（2026-10-15 已裁决不取）；`runtime/debug.ReadBuildInfo` 读版本（本地 `go build` 无 VCS 版本信息时为空）。
- **待实测**: cobra `--version` 的实际输出格式（下「核实清单」#7）。

## 写前必须实测核实清单（implement 阶段 H3 门禁）

| # | 待核实项 | 复核手段 | 核实不到时的动作 |
| --- | --- | --- | --- |
| 1 | modernc DSN 的 `_pragma` 参数语法 | 本地依赖内 `go doc modernc.org/sqlite` / 驱动源码；写一个最小连接读回 `PRAGMA foreign_keys` / `busy_timeout` | 软门禁：停下报告（改形态需同步改颗粒度文档 §3 与契约纪律 6） |
| 2 | `BEGIN IMMEDIATE` 的取得姿势（显式语句 vs 驱动事务参数） | 同上，实测取得写权 + 回滚后连接可用 | 软门禁：停下报告 |
| 3 | 多语句脚本的执行 / 切分方式（`schema.sql` 与合成迁移集） | 最小复现：整脚本 `Exec` 是否可行；不可行则按 `;` 切分（注意字符串字面量内的分号） | 软门禁：停下报告 |
| 4 | tzdata 各平台时区源查找顺序（含 Windows 注册表） | 本机实测 `time.LoadLocation("Asia/Shanghai")` + 标准库 `time` 包实现走查 | 记录读数；（默认层不判隔离，不影响判卷） |
| 5 | 对照探针的现场构建与调用姿势（exit 3 + `TZ_LOAD_FAILED`） | 本机跑一次隔离组（Windows 下预期主组与对照均需真实隔离环境，只验证构建与调用姿势） | 软门禁：停下报告 |
| 6 | CI 隔离容器步骤的镜像内路径（`/usr/share/zoneinfo`、GOROOT `zoneinfo.zip`、容器内模块代理） | 本地 `docker run golang:1.27.2` 实测删除 + 断言 + 跑隔离组（**需要 Docker**） | 记录为「CI 未实测」并明说（本机无 Docker 时不得宣称 CI 判卷通过） |
| 7 | cobra `--version` 输出格式 | 最小复现：设置 `Version` 后跑 `--version` | 记录读数（断言写「包含注入值」即可，不锁死格式） |
| 8 | `gosec` 已安装版本 | `gosec -version` 完整输出 / `go version -m $(command -v gosec)` | 与 v2.29.0 不一致 → 软门禁：停下报告 |

### 核实读数（T001 实测回填，2026-10-15；探针建于仓库外 `D:\code\probe-t001-basalt`，用后删除）

| # | 读数 | 结论 |
| --- | --- | --- |
| 1 | `sql.Open("sqlite", "file:x.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")` → 读回 `busy_timeout=5000`、`foreign_keys=1` | **语法成立**——DSN 形态按颗粒度文档 §3 落地，不切分、不改形态 |
| 2 | `conn.ExecContext(ctx, "BEGIN IMMEDIATE")` 取写权 → 另连接再 `BEGIN IMMEDIATE` 得 `database is locked (5) (SQLITE_BUSY)`；释放后被阻塞方（受 `busy_timeout` 约束）可取得 | **显式语句姿势成立**——`BEGIN IMMEDIATE` 经 `*sql.Conn` 显式执行（走 `Conn` 而非 `Tx`，因需在事务内重判态后自行 `COMMIT`/`ROLLBACK`） |
| 2b | DSN `?_txlock=immediate` 被驱动**接受**（`BeginTx` 无错），但取值语义未回显验证 | **不采用**——探针只证明"不被拒绝"，不能证明真的用了 IMMEDIATE；风险高于显式语句，取 2 |
| 3 | 单次 `ExecContext` 传整段多语句脚本（`CREATE TABLE a; CREATE TABLE b;`）→ 两表均建 | **整脚本 Exec 成立**——`schema.sql` 与迁移文件不必按 `;` 切分 |
| 6 | `PRAGMA journal_mode=WAL` → 回读 `wal` | 成立 |
| 7 | cobra `Version="v0.0.0-probe"` + `--version` → stdout `basalt version v0.0.0-probe`，`Execute()` 返回 nil | **断言写"包含注入值"**（不锁死格式）——默认模板即含 `Name version Version` |
| 8 | `go version -m gosec.exe` → `mod github.com/securego/gosec/v2 v2.29.0` | **版本为 v2.29.0**——`gosec -version` 显示 `Version: dev` 是 `go install module@version` 未注入 ldflags 的显示缺口，**非版本不符**，不触发软门禁 |

## 环境读数（2026-10-15 本轮实测）

| 项 | 读数 | 判定 |
| --- | --- | --- |
| `go version` | `go1.27.2 windows/amd64` | 与 `toolchain go1.27.2` 一致 |
| `make --version` | `GNU Make 4.4.1` | 就位 |
| `gcc --version` | `gcc.exe (Rev5, MSYS2) 16.1.0` | `-race` 前置就位 |
| `gofumpt --version` | `v0.12.0 (go1.27.2)` | 与固定版本一致 |
| `staticcheck -version` | `0.8.2` | 与固定版本一致 |
| `gosec -version` | `Version: dev`（**显示缺口**） | 实际版本 v2.29.0——`go version -m` 取证见核实清单读数 #8 |
| `govulncheck -version` | `Scanner: govulncheck@v1.9.0` / `DB updated: 2026-10-08` | 与固定版本 v1.9.0 一致 |
| `go env GOPROXY` | `https://goproxy.cn,direct` | 与纪律一致（命令仍显式带 `GOPROXY=`） |
