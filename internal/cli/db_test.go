package cli

// 本文件是 `db` 命令组的验收（T124 骨架 + T070 接线 / 设计 §3.1.8）：DSN 必填的分流、
// 缺 DSN 即 fail-closed 且**不回退 SQLite**、未接线子命令如实报错、迁移端口注入与配置
// 投影保真，以及 export / seal / import / verify / switch 的旗标互斥与端口实参。
// The db command group: the DSN split, fail-closed without a DSN, unwired subcommands
// reporting honestly, the migration port projection, and the flag/argument surface of
// the migration-chain subcommands.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// fakeDatabaseMigrator 是 DatabaseMigrator 端口的判卷替身：记录收到的配置与调用次数，
// 返回预置版本 / 预置错误——"命令面把哪份配置交给了实现"本身就是判据。
// The fake records what reaches the port and counts calls: the argument itself is
// part of the acceptance criteria.
type fakeDatabaseMigrator struct {
	calls   int
	got     DatabaseConfig
	version int
	err     error
}

// Migrate 记录实参并返回预置结果。
// Migrate records the argument and returns the canned result.
func (f *fakeDatabaseMigrator) Migrate(_ context.Context, database DatabaseConfig) (int, error) {
	f.calls++
	f.got = database
	return f.version, f.err
}

// fakeDatabaseMigration 是迁移链路的判卷替身。每个方法各自记账：命令面的"分流"判据
// 就是**哪一个方法被调到**（离线子命令不该被 DSN 闸门拦下，也就必须真的抵达实现侧），
// 只断言错误文案区分不出这一点。
// One counter per method: the split criterion is *which* method gets reached, which an
// error-message assertion alone cannot distinguish.
type fakeDatabaseMigration struct {
	exportCalls       int
	sealCalls         int
	importCalls       int
	verifyDumpCalls   int
	verifyTargetCalls int
	switchCalls       int

	exportReq MigrationExportRequest
	sealReq   MigrationSealRequest
	importCfg DatabaseConfig
	importReq MigrationImportRequest
	verifyReq MigrationVerifyRequest
	switchCfg DatabaseConfig
	switchReq MigrationSwitchRequest
	verifyCfg DatabaseConfig

	exportReport MigrationExportReport
	sealReport   MigrationSealReport
	importReport MigrationImportReport
	verifyReport MigrationVerifyReport
	switchReport MigrationSwitchReport

	err error
}

// Export 记录实参并返回预置导出报告。
func (f *fakeDatabaseMigration) Export(_ context.Context, req MigrationExportRequest) (MigrationExportReport, error) {
	f.exportCalls++
	f.exportReq = req
	return f.exportReport, f.err
}

// Seal 记录实参并返回预置封包报告。
func (f *fakeDatabaseMigration) Seal(_ context.Context, req MigrationSealRequest) (MigrationSealReport, error) {
	f.sealCalls++
	f.sealReq = req
	return f.sealReport, f.err
}

// Import 记录配置与实参并返回预置导入报告。
func (f *fakeDatabaseMigration) Import(_ context.Context, database DatabaseConfig, req MigrationImportRequest) (MigrationImportReport, error) {
	f.importCalls++
	f.importCfg = database
	f.importReq = req
	return f.importReport, f.err
}

// VerifyDump 记录实参并返回预置离线复核报告。
func (f *fakeDatabaseMigration) VerifyDump(_ context.Context, req MigrationVerifyRequest) (MigrationVerifyReport, error) {
	f.verifyDumpCalls++
	f.verifyReq = req
	return f.verifyReport, f.err
}

// VerifyTarget 记录配置与实参并返回预置目标库复核报告。
func (f *fakeDatabaseMigration) VerifyTarget(_ context.Context, database DatabaseConfig, req MigrationVerifyRequest) (MigrationVerifyReport, error) {
	f.verifyTargetCalls++
	f.verifyCfg = database
	f.verifyReq = req
	return f.verifyReport, f.err
}

// Switch 记录配置与实参并返回预置切换报告。
func (f *fakeDatabaseMigration) Switch(_ context.Context, database DatabaseConfig, req MigrationSwitchRequest) (MigrationSwitchReport, error) {
	f.switchCalls++
	f.switchCfg = database
	f.switchReq = req
	return f.switchReport, f.err
}

// newFakeMigration 返回"一切顺利"的默认替身：可交付、复核通过、切换写入成功。
// 需要失败形态的用例自行覆盖对应字段——默认值不该逼着每条用例都说一遍"其余正常"。
// A happy-path fake: cases override only the field they are about.
func newFakeMigration() *fakeDatabaseMigration {
	return &fakeDatabaseMigration{
		exportReport: MigrationExportReport{
			OutDir:                 "out",
			MigrationBatchID:       "batch-1",
			SourceInstanceID:       "inst-1",
			SourceInstanceIDOrigin: "roster",
			SourceSnapshotSHA256:   "sha-source",
			EntryCount:             3,
			ReviewStatus:           "not_required",
			Deliverable:            true,
		},
		sealReport: MigrationSealReport{Dir: "sealed", ReviewStatus: "sealed", Included: 2, Excluded: 1},
		importReport: MigrationImportReport{
			Tables:    []MigrationTableRows{{Table: "sessions", Rows: 2}},
			TotalRows: 2,
		},
		verifyReport: MigrationVerifyReport{Mode: "dump", OK: true},
		switchReport: MigrationSwitchReport{
			CutoverID:           "cutover-1",
			CutoverAt:           time.Date(2026, 10, 29, 1, 2, 3, 0, time.UTC),
			ManifestSHA256:      "sha-manifest",
			SourceSchemaVersion: 6,
			RollbackWindowOpen:  true,
		},
	}
}

// fakeHandoff 是 HandoffVerification 端口的判卷替身：记录收到的目录与调用次数，
// 返回预置报告 / 预置错误——"命令面把哪个目录交给了交接核对"本身就是判据。
// The fake records the directory that reaches the handoff port; that argument is part
// of the acceptance criteria.
type fakeHandoff struct {
	calls  int
	got    HandoffVerifyRequest
	report HandoffVerifyReport
	err    error
}

// VerifyHandoff 记录实参并返回预置结果。
// VerifyHandoff records the argument and returns the canned result.
func (f *fakeHandoff) VerifyHandoff(_ context.Context, req HandoffVerifyRequest) (HandoffVerifyReport, error) {
	f.calls++
	f.got = req
	return f.report, f.err
}

