package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
)

// IsolationConfig 是重启生效的隔离配置；不参与在线授权档位变更。
// IsolationConfig is the static container configuration, independent of live policy.
type IsolationConfig struct {
	Backend               string                            `yaml:"backend" json:"backend"`
	OutputRoot            string                            `yaml:"output_root" json:"output_root"`
	ReleaseManifest       string                            `yaml:"release_manifest" json:"release_manifest"`
	RunAsUID              int                               `yaml:"run_as_uid" json:"run_as_uid"`
	RunAsGID              int                               `yaml:"run_as_gid" json:"run_as_gid"`
	MaxConcurrent         int                               `yaml:"max_concurrent" json:"max_concurrent"`
	MaxTotalCPUs          float64                           `yaml:"max_total_cpus" json:"max_total_cpus"`
	MaxTotalMemoryMB      int                               `yaml:"max_total_memory_mb" json:"max_total_memory_mb"`
	MaxTotalTempBytes     int64                             `yaml:"max_total_temp_bytes" json:"max_total_temp_bytes"`
	MaxInputBytes         int64                             `yaml:"max_input_bytes" json:"max_input_bytes"`
	MaxTotalArtifactBytes int64                             `yaml:"max_total_artifact_bytes" json:"max_total_artifact_bytes"`
	Profiles              map[string]IsolationProfileConfig `yaml:"profiles" json:"profiles"`
	Tools                 map[string]string                 `yaml:"tools" json:"tools"`
}

// IsolationProfileConfig 是一个固定档位的运行时和硬限额。
// IsolationProfileConfig fixes one runtime and its resource limits.
type IsolationProfileConfig struct {
	Runtime        string  `yaml:"runtime" json:"runtime"`
	Image          string  `yaml:"image" json:"image"`
	CPUs           float64 `yaml:"cpus" json:"cpus"`
	MemoryMB       int     `yaml:"memory_mb" json:"memory_mb"`
	PIDs           int     `yaml:"pids" json:"pids"`
	TmpfsMB        int     `yaml:"tmpfs_mb" json:"tmpfs_mb"`
	OutputTmpfsMB  int     `yaml:"output_tmpfs_mb" json:"output_tmpfs_mb"`
	MaxOutputBytes int     `yaml:"max_output_bytes" json:"max_output_bytes"`
	TimeoutSeconds int     `yaml:"timeout_seconds" json:"timeout_seconds"`
}

var isolationImagePattern = regexp.MustCompile(`^(?:[^\s@]+@)?sha256:[0-9a-f]{64}$`)

func parseIsolationConfig(section any, present bool) (IsolationConfig, error) {
	var cfg IsolationConfig
	if !present {
		return cfg, nil
	}
	fields, ok := section.(map[string]any)
	if !ok {
		return cfg, errors.New("isolation 必须是映射")
	}
	keys := []string{"backend", "output_root", "release_manifest", "run_as_uid", "run_as_gid", "max_concurrent", "max_total_cpus", "max_total_memory_mb", "max_total_temp_bytes", "max_input_bytes", "max_total_artifact_bytes", "profiles", "tools"}
	if err := isolationKeys(fields, keys, "isolation"); err != nil {
		return cfg, err
	}
	if profiles, ok := fields["profiles"].(map[string]any); ok {
		for name, value := range profiles {
			p, ok := value.(map[string]any)
			if !ok {
				return cfg, fmt.Errorf("isolation.profiles.%s 必须是映射", name)
			}
			if err := isolationKeys(p, []string{"runtime", "image", "cpus", "memory_mb", "pids", "tmpfs_mb", "output_tmpfs_mb", "max_output_bytes", "timeout_seconds"}, "isolation.profiles."+name); err != nil {
				return cfg, err
			}
		}
	}
	raw, err := json.Marshal(section)
	if err != nil {
		return cfg, errors.New("isolation 类型非法")
	}
	cfg.RunAsUID = 65534
	cfg.RunAsGID = 65534
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return IsolationConfig{}, errors.New("isolation 包含非法类型或数值")
	}
	return cfg, nil
}

func isolationKeys(fields map[string]any, keys []string, path string) error {
	for k, v := range fields {
		if !slices.Contains(keys, k) {
			return fmt.Errorf("未知配置键 %s.%s", path, k)
		}
		if v == nil {
			return fmt.Errorf("%s.%s 不接受 null", path, k)
		}
	}
	return nil
}

