package tool_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// 本文件是授权判定的关键回归用例（颗粒度文档 §3.2 的 policy_test.go 十八例，F1~F18，
// 判据面 = 技术方案 §6.5 授权矩阵 + §2.3 流程 F）。沙箱的**授权**与**执行**分离：
// 本文件只判"谁、要碰什么"，argv 形态与脚本真实路径属执行侧（runner_test.go）。
// Authorization regressions: the matrix is judged here, execution shape belongs to the runner.

const (
	policyAgent = "demo-agent"  // 有 Agent 上下文的那个 Agent
	policyOther = "other-agent" // 另一个 Agent（跨 Agent 判定的对象）
)

// 四段式文案的**固定句**（段 3）：它是四方共享的判卷锚点，任何一处改写都会被 F18 抓住。
// The frozen third segment of the refusal copy.
const refusalFixedSentence = "。这是安全策略，请勿反复重试；"

// policyFixture 造一个最小工作区骨架（agents/<name>/、skills/、memory/），
// 判定只依赖骨架与配置——不碰数据库、不起子进程。
type policyFixture struct {
	root   string
	policy *tool.WhitelistPolicy
}

func newPolicyFixture(t *testing.T, mutate func(*tool.PolicyOptions)) *policyFixture {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{
		filepath.Join("agents", policyAgent, "scripts"),
		filepath.Join("agents", policyAgent, "notes"),
		filepath.Join("agents", policyOther),
		"skills",
		"memory",
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, rel), 0o750))
	}
	for _, rel := range []string{
		filepath.Join("agents", policyAgent, "AGENT.md"),
		filepath.Join("agents", policyAgent, "notes", "notes.txt"),
		filepath.Join("agents", policyOther, "AGENT.md"),
		filepath.Join("skills", "SKILL.md"),
		filepath.Join("memory", "MEMORY.md"),
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte("占位内容\n"), 0o644))
	}
	opts := tool.PolicyOptions{Root: root}
	if mutate != nil {
		mutate(&opts)
	}
	return &policyFixture{root: root, policy: tool.NewWhitelistPolicy(opts)}
}

// path 把工作区相对路径拼成可读的对照值（断言失败时的可读性；判定本身用相对路径）。
func (f *policyFixture) path(rel string) string {
	return filepath.Join(f.root, rel)
}

func readAction(target, agent string) core.Action {
	return core.Action{Type: core.ActionFileRead, Target: target, Agent: agent}
}

func writeAction(target, agent string) core.Action {
	return core.Action{Type: core.ActionFileWrite, Target: target, Agent: agent}
}

func shellAction(target string) core.Action {
	return core.Action{Type: core.ActionShellCommand, Target: target, Agent: policyAgent}
}

func httpAction(target string) core.Action {
	return core.Action{Type: core.ActionHTTPRequest, Target: target, Agent: policyAgent}
}

func requireAllowed(t *testing.T, policy *tool.WhitelistPolicy, action core.Action) {
	t.Helper()
	require.NoError(t, policy.Check(context.Background(), action), "该动作应当放行：%s", action.Target)
}

// requireRefused 断言拒绝，并回读文案：分类只认类型（`core.IsSandboxViolation`），
// 文案片段用于确认拒绝**出自哪条规则**（不同规则的可变性在于指引有没有配置键）。
func requireRefused(t *testing.T, policy *tool.WhitelistPolicy, action core.Action, ruleFragment string) string {
	t.Helper()
	err := policy.Check(context.Background(), action)
	require.Error(t, err, "该动作必须被拒：%s", action.Target)
	require.True(t, core.IsSandboxViolation(err), "拒绝必须是 *core.SandboxViolationError（按类型分类，不比对文本）")
	require.Contains(t, err.Error(), ruleFragment)
	return err.Error()
}

