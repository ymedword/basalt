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
	// Workers / ActiveLeases 是分布式执行的只读诊断读数（US-24 §3.1.13，US-25C
	// §2.3.9 扩展为按进程实例的三态读数）：`Workers` 携带同一快照的全库计数与有界
	// 明细，`ActiveLeases` 与它们来自**同一条语句的快照**，统计**全部**负责人（含
	// 没有 worker_nodes 行的 `legacy` 回填租约），因此不等于各节点活跃租约数之和。
	// `WorkerDefaultIntervalSeconds` 是本部署的心跳间隔，仅用于迁移前旧行的诊断
	// 阈值兜底与详情标注，不参与任何归属判定。
	// Workers and ActiveLeases are the distributed runtime's read-only readings from one
	// statement snapshot; the total counts every owner, including owners without a node
	// row.
	Workers                      core.WorkerOverview
	ActiveLeases                 int64
	WorkerDefaultIntervalSeconds int
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
	Workers       *SystemWorkersView   `json:"workers,omitempty"`
	Database      SystemDatabaseView   `json:"database"`
	Auth          SystemAuthView       `json:"auth"`
}

// SystemWorkersView 是 Worker 列表与活跃租约计数（US-24 §3.1.13；US-25C §2.3.9
// 扩展为按进程实例的三态读数）。与 runs / resources 同一条纪律：数据库不可读时
// **整组省略**，不把"读不到"显示成 0；反过来，能读到时的 0 是真实读数。
//
// 统计对象是**进程实例**，不是服务器：同一 `worker_name` 的多个实例各自成行，页面
// 不得按名称合并，也不得由行数推断物理机数量。`counts` / `total` 是全库聚合，
// `nodes` 是有界明细（`truncated = true` 表示明细被截断）。
// The counts are process instances, not servers; they come from the database aggregate,
// never from the bounded detail rows.
type SystemWorkersView struct {
	AsOf         string                 `json:"as_of"`
	Counts       SystemWorkerCountsView `json:"counts"`
	Total        int                    `json:"total"`
	Truncated    bool                   `json:"truncated"`
	ActiveLeases int64                  `json:"active_leases"`
	Nodes        []SystemWorkerView     `json:"nodes"`
}

// SystemWorkerCountsView 是三类服务端状态的全库计数（`active` 只表示近期有心跳，
// 不等于任务运行正常；`stale` 是心跳超时待确认，不证明进程已死）。
// The database-wide counts of the three server-side states.
type SystemWorkerCountsView struct {
	Active  int `json:"active"`
	Stale   int `json:"stale"`
	Stopped int `json:"stopped"`
}

// SystemWorkerView 是单个 Worker 的只读行，字段与 `basalt workers list` 同源同形，
// 另加服务端计算的状态与诊断读数（§3.1.6）。
//
//   - `status` 只取 `active` / `stale` / `stopped` 三值，由服务端用数据库时钟判定；
//     页面不得用浏览器时钟重算；
//   - `heartbeat_interval_seconds` 缺省（旧行）表示迁移前登记，阈值取部署默认并由
//     `stale_after_seconds` 给出实际生效值；
//   - `active_leases` 是该实例仍有效的执行权数量，不是"正在执行的任务数"。
//
// One worker row: the same shape as `basalt workers list`, plus server-computed status
// and diagnostic readings.
type SystemWorkerView struct {
	Instance                 string `json:"instance"`
	Name                     string `json:"name"`
	Hostname                 string `json:"hostname"`
	BuildVersion             string `json:"build_version"`
	SchemaVersion            int    `json:"schema_version"`
	StartedAt                string `json:"started_at"`
	HeartbeatAt              string `json:"heartbeat_at"`
	StoppedAt                string `json:"stopped_at,omitempty"`
	HeartbeatAgeSeconds      int64  `json:"heartbeat_age_seconds"`
	HeartbeatIntervalSeconds *int   `json:"heartbeat_interval_seconds,omitempty"`
	StaleAfterSeconds        int    `json:"stale_after_seconds"`
	Status                   string `json:"status"`
	ActiveLeases             int64  `json:"active_leases"`
}

// workerStaleAfterSeconds 是逐行诊断阈值 `max(3 × 心跳间隔, 30s)`（§2.3.9）。
// 旧行没有登记间隔时用部署默认兜底——不得拿管理台进程自己的间隔套给所有实例。
// workerStaleAfterSeconds is the per-row diagnostic threshold; a nil interval means a
// pre-migration row falling back to the deployment default.
func workerStaleAfterSeconds(interval *int, deploymentDefault int) int {
	seconds := deploymentDefault
	if interval != nil {
		seconds = *interval
	}
	if seconds < 1 {
		seconds = 1
	}
	if threshold := 3 * seconds; threshold > 30 {
		return threshold
	}
	return 30
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
	// Worker 组永远编码为数组（没有实例时 `[]`），与列表端点同一纪律。计数与明细
	// 来自存储层**同一条语句的同一快照**；这里只做投影，不重算、不合并、不补 0。
	// The node list is always an array, empty when no worker has registered; counts and
	// rows are projected from the store's single-statement snapshot.
	overview := readings.Workers
	nodes := make([]SystemWorkerView, 0, len(overview.Nodes))
	for _, node := range overview.Nodes {
		row := SystemWorkerView{
			Instance:                 node.InstanceID,
			Name:                     node.Name,
			Hostname:                 node.Hostname,
			BuildVersion:             node.BuildVersion,
			SchemaVersion:            node.SchemaVersion,
			StartedAt:                formatTime(node.StartedAt),
			HeartbeatAt:              formatTime(node.LastHeartbeatAt),
			HeartbeatAgeSeconds:      node.HeartbeatAgeSeconds,
			HeartbeatIntervalSeconds: node.HeartbeatIntervalSeconds,
			StaleAfterSeconds:        workerStaleAfterSeconds(node.HeartbeatIntervalSeconds, readings.WorkerDefaultIntervalSeconds),
			Status:                   node.Status,
			ActiveLeases:             node.ActiveLeases,
		}
		if node.StoppedAt != nil {
			row.StoppedAt = formatTime(*node.StoppedAt)
		}
		nodes = append(nodes, row)
	}
	view.Workers = &SystemWorkersView{
		AsOf: formatTime(overview.AsOf),
		Counts: SystemWorkerCountsView{
			Active:  overview.Counts.Active,
			Stale:   overview.Counts.Stale,
			Stopped: overview.Counts.Stopped,
		},
		Total:        overview.Total,
		Truncated:    overview.Truncated,
		ActiveLeases: readings.ActiveLeases,
		Nodes:        nodes,
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
