package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// historicalStagePins 固定三个历史 stage 的内容哈希（按 LF 归一）。
// 集合固定为三个文件，不得 glob 目录——否则 current.json 与 us15-*.json 会被误当成历史。
// historicalStagePins freezes the three historical stage files (LF-normalized).
// The set is explicit; globbing would misclassify current.json and the us15 stages.
var historicalStagePins = map[string]string{
	"harness/stages/us8-notify-r1.json":  "85fcb3ebaa1b6615d977ac4457ceaadfdb473d07e1a0b9d1d3d1e127162c2f91",
	"harness/stages/us9-web.json":        "1ff4c036e178f30e01aa5d307c32b6d21ab7b5a3979b8972f025edcc8202a412",
	"harness/stages/us10-scheduler.json": "42670059a5f0ba1839fd37888f44ad32996ab4bc70233485dec16ded6bab444d",
}

// stageTestRoot 返回仓库根；测试二进制的工作目录是 scripts/acceptance。
// stageTestRoot returns the repository root; the test binary runs in scripts/acceptance.
func stageTestRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	return root
}

func readStageForTest(t *testing.T, root, rel string) stage {
	t.Helper()
	var s stage
	require.NoError(t, readJSON(filepath.Join(root, rel), &s), "read %s", rel)
	return s
}

func stageChecksByID(s stage) map[string]check {
	out := make(map[string]check, len(s.Checks))
	for _, c := range s.Checks {
		out[c.ID] = c
	}
	return out
}

// normalizeLevel 把缺省 level 归一为 source，供逐字段比较。
// normalizeLevel folds the implicit source level into the comparable value.
func normalizeLevel(c check) check {
	c.Level = checkLevel(c)
	return c
}

// TestStages_CurrentCoversHistorical 历史 check 不被静默丢失，US-8 替代项必须显式登记。
// TestStages_CurrentCoversHistorical keeps every historical check present and makes the
// US-8 replacement explicit through superseded_checks.
func TestStages_CurrentCoversHistorical(t *testing.T) {
	root := stageTestRoot(t)
	current := readStageForTest(t, root, "harness/stages/current.json")
	currentByID := stageChecksByID(current)

	for rel := range historicalStagePins {
		historical := readStageForTest(t, root, rel)
		for _, old := range historical.Checks {
			replacement, superseded := current.SupersededChecks[old.ID]
			if superseded {
				require.Contains(t, currentByID, replacement, "%s: replacement %s for %s is missing", rel, replacement, old.ID)
				require.NotContains(t, currentByID, old.ID, "%s: superseded check %s must not be copied verbatim", rel, old.ID)
				continue
			}
			require.Contains(t, currentByID, old.ID, "%s: historical check %s missing from current.json", rel, old.ID)
		}
	}

	require.Equal(t, map[string]string{"008-ports-nine": "008-ports-ten"}, current.SupersededChecks,
		"current.json must register exactly the explicit US-8 replacement")
	require.Contains(t, currentByID, "008-ports-ten")
}

// TestStages_CurrentMatchesHistorical 非替代项与历史定义逐字段相等；替代项只校验替代目标与测试名。
// TestStages_CurrentMatchesHistorical requires field-by-field equality for every
// non-superseded check; a superseded entry only binds its replacement and test name.
func TestStages_CurrentMatchesHistorical(t *testing.T) {
	root := stageTestRoot(t)
	current := readStageForTest(t, root, "harness/stages/current.json")
	currentByID := stageChecksByID(current)

	for rel := range historicalStagePins {
		historical := readStageForTest(t, root, rel)
		for _, old := range historical.Checks {
			replacement, superseded := current.SupersededChecks[old.ID]
			if superseded {
				got, ok := currentByID[replacement]
				require.True(t, ok, "%s: replacement %s is missing", rel, replacement)
				require.Equal(t, []string{"TestToolListerPort_HeavyAssemblyRegistersTenTools"}, got.RequiredTests,
					"%s: replacement %s must bind exactly the current test", rel, replacement)
				require.Equal(t, "source", checkLevel(got))
				continue
			}
			got, ok := currentByID[old.ID]
			require.True(t, ok, "%s: historical check %s missing from current.json", rel, old.ID)
			require.Equal(t, normalizeLevel(old), normalizeLevel(got),
				"%s: %s drifted from its historical definition", rel, old.ID)
		}
	}
}

// TestStages_HistoricalFilesPinned 历史 stage 任一字节改动都会变红（按 LF 归一哈希）。
// TestStages_HistoricalFilesPinned fails on any byte change in the three frozen stages.
func TestStages_HistoricalFilesPinned(t *testing.T) {
	root := stageTestRoot(t)
	for rel, want := range historicalStagePins {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err)
		normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
		sum := sha256.Sum256([]byte(normalized))
		require.Equal(t, want, hex.EncodeToString(sum[:]), "%s must stay byte-identical (LF normalized)", rel)
	}
}

// pinnedStageCheck 是 015-* 与 US-8 替代项的静态定义期望表。
// pinnedStageCheck is the static expectation table for the 015-* checks and 008-ports-ten.
type pinnedStageCheck struct {
	stage   string
	id      string
	args    []string
	timeout int
	level   string
}

var pinnedStageChecks = []pinnedStageCheck{
	{
		stage: "harness/stages/current.json",
		id:    "015-acceptance-infra",
		args: []string{
			"go", "test", "-json", "-count=1", "./scripts/acceptance", "-run",
			"^(TestRecord_|TestStageLint_|TestStages_|TestRecords_|TestLedger_|TestFingerprint_|TestAccept_ReportPathsRelative|TestAccept_ExternalRequiresAuthorizationFlags)",
		},
		timeout: 300,
		level:   "source",
	},
	{
		stage:   "harness/stages/current.json",
		id:      "015-invariants",
		args:    []string{"go", "test", "-json", "-count=1", "./scripts/acceptance", "-run", "^TestRepoInvariants_"},
		timeout: 180,
		level:   "source",
	},
	{
		stage: "harness/stages/current.json",
		id:    "015-backfill",
		args: []string{
			"go", "test", "-json", "-count=1", "-tags=e2e", "./internal/e2e", "-run",
			"^(TestBackfill_Bootstrap|TestBackfill_AgentLoop|TestBackfill_ToolSandbox|TestBackfill_Memory|TestBackfill_MCP|TestBackfill_Notify|TestBackfill_Web|TestBackfill_CrossProcessLock|TestBackfill_RestartRecovery|TestBackfill_Scheduler|TestBackfill_ConsolePipe)$",
		},
		timeout: 600,
		level:   "process",
	},
	{
		stage:   "harness/stages/current.json",
		id:      "015-e2e-vet",
		args:    []string{"go", "vet", "-tags=e2e,slow,real", "./internal/e2e"},
		timeout: 180,
		level:   "source",
	},
	{
		stage:   "harness/stages/us15-linux.json",
		id:      "015-signal-linux",
		args:    []string{"go", "test", "-json", "-count=1", "-tags=e2e", "./internal/e2e", "-run", "^TestBackfill_SignalLinux$"},
		timeout: 240,
		level:   "process",
	},
	{
		stage:   "harness/stages/us15-slow.json",
		id:      "015-slow-timeout",
		args:    []string{"go", "test", "-json", "-count=1", "-tags=e2e slow", "./internal/e2e", "-run", "^TestBackfill_SlowTimeout$"},
		timeout: 600,
		level:   "process",
	},
	{
		stage:   "harness/stages/us15-real.json",
		id:      "015-real-model",
		args:    []string{"go", "test", "-json", "-count=1", "-tags=e2e real", "./internal/e2e", "-run", "^TestBackfill_Real$"},
		timeout: 900,
		level:   "external",
	},
	{
		stage:   "harness/stages/us15-real.json",
		id:      "015-real-feishu",
		args:    []string{"go", "test", "-json", "-count=1", "-tags=e2e real", "./internal/e2e", "-run", "^TestBackfill_RealFeishu$"},
		timeout: 180,
		level:   "external",
	},
	{
		stage:   "harness/stages/current.json",
		id:      "008-ports-ten",
		args:    []string{"go", "test", "-json", "-count=1", "./internal/app", "-run", "^TestToolListerPort_HeavyAssemblyRegistersTenTools$"},
		timeout: 300,
		level:   "source",
	},
}

