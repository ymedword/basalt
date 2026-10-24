package app

// 本文件覆盖绑定投影的重建面与准入校验面（颗粒度文档 R4.6 / T120）：
// 以 `AGENT.md` 全量重建（多余删、缺失补、UID 以文件为准）、归档 Agent 参与、
// 身份无法证明时记 `projection_error` 并清空投影（闸门 fail-closed）、准入期
// 逐条点名缺失 / 非法 / 不可用，以及重建面与失败集合的并发安全。
// These tests cover the projection rebuild and the admission check of resource
// bindings: AGENT.md stays authoritative, unprovable identities fail closed, and
// every admission failure names the binding that broke.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/web"
	"github.com/ymedword/basalt/internal/workspace"
)

// 夹具的固定身份：写死比每次随机更好核对——失败信息里的 UID 能与断言逐字对上。
// 载荷只用 Crockford Base32 数字段（I / L / O / U 是禁用字符，单词里很容易踩到）。
// Fixed fixture identities so failure messages line up with the assertions; the
// payloads use digits only because I/L/O/U are refused by the Crockford rules.
var (
	bindingAgentUID  = core.AgentUID("ag_00000000000000000000000001")
	bindingModelUID  = core.ResourceUID("rs_00000000000000000000000002")
	bindingNotifyUID = core.ResourceUID("rs_00000000000000000000000003")
	bindingOtherUID  = core.ResourceUID("rs_00000000000000000000000004")
	bindingExclUID   = core.ResourceUID("rs_00000000000000000000000005")
)

// stubArchiveLister 是归档记录的只读替身：本组测试不经过真实归档事务，只关心
// "归档记录是否携带身份"这一条输入。
// stubArchiveLister feeds archive rows straight to the projection.
type stubArchiveLister struct {
	records []core.AgentArchiveRecord
	err     error
}

// ListArchives 返回夹具设定的记录（或注入的错误）。
func (s *stubArchiveLister) ListArchives(context.Context) ([]core.AgentArchiveRecord, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.records, nil
}

// stubCredentialCheck 是凭据状态检查的替身：用固定错误模拟已撤销 / 不可用。
// stubCredentialCheck returns a fixed credential error.
type stubCredentialCheck struct {
	err   error
	kinds []core.ResourceKind
}

func (s *stubCredentialCheck) CheckBinding(_ context.Context, kind core.ResourceKind, _ core.ResourceUID) error {
	s.kinds = append(s.kinds, kind)
	return s.err
}

// bindingsEnv 是绑定投影与准入的夹具：临时工作区 + 临时库 + 一条 pending 操作
// （种子版本行的外键）+ reporter 的身份镜像。
// bindingsEnv is the fixture shared by the projection and admission tests.
type bindingsEnv struct {
	root       string
	db         *storage.DB
	resources  *storage.ResourceStore
	bindings   *storage.BindingStore
	notify     *storage.NotifyAdminStore
	archives   *stubArchiveLister
	projection *BindingProjection
	opID       string
	logs       *syncBuffer
}

// newBindingsEnv 建夹具：工作区骨架、v3 库、reporter 的身份镜像与一条 pending 操作。
// newBindingsEnv assembles the fixture; no AGENT.md is written yet.
func newBindingsEnv(t *testing.T) *bindingsEnv {
	t.Helper()
	ctx := context.Background()

	root := filepath.Join(t.TempDir(), workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)

	db, err := storage.OpenWritable(ctx, filepath.Join(root, databaseFileName),
		storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	ops := storage.NewManagementStore(db)
	opID := core.NewOperationID(baselineClock())
	require.NoError(t, ops.BeginOperation(ctx, core.OperationRecord{
		OperationID:  opID,
		Action:       "system.baseline",
		TargetKind:   "system",
		TargetID:     "system",
		ActorSubject: core.ActorSubjectMigration,
		AuthMethod:   core.AuthMethodMigration,
		Source:       "test",
		Status:       core.OperationPending,
		CreatedAt:    baselineClock(),
	}))

	resources := storage.NewResourceStore(db)
	require.NoError(t, resources.PutLifecycleIndex(ctx, core.LifecycleRecord{
		SchemaVersion: 2,
		Agent:         "reporter",
		AgentUID:      bindingAgentUID,
		State:         core.AgentActive,
		Generation:    1,
		UpdatedAt:     baselineClock().UnixMilli(),
		OperationID:   opID,
		Checksum:      "sha256:" + sha256Hex(t, []byte("state")),
	}, true, ""))

	archives := &stubArchiveLister{}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	bindingStore := storage.NewBindingStore(db)

	return &bindingsEnv{
		root:      root,
		db:        db,
		resources: resources,
		bindings:  bindingStore,
		notify:    storage.NewNotifyAdminStore(db),
		archives:  archives,
		projection: NewBindingProjection(bindingStore, resources, archives, root, logger,
			baselineClock),
		opID: opID,
		logs: logs,
	}
}

// writeAgentMD 写一份活动 Agent 定义（`agents/<name>/AGENT.md`）。
// writeAgentMD writes one active agent definition.
func (e *bindingsEnv) writeAgentMD(t *testing.T, name, block string) {
	t.Helper()
	writeWorkspaceFile(t, e.root, "agents/"+name+"/AGENT.md", agentMDWithBindings(name, block))
}

// publishModelVersion 写入并发布一个模型连接版本，返回落库后的版本行。
// publishModelVersion publishes one model connection and returns the stored version.
func (e *bindingsEnv) publishModelVersion(t *testing.T, name string) core.ResourceVersion {
	t.Helper()
	ctx := context.Background()
	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	version, _, err := e.resources.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    uid,
		ResourceID:     name,
		ContentHash:    sha256Hex(t, []byte("definition-"+name)),
		DefinitionYAML: "name: " + name + "\nbase_url: https://api.example.com\n",
		CreatedAt:      baselineClock().UnixMilli(),
		OperationID:    e.opID,
	})
	require.NoError(t, err)
	require.NoError(t, e.resources.Publish(ctx, core.ResourceModelConnection, name, version.ID,
		true, e.opID, baselineClock()))
	return version
}

