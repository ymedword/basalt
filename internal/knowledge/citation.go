package knowledge

// 本文件实现引用解析（US-16 §2.10.4 / §3.1.3 / C-33 / T039）：把答案里出现的引用标记
// 与该次上下文中**精确读取**过的来源逐条比对，分成四类互斥结论——`Supported` /
// `Unsupported` / `Unverified` / `Unresolved`。三条硬约束：
//
//  1. **"可定位"与"支持结论"分开**（C-33）：`located` 只说明引用到了正确原文（内容哈希
//     匹配、版本一致、span 未截断），**不等于**原文支持答案里的这句话。`located` 成立后
//     还要过确定性蕴含检查；判不了的一律 `Unverified`，**不得**默认记 `Supported`；
//  2. **入参不接受检索命中**：`CitationSource` 只由 `SourceView` 精确读取填充（带内容
//     哈希）；`Hit` 没有内容哈希，签名层面就进不来。搜索片段 / 向量相似 / 摘要文本
//     **不得**充当引用依据；
//  3. **确定性优先、宁严勿松**：极性（否定 / 肯定）、状态词、数字与单位、日期用确定性
//     检查；发现矛盾即 `Unsupported`。无法用确定性规则判定蕴含的开放结论归
//     `Unverified`——这是设计明确要求的方向，不是"保守过头"。
//
// 引用标记的语法（本实现的词法约定，记入 tasks.md 附录 R49）：答案中的片段标识以
// **独立标识符**出现即视为引用（两侧不是字母 / 数字 / 下划线），例如"（来源 c1）"与
// "见 c3。"；显式包裹形式（`（来源 X）` / `(来源 X)` / `【X】` / `[X]`）额外用于识别
// **未提供来源**的标识，进 `Unresolved`。已知标识按独立出现匹配，避免把普通英文词
// 误判成未解析引用。
//
// This file resolves citations against exactly-read sources. Locating a span never
// implies the source supports the claim: located spans still have to pass deterministic
// entailment checks, and anything undecidable stays Unverified. Retrieval hits cannot be
// passed in — the type has no content hash.

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

// EvidenceRef 是资料引用的类型别名（冻结契约要求 `knowledge.EvidenceRef` 就是
// `core.EvidenceRef`）：只记定位与复核所需的标识，**不含正文**。
// EvidenceRef aliases the core reference, as the frozen contract requires.
type EvidenceRef = core.EvidenceRef

// CitationSource 是一次**精确读取**的结果，且已实际进入本次上下文（§3.1.3）。
// 检索命中**不得**转换成这个类型——它没有内容哈希，无法参与"可定位"判定。
// CitationSource is one exact read that actually entered the context. A retrieval hit
// cannot become one: it carries no content hash.
type CitationSource struct {
	// ChunkID 是片段标识，也是答案里的引用标记。
	ChunkID string
	// VersionID 是实际读取的版本。
	VersionID string
	// EvidenceRef 是读取时使用的引用；有值时用于交叉核对版本与内容哈希。
	EvidenceRef EvidenceRef
	// ContentHash 是实际读取内容的哈希；空值意味着无法判定"可定位"。
	ContentHash string
	// Text 是读取到的原文片段；截断时只有前缀。
	Text string
	// Truncated 表示返回被预算截断——被截断的 span 连 `located` 都不成立。
	Truncated bool
}

// Claim 是答案里的一条主张（一句话）及其引用标记。
// Claim is one sentence of the answer together with its citation markers.
type Claim struct {
	// Text 是去掉引用标记后的原句。
	Text string
	// ChunkIDs 是该句引用的片段（按首次出现位置排序）。
	ChunkIDs []string
	// Reason 是判定原因的机器可读码（仅 Unsupported / Unverified 非空）。
	Reason string
}

