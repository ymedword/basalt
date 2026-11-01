//go:build integration

package dbmigrate

// 本文件判卷**迁移全链路**的无损编码与显式拒绝（颗粒度文档 §2.11.1 / §2.11.2，C-30，T065）：
// 真实 SQLite v6 夹具 → 真实 `Exporter` → 真实 `Importer` → 临时 PG 回读，不用手写假 dump。
// 四个用例守的是同一件事的两面：能存的必须逐值保真，存不下的必须**在导出编码期**拒绝，
// 而不是悄悄替换、截断或留到 PG 导入时才炸。
// These four cases pin the full migration chain rather than the JSONL layer alone: a real
// SQLite fixture goes through the real exporter and importer into a throwaway PostgreSQL
// schema, and every value is read back from the target.

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// 迁移夹具里反复用到的凭据标识（长度与 ULID 一致；库里没有格式 CHECK，取可读值）。
const (
	exactCredentialID = "cred_exact000000000000000001"
	exactOperationID  = "op-1000-exact0"
)

// exactNonce 是 12 字节 nonce，首字节 0x00、次字节 0xff——两端最容易被清洗掉的取值。
var exactNonce = []byte{0x00, 0xff, 0x10, 0, 0, 0, 0, 0, 0, 0, 0, 0}

// exactCiphertext 是任意二进制密文（含 NUL 与 0xff，用来证明 BLOB 没走文本通道）。
var exactCiphertext = []byte{0x00, 0xff, 0x10, 0xab}

