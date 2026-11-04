import { describe, expect, it } from 'vitest'
import { procurementTemplate } from '../src/workflow-template'
import type { FlowAgentCandidate } from '../src/api-flow'
import { parseWorkflowJSON } from '../src/workflow-editor'

const names = ['bid-requirements', 'bid-technical', 'bid-delivery', 'bid-reviewer']
const catalog = (): FlowAgentCandidate[] => names.map(name => ({name, display_name: name, description: 'user current role', eligible: true, reason: 'ready', knowledge_collections: [], memory_scope: 'agent_only', readonly_tools: ['read_file'], agent: {name, uid: 'current-' + name, version: 7, config_hash: 'current-config', manifest_hash: 'current-manifest'}}))

describe('bundled procurement template', () => {
  it('keeps the independent 1024-property and 1024-array-element bounds', () => {
    expect(() => parseWorkflowJSON(JSON.stringify(Array.from({length: 1025}, () => 1)))).toThrow('数组过长')
    expect(() => parseWorkflowJSON(JSON.stringify(Object.fromEntries(Array.from({length: 1025}, (_, i) => [String(i), 1]))))).toThrow('较复杂')
  })
  it('binds all 17 nodes to current identities without changing the frozen coverage', () => {
    const candidates = catalog()
    const result = procurementTemplate(candidates)
    expect(result.missing).toEqual([])
    const parsed = JSON.parse(result.text)
    expect(parsed.nodes).toHaveLength(17)
    for (const node of parsed.nodes) expect(node.agent).toEqual(candidates.find(item => item.name === node.agent.name)!.agent)
    const review = parsed.nodes.filter((node: {id: string}) => node.id.startsWith('review_'))
    const ids = review.flatMap((node: {output_schema: {properties: {report: {properties: {result: {properties: Record<string, unknown>}}}}}}) => Object.keys(node.output_schema.properties.report.properties.result.properties))
    expect(ids).toEqual(Array.from({length: 48}, (_, i) => `R${String(i + 1).padStart(2, '0')}`))
    candidates[0]!.agent!.uid = 'new-identity'
    expect(JSON.parse(procurementTemplate(candidates).text).nodes[0].agent.uid).toBe('new-identity')
  })
  it('refuses missing, disabled and ambiguous roles rather than generating partial pins', () => {
    const candidates = catalog()
    candidates[1]!.eligible = false
    candidates.pop()
    candidates.push(candidates[0]!)
    const result = procurementTemplate(candidates)
    expect(result.text).toBe('')
    expect(result.missing).toEqual(['bid-requirements', 'bid-technical', 'bid-reviewer'])
  })
})
