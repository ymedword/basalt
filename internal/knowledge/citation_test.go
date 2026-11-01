package knowledge

// 本文件覆盖 T039 的引用解析（US-16 §2.10.4 / §3.1.3 / C-33）。三条具名用例逐字落地
// 设计 §4 R16，中文验收点保留原文注释；其后是本轮补强的判别力用例——
// 设计给出的三条只断言 `Supported` 为空，一个"永远返回 Unverified"的实现同样能全绿，
// 因此补一条正向对照（引用即复述必须判 Supported），并把"哪一类"钉死到具体类别上。
//
// This file covers citation resolution. The three R16 cases land verbatim; the extra
// cases pin each verdict to its class, because an implementation that always answers
// "Unverified" would pass the three R16 cases unchanged.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// R16 —— 引用可定位 ≠ 结论被支持。
// 逆改点：located 成立就直接判 Supported → 第一个用例（原文"未验证"、答案"已验证"）
// 会进 Supported；把开放结论默认放行 → 第二个用例的 Unverified 变 Supported。
func TestResolveCitations_ContradictionIsUnsupported(t *testing.T) {
	src := CitationSource{
		ChunkID: "c1", VersionID: "v1", ContentHash: "sha256:aaaa",
		Text: "恢复流程尚未验证，发布前必须补测。",
	}
	answer := "恢复流程已验证，可以发布（来源 c1）。"

	report := ResolveCitations(answer, []CitationSource{src})
	require.Empty(t, report.Supported, "可定位但结论与原文矛盾，不得判 Supported")
	require.Len(t, report.Unsupported, 1, "极性 / 状态词矛盾必须判 Unsupported")
}

func TestResolveCitations_OpenClaimStaysUnverified(t *testing.T) {
	src := CitationSource{
		ChunkID: "c2", VersionID: "v1", ContentHash: "sha256:bbbb",
		Text: "备份目录为 /var/backups，保留 7 天。",
	}
	answer := "备份策略足以满足审计要求（来源 c2）。" // 原文没说"足以满足"，无法确定性判定

	report := ResolveCitations(answer, []CitationSource{src})
	require.Empty(t, report.Supported, "无法判定支持关系时不得默认 Supported")
	require.Len(t, report.Unverified, 1, "开放结论归 Unverified")
}

func TestResolveCitations_TruncatedSpanNotSupported(t *testing.T) {
	src := CitationSource{
		ChunkID: "c3", VersionID: "v1", ContentHash: "sha256:cccc",
		Text: "第一段……", Truncated: true,
	}
	report := ResolveCitations("见 c3。", []CitationSource{src})
	require.Empty(t, report.Supported, "被截断的 span 连 located 都不成立")
}

// 正向对照：引用即复述（主张的全部内容词都在原文里）必须判 Supported。
// 逆改点：把 entailed 恒返回 false（"全部归 Unverified"的退化实现）→ 本用例 Supported 为空。
// 设计的三条 R16 用例都只判"不得 Supported"，没有这一条就测不出退化实现。
func TestResolveCitations_QuotedClaimIsSupported(t *testing.T) {
	src := CitationSource{
		ChunkID: "c2", VersionID: "v1",
		EvidenceRef: EvidenceRef{Kind: "knowledge_document", ID: "doc-1", VersionID: "v1", ContentHash: "sha256:bbbb"},
		ContentHash: "sha256:bbbb",
		Text:        "备份目录为 /var/backups，保留 7 天。",
	}
	answer := "备份目录为 /var/backups，保留 7 天（来源 c2）。"

	report := ResolveCitations(answer, []CitationSource{src})
	require.Len(t, report.Supported, 1, "主张的词全部出自原文，应判 Supported")
	require.Empty(t, report.Unsupported)
	require.Empty(t, report.Unverified)
	require.Equal(t, "c2", report.Supported[0].ChunkID)
	require.Equal(t, "sha256:bbbb", report.Supported[0].ContentHash, "Supported 必须带可定位证据")
	require.Equal(t, "备份目录为 /var/backups，保留 7 天", report.Supported[0].Claim.Text,
		"主张文本要去掉引用标记")
}

// 被截断的引用必须落在 Unsupported（不是 Unverified）：span 不完整时连"原文是不是这么说的"
// 都无从谈起，按"无法判定"放行会把它洗成 Unverified。
// 逆改点：locate 只看 VersionID / ContentHash、忽略 Truncated → 本用例落进 Unverified。
func TestResolveCitations_TruncatedSpanLandsUnsupported(t *testing.T) {
	src := CitationSource{
		ChunkID: "c3", VersionID: "v1", ContentHash: "sha256:cccc",
		Text: "第一段……", Truncated: true,
	}
	report := ResolveCitations("见 c3。", []CitationSource{src})
	require.Len(t, report.Unsupported, 1)
	require.Equal(t, "span_truncated", report.Unsupported[0].Reason)
	require.Empty(t, report.Unverified)
}