// seedModelVersion 与 publishModelVersion 相同，但只返回资源身份（= 当前指针身份）。
// seedModelVersion returns the identity of a freshly published model connection.
func (e *bindingsEnv) seedModelVersion(t *testing.T, name string) core.ResourceUID {
	t.Helper()
	return e.publishModelVersion(t, name).ResourceUID
}

// seedNotifyChannel 建一条共享通知渠道，返回其资源身份。
// seedNotifyChannel creates one shared notification channel.
func (e *bindingsEnv) seedNotifyChannel(t *testing.T, name string) core.ResourceUID {
	t.Helper()
	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	require.NoError(t, e.notify.Create(context.Background(), core.NotifyChannel{
		ResourceUID: uid,
		Name:        name,
		Type:        "webhook",
		URL:         "https://example.com/hook",
		URLHost:     "example.com",
		Description: "测试渠道",
		Scope:       core.ScopeShared,
		OperationID: e.opID,
	}))
	return uid
}

// bindingsBlock 渲染一段 `resource_bindings`（空组写 `[]`，与契约同形）。
// bindingsBlock renders a resource_bindings block.
func bindingsBlock(model, notify []core.ResourceBinding) string {
	return "resource_bindings:\n  model_connection:" + renderBindingGroup(model) +
		"  notify_channel:" + renderBindingGroup(notify)
}

// renderBindingGroup 渲染一组绑定条目（空组 = 行内空列表）。
func renderBindingGroup(entries []core.ResourceBinding) string {
	if len(entries) == 0 {
		return " []\n"
	}
	out := "\n"
	for _, entry := range entries {
		out += "    - name: " + entry.Name + "\n      uid: " + string(entry.UID) + "\n"
	}
	return out
}

// agentMDWithBindings 拼一份完整 AGENT.md。
// agentMDWithBindings renders one AGENT.md carrying the given bindings block.
func agentMDWithBindings(name, block string) string {
	return "---\n" +
		"name: " + name + "\n" +
		"provider: {name: alpha, model: alpha-model}\n" +
		"notify_channels: [ops]\n" +
		block +
		"---\n\n正文\n"
}

