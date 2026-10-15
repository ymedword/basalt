# Phase 1 数据模型：US-1 工程地基

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-15

本 feature 的「数据」共四类：**工作区骨架**（文件系统）、**数据库**（SQLite 单文件）、**版本记录**（迁移与兼容判定的锚点）、**配置**（YAML + 环境变量占位）。列 / 状态字面量的唯一事实源是 [docs/contracts/schema.sql](../../docs/contracts/schema.sql)；本文件**不抄列清单**，只记实体关系、状态机与校验规则。

## 实体 1：工作区（Workspace）

**载体**：文件系统目录树，根 = `Deps.WorkDir` 下的 `.basalt/`（`basalt init` / `basalt status` 均以「当前工作目录」为准）。

| 成员 | 类型 | 判据（可测） |
| --- | --- | --- |
| `agents/` `skills/` `memory/` `logs/` `archive/` `sessions/` | 目录 × 6 | 缺则建；已存在 → Kept |
| `AGENTS.md` `SOUL.md` `USER.md` | 文件 × 3 | 缺则写模板；已存在 → Kept（逐字节不动） |
| `mcp_servers.yaml` | 文件 × 1 | 缺则写模板；内容为**空表**且可被 goccy/go-yaml 解析为空映射 |
| `memory/MEMORY.md` | 文件 × 1 | 缺则写模板；内容含「`## 核心记忆` / `## 归档记忆`」两分区 |
| `memory/agents/` | **不预建** | `Init` 后该目录不存在（写入时落盘） |
| `basalt.db` | 由 `storage` 创建 | 见实体 2 |

**校验规则**：`workspace.Init` 幂等——已存在成员一律不覆盖、不报错；返回 `InitReport`（`Created` / `Kept` 两清单）供二跑对账（二跑 `Created` 为空即幂等证据）。

**关联**：`workspace.Inspect(root)` 只读清点，产出 `Report`（目录 / 文件存在性 + 未初始化判定），供 `basalt status` 节 1 消费。

## 实体 2：数据库（DB）

**载体**：SQLite 单文件 `.basalt/basalt.db`；`modernc.org/sqlite`（纯 Go）。

**结构**：11 张表（`schema.sql` 幂等建齐，列 / 状态字面量以契约文件为准）：

| 表 | 用途 |
| --- | --- |
| `sessions` / `messages` | 会话元数据 + 消息逐条追加（承载工具调用结构） |
| `runs` | 一次运行（执行与审计单位） |
| `llm_calls` / `tool_invocations` | 两阶段写审计（**本 US 仅建表**，写入自 US-2 / US-4 起） |
| `config_snapshots` | 配置快照（内容寻址；US-4 起写入） |
| `scheduled_tasks` / `task_executions` | 定时任务状态与执行历史 |
| `notify_channels` / `memory_entries` | 通知渠道注册表 / SQLite 记忆档载体 |
| `schema_version` | 迁移版本记录（本 US 写入首行 = 1） |

**连接层不变量**（每条都是可测判据）：

| 不变量 | 判据 |
| --- | --- |
| 写池上限 1 | 写句柄 `SetMaxOpenConns(1)`；并发写事务在 Go 侧排队 |
| PRAGMA 逐物理连接 | 任意两条独立连接的 `foreign_keys` 与 `busy_timeout` 均生效（DSN 参数，非启动时一次性语句） |
| WAL 库级持久 | 创建后 `PRAGMA journal_mode` 回读 `wal`；关闭重开仍为 `wal` |
| 事务不跨外部调用 | 本 US 无 LLM / 工具调用；写事务只包 SQL 与版本记录 |
| 只读路径零写 | `OpenReadOnly` 不迁移、不写库、不取实例锁、不触碰锁文件 |

**接口（导出符号契约见 [contracts/storage-api.md](./contracts/storage-api.md)）**：`OpenWritable` / `Bootstrap` / `OpenReadOnly` / `Migrate` / `(*DB).SchemaVersion` / `(*DB).TableNames` / `(*DB).Close`；`SupportedSchemaVersion = 1`。

## 实体 3：版本记录（schema_version）——状态机

**判定唯一依据**：`schema_version` 表的存在性与其中的版本值（契约迁移纪律 2）。

