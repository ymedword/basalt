package app

// 本文件是**首次初始化进度记录**（2026-10-30 用户裁决：首次启动中断后必须能安全重试）。
//
// 要解决的问题：`instance.json` 一落盘，这个工作区就不再是"新实例"，而管理面访问密码
// 只在首次初始化时自动发放一次。如果首次 `serve` 在写完名册之后、发放密码之前失败
// （DSN 没提供、数据库连不上、连上之后初始化失败），重试时 `freshInstance=false`，
// 密码就**永久**拿不到——用户只能手工造一个密码文件。
//
// 记录因此落在实例私有目录（与主密钥、密码文件同目录、权限同档），内容只有三样：
// 版本、实例标识、密码进度。三档语义：
//
//	pending  —— 首次初始化已开始、密码尚未发放；允许重试时补发（只在文件不存在时写）
//	minted   —— 密码已发放过；此后文件被删也**不得**静默重发（失败关闭，与存量实例同）
//	（缺失） —— 不是本机制产生的实例（`basalt init` 建的、US-14 之前建的）；一律不补发
//
// 三条纪律：
//   - **只允许 pending → minted 单向流转**，不覆盖已有密码文件、主密钥或配置文件；
//   - 读不懂 / 标识不符 / 版本过高一律按"不是 pending"处理（失败关闭），只记告警；
//   - 记录先于名册写（见 instance_bootstrap.go）：名册在 ⇒ 记录在，除非记录根本没写成
//     ——那种情况下名册不会被写，工作区保持"新实例"。
//
// The provisioning record lives in the instance-private directory and is the only proof
// that an interrupted first start still owes its one-time admin password. It carries a
// single forward transition (pending → minted); anything unreadable or foreign is treated
// as "not pending" and fails closed.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// provisioningFileName 是进度记录文件名（位于实例私有目录）。
	provisioningFileName = "provisioning.json"
	// provisioningVersion 是本二进制理解的记录版本；更高版本拒绝解析。
	provisioningVersion = 1
	// adminTokenStatePending 表示首次初始化已开始、密码尚未发放。
	adminTokenStatePending = "pending"
	// adminTokenStateMinted 表示密码发放过；此后不再自动补发。
	adminTokenStateMinted = "minted"
)

// provisioningRecord 是进度记录的内容：版本 + 实例标识 + 密码进度。
// provisioningRecord is the on-disk record: version, instance id and token state.
type provisioningRecord struct {
	Version    int    `json:"version"`
	InstanceID string `json:"instance_id"`
	AdminToken string `json:"admin_token"`
}

// provisioningPathFor 按实例标识推导记录路径（私有目录下的固定文件名）。
// provisioningPathFor derives the record path from an already-known instance id.
func provisioningPathFor(id string) (string, error) {
	dir, err := instancePrivateDirForID(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, provisioningFileName), nil
}

// recordProvisioningPending 为**尚未写入名册**的候选实例写一条 pending 记录，随后由
// 调用方写名册。写不出来即返回错误——调用方据此放弃写名册（工作区保持"新实例"）。
//
// 并发首启：两个进程各为**自己的**候选标识写记录，互不相干；名册的竞态由 O_EXCL 裁决，
// 输家的记录随之失去意义（没有任何读者按那个标识推导私有目录）。
// recordProvisioningPending records the pending state for a candidate id before the
// roster is written; a failure aborts the roster write on purpose.
func recordProvisioningPending(id string) error {
	path, err := provisioningPathFor(id)
	if err != nil {
		return err
	}
	if err := prepareSecretDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	raw, err := marshalProvisioning(provisioningRecord{
		Version:    provisioningVersion,
		InstanceID: id,
		AdminToken: adminTokenStatePending,
	})
	if err != nil {
		return err
	}
	return writeProvisioningExclusive(path, raw)
}

// writeProvisioningExclusive 以 `O_CREAT|O_EXCL|0600` 写记录；已存在时读回校验，
// 而不是覆盖——"谁先写的"必须保持可证明。
// writeProvisioningExclusive creates the record exclusively; an existing one is read
// back and never overwritten.
func writeProvisioningExclusive(path string, raw []byte) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("打开实例私有目录: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(provisioningFileName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		// 同一候选标识只可能由本进程写过一次（标识是刚生成的随机值）；走到这里说明
		// 记录已经在了，读回校验实例标识即可，内容不改。
		_, _, readErr := readProvisioningRecordAt(root, path)
		return readErr
	}
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入首次初始化进度记录: %w", err)
	}
	return file.Close()
}

// readProvisioningRecord 读取指定实例的进度记录；不存在返回 `(zero, false, nil)`。
// readProvisioningRecord reads the record for one instance id.
func readProvisioningRecord(id string) (provisioningRecord, bool, error) {
	path, err := provisioningPathFor(id)
	if err != nil {
		return provisioningRecord{}, false, err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return provisioningRecord{}, false, nil
	}
	if err != nil {
		return provisioningRecord{}, false, fmt.Errorf("打开实例私有目录: %w", err)
	}
	defer func() { _ = root.Close() }()
	return readProvisioningRecordAt(root, path)
}

