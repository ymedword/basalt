package app

import (
	"context"
	"errors"
	"slices"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/tool"
)

// flowAuthority reuses the same registry/lifecycle/resource ports as Run. Agent
// Version pins the lifecycle generation; both configuration and resource content
// hashes are checked as well, so a reused name cannot silently rebind a task.
type flowAuthority struct {
	agents          core.AgentSource
	lifecycle       core.LifecyclePort
	gate            *core.AgentGate
	resources       core.RunResources
	snapshots       core.RunSnapshotBuilder
	definitions     flow.DefinitionRepository
	keys            core.OwnerKeyVerifier
	store           *postgres.FlowStore
	tools           core.ToolResolver
	schemas         *tool.SchemaCompiler
	collections     KnowledgeCollectionSource
	posture         func() core.AdminPosture
	writeReady      bool
	newTaskCapacity func(context.Context, core.Principal) error
}

// CheckNewTask is acceptance-only. Existing accepted tasks still wait/requeue
// after a configuration reduction; this check never obtains a Run ticket.
func (a *flowAuthority) CheckNewTask(ctx context.Context, p core.Principal) error {
	if a == nil || a.newTaskCapacity == nil {
		return core.ErrFlowDispatch
	}
	return a.newTaskCapacity(ctx, p)
}

var (
	_ flow.AuthorityResolver  = (*flowAuthority)(nil)
	_ flow.AuthorityFreezer   = (*flowAuthority)(nil)
	_ flow.DefinitionResolver = (*flowAuthority)(nil)
)

func (a *flowAuthority) CheckAgent(ctx context.Context, ref flow.AgentRef, fixedTool string, write bool) error {
	entry, err := a.inspect(ctx, ref)
	if err != nil {
		return err
	}
	if fixedTool != "" {
		if !slices.Contains(entry.Profile.Tools, fixedTool) || a.tools == nil {
			return core.ErrFlowDispatch
		}
		pinned, ok := a.tools.Resolve(fixedTool)
		if !ok {
			return core.ErrFlowDispatch
		}
		spec, err := tool.FlowAdapterFor(ctx, pinned, a.schemas)
		if err != nil {
			return err
		}
		if spec.RecoveryClass != "readonly" && (!write || !a.writeReady) {
			return core.ErrFlowDispatch
		}
	}
	return nil
}

func (a *flowAuthority) inspect(ctx context.Context, ref flow.AgentRef) (core.AgentEntry, error) {
	if a == nil || a.agents == nil || a.lifecycle == nil || a.resources == nil || a.snapshots == nil || a.gate == nil {
		return core.AgentEntry{}, core.ErrFlowDispatch
	}
	entry, ok := a.agents.Lookup(ref.Name)
	if !ok || entry.Profile == nil {
		return core.AgentEntry{}, core.ErrFlowDispatch
	}
	release := a.gate.RLock(ref.Name)
	record, exists, err := a.lifecycle.Read(ctx, ref.Name)
	release()
	if err != nil {
		return core.AgentEntry{}, err
	}
	if !exists || record.State != core.AgentActive || string(record.AgentUID) != ref.UID || record.Generation != ref.Version {
		return core.AgentEntry{}, core.ErrFlowDispatch
	}
	resolved, err := a.resources.Resolve(ctx, entry)
	if err != nil {
		return core.AgentEntry{}, err
	}
	if resolved.Runtime != nil {
		defer resolved.Runtime.Release()
	}
	manifest, err := core.BuildRunResourceManifest(record.AgentUID, record.State, resolved)
	if err != nil {
		return core.AgentEntry{}, err
	}
	if manifest.Hash != ref.ManifestHash {
		return core.AgentEntry{}, core.ErrFlowDispatch
	}
	snapshot, err := a.snapshots.Build(ctx, entry, resolved)
	if err != nil {
		return core.AgentEntry{}, err
	}
	if snapshot.Hash != ref.ConfigHash {
		return core.AgentEntry{}, core.ErrFlowDispatch
	}
	return entry, nil
}

func (a *flowAuthority) ReadDefinition(ctx context.Context, ref flow.DefinitionRef) (flow.DefinitionVersion, error) {
	if a == nil || a.definitions == nil {
		return flow.DefinitionVersion{}, flow.ErrDefinitionNotFound
	}
	return a.definitions.ReadDefinition(ctx, ref)
}

