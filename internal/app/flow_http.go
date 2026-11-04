package app

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/web"
)

type flowHTTP struct {
	runtime *flowRuntime
	entries func() []core.AgentEntry
}

var _ web.FlowBackend = (*flowHTTP)(nil)

func (b *flowHTTP) Enabled() bool {
	return b != nil && b.runtime != nil && b.runtime.service.Config.Enabled
}

func (b *flowHTTP) Definitions(ctx context.Context, p core.Principal, admin bool, cursor string, limit int) (web.FlowDefinitionPage, error) {
	page := web.FlowDefinitionPage{Items: []web.FlowDefinitionView{}}
	if !p.Valid() || !p.HasScope(core.ScopeFlowsRead) || admin && p.Kind != core.PrincipalLocalAdmin {
		return page, core.ErrUnauthorized
	}
	items, next, err := b.runtime.store.ListDefinitions(ctx, admin, cursor, limit)
	if err != nil {
		return page, err
	}
	for _, item := range items {
		page.Items = append(page.Items, flowDefinitionView(item))
	}
	page.Next = next
	return page, nil
}

func (b *flowHTTP) Definition(ctx context.Context, p core.Principal, name string) (web.FlowDefinitionView, error) {
	if !p.Valid() || !p.HasScope(core.ScopeFlowsRead) {
		return web.FlowDefinitionView{}, core.ErrUnauthorized
	}
	v, err := b.runtime.store.FindDefinition(ctx, name)
	if err != nil {
		return web.FlowDefinitionView{}, err
	}
	if v.Retired {
		return web.FlowDefinitionView{}, flow.ErrDefinitionNotFound
	}
	return flowDefinitionView(v), nil
}

func (b *flowHTTP) Validate(ctx context.Context, p core.Principal, actor core.Actor, definition flow.Definition) error {
	if !p.Valid() || p.Kind != core.PrincipalLocalAdmin || actor.Subject == "" || actor.Source == "" || (actor.Method != core.AuthMethodBearer && actor.Method != core.AuthMethodLocalTrust && actor.Method != core.AuthMethodCLI) {
		return core.ErrUnauthorized
	}
	return b.runtime.publisher.Validator.ValidateDefinition(ctx, definition)
}

func (b *flowHTTP) Publish(ctx context.Context, req flow.PublishRequest) (web.FlowDefinitionView, error) {
	v, err := b.runtime.publisher.Publish(ctx, req)
	return flowDefinitionView(v), err
}

func (b *flowHTTP) Retire(ctx context.Context, req flow.RetireRequest) (web.FlowDefinitionView, error) {
	v, err := b.runtime.publisher.Retire(ctx, req)
	return flowDefinitionView(v), err
}

func (b *flowHTTP) Start(ctx context.Context, req flow.StartRequest) (web.FlowTaskView, bool, error) {
	task, err := b.runtime.service.Start(ctx, req)
	if err != nil {
		return web.FlowTaskView{}, false, err
	}
	view, err := b.taskView(ctx, req.Principal, task, "", 50, true)
	return view, task.NewlyAccepted, err
}

func (b *flowHTTP) Tasks(ctx context.Context, p core.Principal, cursor string, limit int, definitionName string) (web.FlowTaskPage, error) {
	page := web.FlowTaskPage{Items: []web.FlowTaskView{}}
	if !p.Valid() || !p.HasScope(core.ScopeFlowsRead) {
		return page, core.ErrUnauthorized
	}
	items, next, err := b.runtime.store.ListTasks(ctx, p, cursor, limit, false, definitionName)
	if err != nil {
		return page, err
	}
	for _, task := range items {
		view, err := b.taskView(ctx, p, task, "", 0, false)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, view)
	}
	page.Next = next
	return page, nil
}

