package cli

// 本文件是 US-3 命令面的关键回归用例（颗粒度文档 §3.2 的 cli/profile_test.go 八例；
// A7 = list / show 走轻路径，A8 = create 的输出明示"选中了谁"、A9 = delete 的归档接线）。
// 包内测试（`package cli`）：展示层与 run 级入口要能被**直接注入**判卷——假 Agents
// 端口顶替实现（cli **不得** import workspace：适配器在 app），真根命令覆盖注册、
// 参数校验与出口码面。
// An in-package test on purpose: the port is faked so the command surface is judged
// without importing the implementation package, and the real root command covers
// registration together with the exit-code path.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeAgents 是 Agents 端口的判卷替身：记录每次调用收到的实参，返回预置结果或预置错误
// （"命令层把什么传给了端口"本身就是判据——provider 名集与 now 都是显式输入）。
// fakeAgents records the arguments it receives and returns canned results: which
// values reach the port is itself part of the acceptance criteria.
type fakeAgents struct {
	listRoot      string
	listProviders []string
	listEntries   []core.AgentEntry
	listErr       error

	createRoot      string
	createName      string
	createProviders []string
	created         []string
	createErr       error

	deleteRoot string
	deleteName string
	deleteNow  time.Time
	archived   string
	deleteErr  error
}

func (f *fakeAgents) List(root string, providers []string) ([]core.AgentEntry, error) {
	f.listRoot, f.listProviders = root, providers
	return f.listEntries, f.listErr
}

func (f *fakeAgents) Create(root, name string, providers []string) ([]string, error) {
	f.createRoot, f.createName, f.createProviders = root, name, providers
	return f.created, f.createErr
}

func (f *fakeAgents) Delete(root, name string, now time.Time) (string, error) {
	f.deleteRoot, f.deleteName, f.deleteNow = root, name, now
	return f.archived, f.deleteErr
}

// profileRun 是一次 profile 子命令运行的返回错误与三路输出（stdout / stderr / 日志）。
// profileRun captures the error and the three output tracks of one command run.
type profileRun struct {
	err    error
	stdout string
	stderr string
	logs   string
}

// profileDeps 组装命令面依赖：Store / Workspace 两个端口**故意留 nil**——profile 的
// 轻路径不开库、不碰工作区端口（命令层只 stat 文件系统），一旦有代码碰到它们就当场
// panic 而不是悄悄通过（与 provider list 同一纪律）。
// profileDeps leaves the store and workspace ports nil on purpose: the profile light
// path must never touch them, so a touch panics loudly instead of passing quietly.
func profileDeps(workDir string, agents Agents) Deps {
	return Deps{
		WorkDir: workDir,
		// 合成值：轻路径用不到 schema 版本，给非零值以免零值把"其实用到了"掩盖过去。
		SupportedSchemaVersion: 7,
		Store:                  nil,
		Workspace:              nil,
		Agents:                 agents,
	}
}

// executeProfile 在注入的依赖上跑一次**真根命令**（命令注册、参数校验与出口码都在被测面内）。
// executeProfile runs the real root command so registration, argument validation
// and the exit-code path stay under test.
func executeProfile(t *testing.T, deps Deps, args ...string) profileRun {
	t.Helper()

	var logs bytes.Buffer
	deps.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	root := NewRootCommand(deps)
	root.SetArgs(args)

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.ExecuteContext(context.Background())
	return profileRun{err: err, stdout: stdout.String(), stderr: stderr.String(), logs: logs.String()}
}

// initProfileWorkspace 造一个"已初始化"的最小工作区：命令层的未初始化判据只看
// `.basalt/agents/` 是否存在（装载由假端口顶替），夹具因而只建这一层目录，不建整套骨架
// ——骨架齐备由 US-1 的 init / status 用例判卷。
// initProfileWorkspace builds the minimal initialized workspace the command layer
// checks for; the full skeleton is covered by the US-1 init / status tests.
func initProfileWorkspace(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, workspaceDirName, "agents"), 0o750))
	return dir
}