// Citation 是一条被判为"可定位且被支持"的引用：主张与来源成对出现。
// Citation pairs a supported claim with the source that supports it.
type Citation struct {
	// Claim 是被支持的主张。
	Claim Claim
	// ChunkID 是支持它的片段。
	ChunkID string
	// VersionID 是该片段的版本。
	VersionID string
	// ContentHash 是读取内容的哈希（可定位证据）。
	ContentHash string
	// EvidenceRef 是读取时使用的引用。
	EvidenceRef EvidenceRef
}

// CitationReport 是四类互斥的引用结论（§3.1.3 / §2.10.4）。
// 同一条主张只会落进一个类别；`Unresolved` 只装"引用了没读过的 ID"。
// CitationReport holds the four mutually exclusive classes; a claim lands in exactly one.
type CitationReport struct {
	// Supported 是同时满足"可定位"与"被支持"的引用。
	Supported []Citation
	// Unsupported 是证据与结论矛盾、或引用本身无法定位的主张。
	Unsupported []Claim
	// Unverified 是可定位但无法确定性判定支持关系的开放结论。
	Unverified []Claim
	// Unresolved 是答案引用了本次上下文**没有读过**的片段标识（按出现顺序去重）。
	Unresolved []string
}

// 判定原因码（实现层派生，不属于冻结契约字面量）。`citation_unresolved` 与诊断契约
// 的 `ReasonCitationUnresolved` 同值，便于两处对账。
// Reasons are implementation-derived; unresolved reuses the diagnostic literal.
const (
	citationReasonNotLocated        = "not_located"
	citationReasonSpanTruncated     = "span_truncated"
	citationReasonVersionMismatch   = "version_mismatch"
	citationReasonContentMismatch   = "content_mismatch"
	citationReasonStatusConflict    = "status_conflict"
	citationReasonNegationConflict  = "negation_conflict"
	citationReasonNumberConflict    = "number_conflict"
	citationReasonTimeConflict      = "time_conflict"
	citationReasonNotEntailed       = "not_entailed"
	citationReasonUnresolvedLiteral = "citation_unresolved"
)

// citationStatusPairs 是状态词的正 / 负对照（设计 §2.10.4 点名"状态词"类检查）。
// 只有同一对里正负互斥才算矛盾；"支持 / 不支持"放在最后，避免与普通用词混淆。
// Status pairs are the deterministic positive/negative vocabulary.
var citationStatusPairs = [][2]string{
	{"已验证", "未验证"},
	{"已通过", "未通过"},
	{"已完成", "未完成"},
	{"已发布", "未发布"},
	{"已修复", "未修复"},
	{"已启用", "未启用"},
	{"已删除", "未删除"},
	{"已生效", "未生效"},
	{"已归档", "未归档"},
	{"已确认", "未确认"},
	{"支持", "不支持"},
}

// citationNegations 是极性检查的否定词，按**长者优先**排列——"尚未"必须先于"未"
// 匹配，否则短语切分会偏一位。
// Longer negation markers come first so phrase cuts stay aligned.
var citationNegations = []string{"尚未", "不得", "禁止", "严禁", "不能", "未能", "未", "不", "没", "无"}

// citationNumberUnitPattern 抓"数字 + 计量单位"（设计点名的"数字与单位"检查）。
// 只收**量纲明确**的单位：把"个 / 条 / 份 / 步"排除在外——它们在中文里常与数字分离
// 出现（"第 3 步"），按相邻匹配会造出假矛盾。
// Only unambiguous measurement units are matched; 个/条/份/步 are excluded because
// they routinely appear detached from the number in Chinese prose.
var citationNumberUnitPattern = regexp.MustCompile(`\d+(?:\.\d+)?\s*(?:小时|分钟|天|秒|次|元|倍|台|人|MB|GB|TB|ms|%)`)

// citationDatePattern 抓常见日期写法（"时间"类确定性检查）。
// The date pattern backs the deterministic time check.
var citationDatePattern = regexp.MustCompile(`\d{4}\s*[-/年]\s*\d{1,2}\s*[-/月]\s*\d{1,2}\s*日?`)

