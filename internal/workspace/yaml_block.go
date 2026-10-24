package workspace

// 本文件是定义文件（`basalt.yaml` 的 `providers:`、`.basalt/mcp_servers.yaml` 的
// `servers:`）的**顶层列表条目块**外科手术面（颗粒度文档 §3.1.3 / 约束 6）：
// 只在目标条目的行区间内替换字节，块外一切字节逐字保留；目标条目内出现注释时
// 以 `commentsLost=true` 告知调用方（由调用方决定拒绝写入或记录 comments_lost）。
//
// 前置条件（约束 6，任一不满足 → 包装 ErrEditUnsupported，调用方零写入）：
// 唯一的顶层键、块式列表（非 `[...]`）、无锚点 / 别名 / 合并键、无重复键、
// 无多文档分隔符；目标文件的顶层必须是映射（空文档 / 纯注释 / 空表 `{}` 除外）。
// This file is the line-surgical writer for top-level list blocks: only the target
// entry's line span is rewritten, every byte outside it is preserved verbatim, and
// any structural construct the writer cannot preserve refuses with ErrEditUnsupported.

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// BlockEntry 是顶层列表块中的一个条目：`Start` / `End` 是 0 基行号（End 开区间，
// 不含条目间的空行），`Raw` 是条目原字节（含行终止符），`Name` 是条目映射的
// `name` 字段。
// BlockEntry describes one list item: a zero-based half-open line span, the raw
// bytes of the span, and the value of the item's `name` field.
type BlockEntry struct {
	Name  string
	Start int
	End   int
	Raw   []byte
}

// FindBlock 定位顶层键 `key` 的块范围：`start` 是键行，`end` 是块尾（开区间，
// 已排除与下一段之间的空行）。键缺失（含空文档）返回 `(-1, -1, nil)`——缺失不是
// 错误，调用方据此走"创建块"路径。
// FindBlock locates the top-level block; a missing key returns (-1, -1, nil).
func FindBlock(doc []byte, key string) (start, end int, err error) {
	parsed, err := parseYAMLBlockDoc(doc)
	if err != nil {
		return 0, 0, err
	}
	blk, err := parsed.resolveBlock(key)
	if err != nil {
		return 0, 0, err
	}
	if blk == nil {
		return -1, -1, nil
	}
	return blk.keyLine, blk.blockEnd, nil
}

// BlockEntries 按文件顺序返回顶层键 `key` 下的条目视图（缩进、空行分隔与条目内
// 注释都保留在 Raw 中）；键缺失返回空切片。
// BlockEntries returns the entries in file order; a missing key yields no entries.
func BlockEntries(doc []byte, key string) ([]BlockEntry, error) {
	parsed, err := parseYAMLBlockDoc(doc)
	if err != nil {
		return nil, err
	}
	blk, err := parsed.resolveBlock(key)
	if err != nil {
		return nil, err
	}
	if blk == nil || len(blk.entries) == 0 {
		return nil, nil
	}
	// Raw 已在 resolveBlock 中复制；再克隆切片头，调用方修改不影响二次解析。
	return append([]BlockEntry(nil), blk.entries...), nil
}

// ReplaceEntry 用 `rendered` 替换名为 `name` 的条目的行区间：块外字节逐字保留，
// 相邻条目与块级注释不动。目标条目内含注释时无法保留，返回 `commentsLost=true`
// 与**已生成**的候选字节——是否落盘由调用方按自身错误策略决定（约束 6 的
// `comments_lost` 记录与失败判据表的 400 拒绝都建立在这个信号上）。
// ReplaceEntry swaps the target entry's lines; entry-internal comments cannot be
// preserved, so the caller receives commentsLost and must decide the policy.
func ReplaceEntry(doc []byte, key, name string, rendered []byte) (out []byte, commentsLost bool, err error) {
	parsed, err := parseYAMLBlockDoc(doc)
	if err != nil {
		return nil, false, err
	}
	blk, err := parsed.resolveBlock(key)
	if err != nil {
		return nil, false, err
	}
	if blk == nil {
		return nil, false, fmt.Errorf("顶层键 %s 不存在: %w", key, fs.ErrNotExist)
	}
	entry, err := blk.findEntry(name)
	if err != nil {
		return nil, false, err
	}

	terminator := lineTerminator(parsed.lines[entry.Start])
	if terminator == "" {
		terminator = "\n"
	}
	indent := leadingSpaces(scheduleLineContent(parsed.lines[entry.Start]))
	insert, renderedName, err := normalizeRenderedEntry(rendered, indent, terminator)
	if err != nil {
		return nil, false, err
	}
	if renderedName != name {
		return nil, false, fmt.Errorf("%w: 待写入条目的 name=%q 与目标条目 %q 不一致", ErrEditUnsupported, renderedName, name)
	}
	return spliceYAMLLines(parsed.lines, entry.Start, entry.End, insert), entryHasComment(parsed.lines, entry.Start, entry.End), nil
}

