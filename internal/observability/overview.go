package observability

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/diagnostics"
)

// OverviewQuery 强制有限时间窗和Top-N，不提供全库列表。
// OverviewQuery requires a bounded window and top-N limit.
type OverviewQuery struct {
	From, To time.Time
	Agent    string
	Limit    int
}

// ReasonCount 是固定原因分类的聚合，不携带错误原文。
// ReasonCount aggregates safe classifications without raw errors.
type ReasonCount struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// TaskGroup 聚合任务组并保留有界Run列表，不选代表Run。
// TaskGroup keeps a bounded drill-down list rather than selecting a representative run.
type TaskGroup struct {
	Agent       string        `json:"agent"`
	WindowStart time.Time     `json:"window_start"`
	WindowEnd   time.Time     `json:"window_end"`
	Failed      int           `json:"failed"`
	Total       int           `json:"total"`
	TopReasons  []ReasonCount `json:"top_reasons"`
	RunIDs      []string      `json:"run_ids"`
}

// Overview 的失败率为空表示没有样本，不等于0%。
// Overview uses a null ratio when no samples exist.
type Overview struct {
	other           *TaskGroup
	Runs            int         `json:"runs"`
	Failed          int         `json:"failed"`
	FailedRatio     *float64    `json:"failed_ratio"`
	Timeouts        int         `json:"timeouts"`
	ToolDenials     int         `json:"tool_denials"`
	MissingAssembly int         `json:"missing_assembly"`
	Groups          []TaskGroup `json:"groups"`
	truncated       bool
	merged          int
}

// MarshalJSON 明示截断与合并的组数，避免把有界前缀冒充总数。
// MarshalJSON discloses bounded results and merged groups.
func (o Overview) MarshalJSON() ([]byte, error) {
	type plain Overview
	return json.Marshal(struct {
		plain
		Truncated    bool       `json:"truncated"`
		MergedGroups int        `json:"merged_groups"`
		OtherGroup   *TaskGroup `json:"other_group"`
	}{plain(o), o.truncated, o.merged, o.other})
}

