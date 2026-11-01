package diagnostics

// 本文件决定诊断段的**生效配置**（颗粒度文档 §2.9「B 提供有界的基础保留与采样配置」、
// tasks.md T095a / FR-065）：`internal/cli` 的 `DiagnosticsConfig` 只如实解析"用户写了
// 什么"，本文件决定"没写时生效什么"。两处**不得**各写一份缺省值——缺省值只在这里有
// 一份，配置层读不到它的存在（它不 import 本包），因此由装配层把两段拼起来。
// This file owns the effective diagnostics settings; the CLI section only records what
// the operator wrote, so the defaults live here and nowhere else.

import "fmt"

const (
	// DefaultRetentionDays 是保留期的缺省值（`diagnostics.retention_days` 未写时生效）。
	// 七天是**有界**的基础值：诊断表不得无界增长，长期保留策略归 OBS（US-16A（拟））。
	// DefaultRetentionDays is the bounded default; long-term retention belongs to OBS.
	DefaultRetentionDays = 7

	// DefaultSampleRate 是采样率的缺省值：1.0 = 全量保留，不丢事件。
	// DefaultSampleRate keeps every event; sampling is an explicit operator choice.
	DefaultSampleRate = 1.0
)

// Config 是诊断段的生效配置：保留期与采样率。
// Config is the effective diagnostics configuration.
type Config struct {
	// RetentionDays 是诊断事件保留期（天）。0 = 未配置 → `DefaultRetentionDays`。
	// RetentionDays is the retention window in days; zero means unset.
	RetentionDays int
	// SampleRate 是采样率（0–1）；**审计不受它影响**。
	// SampleRate is the sampling ratio and never affects audit.
	SampleRate float64
}

// NewConfig 校验并补默认值。负数保留期与越界采样率一律拒绝——静默纠正会把"配错了"
// 显示成"配好了"，而这两项直接决定"证据还在不在"。
// NewConfig validates and fills defaults; out-of-range values are refused rather than
// silently corrected, because both settings decide whether evidence still exists.
func NewConfig(retentionDays int, sampleRate float64) (Config, error) {
	if retentionDays < 0 {
		return Config{}, fmt.Errorf("诊断保留期不得为负（当前 %d）", retentionDays)
	}
	if sampleRate < 0 || sampleRate > 1 {
		return Config{}, fmt.Errorf("诊断采样率必须落在 0–1（当前 %v）", sampleRate)
	}
	if retentionDays == 0 {
		retentionDays = DefaultRetentionDays
	}
	return Config{RetentionDays: retentionDays, SampleRate: sampleRate}, nil
}

// DefaultConfig 返回全默认配置（保留期缺省、全量采样）。
// DefaultConfig returns the all-defaults configuration.
func DefaultConfig() Config {
	return Config{RetentionDays: DefaultRetentionDays, SampleRate: DefaultSampleRate}
}

// Samples 报告一次随机抽样值是否应当保留：`roll` 落在 `[0, SampleRate)` 才留。
// 采样率 1 恒定保留（`roll` 取不到 1），采样率 0 恒定丢弃。
// Samples reports whether a uniform roll in [0,1) keeps the event.
func (c Config) Samples(roll float64) bool {
	return roll < c.SampleRate
}
