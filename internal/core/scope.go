package core

// 本文件声明公共资源的使用范围（颗粒度文档 R4.4 / T104）。范围只有两种字面量：
// `shared`（工作区共享）与 `exclusive`（某个 Agent 专属）。运行时资格是
// 「共享 ∪ 本人专属」；范围转换的证据输入也在这里收口，避免各层自造判定。
// This file declares resource scope: shared or exclusive, with runtime eligibility
// defined as shared ∪ the caller's own exclusive resources.

import "fmt"

// ResourceScope 是资源使用范围的两值字面量（schema.sql CHECK 逐字）。
// 名为 ResourceScope 而非 Scope：后者已是记忆分层（memory.go），同名不同物不得混用。
// ResourceScope is the two-value literal set of resource scope. The explicit name keeps
// it apart from the memory-layer Scope in memory.go.
type ResourceScope string

const (
	// ScopeShared 表示工作区共享，任何有资格的 Agent 都可以使用。
	ScopeShared ResourceScope = "shared"
	// ScopeExclusive 表示某个 Agent 专属，其他 Agent 不得使用。
	ScopeExclusive ResourceScope = "exclusive"
)

// ParseScope 逐字解析范围；其它字面量（含空串）一律拒绝，不做缺省推断。
// ParseScope accepts exactly the two literals; empty or unknown input is refused
// rather than defaulted.
func ParseScope(raw string) (ResourceScope, error) {
	switch ResourceScope(raw) {
	case ScopeShared:
		return ScopeShared, nil
	case ScopeExclusive:
		return ScopeExclusive, nil
	default:
		return "", fmt.Errorf("非法的资源范围 %q", raw)
	}
}

// Eligible 判断调用方 Agent 能否使用该资源：共享资源对所有调用方可用，
// 专属资源只对属主可用；属主为空或范围非法时 fail-closed 为不可用。
// Eligible reports whether the caller may use the resource: shared is open,
// exclusive is owner-only, and malformed input fails closed.
func Eligible(scope ResourceScope, owner, caller AgentUID) bool {
	switch scope {
	case ScopeShared:
		return true
	case ScopeExclusive:
		return owner != "" && caller != "" && owner == caller
	default:
		return false
	}
}

// ScopeUsage 是共享转专属前的占用证据（R4.4）：绑定该资源的其他 Agent、
// 定义文件整词引用、未释放 Run、消息路由引用者。任一项非空即不得收缩。
// ScopeUsage carries the evidence that blocks narrowing a shared resource:
// other bound agents, whole-word definition references, unreleased runs and
// message-route references.
type ScopeUsage struct {
	// BoundAgents 是除目标属主外仍绑定该资源的 Agent UID（活动与归档都算）。
	BoundAgents []AgentUID
	// DocumentRefs 是 `AGENT.md` 正文整词命中资源名的引用者（活动 Agent 名 /
	// 归档目录的 `archive:<id>`）：绑定投影不可信或归档身份缺失（如
	// `archive_legacy_unrecorded`）时，正文是唯一的证据面。
	// DocumentRefs names the documents whose body matches the resource name as a whole
	// word; it is the evidence that survives an untrusted or unrecorded projection.
	DocumentRefs []string
	// ActiveRuns 是 `run_resource_refs.released_at IS NULL` 的 Run 标识。
	ActiveRuns []string
	// MessageRoutes 是消息路由端口报告的路由标识（当前实现返回空）。
	MessageRoutes []string
}

// InUse 报告是否存在任何占用证据。
// InUse reports whether any blocker exists.
func (u ScopeUsage) InUse() bool {
	return len(u.BoundAgents) > 0 || len(u.DocumentRefs) > 0 ||
		len(u.ActiveRuns) > 0 || len(u.MessageRoutes) > 0
}

// Blockers 返回可对外点名的占用者列表（Agent UID / 引用文档 / Run ID / 路由标识）。
// Blockers returns the addressable blockers for the 409 detail.
func (u ScopeUsage) Blockers() []string {
	out := make([]string, 0, len(u.BoundAgents)+len(u.DocumentRefs)+
		len(u.ActiveRuns)+len(u.MessageRoutes))
	for _, uid := range u.BoundAgents {
		out = append(out, uid.String())
	}
	out = append(out, u.DocumentRefs...)
	out = append(out, u.ActiveRuns...)
	out = append(out, u.MessageRoutes...)
	return out
}
