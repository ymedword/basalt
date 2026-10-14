---
name: "init-foundation"
description: >-
  为 Go 项目（单 module + internal/ 分包 + CGO_ENABLED=0 单二进制）初始化工程地基：
  go.mod 与包骨架、门禁（gofmt / gofumpt / vet / staticcheck / gosec / govulncheck /
  带 -race 的测试 / build tag 集成测试 / 覆盖率阈值判卷 / 静态构建）、Makefile 七目标、
  CI 双 job、
  basalt.yaml 与 ${ENV_VAR} 占位校验、slog 双档日志、`basalt init` 工作区幂等、
  SQLite schema 幂等与版本化迁移、契约 YAML 解析单测。版本先核实（goproxy.cn）后落地。
  当用户要「初始化项目 / 搭工程骨架 / 加日志 / 加门禁 / 加安全检查 / 装工程地基」时使用。
  本 skill 是 US-1 的执行参考，流程仍走 /module-dev 1。
argument-hint: "[目标目录（默认当前仓库根）]"
user-invocable: true
disable-model-invocation: false
---

# init-foundation — 初始化工程地基（Go 单二进制）

把「工程地基」一次性、标准化地装好——业务逻辑（核心能力）不在本 skill 范围内。

## 定位与触发

- **定位：US-1 的执行参考（runbook）**——流程仍走三步链的 `/module-dev 1`
  （specify → clarify → plan → tasks → implement → 七项证据）；
  **交付物与验收一律以 US-1 颗粒度文档为准**（`docs/us/us1-*.md`），
  本 skill 只提供「怎么装」的标准做法与宪法级判卷点
- 触发词：初始化工程门禁、搭工程骨架、加日志、加门禁、Go 项目地基、单二进制骨架

## 不做什么（边界）

- 不实现核心能力（Provider / ReAct / Memory / Tool / Web Service）——那是
  US-2 起的业务，走 `/module-dev N`
- 不硬编码任何密钥 / token / API key——一律 `${ENV_VAR}` 占位
- 不替换已存在的业务代码；只新增基础设施与配置
- 不引入监控服务依赖（指标属扩展阶段：只做 `MetricsRegistry` 接口 + 无操作实现预留）
- 不 `git init`、不 commit / push（`git init` 由用户执行）

## 宪法级条目（不得违反）

1. **版本必须先核实后落地**：任何模块坐标写入 `go.mod` 前，必须经模块代理核实
   （步骤 2 命令模板），并与 `docs/TechnicalSolution.md` §1.2 对照；落地后
   `go.mod` / `go.sum` 是唯一事实源。
   - 违反症状：坐标版本不存在 / 与实测结果漂移（凭记忆或教程摘要写版本号）
2. **`fmt` 门禁必须先查退出码、再判输出**（两个格式工具各查一遍）：格式差异时
   退出码为 0 而 stdout 非空；语法错误时退出码非 0 而 stdout 为空——判卷顺序写错
   会把"文件根本解析不了"判成通过。
   - 违反症状：留一个语法错误文件，`make fmt` 竟然通过
3. **`-race` 与 `CGO_ENABLED=0` 分属不同命令 / job**：竞态检测需要 CGO 与 C 编译器，
   发布构建要求 CGO 关闭——两者不得混在一个环境里。
   - 违反症状：竞态检测跑不起来，或产物不再是纯静态单二进制
4. **build tag 集成测试必须单列**：`go test ./...` 默认不执行 `-tags=integration`
   的测试，不单列等于没有。
5. **格式修复只许 `gofumpt -w .`**（gofumpt 是 gofmt 的严格超集），禁手改格式。
   **init 阶段的全仓初始格式化是全仓 `gofumpt -w .` 的唯一合法场景**；此后各 US
   只许 `gofumpt -w <本轮改动的文件>`（全仓 `gofumpt -l .` 只读）。
6. **只新增、不删除、不替换**既有工程配置。
7. **日志统一 `log/slog`，禁止用 `fmt.Println` 当日志**——输出无级别 / 无结构化
   字段，绕过日志配置与采集。
