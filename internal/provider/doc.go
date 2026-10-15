// Package provider 承载 LLM 供应商对接——协议调用与调用审计，不接管循环。
// Package provider holds LLM vendor integration: protocol calls plus call audit,
// never a loop.
//
// 两个交付物：
//
//   - OpenAIClient（client.go）：一次 Chat Completions 调用。协议类型与已知字段的
//     编解码借 go-openai，**HTTP 往返自建**（net/http + encoding/json、一次 POST、
//     不重试、不 fallback、不跟随重定向）——库不接管会话历史、不执行工具。
//   - ProviderService（service.go）：name → 客户端实例的**显式映射表**路由（只查表，
//     不遍历推断——Go map 遍历顺序随机），外加两阶段审计写：预写 pending → 调用 →
//     回填终态；预写失败即**拒绝发出**（fail-closed）。
//
// 硬约束（宪法第 2、3、5 条）：不得启用任何自动工具循环，工具调度唯一归属
// ToolExecutor；映射表在装配处一次构建、之后只读；审计先留痕再发出。
//
// Two deliverables: OpenAIClient (one Chat Completions call over a self-built HTTP
// round trip; the SDK supplies protocol types and codec only) and ProviderService
// (explicit name-to-client routing plus the two-phase, fail-closed audit write).
package provider
