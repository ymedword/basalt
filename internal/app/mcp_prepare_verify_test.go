package app

// 本文件是 US-25C §2.3.8「提交复核分支表」的离线判据（2026-11-10 复核 P1 的组件面）：
// `newMCPPrepareVerify` 的每条分类都必须可判——指针行缺失 / 软删、applied 与目标不符
// （分 publish 与 apply）、目标版本行缺失、定义非法、凭据不可得 / 换代 / 撤销，以及
// 全部相符的 None。凭据冻结必须读**原文定义**（`${VAR}` 槽位），不能读已解析的
// `published.Config`，否则代次恒为空而与目标代次不符。
//
// 生产装配面的判据（首次连接 / 版本更新 / 无变化复用）由
// `wire_mcp_worker_integration_test.go` 承担；本文件只判分类逻辑本身。
// Offline judges for the pre-swap recheck branch table; the production assembly path is
// covered by the worker integration test.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// mcpVerifyServer 是复核夹具的 server 名（与版本行 / 指针行逐字一致）。
	mcpVerifyServer = "alpha-mcp"
	// mcpVerifySlot 是定义里唯一的凭据槽位名（env 键同时也是 `${VAR}` 引用名）。
	mcpVerifySlot = "MCP_ALPHA_TOKEN"
	// mcpVerifyRawDefinition 是**原文**定义：槽位值保持 `${VAR}`，未解析。
	mcpVerifyRawDefinition = "name: alpha-mcp\ntransport: stdio\ncommand: /usr/bin/verify-fixture-mcp\nenv:\n  MCP_ALPHA_TOKEN: ${MCP_ALPHA_TOKEN}\n"
)

// mcpVerifyClock 是固定时间：软删时间可复算。
func mcpVerifyClock() time.Time {
	return time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
}

// verifyStoreFake 只实现复核读取的两个方法（消费方接口在 mcp.go:511）。
type verifyStoreFake struct {
	pointer    core.ResourcePointer
	pointerErr error
	versions   map[int64]core.ResourceVersion
	versionErr error
}

func (f *verifyStoreFake) Pointer(context.Context, core.ResourceKind, string) (core.ResourcePointer, error) {
	if f.pointerErr != nil {
		return core.ResourcePointer{}, f.pointerErr
	}
	return f.pointer, nil
}

func (f *verifyStoreFake) VersionByID(_ context.Context, id int64) (core.ResourceVersion, error) {
	if f.versionErr != nil {
		return core.ResourceVersion{}, f.versionErr
	}
	version, ok := f.versions[id]
	if !ok {
		return core.ResourceVersion{}, core.ErrResourceNotFound
	}
	return version, nil
}

// verifyCredentialFake 记录复核真正读到的输入：冻结的槽位原文与判撤销的调用。
type verifyCredentialFake struct {
	frozen       []mcpCredentialFreeze
	freezeErr    error
	still        bool
	stillErr     error
	frozenName   string
	frozenUID    core.ResourceUID
	frozenConfig core.MCPServerConfig
	freezeCalls  int
	stillCalls   int
}

func (f *verifyCredentialFake) Resolve(context.Context, string, core.ResourceUID, core.MCPServerConfig) (map[string]string, error) {
	return nil, errors.New("复核不得解析凭据（只读冻结）")
}

func (f *verifyCredentialFake) BackfillEnvSources(context.Context, string, core.ResourceUID,
	core.MCPServerConfig, string, time.Time,
) (core.MCPBackfillReport, error) {
	return core.MCPBackfillReport{}, nil
}

func (f *verifyCredentialFake) Freeze(_ context.Context, name string, uid core.ResourceUID,
	cfg core.MCPServerConfig,
) ([]mcpCredentialFreeze, error) {
	f.freezeCalls++
	f.frozenName = name
	f.frozenUID = uid
	f.frozenConfig = cfg
	return f.frozen, f.freezeErr
}

func (f *verifyCredentialFake) StillCurrent(context.Context, core.ResourceUID, []mcpCredentialFreeze) (bool, error) {
	f.stillCalls++
	return f.still, f.stillErr
}

// mcpVerifyFixture 是一份"全部相符"的基线：各用例只改自己关心的那一项。
type mcpVerifyFixture struct {
	uid       core.ResourceUID
	store     *verifyStoreFake
	creds     *verifyCredentialFake
	published core.PublishedMCPServer
	verify    func(context.Context, core.PublishedMCPServer) (core.MCPApplySupersededBy, error)
}

