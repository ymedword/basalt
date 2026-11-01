package compaction

import (
	"context"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是**注入前来源校验**的本地形状（设计 §3.1.11 / C-26）：压缩引擎只认这一个端口，
// 适配层把知识库的校验结论映射过来——本包**不得** import `knowledge`，也**不得**把
// `ValidityResult` 搬进 `core`（core 仍不依赖知识包）。
// The local validity shape and port; the adapter maps the knowledge-side verdict into it,
// so neither this package nor core takes a dependency on the knowledge package.

// 失效原因字面量（`Validity.Reason` 的取值，四值）。
// The four reason literals of Validity.Reason.
const (
	// ValidityRevoked 表示来源被撤权（当前 ACL 不再允许本次 Run 的调用方读取）。
	ValidityRevoked = "revoked"
	// ValidityDeleted 表示来源已删除。
	ValidityDeleted = "deleted"
	// ValidityVersionChanged 表示版本已变（引用指向的版本不再是当前版本）。
	ValidityVersionChanged = "version_changed"
	// ValidityNotFound 表示来源不存在。
	ValidityNotFound = "not_found"
)

// Validity 是一次校验的结论：`Valid` 决定能不能注入，`Reason` 只在不可注入时给分类。
// **`Valid=true` 时 `Reason` 必须为空**——"有效且有原因"是自相矛盾的读数。
// Validity is one check verdict; a valid verdict never carries a reason.
type Validity struct {
	Valid  bool
	Reason string
}

// Inject 报告这份来源**本次**能否进入摘要请求或投影。
// Inject reports whether the source may enter the summary request or the projection.
func (v Validity) Inject() bool { return v.Valid }

// ValidShape 判定结论自身是否自洽：有效时无原因，失效时原因必须在四值之内。
// 词表外的原因按**不可注入**处理（fail-closed），但调用方应据此报适配层错误。
// ValidShape rejects a verdict that contradicts itself; an unknown reason stays a deny.
func (v Validity) ValidShape() bool {
	if v.Valid {
		return v.Reason == ""
	}
	switch v.Reason {
	case ValidityRevoked, ValidityDeleted, ValidityVersionChanged, ValidityNotFound:
		return true
	}
	return false
}

// ValidityChecker 是**注入前来源校验**端口：每次读取都要逐条重算，**不得**缓存结论。
// 身份 `p` 必须是本次 Run 准入时冻结的那一个（显式传参，**不得**从 ctx 取）。
// 同一个实现必须同时服务三处：摘要调用前的过滤、注入前的全量校验、降级回原始历史的
// `core.SourceFilter`——三处口径不同就等于漏检。
// ValidityChecker is the pre-injection source check; one implementation serves the
// pre-summary filter, the injection check and the degraded fallback alike.
type ValidityChecker interface {
	Check(ctx context.Context, p core.Principal, ref core.EvidenceRef) (Validity, error)
}

// invalidMessagePlaceholder 是失效消息在摘要输入 / 投影里的**整条替换文本**
// （§3.1.5 `SourceFilter` 行冻结的字面量）：**换掉正文、保留位置与标识**，
// 不从结果里删除——删除会让工具组失配，也让"这里原本有内容"这一事实消失。
// The frozen placeholder that replaces a whole message whose source went stale.
const invalidMessagePlaceholder = "[内容不可用：来源已失效]"

// refsInjectable 逐条校验一组引用：任一失效即整条消息不可注入。
// 返回的 `reason` 取**第一条**失效引用的原因（稳定顺序，便于对账）。
// refsInjectable checks every reference; one failure makes the whole message unshareable.
func (c *Compactor) refsInjectable(ctx context.Context, p core.Principal, refs []core.EvidenceRef) (bool, string, error) {
	return checkRefs(ctx, c.deps.Validator, p, refs)
}

// checkRefs 是逐条校验的**唯一实现**：引擎的摘要过滤、注入判定与降级回退的
// `core.SourceFilter` 三处共用它——三处各写一份就等于漏检（C-26）。
// checkRefs is the single implementation shared by the pre-summary filter, the injection
// check and the degraded fallback; three copies would mean three different rules.
func checkRefs(ctx context.Context, checker ValidityChecker, p core.Principal, refs []core.EvidenceRef) (bool, string, error) {
	for _, ref := range refs {
		v, err := checker.Check(ctx, p, ref)
		if err != nil {
			return false, "", err
		}
		if !v.Inject() {
			return false, v.Reason, nil
		}
	}
	return true, "", nil
}
