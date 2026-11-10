import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { createServer, replyFor, SCHEDULED_MESSAGE } from '../demo/local-model.mjs'
import { visibleLines, shouldAutoplay, DEMO_COMMAND } from '../.vitepress/data/demo.mjs'
test('scripted reply reads the sample before summarizing, including subsequent turns', () => {
  const first = replyFor([{ role: 'user', content: 'Hello' }])
  assert.equal(first.tool_calls[0].function.name, 'read_file')
  assert.deepEqual(JSON.parse(first.tool_calls[0].function.arguments), { path: 'README.md' })
  const next = replyFor([{ role: 'user', content: 'Hello' }, { role: 'tool', tool_call_id: first.tool_calls[0].id, content: '# Basalt\nA self-hosted Agent OS.' }])
  assert.equal(next.tool_calls, undefined)
  assert.ok(next.content)
  assert.match(replyFor([{ role: 'user', content: 'Hello' }, { role: 'tool', tool_call_id: first.tool_calls[0].id, content: 'File not found' }]).content, /could not be read/)
  assert.ok(replyFor([{ role: 'tool', tool_call_id: first.tool_calls[0].id }, { role: 'user', content: 'Again' }]).tool_calls)
})
test('loopback fixture requires a key, rejects malformed requests and closes', async () => {
  const server = await createServer({ port: 0 })
  try {
    assert.equal(server.address().address, '127.0.0.1')
    const url = `http://127.0.0.1:${server.address().port}/v1/chat/completions`
    assert.equal((await fetch(url, { method: 'POST' })).status, 401)
    const headers = { Authorization: 'Bearer local-placeholder', 'Content-Type': 'application/json' }
    const response = await fetch(url, { method: 'POST', headers, body: JSON.stringify({ messages: [{ role: 'user', content: 'Hello' }] }) })
    assert.equal((await response.json()).choices[0].message.tool_calls[0].function.name, 'read_file')
    assert.equal((await fetch(url, { method: 'POST', headers, body: '{}' })).status, 400)
    assert.equal((await fetch(url.replace('/chat/completions', '/other'))).status, 404)
  } finally { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)) }
})
test('recorded scheduled trigger and reduced-motion output agree with the fixture', async () => {
  const transcript = JSON.parse(await readFile(new URL('../demo/transcript.json', import.meta.url), 'utf8'))
  assert.equal(transcript.command, DEMO_COMMAND)
  assert.equal(transcript.command, 'basalt serve')
  assert.equal(transcript.exitCode, 0)
  assert.equal(transcript.source, 'local-openai-compatible')
  assert.equal(transcript.stub, 'website/demo/local-model.mjs')
  assert.ok(!Number.isNaN(Date.parse(transcript.capturedAt)))
  const expected = replyFor([{ role: 'user', content: SCHEDULED_MESSAGE }]).content.split('\n')
  assert.deepEqual(transcript.lines.slice(-expected.length), expected)
  const trigger = transcript.lines[0].replace(/msg=调度触发/u, 'msg="scheduled trigger"')
  assert.deepEqual(visibleLines(transcript, true), [`$ ${transcript.command}`, trigger, ...expected])
  assert.ok(transcript.lines.some(line => line.includes('msg=调度触发')), 'raw capture stays unchanged')
  assert.ok(!/[㐀-鿿]/u.test(visibleLines(transcript, true).join('\n')), 'terminal presentation is fully English')
  assert.equal(shouldAutoplay(true), false)
  assert.equal(shouldAutoplay(false), true)
})
