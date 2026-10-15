package core

import "errors"

// 哨兵错误：跨包的判定锚点。调用方一律用 errors.Is 判卷——不得比对错误文本
// （文案面向用户，措辞可改；哨兵身份不可改）。
//
// Sentinel errors are the cross-package decision anchors. Callers must use
// errors.Is: user-facing wording may change, sentinel identity may not.

var (
	// ErrSchemaTooNew 表示库内版本高于二进制支持的版本——旧二进制不得打开新库。
	// ErrSchemaTooNew means the database is newer than this binary supports;
	// an old binary must never open a newer database.
	ErrSchemaTooNew = errors.New("database schema is newer than the binary supports")

	// ErrSchemaOutdated 表示库内版本旧于目标版本——迁移是持锁重路径的显式动作，
	// 引导（Bootstrap）与只读校验（OpenReadOnly）都在此拒绝；OpenWritable 不返回它。
	// ErrSchemaOutdated means the database is older than the target version.
	// Migration is an explicit, lock-holding action, so bootstrap and read-only
	// verification refuse here while OpenWritable does not.
	ErrSchemaOutdated = errors.New("database schema is older than the target version")

	// ErrSchemaCorrupt 表示 schema_version 表存在但零行——既非新库也非任何已知版本。
	// ErrSchemaCorrupt means the schema_version table exists but holds no row:
	// neither a fresh database nor any known version.
	ErrSchemaCorrupt = errors.New("schema_version table exists but has no version row")

	// ErrDatabaseMissing 表示库文件未就绪（尚未 init）——只读路径不代建。
	// ErrDatabaseMissing means the database file is not ready (not initialized);
	// read-only paths never create it as a side effect.
	ErrDatabaseMissing = errors.New("database file does not exist")

	// ErrWorkspaceNotInitialized 表示工作区未初始化（缺 .basalt/）。
	// ErrWorkspaceNotInitialized means the workspace is not initialized.
	ErrWorkspaceNotInitialized = errors.New("workspace is not initialized")

	// ErrProviderNotConfigured 表示请求的 provider 不在显式映射表内——路由查表不中，
	// 不遍历推断、不取"唯一的那个"当默认（宪法 III）。对外映射为 NOT_CONFIGURED（US-9）。
	// ErrProviderNotConfigured means the requested provider is not in the explicit map;
	// routing never iterates and never falls back to a default.
	ErrProviderNotConfigured = errors.New("provider is not configured")
)
