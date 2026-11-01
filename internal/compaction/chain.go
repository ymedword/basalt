package compaction

import (
	"errors"
	"fmt"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是**压缩链的代码侧规则**（T086 / C-36）：引用集合由代码求并集（不取模型输出），
// 链资格逐段校验，有效头只认当前 `chain_epoch` 内唯一的合格叶。
// Chain rules live here: reference sets are computed by code, and the head is the unique
// qualified leaf of the current epoch — never merely the newest committed row.

// ErrChainBroken 表示链资格不通过：断链、祖先非 committed、跨 epoch 或分叉。
// ErrChainBroken reports a chain whose links do not qualify.
var ErrChainBroken = errors.New("compaction: 压缩链资格不通过（断链 / 祖先非 committed / 跨 epoch）")

// SourceRefsOf 计算一条新条目的**两个集合**（设计 §3.1.11 冻结签名）：
//   - 审计集 `audit` = 父链 `SourceRefs` ∪ 覆盖消息 `SourceRefs` 的**全部**引用
//     （含本次被剔除的），按 `(ID, VersionID)` 去重、稳定顺序；
//   - 当前依赖集 `included` = 父链 `IncludedRefs` ∪ 覆盖消息中**未被剔除**（`kept=true`）
//     的引用。
//
// `kept[i]` 与 `covered[i]` 一一对应；`kept` 长度不足时按"全部保留"处理（调用方漏传
// 不等于剔除，但也**不得**因此把消息判成未覆盖）。
// SourceRefsOf unions the audit set over every covered reference and the dependency set
// over the kept ones only; the two sets never merge.
func SourceRefsOf(covered []StoredMessage, parentAudit, parentIncluded []core.EvidenceRef, kept []bool) (audit, included []core.EvidenceRef) {
	all := make([][]core.EvidenceRef, 0, len(covered)+1)
	all = append(all, parentAudit)
	for _, msg := range covered {
		if len(msg.SourceRefs) > 0 {
			all = append(all, msg.SourceRefs)
		}
	}
	audit = mergeRefs(all...)

	dep := make([][]core.EvidenceRef, 0, len(covered)+1)
	dep = append(dep, parentIncluded)
	for i, msg := range covered {
		if len(msg.SourceRefs) == 0 {
			continue
		}
		if i < len(kept) && !kept[i] {
			continue
		}
		dep = append(dep, msg.SourceRefs)
	}
	included = mergeRefs(dep...)
	return audit, included
}

// mergeRefs 按**首次出现顺序**合并去重（同 ID + 同版本 + 同哈希视为同一条）。
// 顺序必须稳定：同一份输入的审计集与依赖集要可复现，**不得**依赖 map 遍历。
// mergeRefs unions in first-seen order so the result is reproducible.
func mergeRefs(sets ...[]core.EvidenceRef) []core.EvidenceRef {
	var out []core.EvidenceRef
	seen := make(map[core.EvidenceRef]struct{})
	for _, set := range sets {
		for _, ref := range set {
			if _, ok := seen[ref]; ok {
				continue
			}
			seen[ref] = struct{}{}
			out = append(out, ref)
		}
	}
	return out
}

// ValidateChain 校验一条按**父到子**排列的链是否合格（C-36 的链资格）：
// 根 `ParentCompactionID` 为空且 `ChainEpoch` 与链一致；逐段 `status=committed`、
// `child.CoveredFromMessageID == parent.FirstKeptMessageID`、
// `child.ParentCompactionID == parent.CompactionID`、child 的 epoch 与根相同。
// 空链是**合格的空链**（返回 nil）：调用方据此走"无有效头"分支，而不是把空当错误。
// ValidateChain checks the qualified-link rules over a parent-to-child chain; an empty
// chain is valid and means "no head".
func ValidateChain(chain []CompactionEntry) error {
	if len(chain) == 0 {
		return nil
	}
	root := chain[0]
	if root.ParentCompactionID != "" {
		return fmt.Errorf("%w: 根条目 %s 带父节点", ErrChainBroken, root.CompactionID)
	}
	for i, entry := range chain {
		if entry.Status != StatusCommitted {
			return fmt.Errorf("%w: 条目 %s 状态为 %s", ErrChainBroken, entry.CompactionID, entry.Status)
		}
		if entry.ChainEpoch != root.ChainEpoch {
			return fmt.Errorf("%w: 条目 %s 的 epoch %d 与链根 %d 不一致",
				ErrChainBroken, entry.CompactionID, entry.ChainEpoch, root.ChainEpoch)
		}
		if i == 0 {
			continue
		}
		prev := chain[i-1]
		if entry.ParentCompactionID != prev.CompactionID {
			return fmt.Errorf("%w: %s 的父节点不是 %s", ErrChainBroken, entry.CompactionID, prev.CompactionID)
		}
		if entry.CoveredFromMessageID != prev.FirstKeptMessageID {
			return fmt.Errorf("%w: %s 的覆盖起点不等于父节点 %s 的保留起点",
				ErrChainBroken, entry.CompactionID, prev.CompactionID)
		}
	}
	return nil
}

// ChainHead 返回链的**有效头**（末条）：链为空或资格不通过时返回 `ok=false`——
// "最新 committed 行"不等于有效头，本函数**不**跳过坏祖先取后代。
// ChainHead returns the tail entry only when the whole chain qualifies.
func ChainHead(chain []CompactionEntry) (CompactionEntry, bool) {
	if err := ValidateChain(chain); err != nil || len(chain) == 0 {
		return CompactionEntry{}, false
	}
	return chain[len(chain)-1], true
}

// FindParent 在链上找一条 `committed` 且**链资格通过**的条目，用作新条目的父节点。
// 链按父到子排列时，最后一个合格前缀的末条就是父节点；整条不合格则返回 `ok=false`。
// FindParent returns the last entry of the longest qualified prefix.
func FindParent(chain []CompactionEntry) (CompactionEntry, bool) {
	for i := len(chain); i > 0; i-- {
		if err := ValidateChain(chain[:i]); err == nil {
			return chain[i-1], true
		}
	}
	return CompactionEntry{}, false
}
