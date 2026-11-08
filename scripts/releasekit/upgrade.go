package releasekit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/cli"
)

// GitCommit 返回仓库 HEAD 的 40 位提交号。
// GitCommit returns the 40-character HEAD commit.
func GitCommit(repo string) (string, error) {
	out, err := gitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(out)
	if !commitPattern.MatchString(commit) {
		return "", fmt.Errorf("HEAD 提交号 %q 不是 40 位十六进制", commit)
	}
	return commit, nil
}

type backupMetadata struct {
	Schema     int       `json:"schema"`
	CreatedAt  time.Time `json:"created_at"`
	Workspace  string    `json:"workspace"`
	Config     string    `json:"config"`
	MasterKey  string    `json:"master_key,omitempty"`
	AdminToken string    `json:"admin_token,omitempty"`
	InstallDir string    `json:"install_dir"`
	Commit     string    `json:"commit,omitempty"`
	// 备份完整性：恢复前先核对，避免用被截断的 dump / 配置回滚。
	// Backup integrity: verified before restore so a truncated dump cannot roll back.
	DumpSHA256   string `json:"dump_sha256"`
	ConfigSHA256 string `json:"config_sha256"`
}

// RunUpgradeRecovery 以新归档执行单机升级探针；旧归档必须与已安装二进制一致。
// RunUpgradeRecovery runs the single-host upgrade probe for a release artifact; the
// old archive must match the installed binary.
func RunUpgradeRecovery(ctx context.Context, in Inputs, pkg PackageResult, opts ReleaseOptions) error {
	upgrade := opts.Upgrade
	upgrade.NewArchive = pkg.Archive
	if upgrade.Workspace == "" {
		upgrade.Workspace = opts.Workspace
	}
	if upgrade.DSN == "" {
		upgrade.DSN = opts.DSN
	}
	if upgrade.OldArchive == "" {
		return errors.New("full 档升级探针需要 --old 指向上一版归档")
	}
	upgrade.Commit = in.Commit
	_, err := RunUpgrade(ctx, upgrade)
	return err
}

