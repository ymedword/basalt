package callers

// 本文件覆盖 T024 的周期边界验收点（颗粒度文档 §2.3「周期、边界与恢复规则」表 /
// 图 6 [5] 的 period_start）：day / month 只在调用方时区的本地 00:00 切分，同一瞬时
// 换一种 location 表示不得切出不同周期行，夏令时切换日按本地日历推进（不是 UTC +
// 24h），`at` 的时区不影响结果，非法时区与未知周期一律拒绝。
// The tests pin T024: local-midnight period starts in the caller's timezone, the same
// instant under any location representation, DST-day calendar advance, and rejection
// of unknown periods or timezone names.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestQuota_PeriodBoundaryAndTimezone 是周期边界的判别力用例。
// 文档原名：「周期边界与时区」——UTC 23:59 的瞬时在 Asia/Shanghai 已跨日，用 `at.UTC()`
// 或进程时区直接算会切错周期行。
// 逆改点：把 `at.In(loc)` 去掉（直接用 `at` 的字段）或以 `time.Local` 计算 → 前两组断言红。
func TestQuota_PeriodBoundaryAndTimezone(t *testing.T) {
	// UTC 2026-10-26 23:59 = Asia/Shanghai 2026-10-27 07:59，本地日从 10-26T16:00Z 起算。
	at := time.Date(2026, 10, 26, 23, 59, 0, 0, time.UTC)

	shanghai, err := PeriodStart(PeriodDay, "Asia/Shanghai", at)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 26, 16, 0, 0, 0, time.UTC), shanghai,
		"东八区本地 00:00 折算是前一日 16:00Z")

	utc, err := PeriodStart(PeriodDay, "", at)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), utc,
		"缺省时区按 UTC 起算，且不得与上海同值")

	// 同一瞬时换一种 location 表示，周期起点必须一致（结果只由瞬时决定）。
	atShanghai := at.In(mustLocation(t, "Asia/Shanghai"))
	again, err := PeriodStart(PeriodDay, "Asia/Shanghai", atShanghai)
	require.NoError(t, err)
	require.Equal(t, shanghai, again, "入参的时区表示不得影响周期起点")

	// 下一个日周期同样按本地日历推进。
	next, err := NextPeriodStart(PeriodDay, "Asia/Shanghai", at)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 27, 16, 0, 0, 0, time.UTC), next)
}

// TestQuota_PeriodMonthBoundary 判月周期：起算点是调用方时区当月 1 日 00:00，
// 跨月与跨年都由本地日历推进。
// 逆改点：把月起点写成 `at` 所在地的 1 日（不折算时区）→ 第一组断言红。
func TestQuota_PeriodMonthBoundary(t *testing.T) {
	// UTC 2026-10-31 23:30 = 上海 2026-11-01 07:30，本地月已翻页。
	at := time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC)

	start, err := PeriodStart(PeriodMonth, "Asia/Shanghai", at)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 31, 16, 0, 0, 0, time.UTC), start,
		"上海 11-01 00:00 = 10-31T16:00Z")

	next, err := NextPeriodStart(PeriodMonth, "Asia/Shanghai", at)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 11, 30, 16, 0, 0, 0, time.UTC), next,
		"下一个月的 1 日 00:00 本地时间")

	// 跨年：UTC 2026-12-15 落在 2027-01 之外，下一周期必须翻年。
	dec := time.Date(2026, 12, 15, 12, 0, 0, 0, time.UTC)
	jan, err := NextPeriodStart(PeriodMonth, "", dec)
	require.NoError(t, err)
	require.Equal(t, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), jan)
}

// TestQuota_PeriodDaylightSavingDay 判夏令时切换日：本地日历上的一天只有 23 或 25
// 小时，给 UTC 加 24 小时会落到错误边界。
// 逆改点：把 `NextPeriodStart` 改成 `start.Add(24 * time.Hour)` → 第二条断言红。
func TestQuota_PeriodDaylightSavingDay(t *testing.T) {
	// 2026-03-08 为美东夏令时开始日：本地 00:00 EST = 05:00Z，次日 00:00 EDT = 04:00Z。
	at := time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC)

	start, err := PeriodStart(PeriodDay, "America/New_York", at)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), start)

	next, err := NextPeriodStart(PeriodDay, "America/New_York", at)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC), next,
		"夏令时切换日只有 23 小时")
	require.NotEqual(t, start.Add(24*time.Hour), next, "不得用 UTC + 24h 代替本地日历")
}

// TestQuota_PeriodRejectsInvalidInput 判失败关闭：未知周期、非法 IANA 名都返回错误，
// 不回退 day、不静默用 UTC 顶上；空时区按缺省 UTC。
// 逆改点：给 `Period` 加个 default 分支返回 day，或忽略 LoadLocation 的错误 → 断言红。
func TestQuota_PeriodRejectsInvalidInput(t *testing.T) {
	at := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	_, err := PeriodStart(Period("week"), "UTC", at)
	require.Error(t, err, "week 不是合法周期")
	_, err = NextPeriodStart(Period(""), "UTC", at)
	require.Error(t, err, "空周期必须拒绝，不得默认 day")

	_, err = PeriodStart(PeriodDay, "Mars/Olympus", at)
	require.Error(t, err, "非法 IANA 名必须拒绝")
	_, err = NextPeriodStart(PeriodMonth, "Asia/Shangha1", at)
	require.Error(t, err)

	// 空时区不是错误，按缺省 UTC 处理。
	start, err := PeriodStart(PeriodDay, "  ", at)
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), start)

	require.True(t, PeriodDay.Valid())
	require.True(t, PeriodMonth.Valid())
	require.False(t, Period("DAY").Valid(), "大小写不敏感不算合法；DDL CHECK 只认小写字面量")
}

// mustLocation 加载时区；加载失败直接终止用例（embedded tzdata 保证可用）。
// mustLocation loads a location, failing the test if the name is unavailable.
func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}
