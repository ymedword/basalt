package app

// 本文件是受控自我进化的**启动恢复**（颗粒度文档 §2.3.6 / C-13 / C-15 / §3.1.8）：
// 迁移之后、开放服务之前，扫描 `memory_extractions` 的 pending 行并逐条**只查证**——
// 提交事务已经写出候选元数据或草案行 → `success`；否则结果不可知 → `unknown`。两种结论
// 都由存储层在同一事务内写 `status` + `verified_at` 并把来源游标推进到冻结窗口末端，
// 此后该行保留为历史：未查证的 `unknown` 阻断同来源新窗口，查证之后不再阻断（C-13）。
//
// 三条不可越过的边界：
//  1. **只查证、不重放**：本文件不读消息、不调模型、不删草案文件；依赖面里根本没有抽取器，
//     「恢复时顺手重试一次」不是可以忘记的判断，而是接线表达不出来的动作。
//  2. **不改口**：非 pending 行在存储层是幂等空操作——查证过的 `unknown` 不会被下一次启动
//     补成 `success`，也不会写 `failed` 覆盖可能已提交的 `success`（C-14）。
//  3. **存储故障拒绝启动**：读不到待决行、数不了候选 / 草案、写不进查证结论都返回错误并由
//     装配层中止启动（fail-closed）——带着一批结果未知的抽取行开始服务，等于让同来源的
//     新窗口永久阻断或重复抽取。
//
// Startup verification of pending extraction rows. It verifies and never replays: the
// dependency surface has no extractor, so "retry during recovery" cannot be wired in.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// LearningRecoveryDeps 是启动恢复的装配依赖；字段全部显式传入，不走包级变量（宪法 VII）。
// LearningRecoveryDeps carries the startup-recovery dependencies explicitly.
type LearningRecoveryDeps struct {
	// Store 是查证端口，与只读查询面共用**同一个**仓储实例；**必填**——缺它即恢复不可
	// 判定，拒绝启动而不是悄悄跳过。
	// Store is required; a nil store refuses startup instead of silently skipping.
	Store memory.LearningAdminStore
	// Logger 为 nil 时回落进程默认档；恢复日志**不得**带 `step` 键（重路径的九个次序读数
	// 只由装配流程产生，恢复行挂在 `recovery` 之外不新开阶段）。
	// A nil Logger falls back to the process default; recovery never emits a `step` key.
	Logger *slog.Logger
	// Now 为 nil 时回落 time.Now；整轮恢复只取一次，同批查证共享同一时刻。
	// Now is sampled once per startup so all verdicts in one pass share a timestamp.
	Now func() time.Time
}

// recoverLearning 扫描 pending 抽取行并逐条查证；任一步失败都返回错误，由装配层中止启动。
// 没有 pending 行时是零写入空操作——未启用的实例不因恢复产生任何副作用。
// recoverLearning verifies every pending extraction; a storage failure aborts startup.
func recoverLearning(ctx context.Context, deps LearningRecoveryDeps) error {
	if deps.Store == nil {
		return fmt.Errorf("学习抽取启动恢复: 查证端口未装配")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	pending, err := deps.Store.PendingExtractions(ctx)
	if err != nil {
		return fmt.Errorf("学习抽取启动恢复: 扫描待决行: %w", err)
	}
	if len(pending) == 0 {
		return nil
	}

	verifiedAt := now().UTC()
	success, unknown := 0, 0
	for _, row := range pending {
		status, err := verifyPendingExtraction(ctx, deps.Store, row.ExtractionID)
		if err != nil {
			return err
		}
		if err := deps.Store.RecoverExtraction(ctx, row.ExtractionID, status, verifiedAt); err != nil {
			return fmt.Errorf("学习抽取启动恢复: 写查证结论（%s）: %w", row.ExtractionID, err)
		}
		if status == core.LearningExtractionSuccess {
			success++
		} else {
			unknown++
		}
		// 只记身份与计数；抽取行本身不含正文，这里也不得补读消息。
		// Identity and counters only — the row carries no content and none is read here.
		logger.Info("学习抽取：启动查证完成",
			"extraction_id", row.ExtractionID, "agent", row.Agent,
			"status", string(status), "window_to", row.WindowToMessageID)
	}
	logger.Info("学习抽取：启动恢复完成",
		"verified", len(pending), "success", success, "unknown", unknown)
	return nil
}

// verifyPendingExtraction 按 C-15 的事实来源判结论：候选元数据或草案行存在即证明提交事务
// 已写出结果，判 `success`；两者都没有只能判 `unknown`——不存在第三种「看起来像失败」的
// 推断，失败分类要模型与窗口证据，恢复一律不猜。
// verifyPendingExtraction returns success when the commit left candidates or drafts,
// and unknown otherwise; it never guesses a failure class.
func verifyPendingExtraction(ctx context.Context, store memory.LearningAdminStore,
	extractionID string,
) (core.LearningExtractionState, error) {
	candidates, err := store.ExtractionCandidateCount(ctx, extractionID)
	if err != nil {
		return "", fmt.Errorf("学习抽取启动恢复: 统计候选（%s）: %w", extractionID, err)
	}
	drafts, err := store.ExtractionDraftCount(ctx, extractionID)
	if err != nil {
		return "", fmt.Errorf("学习抽取启动恢复: 统计草案（%s）: %w", extractionID, err)
	}
	if candidates > 0 || drafts > 0 {
		return core.LearningExtractionSuccess, nil
	}
	return core.LearningExtractionUnknown, nil
}
