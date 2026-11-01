//go:build integration

package postgres

// 本文件是 P16 工作包的移植判卷：`resource_connection_tests` 投影、`mcp_credential_sources`
// 来源声明、`instance_secrets` 实例身份记录三个仓储。源文件是 SQLite 谱系
// `internal/storage` 的同名 `*_store_test.go`：测试名、断言、中文验收点与逆改点注释
// 原样保留；只替换「库怎么来」（`openTempDB` → 隔离 schema 的 `portTestDB`）与
// 「原始 SQL 的方言」（`db.read` / `db.write` → 读写合一的 `db.pool`、占位符 `?` →
// `$n`、时间列按 BIGINT Unix 毫秒）。
// Ported from the SQLite lineage `*_store_test.go` files verbatim: names, assertions and
// acceptance notes stay; only the fixture (isolated schema) and the SQL dialect differ.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// ── resource_connection_tests（源：internal/storage/connection_test_store_test.go） ──
// 本表是每资源只保留最后一次结果的投影：upsert 覆盖旧值、非法形状在 SQL 之前拒绝；
// 历史明细不归本表——那是 `management_operations`。
// These tests pin the last-test projection: one row per resource, upsert overwrites,
// invalid shapes rejected before touching SQL.

// TestConnectionTestStore_UpsertKeepsLastOnly：同一资源二次测试覆盖首条（本表按定义
// 只回答「上回测得怎样」），且不新增第二行；不同 UID 互不干扰并按 UID 升序返回。
// 中文验收点：只留最后一条，不追加历史。
func TestConnectionTestStore_UpsertKeepsLastOnly(t *testing.T) {
	db := portTestDB(t)
	ctx := context.Background()
	store := NewConnectionTestStore(db)

	first := core.ResourceConnectionTest{
		Kind:        core.ResourceNotifyChannel,
		ResourceUID: testResourceUID('A'),
		TestedAt:    opNow,
		Status:      core.ConnectionTestSuccess,
		Detail:      "通知渠道测试消息已发送",
		OperationID: "op-1759999999999-aaaaaa",
	}
	require.NoError(t, store.RecordConnectionTest(ctx, first))

	second := core.ResourceConnectionTest{
		Kind:        core.ResourceNotifyChannel,
		ResourceUID: testResourceUID('A'),
		TestedAt:    opNow.Add(2 * time.Minute),
		Status:      core.ConnectionTestFailed,
		Detail:      "对端返回 400",
		OperationID: "op-1760000000119-bbbbbb",
	}
	require.NoError(t, store.RecordConnectionTest(ctx, second))

	other := core.ResourceConnectionTest{
		Kind:        core.ResourceNotifyChannel,
		ResourceUID: testResourceUID('B'),
		TestedAt:    opNow,
		Status:      core.ConnectionTestUnknown,
		Detail:      "请求已发出但结果不明",
		OperationID: "op-1759999999999-cccccc",
	}
	require.NoError(t, store.RecordConnectionTest(ctx, other))

	rows, err := store.ListConnectionTests(ctx, core.ResourceNotifyChannel)
	require.NoError(t, err)
	require.Len(t, rows, 2, "同资源重测必须覆盖，不得追加历史行")
	require.Equal(t, testResourceUID('A'), rows[0].ResourceUID)
	require.Equal(t, core.ConnectionTestFailed, rows[0].Status)
	require.Equal(t, second.Detail, rows[0].Detail)
	require.Equal(t, second.OperationID, rows[0].OperationID)
	require.Equal(t, second.TestedAt, rows[0].TestedAt, "覆盖必须刷新时间戳")
	require.Equal(t, testResourceUID('B'), rows[1].ResourceUID)
	require.Equal(t, core.ConnectionTestUnknown, rows[1].Status)
}

// TestConnectionTestStore_KindIsolation：模型连接与通知渠道的 UID 即使字面相同也各自
// 成行——读取按种类过滤，不得把另一类的最后结果串进来。
// 中文验收点：两类资源不串味。
func TestConnectionTestStore_KindIsolation(t *testing.T) {
	db := portTestDB(t)
	ctx := context.Background()
	store := NewConnectionTestStore(db)

	uid := testResourceUID('C')
	require.NoError(t, store.RecordConnectionTest(ctx, core.ResourceConnectionTest{
		Kind:        core.ResourceModelConnection,
		ResourceUID: uid,
		TestedAt:    opNow,
		Status:      core.ConnectionTestSuccess,
		Detail:      "模型调用成功（12 ms）",
		OperationID: "op-1759999999999-dddddd",
	}))

	models, err := store.ListConnectionTests(ctx, core.ResourceModelConnection)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, core.ResourceModelConnection, models[0].Kind)

	channels, err := store.ListConnectionTests(ctx, core.ResourceNotifyChannel)
	require.NoError(t, err)
	require.Empty(t, channels, "另一类资源的读取不得串行")
}

