package app

// 本文件是 14D「MCP 逐槽位凭据」B1–B8 的判卷（设计 §2.3.2 / §3.2；颗粒度文档 §4.5.4）：
//
//	B1 写入后明文不出现在定义文件 / 版本 / 操作行 / 日志 / 视图里，库里只有密文；
//	B2 同一 server 的每个槽位各自独立版本，替换其一只让该键的 version_no 递增；
//	B3 撤销立即不可用、不回落进程环境，密文置 NULL；
//	B4 普通编辑（改 command / args / 其它槽位）不清除已托管值，也不追加版本；
//	B5 http.headers 的 ${VAR} 与 env 走同一闸门：缺失 503 + operation_id，文件零变化；
//	B6 args 出现 ${ 在新增 / 修改时 400 并点名参数序号，存量条目仍可加载且被诊断点名；
//	B7 来源显式：managed 取不到值时不读进程环境（即使同名变量有值），env 只读进程环境；
//	B8 认证头装配：Authorization 加 `Bearer `、其它头原样（不推断前缀、不去重），
//	   跨主机重定向不携带凭据；响应、日志与审计都不回显头值。
//
// 全部判据打在真实写路径、真实 SQLite 与真实 HTTP 传输上：夹具只提供测试自己的临时
// 工作区与回环服务，**不**替换仓储或传输实现。
// Every judgement runs against the real save path, real SQLite rows and a real
// in-process streamable HTTP server; no store or transport is stubbed.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/testutil"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// mcpCredAlphaEnv / mcpCredBetaEnv 是同一 server 的两个 env 槽位：B2 用它们证明
	// "按键独立版本"，B4 用它们证明"编辑不清值"。两个槽位都走托管，不借用 TestMain
	// 导出的哨兵变量——那是别的用例的判据面。
	mcpCredAlphaEnv = "MCP_CRED_ALPHA_TOKEN"
	mcpCredBetaEnv  = "MCP_CRED_BETA_TOKEN"
	// mcpCredMissingEnv 是"未设置 / 为空"的变量名（B5 的 headers 闸门）。
	mcpCredMissingEnv = "MCP_CRED_ABSENT_TOKEN"

	// B8 的两个请求头槽位：Authorization 走 Bearer 装配，其它头原样。头名不是合法的
	// 环境变量名，所以占位符另用一组合法名字——占位名与槽位名允许不同名。
	mcpCredAuthHeader = "Authorization"
	mcpCredKeyHeader  = "X-Api-Key"
	mcpCredAuthVar    = "MCP_CRED_AUTH_TOKEN"
	mcpCredKeyVar     = "MCP_CRED_KEY_TOKEN"

	// 明文哨兵：只允许存在于内存与密文里；任何可读面出现即是判卷失败。
	mcpCredAlphaOne     = "alpha-secret-version-one-1a2b"
	mcpCredBetaOne      = "beta-secret-version-one-3c4d"
	mcpCredAlphaTwo     = "alpha-secret-version-two-5e6f"
	mcpCredHeaderToken  = "header-secret-value-7g8h"
	mcpCredNestedBearer = "Bearer nested-bearer-value-9i0j"

	// mcpCredEnvTrap 是"同名变量在进程环境里有值"的哨兵：B3 / B7 用它证明实现没有
	// 回落进程环境——一旦回落，错误消失、值变成它，断言随即红。
	mcpCredEnvTrap = "process-env-trap-value-0z9y"
)

// mcpManagedValue 声明一个槽位走托管并给出本次写入的明文。
// mcpManagedValue declares a managed slot and supplies the plaintext to seal.
func mcpManagedValue(slot, value string) MCPCredentialRequest {
	return MCPCredentialRequest{Key: slot, Mode: string(core.CredentialModeManaged), Value: &value}
}

// mcpManagedKeep 声明一个槽位走托管、但本次不给值（沿用既有托管值）。
// mcpManagedKeep declares a managed slot without a value: keep the stored one.
func mcpManagedKeep(slot string) MCPCredentialRequest {
	return MCPCredentialRequest{Key: slot, Mode: string(core.CredentialModeManaged)}
}

// mcpEnvSource 声明一个槽位只读进程环境（不带值）。
// mcpEnvSource declares a process-environment slot.
func mcpEnvSource(slot string) MCPCredentialRequest {
	return MCPCredentialRequest{Key: slot, Mode: string(core.CredentialModeEnv)}
}

// mcpPlaceholder 拼出整值 `${VAR}` 占位表达式。
// mcpPlaceholder renders the whole-value ${VAR} placeholder.
func mcpPlaceholder(variable string) string { return "${" + variable + "}" }

// mcpCredentialEnv 在资源夹具之上补出凭据写面与两个仓储句柄：B3 / B7 需要在真实的
// 操作台账上撤销槽位、需要直读身份与版本行。仓储句柄与夹具共用同一个 `*storage.DB`。
// mcpCredentialEnv adds the credential write surface and direct store handles on top of
// the resource fixture; every handle shares the fixture's single database.
type mcpCredentialEnv struct {
	*resourceEnv
	revoker *CredentialOps
	creds   *storage.CredentialStore
	sources *storage.MCPCredentialSourceStore
}

// newMCPCredentialEnv 装配夹具；撤销写面按 wire 的口径绑定真仓储与基线时钟。
// mutate 在资源写面构造前改依赖（14D 受控重建用例据此换依赖探针）。
// newMCPCredentialEnv wires the fixture; the revoke surface gets the real stores.
// mutate adjusts the resource-admin deps before construction.
func newMCPCredentialEnv(t *testing.T, mutate ...func(*ResourceAdminDeps)) *mcpCredentialEnv {
	t.Helper()
	base := newResourceEnv(t, mutate...)
	creds := storage.NewCredentialStore(base.db)
	revoker, err := NewCredentialOps(CredentialOpsDeps{
		Operations:    base.ops,
		Credentials:   creds,
		Resources:     base.store,
		Notify:        storage.NewNotifyAdminStore(base.db),
		MasterKeyFile: base.masterKeyFile,
		WorkspaceRoot: base.root,
		Logger:        base.logger,
		Now:           baselineClock,
	})
	require.NoError(t, err)
	return &mcpCredentialEnv{
		resourceEnv: base,
		revoker:     revoker,
		creds:       creds,
		sources:     storage.NewMCPCredentialSourceStore(base.db),
	}
}

