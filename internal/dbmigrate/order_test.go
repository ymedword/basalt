package dbmigrate_test

// 本文件是导入拓扑序的验收面（C-41 / 颗粒度文档 §3.4.3）：显式顺序满足全部硬外键，
// 按表名字典序排会被自检直接拒绝。
// This file pins the explicit import topology and proves the self-check rejects a
// lexicographic ordering.

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/dbmigrate"
)

// indexOf 返回表在顺序里的位置；找不到直接让用例失败，避免"比较两个 -1"式的假通过。
func indexOf(t *testing.T, order []string, table string) int {
	t.Helper()
	for i, name := range order {
		if name == table {
			return i
		}
	}
	t.Fatalf("迁移顺序缺少表 %q", table)
	return -1
}

// TestMigrationOrder_AllForeignKeysSatisfied 守"导入按显式拓扑序、自检覆盖全部硬外键"：
// 关键边逐条成立；完整外键表经自检通过；字典序 / 颠倒 / 未知外部目标三种错序都会失败。
// 逆改点：把 migrationOrder 换成 sort.Strings → 字典序负向断言与自检同时红。
// 验收点：C-41、§3.4.3 的导入顺序自检规则。
func TestMigrationOrder_AllForeignKeysSatisfied(t *testing.T) {
	order := dbmigrate.MigrationOrder()
	require.Len(t, order, 32, "SQLite v6 的 33 张表里，schema_version 由 PG 自建，其余 32 张按序导入")

	// 设计写明的关键边：每一条都是逆改点（换顺序即红）。
	require.Less(t, indexOf(t, order, "management_operations"), indexOf(t, order, "resource_versions"))
	require.Less(t, indexOf(t, order, "credentials"), indexOf(t, order, "credential_versions"))
	require.Less(t, indexOf(t, order, "resource_versions"), indexOf(t, order, "resource_pointers"))
	require.Less(t, indexOf(t, order, "tool_invocations"), indexOf(t, order, "notify_deliveries"))
	require.Less(t, indexOf(t, order, "scheduled_tasks"), indexOf(t, order, "task_executions"))
	require.Less(t, indexOf(t, order, "sessions"), indexOf(t, order, "messages"))

	require.NoError(t, dbmigrate.ValidateMigrationOrder(
		order, dbmigrate.MigrationForeignKeys(), dbmigrate.MigrationExternalParents()),
		"显式顺序必须满足全部硬外键")

	// 负向①：表名字典序——导入端最容易偷的懒。
	lexical := append([]string(nil), order...)
	sort.Strings(lexical)
	require.Error(t, dbmigrate.ValidateMigrationOrder(
		lexical, dbmigrate.MigrationForeignKeys(), dbmigrate.MigrationExternalParents()),
		"字典序必须被自检拒绝")

	// 负向②：目标表晚于引用表。
	require.ErrorContains(t, dbmigrate.ValidateMigrationOrder(
		[]string{"messages", "sessions"},
		map[string][]string{"messages": {"sessions"}},
		nil,
	), "晚于引用表")

	// 负向③：外部父表未声明——不得把未知目标默默当外部表放行。
	require.ErrorContains(t, dbmigrate.ValidateMigrationOrder(
		[]string{"sessions"},
		map[string][]string{"sessions": {"callers"}},
		nil,
	), "外部父表清单")

	// 负向④：重复声明。
	require.ErrorContains(t, dbmigrate.ValidateMigrationOrder(
		[]string{"sessions", "sessions"},
		nil, nil,
	), "重复声明")
}

// TestMigration_McpSourcesAfterCredentials 守 `mcp_credential_sources` 必须排在
// `credentials` / `credential_versions` **之后**：这张表在源端没有硬外键（`binding_key`
// 是文本引用），自检与"全部硬外键满足"都发现不了顺序错误，所以单独钉一条。
//
// 现网后果：先写来源行、后写凭据行时，导入中途失败的回滚窗口里会出现"来源指向不存在的
// 托管凭据"，而恢复路径按凭据反查来源，读到的就是空集——凭证槽位看起来像"从未托管过"。
// 逆改点：把 `mcp_credential_sources` 提到 [6] 组之前 → 本条断言红（其余用例全绿）。
func TestMigration_McpSourcesAfterCredentials(t *testing.T) {
	order := dbmigrate.MigrationOrder()
	require.Greater(t, indexOf(t, order, "mcp_credential_sources"), indexOf(t, order, "credentials"),
		"mcp_credential_sources 是 credentials 的语义下游：无硬外键也得排在后面")
	require.Greater(t, indexOf(t, order, "mcp_credential_sources"), indexOf(t, order, "credential_versions"),
		"来源行引用的 binding_key 由 credential_versions.target_binding 定义")
}
