// Package examples 把示例素材编译进二进制，供 `basalt demo install` 使用。
// Package examples embeds the example assets so `basalt demo install` works from
// the released single binary without a source checkout.
//
// 为什么内嵌：Basalt 的发布契约是单二进制（宪法第 8 条），安装命令不得要求用户
// 先 clone 仓库；素材是只读的编译期常量，与 internal/catalog 的目录数据同一形态。
// Why embed: the release contract is a single binary, so the installer must not
// require a checkout; the assets are read-only compile-time constants.
package examples

import "embed"

// Assets 是示例树：daily/ 下五份每日 Demo（含脚本、夹具、技能），shared/ 下
// 共享脚本（样例库生成器）与自研只读 MCP server 源码（嵌套 module）。
// Assets holds the demo tree plus the shared scripts and the docs-readonly source.
//
//go:embed daily shared
var Assets embed.FS
