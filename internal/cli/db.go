package cli

// 本文件是 `db` 命令组（设计 §3.1.8）：PostgreSQL 谱系的运维入口。连接参数只从
// `basalt.yaml` 的 `database` 段取——命令面不在别处另找第二份配置，也**不 import**
// `internal/storage/postgres`（迁移由装配层注入的端口执行，见 ports.go）。
// The db command group over the PostgreSQL lineage. Connection settings come from the
// database section of basalt.yaml only; the migration itself runs behind the injected
// port, so this package never imports the implementation.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// requireMigrationPort 是迁移链路四个子命令的统一缺件判定：端口为 nil 即装配缺陷，
// 命令面**不得**自己直连数据库或自己读 SQLite 补出第二条路径。
// requireMigrationPort names a missing wiring instead of opening a second path itself.
func requireMigrationPort(deps Deps, command string) error {
	if deps.Migration != nil {
		return nil
	}
	e := fmt.Errorf("迁移链路端口未注入——%s 需要装配层注入的 DatabaseMigration（internal/app 提供）", command)
	deps.Logger.Error(command + " 未通过：迁移链路端口未注入")
	return e
}

// requireHandoffPort 是 `db verify --handoff` 的缺件判定：端口为 nil 即装配缺陷，
// 命令面**不得**自己开库补出第二套核对路径（交接判据只有一份实现）。
// requireHandoffPort keeps the handoff judgement in exactly one implementation.
func requireHandoffPort(deps Deps, command string) error {
	if deps.Handoff != nil {
		return nil
	}
	e := fmt.Errorf("交接核对端口未注入——%s 需要装配层注入的 HandoffVerification（internal/app 提供）", command)
	deps.Logger.Error(command + " 未通过：交接核对端口未注入")
	return e
}

// defaultWorkspaceRoot 解析 `--workspace` 缺省值：工作区 = <WorkDir>/.basalt，与 status /
// init / profile 同一路径口径（不从配置取）。
// defaultWorkspaceRoot resolves the default workspace root, matching status/init/profile.
func defaultWorkspaceRoot(deps Deps, given string) string {
	if root := strings.TrimSpace(given); root != "" {
		return root
	}
	return filepath.Join(deps.WorkDir, workspaceDirName)
}

// newDBCommand 组装 `db` 命令组。
// newDBCommand assembles the db command group.
func newDBCommand(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "PostgreSQL 谱系运维（迁移 / 导出 / 导入 / 复核 / 切换）",
		Long: "PostgreSQL 谱系运维命令组。连接参数来自 basalt.yaml 的 database 段；\n" +
			"`migrate` / `import` / `switch` 以及 `verify --target` 缺 database.dsn 即拒绝执行——\n" +
			"运行期唯一后端是 PostgreSQL，**不回退** SQLite。\n" +
			"`export`、`verify --dump`、`verify --handoff` 离线可用，不要求 DSN。\n" +
			"Operations on the PostgreSQL lineage. A missing database.dsn is refused, never\n" +
			"silently replaced by the SQLite lineage; export and both verify modes work offline.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// 与根命令同一口径：未知子命令不得被 cobra 当成「请求帮助」静默吞掉。
			if len(args) > 0 {
				return fmt.Errorf("未知子命令 %q：用 --help 查看可用子命令", args[0])
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newDBMigrateCommand(deps),
		newDBExportCommand(deps),
		newDBImportCommand(deps),
		newDBVerifyCommand(deps),
		newDBSwitchCommand(deps),
	)
	return cmd
}

