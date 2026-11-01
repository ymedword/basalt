package knowledge

// 本文件实现 `Chunker` 的 v1 策略（US-16 §3.1.3 / T035）：按 Markdown 标题做
// **结构切分**，产出带标题路径与原文区间的片段。
//
// 设计卡为切分冻结的只有三件事：配置键与默认值（`knowledge.chunk.max_tokens` /
// `overlap_tokens` / `version` = 400 / 60 / `v1`）、`kb_chunks` 的列集合与
// `ChunkDraft` 的字段。具体切分规则属于实现层，本文件把它固化成 **v1**（裁决记录
// 见 tasks.md 附录 R44 与 run-state 同编号行）：
//  1. **结构优先**：Markdown ATX 标题（1~6 个 `#`，围栏代码块内的不算）维护层级
//     栈，`heading_path` 是祖先标题用 ` > ` 连接；标题行本身不进正文，首个标题
//     之前的正文属于空标题路径的导言区；
//  2. **窗口与回退**：正文按 段落 → 句子 → 行 → 词（空白）→ 硬切 的顺序在窗口内
//     取最靠后的切点，片子不超过 `max_tokens`；Token 估算与摄取侧一致（1 字符
//     1 Token），因此窗口上限就是 `max_tokens` 个字符；
//  3. **重叠只在同一节内**：下一片从上一片结尾回退 `overlap_tokens` 个字符再前进；
//     回退不跨标题节，也不吞掉整片（回退后没有进展时退化为不重叠）；
//  4. **位置是字符（rune）偏移**：`char_start` / `char_end` 左闭右开，指向
//     `DocumentSource.Text` 的字符下标；片段正文与原文**逐字相同**（来源视图按它
//     取原文，偏一位就对不上），`content_hash` 是片段正文的 sha256。
//
// This file implements the v1 Chunker: structure-aware Markdown splitting whose
// chunks are exact source substrings located by rune offsets.

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// 切点优先级：段落 > 句子 > 行 > 词；同级取窗口内最靠后的切点。
// Boundary classes in priority order; the last one inside the window wins.
const (
	classParagraph = iota
	classSentence
	classLine
	classSpace
)

// TextChunker 按 Markdown 结构把文档切成带位置与标题路径的片段（`v1` 策略）。
// 构造后只读，可被并发调用。
// TextChunker is the v1 Markdown-aware chunker; it is read-only after construction
// and safe for concurrent use.
type TextChunker struct {
	maxTokens int
	overlap   int
}

// NewTextChunker 装配切分器：`maxTokens` 是窗口上限（字符数），`overlapTokens`
// 是同一节内相邻片段的重叠长度。窗口与重叠的合法性在 `Split` 里判定而不是在
// 构造期——构造签名没有 error，静默取默认值会让配置错误变成内容错误。
// NewTextChunker builds the chunker; Split re-checks the window invariant because
// the constructor cannot fail and silent defaults are not allowed.
func NewTextChunker(maxTokens, overlapTokens int) *TextChunker {
	return &TextChunker{maxTokens: maxTokens, overlap: overlapTokens}
}

var _ Chunker = (*TextChunker)(nil)

// Split 把文档切成有序片段。空文档、只有标题没有正文的文档返回错误——摄取侧把
// 切分器报错记成 `parser_failed`（阶段 `parse`），两者都属于"解析不出可索引的
// 正文"。取消信号按标题节轮询，长文档不会把已取消的请求跑完。
// Split returns ordered drafts; a document without body text is an error, and
// cancellation is checked per heading section.
func (c *TextChunker) Split(ctx context.Context, src DocumentSource) ([]ChunkDraft, error) {
	if c == nil || c.maxTokens < 1 {
		return nil, fmt.Errorf("knowledge: knowledge.chunk.max_tokens 必须为正")
	}
	if c.overlap < 0 || c.overlap >= c.maxTokens {
		return nil, fmt.Errorf("knowledge: knowledge.chunk.overlap_tokens（%d）必须非负且小于 max_tokens（%d）",
			c.overlap, c.maxTokens)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	runes := []rune(src.Text)
	drafts := make([]ChunkDraft, 0, len(runes)/c.maxTokens+1)
	for _, sec := range scanSections(runes) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, bounds := range c.splitSection(runes, sec) {
			text := string(runes[bounds.start:bounds.end])
			drafts = append(drafts, ChunkDraft{
				Seq:         len(drafts) + 1,
				HeadingPath: sec.headingPath,
				CharStart:   bounds.start,
				CharEnd:     bounds.end,
				Text:        text,
				ContentHash: hashText(text),
			})
		}
	}
	if len(drafts) == 0 {
		return nil, fmt.Errorf("knowledge: 文档没有可切分的正文")
	}
	return drafts, nil
}

// section 是一个标题节：从标题行之后到下一标题行之前的正文区间（字符下标）。
// section is one heading-scoped body range in rune offsets.
type section struct {
	headingPath string
	start       int
	end         int
}

// bounds 是片段在原文中的字符区间（左闭右开，已去首尾空白）。
// bounds is one chunk's rune range, whitespace-trimmed on both ends.
type bounds struct {
	start int
	end   int
}

// heading 是标题栈的一层。
// heading is one level of the heading stack.
type heading struct {
	level int
	text  string
}

