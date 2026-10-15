# 契约：存储能力面（`internal/storage` + `internal/core` 错误面）

**Feature**: [spec.md](../spec.md) | **Plan**: [plan.md](../plan.md) | **Date**: 2026-10-15

本文件记录 US-1 交付的存储能力面导出签名与不变量（取自 [docs/us/us1-foundation.md](../../../docs/us/us1-foundation.md) §3.1 与 §2.3 流程 B）。签名**逐字保真**——实现期不得改名或改参；表 / 列 / 状态字面量以 [docs/contracts/schema.sql](../../../docs/contracts/schema.sql) 为准。

## 导出签名

```go
// package storage

// 写模式打开（能力层）：读写池装配（写池 max 1）+ 逐物理连接 PRAGMA + WAL；
// 判定与写入同一 BEGIN IMMEDIATE 事务（取得写权后重判态）。
func OpenWritable(ctx context.Context, path string, target int) (*DB, error)

// init 引导（策略层）：OpenWritable 薄封装 + 旧库拒绝；created 语义见状态矩阵。
func Bootstrap(ctx context.Context, path string, target int) (created bool, err error)

// status 用：只读打开 + 状态矩阵校验；只校验、不迁移；不取锁、不恢复、不得动锁文件。
func OpenReadOnly(ctx context.Context, path string, target int) (*DB, error)

// 机制层：迁移是显式动作，唯一生产调用方 = US-4 持锁重路径。
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS, target int) error

// 生产装配传给上述四者的 target。
const SupportedSchemaVersion = 1

func (db *DB) SchemaVersion(ctx context.Context) (int, error)
func (db *DB) TableNames(ctx context.Context) ([]string, error)
func (db *DB) Close() error
```

```go
// package core（依赖倒置：接口定义在消费方）

type SchemaIntrospector interface { /* status 消费 / storage 实现——首个依赖倒置样例 */ }
type MetricsRegistry interface { /* 观测占位（实装属扩展阶段） */ }
type NoopMetrics struct{}

var (
    ErrSchemaTooNew            error // 库版本 > 支持版本
    ErrSchemaOutdated          error // 库版本 < 目标（迁移归持锁重路径）
    ErrSchemaCorrupt           error // schema_version 表存在但零行
    ErrDatabaseMissing         error // 库未就绪（未 init）
    ErrWorkspaceNotInitialized error // 工作区未初始化
)
```

```go
// package workspace

type InitReport struct{ /* Created / Kept 两清单 */ }
type Report struct{ /* Inspect 的只读清点结果 */ }

func Init(root string) (InitReport, error)
func Inspect(root string) (Report, error)
```

```go
// package cli

type Config struct{ /* 六段：服务段 + providers / sandbox / memory / budget / author */ }
type Deps struct{ /* WorkDir / SupportedSchemaVersion / Logger —— 显式类型化传递（宪法 VII） */ }

func NewRootCommand(deps Deps) *cobra.Command
func LoadConfig(path string, lookupEnv func(string) (string, bool)) (*Config, error)
```

```go
// package app

func Run(ctx context.Context, args []string) int // 装配骨架 + 退出码唯一出口
func NewLogger(cfg *cli.Config) *slog.Logger     // dev 文本 / prod JSON 双档
```

## 不变量（每条都是可执行判据）

| # | 不变量 | 判据 |
| --- | --- | --- |
| 1 | **判态不得早于写权**——创建 / 迁移的判定与写入在同一 `BEGIN IMMEDIATE` 事务内 | 并发 `Bootstrap` 用例：恰好一个 `created=true`，其余 Kept 且零报错，终态 11 表 + `version = target` |
| 2 | **原子性**——首次建库（全量建表 + 版本行）与每版增量（SQL + 版本记录）各自同一事务 | 失败夹具后**版本表与增量表均不存在**（非空虚断言：先构造可观察残留再断言不存在） |
| 3 | **已存在的库零写**——当前版本 Kept；旧 / 过高 / 损坏均拒绝且不迁移 | 各态断言库版本不变 |
| 4 | **只读路径不迁移** | `OpenReadOnly` 拒绝后版本不变；库缺失返回 `ErrDatabaseMissing` 且不代建 |
| 5 | **PRAGMA 逐物理连接** | 两条独立连接的 `foreign_keys=1` 与 `busy_timeout=5000` 均生效 |
| 6 | **WAL 库级持久** | 创建后回读 `wal`；重开仍为 `wal` |
| 7 | **迁移文件缺失即拒** | 目标 2 而 `0002` 缺失 → 拒绝并点名文件，版本不变 |
| 8 | **契约逐字节一致** | `internal/storage/schema.sql` 与契约文件逐字节相同（单测强制） |
| 9 | **驱动注册副作用显式** | `modernc.org/sqlite` 经 `import _` 注册（Driver 名 `sqlite`）；`cmd/basalt` 经 `import _ "time/tzdata"` 内嵌时区库 |
| 10 | **不取锁 / 不恢复 / 不触碰锁文件** | `init` / `status` 路径无锁文件交互（实例锁归 US-4） |
