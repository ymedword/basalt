package cli

// 本文件是资产 bundle 的命令面（US-24 模块卡 §3.1.11 / §3.1.12；US-25C §3.1.5 追加
// 只读诊断）：
//
//	basalt assets export <bundle-dir> <dest-dir>
//	basalt assets install <bundle-dir>
//	basalt assets verify <bundle-dir>
//	basalt assets status
//
// 三条纪律：
//  1. **参数校验只有一份实现**：子命令把参数原样交给 `assets.CommandRoot`，命令面
//     不重复写"需要几个参数"的分支——两处校验迟早会给出两种错误文案。
//  2. **失败非空**：任何一条错误都带上路径或哈希与原因，**不回显文件内容**（资产可能
//     是私有配置）；`CommandRoot` 的错误文本已按这条纪律写，命令面只做上抛与留痕。
//  3. **install 必须写共享发布记录**：`Publish` 由装配层注入，缺接线时
//     `CommandRoot` 失败关闭——**不得**出现"只装本地"的降级分支。
//
// The asset command surface. Argument validation lives in assets.CommandRoot so there
// is exactly one set of messages, failures are never empty and never echo file
// contents, and install cannot degrade into a local-only copy. `status` is the
// read-only diagnostic: it renders verdicts the adapter computed and never triggers
// a sync or writes anything.

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/internal/assets"
)

// requireAssetsPort 取资产端口；缺失即报缺件——命令面**不得**自己解析资产根或
// 打开数据库补出第二条安装路径。
// requireAssetsPort reports a wiring gap instead of resolving a root of its own.
func requireAssetsPort(deps Deps) (AssetsPort, error) {
	if deps.Assets == nil {
		e := fmt.Errorf("资产端口未注入——assets 需要装配层注入的 AssetsPort（internal/app 提供）")
		deps.Logger.Error("basalt assets 未通过：资产端口未注入")
		return nil, e
	}
	return deps.Assets, nil
}

// newAssetsCommand 组装 `assets` 命令组。
// newAssetsCommand groups the three asset subcommands.
func newAssetsCommand(deps Deps) *cobra.Command {
	group := &cobra.Command{
		Use:   "assets",
		Short: "Agent 资产 bundle：导出、安装与校验",
		Long: "资产 bundle 是内容寻址的 Agent 配置与技能快照（manifest + 逐文件 SHA-256）。\n" +
			"verify 只复算、不写任何东西；export 先校验再逐文件复制；install 安装到\n" +
			"<bundle_root>/bundles/<agent>/<bundle_hash> 并推进共享发布记录。\n" +
			"失败一律非空，且不回显文件内容。\n" +
			"Content-addressed asset bundles: verify recomputes only, export copies after\n" +
			"verifying, and install writes the immutable directory plus the shared pointer.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	group.AddCommand(
		newAssetsRunCommand(deps, "verify", "校验一个 bundle 目录（只复算，零写入）", " <bundle-dir>"),
		newAssetsRunCommand(deps, "export", "把已验证的 bundle 导出到目标目录", " <bundle-dir> <dest-dir>"),
		newAssetsInstallCommand(deps),
		newAssetsStatusCommand(deps),
	)
	return group
}

// newAssetsRunCommand 是最薄的一层：参数原样转交 `assets.CommandRoot`，只负责缺件
// 判定、错误留痕与结果打印。
// newAssetsRunCommand delegates verbatim to assets.CommandRoot.
func newAssetsRunCommand(deps Deps, verb, short, argHint string) *cobra.Command {
	return &cobra.Command{
		Use:   verb + argHint,
		Short: short,
		// 参数个数由 CommandRoot 判：cobra 再判一次会给出第二套文案。
		// Argument counts are judged by CommandRoot so the messages stay single-sourced.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAssetsVerb(cmd, deps, verb, append([]string{verb}, args...))
		},
	}
}

// newAssetsInstallCommand 是 install 的构造：它比其他两个子命令多一个 `--agent`，
// 把"从工作区构建后安装"与"安装现成 bundle"收在同一个子命令里。flag 只是把
// `--agent <name>` 原样拼回命令面参数，**不**在这里判"两个都给"——那条判据在
// `assets.CommandRoot`，各写一份就会给出两种文案。
// newAssetsInstallCommand adds only the --agent flag; every argument judgement stays
// in assets.CommandRoot.
func newAssetsInstallCommand(deps Deps) *cobra.Command {
	var agent string
	command := &cobra.Command{
		Use:   "install <bundle-dir> | install --agent <agent>",
		Short: "安装 bundle 到内容寻址目录并写共享发布记录",
		Long: "install <bundle-dir> 安装一个现成的 bundle 目录（跨机器分发用）。\n" +
			"install --agent <agent> 先从工作区按该 Agent 的当前定义构建 bundle，再安装并发布。\n" +
			"两种形态互斥；安装目录不可变，已存在时只做逐文件比对。",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			argv := append([]string{"install"}, args...)
			if agent != "" {
				argv = append([]string{"install", "--agent", agent}, args...)
			}
			return runAssetsVerb(cmd, deps, "install", argv)
		},
	}
	command.Flags().StringVar(&agent, "agent", "",
		"从工作区构建该 Agent 的当前定义后安装（与 <bundle-dir> 二选一）")
	return command
}

