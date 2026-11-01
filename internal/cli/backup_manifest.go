package cli

// 本文件是两条离线命令共用的**备份判定**面：形态按内容判、锚点按字节重算。
// 判据顺序（颗粒度文档 §2.12.0a 校验 0～1 / §2.12.0b 校验 3～4）：
//
//	① 目录可读且能判出**唯一**形态 —— 二义即 `backup_invalid`（退出 1、零写入）
//	② 清单文件的字节哈希与带外锚点逐字比对 —— 缺失 / 不符即 `backup_unverified`
//
// 为什么锚点必须在命令面算：备份里自带的哈希、审计行与"看起来匹配"都不构成来源证明；
// 唯一可信的是备份时写进变更单 / 交接单 / 离线台账里的那一串十六进制（C-51）。
// This file holds the backup shape/anchor decisions both offline commands share.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// exitPrecondition 是「前置或参数不满足」的退出码（颗粒度文档 §3.1.8b / §3.1.8c）。
// exitPrecondition is the documented "precondition or argument not satisfied" status.
const exitPrecondition = 2

// errAmbiguousBackup 表示备份目录的形态既像两条路线、又像两者都不是：不猜。
// errAmbiguousBackup marks a backup whose shape is ambiguous; guessing is forbidden.
var errAmbiguousBackup = fmt.Errorf("备份目录不构成唯一形态")

// classifyRecoverBackup 按**内容**判定 `recover-record` 的备份路线（§3.1.8b）：
// 含 `manifest.json` → 导出包 A；含 `basalt.db` 与 `instance.json` → 原生备份集 B；
// 两者都像或都不像 → 二义。**不**看目录名、扩展名或调用方口述。
// classifyRecoverBackup judges the recover-record route by content alone.
func classifyRecoverBackup(dir string) (BackupKind, error) {
	hasManifest := regularFileExists(filepath.Join(dir, "manifest.json"))
	hasDB := regularFileExists(filepath.Join(dir, "basalt.db"))
	hasRoster := regularFileExists(filepath.Join(dir, "instance.json"))
	switch {
	case hasManifest && (hasDB || hasRoster):
		return "", errAmbiguousBackup
	case hasManifest:
		return BackupKindExportPackage, nil
	case hasDB && hasRoster:
		return BackupKindNativeSet, nil
	default:
		return "", errAmbiguousBackup
	}
}

// classifyLegacyBackup 判定 `adopt-legacy` 的备份：只认原生备份集（含 `backup.json`）。
// classifyLegacyBackup accepts only the native backup set holding backup.json.
func classifyLegacyBackup(dir string) (BackupKind, error) {
	if regularFileExists(filepath.Join(dir, "backup.json")) {
		return BackupKindNativeSet, nil
	}
	return "", errAmbiguousBackup
}

// backupManifestName 返回该形态下承载清单的文件名（锚点哈希的对象）。
// backupManifestName names the file the external anchor hashes.
func backupManifestName(kind BackupKind) string {
	if kind == BackupKindExportPackage {
		return "manifest.json"
	}
	return "backup.json"
}

// verifiedBackupManifest 完成 ① 形态判定 ② 锚点重算比对，返回形态与小写十六进制哈希。
// 失败时按文档给码：不可读 → 2；二义 → 1（backup_invalid）；锚点缺失 / 不符 / 非法 → 1
// （backup_unverified）。三类失败都发生在调用端口之前，因此都零写入。
// verifiedBackupManifest decides the shape and checks the anchor before any port call.
func verifiedBackupManifest(
	out io.Writer,
	backupDir, expectSHA256 string,
	classify func(string) (BackupKind, error),
	shapeHint string,
) (BackupKind, string, error) {
	info, err := os.Stat(backupDir)
	if err != nil {
		return "", "", refuseCommand(out, exitPrecondition,
			"备份路径不可读：%v", err)
	}
	if !info.IsDir() {
		return "", "", refuseCommand(out, exitPrecondition,
			"备份路径不是目录：%s", backupDir)
	}
	kind, err := classify(backupDir)
	if err != nil {
		return "", "", refuseCommand(out, exitValidation, "backup_invalid：%s", shapeHint)
	}
	manifestPath := filepath.Join(backupDir, backupManifestName(kind))
	computed, err := sha256FileHex(manifestPath)
	if err != nil {
		return "", "", refuseCommand(out, exitValidation,
			"backup_invalid：备份清单不可读（%s）：%v", backupManifestName(kind), err)
	}
	expected, ok := normalizeSHA256(expectSHA256)
	if !ok {
		return "", "", refuseCommand(out, exitValidation,
			"backup_unverified：缺少合法格式的 --expect-backup-sha256（需要 64 位十六进制，来自备份时的带外记录）")
	}
	if expected != computed {
		return "", "", refuseCommand(out, exitValidation,
			"backup_unverified：外部锚点与重算的备份清单哈希不符（expected=%s computed=%s）",
			expected, computed)
	}
	return kind, computed, nil
}

// exitValidation 是「校验失败」的退出码（颗粒度文档 §3.1.8b / §3.1.8c）。
// exitValidation is the documented "verification failed" status.
const exitValidation = 1

// refuseCommand 构造带退出码的拒绝错误并把它写到命令输出面：脚本读退出码，人读文案。
// 文案与错误只差「同一句话走两条通道」——`out` 供管道与测试捕获，返回的错误供 cobra
// 打印到标准错误并让装配骨架取出退出码。
// refuseCommand prints the refusal and returns the status-bearing error behind it.
func refuseCommand(out io.Writer, code int, format string, args ...any) error {
	err := NewExitError(code, format, args...)
	if out != nil {
		fmt.Fprintln(out, err.Error())
	}
	return err
}

// errOfflinePortMissing 是离线端口缺件的固定错误（与 secrets 服务缺件同类但不同因：
// 后者"没装配重路径"，前者"没装配离线适配器"）。
// errOfflinePortMissing keeps a wiring gap distinct from a deployment state.
func errOfflinePortMissing(command string) error {
	return fmt.Errorf("%s 的离线端口未装配（需要主程序的离线适配器装配）——不提供降级实现", command)
}

// sha256FileHex 返回文件字节的小写十六进制 SHA-256。
// sha256FileHex hashes a file's bytes into lowercase hex.
//
// 路径来自备份目录参数，读取经所在目录的句柄（`os.Root`）——与 `readConfigFile`
// 同一先例：既消除静态扫描的 G304，也不跟随越出目录的链接。
// The read is scoped through a root handle on the file's own directory.
func sha256FileHex(path string) (string, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(filepath.Base(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// normalizeSHA256 规范化外部锚点：去空白、要求 64 位十六进制、统一小写。
// 长度或字符集不符即视为**缺失**——半截哈希与空串同样不可信。
// normalizeSHA256 normalises the anchor; anything short of 64 hex digits counts
// as absent.
func normalizeSHA256(raw string) (string, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if len(trimmed) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(trimmed); err != nil {
		return "", false
	}
	return trimmed, true
}

// regularFileExists 判定路径是否存在且为**常规文件**（目录、设备、坏链接都不算）。
// regularFileExists reports a regular file; directories and broken links do not count.
func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}
