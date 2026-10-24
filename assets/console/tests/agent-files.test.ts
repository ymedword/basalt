// Agent 多文件编辑器回归：412 刷新基线保留输入、409 不动基线、写后重读不做乐观更新，
// 以及正文读取失败的文件不参与提交（避免用空正文覆盖原文件）。
// Regressions for the multi-file editor: 412 keeps user text while moving the baseline,
// 409 leaves the baseline untouched, successful writes re-read instead of echoing, and
// unreadable files are never submitted as empty content.
import { flushPromises, mount } from '@vue/test-utils'
import type { DOMWrapper } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { markRaw } from 'vue'
import { ApiClient } from '../src/api'
import { ConsoleAuth } from '../src/auth'
import { AgentFiles } from '../src/components/agent-files'

const AGENT = 'ops'
const FILES_PATH = '/api/v1/agents/ops/files'
const WORKSPACE_PATH = '/api/v1/workspace/file'

interface Call {
  method: string
  path: string
  query: URLSearchParams
  ifMatch: string | null
  body: unknown
}

type Handler = (call: Call) => Response | undefined

let wrapper: ReturnType<typeof mount> | undefined

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  vi.unstubAllGlobals()
})

/** 让数组下标在 noUncheckedIndexedAccess 下有明确失败信息。 */
function required<T>(value: T | undefined, what: string): T {
  if (value === undefined) {
    throw new Error(`找不到${what}`)
  }
  return value
}

function envelope(
  status: number,
  code: string,
  message: string,
  data?: unknown,
  etag = '',
): Response {
  const body: Record<string, unknown> = {
    code,
    message,
    timestamp: '2026-10-22T08:00:00Z',
  }
  if (data !== undefined) {
    body.data = data
  }
  const headers = new Headers()
  if (etag !== '') {
    headers.set('ETag', etag)
  }
  return {
    ok: status >= 200 && status < 300,
    status,
    headers,
    json: async () => body,
  } as Response
}

function filesView(etag: string, paths: string[]): unknown {
  return {
    agent: AGENT,
    lifecycle: {
      agent: AGENT,
      state: 'disabled',
      generation: 1,
      updated_at: '',
      operation_id: '',
      valid: true,
      lifecycle_error: '',
    },
    etag,
    files: paths.map((path) => ({ path, size: path.length })),
  }
}

function workspaceFile(path: string, content: string): unknown {
  return { path, content, size: content.length, truncated: false }
}

