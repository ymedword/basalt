// Package assets 内嵌管理台构建产物。
// Package assets embeds the built console so the single binary carries its own UI.
package assets

import (
	"embed"
	"io/fs"
)

// consoleFS 是构建期产物目录：只含 dist 下已提交的 index.html、assets/* 与
// integrity.json；源码、node_modules 与锁文件**不进**二进制（约束 43）。
// consoleFS holds only the committed build output; sources and dependencies stay out.
//
//go:embed console/dist
var consoleFS embed.FS

// Console 返回管理台产物根（路径相对 dist）。资源缺失属于构建期错误：go:embed 在
// 编译期已保证目录存在，这里的失败只可能是内部路径写错，故按编程错误 panic。
// Console returns the embedded dist root. A missing directory is a programming error
// that go:embed rules out at compile time, so it panics instead of returning error.
func Console() fs.FS {
	sub, err := fs.Sub(consoleFS, "console/dist")
	if err != nil {
		panic("assets: 管理台产物目录缺失: " + err.Error())
	}
	return sub
}
