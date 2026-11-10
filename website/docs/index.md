---
title: Documentation — Basalt, self-hosted Agent OS
description: "What Basalt's Agent OS does, how independent agents share a runtime, current capabilities, engineering verification and future direction."
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
- Opt-in durable sequential flows: immutable task contracts, bounded Agent/tool/subflow steps, original Run/audit links, evidence verification and unknown-side-effect stops.
- An embedded management console, HTTP API, execution records and model/tool call audit.
- A Go runtime that runs as a control plane (`serve`) plus optional Workers (`gateway`) over one self-hosted PostgreSQL (vector and pg_trgm extensions); built-in tools use their respective file, command and network policies.
- Published asset distribution: the control plane publishes an immutable bundle per Agent, Workers install it by content hash, pin each claimed execution and never publish assets.

Long-term memory is saved when save_memory succeeds. Session history is a separate record; ordinary conversation is not automatically promoted to long-term memory. External models and tools may receive request data according to their configured services.

The management token is basic management authentication. Personal accounts, SSO and full tenant governance belong to the planned direction below.

## Core concepts {#concepts}

| Concept | Meaning |
| --- | --- |
| Agent | A named business worker with its own definition, resource declarations and scoped state |
| Run | One bounded execution of a request or scheduled task, with a recorded outcome |
| Session | A persisted conversation that can contain multiple Runs |
| Workspace | The instance's local data directory, .basalt/ by default, holding Agent definitions, their lifecycle state under control/ and supporting files; sessions, runs, audit and the rebuildable lifecycle index live in PostgreSQL |
| Resource | A reusable model connection, MCP service, skill or notification channel. Model connections and MCP servers are bound through published versions; skills bind by the AGENT.md declaration and install the published version; notification channels are referenced by name |

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

`examples/` ships five daily agents and eight templates. Each directory is a portable baseline: no instance IDs, no credentials and no delivery channels. On a new instance's first start Basalt installs the five daily agents as disabled built-ins; an existing workspace is not given them automatically. To use a template, copy its directory into the workspace, then bind your own model, tools and channel; a copied example never widens the command allowlist on its own.

<TemplateTable section="demos" />

The eight templates cover weather, arXiv, policy pages, support tickets, onboarding docs, dependencies, web changes and startup funding. Six run from fixtures without credentials (tier A), one also needs the Playwright browser (tier A*), and one is a configuration recipe only (tier B). [The examples index](https://github.com/ymedword/basalt/blob/main/examples/README.md) lists every directory with its tier, dependencies and evidence status.

<TemplateTable section="templates" />

## MCP catalog {#mcp-catalog}

The console lists six MCP recipes: GitHub, Brave Search, Fetch, Playwright, read-only Filesystem and DBHub. "Add to configuration" prefills the form and never saves on its own. Saving, publishing or rolling back fails closed with a 503 and an operation ID when a referenced environment variable is missing or empty, and a recipe that has not been exercised on this machine says so. Reading the catalog starts no process, installs nothing and writes no record.

<TemplateTable section="catalog" />

## Engineering verification {#verification}

The checks below exist in this repository; running them produces the evidence, and no prose badge replaces a run. `make check` is the seven-gate definition of done: format, static analysis, security, race tests, PostgreSQL integration, coverage floors and the release build. `make acceptance` runs that gate plus the fixed behaviour and injection evaluations.

| Check | Command | Enforced by |
| --- | --- | --- |
| Formatting | `make fmt` | [scripts/check-format.sh](https://github.com/ymedword/basalt/blob/main/scripts/check-format.sh) |
| Static analysis and naming | `make lint` | [Makefile](https://github.com/ymedword/basalt/blob/main/Makefile), [internal/app](https://github.com/ymedword/basalt/tree/main/internal/app) |
| Unit and race tests | `make test` | [internal/core](https://github.com/ymedword/basalt/tree/main/internal/core) |
| PostgreSQL integration | `make test-integration` (requires `BASALT_TEST_PG_DSN`; it fails closed instead of skipping) | [internal/storage/postgres](https://github.com/ymedword/basalt/tree/main/internal/storage/postgres) |
| Coverage floors | `make cover` | [scripts/check-coverage.sh](https://github.com/ymedword/basalt/blob/main/scripts/check-coverage.sh) |
| Release scenarios against the built binary | `make e2e` | [internal/e2e](https://github.com/ymedword/basalt/tree/main/internal/e2e) |
| Full acceptance | `make acceptance` | [scripts/check-acceptance.sh](https://github.com/ymedword/basalt/blob/main/scripts/check-acceptance.sh) |
| Fixed literals | contract files | [schema.sql](https://github.com/ymedword/basalt/blob/main/docs/contracts/schema.sql), [run-and-errors.md](https://github.com/ymedword/basalt/blob/main/docs/contracts/run-and-errors.md), [openapi.yaml](https://github.com/ymedword/basalt/blob/main/docs/contracts/openapi.yaml) |

The CLI animation on the home page is a labelled local fixture: it demonstrates the execution path, not model quality. Real model and external-service runs are outside these gates.

## Future direction {#roadmap}

The direction is an enterprise self-hosted Agent OS where tasks can be executed, checked, traced and improved. Entries marked delivered are part of this version and are described in [What the current code provides](#current-capabilities); entries marked planned are not current capabilities, and no delivery date is committed.

| Priority area | Intended result | Status |
| --- | --- | --- |
| Knowledge and memory | Versioned knowledge collections with citation-bearing retrieval, history compaction and Agent-scoped persistent memory | Delivered |
| Diagnostics and task orchestration | Operation records and run diagnostics, plus durable sequential flows with bounded delegation and evidence checks | Delivered |
| Execution isolation and controlled learning | Opt-in container isolation for controlled shell and code tools, evaluation and approval gates for learned skills, and versioned rollback | Available as opt-in; not in the release support matrix |
| Kubernetes and database HA | Run the same multi-Worker model on Kubernetes and add database high availability | Planned |
| Enterprise governance and integrations | Add enterprise identity, roles, tenant/project boundaries, audit queries, quotas, approvals and richer channel access | Planned |
| Goal-to-deliverable workflows | Organize specialist Agents around a business goal with traceable artifacts and bounded review | Longer-term direction |

Planned areas each need their own design review and reproducible validation, and are not enabled by changing a configuration switch in the current version.

## Project

[GitHub](https://github.com/ymedword/basalt) · [Issues](https://github.com/ymedword/basalt/issues)

This version has no site search. Use the section outline and the guide links above.
