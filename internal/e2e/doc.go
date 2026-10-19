// Package e2e 是 US-15 的进程级回填夹具：只测**真实二进制**，不测组件内函数。
// Package e2e is the process-level backfill harness for US-15: it exercises the
// real binary, never package-internal functions.
//
// 包约定（与模块卡 §3.1.1 同口径）：
//   - 测试文件全部带 build tag：`e2e`（默认回填面）、`e2e && linux`（信号场景）、
//     `e2e && slow`（300s 运行超时）、`e2e && real`（真实模型 / 真实外发）。
//     本文件**不带** tag——否则不带 tag 的构建会报 "build constraints exclude all Go files"；
//     它不含可执行语句，覆盖率判卷按 [no statements] 跳过。
//   - 每个场景的断言按四态口径写：通过 / 失败 / 跳过 / 未验证。回填要求"缺环境即
//     FAIL 不得 SKIP"的场景（真实项）在断言里显式拒绝跳过；框架级 SKIP 只允许出现在
//     非必验路径上。
//   - 夹具复用 internal/testutil：假模型（NewFakeOpenAIServer）、HTTP 回环
//     （NewHTTPServer）、MCP helper（RunMCPHelper）与临时工作区（TempWorkspace）。
//     本包只新增"编排真实进程"的一层，不重写上述夹具。
//   - 外部服务一律回环：假模型、接收器、MCP stdio 全部监听 127.0.0.1 或走进程内管道，
//     测试**不得**访问真实网络、真实凭证或仓库外用户目录。
//   - `TestMain` 在包内构建一次被测二进制并把 `binary_sha256=` 标记打到标准输出；
//     helper 子进程与 `-test.list` 模式短路、不构建、不打印。
//
// Package conventions: every test file carries a build tag; assertions follow the
// four-state vocabulary; fixtures are reused from internal/testutil; all external
// services are loopback-only; TestMain builds the binary once and prints exactly one
// `binary_sha256=` marker (helper subprocesses and list mode print nothing).
package e2e