// syncBuffer 是并发安全的日志缓冲（并发重建会同时写日志）。
// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// TestBindingProjection_RebuildsFromAgentMD —— 全量重建以文件为准：多余的投影行
// 被删掉、缺的补上、UID 取自文件（不是库里的旧值），归档 Agent 一并参与，
// 同一份文件重复重建结果不变。
func TestBindingProjection_RebuildsFromAgentMD(t *testing.T) {
	ctx := context.Background()
	env := newBindingsEnv(t)
	modelUID := env.seedModelVersion(t, "alpha")
	notifyUID := env.seedNotifyChannel(t, "ops")

	// 先写一份**过期**投影：重建必须把它换成文件里的那两条。
	require.NoError(t, env.bindings.Rebuild(ctx, bindingAgentUID, "reporter", "stale-hash",
		core.ResourceBindings{
			ModelConnections: []core.ResourceBinding{{Name: "legacy", UID: bindingOtherUID}},
		}, baselineClock()))

	block := bindingsBlock(
		[]core.ResourceBinding{{Name: "alpha", UID: modelUID}},
		[]core.ResourceBinding{{Name: "ops", UID: notifyUID}},
	)
	env.writeAgentMD(t, "reporter", block)

	// 归档 Agent 同样参与重建：身份来自归档记录，名字只用于人读。
	archivedUID, err := core.NewAgentUID()
	require.NoError(t, err)
	archivedBlock := bindingsBlock([]core.ResourceBinding{{Name: "alpha", UID: modelUID}}, nil)
	writeWorkspaceFile(t, env.root, "archive/reporter-20260101-000000/AGENT.md",
		agentMDWithBindings("reporter", archivedBlock))
	env.archives.records = []core.AgentArchiveRecord{{
		ArchiveID:  "reporter-20260101-000000",
		AgentName:  "reporter",
		AgentUID:   archivedUID,
		ArchivedAt: baselineClock().UnixMilli(),
	}}

	report, err := env.projection.RebuildAll(ctx)
	require.NoError(t, err)
	require.Empty(t, report.Failures)
	require.Equal(t, 2, report.Rebuilt, "活动 + 归档两份定义都要重建")

	activeHash := migrateV3ContentHash([]byte(agentMDWithBindings("reporter", block)))
	rows, err := env.bindings.Bindings(ctx, bindingAgentUID)
	require.NoError(t, err)
	require.Equal(t, []core.AgentResourceBinding{
		{
			AgentUID: bindingAgentUID, AgentName: "reporter", Kind: core.BindingModelConnection,
			RefName: "alpha", ResourceUID: modelUID,
			ContentHash: activeHash, ProjectedAt: baselineClock().UnixMilli(),
		},
		{
			AgentUID: bindingAgentUID, AgentName: "reporter", Kind: core.BindingNotifyChannel,
			RefName: "ops", ResourceUID: notifyUID,
			ContentHash: activeHash, ProjectedAt: baselineClock().UnixMilli(),
		},
	}, rows, "投影必须与文件逐条一致，旧行 legacy 必须消失")

	archivedRows, err := env.bindings.Bindings(ctx, archivedUID)
	require.NoError(t, err)
	require.Len(t, archivedRows, 1)
	require.Equal(t, core.BindingModelConnection, archivedRows[0].Kind)
	require.Equal(t, modelUID, archivedRows[0].ResourceUID)

	// 幂等：同一份文件再跑一次，行数与内容不变。
	again, err := env.projection.RebuildAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, again.Rebuilt)
	againRows, err := env.bindings.Bindings(ctx, bindingAgentUID)
	require.NoError(t, err)
	require.Equal(t, rows, againRows)
}

// TestBindingProjection_ErrorFailsClosed —— 投影不可信时（结构非法 / 身份无法证明）
// 记固定原因并清空该 Agent 的投影：闸门据此回退定义文件扫描，扫描也失败即按
// "可能引用"处理。没有身份的 Agent 不得凭空写出投影行。
func TestBindingProjection_ErrorFailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("invalid_bindings", func(t *testing.T) {
		env := newBindingsEnv(t)
		// 先放一条会被清掉的旧投影：结构非法必须连旧行一起清掉，否则闸门拿着
		// 过期投影继续挡删除，行为与定义文件脱钩。
		require.NoError(t, env.bindings.Rebuild(ctx, bindingAgentUID, "reporter", "stale",
			core.ResourceBindings{
				ModelConnections: []core.ResourceBinding{{Name: "legacy", UID: bindingOtherUID}},
			}, baselineClock()))
		env.writeAgentMD(t, "reporter",
			"resource_bindings:\n  model_connection:\n    - name: alpha\n  notify_channel: []\n")

		report, err := env.projection.RebuildAll(ctx)
		require.NoError(t, err)
		require.Equal(t, []ProjectionFailure{
			{Agent: "reporter", Reason: projectionReasonInvalidBindings},
		}, report.Failures)
		require.Equal(t, 1, report.Cleared)

		reason, failed := env.projection.ProjectionError("reporter")
		require.True(t, failed)
		require.Equal(t, projectionReasonInvalidBindings, reason)

		rows, err := env.bindings.Bindings(ctx, bindingAgentUID)
		require.NoError(t, err)
		require.Empty(t, rows, "结构非法必须清空投影，闸门回退定义文件扫描")
	})

	t.Run("identity_unrecorded", func(t *testing.T) {
		env := newBindingsEnv(t)
		env.writeAgentMD(t, "reporter", bindingsBlock(nil, nil))
		// anonymous 没有身份镜像：宁可记失败，也不按名字猜一个身份写投影。
		writeWorkspaceFile(t, env.root, "agents/anonymous/AGENT.md",
			agentMDWithBindings("anonymous", bindingsBlock(nil, nil)))

		report, err := env.projection.RebuildAll(ctx)
		require.NoError(t, err)
		require.Equal(t, []ProjectionFailure{
			{Agent: "anonymous", Reason: projectionReasonNoIdentity},
		}, report.Failures)
		require.Equal(t, 1, report.Rebuilt, "reporter 仍然正常重建")
		require.Equal(t, map[string]string{"anonymous": projectionReasonNoIdentity},
			env.projection.FailedAgents())
	})

	t.Run("archive_legacy_unrecorded", func(t *testing.T) {
		env := newBindingsEnv(t)
		writeWorkspaceFile(t, env.root, "archive/reporter-20250101-000000/AGENT.md",
			agentMDWithBindings("reporter", bindingsBlock(nil, nil)))
		env.archives.records = []core.AgentArchiveRecord{{
			ArchiveID: "reporter-20250101-000000",
			AgentName: "reporter",
			// AgentUID 为空 = v3 之前的归档：无法证明身份。
			ArchivedAt: baselineClock().UnixMilli(),
		}}

		report, err := env.projection.RebuildAll(ctx)
		require.NoError(t, err)
		require.Equal(t, []ProjectionFailure{
			{Agent: "reporter", Reason: projectionReasonArchiveLegacy},
		}, report.Failures)
	})

	t.Run("rebuild_agent_requires_identity", func(t *testing.T) {
		env := newBindingsEnv(t)
		err := env.projection.RebuildAgent(ctx, "reporter", "", "hash", core.ResourceBindings{})
		require.ErrorIs(t, err, core.ErrAgentResourceBindingInvalid)
	})

	t.Run("archive_listing_failure", func(t *testing.T) {
		env := newBindingsEnv(t)
		env.writeAgentMD(t, "reporter", bindingsBlock(nil, nil))
		env.archives.err = errors.New("磁盘读失败")
		_, err := env.projection.RebuildAll(ctx)
		require.Error(t, err, "归档清单读不到 = 工作区级问题，启动应拒绝")
	})
}