// uid 取 server 当前的资源身份：撤销与凭据行都按它寻址。
// uid returns the server's current resource identity.
func (e *mcpCredentialEnv) uid(t *testing.T, name string) core.ResourceUID {
	t.Helper()
	return e.pointerOf(core.ResourceMCPServer, name).ResourceUID
}

// publishedConfig 从发布指针读回已发布条目：断言一律针对"真的发出去的那份"，不是请求体。
// publishedConfig parses the published entry behind the publish pointer.
func (e *mcpCredentialEnv) publishedConfig(t *testing.T, name string) core.MCPServerConfig {
	t.Helper()
	pointer := e.pointerOf(core.ResourceMCPServer, name)
	version, err := e.store.VersionByID(context.Background(), pointer.PublishedVersionID)
	require.NoError(t, err)
	cfg, err := workspace.ParseMCPServerBlock([]byte(version.DefinitionYAML))
	require.NoError(t, err)
	return cfg
}

// identity 读一个槽位的托管身份（必须存在）。
// identity reads one slot's managed identity, which must exist.
func (e *mcpCredentialEnv) identity(t *testing.T, name, slot string) core.CredentialRecord {
	t.Helper()
	record, err := e.creds.CredentialByOwnerKey(context.Background(),
		core.BindingMCPServer, e.uid(t, name), slot)
	require.NoError(t, err)
	return record
}

// credentialRowHasNoIdentity 报告一个槽位是否**没有**托管身份（env 槽位必须如此）。
// credentialRowHasNoIdentity reports whether a slot has no managed identity at all.
func (e *mcpCredentialEnv) credentialRowHasNoIdentity(t *testing.T, name, slot string) bool {
	t.Helper()
	_, err := e.creds.CredentialByOwnerKey(context.Background(),
		core.BindingMCPServer, e.uid(t, name), slot)
	return errors.Is(err, core.ErrResourceNotFound)
}

// version 读一个凭据的指定版本行。
// version reads one immutable credential version row.
func (e *mcpCredentialEnv) version(t *testing.T, credentialID string, versionNo int) core.CredentialVersionRecord {
	t.Helper()
	row, err := e.creds.Version(context.Background(), credentialID, versionNo)
	require.NoError(t, err)
	return row
}

// versionCount 数一个凭据的版本行数。
// versionCount counts the versions of one credential.
func (e *mcpCredentialEnv) versionCount(t *testing.T, credentialID string) int {
	t.Helper()
	rows, err := e.creds.Versions(context.Background(), credentialID)
	require.NoError(t, err)
	return len(rows)
}

// resolve 按声明来源解析全部槽位（必须成功），并在用例结束时清零结果。
// resolve resolves every slot by its declared source and zeroes the result at cleanup.
func (e *mcpCredentialEnv) resolve(t *testing.T, name string) map[string]string {
	t.Helper()
	values, err := e.mcpCredentials.Resolve(context.Background(), name, e.uid(t, name),
		e.publishedConfig(t, name))
	require.NoError(t, err)
	t.Cleanup(func() { ZeroMCPCredentialValues(values) })
	return values
}

// resolveErr 解析全部槽位并原样返回错误（失败用例用）；成功时同样登记清零。
// resolveErr returns the resolution error unchanged for the failure cases.
func (e *mcpCredentialEnv) resolveErr(t *testing.T, name string) (map[string]string, error) {
	t.Helper()
	values, err := e.mcpCredentials.Resolve(context.Background(), name, e.uid(t, name),
		e.publishedConfig(t, name))
	if values != nil {
		t.Cleanup(func() { ZeroMCPCredentialValues(values) })
	}
	return values, err
}

// statuses 读槽位投影（按槽位字典序）。
// statuses reads the per-slot projection, sorted by slot.
func (e *mcpCredentialEnv) statuses(t *testing.T, name string) []MCPCredentialStatus {
	t.Helper()
	statuses, err := e.mcpCredentials.Statuses(context.Background(), e.uid(t, name),
		e.publishedConfig(t, name))
	require.NoError(t, err)
	return statuses
}

// revoke 经真实撤销写面撤销一个槽位。
// revoke revokes one slot through the real write surface.
func (e *mcpCredentialEnv) revoke(t *testing.T, name, slot string) CredentialRevokeResult {
	t.Helper()
	result, err := e.revoker.Revoke(context.Background(), CredentialRevokeRequest{
		Actor:      resourceActor(),
		Kind:       core.BindingMCPServer,
		Name:       name,
		UID:        e.uid(t, name),
		BindingKey: slot,
	})
	require.NoError(t, err)
	return result
}

// mcpCredentialGateDetail 按 operation_id 读操作行并解析闸门 detail（分类由调用方指定）：
// 两份分类同为 503，但原因不同——合报会把"撤了还在用"引向"去查环境变量"。
// mcpCredentialGateDetail reads the gate detail and checks the requested class.
func mcpCredentialGateDetail(t *testing.T, env *resourceEnv, operationID, wantClass string) envGateDetail {
	t.Helper()
	row, err := env.ops.GetOperation(context.Background(), operationID)
	require.NoError(t, err, "503 必须能按 operation_id 查到操作行")
	require.Equal(t, core.OperationFailed, row.Status)
	require.Equal(t, resourceResultNotConfigured, row.ResultCode)

	var detail envGateDetail
	require.NoError(t, json.Unmarshal(row.DetailJSON, &detail))
	require.Equal(t, resourceResultNotConfigured, detail.ErrorCode)
	require.Equal(t, mcpGateStagePreflight, detail.Stage)
	require.Equal(t, wantClass, detail.FailureClass)
	return detail
}