func (b *flowHTTP) Task(ctx context.Context, p core.Principal, id, cursor string, limit int) (web.FlowTaskView, error) {
	if !p.Valid() || !p.HasScope(core.ScopeFlowsRead) {
		return web.FlowTaskView{}, core.ErrUnauthorized
	}
	task, err := b.runtime.store.Read(ctx, p, id)
	if err != nil {
		return web.FlowTaskView{}, err
	}
	return b.taskView(ctx, p, task, cursor, limit, true)
}

func flowAdminActor(actor core.Actor) bool {
	return actor.Subject != "" && actor.Source != "" && (actor.Method == core.AuthMethodBearer || actor.Method == core.AuthMethodLocalTrust || actor.Method == core.AuthMethodCLI)
}

func (b *flowHTTP) AdminTasks(ctx context.Context, actor core.Actor, cursor string, limit int, definitionName string) (web.FlowTaskPage, error) {
	page := web.FlowTaskPage{Items: []web.FlowTaskView{}}
	if !flowAdminActor(actor) {
		return page, core.ErrUnauthorized
	}
	tasks, next, err := b.runtime.store.ListTasks(ctx, core.LocalAdminPrincipal(), cursor, limit, true, definitionName)
	if err != nil {
		return page, err
	}
	for _, task := range tasks {
		view, err := b.adminTaskView(ctx, task, "", 0, false)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, view)
	}
	page.Next = next
	return page, nil
}

func (b *flowHTTP) AdminTask(ctx context.Context, actor core.Actor, id, cursor string, limit int) (web.FlowTaskView, error) {
	if !flowAdminActor(actor) {
		return web.FlowTaskView{}, core.ErrUnauthorized
	}
	task, err := b.runtime.store.ReadForWorker(ctx, id)
	if err != nil {
		return web.FlowTaskView{}, err
	}
	return b.adminTaskView(ctx, task, cursor, limit, true)
}

func (b *flowHTTP) adminTaskView(ctx context.Context, task flow.Task, cursor string, limit int, detail bool) (web.FlowTaskView, error) {
	// Administrative rescue sees execution metadata only, never an owner-key
	// impersonation or access to private goals, constraints, claims or artifacts.
	v := web.FlowTaskView{ID: task.ID, RootID: task.RootTaskID, ParentID: task.ParentTaskID, DefinitionUID: task.DefinitionUID, DefinitionName: task.AcceptedVersion.Definition.Name, DefinitionVersion: task.DefinitionVersion, Status: task.Status, Version: task.Version, ETag: task.Version.ETag(task.ID), CurrentNode: task.Revision.NodeID, CloseIntent: task.CloseIntent, CancelRequested: task.CancelRequested, Reason: task.Reason, Deadline: task.Deadline, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt, Slots: []web.FlowSlotView{}, Claims: []web.FlowClaimView{}, Constraints: []string{}, Attempts: []web.FlowAttemptView{}}
	var err error
	v.Unknown, err = b.runtime.store.HasUnknownActions(ctx, task.ID)
	if err == nil && detail {
		var attempts []flow.NodeAttempt
		attempts, v.NextAttempt, err = b.runtime.store.ListAdministrativeAttempts(ctx, core.LocalAdminPrincipal(), task.ID, cursor, limit)
		if err == nil {
			v.Attempts, err = b.attemptViews(ctx, core.LocalAdminPrincipal(), task.ID, attempts, true)
		}
	}
	return v, err
}

func (b *flowHTTP) Cancel(ctx context.Context, req flow.CancelRequest) (web.FlowTaskView, error) {
	task, err := b.runtime.service.Cancel(ctx, req)
	if err != nil {
		return web.FlowTaskView{}, err
	}
	return b.taskView(ctx, req.Principal, task, "", 50, true)
}

func (b *flowHTTP) Resume(ctx context.Context, req flow.ResumeRequest) (web.FlowTaskView, error) {
	task, err := b.runtime.service.Recover(ctx, req)
	if err != nil {
		return web.FlowTaskView{}, err
	}
	return b.taskView(ctx, req.Principal, task, "", 50, true)
}

