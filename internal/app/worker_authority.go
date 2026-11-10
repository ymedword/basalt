package app

// 本文件是 US-25C §2.3.1 的**运行期角色门**：同一份启动链在 control（`serve`）与
// Worker（`gateway`）上执行的行为不同——Worker 只做只读装配与合法的执行协调写，
// **不得**执行任何共享初始化写（FR-25C-12）。角色只由子命令判定（D-25C-06），
// **不新增** `role` 配置键；`runtimeRole` 的零值是 Worker，未判定的角色拿不到定义写权。
//
// `sharedInitItems` 是 §2.3.1 表的唯一清单：装配面**逐项**问门
// （`role.allowSharedInit(...)`），判据 `worker_authority_test.go` 另有源码级
// 守护断言要求每一项都在生产装配文件中被引用——声明了却没人问等于没有门。
//
// This file is the runtime role gate for US-25C §2.3.1: workers perform read-only
// assembly and legitimate execution-coordination writes only, and must skip every
// shared-initialization write (FR-25C-12). The role is derived from the subcommand
// alone; the zero value is the fail-closed side.

// sharedInitItem 标识 §2.3.1 表里的一项共享初始化写。取值顺序即模块卡表格顺序，
// 测试 `worker_authority_test.go` 逐项对号，缺项、换序或改名都会红。
// sharedInitItem names one shared-initialization write; the order mirrors the module
// card table verbatim.
type sharedInitItem int

const (
	sharedInitBaseline            sharedInitItem = iota // 基线回填
	sharedInitOperationRecovery                         // 管理操作回填
	sharedInitDefinitionReconcile                       // 定义协调
	sharedInitStartupBindings                           // 启动绑定重建
	sharedInitMCPApplied                                // MCP applied 指针写
	sharedInitSecretsEnsure                             // 主密钥 Ensure
	sharedInitPolicySeed                                // 四类策略种子写
	sharedInitBuiltinOpenBox                            // 开箱 Agent 安装
	sharedInitAssetBootstrap                            // 本地资产补建
	sharedInitTaskDefinitions                           // 调度定义列写
	sharedInitMCPCredentialSource                       // MCP 来源补记
	sharedInitLearningRecovery                          // 学习回填
	sharedInitKnowledgeGeneration                       // 知识代际 Ensure
	sharedInitPayloadBackfill                           // 载荷回填（US-25C 新增）
	sharedInitSkillRepair                               // 技能启动兜底修复（US-25C 新增）
)

// sharedInitNames 是条目到模块卡固定名字的映射；名字进日志与测试诊断，**不得**随意改。
// #nosec G101 -- 冻结的日志名（模块卡字面量），不是凭据；仅因词形含 "secrets" 命中。
// sharedInitNames maps each item to its frozen module-card name.
var sharedInitNames = map[sharedInitItem]string{
	sharedInitBaseline:            "baseline",
	sharedInitOperationRecovery:   "operation_recovery",
	sharedInitDefinitionReconcile: "definition_reconcile",
	sharedInitStartupBindings:     "startup_bindings",
	sharedInitMCPApplied:          "mcp_applied",
	sharedInitSecretsEnsure:       "secrets_ensure",
	sharedInitPolicySeed:          "policy_seed",
	sharedInitBuiltinOpenBox:      "builtin_open_box",
	sharedInitAssetBootstrap:      "asset_bootstrap",
	sharedInitTaskDefinitions:     "task_definitions",
	sharedInitMCPCredentialSource: "mcp_credential_source",
	sharedInitLearningRecovery:    "learning_recovery",
	sharedInitKnowledgeGeneration: "knowledge_generation",
	sharedInitPayloadBackfill:     "payload_backfill",
	sharedInitSkillRepair:         "skill_repair",
}

// sharedInitItems 按模块卡顺序返回全部共享初始化写条目。
// sharedInitItems returns every shared-initialization item in module-card order.
func sharedInitItems() []sharedInitItem {
	return []sharedInitItem{
		sharedInitBaseline,
		sharedInitOperationRecovery,
		sharedInitDefinitionReconcile,
		sharedInitStartupBindings,
		sharedInitMCPApplied,
		sharedInitSecretsEnsure,
		sharedInitPolicySeed,
		sharedInitBuiltinOpenBox,
		sharedInitAssetBootstrap,
		sharedInitTaskDefinitions,
		sharedInitMCPCredentialSource,
		sharedInitLearningRecovery,
		sharedInitKnowledgeGeneration,
		sharedInitPayloadBackfill,
		sharedInitSkillRepair,
	}
}

// sharedInitItemName 返回条目的模块卡固定名字。未知条目返回 `unknown`——只用于诊断，
// 不参与任何判定。
// sharedInitItemName returns the frozen name; unknown items never drive decisions.
func sharedInitItemName(item sharedInitItem) string {
	if name, ok := sharedInitNames[item]; ok {
		return name
	}
	return "unknown"
}

// allowSharedInit 是角色门的唯一判定：control 执行全部共享初始化写，Worker 一项不做。
// 这里**不设**按条目的例外——"部分保留"正是 FR-25C-12 要关闭的形态。
// allowSharedInit is the single decision point: control runs every shared write,
// the worker runs none; per-item exceptions are deliberately absent.
func (role runtimeRole) allowSharedInit(item sharedInitItem) bool {
	return role == roleControl
}

// roleForCommand 按子命令判定运行期角色（D-25C-06）：`serve` 是控制面，`gateway`
// 是 Worker；其余命令走 Worker——没判明的角色**不得**默认拿到定义写权。
// roleForCommand derives the role from the subcommand; anything unrecognized stays
// on the fail-closed side.
func roleForCommand(command string) runtimeRole {
	if command == "serve" {
		return roleControl
	}
	return roleWorker
}