// mcpVerifyInt64 取整型指针（applied 版本可空，用指针表达）。
func mcpVerifyInt64(value int64) *int64 { return &value }

func newMCPVerifyFixture(t *testing.T) *mcpVerifyFixture {
	t.Helper()
	uid := core.ResourceUID("rs_verify_fixture")
	frozen := []mcpCredentialFreeze{{
		key: mcpVerifySlot, mode: core.CredentialModeEnv, id: string(uid), versionNo: 1, keyID: "key-1",
	}}
	f := &mcpVerifyFixture{
		uid: uid,
		store: &verifyStoreFake{
			pointer: core.ResourcePointer{
				Kind:               core.ResourceMCPServer,
				ResourceID:         mcpVerifyServer,
				ResourceUID:        uid,
				PublishedVersionID: 5,
				AppliedVersionID:   mcpVerifyInt64(5),
			},
			versions: map[int64]core.ResourceVersion{
				5: {
					ID:             5,
					Kind:           core.ResourceMCPServer,
					ResourceUID:    uid,
					ResourceID:     mcpVerifyServer,
					VersionNo:      1,
					DefinitionYAML: mcpVerifyRawDefinition,
				},
			},
		},
		creds: &verifyCredentialFake{frozen: frozen, still: true},
		published: core.PublishedMCPServer{
			VersionID:            5,
			CredentialGeneration: mcpCredentialGeneration(frozen),
			Config: core.MCPServerConfig{
				Name: mcpVerifyServer, Transport: mcpTransportStdio, Command: "/usr/bin/verify-fixture-mcp",
			},
		},
	}
	f.verify = newMCPPrepareVerify(f.store, f.creds)
	return f
}

// setDefinition 替换目标版本的定义原文（非法定义 / 无槽位用例）。
func (f *mcpVerifyFixture) setDefinition(raw string) {
	version := f.store.versions[5]
	version.DefinitionYAML = raw
	f.store.versions[5] = version
}

// setApplied 改 applied 指针（nil = 未 applied）。
func (f *mcpVerifyFixture) setApplied(id *int64) {
	f.store.pointer.AppliedVersionID = id
}

