package app

// 本文件是 `web.RunService` 的唯一生产适配器（颗粒度文档 §3.1.31）：把"运行入口 +
// 运行账本读面"组合成一个端口，供 web 的运行列表 / 详情 / 取消与既有运行 / 归档入口
// 共用。适配器**只委托、不复制语义**——Run / Archive / Cancel 归 core.AgentService
// （准入、会话互斥、取消信号与等待都在那里），ListRuns / GetRun 归 storage.RunStore
// （固定排序、窗口与三过滤在 SQL 层）。清单的持久形态与 HTTP 视图是两套结构（约束 23）：
// 本层返回持久形态原样，视图投影由 web/views.go 单向完成，**不得**用视图字段反推账本。
// This adapter joins the run entry point with the ledger read face for web. It only
// delegates: admission, archiving and cancellation stay in core.AgentService, while the
// fixed ordering/window/filters stay in storage.RunStore. View projection is web-side.

import (
	"context"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/web"
)

// runService 是 web.RunService 的生产实现；两个字段必须来自同一次装配——Runs 是
// AgentService 写入账本所用的同一份仓储实例，读面因此不会看到另一条连接池的影子。
// runService is the production web.RunService; both fields come from the same assembly.
type runService struct {
	Service *core.AgentService
	Runs    *storage.RunStore
}

// 编译期断言：方法集必须完整覆盖 web.RunService（缺一个即在装配处失败，而不是在
// 第一个请求到达时才暴露）。
// Compile-time assertion that the adapter satisfies the full port.
var _ web.RunService = (*runService)(nil)

// NewRunService 装配适配器并返回端口类型：装配层是唯一知道接口背后实例的地方。
// NewRunService wires the adapter and hands back the port type.
func NewRunService(service *core.AgentService, runs *storage.RunStore) web.RunService {
	return &runService{Service: service, Runs: runs}
}

// Run 委托唯一运行入口（准入、在途登记、清单预写与循环编排都在 core）。
// Run delegates to the single run entry point.
func (s *runService) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	return s.Service.Run(ctx, req)
}

// Archive 保持既有"按会话归档"语义；参数名随模块卡 §3.1.5 的签名，值是 session_id。
// Archive keeps the session-scoped semantics; the parameter carries a session id.
func (s *runService) Archive(ctx context.Context, agent string) error {
	return s.Service.Archive(ctx, agent)
}

// ListRuns 直接返回账本的持久读侧投影——排序 / 窗口 / 过滤由仓储固定，本层不二次
// 排序、不补造字段、不裁剪清单哈希。
// ListRuns returns the ledger read projection unchanged.
func (s *runService) ListRuns(ctx context.Context, filter core.RunFilter) ([]core.RunDetail, error) {
	return s.Runs.ListRuns(ctx, filter)
}

// GetRun 返回单条详情（含准入清单的持久形态）；不存在时原样上抛 ErrRunNotFound，
// 由 web 映射 404。
// GetRun returns one persisted detail; ErrRunNotFound passes through unchanged.
func (s *runService) GetRun(ctx context.Context, runID string) (core.RunDetail, error) {
	return s.Runs.GetRun(ctx, runID)
}

// Cancel 委托 core 的取消语义：只对进程内在跑的运行发信号并至多等待上限，终态由
// 循环写入；本层**不得**写终态、不得替 core 解释"没送到"。
// Cancel delegates the signal-only cancellation; it never writes terminal state.
func (s *runService) Cancel(ctx context.Context, runID string) (core.RunDetail, error) {
	return s.Service.Cancel(ctx, runID)
}
