# Tasks: US-3 Agent 目录与 Profile 加载

**Input**: Design documents from `/specs/003-agent-profile/`

**Prerequisites**: [plan.md](./plan.md)（必需）、[spec.md](./spec.md)（用户故事与优先级）、[research.md](./research.md)、[data-model.md](./data-model.md)、[quickstart.md](./quickstart.md)

**Tests**: **必须包含**——本项目「harness 先行」是硬纪律：颗粒度文档 [docs/us/us3-agent-profile.md](../../docs/us/us3-agent-profile.md) §3.2 列出全部用例（默认层 68 条 = 18 + 16 + 7 + 9 + 8 + 8 + 2；**本 feature 无集成层**——不触外部依赖），实现与测试同批落地（implement 阶段不允许先实现后补测）。§4.3 写出代码的关键回归用例**原样落地**（断言逐条保真；函数名英文、文档原文以注释保留）。

**Organization**: 任务按用户故事分组，每个故事可独立实现与独立验证。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: 所属用户故事（US1 / US2 / US3）——Spec-Kit 对**本 feature 内部**用户故事的编号（对应 spec.md 的三个 User Story），**不是**项目台账（`docs/us/REGISTRY.md`）里的 US-1 / US-2 / US-3；本 feature 的三个故事同属项目 **US-3（Agent 目录与 Profile 加载）**
- 每条任务都给出精确文件路径

## Path Conventions

单 Go module：`cmd/basalt/`、`internal/`、仓库根。测试与被测包同目录（Go 惯例）；本 feature 无集成层（无 `//go:build integration` 文件）。`contracts/` **不新建**（固定字面量已有唯一事实源——契约 `agent-md.md` §2 / `run-and-errors.md` §4.3.1 / 颗粒度文档 §6，见 [plan.md](./plan.md) Project Structure）。

---

## Phase 1: Setup（共享前置）

**Purpose**: 新增坐标落地与写前核实——不涉任何用户故事

- [x] T001 依赖落位与写前核实：`GOPROXY=https://goproxy.cn go get github.com/robfig/cron/v3@v3.0.1` 落地唯一新增坐标（`go.mod` / `go.sum`）；复核 `golang.org/x/sys v0.48.0` 保持现版本（仍 `// indirect`——**迁直接随 T018 锁文件落地后 `go mod tidy` 完成，版本不变**）；对照 [research.md](./research.md)「写前必须实测核实清单」复核 H1~H4 已核实读数（robfig `parser.go` / `x/sys` `zsyscall_*` / `os.Root` 方法面——均在本机模块缓存，就地 grep）；H5~H7 留给对应任务（T016 / T018）——核实不到即软门禁停下报告

---

## Phase 2: Foundational（阻塞全部用户故事）

**Purpose**: 契约层声明——三个故事都建立在 `core` 的声明面上

**⚠️ CRITICAL**: 本阶段完成前，任何用户故事都不得开工

- [x] T002 `internal/core/profile.go`（新增）：`Profile` / `Identity` / `ProviderRef` / `Channel` / `Settings` / `Schedule` / `AgentEntry`——**纯声明、零可执行语句**（覆盖率判卷跳过）、只依赖标准库（若需 import 则**只有** `time`）；**不带 yaml 标签**（映射由 `workspace` 私有 DTO 显式完成），[docs/us/us3-agent-profile.md](../../docs/us/us3-agent-profile.md) §3.1 逐字

**Checkpoint**: 声明面就位，三个用户故事可按优先级推进

---

## Phase 3: User Story 1 - 一个目录 = 一个 Agent：加载与校验（Priority: P1）🎯 MVP

**Goal**: `LoadAll` 扫描 `.basalt/agents/` 每子目录：解析 frontmatter（同一份字节先查未知键再解码）→ 八条校验（规则 4 占位）+ temperature 字段约束 → 任一不过**整只不注册** + 点名 WARN（`agent` / `reason` / `detail`）、其余照常；`schedules` 六段校验与 id 占用**整只提交**；`CronSpec` 输出 `CRON_TZ=` 前缀

