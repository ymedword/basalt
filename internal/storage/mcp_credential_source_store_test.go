package storage

// 本文件判卷 `mcp_credential_sources`（14D / 设计 §2.3.2）的仓储面：来源必须显式声明、
// 首写修订号 1 之后每次成功写入 +1、补记只填**没有声明**的槽位且不改显式声明。
// 来源进表的意义是撤销不隐式回落进程环境——本文件守住"声明不被补记抹掉"这条。
// These tests pin the per-placeholder credential sources: explicit declarations, the
// revision bump, and a backfill that never rewrites an explicit choice.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// sourceStoreUID 是测试用的 MCP server 资源 UID。
const sourceStoreUID = core.ResourceUID("rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C")

// newSourceStore 给每个用例一个独立的真库。
func newSourceStore(t *testing.T) (*MCPCredentialSourceStore, *DB) {
	t.Helper()
	db := openTempDB(t)
	return NewMCPCredentialSourceStore(db), db
}

// TestMCPCredentialSourceStore_UpsertStartsAtOneAndBumps —— 首写 `revision=1`，其后每次
// 成功写入 +1，模式、时间与操作标识都换成本次的值。
// 逆改点：去掉 `revision + 1` → 修订号永远停在 1，管理台无法判"来源改过几次"，本用例红。
func TestMCPCredentialSourceStore_UpsertStartsAtOneAndBumps(t *testing.T) {
	store, _ := newSourceStore(t)
	ctx := context.Background()

	require.NoError(t, store.Upsert(ctx, core.MCPCredentialSource{
		ServerUID:   sourceStoreUID,
		BindingKey:  "GITHUB_PERSONAL_ACCESS_TOKEN",
		Mode:        core.CredentialModeEnv,
		OperationID: "op-first",
	}, policyNow))

	first, ok, err := store.Get(ctx, sourceStoreUID, "GITHUB_PERSONAL_ACCESS_TOKEN")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, first.Revision)
	require.Equal(t, core.CredentialModeEnv, first.Mode)
	require.Equal(t, "op-first", first.OperationID)
	require.Equal(t, policyNow.UnixMilli(), first.UpdatedAt)

	later := policyNow.Add(2 * time.Minute)
	require.NoError(t, store.Upsert(ctx, core.MCPCredentialSource{
		ServerUID:   sourceStoreUID,
		BindingKey:  "GITHUB_PERSONAL_ACCESS_TOKEN",
		Mode:        core.CredentialModeManaged,
		OperationID: "op-second",
	}, later))

	second, ok, err := store.Get(ctx, sourceStoreUID, "GITHUB_PERSONAL_ACCESS_TOKEN")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2, second.Revision)
	require.Equal(t, core.CredentialModeManaged, second.Mode)
	require.Equal(t, "op-second", second.OperationID)
	require.Equal(t, later.UnixMilli(), second.UpdatedAt)
}

// TestMCPCredentialSourceStore_UpsertRejectsInvalidShape —— UID / 槽位名 / 模式 / 操作号
// 缺一即拒，且不留半截行。
func TestMCPCredentialSourceStore_UpsertRejectsInvalidShape(t *testing.T) {
	store, _ := newSourceStore(t)
	ctx := context.Background()

	cases := map[string]core.MCPCredentialSource{
		"uid":         {ServerUID: "rs_bogus", BindingKey: "TOKEN", Mode: core.CredentialModeEnv, OperationID: "op-x"},
		"binding_key": {ServerUID: sourceStoreUID, BindingKey: "BAD KEY", Mode: core.CredentialModeEnv, OperationID: "op-x"},
		"mode":        {ServerUID: sourceStoreUID, BindingKey: "TOKEN", Mode: core.CredentialMode("plain"), OperationID: "op-x"},
		"operation":   {ServerUID: sourceStoreUID, BindingKey: "TOKEN", Mode: core.CredentialModeEnv, OperationID: "  "},
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			require.Error(t, store.Upsert(ctx, source, policyNow), "%s 非法必须拒绝", name)
		})
	}

	listed, err := store.List(ctx, sourceStoreUID)
	require.NoError(t, err)
	require.Empty(t, listed, "被拒的写入不得留下任何行")
}

