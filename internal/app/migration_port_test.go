package app

// 本文件是 `db export / seal / import / verify / switch` 生产适配器的验收（T070 /
// 设计 §3.1.8）。判据留在 `internal/dbmigrate`，这里判的是**适配器自己的那一层**：
// 拒绝发生在写盘之前还是之后、源库是否只被读、三元组绑没绑、以及"没复核的包"能不能
// 在打开目标库之前就被挡下。
//
// 全部用例**不依赖 PostgreSQL**：涉及目标库的路径一律用"本应在连库之前就被拒绝"的
// 输入来判时序，真正连库的行为归 `//go:build integration` 的套件（用户 2026-10-28 的
// 第二条指令：真实 PG 装配测试走集成测试与隔离库，不依赖 PG 的留作单元测试）。
// These cases never need PostgreSQL: the target-facing paths are asserted through inputs
// that must be refused before the database is opened.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/dbmigrate"
	"github.com/ymedword/basalt/internal/dbmigrate/testutil"
	ws "github.com/ymedword/basalt/internal/workspace"
)

// testInstanceID 是测试名册里的实例标识（符合 `inst_` + 16 位小写十六进制）。
const testInstanceID = "inst_0123456789abcdef"

// newMigrationPort 返回带缓冲日志的生产适配器；时钟钉死在基线读数上，切换时刻可复算。
func newMigrationPort(t *testing.T) (migrationPort, *bytes.Buffer) {
	t.Helper()
	logger, logs := newCapturedLogger()
	return migrationPort{Logger: logger, Now: baselineClock}, logs
}

// seedLegacyWorkspace 造一个**存量实例工作区**：真实的 SQLite v6 库落成 `basalt.db`，
// 名册由调用方决定写不写——缺名册正是 C-47 里那些老实例的形态。
// seedLegacyWorkspace builds a real SQLite v6 workspace; the roster is the caller's call.
func seedLegacyWorkspace(t *testing.T, opts ...testutil.Option) string {
	t.Helper()

	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o700))

	all := append([]testutil.Option{testutil.SchemaVersion(dbmigrate.SourceSchemaVersion)}, opts...)
	source := testutil.SQLiteFixture(t, all...)
	target := filepath.Join(workspace, databaseFileName)
	// VACUUM INTO 落一份独立库文件：夹具库留在它自己的临时目录，工作区里的是真库。
	_, err := source.Exec("VACUUM INTO ?", filepath.ToSlash(target))
	require.NoError(t, err, "把夹具库落成工作区里的 basalt.db")
	require.NoError(t, source.Close())
	return workspace
}

// writeRoster 写实例名册（`<workspace>/instance.json`）。
func writeRoster(t *testing.T, workspace, instanceID string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"version": 1, "instance_id": instanceID})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, instanceMarkerFileName), raw, 0o600))
}

// seedReadyWorkspace 返回一份"可导出"的存量工作区：真库 + 名册。
func seedReadyWorkspace(t *testing.T) string {
	t.Helper()
	workspace := seedLegacyWorkspace(t)
	writeRoster(t, workspace, testInstanceID)
	return workspace
}

// newOutDir 返回一个**尚不存在**的输出目录路径（导出要求目录不存在或为空）。
func newOutDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "migration-out")
}

// exportReady 跑一次成功导出并返回报告，供需要"已封包迁移包"的用例复用。
func exportReady(t *testing.T) (workspace, out string, report cli.MigrationExportReport) {
	t.Helper()
	port, _ := newMigrationPort(t)
	workspace = seedReadyWorkspace(t)
	out = newOutDir(t)
	report, err := port.Export(context.Background(), cli.MigrationExportRequest{Workspace: workspace, Out: out})
	require.NoError(t, err, "可导出的工作区必须导出成功")
	return workspace, out, report
}

