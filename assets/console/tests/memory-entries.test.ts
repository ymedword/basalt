// 记忆条目管理面板回归：六态按钮矩阵、提议 / 确认 / 纠正 / 删除四个写动作、
// 合并回执、冲突回执、过滤查询、Agent 层路径编码、以及失败时提示不被吞。
// 断言点全部落在"页面说了什么"与"发出了什么请求"上——不靠实现细节。
// Memory management panel regressions: the six-state action matrix, the four writes,
// merged and conflicted receipts, the state filter, agent-scoped paths, and failures
// that stay visible. Every assertion is about what the page says or what it sent.
import { flushPromises, mount } from '@vue/test-utils'
import type { DOMWrapper } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { markRaw } from 'vue'
import { ApiClient } from '../src/api'
import type { MemoryEntryState, MemoryEntryView } from '../src/api'
import { ConsoleAuth } from '../src/auth'
import { MemoryEntries, memoryEntryActions } from '../src/components/memory-entries'

const ENTRIES_PATH = '/api/v1/memory/entries'
const PROPOSALS_PATH = '/api/v1/memory/proposals'

interface Call {
  method: string
  path: string
  query: string
  body: unknown
}

type Handler = (call: Call) => Response | undefined

let wrapper: ReturnType<typeof mount> | undefined

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
})

function envelope(status: number, code: string, message: string, data?: unknown): Response {
  const body: Record<string, unknown> = { code, message, timestamp: '2026-11-01T08:00:00Z' }
  if (data !== undefined) {
    body.data = data
  }
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
    json: async () => body,
  } as Response
}

/** 一行条目的最小夹具：默认是全局层的候选。 */
function entry(overrides: Partial<MemoryEntryView> = {}): MemoryEntryView {
  return {
    entry_id: 1,
    scope: 'global',
    agent: '',
    section: 'archive',
    state: 'candidate',
    content: '默认加班车走 21:30 那班',
    source: 'explicit_propose',
    supersedes_id: 0,
    created_at: '2026-11-01T07:00:00Z',
    // US-22：非自动抽取条目没有候选元数据，这个键恒在、值为 null。
    candidate: null,
    ...overrides,
  }
}

/** 挂载面板并把每次请求记下来；未声明的请求直接抛错，避免测试静默走空。 */
function mountPanel(handler: Handler, agent = ''): Call[] {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: string, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      // 查询串按 key 排序后再比，避免测试依赖参数书写顺序。
      const query = [...url.searchParams.entries()]
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([key, value]) => `${key}=${value}`)
        .join('&')
      const rawBody = init?.body
      const call: Call = {
        method: init?.method ?? 'GET',
        path: url.pathname,
        query,
        body: typeof rawBody === 'string' ? JSON.parse(rawBody) : undefined,
      }
      calls.push(call)
      const response = handler(call)
      if (response === undefined) {
        throw new Error(`未预期的请求：${call.method} ${call.path}`)
      }
      return response
    }),
  )
  wrapper = mount(MemoryEntries, {
    // markRaw：避免 VTU 深代理 props 后破坏 ApiClient 的私有字段品牌检查。
    props: { api: markRaw(new ApiClient(new ConsoleAuth())), agent },
  })
  return calls
}

/** 列表应答：把给定条目包成读模型信封。 */
function listResponse(entries: MemoryEntryView[], agent = ''): Response {
  return envelope(200, 'OK', 'success', {
    scope: agent === '' ? 'global' : 'agent',
    agent,
    entries,
  })
}

function button(label: string): DOMWrapper<Element> {
  const found = wrapper!.findAll('button').find((item) => item.text() === label)
  if (found === undefined) {
    throw new Error(`找不到按钮「${label}」`)
  }
  return found
}

function texts(): string {
  return wrapper!.text()
}

