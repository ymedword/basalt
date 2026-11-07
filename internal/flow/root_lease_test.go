package flow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 US-24 §3.1.6 的服务层判据（T020）：
//   ① 管理面 Recover / Revise 写 Queued 时零 token 自取根租约，用完释放；
//   ② 领取失败（ErrLeaseHeld）或未装配 → ErrFlowRootBusy，且**零写入**；
//   ③ 写 Blocked（停住）不需要租约，但必须携带 Isolated；
//   ④ 关闭类写入（Cancel / closeJoined）不领租约，但必须携带 Isolated。
// The unit criteria of §3.1.6: self-acquired root lease for advance-class writes,
// fail-closed refusals, and Isolated on every close-class write.

// rootLeaseRepo 记录服务层交给仓储的请求，不做真实持久化。
type rootLeaseRepo struct {
	task      Task
	recovered []ReconcileTask
	revised   []AcceptRevision
	cancelled []CancelTask
	unknown   bool
}

func (r *rootLeaseRepo) FindAccepted(context.Context, core.Principal, string) (Task, error) {
	return Task{}, ErrNotFound
}

func (r *rootLeaseRepo) Create(context.Context, CreateTask) (Task, error) { return r.task, nil }

func (r *rootLeaseRepo) Claim(context.Context, ClaimNode) (NodeAttempt, error) {
	return NodeAttempt{}, nil
}

func (r *rootLeaseRepo) Commit(context.Context, CommitNode) (Task, error) { return r.task, nil }

func (r *rootLeaseRepo) Cancel(_ context.Context, req CancelTask) (Task, error) {
	r.cancelled = append(r.cancelled, req)
	return r.task, nil
}

func (r *rootLeaseRepo) Abandon(_ context.Context, req AbandonTask) (Task, error) {
	return r.task, nil
}

func (r *rootLeaseRepo) Recover(_ context.Context, req ReconcileTask) (Task, error) {
	r.recovered = append(r.recovered, req)
	return r.task, nil
}

func (r *rootLeaseRepo) Revise(_ context.Context, req AcceptRevision) (Task, error) {
	r.revised = append(r.revised, req)
	return r.task, nil
}

func (r *rootLeaseRepo) Read(context.Context, core.Principal, string) (Task, error) {
	return r.task, nil
}

func (r *rootLeaseRepo) ReadForWorker(context.Context, string) (Task, error) { return r.task, nil }

func (r *rootLeaseRepo) ReadyRoots(context.Context, int64, int) ([]Task, error) { return nil, nil }

func (r *rootLeaseRepo) OpenRoots(context.Context, int) ([]Task, error) { return nil, nil }

func (r *rootLeaseRepo) Defer(context.Context, string, TaskVersion, int64) error { return nil }

func (r *rootLeaseRepo) HasUnknownActions(context.Context, string) (bool, error) {
	return r.unknown, nil
}

// rootLeaseGate 是 joinedRunner + isolatedRunner 的替身：两个判据独立取值。
type rootLeaseGate struct {
	joined   bool
	isolated bool
	runs     int
}

func (g *rootLeaseGate) Run(context.Context, NodeRunRequest) (NodeRunResult, error) {
	g.runs++
	return NodeRunResult{}, nil
}

func (g *rootLeaseGate) Joined(context.Context, Task) (bool, error) { return g.joined, nil }

func (g *rootLeaseGate) Isolated(context.Context, Task) (bool, error) { return g.isolated, nil }

// rootLeaseStore 只实现服务层用到的 Acquire / Release 语义。
type rootLeaseStore struct {
	acquireErr error
	acquired   []string
	released   []core.LeaseToken
	ttls       []time.Duration
}

func (s *rootLeaseStore) Acquire(_ context.Context, kind core.LeaseKind, resourceID string,
	owner core.WorkerIdentity, ttl time.Duration,
) (core.Lease, error) {
	if s.acquireErr != nil {
		return core.Lease{}, s.acquireErr
	}
	s.acquired = append(s.acquired, string(kind)+"/"+resourceID)
	s.ttls = append(s.ttls, ttl)
	return core.Lease{LeaseToken: core.LeaseToken{
		Kind: kind, ResourceID: resourceID,
		OwnerWorkerID: owner.InstanceID, OwnershipEpoch: 1,
	}}, nil
}

