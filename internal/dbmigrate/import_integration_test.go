//go:build integration

package dbmigrate

// 本文件判卷 SQLite v6 → PostgreSQL v1 的**单事务导入**（颗粒度文档 §2.11.2 / §2.11.4，
// C-12 / C-30）：空库断言（含锁内重断言）、失败整体回滚、不写 first_write_at、
// 列集合不符拒绝、生成列由 PG 重算、未知 format_version 中止。
// 全部用例走**真实 SQLite 夹具 + 临时 PG**（`docs/contracts/schema.sql` 建齐 33 张表，
// 经真实 `Exporter` 产出 dump），不用手写假 dump。
// Every case runs a real SQLite fixture through the real Exporter into a throwaway
// PostgreSQL schema; no hand-written fake dumps.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/dbmigrate/testutil"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// importCtx 是本文件用例的公共上下文（包内其它文件的 `ctx` 属 `dbmigrate_test` 包，
// 两个测试包各自独立，不共享变量）。
var importCtx = context.Background()

// TestImport_RequiresEmptyBusinessTables 导入只接受空库：二次导入必须被拒，且被拒的
// 导入**不得**写入任何行（不覆盖、不合并）。
// 逆改点：跳过空表断言 → 二次导入成功或产生重复行，断言红。
func TestImport_RequiresEmptyBusinessTables(t *testing.T) {
	dir, manifest := fixtureDumpWithManifest(t)
	db := pgtest.NewTestDB(t)

	report, err := NewImporter(db, dir, manifest).Import(importCtx)
	require.NoError(t, err)
	require.False(t, report.Failed())
	require.Equal(t, int64(1), report.Rows["messages"], "报告行数按表给出")
	require.Equal(t, int64(1), report.Rows["sessions"])

	_, err = NewImporter(db, dir, manifest).Import(importCtx)
	require.ErrorIs(t, err, ErrTargetNotEmpty, "目标库有业务数据时拒绝导入")

	var messages int
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM messages`).Scan(&messages))
	require.Equal(t, 1, messages, "被拒的导入不得写入任何行")
}

// TestImport_EmptyCheckInsideLock 空表断言必须在 advisory 锁内重新做一次：事务外的预检
// 只用于尽早报错，两个导入同时通过预检时，后到者必须被锁内断言拒绝。
//
// 造窗口的办法：先让另一个事务**持有**同一把 advisory 锁（此时业务表仍为空，导入的
// 预检必定通过），导入阻塞在锁上（用 `pg_locks` 的未授予行核实它确实在等锁），
// 再让持锁事务插入一行并提交。导入随后拿到锁，必须在锁内看到非空并拒绝。
// 逆改点：只在事务外预检、锁内不重断言 → 导入会照常写入（sessions 变 2 行），断言红；
// 事务内根本不取锁 → `waitForAdvisoryWait` 直接失败。
func TestImport_EmptyCheckInsideLock(t *testing.T) {
	dir, manifest := fixtureDumpWithManifest(t)
	db := pgtest.NewTestDB(t)

	locked := make(chan struct{})
	release := make(chan struct{}, 1)
	t.Cleanup(func() {
		// 失败路径（如等锁超时）也要放行持锁事务，避免 cleanup 阶段还能回收 schema。
		select {
		case release <- struct{}{}:
		default:
		}
	})
	holderDone := make(chan error, 1)
	go func() {
		_, err := db.Tx(importCtx, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, importAdvisoryKey); err != nil {
				return err
			}
			close(locked)
			<-release
			// 锁内写入：导入的锁外预检看到的是空表，锁内断言必须看到这一行。
			_, err := tx.ExecContext(ctx, `INSERT INTO sessions
				(session_id, profile_name, channel, user_id, created_at, last_active_at)
				VALUES ('holder', 'demo', 'cli', 'u1', 1, 1)`)
			return err
		})
		holderDone <- err
	}()

	<-locked
	importErr := make(chan error, 1)
	go func() {
		_, err := NewImporter(db, dir, manifest).Import(importCtx)
		importErr <- err
	}()

	waitForAdvisoryWait(t, db)
	release <- struct{}{}
	require.NoError(t, <-holderDone, "持锁事务必须提交成功")

	err := <-importErr
	require.ErrorIs(t, err, ErrTargetNotEmpty, "后到者必须在锁内重新断言并被拒绝")

	var sessions int
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM sessions`).Scan(&sessions))
	require.Equal(t, 1, sessions, "被拒的导入不得写入任何行（唯一一行来自持锁事务）")
}

