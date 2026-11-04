package releasekit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ymedword/basalt/internal/isolation"
)

func verifySandboxManifest(dir string, record ReleaseRecord, policy VerifyPolicy, report *VerifyReport) error {
	path := filepath.Join(dir, imageLockName)
	body, err := os.ReadFile(path) // #nosec G304 -- 发布目录固定清单名
	if errors.Is(err, os.ErrNotExist) && !(policy.RequireSignature && len(record.Images) > 0) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("执行镜像清单: %w", err)
	}
	m, err := isolation.ParseImageManifest(body, policy.RequireSignature)
	if err != nil {
		return err
	}
	lock := ImageLock{Schema: m.Schema, AppBase: m.AppBase, Postgres: m.Postgres, Sandbox: m.Sandbox}
	expected, err := MergeSandboxImages(lock, record.Images, policy.RequireSignature)
	if err != nil {
		return err
	}
	if expected.Sandbox != lock.Sandbox {
		return errors.New("执行镜像清单与发布镜像记录不一致")
	}
	sig := filepath.Join(dir, imageLockSignature)
	_, statErr := os.Stat(sig)
	if policy.RequireSignature && statErr != nil {
		return fmt.Errorf("缺少 %s: %w", imageLockSignature, statErr)
	}
	if statErr == nil && policy.PublicKey != "" {
		if err := VerifyFileSignature(policy.PublicKey, path, sig); err != nil {
			return err
		}
		report.VerifiedSignatures = append(report.VerifiedSignatures, imageLockSignature)
	} else {
		report.Signed = false
	}
	return nil
}
