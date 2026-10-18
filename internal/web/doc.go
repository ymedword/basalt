// Package web 承载 HTTP 接入面：统一响应信封、路由、中间件链、视图投影与
// handlers。所有端点挂在 /api/v1 下；成功与失败都以信封表达，固定字面量以
// docs/contracts/openapi.yaml 与 docs/contracts/run-and-errors.md 为准。
//
// 依赖纪律：本包只依赖 internal/core 与标准库，不得 import 其他 internal 包。
// HTTP 层不写消息、不执行工具、不另起 ReAct 循环——运行统一经 RunService 端口
// 交回既有执行入口（准入 / Run 账本 / 审计 / 消息落库都在那里）。
//
// Package web holds the HTTP surface: the response envelope, routing, the
// middleware chain, view projections and handlers. Every endpoint lives under
// /api/v1 and speaks the envelope on both success and failure; fixed literals
// follow docs/contracts/openapi.yaml and docs/contracts/run-and-errors.md.
//
// Dependency rule: this package depends only on internal/core and the standard
// library. The HTTP layer never writes messages, executes tools, or starts a
// ReAct loop of its own — runs go through the RunService port into the existing
// execution entry point (admission, run ledger, audit and message persistence
// all live there).
package web
