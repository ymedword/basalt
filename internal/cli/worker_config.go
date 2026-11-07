package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/flow"
)

// 本文件是分布式执行的三个配置段（US-24 §3.3，review ① 冻结）：`worker` /
// `scheduler` / `assets`。三段都走**显式解析入口**而不是普通 yaml 解码：yaml
// 解码器对未知键静默忽略，而拼错的键（例如 `lease_ttl_second`）会让进程在
// 缺省租约时长上跑起来——配置看着改了、行为没变，是运维最难查的一类。
// This file carries the three distributed-execution sections frozen by US-24 §3.3:
// worker / scheduler / assets. They decode through an explicit entry point because a
// silently-ignored misspelled key would leave the process on defaults.

// WorkerConfig 是 `worker` 段：租约时长、续约节奏、回收节奏、总在途上限与停机宽限。
// 所有值都是**候选默认**（§3.3），装载期逐条校验，不静默取缺省。
// WorkerConfig is the `worker` section: lease timing, reclaim cadence, the total
// in-flight budget and the shutdown grace. Every value is validated at load time.
type WorkerConfig struct {
	// Name 仅作诊断：空 = hostname；**允许重复**——身份是 InstanceID 不是 Name。
	Name string `json:"name"`
	// LeaseTTLSeconds 是执行权有效期；续约与接管判定都以数据库时间为准。
	LeaseTTLSeconds int `json:"lease_ttl_seconds"`
	// RenewIntervalSeconds 必须 < LeaseTTLSeconds / 2：续约节奏贴近 TTL 会让一次
	// 网络抖动直接跨过到期点。
	RenewIntervalSeconds int `json:"renew_interval_seconds"`
	// ReclaimIntervalSeconds 是回收循环的间隔，不得超过 TTL（超过就等于"过期后
	// 还要再等一个 TTL 才有人收尾"）。
	ReclaimIntervalSeconds int `json:"reclaim_interval_seconds"`
	// ReclaimBatch 是单轮回收的条数上限；回收是固定有界的维护循环，不执行业务推进。
	ReclaimBatch int `json:"reclaim_batch"`
	// HeartbeatIntervalSeconds 只驱动 worker_nodes 的心跳（诊断），不参与任何归属判定。
	HeartbeatIntervalSeconds int `json:"heartbeat_interval_seconds"`
	// MaxInflight 是**三条执行路径共用**的总在途上限（交互 Run / 调度执行组 /
	// Flow 节点 Run），不是仅限后台的上限。
	MaxInflight int `json:"max_inflight"`
	// ShutdownGraceSeconds 必须在 flows.shutdown_grace_seconds + 5 与 HTTP 宽限 − 5
	// 之间：先收 Flow，再收 Worker，最后关连接，三段不能互相挤掉预算。
	ShutdownGraceSeconds int `json:"shutdown_grace_seconds"`
}

// SchedulerConfig 是 `scheduler` 段：到期扫描节奏、单轮领取上限与错失宽限。
// SchedulerConfig is the `scheduler` section: poll cadence, dispatch batch and the
// missed-window grace.
type SchedulerConfig struct {
	// PollIntervalMS 是数据库到期扫描的间隔（不是进程内 cron 定时器）。
	PollIntervalMS int `json:"poll_interval_ms"`
	// DispatchBatch 是单轮最多领取的到期任务数，且必须能被 worker.max_inflight 与
	// flows.max_active 共同容纳（装载期交叉校验）。
	DispatchBatch int `json:"dispatch_batch"`
	// MissedGraceSeconds 是错失窗口宽限：超过它只记一条 skipped 并推进到未来，
	// 不逐点补跑。
	MissedGraceSeconds int `json:"missed_grace_seconds"`
}

// AssetsConfig 是 `assets` 段：内容寻址 bundle 的根目录。
// AssetsConfig is the `assets` section: the content-addressed bundle root.
type AssetsConfig struct {
	// BundleRoot 必须非空——空值曾被解释为"取缺省"，与"校验失败即 fail-closed"
	// 直接矛盾，R3 已删除该二义。
	BundleRoot string `json:"bundle_root"`
}

// DefaultWorkerConfig 返回 §3.3 冻结的缺省值。段缺席时由解析入口填入，
// 管理面与装配面共用同一份缺省。
// DefaultWorkerConfig returns the frozen defaults of §3.3.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		LeaseTTLSeconds:          30,
		RenewIntervalSeconds:     10,
		ReclaimIntervalSeconds:   5,
		ReclaimBatch:             32,
		HeartbeatIntervalSeconds: 10,
		MaxInflight:              64,
		ShutdownGraceSeconds:     45,
	}
}

// DefaultSchedulerConfig 返回 `scheduler` 段缺省值。
// DefaultSchedulerConfig returns the defaults of the scheduler section.
func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{PollIntervalMS: 500, DispatchBatch: 32, MissedGraceSeconds: 300}
}