// TestMCPPrepareVerify_BranchTable 逐支对号模块卡 §2.3.8 的提交复核分支表。
// 失败关闭的判据是"返回 error"，不是"返回 None"——把读失败当通过会让候选带病换入。
func TestMCPPrepareVerify_BranchTable(t *testing.T) {
	t.Parallel()

	deletedAt := mcpVerifyClock().UnixMilli()
	cases := []struct {
		name    string
		mutate  func(*mcpVerifyFixture)
		want    core.MCPApplySupersededBy
		wantErr string
	}{
		{
			name:   "指针行缺失按新的 apply 处理",
			mutate: func(f *mcpVerifyFixture) { f.store.pointerErr = core.ErrResourceNotFound },
			want:   core.MCPApplySupersededByApply,
		},
		{
			name: "指针读失败失败关闭",
			mutate: func(f *mcpVerifyFixture) {
				f.store.pointerErr = errors.New("数据库中断（夹具）")
			},
			wantErr: "读取指针",
		},
		{
			name:   "指针已软删按 apply 处理",
			mutate: func(f *mcpVerifyFixture) { f.store.pointer.DeletedAt = &deletedAt },
			want:   core.MCPApplySupersededByApply,
		},
		{
			name:   "未 applied 且发布仍是目标版本按 apply 处理",
			mutate: func(f *mcpVerifyFixture) { f.setApplied(nil) },
			want:   core.MCPApplySupersededByApply,
		},
		{
			name: "applied 被另一次 apply 抢先按 apply 处理",
			mutate: func(f *mcpVerifyFixture) {
				f.setApplied(mcpVerifyInt64(4))
			},
			want: core.MCPApplySupersededByApply,
		},
		{
			name: "发布已推进到新版本按 publish 处理",
			mutate: func(f *mcpVerifyFixture) {
				f.setApplied(mcpVerifyInt64(4))
				f.store.pointer.PublishedVersionID = 6
			},
			want: core.MCPApplySupersededByPublish,
		},
		{
			name:   "目标版本行缺失按 publish 处理",
			mutate: func(f *mcpVerifyFixture) { delete(f.store.versions, 5) },
			want:   core.MCPApplySupersededByPublish,
		},
		{
			name: "版本行读失败失败关闭",
			mutate: func(f *mcpVerifyFixture) {
				f.store.versionErr = errors.New("数据库中断（夹具）")
			},
			wantErr: "读取目标版本",
		},
		{
			name: "版本行归属不符失败关闭",
			mutate: func(f *mcpVerifyFixture) {
				version := f.store.versions[5]
				version.Kind = core.ResourceModelConnection
				f.store.versions[5] = version
			},
			wantErr: "归属不符",
		},
		{
			name:    "定义原文非法失败关闭",
			mutate:  func(f *mcpVerifyFixture) { f.setDefinition("name: [") },
			wantErr: "定义非法",
		},
		{
			name: "凭据不可得按撤销处理",
			mutate: func(f *mcpVerifyFixture) {
				f.creds.freezeErr = errors.Join(errors.New("冻结凭据"), mcpEnvSlotFailure{err: errors.New("来源已删除")})
			},
			want: core.MCPApplySupersededByRevoke,
		},
		{
			name: "凭据冻结的其他失败失败关闭",
			mutate: func(f *mcpVerifyFixture) {
				f.creds.freezeErr = errors.New("主密钥不可得（夹具）")
			},
			wantErr: "冻结凭据",
		},
		{
			name: "代次不符按撤销处理",
			mutate: func(f *mcpVerifyFixture) {
				f.published.CredentialGeneration = "generation-before-rotation"
			},
			want: core.MCPApplySupersededByRevoke,
		},
		{
			name: "仍有效判定失败失败关闭",
			mutate: func(f *mcpVerifyFixture) {
				f.creds.stillErr = errors.New("读取凭据行失败（夹具）")
			},
			wantErr: "判定凭据撤销",
		},
		{
			name: "凭据在版本号未变时被撤销按撤销处理",
			mutate: func(f *mcpVerifyFixture) {
				f.creds.still = false
			},
			want: core.MCPApplySupersededByRevoke,
		},
		{
			name: "全部相符复用",
			want: core.MCPApplySupersededByNone,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newMCPVerifyFixture(t)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			got, err := f.verify(context.Background(), f.published)
			if tc.wantErr != "" {
				require.Error(t, err, "该分支必须失败关闭")
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestMCPPrepareVerify_FreezesRawDefinition 守"冻结读原文定义"：`published.Config` 在
// 生产里已把 `${VAR}` 解析成明文，在它上面读不到槽位、代次恒为空。复核必须重新读目标
// 版本行的原文再 Freeze，判据是 Freeze 收到的槽位值仍是 `${MCP_ALPHA_TOKEN}`。
// The recheck must freeze the raw definition, not the resolved published config.
func TestMCPPrepareVerify_FreezesRawDefinition(t *testing.T) {
	t.Parallel()

	f := newMCPVerifyFixture(t)
	// 即使装配把已解析的明文塞进 published.Config，Freeze 也只能看到原文占位符。
	f.published.Config.Env = map[string]string{mcpVerifySlot: "resolved-plaintext"}

	got, err := f.verify(context.Background(), f.published)
	require.NoError(t, err)
	require.Equal(t, core.MCPApplySupersededByNone, got)

	require.Equal(t, 1, f.creds.freezeCalls, "复核必须冻结一次凭据代次")
	require.Equal(t, 1, f.creds.stillCalls, "代次相符后必须判一次撤销")
	require.Equal(t, mcpVerifyServer, f.creds.frozenName)
	require.Equal(t, f.uid, f.creds.frozenUID)
	require.Equal(t, "${"+mcpVerifySlot+"}", f.creds.frozenConfig.Env[mcpVerifySlot],
		"Freeze 必须收到未解析的原文定义；收到明文说明读的是 published.Config")
}

// TestMCPPrepareVerify_NilWiringFailsClosed 守"缺接线失败关闭"：缺指针面或凭据面时
// 必须报错，不得静默返回 None 让候选免检换入。
func TestMCPPrepareVerify_NilWiringFailsClosed(t *testing.T) {
	t.Parallel()

	f := newMCPVerifyFixture(t)
	_, err := newMCPPrepareVerify(nil, f.creds)(context.Background(), f.published)
	require.Error(t, err, "缺指针读取面必须失败关闭")

	_, err = newMCPPrepareVerify(f.store, nil)(context.Background(), f.published)
	require.Error(t, err, "缺凭据面必须失败关闭")
}