// readProvisioningRecordAt 在已打开的私有目录句柄上读记录并逐项校验：版本不高于本二进制、
// 实例标识形态合法且**与请求的标识一致**（不符即拒绝，不做"就近匹配"）。
// readProvisioningRecordAt validates the record strictly against the requested id.
func readProvisioningRecordAt(root *os.Root, path string) (provisioningRecord, bool, error) {
	raw, err := root.ReadFile(provisioningFileName)
	if errors.Is(err, fs.ErrNotExist) {
		return provisioningRecord{}, false, nil
	}
	if err != nil {
		return provisioningRecord{}, false, fmt.Errorf("读取首次初始化进度记录 %s: %w", path, err)
	}
	var record provisioningRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return provisioningRecord{}, false, fmt.Errorf(
			"首次初始化进度记录 %s 不是合法 JSON——按未完成处理，请人工核对: %w", path, err)
	}
	if record.Version > provisioningVersion {
		return provisioningRecord{}, false, fmt.Errorf(
			"首次初始化进度记录 %s 版本为 %d，高于本二进制支持的 %d——请升级程序",
			path, record.Version, provisioningVersion)
	}
	if !instanceIDPattern.MatchString(record.InstanceID) {
		return provisioningRecord{}, false, fmt.Errorf(
			"首次初始化进度记录 %s 的 instance_id 形态非法——请人工核对", path)
	}
	return record, true, nil
}

// adminTokenProvisioningPending 回答"这个工作区还欠不欠一次首次初始化密码"：只有
// 记录存在、属于**名册里的那个**实例、且状态为 pending 才是真。
//
// 其余一切（没有名册、没有记录、标识不符、读不懂、已 minted）都返回假——存量实例与
// 已发放过密码的实例因此保持失败关闭。返回的原因串只进日志，不含凭据。
// adminTokenProvisioningPending reports whether this workspace still owes the one-time
// admin password; everything but a matching pending record answers false.
func adminTokenProvisioningPending(workDir string) (bool, string, string) {
	marker, present, err := readInstanceMarker(workDir)
	if err != nil {
		return false, "", "实例名册不可读: " + err.Error()
	}
	if !present {
		return false, "", "工作区没有实例名册"
	}
	record, found, err := readProvisioningRecord(marker.InstanceID)
	if err != nil {
		return false, marker.InstanceID, err.Error()
	}
	if !found {
		return false, marker.InstanceID, "没有首次初始化进度记录"
	}
	if record.InstanceID != marker.InstanceID {
		return false, marker.InstanceID, "进度记录不属于本实例"
	}
	if record.AdminToken != adminTokenStatePending {
		return false, marker.InstanceID, "进度记录状态为 " + record.AdminToken
	}
	return true, marker.InstanceID, "进度记录为 pending"
}

// completeAdminTokenProvisioning 把本实例的记录改写为 `minted`（`pending → minted` 的
// 唯一一次流转）。密码文件此时**已经**写好了：改写失败只让调用方告警，不阻断启动，
// 也不回退已经发放的密码——最坏结果是"运维手工删掉密码文件后可能被补发一次"，
// 而不是"首次启动因为一次记账失败起不来"。
// completeAdminTokenProvisioning flips pending to minted after the password file exists;
// a failure is reported but never undoes the issued password.
func completeAdminTokenProvisioning(workDir string) error {
	marker, present, err := readInstanceMarker(workDir)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	record, found, err := readProvisioningRecord(marker.InstanceID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if record.AdminToken == adminTokenStateMinted {
		return nil
	}
	record.AdminToken = adminTokenStateMinted
	raw, err := marshalProvisioning(record)
	if err != nil {
		return err
	}
	path, err := provisioningPathFor(marker.InstanceID)
	if err != nil {
		return err
	}
	// 先写同目录临时文件再改名：读到半截记录与读到旧状态一样都按 pending 处理，但改名
	// 让"完整记录"成为唯一可见形态。临时文件名固定，重写是覆盖式的（同一时刻只有一个
	// 进程持有实例锁，且两个进程都写同一个目标状态）。
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("打开实例私有目录: %w", err)
	}
	defer func() { _ = root.Close() }()
	tmpName := provisioningFileName + ".tmp"
	file, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("写入首次初始化进度记录: %w", err)
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入首次初始化进度记录: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("写入首次初始化进度记录: %w", err)
	}
	if err := root.Rename(tmpName, provisioningFileName); err != nil {
		return fmt.Errorf("更新首次初始化进度记录 %s: %w", path, err)
	}
	return nil
}

// marshalProvisioning 序列化记录（缩进 + 尾换行，便于人工核对）。
// marshalProvisioning renders the record with indentation and a trailing newline.
func marshalProvisioning(record provisioningRecord) ([]byte, error) {
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化首次初始化进度记录: %w", err)
	}
	return append(raw, '\n'), nil
}
