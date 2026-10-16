package cli

// 本文件是 `session` 命令组（颗粒度文档 §3.1 cli 表 + §6.2 固定口径 5）：`list` 是**只读
// 命令**——不取实例锁、不做崩溃恢复、不清理锁文件。`schema_version` 校验发生在**打开库
// 之前**（契约 §4.3.1），落位在装配层的只读路径上：命令面只经端口取数，端口给不出库就
// 给不出数据（版本不匹配的错误经端口上抛，本层如实转述）。
// The session command group. list is the read-only command: no instance lock, no crash
// recovery, no lock-file cleanup; the schema_version verdict happens ahead of opening the
// database, inside the assembly layer's read-only path.

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/internal/core"
)

// sessionTimeLayout 是"最后活动"的展示形态（§6.2 固定口径 5 逐字：2006-01-02 15:04:05），
// 时刻按 UTC 渲染（裁决落位见 renderSessionList 注释）。
// sessionTimeLayout is the fixed rendering of "last active", verbatim, in UTC.
const sessionTimeLayout = "2006-01-02 15:04:05"

// newSessionCmd 组装 session 命令组。
// newSessionCmd builds the session command group.
func newSessionCmd(deps Deps) *cobra.Command {
	session := &cobra.Command{
		Use:   "session",
		Short: "管理会话（list，只读）",
		Long: "管理会话：list 只读列出全部会话（一行一会话 + 末尾汇总）。\n" +
			"只读命令不取实例锁、不做崩溃恢复、不清理锁文件——因此可以在别的实例持锁时使用。\n" +
			"Manage sessions. list is read-only: no instance lock, no recovery, no cleanup.",
		// 与根命令 / provider / profile 组同一形态：可运行的父命令 + 显式未知命令分支。
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("未知命令 %q：用 --help 查看可用命令", args[0])
			}
			return cmd.Help()
		},
	}
	session.AddCommand(newSessionListCommand(deps))
	return session
}

// newSessionListCommand 组装 session list。
// newSessionListCommand builds session list.
func newSessionListCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出全部会话（只读）",
		Long: "逐条列出全部会话：一行一会话（标识、消息数、最后活动时刻），末尾汇总。\n" +
			"时刻按 UTC 显示（库内一律 UTC 存储；不做本地时区换算）。\n" +
			"只读路径：不取实例锁、不做崩溃恢复、不清理锁文件。\n" +
			"List every session, one line each, with a trailing summary. Times are shown in\n" +
			"UTC — no local-timezone conversion. Read-only path.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSessionList(cmd.Context(), cmd.OutOrStdout(), deps)
		},
	}
}

// runSessionList 读会话清单并渲染。整条路径只有**一个只读调用**：命令面没有第二个出口，
// 也就不可能顺手写出"列个清单顺带清理一下"这类动作（FR-024 / §8.7 轻路径）。
// runSessionList reads the session list and renders it. The path makes exactly one
// read-only call and has no second exit, so no incidental write can ever sneak in.
func runSessionList(ctx context.Context, out io.Writer, deps Deps) error {
	if deps.SessionLister == nil {
		// 装配层没注入端口 = 命令面拿不到任何数据源；这属装配缺陷，明说而不是静默空转。
		return fmt.Errorf("会话端口未注入——session list 需要 SessionLister（装配层 internal/app 提供）")
	}

	sessions, err := deps.SessionLister.ListSessions(ctx)
	if err != nil {
		// 只读路径的失败一律原样上抛（版本不匹配等哨兵错误经 %w 保链，调用方与测试都能
		// errors.Is 判定）——命令层不改写实现层的语义。
		deps.Logger.Error("session list 未通过：读取会话清单失败", "error", err)
		return fmt.Errorf("读取会话清单失败: %w", err)
	}

	renderSessionList(out, sessions)
	return nil
}

// renderSessionList 逐行渲染 + 末尾汇总（§6.2 固定口径 5 逐字）：一行一会话
// `<id>：<消息数> 条（最后活动 <时间>）`，末尾 `共 N 个会话`，空库只有 `未发现会话`。
// renderSessionList renders one line per session plus the trailing summary, verbatim.
//
// 两处取字面量的口径：① 空库**不**打"共 0 个会话"——口径 5 把空库文案与汇总写成并列的
// 两种收场，叠加会给出两行同义文本；② 顺序沿用端口返回的顺序（排序是实现侧职责，
// 命令面重排会把"库给的顺序"与"命令面给的顺序"变成两个来源）。
// An empty database prints the fixed line only, and the port's order is preserved.
//
// 时刻一律按 **UTC** 呈现（`.UTC()`，2026-10-16 用户裁决）：对时间值调 `Local()` 与直接
// 引用 `time` 包的 `Local` 是同一件事——语义上都依赖进程本地时区，只是前者躲得过字面
// grep（工程规范 §1.6 时区纪律；本行刻意不写禁用字面量——注释命中会污染不变量 7 的判据，
// 同陷阱 #47 的 nosec 约定）。库内本就 UTC 入库（迁移纪律 4），展示侧原样透出即可。
// Instants render in UTC, never process-local: calling Local() on a time value is the
// same offence under a different spelling (library times are UTC; standards §1.6).
func renderSessionList(out io.Writer, sessions []core.SessionRecord) {
	if len(sessions) == 0 {
		fmt.Fprintln(out, "未发现会话")
		return
	}
	for _, session := range sessions {
		fmt.Fprintf(out, "%s：%d 条（最后活动 %s）\n",
			session.ID, session.MessageCount, session.LastActiveAt.UTC().Format(sessionTimeLayout))
	}
	fmt.Fprintf(out, "共 %d 个会话\n", len(sessions))
}
