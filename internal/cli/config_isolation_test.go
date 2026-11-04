package cli

import (
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
)

func us20ConfigForTest(t *testing.T, n, tmp, out int, input, logs, total int64) Config {
	t.Helper()
	dir := t.TempDir()
	p := IsolationProfileConfig{Runtime: "runc", Image: "basalt@sha256:" + strings.Repeat("a", 64), CPUs: 1, MemoryMB: 128, PIDs: 32, TmpfsMB: tmp, OutputTmpfsMB: out, MaxOutputBytes: int(logs), TimeoutSeconds: 60}
	g := p
	g.Runtime = "runsc"
	return Config{Execution: ExecutionConfig{AllowRunCode: true}, Isolation: IsolationConfig{Backend: "docker", OutputRoot: dir, ReleaseManifest: filepath.Join(dir, "image-lock.json"), RunAsUID: 65534, RunAsGID: 65534, MaxConcurrent: n, MaxTotalCPUs: 8, MaxTotalMemoryMB: 8192, MaxTotalTempBytes: total, MaxInputBytes: input, MaxTotalArtifactBytes: 1 << 30, Profiles: map[string]IsolationProfileConfig{"oci": p, "gvisor": g}, Tools: map[string]string{"shell": "oci", "run_code": "gvisor"}}}
}

func parseIsolationYAMLForTest(t *testing.T, raw string) (IsolationConfig, error) {
	t.Helper()
	var tree map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(raw), &tree))
	section, present := tree["isolation"]
	c, err := parseIsolationConfig(section, present)
	if err != nil {
		return c, err
	}
	cfg := Config{Isolation: c, Execution: ExecutionConfig{AllowRunCode: true}}
	return c, validateIsolationConfig(&cfg)
}

func TestUS20_Config_OldConfigUnchanged(t *testing.T) {
	c, err := decodeConfig(map[string]any{})
	require.NoError(t, err)
	require.Empty(t, c.Isolation.Backend)
	require.False(t, c.Execution.AllowRunCode)
	for _, profile := range []string{"strict", "standard"} {
		c, err := decodeConfig(map[string]any{"execution": map[string]any{"profile": profile}})
		require.NoError(t, err)
		require.Equal(t, profile, c.Execution.Profile)
		require.False(t, c.Execution.AllowRunCode)
	}
}

func TestUS20_Config_UnknownKeyRejected(t *testing.T) {
	for _, raw := range []string{"isolation:\n  typo: true\n", "isolation:\n  profiles:\n    gvisor:\n      bad: 1\n"} {
		_, err := parseIsolationYAMLForTest(t, raw)
		require.Error(t, err)
		require.Contains(t, err.Error(), "isolation")
	}
	_, err := parseIsolationConfig(map[string]any{"backend": nil}, true)
	require.Error(t, err)
	_, err = parseIsolationConfig("docker", true)
	require.Error(t, err)
}

func TestUS20_Config_RunCodeMustBeGvisor(t *testing.T) {
	c := us20ConfigForTest(t, 2, 64, 64, 16<<20, 256<<10, 300<<20)
	c.Isolation.Tools["run_code"] = "oci"
	err := validateIsolationConfig(&c)
	if err == nil || !strings.Contains(err.Error(), "run_code") {
		t.Fatalf("run_code: oci 必须被拒绝，got %v", err)
	}
}

func TestUS20_Config_ImageMustBePinned(t *testing.T) {
	for _, image := range []string{"basalt:latest", "basalt:1.2.3", "basalt@sha256:xyz"} {
		c := us20ConfigForTest(t, 2, 64, 64, 16<<20, 256<<10, 300<<20)
		p := c.Isolation.Profiles["oci"]
		p.Image = image
		c.Isolation.Profiles["oci"] = p
		if err := validateIsolationConfig(&c); err == nil {
			t.Fatalf("未固定的镜像 %q 必须被拒绝", image)
		}
	}
}

func TestUS20_Config_ImageMustBeApproved(t *testing.T) {
	// 配置形态不授予信任：有效 digest 仍须运行期清单验签，配置不得自带允许列表。
	// A syntactically pinned image never creates an approval through configuration.
	_, err := parseIsolationConfig(map[string]any{"approved_digests": []string{"sha256:" + strings.Repeat("a", 64)}}, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "approved_digests")
}

