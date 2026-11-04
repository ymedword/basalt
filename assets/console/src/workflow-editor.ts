// 可视化编辑只改作者定义，执行、权限和验收继续由同一服务端负责。
// The visual editor authors existing definitions; the server remains execution authority.
import type { FlowAgentPin } from './api-flow'
import { randomFlowID } from './flow-identity'

export type WorkflowSchema = Record<string, unknown>
export interface WorkflowField { name: string; type: 'string' | 'integer' | 'boolean'; required: boolean; description: string }
export interface WorkflowMapping { target: string; source?: string; literal?: unknown }
export interface WorkflowBudget { max_total_tokens: number; max_tool_calls: number; max_duration_seconds: number; max_depth: number; max_children: number; max_attempts_per_node: number; max_revisions: number }
export interface WorkflowNode extends Record<string, unknown> {
  id: string; kind: 'agent'; agent: FlowAgentPin; inputs: WorkflowMapping[]
  output_schema: WorkflowSchema; next?: string; failure: { max_attempts: number; rework_target?: string; max_reworks?: number }
}
export interface WorkflowDocument extends Record<string, unknown> {
  format_version: 1; name: string; entry: string; nodes: WorkflowNode[]; limits: WorkflowBudget; task_contract: 'TASK.json'
  contract: Record<string, unknown> & { contract_version: number; goal: string; constraints: string[]; input_schema: WorkflowSchema; output_schema: WorkflowSchema; budget: WorkflowBudget }
}

export const workflowID = /^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$/
const object = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v)
const defaultSchema = (): WorkflowSchema => ({ type: 'object', properties: { result: { type: 'string' } }, required: ['result'], additionalProperties: false })

// Duplicate keys and numeric precision must be checked before JSON.parse can erase
// them. Unsupported precision stays in advanced raw mode; it is never rounded.
export function parseWorkflowJSON(raw: string): unknown {
  if (new TextEncoder().encode(raw).length > 1_048_576) throw new Error('定义超过 1 MiB，请缩小内容。')
  let at = 0
  let entries = 0
  const skip = () => { while (at < raw.length && /[ \t\r\n]/.test(raw[at]!)) at++ }
  const string = (): string => {
    const start = at++
    while (at < raw.length) {
      const char = raw[at++]
      if (char === '\\') { at++; continue }
      if (char === '"') return JSON.parse(raw.slice(start, at)) as string
    }
    throw new Error('定义的字符串不完整。')
  }
  const value = (depth: number): unknown => {
    if (depth > 16) throw new Error('定义层级过深，请使用高级原文并由服务端核对。')
    skip()
    const char = raw[at]
    if (char === '"') return string()
    if (char === '{' || char === '[') {
      at++; skip()
      const end = char === '{' ? '}' : ']'
      const result: Record<string, unknown> | unknown[] = char === '{' ? Object.create(null) as Record<string, unknown> : []
      const keys = new Set<string>()
      while (raw[at] !== end) {
        // 与契约的“总属性 1024 / 单数组 1024”分开计数，数组元素不是对象属性。
        if (Array.isArray(result)) {
          if (result.length >= 1024) throw new Error('定义数组过长，请使用高级原文。')
          result.push(value(depth + 1))
        }
        else {
          if (++entries > 1024) throw new Error('定义内容较复杂，请使用高级原文。')
          if (raw[at] !== '"') throw new Error('定义字段名格式无效。')
          const key = string()
          if (keys.has(key)) throw new Error('定义有重复字段，不能转为可视化；请先修正高级原文。')
          keys.add(key); skip()
          if (raw[at++] !== ':') throw new Error('定义字段格式无效。')
          result[key] = value(depth + 1)
        }
        skip()
        if (raw[at] === end) break
        if (raw[at++] !== ',') throw new Error('定义分隔符无效。')
        skip()
        if (raw[at] === end) throw new Error('定义不能包含尾随逗号。')
      }
      at++
      return result
    }
    for (const [word, parsed] of [['true', true], ['false', false], ['null', null]] as const) {
      if (raw.slice(at, at + word.length) === word) { at += word.length; return parsed }
    }
    const number = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/.exec(raw.slice(at))?.[0]
    if (!number) throw new Error('定义必须是合法 JSON。')
    if (!/^-?(?:0|[1-9]\d*)$/.test(number) || number === '-0' || !Number.isSafeInteger(Number(number))) throw new Error('定义含精确大数、小数或指数；保留高级原文，避免数值被改写。')
    at += number.length
    return Number(number)
  }
  const result = value(0)
  skip()
  if (at !== raw.length) throw new Error('定义后存在多余内容。')
  return result
}

