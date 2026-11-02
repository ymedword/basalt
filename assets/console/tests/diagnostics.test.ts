// US-16 诊断事件页回归（T098）：五类关联锚点、证据缺口两种标记的**不同**文案、
// 只读（没有任何导出 / 重放入口），以及"没有锚点就不发注定被 400 的请求"。
// 三条契约事实在页面上如实呈现：至少一个主锚点、missing 与 sampled_out 互斥、
// 保留期是配置值而不是保留承诺。
// Diagnostics page regressions: one mandatory anchor, the two evidence-gap flags kept
// apart, no export control, and no request when the anchor is missing.
//
// 夹具取值一律用契约枚举（`decision` 只有 allow / deny / trim / degrade）：页面
// 按契约取值查表翻译，自造的 `sent` / `denied` 会把"页面没翻译"误测成"翻译正确"。
// Fixtures stick to contract enums; invented values would hide a missing mapping.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { DiagnosticEventView, DiagnosticsResult } from '../src/api-diagnostics'
import { createConsoleApp } from '../src/app'

interface Call {
  readonly method: string
  readonly path: string
  readonly query: URLSearchParams
  readonly body: unknown
}

type Route = (call: Call) => unknown

const RUN_ID = 'run-1'

const llmEvent: DiagnosticEventView = {
  event_id: 'evt-1',
  schema_version: 1,
  kind: 'llm_call',
  occurred_at: '2026-10-29T09:30:00Z',
  correlation: {
    request_id: 'req-1',
    run_id: RUN_ID,
    task_id: '',
    operation_id: '',
    ingest_job_id: '',
  },
  actor: 'agent',
  step_id: 'step-1',
  attempt_id: 'att-1',
  parent_step_id: '',
  seq: 7,
  reason_code: '',
  decision: 'allow',
  attrs: { model: 'mock-model', prompt_tokens: 12 },
  audit_ref: { table: 'audit_calls', id: 'aud-1' },
}

const deniedEvent: DiagnosticEventView = {
  ...llmEvent,
  event_id: 'evt-2',
  kind: 'permission_decision',
  reason_code: 'permission_denied',
  decision: 'deny',
  attrs: { denied_count: 2 },
  audit_ref: undefined,
  seq: 8,
}

function result(partial: Partial<DiagnosticsResult> = {}): DiagnosticsResult {
  return { events: [llmEvent, deniedEvent], missing: false, retention_days: 14, ...partial }
}

let wrapper: ReturnType<typeof mount> | undefined
let calls: Call[] = []

function envelope(data: unknown, status = 200, message = ''): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
    json: async () => ({
      code: status >= 200 && status < 300 ? 'OK' : 'FAILED',
      message,
      data,
      timestamp: '2026-10-29T12:00:00Z',
    }),
  } as unknown as Response
}

/** 失败响应哨兵：路由返回它时 harness 造一个非 2xx 信封。 */
// A failure sentinel the harness turns into a non-2xx response.
interface Failure {
  readonly status: number
  readonly message: string
}

function failure(status: number, message: string): Failure {
  return { status, message }
}

function isFailure(value: unknown): value is Failure {
  return typeof value === 'object' && value !== null && 'status' in value && 'message' in value
}

async function signIn(route: Route = () => undefined): Promise<void> {
  calls = []
  vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost')
    const call: Call = {
      method: (init?.method ?? 'GET').toUpperCase(),
      path: url.pathname,
      query: url.searchParams,
      body: init?.body === undefined ? null : JSON.parse(String(init.body)) as unknown,
    }
    calls.push(call)
    const routed = route(call)
    if (isFailure(routed)) {
      return envelope(undefined, routed.status, routed.message)
    }
    return envelope(routed)
  }))
  window.history.replaceState(null, '', '/console/login')
  wrapper = mount(createConsoleApp(), { attachTo: document.body })
  await flushPromises()
  await wrapper.find('#console-token').setValue('test-console-token')
  await wrapper.find('.login-form').trigger('submit')
  await flushPromises()
}

async function visit(path: string): Promise<void> {
  window.history.pushState(null, '', path)
  window.dispatchEvent(new PopStateEvent('popstate'))
  await flushPromises()
}

