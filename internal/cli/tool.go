package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/internal/core"
)

// newToolCommand 组装 tool 命令组：本 US 只交付 list 一条子命令。
// newToolCommand builds the tool command group; this US ships list only.
func newToolCommand(deps Deps) *cobra.Command {
	tool := &cobra.Command{
		Use:   "tool",
		Short: "查看工具注册池（本阶段：list）",
		Long: "查看工具注册池。本阶段只读：list 逐行列出本次装配注册的工具。\n" +
			"Show the tool registry; this phase is read-only and provides the list " +
			"subcommand.",
		// 与根命令同一形态：可运行的父命令 + 显式未知命令分支（不可运行的父命令会
		// 让 cobra 把未知子命令按"请求帮助"处理并以 0 退出）。
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("未知命令 %q：用 --help 查看可用命令", args[0])
			}
			return cmd.Help()
		},
	}
	tool.AddCommand(newToolListCommand(deps))
	return tool
}

// newToolListCommand 组装 tool list。
// newToolListCommand builds tool list.
//
// 重路径（颗粒度文档 §2.4 注 2 的推导链）：答案来自**装配期构建的内存注册池**，
// 池之外根本不存在这份清单——因此它与 chat 同路，启动四步（日志、迁移、恢复、锁）
// 在该路径上照常发生；本命令的「只读」只指**清单读取本身不修改状态**（注 9）。
// A heavy-path command: the answer lives in the assembly-time registry, so the four
// startup steps still happen. Read-only here means the listing itself writes nothing.
func newToolListCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出本次装配注册的工具（名称与描述）",
		Long: "逐行列出注册池里的工具：名称与描述；末尾给出总数。不打印参数 schema。\n" +
			"List the registered tools with their descriptions and a total count. " +
			"Parameter schemas are not printed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runToolList(cmd.OutOrStdout(), deps)
		},
	}
}

// runToolList 经端口取清单后渲染。端口缺失**点名缺件**：命令面不自行构造第二份清单
// （第二份清单要么要 import 工具实现包、要么是一份与装配无关的常量表，两者都是错的），
// 静默成功更不允许——`tool list` 是运维恢复演练判据 4 的读数来源，读数不许造假。
// runToolList renders the port's specs; a missing port is named rather than papered
// over, because this command is a reading source for the operator drill.
func runToolList(out io.Writer, deps Deps) error {
	if deps.Tools == nil {
		e := fmt.Errorf("工具清单端口未注入——该端口由重路径装配注入（工具注册池在装配期构建），" +
			"请检查 internal/app 的装配接线")
		deps.Logger.Error("tool list 未通过：工具清单端口未注入", "error", e)
		return e
	}

	specs := deps.Tools.Tools()
	renderToolList(out, specs)
	return nil
}

// renderToolList 逐行「名称：描述」+ 末尾空行 + 汇总行；空池给**明确文案**——空串与
// "命令什么都没输出"无法区分。
// renderToolList prints one 「name：description」 line per spec plus a total; an empty
// pool gets explicit text because an empty string cannot be told from silence.
//
// 顺序**原样**：清单顺序由实现侧给出（注册池按名字排序导出），命令面不得重排——否则
// 同一份池在两处各有一种顺序，运维对号时会以为是两条不同的路径。
// The order is the implementation's and is preserved verbatim; reordering here would
// give the same pool two orders and make the operator's comparison meaningless.
func renderToolList(out io.Writer, specs []core.ToolSpec) {
	if len(specs) == 0 {
		fmt.Fprintln(out, "未注册任何工具")
		return
	}

	for _, spec := range specs {
		fmt.Fprintf(out, "%s：%s\n", spec.Name, spec.Description)
	}
	fmt.Fprintf(out, "\n共 %d 件工具\n", len(specs))
}
