package app

// 本文件是托管凭据的密码学边界（模块卡 R4.8 / T129）：
//   - AES-256-GCM 加密 / 解密，nonce 12 字节随密文一起落库；
//   - AAD 六段固定：`basalt/v1|owner_kind|owner_uid|credential_id|version_no|target_binding`；
//   - 目标绑定 = SHA-256(canonical(base_url | full_url))，规范化规则只在 core 一处；
//   - 主密钥文件的形态、位置与权限校验。
//
// 四条纪律：
//   - **不缓存主密钥**：本文件只提供"读一次、用一次"的函数；调用方（resolver）每次
//     重新读取文件，主密钥不常驻内存；
//   - **AAD 绑死六段**：任一字段变化都让解密失败——密文不能被搬到别的资源、版本或目标；
//   - **错误不回显材料**：只点名 key_id 与字段名，Key、地址与明文一律不进错误文案；
//   - **失败即不可用**：装载 / 解密失败一律包装 `core.ErrCredentialUnavailable`，调用方
//     映射 503 `CREDENTIAL_UNAVAILABLE`，**不**回落 env、不自动生成新密钥。
//
// This file is the cryptographic boundary of managed credentials: AES-256-GCM with a
// six-segment AAD, target binding hashed from the canonical address, and master-key file
// validation. The key is never cached, AAD changes always fail decryption, and errors
// never echo key material or addresses.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// credentialAADVersion 是 AAD 首段；换算法或分段语义时必须换版本前缀，
	// 让旧密文显式解密失败而不是被按新规则解释。
	// credentialAADVersion prefixes the AAD; changing the scheme must change it.
	credentialAADVersion = "basalt/v1"
	// masterKeyFileVersion 是主密钥文件 `version` 字段的唯一取值。
	// masterKeyFileVersion is the only accepted `version` value.
	masterKeyFileVersion = 1
	// masterKeyBytes 是 AES-256 的密钥长度。
	// masterKeyBytes is the AES-256 key length.
	masterKeyBytes = 32
	// credentialNonceBytes 是 GCM 的 nonce 长度（96 位，标准推荐值）。
	// credentialNonceBytes is the GCM nonce length.
	credentialNonceBytes = 12
	// masterKeyIDPrefix 是 `key_id` 的固定前缀（契约 R4.16 的 UID 前缀表）。
	// masterKeyIDPrefix is the fixed key-id prefix from the literal table.
	masterKeyIDPrefix = "k_"
	// masterKeyFileMaxBytes 是主密钥文件的读取上限：正常文件不到 100 字节，
	// 上限只用来拒绝"指错到别的大文件"的部署错误，不是格式的一部分。
	// masterKeyFileMaxBytes caps the read size so a mis-pointed path fails fast.
	masterKeyFileMaxBytes = 64 << 10
)

// MasterKey 是一次解析出的主密钥。它只活在调用栈上：调用方用完必须 `Zero`，
// **不得**放进长生命周期结构体、包级变量或缓存。
// MasterKey lives on the call stack only; callers zero it and never cache it.
type MasterKey struct {
	KeyID string
	Key   []byte
}

// Zero 清零密钥字节（best-effort：Go 的 GC 仍可能留下副本，这里保证本次引用不再可读）。
// Zero clears the key bytes held by this value.
func (k MasterKey) Zero() {
	for i := range k.Key {
		k.Key[i] = 0
	}
}

// masterKeyFile 是主密钥文件的 JSON 形态：`version` / `key_id` / `key`。
// masterKeyFile mirrors the documented JSON shape byte for byte.
type masterKeyFile struct {
	Version int    `json:"version"`
	KeyID   string `json:"key_id"`
	Key     string `json:"key"`
}

// CredentialAAD 是密文 AAD 的六段输入：版本前缀是常量，其余五段逐段参与。
// `OwnerUID` 是资源 UID（不是名称）；解密前调用方还必须核对它与当前指针一致。
// CredentialAAD carries the six AAD segments; the owner uid is a resource uid, and the
// caller separately checks it still matches the current pointer.
type CredentialAAD struct {
	OwnerKind     core.BindingKind
	OwnerUID      core.ResourceUID
	CredentialID  string
	VersionNo     int
	TargetBinding string
}

