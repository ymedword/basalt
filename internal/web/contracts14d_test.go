package web

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// backticked 取 markdown 行内反引号包裹的字面量。
// backticked matches inline code spans inside a markdown line.
var backticked = regexp.MustCompile("`([^`\n]+)`")

// readContractText 读取 docs/contracts 下的非 YAML 契约文件（run-and-errors.md）原文；
// 缺文件即失败。
// readContractText reads a markdown contract file; a missing file fails the test.
func readContractText(t *testing.T, name string) string {
	t.Helper()
	root, err := os.OpenRoot("../../docs/contracts")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	raw, err := root.ReadFile(name)
	require.NoError(t, err)
	return string(raw)
}

// contractBulletTokens 取某条字面量清单里的全部反引号取值：跳过字段名标签本身与
// 形如 `<resource>.<verb>` 的占位式（它是语法说明，不是字面量）。
// contractBulletTokens lists the inline literals of one bullet, skipping the field-name
// label and the `<resource>.<verb>` placeholder form.
func contractBulletTokens(t *testing.T, markdown, prefix string) []string {
	t.Helper()
	for line := range strings.SplitSeq(markdown, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		tokens := make([]string, 0, 64)
		for _, match := range backticked.FindAllStringSubmatch(line, -1) {
			token := match[1]
			if token == "action" || token == "target_kind" || strings.ContainsAny(token, "<> ") {
				continue
			}
			tokens = append(tokens, token)
		}
		require.NotEmptyf(t, tokens, "%q 行的字面量清单为空", prefix)
		return tokens
	}
	t.Fatalf("run-and-errors.md 缺少以 %q 开头的字面量行", prefix)
	return nil
}

