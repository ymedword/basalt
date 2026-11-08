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

// installCallLog 记录一次 init 里的三个动作顺序——本文件判的正是"首次准备必须
// 排在引导建库之前"（US-25 §2.3.2 / D-25-02）。逆改点：把 init.go 的 Prepare
// 调用移到 Store.Bootstrap 之后或删掉，calls 立刻变成 workspace → bootstrap。
// installCallLog records the order of the three init actions.
type installCallLog struct {
	calls []string
}

// firstInstallStub 是既有谱系用例的最小 FirstInstall 桩：只满足端口契约，
// 不写任何文件（谱系判据只看交给 Bootstrap 的版本常数）。
// firstInstallStub is the minimal port stub for the lineage test.
type firstInstallStub struct{}

func (firstInstallStub) Prepare(string) (cli.FirstInstallReport, error) {
	return cli.FirstInstallReport{}, nil
}

// recordingWorkspace 只记录骨架初始化发生在什么时候，不在盘上写任何东西；
// 顺序判据不依赖真实工作区布局。
// recordingWorkspace only records when the skeleton step ran.
type recordingWorkspace struct{ log *installCallLog }

func (w recordingWorkspace) Init(string) (created, kept []string, err error) {
	w.log.calls = append(w.log.calls, "workspace")
	return []string{"skeleton"}, nil, nil
}

func (recordingWorkspace) Inspect(string) ([]string, error) { return nil, nil }

// recordingFirstInstall 记录 Prepare 的入参，并回报一份"全部新建"的报告——
// 输出断言据此确认命令面只报本次真的写了什么。
type recordingFirstInstall struct {
	log      *installCallLog
	workDirs []string
}

func (f *recordingFirstInstall) Prepare(workDir string) (cli.FirstInstallReport, error) {
	f.log.calls = append(f.log.calls, "prepare")
	f.workDirs = append(f.workDirs, workDir)
	return cli.FirstInstallReport{
		Fresh:                true,
		ConfigCreated:        true,
		ProvisioningRecorded: true,
		MarkerCreated:        true,
		InstanceID:           "inst_0123456789abcdef",
	}, nil
}

// orderingStore 记录引导建库发生在什么时候。
// orderingStore records when the bootstrap ran.
type orderingStore struct{ log *installCallLog }

func (s *orderingStore) Bootstrap(context.Context, string, int) (bool, error) {
	s.log.calls = append(s.log.calls, "bootstrap")
	return true, nil
}

func (s *orderingStore) OpenReadOnly(context.Context, cli.DatabaseConfig) (cli.SchemaView, error) {
	return nil, errors.New("init 用例不得打开运行期只读视图")
}

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
		FirstInstall:           firstInstallStub{},
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

// TestCLI_InitPreparesFirstInstallBeforeBootstrap 判 `init` 的三步顺序：
// 骨架 → 首次准备 → 引导建库。顺序反了（先建库后准备）会让 `detectNewInstance`
// 在准备失败时把工作区判成存量实例，首次初始化资格永久丢失（D-25-02）。
// 逆改点：把 init.go 的 `deps.FirstInstall.Prepare` 移到 `deps.Store.Bootstrap`
// 之后或删除，本用例在 calls 断言处逐字红。
func TestCLI_InitPreparesFirstInstallBeforeBootstrap(t *testing.T) {
	workDir := t.TempDir()
	log := &installCallLog{}
	install := &recordingFirstInstall{log: log}
	store := &orderingStore{log: log}
	var logs bytes.Buffer
	deps := cli.Deps{
		WorkDir:                workDir,
		SupportedSchemaVersion: 1,
		SourceSchemaVersion:    6,
		Logger:                 slog.New(slog.NewTextHandler(&logs, nil)),
		Store:                  store,
		Workspace:              recordingWorkspace{log: log},
		FirstInstall:           install,
	}

	root := cli.NewRootCommand(deps)
	root.SetArgs([]string{"init"})
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	require.NoError(t, root.ExecuteContext(context.Background()), "stderr: %s", stderr.String())
	require.Equal(t, []string{"workspace", "prepare", "bootstrap"}, log.calls,
		"init 必须先把首次准备做完再建库")
	require.Equal(t, []string{workDir}, install.workDirs,
		"首次准备收到的是命令工作目录（端口实现自行拼 .basalt）")
	require.Contains(t, stdout.String(), "首次安装：新建 配置 / 初始化进度 / 实例名册",
		"输出必须逐项报本次真的写了什么")
}

// TestCLI_InitNilFirstInstallFailsClosed 判缺件：FirstInstall 为 nil 时 `init`
// **不得**退回"只建骨架 + 库"的旧行为——那正是首次安装缺陷（有库、无配置、
// 无身份）重新出现的路径。断言引导建库一次都没发生。
// A nil port must fail closed before any bootstrap; a silent fallback is the bug.
func TestCLI_InitNilFirstInstallFailsClosed(t *testing.T) {
	store := &initStore{}
	var logs bytes.Buffer
	deps := cli.Deps{
		WorkDir:                t.TempDir(),
		SupportedSchemaVersion: 1,
		SourceSchemaVersion:    6,
		Logger:                 slog.New(slog.NewTextHandler(&logs, nil)),
		Store:                  store,
		Workspace:              testWorkspace{},
	}

	root := cli.NewRootCommand(deps)
	root.SetArgs([]string{"init"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})

	err := root.ExecuteContext(context.Background())
	require.Error(t, err, "首次安装端口缺失必须报装配错误")
	require.Contains(t, err.Error(), "FirstInstall")
	require.Empty(t, store.targets, "缺件时不得建库")
}
