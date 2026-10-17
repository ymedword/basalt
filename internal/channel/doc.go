// Package channel 承载渠道接入：外部消息进、回复出的适配面。
// Package channel holds channel adapters: messages in, replies out.
//
// **编码纪律（技术方案 §6.1.1——平台差异是这里最大的坑）**：
//
//   - 真控制台经**控制台 API** 读写（Windows：`ReadConsoleW` / `WriteConsoleW`；
//     Unix：终端本身就是 UTF-8 字节流），**不**依赖标准输入输出的字节流假设；
//   - 管道 / 重定向：两侧一律**显式按 UTF-8**；
//   - "是否终端"由平台**终端属性查询**回答（Windows `GetConsoleMode`；Linux
//     `TCGETS`；Darwin `TIOCGETA`）——**不得**靠终端名或环境变量猜；
//   - 平台差异用 build tag 分文件：`console_windows.go` / `console_linux.go` /
//     `console_darwin.go`（Darwin 的请求码与 Linux 不同，**不得**合并成一个文件）。
//
// Encoding discipline: real consoles go through the platform console APIs, pipes and
// redirects are explicit UTF-8, and "is this a terminal" is answered by a platform
// attribute query — never by guessing at environment variables.
//
// 渠道地址（未来 HTTP 类渠道）属凭证——对外（响应 / 日志 / 错误文案）只允许出现
// scheme + host。
// Channel addresses are credentials: outputs may only ever show scheme + host.
package channel