// DefaultAssetsConfig 返回 `assets` 段缺省值。
// DefaultAssetsConfig returns the defaults of the assets section.
func DefaultAssetsConfig() AssetsConfig {
	return AssetsConfig{BundleRoot: ".basalt/assets"}
}

// LeaseTTL 把秒换算成时长（调用方不再各自乘 time.Second）。
func (w WorkerConfig) LeaseTTL() time.Duration {
	return time.Duration(w.LeaseTTLSeconds) * time.Second
}

// RenewInterval 把续约间隔换算成时长。
func (w WorkerConfig) RenewInterval() time.Duration {
	return time.Duration(w.RenewIntervalSeconds) * time.Second
}

// ReclaimInterval 把回收间隔换算成时长。
func (w WorkerConfig) ReclaimInterval() time.Duration {
	return time.Duration(w.ReclaimIntervalSeconds) * time.Second
}

// HeartbeatInterval 把心跳间隔换算成时长。
func (w WorkerConfig) HeartbeatInterval() time.Duration {
	return time.Duration(w.HeartbeatIntervalSeconds) * time.Second
}

// ShutdownGrace 把停机宽限换算成时长。
func (w WorkerConfig) ShutdownGrace() time.Duration {
	return time.Duration(w.ShutdownGraceSeconds) * time.Second
}

// PollInterval 把到期扫描间隔换算成时长。
func (s SchedulerConfig) PollInterval() time.Duration {
	return time.Duration(s.PollIntervalMS) * time.Millisecond
}

// MissedGrace 把错失宽限换算成时长。
func (s SchedulerConfig) MissedGrace() time.Duration {
	return time.Duration(s.MissedGraceSeconds) * time.Second
}

// decodeSection 把 YAML 树里的一段按**未知键拒绝**的规则解到结构上；入参已经
// 预填缺省值，因此缺席字段保持缺省、显式写 null 一律拒绝（与 flows 同口径）。
// decodeSection decodes one section with unknown-key rejection over pre-seeded
// defaults; explicit nulls are refused like the flows section does.
func decodeSection(section any, present bool, target any, name string) error {
	if !present {
		return nil
	}
	fields, ok := section.(map[string]any)
	if !ok {
		return fmt.Errorf("%s 配置必须是映射", name)
	}
	for key, value := range fields {
		if value == nil {
			return fmt.Errorf("%s 配置不接受空值（键 %s）", name, key)
		}
	}
	raw, err := json.Marshal(section)
	if err != nil {
		return fmt.Errorf("%s 配置类型非法", name)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%s 配置包含未知键或非法类型", name)
	}
	return nil
}

