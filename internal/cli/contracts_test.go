package cli_test

import (
	"io/fs"
	"os"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
)

// contractsDir 是契约文件目录（相对本包）：唯一事实源目录，只读消费。
// contractsDir is the contract directory, consumed read-only.
const contractsDir = "../../docs/contracts"

// TestContractYAMLParses：docs/contracts/*.yaml 经 goccy/go-yaml 全解析通过。
//
// 契约是手写的 YAML，缩进、锚点、重复键这类隐性错误不解析就看不见——用项目已有的
// YAML 依赖当"下游解析器"把它读回去，坏契约在门禁里当场红（开发操作 §7 陷阱 32）。
// Every contract YAML must parse through the project's own YAML dependency, so a
// malformed contract fails the gate instead of surfacing at consumption time.
func TestContractYAMLParses(t *testing.T) {
	root, err := os.OpenRoot(contractsDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	names, err := fs.Glob(root.FS(), "*.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, names, "契约目录 %s 下应当有 YAML 文件", contractsDir)

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			raw, err := root.ReadFile(name)
			require.NoError(t, err)

			var doc any
			require.NoError(t, yaml.Unmarshal(raw, &doc), "契约 %s 必须可被 goccy/go-yaml 解析", name)
			require.NotNil(t, doc, "契约 %s 解析结果不得为空文档", name)
		})
	}
}
