package app

// 本文件判 `secrets adopt-legacy` 里**不依赖迁移链**的两件事：备份清单的自洽核对
// （§2.12.0b 字段表）与副本名册门（§3.1.8c 校验 8）。
//
// 端到端用例（真迁移 + 真记录 + 零副作用，`TestSecretsAdoptLegacy_NoRuntimeSideEffects`）
// 归 T077 的 handoff 集：它需要合成的 v1～v5 库夹具，那套夹具在 `internal/storage`
// 包内，本包造不出来。这里用"副本库不是合法 SQLite"绕开迁移：名册门在打开副本库
// **之前**，所以拒绝码与放行顺序都能判，而且一次迁移都不跑。
// This file judges manifest self-consistency and the roster gate. Both run before the
// copy database is opened, so a non-SQLite copy proves the ordering without migrating.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
)

// legacyAdoptManifest 造一份自洽的 §2.12.0b 清单：schema v5、无既有记录、无名册。
// 两个配置哈希与库哈希都由用例给的两份字节算出来——"自洽"是算的，不是抄的。
// legacyAdoptManifest derives a self-consistent manifest from the two byte blobs.
func legacyAdoptManifest(db, config []byte) legacyBackupManifest {
	return legacyBackupManifest{
		FormatVersion:      1,
		SourceHost:         "source-host",
		SourceWorkspace:    "/srv/basalt/.basalt",
		SourceStoppedAt:    "2026-10-28T10:00:00Z",
		BackupCreatedAt:    "2026-10-28T10:05:00Z",
		Operator:           "operator-a",
		SchemaVersion:      5,
		DBSHA256:           hexSHA256(db),
		SourceConfigSHA256: hexSHA256(config),
		BackupConfigSHA256: hexSHA256(config),
		KeyFingerprint:     strings.Repeat("1", 64),
		KeyID:              "mk_legacy",
		KeyFileSHA256:      strings.Repeat("2", 64),
		RecordsAbsent:      true,
		ProcedureRef:       "OPS-2026-007",
		AnchorRef:          "CHG-2026-051",
	}
}

// writeLegacyAdoptBackup 落盘备份目录（清单 + 给定文件），返回目录与清单字节哈希。
// writeLegacyAdoptBackup writes the backup directory and returns the manifest digest.
func writeLegacyAdoptBackup(t *testing.T, manifest legacyBackupManifest, files map[string][]byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), body, 0o600))
	}
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "backup.json"), raw, 0o600))
	return dir, hexSHA256(raw)
}

// requireOfflineRefusal 判拒绝三件套：稳定码、以码开头的文案、文案片段。
// requireOfflineRefusal asserts the stable code, the code-first rendering and a snippet.
func requireOfflineRefusal(t *testing.T, err error, code, want string) {
	t.Helper()
	require.Error(t, err)
	var refusal *offlineRefusal
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, code, refusal.Code)
	require.True(t, strings.HasPrefix(err.Error(), code+":"),
		"错误文案必须以稳定码开头：%s", err)
	require.Contains(t, err.Error(), want)
}