func (b *flowHTTP) Abandon(ctx context.Context, req flow.AbandonRequest) (web.FlowTaskView, error) {
	task, err := b.runtime.service.Abandon(ctx, req)
	if err != nil {
		return web.FlowTaskView{}, err
	}
	// The rescue response is metadata only. Administrative authentication does
	// not turn into the task owner's source-reading or execution authority.
	unknown, err := b.runtime.store.HasUnknownActions(ctx, task.ID)
	if err != nil {
		return web.FlowTaskView{}, err
	}
	return web.FlowTaskView{ID: task.ID, RootID: task.RootTaskID, Status: task.Status, Version: task.Version, ETag: task.Version.ETag(task.ID), CloseIntent: task.CloseIntent, CancelRequested: task.CancelRequested, Unknown: unknown, Slots: []web.FlowSlotView{}, Claims: []web.FlowClaimView{}, Attempts: []web.FlowAttemptView{}}, nil
}

func flowDefinitionView(v flow.DefinitionVersion) web.FlowDefinitionView {
	out := web.FlowDefinitionView{UID: v.DefinitionUID, Version: v.Version, Name: v.Definition.Name, Hash: v.ContentHash, Retired: v.Retired, Goal: v.Definition.Contract.Goal, InputSchema: slices.Clone(v.Definition.Contract.InputSchema), OutputSchema: slices.Clone(v.Definition.Contract.OutputSchema), Nodes: []web.FlowNodeView{}}
	for _, node := range v.Definition.Nodes {
		view := web.FlowNodeView{ID: node.ID, Kind: node.Kind, Tool: node.Tool, Next: node.Next}
		if node.Agent != nil {
			view.Agent = node.Agent.Name
		}
		out.Nodes = append(out.Nodes, view)
	}
	return out
}

func (b *flowHTTP) taskView(ctx context.Context, p core.Principal, task flow.Task, cursor string, limit int, detail bool) (web.FlowTaskView, error) {
	v := web.FlowTaskView{ID: task.ID, RootID: task.RootTaskID, ParentID: task.ParentTaskID, DefinitionUID: task.DefinitionUID, DefinitionName: task.AcceptedVersion.Definition.Name, DefinitionVersion: task.DefinitionVersion, Status: task.Status, Version: task.Version, ETag: task.Version.ETag(task.ID), Goal: task.Contract.Goal, Constraints: append([]string{}, task.Contract.Constraints...), CurrentNode: task.Revision.NodeID, CloseIntent: task.CloseIntent, CancelRequested: task.CancelRequested, Reason: task.Reason, Deadline: task.Deadline, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt, Budget: &task.Contract.Budget, Slots: []web.FlowSlotView{}, Claims: []web.FlowClaimView{}, Attempts: []web.FlowAttemptView{}}
	left, err := b.runtime.store.Remaining(ctx, p, task.ID)
	if errors.Is(err, flow.ErrBudget) {
		v.BudgetExhausted = true
		zero := int64(0)
		v.RemainingTokens, v.RemainingTools = &zero, &zero
	} else if err != nil {
		return v, err
	} else {
		v.RemainingTokens, v.RemainingTools = &left.Tokens, &left.ToolCalls
	}
	v.Unknown, err = b.runtime.store.HasUnknownActions(ctx, task.ID)
	if err != nil {
		return v, err
	}
	for _, spec := range task.Contract.RequiredSlots {
		slot, found := task.Revision.Slots[spec.ID]
		if !found {
			slot = flow.Slot{ID: spec.ID, State: "missing"}
		}
		v.Slots = append(v.Slots, web.FlowSlotView{ID: slot.ID, State: slot.State, EvidenceIDs: append([]string{}, slot.EvidenceIDs...), VerificationIDs: append([]string{}, slot.VerificationIDs...)})
	}
	if !detail {
		return v, nil
	}
	attempts, next, err := b.runtime.store.ListAttempts(ctx, p, task.ID, cursor, limit)
	if err != nil {
		return v, err
	}
	v.NextAttempt = next
	v.Attempts, err = b.attemptViews(ctx, p, task.ID, attempts, false)
	if err != nil {
		return v, err
	}
	graph, err := b.runtime.store.ReadGraph(ctx, p, task.ID)
	if err != nil {
		return v, err
	}
	for _, claim := range graph.Claims {
		refs, err := flowClaimEvidence(graph, claim)
		if err != nil {
			return v, err
		}
		readable, err := b.contentReadable(ctx, p, task, refs)
		if err != nil {
			return v, err
		}
		item := web.FlowClaimView{ID: claim.ID, Revision: claim.Revision, State: claim.State, Readable: readable}
		if readable {
			item.Text = claim.Text
		}
		v.Claims = append(v.Claims, item)
	}
	return v, nil
}

