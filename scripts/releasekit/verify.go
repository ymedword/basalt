package releasekit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	releaseRecordName = "release-record.json"
	recordSignature   = "release-record.json.sig"
	sumsSignature     = "SHA256SUMS.sig"
)

// VerifyRelease 验证发布目录：先逐文件核对哈希，再按策略验证签名；任一失败即停。
// VerifyRelease checks every file hash first and then the signatures according to
// policy; the first failure stops the run.
func VerifyRelease(ctx context.Context, dir string, policy VerifyPolicy) (VerifyReport, error) {
	var report VerifyReport
	if err := ctx.Err(); err != nil {
		return report, err
	}
	entries, err := ParseChecksums(filepath.Join(dir, checksumsName))
	if err != nil {
		return report, err
	}
	listed := make(map[string]bool, len(entries))
	for _, entry := range entries {
		listed[entry.Name] = true
		path := filepath.Join(dir, filepath.FromSlash(entry.Name))
		got, err := FileSHA256(path)
		if err != nil {
			return report, fmt.Errorf("读取 %s：%w", entry.Name, err)
		}
		if got != entry.SHA256 {
			return report, fmt.Errorf("%s 哈希不符：清单 %s，实际 %s", entry.Name, entry.SHA256, got)
		}
		report.VerifiedFiles = append(report.VerifiedFiles, entry.Name)
	}
	if err := checkCoverage(dir, listed); err != nil {
		return report, err
	}
	if _, err := os.Stat(filepath.Join(dir, releaseRecordName)); err != nil {
		return report, fmt.Errorf("缺少 %s：%w", releaseRecordName, err)
	}
	recordBody, err := os.ReadFile(filepath.Join(dir, releaseRecordName)) // #nosec G304 -- 发布文件路径
	if err != nil {
		return report, err
	}
	var record ReleaseRecord
	if err := json.Unmarshal(recordBody, &record); err != nil {
		return report, fmt.Errorf("解析 release-record.json：%w", err)
	}
	if record.Schema != 1 {
		return report, fmt.Errorf("release-record.json schema=%d，期望 1", record.Schema)
	}
	// 记录是发布目录的权威快照：哈希必须与目录内文件互相对得上，否则记录签名之后
	// 仍可替换制品或证据而不被发现。
	if err := crossCheckRecord(dir, record); err != nil {
		return report, err
	}
	// 镜像行按 (goos, goarch, kind=image) 独立核对，不进入归档行循环；矩阵或记录
	// 任一存在镜像内容时都必须逐条通过。
	matrixBody, err := os.ReadFile(filepath.Join(dir, "support-matrix.json")) // #nosec G304 -- 发布文件路径
	if err != nil {
		return report, fmt.Errorf("读取 support-matrix.json：%w", err)
	}
	var matrix SupportMatrix
	if err := json.Unmarshal(matrixBody, &matrix); err != nil {
		return report, fmt.Errorf("解析 support-matrix.json：%w", err)
	}
	// 镜像文件已在 SHA256SUMS 的逐文件循环里核过哈希，这里只做记录与矩阵对账，
	// 不把同一批文件名重复计入 VerifiedFiles。
	if _, err := VerifyImages(dir, record, matrix, VerifyImageOptions{
		RequireRepoDigest: policy.RequireRepoDigest,
	}); err != nil {
		return report, err
	}
	require := policy.RequireSignature
	if require && strings.TrimSpace(policy.PublicKey) == "" {
		return report, errors.New("要求签名时必须提供 --public-key")
	}
	if require {
		if record.Signing.Status != "signed" {
			return report, fmt.Errorf("发布记录签名状态是 %q，final 必须是 signed", record.Signing.Status)
		}
		for _, required := range []string{recordSignature, sumsSignature} {
			if _, err := os.Stat(filepath.Join(dir, required)); err != nil {
				return report, fmt.Errorf("缺少签名 %s：%w", required, err)
			}
		}
	}
	if policy.PublicKey != "" {
		if err := verifyOptionalSignature(policy.PublicKey, filepath.Join(dir, sumsSignature), filepath.Join(dir, checksumsName)); err != nil {
			return report, err
		}
		report.VerifiedSignatures = append(report.VerifiedSignatures, sumsSignature)
		if _, err := os.Stat(filepath.Join(dir, recordSignature)); err == nil {
			if err := verifyOptionalSignature(policy.PublicKey, filepath.Join(dir, recordSignature), filepath.Join(dir, releaseRecordName)); err != nil {
				return report, err
			}
			report.VerifiedSignatures = append(report.VerifiedSignatures, recordSignature)
		} else if require {
			return report, fmt.Errorf("缺少 %s", recordSignature)
		}
	}
	report.Signed = len(report.VerifiedSignatures) == 2
	if require && !report.Signed {
		return report, errors.New("final 发布必须同时验证记录签名与校验和签名")
	}
	if strings.TrimSpace(policy.TrustFile) != "" {
		if err := checkTrust(policy.TrustFile, policy.PublicKey); err != nil {
			return report, err
		}
	}
	return report, nil
}