// requireAssetStatusPort 取分发诊断端口；缺失即报缺件——命令面**不得**自己打开
// 数据库补出第二条读面（与 `workers list` 同口径）。
// requireAssetStatusPort reports a wiring gap instead of opening a read face of its own.
func requireAssetStatusPort(deps Deps) (AssetStatusReader, error) {
	if deps.AssetStatus == nil {
		e := fmt.Errorf("资产状态端口未注入——assets status 需要装配层注入的 AssetStatusReader（internal/app 提供）")
		deps.Logger.Error("basalt assets status 未通过：资产状态端口未注入")
		return nil, e
	}
	return deps.AssetStatus, nil
}

// newAssetsStatusCommand 组装只读诊断子命令（US-25C 模块卡 §3.1.5）：逐 Agent 打印
// 「指针哈希 / 已安装哈希 / 载荷存在 / 技能物化 / 阻塞原因」，**不触发同步、不写库**。
// 判定全部在适配器里，命令面只做渲染——诊断与同步因此共用同一份判定规则。
// newAssetsStatusCommand renders the read-only diagnostic; every verdict comes from
// the adapter, so the diagnostic and the sync share one judgement.
func newAssetsStatusCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "只读诊断：逐 Agent 打印指针 / 已安装 / 载荷 / 技能物化 / 阻塞原因",
		Long: "只读诊断：逐 Agent 打印「指针哈希 / 已安装哈希 / 载荷存在 / 技能物化 / 阻塞原因」。\n" +
			"不触发同步、不取实例锁、不写库，服务运行中也能执行。\n" +
			"载荷读数：present / payload_missing / payload_unavailable；\n" +
			"技能读数：materialized / skills_unavailable / unknown（未安装时没有可核对的对象）。\n" +
			"阻塞原因与同步报告共用词表：payload_missing / payload_unavailable / " +
			"not_installed / corrupt / skills_unavailable；就绪时显示 ——。\n" +
			"Read-only diagnostic: never triggers a sync, takes no instance lock, writes nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			port, err := requireAssetStatusPort(deps)
			if err != nil {
				return err
			}
			report, err := port.Status(cmd.Context())
			if err != nil {
				deps.Logger.Error("basalt assets status 未通过", "error", err)
				return err
			}
			return printAssetStatusReport(cmd, report)
		},
	}
}

// printAssetStatusReport 逐 Agent 渲染诊断行，末尾给一行汇总（agents / ready /
// blocked 由同一份行数据数出来，不另算第二遍）。空表也照实说"没有已发布的 bundle"，
// 不留白让运维猜。
// printAssetStatusReport renders every row and one summary line counted from the same rows.
func printAssetStatusReport(cmd *cobra.Command, report AssetStatusReport) error {
	placeholder := func(value string) string {
		if value == "" {
			return "—"
		}
		return value
	}
	var out strings.Builder
	fmt.Fprintf(&out, "资产根：%s\n", report.AssetRoot)
	if len(report.Agents) == 0 {
		out.WriteString("（没有已发布的 Agent bundle）\n")
	}
	ready := 0
	for _, row := range report.Agents {
		if row.BlockedReason == "" {
			ready++
		}
		fmt.Fprintf(&out, "agent=%s\n", row.Agent)
		fmt.Fprintf(&out, "  指针哈希：%s\n", row.PointerHash)
		fmt.Fprintf(&out, "  已安装哈希：%s\n", placeholder(row.InstalledHash))
		fmt.Fprintf(&out, "  载荷：%s\n", row.Payload)
		fmt.Fprintf(&out, "  技能物化：%s\n", row.Skills)
		fmt.Fprintf(&out, "  阻塞原因：%s\n", placeholder(row.BlockedReason))
	}
	fmt.Fprintf(&out, "汇总：agents=%d ready=%d blocked=%d\n",
		len(report.Agents), ready, len(report.Agents)-ready)
	_, err := fmt.Fprint(cmd.OutOrStdout(), out.String())
	return err
}

// runAssetsVerb 是三种子命令共用的执行壳：取端口、转交 CommandRoot、错误留痕、打印。
// runAssetsVerb is the shared shell: fetch the port, delegate, log failures, print.
func runAssetsVerb(cmd *cobra.Command, deps Deps, verb string, argv []string) error {
	port, err := requireAssetsPort(deps)
	if err != nil {
		return err
	}
	message, err := assets.CommandRoot(cmd.Context(), assets.Deps{
		AssetRoot: port.AssetRoot(),
		Publish:   port.Publish,
		Build:     port.Build,
	}, argv)
	if err != nil {
		deps.Logger.Error("basalt assets "+verb+" 未通过", "error", err)
		return err
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), strings.TrimRight(message, "\n")); err != nil {
		return err
	}
	return nil
}
