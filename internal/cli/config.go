package cli

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
)

// Config 是 basalt.yaml 的解析结果。
// Config is the parsed form of basalt.yaml.
//
// 已钉死键的段用类型化结构承载；键集尚未落位的段（sandbox / shell / memory /
// author）以原始结构保留——消费它们的 US 落位前不预造键（颗粒度文档 §3.1 的
// 「解析并保留」口径）。
// Sections whose keys are fixed carry typed structures; sections whose key sets
// are not yet decided keep their raw decoded shape so that no key is invented
// ahead of the US that consumes it.
type Config struct {
	HTTP      HTTPConfig       `yaml:"http"`
	Log       LogConfig        `yaml:"log"`
	Providers []ProviderConfig `yaml:"providers"`
	Budget    BudgetConfig     `yaml:"budget"`
	Sandbox   map[string]any   `yaml:"sandbox"`
	Shell     map[string]any   `yaml:"shell"`
	Memory    map[string]any   `yaml:"memory"`
	Author    map[string]any   `yaml:"author"`
}

// HTTPConfig 是服务段的监听与请求体上限（字面量出处 docs/contracts/openapi.yaml）。
// HTTPConfig is the service section: listen address and request body cap.
type HTTPConfig struct {
	// Addr 是监听地址；安全默认是本机，远程部署必须显式改。
	// Addr is the listen address; the safe default is loopback.
	Addr string `yaml:"addr"`

	// MaxRequestBytes 是请求体总上限（缺省 1 MiB，解析前拒绝）。
	// MaxRequestBytes caps the whole request body, enforced before parsing.
	MaxRequestBytes int64 `yaml:"max_request_bytes"`
}

// LogConfig 是日志档位——本 US 唯一消费的配置（键名经 2026-10-15 用户裁决：
// 沿用全仓 `<段>.<键>` 惯例）。
// LogConfig is the log profile, the only section US-1 consumes.
type LogConfig struct {
	// Level 取 debug / info / warn / error；空 = info。
	// Level is one of debug, info, warn, error; empty means info.
	Level string `yaml:"level"`

	// Format 取 text（dev 可读）/ json（prod 结构化）；空 = text。
	// Format is text (dev) or json (prod); empty means text.
	Format string `yaml:"format"`
}

// ProviderConfig 是单个供应商条目。
// ProviderConfig is a single provider entry.
type ProviderConfig struct {
	// Name 是映射表的键——装配处据此一次构建显式映射（宪法第 3 条）。
	// Name keys the explicit provider-to-client map built at assembly time.
	Name string `yaml:"name"`

	// BaseURL 是兼容端点。
	// BaseURL is the OpenAI-compatible endpoint.
	BaseURL string `yaml:"base_url"`

	// APIKey 只允许 ${VAR} 占位；解析结果绝不会出现在错误文案或日志里。
	// APIKey accepts a ${VAR} placeholder only, and never appears in errors or logs.
	APIKey string `yaml:"api_key"`

	// DefaultModel 是该供应商的缺省模型（可被 Profile 覆盖）。
	// DefaultModel is the provider default model.
	DefaultModel string `yaml:"default_model"`
}

// BudgetConfig 是运维侧的预算上限——模型只能缩短、不能放大。
// BudgetConfig holds the operator-defined budgets; a model may shorten but
// never enlarge them.
type BudgetConfig struct {
	MaxIterations  int   `yaml:"max_iterations"`
	MaxToolCalls   int   `yaml:"max_tool_calls"`
	MaxOutputBytes int64 `yaml:"max_output_bytes"`
	ContextTokens  int   `yaml:"context_tokens"`
}

// 凭证键路径：只有 providers[i].api_key 是 US-1 认得的凭证键（其余凭证形态——
// mem0 api-key、notify_channels url——随各自消费 US 落位）。
// Credential key paths: providers[i].api_key is the only credential US-1 knows.
var credentialKey = regexp.MustCompile(`^providers\[\d+\]\.api_key$`)

// 完整占位形态：凭证原始值必须整体是一个 ${VAR}，前后不得夹带明文。
var wholePlaceholder = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// 单个占位符；${} 与 ${1X} 这类畸形形态不匹配，会在「残留检查」处被判红。
var placeholder = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// 日志级别与形态的合法取值——非法取值一律拒绝，不静默取默认。
var (
	validLogLevels  = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	validLogFormats = map[string]bool{"text": true, "json": true}
)

