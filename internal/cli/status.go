package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/internal/core"
)

// configFileName 是实例配置的文件名——与工作区**同级**、不在 .basalt/ 内
// （Operations §6；颗粒度文档 §2.4 取数澄清）。
// configFileName is the instance configuration file, a sibling of the workspace.
//
// 装配层（internal/app）为选日志档位也读同一份文件；两处共用同一个字面量是本包里
// 的唯一定义，改动时两处都要跟（app 侧有自己的同名常量）。
const configFileName = "basalt.yaml"

// newStatusCommand 组装 status 子命令：三节只读诊断报告。
// newStatusCommand builds the status subcommand.
//
// 与 init 的分工（流程 D）：status **只读**——不建目录、不代建库、不迁移，唯一工作区
// 路径 = 当前工作目录的 .basalt/（不从配置取，一份报告的骨架与库检查必须出自同一
// 路径）。三节逐节尽力：任一节失败只影响自己的结论，不影响后续节照常检查。
// status is strictly read-only and always diagnoses the same workspace; a failed
// section never stops the following ones.
func newStatusCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "打印只读诊断报告（工作区 / 配置 / 存储）",
		Long: "打印只读诊断报告：工作区骨架、实例配置、存储状态三节逐节检查。\n" +
			"Print a read-only diagnostic report covering the workspace skeleton, " +
			"the instance configuration and the storage state.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd.Context(), cmd.OutOrStdout(), deps)
		},
	}
}

// runStatus 依次检查三节并汇总：全通过返回 nil（exit 0），任一节失败返回错误
// （装配层据此 exit 1）。
// runStatus walks the three sections and summarises: nil means exit 0.
//
// 失败路径双轨都在：报告文案落 stdout 给人看，同一失败另记一条结构化 ERROR 给日志
// ——只印报告不记日志，线上排障就只剩一份没人保存的屏幕输出（FR-006 / 约束 15）。
// Both tracks are present: the human-readable report on stdout and one structured
// ERROR line per failed section.
func runStatus(ctx context.Context, out io.Writer, deps Deps) error {
	wsRoot := filepath.Join(deps.WorkDir, workspaceDirName)
	dbPath := filepath.Join(wsRoot, databaseFileName)
	configPath := filepath.Join(deps.WorkDir, configFileName)

	fmt.Fprintf(out, "工作区 %s\n", wsRoot)

	var failed []string

	// 节 1：工作区骨架。
	missing, err := deps.Workspace.Inspect(wsRoot)
	switch {
	case errors.Is(err, core.ErrWorkspaceNotInitialized):
		writeSection(out, "工作区", "未初始化——先运行 basalt init")
		deps.Logger.Error("工作区未初始化", "path", wsRoot, "error", err)
		failed = append(failed, "工作区")
	case err != nil:
		writeSection(out, "工作区", fmt.Sprintf("无法检查——%v", err))
		deps.Logger.Error("工作区检查失败", "path", wsRoot, "error", err)
		failed = append(failed, "工作区")
	case len(missing) > 0:
		writeSection(out, "工作区", fmt.Sprintf("骨架不完整（缺 %s）——先运行 basalt init",
			strings.Join(missing, "、")))
		deps.Logger.Error("工作区骨架不完整", "path", wsRoot, "missing", strings.Join(missing, "、"))
		failed = append(failed, "工作区")
	default:
		// 齐备时不列条目：清点的是"能不能用"，把六目录五文件全打印一遍只是噪音。
		writeSection(out, "工作区", "就绪（骨架条目齐备）")
	}

	// 节 2：实例配置——缺失容忍（init 本来就不生成配置），存在但无效才算失败。
	cfg, err := LoadConfig(configPath, os.LookupEnv)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeSection(out, "配置", fmt.Sprintf("未发现配置（%s）——按纯工作区诊断继续", configPath))
	case err != nil:
		writeSection(out, "配置", fmt.Sprintf("无效——%v", err))
		deps.Logger.Error("配置无效", "path", configPath, "error", err)
		failed = append(failed, "配置")
	default:
		writeSection(out, "配置", fmt.Sprintf("已装载（%s）——log.level=%s log.format=%s",
			configPath, orDefault(cfg.Log.Level, "（缺省）"), orDefault(cfg.Log.Format, "（缺省）")))
	}

	// 节 3：存储——只读打开；各异常态各自点名，就绪时报版本与表清点。
	if err := checkStorage(ctx, out, deps, dbPath); err != nil {
		failed = append(failed, "存储")
	}

	if len(failed) > 0 {
		summary := strings.Join(failed, "、")
		fmt.Fprintf(out, "汇总：%d 节未通过（%s）\n", len(failed), summary)
		return fmt.Errorf("status 诊断未通过（%s）", summary)
	}
	fmt.Fprintln(out, "汇总：全部通过")
	return nil
}