// TestImport_DoesNotSetFirstWrite 导入本身不是切换事件：既不写 cutover 行，也不写
// first_write_at——后者归切换后的首个业务事务，提前写会让回退窗口直接失效。
// 逆改点：导入尾部补一条 `UPDATE storage_cutover SET first_write_at=...` 或插 cutover 行 → 断言红。
func TestImport_DoesNotSetFirstWrite(t *testing.T) {
	dir, manifest := fixtureDumpWithManifest(t)
	db := pgtest.NewTestDB(t)

	_, err := NewImporter(db, dir, manifest).Import(importCtx)
	require.NoError(t, err)

	var cutoverRows, firstWrites int
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM storage_cutover`).Scan(&cutoverRows))
	require.Zero(t, cutoverRows, "导入不得写 cutover 行")
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT count(*) FROM storage_cutover WHERE first_write_at IS NOT NULL`).Scan(&firstWrites))
	require.Zero(t, firstWrites, "first_write_at 由切换后首个业务事务写，导入不得代写")
}

// TestImport_RejectsColumnMismatch 列集合以清单为**唯一**依据：JSONL 多一列、少一列都拒绝，
// **不**按位置对齐、**不**补 NULL。
// 逆改点：按位置对齐或忽略多余键 → 两个子例都通过（用例红在 ErrorIs 断言上）。
func TestImport_RejectsColumnMismatch(t *testing.T) {
	dir, manifest := fixtureDumpWithManifest(t)
	db := pgtest.NewTestDB(t)

	// ① 清单少声明一列，JSONL 里仍有 → 多列。
	short := tamperManifestTable(t, manifest, "messages", func(table *TableManifest) {
		table.Columns = withoutColumn(table.Columns, "reasoning_content")
	})
	_, err := NewImporter(db, dir, short).Import(importCtx)
	require.ErrorIs(t, err, ErrInvalidValue)
	require.Contains(t, err.Error(), "多列", "JSONL 里多出来的列不得忽略")
	require.Contains(t, err.Error(), "messages", "错误必须指出表名")

	// ② 清单多声明一列，JSONL 里没有 → 缺列（不得补 NULL）。
	extra := tamperManifestTable(t, manifest, "messages", func(table *TableManifest) {
		table.Columns = append(table.Columns, ColumnMeta{
			Name: "ghost_col", SQLiteDecl: "TEXT", PGType: pgTypeText, Nullable: true, Encoding: TagText,
		})
	})
	_, err = NewImporter(db, dir, extra).Import(importCtx)
	require.ErrorIs(t, err, ErrInvalidValue)
	require.Contains(t, err.Error(), "缺列", "JSONL 里缺的列不得按 NULL 补")

	var messages int
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM messages`).Scan(&messages))
	require.Zero(t, messages, "列集合错误的导入不得写入任何行")
}

// TestImport_RecomputedGeneratedColumns 生成列（`messages.tsv`）**不得**出现在清单里、
// **不得**被赋值，由 PostgreSQL 按表达式重算；重算结果必须立即可用于全文检索。
// 逆改点：允许清单声明生成列 → 第一个子例导入成功（ErrorIs 红）；导入后 tsv 为空
// （把生成列当普通列且不赋值，或改表达式）→ 第二个子例的全文检索断言红。
func TestImport_RecomputedGeneratedColumns(t *testing.T) {
	dir, manifest := fixtureDumpWithManifest(t)
	db := pgtest.NewTestDB(t)

	withTSV := tamperManifestTable(t, manifest, "messages", func(table *TableManifest) {
		table.Columns = append(table.Columns, ColumnMeta{
			Name: "tsv", SQLiteDecl: "TEXT", PGType: pgTypeText, Nullable: true, Encoding: TagText,
		})
	})
	_, err := NewImporter(db, dir, withTSV).Import(importCtx)
	require.ErrorIs(t, err, ErrInvalidValue)
	require.Contains(t, err.Error(), "生成列", "生成列只能由目标库重算")

	_, err = NewImporter(db, dir, manifest).Import(importCtx)
	require.NoError(t, err)

	var tsv string
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT tsv::text FROM messages WHERE id = 1`).Scan(&tsv))
	require.Contains(t, tsv, "rollback", "生成列必须由 PG 重算，导入不赋值")
}

