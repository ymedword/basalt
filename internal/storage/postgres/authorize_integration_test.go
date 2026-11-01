//go:build integration

package postgres

// 本文件是 `knowledge.Authz` 的 PostgreSQL 实现（`authorize.go`，US-16 C-08 / T129）的
// 真库判据。四条端口纪律各有一组具名用例：
//
//  1. **拒绝不是空结果**：全拒必须是 `core.ErrForbidden`；回一个空 `Scope` 会把越权
//     伪装成"库里没有"，因此这里既断错误类型，也断返回的 `Scope` 是零值。
//  2. **被拒元素只回计数**：错误文案里**不得**出现集合标识——区分"曾经存在"与"不存在"
//     本身就是越权信息。
//  3. **项目取自 `callers` 行**：跨项目集合与不存在的集合走同一条拒绝路径。
//  4. **身份与 scope 先于任何库查询**：用例故意在**没有任何集合**的库上请求集合，
//     若顺序被颠倒，得到的会是 `ErrForbidden` 而不是 `ErrUnauthorized`。
//
// 逆改点（任一即红）：把"全拒"改回 `return Scope{...}, nil` → 第一个用例的
// `require.ErrorIs` 红；在拒绝文案里回显集合名 → `require.NotContains` 红；把
// `callerProject` 挪到集合解析之后 → 停止 / 停用调用方用例拿到 `ErrForbidden` 而非
// `ErrUnauthorized`；让 `visibleCollections` 不筛 `deleted_at` → 软删集合可见。
//
// These are the database-backed cases for the retrieval authz resolver; each rule in the
// port has a named negative case.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// authzCallerPrincipal 造一个持有给定 scope 的外部调用方身份（不是内置管理员）。
// authzCallerPrincipal builds an external caller identity with the given scopes.
func authzCallerPrincipal(callerID string, scopes ...string) core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: callerID,
		Actor:    core.ActorCaller,
		Scopes:   scopes,
	}
}

// authzSeedCaller 种一行 `service` 调用方；项目与状态由用例指定。
// authzSeedCaller inserts one service caller row with the given project and status.
func authzSeedCaller(t *testing.T, db *DB, callerID, project, status string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'service', $1, $2, $3, $4)`,
		callerID, project, status, knNow.UnixMilli())
	require.NoError(t, err)
}

// authzRegisteredAgents 是 `KnowledgeAgentSource` 的只读替身：按名字回报已注册条目。
// 只放已注册项——未注册的 Agent 在检索面上等同不存在。
// authzRegisteredAgents is a read-only stand-in for the registry: registered entries only.
type authzRegisteredAgents map[string]core.AgentEntry

// Lookup 实现 postgres.KnowledgeAgentSource。
func (f authzRegisteredAgents) Lookup(name string) (core.AgentEntry, bool) {
	entry, ok := f[name]
	return entry, ok
}

// authzRegisteredAgent 造一个已注册、带给定集合声明的 Agent 条目。
// authzRegisteredAgent builds a registered entry carrying the given declarations.
func authzRegisteredAgent(name string, knowledge ...core.KnowledgeBinding) core.AgentEntry {
	return core.AgentEntry{
		Name:       name,
		Registered: true,
		Profile:    &core.Profile{Knowledge: knowledge},
	}
}

// authzSeedKnowledgeProjection 种一行知识集合投影（声明名 → 稳定集合 ID）。
// authzSeedKnowledgeProjection inserts one projected knowledge binding row.
func authzSeedKnowledgeProjection(t *testing.T, db *DB, agentName, agentUID, name, collectionID string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO agent_resource_bindings
			(agent_uid, agent_name, resource_kind, ref_name, resource_uid, content_hash, projected_at)
		VALUES ($1, $2, 'knowledge_collection', $3, $4, 'hash', $5)`,
		agentUID, agentName, name, collectionID, knNow.UnixMilli())
	require.NoError(t, err)
}

