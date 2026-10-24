package workspace

// 本文件是 `basalt.yaml` 中 `providers:` 条目块的解析、校验、渲染与原子写
// （颗粒度文档 §3.1.3 / 约束 7）：条目字段固定为 name / base_url / api_key /
// default_model，渲染键序固定；`api_key` 只接受精确 `${VAR}` 占位，**不**读环境变量、
// **不**解析无关段的占位——解析归装配层（构建运行时客户端时）。
// 失败分类：内容不合法 → `ErrInvalidDefinition`（调用方映射 400 BAD_REQUEST，
// 点名键）；块结构无法安全编辑 → `ErrEditUnsupported`（400 EDIT_UNSUPPORTED）。
// This file parses, validates, renders and atomically writes provider entries. It
// never reads the environment: placeholders are checked for shape only, and values
// of other sections are never resolved here.

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/ymedword/basalt/internal/core"
)

// ErrInvalidDefinition 表示定义内容（字段取值 / 结构与约束不符）未通过候选校验：
// 调用方映射 400 BAD_REQUEST 并点名键。它与 `ErrEditUnsupported` 是两类失败——
// 前者是「内容不合法」，后者是「格式无法安全改写」（400 EDIT_UNSUPPORTED）。
// ErrInvalidDefinition marks a definition whose content violates the candidate
// rules (400 BAD_REQUEST), as opposed to a block that cannot be edited safely.
var ErrInvalidDefinition = errors.New("invalid definition")

// providersKey 是 `basalt.yaml` 中模型连接列表的顶层键。
const providersKey = "providers"

// providerPlaceholder 匹配**整体**的 `${VAR}` 占位：前导 / 后缀文本、空变量名、
// 嵌套占位都不匹配（约束 7）。
// providerPlaceholder matches an entire ${VAR} placeholder; prefixes, suffixes,
// empty names and nested placeholders never match.
var providerPlaceholder = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// ProviderEntry 是 `providers:` 列表中的一个模型连接条目。env 模式下 `APIKey` 读取与
// 渲染时恒为精确 `${VAR}` 占位；「空串 = 更新时保留原值」是服务层语义——调用方必须先
// 填入旧占位再调用渲染。`CredentialMode` 是 v3 的两模式开关（R4.8）：空串 = env（兼容
// v2 定义）；`managed` 时凭据在凭据表，条目里**没有** `api_key`。
// ProviderEntry is one model connection entry. APIKey is always the exact ${VAR}
// placeholder in env mode; CredentialMode is the v3 two-mode switch and a managed entry
// carries no api_key at all.
type ProviderEntry struct {
	Name           string
	BaseURL        string
	APIKey         string
	DefaultModel   string
	CredentialMode string
	// ResourceUID 是**已发布列表**侧才有的稳定身份（来自发布指针）；从文件解析出来的
	// 条目没有这一项（空串 = 未记录）。模板创建据此写 `resource_bindings`——绑定必须
	// 携带 UID，按名解析是被禁止的（颗粒度文档 R4.6）。
	// ResourceUID is only populated for published entries; file-parsed entries leave it
	// empty and template creation writes no binding for them.
	ResourceUID core.ResourceUID
}

// 凭据模式的两个契约字面量：与 `core.CredentialMode` 同一取值，定义文件是字符串面。
// The two contract literals of the credential mode, as they appear in the definition file.
const (
	providerCredentialModeEnv     = "env"
	providerCredentialModeManaged = "managed"
)

// providerCredentialMode 归一化条目的凭据模式：空串 = env（v2 定义兼容），其余只接受
// 两个契约字面量。模式只由字段判定，**不**按 `api_key` 的取值形态推断。
// providerCredentialMode defaults a missing mode to env and refuses unknown literals.
func providerCredentialMode(p ProviderEntry) (string, error) {
	mode := strings.TrimSpace(p.CredentialMode)
	switch mode {
	case "":
		return providerCredentialModeEnv, nil
	case providerCredentialModeEnv, providerCredentialModeManaged:
		return mode, nil
	default:
		return "", fmt.Errorf("%w: 字段 credential_mode 只接受 env / managed（实际 %q）",
			ErrInvalidDefinition, mode)
	}
}

