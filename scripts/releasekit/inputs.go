package releasekit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ValidateVersion 拒绝带 `v` 前缀或非 X.Y.Z 的版本号。
// ValidateVersion rejects a leading `v` or any non X.Y.Z version.
func ValidateVersion(version string) error {
	if !versionPattern.MatchString(version) {
		return fmt.Errorf("版本号 %q 不是 X.Y.Z 形式（不得带 v 前缀）", version)
	}
	return nil
}

// ValidateTag 要求 tag 恰好是 `v` + version。
// ValidateTag requires the tag to be `v` plus the version.
func ValidateTag(version, tag string) error {
	if tag != "v"+version {
		return fmt.Errorf("tag %q 与版本 %q 不一致：必须是 v%s", tag, version, version)
	}
	return nil
}

// LoadInputs 冻结版本、tag、模式与提交号；工作区脏或 tag 不指向 HEAD 即失败。
// LoadInputs freezes the version, tag, mode and commit; a dirty tree or a tag that
// does not point at HEAD is a hard failure.
func LoadInputs(repo, version, tag string, mode Mode) (Inputs, error) {
	if err := ValidateVersion(version); err != nil {
		return Inputs{}, err
	}
	if mode != ModeRehearsal && mode != ModeFinal {
		return Inputs{}, fmt.Errorf("mode %q 必须是 rehearsal 或 final", mode)
	}
	if tag != "" {
		if err := ValidateTag(version, tag); err != nil {
			return Inputs{}, err
		}
	}
	if mode == ModeFinal && tag == "" {
		return Inputs{}, errors.New("final 发布必须提供 tag")
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return Inputs{}, fmt.Errorf("解析仓库根：%w", err)
	}
	status, err := gitOutput(abs, "status", "--porcelain")
	if err != nil {
		return Inputs{}, err
	}
	if strings.TrimSpace(status) != "" {
		return Inputs{}, fmt.Errorf("工作区不干净，拒绝构建发布物：\n%s", strings.TrimSpace(status))
	}
	commit, err := gitOutput(abs, "rev-parse", "HEAD")
	if err != nil {
		return Inputs{}, err
	}
	commit = strings.TrimSpace(commit)
	if !commitPattern.MatchString(commit) {
		return Inputs{}, fmt.Errorf("HEAD 提交号 %q 不是 40 位十六进制", commit)
	}
	if tag != "" {
		points, err := gitOutput(abs, "tag", "--points-at", "HEAD")
		if err != nil {
			return Inputs{}, err
		}
		found := false
		for _, line := range strings.Split(points, "\n") {
			if strings.TrimSpace(line) == tag {
				found = true
				break
			}
		}
		if !found {
			return Inputs{}, fmt.Errorf("tag %s 不存在或未指向 HEAD", tag)
		}
	}
	return Inputs{
		Version:   version,
		Tag:       tag,
		Mode:      mode,
		Commit:    commit,
		Repo:      abs,
		StartedAt: time.Now().UTC(),
	}, nil
}

// Preflight 在构建前核对工具链、签名密钥与输出目录。
// Preflight checks the toolchain, signing keys and output directories before building.
func Preflight(ctx context.Context, in Inputs, opts ReleaseOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkGoToolchain(ctx); err != nil {
		return err
	}
	if in.Mode == ModeFinal {
		if strings.TrimSpace(opts.SigningKey) == "" {
			return errors.New("final 发布必须提供 --signing-key")
		}
		if strings.TrimSpace(opts.PublicKey) == "" {
			return errors.New("final 发布必须提供 --public-key")
		}
		if _, err := os.Stat(opts.SigningKey); err != nil {
			return fmt.Errorf("签名密钥不可用：%w", err)
		}
		if _, err := os.Stat(opts.PublicKey); err != nil {
			return fmt.Errorf("公钥不可用：%w", err)
		}
	}
	// 发布说明与输出目录在汇总前就要检查：说明不符合模板时不要等到签名前才失败，
	// 输出目录先建出来，权限问题也会在构建前暴露。
	if strings.TrimSpace(opts.NotesPath) != "" {
		if err := CheckNotes(opts.NotesPath, in.Version); err != nil {
			return err
		}
	}
	if strings.TrimSpace(opts.OutDir) != "" {
		if err := os.MkdirAll(opts.OutDir, 0o755); err != nil { // #nosec G301 -- 发布输出目录
			return fmt.Errorf("创建输出目录：%w", err)
		}
	}
	return nil
}

// checkGoToolchain 要求发布构建使用 go.mod 固定的 1.27.3。
// checkGoToolchain requires the toolchain pinned by go.mod.
func checkGoToolchain(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "go", "env", "GOVERSION")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("读取 Go 工具链版本：%w", err)
	}
	return verifyGoVersion(strings.TrimSpace(string(out)))
}

// verifyGoVersion 把工具链核对单独抽出，便于用固定读数验证拒绝语义。
// verifyGoVersion isolates the toolchain check so the rejection is testable.
func verifyGoVersion(got string) error {
	if got != "go1.27.3" {
		return fmt.Errorf("发布构建必须使用 go1.27.3，当前工具链是 %s", got)
	}
	return nil
}

// gitOutput 运行 git 并返回标准输出；失败时带 stderr。
// gitOutput runs git and returns stdout, carrying stderr on failure.
func gitOutput(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", args...) // #nosec G204 -- 固定 git 与调用方写死的只读子命令
	cmd.Dir = repo
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s 失败：%w（%s）", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