func validateIsolationConfig(cfg *Config) error {
	c := cfg.Isolation
	if c.Backend == "" {
		if cfg.Execution.AllowRunCode || len(c.Tools) > 0 || len(c.Profiles) > 0 {
			return errors.New("isolation.backend 缺失，不能绑定或授权 run_code")
		}
		return nil
	}
	if c.Backend != "docker" {
		return errors.New("isolation.backend 只接受 docker")
	}
	if !filepath.IsAbs(c.OutputRoot) {
		return errors.New("isolation.output_root 必须是绝对路径")
	}
	if !filepath.IsAbs(c.ReleaseManifest) {
		return errors.New("isolation.release_manifest 必须是绝对路径")
	}
	if c.RunAsUID <= 0 || c.RunAsGID <= 0 {
		return errors.New("isolation.run_as_uid/run_as_gid 必须为正")
	}
	if c.MaxConcurrent <= 0 || c.MaxTotalMemoryMB <= 0 || !positiveFinite(c.MaxTotalCPUs) {
		return errors.New("isolation.max_concurrent/max_total_cpus/max_total_memory_mb 必须为正")
	}
	if c.MaxTotalTempBytes <= 0 || c.MaxInputBytes <= 0 || c.MaxTotalArtifactBytes <= 0 {
		return errors.New("isolation.max_total_temp_bytes/max_input_bytes/max_total_artifact_bytes 必须为正")
	}
	bound := 0
	for name, target := range c.Tools {
		switch name {
		case "shell":
			if target != "local" && target != "oci" {
				return errors.New("isolation.tools.shell 只允许 local/oci")
			}
		case "run_code":
			if target != "gvisor" {
				return errors.New("isolation.tools.run_code 只允许 gvisor")
			}
		default:
			return fmt.Errorf("未知配置键 isolation.tools.%s", name)
		}
		if target != "local" {
			if _, ok := c.Profiles[target]; !ok {
				return fmt.Errorf("isolation.profiles.%s 缺失", target)
			}
			bound++
		}
	}
	if bound == 0 {
		return errors.New("isolation.tools 必须显式绑定至少一个容器档")
	}
	if (c.Tools["run_code"] == "gvisor") != cfg.Execution.AllowRunCode {
		return errors.New("isolation.tools.run_code 与 execution.allow_run_code 必须同时显式开启")
	}
	for name, p := range c.Profiles {
		if name != "oci" && name != "gvisor" {
			return fmt.Errorf("未知 isolation.profiles.%s", name)
		}
		want := "runc"
		if name == "gvisor" {
			want = "runsc"
		}
		if p.Runtime != want {
			return fmt.Errorf("isolation.profiles.%s.runtime 必须为 %s", name, want)
		}
		if !isolationImagePattern.MatchString(p.Image) {
			return fmt.Errorf("isolation.profiles.%s.image 必须固定完整 sha256", name)
		}
		if !positiveFinite(p.CPUs) || p.MemoryMB <= 0 || p.PIDs <= 0 || p.TmpfsMB <= 0 || p.OutputTmpfsMB <= 0 || p.MaxOutputBytes <= 0 {
			return fmt.Errorf("isolation.profiles.%s 的 cpus/memory_mb/pids/tmpfs_mb/output_tmpfs_mb/max_output_bytes 必须为正", name)
		}
		if p.TimeoutSeconds < 1 || p.TimeoutSeconds > 600 {
			return fmt.Errorf("isolation.profiles.%s.timeout_seconds 必须在 1–600", name)
		}
		if slices.Contains([]string{c.Tools["shell"], c.Tools["run_code"]}, name) {
			if float64(c.MaxConcurrent)*p.CPUs > c.MaxTotalCPUs || int64(p.MemoryMB) > int64(c.MaxTotalMemoryMB)/int64(c.MaxConcurrent) {
				return fmt.Errorf("isolation.profiles.%s 超过实例 CPU/内存预算", name)
			}
		}
	}
	floor, other, err := c.TempBudgets()
	if err != nil {
		return err
	}
	return CheckOutputRootTmpfs(floor, floor, other, c.MaxTotalTempBytes)
}

func positiveFinite(n float64) bool { return n > 0 && !math.IsInf(n, 0) && !math.IsNaN(n) }

// TempBudgets 返回字节化输出下界与其他预算，所有乘加先检查溢出。
// TempBudgets computes bound-profile byte budgets with checked arithmetic.
func (c IsolationConfig) TempBudgets() (int64, int64, error) {
	var tmp, out, logs int64
	for _, name := range []string{c.Tools["shell"], c.Tools["run_code"]} {
		if name == "local" || name == "" {
			continue
		}
		p := c.Profiles[name]
		tmp = max(tmp, int64(p.TmpfsMB))
		out = max(out, int64(p.OutputTmpfsMB))
		logs = max(logs, int64(p.MaxOutputBytes))
	}
	if c.MaxConcurrent <= 0 || tmp <= 0 || out <= 0 || logs <= 0 || c.MaxInputBytes <= 0 {
		return 0, 0, errors.New("isolation 临时预算必须为正")
	}
	const mib = int64(1 << 20)
	if tmp > math.MaxInt64/mib || out > math.MaxInt64/mib {
		return 0, 0, errors.New("isolation 临时预算溢出")
	}
	tmp *= mib
	out *= mib
	if tmp > math.MaxInt64-c.MaxInputBytes || tmp+c.MaxInputBytes > math.MaxInt64-logs {
		return 0, 0, errors.New("isolation 临时预算溢出")
	}
	other := tmp + c.MaxInputBytes + logs
	n := int64(c.MaxConcurrent)
	if out > math.MaxInt64/n || other > math.MaxInt64/n {
		return 0, 0, errors.New("isolation 临时预算溢出")
	}
	return n * out, n * other, nil
}

// CheckOutputRootTmpfs 校验实际容量落在输出下界和剩余总预算之间。
// CheckOutputRootTmpfs checks both bounds without overflowing an addition.
func CheckOutputRootTmpfs(size, outputFloor, otherBudget, maxTotal int64) error {
	if size <= 0 || outputFloor <= 0 || otherBudget <= 0 || maxTotal <= 0 || otherBudget >= maxTotal || size < outputFloor || size > maxTotal-otherBudget {
		return errors.New("isolation.output_root 容量不满足输出下界与总量预算")
	}
	return nil
}