// LoadConfig 装载并校验 basalt.yaml。
// LoadConfig loads and validates basalt.yaml.
//
// 校验一律 fail-closed：变量不存在、凭证解析为空、模板畸形、凭证明文、必填缺失、
// 类型非法——六类任一命中即拒绝，绝不静默取默认值。错误文案点名配置键路径
// （如 providers[0].api_key）与变量名，但**不得**回显凭证值（需求 §5.13）；
// 解析 / 解码失败同样只报行列与类别，库的源码摘录一律不外泄（见 yamlErrorSummary）。
// Validation is fail-closed on six classes: missing variable, empty credential,
// malformed placeholder, plaintext credential, missing required field and bad
// type. Messages name the key path and the variable, never the value.
//
// 文件缺失时返回的错误可用 errors.Is(err, fs.ErrNotExist) 判定——是否容忍由调用方
// 决定（status 容忍并明示路径，不计失败）。
// A missing file is reported so that callers can test errors.Is(err, fs.ErrNotExist).
func LoadConfig(path string, lookupEnv func(string) (string, bool)) (*Config, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	raw, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}

	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		// 此处刻意不 %w 原错误：库的默认错误文本带源码摘录，保留链条等于保留
		// 泄露路径（凭证所在行的原文会被带进 stdout / stderr / 日志）。
		return nil, fmt.Errorf("解析配置 %s: %s", path, yamlErrorSummary(err))
	}

	root, ok := asMapping(tree)
	if !ok && tree != nil {
		return nil, fmt.Errorf("配置 %s 的顶层必须是映射（键值对），实际是 %T", path, tree)
	}

	resolved, err := resolveTree(root, "", lookupEnv)
	if err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}

	resolvedMap, ok := resolved.(map[string]any)
	if !ok {
		// 顶层映射经 resolveTree 后仍是映射——走到这里说明实现被改坏了。
		return nil, fmt.Errorf("配置 %s: 内部错误：解析结果顶层不是映射（%T）", path, resolved)
	}

	cfg, err := decodeConfig(resolvedMap)
	if err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("配置 %s: %w", path, err)
	}
	return cfg, nil
}

// readConfigFile 读取配置文件内容——读取范围经 os.Root 钉在配置文件所在目录。
// readConfigFile reads the configuration file, scoping the read through os.Root
// to the directory the file lives in.
//
// 为什么不用裸 os.ReadFile(path)：路径来自调用方参数，静态安全扫描对此判 G304
// （CWE-22 文件包含），而抑制手段（`#nosec`）默认禁用。以父目录为 root 读取既消除
// 该判定，又真把「跳到目录之外的软连接」挡在门外——fail-closed，与编程指南 §7
// 陷阱 7 的真实路径口径同向。错误经 %w 保留 fs.ErrNotExist 链，LoadConfig 对
// 调用方的判态契约（errors.Is(err, fs.ErrNotExist)）不变（已实测）。
// A read through the parent directory is both gate-clean and fail-closed; the
// %w keeps the fs.ErrNotExist chain intact for callers.
func readConfigFile(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()

	raw, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		// os.Root 的错误只带基名——补回完整路径，缺失时调用方能明示路径。
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	return raw, nil
}

// decodeConfig 把解析后的树解成类型化配置——类型非法（如 log.level 写成数字）
// 在这一步拒绝。
// decodeConfig maps the resolved tree onto the typed configuration; type errors
// (a numeric log.level, say) are rejected here.
//
// 两处 yaml 错误都经 yamlErrorSummary 压成「位置 + 类别」并再抹一遍凭证值：
// 这一步的输入是**解析后**的树（凭证已是真实值），库的默认错误文本会把它当源码
// 摘录原样回显——实测：providers[0].default_model 类型非法时，解析后的 api_key
// 值出现在错误摘录里并流进 stdout / stderr。%w 同样刻意省略（理由同 LoadConfig）。
// Both paths render through yamlErrorSummary and re-redact credential values:
// the input here holds resolved secrets, which the library would echo verbatim.
func decodeConfig(tree map[string]any) (*Config, error) {
	buf, err := yaml.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("内部错误：解析结果无法回写: %s",
			redactCredentialValues(yamlErrorSummary(err), tree))
	}
	cfg := new(Config)
	if err := yaml.Unmarshal(buf, cfg); err != nil {
		return nil, fmt.Errorf("配置项类型非法: %s",
			redactCredentialValues(yamlErrorSummary(err), tree))
	}
	return cfg, nil
}

