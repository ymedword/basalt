package web

// 本文件实现系统概览与进程采样两个只读观测端点（模块卡 §3.1.4；契约
// SystemOverview / SystemSample）。概览的数据库侧读数（运行计数、已发布资源
// 计数、库版本）经 SystemReader 端口注入——`storage/db.go` 的轻量 Ping /
// SchemaVersion 与计数 SQL 由 app 层适配，web 不 import storage。库不可用时仍
// 返回 200 且 `database.status=unavailable`，计数一律省略而**不**补 0。
// The system overview and process sampling endpoints. Database-backed readings come
// through the SystemReader port (adapted by app); an unavailable database still
// yields 200 with status=unavailable and omitted counts, never fabricated zeros.

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 数据库就绪枚举（契约 SystemOverview.database.status）。
// Database readiness enum as fixed by the contract.
const (
	databaseStatusOK          = "ok"
	databaseStatusUnavailable = "unavailable"
)

// SystemReadings 是概览的数据库侧聚合读数：一次查询返回运行计数、已发布资源
// 计数与库版本。字段为零值时也可能是真实读数（0 次运行），因此**不带 omitempty**；
// 整组读数只在查询失败时整体省略。
// SystemReadings bundles every database-backed overview reading; a zero value is a
// legitimate reading, so the group is omitted only when the whole read fails.
type SystemReadings struct {
	RunsRunning       int64
	RunsLast24h       int64
	RunsFailedLast24h int64
	ModelConnections  int64
	MCPServers        int64
	Skills            int64
	SchemaVersion     int
}

// SystemReader 是概览的数据库侧数据源（由 app 层适配 storage 的 Ping /
// SchemaVersion 与计数查询）。任一读失败返回错误；handler 据此把
// `database.status` 置 unavailable 并省略计数。
// SystemReader is the overview's database-backed source, adapted by app.
type SystemReader interface {
	ReadOverview(ctx context.Context) (SystemReadings, error)
}

// SystemOverviewView 是 /system/overview 的响应视图（契约 SystemOverview）。
// runs / resources 是整组可选：数据库不可用时省略，避免把"读不到"显示成 0。
// The overview view; runs and resources are omitted when the database is unavailable.
type SystemOverviewView struct {
	Version       string               `json:"version"`
	UptimeSeconds int64                `json:"uptime_seconds"`
	StartedAt     string               `json:"started_at"`
	Agents        SystemAgentsView     `json:"agents"`
	Runs          *SystemRunsView      `json:"runs,omitempty"`
	Resources     *SystemResourcesView `json:"resources,omitempty"`
	Database      SystemDatabaseView   `json:"database"`
	Auth          SystemAuthView       `json:"auth"`
}

// SystemAgentsView 是 Agent 计数（契约 SystemOverview.agents）；invalid 表示装载期
// 校验未通过或状态文件缺失 / 损坏 / 不可读。
// The agent counter; invalid covers load-time rejection and lifecycle anomalies.
type SystemAgentsView struct {
	Total    int `json:"total"`
	Active   int `json:"active"`
	Disabled int `json:"disabled"`
	Archived int `json:"archived"`
	Invalid  int `json:"invalid"`
}

// SystemRunsView 是运行计数（契约 SystemOverview.runs）。
// The run counter.
type SystemRunsView struct {
	Running       int64 `json:"running"`
	Last24h       int64 `json:"last_24h"`
	FailedLast24h int64 `json:"failed_last_24h"`
}

// SystemResourcesView 是已发布资源计数（契约 SystemOverview.resources）。
// The published-resource counter.
type SystemResourcesView struct {
	ModelConnections int64 `json:"model_connections"`
	MCPServers       int64 `json:"mcp_servers"`
	Skills           int64 `json:"skills"`
}

// SystemDatabaseView 是数据库就绪视图：unavailable 时 schema_version 省略（读不到
// 就不报版本号）。The database readiness view; schema_version is absent when
// the database cannot be read.
type SystemDatabaseView struct {
	Status        string `json:"status"`
	SchemaVersion *int   `json:"schema_version,omitempty"`
}

// SystemAuthView 是认证模式视图（契约 SystemOverview.auth）；mode 只在能确定
// 闸门配置时给出。The auth mode view; mode is present only when known.
type SystemAuthView struct {
	Mode         string `json:"mode,omitempty"`
	LoopbackOnly bool   `json:"loopback_only"`
}

// SystemSampleView 是 /system/sample 的响应视图（契约 SystemSample）：三个读数字段
// 空值时必须序列化为 null（不带 omitempty），unavailable 永远是数组。
// The sample view; nil readings serialize as null and unavailable is always an array.
type SystemSampleView struct {
	SampledAt      string   `json:"sampled_at"`
	CPUPercent     *float64 `json:"cpu_percent"`
	MemoryRSSBytes *int64   `json:"memory_rss_bytes"`
	Goroutines     *int64   `json:"goroutines"`
	Unavailable    []string `json:"unavailable"`
}

// adminModeReporter 让概览读取认证闸门的模式字面量；*authGuard 是唯一实现。
// adminModeReporter exposes the guard's mode literal to the overview.
type adminModeReporter interface {
	adminMode() string
}

// adminMode 返回契约枚举值（token / local_trust）。
// adminMode returns the contract enum value.
func (g *authGuard) adminMode() string { return g.cfg.Mode }

// systemBound 校验概览必需的三个端口：数据库读数、注册表、生命周期。
// 缺端口是装配错误，返回 500 固定文案。
// systemBound rejects a missing overview port with the fixed 500.
func (s *Server) systemBound(w http.ResponseWriter) bool {
	if s.opts.System != nil && s.opts.Registry != nil && s.opts.Lifecycle != nil {
		return true
	}
	s.logger().Error("装配缺少系统概览端口（System / Registry / Lifecycle）")
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	return false
}