8. **表结构权威是 `schema.sql`**（幂等）+ `schema_version` 受控前向迁移；
   **不得**引入 ORM、**不得**由 ORM 自动修改结构；已发布迁移只追加、不得改写。
9. **凭证一律 `${ENV_VAR}` 占位**：未解析残留即拒（点名配置键、不静默取默认）；
   明文不得落盘 / 落日志 / 进对话。
10. **发布形态是 `CGO_ENABLED=0` 静态构建**，必须通过（单二进制是本项目核心卖点）。
11. **每步落地后必须跑门禁验证**（步骤 10），失败输出如实汇报，不掩饰、不跳过。

## 决策记录（防漂移，无新证据不推翻）

- **不引第三方日志 / 配置 / 路由框架**：`log/slog` + 标准库 `net/http` + ServeMux
  够用；YAML 只引 `goccy/go-yaml` 一个（frontmatter 与 `basalt.yaml` 共用同一个库，
  零第二依赖）。依赖最小化是宪法第 8 条的延伸——新增坐标必须经用户同意。
- **监控接口预留不实装**：只落 `MetricsRegistry` 接口 + 无操作实现（`internal/core`）；
  核心阶段可观测性 = 结构化日志 + 审计表 + 本接口。若文档升级把监控提前到核心阶段，
  再回看此条。
- **不用 ORM**：显式 SQL + `schema.sql` 幂等 + `schema_version`（宪法第 9 条）。

## 流程（步骤 0~10；每步完成后跑该步验证，失败停下报告）

### 步骤 0：前置检查

- **`git init` 由用户执行**——本 skill **不得**自行初始化仓库（本仓 2026-10-15 已初始化，
  见 `docs/GettingStarted.md` §1.4）；仓库尚无 `.git` 时，
  `git` 类判据（文档入库时间等）一律记 `未入库`
- Go 工具链 1.27.2 就位（`go version`；1.27.2 是**构建版本**，`go.mod` 的 `go` 行
  只写最低要求 `go 1.27`）；`GOPROXY=https://goproxy.cn` 可连通
  （本机 proxy.golang.org 不可达，已实测；环境读数见 `docs/GettingStarted.md` §2）
- US-1 颗粒度文档已生成（`docs/us/us1-*.md`）；未生成 → 停下，先跑 `/module-doc-gen 1`

### 步骤 1：骨架（`go.mod` + `cmd/basalt` + `internal/` 十包）

```bash
go mod init github.com/<org>/basalt   # 模块路径以实际仓库为准
mkdir -p cmd/basalt internal/{app,core,provider,memory,tool,channel,web,storage,cli,workspace}
```

空目录放 `doc.go`（包注释 + 包声明）。包结构纪律（依赖方向 / 接口定义在消费方 /
第三方实现就近封装）见 `docs/TechnicalSolution.md` §10.1；**包数是起点，不冻结**。

`go.mod` 就地写两行版本指令：`go 1.27`（**最低版本要求**——Go 1.21 起 `go` 行不再
是准确版本锁定）+ `toolchain go1.27.2`（**准确构建版本**；本地工具链旧于它时按
`GOTOOLCHAIN=auto` 自动切换 / 下载）。**不得**把 1.27.2 表述成"`go.mod` 锁定"——
准确版本由 `toolchain` 行与 CI 工具链配置（步骤 4）共同固定。

### 步骤 2：版本核实（goproxy.cn 模板）

```bash
# 在已有 go.mod 的仓库根执行；逐坐标核实后再写入 go.mod
for m in github.com/spf13/cobra github.com/goccy/go-yaml github.com/sashabaranov/go-openai \
         modernc.org/sqlite github.com/robfig/cron/v3 github.com/modelcontextprotocol/go-sdk \
         github.com/fsnotify/fsnotify github.com/stretchr/testify; do
  printf '%s latest: ' "$m"; GOPROXY=https://goproxy.cn go list -m -versions "$m" | awk '{print $NF}'
done
```

结果与 `docs/TechnicalSolution.md` §1.2（2026-10-14 经模块代理实测）对照；不一致 →
停下报告（软门禁）。写入 `go.mod` 后由 `go.sum` 锁定。

### 步骤 3：Makefile 门禁七目标