// requireDatabaseDSN 是「要求 DSN」子命令的统一前置（§3.1.8 末段的分流）：装载配置，
// 逐种点名缺文件 / 无效配置 / 空 DSN 三种失败，返回投影好的 `database` 段。
//
// 三种失败都**不**提供降级：不生成缺省 DSN、不打开工作区里的 SQLite 库——"配置没写全"
// 不等于"换个后端接着跑"（宪法原则 VIII：失败即拒启，不回退）。
// The shared precondition for the DSN-requiring subcommands: every failure shape is
// named and none of them degrades to a default or to the SQLite workspace.
func requireDatabaseDSN(deps Deps, command string) (DatabaseConfig, error) {
	path := filepath.Join(deps.WorkDir, configFileName)
	cfg, err := LoadConfig(path, os.LookupEnv)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		e := fmt.Errorf("未发现配置（%s）——%s 需要 database.dsn 指向 PostgreSQL", path, command)
		deps.Logger.Error(command+" 未通过：未发现配置", "path", path, "error", e)
		return DatabaseConfig{}, e
	case err != nil:
		// 原错误上抛：装载层已点名键路径与变量名，且不含凭证值（US-1 的口径照用）。
		deps.Logger.Error(command+" 未通过：配置无效", "path", path, "error", err)
		return DatabaseConfig{}, err
	case strings.TrimSpace(cfg.Database.DSN) == "":
		e := fmt.Errorf("database.dsn 未配置（%s）——%s 需要 PostgreSQL 连接串；运行期唯一后端是 PostgreSQL，不回退 SQLite", path, command)
		deps.Logger.Error(command+" 未通过：缺少 database.dsn", "path", path)
		return DatabaseConfig{}, e
	}
	return cfg.Database, nil
}

// newDBMigrateCommand 组装 `db migrate`：把配置的 PG 带到当前 schema 版本。迁移的
// 次序、advisory 锁与失败回滚都在实现侧（`postgres.Open` → `Migrate` → `ensureSchema`），
// 命令面只负责"缺 DSN 即拒绝"与把版本号打出来。
// newDBMigrateCommand brings the configured PostgreSQL to the current schema version;
// the ordering and locking live behind the port and only the ready version comes back.
func newDBMigrateCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "对配置的 PostgreSQL 执行内嵌版本化迁移（幂等）",
		Long: "连接 basalt.yaml 配置的 PostgreSQL，校验扩展能力后执行内嵌的版本化迁移\n" +
			"（advisory lock 串行、失败整体回滚、重跑幂等）。缺 database.dsn 即拒绝执行——\n" +
			"**不回退** SQLite。serve 首次启动走的是同一条 ensureSchema 路径。\n" +
			"Run the embedded versioned migrations against the configured PostgreSQL; a\n" +
			"missing database.dsn is refused and there is no SQLite fallback.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := requireDatabaseDSN(deps, "db migrate")
			if err != nil {
				return err
			}
			if deps.DatabaseMigrator == nil {
				e := fmt.Errorf("PostgreSQL 迁移端口未注入——db migrate 需要装配层注入的 DatabaseMigrator（internal/app 提供）")
				deps.Logger.Error("db migrate 未通过：迁移端口未注入")
				return e
			}
			version, err := deps.DatabaseMigrator.Migrate(cmd.Context(), cfg)
			if err != nil {
				deps.Logger.Error("db migrate 未通过：迁移失败", "error", err)
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "PostgreSQL schema 已就绪（版本 %d）\n", version)
			deps.Logger.Info("db migrate 完成", "schema_version", version)
			return nil
		},
	}
}