// TestImport_RejectsUnknownFormatVersion 不认识的 `format_version` 一律中止：**不**按
// "看起来像"的字段猜测解析。
// 逆改点：删掉 `Manifest.Validate` 的版本判定 → 0 与 2 都能导进去（断言红）。
func TestImport_RejectsUnknownFormatVersion(t *testing.T) {
	dir, manifest := fixtureDumpWithManifest(t)
	db := pgtest.NewTestDB(t)

	for _, version := range []int{0, ManifestFormatVersion + 1} {
		bad := manifest
		bad.FormatVersion = version
		_, err := NewImporter(db, dir, bad).Import(importCtx)
		require.ErrorIs(t, err, ErrUnsupportedFormat,
			"format_version=%d 必须中止，不得猜测", version)
	}

	var sessions int
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM sessions`).Scan(&sessions))
	require.Zero(t, sessions, "被拒的导入不得写入任何行")
}

// TestMigration_FailedImportLeavesEmptyTables 数据事务中途失败必须整体回滚为空业务表
// （schema 保留，可修复 dump 后重试）。夹具里 `sessions` 已先插入成功，`messages` 的第二行
// 指向不存在的会话，外键拒绝 → 整笔回滚。
// 逆改点：逐表提交 / 分段导入 → 失败后 sessions 留下 1 行，断言红。
func TestMigration_FailedImportLeavesEmptyTables(t *testing.T) {
	dump, manifest := fixtureDumpWithOrphanMessage(t)
	db := pgtest.NewTestDB(t)

	_, err := NewImporter(db, dump, manifest).Import(importCtx)
	require.Error(t, err)

	var msgs, sessions int
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM messages`).Scan(&msgs))
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM sessions`).Scan(&sessions))
	require.Zero(t, msgs+sessions, "失败导入必须整体回滚为空业务表")
}

// TestImport_AdvancesIdentitySequence 非空表的 identity 序列必须推到该列的 max：
// 导入后再插一行不得撞主键。同时守住「按目录查 identity 列」的口径——同一批里
// `management_operations` 的主键是 `operation_id text`、没有 `id` 列，按名字写死
// `pg_get_serial_sequence(t, 'id')` 会在那里直接报错（42703），整笔导入失败。
// 逆改点：删掉 `resetIdentity` 调用 → 新行拿 id=1 撞主键，断言红；改回按 `id` 写死 →
// 本用例与 `TestImport_RequiresEmptyBusinessTables` 一起红在"column \"id\" does not exist"。
func TestImport_AdvancesIdentitySequence(t *testing.T) {
	dir, manifest := fixtureDumpWithManifest(t)
	db := pgtest.NewTestDB(t)

	_, err := NewImporter(db, dir, manifest).Import(importCtx)
	require.NoError(t, err)

	var id int64
	require.NoError(t, db.QueryRowContext(importCtx, `INSERT INTO messages
		(session_id, seq, role, content, created_at)
		VALUES ('s1', 2, 'user', 'after import', 1001)
		RETURNING id`).Scan(&id))
	require.Equal(t, int64(2), id, "序列必须推进到 max(id)，新行从 max+1 起")
}

// fixtureDumpWithManifest 造一份**真实** dump：契约脚本建齐 33 张表的 SQLite v6 源库，
// 写入跨表行，经真实 `Exporter` 导出。
func fixtureDumpWithManifest(t *testing.T) (string, Manifest) {
	t.Helper()
	source := migrationFixture(t)
	seedMigrationRows(t, source)
	return exportFixture(t, source)
}

// fixtureDumpWithOrphanMessage 造一份**外键违例**的 dump：会话 's1' 合法（导入时会先
// 成功插入），另有两条消息，第二条的 session_id 指向不存在的会话。
func fixtureDumpWithOrphanMessage(t *testing.T) (string, Manifest) {
	t.Helper()
	source := migrationFixture(t)
	// 契约脚本把 `foreign_keys` 打开；孤儿行要在**源库**里存得住（真实迁移现场就是
	// "旧库里有历史脏数据"），所以夹具侧临时关掉外键——PG 侧仍会如实拒绝。
	execFixture(t, source, `PRAGMA foreign_keys = OFF`)
	execFixture(t, source, `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at)
		VALUES ('s1', 'demo', 'cli', 'u1', 'active', 2, 1000, 1000)`)
	execFixture(t, source, `INSERT INTO messages (id, session_id, seq, role, content, created_at)
		VALUES (1, 's1', 1, 'user', 'rollback steps', 1000)`)
	execFixture(t, source, `INSERT INTO messages (id, session_id, seq, role, content, created_at)
		VALUES (2, 'missing-session', 1, 'user', 'orphan', 1000)`)
	return exportFixture(t, source)
}

// migrationFixture 用冻结的 SQLite v6 契约脚本建库（`testutil.SQLiteFixture` 已写好
// `schema_version=6`；契约里的建表语句全是 `IF NOT EXISTS`，不会撞车）。
func migrationFixture(t *testing.T) *sql.DB {
	t.Helper()
	db := testutil.SQLiteFixture(t)
	// 单连接：契约脚本里的 `PRAGMA foreign_keys = ON` 是**连接级**设置，多连接时
	// 后续语句可能落到没有该设置的连接上，夹具行为会随调度漂移。
	db.SetMaxOpenConns(1)

	script, err := os.ReadFile(filepath.Join("..", "storage", "schema.sql"))
	require.NoError(t, err, "必须读得到随二进制嵌入的 v6 契约脚本")
	_, err = db.Exec(string(script))
	require.NoError(t, err, "契约脚本必须在夹具库上整脚本执行成功")
	return db
}

// seedMigrationRows 写入跨表行：一张会话、一条消息、一条管理操作（含 JSON 列）。
func seedMigrationRows(t *testing.T, db *sql.DB) {
	t.Helper()
	execFixture(t, db, `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at)
		VALUES ('s1', 'demo', 'cli', 'u1', 'active', 1, 1000, 1000)`)
	execFixture(t, db, `INSERT INTO messages (id, session_id, seq, role, content, created_at)
		VALUES (1, 's1', 1, 'user', 'rollback steps for the deployment', 1000)`)
	execFixture(t, db, `INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source,
		 status, detail_json, created_at)
		VALUES ('op-1000-abcdef', 'agent.content.update', 'agent', 'demo', 'local_trust',
		 'local_trust', 'cli', 'pending', '{"k":"v"}', 1000)`)
}

// exportFixture 用真实导出器产出 dump，返回目录与清单。
func exportFixture(t *testing.T, source *sql.DB) (string, Manifest) {
	t.Helper()
	dir := t.TempDir()
	manifest, err := NewExporter(source, SourceSchemaVersion).Export(importCtx, dir)
	require.NoError(t, err)
	return dir, manifest
}

// execFixture 在夹具上补语句，失败即终止用例。
func execFixture(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	_, err := db.Exec(statement)
	require.NoErrorf(t, err, "夹具执行失败：%s", statement)
}

// tamperManifestTable 返回**改了指定表**的清单副本（原清单不动，子例之间互不影响）。
func tamperManifestTable(t *testing.T, manifest Manifest, table string, mutate func(*TableManifest)) Manifest {
	t.Helper()
	out := manifest
	out.Tables = append([]TableManifest(nil), manifest.Tables...)
	found := false
	for i := range out.Tables {
		if out.Tables[i].Name == table {
			mutate(&out.Tables[i])
			found = true
		}
	}
	require.True(t, found, "清单里没有表 %q", table)
	return out
}

// withoutColumn 返回去掉指定列的列元数据副本（列序保持）。
func withoutColumn(columns []ColumnMeta, drop string) []ColumnMeta {
	out := make([]ColumnMeta, 0, len(columns))
	for _, column := range columns {
		if column.Name == drop {
			continue
		}
		out = append(out, column)
	}
	return out
}

// waitForAdvisoryWait 轮询 `pg_locks`，确认有后端**未获授予**地等在本用例那把
// advisory 锁上——这是"导入确实阻塞在锁上"的直接证据，比"最终被拒"更强：
// 不取锁的实现会在这里超时失败。
func waitForAdvisoryWait(t *testing.T, db *postgres.DB) {
	t.Helper()
	classid, objid := advisoryLockParts(importAdvisoryKey)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		require.NoError(t, db.QueryRowContext(importCtx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE locktype = 'advisory' AND classid::text = $1 AND objid::text = $2 AND NOT granted
		)`, classid, objid).Scan(&waiting))
		if waiting {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.FailNow(t, "10 秒内没观察到导入阻塞在 advisory 锁上",
		"导入可能没在数据事务内取锁，或锁内没有重新断言")
}

