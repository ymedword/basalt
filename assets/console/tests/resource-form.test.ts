// 资源表单回归：托管 Key 直接录入并可发布、env 作为高级方式只收占位、两种模式的
// 本地拒绝零请求（且错误不回显输入）、新建时可选择范围与所有者、编辑留空保留
// 当前凭据、切换模式必须带新值、412 重新读取 ETag 并保留用户输入、400 展示
// 信封 problems。
// Resource form regressions: a managed key is entered directly and published, env stays
// an advanced placeholder-only path, local rejections send nothing and never echo the
// input, create-time scope and owner, a blank edit keeps the credential, a mode switch
// needs a new value, 412 re-reads the ETag while keeping input, and 400 renders problems.
import { flushPromises, mount } from '@vue/test-utils'
import type { DOMWrapper } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { markRaw } from 'vue'
import { ApiClient } from '../src/api'
import { ConsoleAuth } from '../src/auth'
import { ResourceForm } from '../src/components/resource-form'
import type { ResourceKind } from '../src/router'

const MODEL_PATH = '/api/v1/model-connections'
const PROFILES_PATH = '/api/v1/profiles'

const ETAG_V1 = `"sha256:${'1'.repeat(64)}"`
const ETAG_V2 = `"sha256:${'2'.repeat(64)}"`
const ETAG_V3 = `"sha256:${'3'.repeat(64)}"`
const SECRET = 'sk-live-0123456789abcdef'