// 引用了本次上下文根本没读过的片段：只进 Unresolved，不构成可判定主张。
// 逆改点：显式包裹里的未知标识被当普通文本吞掉 → Unresolved 为空。
func TestResolveCitations_UnknownChunkIsUnresolved(t *testing.T) {
	src := CitationSource{ChunkID: "c1", VersionID: "v1", ContentHash: "sha256:aaaa", Text: "无关原文。"}
	report := ResolveCitations("（来源 ghost）", []CitationSource{src})
	require.Equal(t, []string{"ghost"}, report.Unresolved)
	require.Empty(t, report.Supported)
	require.Empty(t, report.Unsupported)
	require.Empty(t, report.Unverified)
}

// 读取时用的引用与实际返回内容对不上：版本或内容哈希不一致都不得判"可定位"。
// 逆改点：locate 不核对 EvidenceRef → 两个子用例都会落到 Supported / Unverified。
func TestResolveCitations_EvidenceRefMismatchIsUnsupported(t *testing.T) {
	t.Run("content_hash", func(t *testing.T) {
		src := CitationSource{
			ChunkID: "c1", VersionID: "v1", ContentHash: "sha256:new",
			EvidenceRef: EvidenceRef{Kind: "knowledge_document", ID: "doc-1", VersionID: "v1", ContentHash: "sha256:old"},
			Text:        "恢复流程尚未验证。",
		}
		report := ResolveCitations("恢复流程尚未验证（来源 c1）。", []CitationSource{src})
		require.Len(t, report.Unsupported, 1)
		require.Equal(t, "content_mismatch", report.Unsupported[0].Reason)
	})
	t.Run("version", func(t *testing.T) {
		src := CitationSource{
			ChunkID: "c1", VersionID: "v2", ContentHash: "sha256:new",
			EvidenceRef: EvidenceRef{Kind: "knowledge_document", ID: "doc-1", VersionID: "v1", ContentHash: "sha256:new"},
			Text:        "恢复流程尚未验证。",
		}
		report := ResolveCitations("恢复流程尚未验证（来源 c1）。", []CitationSource{src})
		require.Len(t, report.Unsupported, 1)
		require.Equal(t, "version_mismatch", report.Unsupported[0].Reason)
	})
}

// 数字与单位必须原样对得上：把"保留 7 天"说成"保留 30 天"是矛盾，不是"无法判定"。
// 逆改点：去掉 numberConflict → 本用例落进 Unverified（数字被当普通内容词忽略）。
func TestResolveCitations_NumberMismatchIsUnsupported(t *testing.T) {
	src := CitationSource{
		ChunkID: "c2", VersionID: "v1", ContentHash: "sha256:bbbb",
		Text: "备份目录为 /var/backups，保留 7 天。",
	}
	report := ResolveCitations("备份保留 30 天（来源 c2）。", []CitationSource{src})
	require.Len(t, report.Unsupported, 1)
	require.Equal(t, "number_conflict", report.Unsupported[0].Reason)
	require.Empty(t, report.Supported)
}

// "可定位"只是一半：标记落在更长的标识符里就不算引用（c10 不是 c1）。
// 逆改点：用 strings.Contains 代替独立标识符匹配 → c10 会被当成引用了 c1。
func TestResolveCitations_MarkerInsideLongerIdentifierIsNotACitation(t *testing.T) {
	src := CitationSource{ChunkID: "c1", VersionID: "v1", ContentHash: "sha256:aaaa", Text: "无关原文。"}
	report := ResolveCitations("见 c10。", []CitationSource{src})
	require.Empty(t, report.Supported)
	require.Empty(t, report.Unsupported)
	require.Empty(t, report.Unverified)
}

// 四类互斥：每句话只落进一个类别。
// 逆改点：把同一句同时写进两个桶（例如矛盾后再走蕴含）→ 计数不再各为 1。
func TestResolveCitations_ClassesAreMutuallyExclusive(t *testing.T) {
	sources := []CitationSource{
		{ChunkID: "c1", VersionID: "v1", ContentHash: "sha256:aaaa", Text: "恢复流程尚未验证，发布前必须补测。"},
		{ChunkID: "c2", VersionID: "v1", ContentHash: "sha256:bbbb", Text: "备份目录为 /var/backups，保留 7 天。"},
	}
	answer := "备份目录为 /var/backups，保留 7 天（来源 c2）。\n" +
		"恢复流程已验证，可以发布（来源 c1）。\n" +
		"备份策略足以满足审计要求（来源 c2）。\n" +
		"（来源 ghost）。\n"

	report := ResolveCitations(answer, sources)
	require.Len(t, report.Supported, 1)
	require.Len(t, report.Unsupported, 1)
	require.Len(t, report.Unverified, 1)
	require.Equal(t, []string{"ghost"}, report.Unresolved)
}

// 没有引用标记的答案不产生任何引用结论（不得凭空推测）。
func TestResolveCitations_NoMarkersYieldsEmptyReport(t *testing.T) {
	src := CitationSource{ChunkID: "c1", VersionID: "v1", ContentHash: "sha256:aaaa", Text: "恢复流程尚未验证。"}
	report := ResolveCitations("恢复流程尚未验证。", []CitationSource{src})
	require.Empty(t, report.Supported)
	require.Empty(t, report.Unsupported)
	require.Empty(t, report.Unverified)
	require.Empty(t, report.Unresolved)
}
