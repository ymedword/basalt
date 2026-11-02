package web

import "context"

// ObservationLifecycle 由HTTP服务已有上下文驱动，不创建第二个信号源。
// ObservationLifecycle uses the HTTP server context without another signal source.
type ObservationLifecycle interface {
	Start(context.Context) error
	Close() error
}