目标名固定 `check fmt lint sec test test-integration cover build`（AI 与 CI 共用）；
`check` 依次聚合这七个目标。**`cover` 是覆盖率阈值判卷**（宪法开发工作流的可执行
判据）：`go test -cover` 只打印百分比、**不会**因低于阈值失败，故 `cover` 目标调一个
按包解析结果并 `exit 1` 的检查脚本（`core` / `tool` / `memory` / `storage` ≥ 70%，
其余 ≥ 50%；有可执行语句但无测试文件的包计 0%、无语句包跳过）。脚本实现形态由本
US 决定，但**必须**在 `make check` 里有这一环。`fmt` 目标**先查退出码、再判输出**
（两个格式工具各查一遍）。配方照 `docs/AiProgrammingGuide.md` §3.2 的 Makefile 建议
原文落地——例如 `fmt` 目标：

```makefile
fmt:  ; out="$$(gofmt -l .)" || { echo "gofmt 异常退出（退出码 $$?，多为语法错误）"; exit 1; }; test -z "$$out" || { echo "未格式化（gofmt）："; echo "$$out"; exit 1; }; out="$$(gofumpt -l .)" || { echo "gofumpt 异常退出（退出码 $$?）"; exit 1; }; test -z "$$out" || { echo "未格式化（gofumpt）："; echo "$$out"; exit 1; }
```

`lint` 目标里的 staticcheck **必须显式指定判定集**（2026-10-15 定）：
`staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...`
——`inherit` 取默认集，追加子集把命名与注释规范变成门禁判卷；形式取命令行旗标
（Makefile 一处钉死，AI 与 CI 同一入口）。⚠ 本机实测更正（2026-10-15）：源码在
`%TEMP%` 等用户缓存目录下时 staticcheck **跳过配置文件定位**——曾据此把
`staticcheck.conf` 写法误判为无效；该写法**有效**，但缓存目录内的探针会给出假象，
论证工具行为必须在缓存目录之外建探针（口径见 `docs/EngineeringStandards.md` §1.1）。

### 步骤 4：CI 双 job（各自固定 CGO 状态）

`.github/workflows/`：job `test` = `make fmt lint sec test test-integration cover`
（默认开启 CGO——`-race` 必需；Linux runner 自带 gcc；`cover` 按包阈值阻断）；job `build` =
`CGO_ENABLED=0` 四平台交叉编译（linux/amd64、linux/arm64、windows/amd64、
darwin/arm64）——**不得**在此 job 跑 `-race`（CGO 状态冲突）。覆盖率报告入库或上传工件。GitHub 侧把两个 job 设为**必需检查**（branch
protection，由用户设置、设置后回读一次；口径见 `docs/AiProgrammingGuide.md`
§8.3 第 6 项）。**"安全扫描拖慢默认构建"的问题在 Go 不存在**——gosec / govulncheck
都是秒级，全部进默认门禁。

两个 job **都必须**经 `actions/setup-go` 把工具链钉到 `go-version: 1.27.2`（与
`go.mod` 的 `toolchain` 行一致）——CI 是"准确构建版本"的第二处事实源，`go` 行只是
最低要求。

### 步骤 5：`basalt.yaml` 加载与 `${ENV_VAR}` 占位校验

- 加载即校验：未解析残留（`${...}` 原样残留）即拒、**点名配置键**、不静默取默认
- 凭证一律 `${ENV_VAR}` 占位；明文检查按原始配置值判断

### 步骤 6：slog 双档日志

- dev：文本 handler（可读）；prod：JSON handler
- 请求级字段经 context 携带——**只承载追踪标识**（宪法第 7 条：业务状态走显式
  类型化参数，**不得**用 `context.WithValue` 传 Profile / Session）

### 步骤 7：`basalt init` 工作区幂等

- 生成 `.basalt/` 工作区：`agents/`、`skills/`、`memory/`、`logs/`、`archive/`
  与 `AGENTS.md`、`SOUL.md`、`USER.md`、`mcp_servers.yaml`（目录约定与基线对齐，
  见 `docs/Compatibility.md` §2.1）
- **幂等**：二跑不覆盖已有文件；`basalt status` 显示库与表就绪

### 步骤 8：SQLite（modernc、WAL、schema 幂等、受控前向迁移）

