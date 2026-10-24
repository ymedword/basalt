package workspace

// 本文件是 mcp_servers.yaml 的解析与形态校验（US-7；US-12 按约束 8 扩展）。
// 条目字段 name / transport / command / args / url / env；列表顺序原样保留
// （= 连接序 = 注册序）。两条读取路径共用同一校验核心，仅以 allowLegacy 区分：
//   - 既有 mcp_servers.yaml（兼容）：缺省 transport 按 stdio 读入；未带 args 的
//     多词 command 按 strings.Fields 拆分并 WARN（与旧 tool 层行为逐字等价）；
//   - 管理面候选（严格，ValidateMCPServerCandidate）：transport 必须显式给出，
//     多词 command 直接拒绝并指引用 args 字段。
//
// 本层**不读环境变量**——`env` 值只校验是否为精确 `${VAR}` 占位，解析（占位 → 终值）
// 归装配层 `app.resolveMCPEnv`（已决 #2；技术方案 §3.7）。
// This file parses and validates mcp_servers.yaml. The file path is legacy-tolerant
// (missing transport means stdio, a multi-word command is split with a WARN); the
// admin candidate path is strict and shares the same validation core. This layer
// never reads the environment: env values are only checked for the exact ${VAR}
// placeholder form; resolution belongs to the assembly layer.

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/ymedword/basalt/internal/core"
)

const (
	mcpTransportStdio = "stdio"
	mcpTransportHTTP  = "http"
)

// ParseMCPServers 读取工作区根部的 mcp_servers.yaml 并产出 server 配置列表。
// 文件缺失 / 空文件 / 纯注释 / `{}` / 无 servers 键 / `servers: null` → 零 server、
// 零错误（整段跳过由装配层负责，FR-002）；其余形态问题一律 fail-closed 拒载。
// 既有文件走兼容路径：缺省 transport = stdio，旧多词 command 拆分并 WARN（约束 8 / 47）。
// ParseMCPServers reads the workspace-root mcp_servers.yaml. The six zero-server
// shapes yield an empty slice and a nil error; every other shape failure is fatal.
// Existing files take the legacy path: an omitted transport means stdio and a
// multi-word command is split with a WARN.
func ParseMCPServers(root string) ([]core.MCPServerConfig, error) {
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() { _ = base.Close() }() // 根句柄只读；关闭失败无后续动作可做
	return parseMCPServers(base)
}

// parseMCPServers 是解析主体（持有根句柄，读取在根上被挡软连接越界）；既有文件
// 一律按兼容路径读入。
// parseMCPServers is the parsing body working on the workspace root handle; the
// on-disk configuration always takes the legacy-tolerant path.
func parseMCPServers(base *os.Root) ([]core.MCPServerConfig, error) {
	return parseMCPServersWith(base, true)
}

// parseMCPServersWith 解析文件并按 allowLegacy 决定旧格式 command 的处置；
// 单条目的语义校验统一交给 normalizeMCPServer（与候选校验共用）。
// parseMCPServersWith parses the file and delegates per-entry semantics to
// normalizeMCPServer, the same validation core used by candidates.
func parseMCPServersWith(base *os.Root, allowLegacy bool) ([]core.MCPServerConfig, error) {
	raw, err := base.ReadFile(mcpServersFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // 缺失 = 零 server
		}
		return nil, fmt.Errorf("读取 %s: %w", mcpServersFile, err)
	}
	return parseMCPServersBytes(raw, allowLegacy)
}