// newFakeHandoff 返回"交接就绪"的默认替身；需要不就绪形态的用例自行覆盖字段。
// A ready-path fake: cases override only the field they are about.
func newFakeHandoff() *fakeHandoff {
	return &fakeHandoff{report: HandoffVerifyReport{
		Dir:           "handoff-dir",
		Workspace:     "/copy-root/.basalt",
		ConfigPath:    "/copy-root/basalt.yaml",
		RosterPath:    "/copy-root/.basalt/instance.json",
		InstanceID:    "inst_fedcba9876543210",
		RosterPresent: true,
		Ready:         true,
	}}
}

// dbRun 是一次 `db ...` 运行的返回错误与三路输出（stdout / stderr / 结构化日志）。
// dbRun captures the error and the three output tracks of one run.
type dbRun struct {
	err    error
	stdout string
	stderr string
	logs   string
}

// executeDB 在给定工作目录上跑一次 `db ...`，走真根命令（注册、退出码与 stderr 文案
// 都属被测面）。Store / Workspace 故意留 nil：轻路径不开库，任何 SQLite 访问都会当场
// panic 而不是悄悄通过——"不回退 SQLite"因此不只是文案断言。
//
// 迁移与交接两个端口都注入 happy-path 替身；需要 nil 或特定失败形态的用例走
// executeDBWith / executeDBFull。
// The real root command is exercised with Store/Workspace nil on purpose: any SQLite
// access would panic instead of passing quietly.
func executeDB(t *testing.T, workDir string, migrator DatabaseMigrator, args ...string) dbRun {
	t.Helper()
	return executeDBFull(t, workDir, migrator, newFakeMigration(), newFakeHandoff(), args...)
}

// executeDBWith 与 executeDB 相同，但显式指定迁移链路端口（nil 用于验缺件分支）。
// 交接端口仍注入 happy-path 替身——本助手不改变它的缺省形态。
// Same as executeDB with an explicit migration port; the handoff port stays wired.
func executeDBWith(t *testing.T, workDir string, migrator DatabaseMigrator, migration DatabaseMigration, args ...string) dbRun {
	t.Helper()
	return executeDBFull(t, workDir, migrator, migration, newFakeHandoff(), args...)
}

// executeDBFull 显式指定两个端口（nil 用于验缺件分支）。
// Both ports are given explicitly; nil exercises a wiring-gap branch.
func executeDBFull(t *testing.T, workDir string, migrator DatabaseMigrator, migration DatabaseMigration, handoff HandoffVerification, args ...string) dbRun {
	t.Helper()

	var logs bytes.Buffer
	deps := Deps{
		WorkDir: workDir,
		// SQLite 谱系的版本（6）：`db` 命令用的是 PostgreSQL 谱系，**不得**把它当迁移目标。
		SupportedSchemaVersion: 6,
		Logger:                 slog.New(slog.NewTextHandler(&logs, nil)),
		DatabaseMigrator:       migrator,
		Migration:              migration,
		Handoff:                handoff,
	}

	root := NewRootCommand(deps)
	root.SetArgs(append([]string{"db"}, args...))

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.ExecuteContext(context.Background())
	return dbRun{err: err, stdout: stdout.String(), stderr: stderr.String(), logs: logs.String()}
}

// dsnConfig 是"配置齐备"的最小 basalt.yaml：只有 DSN 的 database 段。
const dsnConfig = "database:\n  dsn: postgres://basalt@db.example.com:5432/basalt\n"

// TestDBMigrate_RequiresDSNWithoutFallback 验收点：database.dsn 为空时 `db migrate`
// 必须非零退出、点名 database.dsn，且**不得**因为工作区里有 basalt.db 就改走 SQLite。
// 逆改点：把缺 DSN 分支改成"打开工作区 SQLite 继续跑"/返回 nil → 三条断言全红。
func TestDBMigrate_RequiresDSNWithoutFallback(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, "log:\n  level: info\n")

	// 工作区里真的放一个 SQLite 库文件：它的存在**不得**改变结论。
	sqlitePath := filepath.Join(dir, "basalt.db")
	sqliteBytes := []byte("SQLite format 3\x00")
	require.NoError(t, os.WriteFile(sqlitePath, sqliteBytes, 0o600))

	migrator := &fakeDatabaseMigrator{version: 1}
	run := executeDB(t, dir, migrator, "migrate")

	require.Error(t, run.err, "缺 database.dsn 必须 fail-closed")
	require.Contains(t, run.err.Error(), "database.dsn", "错误要点名缺的配置键")
	require.Contains(t, run.err.Error(), "不回退 SQLite", "错误要写明不回退")
	require.Zero(t, migrator.calls, "缺 DSN 时不得调用迁移端口")
	require.Empty(t, run.stdout, "失败路径不得输出成功行")
	require.Contains(t, run.logs, "db migrate 未通过", "失败路径必须留 WARN/ERROR 日志")

	after, err := os.ReadFile(sqlitePath)
	require.NoError(t, err)
	require.Equal(t, sqliteBytes, after, "SQLite 文件不得被打开改写")
}

// TestDBMigrate_PassesConfiguredDatabaseToPort 验收点：database 段逐字段进入端口
// （DSN / 池上限 / 三个超时），成功行带版本号，exit 0。
// 逆改点：漏传任一字段（如只传 DSN）→ 结构体等值断言红；不打版本行 → stdout 断言红。
func TestDBMigrate_PassesConfiguredDatabaseToPort(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, `database:
  dsn: postgres://basalt@db.example.com:5432/basalt
  max_conns: 8
  min_conns: 2
  connect_timeout_seconds: 5
  statement_timeout_seconds: 30
  lock_timeout_seconds: 3
`)

	migrator := &fakeDatabaseMigrator{version: 3}
	run := executeDB(t, dir, migrator, "migrate")

	require.NoError(t, run.err, "配置齐备时 exit 0；stderr=%s", run.stderr)
	require.Equal(t, 1, migrator.calls, "迁移恰好调用一次")
	require.Equal(t, DatabaseConfig{
		DSN:                     "postgres://basalt@db.example.com:5432/basalt",
		MaxConns:                8,
		MinConns:                2,
		ConnectTimeoutSeconds:   5,
		StatementTimeoutSeconds: 30,
		LockTimeoutSeconds:      3,
	}, migrator.got, "命令面必须把配置段原样交给实现侧")
	require.Contains(t, run.stdout, "版本 3", "成功行要报告已就绪的版本")
}

