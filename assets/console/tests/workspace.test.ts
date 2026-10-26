// 导航与读数回归：新入口复用已有 API；不把不完整读取渲染成健康状态。
// Navigation and readiness regressions over existing APIs, without issuing mutations.
import { mount, flushPromises } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createConsoleApp } from '../src/app'
import { matchRoute } from '../src/router'

const summary = {
  version: 'test-version', uptime_seconds: 3600, started_at: '2026-10-21T12:00:00Z',
  agents: { total: 2, active: 1, disabled: 1, archived: 0, invalid: 0 },
  runs: { running: 0, last_24h: 3, failed_last_24h: 0 },
  resources: { model_connections: 1, mcp_servers: 0, skills: 1 },
  database: { status: 'ok', schema_version: 2 }, auth: { mode: 'token', loopback_only: true },
}
const profiles = ['writer', 'reviewer'].map((name, index) => ({
  name, display_name: '', description: name + ' description', provider: 'local-model', model: 'model-v1',
  tools: [], skills: [], schedules: 0, registered: true, runnable: index === 0, problems: [], notify_channels: [],
  lifecycle: { agent: name, state: index === 0 ? 'active' : 'disabled', valid: true, generation: 1, updated_at: '2026-10-21T12:00:00Z', operation_id: 'op-test', lifecycle_error: '' },
}))
let wrapper: ReturnType<typeof mount> | undefined

function setupFetch(profileFailure = false, responses: Record<string, unknown> = {}): void {
  vi.stubGlobal('fetch', vi.fn(async (input: string) => {
    const path = new URL(String(input), 'http://localhost').pathname
    if (profileFailure && path.endsWith('/profiles')) throw new Error('offline')
    let data: unknown = []
    if (path.endsWith('/info')) data = { version: 'test-version', uptime_seconds: 3600, agents: 2, tools: 0, mcp_servers: 0, providers: [] }
    if (path.endsWith('/system/overview')) data = summary
    if (path.endsWith('/system/sample')) data = { sampled_at: '2026-10-21T13:00:00Z', cpu_percent: null, memory_rss_bytes: 1024, goroutines: 12, unavailable: ['cpu_percent'] }
    if (path.endsWith('/profiles')) data = profiles
    if (path.endsWith('/memory')) data = { scope: 'global', agent: '', backend: 'markdown', core: 'shared memory', archive: '' }
    if (Object.hasOwn(responses, path)) data = responses[path]
    return { ok: true, status: 200, headers: new Headers(), json: async () => ({ code: 'OK', message: '', data, timestamp: '2026-10-21T13:00:00Z' }) } as Response
  }))
}

