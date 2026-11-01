package cli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/workspace"
)

// statusResult 是一次 status 运行的三路捕获 + 返回错误——诊断命令的可判面全在
// 这里：给用户的报告（stdout）、cobra 印的错误（stderr）、结构化日志（缓冲）。
// statusResult captures the three output tracks of one status run.
type statusResult struct {
	err    error
	stdout string
	stderr string
	logs   string
}

// statusStore 是可脚本化的存储端口替身：`status` 节 3 只消费"版本 + 表清点"两个读数，
// PostgreSQL 的真实打开与读取由装配层的集成用例判（cli 不 import 驱动）。命令面这里
// 判的是状态矩阵、文案与接线，不需要数据库。
// statusStore scripts the storage port: section 3 consumes only a version and a table
// inventory, so the command-layer tests need no real database.
type statusStore struct {
	view cli.SchemaView
	err  error
}

func (statusStore) Bootstrap(context.Context, string, int) (bool, error) {
	return false, errors.New("status 用例不得走 init 引导")
}

func (s statusStore) OpenReadOnly(context.Context, cli.DatabaseConfig) (cli.SchemaView, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.view, nil
}

// statusView 是 statusStore 交出的只读视图：各读数按用例脚本返回。
// statusView is the scripted read-only view handed out by statusStore.
type statusView struct {
	version    int
	tables     []string
	versionErr error
	tablesErr  error
	closeErr   error
	closed     bool
}

func (v *statusView) SchemaVersion(context.Context) (int, error) { return v.version, v.versionErr }

func (v *statusView) TableNames(context.Context) ([]string, error) { return v.tables, v.tablesErr }

func (v *statusView) Close() error {
	v.closed = true
	return v.closeErr
}

type testWorkspace struct{}

func (testWorkspace) Init(root string) (created, kept []string, err error) {
	report, err := workspace.Init(root)
	if err != nil {
		return nil, nil, err
	}
	return report.Created, report.Kept, nil
}

func (testWorkspace) Inspect(root string) ([]string, error) {
	report, err := workspace.Inspect(root)
	if err != nil {
		return nil, err
	}
	return report.Missing, nil
}

// executeStatus 在注入的工作目录上跑一次 `status`。
//
// 走的是真根命令（NewRootCommand + cobra 派发），不是直接调内部函数：三节报告、
// 退出码出口与 stderr 文案都属被测面，绕开 cobra 就测不到 FR-009/FR-010 的接线。
// The real root command is exercised rather than an internal function, so the
// three-section report, the exit-code path and the stderr wording are all under
// test.
func executeStatus(t *testing.T, workDir string, schemaVersion int, store cli.Store) statusResult {
	t.Helper()

	var logs bytes.Buffer
	deps := cli.Deps{
		WorkDir:                workDir,
		SupportedSchemaVersion: schemaVersion,
		Logger:                 slog.New(slog.NewTextHandler(&logs, nil)),
		Store:                  store,
		Workspace:              testWorkspace{},
	}

	root := cli.NewRootCommand(deps)
	root.SetArgs([]string{"status"})

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.ExecuteContext(context.Background())
	return statusResult{err: err, stdout: stdout.String(), stderr: stderr.String(), logs: logs.String()}
}

// initWorkspace 在 root 下铺一个**完整**工作区骨架——各用例只制造一个失败面，
// 失败归属才唯一。存储不再落工作区（运行期后端是外部 PostgreSQL），骨架之外没有
// 库文件可铺；存储节的状态由 statusStore 按用例脚本给出。
// initWorkspace lays down the workspace skeleton only; storage is external now.
func initWorkspace(t *testing.T, root string) {
	t.Helper()

	_, err := workspace.Init(filepath.Join(root, ".basalt"))
	require.NoError(t, err)
}

// statusDSN 是配置里合法的 PostgreSQL 连接串形态：status 只看它是否非空，
// 打开动作由 statusStore 脚本承担，不会真的连出去。
// statusDSN is a well-formed DSN; section 3 only checks non-emptiness here.
const statusDSN = "postgres://basalt@127.0.0.1:5432/basalt"