// TestMigration_EncodingPreservesExactValues 整数 / 二进制 / 含控制字符的文本 / 空串必须
// 逐值还原：整数不得经 float64、二进制不得变 base64 字符串、非 NUL 控制字符不得被清洗。
// 逆改点：`fromWire` 把整数按 float64 解析再回写 → 2^53+1 变 9007199254740992；
// `toWire` 把 `Bytes` 写成 base64 文本 → 回读的 bytea 变成 ASCII 字节。
func TestMigration_EncodingPreservesExactValues(t *testing.T) {
	source := migrationFixture(t)
	insertSession(t, source)

	// 2^53+1 是 float64 分不清的相邻整数之一（9007199254740993 → 9007199254740992）。
	// 文本里嵌 `char(1)`：合法 UTF-8 的非 NUL 控制字符，编码器只拒 NUL、不拒它。
	execFixture(t, source, `INSERT INTO messages
		(id, session_id, seq, role, content, reasoning_content, created_at)
		VALUES (9007199254740993, 's1', 1, 'user', '中文' || char(1) || 'b', '', 1000)`)
	execFixture(t, source, `INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source,
		 status, detail_json, created_at)
		VALUES ('`+exactOperationID+`', 'credential.rotate', 'model_connection', 'mc-1',
		 'local_trust', 'local_trust', 'cli', 'pending', '{"k":"v"}', 1000)`)
	execFixture(t, source, `INSERT INTO credentials
		(credential_id, owner_kind, owner_uid, binding_key, owner_name, state, current_version,
		 revocation_epoch, key_id, created_at, updated_at, operation_id)
		VALUES ('`+exactCredentialID+`', 'model_connection', 'mc-1', '', 'demo',
		 'active', 1, 0, 'key-1', 1000, 1000, '`+exactOperationID+`')`)
	execFixture(t, source, `INSERT INTO credential_versions
		(credential_id, version_no, state, key_id, target_binding, last4, nonce, ciphertext,
		 created_at, updated_at, operation_id)
		VALUES ('`+exactCredentialID+`', 1, 'active', 'key-1', 'mc-1', '0000',
		 x'00ff10000000000000000000', x'00ff10ab', 1000, 1000, '`+exactOperationID+`')`)

	dump, manifest := exportFixture(t, source)
	db := pgtest.NewTestDB(t)
	_, err := NewImporter(db, dump, manifest).Import(importCtx)
	require.NoError(t, err)

	var id int64
	var content, reasoning string
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT id, content, reasoning_content FROM messages`).Scan(&id, &content, &reasoning))
	require.Equal(t, int64(9007199254740993), id, "2^53+1 必须逐位保留，不得经 float64")
	require.Equal(t, "中文\x01b", content, "非 NUL 控制字符必须逐字节保留")
	require.Equal(t, "", reasoning, "空串必须仍是空串")

	var nonce, ciphertext []byte
	require.NoError(t, db.QueryRowContext(importCtx,
		`SELECT nonce, ciphertext FROM credential_versions
		 WHERE credential_id = $1 AND version_no = 1`, exactCredentialID).Scan(&nonce, &ciphertext))
	require.True(t, bytes.Equal(exactNonce, nonce),
		"BLOB 必须原样进 bytea，不得变成 base64 文本：%x", nonce)
	require.True(t, bytes.Equal(exactCiphertext, ciphertext),
		"密文二进制必须逐字节保留：%x", ciphertext)
}

// TestMigration_RejectsInvalidUTF8 源库里的非法 UTF-8 必须**在导出编码期**中止：PG text
// 存不下它，替换成 U+FFFD 会让数据静默变形。导出失败后不得留下可被导入的清单。
//
// 拒绝发生在编码期，导入根本无从开始，因此本用例不打开临时 PG——这不是省略目标侧断言，
// 而是失败面本就在源侧（`manifest.json` 缺席即"没有可导入的东西"）。
// 逆改点：编码器改用 `strings.ToValidUTF8` 清洗 → 错误消失，导出成功，本用例红。
func TestMigration_RejectsInvalidUTF8(t *testing.T) {
	source := migrationFixture(t)
	insertSession(t, source)
	execFixture(t, source, `INSERT INTO messages (id, session_id, seq, role, content, created_at)
		VALUES (1, 's1', 1, 'user', CAST(x'fffe' AS TEXT), 1000)`)

	outDir := t.TempDir()
	_, err := NewExporter(source, SourceSchemaVersion).Export(importCtx, outDir)
	require.ErrorIs(t, err, ErrInvalidValue)
	require.Contains(t, err.Error(), "UTF-8", "非法 UTF-8 必须显式失败，不得替换成 U+FFFD")
	require.Contains(t, err.Error(), "messages", "错误必须指出表名")
	require.Contains(t, err.Error(), "content", "错误必须指出列名")
	requireNoManifest(t, outDir)
}

// TestMigration_RejectsNulByte NUL 字节同样在导出编码期拒绝：PG text 不接受 0x00，
// 静默截断或替换会让回读值悄悄变短。
// 逆改点：写盘前按 NUL 截断（或替换成空格）→ 写盘不报错，导出成功，本用例红。
func TestMigration_RejectsNulByte(t *testing.T) {
	source := migrationFixture(t)
	insertSession(t, source)
	// 用字节 CAST 造 NUL：`char(0)` 在部分 SQLite 版本里会退化成空串，不以它作夹具。
	execFixture(t, source, `INSERT INTO messages (id, session_id, seq, role, content, created_at)
		VALUES (1, 's1', 1, 'user', CAST(x'610062' AS TEXT), 1000)`)

	outDir := t.TempDir()
	_, err := NewExporter(source, SourceSchemaVersion).Export(importCtx, outDir)
	require.ErrorIs(t, err, ErrInvalidValue)
	require.Contains(t, err.Error(), "NUL", "NUL 文本必须显式失败，不得截断或替换")
	require.Contains(t, err.Error(), "messages")
	require.Contains(t, err.Error(), "content")
	requireNoManifest(t, outDir)
}

// TestMigration_PreservesNullAndEmptyString NULL 与空串在源、dump、目标三处都必须是两个值：
// 合并任意一处，目标列的空串 / NULL 数量就会对不上。
// 逆改点：`toWire` 把空文本当成 NULL 写出 → 目标列空串计数变 0、NULL 计数变 2，断言红。
func TestMigration_PreservesNullAndEmptyString(t *testing.T) {
	source := migrationFixture(t)
	insertSession(t, source)
	// 两行的 content 与 reasoning_content 互换 NULL / 空串：任一列发生合并都能被计数发现。
	execFixture(t, source, `INSERT INTO messages
		(id, session_id, seq, role, content, reasoning_content, created_at)
		VALUES (1, 's1', 1, 'user', NULL, '', 1000)`)
	execFixture(t, source, `INSERT INTO messages
		(id, session_id, seq, role, content, reasoning_content, created_at)
		VALUES (2, 's1', 2, 'assistant', '', NULL, 1001)`)

	dump, manifest := exportFixture(t, source)
	db := pgtest.NewTestDB(t)
	_, err := NewImporter(db, dump, manifest).Import(importCtx)
	require.NoError(t, err)

	for _, column := range []string{"content", "reasoning_content"} {
		var nulls, empties int
		require.NoError(t, db.QueryRowContext(importCtx,
			`SELECT count(*) FILTER (WHERE `+column+` IS NULL),
			        count(*) FILTER (WHERE `+column+` = '')
			 FROM messages`).Scan(&nulls, &empties))
		require.Equal(t, 1, nulls, "%s 列的 NULL 数量必须保持 1", column)
		require.Equal(t, 1, empties, "%s 列的空串数量必须保持 1（不得并入 NULL）", column)
	}
}

// insertSession 写入夹具里唯一的会话（消息的外键父行）。
func insertSession(t *testing.T, source *sql.DB) {
	t.Helper()
	execFixture(t, source, `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at)
		VALUES ('s1', 'demo', 'cli', 'u1', 'active', 2, 1000, 1000)`)
}

// requireNoManifest 断言导出目录里没有 `manifest.json`：没有清单就没有可导入的 dump，
// 这是"拒绝发生在落任何可消费工件之前"的直接证据。
func requireNoManifest(t *testing.T, dir string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, "manifest.json"))
	require.ErrorIs(t, err, os.ErrNotExist, "导出失败不得留下可被导入的清单")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "快照与逐表 JSONL 可以留着排障，目录不该是空的")
}
