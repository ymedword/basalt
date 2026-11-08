package app

// 本文件判新实例配置模板的**命令白名单**（14D T117 现场发现的缺陷的回归面）：
// 全新安装时生成的配置若不登记随包运行时组件，内置 MCP 条目的绝对路径 command 会在
// spawn 之前被策略拒绝（`decision=deny reason=policy_denied`），开箱即用走不到
// "依赖就位"。两条断言各自对应一种真实故障：
//   - 模板登记的命令与安装器写入的条目**不同源** → 少一条即拒；多一条即越权放行；
//   - 生成正文不可装载（单引号里的 Windows 反斜杠被当成转义）→ 首个进程起不来。
//
// These tests pin the allowlist the fresh-instance template writes: same source as the
// installer's entries, and the generated body must load as-is.

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/builtin"
	"github.com/ymedword/basalt/internal/cli"
)

// templateDSNEnv 是生成模板 `database.dsn` 引用的环境变量名；下面的用例断言模板正文
// 与它逐字一致——改名时两边一起改，不会留下"模板引用了没人注入的变量"的断线。
// templateDSNEnv is the variable the generated template references by name.
const templateDSNEnv = "BASALT_DATABASE_DSN"

// TestInstanceConfigTemplate_RegistersBundledCommands 判模板登记的就是安装器可能写入的
// 全部命令：随包条目（免凭据两条 + 凭据就绪后才写的 github）的 command 一律在内，且都是
// 绝对路径——白名单整串逐字比对，相对路径永远不命中。
func TestInstanceConfigTemplate_RegistersBundledCommands(t *testing.T) {
	// 模板把 DSN 写成 `${BASALT_DATABASE_DSN}`：装载期要求该变量存在，值只经环境提供。
	t.Setenv(templateDSNEnv, "postgres://template-fixture/basalt")
	runtimeRoot := t.TempDir()
	wsRoot := t.TempDir()
	text := instanceConfigTemplate(runtimeRoot, wsRoot)

	// ① 生成的正文必须能过配置装载：YAML 层面的故障（转义、缩进）在这里暴露，
	// 而不是等到首个进程启动失败。
	path := filepath.Join(t.TempDir(), configFileName)
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	cfg, err := cli.LoadConfig(path, os.LookupEnv)
	require.NoError(t, err, "生成的配置必须能装载：\n%s", text)
	// 占位确实接到了变量上：运行期唯一存储是 PostgreSQL，这里断的是"接线"，不是值。
	require.Equal(t, "postgres://template-fixture/basalt", cfg.Database.DSN)

	// ② 白名单与条目同源：安装器写什么命令，这里就登记什么。
	paths := builtinRuntimePaths(runtimeRoot, wsRoot)
	want := builtin.AllowlistedCommands(paths)
	require.Equal(t, want, cfg.Shell.AllowedCommands)
	require.NotEmpty(t, want)
	for _, entry := range builtin.CredentialFreeEntries(paths) {
		require.Contains(t, want, entry.Command,
			"免凭据条目 %s 的命令必须在白名单里，否则它会在 spawn 前被策略拒绝", entry.Name)
	}
	github := builtin.GitHubEntry(paths)
	require.Contains(t, want, github.Command,
		"github 条目凭据就绪后才写，但命令必须在首启就登记——否则补上凭据的那次启动仍然被拒")
	for _, command := range want {
		require.True(t, filepath.IsAbs(command), "白名单里的命令必须是绝对路径：%q", command)
	}
}

// TestInstanceConfigTemplate_NoRuntimeRootSkipsAllowlist 判"取不到程序目录"时的回落：
// 不猜路径、整节省略。没有白名单等于显式登记口径（fail-closed），不得退化成放行。
func TestInstanceConfigTemplate_NoRuntimeRootSkipsAllowlist(t *testing.T) {
	t.Setenv(templateDSNEnv, "postgres://template-fixture/basalt")
	text := instanceConfigTemplate("", t.TempDir())
	require.NotContains(t, text, "allowed_commands",
		"取不到程序目录时不得写任何白名单：猜出来的路径会把别的目录里的组件登记进来")

	path := filepath.Join(t.TempDir(), configFileName)
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	cfg, err := cli.LoadConfig(path, os.LookupEnv)
	require.NoError(t, err)
	require.Empty(t, cfg.Shell.AllowedCommands)
}