// parseWorkerConfig 解析 `worker` 段并做**段内**校验；跨段校验在
// validateDistributedConfig 里完成（它同时需要 flows 的上限）。
// parseWorkerConfig parses the worker section; cross-section checks live in
// validateDistributedConfig because they need the flow limits too.
func parseWorkerConfig(section any, present bool) (WorkerConfig, error) {
	cfg := DefaultWorkerConfig()
	if err := decodeSection(section, present, &cfg, "worker"); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// parseSchedulerConfig 解析 `scheduler` 段。
// parseSchedulerConfig parses the scheduler section.
func parseSchedulerConfig(section any, present bool) (SchedulerConfig, error) {
	cfg := DefaultSchedulerConfig()
	if err := decodeSection(section, present, &cfg, "scheduler"); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// parseAssetsConfig 解析 `assets` 段。
// parseAssetsConfig parses the assets section.
func parseAssetsConfig(section any, present bool) (AssetsConfig, error) {
	cfg := DefaultAssetsConfig()
	if err := decodeSection(section, present, &cfg, "assets"); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// httpShutdownGrace 是既有 HTTP 服务的停机宽限（技术方案 §6.5：70s）。worker 段的
// 宽限必须给它留 5s，否则 HTTP 面的排空预算会被 Worker 层吃掉。
// httpShutdownGrace mirrors the existing 70s HTTP contract.
const httpShutdownGrace = 70 * time.Second

// validateDistributedConfig 逐条落地 §3.3 的全部校验规则。原则是**装载期拒绝**：
// 任何一条不成立都让启动失败，不静默取缺省、不把非法值解释成"没配"。
// validateDistributedConfig implements every rule of §3.3 as a load-time refusal.
func validateDistributedConfig(worker WorkerConfig, scheduler SchedulerConfig,
	assets AssetsConfig, flows flow.Config,
) error {
	if worker.LeaseTTLSeconds < 5 || worker.LeaseTTLSeconds > 300 {
		return fmt.Errorf("配置项 worker.lease_ttl_seconds 取值非法（%d）——只允许 5..300 秒",
			worker.LeaseTTLSeconds)
	}
	// 逐字按 §3.3：renew_interval_seconds < lease_ttl_seconds / 2（整数除法）。
	if worker.RenewIntervalSeconds >= worker.LeaseTTLSeconds/2 {
		return fmt.Errorf("配置项 worker.renew_interval_seconds（%d）必须小于 lease_ttl_seconds 的一半（%d）"+
			"——续约节奏贴近到期点，一次网络抖动就会跨过失权边界",
			worker.RenewIntervalSeconds, worker.LeaseTTLSeconds/2)
	}
	if worker.RenewIntervalSeconds < 1 {
		return fmt.Errorf("配置项 worker.renew_interval_seconds 必须为正（%d）", worker.RenewIntervalSeconds)
	}
	if worker.ReclaimIntervalSeconds < 1 || worker.ReclaimIntervalSeconds > 60 {
		return fmt.Errorf("配置项 worker.reclaim_interval_seconds 取值非法（%d）——只允许 1..60 秒",
			worker.ReclaimIntervalSeconds)
	}
	if worker.ReclaimIntervalSeconds > worker.LeaseTTLSeconds {
		return fmt.Errorf("配置项 worker.reclaim_interval_seconds（%d）不得超过 lease_ttl_seconds（%d）"+
			"——否则过期之后还要再等一个 TTL 才有人收尾",
			worker.ReclaimIntervalSeconds, worker.LeaseTTLSeconds)
	}
	if worker.ReclaimBatch < 1 || worker.ReclaimBatch > 256 {
		return fmt.Errorf("配置项 worker.reclaim_batch 取值非法（%d）——只允许 1..256",
			worker.ReclaimBatch)
	}
	if worker.HeartbeatIntervalSeconds < 1 {
		return fmt.Errorf("配置项 worker.heartbeat_interval_seconds 必须为正（%d）",
			worker.HeartbeatIntervalSeconds)
	}
	if worker.MaxInflight < 1 || worker.MaxInflight > 512 {
		return fmt.Errorf("配置项 worker.max_inflight 取值非法（%d）——只允许 1..512",
			worker.MaxInflight)
	}
	// flows.shutdown_grace_seconds 已含 cancel_wait（30+5），不得在这里再叠加一次。
	// The flow grace already contains its cancel wait; it must not be added twice.
	flowGrace := time.Duration(flows.ShutdownGraceSeconds) * time.Second
	workerGrace := worker.ShutdownGrace()
	if workerGrace < flowGrace+5*time.Second {
		return fmt.Errorf("配置项 worker.shutdown_grace_seconds（%d）必须不少于 flows.shutdown_grace_seconds + 5（%s）",
			worker.ShutdownGraceSeconds, (flowGrace + 5*time.Second).String())
	}
	if workerGrace > httpShutdownGrace-5*time.Second {
		return fmt.Errorf("配置项 worker.shutdown_grace_seconds（%d）必须不大于 HTTP 停机宽限 − 5（%s）",
			worker.ShutdownGraceSeconds, (httpShutdownGrace - 5*time.Second).String())
	}
	if scheduler.PollIntervalMS < 100 || scheduler.PollIntervalMS > 5000 {
		return fmt.Errorf("配置项 scheduler.poll_interval_ms 取值非法（%d）——只允许 100..5000",
			scheduler.PollIntervalMS)
	}
	if scheduler.DispatchBatch < 1 || scheduler.DispatchBatch > 256 {
		return fmt.Errorf("配置项 scheduler.dispatch_batch 取值非法（%d）——只允许 1..256",
			scheduler.DispatchBatch)
	}
	if scheduler.DispatchBatch > worker.MaxInflight {
		return fmt.Errorf("配置项 scheduler.dispatch_batch（%d）不得超过 worker.max_inflight（%d）",
			scheduler.DispatchBatch, worker.MaxInflight)
	}
	// 后台单轮要能被总在途预算整体容纳（Flow 节点 Run 与调度执行共用同一个信号量）。
	if scheduler.DispatchBatch+flows.MaxActive > worker.MaxInflight {
		return fmt.Errorf("配置项 scheduler.dispatch_batch（%d）+ flows.max_active（%d）不得超过 worker.max_inflight（%d）",
			scheduler.DispatchBatch, flows.MaxActive, worker.MaxInflight)
	}
	if scheduler.MissedGraceSeconds < 0 || scheduler.MissedGraceSeconds > 86400 {
		return fmt.Errorf("配置项 scheduler.missed_grace_seconds 取值非法（%d）——只允许 0..86400",
			scheduler.MissedGraceSeconds)
	}
	if assets.BundleRoot == "" {
		return errors.New("配置项 assets.bundle_root 不得为空——资产根是内容寻址校验的唯一入口，" +
			"空值不允许被解释成缺省（US-24 R3）")
	}
	return nil
}
