package cli

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/ymedword/basalt/internal/core"
)

// Config 是 basalt.yaml 的解析结果。
// Config is the parsed form of basalt.yaml.
//
// 已钉死键的段用类型化结构承载；键集尚未落位的段（author）以原始结构保留——
// 消费它们的 US 落位前不预造键（颗粒度文档 §3.1 的「解析并保留」口径）。sandbox / shell /
// http 三段自 US-5 起落为结构（§3.5 第 4 项：**沿用既有键名**，不新增也不删除）；
// memory 段自 US-6 起落为结构（§3.5 第 1 项——`backend` 是 `basalt.yaml.example` 里
// 已有的键，零新增配置键）。
// Sections whose keys are fixed carry typed structures; sections whose key sets
// are not yet decided keep their raw decoded shape so that no key is invented
// ahead of the US that consumes it. Only author still keeps its raw shape.
type Config struct {
	HTTP      HTTPConfig       `yaml:"http"`
	Log       LogConfig        `yaml:"log"`
	Providers []ProviderConfig `yaml:"providers"`
	Budget    BudgetConfig     `yaml:"budget"`
	Sandbox   SandboxConfig    `yaml:"sandbox"`
	Shell     ShellConfig      `yaml:"shell"`
	Memory    MemoryConfig     `yaml:"memory"`
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

	// AllowedDomains 是出网域名白名单（大小写不敏感）；空 = 一个都不放行。
	// 键名出处 docs/Operations.md §2.2，**消费归 US-5** 的 http_get / http_post——
	// 本 US 只把键从原始结构搬进类型化字段（消费者未落位不等于键可以晚配：
	// 配置键的形状一变，运维侧就得改文件）。
	// AllowedDomains is the outbound domain allowlist, consumed by US-5's network tools.
	AllowedDomains []string `yaml:"allowed_domains"`
}

// SandboxConfig 是沙箱授权判定的配置面（`sandbox` 段）。
// SandboxConfig is the sandbox section.
//
// 语义读者是 `WhitelistPolicy`（授权判定只看"谁、要碰什么"）：本结构只承载取值，
// 判定规则不在这里——配置装载与策略求值分开，改规则不必改装载。
// The values live here; the rules live in the policy that consumes them.
type SandboxConfig struct {
	// CrossAgentRead 是允许被**跨 Agent 读取**的 Agent 名清单；空 = 一个都不许
	// （未授权即拒，拒绝文案点名本键——FR-011）。
	// CrossAgentRead lists the agents readable across agent boundaries; empty denies all.
	CrossAgentRead []string `yaml:"cross_agent_read"`
}