interface Call {
  method: string
  path: string
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

/** 服务端视图：env 模式只回 `${ENV_VAR}` 引用，托管模式只回状态与尾号。 */
function modelView(baseUrl: string, etag: string): unknown {
  return {
    name: 'main',
    resource_uid: 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A',
    base_url: baseUrl,
    api_key_env: '${MAIN_KEY}',
    default_model: 'm1',
    credential_mode: 'env',
    credential_ref: '',
    credential_state: '',
    credential_version: 0,
    last4: '',
    scope: 'shared',
    owner_agent: '',
    revision: 1,
    published_version_id: 1,
    published_version_no: 1,
    applied_version_id: 1,
    applied_version_no: 1,
    etag,
    referenced_by: [],
  }
}

/** 记录每次请求并按 handler 返回；缺分支直接抛错，避免测试静默走空。 */
function mountForm(
  handler: Handler,
  kind: ResourceKind,
  edit: string | null = null,
): Call[] {
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
  // markRaw：避免 VTU 深代理 props 后破坏 ApiClient 的私有字段品牌检查。
  // markRaw keeps the client out of VTU's deep-reactive props wrapper.
  wrapper = mount(ResourceForm, {
    props: { api: markRaw(new ApiClient(new ConsoleAuth())), kind, edit },
  })
  return calls
}

function button(label: string): DOMWrapper<Element> {
  const found = wrapper!.findAll('button').find((item) => item.text() === label)
  return required(found, `按钮「${label}」`)
}

function inputValue(selector: string): string {
  return (wrapper!.find(selector).element as HTMLInputElement).value
}

describe('resource form', () => {
  it('creates a model connection with a managed key and the default shared scope', async () => {
    const calls = mountForm((call) => {
      if (call.method === 'POST' && call.path === MODEL_PATH) {
        return envelope(200, 'OK', 'success', modelView('https://api.example.com/v1', ETAG_V1), ETAG_V1)
      }
      return undefined
    }, 'model-connections')
    await flushPromises()

    await wrapper!.find('#resource-name').setValue('main')
    await wrapper!.find('#resource-base-url').setValue('https://api.example.com/v1')
    await wrapper!.find('#resource-default-model').setValue('m1')
    // 默认托管模式：密码框直接录完整 Key，页面不自造 scope 之外的其他默认。
    const key = wrapper!.find('#resource-api-key')
    expect(key.attributes('type')).toBe('password')
    await key.setValue(SECRET)
    await button('创建并发布').trigger('click')
    await flushPromises()

    expect(required(calls[0], '创建请求').body).toEqual({
      name: 'main',
      base_url: 'https://api.example.com/v1',
      default_model: 'm1',
      api_key: SECRET,
      credential_mode: 'managed',
      scope: 'shared',
    })
    // 组件不重新 GET 列表、也不做乐观更新：列表由父页面写后重读。
    expect(calls.filter((call) => call.method === 'GET')).toHaveLength(0)
    const created = wrapper!.emitted('created')
    expect(created).toHaveLength(1)
    expect(required(created![0], 'created 事件')[0]).toMatchObject({ name: 'main' })
    expect(wrapper!.find('.notice-info').text()).toContain('已创建并发布')
    // 密钥只出现在请求体里。
    expect(wrapper!.text()).not.toContain(SECRET)
  })

  it('creates in env mode with a normalised placeholder', async () => {
    const calls = mountForm((call) => {
      if (call.method === 'POST' && call.path === MODEL_PATH) {
        return envelope(200, 'OK', 'success', modelView('https://api.example.com/v1', ETAG_V1), ETAG_V1)
      }
      return undefined
    }, 'model-connections')
    await flushPromises()

    await wrapper!.find('#resource-name').setValue('main')
    await wrapper!.find('#resource-base-url').setValue('https://api.example.com/v1')
    await wrapper!.find('#resource-default-model').setValue('m1')
    // env 是高级方式：切换后输入裸变量名，组件负责归一成契约要求的 `${VAR}`。
    await wrapper!.find('#resource-api-key-mode').setValue('env')
    expect(wrapper!.find('#resource-api-key').attributes('type')).toBe('text')
    await wrapper!.find('#resource-api-key').setValue('MAIN_KEY')
    await button('创建并发布').trigger('click')
    await flushPromises()

    expect(required(calls[0], '创建请求').body).toMatchObject({
      api_key: '${MAIN_KEY}',
      credential_mode: 'env',
      scope: 'shared',
    })
  })

  it('creates an exclusive connection only after the owner is chosen', async () => {
    const calls = mountForm((call) => {
      if (call.method === 'GET' && call.path === PROFILES_PATH) {
        return envelope(200, 'OK', 'success', [{ name: 'writer' }, { name: 'reviewer' }])
      }
      if (call.method === 'POST' && call.path === MODEL_PATH) {
        return envelope(200, 'OK', 'success', modelView('https://api.example.com/v1', ETAG_V1), ETAG_V1)
      }
      return undefined
    }, 'model-connections')
    await flushPromises()

    await wrapper!.find('#resource-name').setValue('main')
    await wrapper!.find('#resource-base-url').setValue('https://api.example.com/v1')
    await wrapper!.find('#resource-default-model').setValue('m1')
    await wrapper!.find('#resource-api-key').setValue(SECRET)
    await wrapper!.find('#resource-scope-create-exclusive').setValue()
    await flushPromises()

    // 选专属才读取所有者候选；未选所有者时本地拒绝、零写请求。
    expect(calls.filter((call) => call.method === 'GET' && call.path === PROFILES_PATH)).toHaveLength(1)
    await button('创建并发布').trigger('click')
    await flushPromises()
    expect(wrapper!.find('.notice-error').text()).toContain('专属连接必须选择所有者 Agent')
    expect(calls.filter((call) => call.method === 'POST')).toHaveLength(0)

    await wrapper!.find('#resource-scope-create-owner').setValue('writer')
    await button('创建并发布').trigger('click')
    await flushPromises()
    expect(required(calls.find((call) => call.method === 'POST'), '创建请求').body).toMatchObject({
      scope: 'exclusive',
      owner_agent: 'writer',
    })
  })

  it('rejects a ${VAR} placeholder in managed mode without sending it', async () => {
    const calls = mountForm(() => undefined, 'model-connections')
    await flushPromises()

    await wrapper!.find('#resource-name').setValue('main')
    await wrapper!.find('#resource-base-url').setValue('https://api.example.com/v1')
    await wrapper!.find('#resource-default-model').setValue('m1')
    await wrapper!.find('#resource-api-key').setValue('${MAIN_KEY}')
    await button('创建并发布').trigger('click')
    await flushPromises()

    expect(calls).toHaveLength(0)
    expect(wrapper!.find('.notice-error').text()).toContain('「加密托管」来源不接受 ${VAR} 占位')
    expect(wrapper!.find('.notice-error').text()).not.toContain('MAIN_KEY')
    expect(inputValue('#resource-api-key')).toBe('${MAIN_KEY}')
  })

  it('rejects a plaintext secret in env mode locally without sending it', async () => {
    const calls = mountForm(() => undefined, 'model-connections')
    await flushPromises()

    await wrapper!.find('#resource-name').setValue('main')
    await wrapper!.find('#resource-base-url').setValue('https://api.example.com/v1')
    await wrapper!.find('#resource-default-model').setValue('m1')
    await wrapper!.find('#resource-api-key-mode').setValue('env')
    await wrapper!.find('#resource-api-key').setValue('sk-plaintext')
    await button('创建并发布').trigger('click')
    await flushPromises()

    // 真实密钥不进入请求体、日志或页面文案（错误不回显输入）。
    expect(calls).toHaveLength(0)
    expect(wrapper!.find('.notice-error').text()).toContain('「环境变量」来源只填写变量名')
    expect(wrapper!.find('.notice-error').text()).not.toContain('sk-plaintext')
    expect(inputValue('#resource-api-key')).toBe('sk-plaintext')
    expect(wrapper!.emitted('created')).toBeUndefined()
  })

  it('requires a managed key when creating a model connection', async () => {
    const calls = mountForm(() => undefined, 'model-connections')
    await flushPromises()

    await wrapper!.find('#resource-name').setValue('main')
    await wrapper!.find('#resource-base-url').setValue('https://api.example.com/v1')
    await wrapper!.find('#resource-default-model').setValue('m1')
    await button('创建并发布').trigger('click')
    await flushPromises()

    expect(calls).toHaveLength(0)
    expect(wrapper!.find('.notice-error').text()).toContain('请填写完整的 API 密钥（加密托管）')
  })

  it('re-reads the ETag on 412 and keeps the edited input', async () => {
    let reads = 0
    let writes = 0
    const calls = mountForm((call) => {
      if (call.method === 'GET' && call.path === `${MODEL_PATH}/main`) {
        reads += 1
        if (reads === 1) {
          return envelope(200, 'OK', 'success', modelView('https://api.example.com/v1', ETAG_V1), ETAG_V1)
        }
        if (reads === 2) {
          // 412 后的重读：服务端已有别人的新版本（ETag 前移），用户输入不应被覆盖。
          return envelope(
            200,
            'OK',
            'success',
            modelView('https://other.example/v1', ETAG_V2),
            ETAG_V2,
          )
        }
        return envelope(
          200,
          'OK',
          'success',
          modelView('https://changed.example/v1', ETAG_V3),
          ETAG_V3,
        )
      }
      if (call.method === 'PUT' && call.path === `${MODEL_PATH}/main`) {
        writes += 1
        if (writes === 1) {
          return envelope(412, 'PRECONDITION_FAILED', 'ETag 不匹配')
        }
        return envelope(
          200,
          'OK',
          'success',
          modelView('https://changed.example/v1', ETAG_V3),
          ETAG_V3,
        )
      }
      return undefined
    }, 'model-connections', 'main')
    await flushPromises()

    expect(inputValue('#resource-base-url')).toBe('https://api.example.com/v1')
    expect(wrapper!.find('.resource-etag').text()).toBe(ETAG_V1)

    await wrapper!.find('#resource-base-url').setValue('https://changed.example/v1')
    await button('保存修改').trigger('click')
    await flushPromises()

    const firstWrite = required(
      calls.find((call) => call.method === 'PUT'),
      '第一次保存请求',
    )
    expect(firstWrite.ifMatch).toBe(ETAG_V1)
    // 412：固定文案、重读 ETag、用户输入原样保留。
    expect(wrapper!.find('.notice-error').text()).toContain('资源已被他人修改')
    expect(reads).toBe(2)
    expect(wrapper!.find('.resource-etag').text()).toBe(ETAG_V2)
    expect(inputValue('#resource-base-url')).toBe('https://changed.example/v1')

    // 再保存一次：If-Match 用的是重读后的 ETag，而不是失败前的旧值。
    await button('保存修改').trigger('click')
    await flushPromises()
    const writesNow = calls.filter((call) => call.method === 'PUT')
    expect(required(writesNow[1], '第二次保存请求').ifMatch).toBe(ETAG_V2)
    expect(wrapper!.find('.notice-info').text()).toContain('已保存并发布')
    expect(wrapper!.find('.resource-etag').text()).toBe(ETAG_V3)
  })

  it('renders the envelope problems on 400 and keeps the input', async () => {
    const calls = mountForm((call) => {
      if (call.method === 'POST' && call.path === MODEL_PATH) {
        return envelope(400, 'BAD_REQUEST', '字段 base_url 只接受 http / https', {
          problems: ['base_url 只接受 http / https'],
        })
      }
      return undefined
    }, 'model-connections')
    await flushPromises()

    await wrapper!.find('#resource-name').setValue('main')
    await wrapper!.find('#resource-base-url').setValue('ftp://api.example.com/v1')
    await wrapper!.find('#resource-default-model').setValue('m1')
    await wrapper!.find('#resource-api-key').setValue(SECRET)
    await button('创建并发布').trigger('click')
    await flushPromises()

    expect(required(calls[0], '创建请求').body).toMatchObject({ api_key: SECRET })
    expect(wrapper!.find('.notice-error').text()).toBe('字段 base_url 只接受 http / https')
    expect(wrapper!.findAll('.problems li').map((item) => item.text())).toEqual([
      'base_url 只接受 http / https',
    ])
    // 失败不清空输入，修正后可以原样重试。
    expect(inputValue('#resource-api-key')).toBe(SECRET)
    expect(wrapper!.emitted('created')).toBeUndefined()
  })

  it('keeps the stored credential when an edit leaves the key blank', async () => {
    const calls = mountForm((call) => {
      if (call.method === 'GET' && call.path === `${MODEL_PATH}/main`) {
        return envelope(200, 'OK', 'success', modelView('https://api.example.com/v1', ETAG_V1), ETAG_V1)
      }
      if (call.method === 'PUT' && call.path === `${MODEL_PATH}/main`) {
        return envelope(
          200,
          'OK',
          'success',
          modelView('https://api.example.com/v1', ETAG_V2),
          ETAG_V2,
        )
      }
      return undefined
    }, 'model-connections', 'main')
    await flushPromises()

    // 编辑态只展示引用，不显示也不回填任何凭据值。
    expect(wrapper!.text()).toContain('${MAIN_KEY}')
    expect(inputValue('#resource-api-key')).toBe('')
    await button('保存修改').trigger('click')
    await flushPromises()

    expect(required(calls.find((call) => call.method === 'PUT'), '保存请求').body).toEqual({
      name: 'main',
      base_url: 'https://api.example.com/v1',
      default_model: 'm1',
    })
  })

  it('requires a new value when the credential mode is switched', async () => {
    const calls = mountForm((call) => {
      if (call.method === 'GET' && call.path === `${MODEL_PATH}/main`) {
        return envelope(200, 'OK', 'success', modelView('https://api.example.com/v1', ETAG_V1), ETAG_V1)
      }
      return undefined
    }, 'model-connections', 'main')
    await flushPromises()

    await wrapper!.find('#resource-api-key-mode').setValue('managed')
    await button('保存修改').trigger('click')
    await flushPromises()

    expect(calls.filter((call) => call.method === 'PUT')).toHaveLength(0)
    expect(wrapper!.find('.notice-error').text()).toContain('切换凭据来源必须填写新值')
  })

  it('accepts a pasted placeholder in env mode and shows the current reference', async () => {
    const calls = mountForm((call) => {
      if (call.method === 'GET' && call.path === `${MODEL_PATH}/main`) {
        return envelope(200, 'OK', 'success', modelView('https://api.example.com/v1', ETAG_V1), ETAG_V1)
      }
      if (call.method === 'PUT' && call.path === `${MODEL_PATH}/main`) {
        return envelope(
          200,
          'OK',
          'success',
          modelView('https://api.example.com/v1', ETAG_V2),
          ETAG_V2,
        )
      }
      return undefined
    }, 'model-connections', 'main')
    await flushPromises()

    expect(wrapper!.text()).toContain('${MAIN_KEY}')
    await wrapper!.find('#resource-api-key').setValue('${MAIN_KEY}')
    await button('保存修改').trigger('click')
    await flushPromises()

    expect(required(calls.find((call) => call.method === 'PUT'), '保存请求').body).toMatchObject({
      api_key: '${MAIN_KEY}',
      credential_mode: 'env',
    })
  })
})