// String 拼出六段 AAD 原文；`|` 是唯一分隔符，各段自身不得含 `|`（见 Validate）。
// String renders the six-segment AAD; Validate refuses any segment containing the
// separator, so distinct inputs can never collide into the same bytes.
func (a CredentialAAD) String() string {
	return fmt.Sprintf("%s|%s|%s|%s|%d|%s",
		credentialAADVersion, a.OwnerKind, a.OwnerUID, a.CredentialID, a.VersionNo, a.TargetBinding)
}

// Validate 校验六段的形状：类别与 UID 走领域解析，凭据 ID 走 Crockford 校验，
// 版本号从 1 起，目标绑定是 64 位小写十六进制。
// Validate checks every segment shape before it can reach the AEAD.
func (a CredentialAAD) Validate() error {
	kind, err := core.ParseBindingKind(string(a.OwnerKind))
	if err != nil {
		return fmt.Errorf("凭据 AAD 非法: %w", err)
	}
	if kind != a.OwnerKind {
		return errors.New("凭据 AAD 非法: 绑定类别未规范化")
	}
	uid, err := core.ParseResourceUID(string(a.OwnerUID))
	if err != nil {
		return fmt.Errorf("凭据 AAD 非法: %w", err)
	}
	if uid != a.OwnerUID {
		return errors.New("凭据 AAD 非法: 资源 UID 未规范化")
	}
	if err := core.ValidateCredentialID(a.CredentialID); err != nil {
		return fmt.Errorf("凭据 AAD 非法: %w", err)
	}
	if a.VersionNo < 1 {
		return errors.New("凭据 AAD 非法: version_no 必须 >= 1")
	}
	if !isLowerHex64(a.TargetBinding) {
		return errors.New("凭据 AAD 非法: target_binding 必须是 64 位小写十六进制")
	}
	return nil
}

// CredentialTargetBinding 计算目标绑定哈希：模型连接取 `base_url`，通知渠道取
// 完整地址。完整地址按凭据处理，调用方**不得**把它写进日志或响应。
// CredentialTargetBinding hashes the canonical target address; model connections use
// base_url and channels use the full URL, which is itself credential material.
func CredentialTargetBinding(kind core.BindingKind, baseURL, fullURL string) (string, error) {
	parsed, err := core.ParseBindingKind(string(kind))
	if err != nil {
		return "", fmt.Errorf("计算凭据目标绑定: %w", err)
	}
	binding := core.TargetBinding{Kind: parsed, BaseURL: baseURL, FullURL: fullURL}
	if strings.TrimSpace(binding.Canonical()) == "" {
		return "", errors.New("计算凭据目标绑定: 目标地址不得为空")
	}
	return binding.Hash(), nil
}

// SealCredential 用主密钥加密明文并返回 nonce 与密文。每次调用都取新的随机 nonce：
// 同一 Key 录两次也得到不同密文，避免"密文相同即值相同"的旁路。
// SealCredential encrypts with a fresh random nonce on every call.
func SealCredential(key MasterKey, aad CredentialAAD, plaintext []byte) (nonce, ciphertext []byte, err error) {
	aead, err := newCredentialAEAD(key, aad)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, credentialNonceBytes)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("加密托管凭据: 生成 nonce: %w", err)
	}
	ciphertext = aead.Seal(nil, nonce, plaintext, []byte(aad.String()))
	return nonce, ciphertext, nil
}

