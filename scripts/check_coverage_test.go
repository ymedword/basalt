// Package scripts 只承载脚本自检——本目录没有非测试 Go 文件（判卷脚本本身是
// shell），故该包无可执行语句，覆盖率判据按「无语句包」跳过。
// Package scripts holds the self-tests of the shell judges only; it has no
// non-test Go files, so the coverage gate skips it as a statement-less package.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// 合成模块的 go.mod：无依赖，离线可构建/可测（自检不能反过来依赖网络）。
const probeGoMod = "module example.com/probe\n\ngo 1.27\n"

// writeTree 把给定的相对路径 → 内容落进一个临时模块树，返回树根。
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	return root
}

// runScript 以 `sh check-coverage.sh <args...>` 跑判卷脚本（cwd = 本包目录）。
// 返回合并输出与错误——判据既看退出码，也看可读错误文案。
func runScript(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"check-coverage.sh"}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// 四个函数、只覆盖一个 → 25%，低于「其余包 >= 50%」的底线。
const fourFuncs = `package p

func F(n int) int { return n + 1 }
func G(n int) int { return n + 2 }
func H(n int) int { return n + 3 }
func I(n int) int { return n + 4 }
`

// 两个函数、只覆盖一个 → 恰好 50%（达标判据是 >=，不是 >）。
const twoFuncs = `package p

func F(n int) int { return n + 1 }
func G(n int) int { return n + 2 }
`

// 只调用 F 的测试文件。
const coverFFunc = `package p

import "testing"

func TestF(t *testing.T) {
	if F(1) != 2 {
		t.Fatal("unexpected")
	}
}
`

// TestCheckCoverage_BelowThresholdFails 判据 1：低于阈值 → 非零，且报文点名包。
func TestCheckCoverage_BelowThresholdFails(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"go.mod":      probeGoMod,
		"p/p.go":      fourFuncs,
		"p/p_test.go": coverFFunc,
	})

	out, err := runScript(t, "--tree", tree)
	require.Error(t, err, "覆盖率低于阈值必须失败: %s", out)
	require.Contains(t, out, "example.com/probe/p")
	require.Contains(t, out, "FAIL")
}

// TestCheckCoverage_AtThresholdPasses 判据 2：恰好达标（>= 阈值）→ 通过。
func TestCheckCoverage_AtThresholdPasses(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"go.mod":      probeGoMod,
		"p/p.go":      twoFuncs,
		"p/p_test.go": coverFFunc,
	})

	out, err := runScript(t, "--tree", tree)
	require.NoError(t, err, "恰好达标必须通过: %s", out)
	require.Contains(t, out, "50.0%")
	require.Contains(t, out, "ok")
}

// TestCheckCoverage_NoStatementsSkipped 判据 3：无可执行语句的包跳过——即便是
// 无测试文件、0% 的形态，也不得判失败（无从测量）。
func TestCheckCoverage_NoStatementsSkipped(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"go.mod": probeGoMod,
		"p/p.go": `package p

const X = 1

type T struct {
	N int
}
`,
	})

	out, err := runScript(t, "--tree", tree)
	require.NoError(t, err, "无语句包必须跳过而非判失败: %s", out)
	require.Contains(t, out, "skip")
	require.Contains(t, out, "无可执行语句")
}

// TestCheckCoverage_StatementsWithoutTestsCountZero 判据 4：有可执行语句但无测试
// 文件的包计 0% 判失败——不得豁免。
func TestCheckCoverage_StatementsWithoutTestsCountZero(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"go.mod": probeGoMod,
		"p/p.go": twoFuncs,
	})

	out, err := runScript(t, "--tree", tree)
	require.Error(t, err, "有语句但无测试文件的包必须计 0% 判失败: %s", out)
	require.Contains(t, out, "example.com/probe/p")
	require.Contains(t, out, "0.0%")
}

