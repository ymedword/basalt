package dbmigrate

// 本文件是**源列声明类型 → 目标 PG 类型与编码标签**的映射（颗粒度文档 §2.11.1 的映射表）。
// 声明类型只决定"导到哪里、能不能为空"；**不**决定实际值类型——SQLite 是动态类型，
// 实际存储类在编码时按值判定（见 `encodeStoredValue`）。
// This file maps a source column's declared type to its PostgreSQL target type and canonical
// encoding tag. The declared type never decides the value's own storage class.

import (
	"fmt"
	"strings"
)

// 目标 PG 类型的字面量（与 `docs/contracts/schema.pg.sql` 逐字同源）。
const (
	pgTypeBigInt  = "bigint"
	pgTypeDouble  = "double precision"
	pgTypeText    = "text"
	pgTypeBytea   = "bytea"
	pgTypeNumeric = "numeric"
	pgTypeBoolean = "boolean"
	pgTypeJSONB   = "jsonb"
)

// columnTypeOverrides 是**声明类型推不出目标类型**的那几列，逐个钉死。
//
// 来源是 `docs/contracts/schema.pg.sql`：SQLite 只把布尔存成 INTEGER、把 JSON 存成 TEXT，
// 而目标列分别是 boolean 与 jsonb。此表只允许出现"源表"（`MigrationOrder` 里的表）的列；
// 与契约的对齐由 `TestDeclaredType_MatchesContracts` 机器判卷——契约改动而本表没跟上即红。
// columnTypeOverrides pins the few columns whose target type cannot be derived from the
// declared type; a test cross-checks this table against both frozen contracts.
var columnTypeOverrides = map[string]struct {
	PGType   string
	Encoding ValueTag
}{
	"scheduled_tasks.enabled":           {PGType: pgTypeBoolean, Encoding: TagInteger},
	"agent_lifecycle.valid":             {PGType: pgTypeBoolean, Encoding: TagInteger},
	"management_operations.detail_json": {PGType: pgTypeJSONB, Encoding: TagText},
}

// mapColumnType 按"表.列"先查覆盖表，再按声明类型归类。
// 认不出的声明一律失败——**不**猜成 text 混过去。
func mapColumnType(table, column, decl string) (string, ValueTag, error) {
	if override, ok := columnTypeOverrides[table+"."+column]; ok {
		return override.PGType, override.Encoding, nil
	}
	return mapDeclaredType(decl)
}

// mapDeclaredType 按 SQLite 声明类型归到目标类型与编码标签。
// 空声明（SQLite 允许无类型列）与认不出的声明都拒绝：没有声明就没有导入目标。
func mapDeclaredType(decl string) (string, ValueTag, error) {
	normalized := strings.ToUpper(strings.TrimSpace(decl))
	if normalized == "" {
		return "", "", fmt.Errorf("%w: 列没有声明类型", ErrInvalidValue)
	}
	// 去掉长度 / 精度参数：VARCHAR(64) → VARCHAR，NUMERIC(10,2) → NUMERIC。
	if idx := strings.IndexByte(normalized, '('); idx >= 0 {
		normalized = strings.TrimSpace(normalized[:idx])
	}

	switch {
	case normalized == "BOOLEAN" || normalized == "BOOL":
		return pgTypeBoolean, TagInteger, nil
	case strings.Contains(normalized, "INT"):
		return pgTypeBigInt, TagInteger, nil
	case strings.Contains(normalized, "REAL") ||
		strings.Contains(normalized, "DOUB") ||
		strings.Contains(normalized, "FLOA"):
		return pgTypeDouble, TagReal, nil
	case strings.Contains(normalized, "CHAR") ||
		strings.Contains(normalized, "CLOB") ||
		strings.Contains(normalized, "TEXT"):
		return pgTypeText, TagText, nil
	case strings.Contains(normalized, "BLOB"):
		return pgTypeBytea, TagBytes, nil
	case strings.Contains(normalized, "NUMERIC") || strings.Contains(normalized, "DECIMAL"):
		return pgTypeNumeric, TagText, nil
	default:
		return "", "", fmt.Errorf("%w: 声明类型 %q 没有对应的目标类型", ErrInvalidValue, decl)
	}
}
