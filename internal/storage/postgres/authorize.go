package postgres

// 本文件是 `knowledge.Authz` 的 PostgreSQL 实现（US-16 §2.3 图 4 / C-08 / T129）：
// 把「本次检索能看见哪些集合」解析成 `knowledge.Scope`，让两条检索路径在 SQL 里按
// 集合过滤。四条纪律：
//
//  1. **拒绝不是空结果**：请求的集合一个都不可见时返回 `core.ErrForbidden`，**不得**
//     回一个空 `Scope`。空 `Scope` 在检索管线里等于"查过、没命中"，会把越权伪装成
//     "库里没有"——调用方无从区分，审计也留不下拒绝。
//  2. **项目取自 `callers` 行，不取自身份自带字段**：`core.Principal.Project` 的契约
//     写明「不参与授权判定」；可见范围必须按 `callers.project` 现读（空串 = 不限）。
//     停在 `status='disabled'` 的调用方按「已不可用」拒绝。
//  3. **被拒元素只回计数**：`Scope.DeniedCount` 是唯一出口，被拒集合的标识**不得**
//     出现在返回值、日志标题或诊断 attrs（C-08）。
//  4. **Agent 声明只收窄**：带 Agent 的请求先按注册表条目读声明名、再按投影行换成
//     稳定集合 ID，然后与调用方自己的项目 / scope 求交——**声明不能扩大调用方权限**。
//     未注册的 Agent 一律拒绝（名字打错与"没有权限"不可区分）；有声明但一个都不可见
//     时按越权拒绝；**没有声明**则返回空范围（诚实地说"这个 Agent 看不到集合"），
//     而不是拒绝——那会把"没声明"伪装成调用失败。
//
// This file resolves the retrieval visibility scope. A fully denied request is an
// explicit ErrForbidden, never an empty scope; the caller project comes from the
// `callers` row because Principal.Project is documented as non-authoritative; rejected
// collections are counted, never named. Agent declarations only narrow: they are read
// from the registry entry and projected to ids, then intersected with the caller's own
// project and scope.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// KnowledgeAgentSource 是授权解析读取 Agent 声明的最小面（`*app.agentRegistry`
// 满足它）。只读 `Registered` 条目：未注册的 Agent 在检索面上等同不存在。
// KnowledgeAgentSource is the minimal read face over registered agents.
type KnowledgeAgentSource interface {
	Lookup(name string) (core.AgentEntry, bool)
}

// knowledgeAuthzScope 是 `knowledge.Authz` 的 PostgreSQL 实现。
// knowledgeAuthzScope implements the retrieval Authz port on PostgreSQL.
type knowledgeAuthzScope struct {
	db     *DB
	agents KnowledgeAgentSource
}

// 编译期断言：端口形状由本类型满足。
// The implementation satisfies the consumer port.
var _ knowledge.Authz = (*knowledgeAuthzScope)(nil)

// NewKnowledgeAuthz 构造可见范围解析器；返回类型不导出，调用方按端口使用。
// `agents` 为 nil 时带 Agent 的请求失败关闭（无法证明声明就不放行）。
// NewKnowledgeAuthz builds the resolver; a nil agent source keeps agent requests failing
// closed.
func NewKnowledgeAuthz(db *DB, agents KnowledgeAgentSource) *knowledgeAuthzScope {
	return &knowledgeAuthzScope{db: db, agents: agents}
}

// ready 是所有方法的统一前置：句柄缺失即失败关闭。
// ready is the shared fail-closed precondition.
func (s *knowledgeAuthzScope) ready() error {
	if s == nil || s.db == nil || s.db.pool == nil {
		return errKnowledgeStoreUnconfigured
	}
	return nil
}

