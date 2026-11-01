//go:build integration

package postgres

// 本文件是**移植用例的公共夹具**：SQLite 谱系（`internal/storage`）的既有仓储用例
// 迁到本包时共用的固定时刻、资源 UID 构造与隔离库句柄。用例本身按仓储文件分文件
// 存放（`<store>_port_test.go`），本文件只放跨文件共用、且语义与 SQLite 侧逐字一致
// 的那几件东西——不引入第二套夹具。
//
// 移植口径：断言原样保留（含逆改点注释）；只替换「库怎么来」与「原始 SQL 的方言」
// （`?` → `$n`、读写句柄合一为 `db.pool`、SQLite 的 0/1 布尔改 PG 的 false/true）。
// Shared fixtures for the store cases ported from the SQLite lineage: a fixed clock, a
// resource UID builder and one isolated migrated handle. Assertions stay verbatim.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// opNow 是移植用例的固定写库时刻（与 SQLite 侧同名常量同一时刻，断言可复算）。
// opNow is the fixed instant ported cases write with, identical to the SQLite lineage.
var opNow = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// 跨文件共用的固定标识：SQLite 谱系把它们定义在个别 `*_test.go` 里、其余文件直接
// 引用；移植后放这里，避免每个文件各造一份。
// Cross-file constants kept in one place, mirroring the SQLite lineage.
const (
	testAgentUID      core.AgentUID = "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	testTargetBinding string        = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// testResourceUID 造一个合法的资源 UID（末位区分同一用例里的多个资源），与
// SQLite 侧 `internal/storage/run_store_test.go` 的同名构造逐字一致。
// testResourceUID builds a valid resource uid varying only in its last byte.
func testResourceUID(last byte) core.ResourceUID {
	return core.ResourceUID("rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3" + string(last))
}

// testCredentialID 造一个合法的凭据 ID（`cred_` + 26 位 Crockford 载荷，末位区分用例）。
// testCredentialID builds a valid credential id.
func testCredentialID(last byte) string {
	return "cred_01J8Z6YQ4M2S3T7V9W0X1Y2Z3" + string(last)
}

// seedOperation 建一行管理操作（版本行的 operation_id 外键要求它先存在；
// 真实路径由基线 / US-12 的 BeginOperation 预写）。
// seedOperation inserts the management operation row the resource foreign keys require.
func seedOperation(t *testing.T, db *DB, operationID string) {
	t.Helper()
	require.NoError(t, NewManagementStore(db).BeginOperation(context.Background(), core.OperationRecord{
		OperationID:  operationID,
		Action:       "system.baseline_v2",
		TargetKind:   "system",
		TargetID:     "system",
		ActorSubject: core.ActorSubjectMigration,
		AuthMethod:   core.AuthMethodMigration,
		Source:       "migration",
		CreatedAt:    opNow,
	}))
}

// portTestDB 给每个移植用例一个已迁移的隔离 schema 句柄；schema 随 `t.Cleanup`
// 回收，用例之间互不可见（移植用例的隔离纪律与 SQLite 侧「不共享样例库」相同）。
// portTestDB returns a migrated handle on a throwaway isolated schema.
func portTestDB(t *testing.T) *DB {
	t.Helper()
	return knTestDB(t)
}