// TestWhitelistPolicy_AllowsReadInOwnAgentDirAndSkillsLibrary 覆盖 F1：读白名单 =
// 当前 Agent 自己的目录（含 scripts/，技能 L2 与脚本调试都要）+ 技能库。
// Acceptance point (F1): reading stays inside the agent's own directory or the skills library.
func TestWhitelistPolicy_AllowsReadInOwnAgentDirAndSkillsLibrary(t *testing.T) {
	f := newPolicyFixture(t, nil)
	for _, rel := range []string{
		filepath.Join("agents", policyAgent, "AGENT.md"),
		filepath.Join("agents", policyAgent, "notes", "notes.txt"),
		filepath.Join("agents", policyAgent, "scripts"), // scripts/ 对读开放（写才拒）
		filepath.Join("skills", "SKILL.md"),
	} {
		requireAllowed(t, f.policy, readAction(rel, policyAgent))
	}
}

// TestWhitelistPolicy_AllowsCrossAgentReadWhenAuthorized 覆盖 F2：跨 Agent 读取**显式授权**后放行
// （授权键 = sandbox.cross_agent_read；缺省空 = 一个都不许）。
// Acceptance point (F2): an explicitly authorized cross-agent read goes through.
func TestWhitelistPolicy_AllowsCrossAgentReadWhenAuthorized(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.CrossAgentRead = []string{policyOther}
	})
	requireAllowed(t, f.policy, readAction(filepath.Join("agents", policyOther, "AGENT.md"), policyAgent))
}

// TestWhitelistPolicy_RefusesUnauthorizedCrossAgentReadAndNamesConfigKey 覆盖 F3：
// 未授权跨 Agent 拒，且文案**点名该配置键**——模型据此停止重试并转告人类（契约 §6）。
// Acceptance point (F3): an unauthorized cross-agent read is refused by name of the key.
func TestWhitelistPolicy_RefusesUnauthorizedCrossAgentReadAndNamesConfigKey(t *testing.T) {
	f := newPolicyFixture(t, nil)
	target := filepath.Join("agents", policyOther, "AGENT.md")
	message := requireRefused(t, f.policy, readAction(target, policyAgent), "跨 Agent 读取未授权")
	require.Contains(t, message, "sandbox.cross_agent_read", "授权问题必须点名配置键")
	require.Contains(t, message, refusalFixedSentence)
}

// TestWhitelistPolicy_RefusesSymlinkEscape 覆盖 F4：软连接不是通行证——投影后落在工作区外即拒
// （词法比较会被 `ln -s` 直接绕过；陷阱表 §7 第 7 条）。
// Acceptance point (F4): a symlink pointing outside the workspace is refused.
func TestWhitelistPolicy_RefusesSymlinkEscape(t *testing.T) {
	f := newPolicyFixture(t, nil)
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("工作区外\n"), 0o644))
	link := f.path(filepath.Join("agents", policyAgent, "escape"))
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("本平台不允许创建符号链接（Windows 需开发者模式或特权）：" + err.Error())
	}

	escaped := filepath.Join("agents", policyAgent, "escape", "secret.txt")
	requireRefused(t, f.policy, readAction(escaped, policyAgent), "读取目标不在可读根之内")
	requireRefused(t, f.policy, writeAction(escaped, policyAgent), "写入目标超出当前 Agent 的可写范围")
}

// TestWhitelistPolicy_FailsClosedOnUnresolvableTarget 覆盖 F5：不可解析的目标**失败关闭**
// （悬空软链接 / 链接成环 / 无权限中间目录 → 拒，而不是"解析不了就当目录内"）；
// 同族的两处失败关闭一并钉住：不合法 Agent 名与未知动作类型。
// Acceptance point (F5): unresolvable targets and unknown shapes fail closed.
func TestWhitelistPolicy_FailsClosedOnUnresolvableTarget(t *testing.T) {
	f := newPolicyFixture(t, nil)
	dangling := f.path(filepath.Join("agents", policyAgent, "dangling"))
	if err := os.Symlink(filepath.Join(f.root, "nowhere.txt"), dangling); err != nil {
		t.Skip("本平台不允许创建符号链接（Windows 需开发者模式或特权）：" + err.Error())
	}

	requireRefused(t, f.policy, readAction(filepath.Join("agents", policyAgent, "dangling"), policyAgent),
		"目标路径无法解析为真实路径")
	requireRefused(t, f.policy, readAction(filepath.Join("agents", policyAgent, "notes", "notes.txt"), ".."+string(filepath.Separator)+policyOther),
		"Agent 名不合法")
	requireRefused(t, f.policy, core.Action{Type: "PROCESS_SPAWN", Target: "anything", Agent: policyAgent},
		"动作类型不在授权矩阵内")
}