// Resolve 解析本次检索的可见范围：身份 → scope → 调用方项目 → 集合交集。
// 顺序不可交换：身份与 scope 的判定发生在任何库查询之前，被停用的调用方也**不得**
// 走到集合解析（那会先泄露"这些集合存在"）。
//
// Resolve resolves the visible scope. Identity and scope are checked before any query so
// a disabled caller never reaches collection resolution.
func (s *knowledgeAuthzScope) Resolve(ctx context.Context, p core.Principal, agent string,
	collections []string,
) (knowledge.Scope, error) {
	if err := s.ready(); err != nil {
		return knowledge.Scope{}, err
	}
	if !p.Valid() {
		return knowledge.Scope{}, fmt.Errorf("%w: 检索身份不可用", core.ErrUnauthorized)
	}
	if !p.HasScope(core.ScopeKnowledgeRead) {
		return knowledge.Scope{}, fmt.Errorf("%w: 缺少 %s scope", core.ErrForbidden, core.ScopeKnowledgeRead)
	}
	project, err := s.callerProject(ctx, p.CallerID)
	if err != nil {
		return knowledge.Scope{}, err
	}

	agent = strings.TrimSpace(agent)
	requested := uniqueCollections(collections)
	if len(requested) == 0 && agent == "" {
		return knowledge.Scope{}, fmt.Errorf("%w: 检索必须给出集合或 Agent——不做全库检索",
			knowledge.ErrInvalidRequest)
	}

	// 候选来自两侧：显式集合原样，Agent 声明经投影换成集合 ID。两者都给时取交集
	// ——声明只能收窄调用方的请求，不能凭空多给一个集合。
	// Candidates come from both sides; when both are present the intersection is taken,
	// because a declaration can only narrow what the caller asked for.
	candidates := requested
	deniedNames := 0
	if agent != "" {
		declared, unresolved, err := s.agentCollections(ctx, agent)
		if err != nil {
			return knowledge.Scope{}, err
		}
		deniedNames = unresolved
		if len(requested) == 0 {
			candidates = declared
		} else {
			candidates = intersectCollections(requested, declared)
		}
	}

	visible, err := s.visibleCollections(ctx, project, candidates)
	if err != nil {
		return knowledge.Scope{}, err
	}
	denied := len(candidates) - len(visible) + deniedNames
	if len(visible) == 0 {
		if len(requested) == 0 && len(candidates) == 0 {
			// 只给了 Agent、且它没有可解析的声明：空范围是**诚实**答案（检索会命中零条），
			// 不是越权——调用方并没有请求任何具体集合。
			// An agent with nothing resolvable sees an honestly empty scope.
			return knowledge.Scope{Agent: agent, DeniedCount: deniedNames}, nil
		}
		// 全部被拒即越权：**不得**返回空 Scope 让调用方以为"没有命中"。
		// A fully denied request is an authorization failure, not an empty result.
		return knowledge.Scope{}, fmt.Errorf("%w: 请求的 %d 个集合均不可见", core.ErrForbidden, denied)
	}
	return knowledge.Scope{Collections: visible, Agent: agent, DeniedCount: denied}, nil
}