// parseMCPServersBytes 是 mcp_servers.yaml 的纯字节解析体：文件读取与整份候选校验
// （CommitMCPServersDefinition）共用，保证"写入能通过 = 下次读取能通过"。
// parseMCPServersBytes is the pure-bytes parsing body shared by the file reader and
// whole-document candidate validation.
func parseMCPServersBytes(raw []byte, allowLegacy bool) ([]core.MCPServerConfig, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil // 空文件
	}

	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("解析 %s: %s", mcpServersFile, frontmatterErrorSummary(err))
	}
	if tree == nil {
		return nil, nil // `null` 文档 / 纯注释（注释不产生节点）
	}
	mapping, ok := tree.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s 的顶层必须是映射（servers: 列表），实际是 %T", mcpServersFile, tree)
	}
	rawList, present := mapping["servers"]
	if !present || rawList == nil {
		return nil, nil // 兼容 US-1 模板的 `{}` 与省略 servers 的文件
	}
	items, ok := rawList.([]any)
	if !ok {
		return nil, fmt.Errorf("%s 的 servers 必须是列表，实际是 %T", mcpServersFile, rawList)
	}

	out := make([]core.MCPServerConfig, 0, len(items))
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s 的 servers[%d] 必须是映射，实际是 %T", mcpServersFile, i, item)
		}
		candidate, err := mcpConfigFromEntry(entry, i)
		if err != nil {
			return nil, err
		}
		if seen[candidate.Name] {
			return nil, fmt.Errorf("%s 的 server 名重复: %s", mcpServersFile, candidate.Name)
		}
		seen[candidate.Name] = true
		normalized, err := normalizeMCPServer(candidate, i, mcpServersFile, allowLegacy)
		if err != nil {
			return nil, err
		}
		out = append(out, normalized)
	}
	return out, nil
}

// ValidateMCPServerCandidate 以**严格**模式校验并规范化一条管理面候选配置（约束 8）：
// transport 必须显式给出（stdio / http）；stdio 必须有单个可执行名的 command、不得有
// url；http 必须有 http / https 且无 userinfo 的 url、不得有 command / args；env 值只能
// 是精确 `${VAR}`；args 不得承载明文凭证。与既有文件读取共用 normalizeMCPServer。
// ValidateMCPServerCandidate applies the strict candidate rules and returns the
// normalized configuration; it shares normalizeMCPServer with the file reader.
func ValidateMCPServerCandidate(cfg core.MCPServerConfig) (core.MCPServerConfig, error) {
	return normalizeMCPServer(cfg, -1, "MCP 候选", false)
}

// ValidateMCPServersDocument 校验整份 mcp_servers.yaml 候选（发布 / 删除前改写用）：
// 走与文件读取相同的兼容取径，覆盖未触碰的旧条目——编辑一条不能因旁邻历史条目
// 被连带拒绝；被编辑的那条已由 RenderMCPServerEntry 的严格规范化保证。
// ValidateMCPServersDocument validates a whole candidate mcp_servers.yaml with the
// same legacy-tolerant pass as the file reader, so untouched old entries survive.
func ValidateMCPServersDocument(raw []byte) error {
	if _, err := parseMCPServersBytes(raw, true); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidDefinition, err)
	}
	return nil
}

// CommitMCPServersDefinition 校验整份候选并原子写回 mcp_servers.yaml：校验失败
// **不写文件**（调用方据此映射 400 且磁盘逐字节不变）；写入复用 provider 定义文件的
// 同目录临时文件 + fsync + rename。本函数不持锁，串行由调用方的每定义文件提交锁负责。
// CommitMCPServersDefinition validates the whole candidate and writes it atomically;
// locking stays with the caller's per-definition-file commit lock.
func CommitMCPServersDefinition(path string, doc []byte) error {
	if err := ValidateMCPServersDocument(doc); err != nil {
		return err
	}
	return writeProviderFile(path, doc)
}

