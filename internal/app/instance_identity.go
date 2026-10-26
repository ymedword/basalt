package app

// 本文件定义**实例身份**与**实例私有目录**的解析规则（14D T089 / T090，颗粒度文档
// §2.4.3）。三条纪律：
//
//   - 实例名册（`<工作区>/.basalt/instance.json`）是"这个工作区叫什么"的唯一来源，
//     首次初始化写入后随工作区迁移保持不变——同机多实例因此不会互相覆盖；
//   - 私有目录（主密钥 + 访问密码文件）**必须**在工作区之外，且只按名册推导：没有
//     名册的存量实例**不猜**旧路径（猜错等于把新密钥写到别处，或让既有密文变孤儿）；
//   - 名册里的实例标识会被拼进目录名，因此**先校验再使用**——脏名册是失败条件，
//     不是"随便取个后缀"的许可。
//
// The instance roster (instance.json) is the only source of the instance identity, and
// the private directory is derived from it only: an instance without a roster never
// gets a guessed path.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/ymedword/basalt/internal/cli"
)

const (
	// instanceMarkerFileName 是实例名册文件名（位于工作区根下）。
	instanceMarkerFileName = "instance.json"
	// instanceMarkerVersion 是本二进制理解的名册版本；更高版本拒绝解析。
	instanceMarkerVersion = 1
	// masterKeyFileName 是私有目录里的主密钥文件名。
	masterKeyFileName = "master.key"
	// adminTokenFileName 是私有目录里的访问密码文件名（与主密钥**分文件**）。
	adminTokenFileName = "admin-token"
	// instanceDataDirName 是数据根下的产品目录名。
	instanceDataDirName = "basalt"
	// instancesDirName 是产品目录下的实例清单目录名。
	instancesDirName = "instances"
)

// instanceIDPattern 是实例标识的合法形态：`inst_` + 16 位小写十六进制。
// 它同时是**目录名片段**，因此这里宁可严格——不符合就拒绝，不做清洗。
// The pattern is also a directory-name fragment, so it is checked strictly.
var instanceIDPattern = regexp.MustCompile(`^inst_[0-9a-f]{16}$`)

// errInstanceIdentityMissing 表示工作区里没有实例名册。存量实例（`basalt init` 建的、
// 或 US-14 之前建的）都属于这一类：调用方**不得**据此猜路径或补写名册。
// A missing roster is a normal state for pre-existing instances, never a cue to guess.
var errInstanceIdentityMissing = errors.New("工作区里没有实例名册（instance.json）")

// instanceMarker 是 `instance.json` 的内容：版本 + 不可变实例标识。
// instanceMarker is the roster content: a version and an immutable instance id.
type instanceMarker struct {
	Version    int    `json:"version"`
	InstanceID string `json:"instance_id"`
}

// instanceMarkerPath 返回名册路径（工作区根下的 instance.json）。
// instanceMarkerPath returns the roster path under the workspace root.
func instanceMarkerPath(workDir string) string {
	return filepath.Join(workDir, workspaceDirName, instanceMarkerFileName)
}

// newInstanceID 生成一个新实例标识（8 字节随机数的十六进制）。
// newInstanceID mints a fresh instance id from 8 random bytes.
func newInstanceID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("生成实例标识: %w", err)
	}
	return "inst_" + hex.EncodeToString(raw[:]), nil
}

// readInstanceMarker 读取并校验名册：文件不存在返回 `(zero, false, nil)`；
// 内容非法、版本过高、标识形态不合法一律**报错**（fail-closed），不按缺省值继续。
// readInstanceMarker reads and validates the roster; a malformed roster is an error,
// never a fallback to defaults.
func readInstanceMarker(workDir string) (instanceMarker, bool, error) {
	path := instanceMarkerPath(workDir)
	// 读经工作目录根句柄：目录名与文件名都是常量，工作目录为空串时同样在这里失败关闭。
	// The read goes through a root handle on the working directory, so an empty one fails closed.
	root, rootErr := os.OpenRoot(workDir)
	if errors.Is(rootErr, fs.ErrNotExist) {
		return instanceMarker{}, false, nil
	}
	if rootErr != nil {
		return instanceMarker{}, false, fmt.Errorf("读取实例名册 %s: %w", path, rootErr)
	}
	defer func() { _ = root.Close() }()
	raw, err := root.ReadFile(filepath.Join(workspaceDirName, instanceMarkerFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return instanceMarker{}, false, nil
	}
	if err != nil {
		return instanceMarker{}, false, fmt.Errorf("读取实例名册 %s: %w", path, err)
	}
	var marker instanceMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return instanceMarker{}, false, fmt.Errorf(
			"实例名册 %s 不是合法 JSON——按失败处理，请人工核对或从备份恢复: %w", path, err)
	}
	if marker.Version > instanceMarkerVersion {
		return instanceMarker{}, false, fmt.Errorf(
			"实例名册 %s 的版本为 %d，高于本二进制支持的 %d——请升级程序，不要手工改版本号",
			path, marker.Version, instanceMarkerVersion)
	}
	if !instanceIDPattern.MatchString(marker.InstanceID) {
		return instanceMarker{}, false, fmt.Errorf(
			"实例名册 %s 的 instance_id 形态非法——要求 inst_ 加 16 位十六进制，请人工核对",
			path)
	}
	return marker, true, nil
}

