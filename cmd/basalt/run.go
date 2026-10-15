package main

import (
	"context"

	"github.com/ymedword/basalt/internal/app"
)

// run 是入口包的薄包装：把构建期注入的版本交给装配层，转交参数并原样回传退出码。
// run is the thin entry wrapper: it hands the injected version to the assembly
// layer, forwards the arguments and returns the exit code unchanged.
//
// 单独成函数是为了入口包可测（--version / 未知命令，覆盖率判据不豁免入口包）；
// main 只做 os.Exit 一件事。这里不是信号处理的落点——取消语义由需要它的命令
// 自行接线。
// It exists so the entry package is testable; main only calls os.Exit.
func run(args []string) int {
	app.Version = version
	return app.Run(context.Background(), args)
}