// OpenCredential 用主密钥解密；AAD 任一段与加密时不同都会让 GCM 认证失败。
// OpenCredential decrypts; any AAD difference fails the GCM tag check.
func OpenCredential(key MasterKey, aad CredentialAAD, nonce, ciphertext []byte) ([]byte, error) {
	aead, err := newCredentialAEAD(key, aad)
	if err != nil {
		return nil, err
	}
	if len(nonce) != credentialNonceBytes {
		return nil, fmt.Errorf("%w: 托管凭据 nonce 长度为 %d，必须为 %d",
			core.ErrCredentialUnavailable, len(nonce), credentialNonceBytes)
	}
	if len(ciphertext) == 0 {
		return nil, fmt.Errorf("%w: 托管凭据密文为空（可能已被撤销清空）", core.ErrCredentialUnavailable)
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, []byte(aad.String()))
	if err != nil {
		// 不把 GCM 的原始错误拼进来：它足以区分类别，而调用方只需要"这份密文在当前
		// 主密钥下不可读"这一条结论。AAD / key_id 的具体差异由上层点名。
		return nil, fmt.Errorf("%w: 托管凭据无法用当前主密钥解密（key_id、AAD 或密文不匹配）",
			core.ErrCredentialUnavailable)
	}
	return plaintext, nil
}

// LoadMasterKey 读取并校验主密钥文件：路径必须在工作区外、不是符号链接 / junction、
// 没有硬链接、平台权限受限（Unix 0700 / 0600；Windows 文件与父目录的 DACL 只允许
// 当前用户、SYSTEM 与 Administrators），JSON 形态与 32 字节长度逐项吻合。每次调用都
// 重新读文件与权限——本函数**不**缓存。
// LoadMasterKey reads and validates the master-key file every time; nothing is cached.
func LoadMasterKey(path, workspaceRoot string) (MasterKey, error) {
	if strings.TrimSpace(path) == "" {
		return MasterKey{}, fmt.Errorf("%w: 未配置主密钥文件（secrets.master_key_file）",
			core.ErrCredentialUnavailable)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return MasterKey{}, fmt.Errorf("%w: 主密钥路径无法规范化为绝对路径", core.ErrCredentialUnavailable)
	}
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return MasterKey{}, fmt.Errorf("%w: 主密钥文件不存在或不可读", core.ErrCredentialUnavailable)
		}
		return MasterKey{}, fmt.Errorf("%w: 主密钥路径无法解析", core.ErrCredentialUnavailable)
	}
	// EvalSymlinks 与原文不一致 = 路径上经过了链接：符号链接文件本身、或祖先目录是
	// 符号链接 / junction。两者都拒绝——否则"工作区外"这一条可以被一条链接绕过。
	// A resolved path that differs from the given one traversed a link; that would
	// let a link inside the workspace point at a key outside and vice versa.
	if !sameFilePath(resolved, absPath) {
		return MasterKey{}, fmt.Errorf("%w: 主密钥路径不得经过符号链接 / junction / 重解析点",
			core.ErrCredentialUnavailable)
	}
	if err := requireOutsideWorkspace(resolved, workspaceRoot); err != nil {
		return MasterKey{}, err
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return MasterKey{}, fmt.Errorf("%w: 读取主密钥文件状态失败", core.ErrCredentialUnavailable)
	}
	if !info.Mode().IsRegular() {
		return MasterKey{}, fmt.Errorf("%w: 主密钥必须是普通文件", core.ErrCredentialUnavailable)
	}
	if count, known := hardLinkCount(resolved, info); known && count > 1 {
		return MasterKey{}, fmt.Errorf("%w: 主密钥文件存在 %d 个硬链接（同一份密钥材料挂在多条路径上）",
			core.ErrCredentialUnavailable, count)
	}
	if info.Size() > masterKeyFileMaxBytes {
		return MasterKey{}, fmt.Errorf("%w: 主密钥文件超过 %d 字节，路径可能指错",
			core.ErrCredentialUnavailable, masterKeyFileMaxBytes)
	}
	// 平台权限校验：Unix 按 0700 / 0600 位，Windows 按文件与父目录的 DACL（见各自分支）。
	// Platform permission check: mode bits on Unix, DACLs on Windows.
	if err := checkKeyPermissions(filepath.Dir(resolved), resolved); err != nil {
		return MasterKey{}, err
	}

	// #nosec G304 -- 路径来自部署者配置（secrets.master_key_file），不是请求输入；
	// 且上面已校验绝对路径、链接、工作区边界与权限。
	file, err := os.Open(resolved)
	if err != nil {
		return MasterKey{}, fmt.Errorf("%w: 打开主密钥文件失败", core.ErrCredentialUnavailable)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, masterKeyFileMaxBytes+1))
	if err != nil {
		return MasterKey{}, fmt.Errorf("%w: 读取主密钥文件失败", core.ErrCredentialUnavailable)
	}
	if len(data) > masterKeyFileMaxBytes {
		return MasterKey{}, fmt.Errorf("%w: 主密钥文件超过 %d 字节", core.ErrCredentialUnavailable, masterKeyFileMaxBytes)
	}
	return parseMasterKeyFile(data)
}

