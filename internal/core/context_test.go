package core_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是上下文装配契约（§3.1.0）的**黑盒**验收：预算判定链、来源引用与摘要可注入性。
// 这些类型是跨包契约（`internal/compaction` / `internal/knowledge` / `internal/app` 都消费），
// 故用外部测试包钉住"导出面"——同包测试看不出"导出语义被悄悄改了"。
// This file pins the exported context contracts from outside the package: the budget
// chain, source references and summary injectability.

// TestContext_OutputReserveNotProtocolReserve 守"输出预留不复用协议余量"（C-31）：
// 生效上限只减协议余量；输出预留是**独立**字段，两者互不覆盖。
//
// 逆改点：把 `Limit()` 写成 `ContextTokens − ProtocolReserve − OutputReserve`
// （或让 `ProtocolReserve` 在构造时吸收 `OutputReserve`）→ 第二、三条断言转红；
// 把零值 `OutputReserve` 在 `Limit()` 里补成 `ReserveTokens` → 第一条的对照读数漂移。
// 缺省 0 = 未接线的拒绝路径由压缩引擎判（`Compact` 收到 Fixed / Budget 零值即拒绝），
// 不在本类型的算术里——本用例只钉"两个预留各自读得回来、且只有协议余量减进 Limit"。
func TestContext_OutputReserveNotProtocolReserve(t *testing.T) {
	base := core.ContextBudget{
		ContextTokens:    32000,
		ProtocolReserve:  core.ReserveTokens,
		EstimatorVersion: core.EstimateVersion,
	}
	require.Equal(t, 32000-core.ReserveTokens, base.Limit(),
		"Limit 必须等于 ContextTokens − ProtocolReserve")

	withOutput := base
	withOutput.OutputReserve = 700
	require.Equal(t, base.Limit(), withOutput.Limit(),
		"输出预留不得折进 Limit——它是回复预留，不是协议余量")
	require.Equal(t, core.ReserveTokens, withOutput.ProtocolReserve,
		"设输出预留不得改写协议余量")
	require.Equal(t, 700, withOutput.OutputReserve,
		"两个预留各自独立，读回来不得互相覆盖")

	// 输出预留恰好等于协议余量时也必须仍是两个字段：值相等不等于可互换。
	same := base
	same.OutputReserve = core.ReserveTokens
	require.Equal(t, base.Limit(), same.Limit(),
		"OutputReserve == ReserveTokens 是合法配置，但仍不得参与 Limit")
}

// TestContext_EvidenceRefRequiresIdentity 守"引用必须有身份"：空 ID 无法定位、
// 空版本无法复核失效——缺一即**无引用**，不得当成通配引用放行（注入闸门按此判卷）。
// TestContext_EvidenceRefRequiresIdentity pins deny-by-default reference validity.
func TestContext_EvidenceRefRequiresIdentity(t *testing.T) {
	full := core.EvidenceRef{ID: "doc-1", VersionID: "v1", ContentHash: "h1"}
	require.True(t, full.Valid())
	require.True(t, core.EvidenceRef{ID: "doc-1", VersionID: "v1"}.Valid(),
		"内容哈希可缺省——它只用于复核同一版本的内容是否被改写")

	require.False(t, core.EvidenceRef{VersionID: "v1"}.Valid(), "空 ID 必须非法")
	require.False(t, core.EvidenceRef{ID: "doc-1"}.Valid(), "空版本必须非法")
	require.False(t, core.EvidenceRef{}.Valid(), "零值引用必须非法")
}

// TestContext_SummaryValidityRejectsZero 守"未校验不等于可注入"：零值 `Validity`
// 必须判不可注入（fail-closed），只有显式的两个取值成立。
// TestContext_SummaryValidityRejectsZero keeps the zero value out of the injectable set.
func TestContext_SummaryValidityRejectsZero(t *testing.T) {
	require.True(t, core.SummaryValid.Valid())
	require.True(t, core.SummaryStale.Valid())
	require.False(t, core.SummaryValidity("").Valid(), "零值 = 未校验，必须非法")
	require.False(t, core.SummaryValidity("injectable").Valid(), "词表外的取值必须非法")
}
