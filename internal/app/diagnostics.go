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
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// newDiagnosticsPorts 构造诊断的写入器与读取器。返回的两个实例都由调用方持有：
// 写入器进运行路径（`AdmissionDeps.Diagnostics`），读取器进管理面
// （`web.Options.Diagnostics`）。
// newDiagnosticsPorts builds the recorder (run path) and the reader (management surface).
func newDiagnosticsPorts(cfg cli.DiagnosticsConfig, db *postgres.DB, now func() time.Time,
	logger *slog.Logger,
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
		Store:    store,
		Sampler:  diagnostics.NewRateSampler(settings),
		Redactor: diagnostics.NewWhitelistRedactor(),
		Now:      now,
		Logger:   logger,
	})
	reader := diagnostics.NewReader(store, settings, diagnostics.NewWhitelistRedactor())
	return recorder, reader, nil
}