// parseMasterKeyFile 按固定 JSON 形态解析：未知字段、尾随内容、版本不符、key_id 前缀
// 不是 `k_`、密钥不是 32 字节——逐项拒绝。错误只点名字段，**不**回显任何内容。
// parseMasterKeyFile accepts exactly the documented JSON shape and refuses anything else.
func parseMasterKeyFile(data []byte) (MasterKey, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var file masterKeyFile
	if err := decoder.Decode(&file); err != nil {
		return MasterKey{}, fmt.Errorf("%w: 主密钥文件不是合法 JSON（字段 version / key_id / key）",
			core.ErrCredentialUnavailable)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return MasterKey{}, fmt.Errorf("%w: 主密钥文件含多余内容", core.ErrCredentialUnavailable)
	}
	if file.Version != masterKeyFileVersion {
		return MasterKey{}, fmt.Errorf("%w: 主密钥文件 version 必须为 %d",
			core.ErrCredentialUnavailable, masterKeyFileVersion)
	}
	if !validMasterKeyID(file.KeyID) {
		return MasterKey{}, fmt.Errorf("%w: 主密钥 key_id 必须以 %q 开头且只含字母数字与 _ -",
			core.ErrCredentialUnavailable, masterKeyIDPrefix)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(file.Key))
	if err != nil {
		return MasterKey{}, fmt.Errorf("%w: 主密钥 key 不是合法 base64", core.ErrCredentialUnavailable)
	}
	if len(raw) != masterKeyBytes {
		return MasterKey{}, fmt.Errorf("%w: 主密钥长度为 %d 字节，必须为 %d",
			core.ErrCredentialUnavailable, len(raw), masterKeyBytes)
	}
	return MasterKey{KeyID: file.KeyID, Key: raw}, nil
}

// masterKeyFingerprintLen 是指纹的十六进制长度（8 字节 = 16 个字符）：足够区分部署里
// 的几份密钥，又不至于让人把它当凭据使用。
// masterKeyFingerprintLen is the fingerprint length in hex characters.
const masterKeyFingerprintLen = 16

// GenerateMasterKey 生成一份新主密钥：32 字节取自 crypto/rand，`key_id` = `k_` + 26 位
// Crockford Base32（与 `ag_` / `rs_` / `cred_` 的载荷长度同一形态）。返回值不落盘，
// 调用方决定写到哪个路径，并在用完后 `Zero`。
// GenerateMasterKey mints a 32-byte key and a `k_`-prefixed identifier; nothing is written
// here, and the caller zeroes the value when done.
func GenerateMasterKey() (MasterKey, error) {
	key := make([]byte, masterKeyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return MasterKey{}, fmt.Errorf("生成主密钥: 读取随机数: %w", err)
	}
	keyID, err := newMasterKeyID()
	if err != nil {
		return MasterKey{}, err
	}
	return MasterKey{KeyID: keyID, Key: key}, nil
}

// newMasterKeyID 生成 `k_` + 26 位 Crockford Base32；256 % 32 == 0，逐字节取模无偏。
// newMasterKeyID emits the prefix plus 26 unbiased Crockford Base32 characters.
func newMasterKeyID() (string, error) {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	const payload = 26
	raw := make([]byte, payload)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("生成主密钥标识: 读取随机数: %w", err)
	}
	out := make([]byte, 0, len(masterKeyIDPrefix)+payload)
	out = append(out, masterKeyIDPrefix...)
	for _, b := range raw {
		out = append(out, alphabet[int(b)%len(alphabet)])
	}
	return string(out), nil
}

