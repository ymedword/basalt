//go:build integration

package app

// 本文件是模块卡 §2.12.1（T141 / C-38 / D-32）「凭据校验的两个对象」的判卷：
//
//	(a) 历史密文完整性：`credential_versions` 中全部 `ciphertext IS NOT NULL` 的版本
//	    可解，且 `key_id` / AAD 与当前主密钥一致（含历史版本、MCP 每个槽位、已撤销但
//	    保留密文的版本）；
//	(b) 当前可用性：active 凭据对当前已发布资源、目标地址与 MCP `binding_key` 的核对。
//
// 修复顺序按卡片硬性要求（先红后修）：先跑本用例观察现状下 MCP 槽位被计入 `Failed`
// 的逐字输出，再改实现；红态读数记入 run-state 实施记录。
// This file pins the two report objects of §2.12.1: (a) historical ciphertext integrity
// over every sealed version, and (b) current usability against the published resources.
// The MCP slot gap was reproduced red before the fix, as the card requires.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// secretsVerifySlotOne / secretsVerifySlotTwo 是同一 MCP server 的两个托管槽位：
	// 只要实现按 `binding_key` 区分身份，两条都必须被覆盖且判为可用。
	secretsVerifySlotOne = "MCP_VERIFY_SLOT_ONE"
	secretsVerifySlotTwo = "MCP_VERIFY_SLOT_TWO"

	// 明文哨兵：任何可读面（含 WARN 日志与报告）出现即是判卷失败。
	secretsVerifyModelOne     = "sk-verify-secret-one-1a2b"
	secretsVerifyModelTwo     = "sk-verify-secret-two-3c4d"
	secretsVerifySlotOneValue = "slot-one-secret-value-5e6f"
	secretsVerifySlotTwoValue = "slot-two-secret-value-7g8h"
)

// seedMCPSlotCredential 按生产同一条 AAD（`MCPCredentialTargetBinding` +
// `SealCredential`）直接写一条 MCP 槽位托管凭据：身份带 `BindingKey`，目标绑定绑死
// `(server uid, slot)`。走裸仓储是刻意的——正常写路径（ResourceAdmin）会顺带发布定义，
// 本用例要自己控制定义与凭据的先后，以覆盖"校验只依赖数据行"的口径。
// seedMCPSlotCredential seals one MCP slot credential with the production AAD path.
func seedMCPSlotCredential(t *testing.T, f *resolverFixture, server, slot, plaintext string) string {
	t.Helper()
	uid := fixtureResourceUID(t, core.ResourceMCPServer, server)
	target, err := MCPCredentialTargetBinding(uid, slot)
	require.NoError(t, err)
	credID := fixtureCredentialID(t, "mcp_server/"+server+"/"+slot)
	aad := CredentialAAD{
		OwnerKind:     core.BindingMCPServer,
		OwnerUID:      uid,
		CredentialID:  credID,
		VersionNo:     1,
		TargetBinding: target,
	}
	nonce, ciphertext, err := SealCredential(f.key, aad, []byte(plaintext))
	require.NoError(t, err)
	require.NoError(t, f.credentials.Create(context.Background(), core.CredentialRecord{
		ID:          credID,
		OwnerKind:   core.BindingMCPServer,
		OwnerUID:    uid,
		BindingKey:  slot,
		OwnerName:   server,
		KeyID:       f.key.KeyID,
		OperationID: f.opID,
		CreatedAt:   baselineClock().UnixMilli(),
	}, core.CredentialVersionRecord{
		KeyID:         f.key.KeyID,
		TargetBinding: target,
		Last4:         last4Of(plaintext),
		Nonce:         nonce,
		Ciphertext:    ciphertext,
		OperationID:   f.opID,
	}))
	return credID
}