// agentCollections 读一个**已注册** Agent 声明的集合：注册表条目给声明名，投影行
// （`agent_resource_bindings`，kind = `knowledge_collection`）给名字对应的稳定集合 ID。
// 返回 (候选集合 ID 按声明序, 未能解析成 ID 的声明数)。
//
// 未注册 / 未知 Agent 按 `ErrInvalidRequest` 拒绝：把"名字打错"和"没有可见集合"分开
// 需要一次额外查找，而这两件事对调用方的正确动作不同（改名字 vs 换数据）。
// agentCollections reads the declared ids of a registered agent; an unregistered agent is
// refused instead of silently seeing nothing.
func (s *knowledgeAuthzScope) agentCollections(ctx context.Context, agent string) ([]string, int, error) {
	if s.agents == nil {
		return nil, 0, fmt.Errorf("%w: 知识检索缺少 Agent 注册表来源，无法解析 Agent 声明",
			knowledge.ErrInvalidRequest)
	}
	entry, ok := s.agents.Lookup(agent)
	if !ok || !entry.Registered {
		return nil, 0, fmt.Errorf("%w: Agent %q 未注册，无法解析可见集合",
			knowledge.ErrInvalidRequest, agent)
	}
	var declared []string
	if entry.Profile != nil {
		declared = core.KnowledgeBindingNames(entry.Profile.Knowledge)
	}
	if len(declared) == 0 {
		return nil, 0, nil
	}

	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT ref_name, resource_uid
		  FROM agent_resource_bindings
		 WHERE agent_name = $1 AND resource_kind = $2 AND ref_name = ANY($3)
		 ORDER BY ref_name`, agent, string(core.BindingKnowledgeCollection), declared)
	if err != nil {
		return nil, 0, fmt.Errorf("解析 Agent %q 的知识集合投影: %w", agent, err)
	}
	defer func() { _ = rows.Close() }()

	// 按**声明序**输出（不是投影的字典序）：`Scope.Collections` 因此可复现，且与
	// 声明顺序一致，排障时能逐条对号。
	byName := make(map[string]string, len(declared))
	for rows.Next() {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			return nil, 0, fmt.Errorf("读取 Agent %q 的知识集合投影: %w", agent, err)
		}
		byName[name] = id
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("读取 Agent %q 的知识集合投影: %w", agent, err)
	}

	ids := make([]string, 0, len(declared))
	unresolved := 0
	for _, name := range declared {
		id, ok := byName[name]
		if !ok || strings.TrimSpace(id) == "" {
			unresolved++
			continue
		}
		ids = append(ids, id)
	}
	return ids, unresolved, nil
}

// intersectCollections 返回 requested 中同时出现在 declared 里的元素（保持 requested
// 顺序、去重）：调用方的请求是主语，Agent 声明只决定"其中哪些仍然可用"。
// intersectCollections keeps the request order and drops anything the agent did not
// declare.
func intersectCollections(requested, declared []string) []string {
	if len(requested) == 0 || len(declared) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(declared))
	for _, id := range declared {
		allowed[id] = struct{}{}
	}
	out := make([]string, 0, len(requested))
	for _, id := range requested {
		if _, ok := allowed[id]; !ok {
			continue
		}
		out = append(out, id)
	}
	return out
}

// callerProject 读调用方的项目范围与状态；判定口径见 `lookupCallerProject`。
// callerProject delegates to the shared caller-project lookup.
func (s *knowledgeAuthzScope) callerProject(ctx context.Context, callerID string) (string, error) {
	return lookupCallerProject(ctx, s.db, callerID)
}

// lookupCallerProject 是 `callers` 行项目范围的**唯一**读取入口：检索授权（本文件）
// 与目录授权（`knowledge_catalog.go`）共用，**不得**各自再写一份 SQL，否则两处口径
// 一旦分叉，「同一个调用方在检索与目录里看到不同范围」将无从发现。
// 空串表示不限；停用 / 不存在的调用方按「未认证」拒绝——与 Key 认证「停用即未找到」
// 同一口径。
// lookupCallerProject is the single read path for a caller's project scope, shared by
// retrieval authz and catalog authz so the two can never drift apart. An empty project
// means unrestricted; a disabled or missing caller is refused as unauthenticated.
func lookupCallerProject(ctx context.Context, db *DB, callerID string) (string, error) {
	var project, status string
	err := db.pool.QueryRowContext(ctx,
		`SELECT project, status FROM callers WHERE caller_id = $1`, callerID).
		Scan(&project, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: 调用方不存在", core.ErrUnauthorized)
	}
	if err != nil {
		return "", fmt.Errorf("读取调用方项目范围: %w", err)
	}
	if status != "active" {
		return "", fmt.Errorf("%w: 调用方已停用", core.ErrUnauthorized)
	}
	return project, nil
}

// visibleCollections 在一条查询里判定可见性：未软删，且（调用方项目为空 **或**
// 集合项目与之一致）。软删的集合与项目不匹配的集合一样计入被拒，不区分原因——
// 区分会把「曾经存在」暴露给无权知道的人。
// visibleCollections decides visibility in one query; a soft-deleted collection is
// indistinguishable from a mismatched one, on purpose.
func (s *knowledgeAuthzScope) visibleCollections(ctx context.Context, project string,
	requested []string,
) ([]string, error) {
	if len(requested) == 0 {
		// 空候选不查库：`= ANY(NULL)` 的类型推断依赖驱动，而"没有候选"本身已经可判。
		// No candidates means no query; an empty scope is decided without the database.
		return nil, nil
	}
	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT collection_id FROM kb_collections
		WHERE collection_id = ANY($1)
		  AND deleted_at IS NULL
		  AND ($2 = '' OR project = $2)
		ORDER BY collection_id`, requested, project)
	if err != nil {
		return nil, fmt.Errorf("解析可见集合: %w", err)
	}
	defer func() { _ = rows.Close() }()
	visible := make([]string, 0, len(requested))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读取可见集合: %w", err)
		}
		visible = append(visible, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取可见集合: %w", err)
	}
	return visible, nil
}

// uniqueCollections 去空白、去重并保持请求顺序：`DeniedCount` 是"请求了几个、拒了
// 几个"的差，重复项会让计数虚高；顺序保持让 `Scope.Collections` 可复现。
// uniqueCollections trims and dedupes while keeping the request order, so the denied
// count is exact and the scope is reproducible.
func uniqueCollections(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}