// 中文验收点（A7）：list 逐条渲染 registered 状态 + provider / 模型 + 未注册原因，
// 末行汇总；注册判定所需的 provider 名集由命令面自己经 LoadConfig 装载，并以**显式
// 参数**传进端口（§2.4 注 2——适配器不另读一次配置）。
// Acceptance point: list renders every entry with its registration verdict and the
// summary line; the provider name set travels into the port as an explicit argument.
func TestProfileList_RendersRegisteredAndRejected(t *testing.T) {
	t.Setenv("BASALT_ALPHA_KEY", "resolved-alpha-9f2a")
	t.Setenv("BASALT_BETA_KEY", "resolved-beta-7c31")

	dir := initProfileWorkspace(t)
	writeProviderConfig(t, dir, `providers:
  - name: alpha
    base_url: https://alpha.example.com/v1
    api_key: ${BASALT_ALPHA_KEY}
    default_model: m-alpha
  - name: beta
    base_url: https://beta.example.com
    api_key: ${BASALT_BETA_KEY}
`)

	fake := &fakeAgents{listEntries: []core.AgentEntry{
		{
			Name:       "compat-check",
			Registered: true,
			Profile:    &core.Profile{Provider: core.ProviderRef{Name: "alpha", Model: "m-alpha"}},
		},
		{
			Name:         "broken-agent",
			Registered:   false,
			RejectReason: "invalid schedule: Cron expression must consist of 6 fields",
		},
	}}

	run := executeProfile(t, profileDeps(dir, fake), "profile", "list")
	require.NoError(t, run.err, "正常形态必须 exit 0；stderr=%s", run.stderr)

	// 渲染逐字对齐 §6 固定口径 3：两条状态行 + 汇总行。
	require.Contains(t, run.stdout, "  compat-check：registered=true（provider alpha / 模型 m-alpha）")
	require.Contains(t, run.stdout,
		"  broken-agent：registered=false——invalid schedule: Cron expression must consist of 6 fields")
	require.Contains(t, run.stdout, "汇总：2 个 Agent 目录（已注册 1）")

	require.Equal(t, filepath.Join(dir, workspaceDirName), fake.listRoot, "端口收到的必须是工作区根")
	require.Equal(t, []string{"alpha", "beta"}, fake.listProviders, "provider 名集按配置顺序显式传入")
}

// 中文验收点（A7）：未初始化工作区 → 失败并**点名路径 + 指引 basalt init**；
// 轻路径不代建骨架、不进端口（同一个工作区路径被 status 复用，list 不得顺手把它造出来）。
// Acceptance point: an uninitialized workspace fails with the path and the next step,
// and the light path creates nothing.
func TestProfileList_FailsWhenWorkspaceMissing(t *testing.T) {
	dir := t.TempDir() // 刻意不建 .basalt/agents
	fake := &fakeAgents{}

	run := executeProfile(t, profileDeps(dir, fake), "profile", "list")
	require.Error(t, run.err, "未初始化工作区必须失败")

	agentsDir := filepath.Join(dir, workspaceDirName, "agents")
	require.Contains(t, run.err.Error(), agentsDir, "必须点名缺失路径")
	require.Contains(t, run.err.Error(), "basalt init", "必须给出下一步指引")
	require.Contains(t, run.logs, agentsDir, "日志轨同样点名路径")
	require.Empty(t, fake.listRoot, "未初始化时不得进入装载（零调用）")
	require.NoDirExists(t, filepath.Join(dir, workspaceDirName), "list 不得代建工作区")
}

