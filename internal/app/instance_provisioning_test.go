package app

// 本文件判**首次初始化进度记录**（2026-10-30 用户裁决）：首次启动中断后必须能安全重试。
//
// 覆盖的**逆改点**（每一条都对应一种真实事故）：
//   - 重试时只认 `freshInstance` → 中断后永久拿不到管理面访问密码（用户只能手工造文件）；
//   - 不区分"未完成初始化的新实例"与"已有凭据的实例" → 存量实例被静默补发密码；
//   - 记录为 minted 仍补发 → 运维手工删掉密码文件后，管理面在无人知晓的情况下换了密码；
//   - 补发覆盖已有的密码文件 / 主密钥 / 配置 → 部署事实被改写；
//   - 进度记录写不出来仍写名册 → 留下一个永远拿不到密码的半成品工作区。
//
// These tests pin the provisioning record: a retry may finish the one-time admin password
// only while the record says pending, and nothing existing is ever overwritten.

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
)

// provisioningFixture 是「一个空工作区 + 一个独立的用户数据根」：实例私有目录按名册
// 推导到测试临时目录，绝不触碰真实 `%LOCALAPPDATA%` / `~/.local/share`。
// provisioningFixture pins the per-user data root inside the test.
type provisioningFixture struct {
	workDir  string
	dataRoot string
}

// newProvisioningFixture 建立夹具并把三个平台的数据根变量同时指向临时目录。
// newProvisioningFixture points every platform-specific data-root variable at temp.
func newProvisioningFixture(t *testing.T) *provisioningFixture {
	t.Helper()
	base := t.TempDir()
	workDir := filepath.Join(base, "workspace")
	require.NoError(t, os.MkdirAll(workDir, 0o750))
	dataRoot := filepath.Join(base, "userdata")
	t.Setenv("LOCALAPPDATA", dataRoot)
	t.Setenv("XDG_DATA_HOME", dataRoot)
	t.Setenv("HOME", dataRoot)
	return &provisioningFixture{workDir: workDir, dataRoot: dataRoot}
}

// bootstrap 走一次真实的新实例预检（配置 + 进度记录 + 名册），返回报告。
// bootstrap runs the real new-instance bootstrap once.
func (f *provisioningFixture) bootstrap(t *testing.T) instanceBootstrapReport {
	t.Helper()
	report, err := ensureNewInstanceConfig(f.workDir, "")
	require.NoError(t, err)
	require.True(t, report.Fresh)
	require.True(t, report.ConfigCreated)
	require.True(t, report.ProvisioningRecorded)
	require.True(t, report.MarkerCreated)
	return report
}

// record 读取当前名册对应实例的进度记录。
// record reads the provisioning record of the roster's instance.
func (f *provisioningFixture) record(t *testing.T) (provisioningRecord, bool) {
	t.Helper()
	marker, present, err := readInstanceMarker(f.workDir)
	require.NoError(t, err)
	require.True(t, present, "夹具应当已经有名册")
	rec, found, err := readProvisioningRecord(marker.InstanceID)
	require.NoError(t, err)
	return rec, found
}

// tokenFile 返回按名册推导的密码文件路径。
// tokenFile returns the derived admin-token path.
func (f *provisioningFixture) tokenFile(t *testing.T) string {
	t.Helper()
	path, err := defaultAdminTokenFile(f.workDir)
	require.NoError(t, err)
	return path
}

// tokenConfig 是「token 模式 + 省略 token」的配置，即三来源解析的入口形态。
// tokenConfig is the token mode with the token omitted.
func tokenConfig() *cli.Config {
	return &cli.Config{HTTP: cli.HTTPConfig{
		AdminAuth: cli.AdminAuthConfig{Mode: adminAuthModeToken},
	}}
}

// TestProvisioning_NewInstanceRecordsPendingBeforeRoster 判首发三件套的次序与内容：
// 配置、进度记录（pending、标识与名册一致）、名册都落盘，且报告逐项为真。
func TestProvisioning_NewInstanceRecordsPendingBeforeRoster(t *testing.T) {
	f := newProvisioningFixture(t)
	report := f.bootstrap(t)

	marker, present, err := readInstanceMarker(f.workDir)
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, report.InstanceID, marker.InstanceID)

	rec, found := f.record(t)
	require.True(t, found, "名册在盘，进度记录必须同时在盘")
	require.Equal(t, provisioningVersion, rec.Version)
	require.Equal(t, marker.InstanceID, rec.InstanceID)
	require.Equal(t, adminTokenStatePending, rec.AdminToken)

	// 记录落在实例私有目录（与主密钥同目录），不在工作区里。
	path, err := provisioningPathFor(marker.InstanceID)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(f.dataRoot, instanceDataDirName, instancesDirName, marker.InstanceID),
		filepath.Dir(path))
	require.FileExists(t, path)

	// 首次启动的进程仍然是"新实例"，报告里 Fresh 为真——调用方据此判断要不要按新实例
	// 口径初始化加密身份。
	require.True(t, report.Fresh)
}

