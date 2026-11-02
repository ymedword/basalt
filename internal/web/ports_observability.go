package web

import (
	"context"
	"time"

	"github.com/ymedword/basalt/internal/observability"
)

// ObservabilityReader 是管理面的消费侧只读端口，与后台生命周期分开。
// ObservabilityReader is the management read port, separate from background lifecycle ownership.
type ObservabilityReader interface {
	ObservationEnabled() bool
	Overview(context.Context, observability.OverviewQuery) (observability.Overview, error)
	Timeline(context.Context, observability.TimelineQuery) (observability.Timeline, error)
	Context(context.Context, string) (observability.ContextDiagnosis, error)
	MetricsSnapshot(context.Context, time.Duration) (observability.MetricsSnapshot, error)
	AlertStatus(context.Context, observability.ReadWindow) (observability.AlertStatus, error)
	ExportStatus(context.Context) (observability.ExportStatus, error)
	CleanupStatus(context.Context, observability.ReadWindow) (observability.CleanupStatus, error)
}
