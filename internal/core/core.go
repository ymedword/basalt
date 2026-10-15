package core

import (
	"context"
	"time"
)

// SchemaIntrospector 是只读的表结构自省能力——消费方在 cli，实现在 storage，
// 是本仓库依赖倒置的首个样例（接口定义在消费方，实现放各自的包）。
// SchemaIntrospector is the read-only schema introspection capability. The
// consumer is cli and the implementation lives in storage.
//
// 只有只读校验路径需要它：status 报告版本与表清点。写模式打开与迁移不在此接口内——
// 它们是能力函数（storage.OpenWritable / storage.Migrate），不是接口方法。
//
// Only read-only verification needs it. Write-mode opening and migration stay
// as capability functions in storage rather than interface methods.
type SchemaIntrospector interface {
	// SchemaVersion 返回库内的 schema_version 值；零行属损坏态（ErrSchemaCorrupt）。
	// SchemaVersion returns the version stored in the database; zero rows is a
	// corrupt state.
	SchemaVersion(ctx context.Context) (int, error)

	// TableNames 返回库内用户表的表名（不含 sqlite 内部表），按名排序。
	// TableNames returns the user table names, sorted, excluding SQLite internals.
	TableNames(ctx context.Context) ([]string, error)
}

// MetricsRegistry 是观测占位的接口——实装属扩展阶段。
// MetricsRegistry is the placeholder interface for observability; an actual
// implementation belongs to the extension phase.
//
// 刻意只留两个方法：计数与耗时。既有实装（Prometheus 等）都能满足这个最小面，
// 而多加方法会把这个占位变成一份过早定型的契约。
//
// Deliberately minimal: a counter and a duration. Adding more now would freeze a
// contract before there is anything to observe.
type MetricsRegistry interface {
	// IncCounter 给计数型指标加 delta。
	// IncCounter adds delta to a counter metric.
	IncCounter(name string, delta int64)

	// ObserveDuration 记录一次耗时观测值。
	// ObserveDuration records one duration observation.
	ObserveDuration(name string, d time.Duration)
}

// NoopMetrics 是 MetricsRegistry 的空实现：一切调用无副作用。
// NoopMetrics is the no-op implementation of MetricsRegistry.
type NoopMetrics struct{}

// 编译期断言：NoopMetrics 必须始终满足 MetricsRegistry。
// Compile-time assertion that NoopMetrics satisfies MetricsRegistry.
var _ MetricsRegistry = NoopMetrics{}

// IncCounter 实现 MetricsRegistry，不做任何事。
// IncCounter implements MetricsRegistry and does nothing.
func (NoopMetrics) IncCounter(string, int64) {}

// ObserveDuration 实现 MetricsRegistry，不做任何事。
// ObserveDuration implements MetricsRegistry and does nothing.
func (NoopMetrics) ObserveDuration(string, time.Duration) {}