export function newWorkflow(): WorkflowDocument {
  const limits: WorkflowBudget = { max_total_tokens: 50000, max_tool_calls: 30, max_duration_seconds: 1800, max_depth: 0, max_children: 0, max_attempts_per_node: 1, max_revisions: 0 }
  const schema = defaultSchema()
  return {
    format_version: 1, name: '', entry: '', nodes: [], limits, task_contract: 'TASK.json',
    contract: { contract_version: 1, goal: '', constraints: ['不得编造缺少的来源、资质或业务结论。'], input_schema: { type: 'object', properties: { request: { type: 'string', description: '本次任务的具体要求' } }, required: ['request'], additionalProperties: false }, output_schema: schema, budget: { ...limits }, artifact_spec: [{ id: 'report', format: 'json', required: true }], acceptance: [{ id: 'report-shape', version: 1, kind: 'deterministic', target_kind: 'artifact', target_id: 'report', required: true, schema }], stop_conditions: ['budget', 'deadline', 'unknown'], escalation: '保留已完成阶段和缺口，交由人工核对。' },
  }
}

export function loadWorkflow(raw: string): { document?: WorkflowDocument; reason?: string } {
  try {
    const value = parseWorkflowJSON(raw)
    if (!object(value) || value.format_version !== 1 || value.task_contract !== 'TASK.json' || typeof value.name !== 'string' || !Array.isArray(value.nodes) || !object(value.limits) || !object(value.contract) || typeof value.contract.goal !== 'string' || !Array.isArray(value.contract.constraints) || !value.contract.constraints.every(v => typeof v === 'string') || !object(value.contract.input_schema) || !object(value.contract.output_schema) || !object(value.contract.budget)) throw new Error('此定义需要高级模式；原始内容仍完整保留。')
    const nodes = value.nodes
    if (nodes.length > 64) throw new Error('定义超过 64 个节点。')
    const ids = new Set<string>()
    for (const [i, node] of nodes.entries()) {
      if (!object(node) || node.kind !== 'agent' || typeof node.id !== 'string' || ids.has(node.id) || !object(node.agent) || !object(node.output_schema) || !object(node.failure) || (node.inputs !== undefined && !Array.isArray(node.inputs)) || node.failure.rework_target || node.branch || node.subflow || node.tool) throw new Error('此路线包含高级节点或返工规则，请使用高级原文；可视化不会替换它。')
      ids.add(node.id)
      if ((node.inputs as unknown[] | undefined)?.some(mapping => !object(mapping) || typeof mapping.target !== 'string' || (mapping.source !== undefined && typeof mapping.source !== 'string') || Object.keys(mapping).some(key => !['target', 'source', 'literal'].includes(key)))) throw new Error('输入映射格式无效，请在高级原文中修正；内容未改写。')
      if (node.next && node.next !== (nodes[i + 1] as Record<string, unknown> | undefined)?.id) throw new Error('此路线不是按节点列表顺序连接，请使用高级原文。')
      if (i < nodes.length - 1 && !node.next) throw new Error('此路线存在未连接节点，请在高级原文核对。')
    }
    if (nodes.length && value.entry !== (nodes[0] as Record<string, unknown>).id) throw new Error('入口与路线顺序不同，请使用高级原文。')
    const document = value as unknown as WorkflowDocument
    for (const node of document.nodes) node.inputs ??= []
    return { document }
  } catch (error) { return { reason: error instanceof Error ? error.message : '无法读取此定义；原文已保留。' } }
}