// pinnedRequiredTests 只钉住可静态列举的 required_tests；其余由目录一致性测试钉住。
// pinnedRequiredTests fixes only statically enumerable required_tests; the rest are
// pinned by the catalog consistency tests.
var pinnedRequiredTests = map[string][]string{
	"015-acceptance-infra": {
		"TestRecord_BuildFromReport",
		"TestRecord_BuildFromSourceChanged",
		"TestRecord_ValidateRequiredFields",
		"TestRecord_ValidateNotVerified",
		"TestRecord_OptionsDefaults",
		"TestRecord_ValidateRejectsNAWithoutBasis",
		"TestRecord_ValidateRejectsAbsolutePath",
		"TestRecord_ValidateRejectsSecret",
		"TestRecord_ValidateRedStateRequiresAppliesTo",
		"TestRecord_ExternalAuthorization",
		"TestStageLint_TransformListArgs",
		"TestStageLint_DetectsMissingTest",
		"TestStages_CurrentCoversHistorical",
		"TestStages_CurrentMatchesHistorical",
		"TestStages_HistoricalFilesPinned",
		"TestStages_BackfillCatalogMatchesRequiredTests",
		"TestStages_BackfillCatalogCoversRequiredSet",
		"TestStages_RealCatalogCoversAuthorizedSet",
		"TestStages_PinsUS15Checks",
		"TestRecords_BackfillCoversCatalog",
		"TestRecords_BackfillBindsToExecution",
		"TestRecords_RealBindsToExecution",
		"TestRecords_StatusDoesNotMaskConflicts",
		"TestRecords_PublishedConform",
		"TestRecords_NoSecretsOrAbsolutePaths",
		"TestLedger_IndexesAllRecords",
		"TestFingerprint_IgnoresAcceptanceEvidence",
		"TestAccept_ReportPathsRelative",
		"TestAccept_ExternalRequiresAuthorizationFlags",
	},
	"015-invariants": {
		"TestRepoInvariants_NoForbiddenAgentFramework",
		"TestRepoInvariants_NoProcessLocalTimezone",
		"TestRepoInvariants_NoPlaintextCredential",
		"TestRepoInvariants_NoORM",
		"TestRepoInvariants_TestOnlyDependencyNotLinked",
		"TestRepoInvariants_CIRunDirsUnique",
	},
	"015-e2e-vet":   nil,
	"008-ports-ten": {"TestToolListerPort_HeavyAssemblyRegistersTenTools"},
}

// TestStages_PinsUS15Checks 钉住八个 015-* check 与 US-8 替代项的 argv / 超时 / 层级，
// 以及可静态列举的 required_tests，防止新 check 被静默删项、去标签或改命令行。
// 该测试在 us15-linux / us15-slow / us15-real 三个 stage 落地后才转绿（T035 / T047）。
// TestStages_PinsUS15Checks pins argv, timeout and level for the eight 015-* checks and
// the US-8 replacement, plus the statically enumerated required_tests. It turns green
// once the us15-linux / us15-slow / us15-real stages exist (T035 / T047).
func TestStages_PinsUS15Checks(t *testing.T) {
	root := stageTestRoot(t)
	loaded := map[string]stage{}
	for _, pin := range pinnedStageChecks {
		s, ok := loaded[pin.stage]
		if !ok {
			s = readStageForTest(t, root, pin.stage)
			loaded[pin.stage] = s
		}
		byID := stageChecksByID(s)
		got, ok := byID[pin.id]
		require.True(t, ok, "%s: check %s is missing", pin.stage, pin.id)
		require.Equal(t, pin.args, got.Args, "%s: check %s argv changed", pin.stage, pin.id)
		require.Equal(t, pin.timeout, got.TimeoutSeconds, "%s: check %s timeout changed", pin.stage, pin.id)
		require.Equal(t, pin.level, checkLevel(got), "%s: check %s level changed", pin.stage, pin.id)
	}
	all := map[string]check{}
	for _, s := range loaded {
		for id, c := range stageChecksByID(s) {
			all[id] = c
		}
	}
	for id, want := range pinnedRequiredTests {
		got, ok := all[id]
		require.True(t, ok, "check %s is missing", id)
		require.Equal(t, want, got.RequiredTests, "check %s required_tests changed", id)
	}
}

// catalogEntry 是回填目录的一项：历史子判据 → 现行 check 的具名测试。
// 字母按模块卡 §2.6 / §2.5.2 的拆分顺序计数——未进入目录的拆分（丙类、
// 未验证、不适用、门禁载体）同样占字母，因此部分字母会跳号。
// catalogEntry maps one historical sub-criterion to a named test of a current
// check. Letters follow the module card's split order; splits that stay out of
// the catalog (human judgement, unverified, not applicable, gate carriers)
// still consume a letter, so gaps are expected.
type catalogEntry struct {
	Item  string
	Test  string
	Check string
	Level string
}

func (e catalogEntry) String() string {
	return fmt.Sprintf("%s -> %s (%s/%s)", e.Item, e.Test, e.Check, e.Level)
}

// catalogItemPattern 是模块卡冻结的编号正则，同时按 US 分段强制子节形态。
// catalogItemPattern is the frozen item numbering regex; per-US segments also
// enforce the subsection form required by the module card.
var catalogItemPattern = regexp.MustCompile(`^us(?:10-6\.2-\d{1,2}[a-z]|[123]-6-\d{1,3}[a-z]|[4-9]-6\.\d+-\d{1,3}[a-z]|\d{1,2}-rsU[1-4]-\d{1,3}[a-z])$`)

