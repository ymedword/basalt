// Package catalog 提供只读 MCP 目录（颗粒度文档 §3.1.1）：数据以 go:embed 随二进制，
// `Load` 是纯字节解析 + 校验——不读工作区、不写文件、不访问网络、不启动进程。
// 目录是"配方"不是运行配置：本包不替换 args 里的 `<...>` 编辑标记，不安装任何包，
// 也不产生管理操作行。
// Package catalog serves the read-only MCP catalog embedded in the binary. Load is a
// pure parser and validator; recipes are advice, not live configuration — edit
// markers are never substituted and nothing is installed.
package catalog

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/ymedword/basalt/internal/core"
)

const (
	transportStdio = "stdio"
	transportHTTP  = "http"
)

var (
	// idPattern 是 kebab-case：小写字母 / 数字，单词间单个连字符。
	// idPattern is the kebab-case rule for catalog ids.
	idPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	// envPlaceholderPattern 抓取 `${KEY}` 里的键名，用来断言与 env[].key 一致。
	// envPlaceholderPattern captures the key inside ${KEY} for the equality check.
	envPlaceholderPattern = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
)

// catalogWarningCodes 是目录 warnings 的固定闭集（契约 McpCatalogEntry.warnings）。
// 与诊断 warnings（D-10）各自固定、不合并（Q-04，2026-10-25）；新增 code 必须同时
// 更新契约、前端文案与测试。
// catalogWarningCodes is the fixed catalog warning set; it stays separate from the
// diagnosis set and every new code updates the contract, the console copy and tests.
var catalogWarningCodes = map[string]bool{
	"read_only_tool_face_required":     true,
	"server_write_capability":          true,
	"write_tool_name_collision":        true,
	"browser_required":                 true,
	"sqlite_sample_only":               true,
	"package_entry_unverified":         true,
	"stdio_child_egress_unconstrained": true,
}

// verifiedPlatformCodes 是 `verified_platforms` 的闭集：只写具体平台，不写 `all`。
// verifiedPlatformCodes is the closed platform set; "all" is never a value.
var verifiedPlatformCodes = map[string]bool{
	"windows": true,
	"linux":   true,
	"darwin":  true,
}

// catalogDocument 是目录 YAML 的解码形态：只含两个顶层键，未知键由严格解码拒绝。
// catalogDocument is the decode shape; strict decoding refuses unknown keys.
type catalogDocument struct {
	Version int            `yaml:"version"`
	Items   []catalogEntry `yaml:"items"`
}

// catalogEntry 与 `core.MCPCatalogEntry` 逐字段对应（core 不带 yaml tag，解码形态
// 留在解析包内，与 workspace 的 DTO 惯例一致）。
// catalogEntry mirrors core.MCPCatalogEntry; the decode shape stays in the parser
// package, matching the workspace DTO convention.
type catalogEntry struct {
	ID                string       `yaml:"id"`
	DisplayName       string       `yaml:"display_name"`
	Summary           string       `yaml:"summary"`
	Category          string       `yaml:"category"`
	Transport         string       `yaml:"transport"`
	Command           string       `yaml:"command"`
	Args              []string     `yaml:"args"`
	URL               string       `yaml:"url"`
	Env               []catalogEnv `yaml:"env"`
	Requires          []string     `yaml:"requires"`
	SourceURL         string       `yaml:"source_url"`
	Verified          bool         `yaml:"verified"`
	VerificationNote  string       `yaml:"verification_note"`
	VerifiedAt        string       `yaml:"verified_at"`
	VerifiedPlatforms []string     `yaml:"verified_platforms"`
	Warnings          []string     `yaml:"warnings"`
}

// catalogEnv 是一条 env 占位声明；只声明键名，不携带值。
// catalogEnv declares one env placeholder and never carries a value.
type catalogEnv struct {
	Key         string `yaml:"key"`
	Placeholder string `yaml:"placeholder"`
	Required    bool   `yaml:"required"`
	Description string `yaml:"description"`
}