export function workflowFields(schema: WorkflowSchema): WorkflowField[] | null {
  if (schema.type !== 'object' || !object(schema.properties) || Object.keys(schema).some(k => !['type', 'properties', 'required', 'additionalProperties'].includes(k)) || schema.additionalProperties !== false || (schema.required !== undefined && !Array.isArray(schema.required))) return null
  if (Array.isArray(schema.required) && (new Set(schema.required).size !== schema.required.length || schema.required.some(key => typeof key !== 'string' || !Object.hasOwn(schema.properties as object, key)))) return null
  const result: WorkflowField[] = []
  for (const [name, value] of Object.entries(schema.properties)) {
    if (!workflowID.test(name) || !object(value) || !['string', 'integer', 'boolean'].includes(String(value.type)) || Object.keys(value).some(k => !['type', 'description'].includes(k)) || (value.description !== undefined && typeof value.description !== 'string')) return null
    result.push({ name, type: value.type as WorkflowField['type'], required: (schema.required as unknown[] | undefined)?.includes(name) ?? false, description: typeof value.description === 'string' ? value.description : '' })
  }
  return result
}

export function workflowSchema(fields: WorkflowField[]): WorkflowSchema {
  const properties: Record<string, unknown> = Object.create(null) as Record<string, unknown>
  for (const field of fields) {
    if (!workflowID.test(field.name) || Object.hasOwn(properties, field.name)) throw new Error('字段名须为 1–64 位字母、数字、下划线或横线，并且不能重复。')
    properties[field.name] = { type: field.type, ...(field.description ? { description: field.description } : {}) }
  }
  return { type: 'object', properties, required: fields.filter(field => field.required).map(field => field.name), additionalProperties: false }
}

// Only the starter's single structural rule is editable with this shortcut.
// Explicit user action is required; imported evidence/semantic rules stay intact.
export function canAdoptWorkflowOutput(document: WorkflowDocument): boolean {
  const rules = document.contract.acceptance
  const artifacts = document.contract.artifact_spec
  if (!Array.isArray(rules) || rules.length !== 1 || !Array.isArray(artifacts) || artifacts.length !== 1) return false
  const rule: unknown = rules[0]; const artifact: unknown = artifacts[0]
  return object(rule) && rule.id === 'report-shape' && rule.kind === 'deterministic' && rule.target_kind === 'artifact' && rule.target_id === 'report' && rule.required === true && Object.keys(rule).every(key => ['id', 'version', 'kind', 'target_kind', 'target_id', 'required', 'schema'].includes(key)) && object(artifact) && artifact.id === 'report' && artifact.format === 'json'
}

export function adoptWorkflowOutput(document: WorkflowDocument): WorkflowDocument {
  if (!canAdoptWorkflowOutput(document) || !document.nodes.length) throw new Error('当前验收使用高级规则，请在高级原文中核对；规则未改写。')
  const copy = structuredClone(document)
  const schema = structuredClone(copy.nodes.at(-1)!.output_schema)
  copy.contract.output_schema = schema
  ;(copy.contract.acceptance as Record<string, unknown>[])[0]!.schema = structuredClone(schema)
  return copy
}

function connect(document: WorkflowDocument): void {
  document.entry = document.nodes[0]?.id ?? ''
  for (const [i, node] of document.nodes.entries()) {
    delete node.next
    if (document.nodes[i + 1]) node.next = document.nodes[i + 1]!.id
  }
}

