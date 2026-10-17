package cli

// 本文件是 `profile` 命令组（颗粒度文档 §2.3 流程 C）：list / show 走**轻路径**
// （只读文件系统——不开库、不取实例锁、不做恢复），create / delete 是**写模式**
// （实现侧先取实例锁，被活实例持有时拒绝启动）。注册判定与装载共用同一条代码路径：
// 命令面只把 provider 名集与工作区根**显式**传给端口，不另立解析路径（不得写死假数据）。
// The profile command group: list / show are the read-only light path, while create /
// delete write through the instance lock taken inside the implementation. The
// registration verdict always comes from the port, never from a second parser here.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/internal/core"
)

// agentsDirName 是工作区里 Agent 目录的所在层（骨架清单的一员）。cli **不得** import
// workspace，故此处按同一份骨架口径重复字面量——与 configFileName 的情形相同：改动
// 时两处都要跟（工作区侧的唯一定义在 workspace/templates.go 的骨架清单里）。
// agentsDirName mirrors the workspace skeleton entry. cli must not import the
// implementation package, so the literal is repeated here deliberately.
const agentsDirName = "agents"

// newProfileCommand 组装 profile 命令组：list / show / create / delete 四条子命令。
// newProfileCommand builds the profile command group.
func newProfileCommand(deps Deps) *cobra.Command {
	profile := &cobra.Command{
		Use:   "profile",
		Short: "管理 Agent 目录与 Profile（list / show / create / delete）",
		Long: "管理 Agent 目录与 Profile：list / show 只读（轻路径，不开库、不取锁），\n" +
			"create / delete 写工作区（写模式命令，取工作区实例锁）。\n" +
			"Manage the agent directories: list / show are read-only, create / delete " +
			"write the workspace under the instance lock.",
		// 与根命令 / provider 组同一形态：可运行的父命令 + 显式未知命令分支（不可运行的
		// 父命令会让 cobra 把未知子命令按"请求帮助"处理并以 0 退出）。
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("未知命令 %q：用 --help 查看可用命令", args[0])
			}
			return cmd.Help()
		},
	}
	profile.AddCommand(
		newProfileListCommand(deps),
		newProfileShowCommand(deps),
		newProfileCreateCommand(deps),
		newProfileDeleteCommand(deps),
	)
	return profile
}

// newProfileListCommand 组装 profile list：轻路径（只读文件系统）。
// newProfileListCommand builds profile list, the read-only light path.
func newProfileListCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出全部 Agent 目录与注册状态（只读）",
		Long: "逐条列出 .basalt/agents/ 下的 Agent 目录：注册状态、provider 与模型、\n" +
			"未注册原因，末行汇总。只读文件系统：不开库、不取实例锁。\n" +
			"List every agent directory with its registration verdict; read-only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProfileList(cmd.OutOrStdout(), deps)
		},
	}
}

// newProfileShowCommand 组装 profile show：轻路径；复用 list 的同一装载结果过滤。
// newProfileShowCommand builds profile show, filtering the very same load result.
func newProfileShowCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "show <agent>",
		Short: "展示一个 Agent 的装载结果与声明面（只读）",
		Long: "展示一个 Agent 目录的装载结果与声明面字段（同一份注册判定代码，\n" +
			"不为命令面另立解析路径）。只读文件系统：不开库、不取实例锁。\n" +
			"Show one agent's load result and declaration face; read-only.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProfileShow(cmd.OutOrStdout(), deps, args[0])
		},
	}
}

// newProfileCreateCommand 组装 profile create：写模式（实现侧取实例锁）。
// newProfileCreateCommand builds profile create, a write-mode command.
func newProfileCreateCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "create <agent>",
		Short: "创建一个可装载的 Agent 模板（写模式）",
		Long: "在 .basalt/agents/<agent>/ 写入一份可装载的最小 AGENT.md 模板：\n" +
			"已存在的目录拒绝覆盖，模板里的 provider 取自配置首项。\n" +
			"Create a loadable agent template; existing directories are never overwritten.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProfileCreate(cmd.OutOrStdout(), deps, args[0])
		},
	}
}