func TestUS20_Config_ReleaseManifestRequired(t *testing.T) {
	c := us20ConfigForTest(t, 2, 64, 64, 16<<20, 256<<10, 300<<20)
	c.Isolation.ReleaseManifest = ""
	err := validateIsolationConfig(&c)
	require.Error(t, err)
	require.Contains(t, err.Error(), "release_manifest")
}

func TestUS20_Config_IsolationDoesNotUpgradeProfile(t *testing.T) {
	for _, profile := range []string{"strict", "standard"} {
		c := us20ConfigForTest(t, 2, 64, 64, 16<<20, 256<<10, 300<<20)
		c.Execution.Profile = profile
		require.NoError(t, validateIsolationConfig(&c))
		require.Equal(t, profile, c.Execution.Profile)
		c.Execution.AllowRunCode = false
		require.Error(t, validateIsolationConfig(&c))
		delete(c.Isolation.Tools, "run_code")
		require.NoError(t, validateIsolationConfig(&c))
		c.Execution.AllowRunCode = true
		require.Error(t, validateIsolationConfig(&c))
	}
}

func TestUS20_Config_TempFormulaBytes(t *testing.T) {
	c := us20ConfigForTest(t, 2, 64, 64, 16<<20, 256<<10, 300<<20)
	if err := validateIsolationConfig(&c); err != nil {
		t.Fatalf("公式内的字节换算必须自洽，got %v", err)
	}
	floor, other, err := c.Isolation.TempBudgets()
	require.NoError(t, err)
	require.EqualValues(t, 128<<20, floor)
	require.EqualValues(t, 160<<20+512<<10, other)
	c.Isolation.MaxTotalTempBytes = 100 << 20
	if err := validateIsolationConfig(&c); err == nil {
		t.Fatal("总量不足时必须拒绝启动")
	}
	c.Isolation.MaxInputBytes = math.MaxInt64
	_, _, err = c.Isolation.TempBudgets()
	require.Error(t, err)
	c = us20ConfigForTest(t, 2, 64, 64, 16<<20, 256<<10, 300<<20)
	p := c.Isolation.Profiles["gvisor"]
	p.OutputTmpfsMB = 128
	c.Isolation.Profiles["gvisor"] = p
	require.Error(t, validateIsolationConfig(&c), "必须对全部绑定档位取最大值")
}

func TestUS20_Config_TmpfsUpperBound(t *testing.T) {
	const total = int64(300 << 20)
	const floor = int64(128 << 20)
	const other = int64(160<<20 + 512<<10)
	if err := CheckOutputRootTmpfs(100<<20, floor, other, total); err == nil {
		t.Fatal("输出区小于并发输出预算必须拒绝")
	}
	if err := CheckOutputRootTmpfs(total, floor, other, total); err == nil {
		t.Fatal("输出区顶到 max_total_temp_bytes、再加其他预算会超支，必须拒绝")
	}
	for _, size := range []int64{floor, 139<<20 + 512<<10} {
		if err := CheckOutputRootTmpfs(size, floor, other, total); err != nil {
			t.Fatalf("区间端点必须通过，got %v", err)
		}
	}
}

func TestUS20_Config_PositiveLimits(t *testing.T) {
	for _, mutate := range []func(*Config){func(c *Config) { c.Isolation.MaxConcurrent = 0 }, func(c *Config) { c.Isolation.MaxInputBytes = 0 }, func(c *Config) { c.Isolation.MaxTotalCPUs = math.Inf(1) }, func(c *Config) { c.Isolation.MaxTotalArtifactBytes = 0 }, func(c *Config) { c.Isolation.RunAsUID = 0 }, func(c *Config) {
		p := c.Isolation.Profiles["gvisor"]
		p.OutputTmpfsMB = 0
		c.Isolation.Profiles["gvisor"] = p
	}} {
		c := us20ConfigForTest(t, 2, 64, 64, 16<<20, 256<<10, 300<<20)
		mutate(&c)
		require.Error(t, validateIsolationConfig(&c))
	}
	c := us20ConfigForTest(t, 2, 64, 64, 16<<20, 256<<10, 300<<20)
	raw, err := yaml.Marshal(map[string]any{"isolation": c.Isolation})
	require.NoError(t, err)
	parsed, err := parseIsolationYAMLForTest(t, string(raw))
	require.NoError(t, err)
	require.Equal(t, c.Isolation, parsed)
}
