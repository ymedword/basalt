package knowledge

// 本文件覆盖 T035 的 v1 切分策略：标题路径、字符区间、重叠、边界优先级与
// 切块契约。全部是纯函数用例，不触网、不连库。
//
// This file covers the v1 chunker: heading paths, rune offsets, intra-section
// overlap, boundary preference, and the chunk contract shared with ingest.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chunkSource 把正文包成 `DocumentSource`；切分器只读 `Text`，其余字段照契约填。
// chunkSource wraps body text into the DocumentSource the chunker consumes.
func chunkSource(text string) DocumentSource {
	return DocumentSource{
		Path:        "kb/制度.md",
		Text:        text,
		ContentHash: hashText(text),
		SizeBytes:   int64(len(text)),
	}
}

// splitText 用给定窗口切分，失败即终止用例。
// splitText runs the chunker and fails the test on error.
func splitText(t *testing.T, maxTokens, overlap int, text string) []ChunkDraft {
	t.Helper()
	drafts, err := NewTextChunker(maxTokens, overlap).Split(context.Background(), chunkSource(text))
	require.NoError(t, err)
	require.NotEmpty(t, drafts)
	return drafts
}

// TestSplit_HeadingPathFollowsNesting 断言标题栈按层级拼接、同级标题重置路径。
// 文档原文：`# A` / `## B` / `### C` / `## D` 四节。
func TestSplit_HeadingPathFollowsNesting(t *testing.T) {
	text := "# A\n甲。\n## B\n乙。\n### C\n丙。\n## D\n丁。\n"
	drafts := splitText(t, 400, 60, text)

	paths := make([]string, len(drafts))
	for idx, draft := range drafts {
		paths[idx] = draft.HeadingPath
	}
	assert.Equal(t, []string{"A", "A > B", "A > B > C", "A > D"}, paths)
}

// TestSplit_PreambleHasEmptyHeadingPath 断言首个标题之前的正文属于空标题节的导言区。
// Preamble before the first heading carries an empty heading path.
func TestSplit_PreambleHasEmptyHeadingPath(t *testing.T) {
	drafts := splitText(t, 400, 60, "导言正文。\n# A\n正文。\n")

	require.Len(t, drafts, 2)
	assert.Empty(t, drafts[0].HeadingPath)
	assert.Equal(t, "导言正文。", drafts[0].Text)
	assert.Equal(t, "A", drafts[1].HeadingPath)
}

// TestSplit_TextIsExactSourceSubstring 断言每段正文与原文逐字相同（来源视图按
// 字符区间取原文，偏一位就对不上），并顺带满足摄取侧的切块契约。
// Every draft must be an exact source substring and pass the ingest contract.
func TestSplit_TextIsExactSourceSubstring(t *testing.T) {
	text := "# 甲方制度\n" + strings.Repeat("第一条 适用范围。", 60) +
		"\n## 细则\n" + strings.Repeat("细则正文", 80) + "\n"
	runes := []rune(text)
	drafts := splitText(t, 120, 30, text)

	for _, draft := range drafts {
		require.LessOrEqual(t, draft.CharEnd, len(runes))
		assert.Equal(t, string(runes[draft.CharStart:draft.CharEnd]), draft.Text)
	}
	assert.NoError(t, validateChunks(drafts))
}

// TestSplit_LongSectionStaysUnderMaxTokens 断言长节被切成多段，且每段不超过窗口。
// A long section is split into chunks no longer than the window.
func TestSplit_LongSectionStaysUnderMaxTokens(t *testing.T) {
	text := strings.Repeat("甲", 1200)
	drafts := splitText(t, 100, 20, text)

	require.Greater(t, len(drafts), 10)
	for _, draft := range drafts {
		length := len([]rune(draft.Text))
		assert.GreaterOrEqual(t, length, 1)
		assert.LessOrEqual(t, length, 100)
	}
	assert.Equal(t, 0, drafts[0].CharStart)
	assert.Equal(t, 1200, drafts[len(drafts)-1].CharEnd)
}

