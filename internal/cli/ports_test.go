package cli_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDependencyDirection_CliDoesNotImportImplementations：命令面**不得**直接
// import 实现包（storage / workspace）——端口定义在消费方、适配器由装配层注入
// （D1 裁决：无环**不等于**依赖方向合规）。
// The command surface must not import the implementation packages: the ports are
// consumer-defined and internal/app injects the adapters. Acyclic is not the
// criterion.
//
// 判据是可执行检查而不是评审约定：把 cli 改回直连实现（或有人"顺手"加一行
// import）即判红。查出的是**非测试**导入集——测试侧允许转调真实现包（status 的
// 三态诊断要真库才立得住），被测面的依赖方向只由非测试文件决定。
// The judge reads the non-test import set: test files may adapt the real packages,
// the package under test may not.
func TestDependencyDirection_CliDoesNotImportImplementations(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{join .Imports " "}}`,
		"github.com/ymedword/basalt/internal/cli").Output()
	require.NoError(t, err, "go list 是判据的一部分，必须可用: %s", out)

	imports := strings.Fields(string(out))
	for _, forbidden := range []string{
		"github.com/ymedword/basalt/internal/storage",
		"github.com/ymedword/basalt/internal/workspace",
	} {
		require.NotContains(t, imports, forbidden,
			"命令面不得直接依赖实现包——改用 cli 自己声明的端口（见 ports.go）")
	}
}
