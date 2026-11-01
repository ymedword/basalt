package cli_test

// 本文件判 init 的**谱系选择**：引导建的是 SQLite 源谱系（v6），不是运行期 PostgreSQL
// 谱系（v1）。US-16 把运行期换成 PG 后，`Deps.SupportedSchemaVersion` 改指 PG，init 一度
// 跟着用了同一个常数——新建工作区被写成"v6 schema、v1 版本行"，`db export` 随即以
// "源库是 v1"拒绝（T078 演练实测）。两个版本常数从此分开注入，本文件是这条判据的守卫。
// This file guards init's lineage choice: the bootstrap builds the SQLite source lineage
// (v6), never the runtime PostgreSQL one (v1).

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
)

// initStore 记录 init 交出的引导目标版本——本文件判的正是"传给引导的是哪个常数"。
// initStore records the target version init hands to the bootstrap.
type initStore struct {
	targets []int
}

func (s *initStore) Bootstrap(_ context.Context, _ string, target int) (bool, error) {
	s.targets = append(s.targets, target)
	return true, nil
}

func (s *initStore) OpenReadOnly(context.Context, cli.DatabaseConfig) (cli.SchemaView, error) {
	return nil, errors.New("init 用例不得打开运行期只读视图")
}

// TestInit_BootstrapsSourceLineage 两个版本常数分开传：引导只认源谱系（6），
// 运行期谱系（1）不得流进 init。逆改点：把 init.go 的 `deps.SourceSchemaVersion`
// 改回 `deps.SupportedSchemaVersion`，本用例立刻红在 1 != 6。
func TestInit_BootstrapsSourceLineage(t *testing.T) {
	store := &initStore{}
	var logs bytes.Buffer
	deps := cli.Deps{
		WorkDir:                t.TempDir(),
		SupportedSchemaVersion: 1, // 运行期谱系（PostgreSQL v1）
		SourceSchemaVersion:    6, // 源谱系（SQLite v6）
		Logger:                 slog.New(slog.NewTextHandler(&logs, nil)),
		Store:                  store,
		Workspace:              testWorkspace{},
	}

	root := cli.NewRootCommand(deps)
	root.SetArgs([]string{"init"})
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	require.NoError(t, root.ExecuteContext(context.Background()), "stderr: %s", stderr.String())
	require.Equal(t, []int{6}, store.targets, "init 必须用 SQLite 源谱系版本引导")
	require.Contains(t, stdout.String(), "schema v6", "输出报告的版本必须来自源谱系")
}
