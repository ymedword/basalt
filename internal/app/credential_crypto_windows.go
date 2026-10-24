//go:build windows

package app

// 本文件是主密钥平台校验的 Windows 分支（模块卡 R4.8 / T129；复核 U18 补齐 DACL 校验）。
//
// Windows 的权限位是合成值（可写文件一律报 0666），按位判卷没有意义；真正的访问控制在
// DACL 上。每次 LoadMasterKey 都重新读取**文件与其父目录**的安全描述符，只放行三类
// 受托人：当前进程用户、SYSTEM、Administrators。其余仍生效的 allow ACE（其他用户、
// Users / Authenticated Users 等组、AppContainer 包 SID）、缺失或为空的 DACL（等同
// 所有人可访问）、无法读取的安全描述符、无法识别的 ACE 类型——一律拒绝。
//
// 父目录一起校验的理由：对目录有写权限的人可以删除 / 替换密钥文件（FILE_DELETE_CHILD），
// 只收紧文件本身不够。
//
// 结果不缓存：权限在校验之后被放宽，下一次读取必须能发现。
// The Windows branch reads the file and parent-directory DACLs on every load and allows
// only the current user, SYSTEM and Administrators. Nothing is cached, so an ACL loosened
// after one successful read is caught by the next one.

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/ymedword/basalt/internal/core"
)

// hardLinkCount 用 GetFileInformationByHandle 读 NumberOfLinks。
// hardLinkCount reads NumberOfLinks through GetFileInformationByHandle.
func hardLinkCount(path string, _ os.FileInfo) (uint64, bool) {
	// #nosec G304 -- 路径来自部署者配置（secrets.master_key_file），不是请求输入；
	// 调用方已规范化、拒绝符号链接并校验不在工作区内，这里只读链接计数。
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer func() { _ = file.Close() }()
	var data syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &data); err != nil {
		return 0, false
	}
	return uint64(data.NumberOfLinks), true
}

// checkKeyPermissions 读取文件与父目录的 DACL 并逐条判卷（见文件头）。
// checkKeyPermissions validates the DACL of the key file and its parent directory.
func checkKeyPermissions(dirPath, filePath string) error {
	userSID, err := currentProcessUserSID()
	if err != nil {
		return err
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("%w: 无法构造 SYSTEM 的 SID: %v", core.ErrCredentialUnavailable, err)
	}
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("%w: 无法构造 Administrators 的 SID: %v", core.ErrCredentialUnavailable, err)
	}
	allowed := []*windows.SID{userSID, systemSID, adminsSID}
	for _, target := range []struct{ path, label string }{
		{filePath, "主密钥文件"},
		{dirPath, "主密钥目录"},
	} {
		if err := checkTightDACL(target.path, target.label, allowed); err != nil {
			return err
		}
	}
	return nil
}

// currentProcessUserSID 返回当前进程令牌的用户 SID；测试夹具也用它收紧 ACL。
// currentProcessUserSID returns the token user SID; the test fixtures reuse it.
func currentProcessUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("%w: 无法读取当前进程用户令牌: %v", core.ErrCredentialUnavailable, err)
	}
	return user.User.Sid, nil
}

// checkTightDACL 按「只允许当前用户 + SYSTEM + Administrators」判一份 DACL：
//   - 读不到安全描述符 / 安全描述符没有 DACL（NULL DACL 等同所有人可访问）→ 拒绝；
//   - inherit-only ACE 不影响本对象，跳过；deny ACE 只会收窄，跳过；零权限 allow ACE 无授予，跳过；
//   - 其余 allow ACE 的受托人不在白名单 → 拒绝；对象 / 回调等无法识别的 ACE 类型 → 保守拒绝。
//
// checkTightDACL accepts only the three allowed trustees; anything it cannot classify is
// refused rather than assumed harmless.
func checkTightDACL(path, label string, allowed []*windows.SID) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("%w: %s无法读取安全描述符", core.ErrCredentialUnavailable, label)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return fmt.Errorf("%w: %s没有 DACL（NULL DACL 等同允许所有人访问）", core.ErrCredentialUnavailable, label)
	}
	for index := 0; index < int(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, uint32(index), &ace); err != nil || ace == nil {
			return fmt.Errorf("%w: %s无法读取第 %d 条 ACE", core.ErrCredentialUnavailable, label, index)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return fmt.Errorf("%w: %s的 DACL 含无法识别的 ACE 类型 %d",
				core.ErrCredentialUnavailable, label, ace.Header.AceType)
		}
		if ace.Mask == 0 {
			continue
		}
		// ACCESS_ALLOWED_ACE.SidStart 是变长 SID 的起始 4 字节；GetAce 返回的指针指向
		// 同一块 ACE 分配，SID 按其自身头部长度落在其中。
		// #nosec G103 -- 这是安全描述符 ACL 的固定读取方式：x/sys 只给 ACCESS_ALLOWED_ACE，
		// 变长 SID 必须以 SidStart 地址取出；指针不离开本次 ACE 遍历，转换前后都指向
		// GetNamedSecurityInfo 分配的同一块内存，没有悬垂窗口，也没有免除此转换的封装。
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !trusteeAllowed(sid, allowed) {
			return fmt.Errorf("%w: %s的 DACL 允许未授权主体 %s（只允许当前用户、SYSTEM 与 Administrators）",
				core.ErrCredentialUnavailable, label, sidLabel(sid))
		}
	}
	return nil
}

// trusteeAllowed 判断 ACE 的受托人 SID 是否在白名单内。
// trusteeAllowed reports whether the ACE trustee is one of the allowed SIDs.
func trusteeAllowed(sid *windows.SID, allowed []*windows.SID) bool {
	for _, candidate := range allowed {
		if sid.Equals(candidate) {
			return true
		}
	}
	return false
}

// sidLabel 给出错误文案里的 SID 文本；解析失败时回固定的占位说明，不回空串。
// sidLabel renders a SID for the error text, never as an empty string.
func sidLabel(sid *windows.SID) string {
	if text := sid.String(); text != "" {
		return text
	}
	return "（SID 无法解析）"
}
