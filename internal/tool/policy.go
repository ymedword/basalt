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
	"sync/atomic"

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
	// workspaceDirName 是工作区目录名：`storage_path` 的契约字面量以它起头
	// （`.basalt/versions/skills/<name>/<no>`），而策略根已经是这个目录本身。
	// The workspace directory name; storage_path literals are prefixed with it while the
	// policy root already is that directory.
	workspaceDirName = ".basalt"
	// versionsDirName 是技能不可变发布版本的根目录名（`.basalt/versions`，约束 31）。
	// 运行期 L2/L3 从发布目录读：读档只放行**本次 Run 绑定的**版本目录（人工 review ② F1），
	// 写入整树一律拒。
	// The root of the immutable skill version directories; reads are limited to the run's
	// bound versions and writes are refused for the whole tree.
	versionsDirName = "versions"
	// controlDirName 是受保护控制目录（`.basalt/control/`，生命周期状态文件的所在地）。
	// 它**不在**任何一档放行面之内：单独列出来是为了让拒绝出自一条固定规则，而不是被
	// "工作区之外"这条泛规则顺带挡下——模型读状态文件与读别处的失败原因必须能分开。
	// The protected control directory; it gets its own refusal rule so that "touching the
	// agent's own lifecycle state" is distinguishable from "touching the outside world".
	controlDirName = "control"
)