// TestInstanceConfigTemplate_RequiresDatabaseDSN 判占位不等于"无需准备数据库"：模板里
// `database.dsn` 只写 `${VAR}`，变量缺失时装载层当场拒绝并点名配置键与变量名——生成配置
// 的那个进程自己也不会带着一套缺省连接串偷偷启动，更不会回退 SQLite。
func TestInstanceConfigTemplate_RequiresDatabaseDSN(t *testing.T) {
	// t.Setenv 负责登记"测试结束后恢复原值"，紧接着删掉才是"变量缺失"这一读数：
	// 变量存在但为空是另一种读数（由要求 DSN 的子命令拒绝），两者不混判。
	t.Setenv(templateDSNEnv, "")
	require.NoError(t, os.Unsetenv(templateDSNEnv))
	text := instanceConfigTemplate("", t.TempDir())
	require.Contains(t, text, "${"+templateDSNEnv+"}",
		"模板必须逐字引用环境变量 %s：\n%s", templateDSNEnv, text)

	path := filepath.Join(t.TempDir(), configFileName)
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	_, err := cli.LoadConfig(path, os.LookupEnv)
	require.Error(t, err, "DSN 变量缺失时生成的配置不得被放行")
	require.Contains(t, err.Error(), "database.dsn")
	require.Contains(t, err.Error(), templateDSNEnv)
}

// snapshotWorkDir 把工作区里每个文件的相对路径 → SHA-256 记下来：零写入判据
// 不能只看"文件还在"，还要看字节没变（US-25 §4.6 的顺序 / 幂等断言）。
// snapshotWorkDir hashes every file so "no writes" is checked at the byte level.
func snapshotWorkDir(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		files[rel] = fmt.Sprintf("%x", sha256.Sum256(raw))
		return nil
	})
	require.NoError(t, err)
	return files
}

// TestFirstInstall_PrepareOrderAndIdempotency 判首次准备的四步顺序与幂等：
// 配置 → provisioning.pending → 内置安装意图 → 名册；第二次调用 Fresh=false 且
// 工作区逐字节不变。顺序证据是"pending 记录的实例标识与名册一致"——名册最后写，
// 若 pending 在名册之后才写，就没有任何一致标识可写。
// 逆改点：调换名册与 pending 的写入次序、去掉 O_EXCL、或去掉"非新实例直接返回"
// 的幂等短路，本用例在文件集合 / 报告标志 / 字节快照处变红。
func TestFirstInstall_PrepareOrderAndIdempotency(t *testing.T) {
	workDir := t.TempDir()
	adapter := firstInstallAdapter{}

	report, err := adapter.Prepare(workDir)
	require.NoError(t, err)
	require.True(t, report.Fresh, "全新目录必须判为新实例")
	require.True(t, report.ConfigCreated)
	require.True(t, report.ProvisioningRecorded)
	require.True(t, report.MarkerCreated)
	require.Regexp(t, instanceIDPattern, report.InstanceID)

	// ① 配置 ② pending（属于名册里的同一标识）③ 内置安装意图 ④ 名册。
	_, statErr := os.Stat(filepath.Join(workDir, configFileName))
	require.NoError(t, statErr, "配置必须已落盘")
	marker, present, err := readInstanceMarker(workDir)
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, report.InstanceID, marker.InstanceID, "报告标识必须就是名册里的标识")
	pending, found, err := readProvisioningRecord(report.InstanceID)
	require.NoError(t, err)
	require.True(t, found, "pending 记录必须先于名册、且属于同一候选标识")
	require.Equal(t, report.InstanceID, pending.InstanceID)
	require.Equal(t, adminTokenStatePending, pending.AdminToken)
	_, statErr = os.Stat(filepath.Join(workDir, workspaceDirName, builtinOpenBoxControlDir, builtinOpenBoxFileName))
	require.NoError(t, statErr, "内置安装意图必须先于名册落盘")
	pendingNow, pendingID, _ := adminTokenProvisioningPending(workDir)
	require.True(t, pendingNow, "首次准备后密码进度应为 pending")
	require.Equal(t, report.InstanceID, pendingID)

	// ② 第二次调用零写入：报告全假、标识为空（不伪造"本次新建"）。
	before := snapshotWorkDir(t, workDir)
	second, err := adapter.Prepare(workDir)
	require.NoError(t, err)
	require.False(t, second.Fresh, "已完成的工作区不再是新实例")
	require.False(t, second.ConfigCreated)
	require.False(t, second.ProvisioningRecorded)
	require.False(t, second.MarkerCreated)
	require.Empty(t, second.InstanceID)
	require.Equal(t, before, snapshotWorkDir(t, workDir), "第二次调用必须逐字节零写")
}

