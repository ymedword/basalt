// US-25C §2.3.7 / FR-25C-17：技能发布联动重发的逐 Agent 结果渲染与失败告警。
// 发布成功看版本号；联动重发是后续动作——ok / skipped / superseded 只列徽标，
// missing / failed 追加告警并点名原因。非技能发布不渲染该区块，也不显示「0 个」。
// Per-agent republish results on skill publish: warnings only for missing/failed.
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  REPUBLISH_STATUS_LABELS,
  republishRows,
  republishSummaryText,
  republishWarningText,
} from '../src/components/publish-results'
import { createConsoleApp } from '../src/app'

const ETAG = `"sha256:${'a'.repeat(64)}"`
const UID = 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A'

interface RecordedCall {
  readonly method: string
  readonly path: string
  readonly body: unknown
  readonly ifMatch: string | null
}

interface Reply {
  status?: number
  code?: string
  message?: string
  data?: unknown
  etag?: string
}

let calls: RecordedCall[] = []
let wrapper: ReturnType<typeof mount> | undefined

function setupFetch(reply: (call: RecordedCall) => Reply = () => ({})): void {
  calls = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: string, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      const headers = new Headers(init?.headers ?? {})
      const call: RecordedCall = {
        method: init?.method ?? 'GET',
        path: url.pathname + url.search,
        body: init?.body === undefined ? undefined : JSON.parse(String(init.body)),
        ifMatch: headers.get('If-Match'),
      }
      calls.push(call)
      const result = reply(call)
      const status = result.status ?? 200
      const responseHeaders = new Headers()
      if (result.etag !== undefined) {
        responseHeaders.set('ETag', result.etag)
      }
      return {
        ok: status >= 200 && status < 300,
        status,
        headers: responseHeaders,
        json: async () => ({
          code: result.code ?? 'OK',
          message: result.message ?? '',
          data: result.data ?? null,
          timestamp: '2026-11-09T00:00:00Z',
        }),
      } as Response
    }),
  )
}

function skillDetail(): Record<string, unknown> {
  return {
    name: 'alpha',
    description: 'alpha skill',
    path: 'alpha',
    version_id: 1,
    version_no: 1,
    content_hash: 'hash',
    etag: ETAG,
    file_count: 1,
    referenced_by: [],
    files: [{ path: 'SKILL.md', size: 12, sha256: 'sha', content: '# alpha' }],
  }
}

function versionView(): Record<string, unknown> {
  return {
    version_id: 2,
    resource_kind: 'skill',
    resource_id: 'alpha',
    resource_uid: UID,
    version_no: 2,
    content_hash: `sha256:${'e'.repeat(64)}`,
    created_at: '2026-11-09T08:00:00Z',
    operation_id: 'op-1',
    published: true,
    applied: false,
    reverted_from_version_id: null,
  }
}

function referencesView(): Record<string, unknown> {
  return {
    kind: 'skill',
    id: 'alpha',
    resource_uid: UID,
    published_version_id: 1,
    agents: ['writer', 'reader'],
    bound_agents: [],
    active_runs: [],
  }
}

/** 登录后直达技能详情，避开总览页的旁路请求。 */
async function openDetail(path: string): Promise<void> {
  window.history.replaceState(
    null,
    '',
    `/console/login?next=${encodeURIComponent(path)}`,
  )
  wrapper = mount(createConsoleApp(), { attachTo: document.body })
  await flushPromises()
  await wrapper.find('#console-token').setValue('test-console-token')
  await wrapper.find('.login-form').trigger('submit')
  await flushPromises()
}

function clickButton(label: string): Promise<void> {
  const button = wrapper!.findAll('button').find((item) => item.text() === label)
  expect(button, label).toBeDefined()
  return button!.trigger('click')
}

function publishCalls(): RecordedCall[] {
  return calls.filter((call) => call.method === 'POST' && call.path.includes('/publish'))
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
})