// AppendEntry 在顶层列表块尾部追加一个条目：块已存在时紧跟最后一个条目（条目间
// 空行与后续段落的注释保留）；键缺失时在文件末尾创建 `key:` 块；顶层是命令模板
// 的空表 `{}` 时把该行换成块式列表（`basalt init` 的 mcp_servers.yaml 即此形态）。
// AppendEntry appends one entry after the last item, creating the block at EOF or
// replacing a lone empty flow mapping when the key is absent.
func AppendEntry(doc []byte, key string, rendered []byte) ([]byte, error) {
	parsed, err := parseYAMLBlockDoc(doc)
	if err != nil {
		return nil, err
	}
	blk, err := parsed.resolveBlock(key)
	if err != nil {
		return nil, err
	}

	if blk == nil {
		return parsed.appendNewBlock(key, rendered)
	}

	terminator := "\n"
	if blk.appendAt > 0 {
		if term := lineTerminator(parsed.lines[blk.appendAt-1]); term != "" {
			terminator = term
		}
	}
	indent := -1 // 新块没有既有缩进可参照：沿用待写入条目自身的缩进
	if len(blk.entries) > 0 {
		indent = blk.indent
	}
	insert, renderedName, err := normalizeRenderedEntry(rendered, indent, terminator)
	if err != nil {
		return nil, err
	}
	if err := blk.rejectDuplicateName(renderedName); err != nil {
		return nil, err
	}
	return spliceYAMLLines(parsed.lines, blk.appendAt, blk.appendAt, insert), nil
}

// RemoveEntry 删除名为 `name` 的条目行区间（块外字节逐字保留）；条目内含注释时
// 删除必然丢失注释，返回 `commentsLost=true`。只剩最后一条时仅删除条目行，
// 顶层键保留——空块按 YAML 语义等价于空列表，后续 AppendEntry 可继续追加。
// RemoveEntry deletes the target entry's line span and reports lost comments; the
// top-level key itself survives even when the block becomes empty.
func RemoveEntry(doc []byte, key, name string) (out []byte, commentsLost bool, err error) {
	parsed, err := parseYAMLBlockDoc(doc)
	if err != nil {
		return nil, false, err
	}
	blk, err := parsed.resolveBlock(key)
	if err != nil {
		return nil, false, err
	}
	if blk == nil {
		return nil, false, fmt.Errorf("顶层键 %s 不存在: %w", key, fs.ErrNotExist)
	}
	entry, err := blk.findEntry(name)
	if err != nil {
		return nil, false, err
	}
	return spliceYAMLLines(parsed.lines, entry.Start, entry.End, nil), entryHasComment(parsed.lines, entry.Start, entry.End), nil
}

// yamlBlockDoc 是一次解析后的定义文件视图：lines 保留每行原始字节（SplitAfter，
// 含行终止符，Join 无损往返），root 为 nil 表示空文档 / 纯注释。
// yamlBlockDoc is the parsed document view: split lines round-trip losslessly,
// and a nil root means an empty or comment-only document.
type yamlBlockDoc struct {
	doc   []byte
	lines []string
	root  *ast.MappingNode
}

