package cli

import "context"

// 本文件是命令面的**端口**（依赖倒置的接口面）：接口定义在消费方，实现由装配层
// internal/app 注入。cli 因而不 import storage / workspace——依赖方向保持
// app → cli → core，而"没有编译环"**不等于**方向合规（环只是最粗的形态；实测：
// 直接引用实现包同样让 cli 依赖具体实现，装配层就失去了替换与测试注入的接缝）。
// The command surface's ports: interfaces are defined by their consumer and the
// adapters are injected by internal/app, so cli never imports storage or
// workspace. Acyclicity alone is not the criterion — cli must not depend on the
// concrete implementations either.

// Store 是命令面对持久化基座的端口（init 引导 / status 只读诊断）。
// Store is the command surface's persistence port.
type Store interface {
	// Bootstrap 引导创建库（幂等）：created 为 true 仅当**本进程实际创建**了
	// schema——已存在的当前版本库一律 Kept 且**零写**。
	// Bootstrap provisions the database; created is true only for the process
	// that actually created the schema, and Kept means zero bytes written.
	Bootstrap(ctx context.Context, path string, target int) (created bool, err error)

	// OpenReadOnly 只读打开并做状态矩阵校验：只校验、不迁移、不取锁、不恢复；
	// 异常态经 core 的哨兵错误回报（ErrDatabaseMissing / ErrSchemaOutdated /
	// ErrSchemaTooNew / ErrSchemaCorrupt）。
	// OpenReadOnly validates and opens read-only; failure states travel as the
	// core sentinels.
	OpenReadOnly(ctx context.Context, path string, target int) (SchemaView, error)
}

// SchemaView 是只读打开的库视图——status 节 3 的只读消费面（版本 + 表清点）。
// SchemaView is the read-only database view section 3 of status consumes.
//
// 返回接口而非具体类型：接口的**返回类型也是依赖面**，返回 *storage.DB 等于把
// cli 钉回实现包，注入接缝随即消失。
// Returning an interface matters: a concrete return type would pin cli back to
// the implementation package and dissolve the seam.
type SchemaView interface {
	SchemaVersion(ctx context.Context) (int, error)
	TableNames(ctx context.Context) ([]string, error)
	Close() error
}

// Workspace 是命令面的工作区端口：骨架幂等创建（init）与骨架清点（status 节 1）。
// Workspace is the command surface's workspace port.
//
// 返回形态刻意收成**裸切片**而不用实现包的报告类型——报告类型同样是依赖面，
// 直接使用它等于绕开端口把 cli 绑回 workspace 包。
// Plain slices rather than the implementation's report structs: those types are
// part of the dependency surface too.
type Workspace interface {
	// Init 幂等创建骨架：created / kept 分别是"本进程新建"与"打开时已存在、
	// 逐字节未动"的条目清单。
	// Init creates the skeleton idempotently; a second run reports empty created.
	Init(root string) (created, kept []string, err error)

	// Inspect 清点骨架：missing 为空即齐备；未初始化（连工作区目录都没有）时
	// 返回 core.ErrWorkspaceNotInitialized。
	// Inspect inventories the skeleton; an empty missing means it is complete.
	Inspect(root string) (missing []string, err error)
}