// MasterKeyFingerprint 返回主密钥指纹：SHA-256(32 字节密钥) 的前 16 位小写十六进制。
// 它只用于部署侧核对"服务读到的密钥是不是同一份"——**不得**当作凭据使用，也不能从它
// 还原密钥；`secrets status` / `secrets verify` 只回这个指纹，不回密钥本身。
// MasterKeyFingerprint is the first 16 hex characters of SHA-256 over the key bytes; it is
// an operator comparison aid, never a usable secret.
func MasterKeyFingerprint(key MasterKey) (string, error) {
	if len(key.Key) != masterKeyBytes {
		return "", fmt.Errorf("%w: 主密钥长度必须为 %d 字节", core.ErrCredentialUnavailable, masterKeyBytes)
	}
	sum := sha256.Sum256(key.Key)
	return hex.EncodeToString(sum[:])[:masterKeyFingerprintLen], nil
}

// WriteMasterKey 创建主密钥文件（**不**覆盖已有文件）：父目录不存在则按 0700 创建、
// 文件 0600、路径必须在工作区外且不经过链接，写完立即用 `LoadMasterKey` 自检。
//   - `O_EXCL` 是"已存在即拒绝"的唯一实现点——`secrets init` 的判据不能靠先 stat 再写，
//     那中间有窗口；
//   - Windows 的权限来自父目录继承的 DACL（`os.Chmod` 只切只读位），部署侧必须先用
//     `icacls` 把目录收成最小权限；自检按 DACL 判卷，目录过宽时 init 失败并删除刚建的
//     文件；Unix 分支显式 `Chmod` 以抵消 umask；
//   - 自检失败（权限不符 / 内容截断）删除刚建的文件——半个密钥文件比没有更危险。
//
// WriteMasterKey creates the key file exclusively, never overwrites, and self-checks the
// result with LoadMasterKey; a failed self-check removes the file it just made.
func WriteMasterKey(path, workspaceRoot string, key MasterKey) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: 未配置主密钥文件（secrets.master_key_file）", core.ErrCredentialUnavailable)
	}
	if len(key.Key) != masterKeyBytes {
		return fmt.Errorf("%w: 主密钥长度必须为 %d 字节", core.ErrCredentialUnavailable, masterKeyBytes)
	}
	if !validMasterKeyID(key.KeyID) {
		return fmt.Errorf("%w: 主密钥 key_id 必须以 %q 开头且只含字母数字与 _ -",
			core.ErrCredentialUnavailable, masterKeyIDPrefix)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: 主密钥路径无法规范化为绝对路径", core.ErrCredentialUnavailable)
	}
	dir := filepath.Dir(absPath)
	if err := requireNoLinkAncestors(dir); err != nil {
		return err
	}
	createdDir, err := ensureKeyDirectory(dir)
	if err != nil {
		return err
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil || !sameFilePath(resolvedDir, dir) {
		return fmt.Errorf("%w: 主密钥目录路径不得经过符号链接 / junction / 重解析点",
			core.ErrCredentialUnavailable)
	}
	finalPath := filepath.Join(resolvedDir, filepath.Base(absPath))
	if err := requireOutsideWorkspace(finalPath, workspaceRoot); err != nil {
		return err
	}
	if createdDir {
		// MkdirAll 受 umask 影响；显式收口到 0700 让"目录权限过宽"只在运维自建的目录上出现。
		// #nosec G302 -- 目标是**目录**：0700 已是目录的最小可用权限，G302 按文件 0600 判、不适用。
		if chmodErr := os.Chmod(resolvedDir, 0o700); chmodErr != nil {
			return fmt.Errorf("%w: 设置主密钥目录权限失败", core.ErrCredentialUnavailable)
		}
	}
	if err := writeNewMasterKeyFile(finalPath, key); err != nil {
		return err
	}
	check, err := LoadMasterKey(finalPath, workspaceRoot)
	if err != nil {
		// 自检失败：把刚建的文件撤掉，不把一个读不回来的密钥留在盘上。
		return errors.Join(err, removeKeyFile(finalPath))
	}
	check.Zero()
	if check.KeyID != key.KeyID {
		return errors.Join(
			fmt.Errorf("%w: 主密钥自检的 key_id 与写入值不一致", core.ErrCredentialUnavailable),
			removeKeyFile(finalPath))
	}
	return nil
}