// TestMCPCredential_NoPlaintextAnywhere 覆盖 B1：明文只进密文，任何可读面都看不到它。
// 逆改点：把值写进定义文件 / 版本 / 请求摘要 / 日志 / 视图，或把密文原样存成明文
// → 对应的 NotContains / Ciphertext 断言红。
// TestMCPCredential_NoPlaintextAnywhere covers B1: the material never becomes readable.
func TestMCPCredential_NoPlaintextAnywhere(t *testing.T) {
	// 不入 t.Parallel：本文件改写进程环境（t.Setenv），并行会与其它用例互相干扰。
	env := newMCPCredentialEnv(t)
	const name = "vault"

	version, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:        name,
		Transport:   "stdio",
		Command:     "allowed-mcp",
		Args:        []string{"--root", "./workspace"},
		Env:         map[string]string{mcpCredAlphaEnv: mcpPlaceholder(mcpCredAlphaEnv)},
		Credentials: []MCPCredentialRequest{mcpManagedValue(mcpCredAlphaEnv, mcpCredAlphaOne)},
		Create:      true,
	})
	require.NoError(t, err)

	// 定义文件与每个版本都只有占位符：入库的是"引用"，不是材料。
	definition := env.readMCPConfig()
	require.Contains(t, definition, mcpPlaceholder(mcpCredAlphaEnv))
	require.NotContains(t, definition, mcpCredAlphaOne)
	require.NotContains(t, version.DefinitionYAML, mcpCredAlphaOne)
	for _, historical := range env.versionsOfKind(core.ResourceMCPServer, name) {
		require.NotContains(t, historical.DefinitionYAML, mcpCredAlphaOne)
	}

	// 操作台账：请求摘要的凭据片段只渲染 `槽位=来源`，detail 只带分类。
	rows := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerCreate})
	require.Len(t, rows, 1, "一次保存只留一条 mcp_server.create")
	require.NotContains(t, rows[0].RequestHash, mcpCredAlphaOne)
	require.NotContains(t, string(rows[0].DetailJSON), mcpCredAlphaOne)
	require.Equal(t, mcpCredAlphaEnv+"=managed",
		hashMCPCredentialDecls([]MCPCredentialRequest{mcpManagedValue(mcpCredAlphaEnv, mcpCredAlphaOne)}),
		"请求摘要的凭据片段只渲染 槽位=来源")

	// 日志：保存成功时不打日志，所以先经同一个 logger 写一条探针证明缓冲**确实**在收
	// 日志（否则整段扫描是空断言），再整段扫描——任何一行带值即红。
	env.logger.Warn("凭据判卷日志探针", "marker", "mcp-credential-log-scan-live")
	require.Contains(t, env.logs.String(), "mcp-credential-log-scan-live", "夹具必须真的收集日志")
	require.NotContains(t, env.logs.String(), mcpCredAlphaOne)

	// 库里只有密文：密文与明文不相等、也不包含明文；末 4 位是唯一与材料沾边的投影。
	identity := env.identity(t, name, mcpCredAlphaEnv)
	require.Equal(t, core.BindingMCPServer, identity.OwnerKind)
	require.Equal(t, core.CredentialIdentityActive, identity.State)
	require.Equal(t, 1, identity.CurrentVersion)
	require.Equal(t, mcpCredAlphaEnv, identity.BindingKey, "槽位名就是凭据身份的绑定键")
	stored := env.version(t, identity.ID, 1)
	require.NotEmpty(t, stored.Ciphertext)
	require.NotContains(t, string(stored.Ciphertext), mcpCredAlphaOne)
	require.Equal(t, mcpCredAlphaOne[len(mcpCredAlphaOne)-4:], stored.Last4)

	// 视图投影：只回名字、来源、状态与末 4 位。
	require.Equal(t, []MCPCredentialStatus{{
		Key:   mcpCredAlphaEnv,
		Mode:  core.CredentialModeManaged,
		State: core.MCPCredentialManaged,
		Last4: mcpCredAlphaOne[len(mcpCredAlphaOne)-4:],
	}}, env.statuses(t, name))

	// 值确实可解——否则"没有泄露"只是空断言。
	require.Equal(t, mcpCredAlphaOne, env.resolve(t, name)[mcpCredAlphaEnv])
}