// TestSecretsVerify_CoversMcpSlotCredentials：1 条非 MCP 托管凭据换过一次 Key（2 个
// 密文版本）+ 1 个已发布 MCP server 的两个托管槽位（`binding_key` 不同），
// `secrets verify` 必须覆盖全部密文版本且全部判为可用。
//
// 红态（修复前）：`ListActive` 只数 active 身份（3 条），MCP 槽位走
// `CredentialByOwner`（SQL 只查 `binding_key = ”`）查不到身份 → 2 条被计入 `Failed`。
// 逆改点：校验退回"只遍历 active 身份 + 无槽位身份"→ `Checked`=3 / `Failed`=2，本用例红。
// TestSecretsVerify_CoversMcpSlotCredentials covers every sealed version, slot included.
func TestSecretsVerify_CoversMcpSlotCredentials(t *testing.T) {
	ctx := context.Background()
	f := newCredentialOpsFixture(t)
	logs := &strings.Builder{}
	service := newTestSecretsService(t, f, f.keyPath, logs)

	// 1 条非 MCP 托管凭据：两次写 Key = 第 2 版 active、第 1 版 superseded 且密文保留。
	f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
	first := writeConnection(t, f, "alpha", secretsVerifyModelOne)
	second := writeConnection(t, f, "alpha", secretsVerifyModelTwo)
	require.Equal(t, 1, first.VersionNo)
	require.Equal(t, 2, second.VersionNo)

	// 1 个已发布 MCP server，声明两个托管槽位；两条槽位凭据各自独立身份。
	const server = "vault"
	version := f.seedVersion(t, core.ResourceMCPServer, server,
		mcpDefinition(server, "vault-mcp", secretsVerifySlotOne, secretsVerifySlotTwo), "", "")
	f.publish(t, core.ResourceMCPServer, server, version.ID, true)
	seedMCPSlotCredential(t, f.resolverFixture, server, secretsVerifySlotOne, secretsVerifySlotOneValue)
	seedMCPSlotCredential(t, f.resolverFixture, server, secretsVerifySlotTwo, secretsVerifySlotTwoValue)

	report, err := service.Verify(ctx)
	require.NoError(t, err)
	// 红绿两态都打印实测读数与 WARN 明细：红态证据要能逐字看到 MCP 槽位被计入失败。
	t.Logf("secrets verify 报告：Checked=%d Failed=%d HistoricalOK=%d CurrentChecked=%d CurrentFailed=%d",
		report.Checked, report.Failed, report.HistoricalOK, report.CurrentChecked, report.CurrentFailed)
	if logs.Len() > 0 {
		t.Logf("WARN 日志：\n%s", logs.String())
	}

	// (a) 密文版本口径：模型连接 2 个版本 + MCP 槽位 2 条 = 4，全部可解。
	require.Equal(t, 4, report.Checked,
		"Checked 必须覆盖全部密文版本（含历史版本与 MCP 槽位），而不是只数 active 身份")
	require.Zero(t, report.Failed, "MCP 槽位凭据按 binding_key 归位后不得计入失败")
	require.Equal(t, 1, report.HistoricalOK, "只有 alpha 的第 1 版是非当前版本且可解")
	require.Zero(t, report.RevokedWithCiphertext)
	require.Zero(t, report.MissingCiphertext)
	require.Zero(t, report.KeyIDMismatch)
	require.Zero(t, report.AADMismatch)

	// (b) 当前可用性：alpha + 两个 MCP 槽位，三条都可用（目标绑定与槽位都归位）。
	require.Equal(t, 3, report.CurrentChecked)
	require.Zero(t, report.CurrentFailed, "MCP 槽位按 (server uid, 槽位名) 重算目标绑定后必须可用")

	// 输出纪律：报告与 WARN 日志都不得出现明文。
	for _, plaintext := range []string{
		secretsVerifyModelOne, secretsVerifyModelTwo,
		secretsVerifySlotOneValue, secretsVerifySlotTwoValue,
	} {
		require.NotContains(t, logs.String(), plaintext, "日志不得出现凭据明文")
	}
}

// alphaCredentialID 取 alpha 模型连接当前凭据身份 ID（(a) 篡改按它定位版本行）。
// alphaCredentialID resolves the fixture credential identity of alpha.
func alphaCredentialID(t *testing.T, f *resolverFixture) string {
	t.Helper()
	identity, err := f.credentials.CredentialByOwner(context.Background(),
		core.BindingModelConnection, fixtureResourceUID(t, core.ResourceModelConnection, "alpha"))
	require.NoError(t, err)
	return identity.ID
}

