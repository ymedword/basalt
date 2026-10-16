package cli_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDependencyDirection_CliDoesNotImportImplementations：命令面**不得**直接
// import 实现包（storage / workspace / provider）——端口定义在消费方、适配器由
// 装配层注入（D1 裁决：无环**不等于**依赖方向合规）。
// The command surface must not import the implementation packages: the ports are
// consumer-defined and internal/app injects the adapters. Acyclic is not the
// criterion.
//
// provider 进清单的理由是 US-2 的展示层：`provider list` 也要收窄 base_url，而
// "为复用脱敏函数 import 实现包"是最容易被顺手写下的形态——收窄规则因此在 cli
// 侧自实现（redactedBaseURL），依赖方向改由本判据守。
// provider joins the list for US-2: reusing its URL rule from the command surface
// would be the easy path, so the rule is re-implemented here and this judge holds
// the dependency direction instead.
//
// tool 进清单的理由是 US-5 的工具清单：`tool list` 要的是 `core.ToolSpec`（三件
// 字段的**数据**），而"顺手 import tool 直接读注册表"同样是最短路径——命令面因此
// 只声明 `ToolLister` 端口（ports.go），适配器落在装配层。清单**只增不减**：这条
// 判据是收严而不是放宽，既有三包的覆盖不受影响。
// tool joins the list for US-5: reading the registry directly would be the shortest
// path, so the command surface declares a port instead and the adapter lives in app.
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
		"github.com/ymedword/basalt/internal/provider",
		"github.com/ymedword/basalt/internal/tool",
	} {
		require.NotContains(t, imports, forbidden,
			"命令面不得直接依赖实现包——改用 cli 自己声明的端口（见 ports.go）")
	}
}
