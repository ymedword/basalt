package observability

import (
	"context"
	"time"

	"github.com/ymedword/basalt/internal/diagnostics"
)

// RunFactsReader 在消费方声明只读关联，不暴露数据库句柄。
// RunFactsReader declares read-only run associations at the consumer.
type RunFactsReader interface {
	RunFacts(context.Context, string) (RunFacts, error)
	RunsByRequestID(context.Context, string) ([]string, error)
	RunsBySessionID(context.Context, string) ([]string, error)
	RunsByAgentWindow(context.Context, string, time.Time, time.Time) ([]RunFacts, error)
}

// ExecutionFactsReader 读取执行审计，不引入第二条审计写路径。
// ExecutionFactsReader reads audits without adding an audit write path.
type ExecutionFactsReader interface {
	LLMCallsByRun(context.Context, string) ([]LLMCallFact, error)
	ToolInvocationsByRun(context.Context, string) ([]ToolInvocationFact, error)
	EmbeddingCallsByRun(context.Context, string) ([]EmbeddingCallFact, error)
}

// TaskFactsReader 读取调度与真实运行之间的关联。
// TaskFactsReader reads task-to-run associations.
type TaskFactsReader interface {
	TaskExecutionFacts(context.Context, string, time.Time, time.Time) ([]TaskExecutionFact, error)
	TaskExecutionByRun(context.Context, string) ([]TaskExecutionFact, error)
}

// OperationFactsReader 读取不产生 Run 的管理操作。
// OperationFactsReader reads management operations independently of runs.
type OperationFactsReader interface {
	OperationFacts(context.Context, string) (ManagementOperationFact, error)
}

// IngestFactsReader 读取摄取及其向量调用。
// IngestFactsReader reads ingestion and its embedding calls.
type IngestFactsReader interface {
	IngestJobFacts(context.Context, string) (IngestJobFact, error)
	EmbeddingCallsByIngest(context.Context, string) ([]EmbeddingCallFact, error)
}

// AssemblyFactsReader 读取可丢弃装配证据，不读取知识正文。
// AssemblyFactsReader reads assembly evidence without knowledge bodies.
type AssemblyFactsReader interface {
	AssemblyByRun(context.Context, string) ([]AssemblyRecordFact, error)
}

// DiagnosticFactsReader 复用诊断查询协议，不改旧端点排序。
// DiagnosticFactsReader reuses the diagnostic query protocol.
type DiagnosticFactsReader interface {
	ListEvents(context.Context, diagnostics.Query) ([]diagnostics.DiagnosticEvent, error)
}

// DiagnosticReadEvent 把OBS读侧隐藏计数与冻结的v0事件字段分开。
// DiagnosticReadEvent keeps OBS omission metadata separate from the frozen v0 event.
type DiagnosticReadEvent struct {
	Event        diagnostics.DiagnosticEvent
	HiddenFields int
}

type diagnosticProjectionReader interface {
	ListObservationEvents(context.Context, diagnostics.Query) ([]DiagnosticReadEvent, error)
}

// CleanupFactsReader 只提供窗口级清理证据。
// CleanupFactsReader provides window-level evidence, not per-row tombstones.
type CleanupFactsReader interface {
	CleanupRuns(context.Context, time.Time, time.Time) ([]CleanupRun, error)
}

// Ports 显式注入只读端口，领域包不依赖存储适配层。
// Ports injects read ports without depending on storage adapters.
type Ports struct {
	Runs        RunFactsReader
	Exec        ExecutionFactsReader
	Tasks       TaskFactsReader
	Ops         OperationFactsReader
	Ingest      IngestFactsReader
	Assembly    AssemblyFactsReader
	Diagnostics DiagnosticFactsReader
	Cleanup     CleanupFactsReader
}