// Load 解析并校验整份目录，产出按 `id` 升序的只读快照。校验规则逐条对应颗粒度
// 文档 §3.1.1：版本 / id 唯一且 kebab-case / transport 闭集与传输互斥 / env 占位
// 与键一致且不重复 / source_url 必须 https / verified 四件套自洽 / warnings 闭集。
// 另外要求条目文案与 `requires` 非空——契约把 16 个字段全部列为 required，且
// `requires` 至少一条。空数组一律规范化为非 nil 切片，保证视图恒输出 `[]`。
// Load parses and validates the whole catalog and returns an id-sorted snapshot. The
// rules mirror module card §3.1.1; empty arrays are normalized so views always emit [].
func Load(raw []byte) (core.MCPCatalog, error) {
	var doc catalogDocument
	if err := yaml.UnmarshalWithOptions(raw, &doc, yaml.Strict()); err != nil {
		// 目录是编译期嵌入的固定数据：解析失败只能是数据文件写错，直接 fail-closed。
		// The catalog is embedded at build time, so a parse failure is a data defect.
		return core.MCPCatalog{}, fmt.Errorf("解析 MCP 目录: %v", err)
	}
	if doc.Version <= 0 {
		return core.MCPCatalog{}, fmt.Errorf("MCP 目录的 version 必须为正整数，实际是 %d", doc.Version)
	}

	seen := make(map[string]bool, len(doc.Items))
	items := make([]core.MCPCatalogEntry, 0, len(doc.Items))
	for index, entry := range doc.Items {
		converted, err := entry.toCore(index)
		if err != nil {
			return core.MCPCatalog{}, err
		}
		if seen[converted.ID] {
			return core.MCPCatalog{}, fmt.Errorf("MCP 目录条目 %s 的 id 重复", converted.ID)
		}
		seen[converted.ID] = true
		items = append(items, converted)
	}
	// 排序在解析层完成一次，视图层不再排序（契约 McpCatalog.items：按 id 升序）。
	// Sorting happens here once; views never re-order.
	slices.SortStableFunc(items, func(a, b core.MCPCatalogEntry) int {
		return strings.Compare(a.ID, b.ID)
	})
	return core.MCPCatalog{Version: doc.Version, Items: items}, nil
}