// exportPending 跑一次"有未归类文件"的导出，返回受限核查包与报告。
func exportPending(t *testing.T) (workspace, out string, report cli.MigrationExportReport) {
	t.Helper()
	port, _ := newMigrationPort(t)
	workspace = seedReadyWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("operator notes\n"), 0o600))
	out = newOutDir(t)
	report, err := port.Export(context.Background(), cli.MigrationExportRequest{Workspace: workspace, Out: out})
	require.NoError(t, err, "受限核查包本身是成功产出，只是不可交付")
	return workspace, out, report
}

// sealIncludingNotes 把 `notes.txt` 逐条处置为 include 后封包。
func sealIncludingNotes(t *testing.T, out, digest string) cli.MigrationSealReport {
	t.Helper()
	port, _ := newMigrationPort(t)
	reviewPath := filepath.Join(t.TempDir(), "unclassified-review.json")
	raw, err := json.Marshal(dbmigrate.ReviewFile{
		UnclassifiedDigest: digest,
		Reviewer:           "operator-a",
		ReviewedAt:         baselineClock(),
		Decisions: []dbmigrate.ReviewDecision{
			{RelPath: "notes.txt", Disposition: "include"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(reviewPath, raw, 0o600))

	report, err := port.Seal(context.Background(), cli.MigrationSealRequest{Dir: out, ReviewFile: reviewPath})
	require.NoError(t, err, "逐条处置齐全时必须封包成功")
	return report
}

// TestExport_RefusesOverlappingDirs 验收点：包目录与工作区**互相包含的两种形态**都在
// 任何写入之前被拒——包写进工作区会被下一次盘点当成资产，工作区塞进包目录会让"包"里
// 多出一整棵源树。
// 逆改点：删掉任一向的 containsPath 判定 → 对应用例拿到"源库不存在"而不是"互相包含"。
func TestExport_RefusesOverlappingDirs(t *testing.T) {
	port, _ := newMigrationPort(t)
	base := t.TempDir()

	cases := []struct{ name, workspace, out string }{
		{"out-inside-workspace", filepath.Join(base, "ws"), filepath.Join(base, "ws", "migration-out")},
		{"workspace-inside-out", filepath.Join(base, "out", "ws"), filepath.Join(base, "out")},
		{"same-dir", filepath.Join(base, "shared"), filepath.Join(base, "shared")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := port.Export(context.Background(), cli.MigrationExportRequest{Workspace: tc.workspace, Out: tc.out})

			require.Error(t, err)
			require.Contains(t, err.Error(), "互相包含", "重叠布局必须在写入之前拒绝")
			require.NoDirExists(t, tc.out, "被拒时不得留下输出目录")
		})
	}
}

// TestExport_RefusesNonEmptyOutputDir 验收点：复用非空输出目录被拒——清单只描述本次
// 导出的表，上一批残留的 `*.jsonl` 没有任何下游判据会看见，"复核通过"与"目录里躺着旧
// 数据"因此可以同时成立。
// 逆改点：去掉 requireEmptyOrAbsentDir → 断言红（导出照常开始）。
func TestExport_RefusesNonEmptyOutputDir(t *testing.T) {
	port, _ := newMigrationPort(t)
	workspace := seedReadyWorkspace(t)
	out := newOutDir(t)
	require.NoError(t, os.MkdirAll(out, 0o700))
	leftover := filepath.Join(out, "sessions.jsonl")
	require.NoError(t, os.WriteFile(leftover, []byte("stale\n"), 0o600))

	_, err := port.Export(context.Background(), cli.MigrationExportRequest{Workspace: workspace, Out: out})

	require.Error(t, err)
	require.Contains(t, err.Error(), "非空")
	require.FileExists(t, leftover, "被拒时不得改动已有内容")
}

// TestExport_RefusesWorkspaceWithoutSQLite 验收点：工作区里没有 `basalt.db` 时点名该
// 文件并说明"本命令不建库"——导出只服务建过库的存量工作区。
// 逆改点：缺库分支返回 nil（或顺手建一个空库）→ 断言红。
func TestExport_RefusesWorkspaceWithoutSQLite(t *testing.T) {
	port, _ := newMigrationPort(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	writeRoster(t, workspace, testInstanceID)

	_, err := port.Export(context.Background(), cli.MigrationExportRequest{Workspace: workspace, Out: newOutDir(t)})

	require.Error(t, err)
	require.Contains(t, err.Error(), databaseFileName)
	require.Contains(t, err.Error(), "不建库")
}

// TestExport_RefusesLegacyWithoutRoster 是 C-47 的具名用例：存量实例没有名册时导出
// **拒绝**，并指出唯一出路是显式采纳身份（`basalt instance adopt`，仍是设计提案）。
// 三条禁令逐条判：不得当新实例、不得自动补写名册、不得留半个包。
// 逆改点：缺名册分支改成"生成随机实例标识继续导" → ErrorIs 与"C-47"断言红。
func TestExport_RefusesLegacyWithoutRoster(t *testing.T) {
	port, logs := newMigrationPort(t)
	workspace := seedLegacyWorkspace(t) // 真库，**没有** instance.json
	out := newOutDir(t)

	_, err := port.Export(context.Background(), cli.MigrationExportRequest{Workspace: workspace, Out: out})

	require.ErrorIs(t, err, errInstanceIdentityMissing)
	require.Contains(t, err.Error(), "instance_identity_missing")
	require.Contains(t, err.Error(), "instance adopt", "要给出唯一的下一步")
	require.Contains(t, err.Error(), "C-47", "要能被追溯回设计裁决")
	require.NoDirExists(t, out, "被拒时不得产出任何包")
	require.NoFileExists(t, filepath.Join(workspace, instanceMarkerFileName), "不得自动补写名册")
	require.NotContains(t, logs.String(), "db export 完成", "失败路径不得留下完成日志")
}

// TestExport_ExportsRealWorkspaceAndLeavesSourceUntouched 验收点：真实 v6 工作区导出
// 成功（三元组齐备、可直接交付），且**源库只被读**——`basalt.db` 的字节在导出前后
// 逐字节相等，打开时新建的 `-wal` 为 0 字节。
//
// T070 实测依据：`mode=ro` 打开会建 `-wal` / `-shm` 且关闭后仍在，因此源句柄必须在
// A1 之前打开、A2 之后关闭；本用例把这条时序钉在"导出成功且源库未变"上。
// 逆改点：把 NewExporter 的版本参数换成 PostgreSQL 谱系（1）→ 真实 v6 库被判成
// "不受支持"，本用例当场红（这正是本轮修掉的那个缺陷）。
func TestExport_ExportsRealWorkspaceAndLeavesSourceUntouched(t *testing.T) {
	port, logs := newMigrationPort(t)
	workspace := seedReadyWorkspace(t)
	dbPath := filepath.Join(workspace, databaseFileName)
	before, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	out := newOutDir(t)

	report, err := port.Export(context.Background(), cli.MigrationExportRequest{Workspace: workspace, Out: out})

	require.NoError(t, err)
	require.True(t, report.Deliverable, "无名册之外的干扰项时必须直接可交付")
	require.Equal(t, dbmigrate.AssetReviewNotRequired, report.ReviewStatus)
	require.Equal(t, testInstanceID, report.SourceInstanceID)
	require.Equal(t, dbmigrate.AssetOriginRoster, report.SourceInstanceIDOrigin)
	require.NotEmpty(t, report.MigrationBatchID)
	require.NotEmpty(t, report.SourceSnapshotSHA256)
	require.FileExists(t, filepath.Join(out, "manifest.json"))
	require.FileExists(t, filepath.Join(out, "assets.json"))
	require.Contains(t, logs.String(), "db export 完成")

	after, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	require.Equal(t, before, after, "源库只被读，不被改")

	walInfo, statErr := os.Stat(dbPath + "-wal")
	if statErr == nil {
		require.Zero(t, int(walInfo.Size()), "只读打开新建的 -wal 不得承载任何内容")
	} else {
		require.ErrorIs(t, statErr, os.ErrNotExist)
	}
}

// TestExport_PendingPackageIsNotDeliverable 验收点：有未归类文件时只产出受限核查包——
// 报告标 pending 且不可交付，`assets.json` 已落盘（供 `--seal` 接手），源文件原样躺在
// `review/` 镜像里。
// 逆改点：把 pending 也当成可交付 → Deliverable 断言红；不写 assets.json → Seal 拿不到清单。
func TestExport_PendingPackageIsNotDeliverable(t *testing.T) {
	_, out, report := exportPending(t)

	require.False(t, report.Deliverable, "有未归类条目时不可交付")
	require.Equal(t, dbmigrate.AssetReviewPending, report.ReviewStatus)
	require.Equal(t, 1, report.UnclassifiedCount)
	require.NotEmpty(t, report.UnclassifiedDigest)
	require.FileExists(t, filepath.Join(out, "assets.json"))
	require.FileExists(t, filepath.Join(out, "review", "notes.txt"), "核查区要铺出待处置文件")
}

// TestExport_PendingPackageReportsContentHints 验收点：未归类文件的启发式内容扫描结果
// 进导出报告（C-49），且**只有规则 ID → 命中文件数**——不带文件名、行号或原文；命中
// 不改变可交付性判据，"扫到敏感内容"也不能替代逐条处置。
// 逆改点：把扫描结果从报告里拿掉 → 命中断言红；把 RelPath 写成 map key → 闭集断言红。
func TestExport_PendingPackageReportsContentHints(t *testing.T) {
	port, _ := newMigrationPort(t)
	workspace := seedReadyWorkspace(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "notes"), 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspace, "notes", "2026-10-27-meeting.txt"),
		[]byte("-----BEGIN OPENSSH PRIVATE KEY-----\nplaceholder\n"),
		0o600))
	out := newOutDir(t)

	report, err := port.Export(context.Background(), cli.MigrationExportRequest{Workspace: workspace, Out: out})
	require.NoError(t, err)

	require.False(t, report.Deliverable, "内容命中不是放行依据，可交付性仍按核查状态判")
	require.Equal(t, dbmigrate.AssetReviewPending, report.ReviewStatus)
	require.Equal(t, 1, report.ContentHints["private_key_header"], "未归类文件里的 PEM 私钥头必须进报告")
	require.NotEmpty(t, report.ContentHints)
	for rule := range report.ContentHints {
		require.Contains(t,
			[]string{"private_key_header", "token_shape", "high_entropy", "binary_skipped", "truncated"},
			rule, "报告只输出已登记规则 ID，不得回传文件名或内容")
	}
}

