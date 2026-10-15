package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	// 损坏态夹具要用裸连接造"版本表在、版本行无"的库：驱动经副作用注册。
	_ "modernc.org/sqlite"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/storage"
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

// testStore / testWorkspace 是测试侧的两个端口适配器：转调真实现包（status 的
// 三态诊断要真库才立得住），形态与装配层的适配器一致——端口的一个返回类型是
// cli 定义的接口类型，实现包命名不了它，所以适配只能在消费侧发生。
// Test-side adapters over the real implementations, shaped like the assembly
// layer's: the port returns a cli-defined interface, so the adapter belongs to
// the consumer side.
type testStore struct{}

func (testStore) Bootstrap(ctx context.Context, path string, target int) (bool, error) {
	return storage.Bootstrap(ctx, path, target)
}

func (testStore) OpenReadOnly(ctx context.Context, path string, target int) (cli.SchemaView, error) {
	db, err := storage.OpenReadOnly(ctx, path, target)
	if err != nil {
		// typed-nil：*storage.DB 装箱进接口前必须显式判空。
		return nil, err
	}
	return db, nil
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
func executeStatus(t *testing.T, workDir string, schemaVersion int) statusResult {
	t.Helper()

	var logs bytes.Buffer
	deps := cli.Deps{
		WorkDir:                workDir,
		SupportedSchemaVersion: schemaVersion,
		Logger:                 slog.New(slog.NewTextHandler(&logs, nil)),
		Store:                  testStore{},
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

// initWorkspace 在 root 下铺一个**完整**工作区（骨架 + 库）——各用例只制造一个
// 失败面，失败归属才唯一；基线不齐会让"配置无效"之类的断言混进别的失败原因。
// initWorkspace lays down a complete workspace so each case isolates one failure.
func initWorkspace(t *testing.T, root string, schemaVersion int) {
	t.Helper()

	wsRoot := filepath.Join(root, ".basalt")
	_, err := workspace.Init(wsRoot)
	require.NoError(t, err)

	_, err = storage.Bootstrap(context.Background(), filepath.Join(wsRoot, "basalt.db"), schemaVersion)
	require.NoError(t, err)
}

// writeInstanceConfig 把配置写到工作目录下的 basalt.yaml——status 节 2 读的就是
// 这个位置（与工作区**同级**、不在 .basalt/ 内，Operations §6）。
// writeInstanceConfig writes the instance configuration where section 2 reads it.
func writeInstanceConfig(t *testing.T, workDir, content string) string {
	t.Helper()

	path := filepath.Join(workDir, "basalt.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// corruptDatabase 造一个"schema_version 表在、版本行无"的库——损坏态夹具。
// corruptDatabase builds the corrupt-state fixture: the version table with no row.
func corruptDatabase(t *testing.T, path string) {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	_, err = db.Exec(`CREATE TABLE schema_version (` +
		`version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL)`)
	require.NoError(t, err)
}

// TestStatus_ReadyWithoutConfig 验收点（文档用例名同名）·验收 Demo 主路径：
// init 之后**没有配置文件**也要 exit 0——节 2 报「未发现配置（路径明示）」但不计
// 失败（评审修订 P2-5：允许无配置的诊断模式，无需配置准备步骤）。
func TestStatus_ReadyWithoutConfig(t *testing.T) {
	root := t.TempDir()
	initWorkspace(t, root, storage.SupportedSchemaVersion)

	run := executeStatus(t, root, storage.SupportedSchemaVersion)
	require.NoError(t, run.err, "无配置不得计失败；stdout=%s", run.stdout)

	require.Contains(t, run.stdout, "工作区：就绪")
	require.Contains(t, run.stdout, "未发现配置", "节 2 必须明示未发现配置")
	require.Contains(t, run.stdout, filepath.Join(root, "basalt.yaml"), "未发现配置必须点名路径")
	require.Contains(t, run.stdout, "存储：就绪")
	require.Contains(t, run.stdout, "schema v1")
	require.Contains(t, run.stdout, "11 张表", "就绪时报版本 + 表清点（流程 D 节 3）")
	require.Contains(t, run.stdout, "汇总：全部通过")

	require.NotContains(t, run.logs, "level=ERROR", "全部通过时不得有 ERROR 日志；logs=%s", run.logs)
}

// TestStatus_UninitializedWorkspace 未初始化 → 可读提示（含"先跑 basalt init"）
// + exit 1（契约 §4.3.1：只读命令只报不修——status 不代建任何东西）。
func TestStatus_UninitializedWorkspace(t *testing.T) {
	root := t.TempDir()

	run := executeStatus(t, root, storage.SupportedSchemaVersion)
	require.Error(t, run.err)

	require.Contains(t, run.stdout, "工作区：未初始化")
	require.Contains(t, run.stdout, "basalt init")
	require.Contains(t, run.stdout, "库未就绪", "节 1 失败不阻断节 3——两节各自点名")
	require.Contains(t, run.stdout, "汇总：2 节未通过")

	require.Contains(t, run.logs, "level=ERROR", "失败路径必须留痕（FR-006）；logs=%s", run.logs)
	require.NoFileExists(t, filepath.Join(root, ".basalt"), "status 不得代建工作区")
}

// TestStatus_ConfigInvalidFails 配置存在但无效 → 计失败并点名路径与键；同一次
// 报告里后续节照常给出结论（节内失败不阻断后续节）。
func TestStatus_ConfigInvalidFails(t *testing.T) {
	root := t.TempDir()
	initWorkspace(t, root, storage.SupportedSchemaVersion)
	writeInstanceConfig(t, root, `providers:
  - name: demo
    api_key: plaintext-config-credential
`)

	run := executeStatus(t, root, storage.SupportedSchemaVersion)
	require.Error(t, run.err)

	require.Contains(t, run.stdout, "配置：无效")
	require.Contains(t, run.stdout, "providers[0].api_key")
	require.Contains(t, run.stdout, filepath.Join(root, "basalt.yaml"))
	require.Contains(t, run.stdout, "存储：就绪", "节 2 失败不得阻断节 3")
	require.Contains(t, run.stdout, "汇总：1 节未通过（配置）")

	require.Contains(t, run.logs, "level=ERROR")
	require.Contains(t, run.logs, "配置无效")
}

// TestStatus_SchemaStateFails 存储三态各自点名、各自计失败（§2.3 状态矩阵的
// status 一列）：旧版本（经 Deps 注入支持版本 2 制造）/ 过高 / 损坏。
func TestStatus_SchemaStateFails(t *testing.T) {
	supported := storage.SupportedSchemaVersion

	cases := []struct {
		name     string
		prepare  func(t *testing.T, root string)
		depsVer  int
		wantText []string
	}{
		{
			name: "旧版本：库 v1 低于二进制支持的 v2",
			prepare: func(t *testing.T, root string) {
				initWorkspace(t, root, supported)
			},
			depsVer:  supported + 1,
			wantText: []string{"库版本旧于二进制", "status 只校验、不迁移"},
		},
		{
			name: "过高：库 v2 高于二进制支持的 v1",
			prepare: func(t *testing.T, root string) {
				initWorkspace(t, root, supported+1)
			},
			depsVer:  supported,
			wantText: []string{"库版本高于二进制", "请升级 basalt 二进制"},
		},
		{
			name: "损坏：版本表在但无版本行",
			prepare: func(t *testing.T, root string) {
				wsRoot := filepath.Join(root, ".basalt")
				_, err := workspace.Init(wsRoot)
				require.NoError(t, err)
				corruptDatabase(t, filepath.Join(wsRoot, "basalt.db"))
			},
			depsVer:  supported,
			wantText: []string{"库已损坏", "无版本行", "需人工判定"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.prepare(t, root)

			run := executeStatus(t, root, tc.depsVer)
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
	initWorkspace(t, root, storage.SupportedSchemaVersion)
	writeInstanceConfig(t, root, `providers:
  - name: demo
    api_key: `+plaintext+`
`)

	run := executeStatus(t, root, storage.SupportedSchemaVersion)
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
	initWorkspace(t, root, storage.SupportedSchemaVersion)
	writeInstanceConfig(t, root, `providers:
  - name: demo
    api_key: "`+secret+`
`)

	run := executeStatus(t, root, storage.SupportedSchemaVersion)
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
	initWorkspace(t, root, storage.SupportedSchemaVersion)
	writeInstanceConfig(t, root, `providers:
  - name: demo
    api_key: ${PROBE_API_KEY}
    default_model: [oops]
`)

	run := executeStatus(t, root, storage.SupportedSchemaVersion)
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
	initWorkspace(t, root, storage.SupportedSchemaVersion)
	writeInstanceConfig(t, root, `providers:
  - name: demo
    api_key: *`+alias+`
`)

	run := executeStatus(t, root, storage.SupportedSchemaVersion)
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
			initWorkspace(t, root, storage.SupportedSchemaVersion)
			writeInstanceConfig(t, root, "providers:\n  - name: demo\n    api_key: "+tc.value+"\n")

			run := executeStatus(t, root, storage.SupportedSchemaVersion)
			require.Error(t, run.err, "非字符串凭证必须判失败；stdout=%s", run.stdout)

			require.Contains(t, run.stdout, "配置：无效")
			require.Contains(t, run.stdout, "必须是 ${VAR} 形态的字符串")
			require.Contains(t, run.stdout, "汇总：1 节未通过（配置）")
			require.NotContains(t, run.stdout, tc.value, "不得回显原始值")
			require.Contains(t, run.logs, "level=ERROR")
		})
	}
}