// citationWrappers 是"显式引用包裹"的成对分隔符，用于识别未提供来源的标识。
// Explicit wrappers let the resolver notice ids that were never read.
var citationWrappers = [][2]string{
	{"（来源", "）"},
	{"(来源", ")"},
	{"【来源", "】"},
	{"[来源", "]"},
	{"【", "】"},
	{"[", "]"},
}

// ResolveCitations 解析答案中的引用，返回四类互斥结论（§3.1.3 冻结签名）。
// 入参只接受精确读取且实际进入本次上下文的片段；`sources` 为空时答案里的显式引用
// 全部进 `Unresolved`，不做任何猜测。
// ResolveCitations resolves the answer's citation markers against exactly-read sources.
func ResolveCitations(answer string, sources []CitationSource) CitationReport {
	byID := make(map[string]CitationSource, len(sources))
	for _, src := range sources {
		if src.ChunkID == "" {
			continue
		}
		// 同一标识重复给出时以首条为准：后来的副本**不得**覆盖先读到的内容哈希。
		if _, exists := byID[src.ChunkID]; exists {
			continue
		}
		byID[src.ChunkID] = src
	}

	report := CitationReport{}
	seenUnresolved := make(map[string]struct{})
	for _, sentence := range splitClaims(answer) {
		known, unknown := citedIDs(sentence, byID)
		for _, id := range unknown {
			if _, seen := seenUnresolved[id]; seen {
				continue
			}
			seenUnresolved[id] = struct{}{}
			report.Unresolved = append(report.Unresolved, id)
		}
		if len(known) == 0 {
			// 没有读过任何被引片段：这句话不构成可判定主张，只留 Unresolved 证据。
			continue
		}

		claim := Claim{
			Text:     stripCitationMarkers(sentence, known),
			ChunkIDs: append([]string(nil), known...),
		}
		located, locatedReason := locateSources(known, byID)
		if locatedReason != "" {
			claim.Reason = locatedReason
			report.Unsupported = append(report.Unsupported, claim)
			continue
		}
		if reason := contradiction(claim.Text, located); reason != "" {
			claim.Reason = reason
			report.Unsupported = append(report.Unsupported, claim)
			continue
		}

		supported := false
		for _, src := range located {
			if !entailed(claim.Text, src.Text) {
				continue
			}
			report.Supported = append(report.Supported, Citation{
				Claim:       claim,
				ChunkID:     src.ChunkID,
				VersionID:   src.VersionID,
				ContentHash: src.ContentHash,
				EvidenceRef: src.EvidenceRef,
			})
			supported = true
		}
		if supported {
			continue
		}
		claim.Reason = citationReasonNotEntailed
		report.Unverified = append(report.Unverified, claim)
	}
	return report
}

