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
	"path/filepath"

	"github.com/spf13/cobra"
)

// newSecretsCommand 组装 secrets 命令组：init / status / verify / revoke-managed。
// newSecretsCommand builds the secrets command group.
func newSecretsCommand(deps Deps) *cobra.Command {
	secrets := &cobra.Command{
		Use:   "secrets",
		Short: "主密钥与托管凭据运维（init / status / verify / revoke-managed / check-key / recover-record / adopt-legacy）",
		Long: "主密钥与托管凭据运维：init 生成主密钥（已有或存在托管依赖即拒绝）、\n" +
			"status 报告就绪状态、verify 校验现有密文、revoke-managed 清空全部托管凭据；\n" +
			"check-key 离线校验密钥文件与直接父目录权限；recover-record 从可信备份恢复\n" +
			"初始化记录、adopt-legacy 在隔离副本上完成 schema 3～5 的受控升级——后三条\n" +
			"离线执行、零外呼。\n" +
			"Manage the master key: mint once, report readiness, verify ciphertexts, revoke\n" +
			"every managed credential, preflight the key file offline, recover the record from\n" +
			"a trusted backup, and adopt a legacy schema 3–5 copy. No automatic rotation is\n" +
			"offered.",
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
		newSecretsCheckKeyCommand(deps),
		newSecretsRecoverRecordCommand(deps),
		newSecretsAdoptLegacyCommand(deps),
	)
	return secrets
}

// newSecretsCheckKeyCommand 组装 secrets check-key：Worker 启动预检与运维手工核验共用的
// **离线**入口。它只读主密钥文件与直接父目录权限，零外呼、不连库、不取实例锁。
// newSecretsCheckKeyCommand builds the offline key preflight; it never touches a database,
// the instance lock or the network.
func newSecretsCheckKeyCommand(deps Deps) *cobra.Command {
	var (
		path      string
		workspace string
	)
	cmd := &cobra.Command{
		Use:   "check-key",
		Short: "离线校验主密钥文件与直接父目录权限（Worker 启动预检；不连库）",
		Long: "校验主密钥文件：路径在工作区外、不经符号链接 / junction / 硬链接、文件与\n" +
			"**直接父目录**权限受限（Unix 文件 0600 / 目录 0700；Windows 判 DACL）、JSON\n" +
			"形态与 32 字节长度逐项吻合。通过时只打印 key_id 与指纹；失败时点名原因，\n" +
			"**不**回显密钥内容。\n" +
			"离线执行：不装配运行期、不取实例锁、不迁移、不连库，因此可在 Worker 预检与\n" +
			"实例停机后的手工核验里使用。\n" +
			"Validate the master-key file and its direct parent directory offline; print the\n" +
			"key id and fingerprint only, never the key bytes.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSecretsCheckKey(cmd.OutOrStdout(), deps, path, workspace)
		},
	}
	cmd.Flags().StringVar(&path, "path", "",
		"主密钥文件路径（必填；对应 secrets.master_key_file）")
	cmd.Flags().StringVar(&workspace, "workspace", "",
		"工作区根 <WorkDir>/.basalt（缺省按 --workdir 推导；密钥必须落在它之外）")
	return cmd
}

// runSecretsCheckKey 补齐缺省工作区后调用端口；退出码由错误决定（成功 0，失败 1）。
// runSecretsCheckKey fills the default workspace root, then calls the port.
func runSecretsCheckKey(out io.Writer, deps Deps, path, workspace string) error {
	if deps.MasterKeyCheck == nil {
		return errOfflinePortMissing("secrets check-key")
	}
	if path == "" {
		return refuseCommand(out, exitValidation,
			"缺少 --path：需要主密钥文件路径（对应 secrets.master_key_file）")
	}
	if workspace == "" {
		// 缺省与其余命令同一推导：`<WorkDir>/.basalt`。密钥**必须**在工作区之外，
		// 因此这个缺省就是“工作区边界”的判据输入，而不是可选的展示参数。
		// The default mirrors every other command: `<WorkDir>/.basalt`.
		workspace = filepath.Join(deps.WorkDir, workspaceDirName)
	}
	report, err := deps.MasterKeyCheck.Check(path, workspace)
	if err != nil {
		return refuseCommand(out, exitValidation, "%s", err.Error())
	}
	fmt.Fprintf(out, "主密钥可读：key_id=%s fingerprint=%s\n", report.KeyID, report.Fingerprint)
	return nil
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
		// 恢复态在服务面就按部署错误上抛（不在这里落到"未初始化"，那会把缺钥匙说成
		// 还没初始化）；走到这里的是真正的未初始化：纯 env，或尚未首次生成。
		fmt.Fprintln(out, "主密钥：未初始化（纯 env 部署可跳过；托管凭据需先执行 secrets init）")
	} else if status.State == "" || status.State == secretStateReady {
		fmt.Fprintf(out, "主密钥：已就绪（key_id=%s fingerprint=%s）\n",
			status.KeyID, status.Fingerprint)
	} else {
		// 密钥可读但缺初始化记录（待 adopt）：页面不谎报"就绪"，命令面同样点明。
		fmt.Fprintf(out, "主密钥：密钥文件可用但缺初始化记录（待 adopt；key_id=%s fingerprint=%s）\n",
			status.KeyID, status.Fingerprint)
	}
	writeManagedCounts(out, status)
	return nil
}