// 中文验收点（A7）：缺配置文件 → **不计失败**：输出明示"按未注册处理"，空名集照常装载
// （R1 裁决 5）；同一装载 switch 的第三支——配置**存在但无效** → 原错误上抛（点名键路径
// 与变量名、不含凭证值）。
// Acceptance point: a missing configuration degrades to "unregistered" without
// failing, while an invalid one propagates the original error.
func TestProfileList_WithoutConfigStillListsWithNotice(t *testing.T) {
	dir := initProfileWorkspace(t) // 刻意不写 basalt.yaml
	fake := &fakeAgents{listEntries: []core.AgentEntry{{
		Name:         "compat-check",
		Registered:   false,
		RejectReason: "provider not registered: alpha",
	}}}

	run := executeProfile(t, profileDeps(dir, fake), "profile", "list")
	require.NoError(t, run.err, "缺配置不计失败；stderr=%s", run.stderr)

	require.Contains(t, run.stdout, "未发现配置", "缺配置必须明示（可诊断）")
	require.Contains(t, run.stdout, "按未注册处理", "口径逐字：配置缺失按未注册处理")
	require.Empty(t, fake.listProviders, "缺配置 → 空名集显式传入")
	require.Contains(t, run.stdout, "汇总：1 个 Agent 目录（已注册 0）")

	// 第三支：配置存在但无效。
	writeProviderConfig(t, dir, `providers:
  - name: broken
    base_url: https://broken.example.com/v1
    api_key: ${BASALT_ABSENT_VAR_9F2A}
`)
	brokenFake := &fakeAgents{}
	broken := executeProfile(t, profileDeps(dir, brokenFake), "profile", "list")

	require.Error(t, broken.err, "无效配置必须失败（不按未注册宽容）")
	require.Contains(t, broken.err.Error(), "providers[0].api_key", "原错误上抛：点名键路径")
	require.Contains(t, broken.err.Error(), "BASALT_ABSENT_VAR_9F2A", "原错误上抛：点名变量名")
	require.Empty(t, brokenFake.listRoot, "配置无效时不得进入装载")
}

// 中文验收点（A7）：`show` 复用同一结果集过滤（不另立单只装载路径）——名字不在目录
// 清单里 → 失败点名，且不得打出任何声明面。
// Acceptance point: show filters the same result set and fails by name for an
// unknown agent.
func TestProfileShow_UnknownNameFails(t *testing.T) {
	dir := initProfileWorkspace(t)
	fake := &fakeAgents{listEntries: []core.AgentEntry{{
		Name:       "compat-check",
		Registered: true,
		Profile:    &core.Profile{Provider: core.ProviderRef{Name: "alpha", Model: "m-alpha"}},
	}}}

	run := executeProfile(t, profileDeps(dir, fake), "profile", "show", "ghost-agent")
	require.Error(t, run.err, "目录清单里没有的名字必须失败点名")

	require.Contains(t, run.err.Error(), "ghost-agent")
	require.Contains(t, run.logs, "ghost-agent", "日志轨同样点名")
	require.NotContains(t, run.stdout, "registered=true", "失败路径不得打出别人的声明面")
	require.Equal(t, filepath.Join(dir, workspaceDirName), fake.listRoot, "show 走同一条装载（同一结果集过滤）")
}

