package releasekit

import (
	"fmt"
)

// BuildSupportMatrix 按证据生成支持矩阵；final 只强制两份 full 证据，binary 档
// 没有运行证据时写 compiled-only 并注明原因；rehearsal 缺证据也降级为 compiled-only。
// BuildSupportMatrix derives the matrix from evidence. Only the two full-tier targets
// must carry run evidence in a final release; binary-tier targets stay compiled-only
// with an explicit reason. Rehearsal degrades every missing row to compiled-only.
func BuildSupportMatrix(in Inputs, evidenceDir string) (SupportMatrix, error) {
	matrix := SupportMatrix{Schema: 1}
	for _, target := range DefaultTargets() {
		evidence, err := LoadEvidence(evidenceDir, target.GOOS, target.GOARCH)
		if err != nil {
			if in.Mode == ModeFinal && target.Tier == TierFull {
				return SupportMatrix{}, fmt.Errorf("final 缺少 %s 证据：%w", target.String(), err)
			}
			row := SupportRow{
				GOOS: target.GOOS, GOARCH: target.GOARCH, Tier: target.Tier,
				Status: StatusCompiledOnly,
				Reason: "rehearsal 未提供宿主运行证据",
			}
			if target.Tier == TierBinary {
				row.Reason = "交叉编译目标，只证明可构建与可打包，无宿主运行证据"
			}
			if err := CheckSupportRow(row); err != nil {
				return SupportMatrix{}, err
			}
			matrix.Rows = append(matrix.Rows, row)
			continue
		}
		row := SupportRow{
			GOOS: target.GOOS, GOARCH: target.GOARCH, Tier: target.Tier,
			Status: evidence.Status, Checks: evidence.Checks, Reason: evidence.Reason,
		}
		if err := CheckSupportRow(row); err != nil {
			return SupportMatrix{}, err
		}
		matrix.Rows = append(matrix.Rows, row)
	}
	return matrix, nil
}

// CheckSupportRow 校验三档证据语义。
// CheckSupportRow enforces the three evidence statuses.
func CheckSupportRow(row SupportRow) error {
	switch row.Status {
	case StatusTested:
		if len(row.Checks) == 0 {
			return fmt.Errorf("%s/%s 状态 tested 但没有检查 ID", row.GOOS, row.GOARCH)
		}
	case StatusCompiledOnly, StatusNotTested:
		if row.Reason == "" {
			return fmt.Errorf("%s/%s 状态 %s 必须写原因", row.GOOS, row.GOARCH, row.Status)
		}
	default:
		return fmt.Errorf("%s/%s 状态 %q 不在 tested / compiled-only / not-tested 内", row.GOOS, row.GOARCH, row.Status)
	}
	return nil
}