// TestSeal_RequiresReviewFileForPendingPackage 验收点：待核查包缺处置文件时**在调用
// dbmigrate 之前**点名 `--review-file` 必填——`SealAssetPackage` 会把它报成"摘要与清单
// 不一致"，那句话会把运维指向错误方向。
// 逆改点：把空处置文件直接交给 SealAssetPackage → 文案变成"摘要不一致"，断言红。
func TestSeal_RequiresReviewFileForPendingPackage(t *testing.T) {
	port, _ := newMigrationPort(t)
	_, out, _ := exportPending(t)

	_, err := port.Seal(context.Background(), cli.MigrationSealRequest{Dir: out})

	require.Error(t, err)
	require.Contains(t, err.Error(), "--review-file")
	require.Contains(t, err.Error(), "1 个未归类文件")
}

// TestSeal_IncludesReviewedEntries 验收点：逐条处置为 include 后封包成功，三类处置条数
// 分别计数，清单状态转 sealed。
// 逆改点：不统计 included → 计数断言红；不更新状态 → ReviewStatus 断言红。
func TestSeal_IncludesReviewedEntries(t *testing.T) {
	_, out, report := exportPending(t)

	sealed := sealIncludingNotes(t, out, report.UnclassifiedDigest)

	require.Equal(t, dbmigrate.AssetReviewSealed, sealed.ReviewStatus)
	require.Equal(t, 1, sealed.Included)
	require.Zero(t, sealed.Excluded)
	require.Zero(t, sealed.Handoff)
	require.Equal(t, "operator-a", sealed.Reviewer)
	require.NotEmpty(t, sealed.ReviewDigest)
	require.FileExists(t, filepath.Join(out, "package", "notes.txt"), "include 的条目要进可交付包")
}