// secretStateReady 与 core.SecretStateReady 同一字面量；命令面只做展示，不引入对
// 装配层的依赖。
// secretStateReady mirrors core's literal for display only.
const secretStateReady = "ready"

// newSecretsVerifyCommand 组装 secrets verify：只回计数；失败条数大于零即非零退出。
// newSecretsVerifyCommand builds verify; a non-zero failure count is a command failure.
func newSecretsVerifyCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "校验历史密文与当前托管凭据（只回计数）",
		Long: "两个报告对象：(a) 全部密文版本能否在当前主密钥下解密、key_id 是否一致；\n" +
			"(b) 当前 active 凭据对已发布资源、目标地址与 MCP 槽位的核对。只回计数与\n" +
			"类别，逐条原因在结构化日志里（不含材料）。\n" +
			"Verify the sealed history and the current managed credentials; counts only.",
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
	writeSecretsVerifyBreakdown(out, report)
	if failed := report.Failed + report.CurrentFailed; failed > 0 {
		return fmt.Errorf("secrets verify：%d 条托管凭据校验失败（明细见 WARN 日志）", failed)
	}
	return nil
}

// writeSecretsVerifyBreakdown 只在有失败时补两行分类计数：判卷者第一眼就能区分
// "历史密文坏了"与"当前凭据不可用"，不必先翻日志。只含计数与类别名，不含材料。
// writeSecretsVerifyBreakdown prints the per-category counts only when something
// failed, so the healthy path keeps its single line.
func writeSecretsVerifyBreakdown(out io.Writer, report SecretsVerifyReport) {
	if report.Failed+report.CurrentFailed == 0 {
		return
	}
	fmt.Fprintf(out, "  历史密文：可解 %d / 已撤销 %d / 撤销仍留密文 %d / 密文缺失 %d / key_id 不匹配 %d / 无法解密 %d\n",
		report.HistoricalOK, report.Revoked, report.RevokedWithCiphertext,
		report.MissingCiphertext, report.KeyIDMismatch, report.AADMismatch)
	fmt.Fprintf(out, "  当前可用性：检查 %d / 失败 %d（目标地址不一致 %d / 绑定缺失 %d / 当前版本不可用 %d / 环境变量缺失 %d）\n",
		report.CurrentChecked, report.CurrentFailed, report.TargetMismatch,
		report.BindingMissing, report.KeyRevoked, report.EnvMissing)
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

// ─────────────────────── recover-record / adopt-legacy（离线手工流程） ───────────────────────
//
// 两条命令是**恢复与升级**的入口，不是运维日常：它们只在旧实例停止后、对着可信备份
// 手工执行，因此命令面这一次承担了比其余子命令更多的输入判定——备份形态与外部锚点
// 必须在调用端口**之前**判明，否则"备份看起来像什么就按什么处理"会把错误来源的记录
// 写进本库（颗粒度文档 §3.1.8b / §3.1.8c、§2.12.0a 校验顺序 0～1）。
// Both commands are manual recovery entries: shape and anchor are decided here, before
// the port is called, so a wrong source can never reach the write.

// recoverRecordOptions 是 `secrets recover-record` 的全部输入。
// recoverRecordOptions carries every surface input of recover-record.
type recoverRecordOptions struct {
	// Workspace 是工作区根 `<WorkDir>/.basalt`。
	Workspace string
	// Backup 是可信备份目录。
	Backup string
	// ExpectBackupSHA256 是带外锚点（备份清单的字节哈希）。
	ExpectBackupSHA256 string
	// Confirm 是操作者的显式确认。
	Confirm bool
}

// adoptLegacyOptions 是 `secrets adopt-legacy` 的全部输入。
// adoptLegacyOptions carries every surface input of adopt-legacy.
type adoptLegacyOptions struct {
	// Workspace 是隔离副本的工作区根 `<copy-root>/.basalt`。
	Workspace string
	// Backup 是备份目录（含 backup.json）。
	Backup string
	// ExpectBackupSHA256 是备份清单的带外锚点。
	ExpectBackupSHA256 string
	// Source 是原实例的 `.basalt` 根（与 AttestSourceUnchanged 二选一）。
	Source string
	// AttestSourceUnchanged 是带外见证编号。
	AttestSourceUnchanged string
	// MasterKeyFile 是受控取钥的覆盖路径。
	MasterKeyFile string
	// ExpectKeySHA256 是覆盖取钥的锚点。
	ExpectKeySHA256 string
	// Confirm 是操作者的显式确认。
	Confirm bool
}

// newSecretsRecoverRecordCommand 组装 secrets recover-record。
// newSecretsRecoverRecordCommand builds the offline record-recovery subcommand.
func newSecretsRecoverRecordCommand(deps Deps) *cobra.Command {
	opts := recoverRecordOptions{}
	cmd := &cobra.Command{
		Use:   "recover-record",
		Short: "从可信备份恢复主密钥初始化记录（离线；需要 --confirm）",
		Long: "从**可信备份**恢复 `instance_secrets` 初始化记录（及缺失时的实例名册），\n" +
			"用于 D-40 的恢复态：库里有托管密文、但初始化记录因崩溃 / 误删丢失。\n" +
			"命令离线执行、零外呼：不装配运行期、不启动 MCP、不发通知、不调用模型。\n" +
			"`--expect-backup-sha256` 必须来自备份时的带外记录——备份内自带的哈希\n" +
			"不构成可信来源。不提供 --force / --overwrite / --new-key 一类开关。\n" +
			"Restore the master-key record (and the roster when missing) from a trusted\n" +
			"backup. Offline by construction; the external anchor is mandatory.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSecretsRecoverRecord(cmd.Context(), cmd.OutOrStdout(), deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Workspace, "workspace", "",
		"工作区根 <WorkDir>/.basalt（必填）")
	cmd.Flags().StringVar(&opts.Backup, "backup", "",
		"可信备份目录（必填；按内容判定路线：manifest.json → A，basalt.db + instance.json → B）")
	cmd.Flags().StringVar(&opts.ExpectBackupSHA256, "expect-backup-sha256", "",
		"备份清单的带外 SHA-256 锚点（必填）")
	cmd.Flags().BoolVar(&opts.Confirm, "confirm", false,
		"显式确认执行（缺省即拒绝，无交互提示）")
	return cmd
}

// newSecretsAdoptLegacyCommand 组装 secrets adopt-legacy。
// newSecretsAdoptLegacyCommand builds the controlled legacy-upgrade subcommand.
func newSecretsAdoptLegacyCommand(deps Deps) *cobra.Command {
	opts := adoptLegacyOptions{}
	cmd := &cobra.Command{
		Use:   "adopt-legacy",
		Short: "在隔离副本上完成 schema 3～5 的受控升级并写 attested_legacy 记录（离线）",
		Long: "为 schema 3～5 的合法存量准备 `instance_secrets` 记录：只对**隔离副本**\n" +
			"操作，校验外部锚点、备份清单、原密钥与全部密文连续、源端未漂移之后，\n" +
			"把副本迁移到 v6 并写 1 条 `record_origin=attested_legacy` 记录。\n" +
			"不装配运行期 / MCP / 调度 / HTTP / notifier / provider；不写名册、不改配置、\n" +
			"不生成或复制主密钥、不改写或删除密文。不提供 --force / --new-key / --skip-*。\n" +
			"Prepare a v6 record from a schema 3–5 copy, offline, with an external anchor.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSecretsAdoptLegacy(cmd.Context(), cmd.OutOrStdout(), deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Workspace, "workspace", "",
		"隔离副本的工作区根 <copy-root>/.basalt（必填）")
	cmd.Flags().StringVar(&opts.Backup, "backup", "",
		"备份目录（必填；须含 backup.json 与清单列出的全部文件）")
	cmd.Flags().StringVar(&opts.ExpectBackupSHA256, "expect-backup-sha256", "",
		"backup.json 的带外 SHA-256 锚点（必填）")
	cmd.Flags().StringVar(&opts.Source, "source", "",
		"原实例的 .basalt 根（本地可读时必填，与 --attest-source-unchanged 二选一）")
	cmd.Flags().StringVar(&opts.AttestSourceUnchanged, "attest-source-unchanged", "",
		"源端未变更的带外见证编号（与 --source 二选一）")
	cmd.Flags().StringVar(&opts.MasterKeyFile, "master-key-file", "",
		"受控取钥的覆盖路径（副本主机读不到配置所指密钥时使用）")
	cmd.Flags().StringVar(&opts.ExpectKeySHA256, "expect-key-sha256", "",
		"覆盖取钥的 SHA-256 锚点（备份外副本必填）")
	cmd.Flags().BoolVar(&opts.Confirm, "confirm", false,
		"显式确认执行（缺省即拒绝，无交互提示）")
	return cmd
}

// runSecretsRecoverRecord 执行记录恢复：命令面先判参数 / 形态 / 锚点，通过后才调用端口。
// runSecretsRecoverRecord decides arguments, shape and anchor before touching the port.
func runSecretsRecoverRecord(ctx context.Context, out io.Writer, deps Deps, opts recoverRecordOptions) error {
	if deps.SecretsRecovery == nil {
		return errOfflinePortMissing("secrets recover-record")
	}
	if !opts.Confirm {
		return refuseCommand(out, exitPrecondition,
			"缺少 --confirm：恢复记录会写入名册与初始化记录，必须显式确认（无交互提示）")
	}
	if opts.Workspace == "" {
		return refuseCommand(out, exitPrecondition,
			"缺少 --workspace：需要工作区根 <WorkDir>/.basalt")
	}
	if opts.Backup == "" {
		return refuseCommand(out, exitPrecondition,
			"缺少 --backup：需要可信备份目录")
	}
	kind, computed, err := verifiedBackupManifest(out, opts.Backup, opts.ExpectBackupSHA256,
		classifyRecoverBackup,
		"备份目录既不是导出包（manifest.json）也不是原生备份集（basalt.db + instance.json），拒绝猜测")
	if err != nil {
		return err
	}

	report, err := deps.SecretsRecovery.RecoverSecretsRecord(ctx, RecoverRecordRequest{
		Workspace:      opts.Workspace,
		Backup:         opts.Backup,
		Kind:           kind,
		ManifestSHA256: computed,
		Confirm:        opts.Confirm,
	})
	if err != nil {
		fmt.Fprintln(out, err.Error())
		return err
	}
	writeRecoverRecordReport(out, opts, report)
	return nil
}

// runSecretsAdoptLegacy 执行受控升级：命令面先判参数 / 见证 / 锚点，通过后才调用端口。
// runSecretsAdoptLegacy decides arguments, witness and anchor before the port call.
func runSecretsAdoptLegacy(ctx context.Context, out io.Writer, deps Deps, opts adoptLegacyOptions) error {
	if deps.LegacySecretsAdopt == nil {
		return errOfflinePortMissing("secrets adopt-legacy")
	}
	if !opts.Confirm {
		return refuseCommand(out, exitPrecondition,
			"缺少 --confirm：受控升级会迁移副本并写初始化记录，必须显式确认（无交互提示）")
	}
	if opts.Workspace == "" {
		return refuseCommand(out, exitPrecondition,
			"缺少 --workspace：需要隔离副本的工作区根 <copy-root>/.basalt")
	}
	if opts.Backup == "" {
		return refuseCommand(out, exitPrecondition,
			"缺少 --backup：需要含 backup.json 的备份目录")
	}
	hasSource := opts.Source != ""
	hasAttestation := opts.AttestSourceUnchanged != ""
	switch {
	case hasSource && hasAttestation:
		return refuseCommand(out, exitPrecondition,
			"source_witness_conflict：--source 与 --attest-source-unchanged 只能选一个")
	case !hasSource && !hasAttestation:
		return refuseCommand(out, exitPrecondition,
			"attestation_missing：本地可读时用 --source <original-ws>；源端不可读时必须给 --attest-source-unchanged <ticket-ref>")
	}
	if opts.ExpectKeySHA256 != "" && opts.MasterKeyFile == "" {
		return refuseCommand(out, exitPrecondition,
			"缺少 --master-key-file：--expect-key-sha256 只能用来给覆盖取钥做锚点")
	}
	_, computed, err := verifiedBackupManifest(out, opts.Backup, opts.ExpectBackupSHA256,
		classifyLegacyBackup,
		"备份目录里没有 backup.json——本命令只接受含清单的原生备份集，拒绝猜测；"+
			"没有可用备份时只能保持恢复态（env-only 降级），破坏性重置 retire-managed 未批准，不得执行")
	if err != nil {
		return err
	}

	report, err := deps.LegacySecretsAdopt.AdoptLegacySecrets(ctx, AdoptLegacyRequest{
		Workspace:             opts.Workspace,
		Backup:                opts.Backup,
		ManifestSHA256:        computed,
		Source:                opts.Source,
		AttestSourceUnchanged: opts.AttestSourceUnchanged,
		MasterKeyFile:         opts.MasterKeyFile,
		ExpectKeySHA256:       opts.ExpectKeySHA256,
		Confirm:               opts.Confirm,
	})
	if err != nil {
		fmt.Fprintln(out, err.Error())
		return err
	}
	writeAdoptLegacyReport(out, report)
	return nil
}

// writeRecoverRecordReport 只打印 §3.1.8b 允许的公开项：路径、两侧哈希、名册状态与
// 哈希、记录 `key_id` / 指纹、校验计数、操作号与结束状态。密钥材料 / 密文 / 明文 /
// 完整 `detail_json` 一律不出现。
// writeRecoverRecordReport prints only the public fields §3.1.8b allows.
func writeRecoverRecordReport(out io.Writer, opts recoverRecordOptions, r RecoverRecordResult) {
	fmt.Fprintf(out, "命令结束状态：%s\n", r.FinalState)
	fmt.Fprintf(out, "操作号：%s\n", r.OperationID)
	fmt.Fprintf(out, "备份：kind=%s path=%s\n", r.Kind, r.BackupPath)
	fmt.Fprintf(out, "备份清单哈希：expected=%s computed=%s\n", r.ExpectedSHA256, r.ComputedSHA256)
	fmt.Fprintf(out, "工作区：%s\n", opts.Workspace)
	fmt.Fprintf(out, "名册：恢复前存在=%t 本次恢复=%t sha256=%s\n",
		r.RosterPresentBefore, r.RosterRestored, r.RosterSHA256)
	fmt.Fprintf(out, "记录：key_id=%s fingerprint=%s\n", r.RecordKeyID, r.RecordFingerprint)
	fmt.Fprintf(out, "校验计数：托管密文 %d 条、不可解 %d 条\n", r.Ciphertexts, r.CiphertextsUnreadable)
	if r.InstanceID != "" {
		fmt.Fprintf(out, "实例：%s\n", r.InstanceID)
	}
}

// writeAdoptLegacyReport 只打印 §3.1.8c 允许的公开项：路径、清单两侧哈希、配置哈希
// 四值、schema 前后、取钥来源与哈希、密钥公开摘要、密文条数、名册状态、源端见证与
// 操作号。密钥材料 / 密文 / 明文 / 完整 `detail_json` 一律不出现。
// writeAdoptLegacyReport prints only the public fields §3.1.8c allows.
func writeAdoptLegacyReport(out io.Writer, r AdoptLegacyResult) {
	fmt.Fprintf(out, "命令结束状态：%s\n", r.FinalState)
	fmt.Fprintf(out, "操作号：%s\n", r.OperationID)
	fmt.Fprintf(out, "副本：%s\n", r.Workspace)
	fmt.Fprintf(out, "备份：%s\n", r.BackupPath)
	fmt.Fprintf(out, "备份清单哈希：expected=%s computed=%s\n", r.ExpectedSHA256, r.ComputedSHA256)
	fmt.Fprintf(out, "配置哈希：source=%s backup=%s copy=%s without_key_path=%s\n",
		r.SourceConfigSHA256, r.BackupConfigSHA256, r.CopyConfigSHA256, r.ConfigWithoutKeyPathSHA256)
	fmt.Fprintf(out, "schema：%d → %d\n", r.SchemaBefore, r.SchemaAfter)
	fmt.Fprintf(out, "取钥：source=%s key_id=%s fingerprint=%s key_file_sha256=%s\n",
		r.KeySource, r.KeyID, r.KeyFingerprint, r.KeyFileSHA256)
	fmt.Fprintf(out, "密文：%d 条（迁移后逐字节复核）\n", r.Ciphertexts)
	fmt.Fprintf(out, "名册：存在=%t（本命令不写名册）\n", r.RosterPresent)
	fmt.Fprintf(out, "身份证据：%s\n", r.IdentityEvidence)
	fmt.Fprintf(out, "源端见证：%s\n", r.SourceWitness)
	fmt.Fprintf(out, "配置调整：false（覆盖取钥不改写配置）\n")
	if r.InstanceID != "" {
		fmt.Fprintf(out, "实例：%s\n", r.InstanceID)
	}
}