// masterKeyFileExists 判定主密钥路径上是否已有东西（文件 / 目录 / 链接都算"已存在"）：
// `secrets init` 用它给出比 `O_EXCL` 更早、更可读的拒绝文案；真正的排他仍由 `O_EXCL`
// 保证，两者不互相替代。
// masterKeyFileExists is the early, readable refusal of secrets init; O_EXCL stays the
// real exclusion point.
func masterKeyFileExists(path string) (bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("%w: 主密钥路径无法规范化为绝对路径", core.ErrCredentialUnavailable)
	}
	if _, statErr := os.Lstat(absPath); statErr == nil {
		return true, nil
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return false, fmt.Errorf("%w: 读取主密钥文件状态失败", core.ErrCredentialUnavailable)
	}
	return false, nil
}

// writeNewMasterKeyFile 以 O_EXCL 落盘固定 JSON 形态，写失败即删除半个文件。
// writeNewMasterKeyFile writes the fixed JSON shape with O_EXCL and removes partial output.
func writeNewMasterKeyFile(path string, key MasterKey) error {
	document, err := json.Marshal(masterKeyFile{
		Version: masterKeyFileVersion,
		KeyID:   key.KeyID,
		Key:     base64.StdEncoding.EncodeToString(key.Key),
	})
	if err != nil {
		return fmt.Errorf("%w: 序列化主密钥文件失败", core.ErrCredentialUnavailable)
	}
	document = append(document, '\n')
	// #nosec G304 -- 路径来自部署者配置（secrets.master_key_file），不是请求输入；调用方已
	// 规范化、拒绝符号链接 / 硬链接并校验不在工作区内，O_EXCL 保证不覆盖既有文件。
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errors.New("主密钥文件已存在，拒绝覆盖（不提供自动轮换；确需更换请先离线备份）")
		}
		return fmt.Errorf("%w: 创建主密钥文件失败", core.ErrCredentialUnavailable)
	}
	// 显式收口权限以抵消 umask（Windows 上只切只读位，DACL 由部署继承）。
	if chmodErr := file.Chmod(0o600); chmodErr != nil {
		return errors.Join(fmt.Errorf("%w: 设置主密钥文件权限失败", core.ErrCredentialUnavailable),
			file.Close(), removeKeyFile(path))
	}
	if _, err := file.Write(document); err != nil {
		return errors.Join(fmt.Errorf("%w: 写入主密钥文件失败", core.ErrCredentialUnavailable),
			file.Close(), removeKeyFile(path))
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("%w: 同步主密钥文件失败", core.ErrCredentialUnavailable),
			file.Close(), removeKeyFile(path))
	}
	if err := file.Close(); err != nil {
		return errors.Join(fmt.Errorf("%w: 关闭主密钥文件失败", core.ErrCredentialUnavailable),
			removeKeyFile(path))
	}
	return nil
}

// requireNoLinkAncestors 从目标目录向上找第一个**已存在**的祖先：它经 EvalSymlinks 之后
// 必须与原文一致，否则这条路径上就有链接（符号链接 / junction），"工作区外"与"权限目录"
// 两条判定都可以被它绕过。不存在的层级留给 `MkdirAll` 在已验证的祖先之下创建。
// requireNoLinkAncestors refuses any existing ancestor that resolves to a different path.
func requireNoLinkAncestors(dir string) error {
	current := filepath.Clean(dir)
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			if !sameFilePath(resolved, current) {
				return fmt.Errorf("%w: 主密钥目录路径不得经过符号链接 / junction / 重解析点",
					core.ErrCredentialUnavailable)
			}
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: 主密钥目录路径无法解析", core.ErrCredentialUnavailable)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil // 已到卷根：不会再有可跟随的链接
		}
		current = parent
	}
}