// TestMCPCredential_PerKeyVersions 覆盖 B2：同一 server 的多个槽位各自是独立身份，
// 替换其中一个只递增该键的版本号，其余槽位既不换身份也不追加版本。
// 逆改点：整份配置共用一个凭据、未声明的槽位被顺带写新版本、旧版本不标 superseded
// → 对应断言红。
// TestMCPCredential_PerKeyVersions covers B2: each slot versions independently.
func TestMCPCredential_PerKeyVersions(t *testing.T) {
	env := newMCPCredentialEnv(t)
	const name = "per-key"
	envSlots := map[string]string{
		mcpCredAlphaEnv: mcpPlaceholder(mcpCredAlphaEnv),
		mcpCredBetaEnv:  mcpPlaceholder(mcpCredBetaEnv),
	}

	_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      name,
		Transport: "stdio",
		Command:   "allowed-mcp",
		Env:       envSlots,
		Credentials: []MCPCredentialRequest{
			mcpManagedValue(mcpCredAlphaEnv, mcpCredAlphaOne),
			mcpManagedValue(mcpCredBetaEnv, mcpCredBetaOne),
		},
		Create: true,
	})
	require.NoError(t, err)
	alpha := env.identity(t, name, mcpCredAlphaEnv)
	beta := env.identity(t, name, mcpCredBetaEnv)
	require.NotEqual(t, alpha.ID, beta.ID, "同一 server 的两个槽位是两条凭据身份")
	require.Equal(t, 1, alpha.CurrentVersion)
	require.Equal(t, 1, beta.CurrentVersion)

	// 只替换 alpha：beta 既没有声明也没有给值，必须原样沿用。
	_, err = env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:        name,
		Transport:   "stdio",
		Command:     "allowed-mcp",
		Env:         envSlots,
		Credentials: []MCPCredentialRequest{mcpManagedValue(mcpCredAlphaEnv, mcpCredAlphaTwo)},
		IfMatch:     env.currentETagOf(t, core.ResourceMCPServer, name),
	})
	require.NoError(t, err)

	alphaAfter := env.identity(t, name, mcpCredAlphaEnv)
	betaAfter := env.identity(t, name, mcpCredBetaEnv)
	require.Equal(t, alpha.ID, alphaAfter.ID, "同一槽位的替换是追加版本，不是新身份")
	require.Equal(t, 2, alphaAfter.CurrentVersion, "被替换的键 version_no 递增")
	require.Equal(t, beta.ID, betaAfter.ID)
	require.Equal(t, 1, betaAfter.CurrentVersion, "未声明的键不得被顺带写新版本")
	require.Equal(t, 2, env.versionCount(t, alphaAfter.ID))
	require.Equal(t, 1, env.versionCount(t, betaAfter.ID))

	require.Equal(t, core.CredentialVersionSuperseded, env.version(t, alphaAfter.ID, 1).State,
		"旧版本被新版本替换")
	require.Equal(t, core.CredentialVersionActive, env.version(t, alphaAfter.ID, 2).State)
	require.NotEqual(t, env.version(t, alphaAfter.ID, 1).Ciphertext,
		env.version(t, alphaAfter.ID, 2).Ciphertext, "每个版本是独立密文")

	values := env.resolve(t, name)
	require.Equal(t, mcpCredAlphaTwo, values[mcpCredAlphaEnv])
	require.Equal(t, mcpCredBetaOne, values[mcpCredBetaEnv], "未被替换的键仍解出旧值")
}

// TestMCPCredential_RevokeNoEnvFallback 覆盖 B3：撤销立即生效、不回落进程环境、
// 密文置 NULL；同名变量在进程环境里有值也救不回来。
// 逆改点：撤销只在下次保存生效、解析发现托管不可用时退回 env、撤销不清密文
// → 对应断言红（回落时会解出 mcpCredEnvTrap）。
// TestMCPCredential_RevokeNoEnvFallback covers B3: revoke is immediate and never
// falls back to the process environment.
func TestMCPCredential_RevokeNoEnvFallback(t *testing.T) {
	env := newMCPCredentialEnv(t)
	const name = "revoked"
	// 冒烟前提：同一个变量名在进程环境里是**有值**的。若实现回落 env，本用例会解出
	// 哨兵值而不是报错——这正是要拦的那种"撤了还在用"。
	t.Setenv(mcpCredAlphaEnv, mcpCredEnvTrap)

	_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:        name,
		Transport:   "stdio",
		Command:     "allowed-mcp",
		Env:         map[string]string{mcpCredAlphaEnv: mcpPlaceholder(mcpCredAlphaEnv)},
		Credentials: []MCPCredentialRequest{mcpManagedValue(mcpCredAlphaEnv, mcpCredAlphaOne)},
		Create:      true,
	})
	require.NoError(t, err)
	require.Equal(t, mcpCredAlphaOne, env.resolve(t, name)[mcpCredAlphaEnv])

	identity := env.identity(t, name, mcpCredAlphaEnv)
	require.Equal(t, 0, identity.RevocationEpoch)
	result := env.revoke(t, name, mcpCredAlphaEnv)
	require.True(t, result.Changed)
	require.Equal(t, identity.ID, result.CredentialID)
	require.Equal(t, 1, result.RevocationEpoch)

	// 撤销立即生效：声明仍是 managed，解析必然失败——不回落进程环境。
	values, err := env.resolveErr(t, name)
	require.ErrorIs(t, err, core.ErrCredentialRevoked)
	require.Nil(t, values)
	require.Contains(t, err.Error(), name)
	require.Contains(t, err.Error(), mcpCredAlphaEnv)
	require.NotContains(t, err.Error(), mcpCredEnvTrap, "错误文案不得带出进程环境的值")

	// 身份与全部版本都进终态，密文被清空。
	revoked := env.identity(t, name, mcpCredAlphaEnv)
	require.Equal(t, core.CredentialIdentityRevoked, revoked.State)
	require.Equal(t, 1, revoked.RevocationEpoch)
	stored := env.version(t, identity.ID, 1)
	require.Equal(t, core.CredentialVersionRevoked, stored.State)
	require.Nil(t, stored.Ciphertext, "撤销把密文置 NULL（不可再解密）")

	require.Equal(t, core.MCPCredentialManagedRevoked, env.statuses(t, name)[0].State,
		"视图必须把撤销与缺失分开")
}

