package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 本文件是 US-24 §3.3 三段配置（`worker` / `scheduler` / `assets`）的装载期用例：
// 缺省值、显式值、未知键与空值拒绝、逐条取值范围与跨段上限。判据是**解码 / 校验
// 的返回**——配置非法必须让装载失败，不得静默落回缺省。
// Load-time acceptance for the three distributed-execution sections: defaults, explicit
// values, unknown/null refusal and every §3.3 bound.

func TestWorkerConfig_DefaultsAppliedWhenSectionsAbsent(t *testing.T) {
	cfg, err := decodeConfig(map[string]any{})
	require.NoError(t, err)
	require.Equal(t, DefaultWorkerConfig(), cfg.Worker, "worker 段缺席时落 §3.3 冻结缺省")
	require.Equal(t, DefaultSchedulerConfig(), cfg.Scheduler)
	require.Equal(t, DefaultAssetsConfig(), cfg.Assets)
}

func TestWorkerConfig_ExplicitValuesDecoded(t *testing.T) {
	cfg, err := decodeConfig(map[string]any{
		"worker": map[string]any{
			"name":                       "worker-b",
			"lease_ttl_seconds":          60,
			"renew_interval_seconds":     20,
			"reclaim_interval_seconds":   10,
			"reclaim_batch":              16,
			"heartbeat_interval_seconds": 5,
			"max_inflight":               128,
			"shutdown_grace_seconds":     50,
		},
		"scheduler": map[string]any{"poll_interval_ms": 250, "dispatch_batch": 8, "missed_grace_seconds": 60},
		"assets":    map[string]any{"bundle_root": "/srv/basalt/assets"},
	})
	require.NoError(t, err)
	require.Equal(t, "worker-b", cfg.Worker.Name)
	require.Equal(t, 60*time.Second, cfg.Worker.LeaseTTL())
	require.Equal(t, 20*time.Second, cfg.Worker.RenewInterval())
	require.Equal(t, 10*time.Second, cfg.Worker.ReclaimInterval())
	require.Equal(t, 16, cfg.Worker.ReclaimBatch)
	require.Equal(t, 5*time.Second, cfg.Worker.HeartbeatInterval())
	require.Equal(t, 128, cfg.Worker.MaxInflight)
	require.Equal(t, 50*time.Second, cfg.Worker.ShutdownGrace())
	require.Equal(t, 250*time.Millisecond, cfg.Scheduler.PollInterval())
	require.Equal(t, 8, cfg.Scheduler.DispatchBatch)
	require.Equal(t, 60*time.Second, cfg.Scheduler.MissedGrace())
	require.Equal(t, "/srv/basalt/assets", cfg.Assets.BundleRoot)
}

