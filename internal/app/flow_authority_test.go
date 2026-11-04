package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/tool"
)

type flowOwnerKeysProbe struct {
	scopes  []string
	failure error
	seen    []string
}

type flowLifecycleProbe struct {
	core.LifecyclePort
	record core.LifecycleRecord
	exists bool
}

type flowResourcesProbe struct{ resolved core.ResolvedResources }

func (p flowResourcesProbe) Resolve(context.Context, core.AgentEntry) (core.ResolvedResources, error) {
	return p.resolved, nil
}

func (p *flowLifecycleProbe) Read(context.Context, string) (core.LifecycleRecord, bool, error) {
	return p.record, p.exists, nil
}

func (p *flowOwnerKeysProbe) VerifyOwnerKey(_ context.Context, _ string, keyID, _ string) ([]string, error) {
	p.seen = append(p.seen, keyID)
	return p.scopes, p.failure
}

func flowAuthorityForTest(t *testing.T) (*flowAuthority, flow.DefinitionVersion, core.Principal, *flowOwnerKeysProbe) {
	t.Helper()
	profile := &core.Profile{Name: "writer", Tools: []string{"read_file", "write_file"}}
	snapshot := &core.ConfigSnapshot{Hash: "config", Profile: profile}
	resources := &cmpwResources{}
	resolved, err := resources.Resolve(t.Context(), core.AgentEntry{Name: "writer", Profile: profile})
	require.NoError(t, err)
	resolved.Runtime = nil // Metadata-only probe, without a typed-nil Runtime interface.
	manifest, err := core.BuildRunResourceManifest(core.AgentUID(cmpwAgentUID), core.AgentActive, resolved)
	require.NoError(t, err)
	registry := tool.NewRegistry(nil)
	require.NoError(t, registry.Register(tool.NewReadFileTool(nil, t.TempDir())))
	require.NoError(t, registry.Register(tool.NewWriteFileTool(nil, t.TempDir(), "")))
	scopes := []string{core.ScopeFlowsWrite, core.ScopeFlowsRead, core.AgentInvokeScopePrefix + "writer"}
	p, err := core.OwnedPrincipal("owner", core.AuthorizingKindCallerKey, "original-key", scopes, core.ActorRecovery)
	require.NoError(t, err)
	keys := &flowOwnerKeysProbe{scopes: scopes}
	authority := &flowAuthority{agents: &cmpwAgents{snapshot: snapshot}, lifecycle: &flowLifecycleProbe{record: core.LifecycleRecord{Agent: "writer", AgentUID: core.AgentUID(cmpwAgentUID), State: core.AgentActive, Generation: 1}, exists: true}, gate: core.NewAgentGate(), resources: flowResourcesProbe{resolved}, snapshots: &cmpwSnapshotBuilder{snapshot: snapshot}, keys: keys, tools: registry, schemas: tool.NewSchemaCompiler(), posture: func() core.AdminPosture { return core.AdminPostureToken }}
	definition := flow.DefinitionVersion{DefinitionUID: "fd_pinned", Version: 1, Definition: flow.Definition{Nodes: []flow.Node{{ID: "draft", Kind: "agent", Agent: &flow.AgentRef{UID: cmpwAgentUID, Name: "writer", Version: 1, ConfigHash: "config", ManifestHash: manifest.Hash}}}}}
	return authority, definition, p, keys
}

func TestFlow_AuthorityChecksEachBindingOncePerBoundaryWithoutCachingAcrossCalls(t *testing.T) {
	authority, definition, principal, keys := flowAuthorityForTest(t)
	prototype := definition.Definition.Nodes[0]
	definition.Definition.Nodes = nil
	for i := range 20 {
		node := prototype
		node.ID = fmt.Sprintf("node-%02d", i)
		definition.Definition.Nodes = append(definition.Definition.Nodes, node)
	}
	require.NoError(t, authority.CheckStart(t.Context(), principal, definition))
	require.Len(t, keys.seen, 1, "one boundary must not repeat an identical Agent/tool/key binding twenty times")
	require.NoError(t, authority.CheckStart(t.Context(), principal, definition))
	require.Len(t, keys.seen, 2, "the next boundary must recheck current authority")
	keys.failure = core.ErrUnauthorized
	require.ErrorIs(t, authority.CheckStart(t.Context(), principal, definition), core.ErrUnauthorized)
	require.Len(t, keys.seen, 3, "revocation is never hidden by a cross-call cache")
	keys.failure = nil
	ceiling, err := authority.Freeze(t.Context(), principal, definition)
	require.NoError(t, err)
	keys.seen = nil
	_, err = authority.Current(t.Context(), flow.Task{CallerID: principal.CallerID, AuthorizingKind: string(core.AuthorizingKindCallerKey), AuthorizingKeyID: principal.KeyID, Contract: flow.TaskContract{Scope: ceiling}, AcceptedVersion: definition})
	require.NoError(t, err)
	require.Len(t, keys.seen, 2, "Current plus CheckStart must each recheck the named Agent once, not twenty times")
}

