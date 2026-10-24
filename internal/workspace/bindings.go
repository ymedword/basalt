package workspace

// 本文件是 AGENT.md frontmatter 中 `resource_bindings` 块的解析、序列化与块编辑面
// （颗粒度文档 R4.6 / 契约 agent-md §2.5 / 约束 6）：`AGENT.md` 是绑定的唯一权威，
// 数据库 `agent_resource_bindings` 只是可重建投影。解析只做**结构**校验（对象、
// 两个已知子键、元素 `{name, uid}`、UID 格式与唯一性）；与 `provider.name` /
// `notify_channels` 的一一对应属于保存期与准入判据，不在装载期判。
// This file parses, renders and surgically edits the `resource_bindings` block.
// Parsing checks structure only; correspondence with declared references belongs to
// save-time and admission checks. Anything the line writer cannot preserve verbatim
// is refused instead of being silently regenerated.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/ymedword/basalt/internal/core"
)

// resourceBindingsKey 是 frontmatter 中的顶层键字面量（契约 agent-md §2.5）。
// resourceBindingsKey is the top-level frontmatter key.
const resourceBindingsKey = "resource_bindings"

// resourceBindingsDTO 是 `resource_bindings` 块在 YAML 解码时的私有形态：
// 只有两个已知子键，未知子键由 DisallowUnknownField 拒绝。
// resourceBindingsDTO is the private decode shape of the block; unknown sub-keys
// are refused by DisallowUnknownField.
type resourceBindingsDTO struct {
	ModelConnection []resourceBindingDTO `yaml:"model_connection"`
	NotifyChannel   []resourceBindingDTO `yaml:"notify_channel"`
}

// resourceBindingDTO 是一条 `{name, uid}` 绑定；其余字段一律拒绝。
// resourceBindingDTO is one `{name, uid}` entry; any other field is refused.
type resourceBindingDTO struct {
	Name string `yaml:"name"`
	UID  string `yaml:"uid"`
}

// ParseResourceBindings 从 AGENT.md 原始字节解析 `resource_bindings`：块缺失或
// 值为空映射时返回空结构（旧 Agent 与模板占位符仍可装载）；结构非法（顶层不是
// 映射、子键未知、元素缺 `name` / `uid`、UID 非法、同名或同 UID 重复）一律包装
// `core.ErrAgentResourceBindingInvalid` 拒绝——加载期据此记 WARN `invalid
// resource_bindings` 并整只不注册。
// ParseResourceBindings returns an empty structure when the block is absent and
// refuses any structurally invalid block with ErrAgentResourceBindingInvalid.
func ParseResourceBindings(raw []byte) (core.ResourceBindings, error) {
	front, _, err := splitFrontmatter(raw)
	if err != nil {
		return core.ResourceBindings{}, fmt.Errorf("%w: %v", core.ErrAgentResourceBindingInvalid, err)
	}
	return parseBindingsFront(front)
}

// parseBindingsFront 在**已切出**的 frontmatter 字节上解析绑定块：装载路径
// （parseFrontmatter）与独立解析共用同一段结构校验，不把文件读两遍。
// parseBindingsFront parses the block from already-split frontmatter bytes so the
// loader and the standalone parser share one structural check.
func parseBindingsFront(front []byte) (core.ResourceBindings, error) {
	value, found, err := findFrontmatterValue(front, resourceBindingsKey)
	if err != nil {
		return core.ResourceBindings{}, err
	}
	if !found {
		return core.ResourceBindings{}, nil
	}

	var dto resourceBindingsDTO
	if err := yaml.NodeToValue(value, &dto, yaml.DisallowUnknownField()); err != nil {
		return core.ResourceBindings{}, fmt.Errorf("%w: %s 结构非法: %v",
			core.ErrAgentResourceBindingInvalid, resourceBindingsKey, err)
	}
	out := core.ResourceBindings{}
	for _, entry := range dto.ModelConnection {
		out.ModelConnections = append(out.ModelConnections, core.ResourceBinding{
			Name: entry.Name,
			UID:  core.ResourceUID(entry.UID),
		})
	}
	for _, entry := range dto.NotifyChannel {
		out.NotifyChannels = append(out.NotifyChannels, core.ResourceBinding{
			Name: entry.Name,
			UID:  core.ResourceUID(entry.UID),
		})
	}
	if err := out.Validate(); err != nil {
		return core.ResourceBindings{}, err
	}
	return out, nil
}