// 中文验收点（A7）：`show` 打出该 Agent 的声明面——固定状态行（§6 固定口径 3）在前，
// 声明字段随后；未注册但解析成功的条目仍展示声明面（§2.4 注 3——provider / 模型可见）。
// Acceptance point: show prints the declaration face, including that of an
// unregistered-but-parsed entry.
func TestProfileShow_RendersDeclaredFields(t *testing.T) {
	dir := initProfileWorkspace(t)
	fake := &fakeAgents{listEntries: []core.AgentEntry{{
		Name:       "compat-check",
		Registered: true,
		Profile: &core.Profile{
			Identity:  core.Identity{AgentName: "compat-check"},
			Provider:  core.ProviderRef{Name: "alpha", Model: "m-alpha"},
			Skills:    []string{"contract-check"},
			Tools:     []string{"read_file"},
			Bootstrap: []string{"AGENTS.md", "SOUL.md"},
			Schedules: []core.Schedule{{ID: "compat-check-1", Cron: "0 30 9 * * *", Zone: "UTC"}},
		},
	}}}

	run := executeProfile(t, profileDeps(dir, fake), "profile", "show", "compat-check")
	require.NoError(t, run.err, "stderr=%s", run.stderr)

	require.Contains(t, run.stdout, "  compat-check：registered=true（provider alpha / 模型 m-alpha）",
		"与 list 共用同一固定行")
	for _, want := range []string{"contract-check", "read_file", "AGENTS.md", "SOUL.md", "compat-check-1", "0 30 9 * * *"} {
		require.Containsf(t, run.stdout, want, "声明面须展示 %s", want)
	}

	// 未注册但 frontmatter 解析成功：声明面照常可见，原因行点名（§2.4 注 3）。
	rejected := &fakeAgents{listEntries: []core.AgentEntry{{
		Name:         "broken-agent",
		Registered:   false,
		Profile:      &core.Profile{Provider: core.ProviderRef{Name: "alpha", Model: "m-alpha"}, Skills: []string{"contract-check"}},
		RejectReason: "invalid schedule: Cron expression must consist of 6 fields",
	}}}
	rejectedRun := executeProfile(t, profileDeps(dir, rejected), "profile", "show", "broken-agent")
	require.NoError(t, rejectedRun.err, "show 不是注册判定——未注册也可展示；stderr=%s", rejectedRun.stderr)
	require.Contains(t, rejectedRun.stdout, "  broken-agent：registered=false——invalid schedule: Cron expression must consist of 6 fields")
	require.Contains(t, rejectedRun.stdout, "contract-check", "未注册条目的声明面照常展示")
}

// 中文验收点（A8）：create 走**写模式端口**并把命令面装载出的 provider 名集显式传入；
// 输出明示选中了谁（§6 的 R1 裁决 3）；端口错误（拒锁 / 已存在 / 名字非法）原样上抛，
// 命令层不吞、不改写成新语义，失败路径不打印创建条目。
// Acceptance point: create reaches the write-mode port, announces the selected
// provider, and propagates port errors unchanged.
func TestProfileCreate_CommandUsesWriteModePort(t *testing.T) {
	t.Setenv("BASALT_ALPHA_KEY", "resolved-alpha-9f2a")
	t.Setenv("BASALT_BETA_KEY", "resolved-beta-7c31")

	dir := initProfileWorkspace(t)
	writeProviderConfig(t, dir, `providers:
  - name: alpha
    base_url: https://alpha.example.com/v1
    api_key: ${BASALT_ALPHA_KEY}
    default_model: m-alpha
  - name: beta
    base_url: https://beta.example.com
    api_key: ${BASALT_BETA_KEY}
`)
	fake := &fakeAgents{created: []string{"agents/demo-agent/AGENT.md"}}

	run := executeProfile(t, profileDeps(dir, fake), "profile", "create", "demo-agent")
	require.NoError(t, run.err, "stderr=%s", run.stderr)

	require.Equal(t, filepath.Join(dir, workspaceDirName), fake.createRoot, "写模式的根是工作区，不是工作目录")
	require.Equal(t, "demo-agent", fake.createName)
	require.Equal(t, []string{"alpha", "beta"}, fake.createProviders, "provider 名集显式传入（适配器不另读配置）")
	require.Contains(t, run.stdout, "已选择 provider：alpha（配置首项）", "输出必须明示选中了谁")
	require.Contains(t, run.stdout, "  创建：agents/demo-agent/AGENT.md", "created 条目逐条打印")

	// 端口错误原样上抛：拒锁文案（点名 pid / 开始时间、指引 REST / 管理台）不得被改写。
	busy := errors.New("工作区实例锁已被占用：pid=4242，开始于 2026-10-16T09:30:00Z——另一个活实例正在运行，" +
		"写模式命令拒绝启动（服务在跑时请改用 REST / 管理台）")
	locked := executeProfile(t, profileDeps(dir, &fakeAgents{createErr: busy}), "profile", "create", "demo-agent")
	require.Error(t, locked.err, "拒锁必须失败（拒绝启动）")
	require.Contains(t, locked.err.Error(), "pid=4242", "原错误上抛")
	require.Contains(t, locked.err.Error(), "REST", "原错误上抛（指引随文案一起上抛）")
	require.NotContains(t, locked.stdout, "  创建：", "失败路径不得打印创建条目")
	require.Contains(t, locked.logs, "demo-agent", "写入失败必须留痕")
}