export function addWorkflowNode(document: WorkflowDocument, agent: FlowAgentPin): WorkflowDocument {
  if (document.nodes.length >= 64) throw new Error('最多 64 个节点。')
  const copy = structuredClone(document)
  const referenced = new Set(copy.nodes.flatMap(node => [node.id, ...node.inputs.map(mapping => mapping.source?.split('.')[1] ?? '')]))
  let id = 'step-' + randomFlowID()
  while (referenced.has(id)) id = 'step-' + randomFlowID()
  copy.nodes.push({ id, kind: 'agent', agent: { ...agent }, inputs: [], output_schema: defaultSchema(), failure: { max_attempts: 1 } })
  connect(copy)
  return copy
}

export function moveWorkflowNode(document: WorkflowDocument, id: string, destination: number): WorkflowDocument {
  const copy = structuredClone(document)
  const current = copy.nodes.findIndex(node => node.id === id)
  if (current < 0 || destination < 0 || destination >= copy.nodes.length) return copy
  const [node] = copy.nodes.splice(current, 1)
  copy.nodes.splice(destination, 0, node!)
  connect(copy)
  return copy
}

export function removeWorkflowNode(document: WorkflowDocument, id: string): WorkflowDocument {
  const copy = structuredClone(document)
  copy.nodes = copy.nodes.filter(node => node.id !== id)
  connect(copy)
  return copy
}

export function workflowSources(document: WorkflowDocument, nodeID: string): { value: string; label: string }[] {
  const result: { value: string; label: string }[] = []
  const properties = (prefix: string, label: string, schema: WorkflowSchema, depth = 0): void => {
    if (depth > 12 || !object(schema.properties)) return
    for (const [key, value] of Object.entries(schema.properties)) {
      if (!workflowID.test(key) || !object(value)) continue
      const path = prefix + '.' + key
      result.push({ value: path, label: label + ' · ' + key })
      properties(path, label + ' · ' + key, value, depth + 1)
    }
  }
  properties('input', '任务输入', document.contract.input_schema)
  for (const [index, node] of document.nodes.entries()) {
    if (node.id === nodeID) break
    properties('outputs.' + node.id, `步骤 ${index + 1} · ${node.agent.name} 已验证输出`, node.output_schema)
  }
  return result
}

export function workflowIssues(document: WorkflowDocument): string[] {
  const issues: string[] = []
  if (!workflowID.test(document.name)) issues.push('填写工作流标识：1–64 位字母、数字、下划线或横线。')
  if (!document.contract.goal.trim()) issues.push('填写本工作流要完成的目标。')
  if (!document.nodes.length) issues.push('从已启用的 Agent 中添加至少一个节点。')
  for (const node of document.nodes) {
    if (!node.agent?.uid || !Number.isSafeInteger(node.agent.version) || node.agent.version < 1 || !node.agent.config_hash || !node.agent.manifest_hash) issues.push(node.id + ' 需要选择当前就绪的 Agent。')
    const targets = new Set<string>()
    const available = new Set(workflowSources(document, node.id).map(source => source.value))
    for (const mapping of node.inputs) {
      if (!workflowID.test(mapping.target) || targets.has(mapping.target)) issues.push(node.id + ' 输入字段名无效或重复。')
      targets.add(mapping.target)
      if (mapping.source && !available.has(mapping.source)) issues.push(node.id + ' 的 ' + mapping.target + ' 引用了不存在或不在上游的字段；请重新选择，系统不会自动重绑。')
      if (!mapping.source && !Object.hasOwn(mapping, 'literal') || mapping.source && Object.hasOwn(mapping, 'literal')) issues.push(node.id + ' 的 ' + mapping.target + ' 必须选择一个来源或一个常量。')
    }
  }
  for (const [name, max] of [['max_total_tokens', 200000], ['max_tool_calls', 200], ['max_duration_seconds', 1800]] as const) {
    const value = document.contract.budget[name]
    if (!Number.isSafeInteger(value) || value < 1 || value > max) issues.push('任务预算超出允许范围：' + name)
  }
  return [...new Set(issues)]
}
