package workspace

// 本文件是崩溃恢复六步里的第 ⑤ 步（颗粒度文档 §2.3 流程 G；契约 run-and-errors.md
// §4.3.2 第 5 步）：目录交换（技术方案 §3.4.2 的"多文件提交"）中断在半路时，按残留把工作区
// 收敛回一个确定状态——**回滚**（`.backup` 恢复为正式目录）或**前滚**（正式目录已就位，
// 删掉残留）。调用方是**持锁的重命令**，启动时（取锁后、开跑前）跑一次。
// This file is step ⑤ of the crash-recovery scan: it converges the workspace after an
// interrupted directory exchange, either by rolling back or by rolling forward.
//
// 判定**只看文件系统状态**（不读账本、不猜意图），两条分支各自收敛到零残留，所以本函数
// **幂等**：从同一初始态重跑结果相同，收敛后再跑无事可做。三种状态四种落点：
// 正式目录在位 → 前滚删残留；正式目录缺失 + 备份在位 → 回滚（先在位、再删作废的 staging）；
// 正式条目在位但不是目录 → 判定不成立，保留现场 + WARN；既无备份也无正式目录（只剩
// staging）→ 两份内容都不完整，删掉任何一份都可能毁掉唯一副本，保留现场 + WARN。
// The verdict reads the filesystem only. Two states have a defined resolution (forward,
// rollback) and two are deliberately left intact and logged — where nothing can be judged,
// nothing is deleted.
//
// **US-12 起有 staged 写入方**：Agent 多文件提交（`SubmitAgentFiles`）与模板创建
// （`CreateAgentStaged`）都会在 `agents/` 下产出 `<name>.staging` / `<name>.backup`，
// 本段因此是**活动分支**——启动恢复按残留收敛，冲突与判不动的现场保留并告警。
// US-12 adds the staged writers (multi-file submission and template creation under
// agents/), so this path is live; the residue pass converges or preserves-and-warns.

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

const (
	// backupSuffix / stagingSuffix 是目录交换的两个残留后缀（技术方案 §3.4.2：原目录 →
	// `.backup`，新内容先在 `.staging` 构建再顶上；契约 §4.3.2 第 5 步把两者合称
	// "工作区替换残留"）。后缀即约定：`<name>.backup` 与 `<name>.staging` 与正式目录
	// `<name>` 同在 agents/ 下——**同父目录**才可能用两次 rename 完成交换。
	// The two residue suffixes of the exchange; sibling naming under agents/ is what makes
	// the two-rename swap possible.
	backupSuffix  = ".backup"
	stagingSuffix = ".staging"
)

// residueAction 是一次残留处置的落点（只用于计数与留痕，不对外）。
// residueAction is the verdict of one residue pass; it exists for counting and logging.
type residueAction int

const (
	// actionKept 是"判不动、保留现场"（第三态与第四态）。
	actionKept residueAction = iota

	// actionRollback 是把 `.backup` 恢复为正式目录。
	actionRollback

	// actionForward 是正式目录已在位、删除残留。
	actionForward
)

// ResidueReport 是一次残留判定的两个读数（契约 §4.3.2 第 6 步：条数写进启动摘要的
// 工作区部分）。
// ResidueReport carries the two counters of one residue pass.
type ResidueReport struct {
	// RolledBack 是回滚的目录数（`.backup` 被恢复为正式目录）。
	RolledBack int

	// RolledForward 是前滚的目录数（正式目录就位，残留被删除）。
	RolledForward int
}

// RecoverWorkspaceResidue 扫描 root/agents/ 下的交换残留并处置它们（分支口径见文件头）。
// 返回的 error 是**尽力而为**的汇总（`errors.Join`）：一个 Agent 的残留处置失败不影响
// 其余 Agent，但调用方必须看见它（重路径据此决定要不要开跑）。
// RecoverWorkspaceResidue scans root/agents/ for exchange residue and resolves it; the
// error aggregates per-agent failures without hiding any of them from the caller.
func RecoverWorkspaceResidue(root string, logger *slog.Logger) (report ResidueReport, err error) {
	if logger == nil {
		// 冻结签名收 `*slog.Logger`；调用方图省事传 nil 不该让恢复崩在日志上。
		logger = slog.Default()
	}

	base, err := os.OpenRoot(root) // 全部读写经根约束（与同包其余读取面同一纪律）
	if err != nil {
		return report, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	// agents/ 缺失 = 工作区未建好（与装载面 LoadAll 同一口径：目录不存在时"没有 Agent"
	// 与"工作区没建好"无法区分），如实上抛。
	dir, err := base.Open(agentsDirName)
	if err != nil {
		return report, fmt.Errorf("打开 Agent 目录: %w", err)
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭 Agent 目录 %s: %w", agentsDirName, closeErr))
		}
	}()

	entries, err := dir.ReadDir(-1)
	if err != nil {
		return report, fmt.Errorf("读取 Agent 目录: %w", err)
	}

	// 残留按 **Agent 名分组**、一次判完：`.backup` 与 `.staging` 是同一次交换的两半，
	// 分开判会出现"先删了 staging，回头再想回滚却没有备份"的次序错误。
	names := residueNames(entries)
	for i := 0; i < len(names); {
		stem := residueStem(names[i])
		hasBackup, hasStaging := false, false
		for ; i < len(names) && residueStem(names[i]) == stem; i++ {
			if strings.HasSuffix(names[i], backupSuffix) {
				hasBackup = true
			} else {
				hasStaging = true
			}
		}

		action, stepErr := resolveResidue(base, logger, stem, hasBackup, hasStaging)
		switch action {
		case actionRollback:
			report.RolledBack++
		case actionForward:
			report.RolledForward++
		case actionKept:
		}
		if stepErr != nil {
			err = errors.Join(err, stepErr)
		}
	}

	// 留痕只到「计数」为止（FR-027：日志与错误文案不得回显文件全文或配置原文；逐条处置
	// 另有一条 INFO / WARN 点名 Agent 名）。
	logger.Info("工作区残留判定完成", "rolled_back", report.RolledBack, "rolled_forward", report.RolledForward)
	return report, err
}