// ParseMCPServerBlock 把一份已发布版本的 `definition_yaml` 解析回**恰一条**配置：
// 接受两种已落库形态——块式列表片段（管理写路径存的就是 `BlockEntry.Raw`，带 `- `
// 指示符）与顶层条目映射（基线 / 对账路径剥掉指示符后的逐行保真块）。
//
// 读取走**兼容**路径（约束 8 / 47）：存储字节可能来自旧格式文件，多词 command 要按
// 既有规则拆分并 WARN——发布 / 重启不能因为一条历史条目拒绝整个定义。语义校验
// 与文件读取共用 normalizeMCPServer；失败统一包 `ErrInvalidDefinition`，由调用方
// 把它当"存储定义非法"（500 / 启动拒绝），而不是用户请求错误。
// ParseMCPServerBlock parses one published definition back into exactly one
// configuration. Both stored shapes are accepted, and the legacy-tolerant path keeps
// old multi-word commands working across publish and restart.
func ParseMCPServerBlock(raw []byte) (core.MCPServerConfig, error) {
	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return core.MCPServerConfig{}, fmt.Errorf("%w: MCP 定义不可解析", ErrInvalidDefinition)
	}
	var entry map[string]any
	switch value := tree.(type) {
	case map[string]any:
		entry = value
	case []any:
		if len(value) != 1 {
			return core.MCPServerConfig{}, fmt.Errorf(
				"%w: MCP 定义必须恰含一个条目（实际 %d）", ErrInvalidDefinition, len(value))
		}
		mapping, ok := value[0].(map[string]any)
		if !ok {
			return core.MCPServerConfig{}, fmt.Errorf("%w: MCP 定义的条目必须是映射", ErrInvalidDefinition)
		}
		entry = mapping
	default:
		return core.MCPServerConfig{}, fmt.Errorf("%w: MCP 定义必须是条目映射或单条目列表", ErrInvalidDefinition)
	}
	candidate, err := mcpConfigFromEntry(entry, 0)
	if err != nil {
		return core.MCPServerConfig{}, fmt.Errorf("%w: %s", ErrInvalidDefinition, err)
	}
	normalized, err := normalizeMCPServer(candidate, -1, "MCP 发布版本", true)
	if err != nil {
		return core.MCPServerConfig{}, fmt.Errorf("%w: %s", ErrInvalidDefinition, err)
	}
	return normalized, nil
}

