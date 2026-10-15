// Package cli 承载命令行面：cobra 根命令、两条子命令与配置装载。
// Package cli carries the command-line surface: the cobra root command, its
// subcommands, and configuration loading.
//
// 本包不直连存储实现——它经 core 的接口与 storage 的能力函数接线，
// 依赖方向 cli → core（宪法第 7 条：WorkDir / Logger 等一律显式类型化传递）。
//
// This package talks to storage through capability functions and to core
// through interfaces; nothing is smuggled through context values.
package cli
