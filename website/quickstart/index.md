---
title: Quickstart — Run your first agent
description: Build or bring a binary, configure and enable an agent in the console, stop the server, and run your first CLI conversation.
---

<!-- markdownlint-configure-file {"MD025":{"front_matter_title":""},"MD033":{"allowed_elements":["a","script"]}} -->

<script setup>
import { withBase } from 'vitepress'
</script>

# Quickstart

Create an independent agent, configure and enable it in the local console, then complete a CLI conversation. Keep the same working directory and terminal environment throughout.

## Prerequisites and build

- Prebuilt binary: no additional language tooling.
- Source build: Go, using the toolchain in go.mod.
- A compatible model endpoint, model name and your own API key.
- Embedded SQLite; no separately deployed database or cache.
- Node is only needed to rebuild the console or site, or capture this scripted demo. The tutorial agent needs no external interpreter or MCP server.

Put an existing binary on PATH, or build from the current checkout. The Go build version follows the toolchain in go.mod and CI.

### Build on POSIX

```sh
git clone https://github.com/ymedword/basalt.git
cd basalt
CGO_ENABLED=0 go build -trimpath -o bin/basalt ./cmd/basalt
export PATH="$PWD/bin:$PATH"
```

### Build on Windows PowerShell

```powershell
git clone https://github.com/ymedword/basalt.git
cd basalt
$env:CGO_ENABLED = "0"
go build -trimpath -o bin\basalt.exe ./cmd/basalt
$env:PATH = "$PWD\bin;$env:PATH"
```

## Configure the model and management access {#config}

Create basalt.yaml in the repository root, outside .basalt/. This minimal example declares one model connection. Use your own endpoint, model name and environment variable.

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

In the same terminal, set your model key and generate a management token with a system cryptographic random source. Replace the example key placeholder; keep only environment references in the configuration file.

### POSIX environment

```sh
export BASALT_MODEL_KEY="your-own-model-key"
export BASALT_ADMIN_TOKEN="$(openssl rand -hex 32)"
```

### PowerShell environment

```powershell
$env:BASALT_MODEL_KEY = "your-own-model-key"
$bytes = New-Object byte[] 32
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
$env:BASALT_ADMIN_TOKEN = [Convert]::ToBase64String($bytes)
```

Environment-based credentials do not need a managed-credential master key. The full configuration example contains additional connections and environment variables; it is not a ready-to-run minimal configuration.

## Initialize and create an agent

```sh
basalt init
basalt profile create demo
basalt profile list
```

init creates .basalt/ and its embedded .basalt/basalt.db idempotently. Created-file paths are printed relative to .basalt/. The CLI output has this shape; the model is still a placeholder:

```text
已选择 provider：primary（配置首项）
  创建：agents/demo/AGENT.md
  demo：registered=true（provider primary / 模型 请在此填写模型名）
汇总：1 个 Agent 目录（已注册 1）
```

**registered=true does not mean configured or runnable.** It only means the definition and provider name loaded. A new agent is disabled; the model placeholder “请在此填写模型名” still needs a real model configured in the console.

## Configure and enable in the local console {#console}

```sh
basalt serve
```

Open [local console](http://127.0.0.1:8080/console/) in your browser and enter the management token you generated.

1. Open Agents → demo → 配置 (Configure).
2. In the 模型 (Model) section, select primary under 模型连接 (Model connection). In the 模型 (Model) text field, enter deepseek-chat or your actual model ID.
3. Click 保存配置 (Save configuration) so the console writes the connection binding.
4. Click 返回 Agent 工作区 (Return to Agent workspace), then 启用 Agent (Enable agent), confirm the action and check the enabled state.

The console currently uses Chinese labels; the English annotations above identify the controls. Configuration and enablement use the existing management service and auditing.

## Stop the server and start your first chat {#first-chat}

Press Ctrl+C in the terminal running serve and wait for it to exit and release the instance lock. Independent CLI chat and serve cannot own the same workspace concurrently.

```sh
basalt chat --agent demo --message "Hello"
```

The reply depends on your configured model. The homepage animation uses local scripted replies and is not evidence of a provider model’s quality.

## Verify the session and run records {#verify}

```sh
basalt session list
```

```text
cli-<id>：N 条（最后活动 <UTC 时间>）
共 1 个会话
```

The example preserves the CLI format; identifiers, counts and timestamps depend on the run. An empty workspace prints “未发现会话”. Messages, model calls and run records are stored in .basalt/basalt.db. A tool record exists only if a tool was actually invoked. Restart the service to inspect the agent’s executions and sessions in the console.

## CLI commands {#commands}

The tutorial commands initialize, create, inspect the definition, start management, chat, and list sessions. The binary’s built-in help describes the complete command surface.

## Troubleshooting {#troubleshoot}

- Agent unavailable: enable the configured agent in the console.
- Workspace lock busy: stop the server before starting independent CLI chat.
- Model placeholder: save an actual model name and model connection in the console.
- Configuration rejected: set each referenced environment variable in the same terminal.

## Deployment and operations {#deploy}

The foreground server above listens only on loopback. It is not built-in TLS or a production process supervisor. Configure remote access, reverse proxies, authentication, backups, upgrades and process supervision using the public operations manual.

[Operations (GitHub)](https://github.com/ymedword/basalt/blob/main/docs/Operations.md)

<a :href="withBase('/architecture/')">Explore the architecture and execution →</a>
