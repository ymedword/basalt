package app

// 本文件是主密钥的**原子发布**与**发布残留恢复**（14D T086 / T087）。
//
// 发布用 `os.Link` 而不是 `os.Rename`：Go 的 `Rename` 在 Windows 上走
// `MoveFileEx(MOVEFILE_REPLACE_EXISTING)`，会覆盖已存在文件——"原子 rename"在
// Windows 上不是"不覆盖"语义。`Link` 两平台都原子，且目标存在即失败。
//
// 硬链接与 `LoadMasterKey` 的 `nlink > 1` 检查天然冲突：`Link` 成功、`unlink tmp`
// 之前中断会留下 `nlink == 2` 的完整密钥，下次启动若"有文件就 adopt"会被自己的
// 检查拒掉。恢复只处理**能被 `os.SameFile` 证明指向 `<final>` 的临时链接**，
// 并且绝不放宽 `nlink > 1` 检查——那会把真正的多路径泄露一起放行。
//
// Publication is an os.Link (atomic, never overwrites) rather than os.Rename, which
// silently replaces on Windows. Residue recovery only removes a temp link proven by
// os.SameFile to be the same inode, and never relaxes the hard-link check.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ymedword/basalt/internal/core"
)

// masterKeyTempSuffix 是发布临时链接的固定后缀（`<final>.link`）。
// masterKeyTempSuffix is the fixed temp-link suffix next to the final key path.
const masterKeyTempSuffix = ".link"

// errMasterKeyAlreadyExists 表示目标文件已存在：发布是排他语义，调用方应转入 adopt。
// errMasterKeyAlreadyExists makes the caller fall back to adoption.
var errMasterKeyAlreadyExists = fmt.Errorf(
	"%w: 主密钥文件已存在，拒绝覆盖（不提供自动轮换）", core.ErrCredentialUnavailable)

// errPublishResidueUnprovable 表示存在无法证明来源的发布残留（或多路径硬链接）：
// 一律进恢复态，**不**删文件。
// errPublishResidueUnprovable sends the instance to the recovery state.
var errPublishResidueUnprovable = errors.New("主密钥发布残留来源不可证明")

// secretDirectory 给出主密钥所在目录（实例私有目录）。
// secretDirectory returns the instance-private directory holding the key.
func secretDirectory(masterKeyFile string) string { return filepath.Dir(masterKeyFile) }

// marshalMasterKeyFile 渲染密钥文件正文；与 `writeNewMasterKeyFile` 共用同一形态，
// 保证"临时链接写的内容"就是"最终文件应有的内容"。
// marshalMasterKeyFile renders the key document shared by both write paths.
func marshalMasterKeyFile(key MasterKey) ([]byte, error) { return renderMasterKeyDocument(key) }

// publishMasterKey 把密钥发布到 `finalPath`：受限临时链接 → `os.Link` 发布 →
// unlink 临时 → 完整 `LoadMasterKey` 读回。目标已存在时返回
// `errMasterKeyAlreadyExists` 且不留临时文件；不支持硬链接的文件系统退化为
// `O_EXCL` 直写（接受"半个密钥"窗口，下次启动按恢复态处理）。
// publishMasterKey links a temp file into place, then re-reads through LoadMasterKey.
func publishMasterKey(finalPath, workspaceRoot string, key MasterKey) error {
	document, err := marshalMasterKeyFile(key)
	if err != nil {
		return err
	}
	tmpPath := finalPath + masterKeyTempSuffix
	if err := writeMasterKeyTemp(tmpPath, document); err != nil {
		return err
	}
	linkErr := os.Link(tmpPath, finalPath)
	switch {
	case linkErr == nil:
		if rmErr := os.Remove(tmpPath); rmErr != nil {
			return fmt.Errorf("%w: 发布主密钥后清理临时链接失败", core.ErrCredentialUnavailable)
		}
	case errors.Is(linkErr, fs.ErrExist):
		// 已经有文件了：不覆盖，撤掉自己刚建的临时链接，交给 adopt。
		if rmErr := os.Remove(tmpPath); rmErr != nil {
			return fmt.Errorf("%w: 清理主密钥临时链接失败", core.ErrCredentialUnavailable)
		}
		return errMasterKeyAlreadyExists
	case isHardLinkUnsupported(linkErr):
		// 退化路径：先撤临时链接再 O_EXCL 直写，避免留下两条路径。
		if rmErr := os.Remove(tmpPath); rmErr != nil {
			return fmt.Errorf("%w: 清理主密钥临时链接失败", core.ErrCredentialUnavailable)
		}
		if writeErr := writeNewMasterKeyFile(finalPath, key); writeErr != nil {
			if errors.Is(writeErr, fs.ErrExist) {
				return errMasterKeyAlreadyExists
			}
			return writeErr
		}
	default:
		cleanupErr := os.Remove(tmpPath)
		return errors.Join(fmt.Errorf("%w: 发布主密钥失败", core.ErrCredentialUnavailable), linkErr, cleanupErr)
	}

	// 读回校验走完整 LoadMasterKey（路径 / 权限 / nlink / 格式 / key_id）。
	// 失败时撤掉**本次刚发布**的文件：它来源可证明，留着只会让下次启动进恢复态。
	check, err := LoadMasterKey(finalPath, workspaceRoot)
	if err != nil {
		return errors.Join(err, removeKeyFile(finalPath))
	}
	check.Zero()
	return nil
}

