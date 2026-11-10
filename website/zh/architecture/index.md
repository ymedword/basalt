---
title: 架构 — Basalt｜企业自托管 Agent OS
description: Compose 多 Worker Agent OS 的系统边界、五项核心能力、执行流程、已发布资产分发、模块依赖和部署图。
---

<!-- markdownlint-configure-file {"MD025":{"front_matter_title":""},"MD033":{"allowed_elements":["a","DiagramFigure"]}} -->

# 架构

Basalt 是基于 Go 的统一运行与管理底座，托管多个独立业务 Agent。Agent 的身份、配置与业务状态按作用域管理；执行、接入、调度、存储和审计由底座共享。

## 项目架构全景 {#overview}

项目按接入层、应用层、执行引擎层、基础服务层、存储层五个逻辑层次划分。独立 Agent 是应用层管理的业务对象，生命周期、资源管理和任务调度也放在应用层。`serve` 控制面负责 HTTP API、内嵌管理台与资产发布；`gateway` Worker 领取到期执行并在无 HTTP 监听的进程内运行。安全、预算与审计贯穿各层。模型端点、MCP／HTTP 业务系统、本地子进程和通知接收方位于 Go 运行边界之外。

<DiagramFigure id="project-overview" />

| 层次 | 主要职责 |
| --- | --- |
| 接入层 | 命令行、HTTP API 与内嵌管理台 |
| 应用层 | Agent 定义和生命周期、资源管理、运行准入、任务调度与已发布资产包 |
| 执行引擎层 | 每个 Worker 上的 PromptBuilder、ReActLoop 与 ToolExecutor |
| 基础服务层 | 模型适配、记忆、内置／MCP 工具、技能和通知 |
| 存储层 | 一个自托管 PostgreSQL 保存运行状态与审计，另加每节点工作区文件与内容寻址资产包 |

这里描述运行时职责，源码包的实际导入方向由包依赖专题图单独说明。两个 Agent 示例复用执行引擎与基础服务，分别管理声明、会话和作用域记忆；CLI 写命令与 serve 共用工作区实例锁。

### 已发布资产与 Worker

控制面发布时先写载荷、再移动指针，Worker 不会读到半发布版本。Worker 同步不可变资产包、物化其中的技能、准备资源并刷新注册表后才领取任务；按内容哈希安装，且从不发布。每个领取的执行固定一个 bundle 哈希，准入先校验这一份目录再发出调用。执行期间到达的新发布不影响在途 Run 的固定版本，后续 Run 使用新版本。

## 五项核心能力 {#capabilities}

### 模型接入

显式模型连接、受控版本发布，以及每次运行固定的模型映射。

### 执行循环

Go 执行循环统一负责模型请求、工具调用、预算和停止。

### 记忆

作用域内的持久记忆、核心记忆注入与关键词回查。

### 工具

声明式工具、技能和 MCP，执行前检查策略并记录审计。

### Web 服务

内嵌管理台与经过认证的 HTTP API 使用同一运行底座。

演示管理示例：daily-report 使用 HTTP 工具定时整理简报；file-review 使用文件工具检查工作区。它们可以使用不同模型和技能，分别管理会话、任务与记忆，复用同一底座。示例是用途说明，实际连接、工具和访问策略在管理台配置。

## 一次 Run 如何执行 {#execution}

运行先检查 Agent 生命周期、资源与预算，并固定使用的资源版本。上下文组装后进入自实现 ReAct 循环。模型和工具调用都必须先写 pending 审计，再发出调用，随后回填调用终态。工具只来自该 Agent 的声明集合，并经策略检查。

Run 的四个终态为 completed、limit_reached、cancelled、failed。limit_reached 表示带部分结果的预算停止，不是错误；它与单次调用审计的 success／failed 不是同一组状态。拒绝、失败、取消与崩溃恢复均在执行流图中表达。恢复不盲目重放外部动作；审计终态写失败时保留 pending 并记录故障。

## 工具与 MCP {#tools-and-mcp}

AGENT.md 声明工具、技能与 MCP 引用。技能按需披露；MCP 连接提供协议工具，不能接管运行循环。内置文件、命令和 HTTP 工具执行各自的访问策略。原生工具与 MCP 统一检查声明归属并记录审计；外部 MCP 服务自身的文件或网络操作不受本地工具白名单约束，需要单独配置并信任该服务。

## 专题架构视图 {#diagrams}

下面的架构简图也用于 README；其余四类图分别展开执行过程、模块职责、源码依赖与部署结构：

| 视角 | 说明什么 |
| --- | --- |
| 系统架构简图 | 用五个逻辑层与贯穿各层的约束概括系统 |
| 执行流程 | 一次请求如何经过模型调用、工具执行、审计和停止处理 |
| 模块职责 | 模型接入、执行、记忆、工具与 Web 服务如何分工 |
| Go 包依赖 | 源码包之间实际的导入方向，帮助开发者理解代码边界 |
| 部署结构 | Compose 多 Worker：控制面、Worker、共享 PostgreSQL 与每节点文件 |

<DiagramFigure id="system-architecture" />

<DiagramFigure id="execution-flow" />

<DiagramFigure id="module-map" />

<DiagramFigure id="package-dependencies" />

<DiagramFigure id="deployment" />

## 当前版本边界 {#current-scope}

本版本支持 Compose 多 Worker：一个控制面、一个或多个 Worker 与一个共享 PostgreSQL，已发布资产自动分发并按执行固定版本。单实例 Compose 仍是默认档。可选持久顺序流程通过原 Run 与审计路径协调固定发布步骤，未知副作用停止推进。K8s、数据库高可用与跨节点业务状态共享仍不属于本版本；并行图、自动学习、企业 SSO 与多租户治理同样后置。逻辑作用域不等同于进程或容器故障隔离。模型与外部工具的数据流按所配置的服务决定。

## 显式启用任务隔离

Linux 部署可将受控 shell 命令绑定到加固 OCI/runc，将生成代码工具 `run_code` 绑定到 gVisor/runsc。两档均无网络与服务凭据，镜像固定并验证签名信任，资源有界；启动探针不足时拒绝执行。缺省仍为本地执行。输出区有实例总量硬限，任务可能争用容量；v1 没有单任务输出文件系统硬配额。

既有管理台只读展示执行档和清理结果。MCP、HTTP、通知工具保持原执行路径；服务运行在容器里不代表各任务已隔离。microVM、全工具自动隔离及 Windows/macOS 原生同等隔离不在本次增量中。
