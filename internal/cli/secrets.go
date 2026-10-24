package cli

// 本文件是 `secrets` 命令组（颗粒度文档 R4.8「CLI」段 / T135）：主密钥的生成、只读
// 报告与托管凭据的销毁性恢复。四条子命令共用一条纪律——
//
//   - **不自动轮换**：`init` 在密钥已存在或存在当前托管依赖时拒绝，换密钥会让既有
//     密文永久不可解；
//   - **只回公开标识**：输出里只有 key_id 与指纹，没有密钥、密文或渠道地址；
//   - **撤销不解密**：`revoke-managed` 走与 HTTP 面同一条撤销路径，服务停止时使用。
//
// The secrets command group mints the master key once, reports readiness with public
// identifiers only, and revokes every managed credential without decrypting anything.

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newSecretsCommand 组装 secrets 命令组：init / status / verify / revoke-managed。
// newSecretsCommand builds the secrets command group.
func newSecretsCommand(deps Deps) *cobra.Command {
	secrets := &cobra.Command{
		Use:   "secrets",
		Short: "主密钥与托管凭据运维（init / status / verify / revoke-managed）",
		Long: "主密钥与托管凭据运维：init 生成主密钥（已有或存在托管依赖即拒绝）、\n" +
			"status 报告就绪状态、verify 校验现有密文、revoke-managed 清空全部托管凭据。\n" +
			"Manage the master key: mint once, report readiness, verify ciphertexts, and\n" +
			"revoke every managed credential. No automatic rotation is offered.",
		// 与根命令 / provider / profile 组同一形态：可运行的父命令 + 显式未知命令分支
		// （不可运行的父命令会让 cobra 把未知子命令按"请求帮助"处理并以 0 退出）。
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("未知命令 %q：用 --help 查看可用命令", args[0])
			}
			return cmd.Help()
		},
	}
	secrets.AddCommand(
		newSecretsInitCommand(deps),
		newSecretsStatusCommand(deps),
		newSecretsVerifyCommand(deps),
		newSecretsRevokeManagedCommand(deps),
	)
	return secrets
}

// newSecretsInitCommand 组装 secrets init：生成主密钥，拒绝覆盖与拒绝孤儿两种情况
// 都由服务面判定（命令面不重复实现那份常识，也不把拒绝原因改写成另一套文案）。
// newSecretsInitCommand builds secrets init; every refusal comes from the service.
func newSecretsInitCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "生成主密钥（已存在或存在托管依赖时拒绝）",
		Long: "生成 32 字节主密钥并写到 secrets.master_key_file，同时按平台设最小权限。\n" +
			"主密钥文件已存在、或当前仍有托管凭据依赖时拒绝执行；纯 env 部署可跳过。\n" +
			"Mint the master key once; an existing file or a live managed dependency refuses.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSecretsInit(cmd.Context(), cmd.OutOrStdout(), deps)
		},
	}
}

// runSecretsInit 执行 init 并打印公开标识与依赖计数。
// runSecretsInit prints the public identifiers and the dependency counts.
func runSecretsInit(ctx context.Context, out io.Writer, deps Deps) error {
	if deps.Secrets == nil {
		return errSecretsPortMissing()
	}
	status, err := deps.Secrets.Init(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "主密钥已生成：key_id=%s fingerprint=%s\n", status.KeyID, status.Fingerprint)
	writeManagedCounts(out, status)
	return nil
}

// newSecretsStatusCommand 组装 secrets status：只读报告，未初始化不算失败。
// newSecretsStatusCommand builds the read-only status subcommand.
func newSecretsStatusCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "报告主密钥状态与托管依赖计数（只读）",
		Long: "报告主密钥是否就绪（key_id / 指纹）与当前托管依赖计数。\n" +
			"未配置或未生成主密钥时如实报告「未初始化」，不以非零退出码冒充故障。\n" +
			"Report key readiness and managed-dependency counts; an uninitialised key is\n" +
			"reported as such rather than treated as a command failure.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSecretsStatus(cmd.Context(), cmd.OutOrStdout(), deps)
		},
	}
}