// newDBExportCommand 组装 `db export`。导出属**离线**子命令：它读工作区的 SQLite v6
// 快照，**不要求** `database.dsn`（§3.1.8 末段分流），因此这里不装载配置。
//
// 两种形态二选一：`--out` 导出（可能只产出受限核查包），`--seal` 对已复核的核查包封包。
// 旗标名是冻结面，语义在 T070 落地——`--seal` / `--review-file` 与 `--out` / `--workspace`
// 不混用：混用会让"这次到底盘点没盘点工作区"变成要读实现才知道的事。
// Export is offline and takes no database.dsn. The two shapes are mutually exclusive.
func newDBExportCommand(deps Deps) *cobra.Command {
	var (
		workspace  string
		out        string
		sealDir    string
		reviewFile string
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "从 SQLite v6 工作区导出迁移包（离线可用，不要求 database.dsn）",
		Long: "只读打开工作区的 SQLite v6 快照，产出迁移包与资产清单（离线可用）。\n" +
			"存在未归类文件时只产出受限核查包（不可交付），逐条处置后用 --seal 封包。\n" +
			"Export the SQLite v6 workspace offline; unreviewed assets produce a review\n" +
			"package only, sealed later with --seal.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (strings.TrimSpace(out) == "") == (strings.TrimSpace(sealDir) == "") {
				return errors.New("db export 必须且只能指定 --out <dir>（导出）或 --seal <dir>（对已复核的核查包封包）之一")
			}
			if err := requireMigrationPort(deps, "db export"); err != nil {
				return err
			}
			if strings.TrimSpace(sealDir) != "" {
				if strings.TrimSpace(workspace) != "" {
					return errors.New("db export --seal 不接受 --workspace：封包只读导出目录与处置文件，不再盘点工作区")
				}
				report, err := deps.Migration.Seal(cmd.Context(), MigrationSealRequest{
					Dir:        strings.TrimSpace(sealDir),
					ReviewFile: strings.TrimSpace(reviewFile),
				})
				if err != nil {
					deps.Logger.Error("db export --seal 未通过", "dir", sealDir, "error", err)
					return err
				}
				printSealReport(cmd, report)
				deps.Logger.Info("db export --seal 完成", "dir", report.Dir,
					"review_status", report.ReviewStatus, "included", report.Included,
					"excluded", report.Excluded, "handoff", report.Handoff)
				return nil
			}
			if strings.TrimSpace(reviewFile) != "" {
				return errors.New("db export --review-file 只与 --seal 搭配：导出阶段没有消费者")
			}
			report, err := deps.Migration.Export(cmd.Context(), MigrationExportRequest{
				Workspace: defaultWorkspaceRoot(deps, workspace),
				Out:       strings.TrimSpace(out),
			})
			if err != nil {
				deps.Logger.Error("db export 未通过", "out", out, "error", err)
				return err
			}
			printExportReport(cmd, report)
			deps.Logger.Info("db export 完成", "out", report.OutDir, "batch", report.MigrationBatchID,
				"entries", report.EntryCount, "unclassified", report.UnclassifiedCount,
				"review_status", report.ReviewStatus, "deliverable", report.Deliverable)
			if !report.Deliverable {
				// 返回**非 nil** 错误：核查包不是迁移包，退出码必须能表达这一点。
				return fmt.Errorf("db export：本次只产出受限核查包（%s），**不可交付**——"+
					"按 review/ 逐条处置未归类文件后执行 db export --seal --review-file", report.OutDir)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "要导出的 SQLite v6 工作区目录（缺省 <WorkDir>/.basalt）")
	cmd.Flags().StringVar(&out, "out", "", "迁移包输出目录（必须不存在或为空）")
	cmd.Flags().StringVar(&sealDir, "seal", "", "对已复核的核查包封包（与 --out 二选一）")
	cmd.Flags().StringVar(&reviewFile, "review-file", "", "未归类文件复核结论文件（与 --seal 搭配）")
	return cmd
}

// newDBImportCommand 组装 `db import`：导入写 PostgreSQL，属要求 DSN 的子命令——缺 DSN
// 在**任何**导入动作之前就被拒绝（§3.1.8 末段分流）。
// Import writes to PostgreSQL and therefore requires the DSN before any import work.
func newDBImportCommand(deps Deps) *cobra.Command {
	var dumpDir string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "把迁移包导入配置的 PostgreSQL（要求 database.dsn）",
		Long: "确认目标 schema 为空、无并发 serve 占用后，单事务导入迁移包。\n" +
			"要求 database.dsn；缺 DSN 即拒绝，不回退 SQLite。未复核（pending）的包一律拒收。\n" +
			"Import a migration package into the configured PostgreSQL; the DSN is required\n" +
			"and a pending package is refused.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := requireDatabaseDSN(deps, "db import")
			if err != nil {
				return err
			}
			if err := requireMigrationPort(deps, "db import"); err != nil {
				return err
			}
			if strings.TrimSpace(dumpDir) == "" {
				return errors.New("db import 必须给出迁移包目录（--dump）")
			}
			report, err := deps.Migration.Import(cmd.Context(), cfg, MigrationImportRequest{
				Dump: strings.TrimSpace(dumpDir),
			})
			printImportReport(cmd, report)
			if err != nil {
				deps.Logger.Error("db import 未通过", "dump", dumpDir, "error", err)
				return err
			}
			deps.Logger.Info("db import 完成", "dump", dumpDir,
				"tables", len(report.Tables), "rows", report.TotalRows)
			return nil
		},
	}
	cmd.Flags().StringVar(&dumpDir, "dump", "", "迁移包目录")
	return cmd
}