// TestDBMigrate_RefusesMissingConfig 验收点：没有 basalt.yaml 时点名路径与 database.dsn，
// 端口零调用——"没有配置"不等于"可以用缺省库"。
// 逆改点：缺文件分支返回 nil / 退化成缺省 DSN → 断言红。
func TestDBMigrate_RefusesMissingConfig(t *testing.T) {
	dir := t.TempDir()
	migrator := &fakeDatabaseMigrator{version: 1}

	run := executeDB(t, dir, migrator, "migrate")

	require.Error(t, run.err)
	require.Contains(t, run.err.Error(), "basalt.yaml", "错误要点名配置文件路径")
	require.Contains(t, run.err.Error(), "database.dsn")
	require.Zero(t, migrator.calls)
}

// TestDBMigrate_RefusesInvalidConfig 验收点：配置无效时原错误上抛且零调用端口。
// 逆改点：吞掉装载错误继续执行 → 断言红。
func TestDBMigrate_RefusesInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, "database: [\n")

	migrator := &fakeDatabaseMigrator{version: 1}
	run := executeDB(t, dir, migrator, "migrate")

	require.Error(t, run.err, "无效配置必须 fail-closed")
	require.Zero(t, migrator.calls, "配置无效时不得触碰迁移端口")
	require.Empty(t, run.stdout)
}

// TestDBMigrate_RequiresMigratorPort 验收点：端口缺件如实点名（装配缺陷不该退化成
// "命令跑了但什么也没做"）。
// 逆改点：去掉缺件分支 → 空指针 panic 或静默 0 退出。
func TestDBMigrate_RequiresMigratorPort(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)

	run := executeDB(t, dir, nil, "migrate")

	require.Error(t, run.err, "端口为 nil 时必须拒绝执行")
	require.Contains(t, run.err.Error(), "未注入", "错误文案要点名端口注入")
}

// TestDBMigrate_PropagatesPortError 验收点：迁移失败原样上抛（命令面不改写、不吞掉），
// 且不打成功行。
// 逆改点：吞掉错误返回 nil → ErrorIs 断言红。
func TestDBMigrate_PropagatesPortError(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)

	wantErr := errors.New("连接 PostgreSQL 失败: 测试注入")
	migrator := &fakeDatabaseMigrator{err: wantErr}
	run := executeDB(t, dir, migrator, "migrate")

	require.ErrorIs(t, run.err, wantErr, "实现侧错误必须原样上抛")
	require.Empty(t, run.stdout, "失败路径不得输出成功行")
}

// TestDBDSNSplit_Subcommands 逐条验收 §3.1.8 末段的分流：migrate / import / switch /
// verify --target 要求 DSN；export / verify --dump / verify --handoff 离线可用。
//
// 离线段的判据是**抵达了实现侧**（对应方法被调到），而不只是"错误文案里没有 dsn"：
// 只查文案的话，把离线子命令也接进 DSN 闸门再顺手改一句提示就能蒙混过去。
// The offline cases assert the port method was reached, not merely that the message
// omits "dsn" — a message-only check would pass on a wrongly gated command.
//
// 逆改点：把离线子命令接进 requireDatabaseDSN → 前半段断言红；把要求 DSN 的子命令
// 跳过闸门 → 后半段拿到端口调用而不是"缺 DSN"，断言红。
func TestDBDSNSplit_Subcommands(t *testing.T) {
	cases := []struct {
		name string
		args []string
		// wantDSN 为真表示被 DSN 闸门拦下；否则必须抵达 wantCall 指定的端口方法。
		wantDSN  bool
		wantCall string
	}{
		{name: "migrate", args: []string{"migrate"}, wantDSN: true},
		{name: "import", args: []string{"import", "--dump", "dump-dir"}, wantDSN: true},
		{name: "switch", args: []string{"switch", "--dump", "dump-dir"}, wantDSN: true},
		{name: "verify-target", args: []string{"verify", "--target"}, wantDSN: true},
		{name: "export", args: []string{"export", "--workspace", "ws", "--out", "out"}, wantCall: "export"},
		{name: "verify-dump", args: []string{"verify", "--dump", "dump-dir"}, wantCall: "verify-dump"},
		{name: "verify-handoff", args: []string{"verify", "--handoff", "handoff-dir"}, wantCall: "handoff"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// 配置有效但没有 database 段：分流的判据只能是子命令，不是别处。
			writeProviderConfig(t, dir, "log:\n  level: info\n")

			fake := newFakeMigration()
			handoff := newFakeHandoff()
			run := executeDBFull(t, dir, &fakeDatabaseMigrator{version: 1}, fake, handoff, tc.args...)

			if tc.wantDSN {
				require.Error(t, run.err)
				require.Contains(t, run.err.Error(), "database.dsn", "%s 必须要求 DSN", tc.name)
				require.Equal(t, 0, portCalls(fake), "%s 缺 DSN 时不得抵达实现侧", tc.name)
				require.Zero(t, handoff.calls, "%s 缺 DSN 时不得抵达交接端口", tc.name)
				return
			}
			require.NotContains(t, errorText(run.err), "database.dsn", "%s 离线可用，不得要求 DSN", tc.name)
			switch tc.wantCall {
			case "export":
				require.Equal(t, 1, fake.exportCalls, "export 必须抵达实现侧")
				require.NoError(t, run.err)
			case "verify-dump":
				require.Equal(t, 1, fake.verifyDumpCalls, "verify --dump 必须抵达实现侧")
				require.NoError(t, run.err)
			case "handoff":
				require.Equal(t, 1, handoff.calls, "verify --handoff 必须抵达交接端口")
				require.Equal(t, "handoff-dir", handoff.got.Dir, "命令面必须把 --handoff 原值交给端口")
				require.NoError(t, run.err)
			default:
				require.Failf(t, "未知 wantCall", "%s", tc.name)
			}
		})
	}
}

// portCalls 汇总替身被调用的总次数。
func portCalls(fake *fakeDatabaseMigration) int {
	return fake.exportCalls + fake.sealCalls + fake.importCalls +
		fake.verifyDumpCalls + fake.verifyTargetCalls + fake.switchCalls
}