// TestImport_RefusesPackageBeforeOpeningDatabase 验收点：未复核（pending）的包在**打开
// 目标库之前**被拒——导进去等于替操作者做了处置决定。
//
// 时序判据：DSN 给的是一个连不上的库；若实现先连库，错误会变成连接失败。这里拿到的
// 必须是"尚未复核"。
// 逆改点：把 postgres.Open 提到 requireReviewedPackage 之前 → 断言红。
func TestImport_RefusesPackageBeforeOpeningDatabase(t *testing.T) {
	port, _ := newMigrationPort(t)
	_, out, _ := exportPending(t)

	_, err := port.Import(context.Background(),
		cli.DatabaseConfig{DSN: "postgres://basalt@127.0.0.1:1/basalt"},
		cli.MigrationImportRequest{Dump: out})

	require.Error(t, err)
	require.Contains(t, err.Error(), "尚未复核")
	require.NotContains(t, err.Error(), "连接", "拒绝必须发生在连库之前")
}

// TestSwitch_RefusesUnreviewedPackage 验收点：pending 包同样在 `db switch` 的第一道
// 闸门被拒，且不产生任何切换记录。
// 逆改点：跳过 requireReviewedPackage → 断言红。
func TestSwitch_RefusesUnreviewedPackage(t *testing.T) {
	port, _ := newMigrationPort(t)
	workspace, out, _ := exportPending(t)

	report, err := port.Switch(context.Background(),
		cli.DatabaseConfig{DSN: "postgres://basalt@127.0.0.1:1/basalt"},
		cli.MigrationSwitchRequest{Dump: out, Workspace: workspace})

	require.Error(t, err)
	require.Contains(t, err.Error(), "尚未复核")
	require.Empty(t, report.CutoverID, "被拒时不得留下切换事实")
}