func TestFlow_AuthorityKeepsOriginalKeyAndReadOnlyCeiling(t *testing.T) {
	authority, definition, p, keys := flowAuthorityForTest(t)
	ceiling, err := authority.Freeze(t.Context(), p, definition)
	require.NoError(t, err)
	require.Equal(t, []string{"read_file"}, ceiling.Tools)
	require.Equal(t, []string{cmpwAgentUID}, ceiling.AgentUIDs)
	task := flow.Task{CallerID: p.CallerID, AuthorizingKind: string(core.AuthorizingKindCallerKey), AuthorizingKeyID: p.KeyID, Contract: flow.TaskContract{Scope: ceiling}, AcceptedVersion: definition}
	current, err := authority.Current(t.Context(), task)
	require.NoError(t, err)
	require.Equal(t, p.KeyID, current.KeyID)
	require.Equal(t, p.CallerID, current.CallerID)
	keys.failure = errors.New("original key revoked")
	_, err = authority.Current(t.Context(), task)
	require.Error(t, err)
	for _, key := range keys.seen {
		require.Equal(t, "original-key", key, "must never rebind to another key")
	}
	keys.failure = nil
	keys.scopes = []string{core.AgentInvokeScopePrefix + "writer"}
	_, err = authority.Current(t.Context(), task)
	require.ErrorIs(t, err, core.ErrUnauthorized)
}

func TestFlow_PublicationWriteGateIsSeparateFromReadOnlyFixedTool(t *testing.T) {
	authority, definition, _, _ := flowAuthorityForTest(t)
	ref := *definition.Definition.Nodes[0].Agent
	require.NoError(t, authority.CheckAgent(t.Context(), ref, "read_file", true))
	require.Error(t, authority.CheckAgent(t.Context(), ref, "write_file", true))
	authority.writeReady = true // Isolated design seam, not a production behavior approval.
	require.NoError(t, authority.CheckAgent(t.Context(), ref, "write_file", true))
	require.Error(t, authority.CheckAgent(t.Context(), ref, "write_file", false))
}

func TestFlow_AuthorityRejectsRecreatedOrChangedAgent(t *testing.T) {
	for _, change := range []string{"uid", "generation", "config", "manifest", "disabled"} {
		t.Run(change, func(t *testing.T) {
			authority, definition, p, _ := flowAuthorityForTest(t)
			life := authority.lifecycle.(*flowLifecycleProbe)
			switch change {
			case "uid":
				life.record.AgentUID = "ag_1123456789ABCDEFGHJKMNPQRS"
			case "generation":
				life.record.Generation++
			case "config":
				authority.snapshots.(*cmpwSnapshotBuilder).snapshot.Hash = "new-config"
			case "manifest":
				definition.Definition.Nodes[0].Agent.ManifestHash = "different"
			case "disabled":
				life.record.State = core.AgentDisabled
			}
			require.Error(t, authority.CheckStart(t.Context(), p, definition))
		})
	}
	for _, posture := range []core.AdminPosture{"", core.AdminPostureUnavailable, core.AdminPostureToken, core.AdminPostureLocalTrust} {
		authority, definition, _, _ := flowAuthorityForTest(t)
		authority.posture = func() core.AdminPosture { return posture }
		err := authority.CheckStart(t.Context(), core.LocalAdminCLIPrincipal(), definition)
		if posture == core.AdminPostureToken || posture == core.AdminPostureLocalTrust {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, core.ErrUnauthorized)
		}
	}
}