// TestConnectionTestStore_RejectsInvalidShapes：非法种类、空 UID、非法状态在 SQL 之前
// 拒绝；两者都不落行。
// 中文验收点：脏数据不进表。
func TestConnectionTestStore_RejectsInvalidShapes(t *testing.T) {
	db := portTestDB(t)
	ctx := context.Background()
	store := NewConnectionTestStore(db)

	cases := []struct {
		name string
		rec  core.ResourceConnectionTest
	}{
		{"非法种类", core.ResourceConnectionTest{
			Kind: core.ResourceSkill, ResourceUID: testResourceUID('D'), TestedAt: opNow,
			Status: core.ConnectionTestSuccess,
		}},
		{"空 UID", core.ResourceConnectionTest{
			Kind: core.ResourceNotifyChannel, TestedAt: opNow, Status: core.ConnectionTestSuccess,
		}},
		{"非法状态", core.ResourceConnectionTest{
			Kind: core.ResourceNotifyChannel, ResourceUID: testResourceUID('D'), TestedAt: opNow,
			Status: "ok",
		}},
	}
	for _, tc := range cases {
		require.Error(t, store.RecordConnectionTest(ctx, tc.rec), tc.name)
	}

	rows, err := store.ListConnectionTests(ctx, core.ResourceNotifyChannel)
	require.NoError(t, err)
	require.Empty(t, rows, "被拒绝的记录不得落行")

	_, err = store.ListConnectionTests(ctx, core.ResourceSkill)
	require.Error(t, err, "技能不是连接测试的资源种类")
}

// ── mcp_credential_sources（源：internal/storage/mcp_credential_source_store_test.go） ──
// 来源必须显式声明、首写修订号 1 之后每次成功写入 +1、补记只填**没有声明**的槽位且
// 不改显式声明。来源进表的意义是撤销不隐式回落进程环境——本文件守住"声明不被补记
// 抹掉"这条；本表只记模式与槽位名，**不存任何值**。
// These tests pin the per-placeholder credential sources: explicit declarations, the
// revision bump, and a backfill that never rewrites an explicit choice. Mode and slot
// names only — never a value.

// sourceStoreUID 是测试用的 MCP server 资源 UID。
// sourceStoreUID is the MCP server resource uid used by the cases below.
const sourceStoreUID = core.ResourceUID("rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C")

// newSourceStore 给每个用例一个独立的真库（移植后为隔离 schema 句柄）。
// newSourceStore gives every case its own migrated isolated schema handle.
func newSourceStore(t *testing.T) (*MCPCredentialSourceStore, *DB) {
	t.Helper()
	db := portTestDB(t)
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
	}, opNow))

	first, ok, err := store.Get(ctx, sourceStoreUID, "GITHUB_PERSONAL_ACCESS_TOKEN")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, first.Revision)
	require.Equal(t, core.CredentialModeEnv, first.Mode)
	require.Equal(t, "op-first", first.OperationID)
	require.Equal(t, opNow.UnixMilli(), first.UpdatedAt)

	later := opNow.Add(2 * time.Minute)
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
			require.Error(t, store.Upsert(ctx, source, opNow), "%s 非法必须拒绝", name)
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
	}, opNow))

	report, err := store.BackfillEnvSources(ctx, sourceStoreUID,
		[]string{"GITHUB_PERSONAL_ACCESS_TOKEN", "BRAVE_API_KEY"}, "system.backfill", opNow)
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
		[]string{"GITHUB_PERSONAL_ACCESS_TOKEN", "BRAVE_API_KEY"}, "system.backfill", opNow)
	require.NoError(t, err)
	require.Equal(t, 0, again.FilledCount(), "补记是幂等的")
	require.Equal(t, []string{"BRAVE_API_KEY"}, again.Skipped, "重跑时既有 env 行走 skipped")
}

