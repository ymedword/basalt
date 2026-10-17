package tool

import (
	"context"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// 工作区骨架的目录字面量（与 `basalt init` 落盘的骨架一致）。本包按字面量自行定位，
// **不**反向依赖 `workspace` 包——装配期由 `app` 传入工作区根即可。
// Workspace layout literals; the tool package never imports the workspace package.
const (
	agentsDirName       = "agents"
	skillsDirName       = "skills"
	memoryDirName       = "memory"
	scriptsDirName      = "scripts"
	agentDefinitionFile = "AGENT.md"
)

// 拒绝文案的固定片段（四段式的段 1 规则 / 段 4 放行指引；段 2 是具体目标、段 3 由
// `core.SandboxViolationError` 拼死句）。这两段是**模型可见契约**：改名等于换契约，
// 措辞只在"该档确实有配置键可加"时分叉（contracts/agent-md.md §6）。
// Frozen rule and guidance fragments of the model-visible refusal copy.
const (
	ruleReadOutside           = "读取目标不在可读根之内"
	guidanceReadOutside       = "可读根固定为当前 Agent 自己的目录与 skills 库（工作区约定），没有配置键可以加。"
	ruleCrossAgent            = "跨 Agent 读取未授权"
	guidanceCrossAgent        = "如需授权，请联系运维把该 Agent 名加入配置键 sandbox.cross_agent_read 并重启服务。"
	ruleAgentName             = "Agent 名不合法"
	guidanceAgentName         = "Agent 名取自工作区目录名（单层名字，不含路径分隔符），没有配置键可以加。"
	ruleUnresolvable          = "目标路径无法解析为真实路径"
	guidanceUnresolvable      = "路径中存在悬空软链接 / 链接成环 / 不可进入的中间目录；请改指工作区内的普通路径。"
	ruleUnknownAction         = "动作类型不在授权矩阵内"
	guidanceUnknownAction     = "只有文件读 / 文件写 / 命令 / 出网四类动作可被判定；请检查工具实现传入了什么。"
	ruleProtectedFile         = "AGENT.md 是受保护文件，写入被拒绝"
	guidanceProtectedFile     = "Agent 定义经页面编辑器与脚手架（同一道注册写入门）修改，没有配置键可以加。"
	ruleScriptsReadOnly       = "scripts/ 是只读区，写入被拒绝"
	guidanceScriptsReadOnly   = "脚本受控通道的前提是模型改不动脚本（代码常量），没有配置键可以加。"
	ruleSkillsReadOnly        = "技能库不可写入"
	guidanceSkillsReadOnly    = "技能库由管理员在 skills/ 下维护（代码常量），没有配置键可以加。"
	ruleMemoryNotWritable     = "记忆目录不在可写范围"
	guidanceMemoryNotWritable = "记忆的写入归记忆工具，文件工具不改写 memory/（代码常量），没有配置键可以加。"
	ruleWriteOutside          = "写入目标超出当前 Agent 的可写范围"
	guidanceWriteOutside      = "写范围固定为当前 Agent 自己的目录（代码常量），没有配置键可以加。"
	ruleParentMissing         = "目标父目录不存在"
	guidanceParentMissing     = "write_file 不自动创建父目录；请改在已存在的目录内写入。"
	ruleMultiWord             = "可执行名含空白（多词串）"
	guidanceMultiWord         = "executable 与 arguments 必须分开传（不经 shell 解释）：可执行名写单个可执行文件，参数走 arguments。"
	ruleBashSh                = "bash / sh 不得放行"
	guidanceBashSh            = "需要跑脚本请改用受控解释器通道（配置键 shell.interpreters，脚本须落在该 Agent 的 scripts/ 或技能库内）并重启服务。"
	ruleCommandNotAllowed     = "可执行名不在白名单"
	guidanceCommandNotAllowed = "如需放行，请联系运维把该可执行名加入配置键 shell.allowed_commands 并重启服务。"
	ruleNotDomain             = "出网目标不是域名"
	guidanceNotDomain         = "白名单按域名匹配，IP 字面量与无法解析的地址一律拒；没有配置键可以加。"
	ruleScheme                = "出网 scheme 不被允许"
	guidanceScheme            = "只允许 http / https（代码固定，配置放行不了）；没有配置键可以加。"
	ruleDomainNotAllowed      = "域名不在白名单"
	guidanceDomainNotAllowed  = "如需访问，请联系运维把该域名加入配置键 http.allowed_domains 并重启服务。"
	// 端口档（口径 15，2026-10-17 用户裁决冻结）：只允许 scheme 的默认端口；放行通道
	// 归扩展阶段——指引**不得**指向域名白名单（它管 host、不管端口）。
	ruleNonDefaultPort     = "出网目标端口不在默认档"
	guidanceNonDefaultPort = "当前版本不支持非默认端口（http 用 80、https 用 443，或省略端口）；没有配置键可以加。"
	// 段 2 的安全文案：目标不可点名时（URL 解析失败 / 缺 host）**不得**回显原文——原文可能
	// 带 userinfo 与 query（§3.5 第 9 项脱敏）。
	targetUnresolvable = "（目标无法解析）"
)

// PolicyOptions 是 WhitelistPolicy 的构造参数——**键名逐字沿用** `basalt.yaml`
// （§2.4 注 6：沿用已有契约键、不另造），列表缺省空 = 一个都不许（安全默认）。
// PolicyOptions mirrors the existing configuration keys verbatim; empty lists deny by
// default.
type PolicyOptions struct {
	Root            string   // 工作区根（`.basalt`）；agents/、skills/、memory/ 相对它定位
	CrossAgentRead  []string // sandbox.cross_agent_read：允许被跨 Agent 读取的 Agent 名
	AllowedCommands []string // shell.allowed_commands：executable 整串逐字比对
	Interpreters    []string // shell.interpreters：受控解释器通道（argv 形态由执行侧校验）
	AllowedDomains  []string // http.allowed_domains：出网域名白名单（大小写不敏感）
}

// WhitelistPolicy 是 v1 唯一的授权实现（技术方案 §4.3 授权矩阵 + §2.3 流程 F）：
// 只看"谁、要碰什么"——`Action` 没有 argv 字段，形态类校验属执行侧（`LocalRunner`）。
// WhitelistPolicy authorizes by "who touches what" only; shape checks belong to the runner.
type WhitelistPolicy struct {
	root            string
	crossAgentRead  map[string]struct{}
	allowedCommands map[string]struct{}
	interpreters    map[string]struct{}
	allowedDomains  []string
}

// 编译期断言：调用方只依赖 `core.Policy`，换实现只动装配处（宪法 VI）。
// Compile-time assertion that callers depend on the interface, not this type.
var _ core.Policy = (*WhitelistPolicy)(nil)

// NewWhitelistPolicy 装配白名单策略；列表逐条建集合表（显式面，不做前缀 / 通配推断）。
// NewWhitelistPolicy wires the policy; the lists become explicit lookup tables.
func NewWhitelistPolicy(opts PolicyOptions) *WhitelistPolicy {
	policy := &WhitelistPolicy{
		root:            filepath.Clean(opts.Root),
		crossAgentRead:  make(map[string]struct{}, len(opts.CrossAgentRead)),
		allowedCommands: make(map[string]struct{}, len(opts.AllowedCommands)),
		interpreters:    make(map[string]struct{}, len(opts.Interpreters)),
		allowedDomains:  slices.Clone(opts.AllowedDomains),
	}
	for _, name := range opts.CrossAgentRead {
		policy.crossAgentRead[name] = struct{}{}
	}
	for _, name := range opts.AllowedCommands {
		policy.allowedCommands[name] = struct{}{}
	}
	for _, name := range opts.Interpreters {
		policy.interpreters[name] = struct{}{}
	}
	return policy
}

// Check 判定一个动作是否放行；拒绝一律返回 `*core.SandboxViolationError`（四段式文案，
// 由 `ToolExecutor` 原样回填并分类为 `sandbox_denied`）。ctx 参与接口签名供实现演进
// （取消由工具自身在执行前判定），本实现不因 ctx 改变判定结论。
// Check answers "allowed?"; refusals are SandboxViolationError values that travel into
// the tool result verbatim. ctx does not change the verdict here.
func (p *WhitelistPolicy) Check(ctx context.Context, action core.Action) error {
	switch action.Type {
	case core.ActionFileRead:
		return p.checkFile(action, accessRead)
	case core.ActionFileWrite:
		return p.checkFile(action, accessWrite)
	case core.ActionShellCommand:
		return p.checkShellCommand(action)
	case core.ActionHTTPRequest:
		return p.checkHTTPRequest(action)
	default:
		// 未知动作类型**失败关闭**：新增动作要先过矩阵，不能默许通过。
		return deny(ruleUnknownAction, string(action.Type), guidanceUnknownAction)
	}
}

// fileAccess 区分读 / 写两条放行序（写档的保护规则**先行**，读档没有保护面）。
type fileAccess bool

const (
	accessRead  fileAccess = false
	accessWrite fileAccess = true
)

// checkFile 落 §2.3 流程 F 的文件两档：先投影真实路径（投影不出来即拒），再按档判。
// checkFile follows flow F: project first (refusing on failure), then judge per kind.
func (p *WhitelistPolicy) checkFile(action core.Action, access fileAccess) error {
	if action.Agent != "" && !validAgentName(action.Agent) {
		return deny(ruleAgentName, action.Agent, guidanceAgentName)
	}
	real, err := resolveRealPath(p.root, action.Target)
	if err != nil {
		return deny(ruleUnresolvable, action.Target, guidanceUnresolvable)
	}
	ownDir := p.agentDir(action.Agent)
	if access == accessRead {
		return p.checkFileRead(action, real, ownDir)
	}
	return p.checkFileWrite(action, real, ownDir)
}

// checkFileRead 读档放行序：自己的目录（含 scripts/）→ 技能库 → 显式授权的跨 Agent。
// checkFileRead allows the agent's own directory, the skills library, and explicitly
// authorized cross-agent reads.
func (p *WhitelistPolicy) checkFileRead(action core.Action, real, ownDir string) error {
	if ownDir != "" && withinDir(real, ownDir) {
		return nil
	}
	if withinDir(real, p.skillsDir()) {
		return nil
	}
	if other, isCrossAgent := p.crossAgentTarget(real); isCrossAgent {
		if _, authorized := p.crossAgentRead[other]; authorized {
			return nil
		}
		// 未授权跨 Agent 是**授权问题**（有键可加），与"工作区之外"分开报——文案要点名配置键。
		return deny(ruleCrossAgent, action.Target, guidanceCrossAgent)
	}
	return deny(ruleReadOutside, action.Target, guidanceReadOutside)
}

// checkFileWrite 写档：**保护规则先于一般放行**——受保护路径多数落在自有目录之内，
// 顺序反了就等于让"在自有目录内"把保护面整体放过去。
// Protection rules precede the general write allow; reversing the order would let the
// "inside own directory" clause swallow the protected paths.
func (p *WhitelistPolicy) checkFileWrite(action core.Action, real, ownDir string) error {
	switch {
	case foldCase(filepath.Base(real)) == foldCase(agentDefinitionFile):
		return deny(ruleProtectedFile, action.Target, guidanceProtectedFile)
	case ownDir != "" && withinDir(real, filepath.Join(ownDir, scriptsDirName)):
		return deny(ruleScriptsReadOnly, action.Target, guidanceScriptsReadOnly)
	case withinDir(real, p.skillsDir()):
		return deny(ruleSkillsReadOnly, action.Target, guidanceSkillsReadOnly)
	case withinDir(real, filepath.Join(p.root, memoryDirName)):
		return deny(ruleMemoryNotWritable, action.Target, guidanceMemoryNotWritable)
	}
	if ownDir == "" || !withinDir(real, ownDir) {
		return deny(ruleWriteOutside, action.Target, guidanceWriteOutside)
	}
	info, statErr := os.Stat(filepath.Dir(real))
	if statErr != nil || !info.IsDir() {
		return deny(ruleParentMissing, action.Target, guidanceParentMissing)
	}
	return nil
}

// checkShellCommand 落 §2.3 流程 F 的命令档：bash / sh **硬拒先行**（契约 §5.2 的
// "不得放行"没有例外分支——先查白名单会让 `allowed_commands: [bash]` 这个配置错误变成放行），
// 随后多词串拒、白名单整串逐字比对、解释器通道放行到执行侧。
// The bash/sh hard refusal runs before any allow branch so that a misconfigured
// allowlist can never open the shell back up.
func (p *WhitelistPolicy) checkShellCommand(action core.Action) error {
	target := action.Target
	if isBannedShell(target) {
		return deny(ruleBashSh, target, guidanceBashSh)
	}
	if strings.ContainsAny(target, " \t") {
		return deny(ruleMultiWord, target, guidanceMultiWord)
	}
	if _, ok := p.allowedCommands[target]; ok {
		return nil
	}
	if _, ok := p.interpreters[target]; ok {
		// 放行**到执行侧**：argv 形态（`-c` 拒）与脚本真实路径越界由 LocalRunner 判。
		return nil
	}
	return deny(ruleCommandNotAllowed, target, guidanceCommandNotAllowed)
}

// checkHTTPRequest 落 §2.3 流程 F 的出网档：逐跳校验由调用方在每一跳发送前再次 Check；
// 跳数上限 5 是调用方的代码常量（US-5 的 http 工具），本档只判"这一跳去哪"。
// 判定序：解析 → scheme → hostname / IP 字面量 → **端口档（口径 15）** → 域名白名单；
// 初始请求与每一次重定向同判（判定只此一处）。两处拒绝分支只回安全文案与 host，
// **不得**回显 `action.Target` 原文（原文可能带 userinfo / query——§3.5 第 9 项）。
// Per-hop judgement; the redirect hop cap lives with the http tools (US-5).
func (p *WhitelistPolicy) checkHTTPRequest(action core.Action) error {
	u, err := url.Parse(action.Target)
	if err != nil {
		return deny(ruleNotDomain, targetUnresolvable, guidanceNotDomain)
	}
	// scheme 只认 http / https（**代码固定**：配置放行不了）。
	if u.Scheme != "http" && u.Scheme != "https" {
		return deny(ruleScheme, u.Scheme, guidanceScheme)
	}
	host := u.Hostname()
	// 白名单按域名匹配：IP 字面量目标直接拒（放行 IP 等于绕过白名单）。
	if host == "" {
		return deny(ruleNotDomain, targetUnresolvable, guidanceNotDomain)
	}
	if net.ParseIP(host) != nil {
		// 段 2 点名 IP 字面量本身——它不含 userinfo / query，回显它不泄漏任何东西。
		return deny(ruleNotDomain, host, guidanceNotDomain)
	}
	// 端口档（口径 15，冻结）：`http` 配省略端口或显式 `:80`、`https` 配省略端口或显式
	// `:443` 放行，其余组合（含交叉档 http:443 / https:80）一律拒——判定在 hostname 之后、
	// 域名白名单之前：域名档管 host、端口档管端口，两档不互相顶替。
	if port := u.Port(); port != "" {
		defaultPort, ok := defaultPortFor(u.Scheme)
		if !ok || port != strconv.Itoa(defaultPort) {
			// 段 2 点名 host 与端口（`u.Host` 不含 userinfo），**不含** query。
			return deny(ruleNonDefaultPort, u.Host, guidanceNonDefaultPort)
		}
	}
	if !p.domainAllowed(host) {
		return deny(ruleDomainNotAllowed, host, guidanceDomainNotAllowed)
	}
	return nil
}

// defaultPortFor 返回 scheme 的默认端口；不认识的 scheme 返回 false（调用方据此拒）。
// defaultPortFor reports the scheme's default port; unknown schemes report false.
func defaultPortFor(scheme string) (int, bool) {
	switch scheme {
	case "http":
		return 80, true
	case "https":
		return 443, true
	default:
		return 0, false
	}
}

// domainAllowed 按域名匹配（大小写不敏感）：完整域名逐字比较；条目写成 `*.example.com`
// （通配前缀）时匹配其子域——通配只放宽一个被明确写下的后缀，不做"包含即命中"的模糊匹配。
// Domain matching is case-insensitive; a `*.suffix` entry covers subdomains only.
func (p *WhitelistPolicy) domainAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, entry := range p.allowedDomains {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if suffix, ok := strings.CutPrefix(entry, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == entry {
			return true
		}
	}
	return false
}

// agentDir 定位 Agent 自有目录；名字不合法或为空（无 Agent 上下文）时返回空串——
// 调用方据此跳过"自有目录"分支（无上下文就没有自有目录，不是报错）。
// agentDir locates the agent's own directory; an empty or invalid name yields "".
func (p *WhitelistPolicy) agentDir(name string) string {
	if !validAgentName(name) {
		return ""
	}
	return filepath.Join(p.root, agentsDirName, name)
}

// skillsDir 是技能库根（`.basalt/skills`）——读放行、写一律拒（代码常量）。
// skillsDir is the read-only skills library root.
func (p *WhitelistPolicy) skillsDir() string {
	return filepath.Join(p.root, skillsDirName)
}

// crossAgentTarget 判定 real 是否落在**别的** Agent 目录内并取回那个 Agent 名
// （供授权表比对与文案点名）。名字按路径首段取，不做模糊匹配。
// crossAgentTarget reports whether real sits inside another agent's directory.
func (p *WhitelistPolicy) crossAgentTarget(real string) (string, bool) {
	agentsRoot := filepath.Join(p.root, agentsDirName)
	if !withinDir(real, agentsRoot) {
		return "", false
	}
	rel, err := filepath.Rel(foldCase(agentsRoot), foldCase(real))
	if err != nil {
		return "", false
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	if first == "" || first == "." {
		return "", false
	}
	return first, true
}

// validAgentName 只接受**单层名字**：Agent 名来自工作区目录名，含路径分隔符的形态
// 会让"自有目录"指向工作区外的任意位置（自造的越界入口）。
// Only a single path element is a legal agent name.
func validAgentName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return filepath.Base(name) == name
}

// isBannedShell 识别 bash / sh 的**所有调用形态**（裸名、完整路径、带可执行后缀）：
// 放行它们等于把 shell 解释层请回来，`-c` 内联代码就此绕过 scripts/ 只读区。
// Any spelling of bash / sh — bare, full path, or with an executable suffix — is banned.
func isBannedShell(target string) bool {
	base := foldCase(filepath.Base(target))
	if base == "bash" || base == "sh" {
		return true
	}
	trimmed := strings.TrimSuffix(base, filepath.Ext(base))
	return trimmed == "bash" || trimmed == "sh"
}

// deny 组装一次沙箱拒绝（段 3 由 core.SandboxViolationError 拼；**不得**在这里改写固定句）。
// deny builds one refusal; the fixed third segment lives in core.
func deny(rule, target, guidance string) error {
	return &core.SandboxViolationError{Rule: rule, Target: target, Guidance: guidance}
}