// ShellConfig 是受控子进程执行的配置面（`shell` 段）——三个键都是白名单，空即全拒。
// ShellConfig is the controlled-subprocess section; every key is an allowlist.
type ShellConfig struct {
	// AllowedCommands 是命令白名单：executable **整串逐字**比对（多词串因此天然不命中）；
	// 给 `bash` / `sh` 放行被硬性禁止——写进这里是配置错误，不是授权（在策略侧硬拒）。
	// AllowedCommands matches the executable byte for byte; bash and sh are refused
	// by the policy regardless of what the operator writes here.
	AllowedCommands []string `yaml:"allowed_commands"`

	// Interpreters 是受控解释器通道：只接受 `[脚本路径, 参数…]` 形态（argv 校验在执行侧）。
	// Interpreters is the controlled interpreter channel; argv shape is checked at execution.
	Interpreters []string `yaml:"interpreters"`

	// EnvPassthrough 是透传给子进程的环境变量名白名单——不写进来的变量一律不继承
	// （代理与凭证类变量因此默认不进子进程，FR-012）。
	// EnvPassthrough names the variables handed to child processes; everything else,
	// proxy and credential variables included, is dropped by default.
	EnvPassthrough []string `yaml:"env_passthrough"`
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

// MemoryConfig 是长期记忆的配置面（`memory` 段，US-6 落位）。
// MemoryConfig is the memory section, landed by US-6.
//
// 只有一个键：载体档位。取值域只有两档，由 validateConfig 判卷（fail-closed）；
// 空串（不写 memory 段 / 写了段省略 backend 两条路径）在装载期**就地归一到 markdown**
// ——缺省只有一个来源，下游读到的一定是两档之一，不必各自再写一遍缺省分支。
// The only key is the carrier backend; the empty value is normalized to the default
// at load time so downstream code never re-decides what "unset" means.
type MemoryConfig struct {
	// Backend 取 markdown（缺省，人可读可手改）/ sqlite（可并发可查询）；
	// 其余取值（含 mem0）拒绝启动并点名键与取值。
	// Backend is markdown (default) or sqlite; anything else fails closed.
	Backend string `yaml:"backend"`
}

// 凭证键路径：只有 providers[i].api_key 是 US-1 认得的凭证键（其余凭证形态——
// mem0 api-key、notify_channels url——随各自消费 US 落位）。
// Credential key paths: providers[i].api_key is the only credential US-1 knows.
var credentialKey = regexp.MustCompile(`^providers\[\d+\]\.api_key$`)

// 单个占位符；${} 与 ${1X} 这类畸形形态不匹配，会在「残留检查」处被判红。
var placeholder = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// 日志级别与形态的合法取值——非法取值一律拒绝，不静默取默认。
var (
	validLogLevels  = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	validLogFormats = map[string]bool{"text": true, "json": true}
)

// defaultMemoryBackend 是 memory.backend 的唯一缺省档（约束 18：不写 memory 段
// 与省略 backend 两条路径读到同一个值）。同址声明缺省值与取值域——两者是同一份
// 契约的两面，分开写会漂移。
// defaultMemoryBackend and validMemoryBackends are declared together: the default
// and the domain are two faces of one contract.
const defaultMemoryBackend = "markdown"

// validMemoryBackends 是载体档位的取值域——两档之外（mem0 / 任意未知取值）一律
// 拒绝启动：静默降级会让「我配了 mem0」悄悄变成「记忆写进了文件」，载体形态与
// 运维预期分叉。
var validMemoryBackends = map[string]bool{"markdown": true, "sqlite": true}

// LoadConfig 装载并校验 basalt.yaml。
// LoadConfig loads and validates basalt.yaml.
//
// 校验一律 fail-closed：变量不存在、凭证解析为空、模板畸形、凭证明文、必填缺失、
// 类型非法——六类任一命中即拒绝，绝不静默取默认值。错误文案点名配置键路径
// （如 providers[0].api_key）与变量名，但**不得**回显凭证值（需求文档 §5.2）；
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
// （CWE-22 文件包含），而抑制手段（gosec 的 nosec 标注）默认禁用。以父目录为 root 读取既消除
// 该判定，又真把「跳到目录之外的软连接」挡在门外——fail-closed，与开发操作 §7
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
	// memory.backend：**先归一后判域**——空串（不写 memory 段 / 写了段省略 backend）
	// 是合法输入，在此就地写回缺省档：缺省只有一个来源，装配层读到的一定是两档之一
	// （那里的 default 分支因此只是纵深防御，不是日常路径）。
	if cfg.Memory.Backend == "" {
		cfg.Memory.Backend = defaultMemoryBackend
	}
	if !validMemoryBackends[cfg.Memory.Backend] {
		// 取值域外 fail-closed（约束 18）：mem0 载体延后实现（Compatibility §4 #2），
		// 未知取值同样拒绝——**不得**静默降级到 markdown，否则载体形态与运维预期分叉。
		return fmt.Errorf(
			"配置项 memory.backend 取值非法（%s）——只允许 markdown（缺省）或 sqlite；mem0 载体尚未实现，未知取值一律拒绝启动，不静默降级",
			cfg.Memory.Backend)
	}
	// seen 记条目名 → **首次出现**的下标：重名拒绝要同时点名两条，只报后一条会让
	// 用户不知道跟谁撞了。
	// seen maps a name to its first index so a duplicate can name both entries.
	seen := make(map[string]int, len(cfg.Providers))
	for i, p := range cfg.Providers {
		if p.Name == "" {
			return fmt.Errorf("必填配置项 providers[%d].name 缺失——供应商条目必须有名字（映射表的键）", i)
		}
		if first, dup := seen[p.Name]; dup {
			// 第 7 类 fail-closed：重名不拒绝，映射表就会静默保留最后一条——前面的
			// 条目从此不可达，而失败形态只是"配了却路由不过去"，没有任何报错。
			// A duplicate would leave one entry silently unreachable.
			return fmt.Errorf(
				"配置项 providers[%d].name 与 providers[%d] 重复（%q）——映射表的键必须唯一，否则重名的条目静默不可达",
				first, i, p.Name)
		}
		seen[p.Name] = i

		// base_url 自本 US 起必填：没有地址就构造不出客户端（端点 = base_url + /chat/completions）。
		if p.BaseURL == "" {
			return fmt.Errorf("必填配置项 providers[%d].base_url 缺失——无地址无法构造客户端", i)
		}
		if err := validateProviderBaseURL(p.BaseURL, i); err != nil {
			return err
		}

		if p.APIKey == "" {
			return fmt.Errorf("必填配置项 providers[%d].api_key 缺失——凭证一律写 ${VAR} 占位", i)
		}
	}
	return nil
}

