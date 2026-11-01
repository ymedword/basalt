package dbmigrate

// 本文件是回退判据的**纯单元**判卷（不依赖 PostgreSQL）：三维少一维即按不允许处理、
// 未切换时无损放行、工作区快照采集的接受与拒绝路径。需要真实库的取证用例在
// rollback_integration_test.go。
// Unit-level checks for the rollback gate and workspace capture; the real-database
// evidence cases live in rollback_integration_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAutoRollbackAllowed_RequiresAllThreeDimensions 少一维 = 没取证：只带两维干净
// 也不得放行（C-43 的"三维都在场"）。
// 逆改点：去掉 `len(a.Dimensions) != 3` 守卫 → 本用例允许断言红；
// `BlockReasons` 不报缺维 → 计数串断言红。
func TestAutoRollbackAllowed_RequiresAllThreeDimensions(t *testing.T) {
	assessment := RollbackAssessment{Dimensions: []DimensionEvidence{
		{Dimension: DimensionDatabase, Verdict: EvidenceClean},
		{Dimension: DimensionWorkspace, Verdict: EvidenceClean},
	}}

	require.False(t, assessment.AutoRollbackAllowed(), "少一维不是干净，是没取证")
	require.ErrorIs(t, assessment.RequireAutoRollbackAllowed(), ErrAutoRollbackBlocked)
	require.Contains(t, assessment.BlockReasons(), "三维判据不完整（2/3）")
	require.Contains(t, assessment.Report(), "停止自动回退")
}

// TestAutoRollbackAllowed_BeforeCutoverIsLossless 未切换时源库仍是权威：没有维度、
// 没有报告也要直接放行，且不留任何阻止理由。
func TestAutoRollbackAllowed_BeforeCutoverIsLossless(t *testing.T) {
	assessment := RollbackAssessment{BeforeCutover: true}

	require.True(t, assessment.AutoRollbackAllowed())
	require.NoError(t, assessment.RequireAutoRollbackAllowed())
	require.Empty(t, assessment.BlockReasons())
	require.Contains(t, assessment.Report(), "尚未切换")
}

// TestCaptureWorkspaceTree_HashesRegularFilesAndTreatsMissingAsEmpty 申报的目录递归
// 展开、申报但此刻不存在的条目按空处理（切换后才出现的文件由比对发现）。
func TestCaptureWorkspaceTree_HashesRegularFilesAndTreatsMissingAsEmpty(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "state"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "state", "seen.json"), []byte("v1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "assets.json"), []byte("{}"), 0o644))

	snapshot, err := CaptureWorkspaceTree(root, "assets.json", "state", "runs")
	require.NoError(t, err)
	require.Len(t, snapshot, 2)
	require.Contains(t, snapshot, "state/seen.json")
	require.Contains(t, snapshot, "assets.json")
}

// TestCaptureWorkspaceTree_RefusesSymlink 符号链接一律拒绝、不跟随：快照只覆盖根目录
// 内的普通文件，防止把根外内容或无限环读进快照。
func TestCaptureWorkspaceTree_RefusesSymlink(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "real.txt"), []byte("x"), 0o644))
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("本机不允许创建符号链接，无法验证拒绝路径: %v", err)
	}

	_, err := CaptureWorkspaceTree(root, ".")
	require.Error(t, err)
	require.Contains(t, err.Error(), "符号链接")
}

// TestCaptureWorkspaceTree_RejectsEmptyRootAndEntries 缺根目录或没申报条目都是调用错误，
// 不得默认采整树。
func TestCaptureWorkspaceTree_RejectsEmptyRootAndEntries(t *testing.T) {
	_, err := CaptureWorkspaceTree("")
	require.Error(t, err)

	_, err = CaptureWorkspaceTree(t.TempDir())
	require.Error(t, err)
}

// TestReconcileLegacyMemoryBaseline_RebasesOnlyWhenNumbersMatch 三个数对得上才把
// `memory_entries` 的基线换成导入后实测，且不改写调用方的导入报告（逐表副本）。
// 逆改点：直接写 `baseline.Rows` → 原报告被改的断言红；不换基线 → 回退判据会把
// 导入行读成业务写入（集成用例同步变红）。
func TestReconcileLegacyMemoryBaseline_RebasesOnlyWhenNumbersMatch(t *testing.T) {
	baseline := ImportReport{Rows: map[string]int64{LegacyMemoryTable: 2, "sessions": 1}}
	evidence := DimensionEvidence{Dimension: DimensionDatabase, Verdict: EvidenceClean}

	rows, rebased := reconcileLegacyMemoryBaseline(&evidence, baseline, LegacyMemoryEvidence{
		Checked: true, BaselineRows: 2, Imported: 3, ObservedRows: 5,
	})
	require.True(t, rebased, "导入前 2 + 导入 3 = 导入后 5，对账成立")
	require.Equal(t, int64(5), rows[LegacyMemoryTable])
	require.Equal(t, int64(1), rows["sessions"])
	require.Equal(t, int64(2), baseline.Rows[LegacyMemoryTable], "不得改写调用方的导入报告")
	require.Equal(t, EvidenceClean, evidence.Verdict)
	require.Contains(t, strings.Join(evidence.Findings, "\n"),
		"旧记忆导入对账：导入前 2 行 + 本次导入 3 条 = 导入后 5 行")
}