// authzSeedProjectCollection 种一个归属非空项目的集合。
// authzSeedProjectCollection inserts one collection owned by a named project.
func authzSeedProjectCollection(t *testing.T, db *DB, collectionID, project string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO kb_collections (collection_id, name, project, created_at)
		VALUES ($1, $1, $2, $3)`, collectionID, project, knNow.UnixMilli())
	require.NoError(t, err)
}

// TestAuthz_RefusesWhenNoCollectionVisible 是全拒路径：请求的集合要么不存在、要么已
// 软删，一个都不可见时**必须**报越权，而且**不得**把被拒的标识回显出来。
// TestAuthz_RefusesWhenNoCollectionVisible covers the fully denied path.
func TestAuthz_RefusesWhenNoCollectionVisible(t *testing.T) {
	ctx := context.Background()
	db := knTestDB(t)
	knSeedCollection(t, db, "col_soft_deleted", "已经删掉的集合")
	_, err := db.pool.ExecContext(ctx, `
		UPDATE kb_collections SET deleted_at = $2 WHERE collection_id = $1`,
		"col_soft_deleted", knNow.UnixMilli())
	require.NoError(t, err)

	scope, err := NewKnowledgeAuthz(db, nil).Resolve(ctx, core.LocalAdminPrincipal(), "",
		[]string{"col_soft_deleted", "col_never_existed"})
	require.ErrorIs(t, err, core.ErrForbidden, "全拒必须报越权，而不是回一个空 Scope")
	require.Empty(t, scope.Collections, "拒绝时不得返回任何可见集合")
	require.Zero(t, scope.DeniedCount, "拒绝结果不带计数——计数只在部分可见时有意义")
	require.NotContains(t, err.Error(), "col_soft_deleted", "被拒集合的标识不得出现在错误里")
	require.NotContains(t, err.Error(), "col_never_existed", "被拒集合的标识不得出现在错误里")
}

// TestAuthz_NarrowsToVisibleSubsetAndCountsDenied 是部分可见路径：去空白、去重后按
// 请求集求交，被拒的只以计数呈现。
func TestAuthz_NarrowsToVisibleSubsetAndCountsDenied(t *testing.T) {
	ctx := context.Background()
	db := knTestDB(t)
	knSeedCollection(t, db, "col_a", "a")
	knSeedCollection(t, db, "col_b", "b")

	scope, err := NewKnowledgeAuthz(db, nil).Resolve(ctx, core.LocalAdminPrincipal(), "",
		[]string{" col_b ", "col_a", "col_b", "", "col_missing"})
	require.NoError(t, err)
	require.Equal(t, []string{"col_a", "col_b"}, scope.Collections,
		"可见集合按标识排序；空白与重复条目不得重复计入")
	require.Equal(t, 1, scope.DeniedCount, "被拒计数 = 去重后的请求数 − 可见数")
}

// TestAuthz_RefusesDisabledOrUnknownCaller 是身份路径，且**顺带证明判定顺序**：库里
// 一个集合都没有，若先解析集合再查调用方，得到的会是 `ErrForbidden`。拿到
// `ErrUnauthorized` 才说明调用方判定发生在集合解析之前（停用调用方不该知道集合存在）。
func TestAuthz_RefusesDisabledOrUnknownCaller(t *testing.T) {
	ctx := context.Background()
	db := knTestDB(t)
	authzSeedCaller(t, db, "caller_paused", "", "disabled")
	authz := NewKnowledgeAuthz(db, nil)

	_, err := authz.Resolve(ctx, authzCallerPrincipal("caller_paused", core.ScopeKnowledgeRead),
		"", []string{"col_it"})
	require.ErrorIs(t, err, core.ErrUnauthorized, "停用的调用方按未认证拒绝")

	_, err = authz.Resolve(ctx, authzCallerPrincipal("caller_ghost", core.ScopeKnowledgeRead),
		"", []string{"col_it"})
	require.ErrorIs(t, err, core.ErrUnauthorized, "不存在的调用方按未认证拒绝")
}

// TestAuthz_RefusesCallerWithoutReadScope：有身份但 scope 不足是**越权**（403），不是
// 未认证（401）——两者在审计里的含义不同。
func TestAuthz_RefusesCallerWithoutReadScope(t *testing.T) {
	ctx := context.Background()
	db := knTestDB(t)
	knSeedCollection(t, db, "col_it", "it-runbooks")
	authzSeedCaller(t, db, "caller_writer", "", "active")

	_, err := NewKnowledgeAuthz(db, nil).Resolve(ctx,
		authzCallerPrincipal("caller_writer", core.ScopeKnowledgeWrite), "", []string{"col_it"})
	require.ErrorIs(t, err, core.ErrForbidden, "缺 knowledge:read 是越权，不是未认证")
}

// TestAuthz_RefusesAgentOnlyRequest：既无集合也无 Agent 不做全库检索；只给 Agent 时，
// 没有注册表来源或 Agent 未注册都按**请求不合法**拒绝——把"名字打错"与"没有可见集合"
// 分开，调用方才知道该改名字还是该换数据。**没有声明**的已注册 Agent 则回诚实的空范围
// （检索命中零条），不是拒绝：那会把"没声明"伪装成调用失败（D-19 语义）。
func TestAuthz_RefusesAgentOnlyRequest(t *testing.T) {
	ctx := context.Background()
	db := knTestDB(t)
	admin := core.LocalAdminPrincipal()

	_, err := NewKnowledgeAuthz(db, nil).Resolve(ctx, admin, "  ", nil)
	require.ErrorIs(t, err, knowledge.ErrInvalidRequest, "没有集合也没有 Agent：不做全库检索")

	_, err = NewKnowledgeAuthz(db, nil).Resolve(ctx, admin, " demo ", nil)
	require.ErrorIs(t, err, knowledge.ErrInvalidRequest, "缺注册表来源：无法证明声明就不放行")
	require.Contains(t, err.Error(), "注册表来源",
		"缺口写成可修的缺口，不是含糊的没有权限")

	agents := authzRegisteredAgents{
		"demo": authzRegisteredAgent("demo"),
	}
	authz := NewKnowledgeAuthz(db, agents)

	_, err = authz.Resolve(ctx, admin, "ghost", nil)
	require.ErrorIs(t, err, knowledge.ErrInvalidRequest, "未注册的 Agent 按请求不合法拒绝")
	require.Contains(t, err.Error(), "未注册")

	scope, err := authz.Resolve(ctx, admin, " demo ", nil)
	require.NoError(t, err, "没有声明的已注册 Agent：空范围是诚实答案，不是拒绝")
	require.Empty(t, scope.Collections)
	require.Equal(t, "demo", scope.Agent, "Agent 名回填让调用方知道解析的是谁")
	require.Zero(t, scope.DeniedCount, "没有请求任何集合，没有被拒计数")
}

// TestAuthz_AgentDeclarationOnlyNarrows 是 D-19 的核心不变量：Agent 声明只能缩小
// 可见范围，**不得**扩大调用方权限。三条断言分别覆盖：声明命中投影行、声明 ∩ 调用方
// 项目、显式请求与声明取交集。
// 逆改点：把 candidates 直接取并集（或把声明当成"追加放行"）→ 跨项目断言红；把两者
// 都给时的交集改回并发 → 交集断言红。
func TestAuthz_AgentDeclarationOnlyNarrows(t *testing.T) {
	ctx := context.Background()
	db := knTestDB(t)
	// 两个集合：col_allowed 在调用方项目内，col_foreign 属于别的项目。
	authzSeedProjectCollection(t, db, "col_allowed", "proj-a")
	authzSeedProjectCollection(t, db, "col_foreign", "proj-b")
	authzSeedCaller(t, db, "caller_a", "proj-a", "active")
	// Agent 同时声明了两个集合——声明本身不是授权，跨项目那个必须仍然不可见。
	authzSeedKnowledgeProjection(t, db, "demo", "agent_demo", "runbooks", "col_allowed")
	authzSeedKnowledgeProjection(t, db, "demo", "agent_demo", "foreign", "col_foreign")
	agents := authzRegisteredAgents{
		"demo": authzRegisteredAgent("demo",
			core.KnowledgeBinding{Name: "runbooks", Access: core.KnowledgeAccessRead},
			core.KnowledgeBinding{Name: "foreign", Access: core.KnowledgeAccessRead}),
	}
	authz := NewKnowledgeAuthz(db, agents)
	principal := authzCallerPrincipal("caller_a", core.ScopeKnowledgeRead)

	scope, err := authz.Resolve(ctx, principal, "demo", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"col_allowed"}, scope.Collections,
		"声明不能扩大调用方权限：跨项目集合仍不可见")
	require.Equal(t, 1, scope.DeniedCount, "声明里不可见的那个计入被拒")

	// 只声明了跨项目集合 = 全拒 = 越权，不是空结果。
	foreignOnly := authzRegisteredAgents{
		"demo": authzRegisteredAgent("demo",
			core.KnowledgeBinding{Name: "foreign", Access: core.KnowledgeAccessRead}),
	}
	_, err = NewKnowledgeAuthz(db, foreignOnly).Resolve(ctx, principal, "demo", nil)
	require.ErrorIs(t, err, core.ErrForbidden, "声明全部不可见即越权")

	// 显式请求 ∩ 声明：请求了未声明的集合不能借请求绕过声明。
	scope, err = authz.Resolve(ctx, principal, "demo", []string{"col_allowed", "col_foreign"})
	require.NoError(t, err)
	require.Equal(t, []string{"col_allowed"}, scope.Collections,
		"两者都给时取交集：声明之外的可请求集合不可见")

	// col_other 在调用方项目内、对调用方**可见**，但 Agent 没有声明——必须靠交集把它
	// 挡掉。只请求 [col_allowed, col_foreign] 测不出这层：col_foreign 本来就被项目过滤
	// 掉，删掉交集（直接用 requested）也不会红（2026-10-30 逆改实测）。
	// col_other is visible to the caller but not declared, so only the intersection
	// can exclude it; without it the test has no power against ignoring declarations.
	authzSeedProjectCollection(t, db, "col_other", "proj-a")
	scope, err = authz.Resolve(ctx, principal, "demo", []string{"col_allowed", "col_other"})
	require.NoError(t, err)
	require.Equal(t, []string{"col_allowed"}, scope.Collections,
		"两者都给时取交集：调用方可见但未声明的集合也不得带入")
}

// TestAuthz_AgentDeclarationWithoutProjectionSeesNothing：注册闸门已保证"名字有实体"
// 才注册，但投影行与集合目录之间仍可能漂移（集合被软删、投影残留）。声明解析不到 ID
// 时按诚实空范围处理并计入被拒，**不得**退化成全库检索或 500。
func TestAuthz_AgentDeclarationWithoutProjectionSeesNothing(t *testing.T) {
	ctx := context.Background()
	db := knTestDB(t)
	agents := authzRegisteredAgents{
		"demo": authzRegisteredAgent("demo",
			core.KnowledgeBinding{Name: "runbooks", Access: core.KnowledgeAccessRead}),
	}
	authz := NewKnowledgeAuthz(db, agents)

	scope, err := authz.Resolve(ctx, core.LocalAdminPrincipal(), "demo", nil)
	require.NoError(t, err)
	require.Empty(t, scope.Collections)
	require.Equal(t, 1, scope.DeniedCount, "解析不到投影行的声明计入被拒")
	require.Equal(t, "demo", scope.Agent)
}

// TestAuthz_RefusesCollectionsOfAnotherProject：可见范围按 `callers.project` 现读，
// 跨项目集合与不存在的集合走同一条拒绝路径（不区分原因——区分会泄露"它存在"）。
func TestAuthz_RefusesCollectionsOfAnotherProject(t *testing.T) {
	ctx := context.Background()
	db := knTestDB(t)
	authzSeedProjectCollection(t, db, "col_team_a", "proj-a")
	authzSeedProjectCollection(t, db, "col_team_b", "proj-b")
	authzSeedCaller(t, db, "caller_b", "proj-b", "active")
	authz := NewKnowledgeAuthz(db, nil)
	principal := authzCallerPrincipal("caller_b", core.ScopeKnowledgeRead)

	scope, err := authz.Resolve(ctx, principal, "", []string{"col_team_a", "col_team_b"})
	require.NoError(t, err)
	require.Equal(t, []string{"col_team_b"}, scope.Collections, "跨项目集合不可见")
	require.Equal(t, 1, scope.DeniedCount)

	_, err = authz.Resolve(ctx, principal, "", []string{"col_team_a"})
	require.ErrorIs(t, err, core.ErrForbidden, "只请求跨项目集合即越权")
}

// TestAuthz_RefusesInvalidPrincipalBeforeQuery：形态非法的身份在第一次库查询之前
// 就被拒绝；句柄缺失则失败关闭——两种情况都不得回一个空 Scope。
func TestAuthz_RefusesInvalidPrincipalBeforeQuery(t *testing.T) {
	ctx := context.Background()

	_, err := NewKnowledgeAuthz(knTestDB(t), nil).Resolve(ctx, core.Principal{}, "", []string{"col_it"})
	require.ErrorIs(t, err, core.ErrUnauthorized, "空身份必须按未认证拒绝")

	// 未装配的句柄：端口失败关闭，错误仍然是指针而不是空结果。
	_, err = NewKnowledgeAuthz(&DB{}, nil).Resolve(ctx, core.LocalAdminPrincipal(), "", []string{"col_it"})
	require.ErrorIs(t, err, errKnowledgeStoreUnconfigured, "没有句柄即失败关闭")
}