// errorText 把可能为 nil 的错误折成字符串，便于统一做 NotContains 判定。
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestDBVerify_RequiresExactlyOneMode 验收点：verify 的两种模式（--dump，可加 --target
// 修饰；--handoff）恰好选一种——不带模式或 handoff 与其他混用都给固定错误，而不是被
// 误报成"缺 DSN"。`--dump` + `--target` 是**合法**组合（T070 语义），本用例不再把它当
// 冲突项。
// 逆改点：删掉模式计数 → 无模式时落到"缺 DSN"，断言红。
func TestDBVerify_RequiresExactlyOneMode(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)

	for _, args := range [][]string{
		{"verify"},
		{"verify", "--dump", "dump-dir", "--handoff", "handoff-dir"},
		{"verify", "--handoff", "handoff-dir", "--target"},
	} {
		run := executeDB(t, dir, nil, args...)
		require.Error(t, run.err)
		require.Contains(t, run.err.Error(), "必须且只能指定", "args=%v", args)
		require.NotContains(t, run.err.Error(), "database.dsn", "模式判定必须先于 DSN 判定，args=%v", args)
	}
}

// TestDB_UnwiredSubcommandsNameOwner 验收点：**不得**假装成功。五条子命令全部接线后，
// 本用例守的是最后一道缺件口：`db verify --handoff` 在装配层没注入交接端口时必须非零
// 退出、无输出，并点名缺的是哪个端口、由谁提供——而不是静默成功或空指针 panic。
//
// 函数名保留（stage 的 required_tests 绑定该名，改名属判据面变更）；语义从"子命令停在
// 未接线档、点名归属任务"收窄为"端口缺件、点名装配层"。T142 落地前的前半段语义已由
// `TestDBDSNSplit_Subcommands/verify-handoff` 的"必须抵达实现侧"接管。
// The name is kept for the stage's required_tests; the gap it guards is now the missing
// handoff port rather than an unwired subcommand.
//
// 逆改点：缺件分支返回 nil / 用空报告顶替 → "不得假装成功"与"不得触碰端口"断言红。
func TestDB_UnwiredSubcommandsNameOwner(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)

	fake := newFakeMigration()
	run := executeDBFull(t, dir, &fakeDatabaseMigrator{version: 1}, fake, nil,
		"verify", "--handoff", "handoff-dir")

	require.Error(t, run.err, "端口缺件时不得假装成功")
	require.Contains(t, run.err.Error(), "交接核对端口未注入", "错误要点名缺的端口")
	require.Contains(t, run.err.Error(), "HandoffVerification", "错误要点名装配层该注入什么")
	require.Empty(t, run.stdout, "缺件不得产生输出")
	require.Zero(t, portCalls(fake), "缺件不得触碰迁移链路端口")
	require.Contains(t, run.logs, "交接核对端口未注入", "缺件路径必须留 ERROR 日志")
}

// TestDBVerifyHandoff_PassesDirAndRendersReport 验收点：`db verify --handoff` 把
// `--handoff` 原值交给端口，并就绪报告逐项渲染——名册、记录、密钥、目标配置核对与
// 两组凭据计数都打出来；"已核对"与"未核对"的缺省读法不得混淆。
// 逆改点：漏传 Dir → 实参断言红；跳过 printHandoffReport → stdout 断言红；把两组计数
// 相加成"总数" → 分组断言红。
func TestDBVerifyHandoff_PassesDirAndRendersReport(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, "log:\n  level: info\n")

	handoff := newFakeHandoff()
	handoff.report = HandoffVerifyReport{
		Dir:                 "copy/.basalt",
		Workspace:           "/copy/.basalt",
		ConfigPath:          "/copy/basalt.yaml",
		RosterPath:          "/copy/.basalt/instance.json",
		InstanceID:          "inst_fedcba9876543210",
		RosterPresent:       true,
		RecordPresent:       true,
		RecordKeyID:         "kid-1",
		RecordFingerprint:   "fp-1",
		KeyFile:             "/copy/master.key",
		KeyPresent:          true,
		KeyPathPersisted:    true,
		KeyID:               "kid-1",
		Fingerprint:         "fp-1",
		ConfigRequired:      true,
		ConfigChecked:       true,
		TargetConfigSHA256:  "sha-target",
		CurrentConfigSHA256: "sha-target",
		Credentials:         SecretsVerifyReport{Checked: 2, CurrentChecked: 1},
		Ready:               true,
	}
	run := executeDBFull(t, dir, nil, newFakeMigration(), handoff,
		"verify", "--handoff", "copy/.basalt")

	require.NoError(t, run.err)
	require.Equal(t, 1, handoff.calls)
	require.Equal(t, "copy/.basalt", handoff.got.Dir, "命令面必须把 --handoff 原值交给端口")
	require.Contains(t, run.stdout, "交接就绪", "就绪结论必须打印")
	require.Contains(t, run.stdout, "目标配置核对：已核对", "已核对必须显式打印")
	require.Contains(t, run.stdout, "路径已持久化：是")
	require.Contains(t, run.stdout, "历史密文：检查 2 条，失败 0 条", "两组计数分别打印、不相加")
	require.Contains(t, run.stdout, "当前可用性：检查 1 条，失败 0 条")
}

// TestDB_SubcommandsRegistered 验收点：根命令注册了 db，db 带五条子命令——骨架的
// 形状就是设计表里的五行，少一条即红。
// 逆改点：漏注册任一子命令 → ElementsMatch 断言红。
func TestDB_SubcommandsRegistered(t *testing.T) {
	root := NewRootCommand(Deps{Logger: quietLogger()})

	var db *cobra.Command
	for _, sub := range root.Commands() {
		if sub.Name() == "db" {
			db = sub
		}
	}
	require.NotNil(t, db, "根命令必须注册 db")

	names := make([]string, 0, len(db.Commands()))
	for _, sub := range db.Commands() {
		names = append(names, sub.Name())
	}
	require.ElementsMatch(t, []string{"migrate", "export", "import", "verify", "switch"}, names)
}

// TestDB_ShowsHelpWithoutArguments 验收点：`basalt db` 打印帮助并 exit 0（命令组本身
// 不是错误），帮助里列出五条子命令。
// 逆改点：RunE 改成返回错误 → NoError 断言红。
func TestDB_ShowsHelpWithoutArguments(t *testing.T) {
	run := executeDB(t, t.TempDir(), nil)
	require.NoError(t, run.err, "不带子命令时展示帮助而非报错")
	for _, name := range []string{"migrate", "export", "import", "verify", "switch"} {
		require.Contains(t, run.stdout, name)
	}
}