// 拒绝文案的固定片段（四段式的段 1 规则 / 段 4 放行指引；段 2 是具体目标、段 3 由
// `core.SandboxViolationError` 拼死句）。这两段是**模型可见契约**：改名等于换契约，
// 措辞只在"该档确实有配置键可加"时分叉（contracts/agent-md.md §6）。
// Frozen rule and guidance fragments of the model-visible refusal copy.
const (
	ruleReadOutside         = "读取目标不在可读根之内"
	guidanceReadOutside     = "可读根固定为当前 Agent 自己的目录与 skills 库（工作区约定），没有配置键可以加。"
	ruleCrossAgent          = "跨 Agent 读取未授权"
	guidanceCrossAgent      = "如需授权，请联系运维把该 Agent 名加入配置键 sandbox.cross_agent_read 并重启服务。"
	ruleAgentName           = "Agent 名不合法"
	guidanceAgentName       = "Agent 名取自工作区目录名（单层名字，不含路径分隔符），没有配置键可以加。"
	ruleUnresolvable        = "目标路径无法解析为真实路径"
	guidanceUnresolvable    = "路径中存在悬空软链接 / 链接成环 / 不可进入的中间目录；请改指工作区内的普通路径。"
	ruleUnknownAction       = "动作类型不在授权矩阵内"
	guidanceUnknownAction   = "只有文件读 / 文件写 / 命令 / 出网四类动作可被判定；请检查工具实现传入了什么。"
	ruleProtectedFile       = "AGENT.md 是受保护文件，写入被拒绝"
	guidanceProtectedFile   = "Agent 定义经页面编辑器与脚手架（同一道注册写入门）修改，没有配置键可以加。"
	ruleScriptsReadOnly     = "scripts/ 是只读区，写入被拒绝"
	guidanceScriptsReadOnly = "脚本受控通道的前提是模型改不动脚本（代码常量），没有配置键可以加。"
	ruleSkillsReadOnly      = "技能库不可写入"
	guidanceSkillsReadOnly  = "技能库由管理员在 skills/ 下维护（代码常量），没有配置键可以加。"
	// 运行中的技能工作副本档（F-04 B1）：Run 只读准入时固定的发布版本，工作副本的修改
	// 要等显式发布后才属于新运行。**不得**因为本次 Run 没绑定技能就退回工作副本。
	ruleRunSkillWorkCopy      = "技能工作副本在运行中不可读"
	guidanceRunSkillWorkCopy  = "Run 内只读准入时固定的技能发布版本（发布目录路径已在技能卡片中给出）；工作副本的后续修改要经显式发布才进入新运行，没有配置键可以加。"
	ruleMemoryNotWritable     = "记忆目录不在可写范围"
	guidanceMemoryNotWritable = "记忆的写入归记忆工具，文件工具不改写 memory/（代码常量），没有配置键可以加。"
	// 控制目录档（约束 30）：读与写**同一句话**——它对文件工具整目录不存在"只读"形态，
	// 差异化的读 / 写说法只会让模型以为换个工具就能打开。
	ruleControlDir            = "受保护控制目录不在文件工具的可访问范围"
	guidanceControlDir        = "生命周期与基线状态由管理面维护（代码常量），没有配置键可以加。"
	ruleWriteOutside          = "写入目标超出当前 Agent 的可写范围"
	guidanceWriteOutside      = "写范围固定为当前 Agent 自己的目录（代码常量），没有配置键可以加。"
	ruleParentMissing         = "目标父目录不存在"
	guidanceParentMissing     = "write_file 不自动创建父目录；请改在已存在的目录内写入。"
	ruleMultiWord             = "可执行名含空白（多词串）"
	guidanceMultiWord         = "executable 与 arguments 必须分开传（不经 shell 解释）：可执行名写单个可执行文件，参数走 arguments。绝对路径里的空格是路径的一部分——把它整串登记进 shell.allowed_commands（或 shell.interpreters）即可放行。"
	ruleBashSh                = "bash / sh 不得放行"
	guidanceBashSh            = "需要跑脚本请改用受控解释器通道（配置键 shell.interpreters，脚本须落在该 Agent 的 scripts/ 或技能库内）并重启服务。"
	ruleCommandNotAllowed     = "可执行名不在白名单"
	guidanceCommandNotAllowed = "如需放行，请联系运维把该可执行名加入配置键 shell.allowed_commands 并重启服务。"
	// 交互式 shell 名称集合（14D）：`bash` / `sh` 沿用既有文案，其余新名称单独一句——
	// 它们是**能力上限声明**，指引不能写成"加个配置键就能开"。
	ruleShellName             = "交互式 shell 不得放行"
	guidanceShellName         = "本产品不向模型提供交互式 shell（能力上限，代码常量）；需要跑脚本请改用受控解释器通道（配置键 shell.interpreters）。"
	ruleProgramDirRefused     = "程序目录可执行件的自动放行条件未满足"
	guidanceProgramDirRefused = "程序目录放行要求安装清单登记、真实路径位于 runtime/ 且只读保护生效三项同时满足；未满足时该可执行件按需显式登记（shell.allowed_commands）。"
	ruleNotDomain             = "出网目标不是域名"
	guidanceNotDomain         = "白名单按域名匹配，IP 字面量与无法解析的地址一律拒；没有配置键可以加。"
	ruleScheme                = "出网 scheme 不被允许"
	guidanceScheme            = "只允许 http / https（代码固定，配置放行不了）；没有配置键可以加。"
	ruleDomainNotAllowed      = "域名不在白名单"
	// R8 裁决 2：白名单改为管理台可写并热生效，指引**不得**再让模型等人「改配置并重启」。
	// The allowlist is edited in the console and applied live; the guidance no longer
	// tells anyone to edit config and restart.
	guidanceDomainNotAllowed = "如需访问，请联系管理员在管理台「执行环境」页把该域名加入网络域名白名单，保存后立即生效，无需重启。"
	// 禁止域名档（14D）：两档都先于放行判定，指引指向禁止域名表本身（不是白名单）。
	ruleDeniedDomain     = "域名在禁止域名表中"
	guidanceDeniedDomain = "该域名被管理员显式禁止出网（两档都先生效，白名单放行不了它）；如需恢复请联系管理员在管理台「执行环境」页调整禁止域名表。"
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
	// ProgramDirNames 是安装清单里登记的程序目录可执行名（标准档按名放行到执行侧，
	// 真实路径 / 哈希 / 只读保护三项由 `LocalRunner.Run` 复核）。
	// ProgramDirNames are the installation-manifest executable names; the standard
	// profile admits them by name and the runner re-checks path, hash and protection.
	ProgramDirNames []string
	// ProgramDir 是程序目录根的绝对路径（真实路径）。文件写入在**两档**都拒——它不在
	// 任何 Agent 的可写范围之内。
	// ProgramDir is the program directory root; writes are refused in both profiles.
	ProgramDir string
}