// TestSecretsAdoptLegacy_BackupManifest 判清单自洽（§2.12.0b 字段表）。
// 中文验收点：procedure_ref / anchor_ref 非空；名册字段整组声明或整组为空；名册文件
// 真的被重算（不是"清单说有就算有"）；清单在命令面核对之后被改动即拒绝。
// TestSecretsAdoptLegacy_BackupManifest checks the manifest against its own files.
func TestSecretsAdoptLegacy_BackupManifest(t *testing.T) {
	db := []byte("legacy-v5-database-bytes")
	config := []byte(adoptConfigYAML("local_trust", ""))
	roster := []byte(`{"version":1,"instance_id":"inst_0123456789abcdef"}`)
	base := legacyAdoptManifest(db, config)

	// verify 在独立备份目录里重算清单；extra 是除库与配置之外的文件。
	verify := func(t *testing.T, manifest legacyBackupManifest, extra map[string][]byte) (legacyBackupManifest, error) {
		t.Helper()
		files := map[string][]byte{databaseFileName: db, configFileName: config}
		for name, body := range extra {
			files[name] = body
		}
		dir, sha := writeLegacyAdoptBackup(t, manifest, files)
		return legacyAdoptPort{}.verifyBackup(dir, cli.AdoptLegacyRequest{ManifestSHA256: sha}, cli.AdoptLegacyResult{})
	}

	t.Run("self_consistent_manifest_passes", func(t *testing.T) {
		manifest, err := verify(t, base, nil)
		require.NoError(t, err)
		require.Equal(t, "OPS-2026-007", manifest.ProcedureRef)
		require.Equal(t, "CHG-2026-051", manifest.AnchorRef)
		require.False(t, manifest.RosterPresent)
	})

	t.Run("missing_procedure_ref_is_refused", func(t *testing.T) {
		manifest := base
		manifest.ProcedureRef = "   " // 空白等于没给，不能靠 TrimSpace 之外的空格蒙混
		_, err := verify(t, manifest, nil)
		requireOfflineRefusal(t, err, legacyCodeBackupInvalid, "procedure_ref")
	})

	t.Run("missing_anchor_ref_is_refused", func(t *testing.T) {
		manifest := base
		manifest.AnchorRef = ""
		_, err := verify(t, manifest, nil)
		requireOfflineRefusal(t, err, legacyCodeBackupInvalid, "anchor_ref")
	})

	t.Run("roster_declared_without_fields_is_refused", func(t *testing.T) {
		manifest := base
		manifest.RosterPresent = true // 哈希与实例标识都空：半组声明
		_, err := verify(t, manifest, nil)
		requireOfflineRefusal(t, err, legacyCodeBackupInvalid, "roster_present=true")
	})

	t.Run("roster_fields_without_flag_are_refused", func(t *testing.T) {
		manifest := base
		manifest.RosterSHA256 = hexSHA256(roster) // present=false 却带了名册字段
		manifest.InstanceID = "inst_0123456789abcdef"
		_, err := verify(t, manifest, nil)
		requireOfflineRefusal(t, err, legacyCodeBackupInvalid, "却带了 roster_sha256")
	})

	t.Run("declared_roster_file_missing_in_backup_is_refused", func(t *testing.T) {
		manifest := base
		manifest.RosterPresent = true
		manifest.RosterSHA256 = hexSHA256(roster)
		manifest.InstanceID = "inst_0123456789abcdef"
		_, err := verify(t, manifest, nil) // 备份里没有 instance.json
		requireOfflineRefusal(t, err, legacyCodeBackupInvalid, instanceMarkerFileName+" 不可读")
	})

	t.Run("declared_roster_file_is_recomputed_and_accepted", func(t *testing.T) {
		manifest := base
		manifest.RosterPresent = true
		manifest.RosterSHA256 = hexSHA256(roster)
		manifest.InstanceID = "inst_0123456789abcdef"
		got, err := verify(t, manifest, map[string][]byte{instanceMarkerFileName: roster})
		require.NoError(t, err)
		require.True(t, got.RosterPresent)
		require.Equal(t, "inst_0123456789abcdef", got.InstanceID)
	})

	t.Run("manifest_changed_after_anchor_check_is_refused", func(t *testing.T) {
		// 清单字节在命令面核对之后被换掉：端口侧的纵深防御必须判出来。
		dir, _ := writeLegacyAdoptBackup(t, base, map[string][]byte{
			databaseFileName: db, configFileName: config,
		})
		_, err := legacyAdoptPort{}.verifyBackup(dir, cli.AdoptLegacyRequest{
			ManifestSHA256: strings.Repeat("0", 64),
		}, cli.AdoptLegacyResult{})
		requireOfflineRefusal(t, err, legacyCodeBackupInvalid, "在命令面核对之后被改动")
	})
}