// TestMCPCredential_EditKeepsManagedValues 覆盖 B4：普通编辑只改定义层，已托管的槽位
// 既不丢值也不追加版本（省略声明与"声明 managed 但不给值"两种写法都算编辑）。
// 逆改点：保存时把未声明槽位的托管值清掉、或每次都追加一个空版本 → 对应断言红。
// TestMCPCredential_EditKeepsManagedValues covers B4: an ordinary edit keeps the
// managed material and its version number.
func TestMCPCredential_EditKeepsManagedValues(t *testing.T) {
	env := newMCPCredentialEnv(t)
	const name = "edited"
	t.Setenv(mcpCredBetaEnv, mcpCredBetaOne)

	_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:        name,
		Transport:   "stdio",
		Command:     "allowed-mcp",
		Args:        []string{"--root", "./workspace"},
		Env:         map[string]string{mcpCredAlphaEnv: mcpPlaceholder(mcpCredAlphaEnv)},
		Credentials: []MCPCredentialRequest{mcpManagedValue(mcpCredAlphaEnv, mcpCredAlphaOne)},
		Create:      true,
	})
	require.NoError(t, err)
	alpha := env.identity(t, name, mcpCredAlphaEnv)

	// 第一次编辑：换 command 与 args，并新增一个只读进程环境的槽位；alpha 完全省略。
	_, err = env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      name,
		Transport: "stdio",
		Command:   "other-mcp",
		Args:      []string{"--root", "./elsewhere"},
		Env: map[string]string{
			mcpCredAlphaEnv: mcpPlaceholder(mcpCredAlphaEnv),
			mcpCredBetaEnv:  mcpPlaceholder(mcpCredBetaEnv),
		},
		Credentials: []MCPCredentialRequest{mcpEnvSource(mcpCredBetaEnv)},
		IfMatch:     env.currentETagOf(t, core.ResourceMCPServer, name),
	})
	require.NoError(t, err)
	require.Equal(t, 1, env.identity(t, name, mcpCredAlphaEnv).CurrentVersion,
		"省略声明的槽位不得被追加版本")
	require.Equal(t, 1, env.versionCount(t, alpha.ID))
	alphaValues := env.resolve(t, name)
	require.Equal(t, mcpCredAlphaOne, alphaValues[mcpCredAlphaEnv], "编辑不得清掉托管值")
	require.Equal(t, mcpCredBetaOne, alphaValues[mcpCredBetaEnv], "env 槽位按进程环境解析")

	// 第二次编辑：显式声明 managed、但不给值——同样是"沿用"，不是新版本。
	_, err = env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      name,
		Transport: "stdio",
		Command:   "other-mcp",
		Args:      []string{"--root", "./elsewhere"},
		Env: map[string]string{
			mcpCredAlphaEnv: mcpPlaceholder(mcpCredAlphaEnv),
			mcpCredBetaEnv:  mcpPlaceholder(mcpCredBetaEnv),
		},
		Credentials: []MCPCredentialRequest{mcpManagedKeep(mcpCredAlphaEnv), mcpEnvSource(mcpCredBetaEnv)},
		IfMatch:     env.currentETagOf(t, core.ResourceMCPServer, name),
	})
	require.NoError(t, err)
	require.Equal(t, alpha.ID, env.identity(t, name, mcpCredAlphaEnv).ID)
	require.Equal(t, 1, env.identity(t, name, mcpCredAlphaEnv).CurrentVersion)
	require.Equal(t, 1, env.versionCount(t, alpha.ID))
	require.Equal(t, mcpCredAlphaOne, env.resolve(t, name)[mcpCredAlphaEnv])

	statuses := env.statuses(t, name)
	require.Equal(t, []MCPCredentialStatus{
		{
			Key: mcpCredAlphaEnv, Mode: core.CredentialModeManaged, State: core.MCPCredentialManaged,
			Last4: mcpCredAlphaOne[len(mcpCredAlphaOne)-4:],
		},
		{Key: mcpCredBetaEnv, Mode: core.CredentialModeEnv, State: core.MCPCredentialEnv},
	}, statuses)
}

