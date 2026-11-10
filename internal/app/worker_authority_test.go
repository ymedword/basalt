package app

// 本文件是 US-25C FR-25C-12 的角色门判据：Worker 启动链**逐项**跳过共享初始化写。
// 每一项的形状差异太大（有的写操作行、有的写 applied 指针、有的写知识代际行），
// 在单元层逐个跑真实写面会退化成"每个写面一套库夹具"；这里判的是**角色门本身**：
//
//  1. 生产代码把 §2.3.1 表收敛成唯一的清单（`sharedInitItems`），条目值 / 名字与
//     模块卡逐项对号，缺项或改名都红；
//  2. 同一清单上 Worker 全拒、control 全放——不存在"部分保留"的例外；
//  3. 清单里**每一项**都必须在 `internal/app` 的生产装配面被引用——声明了却没人问，
//     等于没有角色门（源码级守护断言，FR-25C-20 允许的边界守护形态之一）；
//  4. 角色由子命令判定（D-25C-06）：`serve` = control、`gateway` = worker，零值走
//     worker（失败关闭方向）。
//
// 逐项的**真实零写道**由下游具名用例承担：知识代际 `TestWorkerKnowledge_*`（T027）、
// 策略种子 `TestPolicyRefresh_*`（T028）、资源准备 `TestProviderPrepare_*`（T030）、
// 调度定义列本文件同批的 `scheduler_authority_test.go`；A+B 跨进程证据见 §4.5。
// Per-item zero-write evidence lives in the downstream named cases; this file pins the
// gate inventory itself plus the source-level wiring assertion.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sharedInitContract 是模块卡 §2.3.1 表在测试侧的对号清单：条目值、生产常量标识与
// 模块卡里的固定名字三者一一对应，顺序即模块卡表格顺序。
// 前 13 项逐字对应 FR-25C-12；末两项是 US-25C 新增、同样只在 control 启动链执行的
// 共享初始化写（§2.3.1 树图的 BackfillBundlePayloads / repairStaleBundleSkills）。
var sharedInitContract = []struct {
	item     sharedInitItem
	constant string
	name     string
}{
	{sharedInitBaseline, "sharedInitBaseline", "baseline"},
	{sharedInitOperationRecovery, "sharedInitOperationRecovery", "operation_recovery"},
	{sharedInitDefinitionReconcile, "sharedInitDefinitionReconcile", "definition_reconcile"},
	{sharedInitStartupBindings, "sharedInitStartupBindings", "startup_bindings"},
	{sharedInitMCPApplied, "sharedInitMCPApplied", "mcp_applied"},
	{sharedInitSecretsEnsure, "sharedInitSecretsEnsure", "secrets_ensure"},
	{sharedInitPolicySeed, "sharedInitPolicySeed", "policy_seed"},
	{sharedInitBuiltinOpenBox, "sharedInitBuiltinOpenBox", "builtin_open_box"},
	{sharedInitAssetBootstrap, "sharedInitAssetBootstrap", "asset_bootstrap"},
	{sharedInitTaskDefinitions, "sharedInitTaskDefinitions", "task_definitions"},
	{sharedInitMCPCredentialSource, "sharedInitMCPCredentialSource", "mcp_credential_source"},
	{sharedInitLearningRecovery, "sharedInitLearningRecovery", "learning_recovery"},
	{sharedInitKnowledgeGeneration, "sharedInitKnowledgeGeneration", "knowledge_generation"},
	{sharedInitPayloadBackfill, "sharedInitPayloadBackfill", "payload_backfill"},
	{sharedInitSkillRepair, "sharedInitSkillRepair", "skill_repair"},
}

// TestWorkerStartup_SkipsSharedWrites 逐项判 Worker 启动链的共享初始化写门。
// The worker must skip every shared-initialization write, item by item.
func TestWorkerStartup_SkipsSharedWrites(t *testing.T) {
	t.Parallel()

	items := sharedInitItems()
	require.Len(t, items, len(sharedInitContract),
		"共享初始化写清单的条目数必须与模块卡 §2.3.1 / FR-25C-12 对号")
	for i, want := range sharedInitContract {
		require.Equal(t, want.item, items[i], "第 %d 项的条目值必须与模块卡顺序一致", i)
		require.Equal(t, want.name, sharedInitItemName(want.item),
			"第 %d 项的名字必须与模块卡对号", i)

		require.False(t, roleWorker.allowSharedInit(want.item),
			"Worker 必须跳过共享初始化写: %s", want.name)
		require.True(t, roleControl.allowSharedInit(want.item),
			"control 必须执行共享初始化写: %s", want.name)
	}

	// 装配对号：每一项都必须在生产装配面被问到。声明在 worker_authority.go、却没有任何
	// 调用方，等于该写项没有角色门——这正是"漏包一项"的形态。
	// Every item must be consulted by the production assembly, not merely declared.
	source := readAppProductionSources(t)
	for _, want := range sharedInitContract {
		require.Contains(t, source, want.constant,
			"共享初始化写 %s 未在装配面引用——该项没有角色门", want.name)
	}
}

// TestRuntimeRole_RoleComesFromSubcommand 守 D-25C-06：角色只由子命令判定，
// 不新增 `role` 配置键；零值必须是 worker（未判定即失败关闭）。
// The role comes from the subcommand only, and the zero value is the fail-closed side.
func TestRuntimeRole_RoleComesFromSubcommand(t *testing.T) {
	t.Parallel()

	require.Equal(t, roleControl, roleForCommand("serve"),
		"serve 是控制面：唯一执行共享初始化写的角色")
	require.Equal(t, roleWorker, roleForCommand("gateway"),
		"gateway 是 Worker：只做只读装配与执行协调写")
	require.Equal(t, roleWorker, runtimeRole(0),
		"runtimeRole 的零值必须是 worker，未判定的角色不得默认拿到定义写权")

	// 签名对号：newRuntime 必须接收角色（§3.1.5 冻结字面量）。
	var _ func(*App, context.Context, runtimeRole) (*heavyRuntime, error) = (*App).newRuntime
}

// readAppProductionSources 拼接 internal/app 生产源码（不含测试文件与角色门声明文件）：
// 判"装配面是否真的问了角色门"，因此声明文件本身不计入。
// readAppProductionSources concatenates production sources, excluding the gate
// declaration file itself so the assertion cannot be satisfied by the declarations.
func readAppProductionSources(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	var builder strings.Builder
	scanned := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "worker_authority.go" {
			continue
		}
		body, readErr := os.ReadFile(file)
		require.NoError(t, readErr)
		builder.Write(body)
		builder.WriteString("\n")
		scanned++
	}
	require.NotZero(t, scanned, "必须至少扫描到一个生产源码文件")
	return builder.String()
}