/** 记录每次请求并按 handler 返回；缺分支直接抛错，避免测试静默走空。 */
function mountEditor(handler: Handler): Call[] {
  const calls: Call[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: string, init?: RequestInit) => {
      const url = new URL(String(input), 'http://localhost')
      const headers = new Headers(init?.headers)
      const rawBody = init?.body
      const call: Call = {
        method: init?.method ?? 'GET',
        path: url.pathname,
        query: url.searchParams,
        ifMatch: headers.get('If-Match'),
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
  // markRaw：vue-test-utils 会把 mount props 深代理（reactive），代理后的类实例
  // 过不了 ApiClient 私有字段的品牌检查（Receiver must be an instance...）。
  // markRaw keeps the client out of VTU's deep-reactive props wrapper.
  wrapper = mount(AgentFiles, {
    props: { api: markRaw(new ApiClient(new ConsoleAuth())), agent: AGENT },
  })
  return calls
}

function textareas(): DOMWrapper<Element>[] {
  return wrapper!.findAll('.file-content')
}

function textareaValue(index: number): string {
  const editor = required(textareas()[index], `第 ${index} 个文件编辑器`)
  return (editor.element as HTMLTextAreaElement).value
}

function button(label: string): DOMWrapper<Element> {
  const found = wrapper!.findAll('button').find((item) => item.text() === label)
  return required(found, `按钮「${label}」`)
}

function submitCall(calls: Call[]): Call {
  return required(
    calls.find((call) => call.method === 'POST' && call.path === FILES_PATH),
    '提交请求',
  )
}

describe('agent files editor', () => {
  it('reloads the set ETag and baseline on 412 while keeping the user text', async () => {
    let filesReads = 0
    const calls = mountEditor((call) => {
      if (call.method === 'GET' && call.path === FILES_PATH) {
        filesReads += 1
        // 首次加载只有 AGENT.md；412 后重读多出 notes.md，且基线内容已变。
        return filesReads === 1
          ? envelope(200, 'OK', '', filesView('"set-1"', ['AGENT.md']), '"set-1"')
          : envelope(200, 'OK', '', filesView('"set-2"', ['AGENT.md', 'notes.md']), '"set-2"')
      }
      if (call.method === 'GET' && call.path === WORKSPACE_PATH) {
        const path = call.query.get('path') ?? ''
        if (path === `${AGENT}/AGENT.md`) {
          return envelope(200, 'OK', '', workspaceFile(path, filesReads === 1 ? '# v1' : '# v2'))
        }
        return envelope(200, 'OK', '', workspaceFile(path, 'note'))
      }
      if (call.method === 'POST' && call.path === FILES_PATH) {
        return envelope(412, 'PRECONDITION_FAILED', '定义文件已被其他写入修改')
      }
      return undefined
    })
    await flushPromises()

    expect(wrapper!.find('.files-etag').text()).toBe('"set-1"')
    expect(textareaValue(0)).toBe('# v1')
    await required(textareas()[0], 'AGENT.md 编辑器').setValue('# mine')
    await button('提交文件').trigger('click')
    await flushPromises()

    expect(submitCall(calls).ifMatch).toBe('"set-1"')
    expect(submitCall(calls).body).toEqual({
      files: [{ path: 'AGENT.md', content: '# mine' }],
    })

    // 412：固定报错文案；ETag 与基线前移；用户输入原样保留；新文件并入编辑器。
    expect(wrapper!.find('.notice-error').text()).toContain('文件集合已被其他写入修改')
    expect(wrapper!.find('.files-etag').text()).toBe('"set-2"')
    expect(wrapper!.findAll('.file-editor')).toHaveLength(2)
    expect(textareaValue(0)).toBe('# mine')
    expect(textareaValue(1)).toBe('note')
    expect(wrapper!.find('.notice-info').exists()).toBe(false)
  })

  it('shows the server reason on 409 without re-reading or moving the baseline', async () => {
    let filesReads = 0
    const calls = mountEditor((call) => {
      if (call.method === 'GET' && call.path === FILES_PATH) {
        filesReads += 1
        return envelope(200, 'OK', '', filesView('"set-1"', ['AGENT.md']), '"set-1"')
      }
      if (call.method === 'GET' && call.path === WORKSPACE_PATH) {
        return envelope(200, 'OK', '', workspaceFile(`${AGENT}/AGENT.md`, '# v1'))
      }
      if (call.method === 'POST' && call.path === FILES_PATH) {
        return envelope(409, 'CONFLICT', '定义文件与已发布版本不一致：请重新读取后再保存')
      }
      return undefined
    })
    await flushPromises()

    await required(textareas()[0], 'AGENT.md 编辑器').setValue('# mine')
    await button('提交文件').trigger('click')
    await flushPromises()

    expect(submitCall(calls).ifMatch).toBe('"set-1"')
    expect(wrapper!.find('.notice-error').text()).toBe(
      '定义文件与已发布版本不一致：请重新读取后再保存',
    )
    // 409 是分歧闸门：不重读、不动基线，编辑内容保留。
    expect(filesReads).toBe(1)
    expect(wrapper!.find('.files-etag').text()).toBe('"set-1"')
    expect(textareaValue(0)).toBe('# mine')
    expect(wrapper!.find('.notice-info').exists()).toBe(false)
  })

  it('re-reads after a successful write instead of echoing the request body', async () => {
    let filesReads = 0
    const calls = mountEditor((call) => {
      if (call.method === 'GET' && call.path === FILES_PATH) {
        filesReads += 1
        return filesReads === 1
          ? envelope(200, 'OK', '', filesView('"set-1"', ['AGENT.md']), '"set-1"')
          : envelope(200, 'OK', '', filesView('"set-2"', ['AGENT.md', 'extra.md']), '"set-2"')
      }
      if (call.method === 'GET' && call.path === WORKSPACE_PATH) {
        const path = call.query.get('path') ?? ''
        if (path === `${AGENT}/AGENT.md`) {
          // 服务端规范化后的正文与提交内容不同：页面必须展示重读结果。
          return envelope(200, 'OK', '', workspaceFile(path, '# server-normalized'))
        }
        return envelope(200, 'OK', '', workspaceFile(path, 'extra'))
      }
      if (call.method === 'POST' && call.path === FILES_PATH) {
        // 故意回显一个与重读结果不同的集合：组件不得采用它。
        return envelope(200, 'OK', '', filesView('"echo"', ['AGENT.md']), '"echo"')
      }
      return undefined
    })
    await flushPromises()

    await required(textareas()[0], 'AGENT.md 编辑器').setValue('# mine')
    await button('提交文件').trigger('click')
    await flushPromises()

    expect(submitCall(calls).ifMatch).toBe('"set-1"')
    expect(wrapper!.find('.notice-info').text()).toContain('已提交')
    expect(wrapper!.find('.files-etag').text()).toBe('"set-2"')
    expect(wrapper!.findAll('.file-editor')).toHaveLength(2)
    expect(textareaValue(0)).toBe('# server-normalized')
    expect(textareaValue(1)).toBe('extra')
    // 重读后基线一致，提交按钮回到禁用状态（没有残留的脏状态）。
    expect(button('提交文件').attributes('disabled')).toBeDefined()
  })

  it('keeps unreadable files out of the submission', async () => {
    const calls = mountEditor((call) => {
      if (call.method === 'GET' && call.path === FILES_PATH) {
        return envelope(
          200,
          'OK',
          '',
          filesView('"set-1"', ['AGENT.md', 'big.bin']),
          '"set-1"',
        )
      }
      if (call.method === 'GET' && call.path === WORKSPACE_PATH) {
        const path = call.query.get('path') ?? ''
        if (path === `${AGENT}/AGENT.md`) {
          return envelope(200, 'OK', '', workspaceFile(path, '# v1'))
        }
        return envelope(400, 'FILE_TOO_LARGE', '文件超出预览上限')
      }
      if (call.method === 'POST' && call.path === FILES_PATH) {
        return envelope(200, 'OK', '', filesView('"set-2"', ['AGENT.md', 'big.bin']), '"set-2"')
      }
      return undefined
    })
    await flushPromises()

    expect(wrapper!.find('.notice-warn').text()).toContain('big.bin')
    expect(textareas()).toHaveLength(1)
    await required(textareas()[0], 'AGENT.md 编辑器').setValue('# mine')
    await button('提交文件').trigger('click')
    await flushPromises()

    // 只提交可读文件：big.bin 不以空正文回写。
    expect(submitCall(calls).body).toEqual({
      files: [{ path: 'AGENT.md', content: '# mine' }],
    })
  })
})