// TestMCPCredential_HeaderEnvGate 覆盖 B5：请求头槽位与 env 槽位共用同一道闸门。
// 三条分支——env 缺失（env_missing）、声明托管却没有值（credential_unavailable）、
// 修改路径上新增缺值头——都必须 503 + operation_id，且定义文件 / 版本 / 指针零变化。
// 逆改点：闸门只查 env 不查 headers、先写文件再校验、503 不带 operation_id、把两种
// 分类合成一个 → 对应断言红。
// TestMCPCredential_HeaderEnvGate covers B5: header slots pass through the same gate.
func TestMCPCredential_HeaderEnvGate(t *testing.T) {
	t.Run("env 缺失拒绝创建且零写入", func(t *testing.T) {
		env := newMCPCredentialEnv(t)
		const name = "remote-absent"
		t.Setenv(mcpCredMissingEnv, "")
		rowsBefore := env.operationCount()

		_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
			Name:      name,
			Transport: "http",
			URL:       "https://mcp.example.test/v1",
			Headers:   map[string]string{mcpCredKeyHeader: mcpPlaceholder(mcpCredMissingEnv)},
			Credentials: []MCPCredentialRequest{
				mcpEnvSource(mcpCredKeyHeader),
			},
			Create: true,
		})
		require.ErrorIs(t, err, core.ErrProviderNotConfigured)
		require.Contains(t, err.Error(), name)
		require.Contains(t, err.Error(), mcpCredKeyHeader, "错误点名槽位")
		require.Contains(t, err.Error(), mcpCredMissingEnv, "错误点名变量名")

		operationID, ok := core.AdminErrorData(err)["operation_id"].(string)
		require.True(t, ok, "503 必须携带 operation_id")
		require.NotEmpty(t, operationID)
		detail := mcpCredentialGateDetail(t, env.resourceEnv, operationID, mcpGateClassEnvMissing)
		require.Contains(t, detail.ErrorMessage, mcpCredMissingEnv)

		require.Equal(t, rowsBefore+1, env.operationCount(), "闸门失败只留一条 failed 操作行")
		_, statErr := os.Stat(filepath.Join(env.root, mcpServersFileName))
		require.ErrorIs(t, statErr, fs.ErrNotExist, "闸门失败不得创建定义文件")
		_, pointerErr := env.store.Pointer(context.Background(), core.ResourceMCPServer, name)
		require.ErrorIs(t, pointerErr, core.ErrResourceNotFound)
		require.Empty(t, env.versionsOfKind(core.ResourceMCPServer, name))
	})

	t.Run("声明托管但没有值同样是 503", func(t *testing.T) {
		env := newMCPCredentialEnv(t)
		const name = "remote-managed-empty"
		rowsBefore := env.operationCount()

		_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
			Name:        name,
			Transport:   "http",
			URL:         "https://mcp.example.test/v1",
			Headers:     map[string]string{mcpCredKeyHeader: mcpPlaceholder(mcpCredKeyVar)},
			Credentials: []MCPCredentialRequest{mcpManagedKeep(mcpCredKeyHeader)},
			Create:      true,
		})
		require.ErrorIs(t, err, core.ErrProviderNotConfigured)
		operationID, ok := core.AdminErrorData(err)["operation_id"].(string)
		require.True(t, ok)
		// 托管不可用是另一个分类：报成 env 缺失会把排障引向查环境变量。
		mcpCredentialGateDetail(t, env.resourceEnv, operationID, mcpGateClassCredentialUnavailable)

		require.Equal(t, rowsBefore+1, env.operationCount())
		_, statErr := os.Stat(filepath.Join(env.root, mcpServersFileName))
		require.ErrorIs(t, statErr, fs.ErrNotExist)
	})

	t.Run("修改路径上新增缺值头不改变文件", func(t *testing.T) {
		env := newMCPCredentialEnv(t)
		const name = "remote-edit"
		t.Setenv(mcpCredMissingEnv, "")

		_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
			Name:      name,
			Transport: "http",
			URL:       "https://mcp.example.test/v1",
			Headers:   map[string]string{mcpCredKeyHeader: mcpPlaceholder(mcpCredKeyVar)},
			Credentials: []MCPCredentialRequest{
				mcpManagedValue(mcpCredKeyHeader, mcpCredHeaderToken),
			},
			Create: true,
		})
		require.NoError(t, err)
		before := env.readMCPConfig()
		versionsBefore := len(env.versionsOfKind(core.ResourceMCPServer, name))
		rowsBefore := env.operationCount()

		_, err = env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
			Name:      name,
			Transport: "http",
			URL:       "https://mcp.example.test/v1",
			Headers: map[string]string{
				mcpCredKeyHeader:  mcpPlaceholder(mcpCredKeyVar),
				mcpCredAuthHeader: mcpPlaceholder(mcpCredMissingEnv),
			},
			Credentials: []MCPCredentialRequest{
				mcpManagedKeep(mcpCredKeyHeader),
				mcpEnvSource(mcpCredAuthHeader),
			},
			IfMatch: env.currentETagOf(t, core.ResourceMCPServer, name),
		})
		require.ErrorIs(t, err, core.ErrProviderNotConfigured)
		operationID, ok := core.AdminErrorData(err)["operation_id"].(string)
		require.True(t, ok)
		mcpCredentialGateDetail(t, env.resourceEnv, operationID, mcpGateClassEnvMissing)

		require.Equal(t, before, env.readMCPConfig(), "闸门失败不得改动定义文件")
		require.Equal(t, versionsBefore, len(env.versionsOfKind(core.ResourceMCPServer, name)))
		require.Equal(t, rowsBefore+1, env.operationCount())
	})
}

// TestMCPCredential_ArgsPlaceholderRejected 覆盖 B6：args 不解析占位。新增 / 修改出现
// `${` 一律 400 并点名第几个参数（不回显内容、零操作行、零写入）；存量条目仍可加载，
// 由诊断持续报 `args_placeholders_not_resolved`。
// 逆改点：放行 args 占位、错误里回显参数内容、400 之前就写了操作行或文件 → 对应断言红。
// TestMCPCredential_ArgsPlaceholderRejected covers B6: ${ in args is rejected on new and
// edited entries, while legacy entries still load and stay diagnosed.
func TestMCPCredential_ArgsPlaceholderRejected(t *testing.T) {
	env := newMCPCredentialEnv(t)
	const name = "argv-create"
	rowsBefore := env.operationCount()

	_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      name,
		Transport: "stdio",
		Command:   "allowed-mcp",
		// 第 3 个参数是凭证开关，第 4 个参数是占位值——4.5.4 的判据是"参数不解析占位"。
		Args:   []string{"--root", "./work", "--token", mcpPlaceholder(mcpCredAlphaEnv)},
		Create: true,
	})
	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, err.Error(), "args[3]", "错误必须点名第几个参数")
	require.NotContains(t, err.Error(), mcpCredAlphaEnv, "错误不得回显参数内容")
	require.Equal(t, rowsBefore, env.operationCount(), "400 在预写之前：零操作行")
	_, statErr := os.Stat(filepath.Join(env.root, mcpServersFileName))
	require.ErrorIs(t, statErr, fs.ErrNotExist)
	require.Empty(t, env.versionsOfKind(core.ResourceMCPServer, name))

	// 修改路径同样拒绝，且文件与版本零变化。
	const edited = "argv-edit"
	env.createMCPStdio(t, edited, "allowed-mcp")
	before := env.readMCPConfig()
	versionsBefore := len(env.versionsOfKind(core.ResourceMCPServer, edited))
	rowsBefore = env.operationCount()

	_, err = env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      edited,
		Transport: "stdio",
		Command:   "allowed-mcp",
		Args:      []string{"--path=" + mcpPlaceholder(mcpCredAlphaEnv)},
		Env:       map[string]string{"MCP_TEST_TOKEN": mcpPlaceholder("MCP_TEST_TOKEN")},
		IfMatch:   env.currentETagOf(t, core.ResourceMCPServer, edited),
	})
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, err.Error(), "args[0]")
	require.NotContains(t, err.Error(), mcpCredAlphaEnv)
	require.Equal(t, before, env.readMCPConfig())
	require.Equal(t, versionsBefore, len(env.versionsOfKind(core.ResourceMCPServer, edited)))
	require.Equal(t, rowsBefore, env.operationCount())

	// 存量条目：文件里带 args 占位仍可加载（读者宽容），诊断负责把它标出来。
	legacyRoot := t.TempDir()
	legacy := "servers:\n" +
		"  - name: legacy\n" +
		"    transport: stdio\n" +
		"    command: allowed-mcp\n" +
		"    args:\n" +
		"      - --root\n" +
		"      - ${MCP_CRED_LEGACY_ROOT}\n"
	require.NoError(t, os.WriteFile(filepath.Join(legacyRoot, mcpServersFileName), []byte(legacy), 0o600))
	parsed, err := workspace.ParseMCPServers(legacyRoot)
	require.NoError(t, err, "存量条目仍可加载")
	require.Len(t, parsed, 1)
	require.Contains(t, mcpDiagnosisWarnings(parsed[0], mcpTransportStdio),
		mcpDiagnosisWarningCodes[mcpDiagWarnArgsPlaceholders],
		"存量 args 占位必须由诊断持续点名")
}