// findFrontmatterValue 在 frontmatter 顶层映射中取唯一同名键的值节点：键可以带
// 引号（按解码后的文本比较），重复出现 = 锚点不唯一，拒绝。
// findFrontmatterValue returns the value node of the unique key; a duplicate key
// (including differently quoted spellings) is refused.
func findFrontmatterValue(front []byte, key string) (ast.Node, bool, error) {
	mapping, err := frontmatterMapping(front)
	if err != nil {
		return nil, false, err
	}
	if mapping == nil {
		return nil, false, nil
	}
	var value ast.Node
	seen := false
	for _, item := range mapping.Values {
		text, ok := mappingKeyText(item.Key)
		if !ok || text != key {
			continue
		}
		if seen {
			return nil, false, fmt.Errorf("%w: 顶层键 %s 出现多次",
				core.ErrAgentResourceBindingInvalid, key)
		}
		value, seen = item.Value, true
	}
	return value, seen, nil
}

// frontmatterMapping 把 frontmatter 解析成顶层映射节点：空 frontmatter 返回
// (nil, nil)；多文档或顶层不是映射一律按结构非法拒绝（不猜用户的意图）。
// frontmatterMapping parses the frontmatter into its top-level mapping; an empty
// document yields nil and a non-mapping document is refused.
func frontmatterMapping(front []byte) (*ast.MappingNode, error) {
	if strings.TrimSpace(string(front)) == "" {
		return nil, nil
	}
	file, err := parser.ParseBytes(front, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: frontmatter 不可解析（%s）",
			core.ErrAgentResourceBindingInvalid, frontmatterErrorSummary(err))
	}
	if len(file.Docs) != 1 {
		return nil, fmt.Errorf("%w: frontmatter 含 %d 个 YAML 文档",
			core.ErrAgentResourceBindingInvalid, len(file.Docs))
	}
	if file.Docs[0].Body == nil {
		return nil, nil
	}
	mapping, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return nil, fmt.Errorf("%w: frontmatter 顶层必须是映射",
			core.ErrAgentResourceBindingInvalid)
	}
	return mapping, nil
}

// mappingKeyText 取映射键的文本形态（解码后，含引号写法的规范化）；非标量键
// （复杂键）返回 ok=false，由调用方忽略——复杂键不可能是两个已知子键之一。
// mappingKeyText returns the decoded text of a scalar key.
func mappingKeyText(key ast.MapKeyNode) (string, bool) {
	scalar, ok := key.(*ast.StringNode)
	if !ok {
		return "", false
	}
	return scalar.Value, true
}

// renderResourceBindingsBlock 渲染规范的 `resource_bindings` 块：两个子键按固定
// 顺序输出，空组渲染为 `[]`；每一行以调用方的换行风格结尾。所有标量一律经
// yamlQuote——裸写会被 YAML 重新解释（与 schedules 块同一纪律）。
// renderResourceBindingsBlock renders the canonical block with the caller's line
// ending; every scalar is quoted so no value can be reinterpreted.
func renderResourceBindingsBlock(bindings core.ResourceBindings, newline string) string {
	var sb strings.Builder
	sb.WriteString(resourceBindingsKey + ":" + newline)
	writeGroup := func(key string, entries []core.ResourceBinding) {
		if len(entries) == 0 {
			sb.WriteString("  " + key + ": []" + newline)
			return
		}
		sb.WriteString("  " + key + ":" + newline)
		for _, entry := range entries {
			sb.WriteString("    - name: " + yamlQuote(entry.Name) + newline)
			sb.WriteString("      uid: " + yamlQuote(entry.UID.String()) + newline)
		}
	}
	writeGroup(string(core.BindingModelConnection), bindings.ModelConnections)
	writeGroup(string(core.BindingNotifyChannel), bindings.NotifyChannels)
	return sb.String()
}

