// Package cronadapter 封装 robfig/cron v3.0.1，供调度器注册与枚举触发点。
// Package cronadapter wraps robfig/cron v3.0.1 for the scheduler: registration and
// occurrence enumeration. 业务规则（时区字段解析、任务状态、校验子集）不在这里——
// 本包只做解析器构造、任务增删与固定区间枚举（颗粒度文档 §3.1.3 / 约束 4）。
package cronadapter

import (
	"context"
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

// Engine 是 cron 引擎的 robfig 实现（core.CronEngine）。
// Engine is the robfig-backed implementation of core.CronEngine.
type Engine struct {
	cron   *cron.Cron
	parser cron.Parser
	logger *slog.Logger
}

// 编译期断言：Engine 满足消费方（core）声明的引擎接口。
// Compile-time assertion that Engine satisfies the consumer-declared interface.
var _ core.CronEngine = (*Engine)(nil)

// New 构造 6 段解析器（秒 分 时 日 月 周）与引擎；时区由 CRON_TZ= 前缀随 spec 携带
// （robfig v3.0.1 无 parser.WithLocation，见颗粒度文档注 1）。
// New builds the six-field parser (second minute hour dom month dow) and the engine;
// time zones travel with each spec via the CRON_TZ= prefix.
func New(logger *slog.Logger) *Engine {
	p := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	return &Engine{
		cron:   cron.New(cron.WithParser(p), cron.WithLogger(cronLogger{logger: logger})),
		parser: p,
		logger: logger,
	}
}

// Add 注册一个任务并返回停止句柄。缺少 CRON_TZ= 前缀或表达式非法（含 5 段）时拒绝注册。
// Add registers a job and returns its stop handle; specs without the CRON_TZ= prefix or
// with an invalid expression (including five-field specs) are refused.
func (e *Engine) Add(spec string, job func()) (core.CronHandle, error) {
	if !strings.HasPrefix(spec, cronTZPrefix) {
		return nil, fmt.Errorf("cron: spec 缺少 %s 前缀（时区必须随任务携带，约束 5）", cronTZPrefix)
	}
	id, err := e.cron.AddFunc(spec, job)
	if err != nil {
		return nil, fmt.Errorf("cron: 注册任务 %q: %w", spec, err)
	}
	return &handle{engine: e, id: id}, nil
}

// Start 启动引擎调度循环（非阻塞）。
// Start starts the engine's scheduling loop without blocking.
func (e *Engine) Start() { e.cron.Start() }

// Stop 停止调度循环并返回"在途任务全部结束"时关闭的 ctx（robfig 语义）。
// Stop stops the scheduling loop and returns a context closed once running jobs finish.
func (e *Engine) Stop() context.Context { return e.cron.Stop() }

// Occurrences 从 from 起循环 schedule.Next，最多 limit 个且不超过 to。
// 解析失败返回空切片——调用方已在校验期拒绝非法 spec，这里只是防御。
// Occurrences enumerates at most limit trigger times in [from, to]; a parse failure
// yields an empty slice because callers already validated the spec.
func (e *Engine) Occurrences(spec string, from, to time.Time, limit int) []time.Time {
	if limit <= 0 {
		return nil
	}
	schedule, err := e.parser.Parse(spec)
	if err != nil {
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

// handle 绑定引擎与 robfig 的条目 id：Stop 只摘掉这一个任务，多次调用幂等。
// handle binds the engine and the robfig entry id; Stop removes just that entry.
type handle struct {
	engine *Engine
	id     cron.EntryID
}

// 编译期断言：handle 满足 cron.CronHandle。
var _ core.CronHandle = (*handle)(nil)

// Stop 摘除本条注册（robfig Remove 幂等）。
// Stop removes this registration.
func (h *handle) Stop() { h.engine.cron.Remove(h.id) }

// cronLogger 把 robfig 的 Logger 接口接到 slog：错误进 error 字段，键值原样透传。
// cronLogger adapts robfig's Logger interface onto slog.
type cronLogger struct{ logger *slog.Logger }

// Info 透传常规消息。
// Info forwards routine messages.
func (l cronLogger) Info(msg string, keysAndValues ...any) {
	l.logger.Info(msg, keysAndValues...)
}

// Error 透传错误，并以 error 字段携带错误值（字段纪律，EngineeringStandards §4）。
// Error forwards errors, carrying the error value in the error field.
func (l cronLogger) Error(err error, msg string, keysAndValues ...any) {
	args := make([]any, 0, len(keysAndValues)+2)
	args = append(args, keysAndValues...)
	args = append(args, "error", err)
	l.logger.Error(msg, args...)
}
