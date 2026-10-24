---
title: 架构 — 独立 Agent 与共享底座
description: 当前单节点 Agent OS 的系统边界、五项核心能力、执行流程、模块依赖和部署图。
---

<!-- markdownlint-configure-file {"MD025":{"front_matter_title":""},"MD033":{"allowed_elements":["a","DiagramFigure"]}} -->

# 架构

Basalt 是基于 Go 的统一运行与管理底座，托管多个独立业务 Agent。Agent 的身份、配置与业务状态按作用域管理；执行、接入、调度、存储和审计由底座共享。

## 项目架构全景 {#overview}

项目按接入层、应用层、执行引擎层、基础服务层、存储层五个逻辑层次划分。独立 Agent 是应用层管理的业务对象，生命周期、资源管理和任务调度也放在应用层；安全、预算与审计贯穿各层。模型端点、MCP／HTTP 业务系统、本地子进程和通知接收方位于 Go 运行边界之外。

<DiagramFigure id="project-overview" />

| 层次 | 主要职责 |
| --- | --- |
| 接入层 | 命令行、HTTP API 与内嵌管理台 |
| 应用层 | Agent 定义和生命周期、资源管理、运行准入与任务调度 |
| 执行引擎层 | PromptBuilder、ReActLoop 与 ToolExecutor |
| 基础服务层 | 模型适配、记忆、内置／MCP 工具、技能和通知 |
| 存储层 | 内嵌 SQLite 与工作区文件 |

这里描述运行时职责，源码包的实际导入方向由包依赖专题图单独说明。两个 Agent 示例复用执行引擎与基础服务，分别管理声明、会话和作用域记忆；CLI 写命令与 serve 共用工作区实例锁。

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
| 部署结构 | Go 进程、本地数据、浏览器和外部服务如何连接 |

<DiagramFigure id="system-architecture" />

<DiagramFigure id="execution-flow" />

<DiagramFigure id="module-map" />

<DiagramFigure id="package-dependencies" />

<DiagramFigure id="deployment" />

## 当前版本边界 {#current-scope}

本版本为单节点、多 Agent 托管。Agent 协作、多节点分布式执行、向量检索、自动学习、企业 SSO 与多租户治理不属于本版本。逻辑作用域不等同于进程或容器故障隔离。模型与外部工具的数据流按所配置的服务决定。