**Independent Test**: 临时工作区写各形态 Agent 目录（共享验收样例 / 八类坏例 / 一只坏一只好），直接调用装载面断言每只的注册结果、点名原因与日志三键，全程不触网络、不开数据库（quickstart.md 场景 2 / 3 的底层面）

### Tests for User Story 1

- [x] T003 [P] [US1] `internal/workspace/agent_test.go`（新增 18 条 + 夹具家族）：`TestAgentLoader_LoadsCompatSampleAgent`（**§4.3 写出代码原样落地**——含判据 2 十个工具名逐字、`CronSpec` 对拍、判据 3 固定起点 2026-10-01T00:00:00Z 起十次触发逐条相同）/ `_RejectsNameMismatch` / `_RejectsUnregisteredProvider` / `_RejectsBlankModel` / `_AcceptsPlaceholderModel` / `_RejectsMissingSkill` / `_RejectsUnconfiguredMCPServer` / `_RejectsMissingBootstrapFile` / `_IgnoresLocalSkillsDirectory` / `_OneRejectedAgentDoesNotBlockOthers` / `_ParsesAllFrontmatterFields` / `_FillsDocumentedDefaultsWhenAbsent` / `_EmptyChannelsStaysEmpty` / `_ToleratesUnknownFrontmatterKeys` / `_UnknownKeyWarnNamesPathWithoutValue`（**§4.3 判据：只记键路径、不打印值**）/ `_RejectsOutOfRangeTemperature` / `_RejectsNonFiniteTemperature` / `_ParseErrorOmitsSourceExcerpt`（**§4.3 判据：不回显源码片段 / 库原文**）；夹具 helper 家族落在本文件（§2.4 注 8：`writeCompatSampleWorkspace` / `writeWorkspaceWithSchedule` / `writeAgentFrontmatter` / `writeFileInRoot` / `writeMCPServers` / `loadOnlyAgent` / `bufferedLogger`——非导出、写 `t.TempDir()`、**不新增 `testutil` 公开 API**）
- [x] T004 [P] [US1] `internal/workspace/schedule_test.go`（新增 16 条）：`TestAgentLoader_RejectsFiveFieldCron`（**§4.3 原样落地**——文案必含固定句 `Cron expression must consist of 6 fields`）/ `_CronSyntaxErrorCarriesParserText` / `_DefaultZoneIsUTC`（**§4.3 原样落地**——`CRON_TZ=UTC` 对拍，不得回落本机时区）/ `_InvalidZoneRejected` / `_RejectsLocalZone` / `_RejectsBothDomAndDowRestricted`（**§4.3 原样落地**——六子例，`*/2` 计为限定）/ `_RejectsSevenInDayOfWeek` / `_RejectsUnsupportedTokens`（`L` / `W` / `#`）/ `_RejectsBlankScheduleMessage` / `_ScheduleIDSingleTaskDerivesAgentSuffix` / `_ScheduleIDRequiredFromTwoTasks` / `_ScheduleIDDuplicateWithinAgentRejects` / `_ScheduleIDCollisionRejectsLaterAgent` / `_RejectedAgentClaimsNoScheduleIDs`（**§4.3 原样落地**——整只提交反例：alpha 被拒后 beta 可占用 id `x`）/ `_ExplicitIDCollidesWithDerivedID` / `TestCronSpec_PrefixesEveryZone`

### Implementation for User Story 1

