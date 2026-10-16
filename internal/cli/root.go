package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewRootCommand 组装根命令：注册 init / status，并提供全局 --version 选项。
// NewRootCommand assembles the root command: it registers init and status, and
// carries the global --version option.
//
// 版本标识由装配层在返回后写入 root.Version（构建期注入值经 -X main.version
// 传递）——本函数给出可独立使用的缺省值，便于测试单独构造命令。
// The assembly layer writes the build-time version into root.Version afterwards;
// the default here keeps the command usable on its own in tests.
func NewRootCommand(deps Deps) *cobra.Command {
	root := &cobra.Command{
		Use:   "basalt",
		Short: "Basalt —— 面向企业场景的自托管 Agent OS",
		Long: "Basalt —— 面向企业场景的自托管 Agent OS（单 Go 二进制）。\n" +
			"Basalt is a self-hosted agent OS for regulated enterprises, shipped as a single Go binary.",
		// 失败时只由 app.Run 打结构化日志并定退出码——cobra 不再重复打印用法。
		SilenceUsage: true,
		Version:      "dev",
		// 根命令必须"可运行"：cobra 对不可运行的根命令一律按「请求帮助」处理并
		// 以 0 退出——那样未知命令会被静默吞掉，退出码就分不出成败（FR-009）。
		// 注册子命令后，cobra 自己的 unknown command 分支先于本函数生效，两条
		// 路径都非零，只是文案来源不同。
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("未知命令 %q：用 --help 查看可用命令", args[0])
			}
			return cmd.Help()
		},
	}

	// 注册点：init / status / provider / profile / chat / session / tool 七条子命令由各自
	// 的任务落地后在此注册——骨架期保持"零子命令也可编译"，避免门禁依赖尚未交付的故事。
	// Registration site: init, status, provider, profile, chat, session and tool land here
	// as their tasks complete, so the skeleton stays buildable ahead of those stories.
	root.AddCommand(
		newInitCommand(deps),
		newStatusCommand(deps),
		newProviderCommand(deps),
		newProfileCommand(deps),
		newChatCmd(deps),
		newSessionCmd(deps),
		newToolCommand(deps),
	)

	return root
}