// TestBindingAdmission_NamesMissingBinding —— 准入期逐条点名：缺失 / 孤儿 / UID 不是
// 当前指针 / 未发布 / 范围不符 / 凭据不可用，全部拒绝且错误里带引用名。
func TestBindingAdmission_NamesMissingBinding(t *testing.T) {
	ctx := context.Background()
	env := newBindingsEnv(t)
	modelUID := env.seedModelVersion(t, "alpha")
	notifyUID := env.seedNotifyChannel(t, "ops")
	deps := BindingCheckDeps{Resources: env.resources, Notify: env.notify}

	// profileOf 按声明面构造定义：provider 名与渠道名是**引用**，绑定是配对条目。
	profileOf := func(provider string, channels []string,
		model, notify []core.ResourceBinding,
	) *core.Profile {
		return &core.Profile{
			Name:             "reporter",
			Provider:         core.ProviderRef{Name: provider, Model: "alpha-model"},
			NotifyChannels:   channels,
			ResourceBindings: core.ResourceBindings{ModelConnections: model, NotifyChannels: notify},
		}
	}
	okModel := []core.ResourceBinding{{Name: "alpha", UID: modelUID}}
	okNotify := []core.ResourceBinding{{Name: "ops", UID: notifyUID}}

	t.Run("缺绑定", func(t *testing.T) {
		err := CheckAgentBindings(ctx, deps, "reporter", bindingAgentUID,
			profileOf("alpha", []string{"ops"}, nil, okNotify))
		require.ErrorIs(t, err, core.ErrAgentResourceBindingMissing)
		require.Contains(t, err.Error(), `"alpha"`, "错误必须点名缺哪条绑定")
	})

	t.Run("孤儿绑定", func(t *testing.T) {
		err := CheckAgentBindings(ctx, deps, "reporter", bindingAgentUID,
			profileOf("alpha", []string{"ops"},
				append(okModel, core.ResourceBinding{Name: "ghost", UID: bindingOtherUID}),
				okNotify))
		require.ErrorIs(t, err, core.ErrAgentResourceBindingInvalid)
		require.Contains(t, err.Error(), `"ghost"`)
	})

	t.Run("UID不是当前指针", func(t *testing.T) {
		err := CheckAgentBindings(ctx, deps, "reporter", bindingAgentUID,
			profileOf("alpha", []string{"ops"},
				[]core.ResourceBinding{{Name: "alpha", UID: bindingOtherUID}}, okNotify))
		require.ErrorIs(t, err, core.ErrAgentResourceBindingInvalid)
		var problem BindingProblem
		require.ErrorAs(t, err, &problem)
		require.Equal(t, core.BindingModelConnection, problem.Kind)
		require.Equal(t, "alpha", problem.Name, "同名重建不自动接管：UID 不是当前指针就不放行")
	})

	t.Run("资源未发布", func(t *testing.T) {
		err := CheckAgentBindings(ctx, deps, "reporter", bindingAgentUID,
			profileOf("ghost", []string{"ops"},
				[]core.ResourceBinding{{Name: "ghost", UID: bindingOtherUID}}, okNotify))
		require.ErrorIs(t, err, core.ErrAgentResourceUnavailable)
		var problem BindingProblem
		require.ErrorAs(t, err, &problem)
		require.Equal(t, "ghost", problem.Name)
	})

	t.Run("渠道未发布", func(t *testing.T) {
		err := CheckAgentBindings(ctx, deps, "reporter", bindingAgentUID,
			profileOf("alpha", []string{"ghost"}, okModel,
				[]core.ResourceBinding{{Name: "ghost", UID: notifyUID}}))
		require.ErrorIs(t, err, core.ErrAgentResourceUnavailable)
		var problem BindingProblem
		require.ErrorAs(t, err, &problem)
		require.Equal(t, core.BindingNotifyChannel, problem.Kind)
		require.Equal(t, "ghost", problem.Name)
	})

	t.Run("专属范围只认属主", func(t *testing.T) {
		// 另建一条 exclusive 渠道：正常写路径（Create）支持范围，不靠裸 SQL 造状态。
		otherAgent, err := core.NewAgentUID()
		require.NoError(t, err)
		require.NoError(t, env.notify.Create(ctx, core.NotifyChannel{
			ResourceUID:   bindingExclUID,
			Name:          "exclusive",
			Type:          "webhook",
			URL:           "https://example.com/x",
			URLHost:       "example.com",
			Scope:         core.ScopeExclusive,
			OwnerAgentUID: otherAgent,
			OperationID:   env.opID,
		}))

		exclusive := profileOf("alpha", []string{"exclusive"}, okModel,
			[]core.ResourceBinding{{Name: "exclusive", UID: bindingExclUID}})
		err = CheckAgentBindings(ctx, deps, "reporter", bindingAgentUID, exclusive)
		require.ErrorIs(t, err, core.ErrResourceNotEligible)

		// 属主本人可用。
		require.NoError(t, CheckAgentBindings(ctx, deps, "reporter", otherAgent, exclusive))
	})

	t.Run("凭据不可用", func(t *testing.T) {
		cred := &stubCredentialCheck{err: core.ErrCredentialRevoked}
		err := CheckAgentBindings(ctx, BindingCheckDeps{
			Resources: env.resources, Notify: env.notify, Credentials: cred,
		}, "reporter", bindingAgentUID, profileOf("alpha", []string{"ops"}, okModel, okNotify))
		require.ErrorIs(t, err, core.ErrCredentialRevoked)
		require.Equal(t, []core.ResourceKind{core.ResourceModelConnection}, cred.kinds,
			"模型连接凭据不可用时立即拒绝，不再继续校渠道")
	})

	t.Run("全部通过", func(t *testing.T) {
		require.NoError(t, CheckAgentBindings(ctx, deps, "reporter", bindingAgentUID,
			profileOf("alpha", []string{"ops"}, okModel, okNotify)))
	})
}