// TestMCP_BackfillReportsMissingAndDrift —— 设计 §2.16.1 ②：报告必须按槽位分四类列出，
// **不得**只报一个总数。缺行 → filled；既有 env（导入带过来的）→ skipped；文件声明了
// 槽位而库里是 managed → config_drift；conflict 是闭集外模式的防御分支，schema CHECK
// 下不可达，正常数据里必须为空。移植后判据与 SQLite 侧逐条一致。
// 逆改点：报告退化成只返回 filled 计数 → skipped / config_drift 断言红；把 managed
// 既有行并进 skipped → 配置漂移被掩盖，config_drift 断言红。
// TestMCP_BackfillReportsMissingAndDrift pins the four-way per-slot report.
func TestMCP_BackfillReportsMissingAndDrift(t *testing.T) {
	store, _ := newSourceStore(t)
	ctx := context.Background()

	require.NoError(t, store.Upsert(ctx, core.MCPCredentialSource{
		ServerUID:   sourceStoreUID,
		BindingKey:  "ALREADY_ENV",
		Mode:        core.CredentialModeEnv,
		OperationID: "migration.import.v6",
	}, opNow.Add(-24*time.Hour)))
	require.NoError(t, store.Upsert(ctx, core.MCPCredentialSource{
		ServerUID:   sourceStoreUID,
		BindingKey:  "MANAGED_SLOT",
		Mode:        core.CredentialModeManaged,
		OperationID: "op-managed",
	}, opNow))

	report, err := store.BackfillEnvSources(ctx, sourceStoreUID,
		[]string{"ALREADY_ENV", "MANAGED_SLOT", "FRESH_SLOT"}, "system.backfill", opNow)
	require.NoError(t, err)
	require.Equal(t, []string{"FRESH_SLOT"}, report.Filled, "缺行才补")
	require.Equal(t, []string{"ALREADY_ENV"}, report.Skipped, "既有 env 行不动")
	require.Equal(t, []string{"MANAGED_SLOT"}, report.ConfigDrift, "配置漂移要单独可见")
	require.Empty(t, report.Conflict, "闭集外模式在 schema CHECK 下不可达")
	require.Equal(t, 1, report.FilledCount())
	require.Equal(t, 3,
		len(report.Filled)+len(report.Skipped)+len(report.Conflict)+len(report.ConfigDrift),
		"每个槽位恰好落一个分类——不得漏报")
}

// TestMCP_BackfillDoesNotOverwriteMigratedEnvSource —— 设计 §2.16.1 ①：导入带进来的
// env 来源行（含迁移期的 operation_id 与 updated_at）逐字段保留，补记不得 UPDATE。
// 逆改点：把 INSERT ... ON CONFLICT DO NOTHING 改成 upsert → operation_id / revision
// 被改写，本用例红。
// TestMCP_BackfillDoesNotOverwriteMigratedEnvSource keeps migrated env rows verbatim.
func TestMCP_BackfillDoesNotOverwriteMigratedEnvSource(t *testing.T) {
	store, _ := newSourceStore(t)
	ctx := context.Background()
	migratedAt := opNow.Add(-24 * time.Hour)
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
		[]string{"MIGRATED_TOKEN"}, "system.backfill", opNow)
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

	_, err := store.BackfillEnvSources(ctx, "rs_bogus", []string{"TOKEN"}, "op-x", opNow)
	require.Error(t, err)

	_, err = store.BackfillEnvSources(ctx, sourceStoreUID, []string{"TOKEN"}, "", opNow)
	require.Error(t, err)
	require.Contains(t, err.Error(), "operation_id 不能为空")

	// PG 下每条 Exec 各自自动提交：错误前的补记已落库，与 SQLite 侧逐条写入的
	// 可观察结果一致（断言不变）。
	// Each Exec autocommits on PG, so rows before the error persist exactly as they did
	// row-by-row on SQLite; the assertions stay unchanged.
	report, err := store.BackfillEnvSources(ctx, sourceStoreUID,
		[]string{"BRAVE_API_KEY", "BAD KEY", "FETCH_TOKEN"}, "op-x", opNow)
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
		}, opNow))
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
// 移植适配：PG 句柄读写共用一个池（源 SQLite 侧是 read / write 两个），关池即两侧
// 都造出真实故障；子用例名与断言（错误前缀）保持原样。
// Adaptation: the PG handle shares one pool for reads and writes, so closing it forces
// both failures; subtest names and the prefix assertions stay verbatim.
func TestMCPCredentialSourceStore_ErrorPathsReportContext(t *testing.T) {
	ctx := context.Background()

	t.Run("read", func(t *testing.T) {
		store, db := newSourceStore(t)
		closePort16Pool(t, db)
		_, _, err := store.Get(ctx, sourceStoreUID, "TOKEN")
		require.Error(t, err)
		require.Contains(t, err.Error(), "读取 MCP")
		_, err = store.List(ctx, sourceStoreUID)
		require.Error(t, err)
		require.Contains(t, err.Error(), "列出 MCP")
	})

	t.Run("write", func(t *testing.T) {
		store, db := newSourceStore(t)
		closePort16Pool(t, db)
		err := store.Upsert(ctx, core.MCPCredentialSource{
			ServerUID:   sourceStoreUID,
			BindingKey:  "TOKEN",
			Mode:        core.CredentialModeEnv,
			OperationID: "op-x",
		}, opNow)
		require.Error(t, err)
		require.Contains(t, err.Error(), "写入 MCP")

		_, err = store.BackfillEnvSources(ctx, sourceStoreUID, []string{"TOKEN"}, "op-x", opNow)
		require.Error(t, err)
		require.Contains(t, err.Error(), "补记 MCP")
	})
}