// TestSwitch_RefusesSourceChangedSinceSnapshot 验收点：给了 `--workspace` 时源端重盘是
// 第四道门禁——导出之后新出现的文件即 `source_changed_since_snapshot`，且同样发生在
// 打开目标库之前。
// 逆改点：去掉 VerifyAssets 段 → 错误变成连接失败，断言红。
func TestSwitch_RefusesSourceChangedSinceSnapshot(t *testing.T) {
	port, _ := newMigrationPort(t)
	workspace, out, _ := exportReady(t)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "late-arrival.txt"), []byte("drift\n"), 0o600))

	_, err := port.Switch(context.Background(),
		cli.DatabaseConfig{DSN: "postgres://basalt@127.0.0.1:1/basalt"},
		cli.MigrationSwitchRequest{Dump: out, Workspace: workspace})

	require.Error(t, err)
	require.Contains(t, err.Error(), "source_changed_since_snapshot")
	require.Contains(t, err.Error(), "late-arrival.txt", "漂移要点名到具体文件")
	require.NotContains(t, err.Error(), "连接", "拒绝必须发生在连库之前")
}

// TestSwitch_RefusesSourceDatabaseWrittenAfterExport 是颗粒度文档 §4.3 `TestCutover_
// SourceAdvancedRejectsTarget` 的实际承担项：源库在导出**之后**又被写入时（旧进程没停
// 干净），目标端不得接管。判据链是清单记录的 `SourceDBFiles` 身份（尺寸 + SHA-256）在
// 源端重盘时对不上 → `DBFileDrift` → `source_changed_since_snapshot`，且拒绝发生在
// 打开目标库之前。
// 逆改点：`assetProblems` 丢掉 DBFileDrift 分支 → 错误变成连接失败，断言红。
func TestSwitch_RefusesSourceDatabaseWrittenAfterExport(t *testing.T) {
	port, _ := newMigrationPort(t)
	workspace, out, _ := exportReady(t)

	// 导出之后旧实例又落了一条业务行：直接写源库文件，模拟"停机没停干净"。
	source, err := sql.Open("sqlite", filepath.Join(workspace, databaseFileName))
	require.NoError(t, err, "打开工作区源库")
	_, err = source.Exec(`CREATE TABLE sessions (session_id TEXT PRIMARY KEY, status TEXT NOT NULL);
		INSERT INTO sessions (session_id, status) VALUES ('s-after-export', 'active')`)
	require.NoError(t, err, "旧实例在导出后仍接受写入")
	require.NoError(t, source.Close())

	_, err = port.Switch(context.Background(),
		cli.DatabaseConfig{DSN: "postgres://basalt@127.0.0.1:1/basalt"},
		cli.MigrationSwitchRequest{Dump: out, Workspace: workspace})

	require.Error(t, err)
	require.Contains(t, err.Error(), "source_changed_since_snapshot")
	require.Contains(t, err.Error(), "源库文件漂移", "源库在导出后有写入必须单独点名")
	require.Contains(t, err.Error(), "basalt.db")
	require.NotContains(t, err.Error(), "连接", "拒绝必须发生在连库之前")
}

