package app

// 本文件是诊断事件的**装配点**（US-16 / T095 / T097；颗粒度文档 §3.1.6、R23 裁决 3）：
// 把配置段的两个开关（保留期 / 采样率）、存储适配器与脱敏器拼成运行期的读写两件。
//
// 三点口径：
//
//   - **配置非法即拒绝启动**：`diagnostics.NewConfig` 拒绝负保留期与越界采样率。这与
//     其他装配期失败同一条纪律——错配置在启动时暴露，不带着它跑，也不悄悄改成缺省值
//     （`retention_days: -1` 会读成"永不过期"，那是另一种意思）。
//   - **采样率未写 = 1.0**：`SampleRateOrDefault` 是配置面唯一的口径来源，本文件不
//     再补一份默认值——两处各写一个默认值迟早会分叉。
//   - **读面与写面共用同一个 store 实例**：诊断的读与写必须落在同一张表、同一条连接池
//     上，否则"刚发生的调用查不到"会被误判成生产端没发。
//
// This file assembles the diagnostics reader and recorder from the configuration section.
// An invalid setting refuses startup, the sampling default has exactly one owner, and the
// read and write faces share the same store instance.

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// newDiagnosticsPortsWithMetrics 构造诊断的写入器、读取器及可选指标回调。调用方持有实例：
// 写入器进运行路径（`AdmissionDeps.Diagnostics`），读取器进管理面
// （`web.Options.Diagnostics`）。
// newDiagnosticsPortsWithMetrics builds the recorder, reader and optional metric callback.
func newDiagnosticsPortsWithMetrics(cfg cli.DiagnosticsConfig, db *postgres.DB, now func() time.Time,
	logger *slog.Logger, metrics *observability.MetricsCollector,
) (*diagnostics.Recorder, *diagnostics.Reader, error) {
	if db == nil {
		// 重路径恒有库句柄；nil 只可能来自被绕过的装配。与 web 侧同一口径——
		// 缺端口按失败关闭处理，不在这里造一个"什么也不写"的假诊断。
		return nil, nil, fmt.Errorf("诊断装配缺少数据库句柄")
	}
	settings, err := diagnostics.NewConfig(cfg.RetentionDays, cfg.SampleRateOrDefault())
	if err != nil {
		return nil, nil, fmt.Errorf("诊断配置: %w", err)
	}
	store := postgres.NewDiagnosticStore(db)
	recorder := diagnostics.NewRecorder(diagnostics.RecorderOptions{
		OnResult: diagnosticMetricResult(metrics),
		Store:    store,
		Sampler:  diagnostics.NewRateSampler(settings),
		Redactor: diagnostics.NewWhitelistRedactor(),
		Now:      now,
		Logger:   logger,
	})
	reader := diagnostics.NewReader(store, settings, diagnostics.NewWhitelistRedactor())
	return recorder, reader, nil
}

func diagnosticMetricResult(metrics *observability.MetricsCollector) func(diagnostics.Kind, string) {
	if metrics == nil {
		return nil
	}
	return func(kind diagnostics.Kind, result string) {
		metrics.IncCounter("obs_diagnostic_events_total", map[string]string{"kind": string(kind), "result": result})
		if result == "write_failed" {
			metrics.IncCounter("obs_diagnostic_write_failures_total", map[string]string{"kind": string(kind)})
		}
	}
}