// TestFirstInstall_ProvisioningFailureNeverWritesRoster 是 §4.6 顺序判据的失败注入
// 版本：让第②步（provisioning.pending）写不出来，第④步名册必须**不**出现。"pending
// 与名册同标识"只能证明两者都写了，证明不了先后；本用例才真正把次序钉住。
// 逆改点：把名册移到 pending 之前——名册会先落盘，这里在 NoFileExists 处红。
// Injects a failure in step ② and proves the roster (step ④) is never written.
func TestFirstInstall_ProvisioningFailureNeverWritesRoster(t *testing.T) {
	workDir := t.TempDir()
	// 私有根指向"父路径是普通文件"的位置：MkdirAll 必失败，不依赖 chmod / 文件系统。
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	privateRoot := filepath.Join(blocker, "data")
	t.Setenv("LOCALAPPDATA", privateRoot)
	t.Setenv("XDG_DATA_HOME", privateRoot)
	t.Setenv("HOME", privateRoot)

	_, err := (firstInstallAdapter{}).Prepare(workDir)
	require.Error(t, err, "进度记录写不出来时必须失败关闭")
	require.NoFileExists(t, filepath.Join(workDir, workspaceDirName, instanceMarkerFileName),
		"pending 写失败时名册不得出现（名册在 pending 之后）")
	require.FileExists(t, filepath.Join(workDir, configFileName), "配置是第①步，失败前已落盘")
}

// TestFirstInstall_IntentFailureNeverWritesRoster 让第③步（内置安装意图）失败：
// 第②步 pending 必须已写，第④步名册必须不出现。
// 逆改点：把名册移到安装意图之前——名册会先落盘，这里在 NoFileExists 处红。
// Injects a failure in step ③: pending exists, the roster does not.
func TestFirstInstall_IntentFailureNeverWritesRoster(t *testing.T) {
	workDir := t.TempDir()
	privateRoot := t.TempDir()
	t.Setenv("LOCALAPPDATA", privateRoot)
	t.Setenv("XDG_DATA_HOME", privateRoot)
	t.Setenv("HOME", privateRoot)
	wsRoot := filepath.Join(workDir, workspaceDirName)
	require.NoError(t, os.MkdirAll(wsRoot, 0o750))
	// `.basalt/control` 位置放普通文件：内置安装意图的 MkdirAll 失败。
	require.NoError(t, os.WriteFile(filepath.Join(wsRoot, builtinOpenBoxControlDir), []byte("x"), 0o644))

	_, err := (firstInstallAdapter{}).Prepare(workDir)
	require.Error(t, err, "内置安装意图写不出来时必须失败关闭")
	require.NoFileExists(t, filepath.Join(wsRoot, instanceMarkerFileName),
		"安装意图写失败时名册不得出现（名册在意图之后）")
	records, err := filepath.Glob(filepath.Join(privateRoot, instanceDataDirName, instancesDirName, "*", provisioningFileName))
	require.NoError(t, err)
	require.Len(t, records, 1, "pending 是第②步，必须已写")
}