// TestSwitch_DeclaresSkippedSourceReverification 验收点（2026-10-31 判据收紧，FR-092）：
// 「不给 `--workspace` 就跳过源端重盘并留 WARN」的 fail-open 路径已删除——缺工作区
// **必须**拒绝，错误里点名 FR-092 与 `source_changed_since_snapshot`，且拒绝发生在
// 读包 / 连库之前（目标库不可达也报的是工作区错误，不是连接错误）。
// 历史：本用例原名判「未执行要留 WARN」；按已批准规格 FR-092「切换前必须重盘」收紧为
// 拒绝路径，名字保留以维持 stage 的 required_tests 映射（只增不删、不改名）。
// 逆改点：恢复"仅 WARN 继续"分支 → 错误消失（变成连接失败），断言红。
func TestSwitch_DeclaresSkippedSourceReverification(t *testing.T) {
	port, _ := newMigrationPort(t)
	_, out, _ := exportReady(t)

	report, err := port.Switch(context.Background(),
		cli.DatabaseConfig{DSN: "postgres://basalt@127.0.0.1:1/basalt"},
		cli.MigrationSwitchRequest{Dump: out})

	require.Error(t, err, "缺源工作区必须拒绝切换")
	require.Contains(t, err.Error(), "必须给出源工作区")
	require.Contains(t, err.Error(), "source_changed_since_snapshot", "拒绝理由必须点名被保护的判据")
	require.Contains(t, err.Error(), "FR-092")
	require.NotContains(t, err.Error(), "连接", "拒绝必须发生在连库之前")
	require.Empty(t, report.CutoverID, "被拒时不得留下切换事实")
}

// TestVerifyDump_AcceptsSealedPackage 验收点：封包成功的迁移包离线复核通过——报告
// 模式为 dump、结论为真、无差异条目。
// 逆改点：把包内复核接到 review/ 而不是 package/ → 缺项 / 多余项让 OK 变假，断言红。
func TestVerifyDump_AcceptsSealedPackage(t *testing.T) {
	port, _ := newMigrationPort(t)
	_, out, report := exportPending(t)
	sealIncludingNotes(t, out, report.UnclassifiedDigest)

	verified, err := port.VerifyDump(context.Background(), cli.MigrationVerifyRequest{Dump: out})

	require.NoError(t, err)
	require.True(t, verified.OK)
	require.Equal(t, "dump", verified.Mode)
	require.Empty(t, verified.MissingIDs)
}

// TestVerifyDump_ReportsMissingPackagedAsset 验收点：包内少了一个 `included` 条目时，
// 复核**不通过**并逐条点名 `packaged_digest_mismatch`——不是笼统的"校验失败"。
// 逆改点：只看 dump 不看包内资产 → NoError 断言红（少了文件却报通过，正是 C-49 要防的）。
func TestVerifyDump_ReportsMissingPackagedAsset(t *testing.T) {
	port, _ := newMigrationPort(t)
	_, out, report := exportPending(t)
	sealIncludingNotes(t, out, report.UnclassifiedDigest)
	require.NoError(t, os.Remove(filepath.Join(out, "package", "notes.txt")))

	verified, err := port.VerifyDump(context.Background(), cli.MigrationVerifyRequest{Dump: out})

	require.Error(t, err)
	require.Contains(t, err.Error(), "packaged_digest_mismatch")
	require.Contains(t, err.Error(), "notes.txt")
	require.False(t, verified.OK, "报告结论必须与错误一致")
}