// TestBindingProjection_ConcurrentRebuild —— 重建面与失败集合读写并发安全：
// 多个写路径同时全量重建，同时有读者问"这个 Agent 的投影可信吗"，
// 不出现竞态、不丢失失败记录（`-race` 下运行）。
func TestBindingProjection_ConcurrentRebuild(t *testing.T) {
	ctx := context.Background()
	env := newBindingsEnv(t)
	modelUID := env.seedModelVersion(t, "alpha")

	agents := []string{"reporter", "writer", "loader", "reader"}
	for _, name := range agents {
		env.writeAgentMD(t, name, bindingsBlock(
			[]core.ResourceBinding{{Name: "alpha", UID: modelUID}},
			[]core.ResourceBinding{{Name: "ops", UID: bindingNotifyUID}},
		))
	}
	// 前三个 Agent 有身份，reader 没有：失败集合里必须有且只有那一个。
	// （reporter 的身份由夹具写入，其余两个在这里补。）
	for _, name := range agents[1:3] {
		uid, err := core.NewAgentUID()
		require.NoError(t, err)
		require.NoError(t, env.resources.PutLifecycleIndex(ctx, core.LifecycleRecord{
			SchemaVersion: 2, Agent: name, AgentUID: uid, State: core.AgentActive,
			Generation: 1, UpdatedAt: baselineClock().UnixMilli(), OperationID: env.opID,
		}, true, ""))
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 3; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = env.projection.FailedAgents()
					_, _ = env.projection.ProjectionError("reporter")
				}
			}
		}()
	}

	var writers sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for round := 0; round < 5; round++ {
				if _, err := env.projection.RebuildAll(ctx); err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	// 最终状态：失败集合恰好是那个没有身份的 Agent，有身份的投影完整。
	require.Equal(t, map[string]string{"reader": projectionReasonNoIdentity},
		env.projection.FailedAgents())
	rows, err := env.bindings.Bindings(ctx, bindingAgentUID)
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

// agentMDWithProvider 拼一份 provider / 通知声明可定制的 AGENT.md（本文件的解析
// 矩阵要换资源名；公共形态仍由 agentMDWithBindings 承担）。
// agentMDWithProvider renders an AGENT.md with a custom provider and channel list.
func agentMDWithProvider(name, provider, channels, block string) string {
	return "---\n" +
		"name: " + name + "\n" +
		"provider: {name: " + provider + ", model: " + provider + "-model}\n" +
		"notify_channels: " + channels + "\n" +
		block +
		"---\n\n正文\n"
}

// TestAgentConfigReader_ResolvesReferences —— 结构化配置读面的解析矩阵（卡 R4.3
// 「读投影与写语义」）：齐全绑定 → bound + published + eligible 且 binding_complete；
// 缺绑定 → missing 且 UID 为空；孤儿绑定 → orphan；指向未知身份的 UID → stale_uid；
// 墓碑身份 → stale_uid + deleted；`content_etag` 按磁盘原始字节计算。
// 逆改点：把"读不出的状态"写成 bound → stale / deleted 两例红；ETag 改按结构化
// 字段重算 → 与 `os.ReadFile` 的哈希不再相等。
func TestAgentConfigReader_ResolvesReferences(t *testing.T) {
	ctx := context.Background()
	env := newBindingsEnv(t)
	modelUID := env.seedModelVersion(t, "alpha")
	notifyUID := env.seedNotifyChannel(t, "ops")

	lifecycle := workspace.NewLifecycleStore(env.root, baselineClock)
	if _, err := lifecycle.Bootstrap(ctx, "reporter", env.opID); err != nil {
		t.Fatalf("bootstrap lifecycle: %v", err)
	}
	state, exists, err := lifecycle.Read(ctx, "reporter")
	require.NoError(t, err)
	require.True(t, exists)

	reader := NewAgentConfigReader(AgentConfigDeps{
		Root:       env.root,
		Resources:  env.resources,
		Identities: storage.NewIdentityStore(env.db),
		Notify:     env.notify,
		Lifecycle:  lifecycle,
	})

	// 齐全绑定：两条引用都解析成 bound，并补齐发布与资格。
	block := bindingsBlock(
		[]core.ResourceBinding{{Name: "alpha", UID: modelUID}},
		[]core.ResourceBinding{{Name: "ops", UID: notifyUID}},
	)
	env.writeAgentMD(t, "reporter", block)

	raw, err := os.ReadFile(filepath.Join(env.root, "agents", "reporter", "AGENT.md"))
	require.NoError(t, err)

	view, err := reader.Config(ctx, "reporter")
	require.NoError(t, err)
	require.Equal(t, "reporter", view.Agent)
	require.Equal(t, string(state.AgentUID), view.AgentUID)
	require.Equal(t, workspace.ContentETag(raw), view.ContentETag,
		"ETag 必须按磁盘原始字节计算（与 /content 同一基准）")
	require.Equal(t, "alpha", view.Fields.Provider.Name)
	require.Equal(t, []string{"ops"}, view.Fields.NotifyChannels)
	require.Equal(t, 10, view.Fields.Settings.MaxIterations, "缺省设置已在装载期填好")
	require.Equal(t, 20, view.Fields.Settings.MaxHistoryTurns)
	require.True(t, view.BindingComplete)

	require.Len(t, view.References, 2)
	modelRef := view.References[0]
	require.Equal(t, string(core.BindingModelConnection), modelRef.ResourceKind)
	require.Equal(t, "alpha", modelRef.RefName)
	require.Equal(t, string(modelUID), modelRef.ResourceUID)
	require.Equal(t, "alpha", modelRef.ResourceName)
	require.Equal(t, web.BindingStatusBound, modelRef.BindingStatus)
	require.True(t, modelRef.Published)
	require.True(t, modelRef.Eligible)
	require.False(t, modelRef.Deleted)
	require.Empty(t, modelRef.CredentialState, "未装配凭据端口时不猜状态")

	notifyRef := view.References[1]
	require.Equal(t, string(core.BindingNotifyChannel), notifyRef.ResourceKind)
	require.Equal(t, "ops", notifyRef.RefName)
	require.Equal(t, web.BindingStatusBound, notifyRef.BindingStatus)
	require.True(t, notifyRef.Published)
	require.True(t, notifyRef.Eligible)

	// 缺绑定：定义里的每个引用各占一行，UID 为空、不按名称猜。
	env.writeAgentMD(t, "reporter", "")
	view, err = reader.Config(ctx, "reporter")
	require.NoError(t, err)
	require.False(t, view.BindingComplete)
	require.Len(t, view.References, 2)
	for _, ref := range view.References {
		require.Equal(t, web.BindingStatusMissing, ref.BindingStatus, ref.RefName)
		require.Empty(t, ref.ResourceUID, ref.RefName)
		require.Empty(t, ref.ResourceName, ref.RefName)
		require.False(t, ref.Published, ref.RefName)
	}

	// 孤儿绑定：没有对应引用的绑定追加在**该组**末尾并标 orphan；它没有引用侧，
	// 因此不解析身份与发布——未知 UID 不会被误写成 stale_uid。
	orphan := bindingsBlock([]core.ResourceBinding{{Name: "legacy", UID: bindingOtherUID}}, nil)
	env.writeAgentMD(t, "reporter", orphan)
	view, err = reader.Config(ctx, "reporter")
	require.NoError(t, err)
	require.Len(t, view.References, 3)
	require.Equal(t, "legacy", view.References[1].RefName, "模型组的顺序：引用在前、孤儿在该组末尾")
	require.Equal(t, web.BindingStatusOrphan, view.References[1].BindingStatus)
	require.Equal(t, string(bindingOtherUID), view.References[1].ResourceUID)
	require.False(t, view.References[1].Published)
	require.Equal(t, web.BindingStatusMissing, view.References[2].BindingStatus, "通知引用仍单独成行")

	// 未知身份：绑定形状合法但库里没有这条 UID → stale_uid，不是 bound。
	staleUID, err := core.NewResourceUID()
	require.NoError(t, err)
	stale := bindingsBlock([]core.ResourceBinding{{Name: "alpha", UID: staleUID}}, nil)
	env.writeAgentMD(t, "reporter", stale)
	view, err = reader.Config(ctx, "reporter")
	require.NoError(t, err)
	require.Equal(t, web.BindingStatusStaleUID, view.References[0].BindingStatus)
	require.False(t, view.References[0].Published, "证明不了发布状态时不得写成已发布")

	// 墓碑身份：删除后身份行仍是权威事实 → stale_uid + deleted，且不再算发布。
	betaUID := env.seedModelVersion(t, "beta")
	require.NoError(t, env.resources.MarkDeletedByUID(ctx, core.ResourceModelConnection,
		"beta", betaUID, env.opID, baselineClock()))
	writeWorkspaceFile(t, env.root, "agents/reporter/AGENT.md",
		agentMDWithProvider("reporter", "beta", "[]",
			bindingsBlock([]core.ResourceBinding{{Name: "beta", UID: betaUID}}, nil)))
	view, err = reader.Config(ctx, "reporter")
	require.NoError(t, err)
	require.Equal(t, web.BindingStatusStaleUID, view.References[0].BindingStatus)
	require.True(t, view.References[0].Deleted, "墓碑身份必须标 deleted")
	require.False(t, view.References[0].Published)
	require.False(t, view.BindingComplete)
}

// TestAgentConfigFields_DisplayName：表单字段原样投影 `display_name`（空串保持空串，
// 回退技术名只发生在列表 / 详情展示面），身份与技术名不受影响（R6.2）。
// TestAgentConfigFields_DisplayName projects the raw display_name; the technical name
// and identity are untouched and the fallback happens only in display views.
func TestAgentConfigFields_DisplayName(t *testing.T) {
	fields := agentConfigFields(&core.Profile{
		Name:        "writer",
		Description: "描述",
		DisplayName: "值班助手",
	})
	require.Equal(t, "值班助手", fields.DisplayName)
	require.Equal(t, "writer", fields.Name, "技术名不因展示名改变")
	require.Equal(t, "描述", fields.Description)

	blank := agentConfigFields(&core.Profile{Name: "writer"})
	require.Empty(t, blank.DisplayName, "未声明时投影空串，由展示层回退")
	require.Equal(t, "writer", blank.Name)
}

// TestScheduleDeliveryTargets_SaveTimeEligibility —— 保存期校验定时任务的系统代投目标
// （US-12 R7，裁决 D-1）：只认资源 UID、必须是**活跃**渠道、必须在当前 Agent 的可用
// 范围内；空目标跳过；目录未装配时失败关闭。运行侧另有 Prepare 复核，两侧同口径。
// 中文验收点：保存时就挡下"投给已删除渠道 / 别人专属渠道"的配置。
// TestScheduleDeliveryTargets_SaveTimeEligibility pins the save-time target check:
// uid-only, active rows only, eligibility enforced, and a nil catalog fails closed.
func TestScheduleDeliveryTargets_SaveTimeEligibility(t *testing.T) {
	ctx := context.Background()
	env := newBindingsEnv(t)
	channels := storage.NewNotifyChannelRepo(env.db)
	deps := BindingCheckDeps{NotifyChannels: channels}

	sharedUID := env.seedNotifyChannel(t, "ops")
	profileWith := func(deliverTo string) *core.Profile {
		return &core.Profile{Schedules: []core.Schedule{{
			ID: "daily", Cron: "0 0 8 * * *", Zone: "UTC", Message: "摘要", DeliverTo: deliverTo,
		}}}
	}

	t.Run("空目标跳过", func(t *testing.T) {
		require.NoError(t, CheckScheduleDeliveryTargets(ctx, deps, bindingAgentUID, profileWith("")))
		require.NoError(t, CheckScheduleDeliveryTargets(ctx, deps, bindingAgentUID, nil))
	})

	t.Run("活跃共享渠道通过", func(t *testing.T) {
		require.NoError(t, CheckScheduleDeliveryTargets(ctx, deps, bindingAgentUID,
			profileWith(string(sharedUID))))
	})

	t.Run("非UID形态拒绝", func(t *testing.T) {
		err := CheckScheduleDeliveryTargets(ctx, deps, bindingAgentUID, profileWith("ops"))
		require.ErrorIs(t, err, core.ErrAgentResourceBindingInvalid)
		var problem BindingProblem
		require.ErrorAs(t, err, &problem)
		require.Equal(t, core.BindingNotifyChannel, problem.Kind)
		require.Equal(t, "daily", problem.Name, "错误必须点名是哪条任务")
	})

	t.Run("不存在的渠道拒绝", func(t *testing.T) {
		missing, err := core.NewResourceUID()
		require.NoError(t, err)
		err = CheckScheduleDeliveryTargets(ctx, deps, bindingAgentUID, profileWith(string(missing)))
		require.ErrorIs(t, err, core.ErrAgentResourceUnavailable)
		// 契约 run-and-errors §8：对外文案必须点名任务与 UID 前 8 位，而不是只回
		// "绑定的资源不存在"——运维要在保存时就知道改哪一条任务（R7 人工验收发现）。
		// The public message names the task and the uid prefix, not just a generic phrase.
		var adminErr *core.AdminError
		require.ErrorAs(t, err, &adminErr)
		require.Contains(t, adminErr.Message, `定时任务 "daily"`, "必须点名哪条任务")
		require.Contains(t, adminErr.Message, string(missing)[:8], "必须点名目标 UID 前 8 位")
		require.Contains(t, adminErr.Message, "请重新选择", "必须给出下一步")
		var problem BindingProblem
		require.ErrorAs(t, err, &problem, "类别 / 名称 / 原因仍必须可由 errors.As 取到")
		require.Equal(t, "daily", problem.Name)
	})

	t.Run("已删除渠道拒绝", func(t *testing.T) {
		doomedUID := env.seedNotifyChannel(t, "doomed")
		require.NoError(t, env.notify.Delete(ctx, "doomed", env.opID))
		err := CheckScheduleDeliveryTargets(ctx, deps, bindingAgentUID, profileWith(string(doomedUID)))
		require.ErrorIs(t, err, core.ErrAgentResourceUnavailable,
			"墓碑行存在也不算目标可用——保存期只认活跃渠道")
	})

	t.Run("专属渠道只认属主", func(t *testing.T) {
		owner, err := core.NewAgentUID()
		require.NoError(t, err)
		exclUID := bindingExclUID
		require.NoError(t, env.notify.Create(ctx, core.NotifyChannel{
			ResourceUID:   exclUID,
			Name:          "exclusive",
			Type:          "webhook",
			URL:           "https://example.com/x",
			URLHost:       "example.com",
			Scope:         core.ScopeExclusive,
			OwnerAgentUID: owner,
			OperationID:   env.opID,
		}))
		require.ErrorIs(t, CheckScheduleDeliveryTargets(ctx, deps, bindingAgentUID,
			profileWith(string(exclUID))), core.ErrResourceNotEligible)
		require.NoError(t, CheckScheduleDeliveryTargets(ctx, deps, owner, profileWith(string(exclUID))))
	})

	t.Run("目录未装配失败关闭", func(t *testing.T) {
		err := CheckScheduleDeliveryTargets(ctx, BindingCheckDeps{}, bindingAgentUID,
			profileWith(string(sharedUID)))
		require.Error(t, err, "没有渠道目录时不得静默跳过资格校验")
		require.NotErrorIs(t, err, core.ErrAgentResourceUnavailable)
	})
}

// TestSaveBindingCheck_IncludesDeliveryTargets —— 保存期闭包把定时任务目标校验接在
// 绑定校验之后：绑定全过、目标非法时仍然拒绝（证明两条校验都在同一次保存里跑）。
// 中文验收点：目标校验不是"只写在文档里"的约定。
// TestSaveBindingCheck_IncludesDeliveryTargets proves the save closure runs the
// schedule-target check after the binding check.
func TestSaveBindingCheck_IncludesDeliveryTargets(t *testing.T) {
	ctx := context.Background()
	env := newBindingsEnv(t)
	modelUID := env.seedModelVersion(t, "alpha")
	notifyUID := env.seedNotifyChannel(t, "ops")
	check := SaveBindingCheck(nil, BindingCheckDeps{
		Resources:      env.resources,
		Notify:         env.notify,
		NotifyChannels: storage.NewNotifyChannelRepo(env.db),
	})
	profile := &core.Profile{
		Name:           "reporter",
		Provider:       core.ProviderRef{Name: "alpha", Model: "alpha-model"},
		NotifyChannels: []string{"ops"},
		ResourceBindings: core.ResourceBindings{
			ModelConnections: []core.ResourceBinding{{Name: "alpha", UID: modelUID}},
			NotifyChannels:   []core.ResourceBinding{{Name: "ops", UID: notifyUID}},
		},
		Schedules: []core.Schedule{{
			ID: "daily", Cron: "0 0 8 * * *", Zone: "UTC", Message: "摘要", DeliverTo: string(bindingOtherUID),
		}},
	}
	require.ErrorIs(t, check(ctx, "reporter", profile), core.ErrAgentResourceUnavailable,
		"绑定全过也不得放过非法投递目标")

	profile.Schedules[0].DeliverTo = string(notifyUID)
	require.NoError(t, check(ctx, "reporter", profile))
}
