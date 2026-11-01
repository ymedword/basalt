// Package callers 是调用方接入层：Key 的认证与生命周期、三类入口的速率与并发名额、
// 周期与容量配额。它只做认证、解析与限额，**不定义**新的调用方类型——身份统一用
// core.Principal，core 不反向 import 本包。
// Package callers is the caller ingress layer: key authentication and lifecycle, the rate
// and concurrency budgets of the three ingress classes, and period plus capacity quotas.
// It authenticates, resolves and limits only; identity is core.Principal and core never
// imports this package.
//
// **失败一律无差别**：不存在 / 过期 / 吊销统一返回 ErrUnauthorized，不泄露区分；
// **不得**提供「从某个凭据是否存在推断管理能力」的入口——管理能力只由启动期的
// core.AdminPosture 显式表达。
// Failures are indistinguishable: absent, expired and revoked all return ErrUnauthorized,
// and there is no accessor that infers management capability from credential presence.
//
// **被拒请求零占用**：限流与配额判定在占用任何名额之前完成，拒绝路径不得留下运行槽位、
// 预留行或计数（准入顺序见 contracts/run-and-errors.md §8）。
// Rejected requests consume nothing: rate and quota decisions happen before any slot is
// taken, so the denial path leaves no run slot, reservation row or counter behind.
//
// **配额服务故障 fail-closed**：配额存储不可用时拒绝新 Run，不静默放行。
// Quota-store failures fail closed: a new run is refused rather than admitted.
package callers