// TestVerifyDump_RefusesPendingPackage 验收点：受限核查包不能当迁移包复核——先处置再封包。
// 逆改点：跳过 requireReviewedPackage → 断言红。
func TestVerifyDump_RefusesPendingPackage(t *testing.T) {
	port, _ := newMigrationPort(t)
	_, out, _ := exportPending(t)

	_, err := port.VerifyDump(context.Background(), cli.MigrationVerifyRequest{Dump: out})

	require.Error(t, err)
	require.Contains(t, err.Error(), "尚未复核")
}

// TestMigrationPort_RequiresDirectories 验收点：缺工作区 / 缺输出目录 / 缺包目录各自
// 点名参数名，而不是落到"路径为空"这类无法对号的文件系统错误。
// 逆改点：删掉任何一个非空判定 → 对应用例拿到别的错误文案，断言红。
func TestMigrationPort_RequiresDirectories(t *testing.T) {
	port, _ := newMigrationPort(t)
	ctx := context.Background()

	_, exportErr := port.Export(ctx, cli.MigrationExportRequest{Out: "out"})
	require.ErrorContains(t, exportErr, "源工作区")

	_, outErr := port.Export(ctx, cli.MigrationExportRequest{Workspace: "ws"})
	require.ErrorContains(t, outErr, "--out")

	_, importErr := port.Import(ctx, cli.DatabaseConfig{}, cli.MigrationImportRequest{})
	require.ErrorContains(t, importErr, "--dump")

	_, switchErr := port.Switch(ctx, cli.DatabaseConfig{}, cli.MigrationSwitchRequest{})
	require.ErrorContains(t, switchErr, "--dump")

	_, verifyErr := port.VerifyDump(ctx, cli.MigrationVerifyRequest{})
	require.ErrorContains(t, verifyErr, "--dump")
}

// TestExport_RequiresWorkspaceLock 守颗粒度文档 §4.3 `TestExport_RequiresWorkspaceLock`
// （C-46 静止窗口）：导出必须取到 `<ws>/basalt.lock`；被活实例持有时在**采集前**拒绝，
// 且不产出任何可交付物。放在适配器层判——`BuildAssetManifest` 的锁语义已经有单测，
// 这里判的是 `db export` 这条入口真的走到了那把锁。
//
// 逆改点：Export 直接调不取锁的采集函数 → 锁被持有时导出成功，`ErrorIs` 红。
func TestExport_RequiresWorkspaceLock(t *testing.T) {
	ctx := context.Background()
	port, _ := newMigrationPort(t)
	workspace := seedReadyWorkspace(t)

	held, err := ws.AcquireInstanceLock(workspace)
	require.NoError(t, err)

	report, err := port.Export(ctx, cli.MigrationExportRequest{Workspace: workspace, Out: newOutDir(t)})
	require.ErrorIs(t, err, dbmigrate.ErrWorkspaceBusy, "锁被活实例持有必须拒绝导出")
	require.Contains(t, err.Error(), "workspace_busy")
	require.Empty(t, report.MigrationBatchID, "被拒时不得留下导出批次")

	require.NoError(t, held.Release())
	_, err = port.Export(ctx, cli.MigrationExportRequest{Workspace: workspace, Out: newOutDir(t)})
	require.NoError(t, err, "锁释放后导出必须成功——拒绝只因锁被持有")
}

