package main

import (
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run 是入口的薄包装：每个目录一行计数写标准输出（顺序与参数一致），
// 用法错误返回 2、执行失败返回 1。
// run is the thin, testable wrapper: one count per directory on stdout, in
// argument order; 2 for usage errors and 1 for failures.
func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: stmtcount <包目录> [<包目录>...]")
		return 2
	}
	for _, dir := range args {
		count, err := Count(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stmtcount: %v\n", err)
			return 1
		}
		fmt.Println(count)
	}
	return 0
}