| 状态 | 判据 | 写模式打开（`OpenWritable`） | init 准入（`Bootstrap`） | 只读打开（`OpenReadOnly`） |
| --- | --- | --- | --- | --- |
| 新库 | 表不存在（库文件缺失 / 空文件 / 残留） | 创建：全量建表 + 写 `version = target`（**同一事务**，失败整体回滚）；本进程 = 实际创建者 | 同左，`created = true` | 库缺失 → `ErrDatabaseMissing`（不代建）；文件残留但无表 → 拒绝（不代建） |
| 当前版本 | `version == target` | 提交空事务、正常打开 | `created = false`（Kept，零写） | 校验通过，报版本 + 表清点 |
| 旧版本 | `version < target` | **正常打开但不迁移**（迁移是调用方的显式动作） | 拒绝 `ErrSchemaOutdated`（**不迁移**） | 拒绝 `ErrSchemaOutdated`（需由重命令升级） |
| 过高版本 | `version > 支持版本` | 拒绝 `ErrSchemaTooNew` | 拒绝 `ErrSchemaTooNew` | 拒绝 `ErrSchemaTooNew`（提示升级二进制） |
| 损坏 | 表存在但**零行** | 拒绝 `ErrSchemaCorrupt` | 拒绝 `ErrSchemaCorrupt` | 拒绝 `ErrSchemaCorrupt` |

**迁移（`Migrate`）状态转换**：`v == target` → 无操作；`v > target` → 拒绝；`v < target` → 逐版 `v+1..target`，**每版** [增量 SQL + 版本记录] 同一事务；目标版本缺迁移文件 → 拒绝并点名文件（不跳过、不静默）。

**并发规则（核心不变量）**：判定与写入必须在**同一写权事务**内完成——两个 `init` 并发、或 `init` 与「已持锁但初始化事务未提交」的启动窗口并发时，竞争方取得写权后**重判**，见到版本行即以 `created = false`（Kept）成功返回；等待上限由 `busy_timeout(5000)` 兜底，超时如实报错（不吞错）。

**目标版本来源**：一律显式传参——生产装配传 `storage.SupportedSchemaVersion`，测试传合成值（覆盖单版本期不可达的旧库 / 损坏态分支）。

## 实体 4：配置（Config）

**载体**：`basalt.yaml`（与工作区**同级**，不在 `.basalt/` 内；`basalt init` 不生成实例配置，仓库只带 `basalt.yaml.example`）。

| 段 | 本 US 处理 |
| --- | --- |
| 服务段（含日志级别 / 形态；监听键字面量 `http.addr`） | **消费**：日志档位（dev 文本 / prod JSON） |
| `providers` / `sandbox` / `memory` / `budget` / `author` | **解析并保留**（结构进 `*Config`），语义校验随消费 US（US-2 / US-4 / US-6 / US-12）——不预造键 |

**取值规则**：

| 规则 | 判据 |
| --- | --- |
| `${VAR}` 占位解析 | 经注入的 `lookupEnv func(string) (string, bool)`；缺省 `os.LookupEnv` |
| 变量不存在 | 拒绝；文案点名配置键路径与变量名 |
| 凭证键解析为空（变量存在但值为空） | 拒绝（点名键路径）——与「不存在」是**两类不同拒绝** |
| 替换后仍含 `${`（模板畸形） | 拒绝；点名键路径 |
| 凭证键为明文（非 `${VAR}` 形态） | 拒绝；**文案不含明文值** |
| 必填缺失 / 类型非法 | 拒绝；不静默取默认值 |
| 输出面红线 | 错误文案、日志、标准输出、标准错误**四路**均不含凭证值 |
| 文件缺失 | `LoadConfig` 返回可判错误（`errors.Is(err, fs.ErrNotExist)`）——是否容忍由调用方定（`status` 容忍并明示路径，不计失败） |

**类型形状**：`cli.Config`（解析结果）与 `cli.Deps`（显式类型化装配参数：`WorkDir`、`SupportedSchemaVersion`、`Logger`）——均经函数参数传递，不塞 `context`（宪法 VII）。

## 实体 5：错误面（哨兵错误）

| 哨兵错误 | 语义 | 出口 |
| --- | --- | --- |
| `core.ErrSchemaTooNew` | 库版本高于二进制支持版本 | 写 / 只读 / init 引导三路均拒绝 |
| `core.ErrSchemaOutdated` | 库版本旧于目标（迁移归持锁重路径） | `Bootstrap` 与 `OpenReadOnly` 拒绝；`OpenWritable` **不**返回它（打开成功不迁移） |
| `core.ErrSchemaCorrupt` | `schema_version` 表存在但零行 | 三路均拒绝 |
| `core.ErrDatabaseMissing` | 库未就绪（未 init） | `OpenReadOnly` |
| `core.ErrWorkspaceNotInitialized` | 工作区未初始化 | `workspace.Inspect` |

**判定链**：错误经 `errors.Is` / `errors.As` 判卷（测试以此断言，文案仅面向用户）。
