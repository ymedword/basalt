// Package tool 承载工具与沙箱：工具注册、授权判定与命令执行。
// Package tool holds tools and the sandbox: registration, authorization and
// command execution.
//
// 骨架占位：实现随 US-4 / US-5 落位。约束先行记在此处——授权与执行分离为
// Policy.Check 与 CommandRunner.Run 两接口，调用方不得依赖具体实现；工具
// 调度唯一归属 ToolExecutor（宪法第 6 条）。
//
// Skeleton placeholder; the implementation lands with US-4 / US-5. Authorization
// and execution stay apart (Policy.Check vs CommandRunner.Run) and callers must
// not depend on concrete implementations.
package tool