// writeMasterKeyTemp 写临时链接文件：`O_EXCL` 保证不覆盖，权限从第一次写入起受限。
// writeMasterKeyTemp creates the temp file exclusively with restricted permissions.
func writeMasterKeyTemp(tmpPath string, document []byte) error {
	// #nosec G304 -- 路径来自部署者配置的 `secrets.master_key_file` 派生，不是请求输入；
	// 目录已经在写之前通过平台权限校验，`O_EXCL` 保证不覆盖既有文件。
	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// 上一次中断的残留：来源不可证明时不删，直接报错进恢复态判定。
			return fmt.Errorf("%w: 主密钥临时链接已存在（可能是上次发布的残留）",
				core.ErrCredentialUnavailable)
		}
		return fmt.Errorf("%w: 创建主密钥临时链接失败", core.ErrCredentialUnavailable)
	}
	if chmodErr := file.Chmod(0o600); chmodErr != nil {
		return errors.Join(fmt.Errorf("%w: 设置主密钥临时链接权限失败", core.ErrCredentialUnavailable),
			file.Close(), removeTempFile(tmpPath))
	}
	if _, writeErr := file.Write(document); writeErr != nil {
		return errors.Join(fmt.Errorf("%w: 写入主密钥临时链接失败", core.ErrCredentialUnavailable),
			file.Close(), removeTempFile(tmpPath))
	}
	if syncErr := file.Sync(); syncErr != nil {
		return errors.Join(fmt.Errorf("%w: 同步主密钥临时链接失败", core.ErrCredentialUnavailable),
			file.Close(), removeTempFile(tmpPath))
	}
	if closeErr := file.Close(); closeErr != nil {
		return errors.Join(fmt.Errorf("%w: 关闭主密钥临时链接失败", core.ErrCredentialUnavailable),
			removeTempFile(tmpPath))
	}
	return nil
}

// removeTempFile 删除**本次刚创建**的临时链接（失败不掩蔽原始错误）。
// removeTempFile drops a temp link this call created.
func removeTempFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("清理主密钥临时链接: %w", err)
	}
	return nil
}