describe('memory entry action matrix', () => {
  it('opens exactly the actions the server would accept for each state', () => {
    // 候选可确认 / 删除；生效可纠正 / 删除；冲突待裁决只能放弃（删除）。
    // 纠正候选或冲突行在域层只会再产出一条 conflicted，界面不提供这种注定的往返。
    expect(memoryEntryActions('candidate')).toEqual(['confirm', 'delete'])
    expect(memoryEntryActions('active')).toEqual(['correct', 'delete'])
    expect(memoryEntryActions('conflicted')).toEqual(['delete'])
    // 历史行留证据，不是可编辑对象。
    for (const state of ['superseded', 'corrected', 'deleted'] as MemoryEntryState[]) {
      expect(memoryEntryActions(state)).toEqual([])
    }
  })
})

describe('memory entries panel', () => {
  it('separates the management view from the recall surface and offers only state-legal buttons', async () => {
    mountPanel(() =>
      listResponse([
        entry({ entry_id: 1, state: 'candidate' }),
        entry({ entry_id: 2, state: 'active', content: '值班表每周一更新' }),
        entry({ entry_id: 3, state: 'conflicted', content: '冲突：两条口径不一致' }),
        entry({ entry_id: 4, state: 'deleted', content: '早已删掉的旧口径' }),
      ]),
    )
    await flushPromises()

    const text = texts()
    // 管理视图 ≠ 召回面：这句必须在页面上，不能靠用户猜。
    expect(text).toContain('能在这里看到，不代表任何业务调用方')
    expect(text).toContain('召回只返回生效且未到期的条目')
    // 候选不自动生效。
    expect(text).toContain('确认前不参与召回')
    // 六态中文化 + 来源与分区的中文映射。
    expect(text).toContain('候选')
    expect(text).toContain('生效')
    expect(text).toContain('冲突待裁决')
    expect(text).toContain('已删除')
    expect(text).toContain('显式提议')
    expect(wrapper!.findAll('button').filter((item) => item.text() === '确认')).toHaveLength(1)
    expect(wrapper!.findAll('button').filter((item) => item.text() === '纠正')).toHaveLength(1)
    // 三条可删（候选 / 生效 / 冲突），已删除的历史行不给按钮。
    expect(wrapper!.findAll('button').filter((item) => item.text() === '删除')).toHaveLength(3)
    expect(text).toContain('历史行（只读）')
  })

  it('proposes a candidate with the default archive section and explains the gate', async () => {
    const calls = mountPanel((call) => {
      if (call.method === 'GET' && call.path === ENTRIES_PATH) {
        return listResponse([])
      }
      if (call.method === 'POST' && call.path === PROPOSALS_PATH) {
        return envelope(200, 'OK', 'success', {
          entry_id: 9,
          state: 'candidate',
          replaced_id: 0,
          merged: false,
        })
      }
      return undefined
    })
    await flushPromises()

    await wrapper!.find('#memory-propose-content').setValue('默认加班车走 21:30 那班')
    await button('提交候选').trigger('click')
    await flushPromises()

    const post = calls.find((call) => call.method === 'POST')
    // 归档是缺省分区，请求体里不写 section——不把默认值当成显式选择发出去。
    expect(post?.body).toEqual({ content: '默认加班车走 21:30 那班' })
    expect(texts()).toContain('已落候选 #9')
    expect(texts()).toContain('确认后才参与召回')
    // 写入成功后整表重读，不做乐观更新。
    expect(calls.filter((call) => call.method === 'GET')).toHaveLength(2)
    // 提交后清空草稿，避免连点重复提交同一段正文。
    expect((wrapper!.find('#memory-propose-content').element as HTMLTextAreaElement).value).toBe('')
  })

  it('sends section=core only when the core partition is explicitly chosen', async () => {
    const calls = mountPanel((call) => {
      if (call.method === 'GET' && call.path === ENTRIES_PATH) {
        return listResponse([])
      }
      if (call.method === 'POST' && call.path === PROPOSALS_PATH) {
        return envelope(200, 'OK', 'success', {
          entry_id: 10,
          state: 'candidate',
          replaced_id: 0,
          merged: false,
        })
      }
      return undefined
    })
    await flushPromises()

    await wrapper!.find('#memory-propose-section').setValue('core')
    await wrapper!.find('#memory-propose-content').setValue('制度口径：报销必须附发票')
    await button('提交候选').trigger('click')
    await flushPromises()

    expect(calls.find((call) => call.method === 'POST')?.body).toEqual({
      content: '制度口径：报销必须附发票',
      section: 'core',
    })
  })

  it('reports a merge instead of claiming a new row', async () => {
    mountPanel((call) => {
      if (call.method === 'GET' && call.path === ENTRIES_PATH) {
        return listResponse([entry({ entry_id: 5, state: 'active' })])
      }
      if (call.method === 'POST' && call.path === PROPOSALS_PATH) {
        return envelope(200, 'OK', 'success', {
          entry_id: 5,
          state: 'active',
          replaced_id: 0,
          merged: true,
        })
      }
      return undefined
    })
    await flushPromises()

    await wrapper!.find('#memory-propose-content').setValue('默认加班车走 21:30 那班')
    await button('提交候选').trigger('click')
    await flushPromises()

    const text = texts()
    expect(text).toContain('已合并到既有条目 #5')
    // 合并回执必须给真实状态，不能伪装成候选。
    expect(text).toContain('状态：生效')
    expect(text).toContain('本次没有新增行')
  })

  it('confirms a candidate through the single promotion path', async () => {
    let confirmed = false
    const calls = mountPanel((call) => {
      if (call.method === 'GET' && call.path === ENTRIES_PATH) {
        return listResponse([entry({ entry_id: 7, state: confirmed ? 'active' : 'candidate' })])
      }
      if (call.method === 'POST' && call.path === '/api/v1/memory/entries/7/confirm') {
        confirmed = true
        return envelope(200, 'OK', 'success', {
          entry_id: 7,
          state: 'active',
          replaced_id: 0,
          merged: false,
        })
      }
      return undefined
    })
    await flushPromises()

    await button('确认').trigger('click')
    await flushPromises()

    expect(calls.some((call) => call.path === '/api/v1/memory/entries/7/confirm')).toBe(true)
    expect(texts()).toContain('条目 #7 已生效')
    // 确认后重读：页面状态来自服务端，不是本地猜的。
    expect(wrapper!.findAll('button').filter((item) => item.text() === '确认')).toHaveLength(0)
    expect(wrapper!.findAll('button').filter((item) => item.text() === '纠正')).toHaveLength(1)
  })

  it('corrects an active entry and names both rows in the receipt', async () => {
    const calls = mountPanel((call) => {
      if (call.method === 'GET' && call.path === ENTRIES_PATH) {
        return listResponse([entry({ entry_id: 3, state: 'active', content: '旧口径' })])
      }
      if (call.method === 'POST' && call.path === '/api/v1/memory/entries/3/correct') {
        return envelope(200, 'OK', 'success', {
          entry_id: 4,
          state: 'active',
          replaced_id: 3,
          merged: false,
        })
      }
      return undefined
    })
    await flushPromises()

    await button('纠正').trigger('click')
    await flushPromises()
    await wrapper!.find('textarea[aria-label="纠正条目 #3"]').setValue('新口径：以 21:30 为准')
    await button('保存纠正').trigger('click')
    await flushPromises()

    expect(calls.find((call) => call.method === 'POST')?.body).toEqual({
      content: '新口径：以 21:30 为准',
    })
    const text = texts()
    expect(text).toContain('#3 已退役')
    expect(text).toContain('#4 接替为生效条目')
  })

  it('reports a conflicted correction without pretending the old row changed', async () => {
    mountPanel((call) => {
      if (call.method === 'GET' && call.path === ENTRIES_PATH) {
        return listResponse([entry({ entry_id: 2, state: 'active' })])
      }
      if (call.method === 'POST' && call.path === '/api/v1/memory/entries/2/correct') {
        return envelope(200, 'OK', 'success', {
          entry_id: 8,
          state: 'conflicted',
          replaced_id: 0,
          merged: false,
        })
      }
      return undefined
    })
    await flushPromises()

    await button('纠正').trigger('click')
    await flushPromises()
    await wrapper!.find('textarea[aria-label="纠正条目 #2"]').setValue('另一套口径')
    await button('保存纠正').trigger('click')
    await flushPromises()

    const text = texts()
    expect(text).toContain('目标 #2 已不在有效态')
    expect(text).toContain('冲突条目 #8 待人工裁决')
    // 旧条目未改动——这句不能省，否则用户会以为原行已被替换。
    expect(text).toContain('旧条目未改动')
  })

  it('deletes through the confirm dialog and says the row is kept', async () => {
    const calls = mountPanel((call) => {
      if (call.method === 'GET' && call.path === ENTRIES_PATH) {
        return listResponse([entry({ entry_id: 6, state: 'candidate' })])
      }
      if (call.method === 'DELETE' && call.path === '/api/v1/memory/entries/6') {
        return envelope(200, 'OK', 'success', {
          entry_id: 6,
          state: 'deleted',
          replaced_id: 0,
          merged: false,
        })
      }
      return undefined
    })
    await flushPromises()

    await button('删除').trigger('click')
    await flushPromises()
    // 对话框先给出摘要与"行与审计保留"的口径，再等用户确认。
    expect(wrapper!.find('[role="dialog"]').exists()).toBe(true)
    expect(wrapper!.find('[role="dialog"]').text()).toContain('行与审计保留')
    await button('确认删除').trigger('click')
    await flushPromises()

    expect(calls.some((call) => call.method === 'DELETE')).toBe(true)
    expect(texts()).toContain('条目 #6 已软删')
    expect(texts()).toContain('之后不会再被召回')
    expect(wrapper!.find('[role="dialog"]').exists()).toBe(false)
  })

  it('filters by state on the server instead of hiding rows locally', async () => {
    const calls = mountPanel((call) => {
      if (call.method === 'GET' && call.path === ENTRIES_PATH) {
        return listResponse([entry({ entry_id: 1, state: 'candidate' })])
      }
      return undefined
    })
    await flushPromises()

    await wrapper!.find('#memory-filter').setValue('candidate')
    await flushPromises()

    const filtered = calls.filter((call) => call.method === 'GET').at(-1)
    expect(filtered?.query).toBe('state=candidate')
  })

  it('scopes every path to the agent layer when an agent is given', async () => {
    const calls = mountPanel((call) => {
      if (call.method === 'GET' && call.path === '/api/v1/agents/reporter-a/memory/entries') {
        return listResponse([entry({ entry_id: 1, scope: 'agent', agent: 'reporter-a' })], 'reporter-a')
      }
      if (call.method === 'POST' && call.path === '/api/v1/agents/reporter-a/memory/proposals') {
        return envelope(200, 'OK', 'success', {
          entry_id: 2,
          state: 'candidate',
          replaced_id: 0,
          merged: false,
        })
      }
      return undefined
    }, 'reporter-a')
    await flushPromises()

    await wrapper!.find('#memory-propose-content').setValue('这个 Agent 自己的口径')
    await button('提交候选').trigger('click')
    await flushPromises()

    expect(calls.map((call) => `${call.method} ${call.path}`)).toEqual([
      'GET /api/v1/agents/reporter-a/memory/entries',
      'POST /api/v1/agents/reporter-a/memory/proposals',
      'GET /api/v1/agents/reporter-a/memory/entries',
    ])
  })

  it('keeps a rejected write visible and does not reload the table', async () => {
    const calls = mountPanel((call) => {
      if (call.method === 'GET' && call.path === ENTRIES_PATH) {
        return listResponse([entry({ entry_id: 4, state: 'candidate' })])
      }
      if (call.method === 'POST' && call.path === '/api/v1/memory/entries/4/confirm') {
        return envelope(409, 'CONFLICT', '目标条目已不在候选态，请刷新后重试')
      }
      return undefined
    })
    await flushPromises()

    await button('确认').trigger('click')
    await flushPromises()

    expect(texts()).toContain('目标条目已不在候选态，请刷新后重试')
    // 失败不重读、不乐观更新：页面停在服务端已知的状态上等用户处理。
    expect(calls.filter((call) => call.method === 'GET')).toHaveLength(1)
    expect(wrapper!.findAll('button').filter((item) => item.text() === '确认')).toHaveLength(1)
  })
})
