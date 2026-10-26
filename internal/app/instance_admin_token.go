package app

// 本文件是管理面访问密码的**第三个来源**（14D T090，颗粒度文档 §2.4.3 / §3.1.2）：
// `http.admin_auth.mode: token` 而 `token` 省略时，从实例私有目录的 `admin-token`
// 读取；文件不存在时，**只有可证明的新实例**才生成一个并提示一次。
//
// 三条纪律：
//   - 三处（配置文件 / 环境变量 / 密码文件）都没有 → 启动失败并点名三处，
//     **不得**回落到 `local_trust`——那等于把管理面在无凭据状态下开放；
//   - 密码文件与主密钥**分文件**（恢复语义不同：主密钥丢了要恢复或重录凭据，
//     密码丢了重新生成即可），但共用同一个受限目录；
//   - 密码**不得**进日志或错误文本：非交互终端只输出路径，交互式只在生成那一刻
//     显示一次值。
//
// The token file is the third source of the management password; all three sources
// missing is a startup failure, never a fallback to local_trust. The password itself
// never reaches logs, and a non-interactive run only prints the path.

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ymedword/basalt/internal/cli"
)

const (
	// adminAuthModeToken 是 token 模式的取值（与 web.ValidateAdminAuth 同一字面量）。
	adminAuthModeToken = "token"
	// adminTokenBytes 是自动生成密码的随机字节数（base64url 后 43 字符）。
	adminTokenBytes = 32
	// adminTokenFilePerm 是密码文件权限（Unix）：仅属主可读写。
	adminTokenFilePerm os.FileMode = 0o600
)

// adminTokenSource 标明这个密码来自哪一处（用于提示与排障，不含值）。
// adminTokenSource names where the effective password came from.
type adminTokenSource string

const (
	// adminTokenSourceConfig 表示配置文件（经 `${VAR}` 解析）提供了密码。
	adminTokenSourceConfig adminTokenSource = "config"
	// adminTokenSourceFile 表示从实例私有目录的 admin-token 文件读到。
	adminTokenSourceFile adminTokenSource = "file"
	// adminTokenSourceGenerated 表示本次首次启动生成并写入了该文件。
	adminTokenSourceGenerated adminTokenSource = "generated"
)

// adminTokenResolution 是解析结果：密码值、来源与文件位置（配置来源时路径为空）。
// adminTokenResolution carries the value, its source and the file path when applicable.
type adminTokenResolution struct {
	Token  string
	Path   string
	Source adminTokenSource
}

// resolveAdminToken 解析生效的管理面密码；非 token 模式直接返回空结果。
// resolveAdminToken resolves the effective management password for token mode only.
func (a *App) resolveAdminToken(cfg *cli.Config) (adminTokenResolution, error) {
	if cfg == nil || cfg.HTTP.AdminAuth.Mode != adminAuthModeToken {
		return adminTokenResolution{}, nil
	}
	if token := strings.TrimSpace(cfg.HTTP.AdminAuth.Token); token != "" {
		return adminTokenResolution{Token: token, Source: adminTokenSourceConfig}, nil
	}

	path, err := defaultAdminTokenFile(a.workDir)
	if errors.Is(err, errInstanceIdentityMissing) {
		// 存量实例没有名册：不猜路径，直接把"三处都没有"报出来。
		return adminTokenResolution{}, missingAdminTokenError("")
	}
	if err != nil {
		return adminTokenResolution{}, err
	}
	token, readErr := readAdminTokenFile(path)
	switch {
	case readErr == nil:
		return adminTokenResolution{Token: token, Path: path, Source: adminTokenSourceFile}, nil
	case !errors.Is(readErr, fs.ErrNotExist):
		return adminTokenResolution{}, readErr
	}
	if !a.freshInstance {
		return adminTokenResolution{}, missingAdminTokenError(path)
	}

	token, err = generateAdminToken()
	if err != nil {
		return adminTokenResolution{}, err
	}
	if err := writeAdminTokenFile(path, token); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// 并发首启：另一个进程已经写好，以盘上的为准（不覆盖、不生成第二份）。
			existing, readErr := readAdminTokenFile(path)
			if readErr != nil {
				return adminTokenResolution{}, readErr
			}
			return adminTokenResolution{Token: existing, Path: path, Source: adminTokenSourceFile}, nil
		}
		return adminTokenResolution{}, err
	}
	return adminTokenResolution{Token: token, Path: path, Source: adminTokenSourceGenerated}, nil
}

