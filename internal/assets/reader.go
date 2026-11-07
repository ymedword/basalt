package assets

// 本文件是**执行期读取面**：从准入验证过的内容寻址目录里读取配置文件，逐文件复核准入
// 清单记录的 sha256 与大小。它只认清单覆盖的路径——清单之外的相对路径一律拒绝，
// 因此目录里即便混入别的文件，执行面也读不到（FR-24-42：执行消费 manifest 固定目录）。
//
// 读取顺序固定为"读字节 → 复算哈希 → 再交给调用方"：先校验路径存在再另行读取会让
// 校验与使用之间出现可替换窗口，宁可只读一次、只信这一次读到的字节。
//
// The execution-side read face: it serves only the paths the verified manifest covers
// and re-checks each file's digest on the very bytes it hands out.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// BundleReader 绑定一个已通过准入校验的 bundle 目录与其清单里的配置文件列表。它不持有
// 打开的文件句柄：每次读取都重新经根限定打开，调用方因此不需要（也无法忘记）关闭。
// BundleReader binds a verified bundle directory to the config files its manifest covers.
// It holds no open handles, so there is nothing for a caller to leak.
type BundleReader struct {
	dir   string
	files map[string]core.FileHash
}

// OpenBundleReader 绑定 `pin.Dir`（**必须**是 `assets.Resolve` 的返回值，FR-24-44）
// 与清单列出的配置文件。空目录 / 空清单 / 非法相对路径 / 重复项一律失败关闭——
// 这些状态说明准入没有固定资产包，继续读会让执行内容失去可追溯来源。
// OpenBundleReader binds the resolved directory to the manifest's config files and
// fails closed on any state that means "admission did not actually pin a bundle".
func OpenBundleReader(pin core.AssetBundlePin) (*BundleReader, error) {
	if strings.TrimSpace(pin.Dir) == "" {
		return nil, fmt.Errorf("%w: 资产包目录为空（准入未固定资产包）", core.ErrAssetUnavailable)
	}
	if len(pin.ConfigFiles) == 0 {
		return nil, fmt.Errorf("%w: 资产包 %s 的清单没有配置文件", core.ErrAssetVersionMismatch, pin.Hash)
	}
	files := make(map[string]core.FileHash, len(pin.ConfigFiles))
	for _, file := range pin.ConfigFiles {
		if err := safeRelPath(file.Path); err != nil {
			return nil, fmt.Errorf("%w: 清单里的配置文件路径 %q 非法: %v",
				core.ErrAssetVersionMismatch, file.Path, err)
		}
		if _, dup := files[file.Path]; dup {
			return nil, fmt.Errorf("%w: 清单里的配置文件 %s 重复", core.ErrAssetVersionMismatch, file.Path)
		}
		files[file.Path] = file
	}
	return &BundleReader{dir: pin.Dir, files: files}, nil
}

// ReadConfig 读取一个配置文件（路径相对工作区根）并复核准入清单的哈希：清单未覆盖
// 该路径 → 拒绝；字节与清单的 sha256 / 大小不符（截断、被改写）→ 拒绝。错误都是
// `core.ErrAssetVersionMismatch`，由调用方映射为 409，不静默回落工作副本。
// ReadConfig reads one manifest-covered file and re-checks its digest before returning.
func (r *BundleReader) ReadConfig(rel string) ([]byte, error) {
	if r == nil {
		return nil, errors.New("资产包读取面为空")
	}
	expected, ok := r.files[rel]
	if !ok {
		return nil, fmt.Errorf("%w: 资产包 %s 的清单未覆盖配置文件 %s",
			core.ErrAssetVersionMismatch, r.dir, rel)
	}
	bundleRel := ConfigBundleRel(rel)
	source, err := openRegular(r.dir, bundleRel)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取资产包文件 %s: %v", core.ErrAssetVersionMismatch, bundleRel, err)
	}
	defer source.Close()

	// 只读到"清单大小 + 1"字节：文件被换大时不把整个文件读进内存，读到多出来的
	// 一个字节就足以判定不符。
	limit := expected.Size + 1
	data, err := io.ReadAll(io.LimitReader(source, limit))
	if err != nil {
		return nil, fmt.Errorf("%w: 读取资产包文件 %s: %v", core.ErrAssetVersionMismatch, bundleRel, err)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if int64(len(data)) != expected.Size || digest != expected.SHA256 {
		return nil, fmt.Errorf("%w: 资产包文件 %s 与清单不符（清单 %s/%d，实际 %s/%d）",
			core.ErrAssetVersionMismatch, bundleRel, expected.SHA256, expected.Size, digest, len(data))
	}
	return data, nil
}