// WhitelistPolicy 是 v1 唯一的授权实现（技术方案 §4.3 授权矩阵 + §2.3 流程 F）：
// 只看"谁、要碰什么"——`Action` 没有 argv 字段，形态类校验属执行侧（`LocalRunner`）。
// 出网域名白名单是**唯一可热替换**的一档（R8 裁决 2）：其余三档仍是装配期快照。
// WhitelistPolicy authorizes by "who touches what" only. The outbound domain allowlist
// is the only hot-swappable dimension; the other three stay construction-time snapshots.
type WhitelistPolicy struct {
	root            string
	programDir      string
	crossAgentRead  map[string]struct{}
	allowedCommands map[string]struct{}
	interpreters    map[string]struct{}
	programDirs     map[string]struct{}
	// domains 以原子指针持有整份规则快照：保存成功后一次 `Store` 即对**下一次判定**
	// 生效，读方不加锁、也不会看到半份新规则。`nil` 表示空规则（全部拒绝）。
	// domains holds an immutable rule snapshot behind an atomic pointer so a swap is
	// visible to the very next check without locking or partial state.
	domains atomic.Pointer[domainRules]
	// denied 是禁止域名表的活快照（与白名单同一套匹配语法）。两档都先于放行判定。
	// denied is the live denylist snapshot; both profiles consult it before allowing.
	denied atomic.Pointer[domainRules]
	// exceptions 是网络访问例外的活快照（删除后下一次判定即失效）。
	// exceptions is the live network-exception snapshot.
	exceptions atomic.Pointer[[]core.NetworkException]
	// profile 是当前执行档位；零值（未装配）按 **strict** 处理——安全默认：不会因为
	// 忘了设置档位而静默扩大授权。
	// profile is the live execution profile; unset means strict so a missing assembly
	// can never widen authorization by accident.
	profile atomic.Pointer[core.ExecutionProfile]
}

// domainRules 是不可变的域名规则快照：条目已按小写、去首尾空白、去重、升序规整。
// domainRules is the immutable, normalized snapshot swapped on save.
type domainRules struct {
	entries []string
}

// 编译期断言：调用方只依赖 `core.Policy`，换实现只动装配处（宪法 VI）。
// Compile-time assertion that callers depend on the interface, not this type.
var _ core.Policy = (*WhitelistPolicy)(nil)