- [x] T005 [US1] `internal/workspace/agent.go`（新增，装载主体——与 §2.3 流程 A 伪代码**逐块同构**）：`AgentLoaderOptions` / `AgentLoader` / `NewAgentLoader` / `LoadAll`（`os.OpenRoot` 钉根 → `agents/` 缺失**报错**（≠ 零只 Agent）→ `ReadDir(-1)` + **名字节序显式排序** → 非目录 / 软连接条目 `continue` → `readMCPServerNames`（缺失 → 空集）→ 逐只 `loadOne` → 全过者**唯一提交点**合并占用集 + `Registered=true`；被拒者 `RejectReason = mergedReason()` + WARN 固定键）+ 私有 `frontmatterDTO` 全族（`yaml:` 标签逐字对齐契约 §2；`*T` 区分未声明与显式零值）+ `parseFrontmatter`（**同一份字节**先未知键扫描（路径集先排序）再解码）+ `intoProfile` 显式映射与缺省填充（`settings` 未声明 → 10 / 20、显式 0 保留；`channels` 未声明 → `[{name: cli}]`、显式 `[]` → 空；`identity.agent_name` 未声明或空白 → `name`）+ `loadOne` 校验接线（规则 1 / 2 / 3 / 5（经 `missingSkill`）/ 6（存在性半条，WARN 字面量 `mcp server skipped`）/ 7 / temperature 0–2 有限 → `invalid provider.temperature`）+ `reject{reason, detail, claimed}` 与 `mergedReason()`（`reason` 或 `reason + ": " + detail`）+ `frontmatterErrorSummary`（与 `cli.yamlErrorSummary` 同口径；**两处各一份为既知重复**——§2.4 注 12）；**规则 4 为占位**（只解析进声明面、不判存在）。DoD：`go test -race -count=1 ./internal/workspace/`（与 T006 / T007 同批后本故事两文件全绿）
- [x] T006 [US1] `internal/workspace/agent.go`（同文件续——调度面）：`validateSchedules(agent, ss, claim)`（逐任务判定序：① 段数（剥 TZ 前缀）≠ 6 → 固定句；② 语法非法 → robfig 原文进 `detail`；③ 公共子集：`L` / `W` / `#`、dow `7`、日周同时被限定（`*/2` / `1-15` / `1,5` / 单值都算限定）；④ `zone`：空 → `UTC`、`Local` 拒、其余 `time.LoadLocation`（失败附原文）；⑤ `message` 空白拒；⑥ `id`：恰一个缺省派生 `<agent>-1`、≥2 项未显式拒、本只重复拒、与 `claim` 撞车拒（点名占用者）；`reason` 恒为 `invalid schedule`，detail 字面量逐字对齐 §6 固定口径 5）+ `CronSpec(s core.Schedule) (string, error)`（`CRON_TZ=<zone> <cron>`；**调度注册归 US-10**）。写前 H1 复核（robfig `NewParser` / `Parse` / `CRON_TZ=` 于模块缓存 `parser.go`）。DoD：T004 十六条转绿（`go test -race -count=1 ./internal/workspace/ -run 'TestAgentLoader|TestCronSpec'`）
- [x] T007 [US1] `internal/workspace/skill.go`（新增）：`skillCard{name, description, path}` 三要素（`name` 缺失回落技能目录名、`description` 缺失为空串**不发明校验**、`path` 逐字 `.basalt/skills/<name>/SKILL.md`）+ `missingSkill`（声明名在技能库无实体 → 整只不注册）+ `cardsFor`（声明逐条渲染、**不去重**；供 T005 与 US2 段一共用）+ 本地 `skills/` 目录**被无视** + WARN（绑定结果不变）。DoD：`go test -race -count=1 ./internal/workspace/ -run 'TestAgentLoader_(RejectsMissingSkill|IgnoresLocalSkillsDirectory)'`
- [x] T008 [US1] 端到端实测（本故事收尾）：`go test -race -count=1 ./internal/workspace/` 全绿；关键回归对号读数记录（共享样例 / 5 段固定句 / 缺省 UTC / 整只提交 / 未知键只记路径 / 解析错误压缩文案 / 单只失败不拖垮整批）

**Checkpoint**: US1 独立可验收——装载与校验零例外、坏一只不影响整批（MVP 到此可演示）

---

## Phase 4: User Story 2 - 技能声明式绑定与提示词第一段（Priority: P2）

**Goal**: `SectionOne` 无状态原语——`identity.prompt` → 自我称谓 → AGENT.md 正文 → Bootstrap（声明顺序）→ 技能卡片 L1（声明即绑定、真路径、不预载正文）→ 末尾当前时间（含偏移）；每次调用重读、`now` 显式传入、素材缺失点名报错、未注册拒装

**Independent Test**: 固定素材（正文 + 三件 Bootstrap + 技能库卡）调用段一按位置逐对断言次序与收尾格式；删素材重跑断言报错；两次调用之间改文件断言重读生效（quickstart.md 场景 5「段一四要素顺序与两次调用重读」）

### Tests for User Story 2