// TestReconcileLegacyMemoryBaseline_NeverSilencesDelta 没申报 / 多出 / 少掉 / 证据自相矛盾
// 都不得让该表回到干净：多出是报告之外的写入（脏），少掉与自相矛盾是结果未知（无法证明）。
// 已确认的脏不因同时无法证明降级。
// 逆改点：不校验三个数、直接换基线 → "多除 1 行"与"少 1 行"两例的档位断言红；
// 未申报时也换基线 → 未申报用例的 rebased 与行数断言红。
func TestReconcileLegacyMemoryBaseline_NeverSilencesDelta(t *testing.T) {
	baseline := ImportReport{Rows: map[string]int64{LegacyMemoryTable: 2}}
	cases := []struct {
		name     string
		legacy   LegacyMemoryEvidence
		verdict  EvidenceVerdict
		rebased  bool
		fragment string
	}{
		{
			name:    "未申报",
			legacy:  LegacyMemoryEvidence{},
			verdict: EvidenceClean,
			rebased: false,
		},
		{
			name:     "多出报告之外的行",
			legacy:   LegacyMemoryEvidence{Checked: true, BaselineRows: 2, Imported: 3, ObservedRows: 6},
			verdict:  EvidenceDirty,
			fragment: "多 1 行——导入报告之外出现了写入",
		},
		{
			name:     "少掉的导入结果未知",
			legacy:   LegacyMemoryEvidence{Checked: true, BaselineRows: 2, Imported: 3, ObservedRows: 4},
			verdict:  EvidenceUnprovable,
			fragment: "少 1 行——导入未全部落库，结果未知",
		},
		{
			name:     "证据不是针对这份导入报告",
			legacy:   LegacyMemoryEvidence{Checked: true, BaselineRows: 3, Imported: 3, ObservedRows: 6},
			verdict:  EvidenceUnprovable,
			fragment: "证据不是针对这份导入报告",
		},
		{
			name:     "基线与证据数值为负",
			legacy:   LegacyMemoryEvidence{Checked: true, BaselineRows: 2, Imported: -1, ObservedRows: 1},
			verdict:  EvidenceUnprovable,
			fragment: "数值为负",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evidence := DimensionEvidence{Dimension: DimensionDatabase, Verdict: EvidenceClean}
			rows, rebased := reconcileLegacyMemoryBaseline(&evidence, baseline, tc.legacy)
			require.Equal(t, tc.rebased, rebased)
			require.Equal(t, tc.verdict, evidence.Verdict)
			require.Equal(t, int64(2), rows[LegacyMemoryTable], "未成立的对账不得改基线")
			if tc.fragment != "" {
				require.Contains(t, strings.Join(evidence.Findings, "\n"), tc.fragment)
			}
		})
	}
}

// TestReconcileLegacyMemoryBaseline_KeepsConfirmedDirty 窗口已判脏时，对账"无法证明"
// 不得把它降级——报告要先说不可撤销的那一条。
// 逆改点：escalate 无条件覆盖成 unprovable → 本用例红。
func TestReconcileLegacyMemoryBaseline_KeepsConfirmedDirty(t *testing.T) {
	evidence := DimensionEvidence{Dimension: DimensionDatabase, Verdict: EvidenceDirty}

	_, rebased := reconcileLegacyMemoryBaseline(&evidence, ImportReport{Rows: map[string]int64{}}, LegacyMemoryEvidence{
		Checked: true, BaselineRows: 0, Imported: 1, ObservedRows: 1,
	})
	require.False(t, rebased)
	require.Equal(t, EvidenceDirty, evidence.Verdict, "已确认的脏不因同时无法证明降级")
}

// TestCheckLegacyMemoryImport_RejectsMissingDatabase 没有库句柄时不得返回一条"对账
// 成立"的假证据——那样调用方会拿零值当读数写进回退判据。负数的输入校验在集成用例
// 里用真库覆盖（本用例拿不到可用的目标库句柄）。
func TestCheckLegacyMemoryImport_RejectsMissingDatabase(t *testing.T) {
	_, err := CheckLegacyMemoryImport(t.Context(), nil, 0, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "目标库句柄")
}