// PatchResourceBindings 把绑定**整块**写回 AGENT.md：`changes.bindings` 的语义是
// 「数组 = 整体替换」，不做按名合并，也不按名称重新解析 UID（契约 agent-md §2.3）。
// 块不存在时在收尾围栏之前插入；块存在时替换其行区间，块外字节逐字保留。
// 以下任一情况包装 `ErrEditUnsupported` 拒绝且不返回字节（调用方零写入）：
// 围栏缺失 / 不闭合、顶层键重复、块内含注释 / 锚点 / 别名 / 制表符 / 非裸键行、
// 顶层不是映射、或键以引号等 locateTopBlock 认不出的写法出现。
// PatchResourceBindings replaces the whole block; it never merges by name and never
// re-resolves UIDs. Everything it cannot preserve verbatim refuses with
// ErrEditUnsupported so the caller writes nothing.
func PatchResourceBindings(raw []byte, bindings core.ResourceBindings) ([]byte, error) {
	if err := bindings.Validate(); err != nil {
		return nil, err
	}
	f, err := parseFenced(raw)
	if err != nil {
		return nil, err
	}
	block, found, err := f.locateTopBlock(resourceBindingsKey)
	if err != nil {
		return nil, err
	}

	var out []byte
	if found {
		if err := f.scanBindingsBlock(block); err != nil {
			return nil, err
		}
		out, err = f.replaceRange(block.start, block.end, renderResourceBindingsBlock(bindings, f.newline))
		if err != nil {
			return nil, err
		}
	} else {
		if err := bindingInsertionPrecondition(raw); err != nil {
			return nil, err
		}
		f.insertLine(f.closing, renderResourceBindingsBlock(bindings, f.newline))
		out = f.bytes()
	}

	// 回读一致性检查：渲染结果必须解析回同一组绑定，否则拒绝（不静默落盘）。
	// Read-back check: the rendered bytes must parse back into the same bindings.
	if err := verifyRenderedBindings(out, bindings); err != nil {
		return nil, err
	}
	return out, nil
}

// scanBindingsBlock 校验目标块可安全整体替换：键行必须是裸 `resource_bindings:`；
// 块内不得含制表符、注释、锚点 / 别名或合并键——这些构造一旦被规范渲染覆盖，
// 语义会静默改变或注释会静默丢失，宁可拒绝也不猜。
// scanBindingsBlock refuses any construct whose replacement would silently drop a
// comment or change an anchor's meaning.
func (f *fenced) scanBindingsBlock(block topBlock) error {
	if keyLine := scheduleLineContent(f.lines[block.start]); keyLine != resourceBindingsKey+":" {
		return fmt.Errorf("%w: %s 行不是裸块键（含行内值或注释）", ErrEditUnsupported, resourceBindingsKey)
	}
	for i := block.start + 1; i < block.end; i++ {
		content := scheduleLineContent(f.lines[i])
		if strings.TrimSpace(content) == "" {
			continue
		}
		if strings.Contains(content, "\t") {
			return fmt.Errorf("%w: %s 块含制表符", ErrEditUnsupported, resourceBindingsKey)
		}
		if hasUnquotedComment(content) {
			return fmt.Errorf("%w: %s 块含注释", ErrEditUnsupported, resourceBindingsKey)
		}
		if hasUnquotedAnchor(content) {
			return fmt.Errorf("%w: %s 块含锚点 / 别名 / 合并键", ErrEditUnsupported, resourceBindingsKey)
		}
	}
	return nil
}

// bindingInsertionPrecondition 是「块缺失时安全插入」的前提：frontmatter 必须
// 解析为顶层映射，且不存在 locateTopBlock 认不出的同名键写法（引号键 / 流式写法
// 出现在别处会让插入产生重复键）。不满足 → 拒绝，**不得**盲目插入。
// bindingInsertionPrecondition refuses insertion unless the frontmatter is a plain
// mapping with no differently-spelled key of the same name.
func bindingInsertionPrecondition(raw []byte) error {
	front, _, err := splitFrontmatter(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrEditUnsupported, err)
	}
	if _, found, err := findFrontmatterValue(front, resourceBindingsKey); err != nil {
		return fmt.Errorf("%w: %v", ErrEditUnsupported, err)
	} else if found {
		return fmt.Errorf("%w: 顶层键 %s 的写法无法安全块编辑（引号键或流式集合）",
			ErrEditUnsupported, resourceBindingsKey)
	}
	return nil
}

// verifyRenderedBindings 回读渲染结果并与期望值逐项比较（顺序敏感：渲染与解析
// 都保序，顺序变化本身就是不一致）。
// verifyRenderedBindings re-parses the rendered bytes and compares the bindings
// in order; any drift refuses the write.
func verifyRenderedBindings(out []byte, want core.ResourceBindings) error {
	got, err := ParseResourceBindings(out)
	if err != nil {
		return fmt.Errorf("%w: 绑定块回读失败: %v", ErrEditUnsupported, err)
	}
	if !slices.Equal(got.ModelConnections, want.ModelConnections) ||
		!slices.Equal(got.NotifyChannels, want.NotifyChannels) {
		return fmt.Errorf("%w: 绑定块回读与期望不一致", ErrEditUnsupported)
	}
	return nil
}

// BindingsBackfill 是一次迁移期绑定回填的结果：写前 / 写后内容哈希与是否真的写入。
// 哈希是裸 sha256 十六进制（与资源版本内容哈希同一形态）。
// BindingsBackfill is the outcome of one migration-time backfill: the before/after
// content hashes and whether anything was written.
type BindingsBackfill struct {
	BeforeHash string
	AfterHash  string
	Changed    bool
}