- [x] T009 [P] [US2] `internal/workspace/context_loader_test.go`（新增 7 条）：`TestContextLoader_SectionOneCarriesAllFourParts`（**§4.3 写出代码原样落地**——七标记逐位置断言 + 时间**收尾** `HasSuffix` + L2 正文负向探针 `NotContains`）/ `_SectionOneEndsWithTimestamp` / `_OnlyDeclaredSkillsReachSectionOne` / `_RefusesUnregisteredAgent` / `_RereadsBetweenCalls`（改文件后第二次调用取新内容——无缓存语义）/ `_FailsWhenBootstrapFileDisappears` / `_FailsWhenSkillFileDisappears`

### Implementation for User Story 2

- [x] T010 [US2] `internal/workspace/context_loader.go`（新增——与 §2.3 流程 B 伪代码**逐块同构**）：`ContextLoader` / `NewContextLoader(logger)` / `SectionOne(root string, entry core.AgentEntry, now time.Time) (string, error)`（`Profile == nil || !Registered` → 点名拒绝；`os.OpenRoot` 钉根；正文 / Bootstrap / SKILL.md 头**调用时重读**；组装顺序与 §6 固定口径 1 逐字——`你的名字是「<agent_name>」。` / `## <bootstrap 文件>` 小节 / `## 技能` 下 `- <name>：<description>（<真路径>）` 行 / 末尾 `当前时间：<2006-01-02 15:04:05 -07:00>`；任一步失败 → 错误点名路径、**不得**静默省略该节）。DoD：`go test -race -count=1 ./internal/workspace/ -run 'TestContextLoader'` 七条转绿
- [x] T011 [US2] 端到端实测（本故事收尾）：`go test -race -count=1 ./internal/workspace/`（US1 + US2 两批全绿）；段一四要素顺序 / 两次调用重读 / 素材消失报错读数记录

**Checkpoint**: US1 与 US2 独立可验收——技能卡片 L1 的可见面成立

---

## Phase 5: User Story 3 - 写模式命令：建模板、归档与实例锁准入（Priority: P3）

**Goal**: `create` 生成可装载模板（YAML 编码名字、不覆盖、失败清理本次残留、输出明示选中 provider）；`delete` 归档（单次 rename、撞名不覆盖、源目录保留）；实例锁（`create` / `delete` 取锁、拒锁零写入、跨进程判卷、释放幂等不删文件）；`profile` 四子命令经端口 × 适配器接线

**Independent Test**: 临时工作区跑 `create` / `delete` 正反例（合规 / 已存在 / 非法名 / 缺目录 / 归档撞名）断言文件系统终态与输出；实例锁以 helper-process **跨进程**用例判卷（互斥 / 强杀重取 / 拒锁零写入 / 幂等 / 锁文件留存）（quickstart.md 场景 4）

### Tests for User Story 3