function diagnosticCalls(): Call[] {
  return calls.filter((call) => call.path === '/api/v1/diagnostics')
}

/** 提示条原文集合：证据缺口文案按条判定，避免被页眉描述里的同名词干扰。 */
function notices(): string[] {
  return wrapper!.findAll('p.notice').map((node) => node.text())
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('diagnostics page', () => {
  it('treats session lookup through recorded runs as an available anchor', async () => {
    await signIn(call => call.path === '/api/v1/diagnostics' ? result() : undefined)
    await visit('/console/diagnostics?session_id=recorded-session')
    expect(diagnosticCalls()[0]?.query.get('session_id')).toBe('recorded-session')
    expect(wrapper!.findAll('.filters option').some(n => n.text() === '会话 ID')).toBe(true)
  })
  it('keeps retrieval visibility filtering separate from tool permission refusal', async () => {
    await signIn(call => call.path === '/api/v1/diagnostics' ? result({events: [{...deniedEvent, kind: 'retrieval', attrs: {count: 5}}, deniedEvent]}) : undefined)
    await visit('/console/diagnostics?run_id=run-1')
    const rows = wrapper!.findAll('tbody tr')
    expect(rows[0]!.text()).toContain('已按访问范围筛选')
    expect(rows[0]!.text()).toContain('混合过滤计数')
    expect(rows[0]!.text()).not.toContain('权限拒绝')
    expect(rows[1]!.text()).toContain('权限拒绝')
  })

  it('does not claim startup index switches without real anchors have production coverage', async () => {
    await signIn(); await visit('/console/diagnostics')
    expect(wrapper!.findAll('.filters option').some(n => n.text().includes('索引切换') && n.text().includes('暂无生产端'))).toBe(true)
  })
  it('seeds the anchor from the deep link and renders events with pointers', async () => {
    await signIn((call) => (call.path === '/api/v1/diagnostics' ? result() : undefined))
    await visit(`/console/diagnostics?run_id=${RUN_ID}`)

    const requests = diagnosticCalls()
    expect(requests).toHaveLength(1)
    expect(requests[0]?.method).toBe('GET')
    expect(requests[0]?.query.get('run_id')).toBe(RUN_ID)
    // 深链只带一个锚点：其余四类锚点不得被补成空串参数发出去。
    expect(requests[0]?.query.has('request_id')).toBe(false)
    expect(requests[0]?.query.has('operation_id')).toBe(false)
    expect(requests[0]?.query.has('ingest_job_id')).toBe(false)
    expect(requests[0]?.query.has('session_id')).toBe(false)
    expect(requests[0]?.query.get('limit')).toBe('200')

    const rows = wrapper!.findAll('.diagnostics-table tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0]!.text()).toContain('模型调用')
    expect(rows[0]!.text()).toContain('llm_call')
    // 契约取值经 UI 翻译；原始值留在 title 里（对日志用）。
    expect(rows[0]!.text()).toContain('决策 放行')
    expect(rows[0]!.find('[title="allow"]').exists()).toBe(true)
    expect(rows[0]!.text()).toContain('seq 7')
    expect(rows[0]!.text()).toContain('step step-1')
    expect(rows[0]!.text()).toContain('run=')
    expect(rows[0]!.text()).toContain('audit_calls')
    // 权限拒绝显示可读原因与计数，不显示任何资料标题。
    expect(rows[1]!.text()).toContain('权限判定')
    expect(rows[1]!.text()).toContain('决策 拒绝')
    expect(rows[1]!.text()).toContain('原因 权限拒绝')
    expect(rows[1]!.find('[title="permission_denied"]').exists()).toBe(true)
    expect(rows[1]!.text()).toContain('denied_count')

    // 保留期标注为配置值，不是承诺。
    expect(wrapper!.text()).toContain('保留期 14 天（配置值，不是保留承诺）')
    // 只读：页面上没有导出按钮或导出链接（导出归 US-16A 拟 OBS）。
    const exportControls = wrapper!
      .findAll('button, a')
      .filter((node) => node.text().includes('导出') || node.attributes('href')?.includes('export') === true)
    expect(exportControls).toHaveLength(0)
  })

  it('renders the missing flag as an evidence gap rather than an empty-but-fine page', async () => {
    await signIn((call) =>
      call.path === '/api/v1/diagnostics'
        ? result({ events: [], missing: true })
        : undefined,
    )
    await visit('/console/diagnostics?session_id=s-1')

    const gaps = notices().filter((text) => text.includes('证据缺口'))
    expect(gaps).toHaveLength(1)
    expect(gaps[0]).toContain('不等于没有发生过')
    expect(wrapper!.text()).toContain('当前查询范围没有诊断事件')
    // 全量采样下才敢说缺失：不得同时出现采样丢弃的文案。
    expect(notices().some((text) => text.includes('sampled_out'))).toBe(false)
  })

  it('keeps the sampled-out wording separate from the missing wording', async () => {
    await signIn((call) =>
      call.path === '/api/v1/diagnostics'
        ? result({ events: [], missing: false, sampled_out: true })
        : undefined,
    )
    await visit('/console/diagnostics?operation_id=op-1')

    expect(diagnosticCalls()[0]?.query.get('operation_id')).toBe('op-1')
    const sampled = notices().filter((text) => text.includes('采样率低于 1'))
    expect(sampled).toHaveLength(1)
    expect(sampled[0]).toContain('无记录不能证明没发生')
    expect(notices().some((text) => text.includes('证据缺口'))).toBe(false)
  })

  it('refuses to send a request without an anchor and says why', async () => {
    await signIn((call) => (call.path === '/api/v1/diagnostics' ? result() : undefined))
    await visit('/console/diagnostics')

    expect(diagnosticCalls()).toHaveLength(0)

    await wrapper!.find('.filters').trigger('submit')
    await flushPromises()

    expect(diagnosticCalls()).toHaveLength(0)
    expect(wrapper!.findAll('p.notice').some((node) => node.text().includes('必须给出至少一个关联标识'))).toBe(true)
  })

  it('accepts request_id as an anchor and marks anchors with no producer', async () => {
    await signIn((call) => (call.path === '/api/v1/diagnostics' ? result() : undefined))
    await visit('/console/diagnostics?request_id=req-1')

    const requests = diagnosticCalls()
    expect(requests).toHaveLength(1)
    // 准入拒绝发生在 Run 账本之前：request_id 是唯一能定位它的锚点，不能漏。
    expect(requests[0]?.query.get('request_id')).toBe('req-1')
    expect(requests[0]?.query.has('run_id')).toBe(false)

    // 没有生产端的锚点 / 种类在选项上直接标注，避免"查空 = 没发生"的误读。
    const options = wrapper!.findAll('.filters option').map((node) => node.text())
    expect(options.some((text) => text.includes('管理操作 ID') && text.includes('暂无生产端'))).toBe(true)
    expect(options.some((text) => text === '摄取阶段')).toBe(true)
    expect(options.some((text) => text.includes('索引切换') && text.includes('暂无生产端'))).toBe(true)
    expect(options.some((text) => text === '模型调用')).toBe(true)
  })

  it('drops the previous result when the next query is rejected locally', async () => {
    await signIn((call) => (call.path === '/api/v1/diagnostics' ? result() : undefined))
    await visit(`/console/diagnostics?run_id=${RUN_ID}`)
    expect(wrapper!.findAll('.diagnostics-table tbody tr')).toHaveLength(2)

    // 清空关联 ID 再查询：旧表格必须消失，不能和新错误并排留在屏幕上。
    await wrapper!.find('.filters input[type="text"]').setValue('')
    await wrapper!.find('.filters').trigger('submit')
    await flushPromises()

    expect(diagnosticCalls()).toHaveLength(1)
    expect(wrapper!.findAll('.diagnostics-table tbody tr')).toHaveLength(0)
    expect(wrapper!.findAll('p.notice').some((node) => node.text().includes('必须给出至少一个关联标识'))).toBe(true)
  })

  it('surfaces a failing endpoint instead of an empty table', async () => {
    await signIn((call) =>
      call.path === '/api/v1/diagnostics' ? failure(500, '服务器内部错误') : undefined,
    )
    await visit(`/console/diagnostics?run_id=${RUN_ID}`)

    expect(wrapper!.text()).toContain('服务器内部错误')
    expect(wrapper!.text()).not.toContain('当前查询范围没有诊断事件')
  })
})
