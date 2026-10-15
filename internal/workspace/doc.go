// Package workspace 管理工作区骨架：幂等创建与只读清点。
// Package workspace manages the workspace skeleton: idempotent creation and
// read-only inventory.
//
// 工作区 = 当前工作目录下的 .basalt/（目录约定见 docs/Compatibility.md §2.1）；
// 本包只碰文件系统，不碰数据库——建库归 storage.Bootstrap。
//
// The workspace is .basalt/ under the current working directory. This package
// touches the file system only; database bootstrap belongs to storage.
package workspace