func (s *rootLeaseStore) Renew(context.Context, core.LeaseToken, time.Duration) (core.Lease, error) {
	return core.Lease{}, nil
}

func (s *rootLeaseStore) Release(_ context.Context, token core.LeaseToken) error {
	s.released = append(s.released, token)
	return nil
}

func (s *rootLeaseStore) FencedTx(context.Context, core.LeaseToken,
	func(context.Context, core.Tx) error,
) error {
	return nil
}

type rootLeaseAuthority struct{ p core.Principal }

func (a rootLeaseAuthority) Current(context.Context, Task) (core.Principal, error) { return a.p, nil }

func (a rootLeaseAuthority) CheckStart(context.Context, core.Principal, DefinitionVersion) error {
	return nil
}

func (a rootLeaseAuthority) NoAvailableCallerKey(context.Context, string) (bool, error) {
	return false, nil
}

// rootLeaseFixture 组装一个 Blocked 根任务与可配置的两个判据。
func rootLeaseFixture(status TaskStatus) (Task, core.Principal, *Service, *rootLeaseRepo, *rootLeaseGate, *rootLeaseStore) {
	principal := core.LocalAdminCLIPrincipal()
	task := Task{
		ID:               "ft_root",
		RootTaskID:       "ft_root",
		CallerID:         principal.CallerID,
		AuthorizingKeyID: principal.KeyID,
		AuthorizingKind:  string(core.AuthorizingKindAdminBuiltin),
		Status:           status,
		Version:          TaskVersion{Revision: 1, StateVersion: 2},
		CloseIntent:      CloseNone,
		Deadline:         time.Now().Add(time.Hour).UnixMilli(),
		Revision:         TaskRevision{Outputs: map[string]NodeOutput{}},
	}
	repo := &rootLeaseRepo{task: task}
	gate := &rootLeaseGate{}
	leases := &rootLeaseStore{}
	service := &Service{
		Repository: repo,
		Authority:  rootLeaseAuthority{p: principal},
		Runner:     gate,
		Config:     DefaultConfig(),
		Identity:   core.WorkerIdentity{InstanceID: "worker-self", Name: "self"},
		Leases:     leases,
		LeaseTTL:   45 * time.Second,
	}
	return task, principal, service, repo, gate, leases
}

func resumeFor(task Task, principal core.Principal, op string) ResumeRequest {
	return ResumeRequest{
		Principal: principal, TaskID: task.ID, Expected: task.Version,
		OperationID: op, Reason: "management resume",
	}
}

// TestFlowRootLease_ManagementRecoverQueuedAcquiresAndReleases
// 零 token 写 Queued → 自取根租约、把 token 交给仓储、返回前释放。
func TestFlowRootLease_ManagementRecoverQueuedAcquiresAndReleases(t *testing.T) {
	task, principal, service, repo, gate, leases := rootLeaseFixture(Blocked)
	gate.joined, gate.isolated = true, true

	_, err := service.Recover(t.Context(), resumeFor(task, principal, "resume-self"))
	require.NoError(t, err)
	require.Equal(t, []string{"flow_root/ft_root"}, leases.acquired, "必须按根领取 flow_root 租约")
	require.Equal(t, []time.Duration{45 * time.Second}, leases.ttls, "TTL 取装配注入的值")
	require.Len(t, repo.recovered, 1)
	require.Equal(t, Queued, repo.recovered[0].Status)
	require.True(t, repo.recovered[0].Isolated, "Isolated 必须随对账请求下传")
	require.Equal(t, core.LeaseFlowRoot, repo.recovered[0].Request.Lease.Kind)
	require.Equal(t, "ft_root", repo.recovered[0].Request.Lease.ResourceID)
	require.Len(t, leases.released, 1, "请求路径必须在使用后释放自取租约")
	require.Equal(t, "ft_root", leases.released[0].ResourceID)
}