// TestCheckCoverage_GoTestFailurePropagates 判据 5：go test 自身失败 → 非零，
// 且不得把「没有输出」当成「通过」。
func TestCheckCoverage_GoTestFailurePropagates(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"go.mod": probeGoMod,
		"p/p.go": twoFuncs,
		"p/p_test.go": `package p

import "testing"

func TestAlwaysFails(t *testing.T) {
	t.Fatal("夹具故意失败")
}
`,
	})

	out, err := runScript(t, "--tree", tree)
	require.Error(t, err, "测试失败必须传导为判卷失败: %s", out)
	require.Contains(t, out, "go test 失败")
}

// writeProfile 把合成 profile 落到临时目录，返回路径——judge 的 --profile
// 注入面（自检不跑真实覆盖，输入完全可控）。
func writeProfile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coverage.out")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// TestCheckCoverage_RoundingDoesNotPassBelowThreshold：阈值判定必须用**原始计数**
// ——1249/2500 = 49.96% 先四舍五入成 50.0% 再比较会放过不达标包（P2 回归：旧版
// 实测判 ok 并显示 50.0%）。
// The threshold is judged on raw counts, never on the rounded percentage.
func TestCheckCoverage_RoundingDoesNotPassBelowThreshold(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"go.mod": probeGoMod,
		"p/p.go": twoFuncs,
	})
	profile := writeProfile(t, `mode: count
example.com/probe/p/p.go:1.1,2.2 1249 1
example.com/probe/p/p.go:3.1,4.2 1251 0
`)

	out, err := runScript(t, "--profile", profile, "--tree", tree)
	require.Error(t, err, "49.96%% 必须判失败——不得被舍入抬成 50.0%% 放过: %s", out)
	require.Contains(t, out, "FAIL")
	require.Contains(t, out, "49.96%", "显示值取两位小数，与判定（原始计数）同一口径")
}

// TestCheckCoverage_CorruptBodyRejected 判据 6 加固：mode 取值非法、正文夹垃圾行、
// 数值非法都必须报输入错误——旧版只认 `mode: ` 前缀，后续聚合器静默跳过 / 强转
// 不匹配记录，把"输入坏了"判成"全部达标"（P2 回归）。
// A valid mode header with a corrupt body must be rejected, not silently skipped.
func TestCheckCoverage_CorruptBodyRejected(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"go.mod": probeGoMod,
		"p/p.go": twoFuncs,
	})

	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"mode 取值非法", "mode: banana\nexample.com/probe/p/p.go:1.1,2.2 1 1\n", "格式非法"},
		{"正文夹垃圾行", "mode: count\nexample.com/probe/p/p.go:1.1,2.2 1 1\ngarbage line here\n", "正文记录损坏"},
		{"语句数非数字", "mode: count\nexample.com/probe/p/p.go:1.1,2.2 many 1\n", "正文记录损坏"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runScript(t, "--profile", writeProfile(t, tc.content), "--tree", tree)
			require.Error(t, err, "损坏输入必须报错而非静默判卷: %s", out)
			require.Contains(t, out, tc.want)
		})
	}
}

// TestCheckCoverage_MalformedInputFails 判据 6：输入异常（不可读 / 格式非法）→
// 非零 + 可读错误。
func TestCheckCoverage_MalformedInputFails(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"go.mod": probeGoMod,
		"p/p.go": twoFuncs,
	})

	t.Run("unreadable profile", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "no-such-profile.out")
		out, err := runScript(t, "--profile", missing, "--tree", tree)
		require.Error(t, err, "不可读的 profile 必须失败: %s", out)
		require.Contains(t, out, "不可读")
	})

	t.Run("malformed profile", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "garbage.out")
		require.NoError(t, os.WriteFile(bad, []byte("这不是覆盖率 profile\n"), 0o644))

		out, err := runScript(t, "--profile", bad, "--tree", tree)
		require.Error(t, err, "格式非法的 profile 必须失败: %s", out)
		require.Contains(t, out, "格式非法")
	})
}
