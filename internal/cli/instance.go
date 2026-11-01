package cli

// 本文件是 `instance` 命令组（颗粒度文档 §3.1.8a / C-47）：实例身份的离线运维入口。
// 当前只有 `instance adopt`——给"库存在、`instance.json` 缺失"的存量实例补写名册。
//
// 命令面只做三件事：解析参数、经端口发起采纳、把结果投影成可打印的公开事实。全部判据
// （四判定、密钥路径互证、审计两阶段写、崩溃恢复）都在装配层的适配器里；`internal/cli`
// **不得** import `internal/app`，也不自己铸实例标识。
// The instance command group. The surface parses, calls the port and prints; every
// criterion lives in the adapter, and this package never mints an instance id.
//
// `--config-change-ref`（迁移准备模式，T074）已接线：副本配置调整过时，变更单引用是
// 受控调整的凭据；适配器重算规范化摘要证明"只改了密钥路径"。命令面只透传，不判定。
// The migration-preparation flag --config-change-ref is wired through; the adapter
// owns the attestation and the command surface never decides.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// requireInstanceAdoptPort 是 `instance adopt` 的缺件判定：端口为 nil 即装配缺陷，
// 命令面**不得**自己打开 SQLite 或自己写名册补出第二条路径。
// requireInstanceAdoptPort names a missing wiring instead of writing the roster itself.
func requireInstanceAdoptPort(deps Deps, command string) error {
	if deps.InstanceAdopt != nil {
		return nil
	}
	e := fmt.Errorf("实例采纳端口未注入——%s 需要装配层注入的 InstanceAdoption（internal/app 提供）", command)
	deps.Logger.Error(command + " 未通过：实例采纳端口未注入")
	return e
}

// newInstanceCommand 组装 `instance` 命令组。
// newInstanceCommand assembles the instance command group.
func newInstanceCommand(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "instance",
		Short: "实例身份运维（存量工作区的离线采纳）",
		Long: "实例身份运维命令组。\n" +
			"`instance adopt` 为已有 SQLite 库、但没有实例名册（instance.json）的存量工作区\n" +
			"补写身份：离线执行、零外呼，**只写名册**，不生成 / 搬移 / 重加密主密钥，\n" +
			"也不写 instance_secrets 记录。拒绝一律非零退出且零写入。\n" +
			"Offline identity adoption for an existing SQLite workspace: the roster is the\n" +
			"only thing written, and every refusal is non-zero with nothing written.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// 与根命令同一口径：未知子命令不得被 cobra 当成「请求帮助」静默吞掉。
			if len(args) > 0 {
				return fmt.Errorf("未知子命令 %q：用 --help 查看可用子命令", args[0])
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(newInstanceAdoptCommand(deps))
	return cmd
}

// newInstanceAdoptCommand 组装 `instance adopt`。
// newInstanceAdoptCommand assembles `instance adopt`.
func newInstanceAdoptCommand(deps Deps) *cobra.Command {
	var (
		workspace       string
		masterKeyFile   string
		configChangeRef string
	)
	cmd := &cobra.Command{
		Use:   "adopt",
		Short: "为存量工作区补写实例名册（离线；只写 instance.json）",
		Long: "为已有 SQLite 库、但没有 instance.json 的存量工作区建立实例身份。\n" +
			"命令在持工作区锁的前提下按四判定核对（有记录有密文 / 有记录无密文 / 无记录有密文 /\n" +
			"无记录无密文），需要原密钥时要求 secrets.master_key_file 已持久化为绝对路径；\n" +
			"`--master-key-file` 只用于与配置路径互证。写名册前先预写 pending 审计。\n" +
			"迁移准备副本（主密钥记录来自 secrets adopt-legacy）另核对副本配置：整文件哈希与\n" +
			"审计基线不符时必须带 --config-change-ref，且除 secrets.master_key_file 以外的\n" +
			"配置不得有任何变化。\n" +
			"Adopt the identity of an existing workspace offline. The decision table decides\n" +
			"whether the key is required; --master-key-file only corroborates the configured\n" +
			"path. The pending audit row is written before the roster. A migration-preparation\n" +
			"copy additionally attests its config against the adopt-legacy audit baseline.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireInstanceAdoptPort(deps, "instance adopt"); err != nil {
				return err
			}
			ws := defaultWorkspaceRoot(deps, workspace)
			report, err := deps.InstanceAdopt.Adopt(cmd.Context(), InstanceAdoptRequest{
				Workspace: ws,
				// §3.1.8c 路径口径：`basalt.yaml` 是工作区根的同级部署配置，
				// 不属于工作区资产。
				ConfigPath:      filepath.Join(filepath.Dir(ws), configFileName),
				MasterKeyFile:   strings.TrimSpace(masterKeyFile),
				ConfigChangeRef: strings.TrimSpace(configChangeRef),
			})
			printInstanceAdoptReport(cmd, report)
			if err != nil {
				deps.Logger.Error("instance adopt 未通过",
					"workspace", report.Workspace, "case", report.Case, "error", err)
				return err
			}
			deps.Logger.Info("instance adopt 完成",
				"workspace", report.Workspace, "instance_id", report.InstanceID,
				"case", report.Case, "operation_id", report.AuditOperationID)
			return nil
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "旧 SQLite 工作区根（缺省 <WorkDir>/.basalt）")
	cmd.Flags().StringVar(&masterKeyFile, "master-key-file", "",
		"显式主密钥文件路径（只与 secrets.master_key_file 互证，不一致即拒绝）")
	cmd.Flags().StringVar(&configChangeRef, "config-change-ref", "",
		"迁移准备副本的配置变更单引用（副本配置调整过时必须提供）")
	return cmd
}

