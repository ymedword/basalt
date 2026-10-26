package app

// 本文件是管理面访问密码三来源的判卷（14D T093；模块卡验收 F2 / F5–F9）。
//
// 覆盖的**逆改点**（每一条都对应一种真实事故）：
//   - 三处来源全缺时回落到 local_trust（或交出空密码让闸门放行）→ F6 红：
//     等于把管理面在无凭据状态下开放；
//   - 存量实例自动生成密码 / 无名册实例猜路径 → F6 红（无人工确认地改写了部署事实）；
//   - 密码写成编译期常量 → 两实例对比红（所有实例共用同一把公共钥匙）；
//   - 非交互输出里带密码值 → F7 红（CI 日志与终端录制里出现可用凭据）；
//   - 第二次启动重复打印 → F8 红（密码进入每一次的日志滚动）；
//   - 密码与主密钥同文件 / 主密钥恢复态阻塞登录 → F9 红。
//
// 密码值只出现在测试内存与临时目录里；断言一律用"包含 / 不包含"，不落真实凭据。
// These tests pin the third credential source: fail-closed refusals, no local-trust
// fallback, no constant password, TTY-only disclosure, and file separation.

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/web"
)

// adminTokenFixture 是「一个全新工作区 + 一个独立的用户数据根」：实例私有目录按
// 名册推导到测试临时目录，**绝不**触碰真实 `%LOCALAPPDATA%` / `~/.local/share`。
// The fixture points the per-user data root at a temp directory so the private
// directory never leaks into the real user profile.
type adminTokenFixture struct {
	workDir  string
	dataRoot string
	app      *App
}

// newAdminTokenFixture 建立夹具。三个平台环境变量同时指向临时目录：Windows 取
// LOCALAPPDATA，Linux 取 XDG_DATA_HOME，macOS 走 HOME。
// newAdminTokenFixture points every platform-specific data-root variable at the temp
// directory.
func newAdminTokenFixture(t *testing.T) *adminTokenFixture {
	t.Helper()
	base := t.TempDir()
	workDir := filepath.Join(base, "workspace")
	require.NoError(t, os.MkdirAll(workDir, 0o750))
	dataRoot := filepath.Join(base, "userdata")
	t.Setenv("LOCALAPPDATA", dataRoot)
	t.Setenv("XDG_DATA_HOME", dataRoot)
	t.Setenv("HOME", dataRoot)
	return &adminTokenFixture{
		workDir:  workDir,
		dataRoot: dataRoot,
		app:      &App{workDir: workDir, freshInstance: true},
	}
}

// writeRoster 写入实例名册并返回实例标识（私有目录的推导依据）。
// writeRoster writes the instance roster that the private directory derives from.
func (f *adminTokenFixture) writeRoster(t *testing.T) string {
	t.Helper()
	id, err := newInstanceID()
	require.NoError(t, err)
	path := instanceMarkerPath(f.workDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	raw, err := marshalInstanceMarker(instanceMarker{Version: instanceMarkerVersion, InstanceID: id})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o644))
	return id
}

// tokenPath 返回按名册推导的密码文件路径。
// tokenPath returns the derived admin-token path.
func (f *adminTokenFixture) tokenPath(t *testing.T) string {
	t.Helper()
	path, err := defaultAdminTokenFile(f.workDir)
	require.NoError(t, err)
	return path
}

// tokenModeConfig 是「token 模式 + 省略 token」的配置，即三来源解析的入口形态。
// tokenModeConfig is the token mode with the token omitted.
func tokenModeConfig() *cli.Config {
	return &cli.Config{HTTP: cli.HTTPConfig{
		AdminAuth: cli.AdminAuthConfig{Mode: adminAuthModeToken},
	}}
}

