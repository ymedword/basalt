// Package provider 承载 LLM 供应商客户端——只做协议调用，不接管循环。
// Package provider holds LLM vendor clients: protocol calls only, never a loop.
//
// 骨架占位：实现随 US-2 落位。约束先行记在此处——不得启用任何自动工具循环，
// 工具调度唯一归属 ToolExecutor；装配处一次构建 provider name → 客户端实例的
// 显式映射表，路由只查表（宪法第 2、3 条）。
//
// Skeleton placeholder; the implementation lands with US-2. The constraints are
// recorded up front: no automatic tool loops, tool dispatch belongs to
// ToolExecutor alone, and routing consults an explicitly built lookup table.
package provider
