// Package cronadapter 封装 robfig/cron v3.0.1，只做一件事：按表达式算下一次触发点。
// Package cronadapter wraps robfig/cron v3.0.1 for one purpose only: computing the next
// occurrence of an expression.
//
// US-24 起触发时机由数据库的 `next_fire_at` 承载（到期扫描 + 领取事务），进程内不再
// 有 cron 循环、任务句柄或 Stop 生命周期——本包因此收敛为「解析器 + Next / 枚举」，
// 不再持有可启动的 cron.Cron 实例。业务规则（时区前缀、校验子集、任务状态）不在这里，
// 本包只提供解析与枚举（颗粒度文档 §3.1.4 / 约束 4）。
// Since US-24 the trigger timeline lives in the database, so this package holds no
// runnable cron instance: it parses six-field specs and answers "next occurrence".
package cronadapter

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/ymedword/basalt/internal/core"
)

// cronTZPrefix 是任务级时区前缀（契约 agent-md §3.1）。只有 workspace.CronSpec 生成它，
// 本包只做"必须有"的守门：robfig 缺省按服务器本地时区解释，放行即把本机时区带进触发。
// cronTZPrefix is the per-task timezone prefix; only workspace.CronSpec generates it,
// and this package refuses specs without it (constraint 5).
const cronTZPrefix = "CRON_TZ="

// Engine 是 cron 引擎的 robfig 实现（core.CronEngine）：只解析与枚举，不调度。
// Engine is the robfig-backed core.CronEngine implementation. It parses and enumerates;
// it never schedules anything itself.
type Engine struct {
	parser cron.Parser
	logger *slog.Logger
}

// 编译期断言：Engine 满足消费方（core）声明的引擎接口。
// Compile-time assertion that Engine satisfies the consumer-declared interface.
var _ core.CronEngine = (*Engine)(nil)

// New 构造 6 段解析器（秒 分 时 日 月 周）与引擎；时区由 CRON_TZ= 前缀随 spec 携带
// （robfig v3.0.1 无 parser.WithLocation，见颗粒度文档注 1）。
// New builds the six-field parser (second minute hour dom month dow); time zones travel
// with each spec via the CRON_TZ= prefix.
func New(logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{parser: newSixFieldParser(), logger: logger}
}

// newSixFieldParser 构造 6 段解析器（秒 分 时 日 月 周）；引擎与不持有引擎的枚举
// 入口（管理台定时候选的未来三次）共用这一份构造，段数规则只有一处。
// newSixFieldParser is the single six-field parser constructor shared by the engine and
// the engine-less enumeration entry point.
func newSixFieldParser() cron.Parser {
	return cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
}

// Next 返回严格晚于 from 的下一次触发点。缺少 `CRON_TZ=` 前缀或表达式非法（含 5 段）
// 一律返回错误——调度器的到期扫描据此跳过并记 Warn，**不得**退回本机时区。
// Next returns the first occurrence strictly after from. A missing CRON_TZ= prefix or an
// invalid expression (including five-field specs) is an error: the due scan logs and
// skips the task rather than falling back to the server's local zone.
func (e *Engine) Next(spec string, from time.Time) (time.Time, error) {
	if !strings.HasPrefix(spec, cronTZPrefix) {
		return time.Time{}, fmt.Errorf("cron: spec 缺少 %s 前缀（时区必须随任务携带，约束 5）", cronTZPrefix)
	}
	schedule, err := e.parser.Parse(spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("cron: 解析表达式 %q: %w", spec, err)
	}
	next := schedule.Next(from)
	if next.IsZero() {
		return time.Time{}, errors.New("cron: 表达式已无后续触发点")
	}
	return next, nil
}

// Occurrences 从 from 起循环 schedule.Next，最多 limit 个且不超过 to。
// 解析失败返回空切片——调用方已在校验期拒绝非法 spec，这里只是防御。
// Occurrences enumerates at most limit trigger times in [from, to]; a parse failure
// yields an empty slice because callers already validated the spec.
func (e *Engine) Occurrences(spec string, from, to time.Time, limit int) []time.Time {
	return occurrences(e.parser, e.logger, spec, from, to, limit)
}

// ParseOccurrences 是不持有 Engine 的调用方（管理台定时候选的未来三次触发时间）
// 复用同一份 6 段解析规则与枚举算法的入口：spec 必须已带 `CRON_TZ=` 前缀（由
// workspace.CronSpec 生成），解析失败返回空切片。
// ParseOccurrences lets engine-less callers enumerate with the same parser and
// algorithm; the spec must already carry the CRON_TZ= prefix.
func ParseOccurrences(spec string, from, to time.Time, limit int) []time.Time {
	return occurrences(newSixFieldParser(), nil, spec, from, to, limit)
}

// occurrences 是唯一的枚举实现：从 from 起循环 Next，最多 limit 个且不超过 to。
// 解析失败返回空切片（调用方已在校验期拒绝非法 spec）；logger 非 nil 时留一条 Debug，
// 让"窗口内没有触发点"与"表达式解析不了"这两条沉默路径在排障时可分辨。
// occurrences is the single enumeration algorithm behind both entry points; a parse
// failure yields an empty slice and, when a logger is present, one Debug line so the
// silent "no occurrence" and "unparseable spec" paths stay distinguishable.
func occurrences(parser cron.Parser, logger *slog.Logger, spec string, from, to time.Time, limit int) []time.Time {
	if limit <= 0 {
		return nil
	}
	schedule, err := parser.Parse(spec)
	if err != nil {
		if logger != nil {
			logger.Debug("cron 表达式解析失败，返回空枚举", "spec", spec, "error", err)
		}
		return nil
	}

	var out []time.Time
	at := from
	for len(out) < limit {
		next := schedule.Next(at)
		if next.IsZero() || next.After(to) {
			break
		}
		out = append(out, next)
		at = next
	}
	return out
}