// NewWhitelistPolicy 装配白名单策略；列表逐条建集合表（显式面，不做前缀 / 通配推断）。
// NewWhitelistPolicy wires the policy; the lists become explicit lookup tables.
func NewWhitelistPolicy(opts PolicyOptions) *WhitelistPolicy {
	policy := &WhitelistPolicy{
		root:            filepath.Clean(opts.Root),
		programDir:      filepath.Clean(opts.ProgramDir),
		crossAgentRead:  make(map[string]struct{}, len(opts.CrossAgentRead)),
		allowedCommands: make(map[string]struct{}, len(opts.AllowedCommands)),
		interpreters:    make(map[string]struct{}, len(opts.Interpreters)),
		programDirs:     make(map[string]struct{}, len(opts.ProgramDirNames)),
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
	for _, name := range opts.ProgramDirNames {
		if name == "" {
			continue
		}
		// 清单里登记的是**文件名**：标准档按 basename 放行到执行侧，真实路径 / 哈希 /
		// 只读保护三项由 `LocalRunner.Run` 复核。空名不进集合（否则等于放行一切）。
		// Manifest entries are file names; an empty name never enters the set.
		policy.programDirs[foldCase(name)] = struct{}{}
	}
	policy.SetAllowedDomains(opts.AllowedDomains)
	policy.SetExecutionProfile(core.ProfileStrict)
	return policy
}

// SetExecutionProfile 原子替换执行档位：落库成功后调用一次，**下一次**判定即按新档位。
// 传入非法值时保持"严格档"（安全默认）并返回 false，由调用方决定是否记日志。
// SetExecutionProfile swaps the live profile; an invalid value keeps strict.
func (p *WhitelistPolicy) SetExecutionProfile(profile core.ExecutionProfile) bool {
	valid := profile.Valid()
	if !valid {
		profile = core.ProfileStrict
	}
	stored := profile
	p.profile.Store(&stored)
	return valid
}

// ExecutionProfile 返回当前生效的档位；未装配（零值）按严格档算——档位缺失只会更窄，
// 不会更宽。
// ExecutionProfile returns the live profile; unset reads as strict.
func (p *WhitelistPolicy) ExecutionProfile() core.ExecutionProfile {
	if p == nil {
		return core.ProfileStrict
	}
	profile := p.profile.Load()
	if profile == nil || !profile.Valid() {
		return core.ProfileStrict
	}
	return *profile
}

// SetDeniedDomains 原子替换禁止域名表：与白名单同一套规整与匹配语法，两档都先于放行判定。
// SetDeniedDomains swaps the live denylist snapshot; both profiles consult it first.
func (p *WhitelistPolicy) SetDeniedDomains(domains []string) {
	p.denied.Store(newDomainRules(domains))
}

// DeniedDomains 返回当前禁止域名表的副本（供视图投影）。
// DeniedDomains returns a copy of the live denylist for the view layer.
func (p *WhitelistPolicy) DeniedDomains() []string {
	rules := p.denied.Load()
	if rules == nil {
		return []string{}
	}
	return slices.Clone(rules.entries)
}

// SetNetworkExceptions 原子替换访问例外集合：删除后**下一次**判定即失效（拨号器每次读
// 这份快照，不缓存）。
// SetNetworkExceptions swaps the live exception list; dialling reads it per attempt.
func (p *WhitelistPolicy) SetNetworkExceptions(exceptions []core.NetworkException) {
	snapshot := slices.Clone(exceptions)
	p.exceptions.Store(&snapshot)
}

// NetworkExceptions 返回当前例外集合的副本（供拨号器与视图投影）。
// NetworkExceptions returns a copy of the live exceptions.
func (p *WhitelistPolicy) NetworkExceptions() []core.NetworkException {
	snapshot := p.exceptions.Load()
	if snapshot == nil {
		return nil
	}
	return slices.Clone(*snapshot)
}

// newDomainRules 规整一份域名列表为不可变快照（小写、去首尾空白、升序、去重）。
// newDomainRules normalizes a list into an immutable snapshot.
func newDomainRules(domains []string) *domainRules {
	normalized := make([]string, 0, len(domains))
	for _, entry := range domains {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		normalized = append(normalized, entry)
	}
	slices.Sort(normalized)
	normalized = slices.Compact(normalized)
	return &domainRules{entries: normalized}
}

// rulesMatch 判定 hosts 快照里是否有条目匹配 host（匹配器只有 `core.DomainEntryMatches`
// 一份——白名单 / 禁止表 / host 例外 / MCP 撤销复判共用）。
// rulesMatch reports whether any snapshot entry matches the host.
func rulesMatch(rules *domainRules, host string) bool {
	if rules == nil {
		return false
	}
	for _, entry := range rules.entries {
		if core.DomainEntryMatches(entry, host) {
			return true
		}
	}
	return false
}

// SetAllowedDomains 原子替换出网域名规则：调用方已做过严格校验与规整（服务层），
// 这里再做一次幂等的宽松规整，保证手工装配（CLI / 测试）也能得到同一份形状。
// 传入 `nil` 表示空规则——严格档下受此策略约束的出站请求全部拒绝。
// SetAllowedDomains swaps the outbound rule snapshot atomically; nil means deny-all in
// the strict profile.
func (p *WhitelistPolicy) SetAllowedDomains(domains []string) {
	p.domains.Store(newDomainRules(domains))
}

// AllowedDomains 返回当前生效规则的副本（供视图投影；调用方改动不影响内部快照）。
// AllowedDomains returns a copy of the live rules for the view layer.
func (p *WhitelistPolicy) AllowedDomains() []string {
	rules := p.domains.Load()
	if rules == nil {
		return []string{}
	}
	return slices.Clone(rules.entries)
}

// AllowsHost 只判出网规则的域名面：主机名在当前档位下是否有出网授权。MCP http 长连接的
// 撤销复判经它做——判定只有这一份，撤销与逐跳判定不可能分叉（通配 `*.suffix` 只覆盖
// 子域，不覆盖裸后缀）。scheme / 端口不是本方法的事。
// AllowsHost judges the domain face alone; MCP link revocation reuses this very
// decision so revocation and per-hop checks cannot drift apart.
func (p *WhitelistPolicy) AllowsHost(host string) bool {
	if p == nil {
		return false
	}
	return p.hostAuthorized(host)
}

// hostAuthorized 是"这个主机名在**当前档位**下能否出网"的唯一判定：
// 禁止域名表两档都先生效；标准档放行其余（受保护地址段由拨号器在解析后判，域名面
// 管不到）；严格档要求命中白名单或一条 host 类型访问例外。
// hostAuthorized is the single hostname-level verdict: the denylist binds both
// profiles, standard admits the rest, strict needs an allowlist or host exception hit.
func (p *WhitelistPolicy) hostAuthorized(host string) bool {
	if p.deniedHost(host) {
		return false
	}
	if p.ExecutionProfile() == core.ProfileStandard {
		return true
	}
	return p.domainAllowed(host) || p.hostExcepted(host)
}

// deniedHost 判定主机名是否命中禁止域名表。
// deniedHost reports a denylist hit.
func (p *WhitelistPolicy) deniedHost(host string) bool {
	if p == nil {
		return false
	}
	return rulesMatch(p.denied.Load(), host)
}

// hostExcepted 判定是否有 `host` 类型访问例外覆盖该主机名（严格档等价于白名单条目）。
// `ip_port` 例外不在域名面判定：它按解析结果匹配，由拨号器读同一份快照判。
// hostExcepted matches a host-kind exception; ip_port exceptions bind at dial time.
func (p *WhitelistPolicy) hostExcepted(host string) bool {
	snapshot := p.exceptions.Load()
	if snapshot == nil {
		return false
	}
	for _, exception := range *snapshot {
		if exception.Kind != core.NetworkExceptionKindHost {
			continue
		}
		if core.DomainEntryMatches(exception.Value, host) {
			return true
		}
	}
	return false
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
	if action.Agent != "" && !validSingleName(action.Agent) {
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
	// 控制目录**先于**全部放行分支：它在读档本来就不在任何可读根之内，但拒绝必须出自
	// 控制目录这条固定规则（约束 30），不能落成"工作区之外"——否则模型分不清"这是禁区"
	// 与"你指错地方了"。比较发生在真实路径投影之后，软连接绕回控制目录同样命中。
	// The control directory is judged first, on the projected real path, so a symlink
	// that lands back inside it is refused by this rule rather than the generic one.
	if withinDir(real, p.controlDir()) {
		return deny(ruleControlDir, action.Target, guidanceControlDir)
	}
	if ownDir != "" && withinDir(real, ownDir) {
		return nil
	}
	if withinDir(real, p.skillsDir()) {
		// Run 没有工作副本这个读面（F-04 B1）：即使本次 Run 一个技能都没绑定，也**不得**
		// 退回工作副本——那会读到未被本次运行固定的内容。非 Run 调用保留既有规则。
		// Inside a run the work copy is never readable, even with no skill bound; the
		// published version directories remain readable through the binding below.
		if action.RunBound {
			return deny(ruleRunSkillWorkCopy, action.Target, guidanceRunSkillWorkCopy)
		}
		return nil
	}
	// 发布版本目录只放行**本次 Run 绑定的**那些（人工 review ② F1 / 模块卡行 610）：
	// Run 的 L1 卡片逐字给出发布真路径，L2/L3 也要从这里读，但没绑定的版本不属于本次
	// 运行——放行整棵 `.basalt/versions/` 会越过"当前 Run 绑定版本"这条边界。
	// Only the versions bound to this run are readable; the rest of the tree stays out of
	// the readable roots, so a run can never read a version it was not admitted against.
	if p.withinBoundSkillDir(real, action.SkillDirs) {
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
	// 控制目录**先于**全部保护规则：它多数时候落在"自有目录之外"，但一个叫 AGENT.md 的
	// 控制文件不该被报成保护文件——同一条禁区在读写两档必须是同一句规则（约束 30）。
	// The control-directory rule runs before every other write protection so the same
	// forbidden zone always reports the same reason on both access kinds.
	if withinDir(real, p.controlDir()) {
		return deny(ruleControlDir, action.Target, guidanceControlDir)
	}
	switch {
	case foldCase(filepath.Base(real)) == foldCase(agentDefinitionFile):
		return deny(ruleProtectedFile, action.Target, guidanceProtectedFile)
	case ownDir != "" && withinDir(real, filepath.Join(ownDir, scriptsDirName)):
		return deny(ruleScriptsReadOnly, action.Target, guidanceScriptsReadOnly)
	case withinDir(real, p.skillsDir()):
		return deny(ruleSkillsReadOnly, action.Target, guidanceSkillsReadOnly)
	// 发布版本目录与工作副本同档：技能内容只能经发布动作改写（约束 31）。控制目录
	// 规则在本 switch 之前，已先于该分支命中。
	// The published tree is as read-only as the work copy; skill content only changes
	// through an explicit publish.
	case withinDir(real, p.skillVersionsDir()):
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

// checkShellCommand 落 §2.3 流程 F 的命令档：交互式 shell 名称集合**硬拒先行**（契约
// §5.2 的"不得放行"没有例外分支——先查白名单会让 `allowed_commands: [bash]` 这个配置
// 错误变成放行），随后多词串拒、白名单整串逐字比对、解释器通道放行到执行侧；标准档
// 另放行安装清单内的程序目录可执行名（真实路径 / 哈希 / 只读保护由 `LocalRunner.Run`
// 复核，未满足即拒）。
// The shell-name ceiling runs before any allow branch, so a misconfigured allowlist can
// never open an interactive shell back up. Standard additionally admits manifest names
// to the execution side, which re-checks path, hash and read-only protection.
// registeredCommand 判一条命令是否**整串逐字**登记在白名单或解释器集合里（两处都是
// 显式登记面，含空格路径的放行口径因此只有一份）。
// registeredCommand reports an exact match in either explicit registration set.
func (p *WhitelistPolicy) registeredCommand(target string) bool {
	if _, ok := p.allowedCommands[target]; ok {
		return true
	}
	_, ok := p.interpreters[target]
	return ok
}

func (p *WhitelistPolicy) checkShellCommand(action core.Action) error {
	target := action.Target
	if isBannedShell(target) {
		return deny(ruleBashSh, target, guidanceBashSh)
	}
	// 其余交互式 shell 名称（zsh / cmd / powershell …）：能力上限，白名单与解释器通道
	// 都不给它开口子。`bash` / `sh` 已在上面按既有文案拒，这里只管新增名称；按基名判，
	// 所以**先于**下面的多词分支——绝对路径里的 shell 不会因为"含空格"被当成普通路径。
	if core.IsShellName(target) {
		return deny(ruleShellName, target, guidanceShellName)
	}
	if strings.ContainsAny(target, " \t") {
		// 绝对路径里的空白是**路径本身的一部分**（`…\Program Files\Basalt 0.1.0\…`）：
		// 执行侧从不按空白拆词、也不经 shell，整串逐字命中白名单 / 解释器集合的绝对路径
		// 与无空格路径等价（14D §2.2.2 显式登记口径）。**相对形态**的多词串照旧先拒——
		// 那是"把参数拼进 executable"的旧写法，写进白名单也不构成可执行文件路径。
		// Whitespace inside an absolute path is part of the path: an exact allowlist hit
		// is allowed; a relative multi-word string is still refused before the lookup.
		if filepath.IsAbs(target) && p.registeredCommand(target) {
			return nil
		}
		return deny(ruleMultiWord, target, guidanceMultiWord)
	}
	if _, ok := p.allowedCommands[target]; ok {
		return nil
	}
	if _, ok := p.interpreters[target]; ok {
		// 放行**到执行侧**：argv 形态（`-c` 拒）与脚本真实路径越界由 LocalRunner 判。
		return nil
	}
	if p.ExecutionProfile() == core.ProfileStandard {
		if _, ok := p.programDirs[foldCase(filepath.Base(target))]; ok {
			return nil
		}
	}
	return deny(ruleCommandNotAllowed, target, guidanceCommandNotAllowed)
}

// checkHTTPRequest 落 §2.3 流程 F 的出网档：逐跳校验由调用方在每一跳发送前再次 Check；
// 跳数上限 5 是调用方的代码常量（US-5 的 http 工具），本档只判"这一跳去哪"。
// 判定序：解析 → scheme → hostname / IP 字面量 → **禁止域名表（两档先生效）** →
// **端口档（口径 15）** → 档位授权（标准档放行；严格档要白名单或 host 例外）。
// 初始请求与每一次重定向同判（判定只此一处）。拒绝分支只回安全文案与 host，
// **不得**回显 `action.Target` 原文（原文可能带 userinfo / query——§3.5 第 9 项）。
// Per-hop judgement; the denylist binds both profiles before anything is allowed, and
// the redirect hop cap lives with the http tools (US-5).
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
	// 出网按域名判定：IP 字面量目标直接拒（放行 IP 等于绕过域名表；访问受保护地址
	// 请用域名 + 访问例外，`ip_port` 例外在拨号阶段按解析结果匹配）。
	if host == "" {
		return deny(ruleNotDomain, targetUnresolvable, guidanceNotDomain)
	}
	if net.ParseIP(host) != nil {
		// 段 2 点名 IP 字面量本身——它不含 userinfo / query，回显它不泄漏任何东西。
		return deny(ruleNotDomain, host, guidanceNotDomain)
	}
	// 禁止域名**先于**端口与授权：命中即拒，与档位、端口、白名单都无关（"禁止 > 允许"）。
	if p.deniedHost(host) {
		return denyWithReason(ruleDeniedDomain, host, guidanceDeniedDomain, core.DenialDeniedDomain)
	}
	// 端口档（口径 15，冻结）：`http` 配省略端口或显式 `:80`、`https` 配省略端口或显式
	// `:443` 放行，其余组合（含交叉档 http:443 / https:80）一律拒——判定在 hostname 之后、
	// 档位授权之前：域名档管 host、端口档管端口，两档不互相顶替。
	if port := u.Port(); port != "" {
		defaultPort, ok := defaultPortFor(u.Scheme)
		if !ok || port != strconv.Itoa(defaultPort) {
			// 段 2 点名 host 与端口（`u.Host` 不含 userinfo），**不含** query。
			return deny(ruleNonDefaultPort, u.Host, guidanceNonDefaultPort)
		}
	}
	if p.ExecutionProfile() == core.ProfileStandard {
		// 标准档：不在禁止域名表内即可（受保护地址段由拨号器在解析后判——域名面判不到）。
		return nil
	}
	if !p.domainAllowed(host) && !p.hostExcepted(host) {
		return denyWithReason(ruleDomainNotAllowed, host, guidanceDomainNotAllowed,
			core.DenialProfileDenied)
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

// domainAllowed 判白名单命中。匹配器只有 `core.DomainEntryMatches` 一份（白名单、禁止
// 域名表、host 例外与 MCP 撤销复判共用）：`*.suffix` 只覆盖子域，不做"包含即命中"。
// domainAllowed reuses the single shared matcher so no face can drift apart.
func (p *WhitelistPolicy) domainAllowed(host string) bool {
	if p == nil {
		return false
	}
	return rulesMatch(p.domains.Load(), host)
}

// agentDir 定位 Agent 自有目录；名字不合法或为空（无 Agent 上下文）时返回空串——
// 调用方据此跳过"自有目录"分支（无上下文就没有自有目录，不是报错）。
// agentDir locates the agent's own directory; an empty or invalid name yields "".
func (p *WhitelistPolicy) agentDir(name string) string {
	if !validSingleName(name) {
		return ""
	}
	return filepath.Join(p.root, agentsDirName, name)
}

// skillsDir 是技能库根（`.basalt/skills`）——读放行、写一律拒（代码常量）。
// skillsDir is the read-only skills library root.
func (p *WhitelistPolicy) skillsDir() string {
	return filepath.Join(p.root, skillsDirName)
}

// controlDir 是受保护控制目录（`.basalt/control`）——读写一律拒（约束 30）。
// controlDir is the protected control directory, refused for both reads and writes.
func (p *WhitelistPolicy) controlDir() string {
	return filepath.Join(p.root, controlDirName)
}

// skillVersionsDir 是技能不可变发布版本的根（`.basalt/versions/skills`）——写入整树拒
// （约束 31）；读取只放行本次 Run 绑定的版本子目录（见 boundSkillDir），根目录本身
// 不是可读根。
// skillVersionsDir is the write-protected root of the immutable skill version tree; reads
// are limited to the run-bound version directories.
func (p *WhitelistPolicy) skillVersionsDir() string {
	return filepath.Join(p.root, versionsDirName, skillsDirName)
}

// withinBoundSkillDir 判定 real 是否落在本次 Run 绑定的某个技能发布版本目录内（人工
// review ② F1）。绑定值来自 `ResolvedResources.SkillDirs` 的 `storage_path` 字面量；
// 逐条按契约形态校验并投影成真实路径后比较——两边都在真实路径上，软连接绕回未绑定
// 版本同样落空。不合形态的条目**只丢弃它自己**：失败关闭，不会退回"整树放行"。
// withinBoundSkillDir reports whether real sits inside one of the run-bound version
// directories; malformed entries are dropped individually, never widened to the tree.
func (p *WhitelistPolicy) withinBoundSkillDir(real string, dirs []string) bool {
	for _, dir := range dirs {
		bound, ok := p.boundSkillDir(dir)
		if !ok {
			continue
		}
		if withinDir(real, bound) {
			return true
		}
	}
	return false
}

// boundSkillDir 把一处 `storage_path` 字面量投影为策略根下的真实目录；两种契约形态都认
// ——新的身份分段 `versions/skills/<name>/<resource_uid>/<no>` 与旧扁平
// `versions/skills/<name>/<no>`（历史 Run 的绑定）。形态不在契约内（缺 `.basalt/` 前缀 /
// 名字、身份或版本号非法 / 投影失败）一律 false——绑定面不接受"差不多"的路径。
// boundSkillDir projects one storage_path literal onto a real directory; both the
// identity-segmented and the legacy flat shapes are accepted, anything else is refused.
func (p *WhitelistPolicy) boundSkillDir(dir string) (string, bool) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(dir)))
	if clean == "." || filepath.IsAbs(clean) {
		return "", false
	}
	head, rest, found := strings.Cut(clean, string(filepath.Separator))
	if !found || foldCase(head) != foldCase(workspaceDirName) {
		return "", false
	}
	parts := strings.Split(rest, string(filepath.Separator))
	if len(parts) != 4 && len(parts) != 5 {
		return "", false
	}
	if parts[0] != versionsDirName || parts[1] != skillsDirName {
		return "", false
	}
	if !validSingleName(parts[2]) {
		return "", false
	}
	if len(parts) == 5 {
		uid, err := core.ParseResourceUID(parts[3])
		if err != nil || string(uid) != parts[3] {
			return "", false
		}
	}
	versionPart := parts[len(parts)-1]
	version, err := strconv.Atoi(versionPart)
	if err != nil || version < 1 || strconv.Itoa(version) != versionPart {
		return "", false
	}
	real, err := resolveRealPath(p.root, rest)
	if err != nil {
		return "", false
	}
	return real, true
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

// validSingleName 只接受**单层目录名**（Agent 名与技能名同规则）：名字来自工作区目录名，
// 含路径分隔符的形态会让"自有目录"或"绑定版本目录"指向工作区外的任意位置（自造的越界
// 入口）。
// Only a single path element is a legal agent or skill name.
func validSingleName(name string) bool {
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

// denyWithReason 组装一份**能解释"该改哪一条"**的沙箱拒绝：`error_kind` 仍是
// `sandbox_denied`，`Reason` 只作细分（契约 §7.3 的四类）。不属于四类的拒绝（文件范围、
// 命令形态、端口档…）用 `deny` 留空——空串表示"不细分"，不是第五类。
// denyWithReason refines a denial with one of the four contract reasons; the other
// refusals stay unrefined so an empty reason never reads as a fifth category.
func denyWithReason(rule, target, guidance string, reason core.PolicyDenialReason) error {
	return &core.SandboxViolationError{Rule: rule, Target: target, Guidance: guidance, Reason: reason}
}