// checkStorage 是节 3：只读打开库并清点版本与表；返回非 nil 表示本节判定为失败
// （报告文案已写好，日志亦已落）。
// checkStorage is section 3: a read-only open plus version and table inventory. A
// non-nil return means the section failed and has already been reported.
func checkStorage(ctx context.Context, out io.Writer, deps Deps, dbPath string) error {
	db, err := deps.Store.OpenReadOnly(ctx, dbPath, deps.SupportedSchemaVersion)
	switch {
	case errors.Is(err, core.ErrDatabaseMissing):
		return storageFailure(out, deps, "存储", dbPath,
			"库未就绪——先运行 basalt init", err)
	case errors.Is(err, core.ErrSchemaOutdated):
		return storageFailure(out, deps, "存储", dbPath,
			fmt.Sprintf("库版本旧于二进制——status 只校验、不迁移（%v）", err), err)
	case errors.Is(err, core.ErrSchemaTooNew):
		return storageFailure(out, deps, "存储", dbPath,
			fmt.Sprintf("库版本高于二进制——请升级 basalt 二进制（%v）", err), err)
	case errors.Is(err, core.ErrSchemaCorrupt):
		// 版本表在、行没了——既非新库也非任何已知版本，不能靠"再跑一次 init"糊过去
		// （init 对已存在的库是零写），只能人工判定：要点名库路径并让人做决定。
		return storageFailure(out, deps, "存储", dbPath,
			fmt.Sprintf("库已损坏——版本表在但无版本行，需人工判定，不要直接删库（%v）", err), err)
	case err != nil:
		return storageFailure(out, deps, "存储", dbPath, fmt.Sprintf("打开失败——%v", err), err)
	}

	version, versionErr := db.SchemaVersion(ctx)
	tables, tablesErr := db.TableNames(ctx)
	closeErr := db.Close()
	if err := errors.Join(versionErr, tablesErr, closeErr); err != nil {
		return storageFailure(out, deps, "存储", dbPath, fmt.Sprintf("读取失败——%v", err), err)
	}

	writeSection(out, "存储", fmt.Sprintf("就绪（%s，schema v%d，%d 张表）", dbPath, version, len(tables)))
	return nil
}

// storageFailure 统一节 3 的失败三件套：报告文案、结构化 ERROR、返回错误。
// storageFailure centralizes section 3's failure triple.
func storageFailure(out io.Writer, deps Deps, section, path, detail string, err error) error {
	writeSection(out, section, detail)
	deps.Logger.Error("存储状态异常", "path", path, "error", err)
	return err
}

// writeSection 打一行小节结论。
// writeSection prints one section verdict.
func writeSection(out io.Writer, section, detail string) {
	fmt.Fprintf(out, "  %s：%s\n", section, detail)
}

// orDefault 把空配置值显示成占位说明——展示层不替配置决定默认值是什么。
// orDefault renders an empty value as a placeholder rather than inventing the
// default at the presentation layer.
func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
