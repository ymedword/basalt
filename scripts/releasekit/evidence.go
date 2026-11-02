package releasekit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteEvidence 写入宿主证据；RunRelease 在冒烟成功后调用。
// WriteEvidence writes the host evidence after a successful smoke run.
func WriteEvidence(dir string, in Inputs, pkg PackageResult, reproducible *Reproducible) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- 证据目录
		return err
	}
	// 检查清单来自烟测实际执行的项：binary 档不启动服务，就不会写 health / mock-task，
	// 避免证据把没跑过的检查算成通过。
	evidence := Evidence{
		GOOS:          pkg.Target.GOOS,
		GOARCH:        pkg.Target.GOARCH,
		Tier:          pkg.Target.Tier,
		Status:        StatusTested,
		Checks:        smokeChecks(pkg.Target),
		ArchiveSHA256: pkg.SHA256,
		Reproducible:  reproducible,
	}
	return WriteEvidenceFile(dir, evidence)
}

// AggregateReproducible 汇总两份 full 宿主证据的双构建结论：两份都做过且都相等才算
// checked；record 里的单值 second_build_sha256 取 linux/amd64，缺失时退到 windows/amd64。
// AggregateReproducible folds both full-tier host readings: checked only when every full
// target was compared and all comparisons were equal. The record carries a single
// second_build_sha256, taken from linux/amd64 and falling back to windows/amd64.
func AggregateReproducible(dir string) (Reproducible, error) {
	var (
		aggregate Reproducible
		readings  []Reproducible
		missing   bool
		second    string
		fallback  string
	)
	for _, target := range DefaultTargets() {
		if target.Tier != TierFull {
			continue
		}
		evidence, err := LoadEvidence(dir, target.GOOS, target.GOARCH)
		if err != nil || evidence.Reproducible == nil {
			missing = true
			continue
		}
		readings = append(readings, *evidence.Reproducible)
		if target.GOOS == "linux" && target.GOARCH == "amd64" {
			second = evidence.Reproducible.SecondBuildSHA256
		}
		if target.GOOS == "windows" && target.GOARCH == "amd64" {
			fallback = evidence.Reproducible.SecondBuildSHA256
		}
	}
	if len(readings) == 0 {
		return aggregate, nil
	}
	aggregate.Checked = !missing
	aggregate.Equal = aggregate.Checked
	for _, reading := range readings {
		if !reading.Checked || !reading.Equal {
			aggregate.Checked = aggregate.Checked && reading.Checked
			aggregate.Equal = false
		}
	}
	if second == "" {
		second = fallback
	}
	aggregate.SecondBuildSHA256 = second
	return aggregate, nil
}

// WriteEvidenceFile 写入一条任意的证据读数。
// WriteEvidenceFile writes an arbitrary evidence reading.
func WriteEvidenceFile(dir string, evidence Evidence) error {
	if err := CheckSupportRow(SupportRow{
		GOOS: evidence.GOOS, GOARCH: evidence.GOARCH, Tier: evidence.Tier,
		Status: evidence.Status, Checks: evidence.Checks, Reason: evidence.Reason,
	}); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- 证据目录
		return err
	}
	body, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	name := fmt.Sprintf("evidence-%s-%s.json", evidence.GOOS, evidence.GOARCH)
	// #nosec G306 -- 宿主证据随发布目录分发，必须对下载者可读
	return os.WriteFile(filepath.Join(dir, name), body, 0o644)
}

// LoadEvidence 读取一个平台的证据。
// LoadEvidence reads one platform's evidence file.
func LoadEvidence(dir, goos, goarch string) (Evidence, error) {
	path := filepath.Join(dir, fmt.Sprintf("evidence-%s-%s.json", goos, goarch))
	body, err := os.ReadFile(path) // #nosec G304 -- 证据目录
	if err != nil {
		return Evidence{}, err
	}
	var evidence Evidence
	if err := json.Unmarshal(body, &evidence); err != nil {
		return Evidence{}, fmt.Errorf("解析 %s：%w", path, err)
	}
	return evidence, nil
}

// RequireEvidence 要求 final 发布具备 windows/amd64 与 linux/amd64 的实际运行证据。
// RequireEvidence requires final releases to carry actual run evidence for both full
// platforms; rehearsal may omit it and downgrades the matrix instead.
func RequireEvidence(in Inputs, dir string) error {
	if in.Mode != ModeFinal {
		return nil
	}
	for _, target := range DefaultTargets() {
		if target.Tier != TierFull {
			continue
		}
		evidence, err := LoadEvidence(dir, target.GOOS, target.GOARCH)
		if err != nil {
			return fmt.Errorf("final 缺少 %s 宿主证据：%w", target.String(), err)
		}
		if evidence.Status != StatusTested {
			return fmt.Errorf("final 的 %s 证据状态是 %q，必须是 tested", target.String(), evidence.Status)
		}
		if len(evidence.Checks) == 0 {
			return fmt.Errorf("final 的 %s 证据没有检查 ID", target.String())
		}
	}
	return nil
}