// runSecretsStatus 打印状态；未初始化只影响文案，不影响退出码。
// runSecretsStatus reports readiness; an uninitialised key is not an error.
func runSecretsStatus(ctx context.Context, out io.Writer, deps Deps) error {
	if deps.Secrets == nil {
		return errSecretsPortMissing()
	}
	status, err := deps.Secrets.Status(ctx)
	if err != nil {
		return err
	}
	if !status.Initialized {
		fmt.Fprintln(out, "主密钥：未初始化（纯 env 部署可跳过；托管凭据需先执行 secrets init）")
	} else {
		fmt.Fprintf(out, "主密钥：已就绪（key_id=%s fingerprint=%s）\n",
			status.KeyID, status.Fingerprint)
	}
	writeManagedCounts(out, status)
	return nil
}

// newSecretsVerifyCommand 组装 secrets verify：只回计数；失败条数大于零即非零退出。
// newSecretsVerifyCommand builds verify; a non-zero failure count is a command failure.
func newSecretsVerifyCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "逐条校验托管凭据能否在当前主密钥下解密（只回计数）",
		Long: "逐条校验当前 active 的托管凭据：能否在当前主密钥下解密、目标绑定是否仍与\n" +
			"当前定义一致。只回检查与失败条数，明细在结构化日志里（不含材料）。\n" +
			"Verify every active managed credential; only counts are printed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSecretsVerify(cmd.Context(), cmd.OutOrStdout(), deps)
		},
	}
}

// runSecretsVerify 打印计数；有失败时返回错误（退出码非零，供脚本判卷）。
// runSecretsVerify prints counts and fails the command when any credential fails.
func runSecretsVerify(ctx context.Context, out io.Writer, deps Deps) error {
	if deps.Secrets == nil {
		return errSecretsPortMissing()
	}
	report, err := deps.Secrets.Verify(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "托管凭据检查 %d 条，失败 %d 条\n", report.Checked, report.Failed)
	if report.Failed > 0 {
		return fmt.Errorf("secrets verify：%d 条托管凭据校验失败（明细见 WARN 日志）", report.Failed)
	}
	return nil
}

// newSecretsRevokeManagedCommand 组装 secrets revoke-managed：必须显式 --confirm。
// newSecretsRevokeManagedCommand requires an explicit --confirm flag.
func newSecretsRevokeManagedCommand(deps Deps) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "revoke-managed",
		Short: "撤销全部托管凭据（服务停止时的丢失恢复，必须带 --confirm）",
		Long: "清空全部托管凭据的密文、推进撤销纪元并为每条写管理操作行；**不**需要读取\n" +
			"主密钥。这是销毁性恢复：被撤销的版本永不复活，只能重新录入新凭据版本。\n" +
			"**必须**带 --confirm 显式确认。\n" +
			"Revoke every managed credential without decrypting anything; --confirm is\n" +
			"mandatory because the action is destructive and permanent.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSecretsRevokeManaged(cmd.Context(), cmd.OutOrStdout(), deps, confirm)
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false,
		"确认清空全部托管凭据密文（被撤销的版本永不复活）")
	return cmd
}

// runSecretsRevokeManaged 先判 --confirm 再调服务面：缺确认时零副作用、零调用。
// runSecretsRevokeManaged refuses before calling the service when --confirm is absent.
func runSecretsRevokeManaged(ctx context.Context, out io.Writer, deps Deps, confirm bool) error {
	if deps.Secrets == nil {
		return errSecretsPortMissing()
	}
	if !confirm {
		return fmt.Errorf("拒绝执行：revoke-managed 会清空全部托管凭据的密文且不可恢复，" +
			"确认后请加 --confirm")
	}
	revoked, err := deps.Secrets.RevokeManaged(ctx)
	if err != nil {
		return fmt.Errorf("撤销托管凭据（已撤销 %d 条）：%w", revoked, err)
	}
	fmt.Fprintf(out, "已撤销 %d 条托管凭据（密文已清空，需要时重新录入新版本）\n", revoked)
	return nil
}

// writeManagedCounts 打印托管依赖计数（init / status 共用，口径一致）。
// writeManagedCounts prints the managed-dependency counts shared by init and status.
func writeManagedCounts(out io.Writer, status SecretsStatus) {
	fmt.Fprintf(out, "当前托管依赖：模型连接 %d 个、通知渠道 %d 个\n",
		status.ManagedModelConnections, status.ManagedNotifyChannels)
}

// errSecretsPortMissing 是装配缺件的固定错误：命令面拿到 nil 端口时不自造服务，
// 也不把"没装配"说成"没有密钥"（两者的处置完全不同）。
// errSecretsPortMissing keeps a wiring gap distinct from a deployment state.
func errSecretsPortMissing() error {
	return fmt.Errorf("secrets 服务未装配（需要主程序的重路径装配）——不提供降级读面")
}