// advisoryLockParts 把 int64 锁键拆成 `pg_locks` 的 classid / objid 两半（按十进制文本，
// 避开 oid 与 bigint 之间的类型转换口径）。
func advisoryLockParts(key int64) (string, string) {
	bits := uint64(key)
	return strconv.FormatUint(bits>>32, 10), strconv.FormatUint(bits&0xffffffff, 10)
}

// TestMigration_AbortsOnDigestMismatch 守"摘要不符必须在**写库之前**中止"（C-12）：
// 只凭清单里的行数 / 文件存在就开导，被改过的 dump 会以"看起来成功"的姿态落库。
// 夹具只追加一个空格——行数不变、JSONL 仍可解析，**只有**摘要能发现它。
// 逆改点：`Import` 跳过 `verifyDigests` → 导入成功、目标表非空，四条断言同时红。
func TestMigration_AbortsOnDigestMismatch(t *testing.T) {
	dir, manifest := fixtureDumpWithManifest(t)

	path := filepath.Join(dir, "messages.jsonl")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	require.NoError(t, os.WriteFile(path, append(raw, ' '), 0o644))

	db := pgtest.NewTestDB(t)
	_, err = NewImporter(db, dir, manifest).Import(importCtx)
	require.ErrorIs(t, err, ErrDigestMismatch, "被改写的 dump 必须判成摘要不符")
	require.Contains(t, err.Error(), "messages", "错误必须点名被改写的表")

	var sessions, messages int
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM sessions`).Scan(&sessions))
	require.NoError(t, db.QueryRowContext(importCtx, `SELECT count(*) FROM messages`).Scan(&messages))
	require.Zero(t, sessions+messages, "摘要不符必须**在写库之前**中止，不得先导进去再说")
}

// TestMigration_RunsAuthorizationFieldsFilled 守 §3.4.3 的存量 Run 逐列定值：授权来源
// 一律 `admin-builtin` + `local-admin`，`key_id` / `reservation_id` 留空，而 `actor`
// **按 channel 映射**——把新旧两种身份糊成一列，事后就分不出哪些是调度发的、哪些是
// 某把调用方 Key 发的。
// 逆改点：`runActor` 对全部 channel 返回同一个值（或漏掉 admin / cli 的映射）→ 逐行
// actor 断言红；把 `key_id` 填成空串而不是 NULL → CHECK 与 NotValid 断言红。
func TestMigration_RunsAuthorizationFieldsFilled(t *testing.T) {
	source := migrationFixture(t)
	// 四种 channel 各一行：覆盖 `runActorByChannel` 的全部四条映射。
	channels := []string{"scheduler", "admin", "cli", "web"}
	for i, channel := range channels {
		execFixture(t, source, `INSERT INTO runs
			(run_id, session_id, agent, channel, user_id, trigger, status, started_at)
			VALUES ('run-`+channel+`', NULL, 'demo', '`+channel+`', 'u1', 'user', 'completed', `+
			strconv.Itoa(1000+i)+`)`)
	}

	dir, manifest := exportFixture(t, source)
	db := pgtest.NewTestDB(t)
	_, err := NewImporter(db, dir, manifest).Import(importCtx)
	require.NoError(t, err)

	wantActor := map[string]string{
		"scheduler": "scheduler", // 定时触发的执行者
		"admin":     "admin-cli", // 管理台手动补跑
		"cli":       "admin-cli", // 本机命令行
		"web":       "caller",    // 经 HTTP 进来的调用方
	}
	for _, channel := range channels {
		var callerID, authorizingKind, actor string
		var keyID, reservationID sql.NullString
		require.NoError(t, db.QueryRowContext(importCtx,
			`SELECT caller_id, authorizing_kind, key_id, reservation_id, actor
			 FROM runs WHERE run_id = $1`, "run-"+channel).
			Scan(&callerID, &authorizingKind, &keyID, &reservationID, &actor))
		require.Equal(t, "local-admin", callerID, "存量 Run 一律归属内置管理员")
		require.Equal(t, "admin-builtin", authorizingKind,
			"SQLite 期没有调用方 Key：授权来源只能是 admin-builtin")
		require.False(t, keyID.Valid, "admin-builtin 的 key_id 必须是 NULL，不得是空串")
		require.False(t, reservationID.Valid, "存量 Run 无准入预留")
		require.Equal(t, wantActor[channel], actor, "actor 必须按 channel 映射")
	}
}

// TestMigration_ManagedCredentialBindingKeyPreserved 守托管凭据的绑定键在两套谱系间
// **逐值保留**：`credentials.binding_key` 与 `credential_versions.target_binding` 是
// MCP 凭证槽位的定位键，被清空 / 改写后凭据行还在，但再也没有谁能把它挂回那个槽位
// （`owner_kind='mcp_server'` 的 CHECK 也要求 binding_key 非空）。
// 逆改点：`pgOnlyColumns` 给 credentials 加一条 `binding_key` 定值（置空 / 归一）→
// 回读值变化，两条 Equal 断言红。
func TestMigration_ManagedCredentialBindingKeyPreserved(t *testing.T) {
	source := migrationFixture(t)
	// 先落一条管理操作（credentials.operation_id 的外键父行）。
	seedMigrationRows(t, source)

	const credentialID = "cred-mcp-1"
	const bindingKey = "github-mcp-server:token"
	execFixture(t, source, `INSERT INTO credentials
		(credential_id, owner_kind, owner_uid, binding_key, owner_name, state, current_version,
		 revocation_epoch, key_id, created_at, updated_at, operation_id)
		VALUES ('`+credentialID+`', 'mcp_server', 'github-mcp-server', '`+bindingKey+`',
		 'github', 'active', 1, 0, 'key-1', 1000, 1000, 'op-1000-abcdef')`)
	execFixture(t, source, `INSERT INTO credential_versions
		(credential_id, version_no, state, key_id, target_binding, last4, nonce, ciphertext,
		 created_at, updated_at, operation_id)
		VALUES ('`+credentialID+`', 1, 'active', 'key-1', '`+bindingKey+`', '0000',
		 x'000000000000000000000000', x'00ff10ab', 1000, 1000, 'op-1000-abcdef')`)

	dir, manifest := exportFixture(t, source)
	db := pgtest.NewTestDB(t)
	_, err := NewImporter(db, dir, manifest).Import(importCtx)
	require.NoError(t, err)

	var gotBindingKey, gotTargetBinding string
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT binding_key FROM credentials WHERE credential_id = $1`, credentialID).
		Scan(&gotBindingKey))
	require.Equal(t, bindingKey, gotBindingKey, "credentials.binding_key 必须逐值保留")
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT target_binding FROM credential_versions
		 WHERE credential_id = $1 AND version_no = 1`, credentialID).
		Scan(&gotTargetBinding))
	require.Equal(t, bindingKey, gotTargetBinding, "credential_versions.target_binding 必须逐值保留")
}

// TestMigration_PreservesIDsAndAuditStatus 守颗粒度文档 §4.2 R3 的两条原值承诺：
// 稳定 ID 逐值保留（AUTOINCREMENT 主键不得因迁移重新生成），审计状态 `unknown`
// **不得**被归一成 `failed`——`unknown` 的语义是"崩溃后无法确定"，改成 failed 会让
// 审计看起来更干净却说了谎。
// 逆改点：导入按目标序列重排主键 → id 断言红；把 `unknown` 映射成 `failed` →
// unknown 计数变 0、failed 变 2。
func TestMigration_PreservesIDsAndAuditStatus(t *testing.T) {
	source := migrationFixture(t)
	execFixture(t, source, `INSERT INTO llm_calls
		(run_id, round, provider, model, duration_ms, status, created_at)
		VALUES ('run-a', 1, 'p', 'm', 5, 'unknown', 1000)`)
	execFixture(t, source, `INSERT INTO llm_calls
		(run_id, round, provider, model, duration_ms, status, error_message, created_at)
		VALUES ('run-b', 1, 'p', 'm', 5, 'failed', 'boom', 1000)`)

	dir, manifest := exportFixture(t, source)
	db := pgtest.NewTestDB(t)
	_, err := NewImporter(db, dir, manifest).Import(importCtx)
	require.NoError(t, err)

	report, err := VerifyTarget(importCtx, db, manifest)
	require.NoError(t, err)
	require.True(t, report.OK, "导入后的目标库必须与清单逐表一致：%v", report.RowDigestOK)

	var unknownID, failedID int64
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT id FROM llm_calls WHERE status = 'unknown'`).Scan(&unknownID))
	require.Equal(t, int64(1), unknownID, "稳定 ID 必须原值保留")
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT id FROM llm_calls WHERE status = 'failed'`).Scan(&failedID))
	require.Equal(t, int64(2), failedID, "稳定 ID 必须原值保留")

	var unknown, failed int64
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT count(*) FILTER (WHERE status = 'unknown'),
		        count(*) FILTER (WHERE status = 'failed') FROM llm_calls`).
		Scan(&unknown, &failed))
	require.Equal(t, int64(1), unknown, "unknown 必须原样保留，不得归一成 failed")
	require.Equal(t, int64(1), failed)
	require.Equal(t, int64(2), unknown+failed, "总行数不变——不得多产或丢弃行")
}

