package diagnostics

// 本文件是诊断事件的**写入路径**（颗粒度文档 §2.9 / §3.1.6、tasks.md T095）。
//
// 三条纪律，每条都对应一条宪法级分界：
//
//  1. **无返回值**：`Record` 不返回错误、不进调用方的错误链。诊断**可丢弃**，写失败
//     只记 WARN——把它接进业务错误通道，就等于让"观测失败"变成"业务失败"，与审计的
//     fail-closed 正好相反（原则 V 只管审计）。
//  2. **先校验再落库**：形状非法的调用是**代码缺陷**，不是数据问题，因此在这里就挡住，
//     不让一句 `CHECK` 违例进到数据库日志里。采样在**校验之后、脱敏之前**判定：被丢弃的
//     事件不占脱敏预算，也不必为它洗一遍 attrs。
//  3. **落库前必脱敏**：`Redacted` 是唯一入口（`redact.go`），本文件不自己拼规则；
//     `nil` 脱敏器回落白名单实现，所以"忘了注入"仍然是 fail-safe 而不是 fail-open。
//
// This file is the write path. Record never returns an error: diagnostics are droppable,
// so a failed write is a WARN and never travels back into the caller's error chain. Events
// are validated before sampling and redacted before storage, through the single redactor.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"log/slog"
	"sync/atomic"
	"time"
)

// EventStore 是诊断写入的**消费方端口**：实现放 `internal/storage/postgres`
// （`*postgres.DiagnosticStore`），本包只声明形状。端口只有一个方法——写入路径不该
// 顺手拿到读面（读面的缺失语义归 `Reader`）。
// EventStore is the write port; the implementation lives in storage. It exposes exactly
// one method so the write path cannot reach for the read side.
type EventStore interface {
	// InsertEvent 落一条已脱敏的事件；失败即"这条丢了"，重试与否归实现，调用方不重试。
	// InsertEvent persists one redacted event; a failure means the event is lost.
	InsertEvent(ctx context.Context, ev DiagnosticEvent) error
}

// Sampler 是采样策略（§3.1.6）：返回 false = 丢弃这条事件。
// Sampler decides whether one event survives; false drops it.
type Sampler interface {
	Sample() bool
}

// RateSampler 是按采样率抽样的实现：每次判定取一个 `[0,1)` 均匀随机数，落在
// `[0, SampleRate)` 才保留。随机源可注入，采样判定因此可被用例固定（否则"采样 0 一定
// 丢弃"这类断言只能靠概率成立）。
// RateSampler keeps an event when a uniform roll falls below the configured rate. The
// roll source is injectable so sampling assertions are deterministic.
type RateSampler struct {
	config Config
	roll   func() float64
}

// NewRateSampler 按生效配置构造采样器；随机源使用crypto/rand。
// NewRateSampler builds the sampler with the cryptographic source.
func NewRateSampler(config Config) *RateSampler {
	return &RateSampler{config: config, roll: sampleFraction}
}

// sampleFraction 生成[0,1)均匀样本；熵失败时丢弃可丢弃诊断，不传播业务错误。
// sampleFraction samples uniformly in [0,1), dropping diagnostics if entropy fails.
func sampleFraction() float64 {
	return sampleFractionFrom(rand.Reader)
}

func sampleFractionFrom(reader io.Reader) float64 {
	var raw [8]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return 1
	}
	return float64(binary.BigEndian.Uint64(raw[:])>>11) / (1 << 53)
}

// WithRoll 换掉随机源并返回自身（链式；只给用例与需要可复现的装配使用）。
// WithRoll replaces the roll source, for deterministic tests and reproducible wiring.
func (s *RateSampler) WithRoll(roll func() float64) *RateSampler {
	if roll != nil {
		s.roll = roll
	}
	return s
}

// Sample 实现 Sampler：判定规则只有 `Config.Samples` 一份。
// Sample delegates to Config.Samples so the rule has exactly one implementation.
func (s *RateSampler) Sample() bool {
	if s == nil {
		return true
	}
	return s.config.Samples(s.roll())
}

// Recorder 是诊断事件的写入口（§3.1.6 的 `Recorder` 行）。
// Recorder is the single write entry point for diagnostic events.
type Recorder struct {
	store    EventStore
	sampler  Sampler
	redactor Redactor
	now      func() time.Time
	logger   *slog.Logger
	// sampledOut 统计被采样丢弃的条数（只作运行期读数，不落库）。
	// sampledOut counts dropped events; it is a runtime reading, never persisted.
	sampledOut *counter
	onResult   func(Kind, string)
}