// TestDB_UnknownSubcommandFails 验收点：未知子命令非零退出（与根命令同一口径，不被
// cobra 当成"请求帮助"静默吞掉）。
// 逆改点：RunE 忽略多余参数直接 cmd.Help() → 断言红。
func TestDB_UnknownSubcommandFails(t *testing.T) {
	for _, args := range [][]string{{"bogus"}, {"migrate", "extra"}} {
		run := executeDB(t, t.TempDir(), nil, args...)
		require.Error(t, run.err, "args=%v", args)
	}
}

// ─────────────────────────────── db export ───────────────────────────────

// TestExport_RefusesBothOutAndSeal 验收点：`--out` 与 `--seal` 必须且只能二选一——
// 两者都不给或都给都在**任何端口调用之前**被拒（"这次到底盘点没盘点工作区"不能
// 变成要读实现才知道的事）。
// 逆改点：删掉互斥判定 → 两个子例都变成"抵达端口"，断言红。
func TestExport_RefusesBothOutAndSeal(t *testing.T) {
	dir := t.TempDir()

	for _, args := range [][]string{
		{"export"},
		{"export", "--out", "out", "--seal", "sealed"},
	} {
		fake := newFakeMigration()
		// 端口传 nil：互斥判定必须先于缺件判定，否则本用例测不到"旗标先于接线"。
		run := executeDBWith(t, dir, nil, fake, args...)

		require.Error(t, run.err, "args=%v", args)
		require.Contains(t, run.err.Error(), "必须且只能指定", "args=%v", args)
		require.Contains(t, run.err.Error(), "--out", "args=%v", args)
		require.Contains(t, run.err.Error(), "--seal", "args=%v", args)
		require.Zero(t, portCalls(fake), "旗标冲突时不得抵达端口，args=%v", args)
	}
}

// TestExport_SealRejectsWorkspace 验收点：`--seal` 不接受 `--workspace`——封包只读
// 导出目录与处置文件，混用会让两种语义同时成立。拒绝时零端口调用。
// 逆改点：允许混用并静默忽略 workspace → 断言红。
func TestExport_SealRejectsWorkspace(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeMigration()

	run := executeDBWith(t, dir, nil, fake, "export", "--seal", "sealed", "--workspace", "ws")

	require.Error(t, run.err)
	require.Contains(t, run.err.Error(), "不接受 --workspace")
	require.Zero(t, fake.sealCalls, "被拒时不得封包")
}

// TestExport_RejectsReviewFileWithoutSeal 验收点：`--review-file` 只与 `--seal` 搭配；
// 导出阶段没有消费者，静默忽略会让运维以为处置结论生效了。
// 逆改点：忽略该旗标继续导出 → 断言红。
func TestExport_RejectsReviewFileWithoutSeal(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeMigration()

	run := executeDBWith(t, dir, nil, fake, "export", "--out", "out", "--review-file", "review.json")

	require.Error(t, run.err)
	require.Contains(t, run.err.Error(), "--review-file")
	require.Contains(t, run.err.Error(), "--seal")
	require.Zero(t, portCalls(fake), "被拒时不得导出")
}

// TestExport_RequiresMigrationPort 验收点：端口缺件如实点名，不自造第二条导出路径。
// 逆改点：去掉缺件分支 → 空指针 panic 或静默 0 退出。
func TestExport_RequiresMigrationPort(t *testing.T) {
	dir := t.TempDir()

	run := executeDBWith(t, dir, nil, nil, "export", "--out", "out")

	require.Error(t, run.err)
	require.Contains(t, run.err.Error(), "未注入")
	require.Contains(t, run.err.Error(), "DatabaseMigration")
}

// TestExport_PassesExplicitWorkspaceAndOut 验收点：`--workspace` / `--out` 原样进入
// 端口（命令面不做路径改写），成功时 exit 0 并打印三元组与"可交付"。
// 逆改点：忽略 --workspace 改用缺省值 → 请求体等值断言红。
func TestExport_PassesExplicitWorkspaceAndOut(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeMigration()

	run := executeDBWith(t, dir, nil, fake, "export", "--workspace", "ws", "--out", "out")

	require.NoError(t, run.err, "可交付时必须 exit 0；stderr=%s", run.stderr)
	require.Equal(t, MigrationExportRequest{Workspace: "ws", Out: "out"}, fake.exportReq)
	require.Contains(t, run.stdout, "batch-1", "成功行要报告批次号")
	require.Contains(t, run.stdout, "sha-source", "成功行要报告源快照摘要")
	require.Contains(t, run.stdout, "可交付")
}

// TestExport_DefaultsWorkspaceToWorkDirBasalt 验收点：不给 `--workspace` 时缺省为
// `<WorkDir>/.basalt`，与 status / init / profile 同一路径口径。
// 逆改点：缺省成空串或当前目录 → 请求体断言红。
func TestExport_DefaultsWorkspaceToWorkDirBasalt(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeMigration()

	run := executeDBWith(t, dir, nil, fake, "export", "--out", "out")

	require.NoError(t, run.err)
	require.Equal(t, filepath.Join(dir, workspaceDirName), fake.exportReq.Workspace,
		"缺省工作区必须是 <WorkDir>/.basalt")
}

// TestExport_PendingIsNotDeliverable 验收点：`pending` 的核查包**不可交付**——命令
// 必须非零退出（退出码要能表达"这不是迁移包"），同时把下一步写清楚。
// 逆改点：去掉 `!report.Deliverable` 分支 → NoError 断言红（核查包被当成成功）。
func TestExport_PendingIsNotDeliverable(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeMigration()
	fake.exportReport.Deliverable = false
	fake.exportReport.ReviewStatus = "pending"
	fake.exportReport.UnclassifiedCount = 2
	fake.exportReport.UnclassifiedDigest = "sha-unclassified"

	run := executeDBWith(t, dir, nil, fake, "export", "--out", "out")

	require.Error(t, run.err, "核查包不是迁移包，必须非零退出")
	require.Contains(t, run.err.Error(), "不可交付")
	require.Contains(t, run.stdout, "不可交付", "输出要把结论说全，不能只靠退出码")
	require.Contains(t, run.stdout, "sha-unclassified", "未归类摘要要打出来供对账")
	require.Contains(t, run.stdout, "db export --seal", "要给出下一步")
	require.Contains(t, run.logs, "db export 完成", "完成事实要留日志")
}