// TestWorkerConfig_RejectsInvalidSections 逐条钉 §3.3 的取值范围与跨段上限：
// 每个非法输入都必须让装载失败，错误文案点名配置键，且不回显敏感原文。
func TestWorkerConfig_RejectsInvalidSections(t *testing.T) {
	cases := []struct {
		name string
		tree map[string]any
	}{
		{"lease_ttl 下界", map[string]any{"worker": map[string]any{"lease_ttl_seconds": 4}}},
		{"lease_ttl 上界", map[string]any{"worker": map[string]any{"lease_ttl_seconds": 301}}},
		{"renew 贴近 TTL", map[string]any{"worker": map[string]any{"renew_interval_seconds": 15}}},
		{"renew 为零", map[string]any{"worker": map[string]any{"renew_interval_seconds": 0}}},
		{"reclaim 为零", map[string]any{"worker": map[string]any{"reclaim_interval_seconds": 0}}},
		{"reclaim 超 60", map[string]any{"worker": map[string]any{"reclaim_interval_seconds": 61}}},
		{"reclaim 超 TTL", map[string]any{"worker": map[string]any{"reclaim_interval_seconds": 31}}},
		{"reclaim_batch 为零", map[string]any{"worker": map[string]any{"reclaim_batch": 0}}},
		{"reclaim_batch 超 256", map[string]any{"worker": map[string]any{"reclaim_batch": 257}}},
		{"heartbeat 为零", map[string]any{"worker": map[string]any{"heartbeat_interval_seconds": 0}}},
		{"max_inflight 为零", map[string]any{"worker": map[string]any{"max_inflight": 0}}},
		{"max_inflight 超 512", map[string]any{"worker": map[string]any{"max_inflight": 513}}},
		{"shutdown 低于 flow 宽限 + 5", map[string]any{"worker": map[string]any{"shutdown_grace_seconds": 39}}},
		{"shutdown 高于 HTTP 宽限 − 5", map[string]any{"worker": map[string]any{"shutdown_grace_seconds": 66}}},
		{"poll 过小", map[string]any{"scheduler": map[string]any{"poll_interval_ms": 99}}},
		{"poll 过大", map[string]any{"scheduler": map[string]any{"poll_interval_ms": 5001}}},
		{"dispatch_batch 为零", map[string]any{"scheduler": map[string]any{"dispatch_batch": 0}}},
		{"dispatch_batch 超 256", map[string]any{"scheduler": map[string]any{"dispatch_batch": 257}}},
		{"dispatch_batch 超 max_inflight", map[string]any{"scheduler": map[string]any{"dispatch_batch": 65}}},
		{"dispatch_batch + flows.max_active 超 max_inflight", map[string]any{
			"worker":    map[string]any{"max_inflight": 35},
			"scheduler": map[string]any{"dispatch_batch": 32},
		}},
		{"missed_grace 为负", map[string]any{"scheduler": map[string]any{"missed_grace_seconds": -1}}},
		{"missed_grace 超一天", map[string]any{"scheduler": map[string]any{"missed_grace_seconds": 86401}}},
		{"bundle_root 为空", map[string]any{"assets": map[string]any{"bundle_root": ""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeConfig(tc.tree)
			require.Error(t, err)
		})
	}
}

// TestWorkerConfig_RejectsUnknownKeysAndNulls 判未知键与显式 null 都被拒绝：
// 拼错的键（`lease_ttl_second`）若被静默忽略，进程会在缺省租约时长上跑起来——
// 「配置看着改了、行为没变」是运维最难查的一类。
func TestWorkerConfig_RejectsUnknownKeysAndNulls(t *testing.T) {
	for _, section := range []string{"worker", "scheduler", "assets"} {
		t.Run(section+" 未知键", func(t *testing.T) {
			_, err := decodeConfig(map[string]any{section: map[string]any{"unknown-SENTINEL": 1}})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "SENTINEL")
		})
		t.Run(section+" 非映射", func(t *testing.T) {
			_, err := decodeConfig(map[string]any{section: []any{}})
			require.Error(t, err)
		})
	}
	for _, field := range []string{
		"lease_ttl_seconds", "renew_interval_seconds", "reclaim_interval_seconds",
		"reclaim_batch", "heartbeat_interval_seconds", "max_inflight", "shutdown_grace_seconds",
	} {
		_, err := decodeConfig(map[string]any{"worker": map[string]any{field: nil}})
		require.Errorf(t, err, "显式 null 必须被拒绝：%s", field)
	}
	for _, field := range []string{"poll_interval_ms", "dispatch_batch", "missed_grace_seconds"} {
		_, err := decodeConfig(map[string]any{"scheduler": map[string]any{field: nil}})
		require.Errorf(t, err, "显式 null 必须被拒绝：%s", field)
	}
	_, err := decodeConfig(map[string]any{"assets": map[string]any{"bundle_root": nil}})
	require.Error(t, err)
}

// TestWorkerConfig_ErrorNamesTheKey 判错误文案点名配置键：运维要能从启动失败里
// 直接读出是哪个键、哪个范围，而不是"配置校验失败"六个字。
func TestWorkerConfig_ErrorNamesTheKey(t *testing.T) {
	_, err := decodeConfig(map[string]any{"worker": map[string]any{"lease_ttl_seconds": 4}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "worker.lease_ttl_seconds")

	_, err = decodeConfig(map[string]any{"assets": map[string]any{"bundle_root": ""}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "assets.bundle_root")
}
