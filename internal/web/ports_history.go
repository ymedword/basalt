package web

// 本文件是历史回查三条端点的**消费方端口**（US-16 / T056；颗粒度文档 §3.1.10 的
// 「历史端点」行；端点清单以冻结契约 `docs/contracts/openapi.yaml` 的 history 组与
// `GET /sessions` 为准）。四条口径：
//
//  1. 检索与精确读取直接复用 `internal/history` 的领域类型——它们是冻结设计卡
//     §3.1.10 的导出面，`*history.Searcher` / `*history.ExactReader` 结构化满足，
//     web 只是转接，**不**另造同义类型（与 T042 的 `ports_knowledge.go` 同一先例）。
//  2. 引用有效性判定复用 `knowledge.Validity`：`*knowledge.SourceViewer` 结构化满足。
//     前置核验发生在**注入之前**，此处只做**读面标注**——同一条引用今天有效不代表
//     明天有效，每次精确读取都按当次身份重查（C-26 / C-10）。
//  3. 归属过滤是领域层的强制项：web **只**把 Principal 原样传下去，**不**自行拼
//     caller 过滤、**不**在响应层再筛一遍——两处各筛一次必然漂移，而"少筛一处"
//     就是越权。
//  4. **装配缺失按失败关闭**：端口为 nil 时对应端点固定 500（`writeHistoryPortMissing`），
//     **不得**用空命中 / 空引用冒充合法空状态，那会把装配故障显示成"查过了没有"。
//
// This file declares the consumer ports of the history surface. Search and exact read
// reuse the domain types of internal/history (structural satisfaction, no adapters);
// validity reuses knowledge.Validity so the annotation re-checks the current state on
// every read. Ownership filtering is the domain's job and a nil port fails closed with
// a fixed 500 rather than faking an empty result.

import (
	"context"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/history"
	"github.com/ymedword/basalt/internal/knowledge"
)

// HistorySearch 是历史检索的消费方端口：`Search` 自己结算归属、校验查询形状并标注
// 预算截断。web 层**不**重算 scope、**不**重排命中，也不把 `truncated` 丢掉——
// 静默裁剪会让调用方以为已穷尽。
// HistorySearch resolves the scope, validates the query and marks truncation itself;
// the HTTP layer never re-derives scope, re-sorts or drops the truncation mark.
type HistorySearch interface {
	// Search 返回一页命中指针与截断标注。
	// Search returns one page of hit pointers plus the truncation mark.
	Search(ctx context.Context, p core.Principal, q history.HistoryQuery) (history.SearchResult, error)
}

// HistoryExactRead 是精确读取的消费方端口：按消息主键取原文，跨调用方一律未找到
// （C-28）；归属、标识分类与会话复核都在领域层完成。
// HistoryExactRead loads one message by primary key; cross-caller reads are not-found.
type HistoryExactRead interface {
	// Read 返回一条消息的原文视图。
	// Read returns the full view of one message.
	Read(ctx context.Context, p core.Principal, ref history.MessageRef) (history.MessageView, error)
}

// EvidenceValidity 是**消息引用**的有效性判定端口（`knowledge.Validity` 的消费方
// 声明）：每次调用按当次身份重查 ACL / 删除 / 版本，返回冻结四值结论。
//
// 本端口只做读面标注，**不**决定消息本身能否读——消息是 Run 的记录，撤权只让
// 对应引用标为失效（设计 §3.1.10）。判定失败（存储违约）才让整条读取失败关闭；
// 「无权看知识库」是判定结论 `revoked`，不是错误。
// EvidenceValidity annotates message references; a denial is a verdict (revoked), not
// an error, and only a store contract violation fails the whole read closed.
type EvidenceValidity interface {
	// Check 判定引用点名的版本现在是否仍可按原样使用。
	// Check reports whether the referenced version is still usable as-is.
	Check(ctx context.Context, p core.Principal, ref knowledge.EvidenceRef) (knowledge.ValidityResult, error)
}