// parseYAMLBlockDoc 解析并执行块编辑的全局前置检查：单文档、顶层映射。
// 重复键由 YAML 解析器拒绝（SyntaxError），这里统一折叠成位置摘要，不泄露库文案。
// parseYAMLBlockDoc enforces the document-level preconditions; duplicate keys are
// rejected by the parser and folded into a position summary.
func parseYAMLBlockDoc(doc []byte) (*yamlBlockDoc, error) {
	file, err := parser.ParseBytes(doc, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: 定义文件不可安全解析（%s）", ErrEditUnsupported, frontmatterErrorSummary(err))
	}
	if len(file.Docs) != 1 {
		return nil, fmt.Errorf("%w: 定义文件含 %d 个 YAML 文档，多文档分隔符不做块编辑", ErrEditUnsupported, len(file.Docs))
	}
	parsed := &yamlBlockDoc{doc: doc, lines: strings.SplitAfter(string(doc), "\n")}
	body := file.Docs[0].Body
	if body == nil {
		return parsed, nil
	}
	root, ok := body.(*ast.MappingNode)
	if !ok {
		return nil, fmt.Errorf("%w: 定义文件顶层必须是映射", ErrEditUnsupported)
	}
	parsed.root = root
	return parsed, nil
}

// yamlBlock 是目标顶层键的块视图：keyLine 为键行（0 基），entries 按文件序，
// appendAt 是追加条目的插入行号，blockEnd 是块尾（开区间）。
// yamlBlock is the block view of the target top-level key.
type yamlBlock struct {
	key      string
	keyLine  int
	blockEnd int
	indent   int
	entries  []BlockEntry
	appendAt int
}

// resolveBlock 定位并校验目标块；键缺失返回 (nil, nil)。
// resolveBlock locates and validates the target block; a missing key yields nil.
func (d *yamlBlockDoc) resolveBlock(key string) (*yamlBlock, error) {
	mv, err := d.topLevel(key)
	if err != nil {
		return nil, err
	}
	if mv == nil {
		return nil, nil
	}
	keyLine, ok := d.lineOf(mv.Key)
	if !ok || keyLine < 0 || keyLine >= len(d.lines) {
		return nil, fmt.Errorf("%w: 无法定位顶层键 %s 的行号", ErrEditUnsupported, key)
	}

	blk := &yamlBlock{key: key, keyLine: keyLine, blockEnd: keyLine + 1, indent: -1, appendAt: keyLine + 1}
	switch value := mv.Value.(type) {
	case *ast.NullNode:
		// `key:` 没有值：空列表，追加从键行之后开始。
		return blk, nil
	case *ast.SequenceNode:
		if value.IsFlowStyle {
			return nil, fmt.Errorf("%w: %s 是流式集合（[...]），只接受块式列表", ErrEditUnsupported, key)
		}
		if err := rejectUnsafeYAMLNodes(value); err != nil {
			return nil, err
		}
		if len(value.Values) == 0 {
			return blk, nil
		}
		entries, indent, blockEnd, err := d.scanEntries(keyLine)
		if err != nil {
			return nil, err
		}
		if len(entries) != len(value.Values) {
			return nil, fmt.Errorf("%w: %s 的列表条目数与 YAML 结构不一致（行扫描 %d，解析 %d）", ErrEditUnsupported, key, len(entries), len(value.Values))
		}
		for i := range entries {
			name, nameErr := itemName(value.Values[i])
			if nameErr != nil {
				return nil, fmt.Errorf("%w: %s 第 %d 项 %s", ErrEditUnsupported, key, i+1, nameErr)
			}
			entries[i].Name = name
			entries[i].Raw = []byte(strings.Join(d.lines[entries[i].Start:entries[i].End], ""))
		}
		blk.entries = entries
		blk.indent = indent
		blk.blockEnd = blockEnd
		blk.appendAt = entries[len(entries)-1].End
		return blk, nil
	default:
		return nil, fmt.Errorf("%w: %s 不是块式列表", ErrEditUnsupported, key)
	}
}