// newProfileDeleteCommand 组装 profile delete：写模式；真实时钟只在这一层取一次。
// newProfileDeleteCommand builds profile delete; the real clock is read exactly
// here, so everything below stays testable with an injected instant.
func newProfileDeleteCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <agent>",
		Short: "归档一个 Agent 目录（写模式）",
		Long: "把 .basalt/agents/<agent>/ 归档进 archive/<agent>-<时间戳>/（单次原子移动）；\n" +
			"会话 / 记忆 / 审计数据不动。\n" +
			"Archive the agent directory; session, memory and audit data stay untouched.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// 归档名含时间戳 ⇒ 时间是持久副作用，显式传入（§2.4 注 11）。
			return runProfileDelete(cmd.OutOrStdout(), deps, args[0], time.Now())
		},
	}
}

// profileWorkspaceRoot 解析工作区根并做**轻量初始化判据**：`.basalt/agents/` 存在即可
// （工作区 = <WorkDir>/.basalt，与 status 同一路径口径——不从配置取）。未初始化 → 点名
// 路径 + 指引 `basalt init`（与 provider list 同为「点名型」）；四条子命令共用，写模式
// 命令因而在取锁之前就被拦下（零写入）。
// profileWorkspaceRoot resolves the workspace and checks the one directory the agent
// commands need; every subcommand shares it, so write-mode commands are refused
// before the lock is even taken.
func profileWorkspaceRoot(deps Deps) (string, error) {
	wsRoot := filepath.Join(deps.WorkDir, workspaceDirName)
	agentsDir := filepath.Join(wsRoot, agentsDirName)

	if _, err := os.Stat(agentsDir); err != nil {
		failure := fmt.Errorf("工作区未初始化：缺少目录 %s——先执行 basalt init: %w", agentsDir, err)
		deps.Logger.Error("profile 未通过：工作区未初始化", "path", agentsDir, "error", err)
		return "", failure
	}
	return wsRoot, nil
}

// profileProviderNames 装载已注册 provider 名集——它是注册判定的**显式输入**（§2.4 注 2）：
// 命令面自己读配置（cli 的既有职责），经端口参数传入；适配器不另读一次配置（同一文件
// 两次读取正是陷阱形态）。缺配置 → 空名集 + 说明文案（按未注册处理、**不计失败**）；
// 配置无效 → 原错误上抛（装载层已点名键路径与变量名，且不含凭证值）。
// profileProviderNames loads the registered provider names, the explicit input of the
// registration rule; the adapter never re-reads the configuration file.
func profileProviderNames(deps Deps) ([]string, string, error) {
	path := filepath.Join(deps.WorkDir, configFileName)

	cfg, err := LoadConfig(path, os.LookupEnv)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Sprintf("未发现配置（%s）——provider 注册判定按未注册处理", path), nil
	case err != nil:
		deps.Logger.Error("profile 未通过：配置无效", "path", path, "error", err)
		return nil, "", err
	}

	names := make([]string, 0, len(cfg.Providers))
	for _, provider := range cfg.Providers {
		names = append(names, provider.Name)
	}
	return names, "", nil
}

// runProfileList 装载全部 Agent 目录并逐条渲染（§2.3 流程 C）。空清单是**成功**：
// 说明未发现 Agent 目录即可（R1 裁决 5——"没有 Agent"对环境准备而言不是错误）。
// runProfileList loads every agent directory and renders it; an empty listing still
// succeeds, with a line saying so.
func runProfileList(out io.Writer, deps Deps) error {
	wsRoot, err := profileWorkspaceRoot(deps)
	if err != nil {
		return err
	}

	providers, configNotice, err := profileProviderNames(deps)
	if err != nil {
		return err
	}
	if configNotice != "" {
		fmt.Fprintln(out, configNotice)
	}

	entries, err := deps.Agents.List(wsRoot, providers)
	if err != nil {
		failure := fmt.Errorf("装载 Agent 目录失败（%s）: %w", wsRoot, err)
		deps.Logger.Error("profile list 未通过：装载失败", "path", wsRoot, "error", err)
		return failure
	}
	if len(entries) == 0 {
		fmt.Fprintf(out, "未发现 Agent 目录（%s）——把 AGENT.md 放进 %s/<name>/ 后再执行 profile list\n",
			wsRoot, agentsDirName)
	}

	renderProfileList(out, entries)
	return nil
}