// newDBVerifyCommand 组装 `db verify`。
//
// 形态是**两种模式**而不是三种：`--handoff` 单独一种（离线交接核对），`--dump` 是另一种
// 并带一个修饰符 `--target`——目标端比对以清单为唯一基准，没有 `--dump` 就没有比对基准，
// 所以 `--target` 单独出现是"缺基准"而不是"另一种模式"。模式判定必须先于 DSN 判定，
// 否则不带模式的 `verify` 会被误报成"缺 DSN"。
// Two modes, not three: --target modifies --dump because the manifest is its only
// baseline. Mode selection precedes the DSN gate.
func newDBVerifyCommand(deps Deps) *cobra.Command {
	var (
		dumpDir    string
		handoffDir string
		target     bool
	)
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "复核迁移包（可加 --target 复核目标库）/ 交接就绪",
		Long: "两种模式恰好选一种：--dump <dir> 离线复核迁移包与包内资产（加 --target 时\n" +
			"再复核配置的目标库）；--handoff <dir> 只读核对**迁移准备副本**的交接就绪\n" +
			"（名册 / 记录 / 密钥路径与权限 / 全部密文 / 目标配置）。\n" +
			"<dir> 是副本的**工作区根**（<copy-root>/.basalt），同级 basalt.yaml 是被核对的配置；\n" +
			"导出包用 --dump 复核。两种模式都离线可用，不要求 database.dsn；--target 要求 DSN。\n" +
			"Two modes: offline --dump (with the optional --target modifier) or offline\n" +
			"--handoff against the migration-preparation copy's workspace root. Only\n" +
			"--target needs the DSN.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hasDump := strings.TrimSpace(dumpDir) != ""
			hasHandoff := strings.TrimSpace(handoffDir) != ""
			switch {
			case hasHandoff && (hasDump || target):
				return errors.New("db verify 必须且只能指定 --dump <dir> / --handoff <dir> 之一（--target 只修饰 --dump）")
			case hasHandoff:
				if err := requireHandoffPort(deps, "db verify --handoff"); err != nil {
					return err
				}
				report, err := deps.Handoff.VerifyHandoff(cmd.Context(), HandoffVerifyRequest{
					Dir: strings.TrimSpace(handoffDir),
				})
				printHandoffReport(cmd, report)
				if err := handoffResult(report, err); err != nil {
					deps.Logger.Error("db verify --handoff 未通过",
						"handoff", handoffDir, "code", report.Code, "error", err)
					return err
				}
				deps.Logger.Info("db verify --handoff 通过", "handoff", handoffDir,
					"instance_id", report.InstanceID, "credentials_checked", report.Credentials.Checked)
				return nil
			case target && !hasDump:
				// 先判 DSN：没有配置的库连"复核哪个目标"都还不成立，此时报"缺 --dump"
				// 会把人引向错误的下一步。
				if _, err := requireDatabaseDSN(deps, "db verify --target"); err != nil {
					return err
				}
				return errors.New("db verify --target 必须同时给出 --dump <dir>：目标库复核以清单为唯一比对基准")
			case !hasDump:
				return errors.New("db verify 必须且只能指定 --dump <dir>（可加 --target 复核目标库）或 --handoff <dir> 之一")
			}
			if err := requireMigrationPort(deps, "db verify"); err != nil {
				return err
			}
			request := MigrationVerifyRequest{Dump: strings.TrimSpace(dumpDir), Target: target}
			if !target {
				report, err := deps.Migration.VerifyDump(cmd.Context(), request)
				printVerifyReport(cmd, report)
				if err := verifyResult(report, err); err != nil {
					deps.Logger.Error("db verify --dump 未通过", "dump", dumpDir, "error", err)
					return err
				}
				deps.Logger.Info("db verify --dump 通过", "dump", dumpDir, "tables", len(report.Tables))
				return nil
			}
			cfg, err := requireDatabaseDSN(deps, "db verify --target")
			if err != nil {
				return err
			}
			report, err := deps.Migration.VerifyTarget(cmd.Context(), cfg, request)
			printVerifyReport(cmd, report)
			if err := verifyResult(report, err); err != nil {
				deps.Logger.Error("db verify --target 未通过", "dump", dumpDir, "error", err)
				return err
			}
			deps.Logger.Info("db verify --target 通过", "dump", dumpDir, "tables", len(report.Tables))
			return nil
		},
	}
	cmd.Flags().StringVar(&dumpDir, "dump", "", "迁移包目录：离线复核 dump 与资产清单")
	cmd.Flags().StringVar(&handoffDir, "handoff", "",
		"副本工作区根（<copy-root>/.basalt）：只读核对交接就绪（名册 / 密钥 / 密文 / 目标配置）")
	cmd.Flags().BoolVar(&target, "target", false, "在 --dump 基础上复核配置的目标库（要求 database.dsn）")
	return cmd
}