- [x] T012 [P] [US3] `internal/workspace/agent_files_test.go`（新增 9 条）：`TestCreateAgent_WritesLoadableTemplate`（占位模型放行 + 生成物可经装载面注册）/ `_RefusesExistingDirectory`（零写入；既有目录及内容**逐字节保留**）/ `_RejectsInvalidName`（零写入；与 delete 同判据）/ `_EncodesNamesThroughYAML`（**§4.3 判据**：provider 名含空格 / 冒号仍可解析回原名）/ `_RefusesWhenInstanceLockHeld`（拒锁**零业务写入**）/ `TestDeleteAgent_ArchivesWithoutTouchingData` / `_RefusesMissingAgent` / `_RejectsInvalidName` / `_RefusesCollidingArchiveTarget`（`now` 固定 → 目标已存在 → 拒绝且**源目录仍在**——"不得先删后移"的可判据形态）
- [x] T013 [P] [US3] `internal/workspace/lock_test.go`（新增 8 条 + helper-process 设施）：`TestInstanceLock_HelperProcessMutualExclusion`（**主判据：跨进程**）/ `_HelperProcessReacquiresAfterHolderKilled`（强杀后重取）/ `_SecondAcquireFailsWhileHeld`（同进程双句柄——**补充**，不替代跨进程判据）/ `_ReleaseIsIdempotent` / `_KeepsLockFileAfterRelease` / `_BusyErrorNamesHolder`（诊断在锁定区之外，竞争方可读 pid / 开始时间）/ `_ContendedAcquireLeavesLockFileIntact`（竞争失败不改锁文件字节——取锁前不截断的可判据形态）/ `_ReadOnlyCommandsDoNotTouchLockFile`；helper-process 设施落在本文件（`lockHelperMain` + 环境变量分派 + exec 自身测试二进制 + **子进程 stdout 行同步、不得睡眠等待**）
- [x] T014 [P] [US3] `internal/cli/profile_test.go`（新增 8 条）：`TestProfileList_RendersRegisteredAndRejected` / `_FailsWhenWorkspaceMissing` / `_WithoutConfigStillListsWithNotice`（"按未注册处理"且不计失败）/ `TestProfileShow_UnknownNameFails` / `_RendersDeclaredFields` / `TestProfileCreate_CommandUsesWriteModePort`（**§4.3 判据**：输出明示选中的 provider）/ `_WithoutProvidersNotesUnregisteredTemplate` / `TestProfileDelete_CommandUsesWriteModePort`（`now` 注入固定时刻）；`cli` 侧经注入的假 `Agents` 端口判卷（**不得** import `workspace`）——编译前提 = T019 的接口面落位（先写用例、后补实现，同批交付）
- [x] T015 [P] [US3] `internal/app/ports_test.go`（追加 2 条）：`TestAgentsPort_ListForwardsLoaderEntries`（真装载面 + 临时工作区）/ `TestAgentsPort_CreateAndDeleteReachWorkspace`（经适配器触达 `workspace`，文件系统终态判卷）——编译前提 = T021 的适配器落位（同上）

### Implementation for User Story 3