// TestSwitch_RefusesTamperedPackageAfterSeal 补 `TestCutover_RefusesUnreviewedUnclassified`
// 在切换闸门缺的那一半（颗粒度文档 §4.3 变更记录"切换额外要求已封装"）：包已封（复核
// 通过）但 dump 在封包**之后**被改——切换必须重跑摘要判据并拒绝，而不是相信"刚才封过"。
//
// 逆改点：Switch 跳过 VerifyDump（信任封包结论）→ 用连不上的 DSN 会走到连库失败，
// 错误里出现"连接"而不是"文件摘要"，断言红。
func TestSwitch_RefusesTamperedPackageAfterSeal(t *testing.T) {
	port, _ := newMigrationPort(t)
	// 夹具必须带一张**真实业务表**：空库导出不产任何 `.jsonl`，"封包后改文件"无从改起。
	// The fixture needs a real table: an empty source exports no JSONL at all.
	workspace := seedLegacyWorkspace(t, testutil.Table("sessions",
		map[string]any{"id": int64(1), "title": "tamper-after-seal"}))
	writeRoster(t, workspace, testInstanceID)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("operator notes\n"), 0o600))
	out := newOutDir(t)
	report, err := port.Export(context.Background(), cli.MigrationExportRequest{Workspace: workspace, Out: out})
	require.NoError(t, err)
	sealIncludingNotes(t, out, report.UnclassifiedDigest)

	dumpPath := filepath.Join(out, "sessions.jsonl")
	raw, err := os.ReadFile(dumpPath)
	require.NoError(t, err, "导出必须产出 sessions.jsonl")
	require.Contains(t, string(raw), `{"`, "夹具前提：JSONL 行以对象开头")
	require.NoError(t, os.WriteFile(dumpPath, []byte(strings.ReplaceAll(string(raw), `{"`, `{ "`)), 0o600))

	switched, err := port.Switch(context.Background(),
		cli.DatabaseConfig{DSN: "postgres://basalt@127.0.0.1:1/basalt"},
		cli.MigrationSwitchRequest{Dump: out, Workspace: workspace})

	require.Error(t, err)
	require.NotContains(t, err.Error(), "尚未复核", "包已封，拒绝必须来自摘要判据而不是复核状态")
	require.Contains(t, err.Error(), "文件摘要", "封包后改动必须由重跑的摘要判据抓住")
	require.Contains(t, err.Error(), "sessions")
	require.NotContains(t, err.Error(), "连接", "拒绝必须发生在连库之前")
	require.Empty(t, switched.CutoverID, "被拒时不得留下切换事实")
}

// TestAssets_NoPlaintextInPackage 守颗粒度文档 §4.3 `TestAssets_NoPlaintextInPackage`：
// 导出包（含清单与报告）里只能出现 `${VAR}` 占位符的名字，**不得**出现任何凭据值。
// 判据扫整个输出目录——不只 `assets.json`：值从包内文件、清单字段或报告里任一扇门漏出
// 都算失败。
//
// 逆改点：导出路径把 `${VAR}` 解析成进程环境的值再打包 → 哨兵值出现在输出目录，本用例红。
func TestAssets_NoPlaintextInPackage(t *testing.T) {
	const sentinel = "export-sentinel-value-7k2m"
	const varName = "GITHUB_PERSONAL_ACCESS_TOKEN"
	port, _ := newMigrationPort(t)
	workspace := seedReadyWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "mcp_servers.yaml"),
		[]byte("servers:\n  - name: github\n    env:\n      "+varName+": \"${"+varName+"}\"\n"), 0o600))
	t.Setenv(varName, sentinel)

	out := newOutDir(t)
	report, err := port.Export(context.Background(), cli.MigrationExportRequest{Workspace: workspace, Out: out})
	require.NoError(t, err)

	encodedReport, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encodedReport), sentinel, "导出报告不得带凭据值")

	sawPlaceholder := false
	err = filepath.WalkDir(out, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		require.NotContains(t, string(raw), sentinel, "输出目录里任一文件都不得带凭据值：%s", path)
		if strings.Contains(string(raw), "${"+varName+"}") {
			sawPlaceholder = true
		}
		return nil
	})
	require.NoError(t, err)
	require.True(t, sawPlaceholder, "占位符原文必须随包保留——它只是名字，不是值")
}
