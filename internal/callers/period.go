package callers

// 本文件实现图 6 的周期边界（颗粒度文档 §2.3「周期、边界与恢复规则」表 / T024）：
// 周期只有 `day` / `month` 两值，按 `caller_limits.timezone`（IANA 名，缺省 UTC）的
// **本地** 00:00 起算，月周期为该月 1 日 00:00，结果统一折算成 UTC 瞬时存 Unix 毫秒。
// 进程时区**不**参与任何计算——引进程本地时区会让同一份配置在不同机器上切出不同
// 的周期行（判据见 `TestRepoInvariants_NoProcessLocalTimezone`）。
// This file owns the period boundary: day/month only, local midnight in the caller's
// IANA timezone, returned as a UTC instant. The process timezone never participates.

import (
	"fmt"
	"strings"
	"time"

	// 单二进制交付环境（含最小容器）不带 IANA 时区库，嵌一份 tzdata 才能解析
	// `caller_limits.timezone`；这是标准库包，不进 go.mod，也不改变任何既有行为。
	// The embedded tzdata keeps timezone names resolvable in a bare single binary.
	_ "time/tzdata"
)

// DefaultTimezone 是 `caller_limits.timezone` 的缺省值（与 DDL 默认一致）。
// DefaultTimezone matches the DDL default for caller_limits.timezone.
const DefaultTimezone = "UTC"

// Period 是配额周期类型；只有 `day` / `month` 两值，其他取值拒绝配置。
// Period is the closed two-value set of quota periods; anything else is rejected.
type Period string

const (
	// PeriodDay 表示按自然日（调用方时区本地 00:00）切分。
	// PeriodDay is one local calendar day.
	PeriodDay Period = "day"
	// PeriodMonth 表示按自然月（当月 1 日 00:00）切分。
	// PeriodMonth is one local calendar month.
	PeriodMonth Period = "month"
)

// Valid 判定周期是否在闭集内；空值与未知值一律拒绝（**不得**默认成 day）。
// Valid accepts exactly the two literals; empty and unknown values fail.
func (p Period) Valid() bool {
	switch p {
	case PeriodDay, PeriodMonth:
		return true
	default:
		return false
	}
}

// PeriodStart 返回包含 `at` 的那个周期的起点（UTC 瞬时）。`timezone` 为空按
// `DefaultTimezone`；非法 IANA 名与未知周期都返回错误，**不**静默退回 UTC。
// A DST transition that removes local midnight normalises to the first valid instant.
func PeriodStart(p Period, timezone string, at time.Time) (time.Time, error) {
	loc, err := loadCallerLocation(timezone)
	if err != nil {
		return time.Time{}, err
	}
	local := at.In(loc)
	switch p {
	case PeriodDay:
		return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).UTC(), nil
	case PeriodMonth:
		return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc).UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("callers: 未知配额周期 %q", string(p))
	}
}

// NextPeriodStart 返回下一个周期的起点（UTC）；429 `QUOTA_EXHAUSTED` 的 Retry-After
// 与「跨周期提交」用例都以它为唯一口径。日期按**本地**日历推进——夏令时切换日
// 只有 23 或 25 小时，直接给 UTC 加 24 小时会算错边界。
// NextPeriodStart advances on the local calendar, never by adding 24 hours to UTC.
func NextPeriodStart(p Period, timezone string, at time.Time) (time.Time, error) {
	loc, err := loadCallerLocation(timezone)
	if err != nil {
		return time.Time{}, err
	}
	local := at.In(loc)
	switch p {
	case PeriodDay:
		return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, loc).UTC(), nil
	case PeriodMonth:
		return time.Date(local.Year(), local.Month()+1, 1, 0, 0, 0, 0, loc).UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("callers: 未知配额周期 %q", string(p))
	}
}

// loadCallerLocation 解析 IANA 时区名；空值取缺省 UTC，解析失败按错误返回。
// loadCallerLocation resolves the IANA name; an empty value means UTC.
func loadCallerLocation(timezone string) (*time.Location, error) {
	name := strings.TrimSpace(timezone)
	if name == "" {
		name = DefaultTimezone
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("callers: 无效的 IANA 时区 %q: %w", name, err)
	}
	return loc, nil
}