// TestSecretsAdoptLegacy_RosterGate 判副本名册门（§3.1.8c 校验 8）。
// 中文验收点：清单声明的副本名册必须逐字节相符且实例标识一致；清单没声明则副本必须
// 没有名册；名册不可解析是 `backup_roster_invalid`；放行时两档身份证据分别落到
// `roster` / `attested_no_roster`，绝不写成"已证明"。
// TestSecretsAdoptLegacy_RosterGate proves the gate runs before the copy is opened.
func TestSecretsAdoptLegacy_RosterGate(t *testing.T) {
	db := []byte("legacy-v5-database-bytes")
	rosterA := []byte(`{"version":1,"instance_id":"inst_0123456789abcdef"}`)
	rosterB := []byte(`{"version":1,"instance_id":"inst_fedcba9876543210"}`)
	const idA = "inst_0123456789abcdef"
	const idB = "inst_fedcba9876543210"

	// newCopy 造隔离副本：`<root>/.basalt` + 同级 basalt.yaml。库内容刻意不是合法
	// SQLite——名册门在打开副本库之前，放行时会停在"打开副本库失败"。
	newCopy := func(t *testing.T) (ws string, config []byte) {
		t.Helper()
		root := t.TempDir()
		ws = filepath.Join(root, workspaceDirName)
		require.NoError(t, os.MkdirAll(ws, 0o700))
		config = []byte(adoptConfigYAML("local_trust", "",
			adoptKeyLine(filepath.Join(root, "private", masterKeyFileName))))
		require.NoError(t, os.WriteFile(filepath.Join(root, configFileName), config, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(ws, databaseFileName), db, 0o600))
		return ws, config
	}

	// adopt 备好与副本逐字节一致的备份，再跑一次端口；extra 是额外备份文件。
	adopt := func(t *testing.T, ws string, config []byte, manifest legacyBackupManifest,
		extra map[string][]byte,
	) (cli.AdoptLegacyResult, error) {
		t.Helper()
		files := map[string][]byte{databaseFileName: db, configFileName: config}
		for name, body := range extra {
			files[name] = body
		}
		backup, sha := writeLegacyAdoptBackup(t, manifest, files)
		return legacyAdoptPort{Now: baselineClock}.AdoptLegacySecrets(context.Background(), cli.AdoptLegacyRequest{
			Workspace:             ws,
			Backup:                backup,
			ManifestSHA256:        sha,
			AttestSourceUnchanged: "OPS-2026-007",
			Confirm:               true,
		})
	}

	// writeRoster 在副本工作区落一份名册。
	writeRoster := func(t *testing.T, ws string, raw []byte) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(ws, instanceMarkerFileName), raw, 0o600))
	}

	// 带名册的清单：哈希与实例标识指向 rosterA。
	rosterManifest := func(config []byte) legacyBackupManifest {
		manifest := legacyAdoptManifest(db, config)
		manifest.RosterPresent = true
		manifest.RosterSHA256 = hexSHA256(rosterA)
		manifest.InstanceID = idA
		return manifest
	}

	t.Run("manifest_declares_roster_but_copy_lacks_it", func(t *testing.T) {
		ws, config := newCopy(t)
		_, err := adopt(t, ws, config, rosterManifest(config),
			map[string][]byte{instanceMarkerFileName: rosterA})
		requireOfflineRefusal(t, err, legacyCodeIdentityMismatch, "副本里没有")
	})

	t.Run("copy_roster_digest_differs_from_manifest", func(t *testing.T) {
		ws, config := newCopy(t)
		writeRoster(t, ws, rosterB) // 副本名册换了人，长度合法但字节不同
		_, err := adopt(t, ws, config, rosterManifest(config),
			map[string][]byte{instanceMarkerFileName: rosterA})
		requireOfflineRefusal(t, err, legacyCodeIdentityMismatch, "副本名册与清单不符")
	})

	t.Run("manifest_instance_id_disagrees_with_roster_bytes", func(t *testing.T) {
		// 字节相符但清单自报的实例标识是另一个：哈希比对证明不了身份字段，
		// 这条分支就是为"清单在撒谎"准备的。
		ws, config := newCopy(t)
		writeRoster(t, ws, rosterA)
		manifest := rosterManifest(config)
		manifest.InstanceID = idB
		_, err := adopt(t, ws, config, manifest,
			map[string][]byte{instanceMarkerFileName: rosterA})
		requireOfflineRefusal(t, err, legacyCodeIdentityMismatch, "实例标识与清单不符")
	})

	t.Run("copy_roster_is_unparsable", func(t *testing.T) {
		ws, config := newCopy(t)
		writeRoster(t, ws, []byte("{not json"))
		_, err := adopt(t, ws, config, rosterManifest(config),
			map[string][]byte{instanceMarkerFileName: rosterA})
		requireOfflineRefusal(t, err, recoverCodeBackupRosterInvalid, "副本名册不可用")
	})

	t.Run("copy_has_roster_but_manifest_does_not", func(t *testing.T) {
		// 先建名册再采纳会把不在清单里的身份带进副本——按身份不符拒绝，不按"忽略"。
		ws, config := newCopy(t)
		writeRoster(t, ws, rosterA)
		_, err := adopt(t, ws, config, legacyAdoptManifest(db, config), nil)
		requireOfflineRefusal(t, err, legacyCodeIdentityMismatch, "副本已有名册")
	})

	t.Run("matching_roster_passes_gate_and_promotes_evidence_tier", func(t *testing.T) {
		ws, config := newCopy(t)
		writeRoster(t, ws, rosterA)
		result, err := adopt(t, ws, config, rosterManifest(config),
			map[string][]byte{instanceMarkerFileName: rosterA})
		// 名册门放行后停在打开副本库——证明门在库之前，且没有误判成身份不符。
		requireOfflineRefusal(t, err, legacyCodeStateNotLegacy, "副本")
		require.True(t, result.RosterPresent, "清单与副本名册相符时证据档位必须是 roster")
		require.Equal(t, "roster", result.IdentityEvidence)
	})

	t.Run("no_roster_anywhere_keeps_attested_tier", func(t *testing.T) {
		ws, config := newCopy(t)
		result, err := adopt(t, ws, config, legacyAdoptManifest(db, config), nil)
		requireOfflineRefusal(t, err, legacyCodeStateNotLegacy, "副本")
		require.False(t, result.RosterPresent)
		require.Equal(t, "attested_no_roster", result.IdentityEvidence)
	})
}
