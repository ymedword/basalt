package postgres

// 本文件是 **PG 谱系契约的防漂移锁**（US-16 §4 的 `TestPostgresSchemaFile_MatchesContract`，
// T105a）：契约 `docs/contracts/schema.pg.sql` 与运行时迁移源
// `internal/storage/postgres/migrations/0001_postgres_v1.sql` 逐字节一致，且建表脚本声明的
// 表集合恰为冻结的 52 张。形状与 SQLite 侧 `internal/storage/schema_contract_test.go` 一致，
// 两把锁各管一件事：
//
//  1. **逐字节**锁字面量——两份文件分叉（哪怕只差一行注释）立即失败；
//  2. **表集**锁建表数量——即便两处同步删掉一张 `CREATE TABLE`，字节仍相等，表集断言
//     会指出少建了哪张。
//
// 契约头部「迁移纪律」第 6 条点名本用例：运行时迁移源是契约的逐字副本。
// This file double-locks the PG contract: byte equality with the runtime migration source
// and the exact fifty-two-table set the script must declare.

import (
	"bytes"
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// firstDiffLine 定位两份文本的首个差异：返回差异所在行号（1 起）与两侧该行原文。
// 逐字节断言失败时给出可读定位，而不是把几万字节的切片整个摊进失败信息。
// firstDiffLine locates the first differing line so a failure is diagnosable at a glance.
func firstDiffLine(want, got []byte) (line int, wantLine, gotLine string) {
	limit := min(len(want), len(got))
	offset := 0
	for offset < limit && want[offset] == got[offset] {
		offset++
	}
	return 1 + bytes.Count(want[:offset], []byte{'\n'}), lineAt(want, offset), lineAt(got, offset)
}

// lineAt 取 offset 所在行的原文；offset 越过文件末尾时取最后一行。
// lineAt returns the line containing offset, clamping at end of file.
func lineAt(text []byte, offset int) string {
	start := bytes.LastIndexByte(text[:offset], '\n') + 1
	end := bytes.IndexByte(text[offset:], '\n')
	if end < 0 {
		end = len(text)
	} else {
		end += offset
	}
	return string(text[start:end])
}

// pgContractTables 是契约建表脚本声明的 52 张表（排序后）。加表 = 契约变更：
// 契约、迁移脚本与这份清单三处同批更新，缺一处即红。
// pgContractTables is the frozen fifty-two-table set of the PG contract, sorted.
var pgContractTables = []string{
	"agent_archives",
	"agent_lifecycle",
	"agent_resource_bindings",
	"caller_keys",
	"caller_limits",
	"caller_usage_periods",
	"callers",
	"compaction_entries",
	"config_snapshots",
	"context_assembly_records",
	"credential_versions",
	"credentials",
	"diagnostic_events",
	"embedding_calls",
	"execution_policy",
	"instance_secrets",
	"kb_chunk_embeddings",
	"kb_chunks",
	"kb_collections",
	"kb_document_versions",
	"kb_documents",
	"kb_index_generations",
	"kb_ingest_jobs",
	"llm_calls",
	"management_operations",
	"mcp_credential_sources",
	"memory_embeddings",
	"memory_entries",
	"message_evidence_refs",
	"messages",
	"network_domain_policy",
	"network_exceptions",
	"notify_channels",
	"notify_deliveries",
	"quota_reservations",
	"resource_connection_tests",
	"resource_identities",
	"resource_pointers",
	"resource_version_credentials",
	"resource_versions",
	"run_credential_refs",
	"run_notify_refs",
	"run_resource_manifests",
	"run_resource_refs",
	"runs",
	"schedule_deliveries",
	"scheduled_tasks",
	"schema_version",
	"sessions",
	"storage_cutover",
	"task_executions",
	"tool_invocations",
}

// TestPostgresSchemaFile_MatchesContract：契约与运行时迁移源逐字节一致，且建表集合恰为
// 冻结的 52 张。契约变更 = 同批更新两处文件 + 本文件的表清单。
//
// 逆改点：只改迁移脚本不改契约（或反之）→ 字节断言红；两处同步删掉一张表 → 字节仍等、
// 表集断言红（少建的那张在差异里逐字可见）；把列类型改回 `timestamptz` 只改一处 → 字节红。
// 验收点：FR-071 / 契约「迁移纪律」第 3、6 条。
func TestPostgresSchemaFile_MatchesContract(t *testing.T) {
	t.Parallel()

	// 测试的工作目录是包目录，契约文件在三级之上的 docs/contracts/ 下。
	contract, err := os.ReadFile("../../../docs/contracts/schema.pg.sql")
	require.NoError(t, err, "契约文件 docs/contracts/schema.pg.sql 必须可读")
	impl, err := os.ReadFile("migrations/0001_postgres_v1.sql")
	require.NoError(t, err, "运行时迁移源必须可读")

	if !bytes.Equal(contract, impl) {
		line, wantLine, gotLine := firstDiffLine(contract, impl)
		t.Fatalf("运行时迁移源 migrations/0001_postgres_v1.sql 与契约不再逐字节一致：\n"+
			"  首个差异在第 %d 行\n  契约: %q\n  迁移: %q", line, wantLine, gotLine)
	}

	// 表集从**实现副本**里数：与契约同一份字节时同样要数，防"两处同步删表"。
	pattern := regexp.MustCompile(`(?m)^CREATE TABLE IF NOT EXISTS ([a-z_]+) \(`)
	matches := pattern.FindAllStringSubmatch(string(impl), -1)
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match[1])
	}
	slices.Sort(names)
	require.Equal(t, pgContractTables, names,
		"PG 契约的建表集合必须恰为冻结的 52 张（多建 / 少建都在差异里逐字可见）")
}