// TestMCPCredentialSourceStore_UpsertZeroTimeUsesNow —— 零值时间不落成 1970：
// 调用方没给时刻时取当前时间（修订时间戳只用于展示与对账，不参与判定）。
func TestMCPCredentialSourceStore_UpsertZeroTimeUsesNow(t *testing.T) {
	store, _ := newSourceStore(t)
	ctx := context.Background()
	require.NoError(t, store.Upsert(ctx, core.MCPCredentialSource{
		ServerUID:   sourceStoreUID,
		BindingKey:  "BRAVE_API_KEY",
		Mode:        core.CredentialModeEnv,
		OperationID: "op-now",
	}, time.Time{}))

	rec, ok, err := store.Get(ctx, sourceStoreUID, "BRAVE_API_KEY")
	require.NoError(t, err)
	require.True(t, ok)
	require.Positive(t, rec.UpdatedAt)
}

// TestMCPCredentialSourceStore_BackfillOnlyFillsUndeclared —— 补记只给没有声明的槽位写
// `env`，已有显式声明（含 `managed`）一字不动；返回实际补记条数，重跑补记 0 条。
// 逆改点：补记改成 upsert 覆盖 → 撤销后"managed"被改回 env，等于隐式回落进程环境，本用例红。
func TestMCPCredentialSourceStore_BackfillOnlyFillsUndeclared(t *testing.T) {
	store, _ := newSourceStore(t)
	ctx := context.Background()
	require.NoError(t, store.Upsert(ctx, core.MCPCredentialSource{
		ServerUID:   sourceStoreUID,
		BindingKey:  "GITHUB_PERSONAL_ACCESS_TOKEN",
		Mode:        core.CredentialModeManaged,
		OperationID: "op-managed",
	}, policyNow))

	added, err := store.BackfillEnvSources(ctx, sourceStoreUID,
		[]string{"GITHUB_PERSONAL_ACCESS_TOKEN", "BRAVE_API_KEY"}, "system.backfill", policyNow)
	require.NoError(t, err)
	require.Equal(t, 1, added, "只补记没有声明的槽位")

	declared, ok, err := store.Get(ctx, sourceStoreUID, "GITHUB_PERSONAL_ACCESS_TOKEN")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.CredentialModeManaged, declared.Mode, "显式声明不得被补记改写")
	require.Equal(t, 1, declared.Revision)
	require.Equal(t, "op-managed", declared.OperationID)

	backfilled, ok, err := store.Get(ctx, sourceStoreUID, "BRAVE_API_KEY")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.CredentialModeEnv, backfilled.Mode)
	require.Equal(t, 1, backfilled.Revision)

	again, err := store.BackfillEnvSources(ctx, sourceStoreUID,
		[]string{"GITHUB_PERSONAL_ACCESS_TOKEN", "BRAVE_API_KEY"}, "system.backfill", policyNow)
	require.NoError(t, err)
	require.Equal(t, 0, again, "补记是幂等的")
}

// TestMCPCredentialSourceStore_BackfillRejectsInvalidInput —— 非法 UID / 空操作号在写入
// 前拒绝；列表中间出现非法槽位名时如实返回已补记条数与错误（不静默跳过）。
func TestMCPCredentialSourceStore_BackfillRejectsInvalidInput(t *testing.T) {
	store, _ := newSourceStore(t)
	ctx := context.Background()

	_, err := store.BackfillEnvSources(ctx, "rs_bogus", []string{"TOKEN"}, "op-x", policyNow)
	require.Error(t, err)

	_, err = store.BackfillEnvSources(ctx, sourceStoreUID, []string{"TOKEN"}, "", policyNow)
	require.Error(t, err)
	require.Contains(t, err.Error(), "operation_id 不能为空")

	added, err := store.BackfillEnvSources(ctx, sourceStoreUID,
		[]string{"BRAVE_API_KEY", "BAD KEY", "FETCH_TOKEN"}, "op-x", policyNow)
	require.Error(t, err, "非法槽位名不得静默跳过")
	require.Contains(t, err.Error(), "非法槽位名")
	require.Equal(t, 1, added, "错误前的补记条数如实返回")

	listed, err := store.List(ctx, sourceStoreUID)
	require.NoError(t, err)
	require.Len(t, listed, 1, "非法槽位名之后的键不得继续写入")
	require.Equal(t, "BRAVE_API_KEY", listed[0].BindingKey)
}

