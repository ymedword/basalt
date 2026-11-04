package app

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/web"
)

var _ web.FlowAuthoringBackend = (*flowHTTP)(nil)

// 作者读取复用原管理 actor；当前引用仅用于选择，发布／执行仍重新核对。
// Authoring uses the existing admin actor and never turns catalog visibility into authority.
func flowAuthoringActor(actor core.Actor) bool {
	return actor.Subject != "" && actor.Source != "" && (actor.Method == core.AuthMethodBearer || actor.Method == core.AuthMethodCLI || actor.Method == core.AuthMethodLocalTrust)
}

func (b *flowHTTP) AuthoringAgents(ctx context.Context, actor core.Actor, cursor string, limit int) (web.FlowAgentPage, error) {
	page := web.FlowAgentPage{Items: []web.FlowAgentView{}}
	if !flowAuthoringActor(actor) {
		return page, core.ErrUnauthorized
	}
	if limit < 1 || limit > 100 || len(cursor) > 128 {
		return page, flow.ErrDefinitionInvalid
	}
	if b == nil || b.runtime == nil || b.runtime.runner == nil || b.entries == nil {
		return page, core.ErrFlowDispatch
	}
	a, ok := b.runtime.runner.authority.(*flowAuthority)
	if !ok || a == nil {
		return page, core.ErrFlowDispatch
	}
	entries := slices.Clone(b.entries())
	slices.SortFunc(entries, func(x, y core.AgentEntry) int { return strings.Compare(x.Name, y.Name) })
	for _, entry := range entries {
		if entry.Name <= cursor {
			continue
		}
		if len(page.Items) == limit {
			page.Next = page.Items[len(page.Items)-1].Name
			break
		}
		if err := ctx.Err(); err != nil {
			return web.FlowAgentPage{}, err
		}
		view, err := a.authoringAgent(ctx, entry.Name)
		if err != nil {
			return web.FlowAgentPage{}, err
		}
		page.Items = append(page.Items, view)
	}
	return page, nil
}

func (b *flowHTTP) AuthoringDefinition(ctx context.Context, actor core.Actor, ref flow.DefinitionRef) (web.FlowAuthoringVersion, error) {
	if !flowAuthoringActor(actor) {
		return web.FlowAuthoringVersion{}, core.ErrUnauthorized
	}
	if b == nil || b.runtime == nil || b.runtime.store == nil {
		return web.FlowAuthoringVersion{}, core.ErrFlowDispatch
	}
	if ref.UID == "" || len(ref.UID) > 128 || ref.Version < 1 {
		return web.FlowAuthoringVersion{}, flow.ErrDefinitionInvalid
	}
	v, err := b.runtime.store.ReadDefinition(ctx, ref)
	if err != nil {
		return web.FlowAuthoringVersion{}, err
	}
	// RawMessage retains schema/constant numeric literals. A string envelope keeps
	// response.json() from converting those literals into JavaScript numbers.
	raw, err := json.Marshal(v.Definition)
	if err != nil {
		return web.FlowAuthoringVersion{}, err
	}
	return web.FlowAuthoringVersion{UID: v.DefinitionUID, Version: v.Version, Hash: v.ContentHash, Retired: v.Retired, PublishedAt: v.PublishedAt, BundleJSON: string(raw)}, nil
}

func (a *flowAuthority) authoringAgent(ctx context.Context, name string) (web.FlowAgentView, error) {
	v := web.FlowAgentView{Name: name, DisplayName: name, Reason: "resources_unavailable", MemoryScope: "legacy_default", KnowledgeCollections: []string{}, ReadonlyTools: []string{}}
	if a == nil || a.agents == nil || a.lifecycle == nil || a.gate == nil || a.resources == nil || a.snapshots == nil {
		return v, core.ErrFlowDispatch
	}
	unlock := a.gate.RLock(name)
	defer unlock()
	entry, ok := a.agents.Lookup(name)
	if !ok || entry.Profile == nil {
		v.Reason = "agent_unregistered"
		return v, nil
	}
	profile := entry.Profile
	if profile.DisplayName != "" {
		v.DisplayName = profile.DisplayName
	}
	v.Description = profile.Description
	if profile.Settings.IncludeGlobalMemory != nil {
		v.MemoryScope = "agent_only"
		if *profile.Settings.IncludeGlobalMemory {
			v.MemoryScope = "global_and_agent"
		}
	}
	for _, binding := range profile.Knowledge {
		v.KnowledgeCollections = append(v.KnowledgeCollections, binding.Name)
	}
	record, exists, err := a.lifecycle.Read(ctx, name)
	if err != nil {
		return v, err
	}
	if !exists || record.State != core.AgentActive {
		v.Reason = "agent_disabled"
		return v, nil
	}
	resolved, err := a.resources.Resolve(ctx, entry)
	if err != nil {
		if ctx.Err() != nil {
			return v, ctx.Err()
		}
		return v, nil
	}
	if resolved.Runtime != nil {
		defer resolved.Runtime.Release()
	}
	manifest, err := core.BuildRunResourceManifest(record.AgentUID, record.State, resolved)
	if err != nil {
		return v, nil
	}
	snapshot, err := a.snapshots.Build(ctx, entry, resolved)
	if err != nil {
		if ctx.Err() != nil {
			return v, ctx.Err()
		}
		return v, nil
	}
	for _, name := range profile.Tools {
		if a.tools == nil || a.schemas == nil {
			break
		}
		pinned, found := a.tools.Resolve(name)
		if !found {
			continue
		}
		spec, err := tool.FlowAdapterFor(ctx, pinned, a.schemas)
		if err == nil && spec.RecoveryClass == "readonly" {
			v.ReadonlyTools = append(v.ReadonlyTools, name)
		}
	}
	if err := ctx.Err(); err != nil {
		return v, err
	}
	v.Agent = &flow.AgentRef{UID: string(record.AgentUID), Version: record.Generation, Name: name, ConfigHash: snapshot.Hash, ManifestHash: manifest.Hash}
	v.Eligible, v.Reason = true, "ready"
	return v, nil
}