// TestMCPCredential_SourceExplicit 覆盖 B7：来源显式。managed 取不到值（没有托管值 /
// 密文不可读）时立即失败，**不**读进程环境；env 只读进程环境，不碰托管库。三条分支
// 都在同名变量有值时判卷——回落一旦发生就会解出哨兵值。
// 逆改点：托管 miss 时退回 os.LookupEnv、env 槽位去查凭据库、错误里带出环境值
// → 对应断言红。
// TestMCPCredential_SourceExplicit covers B7: a managed miss never reads the process
// environment, and an env slot never reads the managed store.
func TestMCPCredential_SourceExplicit(t *testing.T) {
	t.Run("声明托管但没有托管值不读进程环境", func(t *testing.T) {
		env := newMCPCredentialEnv(t)
		const name = "declared-managed"
		t.Setenv(mcpCredAlphaEnv, mcpCredEnvTrap)

		// 保存路径不会产出"声明托管却没有托管值"这一态（那份配置会被闸门以 503 拒绝），
		// 因此先用 env 来源建条目，再把来源记录改成 managed——这正是手工改库或未来
		// 迁移可能留下的存量状态；解析器必须把它判成 missing 而不是回落 env。
		_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
			Name:        name,
			Transport:   "stdio",
			Command:     "allowed-mcp",
			Env:         map[string]string{mcpCredAlphaEnv: mcpPlaceholder(mcpCredAlphaEnv)},
			Credentials: []MCPCredentialRequest{mcpEnvSource(mcpCredAlphaEnv)},
			Create:      true,
		})
		require.NoError(t, err)
		require.NoError(t, env.sources.Upsert(context.Background(), core.MCPCredentialSource{
			ServerUID:   env.uid(t, name),
			BindingKey:  mcpCredAlphaEnv,
			Mode:        core.CredentialModeManaged,
			OperationID: core.NewOperationID(baselineClock()),
		}, baselineClock()))

		values, resolveErr := env.resolveErr(t, name)
		require.ErrorIs(t, resolveErr, core.ErrCredentialUnavailable)
		require.Nil(t, values)
		require.Contains(t, resolveErr.Error(), mcpCredAlphaEnv)
		require.NotContains(t, resolveErr.Error(), mcpCredEnvTrap, "托管 miss 不得回落进程环境")
		require.Equal(t, core.MCPCredentialMissing, env.statuses(t, name)[0].State,
			"声明托管但无值 = missing，不是 env")
	})

	t.Run("密文不可读不读进程环境", func(t *testing.T) {
		env := newMCPCredentialEnv(t)
		const name = "tampered"
		t.Setenv(mcpCredAlphaEnv, mcpCredEnvTrap)

		_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
			Name:        name,
			Transport:   "stdio",
			Command:     "allowed-mcp",
			Env:         map[string]string{mcpCredAlphaEnv: mcpPlaceholder(mcpCredAlphaEnv)},
			Credentials: []MCPCredentialRequest{mcpManagedValue(mcpCredAlphaEnv, mcpCredAlphaOne)},
			Create:      true,
		})
		require.NoError(t, err)
		identity := env.identity(t, name, mcpCredAlphaEnv)

		// 直接改坏密文（模拟磁盘损坏 / 跨主密钥的残留版本）：GCM 打开必然失败。
		db, err := sql.Open("sqlite", filepath.ToSlash(env.dbPath))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		_, err = db.ExecContext(context.Background(),
			`UPDATE credential_versions SET ciphertext = ? WHERE credential_id = ? AND version_no = 1`,
			[]byte("not-a-valid-ciphertext"), identity.ID)
		require.NoError(t, err)

		values, resolveErr := env.resolveErr(t, name)
		require.ErrorIs(t, resolveErr, core.ErrCredentialUnavailable)
		require.Nil(t, values)
		require.NotContains(t, resolveErr.Error(), mcpCredEnvTrap, "解密失败不得回落进程环境")
		require.NotContains(t, resolveErr.Error(), mcpCredAlphaOne)
		require.Equal(t, core.MCPCredentialManaged, env.statuses(t, name)[0].State,
			"身份仍 active：视图不能说成 env 或 missing")
	})

	t.Run("env 槽位只读进程环境", func(t *testing.T) {
		env := newMCPCredentialEnv(t)
		const name = "env-only"
		t.Setenv(mcpCredAlphaEnv, mcpCredEnvTrap)

		_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
			Name:        name,
			Transport:   "stdio",
			Command:     "allowed-mcp",
			Env:         map[string]string{mcpCredAlphaEnv: mcpPlaceholder(mcpCredAlphaEnv)},
			Credentials: []MCPCredentialRequest{mcpEnvSource(mcpCredAlphaEnv)},
			Create:      true,
		})
		require.NoError(t, err)
		require.Equal(t, mcpCredEnvTrap, env.resolve(t, name)[mcpCredAlphaEnv])
		require.True(t, env.credentialRowHasNoIdentity(t, name, mcpCredAlphaEnv),
			"env 来源不写托管身份")

		// 变量变空即失败：没有托管值可以顶上。
		t.Setenv(mcpCredAlphaEnv, "")
		values, resolveErr := env.resolveErr(t, name)
		require.ErrorIs(t, resolveErr, core.ErrCredentialUnavailable)
		require.Nil(t, values)
		require.Contains(t, resolveErr.Error(), mcpCredAlphaEnv)
	})
}