- [x] T016 [US3] `internal/workspace/templates.go`（改造）：新增 Agent 模板构造（与既有五模板同风格；frontmatter 经 `goccy/go-yaml` `yaml.Marshal` **编码**写入——**不得**字符串插值，名字可含空格 / 冒号 / 中文；`provider.model` 恒为占位「请在此填写模型名」；`tools: []`；其余列表字段为空或省略）。写前 H6 复核（`*float64` / `*int` 未声明解码为 nil；`Marshal` 输出可被自身 `Unmarshal` 回读——于模块缓存）。DoD：`go test -race -count=1 ./internal/workspace/ -run 'TestCreateAgent'`
- [x] T017 [US3] `internal/workspace/agent_files.go`（新增）：`ValidateAgentName(name string) error`（非空、单目录名——不含 `/` 与 `\`、不为 `.` / `..`、不含 NUL；**create / delete 共用同一函数**）+ `CreateAgent(root, name string, providers []string) (created []string, err error)`（**内部先取实例锁**；`ValidateAgentName` 前置；`agents/<name>` 已存在 → 拒绝**不覆盖**、拒绝路径零写入；写失败 → 清理**本次创建**物（既有目录一律不动）、清理失败点名残留路径不掩盖；`provider.name` 取 `providers[0]`、无配置写占位文本「请在此填写 provider 名」）+ `DeleteAgent(root, name string, now time.Time) (archived string, err error)`（取锁；**单次 `os.Root.Rename`** 入 `archive/<name>-<YYYYMMDD-HHMMSS>/`——**不得**复制 + 删除 / 先删源后建目标；归档目标已存在 → **显式存在检查**拒绝并点名目标路径且源目录保留；缺目录点名拒绝；会话 / 记忆 / 审计数据不动；`now` 显式传入）；写失败路径（create / delete 的文件系统写入失败）记 **ERROR** 日志（可观测性双轨——FR-028；错误不吞、不新造日志键）。DoD：T012 九条转绿
- [x] T018 [US3] `internal/workspace/lock.go` + `lock_windows.go` + `lock_unix.go`（新增；**构建约束分文件**）：`InstanceLock{file, once}` / `AcquireInstanceLock(root string) (*InstanceLock, error)`（流程 D 逐块：打开无则创建 **O_CREATE|O_RDWR 无 O_TRUNC**；锁区 `[0,1)`——Windows `windows.LockFileEx`（`x/sys`）、类 Unix `unix.Flock(LOCK_EX|LOCK_NB)`；诊断（instance_id / pid / 开始时间）`WriteAt` 自 offset **4096** 起、定长、字段序固定；诊断写失败 → **先 `Release` 再返回错误**；取不到锁 → 另开**免锁读句柄**读诊断区 → `*LockBusyError{PID, StartedAt, Detail}`，诊断缺失时 `Error()` 如实说明不编造）+ `Release() error`（**幂等**：重复调用 no-op 返回 nil；只解锁 + 关句柄、**不删除锁文件**）；本任务含 `go mod tidy` 使 `golang.org/x/sys v0.48.0` 由 indirect 迁直接（**版本不变、无新增坐标**）。写前 H2 / H3 / H5 / H7 复核（`windows.LockFileEx` / `unix.Flock` / `(*os.File).Fd()` / `errors.Is(err, windows.ERROR_LOCK_VIOLATION)`——于模块缓存 / GOROOT）。DoD：T013 八条转绿（含两例跨进程 helper-process）
- [x] T019 [US3] `internal/cli/ports.go`（改造——前序改造点 1）：新增 `Agents` 端口 `List(root string, providers []string) ([]core.AgentEntry, error)` / `Create(root, name string, providers []string) (created []string, err error)` / `Delete(root, name string, now time.Time) (archived string, err error)`（`providers` 为显式输入——命令面经 `LoadConfig` 取得，**适配器不另读配置**）；`internal/cli/deps.go`（改造——前序改造点 2）：`Deps` 新增 `Agents Agents` 字段（注释指明写模式取锁语义）
- [x] T020 [US3] `internal/cli/profile.go`（新增——与 §2.3 流程 C 伪代码**逐块同构**）：`newProfileCommand`（可运行父命令 + 未知子命令非零，与 provider 组同形态）+ `newProfileListCommand` / `newProfileShowCommand` / `newProfileCreateCommand` / `newProfileDeleteCommand`（cobra 层取 `time.Now()` 传 `runProfileDelete`）+ `runProfileList` / `runProfileShow` / `runProfileCreate` / `runProfileDelete(out, deps, name, now)` + `renderProfileList` / `renderProfileShow`（展示层与装载分离、供注入判卷）；轻路径边界（未初始化 → 点名 + 指引 `basalt init`；配置缺失 → 明示"按未注册处理"**不计失败**；配置无效 → 原错误上抛；空目录清单 → 成功 + 说明）；渲染字号面逐字对齐 §6 固定口径 3（`<name>：registered=true（provider <p> / 模型 <m>）` / `registered=false——<原因>` / 汇总行）；`internal/cli/root.go`（改造——前序改造点 3）：注册 `newProfileCommand(deps)`。DoD：T014 八条转绿
- [x] T021 [US3] `internal/app/ports.go`（改造——前序改造点 4）：新增 `agentsPort{WorkDir string; Logger *slog.Logger}` 适配器（三方法转调 `workspace.AgentLoader`（`AgentLoaderOptions{Providers, Logger}` 显式装配）/ `CreateAgent` / `DeleteAgent`；错误原样上抛）；`internal/app/run.go`（改造——前序改造点 5）：装配 `Deps{... Agents: agentsPort{WorkDir: workDir, Logger: logger}}`。DoD：T015 两条转绿
- [x] T022 [US3] 端到端实测（本故事收尾）：临时工作区 `create → list → show → delete` 往返（含缺配置形态、已存在 / 非法名 / 撞名反例、退出码 0 / 1）；跨进程锁两例（helper-process）读数；`go test -race -count=1 ./internal/workspace/ ./internal/cli/ ./internal/app/` 三包全绿；`internal/cli/ports_test.go` 既有依赖方向判据回归（**A11**——`profile` 命令走端口即自动受判，本 US 不新增判据文件）

**Checkpoint**: 三个用户故事全部独立可验收——`profile` 四子命令可用、实例锁跨进程成立

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: 收口证据与跨故事一致性

- [x] T023 [P] `internal/workspace/doc.go`（改造——前序改造点 8）：包职责句补 `AgentLoader` / `ContextLoader` / 实例锁（文档同步、非行为）
- [x] T024 [P] 覆盖率判卷调平：`make cover` 全包达标（`internal/workspace` 自本 US 起纳入 **≥ 50%** 判卷；`internal/core` 记 `[no statements]` 跳过——**不得**为凑覆盖率往 `core` 加空转测试或语句）；读数记录
- [x] T025 [P] 静态与安全门禁复核：`gofumpt -w` 只对本轮改动文件；`go vet` / staticcheck（显式判定集 `-checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022`）/ `gosec -quiet ./...` / `govulncheck ./...` 零命中；抑制注释（`//nolint` / `#nosec`）零使用
- [x] T026 [P] 全局不变量逐条 grep 自查（`module-dev` 配置区 9 条）+ 依赖方向复核：`go list -f '{{.Imports}}' ./internal/core` 预期 `[]`（`profile.go` 纯声明）；`time.Local` / `use_skill` / `context.WithValue` 零命中；`cli` **不** import `workspace`（既有 `ports_test.go` 判据回归）
- [x] T027 [P] [quickstart.md](./quickstart.md) 七场景逐条执行（场景 7 人工项**未由人执行时如实登记「未执行」**，**不得**记通过）
- [x] T028 文档一致性收口：交付物清单 ↔ 实现逐项核对（[docs/us/us3-agent-profile.md](../../docs/us/us3-agent-profile.md) §3.1 / §3.2 / §3.3 / §3.4 / §3.5）+ harness 映射表 §4.2（A1~A15）逐条对号 + §4.3 关键回归原样落地核对 + `/speckit-analyze` 收口复核；新发现陷阱按陷阱表惯例拟条（带「怎么发现」与「怎么复核」），**经用户确认后**入库
- [x] T029 `make check` 终局全绿 + 七项 DoD 证据留存（门禁输出 / 映射对号 / 存在性核对 / **前序模块 US-1、US-2 全部测试回归** / 不变量自查 / 剩余人工项清单 / 变更总结三段式）；`Makefile` 与 `.github/workflows/ci.yml` **无改动**核对（§3.3——**不得**据此宣称覆盖 Windows 锁分支：CI 只跑 `lock_unix.go`，`lock_windows.go` 运行读数由本机登记）

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup（Phase 1）**: 无依赖，立即开始
- **Foundational（Phase 2）**: 依赖 Setup —— **阻塞全部用户故事**
- **User Stories（Phase 3+）**: 均依赖 Foundational；按 P1 → P2 → P3 顺序推进（本项目单人 + 依赖链，不并行开工）
- **Polish（Phase 6）**: 依赖三个故事完成