// TestExport_PropagatesPortError 验收点：导出失败原样上抛，不打"可交付"。
// 逆改点：吞掉错误返回 nil → ErrorIs 断言红。
func TestExport_PropagatesPortError(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeMigration()
	wantErr := errors.New("主密钥文件缺失: 测试注入")
	fake.err = wantErr

	run := executeDBWith(t, dir, nil, fake, "export", "--out", "out")

	require.ErrorIs(t, run.err, wantErr)
	require.NotContains(t, run.stdout, "可交付")
}

// TestSeal_PassesDirAndReviewFile 验收点：`--seal` 把目录与处置文件原样交给端口，
// 打印最终核查状态与三类处置条数。
// 逆改点：漏传 --review-file → 请求体等值断言红；不打印三类条数 → stdout 断言红。
func TestSeal_PassesDirAndReviewFile(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeMigration()

	run := executeDBWith(t, dir, nil, fake, "export", "--seal", "sealed", "--review-file", "review.json")

	require.NoError(t, run.err, "stderr=%s", run.stderr)
	require.Equal(t, MigrationSealRequest{Dir: "sealed", ReviewFile: "review.json"}, fake.sealReq)
	require.Zero(t, fake.exportCalls, "封包形态不得再走导出")
	require.Contains(t, run.stdout, "封包目录：sealed")
	require.Contains(t, run.stdout, "核查状态：sealed")
	require.Contains(t, run.stdout, "进包 2 / 排除 1 / 移交 0")
	require.Contains(t, run.logs, "db export --seal 完成")
}

// ─────────────────────────────── db import ───────────────────────────────

// TestImport_RequiresDump 验收点：缺 `--dump` 在 DSN 与端口判定之后被点名，且零导入。
// 逆改点：缺 --dump 时仍调用端口 → importCalls 断言红。
func TestImport_RequiresDump(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)
	fake := newFakeMigration()

	run := executeDBWith(t, dir, nil, fake, "import")

	require.Error(t, run.err)
	require.Contains(t, run.err.Error(), "--dump")
	require.Zero(t, fake.importCalls)
}

// TestImport_PassesConfigAndDump 验收点：配置段与包目录原样进入端口，逐表行数与合计
// 打印到输出。
// 逆改点：只传 DSN 不传包目录 → 实参断言红；不打印合计 → stdout 断言红。
func TestImport_PassesConfigAndDump(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)
	fake := newFakeMigration()

	run := executeDBWith(t, dir, nil, fake, "import", "--dump", "dump-dir")

	require.NoError(t, run.err, "stderr=%s", run.stderr)
	require.Equal(t, 1, fake.importCalls)
	require.Equal(t, "postgres://basalt@db.example.com:5432/basalt", fake.importCfg.DSN)
	require.Equal(t, MigrationImportRequest{Dump: "dump-dir"}, fake.importReq)
	require.Contains(t, run.stdout, "sessions")
	require.Contains(t, run.stdout, "合计 1 表 / 2 行")
}

// TestImport_PropagatesPortError 验收点：导入失败原样上抛（不得因为"导了一半"就报成功）。
// 逆改点：吞掉错误返回 nil → ErrorIs 断言红。
func TestImport_PropagatesPortError(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)
	fake := newFakeMigration()
	fake.importReport = MigrationImportReport{}
	wantErr := errors.New("目标 schema 非空: 测试注入")
	fake.err = wantErr

	run := executeDBWith(t, dir, nil, fake, "import", "--dump", "dump-dir")

	require.ErrorIs(t, run.err, wantErr)
	require.Empty(t, run.stdout, "未导入任何表时不得输出表格")
	require.Contains(t, run.logs, "db import 未通过")
}

// ─────────────────────────────── db verify ───────────────────────────────

// TestVerify_TargetRequiresDump 验收点：`--target` 是 `--dump` 的**修饰符**——清单是
// 目标库复核的唯一基准，单独出现时报"必须同时给出 --dump"，而不是去连库。
// 逆改点：把 --target 当成独立模式（连库比对空清单）→ 断言红。
func TestVerify_TargetRequiresDump(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)
	fake := newFakeMigration()

	run := executeDBWith(t, dir, nil, fake, "verify", "--target")

	require.Error(t, run.err)
	require.Contains(t, run.err.Error(), "必须同时给出 --dump")
	require.Zero(t, portCalls(fake), "缺基准时不得抵达端口")
}

// TestVerify_TargetRequiresDSNBeforeDump 验收点：`--target` 既缺 DSN 又缺 `--dump` 时
// **先报 DSN**——没有配置的库连"复核哪个目标"都不成立，先喊缺 --dump 会把人引向
// 错误的下一步。
// 逆改点：把 --dump 判定提前到 DSN 之前 → 断言红。
func TestVerify_TargetRequiresDSNBeforeDump(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, "log:\n  level: info\n")
	fake := newFakeMigration()

	run := executeDBWith(t, dir, nil, fake, "verify", "--target")

	require.Error(t, run.err)
	require.Contains(t, run.err.Error(), "database.dsn")
	require.NotContains(t, run.err.Error(), "必须同时给出 --dump", "DSN 判定先于 --dump 判定")
	require.Zero(t, portCalls(fake))
}

// TestVerify_DumpStaysOffline 验收点：`--dump` 不带 `--target` 时只走离线复核端口，
// 不装载配置、不碰目标库，并打印逐表读数与结论。
// 逆改点：把 --dump 也接进 VerifyTarget → verifyTargetCalls 断言红。
func TestVerify_DumpStaysOffline(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, "log:\n  level: info\n")
	fake := newFakeMigration()
	fake.verifyReport = MigrationVerifyReport{
		Mode:   "dump",
		OK:     true,
		Tables: []MigrationTableCheck{{Table: "sessions", SourceRows: 4, TargetRows: 4, RowDigestOK: true}},
	}

	run := executeDBWith(t, dir, nil, fake, "verify", "--dump", "dump-dir")

	require.NoError(t, run.err, "stderr=%s", run.stderr)
	require.Equal(t, 1, fake.verifyDumpCalls)
	require.Zero(t, fake.verifyTargetCalls, "不带 --target 不得复核目标库")
	require.Equal(t, MigrationVerifyRequest{Dump: "dump-dir"}, fake.verifyReq)
	require.Contains(t, run.stdout, "复核模式：dump")
	require.Contains(t, run.stdout, "源 4 行 / 目标 4 行，行摘要相符")
	require.Contains(t, run.stdout, "结论：通过")
}