// yamlErrorSummary 把 YAML 库的错误压成「[行:列] 自产类别」，供对外错误使用。
// yamlErrorSummary renders a YAML library error as position plus a locally
// produced category.
//
// 为什么连库的**首行文案**也不能用：库文案本身会带出错处的原文——未定义别名的
// 错误带别名名（实测：`api_key: *SENTINEL` 把 SENTINEL 送进 stdout / stderr）、
// 重复映射键带所重复的键、溢出错误带源数字。故这里的口径是**库文案一个字都不
// 外传**：位置取自错误的**结构化字段**（yaml.Error 接口的 GetToken → 行列），
// 类别由具体错误类型自产（类型名是代码事实，不是配置数据）。
// The library message itself can quote the offending source (alias names,
// duplicated keys, numeric literals), so nothing of it leaves this package:
// the position comes from the structured token, the category from the error type.
func yamlErrorSummary(err error) string {
	category := yamlErrorCategory(err)
	var yamlErr yaml.Error
	if errors.As(err, &yamlErr) {
		if tk := yamlErr.GetToken(); tk != nil {
			return fmt.Sprintf("[%d:%d] %s", tk.Position.Line, tk.Position.Column, category)
		}
	}
	return category
}

// yamlErrorCategory 由错误**类型**给出安全类别——这些字面量全部是本包自产的
// 固定文案，不含任何来自配置的数据。库没有给出可识别类型时用兜底类别，同样
// 不携带原文。
// yamlErrorCategory maps the concrete error type to a fixed local label; the
// messages never carry any configuration data.
func yamlErrorCategory(err error) string {
	var (
		syntaxErr     *yaml.SyntaxError
		typeErr       *yaml.TypeError
		overflowErr   *yaml.OverflowError
		dupKeyErr     *yaml.DuplicateKeyError
		unknownFldErr *yaml.UnknownFieldError
		unexpectedErr *yaml.UnexpectedNodeTypeError
	)
	switch {
	case errors.As(err, &syntaxErr):
		return "YAML 语法错误"
	case errors.As(err, &typeErr):
		return "配置项类型非法"
	case errors.As(err, &overflowErr):
		return "配置项数值溢出"
	case errors.As(err, &dupKeyErr):
		return "映射键重复"
	case errors.As(err, &unknownFldErr):
		return "配置项含未知字段"
	case errors.As(err, &unexpectedErr):
		return "配置节点类型不符"
	default:
		return "YAML 解析失败"
	}
}

// redactCredentialValues 把文案里出现的凭证值替换为占位——纵深防御：即便上游库
// 某天改回带值的错误形态，凭证也不会经错误路径外泄。
// redactCredentialValues scrubs resolved credential values from a message.
func redactCredentialValues(msg string, tree map[string]any) string {
	for _, value := range credentialValues(tree) {
		msg = strings.ReplaceAll(msg, value, "<已隐去>")
	}
	return msg
}

// credentialValues 收集解析结果里凭证键的值（键路径口径与 resolveTree 一致——
// 同一套层级生长规则，凭证键判定才能落在同一批键上）。
// credentialValues collects the values under credential key paths.
func credentialValues(tree map[string]any) []string {
	var values []string
	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				childPath := k
				if path != "" {
					childPath = path + "." + k
				}
				if credentialKey.MatchString(childPath) {
					if value, ok := child.(string); ok && value != "" {
						values = append(values, value)
					}
					continue
				}
				walk(child, childPath)
			}
		case []any:
			for i, child := range v {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(tree, "")
	return values
}

// validateConfig 做取值域与必填校验（类型校验已由解码完成）。
// validateConfig checks value domains and required fields.
func validateConfig(cfg *Config) error {
	if cfg.Log.Level != "" && !validLogLevels[cfg.Log.Level] {
		return fmt.Errorf("配置项 log.level 取值非法（%s）——只允许 debug / info / warn / error", cfg.Log.Level)
	}
	if cfg.Log.Format != "" && !validLogFormats[cfg.Log.Format] {
		return fmt.Errorf("配置项 log.format 取值非法（%s）——只允许 text / json", cfg.Log.Format)
	}
	for i, p := range cfg.Providers {
		if p.Name == "" {
			return fmt.Errorf("必填配置项 providers[%d].name 缺失——供应商条目必须有名字（映射表的键）", i)
		}
		if p.APIKey == "" {
			return fmt.Errorf("必填配置项 providers[%d].api_key 缺失——凭证一律写 ${VAR} 占位", i)
		}
	}
	return nil
}