// TestMCPCredentialSourceStore_GetMissAndListOrder —— 未声明 ok=false；List 按绑定键升序、
// 空清单是非 nil 空切片（调用方可以无差别 range）。
// 逆改点：List 返回 nil / 无序 → 页面在空清单上 range 出 null，本用例红。
func TestMCPCredentialSourceStore_GetMissAndListOrder(t *testing.T) {
	store, _ := newSourceStore(t)
	ctx := context.Background()

	_, ok, err := store.Get(ctx, sourceStoreUID, "NEVER_DECLARED")
	require.NoError(t, err)
	require.False(t, ok)

	empty, err := store.List(ctx, sourceStoreUID)
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Empty(t, empty)

	for _, key := range []string{"ZED_TOKEN", "ALPHA_TOKEN", "MIDDLE_TOKEN"} {
		require.NoError(t, store.Upsert(ctx, core.MCPCredentialSource{
			ServerUID:   sourceStoreUID,
			BindingKey:  key,
			Mode:        core.CredentialModeEnv,
			OperationID: "op-" + key,
		}, policyNow))
	}
	listed, err := store.List(ctx, sourceStoreUID)
	require.NoError(t, err)
	require.Equal(t, []string{"ALPHA_TOKEN", "MIDDLE_TOKEN", "ZED_TOKEN"},
		[]string{listed[0].BindingKey, listed[1].BindingKey, listed[2].BindingKey})
	require.Equal(t, sourceStoreUID, listed[0].ServerUID)
}

// TestMCPCredentialSourceStore_ReadValidation —— 读侧同样校验 UID 与槽位名：
// 非法标识是形状错误，不返回"没有声明"（后者会让调用方按缺失处理）。
func TestMCPCredentialSourceStore_ReadValidation(t *testing.T) {
	store, _ := newSourceStore(t)
	ctx := context.Background()

	_, _, err := store.Get(ctx, "rs_bogus", "TOKEN")
	require.Error(t, err)

	_, _, err = store.Get(ctx, sourceStoreUID, "BAD KEY")
	require.Error(t, err)
	require.Contains(t, err.Error(), "非法槽位名")

	_, err = store.List(ctx, "rs_bogus")
	require.Error(t, err)
}

// TestMCPCredentialSourceStore_ErrorPathsReportContext —— 池故障带上下文前缀返回。
func TestMCPCredentialSourceStore_ErrorPathsReportContext(t *testing.T) {
	ctx := context.Background()

	t.Run("read", func(t *testing.T) {
		store, db := newSourceStore(t)
		closeReadPool(t, db)
		_, _, err := store.Get(ctx, sourceStoreUID, "TOKEN")
		require.Error(t, err)
		require.Contains(t, err.Error(), "读取 MCP")
		_, err = store.List(ctx, sourceStoreUID)
		require.Error(t, err)
		require.Contains(t, err.Error(), "列出 MCP")
	})

	t.Run("write", func(t *testing.T) {
		store, db := newSourceStore(t)
		closeWritePool(t, db)
		err := store.Upsert(ctx, core.MCPCredentialSource{
			ServerUID:   sourceStoreUID,
			BindingKey:  "TOKEN",
			Mode:        core.CredentialModeEnv,
			OperationID: "op-x",
		}, policyNow)
		require.Error(t, err)
		require.Contains(t, err.Error(), "写入 MCP")

		_, err = store.BackfillEnvSources(ctx, sourceStoreUID, []string{"TOKEN"}, "op-x", policyNow)
		require.Error(t, err)
		require.Contains(t, err.Error(), "补记 MCP")
	})
}