// RecorderOptions 是 `NewRecorder` 的显式依赖集。全部字段可空，空即取安全缺省：
// 无 store = 诊断关闭（全部丢弃）；无 sampler = 全量保留；无 redactor = 白名单脱敏；
// 无时钟 = 系统时钟；无日志器 = 进程缺省。
// RecorderOptions is the explicit dependency set; every zero value has a safe default.
type RecorderOptions struct {
	// OnResult 只接收固定kind/result，用于进程指标；不返回业务错误。
	// OnResult receives fixed kind/result values for process metrics, without an error channel.
	OnResult func(Kind, string)
	Store    EventStore
	Sampler  Sampler
	Redactor Redactor
	Now      func() time.Time
	Logger   *slog.Logger
}

// NewRecorder 组装写入路径。**无 store 时不 panic、不失败**：实例可以在诊断未启用的
// 装配里存在，`Record` 变成空操作——扫描式调用点因此不必到处判空。
// NewRecorder assembles the write path; a missing store makes Record a no-op rather than
// a startup failure, so call sites never need to special-case a diagnostics-off instance.
func NewRecorder(opts RecorderOptions) *Recorder {
	recorder := &Recorder{
		store:      opts.Store,
		sampler:    opts.Sampler,
		redactor:   opts.Redactor,
		now:        opts.Now,
		logger:     opts.Logger,
		sampledOut: &counter{},
		onResult:   opts.OnResult,
	}
	if recorder.now == nil {
		recorder.now = time.Now
	}
	if recorder.logger == nil {
		recorder.logger = slog.Default()
	}
	return recorder
}

// Record 记录一条诊断事件。**无返回值**：任何失败都只记 WARN，调用方照常继续。
// 缺 `EventID` / `OccurredAt` 时在这里补齐，调用方只需给语义字段（种类、关联、判定）。
// Record persists one event and never returns an error; a missing id or timestamp is
// filled in here so callers only supply the semantic fields.
func (r *Recorder) Record(ctx context.Context, ev DiagnosticEvent) {
	if r == nil || r.store == nil {
		return
	}
	if ev.EventID == "" {
		id, err := NewEventID(r.now())
		if err != nil {
			r.result(ev.Kind, "write_failed")
			r.logger.Warn("诊断事件标识生成失败——事件已丢弃", "kind", string(ev.Kind), "error", err)
			return
		}
		ev.EventID = id
	}
	// 版本号由写入路径固定：调用方无法传错版本，也就没有"传错版本"的用例。
	// The version is fixed by the write path so a caller cannot get it wrong.
	ev.SchemaVersion = SchemaVersion
	if ev.OccurredAt == 0 {
		ev.OccurredAt = r.now().UTC().UnixMilli()
	}
	if err := ev.Validate(); err != nil {
		r.result(ev.Kind, "write_failed")
		r.logger.Warn("诊断事件形状非法——事件已丢弃", "kind", string(ev.Kind), "error", err)
		return
	}
	if r.sampler != nil && !r.sampler.Sample() {
		// 采样丢弃是**正常路径**：不记 WARN，只计数（否则开启采样后日志自己成了噪声源）。
		// A sampled-out event is a normal outcome: counted, never logged as a warning.
		r.sampledOut.add(1)
		r.result(ev.Kind, "sampled_out")
		return
	}
	redacted := ev.Redacted(r.redactor)
	if err := r.store.InsertEvent(ctx, redacted); err != nil {
		r.result(ev.Kind, "write_failed")
		// 诊断**可丢弃**：写失败不影响业务，也不重试（重试会在故障期放大压力）。
		// Diagnostics are droppable: no retry, and the business path never learns of it.
		r.logger.Warn("诊断事件写入失败——已丢弃", "event_id", redacted.EventID,
			"kind", string(redacted.Kind), "error", err)
		return
	}
	r.result(ev.Kind, "recorded")
}

func (r *Recorder) result(kind Kind, result string) {
	if r.onResult != nil && kind.Valid() {
		defer func() {
			if recover() != nil {
				r.logger.Warn("diagnostic metric callback failed", "kind", string(kind), "error_kind", "metric_callback_failed")
			}
		}()
		r.onResult(kind, result)
	}
}

// SampledOut 返回本实例被采样丢弃的事件条数（运行期读数；进程重启清零）。
// SampledOut reports how many events this instance dropped to sampling.
func (r *Recorder) SampledOut() int64 {
	if r == nil || r.sampledOut == nil {
		return 0
	}
	return r.sampledOut.load()
}

// counter 是无锁的进程内计数（诊断读数不需要跨实例一致的全局序号）。
// counter is a lock-free in-process count; diagnostics promise no global order.
type counter struct{ value atomic.Int64 }

func (c *counter) add(delta int64) { c.value.Add(delta) }

func (c *counter) load() int64 { return c.value.Load() }