// printInstanceAdoptReport 渲染采纳事实。**只打印公开标识**：路径、命中判定编号、实例
// 标识、`key_id` 与指纹（公开摘要）、密文计数与状态三元组；**不打印**密钥材料、密文
// 或任何明文。拒绝时也打印已采集到的部分——"停在哪一步"比一句错误更有用。
// printInstanceAdoptReport prints public identifiers only, including on a refusal.
func printInstanceAdoptReport(cmd *cobra.Command, report InstanceAdoptReport) {
	out := cmd.OutOrStdout()
	if report.Workspace != "" {
		fmt.Fprintf(out, "工作区：%s\n", report.Workspace)
	}
	if report.ConfigPath != "" {
		fmt.Fprintf(out, "配置：%s\n", report.ConfigPath)
	}
	if report.Case > 0 {
		fmt.Fprintf(out, "命中判定：案例 %d\n", report.Case)
	}
	if report.RecordKeyID != "" || report.RecordFingerprint != "" {
		fmt.Fprintf(out, "实例记录：key_id=%s 指纹=%s\n", report.RecordKeyID, report.RecordFingerprint)
	}
	if report.KeyPresent {
		fmt.Fprintf(out, "主密钥：%s（key_id=%s 指纹=%s）\n", report.KeyFile, report.KeyID, report.Fingerprint)
	} else if report.KeyFile != "" {
		fmt.Fprintf(out, "主密钥：配置路径 %s（本次未加载）\n", report.KeyFile)
	}
	if report.Case == 1 || report.Case == 2 {
		fmt.Fprintf(out, "托管密文：%d 条（逐条可解 %t，不可解 %d 条）\n",
			report.ManagedCiphertexts, report.CiphertextsVerified, report.CiphertextsUnreadable)
	}
	if report.FirstFailure != "" {
		fmt.Fprintf(out, "首个失败：%s\n", report.FirstFailure)
	}
	if report.State != "" {
		fmt.Fprintf(out, "启动状态：%s / %s", report.State, report.Action)
		if report.Reason != "" {
			fmt.Fprintf(out, "（原因 %s）", report.Reason)
		}
		fmt.Fprintln(out)
	}
	if report.ConfigChecked {
		fmt.Fprintf(out, "目标配置哈希：%s（config_adjusted=%t）\n",
			report.TargetConfigSHA256, report.ConfigAdjusted)
		if report.ConfigChangeRef != "" {
			fmt.Fprintf(out, "配置变更引用：%s\n", report.ConfigChangeRef)
		}
	}
	if report.RecoveredPending {
		fmt.Fprintln(out, "崩溃恢复：已回填上次未完成的采纳操作")
	}
	if report.InstanceID != "" {
		fmt.Fprintf(out, "实例标识：%s\n", report.InstanceID)
	}
	if report.RosterPath != "" {
		fmt.Fprintf(out, "名册：%s\n", report.RosterPath)
	}
	if report.NewPrivateDir != "" {
		fmt.Fprintf(out, "私密目录（未创建，首次 serve 时生成）：%s\n", report.NewPrivateDir)
	}
	if report.AuditOperationID != "" {
		fmt.Fprintf(out, "审计操作：%s\n", report.AuditOperationID)
	}
	fmt.Fprintf(out, "写实例记录：%t（采纳只写名册）\n", report.WroteSecretsRecord)
	if !report.RestartVerified {
		fmt.Fprintln(out, "重启复验：未验证（由随后的 serve 启动判定）")
	}
}