// Overview 从同一个事实端口汇总，辅助来源读取失败不伪装成零。
// Overview aggregates the same fact ports and never masks failed reads as zero.
func (p *Projector) Overview(ctx context.Context, q OverviewQuery) (Overview, error) {
	if q.From.IsZero() || q.To.IsZero() || q.From.After(q.To) || q.To.Sub(q.From) > MaxReadWindow || q.Limit < 0 || q.Limit > 100 {
		return Overview{}, ErrInvalidQuery
	}
	if p.ports.Runs == nil || p.ports.Exec == nil || p.ports.Assembly == nil || p.ports.Diagnostics == nil {
		return Overview{}, errors.New("observability overview sources unavailable")
	}
	runs, err := p.ports.Runs.RunsByAgentWindow(ctx, q.Agent, q.From, q.To)
	if err != nil {
		return Overview{}, err
	}
	runs = slices.Clone(runs)
	slices.SortStableFunc(runs, func(a, b RunFacts) int {
		if order := b.StartedAt.Compare(a.StartedAt); order != 0 {
			return order
		}
		return strings.Compare(a.RunID, b.RunID)
	})
	out := Overview{Groups: []TaskGroup{}}
	if len(runs) > p.cfg.MaxTimelineEntries {
		runs = runs[:p.cfg.MaxTimelineEntries]
		out.truncated = true
	}
	groups := make(map[string]*TaskGroup)
	reasons := make(map[string]map[string]int)
	anomalies := make(map[string]bool)
	for _, run := range runs {
		out.Runs++
		g := groups[run.Agent]
		if g == nil {
			g = &TaskGroup{Agent: run.Agent, WindowStart: q.From, WindowEnd: q.To, TopReasons: []ReasonCount{}, RunIDs: []string{}}
			groups[run.Agent] = g
			reasons[run.Agent] = map[string]int{}
		}
		g.Total++
		bad := run.Status == "failed"
		if bad {
			out.Failed++
			g.Failed++
			reasons[run.Agent][safeContextReason(run.Reason)]++
		}
		calls, err := p.ports.Exec.LLMCallsByRun(ctx, run.RunID)
		if err != nil {
			return Overview{}, err
		}
		for _, call := range calls {
			if call.ErrorKind == "timeout" {
				out.Timeouts++
				bad = true
			}
		}
		assembly, err := p.ports.Assembly.AssemblyByRun(ctx, run.RunID)
		if err != nil {
			return Overview{}, err
		}
		if len(assembly) == 0 {
			out.MissingAssembly++
			bad = true
		}
		events, err := p.ports.Diagnostics.ListEvents(ctx, diagnostics.Query{RunID: run.RunID, Kind: diagnostics.KindPermissionDecision, Limit: diagnostics.MaxQueryLimit})
		if err != nil {
			return Overview{}, err
		}
		if len(events) == diagnostics.MaxQueryLimit {
			out.truncated = true
		}
		for _, event := range events {
			if event.Kind == diagnostics.KindPermissionDecision && event.ReasonCode == diagnostics.ReasonPermissionDenied {
				out.ToolDenials++
				bad = true
			}
		}
		if bad {
			anomalies[run.Agent] = true
			if len(g.RunIDs) < 20 {
				g.RunIDs = append(g.RunIDs, run.RunID)
			}
		}
	}
	if out.Runs > 0 {
		ratio := float64(out.Failed) / float64(out.Runs)
		out.FailedRatio = &ratio
	}
	for agent, g := range groups {
		if !anomalies[agent] {
			continue
		}
		for reason, count := range reasons[agent] {
			g.TopReasons = append(g.TopReasons, ReasonCount{Reason: reason, Count: count})
		}
		slices.SortFunc(g.TopReasons, func(a, b ReasonCount) int {
			if a.Count != b.Count {
				return b.Count - a.Count
			}
			if a.Reason < b.Reason {
				return -1
			}
			if a.Reason > b.Reason {
				return 1
			}
			return 0
		})
		if len(g.TopReasons) > 5 {
			g.TopReasons = g.TopReasons[:5]
		}
		out.Groups = append(out.Groups, *g)
	}
	slices.SortFunc(out.Groups, func(a, b TaskGroup) int {
		if a.Failed != b.Failed {
			return b.Failed - a.Failed
		}
		if a.Agent < b.Agent {
			return -1
		}
		if a.Agent > b.Agent {
			return 1
		}
		return 0
	})
	limit := q.Limit
	if limit == 0 {
		limit = 20
	}
	if len(out.Groups) > limit {
		out.merged = len(out.Groups) - limit
		other := &TaskGroup{Agent: "其他", WindowStart: q.From, WindowEnd: q.To, TopReasons: []ReasonCount{}, RunIDs: []string{}}
		counts := map[string]int{}
		started := map[string]time.Time{}
		for _, run := range runs {
			started[run.RunID] = run.StartedAt
		}
		for _, g := range out.Groups[limit:] {
			other.Total += g.Total
			other.Failed += g.Failed
			for reason, n := range reasons[g.Agent] {
				counts[reason] += n
			}
			other.RunIDs = append(other.RunIDs, g.RunIDs...)
		}
		slices.SortFunc(other.RunIDs, func(a, b string) int {
			if order := started[b].Compare(started[a]); order != 0 {
				return order
			}
			return strings.Compare(a, b)
		})
		if len(other.RunIDs) > 20 {
			other.RunIDs = other.RunIDs[:20]
		}
		for reason, n := range counts {
			other.TopReasons = append(other.TopReasons, ReasonCount{Reason: reason, Count: n})
		}
		slices.SortFunc(other.TopReasons, func(a, b ReasonCount) int {
			if a.Count != b.Count {
				return b.Count - a.Count
			}
			if a.Reason < b.Reason {
				return -1
			}
			if a.Reason > b.Reason {
				return 1
			}
			return 0
		})
		if len(other.TopReasons) > 5 {
			other.TopReasons = other.TopReasons[:5]
		}
		out.other = other
		out.Groups = out.Groups[:limit]
	}
	return out, nil
}