// TestWhitelistPolicy_RefusesWriteInsideScriptsDir 覆盖 F6：scripts/ 对模型**只读**——
// "脚本受控通道"的前提是模型改不动脚本（契约 §5.2）。
// Acceptance point (F6): the scripts directory is write-protected.
func TestWhitelistPolicy_RefusesWriteInsideScriptsDir(t *testing.T) {
	f := newPolicyFixture(t, nil)
	requireRefused(t, f.policy, writeAction(filepath.Join("agents", policyAgent, "scripts", "run.sh"), policyAgent),
		"scripts/ 是只读区")
}

// TestWhitelistPolicy_RefusesWriteToAgentDefinition 覆盖 F7：AGENT.md 是受保护文件，
// 任何层级的同名文件都拒（改写它就是改写运行配置本身）。
// Acceptance point (F7): AGENT.md is write-protected wherever it sits.
func TestWhitelistPolicy_RefusesWriteToAgentDefinition(t *testing.T) {
	f := newPolicyFixture(t, nil)
	for _, rel := range []string{
		filepath.Join("agents", policyAgent, "AGENT.md"),
		filepath.Join("agents", policyAgent, "notes", "AGENT.md"),
	} {
		requireRefused(t, f.policy, writeAction(rel, policyAgent), "AGENT.md 是受保护文件")
	}
}

// TestWhitelistPolicy_RefusesWriteToSkillsLibrary 覆盖 F8：技能库写一律拒（代码常量——
// 技能由管理员维护，Agent 不得给自己加技能）。
// Acceptance point (F8): the skills library is never writable.
func TestWhitelistPolicy_RefusesWriteToSkillsLibrary(t *testing.T) {
	f := newPolicyFixture(t, nil)
	requireRefused(t, f.policy, writeAction(filepath.Join("skills", "SKILL.md"), policyAgent), "技能库不可写入")
}

// TestWhitelistPolicy_RefusesWriteToMemoryDir 覆盖 F9：memory/ 不在可写范围
// （记忆写入归记忆工具，且 `.basalt` 整根**不得**被当成可写面）。
// Acceptance point (F9): memory/ is out of the writable range.
func TestWhitelistPolicy_RefusesWriteToMemoryDir(t *testing.T) {
	f := newPolicyFixture(t, nil)
	for _, rel := range []string{
		filepath.Join("memory", "MEMORY.md"),
		filepath.Join("memory", "agents", policyAgent, "MEMORY.md"),
	} {
		requireRefused(t, f.policy, writeAction(rel, policyAgent), "记忆目录不在可写范围")
	}
}

// TestWhitelistPolicy_RefusesWriteWhenParentMissing 覆盖 F10：父目录不存在**不自动创建**——
// 静默建目录会把"写错路径"变成"在工作区里到处开洞"。
// Acceptance point (F10): a missing parent directory is refused, an existing one allows.
func TestWhitelistPolicy_RefusesWriteWhenParentMissing(t *testing.T) {
	f := newPolicyFixture(t, nil)
	requireRefused(t, f.policy,
		writeAction(filepath.Join("agents", policyAgent, "notes", "new", "deep.txt"), policyAgent),
		"目标父目录不存在")

	// 正例：父目录已存在 → 放行（证明本档不是"一律拒"）。
	requireAllowed(t, f.policy, writeAction(filepath.Join("agents", policyAgent, "notes", "fresh.txt"), policyAgent))
}