// topLevel 取唯一的顶层键值对；同一键出现两次属于重复键，解析器已拒绝，这里保留
// 防御性检查（AST 可能来自被合并的节点）。
// topLevel returns the unique top-level key/value pair.
func (d *yamlBlockDoc) topLevel(key string) (*ast.MappingValueNode, error) {
	if d.root == nil {
		return nil, nil
	}
	var found *ast.MappingValueNode
	for _, mv := range d.root.Values {
		s, ok := mv.Key.(*ast.StringNode)
		if !ok || s.Value != key {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: 顶层键 %s 出现多次", ErrEditUnsupported, key)
		}
		found = mv
	}
	return found, nil
}

// scanEntries 从键行之后逐行扫描列表条目：条目缩进取首个列表项的缩进，条目跨越
// 到下一个同缩进列表项或块尾；条目间的空行留给分隔、块内注释随前一个条目。
// scanEntries scans item spans by line; blank separators stay between spans and
// indented comments travel with the preceding item.
func (d *yamlBlockDoc) scanEntries(keyLine int) ([]BlockEntry, int, int, error) {
	indent := -1
	var entries []BlockEntry
	blockEnd := len(d.lines)

	for i := keyLine + 1; i < len(d.lines); {
		content := scheduleLineContent(d.lines[i])
		if strings.TrimSpace(content) == "" {
			i++
			continue
		}
		trimmed := strings.TrimLeft(content, " ")
		lead := len(content) - len(trimmed)
		if strings.HasPrefix(trimmed, "#") {
			// 注释串归属：跳过空行与后续注释后仍是块内容 → 块内；否则整串属于下一段。
			j := i
			for j < len(d.lines) {
				next := scheduleLineContent(d.lines[j])
				if strings.TrimSpace(next) == "" || strings.HasPrefix(strings.TrimLeft(next, " "), "#") {
					j++
					continue
				}
				break
			}
			if j < len(d.lines) && d.isBlockContent(j, indent) {
				i = j
				continue
			}
			blockEnd = trimTrailingScheduleBlanks(d.lines, keyLine+1, i)
			break
		}
		if lead == indent || indent < 0 {
			if !isYAMLDashLine(trimmed) {
				if indent < 0 {
					return nil, 0, 0, fmt.Errorf("%w: 目标块首项不是列表项（-）", ErrEditUnsupported)
				}
				blockEnd = trimTrailingScheduleBlanks(d.lines, keyLine+1, i)
				break
			}
			indent = lead
			if len(entries) > 0 {
				prev := &entries[len(entries)-1]
				prev.End = trimTrailingScheduleBlanks(d.lines, prev.Start+1, i)
			}
			entries = append(entries, BlockEntry{Start: i})
			i++
			continue
		}
		if lead > indent {
			i++
			continue
		}
		// 缩进小于列表项且非注释：下一个顶层键（或非法缩进）——块到此结束。
		blockEnd = trimTrailingScheduleBlanks(d.lines, keyLine+1, i)
		break
	}

	if len(entries) == 0 {
		return nil, indent, keyLine + 1, nil
	}
	last := &entries[len(entries)-1]
	end := blockEnd
	if end > len(d.lines) {
		end = len(d.lines)
	}
	last.End = trimTrailingScheduleBlanks(d.lines, last.Start+1, end)
	blockEnd = last.End
	return entries, indent, blockEnd, nil
}

// isBlockContent 判定第 i 行是否仍属当前块（缩进行或同缩进的列表项）。
// isBlockContent reports whether the line still belongs to the block.
func (d *yamlBlockDoc) isBlockContent(i, indent int) bool {
	content := scheduleLineContent(d.lines[i])
	if strings.TrimSpace(content) == "" {
		return false
	}
	trimmed := strings.TrimLeft(content, " ")
	if strings.HasPrefix(trimmed, "#") {
		return false
	}
	lead := len(content) - len(trimmed)
	if lead > indent {
		return true
	}
	return lead == indent && isYAMLDashLine(trimmed)
}