// newDBSwitchCommand 组装 `db switch`：切换写 storage_cutover 记录，属要求 DSN 的子命令
// ——缺 DSN 在写入之前就被拒绝。
//
// 机器可判的四道门禁（包完整 / dump 一致 / 目标库一致 / 源端自快照以来未漂移）由
// 实现侧**重跑**一遍，而不是相信"刚才 verify 过了"。`--workspace` 是**必填**：FR-092
// 要求切换前必须重盘 A_cut 与源库身份，缺省跳过会让 source_changed_since_snapshot
// 判据 fail-open，因此缺失即在调用端口之前拒绝。
//
// **handoff 门禁不在本命令内**：本命令既不跑它，也不声称它已核对——交接就绪由
// `db verify --handoff <dir>` 单独判定。
// Three machine-checkable gates are re-run here; the handoff gate runs in
// db verify --handoff and is neither run nor claimed here.
func newDBSwitchCommand(deps Deps) *cobra.Command {
	var (
		dumpDir   string
		workspace string
	)
	cmd := &cobra.Command{
		Use:   "switch",
		Short: "把工作区切到 PostgreSQL（写入 storage_cutover；要求 database.dsn）",
		Long: "重跑四道机器可判门禁（包内资产 / dump 摘要 / 目标库 / 源端重盘）后写入 storage_cutover 记录\n" +
			"（first_write_at 留 NULL）。要求 database.dsn；缺 DSN 即拒绝，不回退 SQLite。\n" +
			"--workspace 必填：切换前必须重盘 A_cut 与源库身份（尺寸 + SHA-256）；缺失即拒绝。\n" +
			"目标库复核通过后、写切换记录之前，把包内 `memory/` 的旧 markdown 记忆一次性只读\n" +
			"导入目标库并逐条对账（D-18）：有任何不可读 / 冲突即拒绝切换，storage_cutover 不落行。\n" +
			"源工作区的旧记忆文件原样保留（D-22 回退层）。\n" +
			"交接就绪（handoff）核对不在本命令内——先用 db verify --handoff <dir> 通过，\n" +
			"本命令不声称已核对。\n" +
			"Switch re-runs the three gates, imports the legacy memory and reconciles every entry,\n" +
			"then writes the cutover row. The handoff gate is out of scope and never reported\n" +
			"as checked.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := requireDatabaseDSN(deps, "db switch")
			if err != nil {
				return err
			}
			if err := requireMigrationPort(deps, "db switch"); err != nil {
				return err
			}
			if strings.TrimSpace(dumpDir) == "" {
				return errors.New("db switch 必须给出迁移包目录（--dump）：切换证据绑定的就是那份清单")
			}
			if strings.TrimSpace(workspace) == "" {
				// FR-092：源端重盘是切换前的**必须**项，不是可选增强。
				return errors.New("db switch 必须给出源工作区（--workspace）：切换前必须重盘 A_cut 与源库身份（FR-092）")
			}
			report, err := deps.Migration.Switch(cmd.Context(), cfg, MigrationSwitchRequest{
				Dump:      strings.TrimSpace(dumpDir),
				Workspace: strings.TrimSpace(workspace),
			})
			if err != nil {
				deps.Logger.Error("db switch 未通过", "dump", dumpDir, "error", err)
				return err
			}
			printSwitchReport(cmd, report)
			deps.Logger.Info("db switch 完成", "dump", dumpDir, "cutover_id", report.CutoverID,
				"manifest_sha256", report.ManifestSHA256)
			return nil
		},
	}
	cmd.Flags().StringVar(&dumpDir, "dump", "", "迁移包目录：切换证据绑定的清单")
	cmd.Flags().StringVar(&workspace, "workspace", "", "源工作区目录：切换前必须重盘核对源端未漂移（必填）")
	return cmd
}

// verifyResult 是复核结论的兜底判定：实现侧按契约在"不通过"时必带错误，但退出码由
// 命令面负责——万一某天只回报告不回错误，"结论：**不通过**"配上 exit 0 会被脚本读成
// 通过。判据面在这里再兜一层，代价是一次布尔判断。
// The port contract pairs a failed verdict with an error; the exit code is still this
// command's responsibility, so a silent OK=false never leaves as exit 0.
func verifyResult(report MigrationVerifyReport, err error) error {
	if err != nil {
		return err
	}
	if report.OK {
		return nil
	}
	return errors.New("db verify：实现侧报告复核不通过却未附带错误——按 fail-closed 处理")
}

