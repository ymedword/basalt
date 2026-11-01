// Package scripts 只承载脚本自检——本目录没有非测试 Go 文件（判卷脚本本身是
// shell），故该包无可执行语句，覆盖率判据按「无语句包」跳过。
// Package scripts holds the self-tests of the shell judges only; it has no
// non-test Go files, so the coverage gate skips it as a statement-less package.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// dsnPlaceholder 只满足判卷脚本的「需要 DSN」前置闸门；合成树里没有集成用例，
// 占位值不会被任何测试连接。
// The placeholder only satisfies the script's DSN preflight; synthetic trees have no
// integration tests, so nothing ever dials it.
const dsnPlaceholder = "postgres://synthetic.invalid/basalt_test?sslmode=disable"

// withoutEnv 返回去掉指定变量的环境副本。Windows 的变量名大小写不敏感，按不区分
// 大小写比较——残余值会让「缺 DSN」用例变成假绿。
// withoutEnv drops the named variables case-insensitively; a leftover value would turn
// the missing-DSN case into a false green.
func withoutEnv(env []string, names ...string) []string {
	out := env[:0:0]
	for _, entry := range env {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		drop := false
		for _, name := range names {
			if strings.EqualFold(key, name) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, entry)
		}
	}
	return out
}

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
// 返回合并输出与错误——判据既看退出码，也看可读错误文案。环境固定注入占位 DSN：
// 合成树没有集成用例，但自生成 profile 的模式要求 DSN；缺 DSN 路径由
// runScriptWithoutDSN 显式构造。
// The placeholder DSN satisfies the preflight; the missing-DSN path is explicit.
func runScript(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"check-coverage.sh"}, args...)...)
	cmd.Env = append(withoutEnv(os.Environ(), "BASALT_TEST_PG_DSN"), "BASALT_TEST_PG_DSN="+dsnPlaceholder)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runScriptWithoutDSN 在**显式移除** BASALT_TEST_PG_DSN 的环境里跑判卷脚本——
// 不依赖调用方恰好没设置，反方向（干净环境）也能复现。
func runScriptWithoutDSN(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"check-coverage.sh"}, args...)...)
	cmd.Env = withoutEnv(os.Environ(), "BASALT_TEST_PG_DSN")
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

// withPackage 把样例源码的 `package p` 行换成指定包名，其余逐字保留。
// withPackage rewrites only the package clause so both probe packages share one body.
func withPackage(name, src string) string {
	return strings.Replace(src, "package p\n", "package "+name+"\n", 1)
}

// TestCheckCoverage_PostgresBaseNameUsesSeventyTier 锁定 `postgres` 档位：
// `internal/storage/postgres` 的包基名是 `postgres`（不在 `internal/storage` 包内），
// 必须落进 70% 档（US-16 §4.7 / T136 / T138）。同一棵树里放两个**内容相同、覆盖率
// 同为 50%** 的包——`p` 通过、`postgres` 失败——证明档位只由包基名决定，不是别处
// 凑巧达标或凑巧失败。
// TestCheckCoverage_PostgresBaseNameUsesSeventyTier locks the 70% tier for the
// `postgres` base name: two identically covered packages diverge, so the tier is
// chosen by base name alone.
func TestCheckCoverage_PostgresBaseNameUsesSeventyTier(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"go.mod":             probeGoMod,
		"p/p.go":             twoFuncs,
		"p/p_test.go":        coverFFunc,
		"postgres/q.go":      withPackage("postgres", twoFuncs),
		"postgres/q_test.go": withPackage("postgres", coverFFunc),
	})

	out, err := runScript(t, "--tree", tree)
	require.Error(t, err, "50%% 的 postgres 包必须按 70%% 档判失败: %s", out)
	require.Contains(t, out, "example.com/probe/postgres")
	require.Contains(t, out, "(< 70%)", "postgres 包必须落在 70% 档")
	require.Contains(t, out, "example.com/probe/p")
	require.Contains(t, out, "(>= 50%)", "同内容的 p 包仍在 50% 档")
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

// TestCheckCoverage_MissingDSNFails 判据 7（D-01 / T125）：自生成 profile 的模式
// 缺 BASALT_TEST_PG_DSN 必须**失败**并给出排查路径——不得先跑一遍测试、再靠
// 「集成用例全跳过」的 profile 判绿。
// A missing DSN fails before any test runs; the hint names the troubleshooting path.
func TestCheckCoverage_MissingDSNFails(t *testing.T) {
	tree := writeTree(t, map[string]string{
		"go.mod":      probeGoMod,
		"p/p.go":      twoFuncs,
		"p/p_test.go": coverFFunc,
	})

	out, err := runScriptWithoutDSN(t, "--tree", tree)
	require.Error(t, err, "缺 DSN 必须失败: %s", out)
	require.Contains(t, out, "BASALT_TEST_PG_DSN")
	require.Contains(t, out, "D-01")
	require.Contains(t, out, "docs/DevelopmentLoop.md")
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