// RenderMCPServerEntry 渲染单条 MCP 条目为定义文件里的块式列表项：键序固定
// `name` / `transport` / `command` / `args` / `url` / `env`，缩进两格（与
// RenderProviderEntry 同一排版）。只输出该传输形态的字段——stdio 不出 url、http
// 不出 command / args，避免"再读一次会不会变成另一条配置"的歧义。env 值在这里
// 恒为精确 `${VAR}` 占位（候选校验已保证），**不**解析环境变量。
// RenderMCPServerEntry renders one entry in the fixed key order with the field set of
// its transport; env values stay exact ${VAR} placeholders.
func RenderMCPServerEntry(cfg core.MCPServerConfig) ([]byte, error) {
	normalized, err := normalizeMCPServer(cfg, -1, "MCP 候选", false)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("  - name: " + renderYAMLScalar(normalized.Name) + "\n")
	b.WriteString("    transport: " + normalized.Transport + "\n")
	switch normalized.Transport {
	case mcpTransportStdio:
		b.WriteString("    command: " + renderYAMLScalar(normalized.Command) + "\n")
		if len(normalized.Args) > 0 {
			b.WriteString("    args:\n")
			for _, arg := range normalized.Args {
				b.WriteString("      - " + renderYAMLScalar(arg) + "\n")
			}
		}
	case mcpTransportHTTP:
		b.WriteString("    url: " + renderYAMLScalar(normalized.URL) + "\n")
	}
	if len(normalized.Env) > 0 {
		keys := make([]string, 0, len(normalized.Env))
		for key := range normalized.Env {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		b.WriteString("    env:\n")
		for _, key := range keys {
			b.WriteString("      " + renderYAMLScalar(key) + ": " + renderYAMLScalar(normalized.Env[key]) + "\n")
		}
	}
	return []byte(b.String()), nil
}

// mcpConfigFromEntry 把 YAML 条目映射转成原始配置：只做必填与类型检查，
// 传输分支、互斥与占位等语义校验统一交给 normalizeMCPServer。
// mcpConfigFromEntry extracts the raw configuration from one YAML entry; semantic
// checks belong to normalizeMCPServer.
func mcpConfigFromEntry(entry map[string]any, index int) (core.MCPServerConfig, error) {
	name := stringField(entry, "name")
	if strings.TrimSpace(name) == "" {
		return core.MCPServerConfig{}, fmt.Errorf("%s 的 servers[%d] 缺少 name", mcpServersFile, index)
	}
	cfg := core.MCPServerConfig{Name: name, Command: stringField(entry, "command"), URL: stringField(entry, "url")}
	// transport 字段一旦出现必须先过类型检查：非字符串经字符串强转会变成空串、
	// 再按缺省 stdio 放行——等于让非法配置悄悄获得执行含义。`null` 视同缺省（与 env 同口径）。
	// A present transport must be a string before its value is examined; coercing a
	// non-string to "" would silently grant the stdio default execution meaning.
	if rawTransport, hasTransport := entry["transport"]; hasTransport && rawTransport != nil {
		typedTransport, ok := rawTransport.(string)
		if !ok {
			return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 的 transport 必须是字符串，实际是 %T", mcpServersFile, name, rawTransport)
		}
		cfg.Transport = typedTransport
	}
	if rawArgs, hasArgs := entry["args"]; hasArgs && rawArgs != nil {
		list, ok := rawArgs.([]any)
		if !ok {
			return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 的 args 必须是列表，实际是 %T", mcpServersFile, name, rawArgs)
		}
		for i, item := range list {
			text, ok := item.(string)
			if !ok {
				return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 的 args[%d] 必须是字符串，实际是 %T", mcpServersFile, name, i, item)
			}
			cfg.Args = append(cfg.Args, text)
		}
	}
	if value, hasEnv := entry["env"]; hasEnv && value != nil {
		envMap, ok := value.(map[string]any)
		if !ok {
			return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 的 env 必须是映射", mcpServersFile, name)
		}
		cfg.Env = make(map[string]string, len(envMap))
		for key, rawValue := range envMap {
			placeholder, ok := rawValue.(string)
			if !ok {
				// 错误只点名字段，**永不回显**配置值——值可能正是凭证（FR-004 / R1a）
				return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 的 env.%s 必须是 ${VAR} 占位（明文凭证不得入库）", mcpServersFile, name, key)
			}
			cfg.Env[key] = placeholder
		}
	}
	return cfg, nil
}

// normalizeMCPServer 是文件读取与候选校验共用的语义校验核心（约束 8）：
// 传输分支的必填 / 互斥、command 单可执行名、args 凭证、env 精确占位。
// `index >= 0` 表示来自文件条目（错误文案带 servers[i]）；`prefix` 是错误文案的
// 来源前缀。allowLegacy 只影响多词 command：兼容路径按首个空白拆分并 WARN，
// 严格路径直接拒绝。
// normalizeMCPServer is the shared validation core. allowLegacy only affects a
// multi-word command: the legacy path splits it with a WARN, the strict path
// rejects it and points at the args field.
func normalizeMCPServer(cfg core.MCPServerConfig, index int, prefix string, allowLegacy bool) (core.MCPServerConfig, error) {
	name := cfg.Name
	if strings.TrimSpace(name) == "" {
		if index >= 0 {
			return core.MCPServerConfig{}, fmt.Errorf("%s 的 servers[%d] 缺少 name", prefix, index)
		}
		return core.MCPServerConfig{}, fmt.Errorf("%s 的 name 必填", prefix)
	}

	transport := strings.TrimSpace(cfg.Transport)
	if transport == "" {
		if !allowLegacy {
			return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 必须显式给出 transport（stdio / http）", prefix, name)
		}
		transport = mcpTransportStdio
	}
	if transport != mcpTransportStdio && transport != mcpTransportHTTP {
		return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 的 transport 不受支持（%s）——只支持 stdio / http", prefix, name, transport)
	}

	command := strings.TrimSpace(cfg.Command)
	args := append([]string(nil), cfg.Args...)
	urlText := strings.TrimSpace(cfg.URL)

	switch transport {
	case mcpTransportStdio:
		if urlText != "" {
			return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 是 stdio 传输，不得给出 url", prefix, name)
		}
		if command == "" {
			return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 缺少 command", prefix, name)
		}
		if fields := strings.Fields(command); len(fields) > 1 {
			if len(args) > 0 {
				return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 同时给出 args 与多词 command——参数只能保留在一处", prefix, name)
			}
			if !allowLegacy {
				return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 的 command 必须是单个可执行名——请把参数移入 args 字段", prefix, name)
			}
			command, args = fields[0], fields[1:]
			// 只记可执行名与参数个数：旧格式 command 可能夹带明文凭证，**不得**整体回显。
			// Log argv[0] and the arg count only; the legacy command may embed a credential.
			slog.Default().Warn("MCP 旧格式多词 command 已按空白拆分——请迁移到 args 字段",
				"file", prefix, "server", name, "command", command, "args", len(args))
		}
	case mcpTransportHTTP:
		if command != "" {
			return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 是 http 传输，不得给出 command", prefix, name)
		}
		if len(args) > 0 {
			return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 是 http 传输，不得给出 args", prefix, name)
		}
		if urlText == "" {
			return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 是 http 传输，缺少 url", prefix, name)
		}
		if err := validateMCPURL(urlText); err != nil {
			return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 的 url %w", prefix, name, err)
		}
	}

	if err := validateMCPArgs(args); err != nil {
		return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 的 %w", prefix, name, err)
	}
	for key, placeholder := range cfg.Env {
		if _, ok := core.EnvPlaceholderName(placeholder); !ok {
			return core.MCPServerConfig{}, fmt.Errorf("%s 的 server %s 的 env.%s 必须是 ${VAR} 占位（明文凭证不得入库）", prefix, name, key)
		}
	}

	return core.MCPServerConfig{
		Name:      name,
		Transport: transport,
		Command:   command,
		Args:      args,
		URL:       urlText,
		Env:       cfg.Env,
	}, nil
}

// validateMCPURL 只接受 http / https、有主机名且无 userinfo 的绝对 URL；
// 错误文案固定，**不回显** URL（可能夹带凭证）。
// validateMCPURL accepts only absolute http(s) URLs with a host and no userinfo;
// the error text never echoes the URL.
func validateMCPURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("必须是不含 userinfo 的 http / https 绝对地址")
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return errors.New("必须是不含 userinfo 的 http / https 绝对地址")
	}
	if u.Host == "" || u.User != nil {
		return errors.New("必须是不含 userinfo 的 http / https 绝对地址")
	}
	return nil
}

