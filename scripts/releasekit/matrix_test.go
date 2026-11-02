package releasekit

import (
	"testing"
)

func TestUS17_SupportMatrix_TestedRequiresEvidence(t *testing.T) {
	row := SupportRow{GOOS: "linux", GOARCH: "amd64", Tier: TierFull, Status: StatusTested}
	if err := CheckSupportRow(row); err == nil {
		t.Fatal("tested 但没有检查 ID 必须拒绝")
	}
	row.Checks = []string{"artifact-unpack"}
	if err := CheckSupportRow(row); err != nil {
		t.Fatalf("带检查 ID 的 tested 应通过：%v", err)
	}
}

func TestUS17_SupportMatrix_CompiledOnlyRequiresReason(t *testing.T) {
	row := SupportRow{GOOS: "linux", GOARCH: "arm64", Tier: TierBinary, Status: StatusCompiledOnly}
	if err := CheckSupportRow(row); err == nil {
		t.Fatal("compiled-only 没有原因必须拒绝")
	}
	row.Reason = "无 arm64 宿主"
	if err := CheckSupportRow(row); err != nil {
		t.Fatalf("带原因的 compiled-only 应通过：%v", err)
	}
}