// ensureKeyDirectory 确保父目录存在：返回"本进程是否创建了它"（只有自建的目录才允许
// 改权限——运维预先建好的目录按其自身策略判卷，权限过宽由 LoadMasterKey 明确拒绝）。
// ensureKeyDirectory reports whether this call created the directory.
func ensureKeyDirectory(dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("%w: 主密钥的父路径不是目录", core.ErrCredentialUnavailable)
		}
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("%w: 读取主密钥目录状态失败", core.ErrCredentialUnavailable)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("%w: 创建主密钥目录失败", core.ErrCredentialUnavailable)
	}
	return true, nil
}

// removeKeyFile 撤掉自检失败留下的文件；删除失败并入错误链（失败路径不得静默）。
// removeKeyFile removes a key file whose self-check failed.
func removeKeyFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("清理主密钥文件: %w", err)
	}
	return nil
}

// newCredentialAEAD 校验 AAD 与密钥后构造 AEAD；密钥长度错误是部署错误（503）。
// newCredentialAEAD builds the AEAD after validating the AAD and key length.
func newCredentialAEAD(key MasterKey, aad CredentialAAD) (cipher.AEAD, error) {
	if err := aad.Validate(); err != nil {
		return nil, err
	}
	if len(key.Key) != masterKeyBytes {
		return nil, fmt.Errorf("%w: 主密钥长度必须为 %d 字节", core.ErrCredentialUnavailable, masterKeyBytes)
	}
	block, err := aes.NewCipher(key.Key)
	if err != nil {
		return nil, fmt.Errorf("%w: 初始化 AES-256 失败", core.ErrCredentialUnavailable)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: 初始化 GCM 失败", core.ErrCredentialUnavailable)
	}
	return aead, nil
}

// requireOutsideWorkspace 判定主密钥路径落在工作区之外。工作区路径解析失败（尚未
// 初始化）时按"无法判定"处理——调用方的启动流程先建工作区再校验主密钥。
// requireOutsideWorkspace refuses a key inside the workspace; an uninitialized workspace
// is treated as "cannot decide here", because the caller validates after setup.
func requireOutsideWorkspace(keyPath, workspaceRoot string) error {
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return nil
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	rel, err := filepath.Rel(root, keyPath)
	if err != nil {
		// 不同卷（Windows 的跨盘路径）：必然在工作区之外。
		return nil
	}
	rel = filepath.Clean(rel)
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return fmt.Errorf("%w: 主密钥文件不得放在工作区内", core.ErrCredentialUnavailable)
	}
	return nil
}

// sameFilePath 比较两条已清理的路径；Windows 上大小写不敏感。
// sameFilePath compares cleaned paths, case-insensitively on Windows.
func sameFilePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// validMasterKeyID 校验 `k_` 前缀与受限字符集。长度不设硬上限以外的形态约束：
// `key_id` 由部署者给出（`basalt secrets init` 生成 26 位 Crockford 载荷），
// 这里只拒绝明显非法（空、空白、路径字符）的值。
// validMasterKeyID checks the prefix and a restricted alphabet without pinning length.
func validMasterKeyID(raw string) bool {
	if !strings.HasPrefix(raw, masterKeyIDPrefix) || len(raw) <= len(masterKeyIDPrefix) || len(raw) > 128 {
		return false
	}
	for i := len(masterKeyIDPrefix); i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// isLowerHex64 判定 64 位小写十六进制（目标绑定哈希的契约形态）。
// isLowerHex64 checks the 64-character lowercase hex shape of a target binding.
func isLowerHex64(raw string) bool {
	if len(raw) != 64 {
		return false
	}
	_, err := hex.DecodeString(raw)
	if err != nil {
		return false
	}
	return raw == strings.ToLower(raw)
}
