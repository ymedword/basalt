// Package app 是唯一允许"什么都知道"的装配层——把各包实现装成可运行实例。
// Package app is the one assembly layer that is allowed to know everything; it
// wires package implementations into a runnable instance.
//
// 本包承载进程退出码的唯一出口（Run）与日志装配（NewLogger）；业务逻辑
// 不得写在这里，装配顺序与依赖方向见 docs/TechnicalSolution.md §10.1。
//
// The process exit code has exactly one outlet here (Run), as does logger
// construction (NewLogger). No business logic belongs in this package.
package app