// ── instance_secrets（源：internal/storage/instance_secrets_store_test.go） ──
// scope 只放行 master_key、主键冲突是"别人先赢"而不是覆盖、计数只算仍带密文的版本、
// adopt 用的枚举按 (凭据 ID, 版本号) 稳定升序。判定逻辑（三项联立）在 app 层判，这里
// 只判 SQL 面；指纹与密钥路径语义按原用例读回，密文列是 bytea。
// These tests pin the instance identity record and the sealed-credential scan: the only
// legal scope, never-overwrite on conflict, and a stable enumeration order.

// instanceTestUID 是测试用的资源 UID（26 位 Crockford 载荷、无易混字符）。
// instanceTestUID is the resource uid used to build the seeded credential owners.
const instanceTestUID = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"

// newInstanceSecretsStore 给每个用例一个独立的真库（移植后为隔离 schema 句柄）。
// newInstanceSecretsStore gives every case its own migrated isolated schema handle.
func newInstanceSecretsStore(t *testing.T) (*InstanceSecretsStore, *DB) {
	t.Helper()
	db := portTestDB(t)
	return NewInstanceSecretsStore(db), db
}

// recordFixture 造一条完整的初始化记录。
// recordFixture builds a complete initialization record.
func recordFixture() InstanceSecretRecord {
	return InstanceSecretRecord{
		Scope:         "master_key",
		KeyID:         "mk-1",
		Fingerprint:   "0123456789abcdef",
		InitializedAt: opNow,
		OperationID:   "op-init",
	}
}

// TestInstanceSecretsStore_LoadMissIsAnAnswer —— 未初始化返回 exists=false（不是错误）；
// scope 只接受 master_key，其它值在读写两侧都拒绝。
// 逆改点：去掉 scope 校验 → 迁移版本号被当加密身份写进同表，本用例红。
func TestInstanceSecretsStore_LoadMissIsAnAnswer(t *testing.T) {
	store, _ := newInstanceSecretsStore(t)
	ctx := context.Background()

	record, exists, err := store.Load(ctx, "master_key")
	require.NoError(t, err)
	require.False(t, exists)
	require.Equal(t, InstanceSecretRecord{}, record)

	_, _, err = store.Load(ctx, "schema_version")
	require.Error(t, err)
	require.Contains(t, err.Error(), "只允许 master_key")

	err = store.Insert(ctx, InstanceSecretRecord{
		Scope:         "schema_version",
		KeyID:         "mk-1",
		Fingerprint:   "0123456789abcdef",
		InitializedAt: opNow,
		OperationID:   "op-init",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "只允许 master_key")
}

// TestInstanceSecretsStore_InsertShapeAndRoundTrip —— 关键字段缺一即拒（不落半截行）；
// 合法写入后按 UTC 毫秒读回，指纹与操作标识一字不差。
func TestInstanceSecretsStore_InsertShapeAndRoundTrip(t *testing.T) {
	store, _ := newInstanceSecretsStore(t)
	ctx := context.Background()

	cases := map[string]func(r *InstanceSecretRecord){
		"key_id":         func(r *InstanceSecretRecord) { r.KeyID = "" },
		"fingerprint":    func(r *InstanceSecretRecord) { r.Fingerprint = "" },
		"operation_id":   func(r *InstanceSecretRecord) { r.OperationID = "" },
		"initialized_at": func(r *InstanceSecretRecord) { r.InitializedAt = time.Time{} },
	}
	for name, mutate := range cases {
		t.Run("reject_"+name, func(t *testing.T) {
			record := recordFixture()
			mutate(&record)
			require.Error(t, store.Insert(ctx, record), "%s 必填", name)
		})
	}

	record := recordFixture()
	require.NoError(t, store.Insert(ctx, record))

	loaded, exists, err := store.Load(ctx, "master_key")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, "master_key", loaded.Scope)
	require.Equal(t, "mk-1", loaded.KeyID)
	require.Equal(t, "0123456789abcdef", loaded.Fingerprint)
	require.Equal(t, "op-init", loaded.OperationID)
	require.True(t, loaded.InitializedAt.Equal(opNow))
	require.Equal(t, time.UTC, loaded.InitializedAt.Location())
}

