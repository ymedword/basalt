---
title: 快速开始 — 跑通第一个 Agent
description: 从构建和最小配置开始，通过管理台配置并启用 Agent，停止服务后完成 CLI 对话并查看会话。
---

<!-- markdownlint-configure-file {"MD025":{"front_matter_title":""},"MD033":{"allowed_elements":["a","script"]}} -->

<script setup>
import { withBase } from 'vitepress'
</script>

# 快速开始

你将创建一个独立 Agent，在本机管理台配置模型并启用，再从 CLI 完成一次对话。以下步骤都在同一个工作目录与终端环境中进行。

## 前置条件与构建

- 预构建二进制：不需要额外语言工具链。
- 源码构建：需要 Go，版本以 go.mod 的 toolchain 为准。
- 兼容的模型端点、模型名与你自己的 API key。
- SQLite 内嵌，无需单独部署数据库或缓存。
- Node 只用于管理台重建、官网构建和脚本化演示采集。教程 Agent 不需要外部解释器或 MCP 服务。

已有二进制可直接将它加入 PATH；否则从当前源码构建。Go 构建版本以 go.mod 的 toolchain 和 CI 为准。

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

## 配置模型与管理认证 {#config}

在仓库根目录新建 basalt.yaml（不是放到 .basalt/ 内）。下例只配置一个模型连接，请把端点、模型名与环境变量改为自己的设置。

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

在同一个终端设置模型密钥，并用系统密码学随机源生成管理 token。将示例占位值换成自己的模型密钥，配置文件只保留环境变量引用。

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

纯环境变量凭据不需要托管凭据主密钥。不要直接照搬完整配置样例：它包含多个模型连接和其他环境变量。

## 初始化并创建 Agent

```sh
basalt init
basalt profile create demo
basalt profile list
```

init 幂等创建 .basalt/ 与其中的 .basalt/basalt.db。创建后的文件路径相对 .basalt/ 打印。实际 CLI 输出的形态如下，模型名尚未填写：

```text
已选择 provider：primary（配置首项）
  创建：agents/demo/AGENT.md
  demo：registered=true（provider primary / 模型 请在此填写模型名）
汇总：1 个 Agent 目录（已注册 1）
```

**registered=true 不等于已配置或可运行。** 它只表示定义与 provider 名已装载。新 Agent 默认 disabled；模型名“请在此填写模型名”必须在管理台配置为真实模型。

## 在本机管理台配置并启用 {#console}

```sh
basalt serve
```

浏览器打开[本地管理台](http://127.0.0.1:8080/console/)，输入刚才生成的管理 token。

1. 打开 Agents → demo → 配置。
2. 在“模型”分区的“模型连接”选择 primary，在“模型”文本框填写 deepseek-chat 或实际模型 ID。
3. 点击“保存配置”，让管理台写入连接绑定。
4. 点击“返回 Agent 工作区”，再点击“启用 Agent”并确认，检查启用状态。

管理台当前为中文。配置与启用走同一套管理服务和审计。

## 停止服务并进行第一次对话 {#first-chat}

先在运行 serve 的终端按 Ctrl+C，等待服务退出并释放实例锁，再运行下面的命令。独立 CLI chat 不能与 serve 同时占用同一工作区。

```sh
basalt chat --agent demo --message "Hello"
```

回复取决于你配置的模型。首页动画使用本地脚本化回复，不是厂商模型效果证明。

## 确认会话与运行记录 {#verify}

```sh
basalt session list
```

```text
cli-<id>：N 条（最后活动 <UTC 时间>）
共 1 个会话
```

上面保留实际 CLI 的输出格式，标识、条数和时间按本次运行变化；空工作区输出“未发现会话”。消息、模型调用与运行记录保存在 .basalt/basalt.db。只有真实触发工具调用时才有对应工具记录。可重新启动服务，在管理台查看该 Agent 的执行与会话。

## CLI 命令 {#commands}

本教程的命令依次负责初始化、创建、检查定义、启动管理服务、对话和列会话。各命令的完整参数通过二进制内置帮助查看。

## 排障 {#troubleshoot}

- Agent 不可用：在管理台完成配置并显式启用。
- 工作区实例锁占用：先停止服务，再启动独立 CLI 对话。
- 模型名仍为占位符：在管理台保存真实模型名与模型连接。
- 配置被拒绝：在同一终端设置配置引用的每个环境变量。

## 部署与运维 {#deploy}

上面的前台服务只监听本机回环，不是内置 TLS 或生产进程守护。服务器远程访问、反向代理、认证、备份、升级和进程托管请按公开运维手册配置。

[运维手册（GitHub）](https://github.com/ymedword/basalt/blob/main/docs/Operations.md)

<a :href="withBase('/zh/architecture/')">了解架构与运行机制 →</a>