// handleSystemOverview 组装概览：版本 / 运行时长来自装配；Agent 计数来自注册表
// 快照与逐 Agent 生命周期读；运行与资源计数来自 SystemReader。数据库读失败时
// 仍返回 200，只把 database.status 置 unavailable 并省略两组计数（约束 33 的
// "不伪造读数"同样适用于概览——读不到就不是 0）。
// handleSystemOverview assembles the overview; an unreadable database degrades to
// status=unavailable with the count groups omitted.
func (s *Server) handleSystemOverview(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.systemBound(w) {
		return
	}
	now := s.now()
	counts := s.countAgents(r.Context())
	view := SystemOverviewView{
		Version:       s.opts.Version,
		UptimeSeconds: uptimeSeconds(s.opts.StartedAt, now),
		StartedAt:     formatTime(s.opts.StartedAt),
		Agents: SystemAgentsView{
			Total:    counts.total,
			Active:   counts.active,
			Disabled: counts.disabled,
			Archived: counts.archived,
			Invalid:  counts.invalid,
		},
		Auth: SystemAuthView{
			Mode:         s.adminMode(),
			LoopbackOnly: loopbackOnlyAddr(s.opts.Addr),
		},
	}
	readings, err := s.opts.System.ReadOverview(r.Context())
	if err != nil {
		s.logger().Error("系统概览数据库读数失败", "error", err)
		view.Database = SystemDatabaseView{Status: databaseStatusUnavailable}
		s.writeOK(w, view)
		return
	}
	schemaVersion := readings.SchemaVersion
	view.Runs = &SystemRunsView{
		Running:       readings.RunsRunning,
		Last24h:       readings.RunsLast24h,
		FailedLast24h: readings.RunsFailedLast24h,
	}
	view.Resources = &SystemResourcesView{
		ModelConnections: readings.ModelConnections,
		MCPServers:       readings.MCPServers,
		Skills:           readings.Skills,
	}
	view.Database = SystemDatabaseView{Status: databaseStatusOK, SchemaVersion: &schemaVersion}
	s.writeOK(w, view)
}

// handleSystemSample 返回最近一次进程采样（最多每 5 秒重算一次，缓存与平台降级
// 都在 Sampler 内完成；本层只投影）。
// handleSystemSample projects the sampler's latest reading.
func (s *Server) handleSystemSample(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.Sampler == nil {
		s.logger().Error("装配缺少 Sampler 端口")
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	sample := s.opts.Sampler.Sample()
	s.writeOK(w, SystemSampleView{
		SampledAt:      formatTime(sample.SampledAt),
		CPUPercent:     sample.CPUPercent,
		MemoryRSSBytes: sample.MemoryRSSBytes,
		Goroutines:     sample.Goroutines,
		Unavailable:    append(make([]string, 0, len(sample.Unavailable)), sample.Unavailable...),
	})
}

// agentCounts 是概览的 Agent 四态计数。
// agentCounts is the four-way agent tally.
type agentCounts struct {
	total    int
	active   int
	disabled int
	archived int
	invalid  int
}

// countAgents 按注册表快照逐项读生命周期：未注册（含轻路径待确认）与状态文件
// 缺失 / 损坏 / 读失败一律计入 invalid——概览**不得**把不可运行项算成可用。
// 状态文件读失败记 ERROR（可观测性双轨：日志之外不静默吞错）。
// countAgents tallies the registry snapshot against each lifecycle file; any
// anomaly counts as invalid and read failures are logged.
func (s *Server) countAgents(ctx context.Context) agentCounts {
	entries := s.opts.Registry.Entries()
	counts := agentCounts{total: len(entries)}
	for _, entry := range entries {
		if !entry.Registered {
			counts.invalid++
			continue
		}
		rec, ok, err := s.opts.Lifecycle.Read(ctx, entry.Name)
		if err != nil {
			s.logger().Error("系统概览读取生命周期状态失败", "agent", entry.Name, "error", err)
			counts.invalid++
			continue
		}
		if !ok {
			counts.invalid++
			continue
		}
		switch rec.State {
		case core.AgentActive:
			counts.active++
		case core.AgentDisabled:
			counts.disabled++
		case core.AgentArchived:
			counts.archived++
		default:
			counts.invalid++
		}
	}
	return counts
}

// uptimeSeconds 把运行时长折算成整秒；起点零值或时钟回拨按 0 处理。
// uptimeSeconds never reports a negative uptime.
func uptimeSeconds(startedAt, now time.Time) int64 {
	if startedAt.IsZero() {
		return 0
	}
	if d := now.Sub(startedAt); d > 0 {
		return int64(d / time.Second)
	}
	return 0
}

// loopbackOnlyAddr 判断监听地址是否只绑回环：通配绑定（空 host / 0.0.0.0 / ::）
// 一律不算回环——管理台据此展示"本机信任模式是否仍然成立"。
// loopbackOnlyAddr reports a loopback-only bind; wildcard binds never count.
func loopbackOnlyAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return isLoopbackHost(addr)
	}
	return isLoopbackHost(host)
}

// adminMode 取认证闸门的模式字面量；测试装配可以没有闸门，此时省略 mode。
// adminMode reads the guard's mode; an absent guard yields an omitted mode.
func (s *Server) adminMode() string {
	if reporter, ok := s.opts.AdminAuth.(adminModeReporter); ok {
		return reporter.adminMode()
	}
	return ""
}
