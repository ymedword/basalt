---
title: Architecture — Basalt, self-hosted Agent OS
description: "The Compose multi-Worker Agent OS: system boundaries, five core capabilities, execution, published-asset distribution, module dependencies and deployment."
---

<!-- markdownlint-configure-file {"MD025":{"front_matter_title":""},"MD033":{"allowed_elements":["a","DiagramFigure"]}} -->

# Architecture

Basalt hosts multiple independent business agents on a unified Go runtime and management foundation. Identity, configuration and business state are scoped per agent; execution, connections, scheduling, storage and auditing are shared.

## Project architecture {#overview}

The project uses five logical layers: access, application, execution engine, infrastructure services and storage. Independent Agents are managed objects in the application layer, alongside lifecycle, resource and scheduling services. A `serve` control plane owns the HTTP API, the embedded console and asset publication; `gateway` Workers claim due executions and run them without an HTTP listener. Security, budgets and auditing apply across layers. External model endpoints, MCP/HTTP systems, local subprocesses and notification destinations sit outside the Go runtime boundary.

<DiagramFigure id="project-overview" />

| Layer | Responsibilities |
| --- | --- |
| Access | CLI, HTTP API and the embedded console |
| Application | Agent definitions and lifecycle, resource management, Run admission, task scheduling and published asset bundles |
| Execution engine | PromptBuilder, ReActLoop and ToolExecutor on every Worker |
| Infrastructure services | Model adapters, memory, built-in/MCP tools, skills and notifications |
| Storage | One self-hosted PostgreSQL for runtime state and audit, plus per-node workspace files and content-addressed asset bundles |

The layers describe runtime responsibilities. Actual Go import directions are shown separately in the package-dependency view. Both Agent examples reuse the engine and infrastructure services, with separate declarations, sessions and scoped memory. CLI writers and serve share an exclusive workspace lock.

### Published assets and Workers

A control-plane publication writes the payload first and then moves the pointer, so a Worker never reads a half-published version. Workers synchronize the immutable bundle, materialize its skills, prepare resources and refresh the registry before claiming work; they install bundles by content hash and never publish. Each claimed execution pins one bundle hash, and admission verifies that exact directory before dispatch. A publication that arrives while a Worker is executing leaves the in-flight Run on its pinned version; new Runs use the new one.

## Five core capabilities {#capabilities}

### Provider

Explicit model connections, published versions and a frozen mapping for each run.

### ReAct

A Go execution loop owns model requests, tool calls, budgets and stopping.

### Memory

Scoped persistent memory, core-context injection and keyword recall.

### Tool

Declared tools, skills and MCP, with policy checks and audited execution.

### Web Service

An embedded console and authenticated HTTP API share the same runtime.

Demo management examples: daily-report uses HTTP tools for scheduled briefings; file-review uses file tools for workspace checks. They can use different models and skills, with separate sessions, tasks and memory on the same foundation. These are illustrative roles; configure the actual connections, tools and access policies in the console.

## How a run executes {#execution}

Admission checks the agent lifecycle, resources and budget, and freezes the resource versions. Context assembly feeds the Go ReAct loop. Model and tool calls require a pending audit record before dispatch, followed by a call-level terminal update. Tools come only from the agent’s declared set and pass policy checks.

Run terminal states are completed, limit_reached, cancelled and failed. limit_reached means budget stopping with partial results, not an error; these states are distinct from call-level audit success/failed. The execution diagram includes denial, failure, cancellation and crash recovery. Recovery does not blindly replay external actions; a failed audit-terminal write leaves pending evidence and an error record.

## Tools and MCP {#tools-and-mcp}

AGENT.md declares tools, skills and MCP references. Skills are disclosed on demand. MCP connections supply protocol tools without owning the execution loop. Built-in file, command and HTTP tools enforce their respective access policies. Native and MCP tools share declared-tool checks and auditing; an external MCP server's own file or network operations are outside the local tool whitelist. Configure and trust that server separately.

## Focused architecture views {#diagrams}

The compact summary below is also used in the README. Four additional views explain execution, module responsibilities, source-code dependencies and deployment:

| View | What it explains |
| --- | --- |
| System architecture summary | The five logical layers and their cross-cutting controls, at a glance |
| Execution flow | How a request moves through model calls, tools, auditing and stopping |
| Module map | How model access, execution, memory, tools and web services divide responsibilities |
| Go package dependencies | Actual import directions and source-code boundaries |
| Deployment | Compose multi-Worker: the control plane, Workers, shared PostgreSQL and per-node files |

<DiagramFigure id="system-architecture" />

<DiagramFigure id="execution-flow" />

<DiagramFigure id="module-map" />

<DiagramFigure id="package-dependencies" />

<DiagramFigure id="deployment" />

## Current scope {#current-scope}

This version provides Compose multi-Worker hosting: a control plane, one or more Workers and one shared PostgreSQL, with published assets distributed automatically and pinned per execution. The single-instance Compose file remains the default. Opt-in durable sequential flows coordinate immutable published steps through the original Run and audit path; unknown side effects stop advancement. Kubernetes, database high availability and cross-node business-state sharing remain outside this version, as do parallel graphs, automatic learning, enterprise SSO and multi-tenant governance. Logical scopes do not provide process or container fault isolation. Data sent to model endpoints and external tools follows their configured services.

## Opt-in task isolation

Linux deployments can explicitly bind controlled shell commands to hardened OCI/runc and generated code (`run_code`) to gVisor/runsc. Both run without network access or service credentials, with fixed images and bounded resources. Startup verifies runtime capability and signed image trust; unavailable isolation refuses execution. The default remains local execution. Output space has an instance-wide hard limit and can be contended by tasks; v1 has no per-task output filesystem quota.

The existing console displays execution profiles and cleanup outcomes without adding configuration controls. MCP, HTTP and notification tools keep their existing paths. A service container alone does not isolate individual tasks. microVMs, automatic isolation of all tools and equivalent native Windows/macOS isolation are outside this increment.
