import rawTemplate from '../../../examples/workflows/bid-readiness/defaults/materials/workflow.json?raw'
import type { FlowAgentCandidate, FlowAgentPin } from './api-flow'
import { parseWorkflowJSON } from './workflow-editor'

// 发布时仍由服务端复核，模板只绑定本次目录读到的已就绪身份。
export function procurementTemplate(catalog: FlowAgentCandidate[]): { text: string; missing: string[] } {
  const bundle = parseWorkflowJSON(rawTemplate) as { nodes: { agent: { name: string } | FlowAgentPin }[] }
  const missing: string[] = []
  for (const node of bundle.nodes) {
    const name = node.agent.name
    const candidates = catalog.filter(item => item.name === name)
    const current = candidates.length === 1 ? candidates[0] : undefined
    if (!current?.eligible || !current.agent || current.agent.name !== name) { if (!missing.includes(name)) missing.push(name); continue }
    node.agent = { ...current.agent }
  }
  return { text: missing.length ? '' : JSON.stringify(bundle, null, 2), missing }
}