// findEntry 取唯一的同名条目；不存在 → 包装 fs.ErrNotExist（调用方按资源不存在处理），
// 同名出现两次 → 无法确定目标，拒绝。
// findEntry returns the unique named entry; a duplicate name refuses the edit.
func (b *yamlBlock) findEntry(name string) (BlockEntry, error) {
	var found *BlockEntry
	for i := range b.entries {
		if b.entries[i].Name != name {
			continue
		}
		if found != nil {
			return BlockEntry{}, fmt.Errorf("%w: 条目名 %s 重复，无法确定编辑目标", ErrEditUnsupported, name)
		}
		found = &b.entries[i]
	}
	if found == nil {
		return BlockEntry{}, fmt.Errorf("定义文件中不存在条目 %s: %w", name, fs.ErrNotExist)
	}
	return *found, nil
}

// rejectDuplicateName 拒绝追加已存在的条目名：同名条目会让后续按名定位无法进行。
// rejectDuplicateName refuses appending a name that already exists.
func (b *yamlBlock) rejectDuplicateName(name string) error {
	for i := range b.entries {
		if b.entries[i].Name == name {
			return fmt.Errorf("%w: 条目名 %s 已存在，追加会造成重复", ErrEditUnsupported, name)
		}
	}
	return nil
}

// appendNewBlock 在键缺失时创建块：空文档 / 纯注释 / 普通映射末尾追加 `key:` 块；
// 顶层是唯一的空流式映射 `{}`（basalt init 的模板）时把该行替换成块式列表。
// appendNewBlock creates the block at EOF, or replaces a lone empty `{}` line.
func (d *yamlBlockDoc) appendNewBlock(key string, rendered []byte) ([]byte, error) {
	if d.root != nil && d.root.IsFlowStyle {
		rootLine, ok := d.lineOf(d.root)
		if !ok || rootLine < 0 || rootLine >= len(d.lines) || strings.TrimSpace(scheduleLineContent(d.lines[rootLine])) != "{}" || len(d.root.Values) != 0 {
			return nil, fmt.Errorf("%w: 定义文件顶层是流式映射，无法追加块式列表", ErrEditUnsupported)
		}
		terminator := lineTerminator(d.lines[rootLine])
		if terminator == "" {
			terminator = d.newlineStyle()
		}
		lines, _, err := normalizeRenderedEntry(rendered, -1, terminator)
		if err != nil {
			return nil, err
		}
		insert := append([]string{key + ":" + terminator}, lines...)
		return spliceYAMLLines(d.lines, rootLine, rootLine+1, insert), nil
	}

	terminator := d.newlineStyle()
	text := strings.Join(d.lines, "")
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += terminator
	}
	lines, _, err := normalizeRenderedEntry(rendered, -1, terminator)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString(text)
	b.WriteString(key + ":" + terminator)
	for _, line := range lines {
		b.WriteString(line)
	}
	return []byte(b.String()), nil
}

// newlineStyle 取文件的换行风格：首个 CRLF 行决定；纯 LF 文件返回 "\n"。
// newlineStyle derives the file's line ending style from its first CRLF line.
func (d *yamlBlockDoc) newlineStyle() string {
	for _, line := range d.lines {
		if strings.HasSuffix(line, "\r\n") {
			return "\r\n"
		}
	}
	return "\n"
}

// normalizeRenderedEntry 校验并规格化待写入条目：必须是单条块式列表项、映射含
// `name` 字段、无锚点 / 别名 / 合并键；行终止符统一为文件风格，缩进平移到目标
// 列表项缩进（indent < 0 表示沿用条目自身缩进）。
// normalizeRenderedEntry validates and re-indents the rendered entry, converting
// its line terminators to the file's style.
func normalizeRenderedEntry(rendered []byte, indent int, terminator string) ([]string, string, error) {
	name, err := validateRenderedEntry(rendered)
	if err != nil {
		return nil, "", err
	}

	text := strings.ReplaceAll(string(rendered), "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	rawLines := strings.Split(text, "\n")

	renderedIndent := -1
	for _, line := range rawLines {
		if line == "" {
			continue
		}
		lead := leadingSpaces(line)
		if renderedIndent < 0 {
			renderedIndent = lead
			if !isYAMLDashLine(line[lead:]) {
				return nil, "", fmt.Errorf("%w: 待写入条目首行不是列表项（-）", ErrEditUnsupported)
			}
		}
		if lead < renderedIndent {
			return nil, "", fmt.Errorf("%w: 待写入条目缩进不一致", ErrEditUnsupported)
		}
	}
	if renderedIndent < 0 {
		return nil, "", fmt.Errorf("%w: 待写入条目为空", ErrEditUnsupported)
	}

	target := indent
	if target < 0 {
		target = renderedIndent
	}
	delta := target - renderedIndent
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		if line != "" {
			switch {
			case delta > 0:
				line = strings.Repeat(" ", delta) + line
			case delta < 0:
				if leadingSpaces(line) < -delta {
					return nil, "", fmt.Errorf("%w: 待写入条目缩进无法平移到目标列表", ErrEditUnsupported)
				}
				line = line[-delta:]
			}
		}
		lines = append(lines, line+terminator)
	}
	return lines, name, nil
}

