package dbmigrate

// 本文件是**导入拓扑序**的显式声明与自检（C-41 / 颗粒度文档 §3.4.3）：SQLite v6 的 32 张
// 业务表按硬外键与逻辑依赖排成一份固定次序，导入端按它逐表插入，**不**按表名字典序、
// 也**不**靠数据库返回顺序推断。
// This file declares the explicit import topology and its self-check: 32 source tables in a
// fixed order derived from hard foreign keys and logical dependencies, never from sorting
// table names or from whatever order the database hands back.

import (
	"errors"
	"fmt"
	"strings"
)

// migrationOrder 是导入的**唯一**权威次序（§3.4.3 的 [1]～[11] 组，组内保持文档列出的
// 相对次序）。`schema_version` 不在其中：PG 谱系自建，不从 SQLite 沿用版本号。
//
// 每一条位置都有理由，改动前先读组注释：
//   - [1] management_operations 最早：resource_versions / resource_pointers / credentials /
//     credential_versions / agent_lifecycle / agent_archives 都引用它；
//   - [2] sessions 先于 messages（messages.session_id 引用）；
//   - [3] 四张无外键的表先于引用它们的 run_* 与 notify_deliveries；
//   - [4] scheduled_tasks 先于 task_executions / schedule_deliveries；
//   - [5] 资源版本链：identities → versions → pointers（pointers 引用 versions）；
//   - [6] 凭据链：credentials → credential_versions → resource_version_credentials；
//   - [7] mcp_credential_sources 无外键，但语义上必须在 credentials 之后——提前会让导入
//     记录与 BackfillEnvSources 互相覆盖（C-41）；
//   - [8] 实例级单行表互不依赖，成组；
//   - [9] 绑定 / 渠道 / 连接测试 / 记忆 / 生命周期 / 归档；
//   - [10] run_* 四表需要 runs + resource_versions + credentials 已就位；
//   - [11] notify_deliveries 最后：tool_invocations 与 runs 都必须先存在。
//
// The order is authoritative; every position is explained above so a reordering is a
// deliberate design change rather than an accident.
var migrationOrder = []string{
	// [1]
	"management_operations",
	// [2]
	"sessions",
	"messages",
	// [3]
	"runs",
	"config_snapshots",
	"llm_calls",
	"tool_invocations",
	// [4]
	"scheduled_tasks",
	"task_executions",
	"schedule_deliveries",
	// [5]
	"resource_identities",
	"resource_versions",
	"resource_pointers",
	// [6]
	"credentials",
	"credential_versions",
	"resource_version_credentials",
	// [7]
	"mcp_credential_sources",
	// [8]
	"instance_secrets",
	"execution_policy",
	"network_exceptions",
	"network_domain_policy",
	// [9]
	"agent_resource_bindings",
	"notify_channels",
	"resource_connection_tests",
	"memory_entries",
	"agent_lifecycle",
	"agent_archives",
	// [10]
	"run_resource_manifests",
	"run_resource_refs",
	"run_credential_refs",
	"run_notify_refs",
	// [11]
	"notify_deliveries",
}

// migrationForeignKeys 是源端表之间的**硬外键**（引用表 → 被引用的目标表），逐条对应
// `docs/contracts/schema.pg.sql` 的 DDL。只列源端表之间的边：目标是 PG 谱系自建表的边
// （`sessions` / `runs` / `scheduled_tasks` 的 `caller_id → callers`）在
// `migrationExternalParents` 里声明。
//
// Only edges between source tables live here; edges into PG-native tables are declared
// as external parents instead.
var migrationForeignKeys = map[string][]string{
	// PG 谱系自建表的边：callers 在导入前已由建表与种子就绪。
	"sessions":        {"callers"},
	"runs":            {"callers"},
	"scheduled_tasks": {"callers"},
	// 源端表之间的硬外键。
	"messages":                     {"sessions"},
	"task_executions":              {"scheduled_tasks"},
	"schedule_deliveries":          {"scheduled_tasks"},
	"resource_versions":            {"management_operations"},
	"resource_pointers":            {"resource_versions", "management_operations"},
	"credentials":                  {"management_operations"},
	"credential_versions":          {"credentials", "management_operations"},
	"resource_version_credentials": {"resource_versions"},
	"memory_entries":               {"memory_entries"}, // 自引用：supersedes_id
	"agent_lifecycle":              {"management_operations"},
	"agent_archives":               {"management_operations"},
	"run_resource_manifests":       {"runs"},
	"run_resource_refs":            {"runs", "resource_versions"},
	"run_credential_refs":          {"runs"},
	"run_notify_refs":              {"runs"},
	"notify_deliveries":            {"tool_invocations", "runs"},
}