// quietAppLogger 丢弃装配留痕，保持测试输出干净（日志内容另有专门断言）。
// quietAppLogger drops assembly logging inside tests.
func quietAppLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// probeAdminLogin 用「闸门配置的密码」与「实际提交的密码」经真实认证闸门访问一次
// 受保护处理函数，返回响应与受保护处理函数被调用的次数（拒绝路径必须为零）。
// probeAdminLogin drives the real guard with a configured and a presented password.
func probeAdminLogin(t *testing.T, configured, presented string) (*httptest.ResponseRecorder, int) {
	t.Helper()
	guard, err := web.NewAuthGuard(web.AdminAuthConfig{
		Mode:           adminAuthModeToken,
		Token:          configured,
		AllowedHosts:   []string{"127.0.0.1:8080"},
		AllowedOrigins: []string{"http://127.0.0.1:8080"},
	}, quietAppLogger(), time.Now)
	require.NoError(t, err)
	hits := 0
	handler := guard.Wrap(func(w http.ResponseWriter, _ *http.Request, _ core.Actor) {
		hits++
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+presented)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec, hits
}

// TestAdminToken_NoLocalTrustFallback 覆盖 F6 的拒绝面与 F5 的「无内置缺省密码」：
// 存量实例与无名册实例都必须在三来源全缺时**启动失败**，错误点名三处并明说
// 不回落 local_trust；新实例生成的密码必须是随机值而不是常量。
// 逆改点：任一分支回落到 local_trust → 断言红；把生成的密码换成固定常量 →
// 两实例对比红；把拒绝改写成"按未初始化继续" → 路径断言红。
func TestAdminToken_NoLocalTrustFallback(t *testing.T) {
	t.Run("存量实例：有名册无密码文件 → 启动失败并点名三处", func(t *testing.T) {
		f := newAdminTokenFixture(t)
		f.writeRoster(t)
		f.app.freshInstance = false

		res, err := f.app.resolveAdminToken(tokenModeConfig())
		require.Error(t, err, "存量实例的密码文件缺失不得被当成可生成")
		require.Empty(t, res.Token, "失败路径不得交出任何密码值")
		msg := err.Error()
		require.Contains(t, msg, "http.admin_auth.token", "错误必须点名配置键")
		require.Contains(t, msg, "三处都没有提供", "错误必须说明三处来源都缺")
		require.Contains(t, msg, f.tokenPath(t), "错误必须点名密码文件路径")
		require.Contains(t, msg, "local_trust", "错误必须明说不得回落 local_trust")
		require.NoFileExists(t, f.tokenPath(t), "失败路径不得落盘任何密码文件")
	})

	t.Run("无名册实例：不猜路径，同样启动失败", func(t *testing.T) {
		f := newAdminTokenFixture(t)
		res, err := f.app.resolveAdminToken(tokenModeConfig())
		require.Error(t, err)
		require.Empty(t, res.Token)
		msg := err.Error()
		require.Contains(t, msg, "instance.json", "无名册时必须点名缺失的名册")
		require.Contains(t, msg, "无法推导路径", "无名册时不得猜私有目录")
		require.Contains(t, msg, "local_trust")
	})

	t.Run("无内置缺省密码：两次新实例生成不同的随机值", func(t *testing.T) {
		first := newAdminTokenFixture(t)
		first.writeRoster(t)
		a, err := first.app.resolveAdminToken(tokenModeConfig())
		require.NoError(t, err)
		require.Equal(t, adminTokenSourceGenerated, a.Source)

		second := newAdminTokenFixture(t)
		second.writeRoster(t)
		b, err := second.app.resolveAdminToken(tokenModeConfig())
		require.NoError(t, err)
		require.Equal(t, adminTokenSourceGenerated, b.Source)
		require.NotEqual(t, a.Token, b.Token, "访问密码必须是随机生成，不得是编译期常量")
		require.Len(t, a.Token, base64.RawURLEncoding.EncodedLen(adminTokenBytes),
			"密码长度必须由随机字节数决定")
		require.NotContains(t, instanceConfigTemplate("", ""), "token:",
			"生成的配置模板只写 mode，不得把密码值写进配置文件")
	})
}

// TestAdminToken_AcceptsInstanceFile 覆盖 F6 的接受面与 F2 的登录判定：部署者写入
// 的密码文件是合法来源；正确密码放行、错误密码 401 且不回显任何值。
// 逆改点：只认配置与环境变量（忽略文件）→ 首个断言红；把提交值回显进 401 →
// 回显断言红。
func TestAdminToken_AcceptsInstanceFile(t *testing.T) {
	const deployerPassword = "deployer-provided-password-7b1c"

	t.Run("密码文件是合法来源且可用于登录", func(t *testing.T) {
		f := newAdminTokenFixture(t)
		f.writeRoster(t)
		f.app.freshInstance = false // 存量实例：文件是唯一来源
		path := f.tokenPath(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(deployerPassword+"\r\n"), 0o600),
			"部署者常用编辑器写入尾随换行，读取时必须去掉")

		res, err := f.app.resolveAdminToken(tokenModeConfig())
		require.NoError(t, err)
		require.Equal(t, adminTokenSourceFile, res.Source)
		require.Equal(t, deployerPassword, res.Token)
		require.Equal(t, path, res.Path)

		rec, hits := probeAdminLogin(t, res.Token, deployerPassword)
		require.Equal(t, http.StatusOK, rec.Code, "密码文件来源必须能登录（F6）")
		require.Equal(t, 1, hits)

		wrong, wrongHits := probeAdminLogin(t, res.Token, "wrong-password-value")
		require.Equal(t, http.StatusUnauthorized, wrong.Code)
		require.Zero(t, wrongHits, "拒绝路径不得调用受保护处理函数")
		require.NotContains(t, wrong.Body.String(), deployerPassword, "401 不得回显正确值")
		require.NotContains(t, wrong.Body.String(), "wrong-password-value", "401 不得回显提交值")
	})

	t.Run("显式配置优先于文件", func(t *testing.T) {
		f := newAdminTokenFixture(t)
		f.writeRoster(t)
		f.app.freshInstance = false
		path := f.tokenPath(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("file-password"), 0o600))

		cfg := tokenModeConfig()
		cfg.HTTP.AdminAuth.Token = "explicit-config-value"
		res, err := f.app.resolveAdminToken(cfg)
		require.NoError(t, err)
		require.Equal(t, adminTokenSourceConfig, res.Source)
		require.Equal(t, "explicit-config-value", res.Token)
		require.Empty(t, res.Path, "配置来源不涉及密码文件路径")
	})

	t.Run("空密码文件不算来源", func(t *testing.T) {
		f := newAdminTokenFixture(t)
		f.writeRoster(t)
		f.app.freshInstance = false
		path := f.tokenPath(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("  \n"), 0o600))

		_, err := f.app.resolveAdminToken(tokenModeConfig())
		require.Error(t, err, "文件在但内容为空不是有效来源")
		require.Contains(t, err.Error(), "内容为空")
	})
}