// handoffResult 与 verifyResult 同一口径：实现侧按契约在"不就绪"时必带错误，退出码仍由
// 命令面兜底——万一某天只回报告不回错误，"**不就绪**"配上 exit 0 会被脚本读成通过。
// The port contract pairs a not-ready verdict with an error; the exit code is still this
// command's responsibility, so a silent Ready=false never leaves as exit 0.
func handoffResult(report HandoffVerifyReport, err error) error {
	if err != nil {
		return err
	}
	if report.Ready {
		return nil
	}
	return errors.New("db verify --handoff：实现侧报告不就绪却未附带错误——按 fail-closed 处理")
}

// ─────────────────────────────── 报告渲染 ───────────────────────────────
//
// 渲染只做**投影**：逐项读数原样打出，不在这里做二次判定（判据在实现侧）。
// 唯一例外是"不可交付 / 未执行 / 未核对"这三类**否定事实**——它们必须显式出现，
// 否则缺省读法会把"没跑"当成"跑过了"。缺一条都会让运维把结果读反。
// Rendering only projects readings. The three negative facts (not deliverable, not
// executed, not checked) must be printed explicitly, or silence reads as success.

// printExportReport 渲染导出结果：三元组、条目统计与核查状态。
func printExportReport(cmd *cobra.Command, report MigrationExportReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "迁移包目录：%s\n", report.OutDir)
	fmt.Fprintf(out, "迁移批次：%s\n", report.MigrationBatchID)
	fmt.Fprintf(out, "源实例：%s（来源 %s）\n", report.SourceInstanceID, report.SourceInstanceIDOrigin)
	fmt.Fprintf(out, "源快照 SHA-256：%s\n", report.SourceSnapshotSHA256)
	fmt.Fprintf(out, "资产条目：%d 条（未归类 %d 条）\n", report.EntryCount, report.UnclassifiedCount)
	if len(report.Agents) > 0 {
		fmt.Fprintf(out, "覆盖 Agent：%s\n", strings.Join(report.Agents, "、"))
	}
	if report.Deliverable {
		fmt.Fprintf(out, "核查状态：%s（可交付）\n", report.ReviewStatus)
		return
	}
	fmt.Fprintf(out, "核查状态：%s（**不可交付**）\n", report.ReviewStatus)
	if report.UnclassifiedDigest != "" {
		fmt.Fprintf(out, "未归类摘要：%s\n", report.UnclassifiedDigest)
	}
	if len(report.ContentHints) > 0 {
		rules := make([]string, 0, len(report.ContentHints))
		for rule := range report.ContentHints {
			rules = append(rules, rule)
		}
		slices.Sort(rules)
		parts := make([]string, 0, len(rules))
		for _, rule := range rules {
			parts = append(parts, fmt.Sprintf("%s %d", rule, report.ContentHints[rule]))
		}
		fmt.Fprintf(out, "内容扫描（启发式，仅提示）：%s\n", strings.Join(parts, "、"))
		fmt.Fprintln(out, "  命中不等于泄密，未命中也不证明包内无秘密；逐条处置仍由人工判断。")
	}
	fmt.Fprintf(out, "下一步：按 %s 逐条处置后执行 db export --seal --review-file\n",
		filepath.Join(report.OutDir, "review"))
}

// printSealReport 渲染封包结果：最终核查状态与三类处置条数。
func printSealReport(cmd *cobra.Command, report MigrationSealReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "封包目录：%s\n", report.Dir)
	fmt.Fprintf(out, "核查状态：%s\n", report.ReviewStatus)
	fmt.Fprintf(out, "处置统计：进包 %d / 排除 %d / 移交 %d\n", report.Included, report.Excluded, report.Handoff)
	if report.Reviewer != "" {
		fmt.Fprintf(out, "复核人：%s\n", report.Reviewer)
	}
	if report.ReviewDigest != "" {
		fmt.Fprintf(out, "核查摘要：%s\n", report.ReviewDigest)
	}
}