// readProviderFile 读取定义文件——读取范围经 `os.Root` 钉在文件所在目录。
// readProviderFile reads the definition file through an `os.Root` opened on its
// own directory.
//
// 为什么不用裸 `os.ReadFile(path)`：路径来自调用方参数，静态安全扫描对此判 G304
// （CWE-22 文件包含），而抑制手段默认禁用。以父目录为 root 读取既消除该判定，又把
// 「跳到目录之外的软连接」挡在门外——fail-closed，与 `cli.readConfigFile` 同一先例。
// The os.Root scoping is gate-clean and fail-closed for symlinks escaping the
// directory, mirroring cli.readConfigFile.
func readProviderFile(path string) (data []byte, err error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭目录 %s: %w", filepath.Dir(path), closeErr))
		}
	}()
	return root.ReadFile(filepath.Base(path))
}

// ReadProviderEntries 读取定义文件并校验其中的 `providers:` 块；文件缺失或内容
// 不合法都返回错误（读取不代建文件）。没有该键 → 空列表、无错误。
// ReadProviderEntries reads the definition file and validates the providers block;
// a missing key yields no entries, while a missing file is an error.
func ReadProviderEntries(path string) ([]ProviderEntry, error) {
	raw, err := readProviderFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取模型连接定义 %s: %w", path, err)
	}
	return ValidateProviderEntries(raw)
}

