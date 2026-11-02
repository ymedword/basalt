//go:build e2e

package e2e

// 本文件判 US-17 的单机升级与失败恢复（FR-12/FR-13）：真实 `pg_dump` / `pg_restore`、
// 真实归档替换、真实迁移命令。失败恢复必须证明「备份后的写入被恢复掉」，对照组证明
// 该断言有判别力——省略恢复步骤时标记必须还在。
// This file covers US-17's single-machine upgrade and failure recovery with real
// pg_dump/pg_restore, real archive replacement and the real migrate command. The
// control case proves the recovery assertion has discriminating power.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ymedword/basalt/scripts/releasekit"
)

const (
	// pgToolEnv 打开测试二进制的 pg 工具替身模式：替身按**文件名**分派（pg_dump /
	// pg_restore / fail），因为 RunUpgrade 只接受命令名、运行期环境不变。
	// pgToolEnv enables the pg-tool stand-in; dispatch is by executable name because
	// RunUpgrade takes a command name and the environment cannot change mid-run.
	pgToolEnv = "BASALT_E2E_PG_TOOL"

	pgMarkerDSNEnv       = "BASALT_E2E_PG_MARKER_DSN"
	pgMarkerEnv          = "BASALT_E2E_PG_MARKER"
	pgMarkerWorkspaceEnv = "BASALT_E2E_PG_MARKER_WORKSPACE"

	// postBackupSchemaVersion 是"备份之后库被改成过新版本"的哨兵：新版迁移必须拒绝，
	// 恢复后必须回到当前支持版本。
	// postBackupSchemaVersion is the too-new sentinel written after the dump.
	postBackupSchemaVersion = 999
)