// runProfileShow 过滤同一份装载结果（不另立单只装载路径）；名字不在目录清单 → 失败点名。
// runProfileShow filters the very same load result; an unknown name fails loudly.
func runProfileShow(out io.Writer, deps Deps, name string) error {
	wsRoot, err := profileWorkspaceRoot(deps)
	if err != nil {
		return err
	}

	providers, configNotice, err := profileProviderNames(deps)
	if err != nil {
		return err
	}
	if configNotice != "" {
		fmt.Fprintln(out, configNotice)
	}

	entries, err := deps.Agents.List(wsRoot, providers)
	if err != nil {
		failure := fmt.Errorf("装载 Agent 目录失败（%s）: %w", wsRoot, err)
		deps.Logger.Error("profile show 未通过：装载失败", "agent", name, "path", wsRoot, "error", err)
		return failure
	}

	for _, entry := range entries {
		if entry.Name == name {
			renderProfileShow(out, entry)
			return nil
		}
	}

	failure := fmt.Errorf("未发现 Agent 目录：%s（工作区 %s）——先执行 profile list 查看已有目录", name, wsRoot)
	deps.Logger.Error("profile show 未通过：目录不存在", "agent", name, "path", wsRoot)
	return failure
}

// runProfileCreate 走写模式端口：输出**明示选中了谁**（配置首项）；无 provider 可选时
// 模板写占位文本并说明尚未注册（§2.3 流程 C / R1 裁决 3）。端口错误（拒锁 / 已存在 /
// name 非法）原样上抛——命令层不改写实现层的语义。
// runProfileCreate announces which provider was selected and propagates port errors
// unchanged.
func runProfileCreate(out io.Writer, deps Deps, name string) error {
	wsRoot, err := profileWorkspaceRoot(deps)
	if err != nil {
		return err
	}

	providers, missingConfigNotice, err := profileProviderNames(deps)
	if err != nil {
		return err
	}
	switch {
	case len(providers) > 0:
		fmt.Fprintf(out, "已选择 provider：%s（配置首项）\n", providers[0])
	case missingConfigNotice != "":
		fmt.Fprintln(out, "未发现 provider 配置——模板里的 provider.name 是占位文本，补齐配置前该 Agent 不会注册")
	default:
		// 配置在、providers 段为空：同样走占位文本路径——"没有 provider 可选"不是写模式的失败。
		fmt.Fprintln(out, "providers 段为空——模板里的 provider.name 是占位文本，补齐配置前该 Agent 不会注册")
	}

	created, err := deps.Agents.Create(wsRoot, name, providers)
	if err != nil {
		deps.Logger.Error("profile create 未通过：工作区写入被拒", "agent", name, "path", wsRoot, "error", err)
		return err
	}
	for _, relPath := range created {
		fmt.Fprintf(out, "  创建：%s\n", relPath)
	}
	return nil
}

// runProfileDelete 走写模式端口的归档路径；now 由调用方显式传入（可判卷）。失败原样
// 上抛，成功打印归档目标（供人手工核对归档在哪里）。
// runProfileDelete archives through the write-mode port with the caller's clock.
func runProfileDelete(out io.Writer, deps Deps, name string, now time.Time) error {
	wsRoot, err := profileWorkspaceRoot(deps)
	if err != nil {
		return err
	}

	archived, err := deps.Agents.Delete(wsRoot, name, now)
	if err != nil {
		deps.Logger.Error("profile delete 未通过：工作区写入被拒", "agent", name, "path", wsRoot, "error", err)
		return err
	}
	fmt.Fprintf(out, "  归档：%s\n", archived)
	return nil
}

// renderProfileList 逐条渲染 + 汇总行——两行状态行与汇总行的措辞是**固定口径**
// （§6 第 3 项），改措辞等于改判据。US-7 增第三态后缀：**未验证数 > 0** 时才追加
// `/ 未验证 %d`（斜杠前一个空格）；零计数时汇总行与 US-3 逐字节相同（轻路径在零 MCP
// 配置下的行为承诺）。
// renderProfileList renders every entry plus the summary line, verbatim per §6; the
// unverified suffix appears only when the count is positive, keeping the zero case
// byte-identical to US-3.
func renderProfileList(out io.Writer, entries []core.AgentEntry) {
	for _, entry := range entries {
		renderProfileLine(out, entry)
	}
	fmt.Fprintf(out, "汇总：%d 个 Agent 目录（已注册 %d）", len(entries), registeredCount(entries))
	if unverified := unverifiedCount(entries); unverified > 0 {
		fmt.Fprintf(out, " / 未验证 %d", unverified)
	}
	fmt.Fprintln(out)
}