// TestMigration_ExecutionPolicyPreserved 守颗粒度文档 §4.3 `TestPolicy_StrictNotDowngraded`：
// 存量执行档 `strict` 与它的修订号 / 操作关联在迁移后逐值不变——把 strict 静默改回
// standard 等于在无人察觉时放松了沙箱授权。
// 逆改点：`pgOnlyColumns` 给 execution_policy 填定值、或导入按"缺省 standard"重写 →
// profile / revision / operation_id 三条断言红。
func TestMigration_ExecutionPolicyPreserved(t *testing.T) {
	source := migrationFixture(t)
	execFixture(t, source, `INSERT INTO execution_policy
		(policy_scope, profile, revision, updated_at, operation_id)
		VALUES ('global', 'strict', 7, 1000, 'op-policy-1')`)

	dir, manifest := exportFixture(t, source)
	db := pgtest.NewTestDB(t)
	_, err := NewImporter(db, dir, manifest).Import(importCtx)
	require.NoError(t, err)
	report, err := VerifyTarget(importCtx, db, manifest)
	require.NoError(t, err)
	require.True(t, report.OK, "导入后的目标库必须与清单逐表一致")

	var profile, operationID string
	var revision int64
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT profile, revision, operation_id FROM execution_policy WHERE policy_scope = 'global'`).
		Scan(&profile, &revision, &operationID))
	require.Equal(t, "strict", profile, "存量 strict 不得静默改成 standard")
	require.Equal(t, int64(7), revision, "revision 不得重置")
	require.Equal(t, "op-policy-1", operationID, "操作关联必须保留")
}

// TestMigration_NetworkExceptionsPreserveRevision 守颗粒度文档 §4.3
// `TestPolicy_NetworkExceptionsPreserveRevision`：网络例外的值与各自的修订号原样迁移——
// 修订号被重置会让"例外改过几次"与运行时的修订匹配判据一起失真。
// 逆改点：导入按新表重新编号 revision → 修订号断言红；只迁 kind 不迁 value →
// ElementsMatch 红。
func TestMigration_NetworkExceptionsPreserveRevision(t *testing.T) {
	source := migrationFixture(t)
	execFixture(t, source, `INSERT INTO network_exceptions
		(exception_id, kind, value, note, revision, created_at, operation_id)
		VALUES ('exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C', 'host', 'api.example.com', '第三方接口', 3, 1000, 'op-exc-1')`)
	execFixture(t, source, `INSERT INTO network_exceptions
		(exception_id, kind, value, note, revision, created_at, operation_id)
		VALUES ('exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z3D', 'ip_port', '10.0.0.5:443', '内网服务', 3, 1000, 'op-exc-1')`)

	dir, manifest := exportFixture(t, source)
	db := pgtest.NewTestDB(t)
	_, err := NewImporter(db, dir, manifest).Import(importCtx)
	require.NoError(t, err)
	report, err := VerifyTarget(importCtx, db, manifest)
	require.NoError(t, err)
	require.True(t, report.OK, "导入后的目标库必须与清单逐表一致")

	rows, err := db.QueryContext(importCtx, `SELECT value, kind, revision FROM network_exceptions`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	values := map[string]string{}
	revisions := map[string]int64{}
	for rows.Next() {
		var value, kind string
		var revision int64
		require.NoError(t, rows.Scan(&value, &kind, &revision))
		values[value] = kind
		revisions[value] = revision
	}
	require.NoError(t, rows.Err())
	require.ElementsMatch(t, []string{"api.example.com", "10.0.0.5:443"}, mapKeys(values))
	require.Equal(t, "host", values["api.example.com"])
	require.Equal(t, "ip_port", values["10.0.0.5:443"])
	require.Equal(t, int64(3), revisions["api.example.com"], "例外修订号不得重置")
	require.Equal(t, int64(3), revisions["10.0.0.5:443"])
}

// TestMigration_McpSourceModesWithoutValues 守颗粒度文档 §4.3 `TestMigration_EnvSourceNamesOnly`：
// MCP 凭证来源只迁"槽位名 + 模式"，env 槽位**不得**把变量值写进任何导出物；托管槽位
// 迁移后仍是托管，不允许被补记逻辑隐式降级成 env（那等于撤销后回落进程环境）。
// 逆改点：导出把 env 变量值写进 JSONL → NotContains("ghp_") 红；导入把 managed 改成
// env → 模式断言红。
func TestMigration_McpSourceModesWithoutValues(t *testing.T) {
	source := migrationFixture(t)
	const managedUID = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C"
	const envUID = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3D"
	execFixture(t, source, `INSERT INTO mcp_credential_sources
		(server_uid, binding_key, mode, revision, updated_at, operation_id)
		VALUES ('`+managedUID+`', 'GITHUB_PERSONAL_ACCESS_TOKEN', 'managed', 2, 1000, 'op-src-1')`)
	execFixture(t, source, `INSERT INTO mcp_credential_sources
		(server_uid, binding_key, mode, revision, updated_at, operation_id)
		VALUES ('`+envUID+`', 'FETCH_TOKEN', 'env', 1, 1000, 'op-src-2')`)

	dir, manifest := exportFixture(t, source)
	raw, err := os.ReadFile(filepath.Join(dir, "mcp_credential_sources.jsonl"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "GITHUB_PERSONAL_ACCESS_TOKEN", "变量名要记录")
	require.NotContains(t, string(raw), "ghp_", "env 槽位只记变量名，不得带值")

	db := pgtest.NewTestDB(t)
	_, err = NewImporter(db, dir, manifest).Import(importCtx)
	require.NoError(t, err)
	report, err := VerifyTarget(importCtx, db, manifest)
	require.NoError(t, err)
	require.True(t, report.OK, "导入后的目标库必须与清单逐表一致")

	var managedMode, envMode string
	var managedRevision, envRevision int64
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT mode, revision FROM mcp_credential_sources
		 WHERE server_uid = $1 AND binding_key = 'GITHUB_PERSONAL_ACCESS_TOKEN'`, managedUID).
		Scan(&managedMode, &managedRevision))
	require.Equal(t, "managed", managedMode, "托管槽位迁移后仍是托管")
	require.Equal(t, int64(2), managedRevision)
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT mode, revision FROM mcp_credential_sources
		 WHERE server_uid = $1 AND binding_key = 'FETCH_TOKEN'`, envUID).
		Scan(&envMode, &envRevision))
	require.Equal(t, "env", envMode, "env 槽位迁移后仍是 env")
	require.Equal(t, int64(1), envRevision)
}

// mapKeys 返回 map 的键切片（顺序无关的集合断言用）。
func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

// TestVerify_CredentialBindingsMatch 守颗粒度文档 §4.3 C-48 的"凭据绑定逐值可验"：
// 导出后被改动的 `credentials.binding_key` 必须让离线复核失败并点名表——凭据绑定是
// 解密 AAD 的一部分，绑定值漂移比少了整行更隐蔽：行数与主键集合都可能不变。
//
// 逆改点：行摘要只算主键列（不算整行）→ 改绑定值后 `RowDigestOK["credentials"]` 仍真，
// 本用例红；把 credentials 排除出行摘要 → 复核直接报通过，`ErrorIs` 红。
func TestVerify_CredentialBindingsMatch(t *testing.T) {
	const binding = "GITHUB_PERSONAL_ACCESS_TOKEN"
	source := migrationFixture(t)
	execFixture(t, source, `INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source, status, created_at)
		VALUES ('op-cred-verify-1', 'mcp.credential.save', 'mcp_server', 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C',
		        'cli:tester', 'cli', 'cli', 'success', 1000)`)
	execFixture(t, source, `INSERT INTO credentials
		(credential_id, owner_kind, owner_uid, binding_key, owner_name, state,
		 current_version, revocation_epoch, key_id, created_at, updated_at, operation_id)
		VALUES ('cred_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C', 'mcp_server', 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C',
		        '`+binding+`', 'GitHub', 'active', 1, 0, 'mk_verify_1', 1000, 1000, 'op-cred-verify-1')`)
	execFixture(t, source, `INSERT INTO credential_versions
		(credential_id, version_no, state, key_id, target_binding, last4, nonce, ciphertext,
		 created_at, updated_at, operation_id)
		VALUES ('cred_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C', 1, 'active', 'mk_verify_1',
		        'sha256:target-binding', '1234', X'000102030405060708090a0b', X'01020304',
		        1000, 1000, 'op-cred-verify-1')`)

	dir, manifest := exportFixture(t, source)
	report, err := VerifyDump(importCtx, dir, manifest)
	require.NoError(t, err)
	require.True(t, report.OK, "未被改动的 dump 必须通过")

	path := filepath.Join(dir, "credentials.jsonl")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), binding, "夹具前提：绑定值必须出现在 dump 里")
	require.NoError(t, os.WriteFile(path,
		[]byte(strings.ReplaceAll(string(raw), binding, binding+"X")), 0o600))

	tampered, err := VerifyDump(importCtx, dir, manifest)
	require.ErrorIs(t, err, ErrVerifyFailed, "绑定值被改后复核必须失败")
	require.False(t, tampered.RowDigestOK["credentials"], "行摘要必须察觉整行内容变化")
	require.Contains(t, err.Error(), "行摘要")
	require.Contains(t, err.Error(), "credentials")
}