// crossCheckRecord 把记录里的每个哈希与发布目录内的实际文件对账，并要求状态为
// tested 的平台在目录里带着与归档哈希一致的证据。
// crossCheckRecord reconciles every hash in the record with the files actually present
// and requires tested platforms to ship evidence matching the archived artifact.
func crossCheckRecord(dir string, record ReleaseRecord) error {
	if len(record.Limitations) == 0 {
		return errors.New("release-record.json 的 limitations 不得为空")
	}
	if err := compareRecordedHash(dir, "support-matrix.json", record.SupportMatrixSHA256); err != nil {
		return err
	}
	if err := compareRecordedHash(dir, "notes.md", record.NotesSHA256); err != nil {
		return err
	}
	artifacts := make(map[string]ArtifactRecord, len(record.Artifacts))
	for _, artifact := range record.Artifacts {
		artifacts[artifact.GOOS+"/"+artifact.GOARCH] = artifact
		if err := compareRecordedHash(dir, artifact.Name, artifact.SHA256); err != nil {
			return err
		}
		base := strings.TrimSuffix(strings.TrimSuffix(artifact.Name, ".zip"), ".tar.gz")
		if err := compareRecordedHash(dir, base+".sbom.cdx.json", artifact.SBOMSHA256); err != nil {
			return err
		}
		if err := compareRecordedHash(dir, base+".provenance.json", artifact.ProvenanceSHA256); err != nil {
			return err
		}
	}
	for _, row := range record.Evidence {
		// kind 只允许空（归档行）与 image（镜像行）；别的取值会被两条核对路径同时
		// 跳过，变成谁都不对账的静默行。
		// Only "" (archive) and image are valid kinds; anything else would be skipped
		// by both check paths and silently escape reconciliation.
		switch row.Kind {
		case "":
		case kindImage:
			// 镜像行由 VerifyImages 按镜像记录核对；同平台的归档 artifact 与归档证据
			// 不能被镜像行借用，否则会得到错配的绿或红（red/green mismatch）。
			continue
		default:
			return fmt.Errorf("记录证据行 %s/%s 的 kind %q 不在 \"\" / image 内", row.GOOS, row.GOARCH, row.Kind)
		}
		if row.Status != StatusTested {
			continue
		}
		artifact, ok := artifacts[row.GOOS+"/"+row.GOARCH]
		if !ok {
			return fmt.Errorf("证据声明 %s/%s tested，但记录里没有对应制品", row.GOOS, row.GOARCH)
		}
		evidence, err := LoadEvidence(dir, row.GOOS, row.GOARCH)
		if err != nil {
			return fmt.Errorf("记录声明 %s/%s tested，但发布目录缺少证据文件：%w", row.GOOS, row.GOARCH, err)
		}
		if evidence.ArchiveSHA256 != artifact.SHA256 {
			return fmt.Errorf("%s/%s 的证据绑定归档 %s，与记录里的归档 %s 不一致",
				row.GOOS, row.GOARCH, evidence.ArchiveSHA256, artifact.SHA256)
		}
		if evidence.Status != row.Status {
			return fmt.Errorf("%s/%s 的证据状态 %q 与记录 %q 不一致", row.GOOS, row.GOARCH, evidence.Status, row.Status)
		}
	}
	return nil
}

func compareRecordedHash(dir, name, want string) error {
	if strings.TrimSpace(want) == "" {
		return fmt.Errorf("记录缺少 %s 的哈希", name)
	}
	got, err := FileSHA256(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		return fmt.Errorf("读取 %s：%w", name, err)
	}
	if got != want {
		return fmt.Errorf("%s 哈希与记录不一致：记录 %s，实际 %s", name, want, got)
	}
	return nil
}

func verifyOptionalSignature(publicKey, sigPath, target string) error {
	if _, err := os.Stat(sigPath); err != nil {
		return fmt.Errorf("签名文件不可用 %s：%w", sigPath, err)
	}
	return VerifyFileSignature(publicKey, target, sigPath)
}

func checkTrust(trustFile, publicKey string) error {
	if strings.TrimSpace(publicKey) == "" {
		return errors.New("使用 --trust 时必须提供 --public-key")
	}
	fingerprint, err := Fingerprint(publicKey)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(trustFile) // #nosec G304 -- 信任文件由用户显式传入
	if err != nil {
		return fmt.Errorf("读取信任文件：%w", err)
	}
	if !strings.Contains(string(body), fingerprint) {
		return fmt.Errorf("公钥指纹 %s 不在信任文件中", fingerprint)
	}
	return nil
}

func checkCoverage(dir string, listed map[string]bool) error {
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == checksumsName || rel == sumsSignature {
			return nil
		}
		if !listed[rel] {
			return fmt.Errorf("发布文件 %s 不在 SHA256SUMS 内", rel)
		}
		return nil
	})
}
