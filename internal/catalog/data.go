package catalog

// 本文件把目录数据编进二进制（颗粒度文档 §3.1.1）：编译期快照与制品版本天然绑定，
// 请求期不重读文件、不联网。`Builtin` 只做一次解析的入口——缓存由装配层持有，
// 本包不保留可变包级状态（原则 7）。

// This file embeds the catalog snapshot in the binary. Builtin is the single parse
// entry point; the assembly layer holds the snapshot, and the package keeps no
// mutable package-level state.

import (
	_ "embed"

	"github.com/ymedword/basalt/internal/core"
)

//go:embed data/mcp_catalog.yaml
var builtinYAML []byte

// Builtin 解析编译期嵌入的目录快照。装配层调用一次并保存结果；解析失败是数据文件
// 缺陷（构建 / 发布问题），启动必须 fail-closed——不得用空目录充数。
// Builtin parses the embedded snapshot once at assembly time; a parse failure is a
// release defect and startup must fail closed instead of serving an empty catalog.
func Builtin() (core.MCPCatalog, error) {
	return Load(builtinYAML)
}
