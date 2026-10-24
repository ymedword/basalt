package storage

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSchemaFile_MatchesContract：实现副本与契约逐字节一致——防"字面量第二份
// 定义"漂移（颗粒度文档 §2.2 约束 12；开发操作 §8.1 第 5 项）。
// 契约变更 = 同批更新两处：docs/contracts/schema.sql 与 internal/storage/schema.sql。
func TestSchemaFile_MatchesContract(t *testing.T) {
	impl, err := os.ReadFile(schemaFileName)
	require.NoError(t, err)

	// 测试的工作目录是包目录，契约文件在两个上级的 docs/contracts/ 下。
	contract, err := os.ReadFile("../../docs/contracts/schema.sql")
	require.NoError(t, err)

	require.Equal(t, contract, impl, "internal/storage/schema.sql 与契约不再逐字节一致")
}

// TestSchemaFile_DeclaresContractTables：建表脚本声明的用户表集合恰为契约的
// 26 张。逐字节一致锁字面量，本用例再锁"表集"——即便两处同步删掉了一张
// CREATE TABLE，表集断言仍会指出少建了哪张（颗粒度文档 §2.2 约束 12）。
// The declared table set must equal the 26-table contract set.
func TestSchemaFile_DeclaresContractTables(t *testing.T) {
	impl, err := os.ReadFile(schemaFileName)
	require.NoError(t, err)

	pattern := regexp.MustCompile(`(?m)^CREATE TABLE IF NOT EXISTS ([a-z_]+) \(`)
	matches := pattern.FindAllStringSubmatch(string(impl), -1)
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match[1])
	}
	slices.Sort(names)
	require.Equal(t, contractTables, names)
}