// TestSplit_AdjacentChunksOverlapWithinSection 断言同一节内相邻片段按窗口回退重叠，
// 无空白的定长串上重叠长度正好等于 `overlap_tokens`。
// Inside one section adjacent chunks rewind exactly by the overlap.
func TestSplit_AdjacentChunksOverlapWithinSection(t *testing.T) {
	drafts := splitText(t, 100, 20, strings.Repeat("甲", 300))

	expected := [][2]int{{0, 100}, {80, 180}, {160, 260}, {240, 300}}
	require.Len(t, drafts, len(expected))
	for idx, want := range expected {
		assert.Equal(t, want[0], drafts[idx].CharStart, "第 %d 段起点", idx+1)
		assert.Equal(t, want[1], drafts[idx].CharEnd, "第 %d 段终点", idx+1)
	}
	for idx := 1; idx < len(drafts); idx++ {
		assert.Equal(t, 20, drafts[idx-1].CharEnd-drafts[idx].CharStart)
	}
}

// TestSplit_OverlapNeverCrossesHeadingBoundary 断言重叠只在本节内回退：下一节的
// 首段从本节正文起点开始，不会回退进上一节。
// Overlap never rewinds into the previous heading section.
func TestSplit_OverlapNeverCrossesHeadingBoundary(t *testing.T) {
	text := "# A\n" + strings.Repeat("甲", 200) + "\n\n# B\n" + strings.Repeat("乙", 50) + "\n"
	drafts := splitText(t, 100, 20, text)

	bodyStart := -1
	for idx, r := range []rune(text) {
		if r == '乙' {
			bodyStart = idx
			break
		}
	}
	require.GreaterOrEqual(t, bodyStart, 0)

	for _, draft := range drafts {
		if draft.HeadingPath != "B" {
			continue
		}
		assert.Equal(t, bodyStart, draft.CharStart)
		assert.Equal(t, strings.Repeat("乙", 50), draft.Text)
		return
	}
	t.Fatalf("没有切出标题 B 的片段")
}

// TestSplit_ChunksAreTrimmedAndNonEmpty 断言片段首尾无空白，空行与缩进不会成为正文。
// Chunks are trimmed on both ends and never empty.
func TestSplit_ChunksAreTrimmedAndNonEmpty(t *testing.T) {
	text := "# A\n   缩进正文   \n\n\n第二段\n"
	drafts := splitText(t, 400, 60, text)

	for _, draft := range drafts {
		require.NotEmpty(t, draft.Text)
		first := []rune(draft.Text)[0]
		last := []rune(draft.Text)[len([]rune(draft.Text))-1]
		assert.False(t, unicode.IsSpace(first), "第 %d 段以空白开头", draft.Seq)
		assert.False(t, unicode.IsSpace(last), "第 %d 段以空白结尾", draft.Seq)
	}
}

// TestSplit_SeqStartsAtOneAndIsContiguous 断言序号从 1 起全文连续（跨标题节不重排）。
// Seq starts at one and stays contiguous across sections.
func TestSplit_SeqStartsAtOneAndIsContiguous(t *testing.T) {
	text := "# A\n" + strings.Repeat("甲", 150) + "\n# B\n" + strings.Repeat("乙", 150) + "\n"
	drafts := splitText(t, 100, 0, text)

	require.Greater(t, len(drafts), 2)
	for idx, draft := range drafts {
		assert.Equal(t, idx+1, draft.Seq)
	}
}

// TestSplit_ContentHashMatchesText 断言片段哈希是正文的 sha256 十六进制。
// The chunk hash is the sha256 hex of its own body.
func TestSplit_ContentHashMatchesText(t *testing.T) {
	drafts := splitText(t, 400, 60, "# A\n正文内容。\n")

	require.NotEmpty(t, drafts)
	for _, draft := range drafts {
		assert.Equal(t, hashText(draft.Text), draft.ContentHash)
		assert.Len(t, draft.ContentHash, 64)
	}
}

