package postgres

import "log/slog"

// ObserveAuditWriteFailures 绑定同进程指标回调，不改变审计错误或发起重试。
// ObserveAuditWriteFailures binds process telemetry without changing errors or retrying.
func (db *DB) ObserveAuditWriteFailures(observer func()) {
	if observer == nil {
		db.auditObserver.Store(nil)
		return
	}
	db.auditObserver.Store(&observer)
}

func (db *DB) noteAuditWriteFailure(source, phase string) {
	if db == nil {
		return
	}
	if observer := db.auditObserver.Load(); observer != nil {
		invokeAuditObserver(*observer)
		slog.Error("audit write failed", "source", source, "phase", phase, "error_kind", "audit_write_failed")
	}
}

func invokeAuditObserver(observer func()) {
	defer func() {
		if recover() != nil {
			slog.Error("audit metric callback failed", "error_kind", "metric_callback_failed")
		}
	}()
	observer()
}
