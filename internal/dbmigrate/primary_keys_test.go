package dbmigrate

// 本文件把导入端的主键列声明（`sourcePrimaryKeys`）钉到**两份冻结契约**上
// （颗粒度文档 §2.11.1「IDSetSHA256 的编码必须固定」）：清单不存主键列，导入端重算
// ID 集合摘要只能靠这张表；契约改而这张表没跟上，摘要就会按错的列序算——两边都"算得出"
// 却对不上，且不会有人工察觉的报错。
// This test pins the importer's key-column table to both frozen contracts. The manifest
// stores no key columns, so a drift here silently changes the ID-set digest.

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSourcePrimaryKeys_MatchContracts 逐表对拍：迁移顺序里的每张表都必须有主键列声明，
// 且列序与 SQLite / PostgreSQL 两份契约逐字一致；表里也不得出现迁移顺序之外的条目。
// 逆改点：改某张复合主键表的列序（如把 `(run_id, agent_uid, channel_uid)` 换成
// `(agent_uid, run_id, channel_uid)`）→ 对应表的断言红；删一条 → "缺少主键列声明" 红；
// 契约新增一张表而本表没补 → 同上。
func TestSourcePrimaryKeys_MatchContracts(t *testing.T) {
	sqlite := parseContractKeys(t, contractPaths.SQLite)
	postgres := parseContractKeys(t, contractPaths.PostgreSQL)

	require.Len(t, sourcePrimaryKeys, len(migrationOrder),
		"主键列声明必须与迁移顺序一一对应")
	for _, table := range migrationOrder {
		declared, ok := sourcePrimaryKeys[table]
		require.True(t, ok, "表 %q 缺少主键列声明——导入端算不出 IDSetSHA256", table)
		require.Equal(t, declared, singlePrimaryKey(t, contractPaths.SQLite, sqlite, table),
			"表 %q 的主键列序必须与 SQLite 契约逐字一致", table)
		require.Equal(t, declared, singlePrimaryKey(t, contractPaths.PostgreSQL, postgres, table),
			"表 %q 的主键列序必须与 PG 契约逐字一致", table)
	}
	for table := range sourcePrimaryKeys {
		require.Contains(t, migrationOrder, table,
			"sourcePrimaryKeys 里有迁移顺序之外的表 %q——过期条目", table)
	}
}

// singlePrimaryKey 取一张表在契约里声明的唯一主键（列序即声明序）。
// 一条主键都没有、或声明了两条（SQL 里不允许，但解析口径漂移时可能出现）都直接失败。
func singlePrimaryKey(t *testing.T, path string, keys map[string][][]string, table string) []string {
	t.Helper()
	groups, ok := keys[table]
	require.Truef(t, ok, "%s：契约缺少表 %q", path, table)
	require.Lenf(t, groups, 1, "%s：表 %q 应当恰好声明一个主键", path, table)
	return groups[0]
}

// parseContractKeys 把契约 DDL 解析成 表名 → 主键列清单（每张表只允许一条主键）。
// 复用 `decltype_test.go` 的切分器：注释剥离、括号体切分、顶层逗号切分三件与类型对拍
// 同源，避免契约格式的两套解析口径。
func parseContractKeys(t *testing.T, path string) map[string][][]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoErrorf(t, err, "契约文件必须存在：%s", path)

	text := stripSQLComments(string(raw))
	const prefix = "CREATE TABLE IF NOT EXISTS "
	keys := make(map[string][][]string)

	for {
		index := strings.Index(text, prefix)
		if index < 0 {
			break
		}
		rest := text[index+len(prefix):]
		open := strings.IndexByte(rest, '(')
		require.Greaterf(t, open, 0, "%s：CREATE TABLE 后缺列定义括号", path)
		name := strings.TrimSpace(rest[:open])
		body, tail := splitParenBody(t, path, rest[open:])

		var groups [][]string
		for _, item := range splitTopLevel(t, body) {
			if columns, isKey := primaryKeyOf(t, path, name, item); isKey {
				groups = append(groups, columns)
			}
		}
		keys[name] = groups
		text = tail
	}
	require.NotEmpty(t, keys, "%s：没解析出任何表——解析口径与契约格式已漂移", path)
	return keys
}

// primaryKeyOf 从一条表体条目里取主键列（列序即声明序）。
// 三种形态：列内联 `id INTEGER PRIMARY KEY`、表级 `PRIMARY KEY (a, b)`、
// 以及具名约束 `CONSTRAINT ... PRIMARY KEY (...)`——最后一种契约当前不用，
// 出现即显式失败，**不**静默跳过（跳过会让"该表没主键"看起来像契约本意）。
func primaryKeyOf(t *testing.T, path, table, item string) ([]string, bool) {
	t.Helper()
	if !strings.Contains(strings.ToLower(item), "primary key") {
		return nil, false
	}
	fields := strings.Fields(item)
	require.NotEmptyf(t, fields, "%s：表 %q 出现空条目", path, table)

	switch strings.ToLower(fields[0]) {
	case "primary":
		open := strings.IndexByte(item, '(')
		require.Greaterf(t, open, 0, "%s：表 %q 的表级 PRIMARY KEY 缺括号", path, table)
		body, _ := splitParenBody(t, path, item[open:])
		var columns []string
		for _, part := range splitTopLevel(t, body) {
			columns = append(columns, strings.Trim(part, " \"`"))
		}
		require.NotEmptyf(t, columns, "%s：表 %q 的 PRIMARY KEY 没有列", path, table)
		return columns, true
	case "constraint":
		require.FailNowf(t, "契约出现具名主键约束，解析口径需扩展",
			"%s：表 %q 的条目 %q", path, table, item)
	default:
		// 列内联主键：首字段即列名（`id INTEGER PRIMARY KEY AUTOINCREMENT`）。
		return []string{fields[0]}, true
	}
	return nil, false
}