// resolveTree 递归解析 ${VAR} 占位，路径随层级生长（providers[0].api_key）。
// resolveTree walks the decoded tree and resolves ${VAR} placeholders, growing
// the key path as it descends so errors can name the exact location.
//
// 凭证键路径在此先过**类型关**（必须字符串）再进 resolveString 的**形态关**
// （必须整体 ${VAR}）——两关都在解码器上游，数字 / 布尔绕不过去。
// Credential paths pass a type gate here and a shape gate in resolveString, both
// ahead of the decoder.
func resolveTree(node any, path string, lookupEnv func(string) (string, bool)) (any, error) {
	if credentialKey.MatchString(path) {
		raw, ok := node.(string)
		if !ok {
			// 类型关必须在解析**之前**：数字 / 布尔等会经解码器被转成字符串，
			// 连占位形态检查一起绕过（实测：完整工作区下 `api_key: 123456789`
			// 与 `api_key: true` 的 status 判 exit 0）。先要求"字符串"，再谈
			// 占位形态——两关都在解码器上游，明文凭证没有旁路。
			// The type check runs before decoding: numbers and booleans would be
			// coerced into strings by the decoder and bypass the placeholder
			// check entirely.
			return nil, fmt.Errorf(
				"配置项 %s 必须是 ${VAR} 形态的字符串（当前为 %s）——凭证不得写死，也不得以其他类型承载",
				path, nodeTypeName(node))
		}
		return resolveString(raw, path, lookupEnv)
	}
	switch v := node.(type) {
	case string:
		return resolveString(v, path, lookupEnv)
	case map[string]any:
		out := make(map[string]any, len(v))
		// 键序固定（排序后遍历）：同一份坏配置两次运行必须点名同一个键——map 遍历
		// 顺序在 Go 里是随机的，不排序时"先报到哪个缺失变量"每次都不同，排障的人
		// 会以为配置变了。解析本身与顺序无关，这里只为错误文案确定性。
		// Sorted traversal: resolution is order-independent, but which missing key
		// gets reported first must not change between runs.
		for _, k := range slices.Sorted(maps.Keys(v)) {
			child := v[k]
			childPath := k
			if path != "" {
				childPath = path + "." + k
			}
			rv, err := resolveTree(child, childPath, lookupEnv)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, child := range v {
			rv, err := resolveTree(child, fmt.Sprintf("%s[%d]", path, i), lookupEnv)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	default:
		return node, nil
	}
}

// resolveString 解析单个字符串值。
// resolveString resolves one string value.
//
// 凭证键先判形态再解析：原始值整体不是 ${VAR} 即明文，直接拒绝且不回显该值。
// 非凭证键允许「部分占位」形态（如 http://${HOST}:8080），解析为空不判失败。
// Credential keys are shape-checked first: anything that is not exactly one
// ${VAR} is plaintext and is rejected without echoing it.
func resolveString(raw, path string, lookupEnv func(string) (string, bool)) (any, error) {
	credential := credentialKey.MatchString(path)

	if credential && !wholePlaceholder.MatchString(raw) {
		return nil, fmt.Errorf(
			"配置项 %s 是明文凭证——凭证一律写 ${VAR} 占位并经环境变量提供", path)
	}

	var missing string
	resolved := placeholder.ReplaceAllStringFunc(raw, func(token string) string {
		name := token[2 : len(token)-1]
		value, ok := lookupEnv(name)
		if !ok {
			// 记录第一个缺失变量即可——「空串替换」不能绕过（lookupEnv 判的是存在性）。
			if missing == "" {
				missing = name
			}
			return token
		}
		return value
	})
	if missing != "" {
		return nil, fmt.Errorf(
			"配置项 %s 引用的环境变量 %s 不存在——凭证与占位值必须由环境变量提供，不得写死", path, missing)
	}
	if strings.Contains(resolved, "${") {
		return nil, fmt.Errorf(
			"配置项 %s 解析后仍残留 ${ 占位（模板畸形）——检查占位符是否闭合、变量名是否合法", path)
	}
	if credential && resolved == "" {
		return nil, fmt.Errorf("配置项 %s 解析为空——凭证不得为空值", path)
	}
	return resolved, nil
}

// nodeTypeName 给出节点类型名供错误文案使用——只报类型（%T 的代码事实），
// 不回显值本身；空值也要有人话可读。
// nodeTypeName names the node type for error messages without echoing the value.
func nodeTypeName(node any) string {
	if node == nil {
		return "空值"
	}
	return fmt.Sprintf("%T", node)
}

// asMapping 把解码结果规整成映射；空文档（nil）视为空配置。
// asMapping normalizes the decoded root into a mapping; an empty document is an
// empty configuration.
func asMapping(tree any) (map[string]any, bool) {
	if tree == nil {
		return map[string]any{}, true
	}
	m, ok := tree.(map[string]any)
	return m, ok
}