// toCore 校验一条目录条目并转成领域类型。错误文案点名条目（有 id 用 id，否则用
// 序号）与具体字段，不回显整段 YAML。
// toCore validates one entry and converts it; errors name the entry and the field.
func (e catalogEntry) toCore(index int) (core.MCPCatalogEntry, error) {
	label := fmt.Sprintf("MCP 目录条目[%d]", index)
	if id := strings.TrimSpace(e.ID); id != "" {
		label = fmt.Sprintf("MCP 目录条目 %s", id)
	}
	fail := func(format string, args ...any) (core.MCPCatalogEntry, error) {
		return core.MCPCatalogEntry{}, fmt.Errorf("%s %s", label, fmt.Sprintf(format, args...))
	}

	id := strings.TrimSpace(e.ID)
	if !idPattern.MatchString(id) {
		return fail("的 id 必须是非空 kebab-case（小写字母 / 数字 / 单个连字符）")
	}
	displayName := strings.TrimSpace(e.DisplayName)
	if displayName == "" {
		return fail("缺少 display_name")
	}
	summary := strings.TrimSpace(e.Summary)
	if summary == "" {
		return fail("缺少 summary")
	}
	category := strings.TrimSpace(e.Category)
	if category == "" {
		return fail("缺少 category")
	}

	transport := strings.TrimSpace(e.Transport)
	command := strings.TrimSpace(e.Command)
	rawURL := strings.TrimSpace(e.URL)
	switch transport {
	case transportStdio:
		if command == "" {
			return fail("是 stdio 传输，必须给出 command")
		}
		if rawURL != "" {
			return fail("是 stdio 传输，不得给出 url")
		}
	case transportHTTP:
		if command != "" {
			return fail("是 http 传输，不得给出 command")
		}
		if len(e.Args) > 0 {
			return fail("是 http 传输，不得给出 args")
		}
		if err := validateHTTPSURL(rawURL); err != nil {
			return fail("的 url %s", err.Error())
		}
	default:
		return fail("的 transport 不受支持——只允许 stdio / http")
	}

	envKeys := make(map[string]bool, len(e.Env))
	env := make([]core.MCPCatalogEnv, 0, len(e.Env))
	for envIndex, item := range e.Env {
		key := strings.TrimSpace(item.Key)
		if key == "" {
			return fail("的 env[%d] 缺少 key", envIndex)
		}
		if envKeys[key] {
			return fail("的 env 键 %s 重复", key)
		}
		envKeys[key] = true
		placeholder := strings.TrimSpace(item.Placeholder)
		match := envPlaceholderPattern.FindStringSubmatch(placeholder)
		if match == nil || match[1] != key {
			return fail("的 env.%s 的 placeholder 必须恰为 ${%s}", key, key)
		}
		env = append(env, core.MCPCatalogEnv{
			Key:         key,
			Placeholder: placeholder,
			Required:    item.Required,
			Description: strings.TrimSpace(item.Description),
		})
	}

	requires := make([]string, 0, len(e.Requires))
	for requireIndex, require := range e.Requires {
		text := strings.TrimSpace(require)
		if text == "" {
			return fail("的 requires[%d] 为空", requireIndex)
		}
		requires = append(requires, text)
	}
	if len(requires) == 0 {
		return fail("的 requires 至少要有一条依赖说明")
	}

	if err := validateHTTPSURL(strings.TrimSpace(e.SourceURL)); err != nil {
		return fail("的 source_url %s", err.Error())
	}

	warnings := make([]string, 0, len(e.Warnings))
	for warnIndex, warning := range e.Warnings {
		code := strings.TrimSpace(warning)
		if !catalogWarningCodes[code] {
			return fail("的 warnings[%d] 不在目录固定集合内（%q）", warnIndex, code)
		}
		warnings = append(warnings, code)
	}

	note := strings.TrimSpace(e.VerificationNote)
	verifiedAt := strings.TrimSpace(e.VerifiedAt)
	platforms := make([]string, 0, len(e.VerifiedPlatforms))
	for platformIndex, platform := range e.VerifiedPlatforms {
		code := strings.TrimSpace(platform)
		if !verifiedPlatformCodes[code] {
			return fail("的 verified_platforms[%d] 不受支持——只允许 windows / linux / darwin", platformIndex)
		}
		platforms = append(platforms, code)
	}
	if e.Verified {
		if verifiedAt == "" {
			return fail("已标记 verified=true，必须给出 verified_at")
		}
		if len(platforms) == 0 {
			return fail("已标记 verified=true，必须给出 verified_platforms")
		}
	} else {
		// 未实测的条目不得留"看起来已验证"的痕迹：说明必填，时间与平台必须为空。
		// Unmeasured entries must not look verified: a note is required, a date is not.
		if note == "" {
			return fail("标记 verified=false 时必须给出 verification_note（写清待实测项）")
		}
		if verifiedAt != "" {
			return fail("标记 verified=false 时 verified_at 必须为空")
		}
		if len(platforms) != 0 {
			return fail("标记 verified=false 时 verified_platforms 必须为空数组")
		}
	}

	args := make([]string, 0, len(e.Args))
	args = append(args, e.Args...)

	return core.MCPCatalogEntry{
		ID:                id,
		DisplayName:       displayName,
		Summary:           summary,
		Category:          category,
		Transport:         transport,
		Command:           command,
		Args:              args,
		URL:               rawURL,
		Env:               env,
		Requires:          requires,
		SourceURL:         strings.TrimSpace(e.SourceURL),
		Verified:          e.Verified,
		VerificationNote:  note,
		VerifiedAt:        verifiedAt,
		VerifiedPlatforms: platforms,
		Warnings:          warnings,
	}, nil
}

// validateHTTPSURL 只接受 https 且有主机名的绝对地址；错误文案固定，不回显 URL。
// validateHTTPSURL accepts only absolute https URLs with a host; the error text never
// echoes the URL.
func validateHTTPSURL(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("必须是 https 绝对地址（当前为空）")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || strings.ToLower(parsed.Scheme) != "https" || parsed.Host == "" {
		return fmt.Errorf("必须是带主机名的 https 绝对地址")
	}
	if parsed.User != nil {
		return fmt.Errorf("必须是带主机名的 https 绝对地址（不得含 userinfo）")
	}
	return nil
}