func (b *flowHTTP) attemptViews(ctx context.Context, p core.Principal, id string, attempts []flow.NodeAttempt, admin bool) ([]web.FlowAttemptView, error) {
	ids := make([]string, 0, len(attempts))
	for _, a := range attempts {
		ids = append(ids, a.ID)
	}
	bound, err := b.runtime.store.AdmittedAttemptRuns(ctx, p, id, ids, admin)
	if err != nil {
		return nil, err
	}
	views := make([]web.FlowAttemptView, 0, len(attempts))
	for _, a := range attempts {
		views = append(views, web.FlowAttemptView{ID: a.ID, TaskID: a.TaskID, NodeID: a.NodeID, NodeInvocationID: a.NodeInvocationID, Attempt: a.Attempt, TaskRevision: a.TaskRevision, ExecutionEpoch: a.ExecutionEpoch, RunID: a.RunID, SessionID: a.SessionID, RunAdmitted: bound[a.ID], State: a.State, InputHash: a.InputHash, OutputHash: a.OutputHash, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt})
	}
	return views, nil
}

func (b *flowHTTP) contentReadable(ctx context.Context, p core.Principal, task flow.Task, refs []flow.EvidenceRef) (bool, error) {
	authority := b.runtime.service.Authority.(*flowAuthority)
	denied := false
	if err := authority.walk(ctx, task.AcceptedVersion, func(node flow.Node) error {
		if p.Kind == core.PrincipalCaller {
			if authority.keys == nil {
				denied = true
				return nil
			}
			scopes, err := authority.keys.VerifyOwnerKey(ctx, p.CallerID, p.KeyID, node.Agent.Name)
			if errors.Is(err, core.ErrUnauthorized) {
				denied = true
				return nil
			}
			if err != nil {
				return err
			}
			p.Scopes = slices.DeleteFunc(slices.Clone(p.Scopes), func(scope string) bool { return !slices.Contains(scopes, scope) })
			if !p.HasScope(core.ScopeFlowsRead) {
				denied = true
				return nil
			}
		}
		if !p.HasAgentScope(node.Agent.Name) {
			denied = true
			return nil
		}
		if err := authority.CheckAgent(ctx, *node.Agent, node.Tool, node.Kind == "tool"); err != nil {
			if errors.Is(err, core.ErrFlowDispatch) {
				denied = true
				return nil
			}
			return err
		}
		return nil
	}); err != nil {
		return false, err
	}
	if denied {
		return false, nil
	}
	err := checkFlowSources(ctx, p, task, refs, b.runtime.store, b.runtime.runner.sources)
	if errors.Is(err, core.ErrUnauthorized) || errors.Is(err, core.ErrForbidden) || errors.Is(err, flow.ErrStateConflict) {
		return false, nil
	}
	return err == nil, err
}