// TestProvisioning_InterruptedFirstStartResumesOnRetry 判主路径：首次启动在发放密码
// 之前中断（名册已写），重试时 `freshInstance=false`，但记录仍是 pending——这一次必须
// 把密码发出来，并把记录流转为 minted；再下一次启动读文件，不再发新密码。
func TestProvisioning_InterruptedFirstStartResumesOnRetry(t *testing.T) {
	f := newProvisioningFixture(t)
	report := f.bootstrap(t)
	require.NoFileExists(t, f.tokenFile(t), "首次启动中断：密码文件尚未写出")

	// 重试：本次启动不再是新实例。
	retry := &App{workDir: f.workDir, freshInstance: false}
	first, err := retry.resolveAdminToken(tokenConfig())
	require.NoError(t, err, "首次初始化未完成的重试必须能补发密码")
	require.Equal(t, adminTokenSourceGenerated, first.Source)
	require.NotEmpty(t, first.Token)

	onDisk, err := readAdminTokenFile(f.tokenFile(t))
	require.NoError(t, err)
	require.Equal(t, first.Token, onDisk)

	rec, found := f.record(t)
	require.True(t, found)
	require.Equal(t, adminTokenStateMinted, rec.AdminToken, "发放之后记录必须流转为 minted")
	require.Equal(t, report.InstanceID, rec.InstanceID)

	// 第三次启动：读文件，不重新生成。
	again, err := (&App{workDir: f.workDir}).resolveAdminToken(tokenConfig())
	require.NoError(t, err)
	require.Equal(t, adminTokenSourceFile, again.Source)
	require.Equal(t, first.Token, again.Token)
}

// TestProvisioning_PendingNeverOverwritesExistingPassword 判"不发第二把钥匙"：记录是
// pending、密码文件却已经存在（上次发放后、改写记录之前中断）时，盘上的密码是唯一
// 真相源；本次只把记录补记为 minted，不生成、不覆盖。
func TestProvisioning_PendingNeverOverwritesExistingPassword(t *testing.T) {
	f := newProvisioningFixture(t)
	f.bootstrap(t)

	const existing = "hand-written-password"
	require.NoError(t, writeAdminTokenFile(f.tokenFile(t), existing)) // O_EXCL：确认此刻没有文件

	resolved, err := (&App{workDir: f.workDir}).resolveAdminToken(tokenConfig())
	require.NoError(t, err)
	require.Equal(t, adminTokenSourceFile, resolved.Source)
	require.Equal(t, existing, resolved.Token)

	rec, _ := f.record(t)
	require.Equal(t, adminTokenStateMinted, rec.AdminToken)
	onDisk, err := readAdminTokenFile(f.tokenFile(t))
	require.NoError(t, err)
	require.Equal(t, existing, onDisk, "已有密码文件不得被改写")
}

// TestProvisioning_MintedRecordNeverRemints 判 minted 之后失败关闭：运维删掉密码文件
// 不会触发静默补发——管理面密码的更换必须是显式动作。
func TestProvisioning_MintedRecordNeverRemints(t *testing.T) {
	f := newProvisioningFixture(t)
	f.bootstrap(t)
	require.NoError(t, writeAdminTokenFile(f.tokenFile(t), "issued-password"))
	_, err := (&App{workDir: f.workDir}).resolveAdminToken(tokenConfig())
	require.NoError(t, err)
	require.NoError(t, os.Remove(f.tokenFile(t)))

	_, err = (&App{workDir: f.workDir}).resolveAdminToken(tokenConfig())
	require.Error(t, err, "minted 之后密码文件被删，不得静默补发")
	require.Contains(t, err.Error(), "http.admin_auth.token")
	require.NoFileExists(t, f.tokenFile(t), "拒绝时不得写出新密码")
}

// TestProvisioning_ExistingInstanceWithoutRecordRefuses 判存量实例（`basalt init` 建的、
// US-14 之前建的）：没有进度记录就没有"首次初始化未完成"的证据，一律不补发。
func TestProvisioning_ExistingInstanceWithoutRecordRefuses(t *testing.T) {
	f := newProvisioningFixture(t)
	f.bootstrap(t)
	// 手工删掉记录，模拟不受本机制管理的存量实例。
	marker, present, err := readInstanceMarker(f.workDir)
	require.NoError(t, err)
	require.True(t, present)
	path, err := provisioningPathFor(marker.InstanceID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))

	_, err = (&App{workDir: f.workDir}).resolveAdminToken(tokenConfig())
	require.Error(t, err)
	require.Contains(t, err.Error(), "http.admin_auth.token")
	require.NoFileExists(t, f.tokenFile(t))
}