// backfillCatalog 逐项转写模块卡 §2.6 与 §2.5.2：item / test / check / level。
// 目录同时收录可执行的甲类进程级项与已授权外部项；丙类、未授权乙类、一次性
// 与门禁 / CI 承载项不在此列。US-10 第 10 项的两个子测试中，目录以
// gateway_lifecycle 承载该 item，chat_no_trigger 是同 check 报告内的附加断言。
// test 字段是 `go test -json` 的 PASS 事件全名：有子测试写 Parent/sub，无子测试
// 写 Parent；各 stage 的 required_tests 必须逐字收录这些名字（模块卡 §2.6）。
// backfillCatalog transcribes module card sections 2.6 and 2.5.2 verbatim.
// The test field is the full PASS-event name (Parent/sub when the case has
// subtests); required_tests in every stage must list these names literally.
var backfillCatalog = []catalogEntry{
	// US-1 §6：表格数据行 1~9；行 2 Demo①~⑤ 字母 a~e，行 5 探针①/② 字母 a/b。
	{Item: "us1-6-2b", Test: "TestBackfill_Bootstrap/init_idempotent", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-2c", Test: "TestBackfill_Bootstrap/status_no_config", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-2d", Test: "TestBackfill_Bootstrap/version_injection", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-3a", Test: "TestBackfill_Bootstrap/negative_three", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-5a", Test: "TestBackfill_RestartRecovery/crash_recovery", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-5b", Test: "TestBackfill_CrossProcessLock/instance_lock", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-7a", Test: "TestBackfill_Bootstrap/credential_scan", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-8a", Test: "TestBackfill_Bootstrap/assembly", Check: "015-backfill", Level: "process"},
	// US-2 §6：行 3 Demo①~⑥ 字母 a~f（a 真实调用为乙，d/e 共用 provider_negative）。
	{Item: "us2-6-3a", Test: "TestBackfill_Real/provider_call", Check: "015-real-model", Level: "external"},
	{Item: "us2-6-3c", Test: "TestBackfill_Bootstrap/provider_unreachable_audit", Check: "015-backfill", Level: "process"},
	{Item: "us2-6-3d", Test: "TestBackfill_Bootstrap/provider_negative", Check: "015-backfill", Level: "process"},
	{Item: "us2-6-3e", Test: "TestBackfill_Bootstrap/provider_negative", Check: "015-backfill", Level: "process"},
	{Item: "us2-6-4a", Test: "TestBackfill_Bootstrap/credential_scan", Check: "015-backfill", Level: "process"},
	{Item: "us2-6-5a", Test: "TestBackfill_Bootstrap/assembly", Check: "015-backfill", Level: "process"},
	{Item: "us2-6-6a", Test: "TestBackfill_Bootstrap", Check: "015-backfill", Level: "process"},
	// US-3 §6：原文 # 列 1~8。
	{Item: "us3-6-2a", Test: "TestBackfill_Bootstrap/profile", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-2b", Test: "TestBackfill_Bootstrap/bad_agents", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-2c", Test: "TestBackfill_AgentLoop/skill_l1", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-4a", Test: "TestBackfill_CrossProcessLock/write_mode", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-5a", Test: "TestBackfill_CrossProcessLock/platform_lock", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-6a", Test: "TestBackfill_Bootstrap/credential_scan", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-7a", Test: "TestBackfill_Bootstrap/cli_import_graph", Check: "015-backfill", Level: "process"},
	// US-4 §6.1：第 2 项 Demo①甲/乙(a/b)、②甲/乙(c/d)、③甲(e)、④甲(g)、⑤甲/乙(i/j)、
	// ⑦甲(l)；未验证半与 Demo⑥ 占字母但不进目录。第 4 项 丙(a)/甲(b)。
	{Item: "us4-6.1-2a", Test: "TestBackfill_AgentLoop/multiturn_identity", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-2b", Test: "TestBackfill_Real/multiturn", Check: "015-real-model", Level: "external"},
	{Item: "us4-6.1-2c", Test: "TestBackfill_AgentLoop/read_file", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-2d", Test: "TestBackfill_Real/chat_tool", Check: "015-real-model", Level: "external"},
	{Item: "us4-6.1-2e", Test: "TestBackfill_AgentLoop/sandbox_denied_audit", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-2g", Test: "TestBackfill_AgentLoop/reasoning_roundtrip", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-2i", Test: "TestBackfill_RestartRecovery/session_history", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-2j", Test: "TestBackfill_Real/restart_session", Check: "015-real-model", Level: "external"},
	{Item: "us4-6.1-2l", Test: "TestBackfill_AgentLoop/injection_denied", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-3a", Test: "TestBackfill_AgentLoop/reasoning_required", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-4b", Test: "TestBackfill_ConsolePipe", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-6a", Test: "TestBackfill_ToolSandbox/process_tree", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-7a", Test: "TestBackfill_Bootstrap/assembly", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-10a", Test: "TestBackfill_AgentLoop/redaction", Check: "015-backfill", Level: "process"},
	// US-5 §6.1：原文 # 列 1~12。
	{Item: "us5-6.1-2a", Test: "TestBackfill_Real/weather_tool", Check: "015-real-model", Level: "external"},
	{Item: "us5-6.1-3a", Test: "TestBackfill_ToolSandbox/sandbox_denied", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-4a", Test: "TestBackfill_Bootstrap/tool_list", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-5a", Test: "TestBackfill_ToolSandbox/shell", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-6a", Test: "TestBackfill_ToolSandbox/process_tree", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-9a", Test: "TestBackfill_AgentLoop/redaction", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-11b", Test: "TestBackfill_ConsolePipe", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-12a", Test: "TestBackfill_ToolSandbox/port_redaction", Check: "015-backfill", Level: "process"},
	// US-6 §6.1：原文 # 列 1~13；行 2 甲/乙、行 3 两档、行 6 丙/甲。
	{Item: "us6-6.1-2a", Test: "TestBackfill_Memory/demo_core", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-2b", Test: "TestBackfill_Real/memory_save", Check: "015-real-model", Level: "external"},
	{Item: "us6-6.1-3a", Test: "TestBackfill_Memory/demo_markdown", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-3b", Test: "TestBackfill_Memory/demo_sqlite", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-4a", Test: "TestBackfill_Memory/edit_timing", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-5a", Test: "TestBackfill_Memory/quota", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-6b", Test: "TestBackfill_ConsolePipe", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-7a", Test: "TestBackfill_Memory/concurrency", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-9a", Test: "TestBackfill_Memory/redaction", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-11a", Test: "TestBackfill_Bootstrap/tool_list", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-13a", Test: "TestBackfill_Memory/archive_size", Check: "015-backfill", Level: "process"},
	// US-6 run-state U-1 清单 E5 / E8。
	{Item: "us6-rsU1-5a", Test: "TestBackfill_Memory/scopes", Check: "015-backfill", Level: "process"},
	{Item: "us6-rsU1-8a", Test: "TestBackfill_Memory/corrupt", Check: "015-backfill", Level: "process"},
	// US-7 §6.1：原文 # 列 1~13；行 2 甲/乙/未验证、行 9 丙/甲。
	{Item: "us7-6.1-2a", Test: "TestBackfill_MCP/zero_code", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-2b", Test: "TestBackfill_Real/mcp_tool", Check: "015-real-model", Level: "external"},
	{Item: "us7-6.1-3a", Test: "TestBackfill_MCP/bad_command", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-4a", Test: "TestBackfill_MCP/two_paths", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-5a", Test: "TestBackfill_MCP/tool_list", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-6a", Test: "TestBackfill_MCP/env_negative", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-7a", Test: "TestBackfill_MCP/shutdown", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-9b", Test: "TestBackfill_ConsolePipe", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-10a", Test: "TestBackfill_MCP/redaction", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-13a", Test: "TestBackfill_MCP/concurrency", Check: "015-backfill", Level: "process"},
	// US-7 run-state U-3 quickstart S1~S5；S1 甲/乙/未验证。
	{Item: "us7-rsU3-1a", Test: "TestBackfill_MCP/zero_code", Check: "015-backfill", Level: "process"},
	{Item: "us7-rsU3-1b", Test: "TestBackfill_Real/mcp_tool", Check: "015-real-model", Level: "external"},
	{Item: "us7-rsU3-2a", Test: "TestBackfill_MCP/bad_command", Check: "015-backfill", Level: "process"},
	{Item: "us7-rsU3-3a", Test: "TestBackfill_MCP/env_negative", Check: "015-backfill", Level: "process"},
	{Item: "us7-rsU3-4a", Test: "TestBackfill_MCP/two_paths", Check: "015-backfill", Level: "process"},
	{Item: "us7-rsU3-5a", Test: "TestBackfill_MCP/shutdown", Check: "015-backfill", Level: "process"},
	// US-8 §6.1：原文 # 列 1~14；行 7 甲/乙。
	{Item: "us8-6.1-4a", Test: "TestBackfill_Notify/channel_registration", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-5a", Test: "TestBackfill_RealFeishu/send", Check: "015-real-feishu", Level: "external"},
	{Item: "us8-6.1-7a", Test: "TestBackfill_Notify/sentinel_redaction", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-7b", Test: "TestBackfill_RealFeishu/redaction", Check: "015-real-feishu", Level: "external"},
	{Item: "us8-6.1-8a", Test: "TestBackfill_Bootstrap/tool_list", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-9a", Test: "TestBackfill_Notify/agent_decl", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-10a", Test: "TestBackfill_Notify/domain_denied", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-11a", Test: "TestBackfill_Notify/env_negative", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-13a", Test: "TestBackfill_Notify/effect_timing", Check: "015-backfill", Level: "process"},
	// US-9 §6.1：原文 # 列 1~15；行 6 甲/未验证（平台限制），行 15 为门禁载体不进目录。
	{Item: "us9-6.1-1a", Test: "TestBackfill_Web/listen_addr", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-2a", Test: "TestBackfill_Web/body_byte_limit", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-3a", Test: "TestBackfill_Web/concurrency_cap", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-4a", Test: "TestBackfill_Web/session_409", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-5a", Test: "TestBackfill_Web/archive_restart", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-6a", Test: "TestBackfill_SignalLinux/grace_exhausted", Check: "015-signal-linux", Level: "process"},
	{Item: "us9-6.1-7a", Test: "TestBackfill_Web/cross_origin", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-8a", Test: "TestBackfill_Web/archive_inflight", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-9a", Test: "TestBackfill_Web/redaction", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-10a", Test: "TestBackfill_Web/release_order", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-11a", Test: "TestBackfill_Web/tool_catalog", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-12a", Test: "TestBackfill_Web/session_id", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-13a", Test: "TestBackfill_Web/budget_limit", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-14a", Test: "TestBackfill_Web/credential_face", Check: "015-backfill", Level: "process"},
	// US-9 §6.2：原文 # 列 1~11。
	{Item: "us9-6.2-1a", Test: "TestBackfill_Web/curl_flow", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-2a", Test: "TestBackfill_Web/concurrent_409", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-3a", Test: "TestBackfill_Web/stateless_invoke", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-4a", Test: "TestBackfill_Web/listen_addr_override", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-5a", Test: "TestBackfill_Real/web_invoke_tool", Check: "015-real-model", Level: "external"},
	{Item: "us9-6.2-5b", Test: "TestBackfill_AgentLoop/read_file", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-6a", Test: "TestBackfill_RestartRecovery/serve", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-7a", Test: "TestBackfill_Web/gateway_lifecycle", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-8a", Test: "TestBackfill_CrossProcessLock/heavy_commands", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-9a", Test: "TestBackfill_Web/credential_face", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-10a", Test: "TestBackfill_Web/budget_limit", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-11a", Test: "TestBackfill_Web/cross_origin", Check: "015-backfill", Level: "process"},
	// US-10 §2.5.2：16 项的 W2 拆分；6c 不适用、15b 平台限制、16c 为乙，均按原字母。
	{Item: "us10-6.2-1b", Test: "TestBackfill_Scheduler/cron_fire", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-1c", Test: "TestBackfill_Real/scheduler_fire", Check: "015-real-model", Level: "external"},
	{Item: "us10-6.2-2b", Test: "TestBackfill_Scheduler/manual_run", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-3b", Test: "TestBackfill_Scheduler/two_tables", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-4b", Test: "TestBackfill_Scheduler/agent_self_manage", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-4c", Test: "TestBackfill_Real/scheduler_manage_self", Check: "015-real-model", Level: "external"},
	{Item: "us10-6.2-5b", Test: "TestBackfill_Scheduler/agent_other_denied", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-5c", Test: "TestBackfill_Real/scheduler_manage_other_denied", Check: "015-real-model", Level: "external"},
	{Item: "us10-6.2-6b", Test: "TestBackfill_Scheduler/zone_shanghai", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-7b", Test: "TestBackfill_RestartRecovery/reconcile", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-8b", Test: "TestBackfill_RestartRecovery/missed_fire", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-9b", Test: "TestBackfill_RestartRecovery/crash_recovery", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-10b", Test: "TestBackfill_Web/gateway_lifecycle", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-11b", Test: "TestBackfill_Scheduler/concurrent_mutex", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-12b", Test: "TestBackfill_SlowTimeout/run_timeout", Check: "015-slow-timeout", Level: "process"},
	{Item: "us10-6.2-13b", Test: "TestBackfill_Scheduler/archive_meets_cron", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-14b", Test: "TestBackfill_Scheduler/cross_agent_task_id", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-15a", Test: "TestBackfill_SignalLinux/sigterm_order", Check: "015-signal-linux", Level: "process"},
	{Item: "us10-6.2-16a", Test: "TestBackfill_Bootstrap/credential_scan", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-16b", Test: "TestBackfill_Bootstrap/credential_template", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-16c", Test: "TestBackfill_Real/provider_call", Check: "015-real-model", Level: "external"},
}

// requiredBackfillItems 是甲类冻结全集（source / process），独立于目录与
// stage 定义书写——同时删目录项与其 required_test 时目录 ↔ stage 互验会自洽
// 变绿，只有这份独立清单仍能判红。
// requiredBackfillItems is the frozen process-level set, written independently
// from the catalog and stage files so that deleting a catalog item together
// with its required_test still fails.
var requiredBackfillItems = []catalogEntry{
	{Item: "us1-6-2b", Test: "TestBackfill_Bootstrap/init_idempotent", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-2c", Test: "TestBackfill_Bootstrap/status_no_config", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-2d", Test: "TestBackfill_Bootstrap/version_injection", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-3a", Test: "TestBackfill_Bootstrap/negative_three", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-5a", Test: "TestBackfill_RestartRecovery/crash_recovery", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-5b", Test: "TestBackfill_CrossProcessLock/instance_lock", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-7a", Test: "TestBackfill_Bootstrap/credential_scan", Check: "015-backfill", Level: "process"},
	{Item: "us1-6-8a", Test: "TestBackfill_Bootstrap/assembly", Check: "015-backfill", Level: "process"},
	{Item: "us2-6-3c", Test: "TestBackfill_Bootstrap/provider_unreachable_audit", Check: "015-backfill", Level: "process"},
	{Item: "us2-6-3d", Test: "TestBackfill_Bootstrap/provider_negative", Check: "015-backfill", Level: "process"},
	{Item: "us2-6-3e", Test: "TestBackfill_Bootstrap/provider_negative", Check: "015-backfill", Level: "process"},
	{Item: "us2-6-4a", Test: "TestBackfill_Bootstrap/credential_scan", Check: "015-backfill", Level: "process"},
	{Item: "us2-6-5a", Test: "TestBackfill_Bootstrap/assembly", Check: "015-backfill", Level: "process"},
	{Item: "us2-6-6a", Test: "TestBackfill_Bootstrap", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-2a", Test: "TestBackfill_Bootstrap/profile", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-2b", Test: "TestBackfill_Bootstrap/bad_agents", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-2c", Test: "TestBackfill_AgentLoop/skill_l1", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-4a", Test: "TestBackfill_CrossProcessLock/write_mode", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-5a", Test: "TestBackfill_CrossProcessLock/platform_lock", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-6a", Test: "TestBackfill_Bootstrap/credential_scan", Check: "015-backfill", Level: "process"},
	{Item: "us3-6-7a", Test: "TestBackfill_Bootstrap/cli_import_graph", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-2a", Test: "TestBackfill_AgentLoop/multiturn_identity", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-2c", Test: "TestBackfill_AgentLoop/read_file", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-2e", Test: "TestBackfill_AgentLoop/sandbox_denied_audit", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-2g", Test: "TestBackfill_AgentLoop/reasoning_roundtrip", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-2i", Test: "TestBackfill_RestartRecovery/session_history", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-2l", Test: "TestBackfill_AgentLoop/injection_denied", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-3a", Test: "TestBackfill_AgentLoop/reasoning_required", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-4b", Test: "TestBackfill_ConsolePipe", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-6a", Test: "TestBackfill_ToolSandbox/process_tree", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-7a", Test: "TestBackfill_Bootstrap/assembly", Check: "015-backfill", Level: "process"},
	{Item: "us4-6.1-10a", Test: "TestBackfill_AgentLoop/redaction", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-3a", Test: "TestBackfill_ToolSandbox/sandbox_denied", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-4a", Test: "TestBackfill_Bootstrap/tool_list", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-5a", Test: "TestBackfill_ToolSandbox/shell", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-6a", Test: "TestBackfill_ToolSandbox/process_tree", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-9a", Test: "TestBackfill_AgentLoop/redaction", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-11b", Test: "TestBackfill_ConsolePipe", Check: "015-backfill", Level: "process"},
	{Item: "us5-6.1-12a", Test: "TestBackfill_ToolSandbox/port_redaction", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-2a", Test: "TestBackfill_Memory/demo_core", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-3a", Test: "TestBackfill_Memory/demo_markdown", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-3b", Test: "TestBackfill_Memory/demo_sqlite", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-4a", Test: "TestBackfill_Memory/edit_timing", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-5a", Test: "TestBackfill_Memory/quota", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-6b", Test: "TestBackfill_ConsolePipe", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-7a", Test: "TestBackfill_Memory/concurrency", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-9a", Test: "TestBackfill_Memory/redaction", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-11a", Test: "TestBackfill_Bootstrap/tool_list", Check: "015-backfill", Level: "process"},
	{Item: "us6-6.1-13a", Test: "TestBackfill_Memory/archive_size", Check: "015-backfill", Level: "process"},
	{Item: "us6-rsU1-5a", Test: "TestBackfill_Memory/scopes", Check: "015-backfill", Level: "process"},
	{Item: "us6-rsU1-8a", Test: "TestBackfill_Memory/corrupt", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-2a", Test: "TestBackfill_MCP/zero_code", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-3a", Test: "TestBackfill_MCP/bad_command", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-4a", Test: "TestBackfill_MCP/two_paths", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-5a", Test: "TestBackfill_MCP/tool_list", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-6a", Test: "TestBackfill_MCP/env_negative", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-7a", Test: "TestBackfill_MCP/shutdown", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-9b", Test: "TestBackfill_ConsolePipe", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-10a", Test: "TestBackfill_MCP/redaction", Check: "015-backfill", Level: "process"},
	{Item: "us7-6.1-13a", Test: "TestBackfill_MCP/concurrency", Check: "015-backfill", Level: "process"},
	{Item: "us7-rsU3-1a", Test: "TestBackfill_MCP/zero_code", Check: "015-backfill", Level: "process"},
	{Item: "us7-rsU3-2a", Test: "TestBackfill_MCP/bad_command", Check: "015-backfill", Level: "process"},
	{Item: "us7-rsU3-3a", Test: "TestBackfill_MCP/env_negative", Check: "015-backfill", Level: "process"},
	{Item: "us7-rsU3-4a", Test: "TestBackfill_MCP/two_paths", Check: "015-backfill", Level: "process"},
	{Item: "us7-rsU3-5a", Test: "TestBackfill_MCP/shutdown", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-4a", Test: "TestBackfill_Notify/channel_registration", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-7a", Test: "TestBackfill_Notify/sentinel_redaction", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-8a", Test: "TestBackfill_Bootstrap/tool_list", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-9a", Test: "TestBackfill_Notify/agent_decl", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-10a", Test: "TestBackfill_Notify/domain_denied", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-11a", Test: "TestBackfill_Notify/env_negative", Check: "015-backfill", Level: "process"},
	{Item: "us8-6.1-13a", Test: "TestBackfill_Notify/effect_timing", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-1a", Test: "TestBackfill_Web/listen_addr", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-2a", Test: "TestBackfill_Web/body_byte_limit", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-3a", Test: "TestBackfill_Web/concurrency_cap", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-4a", Test: "TestBackfill_Web/session_409", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-5a", Test: "TestBackfill_Web/archive_restart", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-6a", Test: "TestBackfill_SignalLinux/grace_exhausted", Check: "015-signal-linux", Level: "process"},
	{Item: "us9-6.1-7a", Test: "TestBackfill_Web/cross_origin", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-8a", Test: "TestBackfill_Web/archive_inflight", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-9a", Test: "TestBackfill_Web/redaction", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-10a", Test: "TestBackfill_Web/release_order", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-11a", Test: "TestBackfill_Web/tool_catalog", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-12a", Test: "TestBackfill_Web/session_id", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-13a", Test: "TestBackfill_Web/budget_limit", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.1-14a", Test: "TestBackfill_Web/credential_face", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-1a", Test: "TestBackfill_Web/curl_flow", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-2a", Test: "TestBackfill_Web/concurrent_409", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-3a", Test: "TestBackfill_Web/stateless_invoke", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-4a", Test: "TestBackfill_Web/listen_addr_override", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-5b", Test: "TestBackfill_AgentLoop/read_file", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-6a", Test: "TestBackfill_RestartRecovery/serve", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-7a", Test: "TestBackfill_Web/gateway_lifecycle", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-8a", Test: "TestBackfill_CrossProcessLock/heavy_commands", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-9a", Test: "TestBackfill_Web/credential_face", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-10a", Test: "TestBackfill_Web/budget_limit", Check: "015-backfill", Level: "process"},
	{Item: "us9-6.2-11a", Test: "TestBackfill_Web/cross_origin", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-1b", Test: "TestBackfill_Scheduler/cron_fire", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-2b", Test: "TestBackfill_Scheduler/manual_run", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-3b", Test: "TestBackfill_Scheduler/two_tables", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-4b", Test: "TestBackfill_Scheduler/agent_self_manage", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-5b", Test: "TestBackfill_Scheduler/agent_other_denied", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-6b", Test: "TestBackfill_Scheduler/zone_shanghai", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-7b", Test: "TestBackfill_RestartRecovery/reconcile", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-8b", Test: "TestBackfill_RestartRecovery/missed_fire", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-9b", Test: "TestBackfill_RestartRecovery/crash_recovery", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-10b", Test: "TestBackfill_Web/gateway_lifecycle", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-11b", Test: "TestBackfill_Scheduler/concurrent_mutex", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-12b", Test: "TestBackfill_SlowTimeout/run_timeout", Check: "015-slow-timeout", Level: "process"},
	{Item: "us10-6.2-13b", Test: "TestBackfill_Scheduler/archive_meets_cron", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-14b", Test: "TestBackfill_Scheduler/cross_agent_task_id", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-15a", Test: "TestBackfill_SignalLinux/sigterm_order", Check: "015-signal-linux", Level: "process"},
	{Item: "us10-6.2-16a", Test: "TestBackfill_Bootstrap/credential_scan", Check: "015-backfill", Level: "process"},
	{Item: "us10-6.2-16b", Test: "TestBackfill_Bootstrap/credential_template", Check: "015-backfill", Level: "process"},
}

// requiredRealItems 是乙类已授权外部项的冻结全集（模块卡 §4.5 覆盖表），独立
// 于目录与 stage 定义书写——同时删目录项与其 required_test 时，只有这份清单
// 仍能判红。授权 scope 见 §4.5：真实模型调用 ≤30 次、飞书发送 ≤5 次。
// requiredRealItems is the frozen authorized external set (module card §4.5),
// written independently of the catalog and stage files so that a simultaneous
// deletion still fails.
var requiredRealItems = []catalogEntry{
	{Item: "us2-6-3a", Test: "TestBackfill_Real/provider_call", Check: "015-real-model", Level: "external"},
	{Item: "us4-6.1-2b", Test: "TestBackfill_Real/multiturn", Check: "015-real-model", Level: "external"},
	{Item: "us4-6.1-2d", Test: "TestBackfill_Real/chat_tool", Check: "015-real-model", Level: "external"},
	{Item: "us4-6.1-2j", Test: "TestBackfill_Real/restart_session", Check: "015-real-model", Level: "external"},
	{Item: "us5-6.1-2a", Test: "TestBackfill_Real/weather_tool", Check: "015-real-model", Level: "external"},
	{Item: "us6-6.1-2b", Test: "TestBackfill_Real/memory_save", Check: "015-real-model", Level: "external"},
	{Item: "us7-6.1-2b", Test: "TestBackfill_Real/mcp_tool", Check: "015-real-model", Level: "external"},
	{Item: "us7-rsU3-1b", Test: "TestBackfill_Real/mcp_tool", Check: "015-real-model", Level: "external"},
	{Item: "us8-6.1-5a", Test: "TestBackfill_RealFeishu/send", Check: "015-real-feishu", Level: "external"},
	{Item: "us8-6.1-7b", Test: "TestBackfill_RealFeishu/redaction", Check: "015-real-feishu", Level: "external"},
	{Item: "us9-6.2-5a", Test: "TestBackfill_Real/web_invoke_tool", Check: "015-real-model", Level: "external"},
	{Item: "us10-6.2-1c", Test: "TestBackfill_Real/scheduler_fire", Check: "015-real-model", Level: "external"},
	{Item: "us10-6.2-4c", Test: "TestBackfill_Real/scheduler_manage_self", Check: "015-real-model", Level: "external"},
	{Item: "us10-6.2-5c", Test: "TestBackfill_Real/scheduler_manage_other_denied", Check: "015-real-model", Level: "external"},
	{Item: "us10-6.2-16c", Test: "TestBackfill_Real/provider_call", Check: "015-real-model", Level: "external"},
}

// validateCatalog 校验编号格式、唯一性与层级取值。
// validateCatalog checks item format, uniqueness and level values.
func validateCatalog(entries []catalogEntry) error {
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !catalogItemPattern.MatchString(e.Item) {
			return fmt.Errorf("catalog item %q does not match the frozen numbering rule", e.Item)
		}
		if seen[e.Item] {
			return fmt.Errorf("catalog item %q is duplicated", e.Item)
		}
		seen[e.Item] = true
		switch e.Level {
		case "source", "process", "external":
		default:
			return fmt.Errorf("catalog item %s has unknown level %q", e.Item, e.Level)
		}
		if e.Check == "" || e.Test == "" {
			return fmt.Errorf("catalog item %s has an empty check or test", e.Item)
		}
	}
	return nil
}

// matchCatalogToChecks 双向校验目录与 stage 定义：目录的测试名必须逐字出现在
// 所在 check 的 required_tests（有子测试写 Parent/sub，无子测试写 Parent）；
// check 的每个 required_test 也必须至少由一个目录项承载。
// matchCatalogToChecks validates both directions between the catalog and the
// stage definitions; names must be literal, subtests as Parent/sub.
func matchCatalogToChecks(entries []catalogEntry, checks map[string]check) error {
	covered := make(map[string]map[string]bool)
	for _, e := range entries {
		c, ok := checks[e.Check]
		if !ok {
			return fmt.Errorf("catalog item %s points at unknown check %s", e.Item, e.Check)
		}
		if got := checkLevel(c); got != e.Level {
			return fmt.Errorf("catalog item %s declares level %s but check %s is %s", e.Item, e.Level, e.Check, got)
		}
		if covered[e.Check] == nil {
			covered[e.Check] = make(map[string]bool)
		}
		matched := false
		for _, required := range c.RequiredTests {
			if required == e.Test {
				covered[e.Check][required] = true
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("catalog item %s test %s is not required by check %s", e.Item, e.Test, e.Check)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(covered)) {
		for _, required := range checks[id].RequiredTests {
			if !covered[id][required] {
				return fmt.Errorf("check %s required test %s has no catalog item", id, required)
			}
		}
	}
	return nil
}

// compareFrozenSubset 双向比较目录的非 external 子集与冻结全集。
// compareFrozenSubset compares the non-external catalog subset with the frozen
// set in both directions.
func compareFrozenSubset(catalog, frozen []catalogEntry) error {
	got := make(map[string]bool)
	for _, e := range catalog {
		if e.Level != "external" {
			got[e.String()] = true
		}
	}
	want := make(map[string]bool, len(frozen))
	for _, e := range frozen {
		want[e.String()] = true
	}
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if !got[key] {
			return fmt.Errorf("frozen item missing from catalog: %s", key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(got)) {
		if !want[key] {
			return fmt.Errorf("catalog item missing from the frozen set: %s", key)
		}
	}
	return nil
}

// compareExternalSubset 双向比较目录的 external 子集与已授权冻结全集。
// compareExternalSubset compares the external catalog subset with the frozen
// authorized set in both directions.
func compareExternalSubset(catalog, frozen []catalogEntry) error {
	got := make(map[string]bool)
	for _, e := range catalog {
		if e.Level == "external" {
			got[e.String()] = true
		}
	}
	want := make(map[string]bool, len(frozen))
	for _, e := range frozen {
		want[e.String()] = true
	}
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if !got[key] {
			return fmt.Errorf("authorized external item missing from catalog: %s", key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(got)) {
		if !want[key] {
			return fmt.Errorf("catalog external item missing from the authorized set: %s", key)
		}
	}
	return nil
}

// withoutEntry 返回删除指定 item 后的副本，供反例使用。
// withoutEntry returns a copy with one item removed, for counterexamples.
func withoutEntry(entries []catalogEntry, item string) []catalogEntry {
	out := make([]catalogEntry, 0, len(entries))
	for _, e := range entries {
		if e.Item != item {
			out = append(out, e)
		}
	}
	return out
}

// cloneChecks 浅拷贝 check 索引；调用方只替换切片，不改写原值。
// cloneChecks shallow-copies the check index; callers replace slices instead of
// mutating them in place.
func cloneChecks(in map[string]check) map[string]check {
	out := make(map[string]check, len(in))
	for id, c := range in {
		out[id] = c
	}
	return out
}

// TestStages_BackfillCatalogMatchesRequiredTests 校验目录编号规则、目录与四个
// stage 的 required_tests 逐字双向一致，并用反例证明删顶层 required_test、删
// 子测试名、挪 check、重号与非法编号都会变红。
// TestStages_BackfillCatalogMatchesRequiredTests validates the item numbering,
// literal both-direction matching against the four stage files, and the
// counterexamples for a deleted parent name, a deleted subtest name, a moved
// check, duplicate items and invalid ids.
func TestStages_BackfillCatalogMatchesRequiredTests(t *testing.T) {
	require.NoError(t, validateCatalog(backfillCatalog))

	root := stageTestRoot(t)
	checks := make(map[string]check)
	for _, rel := range []string{
		"harness/stages/current.json",
		"harness/stages/us15-linux.json",
		"harness/stages/us15-slow.json",
		"harness/stages/us15-real.json",
	} {
		s := readStageForTest(t, root, rel)
		for id, c := range stageChecksByID(s) {
			_, duplicated := checks[id]
			require.False(t, duplicated, "check %s is defined by more than one stage file", id)
			checks[id] = c
		}
	}
	require.NoError(t, matchCatalogToChecks(backfillCatalog, checks))

	t.Run("deleted_required_test_is_red", func(t *testing.T) {
		pruned := cloneChecks(checks)
		c := pruned["015-backfill"]
		c.RequiredTests = slices.DeleteFunc(slices.Clone(c.RequiredTests), func(s string) bool {
			return s == "TestBackfill_ConsolePipe"
		})
		pruned["015-backfill"] = c
		require.Error(t, matchCatalogToChecks(backfillCatalog, pruned),
			"删掉 required_test 后,目录中的 TestBackfill_ConsolePipe 必须无人承载")
	})

	t.Run("deleted_subtest_is_red", func(t *testing.T) {
		// 子测试名必须逐字出现在 required_tests：只删 Parent/sub 仍须判红，
		// 不得靠投影到顶层名掩盖遗漏（模块卡 §2.6 与 §4.2）。
		pruned := cloneChecks(checks)
		c := pruned["015-backfill"]
		c.RequiredTests = slices.DeleteFunc(slices.Clone(c.RequiredTests), func(s string) bool {
			return s == "TestBackfill_Bootstrap/init_idempotent"
		})
		pruned["015-backfill"] = c
		require.Error(t, matchCatalogToChecks(backfillCatalog, pruned),
			"删掉子测试名后,目录中的 TestBackfill_Bootstrap/init_idempotent 必须无人承载")
	})

	t.Run("linux_item_moved_into_backfill_is_red", func(t *testing.T) {
		moved := slices.Clone(backfillCatalog)
		for i := range moved {
			if moved[i].Item == "us9-6.1-6a" {
				moved[i].Check = "015-backfill"
			}
		}
		require.Error(t, matchCatalogToChecks(moved, checks),
			"把 linux 项塞进 015-backfill 后,015-backfill 的 required_tests 不含该子测试全名")
	})

	t.Run("duplicate_item_is_red", func(t *testing.T) {
		dup := append(slices.Clone(backfillCatalog), backfillCatalog[0])
		require.Error(t, validateCatalog(dup))
	})

	t.Run("invalid_item_id_is_red", func(t *testing.T) {
		bad := append(slices.Clone(backfillCatalog), catalogEntry{
			Item:  "us9-6-1a",
			Test:  "TestBackfill_Web/listen_addr",
			Check: "015-backfill",
			Level: "process",
		})
		require.Error(t, validateCatalog(bad), "US-9 漏写子节必须非法")
	})
}

// TestStages_BackfillCatalogCoversRequiredSet 校验目录非 external 子集与冻结
// 全集双向相等；反例证明只删一侧、或同时删目录项与 required_test 仍红。
// TestStages_BackfillCatalogCoversRequiredSet compares the non-external catalog
// subset with the frozen set and proves that one-sided or simultaneous
// deletion still fails.
func TestStages_BackfillCatalogCoversRequiredSet(t *testing.T) {
	require.NoError(t, compareFrozenSubset(backfillCatalog, requiredBackfillItems))

	t.Run("missing_catalog_item_is_red", func(t *testing.T) {
		pruned := withoutEntry(backfillCatalog, "us1-6-3a")
		require.Error(t, compareFrozenSubset(pruned, requiredBackfillItems))
	})

	t.Run("missing_frozen_item_is_red", func(t *testing.T) {
		short := withoutEntry(requiredBackfillItems, "us2-6-6a")
		require.Error(t, compareFrozenSubset(backfillCatalog, short))
	})

	t.Run("simultaneous_deletion_is_still_red", func(t *testing.T) {
		// 同时删目录项与其 required_test：目录 ↔ stage 互验仍自洽（两侧一起消失），
		// 冻结全集保留该项，因此仍红——这正是冻结全集独立书写的原因。
		root := stageTestRoot(t)
		checks := make(map[string]check)
		for _, rel := range []string{
			"harness/stages/current.json",
			"harness/stages/us15-linux.json",
			"harness/stages/us15-slow.json",
		} {
			for id, c := range stageChecksByID(readStageForTest(t, root, rel)) {
				checks[id] = c
			}
		}
		c := checks["015-backfill"]
		c.RequiredTests = slices.DeleteFunc(slices.Clone(c.RequiredTests), func(s string) bool {
			return s == "TestBackfill_ConsolePipe"
		})
		checks["015-backfill"] = c

		pruned := catalogWithinChecks(backfillCatalog, checks)
		pruned = slices.DeleteFunc(pruned, func(e catalogEntry) bool {
			return e.Test == "TestBackfill_ConsolePipe"
		})
		require.NoError(t, matchCatalogToChecks(pruned, checks),
			"目录 ↔ stage 互验在同时删除时会自洽变绿,不能单独作为覆盖证明")
		require.ErrorContains(t, compareFrozenSubset(pruned, requiredBackfillItems), "us4-6.1-4b",
			"冻结全集必须保留被删项并判红")
	})
}

// TestStages_RealCatalogCoversAuthorizedSet 校验 external 目录子集与
// `requiredRealItems` 双向相等，并证明 us15-real stage 的两个 check 都是
// external、目录项全部落在其 required_tests 上；反例覆盖单侧删除与同时删除。
// TestStages_RealCatalogCoversAuthorizedSet compares the external catalog subset
// with requiredRealItems and proves the us15-real stage only declares external
// checks covered by the catalog; counterexamples cover one-sided and
// simultaneous deletion.
func TestStages_RealCatalogCoversAuthorizedSet(t *testing.T) {
	require.NoError(t, compareExternalSubset(backfillCatalog, requiredRealItems))

	root := stageTestRoot(t)
	checks := stageChecksByID(readStageForTest(t, root, "harness/stages/us15-real.json"))
	require.Len(t, checks, 2, "us15-real 必须恰好声明 015-real-model 与 015-real-feishu")
	for _, id := range slices.Sorted(maps.Keys(checks)) {
		require.Equal(t, "external", checkLevel(checks[id]), "check %s 必须是 external", id)
	}
	require.NoError(t, matchCatalogToChecks(catalogWithinChecks(backfillCatalog, checks), checks))

	t.Run("missing_catalog_item_is_red", func(t *testing.T) {
		pruned := withoutEntry(backfillCatalog, "us4-6.1-2j")
		require.ErrorContains(t, compareExternalSubset(pruned, requiredRealItems), "us4-6.1-2j")
	})

	t.Run("missing_frozen_item_is_red", func(t *testing.T) {
		short := withoutEntry(requiredRealItems, "us9-6.2-5a")
		require.ErrorContains(t, compareExternalSubset(backfillCatalog, short), "us9-6.2-5a")
	})

	t.Run("simultaneous_deletion_is_still_red", func(t *testing.T) {
		// 同时删目录项与其 required_test：目录 ↔ stage 互验仍自洽，只有冻结
		// 全集能判红。015-real-feishu 的两个子测试一起删除，冻结全集报第一条。
		// Deleting both feishu catalog items and their required subtest names
		// stays green in the mutual check; the frozen set still fails.
		deleted := map[string]bool{
			"TestBackfill_RealFeishu/send":      true,
			"TestBackfill_RealFeishu/redaction": true,
		}
		prunedChecks := cloneChecks(checks)
		c := prunedChecks["015-real-feishu"]
		c.RequiredTests = slices.DeleteFunc(slices.Clone(c.RequiredTests), func(s string) bool {
			return deleted[s]
		})
		prunedChecks["015-real-feishu"] = c

		prunedCatalog := catalogWithinChecks(backfillCatalog, prunedChecks)
		prunedCatalog = slices.DeleteFunc(prunedCatalog, func(e catalogEntry) bool {
			return deleted[e.Test]
		})
		require.NoError(t, matchCatalogToChecks(prunedCatalog, prunedChecks),
			"目录 ↔ stage 互验在同时删除时会自洽变绿,不能单独作为覆盖证明")
		require.ErrorContains(t, compareExternalSubset(prunedCatalog, requiredRealItems), "us8-6.1-5a",
			"冻结全集必须保留被删的外部项并判红（同 check 的两项会一起消失，报第一条）")
	})
}

// catalogWithinChecks 保留 check 已加载的目录项，供只加载部分 stage 的反例使用。
// catalogWithinChecks keeps entries whose check is among the loaded ones, for
// counterexamples that load only a subset of the stage files.
func catalogWithinChecks(entries []catalogEntry, checks map[string]check) []catalogEntry {
	out := make([]catalogEntry, 0, len(entries))
	for _, e := range entries {
		if _, ok := checks[e.Check]; ok {
			out = append(out, e)
		}
	}
	return out
}

// ciStageRefPattern 提取 workflow 命令行中以字面量给出的 --stage 参数。
// ciStageRefPattern extracts literal --stage arguments from workflow run lines.
var ciStageRefPattern = regexp.MustCompile(`--stage\s+("[^"]*"|'[^']*'|\S+)`)

// ciExecutedStages 返回 CI 实际执行的 stage 路径：current.json 恒被承载，
// 再加上所有 workflow 中 --stage 点名的字面量路径。动态值无法核验，直接判红。
// ciExecutedStages lists stages CI actually loads: current.json always, plus every
// literal --stage path in the workflows. A dynamic value cannot be verified and is red.
func ciExecutedStages(t *testing.T, root string) []string {
	t.Helper()
	seen := map[string]bool{"harness/stages/current.json": true}
	var workflows []string
	for _, glob := range []string{"*.yml", "*.yaml"} {
		found, err := filepath.Glob(filepath.Join(root, ".github", "workflows", glob))
		require.NoError(t, err, "glob workflow 失败")
		workflows = append(workflows, found...)
	}
	require.NotEmpty(t, workflows, "未找到 workflow 文件——CI stage 可见性判据失效")
	for _, wf := range workflows {
		data, err := os.ReadFile(wf)
		require.NoError(t, err, "读取 %s 失败", wf)
		rel, err := filepath.Rel(root, wf)
		require.NoError(t, err)
		rel = filepath.ToSlash(rel)
		for i, line := range strings.Split(string(data), "\n") {
			// 注释行里的 --stage 只是说明文字，不构成 CI 执行引用。
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			for _, m := range ciStageRefPattern.FindAllStringSubmatch(line, -1) {
				stagePath := strings.Trim(m[1], `"'`)
				require.True(t, strings.HasSuffix(stagePath, ".json") && !strings.Contains(stagePath, "$"),
					"%s:%d 的 --stage 参数 %q 不是字面量 stage 路径，无法核验 git 可见性", rel, i+1, stagePath)
				seen[stagePath] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// TestStages_CIInputsGitVisible 守 CI stage 的输入面与干净检出一致：CI 执行
// （current.json 承载或 workflow 以 --stage 引用）的 stage，其 inputs / protect
// 必须是 git 跟踪的入库路径。干净检出只有入库文件，私有模块卡（docs/us/*.md）
// 一旦进入 CI stage，loadStage 的逐项存在性校验会让运行直接退出 2。
// 判据：对每个 CI stage 的 inputs / protect 逐项执行 `git ls-files --error-unmatch`。
// 逆改点：把 docs/us/*.md 加回 current.json 的 inputs，或让 workflow 引用含私有
// 路径的 stage，即红。
// TestStages_CIInputsGitVisible keeps every CI-executed stage loadable from a clean
// checkout: each input/protect path must be git-visible. A private module card in a
// CI stage makes loadStage fail with exit 2 before any check runs.
func TestStages_CIInputsGitVisible(t *testing.T) {
	root := stageTestRoot(t)
	stages := ciExecutedStages(t, root)
	require.NotEmpty(t, stages, "没有解析到 CI 执行的 stage——判据面失效")

	for _, rel := range stages {
		s := readStageForTest(t, root, rel)
		scopes := []struct {
			name  string
			paths []string
		}{
			{name: "inputs", paths: s.Inputs},
			{name: "protect", paths: s.Protect},
		}
		for _, scope := range scopes {
			for _, p := range scope.paths {
				cmd := exec.Command("git", "ls-files", "--error-unmatch", "--", p)
				cmd.Dir = root
				out, err := cmd.CombinedOutput()
				require.NoError(t, err,
					"%s 的 %s %q 不在 git 跟踪面内（干净检出加载该 stage 会失败）: %s",
					rel, scope.name, p, strings.TrimSpace(string(out)))
			}
		}
	}
}
