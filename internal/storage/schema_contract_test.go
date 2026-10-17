package storage

import (
	"os"
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