// printImportReport 渲染导入结果：逐表行数（实现侧已按表名排序）+ 合计。
func printImportReport(cmd *cobra.Command, report MigrationImportReport) {
	if len(report.Tables) == 0 {
		return
	}
	out := cmd.OutOrStdout()
	for _, row := range report.Tables {
		fmt.Fprintf(out, "  %-32s %d 行\n", row.Table, row.Rows)
	}
	fmt.Fprintf(out, "合计 %d 表 / %d 行（耗时 %s）\n",
		len(report.Tables), report.TotalRows, report.Duration.Round(time.Millisecond))
}

// printVerifyReport 渲染复核读数：逐表两侧行数与摘要判定，再逐条列差异。
func printVerifyReport(cmd *cobra.Command, report MigrationVerifyReport) {
	if report.Mode == "" {
		return
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "复核模式：%s\n", report.Mode)
	for _, row := range report.Tables {
		digest := "相符"
		if !row.RowDigestOK {
			digest = "**不符**"
		}
		fmt.Fprintf(out, "  %-32s 源 %d 行 / 目标 %d 行，行摘要%s\n",
			row.Table, row.SourceRows, row.TargetRows, digest)
	}
	for _, line := range report.MissingIDs {
		fmt.Fprintf(out, "  主键差异：%s\n", line)
	}
	for _, line := range report.ExtraIDs {
		fmt.Fprintf(out, "  主键差异：%s\n", line)
	}
	for _, line := range report.ReferenceFailures {
		fmt.Fprintf(out, "  引用不一致：%s\n", line)
	}
	for _, line := range report.Notes {
		fmt.Fprintf(out, "  说明：%s\n", line)
	}
	verdict := "通过"
	if !report.OK {
		verdict = "**不通过**"
	}
	fmt.Fprintf(out, "结论：%s（耗时 %s）\n", verdict, report.Duration.Round(time.Millisecond))
}

// printHandoffReport 渲染交接核对读数：身份 / 记录 / 密钥 / 配置逐项 + 两组凭据计数。
//
// 三处"否定事实"必须显式出现，缺省读法才不会被读反：名册**缺失**、目标配置**未核对**
// （普通存量实例）与**未完成**（迁移准备副本没对完）。报告只含摘要与计数，不打印任何
// 密钥材料、密文或明文。
// The negative facts are printed explicitly; the report carries summaries and counts only.
func printHandoffReport(cmd *cobra.Command, report HandoffVerifyReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "副本工作区：%s\n", report.Workspace)
	fmt.Fprintf(out, "配置：%s\n", report.ConfigPath)
	if report.RosterPresent {
		fmt.Fprintf(out, "实例名册：%s（%s）\n", report.InstanceID, report.RosterPath)
	} else {
		fmt.Fprintf(out, "实例名册：**缺失**（%s）\n", report.RosterPath)
	}
	if report.RecordPresent {
		fmt.Fprintf(out, "主密钥记录：key_id=%s 指纹=%s\n", report.RecordKeyID, report.RecordFingerprint)
	} else {
		fmt.Fprintln(out, "主密钥记录：**无**（纯 env 或尚未初始化）")
	}
	if report.KeyFile != "" {
		persisted := "否"
		if report.KeyPathPersisted {
			persisted = "是"
		}
		fmt.Fprintf(out, "主密钥文件：%s（路径已持久化：%s）\n", report.KeyFile, persisted)
		if report.KeyPresent {
			fmt.Fprintf(out, "密钥核对：key_id=%s 指纹=%s\n", report.KeyID, report.Fingerprint)
		}
	}
	switch {
	case report.ConfigChecked:
		fmt.Fprintf(out, "目标配置核对：已核对（当前 %s == 审计 %s）\n",
			report.CurrentConfigSHA256, report.TargetConfigSHA256)
	case report.ConfigRequired:
		fmt.Fprintf(out, "目标配置核对：**未完成**（审计 %s，当前 %s）\n",
			report.TargetConfigSHA256, report.CurrentConfigSHA256)
	default:
		fmt.Fprintln(out, "目标配置核对：**未核对**")
	}
	if report.ConfigNote != "" {
		fmt.Fprintf(out, "  说明：%s\n", report.ConfigNote)
	}
	// (a) 与 (b) 是两个独立报告对象，**不**相加成一个"总数"（§2.12.1）。
	fmt.Fprintf(out, "历史密文：检查 %d 条，失败 %d 条\n",
		report.Credentials.Checked, report.Credentials.Failed)
	fmt.Fprintf(out, "当前可用性：检查 %d 条，失败 %d 条\n",
		report.Credentials.CurrentChecked, report.Credentials.CurrentFailed)
	writeSecretsVerifyBreakdown(out, report.Credentials)
	if report.Ready {
		fmt.Fprintf(out, "结论：交接就绪（耗时 %s）\n", report.Duration.Round(time.Millisecond))
		return
	}
	fmt.Fprintf(out, "结论：**不就绪**（%s，耗时 %s）\n",
		report.Code, report.Duration.Round(time.Millisecond))
	if report.Detail != "" {
		fmt.Fprintf(out, "原因：%s\n", report.Detail)
	}
	if report.Recovery != "" {
		fmt.Fprintf(out, "恢复指引：%s\n", report.Recovery)
	}
}