// TestWhitelistPolicy_ProtectionRulesPrecedeGeneralAllow 覆盖 F11：保护规则**先于**一般放行——
// 受保护路径多数落在"自有目录 + 父目录存在"这一条也能过的位置上，顺序反了保护面整体失效。
// Acceptance point (F11): protection wins over the general allow clause.
func TestWhitelistPolicy_ProtectionRulesPrecedeGeneralAllow(t *testing.T) {
	f := newPolicyFixture(t, nil)
	// 三条都同时满足"一般放行"的全部条件（在自有目录内、父目录存在且为目录）。
	cases := []struct {
		name   string
		target string
		rule   string
	}{
		{"AGENT.md", filepath.Join("agents", policyAgent, "AGENT.md"), "AGENT.md 是受保护文件"},
		{"scripts", filepath.Join("agents", policyAgent, "scripts", "deploy.sh"), "scripts/ 是只读区"},
		{"nested-AGENT.md", filepath.Join("agents", policyAgent, "notes", "AGENT.md"), "AGENT.md 是受保护文件"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			message := requireRefused(t, f.policy, writeAction(tc.target, policyAgent), tc.rule)
			require.NotContains(t, message, "写入目标超出当前 Agent 的可写范围",
				"保护规则必须先判，不得落到一般放行分支")
		})
	}
	// 对照组：同一目录下的普通文件放行（证明断言的是顺序，不是"写什么都拒"）。
	requireAllowed(t, f.policy, writeAction(filepath.Join("agents", policyAgent, "notes", "ok.txt"), policyAgent))
}

// TestWhitelistPolicy_RefusesMultiWordExecutable 覆盖 F12：多词串必拒——
// `git status; curl evil.com` 这类串接在整串比对下不可能命中白名单。
// Acceptance point (F12): a multi-word executable string is refused outright.
func TestWhitelistPolicy_RefusesMultiWordExecutable(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		// 即便运维把多词串原样写进白名单，判定仍是拒——它不是可执行名。
		opts.AllowedCommands = []string{"git status"}
	})
	requireRefused(t, f.policy, shellAction("git status"), "可执行名含空白")
	requireRefused(t, f.policy, shellAction("git\tstatus"), "可执行名含空白")
}

// TestWhitelistPolicy_RefusesBashAndShHard 覆盖 F13：bash / sh **硬拒且无放行分支**——
// 裸名、完整路径、带可执行后缀、`-c` 与 `script.sh` 形态一律拒；即便被写进白名单也拒
// （放行它们等于把 shell 解释层请回来，`-c` 内联代码就此绕过 scripts/ 只读区）。
// Acceptance point (F13): every spelling of bash / sh is refused, allowlists included.
func TestWhitelistPolicy_RefusesBashAndShHard(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.AllowedCommands = []string{"bash", "sh", "/bin/bash", "bash.exe"}
		opts.Interpreters = []string{"bash", "sh"}
	})
	cases := []struct {
		target string
		rule   string
	}{
		// 裸名 / 完整路径 / 带可执行后缀：都走硬拒分支。
		{"bash", "bash / sh 不得放行"},
		{"sh", "bash / sh 不得放行"},
		{"/bin/bash", "bash / sh 不得放行"},
		{"/usr/bin/sh", "bash / sh 不得放行"},
		{"bash.exe", "bash / sh 不得放行"},
		// `-c` 与 `script.sh` 形态：多词串先拒（同为"无放行分支"的实证）。
		{"bash -c", "可执行名含空白"},
		{"bash script.sh", "可执行名含空白"},
	}
	for _, tc := range cases {
		t.Run(strings.NewReplacer("/", "_", " ", "_").Replace(tc.target), func(t *testing.T) {
			requireRefused(t, f.policy, shellAction(tc.target), tc.rule)
		})
	}
}