// TestInstanceSecretsStore_ConflictNeverOverwrites —— 主键冲突映射为
// `ErrInstanceSecretExists`，既有行原样保留：记录是"谁先写谁算"的胜负凭据，覆盖等于
// 允许第二把主密钥接管第一把。
// 逆改点：改成 `INSERT OR REPLACE` / upsert → 第二把密钥悄悄接管，本用例红。
func TestInstanceSecretsStore_ConflictNeverOverwrites(t *testing.T) {
	store, _ := newInstanceSecretsStore(t)
	ctx := context.Background()
	require.NoError(t, store.Insert(ctx, recordFixture()))

	intruder := recordFixture()
	intruder.KeyID = "mk-2"
	intruder.Fingerprint = "fedcba9876543210"
	intruder.OperationID = "op-intruder"
	err := store.Insert(ctx, intruder)
	require.ErrorIs(t, err, ErrInstanceSecretExists)
	require.Contains(t, err.Error(), "master_key")

	loaded, exists, err := store.Load(ctx, "master_key")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, "mk-1", loaded.KeyID, "既有行不得被覆盖")
	require.Equal(t, "0123456789abcdef", loaded.Fingerprint)
	require.Equal(t, "op-init", loaded.OperationID)
}

// seedSealedCredentials 直插两个凭据身份与三个版本（revoked 行密文为 NULL），
// 判 adopt 用的计数与逐条枚举。仅用于造出仓储 API 造不出的既成状态。
// 移植适配：原始 SQL 一律改 `db.pool` 与 `$n` 占位符；时间列为 BIGINT Unix 毫秒；
// 密文列是 bytea（nil 参数按 NULL 落库）。
// Direct-SQL seeding (the fixture state the repository API cannot produce); placeholders
// and the handle are the only dialect changes.
func seedSealedCredentials(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	_, err := db.pool.ExecContext(ctx,
		`INSERT INTO management_operations
		   (operation_id, action, target_kind, target_id, actor_subject, auth_method, source,
		    status, created_at)
		 VALUES ('op-cred', 'credential.create', 'credential', 'cred_a', 'token:abc123',
		         'bearer', '127.0.0.1:5000', 'success', $1)`, opNow.UnixMilli())
	require.NoError(t, err)

	const insertCred = `INSERT INTO credentials
		(credential_id, owner_kind, owner_uid, owner_name, state, current_version,
		 revocation_epoch, key_id, created_at, updated_at, operation_id)
		VALUES ($1, $2, $3, $4, 'active', $5, 0, 'mk-1', $6, $7, 'op-cred')`
	_, err = db.pool.ExecContext(ctx, insertCred, "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		"model_connection", instanceTestUID, "openai", 2, opNow.UnixMilli(), opNow.UnixMilli())
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, insertCred, "cred_bbbbbbbbbbbbbbbbbbbbbbbbbb",
		"notify_channel", "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B", "alerts", 1,
		opNow.UnixMilli(), opNow.UnixMilli())
	require.NoError(t, err)

	const insertVersion = `INSERT INTO credential_versions
		(credential_id, version_no, state, key_id, target_binding, last4, nonce, ciphertext,
		 created_at, updated_at, operation_id)
		VALUES ($1, $2, $3, 'mk-1', 'binding-hash', '1234', $4, $5, $6, $7, 'op-cred')`
	nonce := make([]byte, 12)
	_, err = db.pool.ExecContext(ctx, insertVersion, "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa", 2,
		"active", nonce, []byte{0x02}, opNow.UnixMilli(), opNow.UnixMilli())
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, insertVersion, "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa", 1,
		"superseded", nonce, []byte{0x01}, opNow.UnixMilli(), opNow.UnixMilli())
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, insertVersion, "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa", 3,
		"revoked", nonce, nil, opNow.UnixMilli(), opNow.UnixMilli())
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, insertVersion, "cred_bbbbbbbbbbbbbbbbbbbbbbbbbb", 1,
		"active", nonce, []byte{0x03}, opNow.UnixMilli(), opNow.UnixMilli())
	require.NoError(t, err)
}