func (a *flowAuthority) walk(ctx context.Context, definition flow.DefinitionVersion, visit func(flow.Node) error) error {
	seen := make(map[flow.DefinitionRef]bool)
	count := 0
	var walk func(flow.DefinitionVersion, int) error
	walk = func(def flow.DefinitionVersion, depth int) error {
		if depth > 3 {
			return flow.ErrBudget
		}
		ref := flow.DefinitionRef{UID: def.DefinitionUID, Version: def.Version}
		if seen[ref] {
			return nil
		}
		seen[ref] = true
		for _, node := range def.Definition.Nodes {
			count++
			if count > 256 {
				return flow.ErrBudget
			}
			if node.Agent != nil {
				if err := visit(node); err != nil {
					return err
				}
			}
			if node.Subflow != nil {
				child, err := a.ReadDefinition(ctx, *node.Subflow)
				if err != nil {
					return err
				}
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(definition, 0)
}

func (a *flowAuthority) CheckStart(ctx context.Context, p core.Principal, def flow.DefinitionVersion) error {
	if !p.Valid() || !p.HasScope(core.ScopeFlowsWrite) {
		return core.ErrUnauthorized
	}
	if p.Kind == core.PrincipalLocalAdmin {
		if a.posture == nil {
			return core.ErrUnauthorized
		}
		posture := a.posture()
		if posture != core.AdminPostureToken && posture != core.AdminPostureLocalTrust {
			return core.ErrUnauthorized
		}
	}
	// 同一边界内相同版本／工具绑定只核对一次；下个边界重新建表，绝不缓存撤权。
	// Deduplicate identical immutable bindings within this call, never across boundaries.
	checked := map[struct {
		agent flow.AgentRef
		tool  string
		write bool
	}]bool{}
	return a.walk(ctx, def, func(node flow.Node) error {
		if !p.HasAgentScope(node.Agent.Name) {
			return core.ErrUnauthorized
		}
		binding := struct {
			agent flow.AgentRef
			tool  string
			write bool
		}{*node.Agent, node.Tool, node.Kind == "tool"}
		if checked[binding] {
			return ctx.Err()
		}
		if p.Kind == core.PrincipalCaller {
			if a.keys == nil {
				return core.ErrUnauthorized
			}
			scopes, err := a.keys.VerifyOwnerKey(ctx, p.CallerID, p.KeyID, node.Agent.Name)
			if err != nil {
				return err
			}
			if !slices.Contains(scopes, core.ScopeFlowsWrite) {
				return core.ErrUnauthorized
			}
		}
		if err := a.CheckAgent(ctx, *node.Agent, node.Tool, node.Kind == "tool"); err != nil {
			return err
		}
		checked[binding] = true
		return nil
	})
}

func (a *flowAuthority) Current(ctx context.Context, task flow.Task) (core.Principal, error) {
	if a == nil {
		return core.Principal{}, core.ErrUnauthorized
	}
	scopes := slices.Clone(task.Contract.Scope.Scopes)
	if task.AuthorizingKind == string(core.AuthorizingKindAdminBuiltin) {
		scopes = nil
	} else {
		if a.keys == nil {
			return core.Principal{}, core.ErrUnauthorized
		}
		checkedAgents := map[string]bool{}
		err := a.walk(ctx, task.AcceptedVersion, func(node flow.Node) error {
			if checkedAgents[node.Agent.Name] {
				return ctx.Err()
			}
			current, err := a.keys.VerifyOwnerKey(ctx, task.CallerID, task.AuthorizingKeyID, node.Agent.Name)
			if err != nil {
				return err
			}
			scopes = slices.DeleteFunc(scopes, func(s string) bool { return !slices.Contains(current, s) })
			checkedAgents[node.Agent.Name] = true
			return nil
		})
		if err != nil {
			return core.Principal{}, err
		}
	}
	p, err := core.OwnedPrincipal(task.CallerID, core.AuthorizingKind(task.AuthorizingKind), task.AuthorizingKeyID, scopes, core.ActorRecovery)
	if err != nil {
		return p, err
	}
	if err := a.CheckStart(ctx, p, task.AcceptedVersion); err != nil {
		return core.Principal{}, err
	}
	return p, nil
}

func (a *flowAuthority) Freeze(ctx context.Context, p core.Principal, definition flow.DefinitionVersion) (core.FlowAuthority, error) {
	if err := a.CheckStart(ctx, p, definition); err != nil {
		return core.FlowAuthority{}, err
	}
	kind := core.AuthorizingKindCallerKey
	scopes := slices.Clone(p.Scopes)
	if p.Kind == core.PrincipalLocalAdmin {
		kind = core.AuthorizingKindAdminBuiltin
		scopes = []string{core.ScopeFlowsRead, core.ScopeFlowsWrite, core.ScopeKnowledgeRead}
	}
	ceiling := core.FlowAuthority{CallerID: p.CallerID, KeyID: p.KeyID, AuthorizingKind: kind, Scopes: scopes}
	err := a.walk(ctx, definition, func(node flow.Node) error {
		entry, err := a.inspect(ctx, *node.Agent)
		if err != nil {
			return err
		}
		ceiling.AgentUIDs = append(ceiling.AgentUIDs, node.Agent.UID)
		for _, name := range entry.Profile.Tools {
			if a.tools == nil {
				continue
			}
			pinned, ok := a.tools.Resolve(name)
			if !ok {
				continue
			}
			spec, err := tool.FlowAdapterFor(ctx, pinned, a.schemas)
			if err != nil {
				continue
			}
			if node.Kind == "tool" && name == node.Tool || node.Kind == "agent" && spec.RecoveryClass == "readonly" {
				ceiling.Tools = append(ceiling.Tools, name)
			}
		}
		if p.HasScope(core.ScopeKnowledgeRead) && len(entry.Profile.Knowledge) > 0 {
			if a.collections == nil {
				return core.ErrFlowDispatch
			}
			var names []string
			for _, binding := range entry.Profile.Knowledge {
				names = append(names, binding.Name)
			}
			ids, missing, err := a.collections.CollectionIDsByName(ctx, names)
			if err != nil {
				return err
			}
			if len(missing) > 0 {
				return core.ErrFlowDispatch
			}
			for _, name := range names {
				ceiling.CollectionIDs = append(ceiling.CollectionIDs, ids[name])
			}
		}
		return nil
	})
	if err != nil {
		return core.FlowAuthority{}, err
	}
	for _, values := range []*[]string{&ceiling.AgentUIDs, &ceiling.Tools, &ceiling.CollectionIDs} {
		slices.Sort(*values)
		*values = slices.Compact(*values)
	}
	return ceiling, nil
}

func (a *flowAuthority) NoAvailableCallerKey(ctx context.Context, callerID string) (bool, error) {
	if a == nil || a.store == nil {
		return false, errors.New("flow closing-key reader unavailable")
	}
	return a.store.NoAvailableClosingKey(ctx, callerID)
}
