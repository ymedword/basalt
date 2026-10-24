export const CAPABILITIES = [
  { id: 'define', tagEn: 'DEFINE', tagZh: '定义', titleEn: 'Independent agents', titleZh: '独立业务 Agent', bodyEn: 'Give each agent its own definition, workspace, sessions and scoped memory.', bodyZh: '分别定义 Agent，按作用域管理工作区、会话与记忆；复用公共运行底座。', illustration: '/illustrations/01-define.svg', altEn: 'Separate basalt columns on a shared foundation', altZh: '共享基础上的独立岩柱', evidence: 'internal/workspace/agent.go' },
  { id: 'extend', tagEn: 'EXTEND', tagZh: '扩展', titleEn: 'Models, tools & skills', titleZh: '模型、工具与技能', bodyEn: 'Configure models and extend agents with built-in tools, MCP and reusable skills.', bodyZh: '配置模型连接，使用内置工具、MCP 和可复用技能扩展 Agent。', illustration: '/illustrations/02-extend.svg', altEn: 'Interlocking modular instruments', altZh: '相互连接的模块化构件', evidence: 'internal/tool/mcptools.go' },
  { id: 'schedule', tagEn: 'SCHEDULE', tagZh: '调度', titleEn: 'Schedules & notifications', titleZh: '定时任务与通知', bodyEn: 'Run recurring tasks and deliver results to configured notification channels.', bodyZh: '按计划执行任务，并将结果发送到配置的通知渠道。', illustration: '/illustrations/03-schedule.svg', altEn: 'Clock dial and notification signals', altZh: '时钟与通知信号', evidence: 'internal/core/scheduler.go' },
  { id: 'remember', tagEn: 'REMEMBER', tagZh: '记忆', titleEn: 'Persistent memory', titleZh: '持久记忆', bodyEn: 'Save agent-scoped memories and recall them across conversations.', bodyZh: '保存 Agent 作用域内的记忆，并在后续会话中使用或检索。', illustration: '/illustrations/04-remember.svg', altEn: 'Layers of recorded information', altZh: '分层保存的记录', evidence: 'internal/memory/memory.go' },
  { id: 'observe', tagEn: 'OBSERVE', tagZh: '观测', titleEn: 'Runs you can inspect', titleZh: '可查看的执行过程', bodyEn: 'Review runs, model and tool activity, and management operations.', bodyZh: '查看运行、模型与工具调用记录，以及管理操作。', illustration: '/illustrations/05-observe.svg', altEn: 'Aligned execution records and measurements', altZh: '对齐的执行记录与测量轨迹', evidence: 'internal/web/handlers_runs.go' },
  { id: 'self-host', tagEn: 'SELF-HOST', tagZh: '自托管', titleEn: 'Self-hosted control', titleZh: '自托管与访问控制', bodyEn: 'Run Basalt in Go, with file, command and network access policies.', bodyZh: '使用 Go 服务运行 Basalt，按策略控制文件、命令与网络访问。', illustration: '/illustrations/06-self-host.svg', altEn: 'A computing module within a defined deployment boundary', altZh: '明确部署边界中的计算单元', evidence: 'internal/tool/policy.go' }
]
export const CORE_CAPABILITIES = [
  { id: 'provider', titleEn: 'Provider', titleZh: '模型接入', bodyEn: 'Explicit model connections, published versions and a frozen mapping for each run.', bodyZh: '显式模型连接、受控版本发布，以及每次运行固定的模型映射。', evidence: 'internal/app/providers.go' },
  { id: 'react', titleEn: 'ReAct', titleZh: '执行循环', bodyEn: 'A Go execution loop owns model requests, tool calls, budgets and stopping.', bodyZh: 'Go 执行循环统一负责模型请求、工具调用、预算和停止。', evidence: 'internal/core/loop.go' },
  { id: 'memory', titleEn: 'Memory', titleZh: '记忆', bodyEn: 'Scoped persistent memory, core-context injection and keyword recall.', bodyZh: '作用域内的持久记忆、核心记忆注入与关键词回查。', evidence: 'internal/memory/memory.go' },
  { id: 'tool', titleEn: 'Tool', titleZh: '工具', bodyEn: 'Declared tools, skills and MCP, with policy checks and audited execution.', bodyZh: '声明式工具、技能和 MCP，执行前检查策略并记录审计。', evidence: 'internal/core/tool_exec.go' },
  { id: 'web', titleEn: 'Web Service', titleZh: 'Web 服务', bodyEn: 'An embedded console and authenticated HTTP API share the same runtime.', bodyZh: '内嵌管理台与经过认证的 HTTP API 使用同一运行底座。', evidence: 'internal/web/server.go' }
]
export const DIAGRAMS = [
  { id: 'project-overview', titleEn: 'Project architecture overview', titleZh: '项目架构全景', descriptionEn: 'Five logical layers: access, application, execution engine, infrastructure services and storage, with cross-cutting security and audit.', descriptionZh: '按接入层、应用层、执行引擎层、基础服务层、存储层划分，安全与审计贯穿各层。', width: 1600, height: 1690 },
  { id: 'system-architecture', titleEn: 'System architecture summary', titleZh: '系统架构简图', descriptionEn: 'A compact view of five logical layers, with security and operations across them.', descriptionZh: '用五个逻辑层概括系统，安全与运行保障贯穿各层。', width: 1360, height: 640 },
  { id: 'execution-flow', titleEn: 'Core execution flow', titleZh: '核心执行流程', descriptionEn: 'Admission, model and tool calls, auditing, bounded stopping and recovery.', descriptionZh: '准入、模型与工具调用、审计、有界停止及恢复。' },
  { id: 'module-map', titleEn: 'Module map', titleZh: '模块职责', descriptionEn: 'Responsibilities and the interfaces connecting implementation modules.', descriptionZh: '实现模块的职责与协作接口。' },
  { id: 'package-dependencies', titleEn: 'Go package dependencies', titleZh: 'Go 包依赖', descriptionEn: 'Actual package import directions in the current Go source.', descriptionZh: '当前 Go 源码中的真实包导入方向。' },
  { id: 'deployment', titleEn: 'Current deployment', titleZh: '当前部署', descriptionEn: 'A single Go process, local state and explicitly configured external services.', descriptionZh: '单个 Go 进程、本地状态与显式配置的外部服务。' }
]
export const CURRENT_SCOPE_NOTE = {
  en: 'This version hosts multiple agents in one runtime. Agent collaboration, multi-node execution, vector retrieval, automatic learning, SSO and multi-tenant governance are outside this version. Logical scopes do not provide process or container fault isolation.',
  zh: '当前版本在统一运行时托管多个 Agent。Agent 协作、多节点执行、向量检索、自动学习、SSO 和多租户治理不属于本版本。逻辑作用域不等同于进程或容器故障隔离。'
}