// validateRenderedEntry 解析待写入条目并返回其中的 name 字段：文档必须是单条
// 块式列表项 → 块式映射；流式集合、锚点 / 别名 / 合并键一律拒绝（约束 6）。
// validateRenderedEntry parses the rendered entry and returns its name.
func validateRenderedEntry(rendered []byte) (string, error) {
	file, err := parser.ParseBytes(rendered, 0)
	if err != nil {
		return "", fmt.Errorf("%w: 待写入条目不可解析（%s）", ErrEditUnsupported, frontmatterErrorSummary(err))
	}
	if len(file.Docs) != 1 || file.Docs[0].Body == nil {
		return "", fmt.Errorf("%w: 待写入条目必须恰为一个列表项", ErrEditUnsupported)
	}
	seq, ok := file.Docs[0].Body.(*ast.SequenceNode)
	if !ok || seq.IsFlowStyle || len(seq.Values) != 1 {
		return "", fmt.Errorf("%w: 待写入条目必须恰为一个块式列表项", ErrEditUnsupported)
	}
	if err := rejectUnsafeYAMLNodes(seq); err != nil {
		return "", fmt.Errorf("待写入条目: %w", err)
	}
	name, err := itemName(seq.Values[0])
	if err != nil {
		return "", fmt.Errorf("%w: 待写入条目 %s", ErrEditUnsupported, err)
	}
	return name, nil
}

// itemName 取列表项映射的 `name` 字符串字段：项必须是块式映射。
// itemName extracts the item's string `name` field.
func itemName(node ast.Node) (string, error) {
	mapping, ok := node.(*ast.MappingNode)
	if !ok {
		return "", fmt.Errorf("不是映射")
	}
	if mapping.IsFlowStyle {
		return "", fmt.Errorf("是流式映射")
	}
	var name string
	found := false
	for _, mv := range mapping.Values {
		s, ok := mv.Key.(*ast.StringNode)
		if !ok || s.Value != "name" {
			continue
		}
		value, ok := mv.Value.(*ast.StringNode)
		if !ok {
			return "", fmt.Errorf("的 name 不是字符串")
		}
		name = value.Value
		found = true
	}
	if !found {
		return "", fmt.Errorf("缺少 name")
	}
	return name, nil
}

// unsafeYAMLNodeFinder 收集目标子树里的锚点 / 别名 / 合并键：块编辑无法保留它们
// 的引用语义，必须整体拒绝。
// unsafeYAMLNodeFinder records anchor, alias and merge-key nodes in the subtree.
type unsafeYAMLNodeFinder struct {
	kind string
}

// Visit 实现 ast.Visitor；发现第一种不支持构造后停止下探。
// Visit implements ast.Visitor and stops descending once a construct is found.
func (f *unsafeYAMLNodeFinder) Visit(node ast.Node) ast.Visitor {
	if node == nil || f.kind != "" {
		return nil
	}
	switch node.(type) {
	case *ast.AnchorNode:
		f.kind = "锚点（&）"
	case *ast.AliasNode:
		f.kind = "别名（*）"
	case *ast.MergeKeyNode:
		f.kind = "合并键（<<）"
	}
	if f.kind != "" {
		return nil
	}
	return f
}