// missingAdminTokenError 是"三处都没有"的固定错误：点名配置键、环境变量与文件路径，
// 并明说**不**回落 local_trust。
// missingAdminTokenError names all three sources and refuses the local_trust fallback.
func missingAdminTokenError(path string) error {
	where := path
	if where == "" {
		where = "实例私有目录（工作区里没有 instance.json，无法推导路径）"
	}
	return fmt.Errorf(
		"必填配置项 http.admin_auth.token 缺失——三处都没有提供：配置 http.admin_auth.token（${VAR} 占位）、"+
			"对应环境变量、密码文件 %s。新建实例首次 serve 会自动生成该文件；存量实例请显式配置或手工写入该文件。"+
			"**不**回落到 local_trust", where)
}

// generateAdminToken 生成一次性的随机密码（32 字节，base64url 无填充）。
// generateAdminToken mints a random password from 32 bytes.
func generateAdminToken() (string, error) {
	raw := make([]byte, adminTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成管理面访问密码: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// readAdminTokenFile 读取密码文件：去掉尾部空白（编辑器与 echo 常带换行），
// 空内容按失败处理——"文件在但是空的"不是有效来源。
// readAdminTokenFile trims trailing whitespace and refuses an empty file.
func readAdminTokenFile(path string) (string, error) {
	// #nosec G304 -- 路径由实例私有目录推导（名册实例标识 + 固定目录名 + 固定文件名），
	// 不是请求输入；与主密钥读取同一档豁免，语义见 credential_crypto.go。
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("密码文件 %s 内容为空——写入一行密码后重启，或显式配置 token", path)
	}
	if strings.Contains(token, "${") {
		return "", fmt.Errorf("密码文件 %s 里仍是未解析的 ${...} 占位——写入真实密码", path)
	}
	return token, nil
}

// writeAdminTokenFile 先把私有目录按平台收紧权限，再以 `O_CREAT|O_EXCL|0600` 写文件。
// 已存在返回 `fs.ErrExist`（由调用方按"别人抢先"处理）。
// writeAdminTokenFile tightens the private directory, then writes the file exclusively.
func writeAdminTokenFile(path, token string) error {
	if err := prepareSecretDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	// #nosec G304 -- 同上：实例私有目录推导出的固定文件名，`O_CREATE|O_EXCL` 独占创建，
	// 不是请求输入（已存在的文件绝不覆盖）。
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, adminTokenFilePerm)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入密码文件 %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭密码文件 %s: %w", path, err)
	}
	return nil
}

// announceAdminToken 只对**本次生成**的密码提示一次：交互终端显示值，非交互只显示路径。
// 其余来源不打印任何东西——密码不进常规输出。
// announceAdminToken prints the freshly generated password once; an interactive
// terminal gets the value, anything else only the path.
func announceAdminToken(out io.Writer, resolution adminTokenResolution, interactive bool) {
	if resolution.Source != adminTokenSourceGenerated {
		return
	}
	if interactive {
		fmt.Fprintf(out, "管理台访问密码（只显示这一次）：%s\n", resolution.Token)
		fmt.Fprintf(out, "密码文件：%s（与主密钥同目录、分文件；请妥善保存）\n", resolution.Path)
		return
	}
	fmt.Fprintf(out, "管理台访问密码已写入 %s（非交互终端不显示内容，请查看该文件）\n", resolution.Path)
}

// isInteractiveWriter 报告输出是不是字符设备（终端）。重定向到文件 / 管道时为假。
// isInteractiveWriter reports whether the writer is a character device.
func isInteractiveWriter(out io.Writer) bool {
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