### User Story Dependencies

- **US1（P1）**: Foundational 后即可开工——消费 `core` 声明面（T002）与 robfig 坐标（T001）；T005 / T006 / T007 同属 `internal/workspace` 包，一包内互相引用（`missingSkill` / `cardsFor`），按 T005 → T006 → T007 顺序落地、同批编译
- **US2（P2）**: 依赖 US1 的夹具家族与 `cardsFor`（T003 / T007）；`SectionOne` 本身只消费 `core.AgentEntry`
- **US3（P3）**: 依赖 US1 装载面（`CreateAgent` 的模板可装载性判卷复用 `LoadAll`）；命令面（T019 / T020 / T021）依赖 T016~T018 的 `workspace` 实现

### Within Each User Story

- 测试与实现**同批落地**（harness 先行：先写用例、确认其因缺实现而失败，再补实现）
- 声明 / DTO → 校验 → 单只装载 → 整批装载 → 端到端实测
- 每个故事收尾必须「该故事的测试全绿」才进下一个故事
- §4.3 写出代码的关键回归用例（`LoadsCompatSampleAgent` / `RejectsFiveFieldCron` / `DefaultZoneIsUTC` / `RejectsBothDomAndDowRestricted` / `RejectedAgentClaimsNoScheduleIDs` / `SectionOneCarriesAllFourParts`）**原样落地**（断言逐条保真；函数名英文、文档原文以注释保留）
- 涉及第三方 API 的任务**写前先在本地依赖核实**（H3 门禁，清单见 [research.md](./research.md)）：T006（H1 robfig）、T016（H6 goccy）、T018（H2 / H3 / H5 / H7）