// TestVerify_TargetPassesConfigToPort 验收点：`--dump` + `--target` 走目标库端口，
// 配置段与请求体原样下传；不通过时逐条打印差异并原样上抛实现侧错误。
// 逆改点：漏传配置 / 把 Target 留在 false → 断言红。
func TestVerify_TargetPassesConfigToPort(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)
	fake := newFakeMigration()
	wantErr := errors.New("verify_failed: 表 sessions 缺 1 行")
	fake.err = wantErr
	fake.verifyReport = MigrationVerifyReport{
		Mode:              "target",
		OK:                false,
		Tables:            []MigrationTableCheck{{Table: "sessions", SourceRows: 4, TargetRows: 3}},
		MissingIDs:        []string{"sessions: abc-1"},
		ReferenceFailures: []string{"sessions.agent_id → agents.id: xyz"},
	}

	run := executeDBWith(t, dir, nil, fake, "verify", "--dump", "dump-dir", "--target")

	require.ErrorIs(t, run.err, wantErr, "实现侧错误必须原样上抛")
	require.Equal(t, 1, fake.verifyTargetCalls)
	require.Zero(t, fake.verifyDumpCalls, "带 --target 时不得只做离线复核")
	require.Equal(t, MigrationVerifyRequest{Dump: "dump-dir", Target: true}, fake.verifyReq)
	require.Equal(t, "postgres://basalt@db.example.com:5432/basalt", fake.verifyCfg.DSN)
	require.Contains(t, run.stdout, "行摘要**不符**")
	require.Contains(t, run.stdout, "sessions: abc-1")
	require.Contains(t, run.stdout, "结论：**不通过**")
}

// TestVerify_RefusesFailedVerdictWithoutError 验收点：端口返回"不通过"却**没带错误**
// 时，命令面仍须非零退出——否则"结论：**不通过**"配上 exit 0 会被脚本读成通过。
// 这是端口契约违反时的兜底，不是替代实现侧的判定。
// 逆改点：删掉 verifyResult 的 OK 分支 → NoError 断言红（典型"报绿而实际没通过"）。
func TestVerify_RefusesFailedVerdictWithoutError(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, "log:\n  level: info\n")
	fake := newFakeMigration()
	fake.verifyReport = MigrationVerifyReport{Mode: "dump", OK: false}

	run := executeDBWith(t, dir, nil, fake, "verify", "--dump", "dump-dir")

	require.Error(t, run.err, "不通过的结论不得以 0 退出")
	require.Contains(t, run.err.Error(), "fail-closed")
	require.Contains(t, run.stdout, "结论：**不通过**", "读数仍要打出来")
	require.Contains(t, run.logs, "db verify --dump 未通过")
}

// TestDBVerifyHandoff_RefusesNotReadyVerdictWithoutError 验收点：端口返回"不就绪"却
// **没带错误**时，命令面仍须非零退出——否则"结论：**不就绪**"配上 exit 0 会被脚本读成
// 通过。不就绪读数（稳定码 / 原因 / 恢复指引）仍要打出来，运维才知道停在哪一项。
// 逆改点：删掉 handoffResult 的 Ready 判定 → NoError 断言红（"报绿而实际没通过"）。
func TestDBVerifyHandoff_RefusesNotReadyVerdictWithoutError(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, "log:\n  level: info\n")

	handoff := newFakeHandoff()
	handoff.report = HandoffVerifyReport{
		Dir:           "copy/.basalt",
		RosterPresent: true,
		InstanceID:    "inst_fedcba9876543210",
		Code:          "instance_identity_mismatch",
		Detail:        "主密钥记录与名册不符",
		Recovery:      "用名册对应的主密钥重做交接副本",
		Ready:         false,
	}

	run := executeDBFull(t, dir, nil, newFakeMigration(), handoff,
		"verify", "--handoff", "copy/.basalt")

	require.Error(t, run.err, "不就绪的结论不得以 0 退出")
	require.Contains(t, run.err.Error(), "fail-closed")
	require.Contains(t, run.stdout, "结论：**不就绪**", "读数仍要打出来")
	require.Contains(t, run.stdout, "instance_identity_mismatch")
	require.Contains(t, run.stdout, "原因：主密钥记录与名册不符")
	require.Contains(t, run.stdout, "恢复指引：用名册对应的主密钥重做交接副本")
	require.Contains(t, run.logs, "db verify --handoff 未通过")
}

// ─────────────────────────────── db switch ───────────────────────────────

// TestSwitch_RequiresDump 验收点：缺 `--dump` 在 DSN 与端口判定之后被点名，且零写入。
// 逆改点：缺 --dump 时仍调用端口 → switchCalls 断言红。
func TestSwitch_RequiresDump(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)
	fake := newFakeMigration()

	run := executeDBWith(t, dir, nil, fake, "switch")

	require.Error(t, run.err)
	require.Contains(t, run.err.Error(), "--dump")
	require.Zero(t, fake.switchCalls)
}

// TestSwitch_PassesConfigDumpAndWorkspace 验收点：配置段 / 包目录 / 源工作区原样进入
// 端口；`--workspace` 为必填项，输出声明"源端重盘已执行"。
// 逆改点：丢掉 Workspace → 实参断言红；不声明重盘已执行 → stdout 断言红。
func TestSwitch_PassesConfigDumpAndWorkspace(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)
	fake := newFakeMigration()

	run := executeDBWith(t, dir, nil, fake, "switch", "--dump", "dump-dir", "--workspace", "ws")

	require.NoError(t, run.err, "stderr=%s", run.stderr)
	require.Equal(t, 1, fake.switchCalls)
	require.Equal(t, "postgres://basalt@db.example.com:5432/basalt", fake.switchCfg.DSN)
	require.Equal(t, MigrationSwitchRequest{Dump: "dump-dir", Workspace: "ws"}, fake.switchReq)
	require.Contains(t, run.stdout, "切换记录：cutover-1")
	require.Contains(t, run.stdout, "绑定清单 SHA-256：sha-manifest")
	require.Contains(t, run.stdout, "源端重盘：已执行")
}