- 驱动 `modernc.org/sqlite`（纯 Go、无 CGO——单二进制前提），经 `database/sql`
- WAL；**读写池分离、写池 1**（SQLite 单写者）
- `schema.sql` 幂等建齐**核心八张 + 支撑三张**（共 11 张）：sessions / messages /
  runs / config_snapshots / llm_calls / tool_invocations / scheduled_tasks /
  task_executions + notify_channels / memory_entries / schema_version
- `schema_version` 受控前向迁移：已发布迁移**只追加、不得改写**
- **时区数据库内嵌**：`import _ "time/tzdata"`——裸镜像里 IANA 时区名才解析得出来，
  否则带 `zone: Asia/Shanghai` 的 Agent 到了容器里整只不注册
- 仓储接口定义在 `core`、实现在 `storage`（依赖倒置）

### 步骤 9：契约 YAML 解析单测

- 用项目已有的 YAML 依赖（`goccy/go-yaml`）解析 `docs/contracts/*.yaml`——值以
  `*` / `&` / `!` / `%` 等指示符开头又不加引号的标量会让整个文件解析失败，此类
  隐性错误当场红（编程指南 §7 陷阱表第 32 条）

### 步骤 10：验证（正向 + 负向）与报告

1. `make check` 全绿（贴关键输出）
2. `basalt init` 产出 `.basalt/` 工作区；二跑幂等（已有文件不覆盖）
3. `basalt status` 显示库与表就绪
4. `CGO_ENABLED=0 go build` 出静态二进制
5. tzdata 内嵌实测：`TZ=` 清空 + 无系统 zoneinfo 的环境里
   `time.LoadLocation("Asia/Shanghai")` 仍成功
6. **负向验证**：故意留一个未格式化文件 → `make fmt` 非零退出；再留一个语法错误
   文件 → 同样非零退出；故意留一个 `${UNRESOLVED}` 残留 → 启动即拒
7. 如实报告：改了哪些文件、哪些步骤被跳过、未验证项——一切真跑命令必须由有 Go
   工具链的机器执行；当前机器无工具链时如实登记为未验证（**不得**以"应当能过"代替实测）

## 检查清单（Definition of Done）

映射 US-1 颗粒度文档 §3「交付物清单」与 §5 七项证据；**以颗粒度文档为准**，
本清单是可核对的底线：

- [ ] `make check` 全绿（fmt / lint / sec / test / test-integration / cover / build 七目标；`cover` = 按包阈值判失败，脚本本 US 交付）
- [ ] `internal/` 十包骨架可编译（空目录有 `doc.go`）
- [ ] CI 双 job 各自固定 CGO 状态（test 开 CGO / build `CGO_ENABLED=0`）
- [ ] `basalt.yaml` 加载 + `${ENV_VAR}` 未解析残留即拒（点名配置键）
- [ ] slog dev 文本 / prod JSON 实测可切换
- [ ] `basalt init` 幂等（二跑不覆盖已有文件）；`basalt status` 可用
- [ ] SQLite WAL + 读写池分离（写池 1）+ `schema.sql` 幂等 11 表 + `schema_version`
- [ ] tzdata 内嵌（无系统 zoneinfo 环境 `time.LoadLocation` 仍成功）
- [ ] 契约 YAML 解析单测绿（`docs/contracts/*.yaml`）
- [ ] `CGO_ENABLED=0 go build` 通过（静态单二进制）
- [ ] 凭证全 `${ENV_VAR}` 占位，无明文
- [ ] 坐标版本经 goproxy.cn 核实并与技术方案 §1.2 对照

## 与 constitution / Spec-Kit 的分工

- **本 skill**：把工程地基「装上」（US-1 的装法参考；一次性、可复用）
- **constitution（`.specify/memory/constitution.md`）**：把硬约束「钉死」，让 AI 每次都遵守
- **CI + Makefile**：把检查「强制执行」（机器把关，不靠人自觉）
- **Spec-Kit user story**：地基起好后，按核心能力逐个开发
  （`/module-doc-gen N` → `/us-register N` → `/module-dev N`），每 US 收口跑
  `/speckit-analyze`