// printSwitchReport 渲染切换事实，并**显式**打印本命令没有执行 / 没有核对的门禁。
// `--workspace` 已是必填（FR-092），源端重盘在这里必然是"已执行"——不再保留
// "未执行"分支：那种路径只剩拒绝，走不到渲染。
func printSwitchReport(cmd *cobra.Command, report MigrationSwitchReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "切换记录：%s（cutover_at=%s）\n", report.CutoverID, report.CutoverAt.Format(time.RFC3339))
	fmt.Fprintf(out, "绑定清单 SHA-256：%s\n", report.ManifestSHA256)
	fmt.Fprintf(out, "源 schema 版本：v%d\n", report.SourceSchemaVersion)
	window := "已关闭（首次业务写入已登记）"
	if report.RollbackWindowOpen {
		window = "开放（first_write_at 仍为 NULL）"
	}
	fmt.Fprintf(out, "回退窗口：%s\n", window)
	printMemoryImportReport(out, report.Memory)
	printMemoryBaselineReport(out, report.MemoryBaseline)
	fmt.Fprintln(out, "源端重盘：已执行（源工作区与清单一致）")
	fmt.Fprintln(out, "交接就绪（handoff）：**未核对**——本命令不跑该门禁；切换前必须先用 db verify --handoff <dir> 通过")
}

// printMemoryBaselineReport 渲染导入后的行数对账（S-F04）：三个数相等才写切换记录，
// 命令面因此只说事实，不复述"已核对"的口头保证——数字本身就是核对结果。`Checked`
// 为假（替身 / 历史报告）时**不打印**，不拿零值冒充读到的零行。
//
// The line is printed only when a reconciliation was actually produced; zero values are
// never dressed up as measured zeros.
func printMemoryBaselineReport(out io.Writer, baseline MigrationMemoryBaseline) {
	if !baseline.Checked {
		return
	}
	fmt.Fprintf(out, "导入后基线：memory_entries %d 行（导入前 %d + 本次导入 %d）——回退判据的该表基线取此值\n",
		baseline.ObservedRows, baseline.ImportRows, baseline.Imported)
}

// printMemoryImportReport 渲染旧记忆导入的四类对账（D-18）。三种口径不许含糊：
//   - 零条**不是**"迁移成功"，必须把"源里没有可导入的旧记忆条目"这句原话说出来；
//   - 跳过 = 权威库里已有同一条（重复导入幂等），不是失败；
//   - 冲突 / 不可读非空时切换不会成立（实现侧已拒绝），这里仍逐条打出，供操作者处置。
//
// The rendering refuses to dress an empty source up as a successful migration; skipped
// entries are the idempotent case, conflicts and unreadable ones are listed verbatim.
func printMemoryImportReport(out io.Writer, report MigrationMemoryImport) {
	fmt.Fprintf(out, "旧记忆导入：源条目 %d 条（导入 %d / 跳过 %d / 冲突 %d / 不可读 %d）\n",
		report.Total, report.Imported, report.Skipped, len(report.Conflicts), len(report.Unreadable))
	if report.Total == 0 {
		fmt.Fprintln(out, "  **空载体**：源里没有可导入的旧记忆条目——这条读数只说明旧载体为空，不是「迁移成功」的证据")
	}
	if report.Skipped > 0 {
		fmt.Fprintln(out, "  跳过 = 权威库里已有同一条（重复导入幂等），不是失败")
	}
	for _, line := range report.Conflicts {
		fmt.Fprintf(out, "  冲突：%s（权威库那条已不在生效态，导入不覆盖，须人工裁决）\n", line)
	}
	for _, line := range report.Unreadable {
		fmt.Fprintf(out, "  不可读：%s\n", line)
	}
}
