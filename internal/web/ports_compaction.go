package web

// 本文件是压缩只读视图端点的**消费方端口**（US-16 / T089；颗粒度文档 §3.1.9 的
// 「压缩视图」行；端点与字段以冻结契约 `docs/contracts/openapi.yaml` 的
// `GET /compactions` / `CompactionView` 为准）。四条口径：
//
//  1. **只读派生视图**：本端点只回**已经发生**的压缩事实，**不提供**手动触发；
//     压缩的触发权只在引擎（`internal/compaction`），管理台点不出压缩。
//  2. 直接复用 `internal/compaction` 的领域类型——`*postgres.CompactionStore` 以
//     具体方法 `ListCurrentChain` 结构化满足（R102·3：领域接口 `CompactionStore`
//     冻结为 4 方法，读全链是**读面专用**的第五个方法，不加进领域接口——加进去
//     会打断既有 fakeStore，也会让"引擎只需要四件事"的边界变糊）。
//  3. 排序 / 代次 / 截断一律由存储层固定：handler **不**重排、**不**过滤状态、
//     **不**替调用方收窄代次——读面看到的就是当前代次的全部事实（含 failed /
//     stale 与原因码）。
//  4. **装配缺失按失败关闭**：端口为 nil 时端点固定 500（`writeCompactionPortMissing`），
//     **不得**用空链冒充"从未压缩"——那会把装配故障显示成正常空状态。
//
// This file declares the consumer port of the read-only compaction surface. The domain
// interface stays frozen at four methods; the store satisfies this port structurally
// with a read-only fifth method, and a nil port fails closed with a fixed 500 rather
// than faking "never compacted".

import (
	"context"

	"github.com/ymedword/basalt/internal/compaction"
)

// CompactionChain 是压缩只读视图的消费方端口：按 scope 取**当前 `chain_epoch`** 的
// 条目（含 committed / failed / stale），按 `(chain_epoch, created_at)` 升序截到
// `limit` 条。`limit` 的下界由 handler 在触达端口之前判（契约 minimum 1）。
// CompactionChain reads the current generation of one scope in contract order,
// truncated to limit; the handler enforces the contract's lower bound first.
type CompactionChain interface {
	// ListCurrentChain 返回当前代次的一页条目。
	// ListCurrentChain returns one page of the current generation.
	ListCurrentChain(ctx context.Context, scope compaction.Scope, limit int) ([]compaction.CompactionEntry, error)
}