// validateProviderBaseURL 是第 8 类 fail-closed：`base_url` 只支持 scheme + host +
// path 三部分，含 userinfo / query / fragment 的取值**拒绝**（缺失 scheme 不代补；
// 非 http(s) 方案同拒——两种情形文案各说各的，原因不同不混写）。
// validateProviderBaseURL rejects every base_url carrying a component outside
// scheme + host + path; it never rewrites the user's configuration.
//
// 为什么拒绝而不是静默裁剪：删掉这些部分会**改写用户配置的含义**——userinfo 里可能
// 就是认证信息、query 里可能带着网关路由参数；裁剪把认证 / 路由错误推迟成运行时才
// 暴露、且难定位的故障。
// Silently dropping them would rewrite the configuration's meaning and turn an
// authentication or routing error into a hard-to-place runtime failure.
//
// 文案纪律：只点名 `providers[i].base_url` 与不支持的**成分名**，**不回显原 URL、
// 也不回显其中的值**（userinfo 里可能就是一份凭证）——`url.Parse` 的原始错误自带
// 整条 URL，**不得**直传。
// The message names the key path and the offending component only, never the URL
// or any of its values.
func validateProviderBaseURL(raw string, i int) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		// 原错误直传会把整条 URL 带进文案——这里只保留"解析不了"这个事实。
		return fmt.Errorf(
			"配置项 providers[%d].base_url 无法解析——只支持 scheme + host + path（不支持的成分: scheme）", i)
	}
	switch {
	case parsed.Scheme == "":
		return fmt.Errorf(
			"配置项 providers[%d].base_url 只支持 scheme + host + path（不支持的成分: scheme）——缺失 scheme 不代补 https", i)
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		// 与上一条分开：方案存在但不受支持时，说"缺失 scheme"是错的事实陈述——
		// 文案只点成分名 + 支持面，不描述被拒的具体取值（取值来自配置，不外带）。
		return fmt.Errorf(
			"配置项 providers[%d].base_url 只支持 scheme + host + path（不支持的成分: scheme）——只接受 http 与 https 方案", i)
	case parsed.Host == "":
		return fmt.Errorf(
			"配置项 providers[%d].base_url 只支持 scheme + host + path（不支持的成分: host）", i)
	case parsed.User != nil:
		return fmt.Errorf(
			"配置项 providers[%d].base_url 只支持 scheme + host + path（不支持的成分: userinfo）", i)
	case parsed.RawQuery != "" || parsed.ForceQuery:
		return fmt.Errorf(
			"配置项 providers[%d].base_url 只支持 scheme + host + path（不支持的成分: query）", i)
	case parsed.Fragment != "" || strings.Contains(raw, "#"):
		// 尾随的空 `#`（`…/v1#`）解析后 Fragment 是**空串**，只看成分字段会漏——而拼接端点后
		// `/chat/completions` 整段落进 fragment，请求实际打到 base path（静默改写端点）。
		// `url.URL` 没有 ForceFragment 字段（query 侧才有 ForceQuery），故按字面 `#` 判；
		// 路径里的 `%23` 是合法编码，不含字面 `#`，不连坐。
		return fmt.Errorf(
			"配置项 providers[%d].base_url 只支持 scheme + host + path（不支持的成分: fragment）", i)
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

	if _, isRef := core.EnvPlaceholderName(raw); credential && !isRef {
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
