import { createServer as httpServer } from 'node:http'
import { pathToFileURL } from 'node:url'

// 此夹具只模拟协议，不进行推断；浏览器不会调用该进程。
// This fixture simulates the protocol, not inference; the browser never calls it.
export function replyFor(messages) {
  const lastUser = messages.findLastIndex(message => message.role === 'user')
  const result = messages.slice(lastUser + 1).findLast(message => message.role === 'tool' && message.tool_call_id === 'basalt-demo-readme')
  if (result) return {
    role: 'assistant',
    content: typeof result.content === 'string' && result.content.includes('# Basalt')
      ? 'Basalt is a self-hosted Agent OS.\nIndependent agents share execution, scheduling and auditing.'
      : 'README.md could not be read. Check the sample file and the agent\'s file access.'
  }
  return { role: 'assistant', content: null, tool_calls: [{ id: 'basalt-demo-readme', type: 'function', function: { name: 'read_file', arguments: JSON.stringify({ path: 'README.md' }) } }] }
}

export function createServer({ port = 18769 } = {}) {
  if (!Number.isInteger(Number(port)) || Number(port) < 0 || Number(port) > 65535) throw new Error('Invalid port')
  const server = httpServer(async (request, response) => {
    const json = (status, value) => { response.writeHead(status, { 'Content-Type': 'application/json' }); response.end(JSON.stringify(value)) }
    if (request.method !== 'POST' || !/^\/(?:v1\/)?chat\/completions$/.test(request.url ?? '')) { json(404, { error: { message: 'Not found' } }); return }
    if (!/^Bearer\s+\S+$/i.test(request.headers.authorization ?? '')) { json(401, { error: { message: 'A nonempty placeholder API key is required' } }); return }
    try {
      const chunks = []
      let size = 0
      for await (const chunk of request) {
        size += chunk.length
        if (size > 1024 * 1024) { json(413, { error: { message: 'Request too large' } }); return }
        chunks.push(chunk)
      }
      const body = JSON.parse(Buffer.concat(chunks).toString('utf8'))
      if (!Array.isArray(body.messages) || !body.messages.every(item => item && typeof item.role === 'string')) { json(400, { error: { message: 'messages must be an array of messages' } }); return }
      if (body.stream) { json(400, { error: { message: 'This fixture uses non-streaming responses' } }); return }
      const message = replyFor(body.messages)
      json(200, { id: 'basalt-local-demo', object: 'chat.completion', created: Math.floor(Date.now() / 1000), model: body.model ?? 'demo-model', choices: [{ index: 0, message, finish_reason: message.tool_calls ? 'tool_calls' : 'stop' }], usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 } })
    } catch { json(400, { error: { message: 'Invalid request' } }) }
  })
  return new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(Number(port), '127.0.0.1', () => resolve(server))
  })
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const server = await createServer({ port: Number(process.env.PORT ?? 18769) })
  process.stdout.write(`Local scripted model: http://127.0.0.1:${server.address().port}/v1\n`)
  const stop = () => { server.close(() => process.exit(0)); server.closeAllConnections() }
  process.on('SIGINT', stop)
  process.on('SIGTERM', stop)
}