// migrationExternalParents 是源端表引用、但**不由导入创建**的 PG 谱系表：它们在导入前
// 已由 PG 建表与种子就绪。自检遇到不在迁移顺序里的外键目标时，只认这份清单。
// External parents already exist before import; any other unknown target is an error.
var migrationExternalParents = []string{"callers"}

// MigrationOrder 返回导入顺序的副本：调用方拿到的是快照，改它不会污染权威次序。
// MigrationOrder returns a copy so callers cannot mutate the authoritative order.
func MigrationOrder() []string {
	out := make([]string, len(migrationOrder))
	copy(out, migrationOrder)
	return out
}

// MigrationForeignKeys 返回源端硬外键的副本（引用表 → 目标表）。
// MigrationForeignKeys returns a copy of the source-to-source foreign keys.
func MigrationForeignKeys() map[string][]string {
	out := make(map[string][]string, len(migrationForeignKeys))
	for table, targets := range migrationForeignKeys {
		cp := make([]string, len(targets))
		copy(cp, targets)
		out[table] = cp
	}
	return out
}

// MigrationExternalParents 返回外部父表清单的副本。
// MigrationExternalParents returns a copy of the external-parent list.
func MigrationExternalParents() []string {
	out := make([]string, len(migrationExternalParents))
	copy(out, migrationExternalParents)
	return out
}

// ValidateMigrationOrder 是导入前的**自检**：任何外键目标晚于引用表即失败，引用表或目标表
// 未声明也失败。自引用（`memory_entries.supersedes_id`）合法；目标是外部父表时必须在
// `externalParents` 里显式声明，**不得**把未知目标默默当外部表放行。
//
// 失败即停：顺序错了的导入会撞外键或写入半量数据，宁可拒绝启动也不"边导边修"。
// The self-check fails closed: a late target, an undeclared table or an unknown external
// parent all abort before any row is read.
func ValidateMigrationOrder(order []string, foreignKeys map[string][]string, externalParents []string) error {
	if len(order) == 0 {
		return errors.New("dbmigrate: 迁移顺序为空——导入前必须先声明拓扑序")
	}

	position := make(map[string]int, len(order))
	for i, table := range order {
		if strings.TrimSpace(table) == "" {
			return fmt.Errorf("dbmigrate: 迁移顺序第 %d 项为空表名", i)
		}
		if _, dup := position[table]; dup {
			return fmt.Errorf("dbmigrate: 迁移顺序重复声明 %q", table)
		}
		position[table] = i
	}

	external := make(map[string]bool, len(externalParents))
	for _, table := range externalParents {
		external[table] = true
	}

	for table := range foreignKeys {
		if _, ok := position[table]; !ok {
			// 顺序里没有的表不可能被导入，声明它的外键说明两份清单已经漂移。
			return fmt.Errorf("dbmigrate: 外键清单里的引用表 %q 不在迁移顺序中", table)
		}
	}

	// 按 order 遍历而不是按 map 遍历：同样的输入永远得到同样的首条失败信息。
	for _, table := range order {
		for _, target := range foreignKeys[table] {
			if target == table {
				continue // 自引用不产生跨表顺序约束
			}
			targetPos, inOrder := position[target]
			if !inOrder {
				if external[target] {
					continue
				}
				return fmt.Errorf("dbmigrate: %q 的外键目标 %q 既不在迁移顺序中，也不在外部父表清单里", table, target)
			}
			if targetPos > position[table] {
				return fmt.Errorf("dbmigrate: 外键目标 %q 晚于引用表 %q（位置 %d > %d）",
					target, table, targetPos, position[table])
			}
		}
	}
	return nil
}
