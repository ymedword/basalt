import { describe, expect, it, vi } from 'vitest'
import { addWorkflowNode, adoptWorkflowOutput, loadWorkflow, moveWorkflowNode, newWorkflow, parseWorkflowJSON, removeWorkflowNode, workflowFields, workflowIssues, workflowSchema, workflowSources } from '../src/workflow-editor'

const agent = { uid: 'agent-1', name: 'researcher', version: 1, config_hash: 'config', manifest_hash: 'manifest' }
const twoNodes = () => {
  const document = addWorkflowNode(addWorkflowNode(newWorkflow(), agent), agent)
  document.entry = 'step-1'; document.nodes[0]!.id = 'step-1'; document.nodes[0]!.next = 'step-2'; document.nodes[1]!.id = 'step-2'
  document.name = 'opportunity-review'; document.contract.goal = 'Prepare a sourced draft'
  document.nodes[1]!.inputs = [{ target: 'facts', source: 'outputs.step-1.result' }]
  return document
}

describe('workflow authoring boundaries', () => {
  it('keeps node identities and broken references safe on supported HTTP intranet pages', () => {
    const secureRandom = crypto.getRandomValues.bind(crypto)
    vi.stubGlobal('crypto', { getRandomValues: secureRandom })
    try {
      const document = twoNodes()
      const next = addWorkflowNode(removeWorkflowNode(document, 'step-1'), agent)
      expect(next.nodes[1]!.id).not.toBe('step-1')
      expect(next.nodes[1]!.id).toMatch(/^step-[0-9a-f-]+$/)
      const reordered = moveWorkflowNode(next, next.nodes[1]!.id, 0)
      expect(workflowIssues(reordered).join()).toContain('不会自动重绑')
    } finally { vi.unstubAllGlobals() }
  })
  it('orders stable node identities and exposes only prior verified outputs', () => {
    const document = twoNodes()
    expect(workflowSources(document, 'step-1').map(x => x.value)).toEqual(['input.request'])
    expect(workflowSources(document, 'step-2').map(x => x.value)).toEqual(['input.request', 'outputs.step-1.result'])
    const moved = moveWorkflowNode(document, 'step-2', 0)
    expect(moved.entry).toBe('step-2')
    expect(moved.nodes.map(x => x.id)).toEqual(['step-2', 'step-1'])
    expect(moved.nodes[0]!.next).toBe('step-1')
    expect(moved.nodes[0]!.inputs[0]!.source).toBe('outputs.step-1.result')
    expect(workflowIssues(moved).join()).toContain('不在上游')
    expect(workflowIssues(document)).toEqual([])
  })
  it('keeps broken references visible when a node is removed, without rebinding', () => {
    const changed = removeWorkflowNode(twoNodes(), 'step-1')
    expect(changed.nodes[0]!.inputs[0]!.source).toBe('outputs.step-1.result')
    expect(workflowIssues(changed).join()).toContain('不会自动重绑')
  })
  it('never reconnects a removed producer when a different Agent is inserted and moved before its consumer', () => {
    const document = twoNodes()
    const removed = removeWorkflowNode(document, 'step-1')
    const replacement = addWorkflowNode(removed, { ...agent, uid: 'different-agent', name: 'reviewer' })
    expect(replacement.nodes[1]!.id).not.toBe('step-1')
    const moved = moveWorkflowNode(replacement, replacement.nodes[1]!.id, 0)
    expect(moved.nodes[1]!.inputs[0]!.source).toBe('outputs.step-1.result')
    expect(workflowIssues(moved).join()).toContain('不会自动重绑')
  })
  it.each(['{"x":9007199254740993}', '{"x":0.1234567890123456789}', '{"x":1e3}', '{"x":-0}', '{"x":1,"\\u0078":2}', '{"x":1,}', '\u00a0{}'])('does not normalize unsafe raw JSON %s', raw => {
    expect(() => parseWorkflowJSON(raw)).toThrow()
  })
  it('keeps hostile keys as own data without prototype mutation', () => {
    const value = parseWorkflowJSON('{"__proto__":{"safe":true},"constructor":1}') as Record<string, unknown>
    expect(Object.getPrototypeOf(value)).toBeNull()
    expect(Object.hasOwn(value, '__proto__')).toBe(true)
    expect(Object.getPrototypeOf({})).not.toHaveProperty('safe')
  })
  it.each([null, { target: 1 }, { target: 'x', source: 4 }, { target: 'x', source: 'input.request', extra: true }])('rejects malformed mappings from visual conversion', mapping => {
    const document = twoNodes()
    document.nodes[0]!.inputs = [mapping] as never
    expect(loadWorkflow(JSON.stringify(document)).document).toBeUndefined()
  })
  it('preserves advanced acceptance rules while declining branches and rework', () => {
    const document = twoNodes()
    document.contract.acceptance = [{ id: 'evidence-check', kind: 'evidence', min_sources: 2, custom: 'retain' }]
    expect(loadWorkflow(JSON.stringify(document)).document?.contract.acceptance).toEqual(document.contract.acceptance)
    document.nodes[0]!.failure.rework_target = 'step-1'
    expect(loadWorkflow(JSON.stringify(document)).reason).toContain('高级')
  })
  it('changes final structural acceptance only via the explicit shortcut, never replacing advanced rules', () => {
    const document = twoNodes()
    const original = structuredClone(document.contract.acceptance)
    document.nodes[1]!.output_schema = workflowSchema([{ name: 'summary', type: 'string', required: true, description: '' }])
    expect(document.contract.acceptance).toEqual(original)
    const adopted = adoptWorkflowOutput(document)
    expect(adopted.contract.output_schema).toEqual(document.nodes[1]!.output_schema)
    expect((adopted.contract.acceptance as {schema: unknown}[])[0]!.schema).toEqual(document.nodes[1]!.output_schema)
    document.contract.acceptance = [...original as unknown[], { id: 'evidence-check', kind: 'evidence' }]
    expect(() => adoptWorkflowOutput(document)).toThrow('高级')
    expect((document.contract.acceptance as unknown[])).toHaveLength(2)
  })
  it('round trips supported fields and refuses to flatten nested or constrained schemas', () => {
    const fields = [{ name: 'request', type: 'string' as const, required: true, description: 'Question' }]
    expect(workflowFields(workflowSchema(fields))).toEqual(fields)
    expect(workflowFields({ type: 'object', properties: { x: { type: 'string', minLength: 2 } }, additionalProperties: false })).toBeNull()
    expect(workflowFields({ type: 'object', properties: {}, required: ['missing'], additionalProperties: false })).toBeNull()
  })
  it('bounds the parser before rendering untrusted imports', () => {
    expect(() => parseWorkflowJSON('['.repeat(20) + '0' + ']'.repeat(20))).toThrow()
    expect(() => parseWorkflowJSON(JSON.stringify(Array(1100).fill(0)))).toThrow()
    expect(() => parseWorkflowJSON(JSON.stringify('x'.repeat(1_048_576)))).toThrow()
  })
})