// TestInstanceSecretsStore_CountAndEnumerateSealed —— 计数只算仍带密文的版本（撤销行
// 为 NULL 自然不计入）；枚举按 (凭据 ID, 版本号) 升序且只列带密文的行，解密材料
// （AAD 段 + 密文）逐字段读回。
// 逆改点：计数带上 revoked 行 / 枚举丢排序 → adopt 试解点位不可复现，本用例红。
func TestInstanceSecretsStore_CountAndEnumerateSealed(t *testing.T) {
	store, db := newInstanceSecretsStore(t)
	ctx := context.Background()

	count, err := store.CountManagedCiphertext(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, count)

	empty, err := store.SealedCredentials(ctx)
	require.NoError(t, err)
	require.Empty(t, empty)

	seedSealedCredentials(t, db)

	count, err = store.CountManagedCiphertext(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, count, "只算仍带密文的版本，revoked 的 NULL 不计入")

	sealed, err := store.SealedCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, sealed, 3)
	require.Equal(t, []string{
		"cred_aaaaaaaaaaaaaaaaaaaaaaaaaa", "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		"cred_bbbbbbbbbbbbbbbbbbbbbbbbbb",
	},
		[]string{sealed[0].CredentialID, sealed[1].CredentialID, sealed[2].CredentialID})
	require.Equal(t, []int{1, 2, 1}, []int{sealed[0].VersionNo, sealed[1].VersionNo, sealed[2].VersionNo})

	first := sealed[0]
	require.Equal(t, "model_connection", first.OwnerKind)
	require.Equal(t, instanceTestUID, first.OwnerUID)
	require.Equal(t, "mk-1", first.KeyID)
	require.Equal(t, "binding-hash", first.TargetBinding)
	require.Len(t, first.Nonce, 12)
	require.Equal(t, []byte{0x01}, first.Ciphertext)
}

// TestInstanceSecretsStore_ErrorPathsReportContext —— 池故障带上下文前缀返回
// （判定流程据此区分"未初始化"与"读不出来"）。
// 逆改点：错误被吞成 exists=false → 读故障被误判成新建实例，本用例红。
// 移植适配：PG 句柄读写共用一个池，关池即读写两侧都造出真实故障（断言不变）。
// Adaptation: one shared pool means closing it fails both paths; assertions unchanged.
func TestInstanceSecretsStore_ErrorPathsReportContext(t *testing.T) {
	ctx := context.Background()

	t.Run("load", func(t *testing.T) {
		store, db := newInstanceSecretsStore(t)
		closePort16Pool(t, db)
		_, _, err := store.Load(ctx, "master_key")
		require.Error(t, err, "读故障不得伪装成未初始化")
		require.Contains(t, err.Error(), "读取 instance_secrets 记录")
	})

	t.Run("count_and_enumerate", func(t *testing.T) {
		store, db := newInstanceSecretsStore(t)
		closePort16Pool(t, db)
		_, err := store.CountManagedCiphertext(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "统计现存托管密文")
		_, err = store.SealedCredentials(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "列出现存托管密文")
	})

	t.Run("insert", func(t *testing.T) {
		store, db := newInstanceSecretsStore(t)
		closePort16Pool(t, db)
		err := store.Insert(ctx, recordFixture())
		require.Error(t, err)
		require.False(t, errors.Is(err, ErrInstanceSecretExists), "池故障不是主键冲突")
		require.Contains(t, err.Error(), "写入 instance_secrets 记录")
	})
}

// closePort16Pool 关掉句柄的连接池，造出真实的查询 / 写入失败。
// 源 SQLite 用例有 `closeReadPool` / `closeWritePool` 两个助手（读写分池）；PG 句柄
// 只有一个池，关它即等价于两侧同时故障，故合并为一个助手。
// closePort16Pool forces real query/write failures; the SQLite lineage has separate
// read/write helpers, while the PG handle has a single pool.
func closePort16Pool(t *testing.T, db *DB) {
	t.Helper()
	require.NoError(t, db.pool.Close())
}