// ValidateProviderEntries 校验 provider 定义并返回条目列表，接受两种字节形态：
//
//   - **完整文档**（顶层映射，含 `providers:` 块）——发布前校验块编辑后的候选文件；
//     没有该键 → 空列表。
//   - **资源块字节**（顶层块式列表）——即已发布版本的 `definition_yaml`（撤回 /
//     发布旧版本时校验历史块原文）。
//
// 其它形态（空标量、流式集合、多文档、含锚点 / 别名 / 合并键）一律拒绝。只校验
// 字段与结构，**不**解析任何环境变量。
// ValidateProviderEntries accepts either a whole document with a providers block or
// a bare block sequence (a published definition_yaml excerpt), and never resolves
// environment variables.
func ValidateProviderEntries(raw []byte) ([]ProviderEntry, error) {
	file, err := parser.ParseBytes(raw, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: provider 定义不可解析", ErrInvalidDefinition)
	}
	if len(file.Docs) != 1 {
		return nil, fmt.Errorf("%w: provider 定义必须恰为一个 YAML 文档", ErrInvalidDefinition)
	}
	if seq, ok := file.Docs[0].Body.(*ast.SequenceNode); ok {
		return providerEntriesFromSequence(seq)
	}

	doc, err := parseYAMLBlockDoc(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: provider 定义不可解析", ErrInvalidDefinition)
	}
	blk, err := doc.resolveBlock(providersKey)
	if err != nil {
		return nil, fmt.Errorf("%w: providers 块结构不支持（%s）", ErrInvalidDefinition, err)
	}
	if blk == nil {
		return nil, nil
	}

	entries := make([]ProviderEntry, 0, len(blk.entries))
	for i, entry := range blk.entries {
		parsed, err := parseProviderEntry(entry.Raw)
		if err != nil {
			return nil, fmt.Errorf("providers 第 %d 项: %w", i+1, err)
		}
		entries = append(entries, parsed)
	}
	if err := validateUniqueProviderNames(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// providerEntriesFromSequence 校验资源块字节形态的条目列表（已发布版本的
// `definition_yaml`）：顶层必须是块式列表，条目结构同文档形态。
// providerEntriesFromSequence validates the published block-excerpt form.
func providerEntriesFromSequence(seq *ast.SequenceNode) ([]ProviderEntry, error) {
	if seq.IsFlowStyle {
		return nil, fmt.Errorf("%w: providers 资源块必须是块式列表", ErrInvalidDefinition)
	}
	if err := rejectUnsafeYAMLNodes(seq); err != nil {
		return nil, fmt.Errorf("%w: providers 资源块%s", ErrInvalidDefinition, err)
	}
	entries := make([]ProviderEntry, 0, len(seq.Values))
	for i, node := range seq.Values {
		mapping, ok := node.(*ast.MappingNode)
		if !ok || mapping.IsFlowStyle {
			return nil, fmt.Errorf("%w: providers 第 %d 项必须是块式映射", ErrInvalidDefinition, i+1)
		}
		parsed, err := providerEntryFromMapping(mapping)
		if err != nil {
			return nil, fmt.Errorf("providers 第 %d 项: %w", i+1, err)
		}
		entries = append(entries, parsed)
	}
	if err := validateUniqueProviderNames(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// validateUniqueProviderNames 拒绝重名条目：同一文件里按名定位必须唯一。
// validateUniqueProviderNames refuses duplicate names within one definition.
func validateUniqueProviderNames(entries []ProviderEntry) error {
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if _, dup := seen[entry.Name]; dup {
			return fmt.Errorf("%w: providers 条目名 %s 重复", ErrInvalidDefinition, entry.Name)
		}
		seen[entry.Name] = struct{}{}
	}
	return nil
}

// RenderProviderEntry 渲染单个条目：键序固定 `name` / `base_url` / `api_key` /
// `credential_mode` / `default_model`（后两者缺省时整行省略），块式列表项缩进两格。
// `api_key` 原样输出占位（已验证为精确 `${VAR}`，不含需要转移的字符）；其余字段按需
// 加引号。managed 模式**不**输出 `api_key` 行——条目里根本没有这个值。
// RenderProviderEntry renders one entry in the fixed key order; managed entries carry
// no api_key line at all.
func RenderProviderEntry(p ProviderEntry) ([]byte, error) {
	if err := validateProviderEntry(p); err != nil {
		return nil, err
	}
	mode, err := providerCredentialMode(p)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("  - name: " + renderYAMLScalar(p.Name) + "\n")
	b.WriteString("    base_url: " + renderYAMLScalar(p.BaseURL) + "\n")
	if mode != providerCredentialModeManaged {
		b.WriteString("    api_key: " + p.APIKey + "\n")
	}
	if strings.TrimSpace(p.CredentialMode) != "" {
		b.WriteString("    credential_mode: " + mode + "\n")
	}
	b.WriteString("    default_model: " + renderYAMLScalar(p.DefaultModel) + "\n")
	return []byte(b.String()), nil
}

// UpdateProviderEntry 写入（替换或追加）一条模型连接并原子落盘。条目已存在时只替换
// 该条目字节，不存在时追加到块尾；块外字节逐字保留。
//
// 目标条目内含注释、整条替换无法保留时**不写文件**、返回 `commentsLost=true`——调用方
// 据此映射 400 EDIT_UNSUPPORTED（失败判据表要求文件逐字节不变）。任何校验失败同样
// 零写入：先校验候选（含其余条目），再写临时文件 + fsync + rename。
// UpdateProviderEntry replaces or appends one entry and writes atomically. When the
// entry's comments cannot survive the rewrite it returns commentsLost without
// touching the file, so the caller can map EDIT_UNSUPPORTED with a byte-identical
// file. Validation happens before the temp-file + fsync + rename write.
func UpdateProviderEntry(path string, p ProviderEntry) (commentsLost bool, err error) {
	rendered, err := RenderProviderEntry(p)
	if err != nil {
		return false, err
	}
	doc, err := readProviderFile(path)
	if err != nil {
		return false, fmt.Errorf("读取模型连接定义 %s: %w", path, err)
	}
	entries, err := BlockEntries(doc, providersKey)
	if err != nil {
		return false, err
	}

	var (
		candidate []byte
		lost      bool
	)
	if providerEntryExists(entries, p.Name) {
		candidate, lost, err = ReplaceEntry(doc, providersKey, p.Name, rendered)
	} else {
		candidate, err = AppendEntry(doc, providersKey, rendered)
	}
	if err != nil {
		return false, err
	}
	if lost {
		return true, nil
	}
	if _, err := ValidateProviderEntries(candidate); err != nil {
		return false, err
	}
	if err := writeProviderFile(path, candidate); err != nil {
		return false, err
	}
	return false, nil
}

// RemoveProviderEntry 删除一条模型连接并原子落盘；条目不存在 → `fs.ErrNotExist`
// （调用方映射 404）。条目内含注释无法保留时**不写文件**、返回 `commentsLost=true`；
// 删除后整份定义仍须通过校验才落盘。
// RemoveProviderEntry removes one entry atomically. A missing target yields
// fs.ErrNotExist; comments inside the entry stop the write and report commentsLost.
func RemoveProviderEntry(path, name string) (commentsLost bool, err error) {
	doc, err := readProviderFile(path)
	if err != nil {
		return false, fmt.Errorf("读取模型连接定义 %s: %w", path, err)
	}
	candidate, lost, err := RemoveEntry(doc, providersKey, name)
	if err != nil {
		return false, err
	}
	if lost {
		return true, nil
	}
	if _, err := ValidateProviderEntries(candidate); err != nil {
		return false, err
	}
	if err := writeProviderFile(path, candidate); err != nil {
		return false, err
	}
	return false, nil
}

// parseProviderEntry 解析单个条目块（BlockEntries 给出的 Raw）：必须是单个块式列表项、
// 块式映射，字段值必须是字符串，未知字段拒绝（渲染只能输出固定五键，放行未知字段
// 会在下次写入时静默丢失）。
// parseProviderEntry parses one entry block: a single block-style mapping item with
// string values and only the five known keys.
func parseProviderEntry(raw []byte) (ProviderEntry, error) {
	file, err := parser.ParseBytes(raw, 0)
	if err != nil {
		return ProviderEntry{}, fmt.Errorf("%w: 条目不可解析", ErrInvalidDefinition)
	}
	if len(file.Docs) != 1 || file.Docs[0].Body == nil {
		return ProviderEntry{}, fmt.Errorf("%w: 条目必须是单个列表项", ErrInvalidDefinition)
	}
	seq, ok := file.Docs[0].Body.(*ast.SequenceNode)
	if !ok || seq.IsFlowStyle || len(seq.Values) != 1 {
		return ProviderEntry{}, fmt.Errorf("%w: 条目必须是单个块式列表项", ErrInvalidDefinition)
	}
	mapping, ok := seq.Values[0].(*ast.MappingNode)
	if !ok || mapping.IsFlowStyle {
		return ProviderEntry{}, fmt.Errorf("%w: 条目必须是块式映射", ErrInvalidDefinition)
	}
	return providerEntryFromMapping(mapping)
}

// providerEntryFromMapping 把条目映射读成 ProviderEntry：只接受五个已知字符串键，
// 未知键拒绝（渲染只能输出这五键，放行未知字段会在下次写入时静默丢失）。
// providerEntryFromMapping reads one entry mapping, accepting only the five known
// string keys; unknown keys are refused to avoid silent data loss on rewrite.
func providerEntryFromMapping(mapping *ast.MappingNode) (ProviderEntry, error) {
	var p ProviderEntry
	for _, mv := range mapping.Values {
		key, ok := mv.Key.(*ast.StringNode)
		if !ok {
			return ProviderEntry{}, fmt.Errorf("%w: 条目含非字符串键", ErrInvalidDefinition)
		}
		value, ok := mv.Value.(*ast.StringNode)
		if !ok {
			return ProviderEntry{}, fmt.Errorf("%w: 字段 %s 必须是字符串", ErrInvalidDefinition, key.Value)
		}
		switch key.Value {
		case "name":
			p.Name = value.Value
		case "base_url":
			p.BaseURL = value.Value
		case "api_key":
			p.APIKey = value.Value
		case "default_model":
			p.DefaultModel = value.Value
		case "credential_mode":
			p.CredentialMode = value.Value
		default:
			return ProviderEntry{}, fmt.Errorf("%w: 未知字段 %s（只接受 name / base_url / api_key / default_model / credential_mode）", ErrInvalidDefinition, key.Value)
		}
	}
	if err := validateProviderEntry(p); err != nil {
		return ProviderEntry{}, err
	}
	return p, nil
}

// validateProviderEntry 执行约束 7 的字段校验：必填非空、base_url 只接受 http / https
// 且无 userinfo；`api_key` 的规则按模式分支——env 只接受精确 `${VAR}` 占位，managed
// **不得**出现（凭据在凭据表，落盘任何值都会变成明文残留）。错误文案点名键，**不**
// 回显凭证值。
// validateProviderEntry applies the candidate rules and names the offending key
// without echoing the credential value; api_key is env-only.
func validateProviderEntry(p ProviderEntry) error {
	if p.Name == "" || p.Name != strings.TrimSpace(p.Name) {
		return fmt.Errorf("%w: 字段 name 必须是非空且首尾无空白的字符串", ErrInvalidDefinition)
	}
	if p.BaseURL == "" {
		return fmt.Errorf("%w: 字段 base_url 必填", ErrInvalidDefinition)
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return fmt.Errorf("%w: 字段 base_url 不是合法 URL", ErrInvalidDefinition)
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return fmt.Errorf("%w: 字段 base_url 只接受 http / https", ErrInvalidDefinition)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: 字段 base_url 缺少主机名", ErrInvalidDefinition)
	}
	if u.User != nil {
		return fmt.Errorf("%w: 字段 base_url 不得包含 userinfo", ErrInvalidDefinition)
	}
	if p.DefaultModel == "" || p.DefaultModel != strings.TrimSpace(p.DefaultModel) {
		return fmt.Errorf("%w: 字段 default_model 必须是非空且首尾无空白的字符串", ErrInvalidDefinition)
	}
	mode, err := providerCredentialMode(p)
	if err != nil {
		return err
	}
	if mode == providerCredentialModeManaged {
		if strings.TrimSpace(p.APIKey) != "" {
			return fmt.Errorf("%w: 字段 api_key 在 credential_mode: managed 下不得出现（凭据存于凭据表）",
				ErrInvalidDefinition)
		}
		return nil
	}
	if !providerPlaceholder.MatchString(p.APIKey) {
		return fmt.Errorf("%w: 字段 api_key 必须是精确 ${VAR} 占位（不接受明文、${}、嵌套或带前缀的值）", ErrInvalidDefinition)
	}
	return nil
}

// providerEntryExists 判定条目名是否已在块内出现（调用方已保证定义合法，故重名不会出现）。
// providerEntryExists reports whether the name already exists in the block.
func providerEntryExists(entries []BlockEntry, name string) bool {
	for i := range entries {
		if entries[i].Name == name {
			return true
		}
	}
	return false
}

// renderYAMLScalar 输出 YAML 标量：安全时原样，否则双引号转义（防止值里的换行
// 或 `#` 改变文档结构）。provider 条目、AGENT.md frontmatter 值共用这一个渲染器。
// renderYAMLScalar emits a plain scalar when safe, otherwise a quoted one.
func renderYAMLScalar(s string) string {
	if plainYAMLScalarSafe(s) {
		return s
	}
	return yamlQuote(s)
}

// plainYAMLScalarSafe 判定一个值能否不加引号输出：拒绝空值、首尾空白、行首指示符、
// `: ` / 行尾 `:` / ` #`、控制字符与 YAML 的换行类码点。这是保守的**输出**判据，
// 不作为解析依据。
// plainYAMLScalarSafe is a conservative output check, not a parser rule.
func plainYAMLScalarSafe(s string) bool {
	if s == "" || strings.TrimSpace(s) != s {
		return false
	}
	if strings.ContainsAny(s[:1], "-?:,[]{}#&*!|>'\"%@`") {
		return false
	}
	if strings.Contains(s, ": ") || strings.HasSuffix(s, ":") || strings.Contains(s, " #") {
		return false
	}
	for _, r := range s {
		switch r {
		case 0x85, 0x2028, 0x2029, 0xfeff:
			return false
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// CommitProviderDefinition 把整份候选定义（调用方已在块编辑阶段拼好）校验后原子写回：
// 校验覆盖全文件而非单条目——发布与撤回改写的是既有块，块外内容若被破坏必须在这里被拒。
// 校验失败**不写文件**（调用方据此映射 400 且文件逐字节不变）；写入复用同目录临时文件 +
// fsync + rename，失败保留原文件。本函数不持有任何锁，串行由调用方（每定义文件提交锁）负责。
// CommitProviderDefinition validates a whole candidate definition and writes it
// atomically. Validation failures leave the file byte-identical; locking is the
// caller's job.
func CommitProviderDefinition(path string, doc []byte) error {
	if _, err := ValidateProviderEntries(doc); err != nil {
		return err
	}
	return writeProviderFile(path, doc)
}

// writeProviderFile 原子写定义文件：同目录临时文件 → Sync → Close → rename；
// 任一步失败都清理临时文件并保留原文件。
// writeProviderFile writes through a sibling temp file and renames it into place;
// every failure path removes the temp file and leaves the original untouched.
func writeProviderFile(path string, data []byte) error {
	return writeProviderFileWith(path, data, os.Rename)
}

// writeProviderFileWith 是原子写主体；rename 可注入，供失败路径测试。
// writeProviderFileWith is the injectable body used by the failure-path test.
func writeProviderFileWith(path string, data []byte, rename func(oldpath, newpath string) error) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return fmt.Errorf("建临时文件: %w", err)
	}
	tmp := f.Name()
	discard := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		discard()
		return fmt.Errorf("写临时文件: %w", err)
	}
	if err := f.Sync(); err != nil {
		discard()
		return fmt.Errorf("同步临时文件: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("关闭临时文件: %w", err)
	}
	if err := rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换定义文件 %s: %w", base, err)
	}
	return nil
}