// instanceDataRoot 返回当前用户的数据根目录：Windows 取 `%LOCALAPPDATA%`，
// macOS 取 `~/Library/Application Support`，其余取 `$XDG_DATA_HOME`（缺省
// `~/.local/share`）。解析不出即报错并提示显式配置——不猜、不落到临时目录。
// instanceDataRoot resolves the per-user data root; an unresolvable root is an error.
func instanceDataRoot() (string, error) {
	switch runtime.GOOS {
	case "windows":
		if dir := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); dir != "" {
			return dir, nil
		}
		if dir, err := os.UserConfigDir(); err == nil {
			return dir, nil
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support"), nil
		}
	default:
		if dir := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); dir != "" && filepath.IsAbs(dir) {
			return dir, nil
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "share"), nil
		}
	}
	return "", errors.New(
		"无法解析当前用户的数据目录（LOCALAPPDATA / XDG_DATA_HOME / HOME 均不可用）" +
			"——请在 secrets.master_key_file 显式给出主密钥路径")
}

// defaultInstancePrivateDir 按名册推导实例私有目录；没有名册即返回
// `errInstanceIdentityMissing`（存量实例不猜路径）。
// defaultInstancePrivateDir derives the private directory from the roster only.
func defaultInstancePrivateDir(workDir string) (string, error) {
	marker, present, err := readInstanceMarker(workDir)
	if err != nil {
		return "", err
	}
	if !present {
		return "", errInstanceIdentityMissing
	}
	root, err := instanceDataRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, instanceDataDirName, instancesDirName, marker.InstanceID), nil
}

// defaultMasterKeyFile 返回缺省主密钥路径（私有目录下的 master.key）。
// defaultMasterKeyFile returns the default master-key path inside the private directory.
func defaultMasterKeyFile(workDir string) (string, error) {
	dir, err := defaultInstancePrivateDir(workDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, masterKeyFileName), nil
}

// defaultAdminTokenFile 返回缺省访问密码文件路径（与主密钥同目录、分文件）。
// defaultAdminTokenFile returns the admin-token path beside the master key.
func defaultAdminTokenFile(workDir string) (string, error) {
	dir, err := defaultInstancePrivateDir(workDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, adminTokenFileName), nil
}

// resolveMasterKeyFile 解析**启动时生效**的主密钥路径（T089，颗粒度文档 §2.4.3）：
// 显式配置优先；未配置时按名册推导缺省路径。两种情形不推导——
// `auto_init: false`（显式纯 env 实例）与没有名册的存量实例（不猜旧路径）。
// 返回空串表示"启动时未解析出主密钥路径"：托管写入会以固定原因拒绝，**但**用户仍可
// 通过管理台显式初始化（`Init` 会建立名册并把路径交给共享 locator，见 master_key_locator.go）。
// resolveMasterKeyFile applies the explicit-then-derived rule at startup and returns an
// empty path when the instance has no roster yet; a console init may establish it later.
func resolveMasterKeyFile(cfg *cli.Config, workDir string) (string, error) {
	if cfg != nil {
		if explicit := strings.TrimSpace(cfg.Secrets.MasterKeyFile); explicit != "" {
			return cfg.Secrets.MasterKeyFile, nil
		}
		if !cfg.Secrets.AutoInitEnabled() {
			return "", nil
		}
	}
	path, err := defaultMasterKeyFile(workDir)
	if errors.Is(err, errInstanceIdentityMissing) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

// ensureInstanceMarker 在用户显式启用加密托管时建立实例身份：已有名册原样返回，
// 没有就以 `O_EXCL` 写一个，并发时以盘上的名册为准（标识不可变）。
//
// 写名册是**身份**动作，不是密钥动作：它只回答"这个工作区叫什么"，密钥路径仍由
// 名册推导且必须在工作区之外（`publishMasterKey` 的 `LoadMasterKey` 读回会再校验）。
// 既有名册（含版本过高 / 形态非法）一律按读名册的错误上抛，不在坏名册上补写。
// ensureInstanceMarker establishes the instance identity on explicit enablement; an
// existing roster is returned verbatim and a malformed one is an error, never rewritten.
func ensureInstanceMarker(workDir string) (instanceMarker, error) {
	if strings.TrimSpace(workDir) == "" {
		return instanceMarker{}, errors.New("工作目录为空，无法建立实例标识")
	}
	marker, present, err := readInstanceMarker(workDir)
	if err != nil {
		return instanceMarker{}, err
	}
	if present {
		return marker, nil
	}
	base, err := os.OpenRoot(workDir)
	if err != nil {
		return instanceMarker{}, fmt.Errorf("打开工作目录 %s: %w", workDir, err)
	}
	defer func() { _ = base.Close() }()
	// 工作区目录可能还不存在（从未跑过 `basalt init` 的目录树）：名册要落在工作区根下。
	if err := base.MkdirAll(workspaceDirName, 0o750); err != nil {
		return instanceMarker{}, fmt.Errorf("创建工作区目录 %s: %w", workspaceDirName, err)
	}
	id, err := newInstanceID()
	if err != nil {
		return instanceMarker{}, err
	}
	candidate := instanceMarker{Version: instanceMarkerVersion, InstanceID: id}
	raw, err := marshalInstanceMarker(candidate)
	if err != nil {
		return instanceMarker{}, err
	}
	created, err := createExclusiveFile(base, filepath.Join(workspaceDirName, instanceMarkerFileName), raw)
	if err != nil {
		return instanceMarker{}, fmt.Errorf("写入实例名册 %s: %w", instanceMarkerFileName, err)
	}
	if created {
		return candidate, nil
	}
	// 另一个写入者抢先：以盘上的名册为准，标识不可变。
	winner, present, readErr := readInstanceMarker(workDir)
	if readErr != nil {
		return instanceMarker{}, readErr
	}
	if !present {
		return instanceMarker{}, errors.New("实例名册写入竞争后仍不可读——请人工核对工作区")
	}
	return winner, nil
}