// BackfillAgentBindings 是 v3 迁移第 4 步的写面：读 `agents/<name>/AGENT.md`，用
// PatchResourceBindings 生成新字节并原子写回。三条纪律与迁移卡一致：
//   - **块外字节逐字保留**（PatchResourceBindings 的行区间替换）；
//   - 渲染结果与原文逐字节相同 → 不写（重跑幂等，`Changed=false`）；
//   - 结构无法安全编辑 → 原样返回 `ErrEditUnsupported`，磁盘逐字节不变。
//
// BackfillAgentBindings applies the binding block during the v3 migration. It never
// rewrites bytes outside the block and never touches the file when nothing changed.
func BackfillAgentBindings(root, agent string, bindings core.ResourceBindings) (out BindingsBackfill, err error) {
	if err := ValidateAgentName(agent); err != nil {
		return out, err
	}
	if err := bindings.Validate(); err != nil {
		return out, err
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return out, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	rel := agentDefinitionPath(agent)
	raw, err := base.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		return out, fmt.Errorf("读取 Agent %s 的定义文件: %w", agent, err)
	}
	out.BeforeHash = sha256Hex(raw)

	next, err := PatchResourceBindings(raw, bindings)
	if err != nil {
		return out, err
	}
	out.AfterHash = sha256Hex(next)
	if bytes.Equal(next, raw) {
		return out, nil
	}

	rename := func(root *os.Root, oldname, newname string) error {
		return root.Rename(oldname, newname)
	}
	if err := writeRootFileAtomic(base, rel, next, rename); err != nil {
		return out, err
	}
	out.Changed = true
	return out, nil
}

// AgentMigrationView 是 v3 迁移对一个 Agent 的**只读**快照：定义文件原始字节、
// 解析出的 Profile（含文件里已有的绑定块）与修改时间。迁移用它在写之前决定
// "证明得了连续性吗"，以及写之后重建投影——两次读的都是文件本身，不缓存。
// AgentMigrationView is the read-only snapshot the v3 migration takes of one agent.
type AgentMigrationView struct {
	Raw      []byte
	Profile  *core.Profile
	Bindings core.ResourceBindings
	ModTime  time.Time
}

// InspectAgentForMigration 读出迁移需要的三样东西：原始字节（写前哈希与块编辑的
// 输入）、Profile（`provider.name` 与 `notify_channels` 是待证明的引用）、已有绑定
// （重跑时判定"计划与文件是否一致"）。文件不存在原样返回 `fs.ErrNotExist`——调用方
// 据此记 `definition_missing`，**不**把它当成迁移失败。
// InspectAgentForMigration reads the bytes, the profile and the existing bindings;
// a missing definition returns fs.ErrNotExist untouched.
func InspectAgentForMigration(root, agent string) (out AgentMigrationView, err error) {
	if err := ValidateAgentName(agent); err != nil {
		return out, err
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return out, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	rel := filepath.FromSlash(agentDefinitionPath(agent))
	info, err := base.Stat(rel)
	if err != nil {
		return out, fmt.Errorf("读取 Agent %s 的定义文件: %w", agent, err)
	}
	raw, err := base.ReadFile(rel)
	if err != nil {
		return out, fmt.Errorf("读取 Agent %s 的定义文件: %w", agent, err)
	}

	profile, _, err := parseFrontmatter(raw)
	if err != nil {
		return out, fmt.Errorf("解析 Agent %s 的定义文件: %w", agent, err)
	}
	return AgentMigrationView{
		Raw:      raw,
		Profile:  profile,
		Bindings: profile.ResourceBindings,
		ModTime:  info.ModTime(),
	}, nil
}

// hasUnquotedAnchor 报告行内（引号之外）是否出现 YAML 锚点 / 别名 / 合并键标记。
// 这些构造可能被别处引用，覆盖块内文本会改变外部含义，因此一律拒绝。
// hasUnquotedAnchor reports YAML anchor, alias or merge-key markers outside quotes.
func hasUnquotedAnchor(line string) bool {
	quote := byte(0)
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch quote {
		case '\'':
			if ch != '\'' {
				continue
			}
			if i+1 < len(line) && line[i+1] == '\'' {
				i++
				continue
			}
			quote = 0
			continue
		case '"':
			if ch == '\\' {
				i++
				continue
			}
			if ch == '"' {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '&', '*':
			return true
		case '<':
			if strings.HasPrefix(line[i:], "<<:") {
				return true
			}
		}
	}
	return false
}
