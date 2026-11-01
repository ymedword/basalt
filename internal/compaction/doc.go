// Package compaction 是会话压缩引擎：每轮模型调用前判断是否需要压缩、按会话与消息顺序
// 定覆盖范围与切点、调用摘要、追加压缩条目、重建投影并做失效校验。
// Package compaction is the session compaction engine: decide before each model call
// whether to compact, derive coverage and cut points from session message order, run the
// summary, append the entry, rebuild the projection and validate invalidation.
//
// **只说自己的事**：本包**不**组装提示词（PromptBuilder 归 core）、**不**写消息仓储、
// **不**生成任务状态。
// It does one job only: prompt assembly stays in core, message persistence stays in the
// storage adapter, and task state is never produced here.
//
// **依赖方向**：只 import core 与标准库；消息经 core.HistorySource、存储经
// CompactionStore 端口注入——core **不得**反向 import 本包。
// Dependency direction: core plus the standard library only. Messages arrive through
// core.HistorySource and persistence through the CompactionStore port.
//
// **摘要不是消息，链条只追加**：压缩条目不写回 messages，读只取当前 chain_epoch 的
// committed 链并做链资格校验；祖先失效则换新 epoch 重建根，中段失效则回退到上一份可注入
// 摘要——**不得**把旧摘要重提到新范围，**不得**返回半成品摘要。
// Summaries are not messages and the chain is append-only: entries are never written back
// into messages, reads take only committed links of the current epoch after a chain
// qualification check, and a stale ancestor rebuilds a new root in a new epoch instead of
// re-raising an old summary onto new coverage.
package compaction