// TestProvisioning_ForeignOrBrokenRecordRefuses 判"读不懂就不算 pending"：记录属于别的
// 实例、内容不是合法 JSON、状态不是 pending 时，一律按已有凭据的实例失败关闭。
func TestProvisioning_ForeignOrBrokenRecordRefuses(t *testing.T) {
	cases := []struct {
		name  string
		write func(t *testing.T, path, ownID string)
	}{
		{
			name: "标识不符",
			write: func(t *testing.T, path, _ string) {
				other, err := newInstanceID()
				require.NoError(t, err)
				raw, err := marshalProvisioning(provisioningRecord{
					Version: provisioningVersion, InstanceID: other, AdminToken: adminTokenStatePending,
				})
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, raw, 0o600))
			},
		},
		{
			name: "不是合法 JSON",
			write: func(t *testing.T, path, _ string) {
				require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
			},
		},
		{
			name: "版本高于本二进制",
			write: func(t *testing.T, path, ownID string) {
				raw, err := marshalProvisioning(provisioningRecord{
					Version: provisioningVersion + 1, InstanceID: ownID, AdminToken: adminTokenStatePending,
				})
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, raw, 0o600))
			},
		},
		{
			name: "未知状态",
			write: func(t *testing.T, path, ownID string) {
				raw, err := marshalProvisioning(provisioningRecord{
					Version: provisioningVersion, InstanceID: ownID, AdminToken: "half-done",
				})
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, raw, 0o600))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProvisioningFixture(t)
			f.bootstrap(t)
			marker, present, err := readInstanceMarker(f.workDir)
			require.NoError(t, err)
			require.True(t, present)
			path, err := provisioningPathFor(marker.InstanceID)
			require.NoError(t, err)
			tc.write(t, path, marker.InstanceID)

			_, resolveErr := (&App{workDir: f.workDir}).resolveAdminToken(tokenConfig())
			require.Error(t, resolveErr, "记录不是 pending 时必须失败关闭")
			require.Contains(t, resolveErr.Error(), "http.admin_auth.token")
			require.NoFileExists(t, f.tokenFile(t), "失败关闭时不得写出密码")
		})
	}
}

// TestProvisioning_RosterFailureLeavesRecordForRetry 判"名册写不出来就不算实例"：
// 名册目录被占成普通文件时预检失败，下一次（修好后）仍然是新实例并重新走完整流程。
func TestProvisioning_RosterFailureLeavesRecordForRetry(t *testing.T) {
	f := newProvisioningFixture(t)
	// 把 `.basalt` 占成一个普通文件：MkdirAll 会失败，预检在写任何东西之前返回错误。
	require.NoError(t, os.WriteFile(filepath.Join(f.workDir, workspaceDirName), []byte("occupied"), 0o600))
	_, err := ensureNewInstanceConfig(f.workDir, "")
	require.Error(t, err, "工作区目录建不出来时必须失败，而不是写一半")

	// 修好之后重试：仍然按新实例走完整流程。
	require.NoError(t, os.Remove(filepath.Join(f.workDir, workspaceDirName)))
	report := f.bootstrap(t)
	require.True(t, report.MarkerCreated)
}

// TestProvisioning_ConfigWinsOverRosterOrder 判次序的负向：名册已存在（别人赢了）时，
// 本次不再写配置——配置生成只属于"库与名册都不在"的那一刻。
func TestProvisioning_ConfigWinsOverRosterOrder(t *testing.T) {
	f := newProvisioningFixture(t)
	f.bootstrap(t)
	require.NoError(t, os.Remove(filepath.Join(f.workDir, configFileName)))

	report, err := ensureNewInstanceConfig(f.workDir, "")
	require.NoError(t, err)
	require.False(t, report.Fresh, "名册在盘就不是新实例")
	require.False(t, report.ConfigCreated)
	require.False(t, report.ProvisioningRecorded)
	require.NoFileExists(t, filepath.Join(f.workDir, configFileName),
		"名册在盘的存量实例不得被补生成配置")
}

// TestProvisioning_RecordFileIsPrivate 判记录与密码文件同档：0600（Unix）——它虽不含
// 凭据，但与主密钥同目录，权限口径不分家。
func TestProvisioning_RecordFileIsPrivate(t *testing.T) {
	f := newProvisioningFixture(t)
	f.bootstrap(t)
	marker, present, err := readInstanceMarker(f.workDir)
	require.NoError(t, err)
	require.True(t, present)
	path, err := provisioningPathFor(marker.InstanceID)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if os.PathSeparator != '\\' {
		require.Equal(t, fs.FileMode(0o600), info.Mode().Perm(), "进度记录必须仅属主可读写")
	}
}
