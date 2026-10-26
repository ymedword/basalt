//go:build windows

package app

// 本文件是实例私有目录的平台准备（14D T088）与硬链接退化判定（T086）的 Windows
// 分支。`os.Chmod` 在 Windows 只切只读位、不设 DACL；"先建文件再 SetSecurityInfo"
// 会让文件在这段时间里按继承 ACL 可访问。因此**首选** `CreateDirectory` +
// `SECURITY_ATTRIBUTES`：目录从创建那一刻起就带受限 DACL（没有"先宽后收"的窗口），
// 随后 `GetNamedSecurityInfo` 读回校验。API 不可用即中止，**不退回**"建完再补权限"。
// The Windows branch creates the directory with a restricted DACL through
// CreateDirectory + SECURITY_ATTRIBUTES, then re-reads it with GetNamedSecurityInfo.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/ymedword/basalt/internal/core"
)

// restrictedDirectorySDDL 构造「仅当前用户 + SYSTEM + Administrators」的受保护 DACL：
// `P` 表示 SE_DACL_PROTECTED（不继承父目录的 ACE），`OICI` 让子对象继承，`FA` 是完全控制。
// restrictedDirectorySDDL renders the protected DACL used at directory creation.
func restrictedDirectorySDDL() (string, error) {
	userSID, err := currentProcessUserSID()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", userSID.String()), nil
}

// prepareSecretDirectory 逐级创建缺失目录（每级都带受限 DACL），随后读回校验最内层。
// 已存在的目录**只校验不改写**：运维自建的目录如果 DACL 过宽，正确做法是拒绝启动
// 并让人修，而不是替它静默重写安全描述符。
// prepareSecretDirectory creates every missing level with the restricted DACL, then
// re-reads the innermost directory instead of rewriting an existing one.
func prepareSecretDirectory(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("%w: 实例私有目录路径无法规范化为绝对路径", core.ErrCredentialUnavailable)
	}
	missing, err := missingDirectories(abs)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		sddl, sddlErr := restrictedDirectorySDDL()
		if sddlErr != nil {
			return sddlErr
		}
		descriptor, sdErr := windows.SecurityDescriptorFromString(sddl)
		if sdErr != nil {
			return fmt.Errorf("%w: 构造实例私有目录安全描述符失败", core.ErrCredentialUnavailable)
		}
		attributes := &windows.SecurityAttributes{
			Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
			SecurityDescriptor: descriptor,
		}
		for _, path := range missing {
			pathp, ptrErr := windows.UTF16PtrFromString(path)
			if ptrErr != nil {
				return fmt.Errorf("%w: 实例私有目录路径无法转换", core.ErrCredentialUnavailable)
			}
			if createErr := windows.CreateDirectory(pathp, attributes); createErr != nil {
				if errors.Is(createErr, windows.ERROR_ALREADY_EXISTS) {
					continue
				}
				return fmt.Errorf("%w: 创建实例私有目录失败", core.ErrCredentialUnavailable)
			}
		}
	}
	allowed, err := allowedKeyTrustees()
	if err != nil {
		return err
	}
	return checkTightDACL(abs, "实例私有目录", allowed)
}

// missingDirectories 返回从最浅到最深的缺失层级（已存在的最深祖先之下）。
// missingDirectories lists the missing levels from shallowest to deepest.
func missingDirectories(abs string) ([]string, error) {
	var missing []string
	current := abs
	for {
		_, err := os.Lstat(current)
		switch {
		case err == nil:
			slices.Reverse(missing)
			return missing, nil
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, current)
		default:
			return nil, fmt.Errorf("%w: 读取实例私有目录状态失败", core.ErrCredentialUnavailable)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// 到达卷根：卷根本身不可创建，缺失链到此为止。
			slices.Reverse(missing)
			return missing, nil
		}
		current = parent
	}
}

// isHardLinkUnsupported 判断 `os.Link` 的失败是否属于"该文件系统不支持硬链接"
// （FAT / exFAT）。此时退化为 `O_EXCL` 直写，不覆盖语义仍然成立。
// isHardLinkUnsupported routes filesystems without hard links to the O_EXCL fallback.
func isHardLinkUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
