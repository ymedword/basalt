---
title: Documentation — Understand and use Basalt
description: "What Basalt's Agent OS does, how independent agents share a runtime, current capabilities, guides and future direction."
---

<!-- markdownlint-configure-file {"MD025":{"front_matter_title":""},"MD033":{"allowed_elements":["a","script","TemplateTable"]}} -->

<script setup>
import { withBase } from 'vitepress'
import TemplateTable from '../.vitepress/theme/components/TemplateTable.vue'
</script>

# Documentation

Start with what Basalt manages, then follow the quickstart or explore its architecture.

## What is Basalt? {#what-is-basalt}

**Basalt is a self-hosted Agent OS for enterprise teams: a shared runtime and management platform for independent business agents.**

Each Agent has its own directory and AGENT.md definition for identity, model, tools, skills and tasks. It uses the platform's execution loop, model connections, scheduling, persistence and audit records. You can add another business Agent by configuring its responsibilities and resources while reusing the same infrastructure.

For example, a daily-report Agent can prepare scheduled briefings, while a file-review Agent checks files in its permitted workspace. They have separate definitions, sessions and scoped memory; both use the same underlying services. These are configuration examples, not predefined business solutions.

## Why call it an Agent OS? {#agent-os}

Here, OS describes the application platform that runs and manages Agent workloads on your infrastructure. The distinction is the scope of responsibility:

| Layer | Responsibility |
| --- | --- |
| Model | Accept context and return a response or a proposed tool call |
| Agent runtime | Assemble context, execute a bounded model/tool loop, persist state and record calls |
| Agent OS | Supply that runtime to multiple Agents and manage their lifecycle, shared resources, schedules, access policies and operations |

Basalt combines the last two layers in a Go application. The model proposes actions; the runtime checks declared tools, applies the relevant policy and records execution. Operators configure Agents and inspect their runs through the console and API.

**Independent per Agent:** identity, configuration, allowed resources, sessions and scoped business state. **Shared by the platform:** execution, model/tool adapters, scheduling, storage, audit and management services. Logical scope is not process or container fault isolation.

## What the current code provides {#current-capabilities}

- Directory-based Agent definitions, configuration, explicit enable/disable and archival.
- Explicit model connections, published resource versions, built-in tools, MCP and reusable skills.
- Bounded ReAct execution, context assembly, sessions and Agent-scoped persistent memory.
- Per-Agent schedules and delivery to configured notification channels.
- An embedded management console, HTTP API, execution records and model/tool call audit.
- A single-node Go runtime with embedded SQLite and local files; built-in tools use their respective file, command and network policies.

Long-term memory is saved when save_memory succeeds. Session history is a separate record; ordinary conversation is not automatically promoted to long-term memory. External models and tools may receive request data according to their configured services.

The management token is basic management authentication. Personal accounts, SSO and full tenant governance belong to the planned direction below.

## Core concepts {#concepts}

| Concept | Meaning |
| --- | --- |
| Agent | A named business worker with its own definition, resource declarations and scoped state |
| Run | One bounded execution of a request or scheduled task, with a recorded outcome |
| Session | A persisted conversation that can contain multiple Runs |
| Workspace | The instance's local data directory, .basalt/ by default, containing the database, Agent definitions and supporting files |
| Resource | A reusable model connection, MCP service, skill or notification channel; eligible published versions are bound to an Agent and resolved for execution |

## Get started

<a :href="withBase('/quickstart/')">Run your first agent</a>

Build, configure, enable in the console and start a CLI conversation.

## Understand

<a :href="withBase('/architecture/#overview')">Project architecture overview</a> · <a :href="withBase('/architecture/#tools-and-mcp')">Tools and MCP</a>

Start with the full project map, then use the compact summary or explore execution, modules, package dependencies and deployment in detail.

## Operate

<a :href="withBase('/quickstart/#deploy')">Deployment entry point</a> · [Operations (GitHub)](https://github.com/ymedword/basalt/blob/main/docs/Operations.md)

Management authentication, listening addresses, access policies, backups, upgrades and troubleshooting.

## Templates & demos {#templates-demos}

`examples/` ships five daily agents and eight templates. Each directory is a portable baseline: no instance IDs, no credentials and no delivery channels. Copy one into a workspace, then bind your own model, tools and channel; Basalt never imports an example and never widens the command allowlist on its own.

<TemplateTable section="demos" />

The eight templates cover weather, arXiv, policy pages, support tickets, onboarding docs, dependencies, web changes and startup funding. Six run from fixtures without credentials (tier A), one also needs the Playwright browser (tier A*), and one is a configuration recipe only (tier B). [The examples index](https://github.com/ymedword/basalt/blob/main/examples/README.md) lists every directory with its tier, dependencies and evidence status.

<TemplateTable section="templates" />

## MCP catalog {#mcp-catalog}

The console lists six MCP recipes: GitHub, Brave Search, Fetch, Playwright, read-only Filesystem and DBHub. "Add to configuration" prefills the form and never saves on its own. Saving, publishing or rolling back fails closed with a 503 and an operation ID when a referenced environment variable is missing or empty, and a recipe that has not been exercised on this machine says so. Reading the catalog starts no process, installs nothing and writes no record.

<TemplateTable section="catalog" />

## Future direction {#roadmap}

The direction is an enterprise self-hosted Agent OS where tasks can be executed, checked, traced and improved. **The following entries are planned work, not current capabilities; no delivery date is committed.**

| Priority area | Intended result | Status |
| --- | --- | --- |
| Knowledge and memory | Use private material with permission checks, traceable citations, semantic retrieval and a complete update/delete lifecycle | Planned; first capability priority |
| Diagnostics and task orchestration | Explain task failures, persist long-task progress and support bounded delegation and revision | Planned |
| Execution isolation and controlled learning | Establish isolation, evaluation and approval before proposing reusable skills; version and roll back accepted changes | Planned |
| Distributed execution | Externalize state and coordinate task ownership, budgets and recovery across nodes | Planned |
| Enterprise governance and integrations | Add enterprise identity, roles, tenant/project boundaries, audit queries, quotas, approvals and richer channel access | Planned |
| Goal-to-deliverable workflows | Organize specialist Agents around a business goal with traceable artifacts and bounded review | Longer-term direction |

Knowledge and memory come first. Diagnostics support later orchestration; isolation and learning admission precede controlled capability updates. Distributed execution and enterprise governance build on those foundations. Each area needs its own design review and reproducible validation. Features described here are not enabled by changing a configuration switch in the current version.

## Project

[GitHub](https://github.com/ymedword/basalt) · [Issues](https://github.com/ymedword/basalt/issues)

This version has no site search. Use the section outline and the guide links above.
