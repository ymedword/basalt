export const QUICKSTART_COMMANDS = [
  'basalt init',
  'basalt profile create demo',
  'basalt profile list',
  'basalt serve',
  'basalt chat --agent demo --message "Hello"',
  'basalt session list'
]
export const TOKEN_GENERATION = {
  posix: 'export BASALT_ADMIN_TOKEN="$(openssl rand -hex 32)"',
  windows: '$bytes = New-Object byte[] 32\n$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()\ntry { $rng.GetBytes($bytes) } finally { $rng.Dispose() }\n$env:BASALT_ADMIN_TOKEN = [Convert]::ToBase64String($bytes)'
}
export const MINIMAL_CONFIG = `providers:
  - name: primary
    base_url: https://api.deepseek.com
    credential_mode: env
    api_key: \${BASALT_MODEL_KEY}
    default_model: deepseek-chat
http:
  addr: 127.0.0.1:8080
  admin_auth:
    mode: token
    token: \${BASALT_ADMIN_TOKEN}`
export const PREREQUISITES = [
  { id: 'runtime', en: 'Prebuilt binary: no additional language tooling.', zh: '预构建二进制：不需要额外语言工具链。' },
  { id: 'source', en: 'Source build: Go, using the toolchain in go.mod.', zh: '源码构建：需要 Go，版本以 go.mod 的 toolchain 为准。' },
  { id: 'model', en: 'A compatible model endpoint, model name and your own API key.', zh: '兼容的模型端点、模型名与你自己的 API key。' },
  { id: 'data', en: 'Embedded SQLite; no separately deployed database or cache.', zh: 'SQLite 内嵌，无需单独部署数据库或缓存。' },
  { id: 'maintenance', en: 'Node is only needed to rebuild the console or site, or capture this scripted demo. The tutorial agent needs no external interpreter or MCP server.', zh: 'Node 只用于管理台重建、官网构建和脚本化演示采集。教程 Agent 不需要外部解释器或 MCP 服务。' }
]
export const STEPS = [
  { id: 'config' }, { id: 'init', command: QUICKSTART_COMMANDS[0] },
  { id: 'create', command: QUICKSTART_COMMANDS[1] }, { id: 'inspect', command: QUICKSTART_COMMANDS[2] },
  { id: 'console', command: QUICKSTART_COMMANDS[3] }, { id: 'stop' },
  { id: 'first-chat', command: QUICKSTART_COMMANDS[4] }, { id: 'verify', command: QUICKSTART_COMMANDS[5] },
  { id: 'commands' }, { id: 'troubleshoot' }, { id: 'deploy' }
]
export const TROUBLESHOOTING = [
  { id: 'disabled', en: 'Agent unavailable: enable the configured agent in the console.', zh: 'Agent 不可用：在管理台完成配置并显式启用。' },
  { id: 'lock', en: 'Workspace lock busy: stop the server before starting independent CLI chat.', zh: '工作区实例锁占用：先停止服务，再启动独立 CLI 对话。' },
  { id: 'placeholder', en: 'Model placeholder: save an actual model name and model connection in the console.', zh: '模型名仍为占位符：在管理台保存真实模型名与模型连接。' },
  { id: 'credentials', en: 'Configuration rejected: set each referenced environment variable in the same terminal.', zh: '配置被拒绝：在同一终端设置配置引用的每个环境变量。' }
]
export const DURATION_MEASURED = false

export function extractBasaltCommands(markdown) {
  return [...markdown.matchAll(/^\s*basalt\s+[^\r\n]+/gm)].map(match => match[0].trim())
}
export function assertQuickstartParity(readme, zhReadme) {
  for (const [name, text] of [['README.md', readme], ['README.zh-CN.md', zhReadme]]) {
    const block = text.match(/<!-- quickstart-commands:start -->([\s\S]*?)<!-- quickstart-commands:end -->/)
    if (!block) throw new Error(`${name}: missing quickstart-commands block`)
    if (JSON.stringify(extractBasaltCommands(block[1])) !== JSON.stringify(QUICKSTART_COMMANDS)) throw new Error(`${name}: quickstart command sequence drifted`)
  }
  return true
}