describe('republish result helpers', () => {
  it('labels every contract status and treats unknown ones as alerts', () => {
    expect(REPUBLISH_STATUS_LABELS).toEqual({
      ok: '已重发',
      skipped: '无需重发',
      superseded: '已被更新的版本超越',
      missing: '定义缺失',
      failed: '重发失败',
    })
    const rows = republishRows([
      { agent: 'writer', status: 'ok' },
      { agent: 'reader', status: 'failed', error: '重建 Agent 资产失败' },
      // 服务端未来扩展的状态：原样显示、按告警处理，不静默当成成功。
      { agent: 'legacy', status: 'deferred' as never },
    ])
    expect(rows.map((row) => row.label)).toEqual(['已重发', '重发失败', 'deferred'])
    expect(rows.map((row) => row.alert)).toEqual([false, true, true])
    expect(republishRows(undefined)).toEqual([])
  })

  it('warns only for missing / failed dependents and never for the benign statuses', () => {
    const benign = [
      { agent: 'writer', status: 'ok' as const },
      { agent: 'reader', status: 'skipped' as const },
      { agent: 'beta', status: 'superseded' as const },
    ]
    expect(republishWarningText(benign)).toBe('')
    expect(republishWarningText(undefined)).toBe('')

    const warning = republishWarningText([
      ...benign,
      { agent: 'gamma', status: 'missing', error: 'Agent 定义缺失' },
      { agent: 'delta', status: 'failed', error: '重建 Agent 资产失败' },
    ])
    expect(warning).toContain('技能已发布，但以下依赖 Agent 的重发未完成')
    expect(warning).toContain('gamma（定义缺失：Agent 定义缺失）')
    expect(warning).toContain('delta（重发失败：重建 Agent 资产失败）')
    expect(warning).toContain('控制面修复器会在后续轮次自动重试')
    // 告警不覆盖发布结果本身，也不点名无关的良性 Agent。
    expect(warning).not.toContain('writer')
    expect(warning).not.toContain('reader')
  })

  it('summarizes the batch and renders nothing when no republish was triggered', () => {
    expect(republishSummaryText(undefined)).toBe('')
    expect(republishSummaryText([])).toBe('')
    const summary = republishSummaryText([
      { agent: 'writer', status: 'ok' },
      { agent: 'reader', status: 'ok' },
      { agent: 'beta', status: 'skipped' },
      { agent: 'gamma', status: 'failed', error: '重建 Agent 资产失败' },
    ])
    expect(summary).toContain('共 4 个依赖 Agent')
    expect(summary).toContain('已重发 2')
    expect(summary).toContain('无需重发 1')
    expect(summary).toContain('重发失败 1')
  })
})

describe('skill publish results in the resource detail view', () => {
  it('renders per-agent results and raises a warning for failed dependents', async () => {
    setupFetch((call) => {
      if (call.path.endsWith('/info')) {
        return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 0, providers: [] } }
      }
      if (call.method === 'GET' && call.path === '/api/v1/admin/skills/alpha/edit') {
        return { data: skillDetail(), etag: ETAG }
      }
      if (call.method === 'GET' && call.path === '/api/v1/resources/skill/alpha/versions') {
        return { data: [versionView()] }
      }
      if (call.method === 'GET' && call.path === '/api/v1/resources/skill/alpha/references') {
        return { data: referencesView() }
      }
      if (call.method === 'POST' && call.path === '/api/v1/resources/skill/alpha/publish') {
        return {
          data: {
            version: versionView(),
            pending_restart: false,
            publish: [
              { agent: 'writer', status: 'ok' },
              { agent: 'reader', status: 'skipped' },
              { agent: 'gamma', status: 'failed', error: '重建 Agent 资产失败' },
            ],
          },
        }
      }
      return {}
    })
    await openDetail('/console/resources/skills/alpha')

    await clickButton('发布最新版本')
    await flushPromises()

    const writes = publishCalls()
    expect(writes).toHaveLength(1)
    expect(writes[0]!.ifMatch).toBe(ETAG)
    expect(writes[0]!.body).toEqual({})

    const text = wrapper!.text()
    // 发布结论与联动结果是两件事，分别出现。
    expect(text).toContain('已发布版本 2')
    expect(text).toContain('技能发布联动结果')
    expect(text).toContain('共 3 个依赖 Agent')
    expect(text).toContain('已重发')
    expect(text).toContain('无需重发')
    expect(text).toContain('重发失败')
    expect(text).toContain('gamma')
    expect(text).toContain('重建 Agent 资产失败')
    // 告警条：发布成功、重发未完成。
    const alerts = wrapper!.findAll('.notice-warn').map((item) => item.text())
    expect(alerts.some((item) => item.includes('依赖 Agent 的重发未完成'))).toBe(true)
    expect(alerts.some((item) => item.includes('gamma（重发失败：重建 Agent 资产失败）'))).toBe(true)
    // 发布本身成功：不得渲染成错误条。
    expect(wrapper!.findAll('.notice-error').map((item) => item.text()).join('')).not.toContain('依赖 Agent')
  })

  it('keeps the block silent when no dependent republish was triggered', async () => {
    setupFetch((call) => {
      if (call.path.endsWith('/info')) {
        return { data: { version: 'test', uptime_seconds: 1, agents: 1, tools: 0, mcp_servers: 0, providers: [] } }
      }
      if (call.method === 'GET' && call.path === '/api/v1/admin/skills/alpha/edit') {
        return { data: skillDetail(), etag: ETAG }
      }
      if (call.method === 'GET' && call.path === '/api/v1/resources/skill/alpha/versions') {
        return { data: [versionView()] }
      }
      if (call.method === 'GET' && call.path === '/api/v1/resources/skill/alpha/references') {
        return { data: referencesView() }
      }
      if (call.method === 'POST' && call.path === '/api/v1/resources/skill/alpha/publish') {
        return { data: { version: versionView(), pending_restart: false } }
      }
      return {}
    })
    await openDetail('/console/resources/skills/alpha')

    await clickButton('发布最新版本')
    await flushPromises()

    const text = wrapper!.text()
    expect(text).toContain('已发布版本 2')
    expect(text).not.toContain('技能发布联动结果')
    expect(text).not.toContain('共 0 个依赖 Agent')
    expect(wrapper!.findAll('.notice-warn')).toHaveLength(0)
  })
})
