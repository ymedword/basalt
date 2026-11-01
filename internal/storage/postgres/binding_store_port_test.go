//go:build integration

package postgres

// 本文件是 `agent_resource_bindings` 投影仓储的判卷（颗粒度文档 R4.6 / T114，check
// `012-storage-admin`）：全量重建幂等、定义删掉的绑定随之消失、反查给出引用者。
// 三条用例各守一条纪律——重复投影不改结果、投影跟随定义、闸门读投影不读猜测。
//
// 由 SQLite 谱系 `internal/storage/binding_store_test.go` 逐条移植：断言与逆改点
// 原样保留，只把夹具换成隔离 schema（`portTestDB`）与读写合一的 `db.pool`。
// Ported verbatim from the SQLite lineage; only the fixture and the handle differ.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// bindingFixture 是一份形状完整的 AGENT.md 绑定投影输入（一条模型连接 + 一条通知渠道）。
// bindingFixture builds a complete projection input.
func bindingFixture() core.ResourceBindings {
	return core.ResourceBindings{
		ModelConnections: []core.ResourceBinding{
			{Name: "primary", UID: testResourceUID('A')},
		},
		NotifyChannels: []core.ResourceBinding{
			{Name: "ops", UID: testResourceUID('B')},
		},
	}
}

// bindingRows 直接读投影表的行数（判"删干净了没有"）。
// bindingRows counts projection rows straight from the table.
func bindingRows(t *testing.T, db *DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM agent_resource_bindings`).Scan(&count))
	return count
}

// TestBindingStore_RebuildIsIdempotent：同一输入重建两次，行集与内容完全一致；
// 返回的顺序按 `(kind, ref_name)` 稳定。不同 Agent 的投影互不影响。
// 中文验收点：重复投影不改结果。
func TestBindingStore_RebuildIsIdempotent(t *testing.T) {
	db := portTestDB(t)
	store := NewBindingStore(db)
	ctx := context.Background()

	first := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A")
	second := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B")
	bindings := bindingFixture()

	require.NoError(t, store.Rebuild(ctx, first, "assistant", "sha256:h1", bindings, opNow))
	require.NoError(t, store.Rebuild(ctx, second, "writer", "sha256:h2", bindings, opNow))
	require.Equal(t, 4, bindingRows(t, db))

	require.NoError(t, store.Rebuild(ctx, first, "assistant", "sha256:h1", bindings, opNow))
	require.Equal(t, 4, bindingRows(t, db), "重复投影不得翻倍")

	rows, err := store.Bindings(ctx, first)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	// 排序按 (resource_kind, ref_name)：model_connection 在 notify_channel 之前。
	require.Equal(t, core.BindingModelConnection, rows[0].Kind)
	require.Equal(t, "primary", rows[0].RefName)
	require.Equal(t, testResourceUID('A'), rows[0].ResourceUID)
	require.Equal(t, "sha256:h1", rows[0].ContentHash)
	require.Equal(t, opNow.UnixMilli(), rows[0].ProjectedAt)
	require.Equal(t, core.BindingNotifyChannel, rows[1].Kind)
	require.Equal(t, "ops", rows[1].RefName)

	// Clear 只动目标 Agent。
	require.NoError(t, store.Clear(ctx, first))
	require.Equal(t, 2, bindingRows(t, db))
	rows, err = store.Bindings(ctx, first)
	require.NoError(t, err)
	require.Empty(t, rows)
}

// TestBindingStore_ReverseLookup：按资源 UID 反查引用者——多个 Agent 绑同一资源时
// 全部返回；未绑定的资源返回空；非法 UID 在查询前拒绝。
// 中文验收点：删除闸门拿到的是投影事实，不是"当前没有运行"的推断。
func TestBindingStore_ReverseLookup(t *testing.T) {
	db := portTestDB(t)
	store := NewBindingStore(db)
	ctx := context.Background()

	shared := testResourceUID('A')
	first := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A")
	second := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B")
	bindings := core.ResourceBindings{
		ModelConnections: []core.ResourceBinding{{Name: "primary", UID: shared}},
	}
	require.NoError(t, store.Rebuild(ctx, first, "assistant", "sha256:h1", bindings, opNow))
	require.NoError(t, store.Rebuild(ctx, second, "writer", "sha256:h2", bindings, opNow))

	referrers, err := store.ReferencingAgents(ctx, shared)
	require.NoError(t, err)
	require.Len(t, referrers, 2)
	require.Equal(t, "assistant", referrers[0].AgentName, "按 agent_name 升序")
	require.Equal(t, first, referrers[0].AgentUID)
	require.Equal(t, "writer", referrers[1].AgentName)

	referrers, err = store.ReferencingAgents(ctx, testResourceUID('Z'))
	require.NoError(t, err)
	require.Empty(t, referrers)

	_, err = store.ReferencingAgents(ctx, core.ResourceUID("not-a-uid"))
	require.Error(t, err)
}

// TestBindingStore_RebuildProjectsKnowledgeCollections —— D-19 的投影落库：知识集合
// 行与资源绑定**同批**整份替换（kind = knowledge_collection，ref_name = 声明名，
// resource_uid = 稳定集合 ID）；定义删掉声明 → 旧知识行随之消失；缺集合 ID 的绑定
// 在写库前拒绝，且不破坏旧投影。
// 逆改点：Rebuild 只写资源绑定（忽略 knowledge 参数）→ 行数断言红；把两个写点拆成
// 两条路径 → 拒绝路径的"旧行仍在"断言可能红。
func TestBindingStore_RebuildProjectsKnowledgeCollections(t *testing.T) {
	db := portTestDB(t)
	store := NewBindingStore(db)
	ctx := context.Background()

	agent := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A")
	knowledge := []core.KnowledgeCollectionBinding{
		{Name: "runbooks", CollectionID: "col_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"},
		{Name: "faq", CollectionID: "col_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"},
	}
	require.NoError(t, store.Rebuild(ctx, agent, "assistant", "sha256:h1", bindingFixture(), opNow,
		knowledge...))
	require.Equal(t, 4, bindingRows(t, db), "两条资源绑定 + 两条知识集合")

	rows, err := store.Bindings(ctx, agent)
	require.NoError(t, err)
	require.Len(t, rows, 4)
	// 排序按 (resource_kind, ref_name)：knowledge_collection 排在 model_connection 之前。
	require.Equal(t, core.BindingKnowledgeCollection, rows[0].Kind)
	require.Equal(t, "faq", rows[0].RefName)
	require.Equal(t, core.ResourceUID("col_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"), rows[0].ResourceUID,
		"身份列必须是集合 ID，不是声明名")
	require.Equal(t, "sha256:h1", rows[0].ContentHash, "知识行与资源行共享同一份定义哈希")
	require.Equal(t, core.BindingKnowledgeCollection, rows[1].Kind)
	require.Equal(t, "runbooks", rows[1].RefName)
	refs, err := store.KnowledgeReferences(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"assistant"}, refs["col_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"])
	require.Equal(t, []string{"assistant"}, refs["col_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"])

	// 缺集合 ID 的声明在写库前拒绝，旧投影原样保留。
	require.Error(t, store.Rebuild(ctx, agent, "assistant", "sha256:h2", bindingFixture(), opNow,
		core.KnowledgeCollectionBinding{Name: "runbooks"}))
	require.Equal(t, 4, bindingRows(t, db), "被拒的写不得清掉旧投影")

	// 声明删掉：知识行随之消失（整份替换，不残留）。
	require.NoError(t, store.Rebuild(ctx, agent, "assistant", "sha256:h3", bindingFixture(), opNow))
	require.Equal(t, 2, bindingRows(t, db), "资源绑定保留、知识行消失")
	refs, err = store.KnowledgeReferences(ctx)
	require.NoError(t, err)
	require.Empty(t, refs)
}

// TestBindingStore_RebuildReplacesStaleRows：定义里删掉一条绑定，重建后投影里就没有它；
// 反查随之不再命中——否则删除闸门会永远挡住一个已经没人引用的资源。
// 中文验收点：投影跟随定义，旧绑定不残留。
func TestBindingStore_RebuildReplacesStaleRows(t *testing.T) {
	db := portTestDB(t)
	store := NewBindingStore(db)
	ctx := context.Background()

	agent := core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A")
	require.NoError(t, store.Rebuild(ctx, agent, "assistant", "sha256:h1", bindingFixture(), opNow))
	require.Equal(t, 2, bindingRows(t, db))

	retired := testResourceUID('B')
	referrers, err := store.ReferencingAgents(ctx, retired)
	require.NoError(t, err)
	require.Len(t, referrers, 1)

	// 新定义只剩模型连接：通知渠道那条绑定被删掉。
	require.NoError(t, store.Rebuild(ctx, agent, "assistant", "sha256:h2", core.ResourceBindings{
		ModelConnections: []core.ResourceBinding{{Name: "primary", UID: testResourceUID('A')}},
	}, opNow.Add(time.Second)))
	require.Equal(t, 1, bindingRows(t, db), "旧的渠道绑定被全量替换掉")

	referrers, err = store.ReferencingAgents(ctx, retired)
	require.NoError(t, err)
	require.Empty(t, referrers, "退役资源不再被引用")

	rows, err := store.Bindings(ctx, agent)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "sha256:h2", rows[0].ContentHash, "投影记录跟着新定义走")

	// 非法输入在写库前拒绝：空名称 / 非法 UID / 未知 Agent 身份。
	require.Error(t, store.Rebuild(ctx, agent, "", "sha256:h3", core.ResourceBindings{}, opNow))
	require.Error(t, store.Rebuild(ctx, agent, "assistant", "sha256:h3", core.ResourceBindings{
		ModelConnections: []core.ResourceBinding{{Name: "primary", UID: "rs_bogus"}},
	}, opNow))
	require.Error(t, store.Rebuild(ctx, core.AgentUID("bogus"), "assistant", "sha256:h3", core.ResourceBindings{}, opNow))
	require.Equal(t, 1, bindingRows(t, db), "被拒的写不留行")
}