// scanSections 逐行扫描 ATX 标题并产出标题节。围栏代码块（``` / `~~~`）里的 `#`
// 不是标题；标题行不属于任何节，因此只有标题没有正文的文档产出空切片。
// scanSections splits the document at ATX headings, ignoring fenced code blocks.
func scanSections(runes []rune) []section {
	sections := make([]section, 0, 8)
	stack := make([]heading, 0, 6)
	inFence := false
	bodyStart := 0
	for lineStart := 0; lineStart < len(runes); {
		lineEnd := lineStart
		for lineEnd < len(runes) && runes[lineEnd] != '\n' {
			lineEnd++
		}
		next := lineEnd
		if next < len(runes) {
			next++
		}
		line := strings.TrimSpace(string(runes[lineStart:lineEnd]))
		switch {
		case strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~"):
			inFence = !inFence
		case !inFence:
			if level, text, ok := parseHeading(line); ok {
				if bodyStart < lineStart {
					sections = append(sections, section{
						headingPath: headingPath(stack),
						start:       bodyStart,
						end:         lineStart,
					})
				}
				for len(stack) > 0 && stack[len(stack)-1].level >= level {
					stack = stack[:len(stack)-1]
				}
				stack = append(stack, heading{level: level, text: text})
				bodyStart = next
			}
		}
		lineStart = next
	}
	if bodyStart < len(runes) {
		sections = append(sections, section{
			headingPath: headingPath(stack),
			start:       bodyStart,
			end:         len(runes),
		})
	}
	return sections
}

// parseHeading 解析一行 ATX 标题：1~6 个 `#` 后必须接空白或行尾；收尾的井号序列
// 只在前面隔着空白（或标题为空）时才算装饰，`C#` 里的 `#` 是标题正文。
// parseHeading accepts 1-6 leading hashes followed by whitespace; a trailing hash
// run is decorative only when separated by whitespace.
func parseHeading(line string) (int, string, bool) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	rest := line[level:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false
	}
	text := strings.TrimSpace(rest)
	if stripped := strings.TrimRight(text, "#"); stripped != text {
		if stripped == "" || strings.HasSuffix(stripped, " ") || strings.HasSuffix(stripped, "\t") {
			text = strings.TrimSpace(stripped)
		}
	}
	return level, text, true
}

// headingPath 把标题栈拼成 "A > B > C"；空标题不进路径，避免出现悬空分隔符。
// headingPath joins the stack with " > ", skipping empty titles.
func headingPath(stack []heading) string {
	parts := make([]string, 0, len(stack))
	for _, h := range stack {
		if h.text != "" {
			parts = append(parts, h.text)
		}
	}
	return strings.Join(parts, " > ")
}

// splitSection 把一个标题节的正文切成片段：先跳过前导空白，再在窗口内按边界
// 优先级取切点；同一节内下一片从上一片结尾回退 `overlap` 个字符（回退后没有
// 进展时退化为不重叠，保证循环严格前进）。
// splitSection walks the body with a sliding window; overlap only rewinds inside
// the same section and never stalls the loop.
func (c *TextChunker) splitSection(runes []rune, sec section) []bounds {
	spans := make([]bounds, 0, 1)
	start := sec.start
	for start < sec.end {
		for start < sec.end && unicode.IsSpace(runes[start]) {
			start++
		}
		if start >= sec.end {
			break
		}
		windowEnd := start + c.maxTokens
		if windowEnd > sec.end {
			windowEnd = sec.end
		}
		cut := c.chooseCut(runes, start, windowEnd, sec.end)
		end := cut
		for end > start && unicode.IsSpace(runes[end-1]) {
			end--
		}
		spans = append(spans, bounds{start: start, end: end})
		if cut >= sec.end {
			break
		}
		nextStart := cut - c.overlap
		if nextStart <= start {
			nextStart = cut
		}
		start = nextStart
	}
	return spans
}

// chooseCut 在 (start, windowEnd] 内按 段落 > 句子 > 行 > 词 的优先级找最靠后的
// 切点；一类边界都没有时按窗口硬切（超长无空白的串走这条兜底）。
// chooseCut prefers the last paragraph, then sentence, line and word boundary in
// the window, and falls back to a hard cut at the window edge.
func (c *TextChunker) chooseCut(runes []rune, start, windowEnd, sectionEnd int) int {
	if windowEnd >= sectionEnd {
		return sectionEnd
	}
	for class := classParagraph; class <= classSpace; class++ {
		cut := -1
		for i := start + 1; i <= windowEnd; i++ {
			if boundaryClass(runes, i) == class {
				cut = i
			}
		}
		if cut > start {
			return cut
		}
	}
	return windowEnd
}

// boundaryClass 判定下标 i（切在 runes[i] 之前）属于哪一类边界；不是边界返回 -1。
// 段落边界是空行起点，句边界接在句末标点之后，行边界贴着换行，词边界贴着空白。
// boundaryClass classifies the cut position before runes[i], or returns -1.
func boundaryClass(runes []rune, i int) int {
	if i <= 0 || i >= len(runes) {
		return -1
	}
	if runes[i] == '\n' && (i+1 == len(runes) || runes[i+1] == '\n') {
		return classParagraph
	}
	if isSentenceEnd(runes, i) {
		return classSentence
	}
	if runes[i] == '\n' || runes[i-1] == '\n' {
		return classLine
	}
	if unicode.IsSpace(runes[i]) || unicode.IsSpace(runes[i-1]) {
		return classSpace
	}
	return -1
}

// isSentenceEnd 判定在 i 处切是否落在句末：前一个字符是中西文句末标点；西文句点
// 只有后面是空白（或文本结尾）时才算，避免在 `3.14`、`example.com` 里断句。
// isSentenceEnd keeps a Latin period from splitting numbers and host names.
func isSentenceEnd(runes []rune, i int) bool {
	if i <= 0 || i > len(runes) {
		return false
	}
	switch runes[i-1] {
	case '。', '！', '？', '；', '!', '?', ';':
		return true
	case '.':
		return i == len(runes) || unicode.IsSpace(runes[i])
	}
	return false
}