// TestSwitch_DeclaresWhatItDidNotCheck 验收点（2026-10-31 判据收紧，FR-092）：
// `--workspace` 由「可选、不给就声明未执行」收紧为**必填**——缺失时在调用端口之前
// 拒绝，不打印任何切换记录；成功路径仍**必须**打印否定事实：handoff 门禁不在本命令
// 内、从未被核对，并指向真正跑该门禁的命令。
// 历史：本用例原名判「不给 --workspace 时声明未执行」；按 FR-092 收紧后改为拒绝路径 + 成功
// 路径的 handoff 否定事实，名字保留以维持 stage 的 required_tests 映射。
// 逆改点：恢复"缺 workspace 继续" → 错误消失、switchCalls 非零；删掉 handoff 声明 →
// 成功路径断言红；把 handoff 写成"已核对" → 断言红。
func TestSwitch_DeclaresWhatItDidNotCheck(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)
	fake := newFakeMigration()

	run := executeDBWith(t, dir, nil, fake, "switch", "--dump", "dump-dir")
	require.Error(t, run.err, "缺 --workspace 必须拒绝")
	require.Contains(t, run.err.Error(), "--workspace")
	require.Contains(t, run.err.Error(), "FR-092")
	require.Zero(t, fake.switchCalls, "缺必填参数不得触达端口")
	require.Empty(t, run.stdout, "拒绝路径不得打印切换记录")

	// 两个必填项都缺时先点名 --dump（校验顺序），但无论先报哪个都必须零调用——
	// 拒绝理由的顺序不是契约，fail-closed 才是。
	both := executeDBWith(t, dir, nil, fake, "switch")
	require.Error(t, both.err, "缺 --dump 与 --workspace 都必须拒绝")
	require.Zero(t, fake.switchCalls, "任何必填项缺失都不得触达端口")
	require.Empty(t, both.stdout, "拒绝路径不得打印切换记录")

	ok := executeDBWith(t, dir, nil, fake, "switch", "--dump", "dump-dir", "--workspace", "ws")
	require.NoError(t, ok.err, "stderr=%s", ok.stderr)
	require.Contains(t, ok.stdout, "源端重盘：已执行")
	require.Contains(t, ok.stdout, "交接就绪（handoff）：**未核对**")
	require.Contains(t, ok.stdout, "db verify --handoff", "否定事实必须给出可执行的下一步")
	require.NotContains(t, ok.stdout, "handoff）：已核对", "不得声称未执行的门禁已核对")
}

// TestSwitch_PrintsMemoryImportReconciliation 验收点：旧记忆导入的四类条数逐项打出来，
// 跳过条目附一句"重复导入幂等"的说明——只写"导入成功"会让运维以为跳过是失败。
// 逆改点：不打印记忆块 → 条数断言红；把跳过说成失败或相反 → 说明句断言红。
func TestSwitch_PrintsMemoryImportReconciliation(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)
	fake := newFakeMigration()
	fake.switchReport.Memory = MigrationMemoryImport{Imported: 2, Skipped: 1, Total: 3}

	run := executeDBWith(t, dir, nil, fake, "switch", "--dump", "dump-dir", "--workspace", "ws")

	require.NoError(t, run.err, "stderr=%s", run.stderr)
	require.Contains(t, run.stdout, "旧记忆导入：源条目 3 条（导入 2 / 跳过 1 / 冲突 0 / 不可读 0）")
	require.Contains(t, run.stdout, "跳过 = 权威库里已有同一条（重复导入幂等），不是失败")
	require.NotContains(t, run.stdout, "空载体")
}

// TestSwitch_PrintsEmptyCarrierAsNonEvidence 验收点：**空载体必须显式声明它不是迁移
// 成功的证据**——源里一条旧记忆都没有时，报告全零与"导入成功但没东西可导"在数字上
// 完全一样；不点破这一层，读数会被当成"记忆已经迁好了"。
// 逆改点：删掉 Total == 0 分支 → "不是「迁移成功」的证据"断言红（空载体悄悄报绿）。
func TestSwitch_PrintsEmptyCarrierAsNonEvidence(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)
	fake := newFakeMigration() // 缺省 switchReport.Memory 即四类全零

	run := executeDBWith(t, dir, nil, fake, "switch", "--dump", "dump-dir", "--workspace", "ws")

	require.NoError(t, run.err, "stderr=%s", run.stderr)
	require.Contains(t, run.stdout, "旧记忆导入：源条目 0 条")
	require.Contains(t, run.stdout, "**空载体**", "空源必须点名")
	require.Contains(t, run.stdout, "不是「迁移成功」的证据", "空载体不得被读成迁移成功")
}

// TestSwitch_PrintsMemoryBaseline 验收点：导入后的行数对账（S-F04）在切换输出里写明
// 三个数——回退判据该取哪个基线由这段读数决定，不能只让实现里知道。
// 未产出对账（`Checked` 为假）时不打印：零值不得冒充"读到的零行"。
// 逆改点：删掉渲染 / 不看 Checked 无条件打印 → 两条断言各自红。
func TestSwitch_PrintsMemoryBaseline(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)
	fake := newFakeMigration()
	fake.switchReport.MemoryBaseline = MigrationMemoryBaseline{
		Checked: true, ImportRows: 0, Imported: 3, ObservedRows: 3,
	}

	run := executeDBWith(t, dir, nil, fake, "switch", "--dump", "dump-dir", "--workspace", "ws")
	require.NoError(t, run.err, "stderr=%s", run.stderr)
	require.Contains(t, run.stdout,
		"导入后基线：memory_entries 3 行（导入前 0 + 本次导入 3）——回退判据的该表基线取此值")

	unchecked := newFakeMigration()
	run = executeDBWith(t, dir, nil, unchecked, "switch", "--dump", "dump-dir", "--workspace", "ws")
	require.NoError(t, run.err, "stderr=%s", run.stderr)
	require.NotContains(t, run.stdout, "导入后基线", "没有对账证据时不得打印零值冒充读数")
}

// TestSwitch_PropagatesPortError 验收点：切换失败原样上抛，不打印切换记录
// （写失败时说"已切换"是最坏的一种谎）。
// 逆改点：先打印报告再判错序错位 / 吞掉错误 → 断言红。
func TestSwitch_PropagatesPortError(t *testing.T) {
	dir := t.TempDir()
	writeProviderConfig(t, dir, dsnConfig)
	fake := newFakeMigration()
	wantErr := errors.New("目标库与清单不一致: 测试注入")
	fake.err = wantErr

	run := executeDBWith(t, dir, nil, fake, "switch", "--dump", "dump-dir", "--workspace", "ws")

	require.ErrorIs(t, run.err, wantErr)
	require.Empty(t, run.stdout, "失败路径不得打印切换记录")
	require.Contains(t, run.logs, "db switch 未通过")
}