// TestAdminToken_NonTTYNoValue 覆盖 F7：stdout 非 TTY 时只输出密码文件路径，
// **不**含密码值；同一场景下密码仍可用于登录。
// 逆改点：非交互分支打印了值 → 断言红（CI 日志里出现可用凭据）。
func TestAdminToken_NonTTYNoValue(t *testing.T) {
	f := newAdminTokenFixture(t)
	f.writeRoster(t)
	res, err := f.app.resolveAdminToken(tokenModeConfig())
	require.NoError(t, err)
	require.Equal(t, adminTokenSourceGenerated, res.Source)

	// bytes.Buffer 与重定向到普通文件都属"非交互"：值绝不出现。
	var buffered bytes.Buffer
	announceAdminToken(&buffered, res, isInteractiveWriter(&buffered))
	require.Contains(t, buffered.String(), res.Path, "非交互终端必须给出密码文件路径")
	require.NotContains(t, buffered.String(), res.Token, "非交互终端绝不能打印密码值")

	redirected, err := os.Create(filepath.Join(t.TempDir(), "stdout.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = redirected.Close() })
	require.False(t, isInteractiveWriter(redirected), "重定向到普通文件不是字符设备")
	require.False(t, isInteractiveWriter(&bytes.Buffer{}), "内存缓冲不是字符设备")

	// 同一场景下密码仍可用：盘上读回的值与登录判定一致。
	onDisk, err := readAdminTokenFile(res.Path)
	require.NoError(t, err)
	require.Equal(t, res.Token, onDisk)
	rec, _ := probeAdminLogin(t, onDisk, onDisk)
	require.Equal(t, http.StatusOK, rec.Code, "非交互生成不降低密码的可用性")
}

// TestAdminToken_InteractiveOnce 覆盖 F8：交互式终端首次生成时显示一次值与路径；
// 同一实例第二次启动读到文件来源，不再输出任何内容。
// 逆改点：把提示挂在每次启动上 → 第二次输出断言红。
func TestAdminToken_InteractiveOnce(t *testing.T) {
	f := newAdminTokenFixture(t)
	f.writeRoster(t)

	first, err := f.app.resolveAdminToken(tokenModeConfig())
	require.NoError(t, err)
	require.Equal(t, adminTokenSourceGenerated, first.Source)

	var stdout bytes.Buffer
	announceAdminToken(&stdout, first, true)
	require.Contains(t, stdout.String(), first.Token, "交互式首次启动必须显示一次密码值")
	require.Contains(t, stdout.String(), first.Path)
	require.Equal(t, 1, strings.Count(stdout.String(), first.Token), "同一密码只显示一次")

	second, err := f.app.resolveAdminToken(tokenModeConfig())
	require.NoError(t, err)
	require.Equal(t, adminTokenSourceFile, second.Source, "第二次启动必须读到既有文件")
	require.Equal(t, first.Token, second.Token, "不得生成第二把密码")
	var again bytes.Buffer
	announceAdminToken(&again, second, true)
	require.Empty(t, again.String(), "非首次生成不得再次打印（值或路径）")
}

// TestInstanceSecrets_AdminTokenSeparateFile 覆盖 F9：密码文件位于实例私有目录
// （工作区外）、与主密钥**分文件**、权限同级；主密钥进入恢复态时管理台登录不受影响。
// 逆改点：把密码塞进主密钥文件 / 让恢复态阻塞登录 → 对应断言红。
func TestInstanceSecrets_AdminTokenSeparateFile(t *testing.T) {
	ctx := context.Background()
	env := newResolverFixture(t)
	workDir := filepath.Dir(env.root) // workDir/.basalt 才是工作区根
	dataRoot := filepath.Join(t.TempDir(), "userdata")
	t.Setenv("LOCALAPPDATA", dataRoot)
	t.Setenv("XDG_DATA_HOME", dataRoot)
	t.Setenv("HOME", dataRoot)

	id, err := newInstanceID()
	require.NoError(t, err)
	markerPath := instanceMarkerPath(workDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(markerPath), 0o750))
	raw, err := marshalInstanceMarker(instanceMarker{Version: instanceMarkerVersion, InstanceID: id})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(markerPath, raw, 0o644))

	keyPath, err := defaultMasterKeyFile(workDir)
	require.NoError(t, err)
	tokenPath, err := defaultAdminTokenFile(workDir)
	require.NoError(t, err)
	require.NotEqual(t, keyPath, tokenPath, "访问密码与主密钥必须分文件")
	require.Equal(t, filepath.Dir(keyPath), filepath.Dir(tokenPath), "两者同处实例私有目录")
	require.NotContains(t, keyPath, env.root, "私有目录必须在工作区之外")
	require.True(t, strings.HasPrefix(keyPath, dataRoot), "私有目录必须在用户数据根之下")

	store := storage.NewInstanceSecretsStore(env.db)
	newService := func() *InstanceSecretsService {
		service, err := NewInstanceSecretsService(InstanceSecretsDeps{
			Store:         store,
			Operations:    env.ops,
			MasterKeyFile: keyPath,
			WorkspaceRoot: env.root,
			AutoInit:      true,
			FreshInstance: true,
			Logger:        quietAppLogger(),
			Now:           baselineClock,
		})
		require.NoError(t, err)
		return service
	}
	status, err := newService().Ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateReady, status.Decision.State, "新实例首次初始化必须就绪")

	app := &App{workDir: workDir, freshInstance: true}
	res, err := app.resolveAdminToken(tokenModeConfig())
	require.NoError(t, err)
	require.Equal(t, adminTokenSourceGenerated, res.Source)
	require.Equal(t, tokenPath, res.Path)

	require.FileExists(t, keyPath)
	require.FileExists(t, tokenPath)
	keyBytes, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	tokenBytes, err := os.ReadFile(tokenPath)
	require.NoError(t, err)
	require.NotContains(t, string(keyBytes), res.Token, "主密钥文件里不得混入访问密码")
	require.Equal(t, res.Token+"\n", string(tokenBytes), "密码文件只写一行密码")

	// 权限同级：Unix 上两个文件都只对属主开放；Windows 由目录的受限 DACL 继承
	// （A12 已单独判卷 DACL 先于写入）。
	if runtime.GOOS != "windows" {
		for _, path := range []string{keyPath, tokenPath} {
			info, statErr := os.Stat(path)
			require.NoError(t, statErr)
			require.Zero(t, info.Mode().Perm()&0o077, "%s 不得对组 / 其他用户开放", path)
		}
	}

	// 主密钥进入恢复态不得阻塞登录：密码与密钥的恢复语义不同（分文件的理由）。
	require.NoError(t, os.Remove(keyPath))
	recovered, err := newService().Ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, core.SecretStateRecoveryRequired, recovered.Decision.State)
	again, err := app.resolveAdminToken(tokenModeConfig())
	require.NoError(t, err)
	require.Equal(t, res.Token, again.Token, "主密钥恢复态不得阻塞管理台登录")
	require.Equal(t, adminTokenSourceFile, again.Source)
}