// renderProfileLine 是 list / show 共用的状态行（§6 固定口径 3）：注册行带 provider 与
// 模型（取自声明面），未注册行带原因（契约字面量原样 + detail 的人读合并，§6 第 6 项）。
// 第三态 `unverified` 是轻路径专属（重路径两名单齐备、不产生）——行文固定，不带原因：
// 「待确认」不是拒载，写成 `registered=false——` 会让运维把它当失败处理。
// renderProfileLine is the line list and show share; the unverified state is the light
// path's third state and must not degrade into the rejected line.
func renderProfileLine(out io.Writer, entry core.AgentEntry) {
	if entry.Unverified {
		fmt.Fprintf(out, "  %s：registered=unverified——MCP 工具与连接需重路径确认\n", entry.Name)
		return
	}
	if !entry.Registered {
		fmt.Fprintf(out, "  %s：registered=false——%s\n", entry.Name, entry.RejectReason)
		return
	}

	// Registered ⇒ Profile 非 nil（§2.4 注 3：注册蕴含 frontmatter 解析成功）；取值前
	// 仍判一次 nil 只作纵深防御——展示层既不 panic，也不谎报 registered 状态。
	providerName, model := "", ""
	if entry.Profile != nil {
		providerName, model = entry.Profile.Provider.Name, entry.Profile.Provider.Model
	}
	fmt.Fprintf(out, "  %s：registered=true（provider %s / 模型 %s）\n", entry.Name, providerName, model)
}

// registeredCount 数已注册的条目（汇总行的第二个数）。语义不变：只数 `Registered`——
// 第三态 `unverified` 的 `Registered` 恒 false，不混进"已注册"。
// registeredCount counts the registered entries for the summary line; the third state
// never counts as registered.
func registeredCount(entries []core.AgentEntry) int {
	count := 0
	for _, entry := range entries {
		if entry.Registered {
			count++
		}
	}
	return count
}

// unverifiedCount 数第三态条目（汇总行后缀的判据来源）。
// unverifiedCount counts the unverified entries for the summary suffix.
func unverifiedCount(entries []core.AgentEntry) int {
	count := 0
	for _, entry := range entries {
		if entry.Unverified {
			count++
		}
	}
	return count
}

// renderProfileShow 打出一个 Agent 的**声明面**：固定状态行在前，声明字段随后
// （身份 / 技能 / 工具 / bootstrap / 定时）。frontmatter 未解析成功（Profile 为 nil）
// 时只有状态行——那一行已经把原因说清了，不另编造"缺声明面"的文案。
// renderProfileShow prints the status line first and the declared fields after it.
func renderProfileShow(out io.Writer, entry core.AgentEntry) {
	renderProfileLine(out, entry)
	if entry.Profile == nil {
		return
	}

	profile := entry.Profile
	fmt.Fprintf(out, "  身份：agent_name=%s\n", profile.Identity.AgentName)
	fmt.Fprintf(out, "  技能：%s\n", declaredList(profile.Skills))
	fmt.Fprintf(out, "  工具：%s\n", declaredList(profile.Tools))
	fmt.Fprintf(out, "  bootstrap：%s\n", declaredList(profile.Bootstrap))
	for _, schedule := range profile.Schedules {
		fmt.Fprintf(out, "  定时：%s（%s，%s）\n", schedule.ID, schedule.Cron, schedule.Zone)
	}
}

// declaredList 渲染声明的列表；空列表写「无」而不是留白——"声明为空"与"未声明"的区分
// 在解析期（§2.2 约束 5），展示层不重复示意。
// declaredList renders a declared list; an empty one reads as 无 instead of blank.
func declaredList(items []string) string {
	if len(items) == 0 {
		return "无"
	}
	return strings.Join(items, "、")
}
