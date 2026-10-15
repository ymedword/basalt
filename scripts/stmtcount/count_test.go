package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writePackage 在临时目录里放一个单文件包，返回目录。
// 夹具写在 t.TempDir() 里而非 testdata/——含语法错误的夹具不能出现在仓库中
// （全仓 `gofmt -l .` 会因解析失败而非零退出，把格式门禁判红）。
func writePackage(t *testing.T, name, source string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(source), 0o644))
	return dir
}

// TestStmtCount_CountsExecutableStatements 验收点（文档用例名同名）：函数体里的
// 语句逐条计数——if 语句自身算一条，两个 return 各算一条，函数体块不算。
func TestStmtCount_CountsExecutableStatements(t *testing.T) {
	dir := writePackage(t, "p.go", `package p

func F(n int) int {
	if n > 0 {
		return n
	}
	return -n
}
`)

	count, err := Count(dir)
	require.NoError(t, err)
	require.Equal(t, 3, count)
}

// TestStmtCount_NoStatements 验收点：只有声明（const / type / 结构体字段）的包
// 没有可执行语句——覆盖率判据按此跳过该包。
func TestStmtCount_NoStatements(t *testing.T) {
	dir := writePackage(t, "p.go", `package p

const X = 1

type T struct {
	N int
}
`)

	count, err := Count(dir)
	require.NoError(t, err)
	require.Zero(t, count)
}

// TestStmtCount_ParseError 验收点：不可解析的源文件必须报错——不得把「解析不了」
// 当成「没有语句」。
func TestStmtCount_ParseError(t *testing.T) {
	dir := writePackage(t, "p.go", "package p\n\nfunc F( {\n")

	_, err := Count(dir)
	require.Error(t, err)
}

// TestStmtCount_RunPropagatesUsageAndFailure 验收点：入口包装的三种退出码——
// 无参数（用法错误 2）、目录不可读（失败 1）、正常（0）。
func TestStmtCount_RunPropagatesUsageAndFailure(t *testing.T) {
	require.Equal(t, 2, run(nil), "无参数必须判用法错误")

	dir := writePackage(t, "p.go", "package p\n\nfunc F() {\n\treturn\n}\n")
	require.Equal(t, 1, run([]string{filepath.Join(dir, "不存在的目录")}), "不可读目录必须判失败")

	require.Equal(t, 0, run([]string{dir}), "正常目录必须成功")
}
