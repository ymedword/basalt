package observability

import "context"

// FlowAssociation 仅来自原 Run、节点尝试与工具审计的真实关联，不含正文。
// FlowAssociation contains ledger-backed identities, never task or result bodies.
type FlowAssociation struct {
	TaskID        string            `json:"task_id"`
	RootTaskID    string            `json:"root_task_id"`
	NodeID        string            `json:"node_id"`
	NodeAttemptID string            `json:"node_attempt_id"`
	Revision      int64             `json:"revision"`
	Epoch         int64             `json:"execution_epoch"`
	ActionID      string            `json:"action_id,omitempty"`
	Actions       map[string]string `json:"-"`
}

// FlowFactsReader 是可选只读端口；查询不能推进、恢复或创建 Run。
// FlowFactsReader can only expand admitted runs and their original associations.
type FlowFactsReader interface {
	RunsByFlowTaskID(context.Context, string) ([]string, error)
	FlowAssociation(context.Context, string) (FlowAssociation, error)
}

func (p *Projector) associateFlow(ctx context.Context, runID string, entries []TimelineEntry) ([]TimelineEntry, error) {
	reader, ok := p.ports.Runs.(FlowFactsReader)
	if !ok {
		return entries, nil
	}
	link, err := reader.FlowAssociation(ctx, runID)
	if err != nil || link.TaskID == "" {
		return entries, err
	}
	for i := range entries {
		copy := link
		copy.Actions = nil
		copy.ActionID = link.Actions[entries[i].AuditRef]
		entries[i].Flow = &copy
	}
	return entries, nil
}