// writeInstanceConfig 把配置写到工作目录下的 basalt.yaml——status 节 2 读的就是
// 这个位置（与工作区**同级**、不在 .basalt/ 内，Operations §6）。
// writeInstanceConfig writes the instance configuration where section 2 reads it.
func writeInstanceConfig(t *testing.T, workDir, content string) string {
	t.Helper()

	path := filepath.Join(workDir, "basalt.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// writeDatabaseConfig 写一份只含 `database` 段的合法配置——存储节因此有 DSN 可读。
// writeDatabaseConfig writes a minimal valid configuration for section 3.
func writeDatabaseConfig(t *testing.T, workDir string) {
	t.Helper()
	writeInstanceConfig(t, workDir, "database:\n  dsn: "+statusDSN+"\n")
}

// TestStatus_ReadyWithConfig ·验收 Demo 主路径：工作区齐备 + `database.dsn` 已配置
// → 三节全部通过、exit 0；节 3 报后端、schema 版本与表清点，且视图被关闭。
func TestStatus_ReadyWithConfig(t *testing.T) {
	root := t.TempDir()
	initWorkspace(t, root)
	writeDatabaseConfig(t, root)

	target := postgres.SupportedSchemaVersion
	view := &statusView{version: target, tables: []string{"sessions", "runs", "messages"}}
	run := executeStatus(t, root, target, statusStore{view: view})
	require.NoError(t, run.err, "全部通过时 exit 0；stdout=%s", run.stdout)

	require.Contains(t, run.stdout, "工作区：就绪")
	require.Contains(t, run.stdout, "配置：已装载")
	require.Contains(t, run.stdout, fmt.Sprintf("存储：就绪（PostgreSQL，schema v%d，3 张表）", target))
	require.Contains(t, run.stdout, "汇总：全部通过")
	require.True(t, view.closed, "只读视图必须被关闭")

	require.NotContains(t, run.logs, "level=ERROR", "全部通过时不得有 ERROR 日志；logs=%s", run.logs)
}

// TestStatus_NoConfigFailsStorageSection：**没有配置文件**时节 2 报「未发现配置」
// 但不计失败；存储节没有 DSN 可读，如实报「未配置 database.dsn」并计失败。
// US-16 起运行期后端是外部 PostgreSQL：workspace-only 的诊断无法声称存储就绪，
// 也就不再把"无配置"当成全绿（P-01：`status` 改读 PG）。
// Without a configuration the storage section cannot be judged at all, so the
// command exits non-zero with an explicit missing-DSN message.
func TestStatus_NoConfigFailsStorageSection(t *testing.T) {
	root := t.TempDir()
	initWorkspace(t, root)

	run := executeStatus(t, root, postgres.SupportedSchemaVersion,
		statusStore{err: errors.New("无 DSN 时不得打开数据库")})
	require.Error(t, run.err)

	require.Contains(t, run.stdout, "工作区：就绪")
	require.Contains(t, run.stdout, "未发现配置", "节 2 必须明示未发现配置")
	require.Contains(t, run.stdout, filepath.Join(root, "basalt.yaml"), "未发现配置必须点名路径")
	require.Contains(t, run.stdout, "未配置 database.dsn", "存储节必须点名缺的键")
	require.Contains(t, run.stdout, "汇总：1 节未通过（存储）")
	require.NotContains(t, run.stdout, "无 DSN 时不得打开数据库", "缺 DSN 时根本不该尝试打开")

	require.Contains(t, run.logs, "level=ERROR", "失败路径必须留痕（FR-006）；logs=%s", run.logs)
}

// TestStatus_UninitializedWorkspace 未初始化 → 可读提示（含"先跑 basalt init"）
// + exit 1（契约 §4.3.1：只读命令只报不修——status 不代建任何东西）。
func TestStatus_UninitializedWorkspace(t *testing.T) {
	root := t.TempDir()

	run := executeStatus(t, root, postgres.SupportedSchemaVersion,
		statusStore{err: errors.New("工作区未初始化时不得打开数据库")})
	require.Error(t, run.err)

	require.Contains(t, run.stdout, "工作区：未初始化")
	require.Contains(t, run.stdout, "basalt init")
	require.Contains(t, run.stdout, "未配置 database.dsn", "节 1 失败不阻断节 3——两节各自点名")
	require.Contains(t, run.stdout, "汇总：2 节未通过")

	require.Contains(t, run.logs, "level=ERROR", "失败路径必须留痕（FR-006）；logs=%s", run.logs)
	require.NoFileExists(t, filepath.Join(root, ".basalt"), "status 不得代建工作区")
}

// TestStatus_ConfigInvalidFails 配置存在但无效 → 计失败并点名路径与键；同一次
// 报告里后续节照常给出结论（节内失败不阻断后续节）。配置装载失败意味着拿不到
// `database.dsn`，存储节因此同样计失败——两节各自点名。
func TestStatus_ConfigInvalidFails(t *testing.T) {
	root := t.TempDir()
	initWorkspace(t, root)
	writeInstanceConfig(t, root, `providers:
  - name: demo
    api_key: plaintext-config-credential
`)

	run := executeStatus(t, root, postgres.SupportedSchemaVersion,
		statusStore{err: errors.New("配置无效时不得打开数据库")})
	require.Error(t, run.err)

	require.Contains(t, run.stdout, "配置：无效")
	require.Contains(t, run.stdout, "providers[0].api_key")
	require.Contains(t, run.stdout, filepath.Join(root, "basalt.yaml"))
	require.Contains(t, run.stdout, "未配置 database.dsn", "节 2 失败不得阻断节 3")
	require.Contains(t, run.stdout, "汇总：2 节未通过（配置、存储）")

	require.Contains(t, run.logs, "level=ERROR")
	require.Contains(t, run.logs, "配置无效")
}

// TestStatus_SchemaStateFails 存储三态各自点名、各自计失败（§2.3 状态矩阵的
// status 一列）：旧版本（经 Deps 注入更高的支持版本制造）/ 过高 / 损坏。
func TestStatus_SchemaStateFails(t *testing.T) {
	supported := postgres.SupportedSchemaVersion

	cases := []struct {
		name     string
		view     *statusView
		depsVer  int
		wantText []string
	}{
		{
			name:    "旧版本：库 v1 低于二进制支持的 v2",
			view:    &statusView{version: supported},
			depsVer: supported + 1,
			wantText: []string{
				"库版本旧于二进制", "先运行 basalt db migrate",
			},
		},
		{
			name:     "过高：库 v2 高于二进制支持的 v1",
			view:     &statusView{version: supported + 1},
			depsVer:  supported,
			wantText: []string{"库版本高于二进制", "请升级 basalt 二进制"},
		},
		{
			name:    "损坏：版本表在但无版本行",
			view:    &statusView{versionErr: core.ErrSchemaCorrupt},
			depsVer: supported,
			wantText: []string{
				"库已损坏", "版本表在但无版本行", "需人工判定",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			initWorkspace(t, root)
			writeDatabaseConfig(t, root)

			run := executeStatus(t, root, tc.depsVer, statusStore{view: tc.view})
			require.Error(t, run.err)
			require.Contains(t, run.stdout, "存储：")
			for _, want := range tc.wantText {
				require.Contains(t, run.stdout, want)
			}
			require.Contains(t, run.stdout, "工作区：就绪", "存储失败不得阻断节 1 的结论")

			require.Contains(t, run.logs, "level=ERROR")
			require.Contains(t, run.logs, "存储状态异常")
		})
	}
}

// TestStatus_NoCredentialLeakInOutputs 明文凭证配置 → exit 1，且**三条输出路径**
// （logger 缓冲 / stdout / stderr）均不含凭证值——只测 err.Error() 之外的面才算
// 实测（约束 7；Operations §6）。
func TestStatus_NoCredentialLeakInOutputs(t *testing.T) {
	const plaintext = "leak-probe-credential-3f9a2b"

	root := t.TempDir()
	initWorkspace(t, root)
	writeInstanceConfig(t, root, `providers:
  - name: demo
    api_key: `+plaintext+`
`)

	run := executeStatus(t, root, postgres.SupportedSchemaVersion,
		statusStore{err: errors.New("配置无效时不得打开数据库")})
	require.Error(t, run.err)

	require.NotContains(t, run.logs, plaintext, "logger 缓冲不得出现凭证值")
	require.NotContains(t, run.stdout, plaintext, "stdout 不得出现凭证值")
	require.NotContains(t, run.stderr, plaintext, "stderr 不得出现凭证值")
	require.NotContains(t, run.err.Error(), plaintext, "返回错误文案不得出现凭证值")

	require.Contains(t, run.logs, "level=ERROR", "失败路径必须留痕——不泄露不等于不记")
}

// requireNoCredentialLeak 断言凭证值不出现在四条输出路径上——与
// TestStatus_NoCredentialLeakInOutputs 同一判据，供配置解析的两条错误路径复用。
func requireNoCredentialLeak(t *testing.T, run statusResult, secret string) {
	t.Helper()
	require.NotContains(t, run.logs, secret, "logger 缓冲不得出现凭证值")
	require.NotContains(t, run.stdout, secret, "stdout 不得出现凭证值")
	require.NotContains(t, run.stderr, secret, "stderr 不得出现凭证值")
	require.NotContains(t, run.err.Error(), secret, "返回错误文案不得出现凭证值")
}

// TestStatus_SyntaxErrorDoesNotLeakCredential：语法错误路径（P1 回归）——凭证所在
// 行的 YAML 语法错误曾把整段源码摘录（含凭证原文）带进 stdout / stderr / 日志。
// The credential used to leak through the YAML syntax-error excerpt.
func TestStatus_SyntaxErrorDoesNotLeakCredential(t *testing.T) {
	const secret = "sk-status-syntax-leak-3f9a2b"

	root := t.TempDir()
	initWorkspace(t, root)
	writeInstanceConfig(t, root, `providers:
  - name: demo
    api_key: "`+secret+`
`)

	run := executeStatus(t, root, postgres.SupportedSchemaVersion,
		statusStore{err: errors.New("配置无效时不得打开数据库")})
	require.Error(t, run.err)

	requireNoCredentialLeak(t, run, secret)
	require.Contains(t, run.logs, "level=ERROR", "失败路径必须留痕——脱敏不等于不记")
}

// TestStatus_TypeErrorDoesNotLeakResolvedCredential：类型错误路径（P1 回归）——
// 错误发生在 ${VAR} 解析**之后**，摘录里是凭证真实值；四条输出路径均不得出现。
// The excerpt would show the resolved credential value.
func TestStatus_TypeErrorDoesNotLeakResolvedCredential(t *testing.T) {
	const secret = "sk-status-type-leak-0987654321"
	t.Setenv("PROBE_API_KEY", secret)

	root := t.TempDir()
	initWorkspace(t, root)
	writeInstanceConfig(t, root, `providers:
  - name: demo
    api_key: ${PROBE_API_KEY}
    default_model: [oops]
`)

	run := executeStatus(t, root, postgres.SupportedSchemaVersion,
		statusStore{err: errors.New("配置无效时不得打开数据库")})
	require.Error(t, run.err)

	requireNoCredentialLeak(t, run, secret)
	require.Contains(t, run.logs, "level=ERROR", "失败路径必须留痕——脱敏不等于不记")
}

// TestStatus_UndefinedAliasDoesNotLeakCredential：未定义别名路径（P1 回归）——
// 库错误的**首行**带别名名（err.Error() 还附整段源码摘录），实测 stdout / stderr
// 均命中；四条输出路径都不许出现。
// The first line of the library error carries the alias name; nothing of it may
// reach any of the four output tracks.
func TestStatus_UndefinedAliasDoesNotLeakCredential(t *testing.T) {
	const alias = "SENTINEL-ALIAS-3f9a2b"

	root := t.TempDir()
	initWorkspace(t, root)
	writeInstanceConfig(t, root, `providers:
  - name: demo
    api_key: *`+alias+`
`)

	run := executeStatus(t, root, postgres.SupportedSchemaVersion,
		statusStore{err: errors.New("配置无效时不得打开数据库")})
	require.Error(t, run.err)

	requireNoCredentialLeak(t, run, alias)
	require.Contains(t, run.stdout, "配置：无效", "别名错误必须计失败而非静默")
	require.Contains(t, run.logs, "level=ERROR", "失败路径必须留痕——脱敏不等于不记")
}

// TestStatus_NonStringCredentialFailsConfigSection：非字符串凭证（P1 回归）——
// 数字 / 布尔曾被解码器转成字符串、连占位形态检查一起绕过，完整工作区下 status
// 判 exit 0（配置被接受）；现在节 2 必须判失败并点名键路径。
// Numbers and booleans used to slip through the placeholder check via decoder
// coercion; the config section must fail now.
func TestStatus_NonStringCredentialFailsConfigSection(t *testing.T) {
	cases := []struct{ name, value string }{
		{"数字", "123456789"},
		{"布尔", "true"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			initWorkspace(t, root)
			writeInstanceConfig(t, root, "providers:\n  - name: demo\n    api_key: "+tc.value+"\n")

			run := executeStatus(t, root, postgres.SupportedSchemaVersion,
				statusStore{err: errors.New("配置无效时不得打开数据库")})
			require.Error(t, run.err, "非字符串凭证必须判失败；stdout=%s", run.stdout)

			require.Contains(t, run.stdout, "配置：无效")
			require.Contains(t, run.stdout, "必须是 ${VAR} 形态的字符串")
			require.Contains(t, run.stdout, "汇总：2 节未通过（配置、存储）",
				"配置无效拿不到 DSN，存储节同轮失败")
			require.NotContains(t, run.stdout, tc.value, "不得回显原始值")
			require.Contains(t, run.logs, "level=ERROR")
		})
	}
}