// RunUpgrade 按「停止 → 三对象备份 → 替换 → 迁移/启动 → 校验」执行单机升级；
// 任一步失败即恢复数据库、配置、workspace 与旧二进制。
// RunUpgrade performs the single-machine upgrade in the mandated order and restores
// the database, config, workspace and old binary on any failure.
func RunUpgrade(ctx context.Context, opts UpgradeOptions) (Backup, error) {
	if opts.NewArchive == "" {
		return Backup{}, errors.New("必须提供新归档 --new")
	}
	if opts.DSN == "" {
		return Backup{}, errors.New("必须提供 PostgreSQL DSN")
	}
	if opts.Workspace == "" || opts.ConfigPath == "" {
		return Backup{}, errors.New("必须提供 --workspace 与 --config")
	}
	installDir := opts.InstallDir
	if installDir == "" {
		installDir = filepath.Dir(opts.ConfigPath)
	}
	// 灌库与迁移必须是同一个库：pg_dump 用 --dsn，db migrate 读 basalt.yaml。
	cfg, err := cli.LoadConfig(opts.ConfigPath, os.LookupEnv)
	if err != nil {
		return Backup{}, fmt.Errorf("读取 %s：%w", opts.ConfigPath, err)
	}
	if cfg.Database.DSN != opts.DSN {
		return Backup{}, errors.New("--dsn 与 basalt.yaml 的 database.dsn 不一致——备份与迁移必须是同一个库")
	}
	// 旧归档声明"从哪一版升级"：已安装二进制必须与它逐字节一致，否则探针判的不是这次升级。
	if opts.OldArchive != "" {
		installed := filepath.Join(installDir, hostBinaryName())
		want, err := archiveBinarySHA(opts.OldArchive)
		if err != nil {
			return Backup{}, err
		}
		got, err := FileSHA256(installed)
		if err != nil {
			return Backup{}, fmt.Errorf("读取已安装二进制 %s：%w", installed, err)
		}
		if got != want {
			return Backup{}, fmt.Errorf("已安装二进制与旧归档不一致：%s != %s", got, want)
		}
	}
	if len(opts.StopCommand) > 0 {
		if err := runCommand(ctx, filepath.Dir(opts.ConfigPath), opts.StopCommand...); err != nil {
			return Backup{}, fmt.Errorf("停止旧服务失败：%w", err)
		}
	}
	root := opts.WorkDir
	if root == "" {
		root = filepath.Join(filepath.Dir(opts.ConfigPath), "backups",
			"upgrade-"+time.Now().UTC().Format("20060102T150405Z"))
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Backup{}, fmt.Errorf("创建备份目录：%w", err)
	}
	backup := Backup{
		Root: root, DatabaseDSN: opts.DSN, Workspace: opts.Workspace, Config: opts.ConfigPath,
		MasterKey: opts.MasterKey, AdminToken: opts.AdminToken,
	}
	backup.PostgresDump = filepath.Join(root, "postgres.dump")
	// 先拍工作区、配置与凭据（快、且此后再无写者——服务已在 StopCommand 停掉），
	// 最后跑耗时的数据库 dump：dump 是备份窗口的最后一步，它之后的任何写入都发生在
	// "备份完成"之后，恢复时必须被抛弃而不是被当成备份内容。三对象齐备后才写
	// backup.json——半截备份没有摘要元数据，restoreWith 会拒绝采用。
	// Snapshot the cheap, already-quiescent objects first and run the heavy dump last,
	// so anything written during the dump is strictly post-backup and must be discarded
	// by a restore. backup.json (with digests) is written only after all three objects
	// exist, so a half-finished backup is never accepted by restoreWith.
	if err := copyDir(opts.Workspace, filepath.Join(root, "workspace")); err != nil {
		return Backup{}, fmt.Errorf("workspace 备份失败：%w", err)
	}
	if err := copyFile(opts.ConfigPath, filepath.Join(root, "basalt.yaml"), 0o600); err != nil {
		return Backup{}, fmt.Errorf("配置备份失败：%w", err)
	}
	for source, name := range map[string]string{opts.MasterKey: "master.key", opts.AdminToken: "admin.token"} {
		if source == "" {
			continue
		}
		// 点名了凭据文件就必须真的备份到：静默跳过等于备份"成功"却少了钥匙。
		if !fileExists(source) {
			return Backup{}, fmt.Errorf("凭据文件不存在：%s", source)
		}
		if err := copyFile(source, filepath.Join(root, name), 0o600); err != nil {
			return Backup{}, err
		}
	}
	args := []string{
		"--format=custom", "--no-owner", "--file", backup.PostgresDump,
		"--dbname", opts.DSN,
	}
	if opts.Schema != "" {
		args = append(args, "--schema="+opts.Schema)
	}
	if err := runExternal(ctx, opts.PGDump, "pg_dump", args...); err != nil {
		return Backup{}, fmt.Errorf("PostgreSQL 备份失败：%w", err)
	}
	dumpSHA, err := FileSHA256(backup.PostgresDump)
	if err != nil {
		return Backup{}, fmt.Errorf("读取 PostgreSQL 备份摘要：%w", err)
	}
	configSHA, err := FileSHA256(filepath.Join(root, "basalt.yaml"))
	if err != nil {
		return Backup{}, fmt.Errorf("读取配置备份摘要：%w", err)
	}
	metadata := backupMetadata{
		Schema: 1, CreatedAt: time.Now().UTC(), Workspace: opts.Workspace,
		Config: opts.ConfigPath, MasterKey: opts.MasterKey, AdminToken: opts.AdminToken,
		InstallDir: installDir, Commit: opts.Commit,
		DumpSHA256: dumpSHA, ConfigSHA256: configSHA,
	}
	if err := writeJSON(filepath.Join(root, "backup.json"), metadata); err != nil {
		return Backup{}, err
	}
	if err := installArchive(opts.NewArchive, installDir); err != nil {
		return backup, errors.Join(err, restoreWith(root, opts.DSN, opts.PGRestore))
	}
	if err := migrateWithNewBinary(ctx, installDir, opts); err != nil {
		return backup, errors.Join(err, restoreWith(root, opts.DSN, opts.PGRestore))
	}
	return backup, nil
}

func migrateWithNewBinary(ctx context.Context, installDir string, opts UpgradeOptions) error {
	binary := filepath.Join(installDir, hostBinaryName())
	cmd := exec.CommandContext(ctx, binary, "db", "migrate") // #nosec G204 -- binary 是刚安装的本仓发布物，子命令固定
	cmd.Dir = filepath.Dir(opts.ConfigPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("新版迁移失败：%w\n%s", err, out)
	}
	if len(opts.StartCommand) == 0 {
		return nil
	}
	// #nosec G204 -- 启动命令由运维在 --start 显式给出，不是外部输入
	start := exec.CommandContext(ctx, opts.StartCommand[0], opts.StartCommand[1:]...)
	start.Dir = filepath.Dir(opts.ConfigPath)
	if err := start.Start(); err != nil {
		return fmt.Errorf("新版启动失败：%w", err)
	}
	if opts.HealthURL == "" {
		return nil
	}
	if err := waitHealthy(ctx, opts.HealthURL, 30*time.Second); err != nil {
		_ = start.Process.Kill()
		return err
	}
	return nil
}

func waitHealthy(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			if response, err := client.Do(request); err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("健康检查超时：%s", url)
}

// verifyBackupIntegrity 在恢复前核对备份摘要；旧格式备份无摘要则拒绝恢复。
// verifyBackupIntegrity checks the recorded digests before restoring; backups
// without digests are rejected rather than restored on trust.
func verifyBackupIntegrity(root string, metadata backupMetadata) error {
	for _, item := range []struct{ name, want string }{
		{"postgres.dump", metadata.DumpSHA256},
		{"basalt.yaml", metadata.ConfigSHA256},
	} {
		if item.want == "" {
			return fmt.Errorf("备份 %s 缺少摘要：无法确认完整性", item.name)
		}
		got, err := FileSHA256(filepath.Join(root, item.name))
		if err != nil {
			return fmt.Errorf("读取备份 %s：%w", item.name, err)
		}
		if got != item.want {
			return fmt.Errorf("备份 %s 摘要不一致：%s != %s", item.name, got, item.want)
		}
	}
	return nil
}

