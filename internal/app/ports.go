package app

import (
	"context"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// 本文件是装配层的**适配器**：把实现包（storage / workspace）接到命令面自己声明的
// 端口上（见 internal/cli/ports.go）。适配器只能放在装配层——端口的一个返回类型
// 是 cli 定义的接口类型，storage 无法命名它（那会成为反向依赖）。
// The assembly layer is the only place these adapters can live: a port returns a
// cli-defined interface type, which storage cannot name without depending back
// on cli.

// storageStore 把 storage 包适配成 cli.Store。
// storageStore adapts the storage package to cli.Store.
type storageStore struct{}

// Bootstrap 转调引导入口。
// Bootstrap forwards to the storage bootstrap entry.
func (storageStore) Bootstrap(ctx context.Context, path string, target int) (bool, error) {
	return storage.Bootstrap(ctx, path, target)
}

// OpenReadOnly 转调只读打开，并把库句柄接到只读视图端口上。
// OpenReadOnly forwards the read-only open and returns it as the view port.
func (storageStore) OpenReadOnly(ctx context.Context, path string, target int) (cli.SchemaView, error) {
	db, err := storage.OpenReadOnly(ctx, path, target)
	if err != nil {
		// typed-nil 陷阱：*storage.DB 装箱进接口时**必须**在这一步显式判空——
		// 否则得到"非空接口 + 空指针"，上层的 err != nil 判据全部失效，nil 检查
		// 形同虚设（实测形态：`if db == nil` 为 false 而方法调用 panic）。
		// A typed nil boxed into the interface is non-nil, so the error path must
		// return early here.
		return nil, err
	}
	return db, nil
}

// workspacePort 把 workspace 包适配成 cli.Workspace。
// workspacePort adapts the workspace package to cli.Workspace.
type workspacePort struct{}

// Init 转调骨架创建，报告类型收成裸切片（端口形态）。
// Init forwards the skeleton creation, flattening the report struct.
func (workspacePort) Init(root string) (created, kept []string, err error) {
	report, err := workspace.Init(root)
	if err != nil {
		return nil, nil, err
	}
	return report.Created, report.Kept, nil
}

// Inspect 转调骨架清点，只把缺失清单交回端口。
// Inspect forwards the skeleton inventory, returning the missing list only.
func (workspacePort) Inspect(root string) ([]string, error) {
	report, err := workspace.Inspect(root)
	if err != nil {
		return nil, err
	}
	return report.Missing, nil
}