// TestSplit_OffsetsAreRuneOffsets 断言位置是字符下标而不是字节下标：中文标题后
// 的正文按字符切片才对得上原文。
// Offsets are rune indices, not byte indices.
func TestSplit_OffsetsAreRuneOffsets(t *testing.T) {
	text := "# 标题\n中文正文，含逗号。\n"
	drafts := splitText(t, 400, 60, text)

	require.Len(t, drafts, 1)
	draft := drafts[0]
	assert.Equal(t, 5, draft.CharStart)
	assert.Equal(t, "中文正文，含逗号。", draft.Text)
	assert.NotEqual(t, len([]byte(draft.Text)), draft.CharEnd-draft.CharStart)
	assert.Equal(t, draft.Text, string([]rune(text)[draft.CharStart:draft.CharEnd]))
}

// TestSplit_EmptyDocumentRejected 断言空文档不产出片段（摄取侧会记 `parser_failed`）。
// An empty document yields an error instead of a silent empty index.
func TestSplit_EmptyDocumentRejected(t *testing.T) {
	_, err := NewTextChunker(400, 60).Split(context.Background(), chunkSource(""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "没有可切分")
}

// TestSplit_WhitespaceOnlyRejected 断言纯空白文档与空文档一样被拒绝。
// Whitespace-only input is rejected like an empty one.
func TestSplit_WhitespaceOnlyRejected(t *testing.T) {
	_, err := NewTextChunker(400, 60).Split(context.Background(), chunkSource("  \n\n\t\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "没有可切分")
}

// TestSplit_HeadingOnlyDocumentRejected 断言只有标题没有正文的文档不产出片段。
// A document with headings but no body produces nothing and is rejected.
func TestSplit_HeadingOnlyDocumentRejected(t *testing.T) {
	_, err := NewTextChunker(400, 60).Split(context.Background(), chunkSource("# A\n## B\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "没有可切分")
}

// TestSplit_HeadingInsideFenceIsBody 断言围栏代码块里的 `#` 是正文而不是标题。
// A hash inside a fenced code block stays body text.
func TestSplit_HeadingInsideFenceIsBody(t *testing.T) {
	drafts := splitText(t, 400, 60, "```\n# 不是标题\n```\n")

	require.Len(t, drafts, 1)
	assert.Empty(t, drafts[0].HeadingPath)
	assert.Contains(t, drafts[0].Text, "# 不是标题")
}

// TestSplit_HashesDeeperThanSixAreBody 断言七个井号不是标题（ATX 只到六级）。
// Seven hashes are body text, not a heading.
func TestSplit_HashesDeeperThanSixAreBody(t *testing.T) {
	drafts := splitText(t, 400, 60, "####### 七个井号\n正文\n")

	require.Len(t, drafts, 1)
	assert.Empty(t, drafts[0].HeadingPath)
	assert.Contains(t, drafts[0].Text, "#######")
}

// TestSplit_ParagraphBoundaryPreferred 断言段落边界优先于窗口硬切：窗口还能装下
// 下一段的一部分时，片段收在段落末尾而不是把两段劈开。
// A paragraph boundary wins over filling the window.
func TestSplit_ParagraphBoundaryPreferred(t *testing.T) {
	text := strings.Repeat("甲", 100) + "。\n\n" + strings.Repeat("乙", 250)
	drafts := splitText(t, 300, 0, text)

	require.Len(t, drafts, 2)
	assert.Equal(t, 101, drafts[0].CharEnd)
	assert.Equal(t, strings.Repeat("甲", 100)+"。", drafts[0].Text)
	assert.Equal(t, 103, drafts[1].CharStart)
	assert.Equal(t, strings.Repeat("乙", 250), drafts[1].Text)
}

// TestSplit_SentenceBoundaryWithinLongParagraph 断言长段落按句末标点回退，而不是
// 在窗口边缘把句子劈开：窗口 100、句点位于 45/90/135/180 时，首片收在 90。
// A long paragraph is cut at the last sentence end inside the window.
func TestSplit_SentenceBoundaryWithinLongParagraph(t *testing.T) {
	sentence := strings.Repeat("句", 44) + "。"
	text := strings.Repeat(sentence, 6)
	drafts := splitText(t, 100, 0, text)

	require.Greater(t, len(drafts), 1)
	assert.Equal(t, 90, drafts[0].CharEnd)
	assert.True(t, strings.HasSuffix(drafts[0].Text, "。"))
}

// TestSplit_ZeroOverlapProducesDisjointChunks 断言重叠为零时相邻片段首尾相接、无覆盖。
// Zero overlap yields disjoint, adjacent chunks.
func TestSplit_ZeroOverlapProducesDisjointChunks(t *testing.T) {
	drafts := splitText(t, 100, 0, strings.Repeat("甲", 300))

	require.Len(t, drafts, 3)
	for idx := 1; idx < len(drafts); idx++ {
		assert.Equal(t, drafts[idx-1].CharEnd, drafts[idx].CharStart)
	}
}

// TestSplit_LongUnbrokenRunIsHardCut 断言没有任何边界的长串按窗口硬切，不丢字符。
// A run without any boundary is hard-cut at the window edge without loss.
func TestSplit_LongUnbrokenRunIsHardCut(t *testing.T) {
	drafts := splitText(t, 400, 0, strings.Repeat("a", 850))

	require.Len(t, drafts, 3)
	assert.Equal(t, 400, len([]rune(drafts[0].Text)))
	assert.Equal(t, 400, len([]rune(drafts[1].Text)))
	assert.Equal(t, 50, len([]rune(drafts[2].Text)))
	assert.Equal(t, 850, drafts[2].CharEnd)
}

// TestSplit_HeadingTrailingHashesTrimmed 断言标题收尾的装饰井号不进标题路径。
// Decorative trailing hashes are stripped from the heading path.
func TestSplit_HeadingTrailingHashesTrimmed(t *testing.T) {
	drafts := splitText(t, 400, 60, "## 标题 ##\n正文\n")

	require.Len(t, drafts, 1)
	assert.Equal(t, "标题", drafts[0].HeadingPath)
}

// TestSplit_CRLFDocumentsKeepOffsets 断言 CRLF 文档的标题识别与区间都不带 `\r`。
// CRLF documents keep heading detection and offsets free of stray carriage returns.
func TestSplit_CRLFDocumentsKeepOffsets(t *testing.T) {
	text := "# 标题\r\n正文一。\r\n"
	runes := []rune(text)
	drafts := splitText(t, 400, 60, text)

	require.Len(t, drafts, 1)
	assert.Equal(t, "标题", drafts[0].HeadingPath)
	assert.Equal(t, "正文一。", drafts[0].Text)
	assert.Equal(t, drafts[0].Text, string(runes[drafts[0].CharStart:drafts[0].CharEnd]))
}

// TestSplit_InvalidConfigRejected 断言非法窗口与重叠在切分前被拒绝，不静默取默认值。
// Illegal window and overlap are rejected before any splitting happens.
func TestSplit_InvalidConfigRejected(t *testing.T) {
	cases := map[string]*TextChunker{
		"窗口为零":   NewTextChunker(0, 0),
		"重叠为负":   NewTextChunker(10, -1),
		"重叠吃掉窗口": NewTextChunker(10, 10),
	}
	for name, chunker := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := chunker.Split(context.Background(), chunkSource("正文\n"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "knowledge.chunk")
		})
	}
}

// TestSplit_CanceledContextReturnsError 断言已取消的请求不会把文档切完。
// A canceled context stops the split instead of finishing the document.
func TestSplit_CanceledContextReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewTextChunker(100, 20).Split(ctx, chunkSource(strings.Repeat("甲", 500)))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestSplit_ConcurrentCallsAreIndependent 断言切分器构造后只读：并发调用互不串味。
// The chunker is read-only after construction and safe for concurrent use.
func TestSplit_ConcurrentCallsAreIndependent(t *testing.T) {
	chunker := NewTextChunker(100, 20)
	texts := make([]string, 8)
	for idx := range texts {
		texts[idx] = strings.Repeat(string(rune('甲'+idx)), 250)
	}

	var wg sync.WaitGroup
	errs := make([]error, len(texts))
	for idx := range texts {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = chunker.Split(context.Background(), chunkSource(texts[idx]))
		}(idx)
	}
	wg.Wait()

	for idx, err := range errs {
		assert.NoError(t, err, "第 %d 份文档", idx+1)
	}
}