// rejectUnsafeYAMLNodes 拒绝目标子树中的锚点 / 别名 / 合并键（约束 6）。
// rejectUnsafeYAMLNodes refuses anchors, aliases and merge keys in the subtree.
func rejectUnsafeYAMLNodes(node ast.Node) error {
	finder := &unsafeYAMLNodeFinder{}
	ast.Walk(finder, node)
	if finder.kind != "" {
		return fmt.Errorf("%w: 目标块含%s，无法安全块编辑", ErrEditUnsupported, finder.kind)
	}
	return nil
}

// entryHasComment 判定条目行区间内是否含注释（整行注释或引号外的行内注释）：
// 这些字节在整条替换 / 删除时无法保留，如实上报。
// entryHasComment reports comments within the entry span, which a whole-entry
// rewrite cannot preserve.
func entryHasComment(lines []string, start, end int) bool {
	for i := start; i < end; i++ {
		if hasUnquotedComment(scheduleLineContent(lines[i])) {
			return true
		}
	}
	return false
}

// spliceYAMLLines 用 insert 替换 [start, end) 行区间：区间外逐字节拼接，不做任何
// 归一化（SplitAfter + Join 是无损往返）。
// spliceYAMLLines replaces a line range without touching any other byte.
func spliceYAMLLines(lines []string, start, end int, insert []string) []byte {
	var b strings.Builder
	for _, line := range lines[:start] {
		b.WriteString(line)
	}
	for _, line := range insert {
		b.WriteString(line)
	}
	for _, line := range lines[end:] {
		b.WriteString(line)
	}
	return []byte(b.String())
}

// isYAMLDashLine 判定一个去除缩进后的行是否开启一个块式列表项。
// isYAMLDashLine reports a block sequence item indicator.
func isYAMLDashLine(s string) bool {
	return s == "-" || strings.HasPrefix(s, "- ")
}

// lineOf 定位节点所在的 0 基行号。
//
// 不采信 `Position.Offset`：实测 goccy v1.19.2 在连续注释行之后会少算偏移
// （键前两行注释少 1、三行少 2，中文与纯 ASCII 同病），且中文内容按字符计数、
// 其它路径按字节计数——两种基准混在一起，换算不出可靠行号（评测夹具的
// "两行中文注释 + providers:" 就是这样被打红的）。`Position.Line` 在 LF 文档
// 里逐例实测正确；CRLF 下它把 `\r` 也当换行，每条 CRLF 让行号多算一行。虚增量
// 恰是目标行之前的 CRLF 条数，因此按「报出行号与该行行号之差 = 该行之前的 CRLF
// 条数」做定点校正即可还原真实行号（LF 文档校正量为 0，自然落在原行号）。
// lineOf derives the zero-based line from Position.Line and repairs goccy's CRLF
// inflation; Position.Offset is unusable (it under-counts after comment runs and
// mixes byte and rune units).
func (d *yamlBlockDoc) lineOf(node ast.Node) (int, bool) {
	if node == nil {
		return 0, false
	}
	token := node.GetToken()
	if token == nil || token.Position == nil {
		return 0, false
	}
	reported := token.Position.Line - 1
	if reported < 0 {
		return 0, false
	}
	if reported >= len(d.lines) {
		reported = len(d.lines) - 1
	}
	for line := reported; line >= 0; line-- {
		if d.carriageReturnsBefore(line) == reported-line {
			return line, true
		}
	}
	return 0, false
}

// carriageReturnsBefore 数第 line 行（0 基）之前以 CRLF 结尾的行数——即 goccy 在
// 该行行号上多算的行数。只数行尾的 `\r\n`：正文里的孤立 `\r` 不产生换行，不参与校正。
// carriageReturnsBefore counts CRLF-terminated lines before a zero-based line.
func (d *yamlBlockDoc) carriageReturnsBefore(line int) int {
	count := 0
	for i := 0; i < line && i < len(d.lines); i++ {
		if strings.HasSuffix(d.lines[i], "\r\n") {
			count++
		}
	}
	return count
}