// resolveResidue 处置一个 Agent 名下的全部残留（四种落点见文件头），返回落点供计数；
// 返回的 error 由调用方汇总，单个 Agent 的失败不拖垮整批。
// resolveResidue resolves the residue of one agent; failures stay per-agent.
func resolveResidue(base *os.Root, logger *slog.Logger, stem string, hasBackup, hasStaging bool) (residueAction, error) {
	formalRel := path.Join(agentsDirName, stem)

	// 判据落在**条目自身**（Lstat，不跟随软连接）：软连接不是正式目录，认了它就会把备份
	// 删掉；同包装载面同样把软连接条目如实忽略（"附属物不得成为第二入口"）。
	info, statErr := base.Lstat(filepath.FromSlash(formalRel))
	switch {
	case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
		return actionKept, fmt.Errorf("检查 Agent 目录 %s: %w", formalRel, statErr)
	case statErr == nil && !info.IsDir():
		// 正式条目在位但不是目录（文件 / 软连接）：两条分支的判据都不成立——回滚的
		// rename 会被目标占住，前滚的"正式目录完整"是假象——保留现场，点名类型。
		logger.Warn("工作区残留：正式条目不是目录，保留现场",
			"agent", stem, "reason", "formal entry is not a directory",
			"detail", fmt.Sprintf("%s：%s", formalRel, fileKind(info)))
		return actionKept, nil
	}
	formalIsDir := statErr == nil

	switch {
	case formalIsDir:
		// 前滚：正式目录在位 = 交换已完成（或其后的清理没跑完）→ 删残留。
		removed, removeErr := removeResidues(base, stem, hasBackup, hasStaging)
		if removeErr != nil {
			return actionKept, removeErr
		}
		logger.Info("工作区残留：前滚（正式目录在位，删除残留）",
			"agent", stem, "action", "forward", "removed", removed)
		return actionForward, nil

	case hasBackup:
		// 回滚：正式目录缺失 + 备份在位 = 交换断在"原目录已移走、新内容还没顶上"之间
		//（§11.4 的第一次 rename 之后）。先恢复、后清理——rename 失败时备份与 staging
		// 都还在，下次启动原样重来。
		if renameErr := base.Rename(
			filepath.FromSlash(path.Join(agentsDirName, stem+backupSuffix)),
			filepath.FromSlash(formalRel),
		); renameErr != nil {
			return actionKept, fmt.Errorf("回滚 Agent 目录 %s: %w", formalRel, renameErr)
		}
		stagingRemoved := 0
		if hasStaging {
			// 交换已按回滚收场，staging 是作废的新内容（它从未经校验顶上过），一并清理。
			var stageErr error
			stagingRemoved, stageErr = removeResidues(base, stem, false, true)
			if stageErr != nil {
				return actionRollback, stageErr
			}
		}
		logger.Info("工作区残留：回滚（.backup 恢复为正式目录）",
			"agent", stem, "action", "rollback", "staging_removed", stagingRemoved)
		return actionRollback, nil

	case hasStaging:
		// 第三态：既无备份也无正式目录，只剩半成品 staging——两份内容都不完整，删掉任何
		// 一份都可能毁掉唯一副本（判定不了的事不做），保留现场并点名。
		logger.Warn("工作区残留：无备份也无正式目录，保留现场",
			"agent", stem, "reason", "no backup and no formal directory",
			"detail", path.Join(agentsDirName, stem+stagingSuffix))
		return actionKept, nil
	}
	return actionKept, nil
}

// residueNames 取本次扫描认得的残留条目名：只认**目录**（软连接条目连自身都不算目录，
// 与装载面同一判据）、只认两个后缀、且后缀前**必须有主名**。显式按名字节序排序：判定的
// 顺序不得依赖 `ReadDir` 的隐式序。
// residueNames lists the residue entries this pass recognises, sorted explicitly.
func residueNames(entries []fs.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || residueStem(e.Name()) == "" {
			continue
		}
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// residueStem 去掉残留后缀给出主名；不是残留（或没有主名，如目录 `.backup` 本身）返回空串。
// residueStem strips the residue suffix, returning "" for non-residue names.
func residueStem(name string) string {
	for _, suffix := range []string{backupSuffix, stagingSuffix} {
		if stem := strings.TrimSuffix(name, suffix); stem != name && stem != "" {
			return stem
		}
	}
	return ""
}

// removeResidues 删除该 Agent 名下的指定残留并返回删除条数。备份先于 staging：先删
// staging 再删备份时若中途出错，剩下的备份仍是可回滚的完整副本。
// removeResidues deletes the requested residues, backup first, and counts them.
func removeResidues(base *os.Root, stem string, backup, staging bool) (int, error) {
	removed := 0
	for _, r := range []struct {
		suffix string
		want   bool
	}{{backupSuffix, backup}, {stagingSuffix, staging}} {
		if !r.want {
			continue
		}
		rel := path.Join(agentsDirName, stem+r.suffix)
		if err := base.RemoveAll(filepath.FromSlash(rel)); err != nil {
			return removed, fmt.Errorf("删除残留 %s: %w", rel, err)
		}
		removed++
	}
	return removed, nil
}
