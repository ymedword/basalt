# Basalt

<!-- markdownlint-configure-file {"MD033":{"allowed_elements":["a","details","summary"]}} -->

[简体中文](README.zh-CN.md)

**Independent agents. Shared infrastructure.**

Basalt is a self-hosted Agent OS for enterprise teams. Define independent business agents through directories and declarative configuration, with their own models, tools, sessions and scoped memory. Share execution, connections, scheduling, storage and auditing across the platform.

[Quick start](#quick-start) · [Documentation](website/docs/index.md) · [Architecture](#architecture) · [Issues](https://github.com/ymedword/basalt/issues)

## Why Basalt

- **Independent agents** — Configure different models, tools, skills and scoped business state on one platform.
- **Reusable resources** — Extend agents through model connections, built-in tools, MCP and skills.
- **Scheduled automation** — Manage recurring tasks per agent and deliver results to configured channels.
- **Inspectable execution** — Persist runs and model/tool calls, with budgets and applicable tool policies.
- **Self-hosted operation** — A Go binary with an embedded console and SQLite; no Node or separate database service at runtime.

For example, configure daily-report for scheduled briefings and file-review for permitted workspace checks. They reuse the platform while keeping separate configurations, sessions and memory.

## Architecture

![Basalt architecture: access, application, execution engine, infrastructure services and storage](website/public/diagrams/en/system-architecture.svg)

The current architecture is single-node, multi-agent hosting with logical Agent scopes. The [full system diagram](website/public/diagrams/en/project-overview.svg) expands components and external boundaries. [Architecture documentation](website/architecture/index.md) covers execution, dependencies and deployment; [product documentation](website/docs/index.md) separates current capabilities from planned direction.

## Quick start

For a source build, use Git and the Go toolchain declared in go.mod. You also need a compatible model endpoint, model name and API key. If you already have a built binary, put it on PATH and skip the build; running the binary needs neither Go nor Node.

### 1. Build

```sh
git clone https://github.com/ymedword/basalt.git
cd basalt
CGO_ENABLED=0 go build -trimpath -o bin/basalt ./cmd/basalt
export PATH="$PWD/bin:$PATH"
```

<details>

<summary>Windows PowerShell build</summary>

```powershell
git clone https://github.com/ymedword/basalt.git
cd basalt
$env:CGO_ENABLED = "0"
go build -trimpath -o bin\basalt.exe ./cmd/basalt
$env:PATH = "$PWD\bin;$env:PATH"
```

</details>

### 2. Configure the model and management access

Expand the minimal configuration below, create basalt.yaml in the current directory, and set the environment variables it references.

<details>

<summary>Minimal configuration and environment variables (POSIX / PowerShell)</summary>

```yaml
providers:
  - name: primary
    base_url: https://api.deepseek.com
    credential_mode: env
    api_key: ${BASALT_MODEL_KEY}
    default_model: deepseek-chat
http:
  addr: 127.0.0.1:8080
  admin_auth:
    mode: token
    token: ${BASALT_ADMIN_TOKEN}
```

#### POSIX environment

```sh
export BASALT_MODEL_KEY="your-own-model-key"
export BASALT_ADMIN_TOKEN="$(openssl rand -hex 32)"
```

#### PowerShell environment

```powershell
$env:BASALT_MODEL_KEY = "your-own-model-key"
$bytes = New-Object byte[] 32
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
$env:BASALT_ADMIN_TOKEN = [Convert]::ToBase64String($bytes)
```

Replace the example key with your own value and adjust the endpoint/model for your service. Keep environment-variable references in the configuration file.

</details>

### 3. Create and run your first agent

<!-- quickstart-commands:start -->

```sh
basalt init
basalt profile create demo
basalt profile list
```

A new agent is disabled. registered=true only means its definition loaded. Start management next to configure the actual model and enable the agent.

```sh
basalt serve
```

Open the [local console](http://127.0.0.1:8080/console/) and enter the management token generated above:

1. Open demo in Agents, select its configuration, choose the primary model connection and enter the actual model ID.
2. Save, return to the Agent workspace, enable the agent and confirm.
3. Press Ctrl+C to stop the server and release the workspace lock before CLI chat.

```sh
basalt chat --agent demo --message "Hello"
basalt session list
```

<!-- quickstart-commands:end -->

[The full quickstart](website/quickstart/index.md) includes console field mappings and expected output; the console currently uses Chinese labels. Replies depend on your model and configuration. The website animation is a labelled local scripted demo.

## Documentation

| Your goal | Start here |
| --- | --- |
| Understand the Agent OS, current scope and direction | [Product documentation](website/docs/index.md) |
| Build, configure and run an agent | [Quickstart](website/quickstart/index.md) |
| Explore detailed architecture and execution | [Architecture](website/architecture/index.md) |
| Deploy, authenticate, back up and upgrade | [Operations](docs/Operations.md) |
| Look up configuration and API contracts | [Configuration example](basalt.yaml.example) · [HTTP API](docs/contracts/openapi.yaml) |

The management token provides basic management authentication. Operations and architecture documentation explain remote access, model data flows and tool permissions.

## Feedback and contributions

Use [GitHub Issues](https://github.com/ymedword/basalt/issues) for bugs and proposals. Include your platform, reproduction steps and relevant error messages.