// TestFlowRootLease_ManagementRecoverBusyRefused
// 另一个执行者持有根租约 → ErrFlowRootBusy，且仓储零写入（不得降级成无租约写入）。
func TestFlowRootLease_ManagementRecoverBusyRefused(t *testing.T) {
	task, principal, service, repo, gate, leases := rootLeaseFixture(Blocked)
	gate.joined = true
	leases.acquireErr = core.ErrLeaseHeld

	_, err := service.Recover(t.Context(), resumeFor(task, principal, "resume-busy"))
	require.ErrorIs(t, err, ErrFlowRootBusy)
	require.Empty(t, repo.recovered, "撞上租约时不得写库")
	require.Empty(t, leases.released)
}

// TestFlowRootLease_RecoverUnassembledQueuedRefused
// 未注入 Identity / Leases 的装配写 Queued 只能拒绝，不得退化成无租约写入。
func TestFlowRootLease_RecoverUnassembledQueuedRefused(t *testing.T) {
	task, principal, service, repo, gate, _ := rootLeaseFixture(Blocked)
	gate.joined = true
	service.Leases, service.Identity = nil, core.WorkerIdentity{}

	_, err := service.Recover(t.Context(), resumeFor(task, principal, "resume-unassembled"))
	require.ErrorIs(t, err, ErrFlowRootBusy)
	require.Empty(t, repo.recovered, "未装配不得写库")
}

// TestFlowRootLease_RecoverBlockedNeedsNoLease
// 写 Blocked 是停住，不要求根租约；但 Isolated 仍必须下传。
func TestFlowRootLease_RecoverBlockedNeedsNoLease(t *testing.T) {
	task, principal, service, repo, gate, leases := rootLeaseFixture(Blocked)
	gate.joined, gate.isolated = false, true

	_, err := service.Recover(t.Context(), resumeFor(task, principal, "resume-blocked"))
	require.NoError(t, err)
	require.Empty(t, leases.acquired, "写 Blocked 不需要领取根租约")
	require.Len(t, repo.recovered, 1)
	require.Equal(t, Blocked, repo.recovered[0].Status)
	require.True(t, repo.recovered[0].Isolated, "关闭 / 停住类写入也必须携带 Isolated")
	require.Empty(t, repo.recovered[0].Request.Lease.ResourceID, "Blocked 不要求 token")
}

// TestFlowRootLease_RecoverWithTokenSkipsAcquire
// 回收器（A3）已持租约时原样使用：不重复领取，也不释放非自取的 token。
func TestFlowRootLease_RecoverWithTokenSkipsAcquire(t *testing.T) {
	task, principal, service, repo, gate, leases := rootLeaseFixture(Blocked)
	gate.joined, gate.isolated = true, true
	req := resumeFor(task, principal, "reclaim-token")
	req.Lease = core.LeaseToken{Kind: core.LeaseFlowRoot, ResourceID: "ft_root", OwnershipEpoch: 7}

	_, err := service.Recover(t.Context(), req)
	require.NoError(t, err)
	require.Empty(t, leases.acquired, "回收器已持租约时不得重复领取")
	require.Empty(t, leases.released, "回收器路径不能释放非自取的租约")
	require.Len(t, repo.recovered, 1)
	require.EqualValues(t, 7, repo.recovered[0].Request.Lease.OwnershipEpoch, "必须原样传下来自回收器的 token")
}

// TestFlowRootLease_CloseWritesCarryIsolated
// 关闭类写入不领租约，但 Cancel / closeJoined 都必须携带 Isolated。
func TestFlowRootLease_CloseWritesCarryIsolated(t *testing.T) {
	task, principal, service, repo, gate, leases := rootLeaseFixture(Queued)
	gate.isolated = true

	_, err := service.Cancel(t.Context(), CancelRequest{
		Principal: principal, TaskID: task.ID,
		Expected: task.Version, OperationID: "cancel-close", Reason: "close before run",
	})
	require.NoError(t, err)
	require.Len(t, repo.cancelled, 1)
	require.True(t, repo.cancelled[0].Isolated, "Cancel 必须携带 Isolated")
	require.Empty(t, leases.acquired, "关闭类写入不领租约")

	repo.recovered = nil
	_, err = service.closeJoined(t.Context(), principal, task)
	require.NoError(t, err)
	require.Len(t, repo.recovered, 1)
	require.Equal(t, Blocked, repo.recovered[0].Status)
	require.True(t, repo.recovered[0].Isolated, "closeJoined 必须携带 Isolated")
	require.True(t, repo.recovered[0].Joined)
	require.Empty(t, leases.acquired)
}
