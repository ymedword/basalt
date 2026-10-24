package workspace

// 本文件是状态文件 schema v2 身份面的关键回归用例（颗粒度文档 R4.4 / T109 /
// 专属 check `012-workspace-bindings`）：v1 → v2 原子升级保留状态与代数、
// checksum 按新字节重算并覆盖 agent_uid、升级失败保持 v1 原样且无临时残片。
// Regression tests for the state-file identity surface: in-place v1→v2 upgrade,
// checksum recomputation covering agent_uid, and failure leaving v1 untouched.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// seedV1StateFile 在临时工作区里落一份 US-11 形态（v1）的状态文件并返回其原始字节。
// seedV1StateFile writes a US-11-shaped (v1) state file and returns its raw bytes.
func seedV1StateFile(t *testing.T, root string) []byte {
	t.Helper()
	rec := core.LifecycleRecord{
		SchemaVersion: lifecycleSchemaVersionV1,
		Agent:         "ops",
		State:         core.AgentActive,
		Generation:    2,
		UpdatedAt:     1760000000000,
		OperationID:   "op-1760000000000-aaaaaa",
	}
	raw, err := encodeLifecycleState(rec)
	require.NoError(t, err)
	writeLifecycleStateFile(t, root, "ops", raw)
	return raw
}

// TestIdentityState_UpgradeV1ToV2：v1 文件升级补发稳定身份，状态 / 代数 / 代理名
// 不变；已是 v2 再升为 no-op；给定身份建初始状态时身份不一致 → 冲突。
func TestIdentityState_UpgradeV1ToV2(t *testing.T) {
	ctx := context.Background()
	store, root, _, clock := newLifecycleTestStore(t)
	seedV1StateFile(t, root)

	rec, upgraded, err := store.UpgradeIdentity(ctx, "ops", "op-1760000000001-mig001")
	require.NoError(t, err)
	require.True(t, upgraded)
	require.Equal(t, lifecycleSchemaVersion, rec.SchemaVersion)
	require.True(t, rec.AgentUID.Valid())
	require.Equal(t, core.AgentActive, rec.State)
	require.Equal(t, int64(2), rec.Generation)
	require.Equal(t, "op-1760000000001-mig001", rec.OperationID)
	require.Equal(t, clock.now().UnixMilli(), rec.UpdatedAt)

	// 落盘结果可被严格读取，且身份与返回记录一致。
	read, exists, err := store.Read(ctx, "ops")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, rec.AgentUID, read.AgentUID)
	require.Equal(t, rec.Checksum, read.Checksum)

	// 已是 v2 → no-op，身份不变。
	again, upgraded, err := store.UpgradeIdentity(ctx, "ops", "op-1760000000002-mig002")
	require.NoError(t, err)
	require.False(t, upgraded)
	require.Equal(t, rec.AgentUID, again.AgentUID)

	// 给定身份建初始状态：身份一致 → 原样返回；不一致 → 冲突，文件不变。
	before := readLifecycleStateFile(t, root, "ops")
	same, err := store.BootstrapIdentity(ctx, "ops", rec.AgentUID, "op-1760000000003-mig003")
	require.NoError(t, err)
	require.Equal(t, rec.AgentUID, same.AgentUID)
	require.Equal(t, before, readLifecycleStateFile(t, root, "ops"))

	other, err := core.NewAgentUID()
	require.NoError(t, err)
	_, err = store.BootstrapIdentity(ctx, "ops", other, "op-1760000000004-mig004")
	require.ErrorIs(t, err, core.ErrLifecycleConflict)
	require.Equal(t, before, readLifecycleStateFile(t, root, "ops"))

	// 文件缺失：升级返回 (zero,false,nil)，交由调用方记 legacy_unrecorded。
	missing, upgraded, err := store.UpgradeIdentity(ctx, "ghost", "op-1760000000005-mig005")
	require.NoError(t, err)
	require.False(t, upgraded)
	require.Equal(t, core.LifecycleRecord{}, missing)
}

// TestIdentityState_ChecksumRecomputed：升级后的 checksum 必须覆盖新字节（含
// agent_uid）——独立重算与改动 agent_uid 后解码失败两条判据同时成立。
func TestIdentityState_ChecksumRecomputed(t *testing.T) {
	ctx := context.Background()
	store, root, _, _ := newLifecycleTestStore(t)
	v1Raw := seedV1StateFile(t, root)

	rec, upgraded, err := store.UpgradeIdentity(ctx, "ops", "op-1760000000001-mig001")
	require.NoError(t, err)
	require.True(t, upgraded)

	raw := readLifecycleStateFile(t, root, "ops")
	require.NotEqual(t, v1Raw, raw, "v1 字节必须已被 v2 取代")

	// 独立重算：把 checksum 值替换为空串后对整份字节做 sha256。
	blanked := bytes.Replace(raw, []byte(rec.Checksum), []byte(""), 1)
	sum := sha256.Sum256(blanked)
	require.Equal(t, checksumPrefix+hex.EncodeToString(sum[:]), rec.Checksum)

	// 改动 agent_uid（保持合法格式）但不动 checksum → 校验和不符，拒绝读取。
	replacement, err := core.NewAgentUID()
	require.NoError(t, err)
	require.NotEqual(t, rec.AgentUID, replacement)
	tampered := bytes.Replace(raw, []byte(rec.AgentUID.String()), []byte(replacement.String()), 1)
	require.NotEqual(t, raw, tampered)
	_, err = decodeLifecycleState(tampered)
	require.ErrorContains(t, err, "校验和不符")
}

// TestIdentityState_UpgradeFailureKeepsV1：rename 失败 → 错误上报、原文件逐字节
// 保持 v1、无临时残片；损坏文件 → 拒绝升级且不覆盖。
func TestIdentityState_UpgradeFailureKeepsV1(t *testing.T) {
	ctx := context.Background()
	store, root, _, _ := newLifecycleTestStore(t)
	v1Raw := seedV1StateFile(t, root)

	store.rename = func(*os.Root, string, string) error {
		return errors.New("注入的 rename 失败")
	}
	_, _, err := store.UpgradeIdentity(ctx, "ops", "op-1760000000001-mig001")
	require.ErrorContains(t, err, "注入的 rename 失败")
	require.Equal(t, v1Raw, readLifecycleStateFile(t, root, "ops"))

	// v1 原样可读：失败不把 Agent 拖成不可用状态。
	rec, exists, err := store.Read(ctx, "ops")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, lifecycleSchemaVersionV1, rec.SchemaVersion)
	require.Empty(t, rec.AgentUID)

	// 临时文件清理干净：控制目录里只剩状态文件本身。
	entries, err := os.ReadDir(stateDirPath(root))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "ops.json", entries[0].Name())

	// 损坏文件：拒绝升级，原字节不被覆盖。
	store.rename = nil
	writeLifecycleStateFile(t, root, "ops", []byte("{not json"))
	_, _, err = store.UpgradeIdentity(ctx, "ops", "op-1760000000002-mig002")
	require.Error(t, err)
	require.Equal(t, []byte("{not json"), readLifecycleStateFile(t, root, "ops"))
}

// stateDirPath 返回某个工作区根下的状态文件目录（测试直读目录用）。
// stateDirPath returns the control/agents directory of a workspace root.
func stateDirPath(root string) string {
	return filepath.Dir(stateFilePath(root, "ops"))
}