// 中文验收点（A8）：无 provider 配置 → 模板写占位文本并**说明尚未注册**（不计失败）；
// 空名集显式传入端口，且输出不得谎称选中了谁。
// Acceptance point: without configured providers the template is a placeholder and
// the command says so instead of claiming a selection.
func TestProfileCreate_WithoutProvidersNotesUnregisteredTemplate(t *testing.T) {
	dir := initProfileWorkspace(t) // 刻意不写 basalt.yaml
	fake := &fakeAgents{created: []string{"agents/demo-agent/AGENT.md"}}

	run := executeProfile(t, profileDeps(dir, fake), "profile", "create", "demo-agent")
	require.NoError(t, run.err, "缺配置不计失败（占位文本照常落盘）；stderr=%s", run.stderr)

	require.Equal(t, "demo-agent", fake.createName)
	require.Empty(t, fake.createProviders, "缺配置 → 空名集（模板用占位文本）")
	require.Contains(t, run.stdout, "未发现 provider 配置")
	require.Contains(t, run.stdout, "占位文本")
	require.Contains(t, run.stdout, "不会注册", "必须说明补齐配置前该 Agent 不会注册")
	require.NotContains(t, run.stdout, "已选择 provider", "无配置时不得谎称选中了谁")
}

// 中文验收点（A9）：delete 走**写模式端口**；命令入口取真实时钟（cobra 层 time.Now()），
// 而 run 级入口的 now 是显式参数——注入固定时刻必须原样到达端口（归档名含时间戳，
// 时间显式传入是可判卷前提，§2.4 注 11）；缺目录错误原样上抛。
// Acceptance point: delete reaches the write-mode port; the injected instant reaches
// it unchanged at the run level, while the cobra entry supplies the real clock.
func TestProfileDelete_CommandUsesWriteModePort(t *testing.T) {
	dir := initProfileWorkspace(t)
	const archived = "archive/demo-agent-20261016-093000"

	fake := &fakeAgents{archived: archived}
	run := executeProfile(t, profileDeps(dir, fake), "profile", "delete", "demo-agent")
	require.NoError(t, run.err, "stderr=%s", run.stderr)

	require.Equal(t, filepath.Join(dir, workspaceDirName), fake.deleteRoot, "写模式的根是工作区")
	require.Equal(t, "demo-agent", fake.deleteName)
	require.False(t, fake.deleteNow.IsZero(), "命令入口取真实时钟")
	require.Contains(t, run.stdout, "  归档："+archived, "归档目标逐字打印")

	// run 级入口：注入固定时刻 → 端口收到的就是它（不是函数内自取的时间）。
	fixed := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
	directFake := &fakeAgents{archived: archived}
	var out bytes.Buffer
	require.NoError(t, runProfileDelete(&out, profileDeps(dir, directFake), "demo-agent", fixed))
	require.Equal(t, fixed, directFake.deleteNow, "注入的时刻必须原样到达端口")
	require.Contains(t, out.String(), "  归档："+archived)

	// 缺目录 / 撞名 → 端口错误原样上抛，且不打印归档条目。
	missing := errors.New("待归档目录不存在：agents/ghost-agent")
	bad := executeProfile(t, profileDeps(dir, &fakeAgents{deleteErr: missing}), "profile", "delete", "ghost-agent")
	require.Error(t, bad.err, "缺目录必须失败")
	require.Contains(t, bad.err.Error(), "ghost-agent")
	require.NotContains(t, bad.stdout, "  归档：", "失败路径不得打印归档条目")
}