// mcpCredentialHTTPServer 起一个真实的 streamable HTTP MCP server，并把每个请求的请求头
// 抄一份留下：判卷看的是**线上到达的头**，不是我们塞进配置的值。
// mcpCredentialHTTPServer starts a real streamable HTTP MCP server and records the
// headers of every request that actually arrives.
func mcpCredentialHTTPServer(t *testing.T) (string, func() []http.Header) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "basalt-us14-credential-server", Version: "v1"}, nil)
	for _, standard := range testutil.StandardMCPTools() {
		server.AddTool(&mcp.Tool{
			Name:        standard.Name,
			Description: standard.Description,
			InputSchema: standard.Schema,
		}, standard.Handler)
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	var mu sync.Mutex
	recorded := make([]http.Header, 0, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		recorded = append(recorded, r.Header.Clone())
		mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []http.Header {
		mu.Lock()
		defer mu.Unlock()
		return append([]http.Header(nil), recorded...)
	}
}

// TestMCPCredential_BearerAssembly 覆盖 B8：认证头装配只认头名——`Authorization` 加
// `Bearer ` 前缀（值里已经带 `Bearer` 就再叠一层，不推断、不去重），其它头原样；
// 3xx 不被跟随，凭据不会走到另一台主机；头值不进日志、审计与已发布定义。
// 逆改点：按值猜前缀 / 去重、给所有头都加前缀、跟随重定向、把头值写进日志或定义
// → 对应断言红。
// TestMCPCredential_BearerAssembly covers B8: the wire headers are assembled by name
// only, and a cross-host redirect never carries them.
func TestMCPCredential_BearerAssembly(t *testing.T) {
	env := newMCPCredentialEnv(t)
	url, seen := mcpCredentialHTTPServer(t)
	const name = "remote-auth"
	headers := map[string]string{
		mcpCredAuthHeader: mcpPlaceholder(mcpCredAuthVar),
		mcpCredKeyHeader:  mcpPlaceholder(mcpCredKeyVar),
	}

	_, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      name,
		Transport: "http",
		URL:       url,
		Headers:   headers,
		Credentials: []MCPCredentialRequest{
			mcpManagedValue(mcpCredAuthHeader, mcpCredNestedBearer),
			mcpManagedValue(mcpCredKeyHeader, mcpCredHeaderToken),
		},
		Create: true,
	})
	require.NoError(t, err)

	// 头值只以占位符落盘，也不出现在日志与操作台账里。
	require.NotContains(t, env.readMCPConfig(), mcpCredHeaderToken)
	require.NotContains(t, env.readMCPConfig(), mcpCredNestedBearer)
	require.NotContains(t, env.logs.String(), mcpCredHeaderToken)
	require.NotContains(t, env.logs.String(), mcpCredNestedBearer)
	for _, row := range env.listOperations(core.OperationFilter{Action: core.ActionMCPServerCreate}) {
		require.NotContains(t, string(row.DetailJSON), mcpCredHeaderToken)
		require.NotContains(t, row.RequestHash, mcpCredHeaderToken)
	}

	cfg := env.publishedConfig(t, name)
	require.Equal(t, headers, cfg.Headers, "已发布条目保留的是占位符，不是值")
	resolved := ApplyResolvedMCP(cfg, env.resolve(t, name))
	require.Equal(t, mcpCredHeaderToken, resolved.Headers[mcpCredKeyHeader])
	require.Equal(t, mcpCredNestedBearer, resolved.Headers[mcpCredAuthHeader])

	pool := tool.NewMCPPool(tool.MCPPoolOptions{
		Logger:         env.logger,
		ConnectTimeout: 5 * time.Second,
		Version:        "us14-credential-test",
	})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	conns, failures, err := pool.ConnectDetailed(context.Background(),
		[]core.MCPServerConfig{resolved})
	require.NoError(t, err)
	require.Empty(t, failures, "带认证头的 http 条目必须连上")
	require.Len(t, conns, 1)
	require.Len(t, conns[0].Tools, 3, "标准工具集三件全部列出")

	requests := seen()
	require.NotEmpty(t, requests, "判卷看线上到达的头：至少要有一次真实请求")
	for _, header := range requests {
		require.Equal(t, "Bearer "+mcpCredNestedBearer, header.Get(mcpCredAuthHeader),
			"Authorization 一律加 Bearer 前缀——值本身已带 Bearer 就再叠一层，不推断、不去重")
		require.Equal(t, mcpCredHeaderToken, header.Get(mcpCredKeyHeader), "其它头原样")
	}
	// 跨主机重定向：目标主机一次请求都收不到，凭据不会被 3xx 带出去。
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	redirected := resolved
	redirected.Name = name + "-redirect"
	redirected.URL = redirector.URL
	redirectConns, redirectFailures, err := pool.ConnectDetailed(context.Background(),
		[]core.MCPServerConfig{redirected})
	require.NoError(t, err)
	require.Empty(t, redirectConns, "3xx 不被跟随：连接按失败处理")
	require.Len(t, redirectFailures, 1, "失败按 server 吸收成一条结构化记录")
	require.Zero(t, targetHits.Load(), "凭据不得随重定向走到另一个主机")

	// 这次失败让池真的写了 WARN（失败路径会带上错误摘要）：整段扫描才有对象，
	// 免得"日志不含头值"变成对空缓冲的空断言。
	require.NotEmpty(t, env.logs.String(), "连接失败必须留一条 WARN")
	require.NotContains(t, env.logs.String(), mcpCredHeaderToken)
	require.NotContains(t, env.logs.String(), mcpCredNestedBearer)
}