// TestFirstInstall_ConcurrentStartOneWinner 判并发首启：多个 Prepare 同时跑，
// 配置与名册各按 O_EXCL 裁决，**恰好一个** winner；所有进程读到的实例标识一致。
// pending 记录允许每个候选各写一条（输家的记录属于一个从未成为实例的随机标识，
// 没有任何读者），因此这里只断言配置 / 名册的写数。
// 逆改点：去掉 createExclusiveFile 的 O_EXCL 或改成覆盖写，本用例的 winner 计数
// 会大于一，名册标识也会出现两个。
func TestFirstInstall_ConcurrentStartOneWinner(t *testing.T) {
	workDir := t.TempDir()
	const starters = 8
	type attempt struct {
		report cli.FirstInstallReport
		err    error
	}
	results := make([]attempt, starters)
	begin := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-begin
			results[index].report, results[index].err = firstInstallAdapter{}.Prepare(workDir)
		}(i)
	}
	close(begin)
	wg.Wait()

	configCreators, markerCreators := 0, 0
	ids := map[string]struct{}{}
	for _, result := range results {
		require.NoError(t, result.err)
		if result.report.ConfigCreated {
			configCreators++
		}
		if result.report.MarkerCreated {
			markerCreators++
		}
		if result.report.InstanceID != "" {
			ids[result.report.InstanceID] = struct{}{}
		}
	}
	require.Equal(t, 1, configCreators, "并发首启只允许一个进程写出配置")
	require.Equal(t, 1, markerCreators, "并发首启只允许一个进程写出名册")
	require.Len(t, ids, 1, "所有进程必须以盘上名册的标识为准")

	marker, present, err := readInstanceMarker(workDir)
	require.NoError(t, err)
	require.True(t, present)
	require.Contains(t, ids, marker.InstanceID)
}

// TestUS25_FirstInstall_HalfWrittenRosterFailsClosed 判半截名册：`detectNewInstance`
// 只按"文件存在"判存量，若把半截名册当成"未创建"自动续做，就会在一个失去自动
// 初始化资格的工作区上继续建库甚至铸钥（C-04 / FR-25-05）。适配器必须读一次名册
// 并失败关闭；判定表在非 fresh、无 pending 时也只能给 await_init。
// 逆改点：删掉 firstInstallAdapter.Prepare 里的 readInstanceMarker 校验，本用例
// 在 require.Error 处变红；把判定表的 create 条件改成只看"无记录无密钥"，最后
// 一条断言变红。
func TestUS25_FirstInstall_HalfWrittenRosterFailsClosed(t *testing.T) {
	workDir := t.TempDir()
	wsRoot := filepath.Join(workDir, workspaceDirName)
	require.NoError(t, os.MkdirAll(wsRoot, 0o750))
	rosterPath := filepath.Join(wsRoot, instanceMarkerFileName)
	half := []byte(`{"version":1,"instance_id":"inst_0123456789ab`)
	require.NoError(t, os.WriteFile(rosterPath, half, 0o600))

	before := snapshotWorkDir(t, workDir)
	_, err := firstInstallAdapter{}.Prepare(workDir)
	require.Error(t, err, "半截名册必须失败关闭，不得当作未创建自动续做")
	require.Contains(t, err.Error(), "实例名册")
	require.Contains(t, err.Error(), "人工核对")
	require.Equal(t, before, snapshotWorkDir(t, workDir), "失败关闭时不得写任何文件（含不覆盖半截名册）")

	pending, _, reason := adminTokenProvisioningPending(workDir)
	require.False(t, pending, "读不懂的名册不得被当作 pending 续做资格")
	require.Contains(t, reason, "名册")
	decision := EvaluateSecrets(SecretsFacts{Configured: true, FreshInstance: false, AutoInit: true})
	require.Equal(t, SecretsActionAwaitInit, decision.Action, "半截名册不得触发自动铸钥")
}