// runPGToolHelper 是 pg 工具替身的入口；在 TestMain 中先于一切测试逻辑分派。
// runPGToolHelper is the stand-in entry point, dispatched before any test logic.
func runPGToolHelper(args []string) int {
	name := strings.ToLower(filepath.Base(os.Args[0]))
	switch {
	case strings.Contains(name, "fail"):
		return 1
	case strings.Contains(name, "pg_dump"):
		if err := runRealTool("pg_dump", args); err != nil {
			fmt.Fprintln(os.Stderr, "pg_dump 替身:", err)
			return 1
		}
		if err := applyPostBackupMutation(); err != nil {
			fmt.Fprintln(os.Stderr, "备份后写入失败:", err)
			return 1
		}
		return 0
	case strings.Contains(name, "pg_restore"):
		if err := runRealTool("pg_restore", args); err != nil {
			fmt.Fprintln(os.Stderr, "pg_restore 替身:", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "无法从文件名 %q 判断 pg 工具替身模式\n", name)
		return 2
	}
}

func runRealTool(name string, args []string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// applyPostBackupMutation 在真实 dump 之后写三类"备份后状态"：库里的标记行、过新的
// schema 版本、工作区文件。恢复成功时三者都必须消失。
// applyPostBackupMutation writes the post-backup state: a marker row, a too-new schema
// version and a workspace file. A successful restore removes all three.
func applyPostBackupMutation() error {
	dsn := os.Getenv(pgMarkerDSNEnv)
	marker := os.Getenv(pgMarkerEnv)
	workspace := os.Getenv(pgMarkerWorkspaceEnv)
	if dsn == "" || marker == "" || workspace == "" {
		return fmt.Errorf("%s / %s / %s 未设置", pgMarkerDSNEnv, pgMarkerEnv, pgMarkerWorkspaceEnv)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`INSERT INTO upgrade_marker (marker) VALUES ($1)`, marker); err != nil {
		return fmt.Errorf("写入备份后标记: %w", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version, applied_at, description)
		VALUES ($1, 0, 'e2e too new')`, postBackupSchemaVersion); err != nil {
		return fmt.Errorf("写入过新 schema 版本: %w", err)
	}
	return os.WriteFile(filepath.Join(workspace, "post-backup-marker.txt"), []byte(marker), 0o600)
}

// upgradeFixture 是一次单机升级的完整夹具：已迁移到当前版本的隔离 schema、旧二进制
// 安装目录、工作区与配置。
// upgradeFixture is one upgrade case: a migrated isolated schema, an "old" install
// directory, the workspace and the config.
type upgradeFixture struct {
	h          *harness
	archive    string
	version    string
	installDir string
	installed  string
	oldSHA256  string
	workspace  string
	configPath string
}

func newUpgradeFixture(t *testing.T) *upgradeFixture {
	t.Helper()
	archive := requireArtifact(t)
	version := os.Getenv(artifactVersionEnv)
	for _, tool := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("缺少 %s：升级/恢复用例必须用真实 PostgreSQL 工具（%v）", tool, err)
		}
	}
	h := newHarness(t)
	initWorkspace(t, h)
	h.writeConfig(t, configWithAddr("http://127.0.0.1:1", freeLoopbackAddr(t)))
	requireExit(t, h.run(t, nil, "db", "migrate"), 0, "db migrate")

	installDir := t.TempDir()
	installed := filepath.Join(installDir, hostBinaryName())
	copyExecutable(t, binaryPath, installed)
	oldSHA, err := fileSHA256(installed)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(h.Root, ".basalt")
	db := h.openDB(t)
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS upgrade_seed (id text PRIMARY KEY)`,
		`INSERT INTO upgrade_seed (id) VALUES ('old-data') ON CONFLICT DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS upgrade_marker (marker text NOT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("准备升级数据失败（%s）：%v", statement, err)
		}
	}
	configPath := filepath.Join(h.Root, "basalt.yaml")
	t.Setenv(pgMarkerDSNEnv, h.Schema.DSN)
	t.Setenv(pgMarkerWorkspaceEnv, workspace)
	return &upgradeFixture{
		h: h, archive: archive, version: version, installDir: installDir,
		installed: installed, oldSHA256: oldSHA, workspace: workspace, configPath: configPath,
	}
}

func hostBinaryName() string {
	if runtime.GOOS == "windows" {
		return "basalt.exe"
	}
	return "basalt"
}

func (f *upgradeFixture) options(t *testing.T, name string) releasekit.UpgradeOptions {
	t.Helper()
	return releasekit.UpgradeOptions{
		NewArchive: f.archive, DSN: f.h.Schema.DSN, Schema: f.h.Schema.Name,
		Workspace: f.workspace, ConfigPath: f.configPath, InstallDir: f.installDir,
		WorkDir: filepath.Join(t.TempDir(), name),
	}
}

func runUpgrade(t *testing.T, opts releasekit.UpgradeOptions) (releasekit.Backup, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return releasekit.RunUpgrade(ctx, opts)
}

// pgToolCopy 把测试二进制复制成指定文件名的替身：文件名决定替身行为。
// pgToolCopy copies the test binary under a name that selects the stand-in behavior.
func pgToolCopy(t *testing.T, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制：%v", err)
	}
	target := filepath.Join(t.TempDir(), name)
	copyExecutable(t, self, target)
	return target
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取 %s：%v", src, err)
	}
	if err := os.WriteFile(dst, body, 0o755); err != nil {
		t.Fatalf("写入 %s：%v", dst, err)
	}
}

func schemaVersionMax(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	if err := db.QueryRow(`SELECT COALESCE(max(version), 0) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("读取 schema_version：%v", err)
	}
	return version
}

func markerCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	return scalarInt(t, db, `SELECT COUNT(*) FROM upgrade_marker`)
}

// exeSuffix 给替身文件名补平台后缀，Windows 上必须能直接执行。
// exeSuffix appends the platform executable suffix to stand-in names.
func exeSuffix(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// TestUS17_Upgrade_Success：升级后旧数据保留、schema 到 7、新任务通过（FR-12）。
// TestUS17_Upgrade_Success: old data survives, the schema reaches 7, and the new
// binary serves a mock task.
func TestUS17_Upgrade_Success(t *testing.T) {
	f := newUpgradeFixture(t)
	backup, err := runUpgrade(t, f.options(t, "backup"))
	if err != nil {
		t.Fatalf("升级应成功：%v", err)
	}
	if info, err := os.Stat(filepath.Join(backup.Root, "postgres.dump")); err != nil || info.Size() == 0 {
		t.Fatalf("备份缺少 postgres.dump：%v", err)
	}
	if info, err := os.Stat(filepath.Join(backup.Root, "basalt.yaml")); err != nil || info.Size() == 0 {
		t.Fatalf("备份缺少配置：%v", err)
	}
	if info, err := os.Stat(filepath.Join(f.installDir, hostBinaryName()+".old")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("升级必须保留 basalt.old：%v", err)
	}
	db := f.h.openDB(t)
	if got := schemaVersionMax(t, db); got != 7 {
		t.Fatalf("升级后 schema_version = %d，期望 7", got)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM upgrade_seed WHERE id = 'old-data'`); n != 1 {
		t.Fatalf("升级后旧数据应保留（1 行），实际 %d 行", n)
	}
	if got, err := fileSHA256(f.installed); err != nil || got != binarySHA256 {
		t.Fatalf("安装目录应换成归档二进制：sha = %s（%v），期望 %s", got, err, binarySHA256)
	}

	// 新任务通过：用**安装目录里的新二进制**起 serve 并完成一条 mock 任务。
	f.h.Binary = f.installed
	fake := newFakeModel(t, textResponse("升级后任务完成"))
	addr, p := startReleaseServe(t, f.h, fake.URL(), "升级后任务")
	defer p.stop(t)
	sess := webCreateSession(t, addr, "demo", nil)
	message := webPostMessage(t, addr, sess.SessionID, "升级后的新任务")
	requireWebStatus(t, message, http.StatusOK, "OK", "", "升级后的新任务")
	var run webRun
	decodeWebData(t, message, &run, "升级后的新任务")
	if run.Status != "completed" {
		t.Fatalf("升级后新任务 status = %q，期望 completed（reason %q）", run.Status, run.Reason)
	}
}

// TestUS17_Upgrade_FailureRecovery：备份后的数据库写入、过新 schema 与工作区文件在
// 恢复后全部消失，旧二进制回到原位并可启动（FR-13）。
// TestUS17_Upgrade_FailureRecovery: post-backup writes disappear after recovery and the
// old binary is back in place.
func TestUS17_Upgrade_FailureRecovery(t *testing.T) {
	f := newUpgradeFixture(t)
	marker := fmt.Sprintf("post-backup-%d", time.Now().UnixNano())
	t.Setenv(pgMarkerEnv, marker)
	t.Setenv(pgToolEnv, "1")

	opts := f.options(t, "backup")
	opts.PGDump = pgToolCopy(t, exeSuffix("pg_dump-helper"))
	opts.PGRestore = pgToolCopy(t, exeSuffix("pg_restore-helper"))
	if _, err := runUpgrade(t, opts); err == nil {
		t.Fatal("schema 过新时新版迁移必须失败")
	}

	db := f.h.openDB(t)
	if got := markerCount(t, db); got != 0 {
		t.Fatalf("恢复后备份后标记应消失，实际仍有 %d 行", got)
	}
	if got := schemaVersionMax(t, db); got != 7 {
		t.Fatalf("恢复后 schema_version = %d，期望回到 7", got)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "post-backup-marker.txt")); !os.IsNotExist(err) {
		t.Fatalf("恢复后工作区标记文件应消失：%v", err)
	}
	if got, err := fileSHA256(f.installed); err != nil || got != f.oldSHA256 {
		t.Fatalf("恢复后应换回旧二进制：sha = %s（%v），期望 %s", got, err, f.oldSHA256)
	}
	res := f.h.run(t, nil, "--version")
	requireExit(t, res, 0, "恢复后旧版 --version")
	requireContains(t, res.Stdout, f.version, "恢复后旧版 --version")
}

// TestUS17_Upgrade_FailureRecoveryNeedsRestore：对照组——省略恢复步骤时标记必须还在，
// 证明主用例的"标记消失"断言有判别力（FR-13）。
// TestUS17_Upgrade_FailureRecoveryNeedsRestore: without restore the marker must remain.
func TestUS17_Upgrade_FailureRecoveryNeedsRestore(t *testing.T) {
	f := newUpgradeFixture(t)
	marker := fmt.Sprintf("control-%d", time.Now().UnixNano())
	t.Setenv(pgMarkerEnv, marker)
	t.Setenv(pgToolEnv, "1")

	opts := f.options(t, "backup")
	opts.PGDump = pgToolCopy(t, exeSuffix("pg_dump-helper"))
	opts.PGRestore = pgToolCopy(t, exeSuffix("pg_restore-fail"))
	if _, err := runUpgrade(t, opts); err == nil {
		t.Fatal("恢复步骤失败时升级必须报错")
	}

	db := f.h.openDB(t)
	if got := markerCount(t, db); got != 1 {
		t.Fatalf("对照组：未执行恢复时备份后标记必须仍在（1 行），实际 %d 行", got)
	}
	if got := schemaVersionMax(t, db); got != postBackupSchemaVersion {
		t.Fatalf("对照组：schema_version = %d，期望仍是 %d（未恢复）", got, postBackupSchemaVersion)
	}
	if _, err := os.Stat(filepath.Join(f.workspace, "post-backup-marker.txt")); err != nil {
		t.Fatalf("对照组：工作区标记文件应仍在：%v", err)
	}
	if got, err := fileSHA256(f.installed); err != nil || got != binarySHA256 {
		t.Fatalf("对照组：新二进制应仍在原位（未恢复）：sha = %s（%v）", got, err)
	}
}

// TestUS17_Upgrade_MigrationPath：基线 schema < 7，迁移后旧数据可读（FR-12）。
// TestUS17_Upgrade_MigrationPath: a v6 baseline migrates forward and keeps old data.
func TestUS17_Upgrade_MigrationPath(t *testing.T) {
	f := newUpgradeFixture(t)
	db := f.h.openDB(t)
	// v7 只新增历史保留两张表与两个索引（0007_history_retention.sql）：删掉它们并把
	// 版本记录退回 6，得到真实的 v6 基线；旧数据表不动。
	for _, statement := range []string{
		`DROP TABLE IF EXISTS history_retention_rollups`,
		`DROP TABLE IF EXISTS history_retention_runs`,
		`DROP INDEX IF EXISTS idx_runs_retention_session`,
		`DROP INDEX IF EXISTS idx_sessions_retention`,
		`DELETE FROM schema_version WHERE version = 7`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("构造 v6 基线失败（%s）：%v", statement, err)
		}
	}
	if got := schemaVersionMax(t, db); got != 6 {
		t.Fatalf("基线 schema_version = %d，期望 6", got)
	}
	requireExit(t, f.h.run(t, nil, "db", "migrate"), 0, "v6 → v7 迁移")
	if got := schemaVersionMax(t, db); got != 7 {
		t.Fatalf("迁移后 schema_version = %d，期望 7", got)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM upgrade_seed WHERE id = 'old-data'`); n != 1 {
		t.Fatalf("迁移后旧数据应可读（1 行），实际 %d 行", n)
	}
	var present string
	if err := db.QueryRow(`SELECT to_regclass('history_retention_runs')::text`).Scan(&present); err != nil || present == "" {
		t.Fatalf("迁移后缺少 v7 表：%q（%v）", present, err)
	}
}