func flowClaimEvidence(graph flow.EvidenceGraph, claim flow.ClaimRevision) ([]flow.EvidenceRef, error) {
	if err := flow.ValidateGraph(graph); err != nil {
		return nil, err
	}
	seen := map[flow.ClaimRef]bool{}
	ids := map[string]bool{}
	todo := []flow.ClaimRef{{ID: claim.ID, Revision: claim.Revision}}
	for len(todo) > 0 {
		ref := todo[0]
		todo = todo[1:]
		if seen[ref] {
			continue
		}
		seen[ref] = true
		found := false
		for _, c := range graph.Claims {
			if c.ID == ref.ID && c.Revision == ref.Revision {
				found = true
				for _, id := range c.EvidenceIDs {
					ids[id] = true
				}
				todo = append(todo, c.Dependencies...)
				break
			}
		}
		if !found {
			return nil, flow.ErrStateConflict
		}
	}
	refs := []flow.EvidenceRef{}
	for _, e := range graph.Evidence {
		if ids[e.ID] {
			refs = append(refs, e)
		}
	}
	return refs, nil
}

func (b *flowHTTP) Artifacts(ctx context.Context, p core.Principal, id, cursor string, limit int) (web.FlowArtifactPage, error) {
	page := web.FlowArtifactPage{Items: []web.FlowArtifactView{}}
	if !p.Valid() || !p.HasScope(core.ScopeFlowsRead) {
		return page, core.ErrUnauthorized
	}
	task, err := b.runtime.store.Read(ctx, p, id)
	if err != nil {
		return page, err
	}
	graph, err := b.runtime.store.ReadGraph(ctx, p, id)
	if err != nil {
		return page, err
	}
	afterID, afterVersion, err := flowArtifactCursor(cursor)
	if err != nil {
		return page, err
	}
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return page, flow.ErrDefinitionInvalid
	}
	for _, a := range graph.Artifacts {
		if a.ID < afterID || a.ID == afterID && a.Version <= afterVersion {
			continue
		}
		if len(page.Items) == limit {
			last := page.Items[len(page.Items)-1]
			page.Next = base64.RawURLEncoding.EncodeToString([]byte(last.ID + "\n" + strconv.FormatInt(last.Version, 10)))
			break
		}
		// Direct artifact evidence comes from its exact original verification
		// attempt; claim dependencies add only their exact versioned sources.
		refs, witnessed, err := b.runtime.store.ArtifactSources(ctx, p, id, a.ID, a.Version)
		if err != nil {
			return page, err
		}
		for _, ref := range a.Claims {
			found := false
			for _, c := range graph.Claims {
				if c.ID == ref.ID && c.Revision == ref.Revision {
					found = true
					extra, err := flowClaimEvidence(graph, c)
					if err != nil {
						return page, err
					}
					refs = append(refs, extra...)
					break
				}
			}
			if !found {
				return page, flow.ErrStateConflict
			}
		}
		readable, err := b.contentReadable(ctx, p, task, refs)
		if err != nil {
			return page, err
		}
		item := web.FlowArtifactView{ID: a.ID, Version: a.Version, TaskRevision: a.TaskRevision, State: a.State, Format: a.Format, Hash: a.ContentHash, Readable: readable && witnessed}
		if item.Readable {
			item.Content = string(a.Content)
		} else if readable {
			item.Reason = "artifact_basis_unconfirmed"
		} else {
			item.Reason = "current_source_access_required"
		}
		page.Items = append(page.Items, item)
	}
	return page, nil
}

func flowArtifactCursor(cursor string) (string, int64, error) {
	if cursor == "" {
		return "", 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) > 256 {
		return "", 0, flow.ErrDefinitionInvalid
	}
	parts := strings.Split(string(raw), "\n")
	if len(parts) != 2 || parts[0] == "" {
		return "", 0, flow.ErrDefinitionInvalid
	}
	version, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || version < 1 {
		return "", 0, flow.ErrDefinitionInvalid
	}
	return parts[0], version, nil
}

func (b *flowHTTP) Verifications(ctx context.Context, p core.Principal, id, cursor string, limit int) (web.FlowVerificationPage, error) {
	if !p.Valid() || !p.HasScope(core.ScopeFlowsRead) {
		return web.FlowVerificationPage{}, core.ErrUnauthorized
	}
	items, next, err := b.runtime.store.ListVerifications(ctx, p, id, cursor, limit)
	return web.FlowVerificationPage{Items: items, Next: next}, err
}