// TestSecretsVerify_DistinguishesHistoricalAndCurrent：两个报告对象必须能分别判红，
// 否则"合并成一个 Failed"会让运维分不清"历史密文坏了（新调用没事）"与"当前凭据
// 不可用（马上会失败）"。
//
// 两个子例互为逆改点：
//   - 只验当前版本的实现 → 子例一 (a) 红（历史版本被篡改却查不出来）；
//   - 只解密不核对当前绑定的实现 → 子例二 (b) 红（地址换了仍报通过）。
//
// TestSecretsVerify_DistinguishesHistoricalAndCurrent keeps the two report objects
// decidable apart in both directions.
func TestSecretsVerify_DistinguishesHistoricalAndCurrent(t *testing.T) {
	t.Run("historical_corruption_is_history_only", func(t *testing.T) {
		ctx := context.Background()
		f := newCredentialOpsFixture(t)
		logs := &strings.Builder{}
		service := newTestSecretsService(t, f, f.keyPath, logs)
		f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
		require.Equal(t, 1, writeConnection(t, f, "alpha", secretsVerifyModelOne).VersionNo)
		require.Equal(t, 2, writeConnection(t, f, "alpha", secretsVerifyModelTwo).VersionNo)

		// 只翻历史版本（v1）密文的一个字节：当前版本（v2）完好。
		_, err := f.db.ExecContext(ctx,
			`UPDATE credential_versions
			    SET ciphertext = set_byte(ciphertext, 0, (get_byte(ciphertext, 0) + 1) % 256)
			  WHERE credential_id = $1 AND version_no = 1`, alphaCredentialID(t, f.resolverFixture))
		require.NoError(t, err)

		report, err := service.Verify(ctx)
		require.NoError(t, err)
		t.Logf("篡改历史版本后：Checked=%d Failed=%d AADMismatch=%d HistoricalOK=%d CurrentChecked=%d CurrentFailed=%d",
			report.Checked, report.Failed, report.AADMismatch, report.HistoricalOK,
			report.CurrentChecked, report.CurrentFailed)

		require.Equal(t, 2, report.Checked)
		require.Equal(t, 1, report.Failed, "(a) 必须发现历史版本损坏")
		require.Equal(t, 1, report.AADMismatch)
		require.Zero(t, report.HistoricalOK, "损坏的历史版本不计入可解")
		require.Equal(t, 1, report.CurrentChecked)
		require.Zero(t, report.CurrentFailed, "(b) 当前版本完好，不得跟着报红")
	})

	t.Run("stale_binding_is_current_only", func(t *testing.T) {
		ctx := context.Background()
		f := newCredentialOpsFixture(t)
		logs := &strings.Builder{}
		service := newTestSecretsService(t, f, f.keyPath, logs)
		f.seedManagedModelConnection(t, "alpha", "https://api.example.com/v1")
		writeConnection(t, f, "alpha", secretsVerifyModelOne)
		writeConnection(t, f, "alpha", secretsVerifyModelTwo)

		// 地址换到 v2 并重新发布：密文仍按旧地址封，解密本身不受影响（AAD 取自
		// 版本行），但目标绑定与当前定义不一致——必须重新录入 Key。
		version := f.seedVersion(t, core.ResourceModelConnection, "alpha",
			managedProviderDefinition("alpha", "https://api.example.com/v2", "alpha-model"), "", "")
		f.publish(t, core.ResourceModelConnection, "alpha", version.ID, true)

		report, err := service.Verify(ctx)
		require.NoError(t, err)
		t.Logf("地址变更后：Checked=%d Failed=%d CurrentChecked=%d CurrentFailed=%d TargetMismatch=%d",
			report.Checked, report.Failed, report.CurrentChecked, report.CurrentFailed,
			report.TargetMismatch)

		require.Equal(t, 2, report.Checked)
		require.Zero(t, report.Failed, "(a) 历史密文本身完好")
		require.Equal(t, 1, report.CurrentChecked)
		require.Equal(t, 1, report.CurrentFailed, "(b) 必须发现目标绑定过期")
		require.Equal(t, 1, report.TargetMismatch)
	})
}
