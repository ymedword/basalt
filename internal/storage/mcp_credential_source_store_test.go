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

	report, err := store.BackfillEnvSources(ctx, sourceStoreUID,
		[]string{"GITHUB_PERSONAL_ACCESS_TOKEN", "BRAVE_API_KEY"}, "system.backfill", policyNow)
	require.NoError(t, err)
	require.Equal(t, []string{"BRAVE_API_KEY"}, report.Filled, "只补记没有声明的槽位")
	require.Empty(t, report.Skipped)
	require.Empty(t, report.Conflict)
	require.Equal(t, []string{"GITHUB_PERSONAL_ACCESS_TOKEN"}, report.ConfigDrift,
		"文件声明了槽位、库里却是 managed：按配置漂移上报，不改既有行")

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
	require.Equal(t, 0, again.FilledCount(), "补记是幂等的")
	require.Equal(t, []string{"BRAVE_API_KEY"}, again.Skipped, "重跑时既有 env 行走 skipped")
}

// TestMCP_BackfillReportsMissingAndDrift —— 设计 §2.16.1 ②：报告必须按槽位分四类列出，
// **不得**只报一个总数。缺行 → filled；既有 env（导入带过来的）→ skipped；文件声明了
// 槽位而库里是 managed → config_drift；conflict 是闭集外模式的防御分支，schema CHECK
// 下不可达，正常数据里必须为空。
// 逆改点：报告退化成只返回 filled 计数 → skipped / config_drift 断言红；把 managed
// 既有行并进 skipped → 配置漂移被掩盖，config_drift 断言红。
// TestMCP_BackfillReportsMissingAndDrift pins the four-way per-slot report.
func TestMCP_BackfillReportsMissingAndDrift(t *testing.T) {
	store, _ := newSourceStore(t)
	ctx := context.Background()

	// ALREADY_ENV 模拟迁移带入的 env 行：操作号是导入期的那一次，不是本次补记。
	require.NoError(t, store.Upsert(ctx, core.MCPCredentialSource{
		ServerUID:   sourceStoreUID,
		BindingKey:  "ALREADY_ENV",
		Mode:        core.CredentialModeEnv,
		OperationID: "migration.import.v6",
	}, policyNow.Add(-24*time.Hour)))
	// MANAGED_SLOT 在已发布定义里声明（文件只能表达 `${VAR}`），库里却是托管。
	require.NoError(t, store.Upsert(ctx, core.MCPCredentialSource{
		ServerUID:   sourceStoreUID,
		BindingKey:  "MANAGED_SLOT",
		Mode:        core.CredentialModeManaged,
		OperationID: "op-managed",
	}, policyNow))

	report, err := store.BackfillEnvSources(ctx, sourceStoreUID,
		[]string{"ALREADY_ENV", "MANAGED_SLOT", "FRESH_SLOT"}, "system.backfill", policyNow)
	require.NoError(t, err)
	require.Equal(t, []string{"FRESH_SLOT"}, report.Filled, "缺行才补")
	require.Equal(t, []string{"ALREADY_ENV"}, report.Skipped, "既有 env 行不动")
	require.Equal(t, []string{"MANAGED_SLOT"}, report.ConfigDrift, "配置漂移要单独可见")
	require.Empty(t, report.Conflict, "闭集外模式在 schema CHECK 下不可达")
	require.Equal(t, 1, report.FilledCount())
	require.Equal(t, 3,
		len(report.Filled)+len(report.Skipped)+len(report.Conflict)+len(report.ConfigDrift),
		"每个槽位恰好落一个分类——不得漏报")

	// "分类出来"还要"没被动过"：managed 既有行必须原样保留（模式、操作号、版本都不变），
	// 否则报告说 config_drift、库里却已被覆写成 env —— 漂移就被自己的补记抹平了。
	managed, found, err := store.Get(ctx, sourceStoreUID, "MANAGED_SLOT")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, core.CredentialModeManaged, managed.Mode, "managed 行不得被降级成 env")
	require.Equal(t, "op-managed", managed.OperationID, "既有行的操作号不得被补记覆盖")
	require.Equal(t, 1, managed.Revision, "既有行不得因补记加版本")
}

// TestMCP_BackfillDoesNotOverwriteMigratedEnvSource —— 设计 §2.16.1 ①：导入带进来的
// env 来源行（含迁移期的 operation_id 与 updated_at）逐字段保留，补记不得 UPDATE。
// 逆改点：把 INSERT ... DO NOTHING 改成 upsert → operation_id / revision 被改写，本用例红。
// TestMCP_BackfillDoesNotOverwriteMigratedEnvSource keeps migrated env rows verbatim.
func TestMCP_BackfillDoesNotOverwriteMigratedEnvSource(t *testing.T) {
	store, _ := newSourceStore(t)
	ctx := context.Background()
	migratedAt := policyNow.Add(-24 * time.Hour)
	require.NoError(t, store.Upsert(ctx, core.MCPCredentialSource{
		ServerUID:   sourceStoreUID,
		BindingKey:  "MIGRATED_TOKEN",
		Mode:        core.CredentialModeEnv,
		OperationID: "migration.import.v6",
	}, migratedAt))
	before, ok, err := store.Get(ctx, sourceStoreUID, "MIGRATED_TOKEN")
	require.NoError(t, err)
	require.True(t, ok)

	report, err := store.BackfillEnvSources(ctx, sourceStoreUID,
		[]string{"MIGRATED_TOKEN"}, "system.backfill", policyNow)
	require.NoError(t, err)
	require.Empty(t, report.Filled, "既有行不算补记")
	require.Equal(t, []string{"MIGRATED_TOKEN"}, report.Skipped)

	after, ok, err := store.Get(ctx, sourceStoreUID, "MIGRATED_TOKEN")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, before, after, "导入带入的 env 来源行逐字段不变")
	require.Equal(t, "migration.import.v6", after.OperationID, "补记不得改写来源行的操作号")
	require.Equal(t, 1, after.Revision)
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

	report, err := store.BackfillEnvSources(ctx, sourceStoreUID,
		[]string{"BRAVE_API_KEY", "BAD KEY", "FETCH_TOKEN"}, "op-x", policyNow)
	require.Error(t, err, "非法槽位名不得静默跳过")
	require.Contains(t, err.Error(), "非法槽位名")
	require.Equal(t, 1, report.FilledCount(), "错误前的补记条数如实返回")
	require.Equal(t, []string{"BRAVE_API_KEY"}, report.Filled)

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