// recoverPublishResidue 在实例锁内、读判定表之前执行的四分支恢复：
//
//  1. `<final>` 不存在：只清理同名临时链接（普通文件）；它是符号链接或目录时不动；
//  2. `<final>` 存在且 `nlink == 1`：若临时链接与它是同一 inode，删掉这条无害重复；
//  3. `<final>` 存在且 `nlink > 1`：仅当临时链接存在且 `os.SameFile` 为真才认定是
//     本次发布的残留 → unlink → 重新 stat 期望 `nlink == 1` → 再走完整 LoadMasterKey；
//  4. `<final>` 存在且 `nlink > 1`，临时链接缺失或指向别的 inode：来源不可证明 →
//     返回 `errPublishResidueUnprovable`，**不删**任何文件。
//
// recoverPublishResidue returns whether it removed a provably-owned temp link.
func recoverPublishResidue(finalPath, workspaceRoot string) (bool, error) {
	tmpPath := finalPath + masterKeyTempSuffix
	finalInfo, err := os.Lstat(finalPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// 读不到状态：交给判定表按"读不回来"处理，这里不猜。
			return false, nil
		}
		// 分支 1：只剩临时链接。
		tmpInfo, tmpErr := os.Lstat(tmpPath)
		if tmpErr != nil {
			if errors.Is(tmpErr, fs.ErrNotExist) {
				return false, nil
			}
			return false, nil
		}
		if !tmpInfo.Mode().IsRegular() {
			// 指向别处的链接 / 目录：来源不可证明，不动它。
			return false, nil
		}
		if rmErr := os.Remove(tmpPath); rmErr != nil {
			return false, fmt.Errorf("清理主密钥临时链接失败: %w", rmErr)
		}
		return true, nil
	}
	if !finalInfo.Mode().IsRegular() {
		return false, nil
	}
	links, known := hardLinkCount(finalPath, finalInfo)
	if !known {
		return false, nil
	}
	if links == 1 {
		// 分支 2：正常；临时链接若与 `<final>` 同一 inode（`Link` 成功后的重复），删掉。
		if same, ok := sameInode(tmpPath, finalInfo); ok && same {
			if rmErr := os.Remove(tmpPath); rmErr != nil {
				return false, fmt.Errorf("清理主密钥临时链接失败: %w", rmErr)
			}
			return true, nil
		}
		return false, nil
	}
	// 分支 3 / 4：`nlink > 1`。
	same, ok := sameInode(tmpPath, finalInfo)
	if !ok || !same {
		return false, errPublishResidueUnprovable
	}
	if rmErr := os.Remove(tmpPath); rmErr != nil {
		return false, fmt.Errorf("清理主密钥临时链接失败: %w", rmErr)
	}
	// 必须重新 stat：`finalInfo` 是 unlink 之前的快照，Unix 分支按传入的 FileInfo 读
	// `Nlink`，复用快照会一直读到 2（Windows 分支按路径重读、恰好没暴露）——两平台
	// 判定就此分叉，Linux 上表现为把可证明的残留误判成不可证明。契约本就是"重新 stat
	// 期望 nlink == 1"，这里真的重读。
	// Re-stat after unlinking: the Unix branch reads Nlink from the given FileInfo, so
	// reusing the pre-unlink snapshot reads a stale 2 and misclassifies provable residue.
	afterInfo, statErr := os.Lstat(finalPath)
	if statErr != nil {
		return false, errPublishResidueUnprovable
	}
	after, known := hardLinkCount(finalPath, afterInfo)
	if !known || after != 1 {
		return false, errPublishResidueUnprovable
	}
	key, loadErr := LoadMasterKey(finalPath, workspaceRoot)
	if loadErr != nil {
		return false, loadErr
	}
	key.Zero()
	return true, nil
}

// sameInode 报告临时链接是否存在且与 `<final>` 是同一 inode；第二个返回值表示
// "能否做出比较"（临时链接缺失或 stat 失败时为 false）。
// sameInode reports whether the temp link is the same inode as the final file.
func sameInode(tmpPath string, finalInfo os.FileInfo) (bool, bool) {
	tmpInfo, err := os.Lstat(tmpPath)
	if err != nil {
		return false, false
	}
	return os.SameFile(finalInfo, tmpInfo), true
}

// publishArtifacts 是"发布产物存在性"的枚举，恢复查证用它区分
// "可证明未创建任何文件" 与 "有东西但来源不明"。
// publishArtifacts enumerates what publication left on disk.
type publishArtifacts int

const (
	// artifactsNone 表示 `<final>` 与临时链接都不存在。
	artifactsNone publishArtifacts = iota
	// artifactsPartial 表示只存在其中之一，或两者不是同一 inode。
	artifactsPartial
)

// publishArtifactsPresent 检查发布产物；任何一项存在都算 `artifactsPartial`。
// publishArtifactsPresent reports what publication left behind.
func publishArtifactsPresent(finalPath string) publishArtifacts {
	if _, err := os.Lstat(finalPath); err == nil {
		return artifactsPartial
	}
	if _, err := os.Lstat(finalPath + masterKeyTempSuffix); err == nil {
		return artifactsPartial
	}
	return artifactsNone
}