func hostBinaryName() string {
	if runtime.GOOS == "windows" {
		return "basalt.exe"
	}
	return "basalt"
}

// archiveBinarySHA 解出归档内二进制的摘要，用于确认"从哪一版升级"。
// archiveBinarySHA extracts the archived binary and returns its digest, which pins
// the version being upgraded from.
func archiveBinarySHA(archive string) (string, error) {
	dir, err := os.MkdirTemp("", "basalt-upgrade-old-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := extractArchive(archive, dir); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return "", errors.New("旧归档必须只有一个顶层目录")
	}
	return FileSHA256(filepath.Join(dir, entries[0].Name(), hostBinaryName()))
}

func installArchive(archive, installDir string) error {
	dir, err := os.MkdirTemp("", "basalt-upgrade-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := extractArchive(archive, dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return errors.New("升级归档必须只有一个顶层目录")
	}
	source := filepath.Join(dir, entries[0].Name())
	if err := os.MkdirAll(installDir, 0o755); err != nil { // #nosec G301 -- 安装目录
		return err
	}
	target := filepath.Join(installDir, hostBinaryName())
	if fileExists(target) {
		if err := copyFile(target, target+".old", 0o755); err != nil {
			return err
		}
	}
	return copyFile(filepath.Join(source, hostBinaryName()), target, 0o755)
}

// RestoreBackup 从备份恢复三对象与旧二进制；DSN 从 BASALT_UPGRADE_DSN 读取。
// RestoreBackup restores the three objects and old binary; the DSN comes from
// BASALT_UPGRADE_DSN so it never lands in the backup metadata.
func RestoreBackup(root string) error {
	dsn := os.Getenv("BASALT_UPGRADE_DSN")
	if dsn == "" {
		return errors.New("恢复需要 BASALT_UPGRADE_DSN")
	}
	return RestoreBackupWithDSN(root, dsn)
}

// RestoreBackupWithDSN 用显式 DSN 恢复三对象与旧二进制。
// RestoreBackupWithDSN restores the backup with an explicit DSN.
func RestoreBackupWithDSN(root, dsn string) error {
	return restoreWith(root, dsn, "")
}

func restoreWith(root, dsn, pgRestore string) error {
	metadata, err := readBackupMetadata(root)
	if err != nil {
		return err
	}
	if err := verifyBackupIntegrity(root, metadata); err != nil {
		return err
	}
	if err := runExternal(context.Background(), pgRestore, "pg_restore", "--clean", "--if-exists",
		"--no-owner", "--dbname", dsn, filepath.Join(root, "postgres.dump")); err != nil {
		return fmt.Errorf("PostgreSQL 恢复失败：%w", err)
	}
	// 恢复必须是"回到备份时刻"，不是"把备份合进去"：升级期间新写的文件若留着，
	// 恢复后的 workspace 就不再是备份的那一份。
	if err := os.RemoveAll(metadata.Workspace); err != nil {
		return fmt.Errorf("清理 workspace %s：%w", metadata.Workspace, err)
	}
	if err := copyDir(filepath.Join(root, "workspace"), metadata.Workspace); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(root, "basalt.yaml"), metadata.Config, 0o600); err != nil {
		return err
	}
	for _, pair := range [][2]string{{"master.key", metadata.MasterKey}, {"admin.token", metadata.AdminToken}} {
		if pair[1] == "" {
			continue
		}
		if !fileExists(filepath.Join(root, pair[0])) {
			return fmt.Errorf("备份缺少 %s——恢复不完整", pair[0])
		}
		if err := copyFile(filepath.Join(root, pair[0]), pair[1], 0o600); err != nil {
			return err
		}
	}
	old := filepath.Join(metadata.InstallDir, hostBinaryName()) + ".old"
	if fileExists(old) {
		return copyFile(old, filepath.Join(metadata.InstallDir, hostBinaryName()), 0o755)
	}
	return nil
}

func readBackupMetadata(root string) (backupMetadata, error) {
	body, err := os.ReadFile(filepath.Join(root, "backup.json")) // #nosec G304 -- 备份目录
	if err != nil {
		return backupMetadata{}, err
	}
	var metadata backupMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return backupMetadata{}, err
	}
	return metadata, nil
}

func runExternal(ctx context.Context, override, name string, args ...string) error {
	if override != "" {
		name = override
	}
	// #nosec G204,G702 -- 可执行件是 pg_dump / pg_restore 或其显式替身，参数由本工具构造
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s 失败：%w（%s）", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runCommand(ctx context.Context, dir string, argv ...string) error {
	// #nosec G204 -- 停止命令由运维在 --stop 显式给出，不是外部输入
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v：%w（%s）", argv, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 不是目录", src)
	}
	return copyTree(src, dst)
}