// splitClaims 把答案切成句子：以句号 / 问号 / 叹号 / 分号 / 换行为界。
// 逗号**不**切——同一句里的转折仍是一条主张。
// Sentences break on terminal punctuation and newlines; commas do not split.
func splitClaims(answer string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		text := strings.TrimSpace(b.String())
		b.Reset()
		if text != "" {
			out = append(out, text)
		}
	}
	for _, r := range answer {
		switch r {
		case '。', '！', '？', '；', '!', '?', ';', '\n':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

// citedIDs 返回该句引用的**已读**片段（按首次出现位置排序）与显式标出但**没读过**的
// 标识（按出现顺序去重）。已读标识按独立出现匹配；未读标识只从显式包裹里取。
// Known ids match as standalone tokens; unknown ids only come from explicit wrappers.
func citedIDs(sentence string, byID map[string]CitationSource) (known, unknown []string) {
	type positioned struct {
		id  string
		pos int
	}
	var found []positioned
	for id := range byID {
		if pos := firstStandaloneIndex(sentence, id); pos >= 0 {
			found = append(found, positioned{id: id, pos: pos})
		}
	}
	slices.SortFunc(found, func(a, b positioned) int {
		if c := cmp.Compare(a.pos, b.pos); c != 0 {
			return c
		}
		return cmp.Compare(a.id, b.id)
	})
	seen := make(map[string]struct{}, len(found))
	for _, item := range found {
		seen[item.id] = struct{}{}
		known = append(known, item.id)
	}
	for _, id := range explicitCitationIDs(sentence) {
		if _, ok := byID[id]; ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unknown = append(unknown, id)
	}
	return known, unknown
}

// explicitCitationIDs 取出显式包裹里的标识（`（来源 X）` / `【X】` / `[X]`）。
// It reads the ids written inside the explicit wrappers.
func explicitCitationIDs(sentence string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, pair := range citationWrappers {
		open, closeMark := pair[0], pair[1]
		from := 0
		for {
			i := strings.Index(sentence[from:], open)
			if i < 0 {
				break
			}
			start := from + i + len(open)
			j := strings.Index(sentence[start:], closeMark)
			if j < 0 {
				break
			}
			inner := strings.TrimSpace(sentence[start : start+j])
			from = start + j + len(closeMark)
			if !isIdentifierToken(inner) {
				continue
			}
			if _, dup := seen[inner]; dup {
				continue
			}
			seen[inner] = struct{}{}
			out = append(out, inner)
		}
	}
	return out
}

// stripCitationMarkers 去掉句子里的引用标记：先整段去掉显式包裹，再去掉裸标识。
// 留下的才是"主张文本"，供矛盾与蕴含检查使用。
// Stripping the markers leaves the claim text that the checks operate on.
func stripCitationMarkers(sentence string, known []string) string {
	knownSet := make(map[string]struct{}, len(known))
	for _, id := range known {
		knownSet[id] = struct{}{}
	}
	out := sentence
	for _, pair := range citationWrappers {
		open, closeMark := pair[0], pair[1]
		var b strings.Builder
		from := 0
		for {
			i := strings.Index(out[from:], open)
			if i < 0 {
				b.WriteString(out[from:])
				break
			}
			start := from + i
			innerStart := start + len(open)
			j := strings.Index(out[innerStart:], closeMark)
			if j < 0 {
				b.WriteString(out[from:])
				break
			}
			inner := strings.TrimSpace(out[innerStart : innerStart+j])
			_, isKnown := knownSet[inner]
			if !isKnown && !isIdentifierToken(inner) {
				// 不是引用包裹，原样保留（例如正文里的"（见附录）"）。
				b.WriteString(out[from : innerStart+j+len(closeMark)])
				from = innerStart + j + len(closeMark)
				continue
			}
			b.WriteString(out[from:start])
			from = innerStart + j + len(closeMark)
		}
		out = b.String()
	}
	for _, id := range known {
		out = replaceStandalone(out, id, "")
	}
	return strings.Join(strings.Fields(out), " ")
}

// locateSources 判定一组来源是否全部"可定位"：未截断、版本与内容哈希齐备，
// 且与读取时使用的 `EvidenceRef` 一致。任一不成立即返回原因码。
// All cited sources must be locatable: not truncated, versioned, hashed, and consistent
// with the reference they were read under.
func locateSources(ids []string, byID map[string]CitationSource) ([]CitationSource, string) {
	out := make([]CitationSource, 0, len(ids))
	for _, id := range ids {
		src := byID[id]
		if src.Truncated {
			return nil, citationReasonSpanTruncated
		}
		if src.VersionID == "" || src.ContentHash == "" {
			return nil, citationReasonNotLocated
		}
		if src.EvidenceRef.Valid() {
			if src.EvidenceRef.VersionID != src.VersionID {
				return nil, citationReasonVersionMismatch
			}
			if src.EvidenceRef.ContentHash != "" && src.EvidenceRef.ContentHash != src.ContentHash {
				return nil, citationReasonContentMismatch
			}
		}
		out = append(out, src)
	}
	return out, ""
}

// contradiction 用确定性检查找矛盾：状态词、否定极性、数字与单位、日期。
// 返回空串表示没有发现矛盾——**不等于**结论被支持（见 entailed）。
// It reports the first deterministic contradiction; an empty result is not support.
func contradiction(claim string, sources []CitationSource) string {
	for _, src := range sources {
		if reason := statusConflict(claim, src.Text); reason != "" {
			return reason
		}
		if reason := negationConflict(claim, src.Text); reason != "" {
			return reason
		}
		if reason := numberConflict(claim, src.Text); reason != "" {
			return reason
		}
		if reason := timeConflict(claim, src.Text); reason != "" {
			return reason
		}
	}
	return ""
}

// statusConflict 检查状态词的正负对照：同一对里一方取正、另一方取负即矛盾。
// It flags a positive/negative mismatch inside one status pair.
func statusConflict(claim, source string) string {
	for _, pair := range citationStatusPairs {
		positive, negative := pair[0], pair[1]
		claimPos, claimNeg := strings.Contains(claim, positive), strings.Contains(claim, negative)
		srcPos, srcNeg := strings.Contains(source, positive), strings.Contains(source, negative)
		if claimPos && !claimNeg && srcNeg && !srcPos {
			return citationReasonStatusConflict
		}
		if claimNeg && !claimPos && srcPos && !srcNeg {
			return citationReasonStatusConflict
		}
	}
	return ""
}

// negationConflict 检查"共享短语上的否定"：来源否定了某短语、主张却肯定它 → 矛盾。
// 主张里原样保留否定形式（如"尚未验证"）时不算矛盾。
// A phrase negated by the source but affirmed by the claim is a contradiction; keeping
// the negated form in the claim is agreement.
func negationConflict(claim, source string) string {
	for _, marker := range citationNegations {
		from := 0
		for {
			i := strings.Index(source[from:], marker)
			if i < 0 {
				break
			}
			start := from + i
			phrase := leadingHanRunes(source[start+len(marker):], 4)
			from = start + len(marker)
			if len([]rune(phrase)) < 2 {
				continue
			}
			if strings.Contains(claim, marker+phrase) {
				continue
			}
			if strings.Contains(claim, phrase) {
				return citationReasonNegationConflict
			}
		}
	}
	return ""
}

// numberConflict 检查数字与单位：主张里出现的"数字 + 单位"必须在来源中原样出现。
// It requires every number-with-unit in the claim to appear verbatim in the source.
func numberConflict(claim, source string) string {
	compactSource := compactSpaces(source)
	for _, token := range citationNumberUnitPattern.FindAllString(claim, -1) {
		if !strings.Contains(compactSource, compactSpaces(token)) {
			return citationReasonNumberConflict
		}
	}
	return ""
}

// timeConflict 检查日期：主张里的日期必须在来源中原样出现。
// Dates written in the claim must appear verbatim in the source.
func timeConflict(claim, source string) string {
	compactSource := compactSpaces(source)
	for _, token := range citationDatePattern.FindAllString(claim, -1) {
		if !strings.Contains(compactSource, compactSpaces(token)) {
			return citationReasonTimeConflict
		}
	}
	return ""
}

// entailed 做**确定性**蕴含检查：主张的全部双字内容与英文词都必须出现在来源里。
// 这是"引用即复述"的窄口径——改写、概括、推断都不算，归 `Unverified`；模型 Judge
// 属条件实验（D-28），本 US 不交付，**不得**用"没发现矛盾"冒充被支持。
// Deterministic entailment: every CJK bigram and ASCII word of the claim must appear in
// the source. Paraphrases stay Unverified until a judge is delivered and approved.
func entailed(claim, source string) bool {
	grams := cjkBigrams(claim)
	words := asciiWords(claim)
	if len(grams) == 0 && len(words) == 0 {
		return false
	}
	sourceGrams := make(map[string]struct{})
	for _, gram := range cjkBigrams(source) {
		sourceGrams[gram] = struct{}{}
	}
	sourceWords := make(map[string]struct{})
	for _, word := range asciiWords(source) {
		sourceWords[word] = struct{}{}
	}
	for _, gram := range grams {
		if _, ok := sourceGrams[gram]; !ok {
			return false
		}
	}
	for _, word := range words {
		if _, ok := sourceWords[word]; !ok {
			return false
		}
	}
	return true
}

// cjkBigrams 取连续汉字段的双字组；单字段不产生双字组。
// It returns the bigrams of each contiguous Han run.
func cjkBigrams(text string) []string {
	var out []string
	var prev rune
	havePrev := false
	for _, r := range text {
		if !unicode.Is(unicode.Han, r) {
			havePrev = false
			continue
		}
		if havePrev {
			out = append(out, string([]rune{prev, r}))
		}
		prev = r
		havePrev = true
	}
	return out
}

// asciiWords 取 ASCII 字母 / 数字词并转小写（"var" / "backups" 这类外文词也要对得上）。
// ASCII words are lowercased so identifiers and paths compare case-insensitively.
func asciiWords(text string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, strings.ToLower(b.String()))
			b.Reset()
		}
	}
	for _, r := range text {
		if r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// leadingHanRunes 取紧跟否定词的最多 limit 个连续汉字（遇到标点或其它字符即止）。
// It collects the Han phrase that follows a negation marker.
func leadingHanRunes(text string, limit int) string {
	var out []rune
	for _, r := range text {
		if len(out) >= limit || !unicode.Is(unicode.Han, r) {
			break
		}
		out = append(out, r)
	}
	return string(out)
}

// compactSpaces 去掉全部空白，让"保留 7 天"与"保留7天"按同一口径比较。
// Removing whitespace makes the number/date comparisons insensitive to spacing.
func compactSpaces(text string) string {
	return strings.Join(strings.Fields(text), "")
}

// isIdentifierToken 判定括号里的是否是标识符形态（字母 / 数字 / `_` / `-` / `.`）。
// It accepts identifier-shaped tokens only.
func isIdentifierToken(text string) bool {
	if text == "" || len(text) > 128 {
		return false
	}
	hasWord := false
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			hasWord = true
			continue
		}
		if r != '_' && r != '-' && r != '.' && r != ':' && r != '/' {
			return false
		}
	}
	return hasWord
}