async function signIn(profileFailure = false, responses: Record<string, unknown> = {}): Promise<void> {
  setupFetch(profileFailure, responses)
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

afterEach(() => { wrapper?.unmount(); wrapper = undefined; vi.unstubAllGlobals() })

describe('workspace navigation', () => {
  it('keeps the resource publication kinds while exposing existing read pages', () => {
    for (const path of ['/console/library/tools', '/console/library/memory', '/console/settings/notifications', '/console/settings/sandbox']) {
      expect(matchRoute(path).name).not.toBe('not-found')
    }
    expect(matchRoute('/console/resources/widgets').name).toBe('not-found')
  })

  it('signs in and groups the navigation without a competing global run entry', async () => {
    await signIn()
    expect(wrapper!.find('.console-sidebar').exists()).toBe(true)
    expect(wrapper!.findAll('.nav-group-label').map(item => item.text())).toEqual(['工作区', '资源库', '系统管理'])
    expect(wrapper!.find('.console-sidebar a[href="/console/runs"]').exists()).toBe(false)
    expect(wrapper!.find('h1').text()).toBe('系统总览')
    expect(wrapper!.find('.clear-state').text()).toContain('当前没有待处理的配置问题')
    expect(wrapper!.text()).toContain('未执行模型探测')
  })

  it('navigates to shared memory using the existing authenticated read API', async () => {
    await signIn()
    await wrapper!.find('a[href="/console/library/memory"]').trigger('click')
    await flushPromises()
    expect(wrapper!.find('h1').text()).toBe('全局记忆')
    expect(wrapper!.text()).toContain('shared memory')
    expect(wrapper!.find('.nav-item.active').text()).toContain('全局记忆')
  })

  it('moves focus to main content without a route transition', async () => {
    await signIn()
    const before = window.location.pathname
    await wrapper!.find('.skip-link').trigger('click')
    expect(document.activeElement?.id).toBe('console-main')
    expect(window.location.pathname).toBe(before)
    expect(wrapper!.find('h1').text()).toBe('系统总览')
  })

  it('filters Agent cards without dropping the lifecycle distinction', async () => {
    await signIn()
    await wrapper!.find('.console-sidebar a[href="/console/agents"]').trigger('click')
    await flushPromises()
    expect(wrapper!.findAll('.agent-card')).toHaveLength(2)
    await wrapper!.find('input[type="search"]').setValue('writer')
    expect(wrapper!.findAll('.agent-card')).toHaveLength(1)
    expect(wrapper!.find('.agent-card').text()).toContain('可运行')
    await wrapper!.find('input[type="search"]').setValue('reviewer')
    expect(wrapper!.find('.agent-card').text()).toContain('停用')
    expect(wrapper!.find('.agent-card').text()).toContain('暂不可运行')
  })

  it('does not declare the workspace clear when profile retrieval fails', async () => {
    await signIn(true)
    expect(wrapper!.find('.system-strip-status').text()).toContain('状态待确认')
    expect(wrapper!.find('.clear-state').text()).not.toContain('当前没有待处理的配置问题')
    expect(wrapper!.find('.notice-error').exists()).toBe(true)
  })

  it('keeps run navigation unselected and final output in a single section', async () => {
    const run = { run_id: 'run-long-0123456789abcdef', agent: 'writer', session_id: 's1', status: 'completed', trigger: 'manual', channel: 'api', iterations: 2, tool_calls: 1, config_hash: 'abc', manifest_hash: 'def', started_at: '2026-10-21T12:00:00Z', finished_at: '2026-10-21T12:01:00Z', reason: '', final_text: '', resource_manifest: null }
    await signIn(false, { '/api/v1/runs': [run], ['/api/v1/runs/' + run.run_id]: run })
    expect(wrapper!.find('.recent-run').attributes('href')).toBe('/console/runs/' + run.run_id)
    await visit('/console/runs')
    expect(wrapper!.find('h1').text()).toBe('运行列表')
    expect(wrapper!.find('.topbar-breadcrumb').text()).toContain('运行列表')
    expect(wrapper!.findAll('.nav-item.active')).toHaveLength(0)
    expect(wrapper!.find('.runs-table .cell-id').attributes('title')).toBe(run.run_id)
    await wrapper!.find('.runs-table a').trigger('click')
    await flushPromises()
    expect(wrapper!.find('h1').text()).toBe('运行详情')
    expect(wrapper!.findAll('h1')).toHaveLength(1)
    expect(wrapper!.find('.detail-id').text()).toBe(run.run_id)
    expect(wrapper!.find('.detail-id').attributes('title')).toBe(run.run_id)
    expect(wrapper!.find('.definitions .cell-id').attributes('title')).toBe(run.session_id)
    expect(wrapper!.findAll('h2').filter(item => item.text() === '最终文本')).toHaveLength(1)
    expect(wrapper!.findAll('dt').some(item => item.text() === '最终文本')).toBe(false)
    expect(wrapper!.findAll('.nav-item.active')).toHaveLength(0)
  })

  it('uses one capability entry and the same page heading across resources and errors', async () => {
    await signIn()
    for (const [path, title] of [
      ['/console/resources/skills', '技能'], ['/console/resources/model-connections', '模型与连接'],
      ['/console/resources/mcp-servers', '工具与 MCP'], ['/console/operations', '管理操作'],
      ['/console/missing', '页面不存在'],
    ]) {
      await visit(path!)
      expect(wrapper!.find('.page-heading h1').text()).toBe(title)
      expect(wrapper!.find('.page-heading .page-description').text().length).toBeGreaterThan(0)
    }
    await visit('/console/resources/mcp-servers')
    expect(wrapper!.find('.nav-item.active').text()).toContain('工具与 MCP')
    expect(wrapper!.findAll('.capability-tabs a').map(link => link.text())).toEqual(['可用工具', 'MCP 服务'])
    expect(wrapper!.find('.capability-tabs a[aria-current="page"]').text()).toBe('MCP 服务')
    expect(wrapper!.findAll('.nav-arrow')).toHaveLength(0)
  })

  it('preserves full tool identifiers and explains the backend directory template', async () => {
    const policyMeta = { etag: '"sha256:test"', revision: 1, source: 'instance_init', updated_at: '', updated_by: '' }
    await signIn(false, {
      '/api/v1/tools': [{ name: 'manage_schedule', description: '维护定时任务', source: 'builtin' }],
      '/api/v1/sandbox': {
        file_allowed_paths: ['.basalt/agents/<agent>/'], shell_allowed_commands: [], shell_interpreters: [],
        http_allowed_domains: [], network_allowlist: { etag: '"sha256:test"', revision: 1, source: 'config_seed', updated_at: '', updated_by: '' },
        execution_profile: 'standard', execution_policy: policyMeta,
        denied_domains: [], network_denylist: policyMeta,
      },
      '/api/v1/execution-policy': {
        ...policyMeta, profile: 'standard', exceptions_count: 0, protected_addresses: [], shell_names: [],
        pending_confirmations: [],
      },
      '/api/v1/network-exceptions': { etag: '"sha256:test"', exceptions: [] },
    })
    await visit('/console/library/tools')
    expect(wrapper!.find('.col-tool .cell-id').attributes('title')).toBe('manage_schedule')
    expect(wrapper!.find('td.col-source').text()).toBe('内置工具')
    await visit('/console/settings/sandbox')
    expect(wrapper!.find('.path-note').text()).toContain('按当前 Agent 名替换')
    expect(wrapper!.find('.policy-grid code').text()).toBe('.basalt/agents/<agent>/')
  })
})

describe('instance secrets surfaces', () => {
  /** 契约视图夹具：状态与汇总计数是页面唯一依赖的字段。 */
  function secretsView(
    state: 'ready' | 'uninitialized' | 'recovery_required',
    reason = '',
  ): unknown {
    return {
      state,
      key_id: state === 'ready' ? 'k_7Q3M9X2P' : '',
      initialized_at: state === 'ready' ? '2026-10-20T08:00:00Z' : '',
      key_file: state === 'ready' ? '/var/lib/basalt/secrets/instance.key' : '',
      recovery_reason: reason,
      // 未初始化默认按「可就地初始化」给：普通流程不显示"请先初始化"。
      can_initialize: state === 'uninitialized',
      credential_summary: { managed: 3, managed_revoked: 0, env: 1, missing: 0 },
    }
  }

  function alerts(): string {
    return wrapper!.findAll('[role="alert"]').map((item) => item.text()).join('\n')
  }

  it('shows the instance encryption state and credential summary on the overview', async () => {
    await signIn(false, { '/api/v1/instance/secrets': secretsView('ready') })
    const text = wrapper!.text()
    expect(text).toContain('实例与凭据')
    expect(text).toContain('实例加密服务')
    expect(text).toContain('就绪（k_7Q3M…）')
    expect(text).toContain('当前连接凭据')
    expect(text).toContain('3 个托管 / 1 个环境变量')
    // 就绪态不挂恢复横幅。
    expect(alerts()).not.toContain('实例主密钥需要恢复')
  })

  it('keeps the recovery banner on every authenticated page', async () => {
    await signIn(false, {
      '/api/v1/instance/secrets': secretsView('recovery_required', 'missing'),
    })
    expect(alerts()).toContain('实例主密钥需要恢复')
    expect(alerts()).toContain('docs/Operations.md')
    expect(alerts()).toContain('不要删除密钥文件')
    await visit('/console/runs')
    expect(alerts()).toContain('实例主密钥需要恢复')
  })

  it('does not claim readiness or raise the banner for an unrecognised payload', async () => {
    await signIn(false, { '/api/v1/instance/secrets': [] })
    expect(wrapper!.text()).toContain('待确认')
    expect(wrapper!.text()).not.toContain('就绪（')
    expect(alerts()).not.toContain('实例主密钥需要恢复')
  })

  it('states the shared access password and its limits on the login page', async () => {
    setupFetch()
    window.history.replaceState(null, '', '/console/login')
    wrapper = mount(createConsoleApp(), { attachTo: document.body })
    await flushPromises()
    const text = wrapper.text()
    expect(text).toContain('共享访问密码')
    expect(text).toContain('系统不提供个人账户、角色或操作人隔离')
  })
})
