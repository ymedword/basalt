package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// 工作区路径约定：工作区 = <WorkDir>/.basalt，库文件在工作区**内**。init 与 status
// 共用这一份口径——一份报告里的骨架检查与库检查出自同一路径，不从配置取
// （颗粒度文档 §2.4 取数澄清；Operations §1.1 / §5.3）。
// The workspace is <WorkDir>/.basalt and the database lives inside it; both
// commands share this convention rather than reading it from the configuration.
const (
	workspaceDirName = ".basalt"
	databaseFileName = "basalt.db"
)

// newInitCommand 组装 init 子命令：在工作目录创建 .basalt/ 骨架与库文件。
// newInitCommand builds the init subcommand.
//
// 边界（契约 §4.3.1 / 颗粒度文档 §2.4）：init **不取实例锁、不做恢复、不触碰锁
// 文件**，也不读配置（流程 A 与流程 C 的分工）；它对数据库的写入被限定为引导创建
// ——已存在的库一律零写，升级是持锁重命令的显式动作（US-4 接线）。
// init takes no instance lock, performs no recovery, touches no lock file and
// reads no configuration; its only database write is bootstrap creation.
func newInitCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "在当前目录初始化工作区（幂等）",
		Long: "在当前目录初始化工作区（幂等）：创建 .basalt/ 骨架与 basalt.db。\n" +
			"Initialize the workspace in the current directory (idempotent): creates " +
			"the .basalt/ skeleton and basalt.db.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd.Context(), cmd.OutOrStdout(), deps)
		},
	}
}

// runInit 依次落两步——先骨架后库，再回报结果清单。
// runInit runs the two steps in order and reports what happened.
//
// 失败路径的可读文案与 ERROR 日志分属两处、都不缺：本函数把错误**上抛**（不吞、
// 不在此 return 前静默），cobra 打印可读文案到标准错误，装配层 app.Run 用
// deps.Logger 记结构化 ERROR 并以退出码 1 收口——命令面只有一个退出码出口。
// Errors travel up: cobra prints the readable message, app.Run logs it and owns
// the exit code. Logging here as well would double every failure line.
func runInit(ctx context.Context, out io.Writer, deps Deps) error {
	wsRoot := filepath.Join(deps.WorkDir, workspaceDirName)

	created, kept, err := deps.Workspace.Init(wsRoot)
	if err != nil {
		return fmt.Errorf("初始化工作区 %s: %w", wsRoot, err)
	}

	// 首次准备必须排在引导建库之前（US-25 §2.3.2 / D-25-02）：配置、进度与名册先落盘，
	// 库最后建——反过来会让 `init` 在准备失败时留下"有库、无配置、无身份"的目录，
	// 而 `detectNewInstance` 从此判它是存量实例，首次初始化资格永久丢失。
	// 端口缺失即装配错误；**不得**退回"只建骨架 + 库"的旧行为。
	// The first-install step precedes the bootstrap; a missing port is a wiring error
	// and never a reason to fall back to the old skeleton-plus-database behaviour.
	if deps.FirstInstall == nil {
		return errors.New("首次安装端口未装配：拒绝在缺少 FirstInstall 时建库")
	}
	firstInstall, err := deps.FirstInstall.Prepare(deps.WorkDir)
	if err != nil {
		return fmt.Errorf("首次安装准备失败（未建库，修正后可重试）: %w", err)
	}

	dbPath := filepath.Join(wsRoot, databaseFileName)
	// 引导建的是 **SQLite 源谱系** 的库（v6），不是运行期 PostgreSQL 谱系（v1）——两个
	// 版本常数混用会把一份 v6 schema 标成 v1，`db export` 立刻拒绝导出（T078 实测）。
	// The bootstrap builds the SQLite source lineage (v6), not the runtime PG lineage (v1).
	dbCreated, err := deps.Store.Bootstrap(ctx, dbPath, deps.SourceSchemaVersion)
	if err != nil {
		return fmt.Errorf("初始化数据库 %s: %w", dbPath, err)
	}

	// 输出 Created / Kept 清单：二跑 Created 为空即是幂等证据（流程 A）。
	fmt.Fprintf(out, "工作区 %s\n", wsRoot)
	writeItems(out, "新建", created)
	writeItems(out, "保留", kept)
	fmt.Fprintf(out, "首次安装：%s\n", firstInstallOutcome(firstInstall))
	fmt.Fprintf(out, "数据库 %s：%s（schema v%d）\n", dbPath, databaseOutcome(dbCreated), deps.SourceSchemaVersion)
	return nil
}

// firstInstallOutcome 只报本次真的写了什么；二次运行全为假时明确说"零写"，
// 不把"没有新建"印成空白（与 writeItems 同一理由）。
// firstInstallOutcome reports only what this run actually wrote.
func firstInstallOutcome(report FirstInstallReport) string {
	if !report.Fresh {
		return "已就绪，零写"
	}
	var written []string
	if report.ConfigCreated {
		written = append(written, "配置")
	}
	if report.ProvisioningRecorded {
		written = append(written, "初始化进度")
	}
	if report.MarkerCreated {
		written = append(written, "实例名册")
	}
	if len(written) == 0 {
		return "已就绪，零写"
	}
	return "新建 " + strings.Join(written, " / ") + "，实例 " + report.InstanceID
}

// databaseOutcome 把 Bootstrap 的 created 语义翻成给人看的一句话。
// databaseOutcome renders the created flag as human-readable wording.
func databaseOutcome(created bool) string {
	if created {
		return "已创建"
	}
	return "已存在，零写"
}

// writeItems 打一行清单；空清单也要显式出现——"没有新建"本身就是幂等结论，
// 印成空白会让人分不清"没有"与"没打印"。
// writeItems prints one inventory line; an empty inventory still shows up,
// because "nothing created" is itself the idempotency result.
func writeItems(out io.Writer, label string, items []string) {
	if len(items) == 0 {
		fmt.Fprintf(out, "  %s：（无）\n", label)
		return
	}
	fmt.Fprintf(out, "  %s（%d）：%s\n", label, len(items), strings.Join(items, " "))
}