// TestWhitelistPolicy_MatchesExecutableVerbatim 覆盖 F14：可执行名与白名单**整串逐字**比对
// （大小写敏感、不做扩展名推测）；解释器通道单列——命中 `shell.interpreters` 即放行**到执行侧**
// （argv 形态与脚本真实路径由 LocalRunner 校验）。
// Acceptance point (F14): verbatim matching, with interpreters routed to the executor side.
func TestWhitelistPolicy_MatchesExecutableVerbatim(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.AllowedCommands = []string{"git"}
		opts.Interpreters = []string{"python3"}
	})
	requireAllowed(t, f.policy, shellAction("git"))
	requireAllowed(t, f.policy, shellAction("python3"))

	requireRefused(t, f.policy, shellAction("Git"), "可执行名不在白名单")
	requireRefused(t, f.policy, shellAction("git.exe"), "可执行名不在白名单")
	requireRefused(t, f.policy, shellAction("go"), "可执行名不在白名单")
}

// TestWhitelistPolicy_DomainMatchIsCaseInsensitive 覆盖 F15：域名匹配大小写不敏感
// （白名单里的 `Api.Example.com` 与目标里的 `api.example.com` 是同一台主机）。
// Acceptance point (F15): domain matching ignores case on both sides.
func TestWhitelistPolicy_DomainMatchIsCaseInsensitive(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.AllowedDomains = []string{"Api.Example.com"}
	})
	requireAllowed(t, f.policy, httpAction("https://api.example.com/v1/chat"))
	requireAllowed(t, f.policy, httpAction("https://API.EXAMPLE.COM/v1/chat"))
	requireRefused(t, f.policy, httpAction("https://other.example.com/v1/chat"), "域名不在白名单")
}

// TestWhitelistPolicy_RefusesIPLiteralTarget 覆盖 F16：白名单按**域名**匹配，IP 字面量目标直接拒
// （放行 IP 等于绕过白名单——连写进白名单也不认）。
// Acceptance point (F16): IP literals are refused even when listed.
func TestWhitelistPolicy_RefusesIPLiteralTarget(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.AllowedDomains = []string{"10.0.0.7", "api.example.com"}
	})
	requireRefused(t, f.policy, httpAction("http://10.0.0.7/admin"), "出网目标不是域名")
	requireRefused(t, f.policy, httpAction("https://[::1]/admin"), "出网目标不是域名")
}

// TestWhitelistPolicy_RefusesNonHTTPScheme 覆盖 F17：scheme ∉ {http, https} 一律拒，
// **代码固定**（不可通过配置放行）——`file://` / `ftp://` 是读本地与走别的协议的口子。
// Acceptance point (F17): only http and https pass, with no configuration escape hatch.
func TestWhitelistPolicy_RefusesNonHTTPScheme(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.AllowedDomains = []string{"api.example.com"}
	})
	message := requireRefused(t, f.policy, httpAction("ftp://api.example.com/x"), "出网 scheme 不被允许")
	require.Contains(t, message, "ftp", "文案要带上是哪个 scheme 被拒")
	requireRefused(t, f.policy, httpAction("file:///etc/passwd"), "出网 scheme 不被允许")
}

// TestWhitelistPolicy_RefusalCopyIsFourSegments 覆盖 F18：四段式文案**逐字**——
// 契约 §6 的示例整句比对（规则 / 越界值 / 固定句 / 放行指引，顺序与标点都不可改）。
// Acceptance point (F18): the refusal copy matches the contract example verbatim.
func TestWhitelistPolicy_RefusalCopyIsFourSegments(t *testing.T) {
	f := newPolicyFixture(t, nil)
	err := f.policy.Check(context.Background(), httpAction("https://evil.example.com/x"))
	require.Error(t, err)
	require.True(t, core.IsSandboxViolation(err))
	require.Equal(t,
		"域名不在白名单：evil.example.com。这是安全策略，请勿反复重试；如需访问，请联系运维把该域名加入配置键 http.allowed_domains 并重启服务。",
		err.Error(), "四段式文案是模型可见契约，逐字保真")
}