// mcpCredentialFlags 是 args 里被视为承载凭证的开关名（小写比较）。
// mcpCredentialFlags lists the argument flags treated as credential carriers.
var mcpCredentialFlags = map[string]bool{
	"--token":         true,
	"-token":          true,
	"--api-key":       true,
	"--apikey":        true,
	"--key":           true,
	"--secret":        true,
	"--password":      true,
	"--passwd":        true,
	"--auth":          true,
	"--authorization": true,
	"--bearer-token":  true,
}

// validateMCPArgs 拒绝以明文承载凭证的参数：`--flag=value` 与 `--flag value` 两种形态
// 的值都必须是精确 `${VAR}` 占位。错误文案只点名参数序号与开关，**不回显**取值。
// validateMCPArgs rejects plaintext credentials in args; the error text names the
// argument index and the flag but never echoes the value.
func validateMCPArgs(args []string) error {
	for i, arg := range args {
		flag, value, hasValue := strings.Cut(arg, "=")
		if hasValue {
			if !mcpCredentialFlags[strings.ToLower(strings.TrimSpace(flag))] {
				continue
			}
			if _, ok := core.EnvPlaceholderName(value); ok {
				continue
			}
			return fmt.Errorf("args[%d]（%s=…）的取值必须是 ${VAR} 占位——明文凭证不得写入 args", i, strings.TrimSpace(flag))
		}
		if !mcpCredentialFlags[strings.ToLower(strings.TrimSpace(arg))] {
			continue
		}
		if i+1 < len(args) {
			if _, ok := core.EnvPlaceholderName(args[i+1]); !ok {
				return fmt.Errorf("args[%d]（%s 的取值）必须是 ${VAR} 占位——明文凭证不得写入 args", i, arg)
			}
		}
	}
	return nil
}

// mcpServerNames 取配置列表的 server 名序列（保持配置序——装载器规则 4/6 按声明序
// 报首个失败名，顺序是判据的一部分，**不得**排序）。
// mcpServerNames returns the server names in configuration order; sorting would
// change the first-failure reporting of loader rules 4 and 6.
func mcpServerNames(specs []core.MCPServerConfig) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

// stringField 取映射里的字符串字段；缺失或非字符串一律按空串处理（空串在调用点
// 各自对应一条点名错误）。
// stringField returns a string field or "" when absent or of another type.
func stringField(entry map[string]any, key string) string {
	value, _ := entry[key].(string)
	return value
}