// TestContracts14D_EnumsMatchGoLiterals 钉住 14D 新增 / 扩展的每个契约闭集与 Go 字面量
// 逐字一致：契约多一个取值 → Go 侧不接受（页面把它当非法丢弃），Go 多一个 → 写入端会
// 发出契约外字面量。两侧任一漂移都红。
// Every closed set introduced or extended by 14D must match its Go literals exactly.
func TestContracts14D_EnumsMatchGoLiterals(t *testing.T) {
	t.Parallel()

	doc := readContractDoc(t, "openapi.yaml")

	// 执行档位：两档写在四处，四处必须是同一闭集（读视图、写请求、前置断言、沙箱投影）。
	profiles := []string{string(core.ProfileStandard), string(core.ProfileStrict)}
	for _, site := range []struct{ schema, field string }{
		{"ExecutionPolicyView", "profile"},
		{"ExecutionPolicyWrite", "profile"},
		{"ExecutionPolicyWrite", "expected_profile"},
		{"SandboxView", "execution_profile"},
	} {
		require.ElementsMatch(t, profiles, contractEnum(t, doc, site.schema, site.field),
			"%s.%s 的执行档位闭集必须等于 core.ExecutionProfile", site.schema, site.field)
	}

	// 实例加密三态与恢复细分原因：`recovery_required` 不得被归并成"未初始化"或"就绪"。
	secretStates := []string{
		string(core.SecretStateUninitialized),
		string(core.SecretStateReady),
		string(core.SecretStateRecoveryRequired),
	}
	require.ElementsMatch(t, secretStates, contractEnum(t, doc, "InstanceSecretsView", "state"),
		"InstanceSecretsView.state 必须等于 core.SecretState")

	recoveryReasons := []string{
		string(core.SecretRecoveryNone),
		string(core.SecretRecoveryMissing),
		string(core.SecretRecoveryCorrupt),
		string(core.SecretRecoveryKeyIDMismatch),
		string(core.SecretRecoveryUnprovableProvenance),
		string(core.SecretRecoveryUnavailable),
		string(core.SecretRecoveryKeyPathMissing),
	}
	require.ElementsMatch(t, recoveryReasons, contractEnum(t, doc, "InstanceSecretsView", "recovery_reason"),
		"InstanceSecretsView.recovery_reason 必须等于 core.SecretRecoveryReason")

	// 逐占位符凭据来源与状态：来源显式声明，不靠"托管行是否存在"推断。
	credentialModes := []string{string(core.CredentialModeManaged), string(core.CredentialModeEnv)}
	require.ElementsMatch(t, credentialModes, contractEnum(t, doc, "McpCredentialKeyView", "mode"),
		"McpCredentialKeyView.mode 必须等于 core.CredentialMode")
	require.ElementsMatch(t, credentialModes, contractEnum(t, doc, "McpCredentialInput", "mode"),
		"McpCredentialInput.mode 必须等于 core.CredentialMode")

	keyStates := []string{
		string(core.MCPCredentialManaged),
		string(core.MCPCredentialManagedRevoked),
		string(core.MCPCredentialEnv),
		string(core.MCPCredentialMissing),
	}
	require.ElementsMatch(t, keyStates, contractEnum(t, doc, "McpCredentialKeyView", "state"),
		"McpCredentialKeyView.state 必须等于 core.MCPCredentialKeyState")

	dependencyStates := []string{
		string(core.MCPDependencyReady),
		string(core.MCPDependencyMissing),
		string(core.MCPDependencyVersionMismatch),
	}
	require.ElementsMatch(t, dependencyStates, contractEnum(t, doc, "McpDependencyDiagnosis", "state"),
		"McpDependencyDiagnosis.state 必须等于 core.MCPDependencyState")

	// 受控 apply：结果三态 + 取代者（空串 = 未被取代）。
	applyOutcomes := []string{
		string(core.MCPApplyApplied),
		string(core.MCPApplySuperseded),
		string(core.MCPApplyFailed),
	}
	require.ElementsMatch(t, applyOutcomes, contractEnum(t, doc, "McpServerApplyResult", "outcome"),
		"McpServerApplyResult.outcome 必须等于 core.MCPApplyOutcome")

	supersededBy := []string{
		string(core.MCPApplySupersededByNone),
		string(core.MCPApplySupersededByPublish),
		string(core.MCPApplySupersededByRevoke),
		string(core.MCPApplySupersededByApply),
	}
	require.ElementsMatch(t, supersededBy, contractEnum(t, doc, "McpServerApplyResult", "superseded_by"),
		"McpServerApplyResult.superseded_by 必须等于 core.MCPApplySupersededBy")

	// 生效来源扩到四值：白名单 / 禁止域名只出前两者，档位与实例初始化各占一个。
	sources := []string{
		core.NetworkAllowlistSourceConfigSeed,
		core.NetworkAllowlistSourceDatabase,
		core.NetworkAllowlistSourceMigration,
		core.NetworkAllowlistSourceInstanceInit,
	}
	require.ElementsMatch(t, sources, contractEnum(t, doc, "NetworkAllowlistView", "source"),
		"NetworkAllowlistView.source 的四个来源必须与 core 字面量一致")

	// 升级迁移待确认项：两轴与两原因各自闭集，不合并成一个"策略问题"。
	axes := []string{string(core.PendingAxisNetworkAllowlist), string(core.PendingAxisShellCommand)}
	require.ElementsMatch(t, axes, contractEnum(t, doc, "PendingPolicyConfirmation", "axis"),
		"PendingPolicyConfirmation.axis 必须等于 core.PendingConfirmationAxis")

	pendingReasons := []string{
		string(core.PendingReasonProtectedAddress),
		string(core.PendingReasonShellName),
	}
	require.ElementsMatch(t, pendingReasons, contractEnum(t, doc, "PendingPolicyConfirmation", "reason"),
		"PendingPolicyConfirmation.reason 必须等于 core.PendingConfirmationReason")

	// 诊断的 stage 与 failure_class 由页面判卷函数把关：候选集含常见错值，
	// 两侧任一多收 / 少收都红。空串是**合法**的 failure_class（成功）。
	testStages := contractEnum(t, doc, "McpServerTestResult", "stage")
	require.ElementsMatch(t, testStages,
		acceptedBy([]string{"", "preflight", "spawn", "initialize", "tools_list", "complete", "connect", "done"}, isMCPTestStage),
		"McpServerTestResult.stage 的接受集合必须等于契约枚举")

	testClasses := contractEnum(t, doc, "McpServerTestResult", "failure_class")
	require.ElementsMatch(t, testClasses,
		acceptedBy([]string{
			"", "command_not_found", "permission_denied", "policy_denied", "timeout",
			"http_unauthorized", "http_unreachable", "handshake_failed", "tools_list_failed",
			"env_missing", "dependency_unconfirmed", "drain_timeout", "ok", "failed",
		}, isMCPTestFailureClass),
		"McpServerTestResult.failure_class 的接受集合必须等于契约枚举")

	// apply 的失败分类复用测试闭集并**追加** drain_timeout：它不是连接失败，
	// 而是切换已生效、旧连接排空超窗。
	applyClasses := contractEnum(t, doc, "McpServerApplyResult", "failure_class")
	require.ElementsMatch(t, append(slices.Clone(testClasses), core.MCPApplyFailureDrainTimeout), applyClasses,
		"McpServerApplyResult.failure_class = 测试闭集 + drain_timeout")
	applyCandidates := append(slices.Clone(applyClasses), "drain", "drain_timeout ", "superseded")
	require.ElementsMatch(t, applyClasses, acceptedBy(applyCandidates, isMCPTestFailureClassWithDrain),
		"apply 失败分类的接受集合必须等于契约枚举")

	// run-and-errors.md §9.3 是写入端的字面量表：动作与目标类型必须与契约同名同值，
	// 否则操作行会写出读侧无法过滤的值。
	errorsDoc := readContractText(t, "run-and-errors.md")

	actions := contractBulletTokens(t, errorsDoc, "- `action` 字面量")
	sorted := slices.Sorted(slices.Values(actions))
	require.Equal(t, sorted, slices.Compact(slices.Clone(sorted)),
		"§9.3 的动作字面量表不得有重复项")
	require.Subset(t, actions, []string{
		core.ActionSystemSecretsInit,
		core.ActionMCPServerApply,
		core.ActionMCPServerCredentialRevoke,
		core.ActionNetworkDenylistUpdate,
		core.ActionNetworkExceptionCreate,
		core.ActionNetworkExceptionDelete,
		core.ActionExecutionPolicyUpdate,
		core.ActionExecutionPolicyConfirm,
	}, "14D 的八个新动作必须逐字出现在 §9.3")

	targetKinds := contractBulletTokens(t, errorsDoc, "- `target_kind` 字面量")
	require.ElementsMatch(t, contractEnum(t, doc, "OperationView", "target_kind"), targetKinds,
		"§9.3 的 target_kind 表必须与 openapi 的 OperationView.target_kind 一致")
	require.Subset(t, targetKinds, []string{
		core.TargetKindSystem,
		core.TargetKindNetworkDenylist,
		core.TargetKindNetworkException,
		core.TargetKindExecutionPolicy,
	}, "14D 新增的四个 target_kind 必须逐字出现在 §9.3")
}

// isMCPTestFailureClassWithDrain 在测试闭集之上追加 apply 专属的 drain_timeout；
// 两者不合并成一个函数，测试失败分类不得接受 drain_timeout。
// isMCPTestFailureClassWithDrain extends the test set with apply's drain_timeout only.
func isMCPTestFailureClassWithDrain(class string) bool {
	return class == core.MCPApplyFailureDrainTimeout || isMCPTestFailureClass(class)
}