// isIdentifierRune 是"独立出现"的边界判据：ASCII 字母 / 数字 / 下划线算标识符内部。
// 汉字与标点都算边界——中文行文里标识符两侧通常没有空格。
// CJK and punctuation count as boundaries; the identifier alphabet is ASCII-only.
func isIdentifierRune(r rune) bool {
	return r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_')
}

// firstStandaloneIndex 找到 id 第一次以独立标识符出现的位置（字节下标），没有则 -1。
// It returns the first byte offset where id appears with identifier boundaries.
func firstStandaloneIndex(text, id string) int {
	if id == "" {
		return -1
	}
	from := 0
	for from+len(id) <= len(text) {
		i := strings.Index(text[from:], id)
		if i < 0 {
			return -1
		}
		start := from + i
		end := start + len(id)
		if hasIdentifierBoundaries(text, start, end) {
			return start
		}
		from = start + 1
	}
	return -1
}

// replaceStandalone 把独立出现的 id 替换成 repl（引用标记的裸形式）。
// It rewrites standalone occurrences of id.
func replaceStandalone(text, id, repl string) string {
	if id == "" {
		return text
	}
	var b strings.Builder
	from := 0
	for from+len(id) <= len(text) {
		i := strings.Index(text[from:], id)
		if i < 0 {
			break
		}
		start := from + i
		end := start + len(id)
		if !hasIdentifierBoundaries(text, start, end) {
			b.WriteString(text[from:end])
			from = end
			continue
		}
		b.WriteString(text[from:start])
		b.WriteString(repl)
		from = end
	}
	if from < len(text) {
		b.WriteString(text[from:])
	}
	return b.String()
}

// hasIdentifierBoundaries 判定 [start, end) 两侧都不是标识符字符。
// It checks that both sides of the span are outside the identifier alphabet.
func hasIdentifierBoundaries(text string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if isIdentifierRune(r) {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if isIdentifierRune(r) {
			return false
		}
	}
	return true
}