### Parallel Opportunities

- Setup：仅 T001（其余任务都等它）
- Foundational：仅 T002
- US1：T003 / T004 两个测试文件可并行；T005~T007 同文件的顺序落地不并行
- US2：T009（测试）与 T010（实现）先后；T011 收尾
- US3：T012 / T013 / T014 / T015 四个测试文件可并行；T016 / T017 / T018（workspace 侧）可并行（不同文件——T017 依赖 T016 的模板函数形态时按序）；T019 → T020 / T021 依次（接口面先行）
- Polish：T023~T027 可并行

---

## Parallel Example: User Story 3

```text
# 四个测试文件可同时写（互不同文件）：
T012 internal/workspace/agent_files_test.go（9 条）
T013 internal/workspace/lock_test.go（8 条 + helper-process 设施）
T014 internal/cli/profile_test.go（8 条，假 Agents 端口注入）
T015 internal/app/ports_test.go（追加 2 条）

# workspace 侧实现可并行（不同文件）：
T016 internal/workspace/templates.go（改造：Agent 模板）
T018 internal/workspace/lock.go + lock_windows.go + lock_unix.go
```

---

## Implementation Strategy

### MVP First（User Story 1）

1. Phase 1 Setup → Phase 2 Foundational
2. Phase 3（US1）：装载与校验（八条 + temperature、未知键宽容可见、调度六段与整只提交）
3. **停下验证**：`go test -race` `internal/workspace` 全绿 + 关键回归对号（共享样例 / 5 段固定句 / 缺省 UTC / 整只提交）
4. 本 feature 的 MVP 到此即可演示（一个目录 = 一个 Agent 可判卷）

### Incremental Delivery

1. Setup + Foundational → 声明面就位
2. US1 → 装载与校验（MVP）
3. US2 → 段一拼装（技能卡片 L1 可见面）
4. US3 → 写模式命令 + 实例锁 + 命令面接线（`profile` 四子命令可用）
5. Polish → 覆盖率 / 不变量 / quickstart / 收口证据

---

## Notes

- **[P]** = 不同文件、无未完成依赖
- 测试与实现同批交付；**不得**删断言 / 无理由 `t.Skip` / 放宽阈值让门禁变绿；测试函数名英文（`TestXxx_yyy`），文档 harness 中文验收点以注释保留
- **不自动 commit / push / tag**——每个 US 的提交由用户执行；提交信息格式见 [docs/EngineeringStandards.md](../../docs/EngineeringStandards.md) §5
- 软门禁：交付物清单外的对外概念、已定字面量修改（固定句 `Cron expression must consist of 6 fields` / 真路径 `.basalt/skills/<name>/SKILL.md` / 锁文件 `.basalt/basalt.lock` / 归档名 `<name>-<YYYYMMDD-HHMMSS>` / §6 固定口径全部）、第三方 API 核实不到 → **停下报告**
- 前序改造点（§3.5）八处分别落在 T016（templates.go）/ T018（go.mod x/sys 迁直接；T001 落 robfig）/ T019（ports.go + deps.go）/ T020（root.go）/ T021（app ports.go + run.go）/ T023（doc.go），逐条为软门禁；§3.5 第 9 项**占位点**是登记（tools 门禁 → US-4、MCP 连接半条 → US-7、快照 → US-4 / US-11、schedules 注册 → US-10、记忆注入 → US-6、PromptBuilder → US-4、REST / 管理台 → US-9 / US-12、锁重路径与恢复 → US-4），**不产生本 US 任务、不改写**
- 核心逻辑必须与 §2.3 流程 A–D 的伪代码**逐块对照落地**（同构性核对，签名逐字一致）；内部函数名（`parseFrontmatter` / `intoProfile` / `loadOne` / `readMCPServerNames` / `validateSchedules` / `missingSkill` / `cardsFor` / `mergedReason` / `frontmatterErrorSummary` / `lockHelperMain`）同为交付面，逐项可核
- 契约文件**不新建**；`internal/testutil` **不扩充**（夹具各包内联——§2.4 注 8）；`AGENT.md` 内 `${ENV_VAR}` 解析与 `settings.*` 取值合理性校验**不做**（无文档依据）
