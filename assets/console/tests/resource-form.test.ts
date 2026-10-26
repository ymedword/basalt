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
/** 14D：模型连接表单挂载时会读实例加密状态，决定「加密托管」是否可选。 */
const SECRETS_PATH = '/api/v1/instance/secrets'

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

/** 实例加密就绪夹具：只影响「加密托管」是否可选，不含任何真实密钥材料。 */
// Instance-encryption ready fixture; it only decides whether managed is selectable.
function secretsReady(): unknown {
  return {
    state: 'ready',
    key_id: 'k_test000001',
    initialized_at: '2026-10-20T08:00:00Z',
    key_file: '/var/lib/basalt/secrets/instance.key',
    recovery_reason: '',
    can_initialize: false,
    credential_summary: { managed: 0, managed_revoked: 0, env: 0, missing: 0 },
  }
}

/** 去掉挂载时的实例加密只读探测，只留本测试关心的资源读写请求。 */
// Drops the mount-time instance-secrets read; resource reads/writes remain.
function resourceCalls(calls: Call[]): Call[] {
  return calls.filter((call) => !(call.method === 'GET' && call.path === SECRETS_PATH))
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
      // 挂载时的实例加密探测默认用就绪夹具应答；用例可自行覆盖（未初始化 / 恢复态）。
      // The mount-time secrets probe defaults to the ready fixture; cases may override it.
      const response =
        call.method === 'GET' && call.path === SECRETS_PATH
          ? (handler(call) ?? envelope(200, 'OK', 'success', secretsReady()))
          : handler(call)
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

    expect(required(resourceCalls(calls)[0], '创建请求').body).toEqual({
      name: 'main',
      base_url: 'https://api.example.com/v1',
      default_model: 'm1',
      api_key: SECRET,
      credential_mode: 'managed',
      scope: 'shared',
    })
    // 组件不重新 GET 列表、也不做乐观更新：列表由父页面写后重读。
    expect(resourceCalls(calls).filter((call) => call.method === 'GET')).toHaveLength(0)
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

    expect(required(resourceCalls(calls)[0], '创建请求').body).toMatchObject({
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

    // 本地拒绝零资源请求（实例加密探测发生在挂载时，不属于本次交互）。
    expect(resourceCalls(calls)).toHaveLength(0)
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
    // 本地拒绝零资源请求（实例加密探测发生在挂载时，不属于本次交互）。
    expect(resourceCalls(calls)).toHaveLength(0)
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

    // 本地拒绝零资源请求（实例加密探测发生在挂载时，不属于本次交互）。
    expect(resourceCalls(calls)).toHaveLength(0)
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

    expect(required(resourceCalls(calls)[0], '创建请求').body).toMatchObject({ api_key: SECRET })
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

  it('keeps managed usable on a legacy instance and initialises it on the first save', async () => {
    const uninitialized = {
      ...(secretsReady() as Record<string, unknown>),
      state: 'uninitialized',
      key_id: '',
      initialized_at: '',
      can_initialize: true,
    }
    let initialised = false
    const calls = mountForm((call) => {
      if (call.method === 'GET' && call.path === SECRETS_PATH) {
        return envelope(
          200,
          'OK',
          'success',
          initialised ? secretsReady() : uninitialized,
        )
      }
      if (call.method === 'POST' && call.path === `${SECRETS_PATH}/init`) {
        initialised = true
        return envelope(200, 'OK', 'success', secretsReady())
      }
      return undefined
    }, 'model-connections')
    await flushPromises()

    // 存量实例：托管默认可用，不需要先点初始化；说明句给出「首次保存自动完成」。
    expect(
      wrapper!.find('#resource-api-key-mode option[value="managed"]').attributes('disabled'),
    ).toBeUndefined()
    expect(
      wrapper!.find('#resource-api-key-mode option[value="env"]').attributes('disabled'),
    ).toBeUndefined()
    expect(wrapper!.text()).toContain('首次保存加密托管凭据时会自动完成初始化')
    // 来源选择在「高级」折叠区里，普通表单不再要求先做这个选择。
    expect(wrapper!.find('details.credential-advanced summary').text()).toContain('高级：凭据来源')
    // 初始化入口只在高级诊断里，普通流程不出现「请先初始化」这一步。
    const advanced = wrapper!.find('details.technical-details')
    expect(advanced.find('summary').text()).toContain('高级：实例加密诊断')
    expect(advanced.text()).toContain('也可以在这里手动初始化')

    let changed = 0
    const onChange = (): void => {
      changed += 1
    }
    window.addEventListener('basalt:instance-secrets-changed', onChange)
    await advanced.findAll('button').find((item) => item.text() === '初始化实例加密')!.trigger('click')
    await flushPromises()
    // 一次性动作：对话框必须写明只搬工作区会让托管凭据无法解密。
    expect(wrapper!.find('.dialog').text()).toContain('只搬工作区会让托管凭据无法解密')
    await wrapper!
      .findAll('.dialog-actions button')
      .find((item) => item.text() === '初始化')!
      .trigger('click')
    await flushPromises()
    window.removeEventListener('basalt:instance-secrets-changed', onChange)

    expect(required(calls.find((call) => call.method === 'POST'), '初始化请求').body).toEqual({
      confirm: true,
    })
    // 初始化后无需刷新页面：状态就地翻成就绪，并通知全站横幅复核。
    expect(changed).toBe(1)
    expect(wrapper!.text()).toContain('实例加密服务已初始化')
    expect(wrapper!.text()).toContain('主密钥已就绪')
  })

  it('saves a key on an uninitialised instance without an explicit init step', async () => {
    // R2-1 主链路（页面侧）：存量实例 → 录入 API Key → 保存 → 状态就地翻成就绪。
    const uninitialized = {
      ...(secretsReady() as Record<string, unknown>),
      state: 'uninitialized',
      key_id: '',
      initialized_at: '',
      can_initialize: true,
    }
    let saved = false
    const calls = mountForm((call) => {
      if (call.method === 'GET' && call.path === SECRETS_PATH) {
        return envelope(200, 'OK', 'success', saved ? secretsReady() : uninitialized)
      }
      if (call.method === 'POST' && call.path === MODEL_PATH) {
        saved = true
        return envelope(200, 'OK', 'success', modelView('https://api.example.com/v1', ETAG_V1), ETAG_V1)
      }
      return undefined
    }, 'model-connections')
    await flushPromises()

    await wrapper!.find('#resource-name').setValue('main')
    await wrapper!.find('#resource-base-url').setValue('https://api.example.com/v1')
    await wrapper!.find('#resource-default-model').setValue('m1')
    await wrapper!.find('#resource-api-key').setValue(SECRET)
    await button('创建并发布').trigger('click')
    await flushPromises()

    expect(required(resourceCalls(calls)[0], '创建请求').body).toMatchObject({
      api_key: SECRET,
      credential_mode: 'managed',
    })
    // 保存后重新读取状态：不需要手动初始化，也没有额外点击。
    expect(calls.filter((call) => call.method === 'POST' && call.path === `${SECRETS_PATH}/init`)).toHaveLength(0)
    expect(wrapper!.text()).toContain('就绪（k_test…）')
    expect(wrapper!.text()).toContain('主密钥已就绪')
  })

  it('disables managed only for an explicit pure-env deployment', async () => {
    mountForm(() => {
      return envelope(200, 'OK', 'success', {
        ...(secretsReady() as Record<string, unknown>),
        state: 'uninitialized',
        key_id: '',
        initialized_at: '',
        can_initialize: false,
      })
    }, 'model-connections')
    await flushPromises()

    // 只有显式纯 env（can_initialize=false）才禁用托管；env 来源保持可用。
    expect(
      wrapper!.find('#resource-api-key-mode option[value="managed"]').attributes('disabled'),
    ).toBeDefined()
    expect(
      wrapper!.find('#resource-api-key-mode option[value="env"]').attributes('disabled'),
    ).toBeUndefined()
    const text = wrapper!.text()
    expect(text).toContain('此实例以纯环境变量方式部署（secrets.auto_init: false）；加密托管不可用。')
    expect(text).toContain('不接受加密托管')
    // 不能就地初始化时不提供初始化按钮。
    expect(wrapper!.findAll('button').some((item) => item.text() === '初始化实例加密')).toBe(false)
  })

  it('disables the managed source in recovery and names the recovery guide', async () => {
    mountForm(() => {
      return envelope(200, 'OK', 'success', {
        ...(secretsReady() as Record<string, unknown>),
        state: 'recovery_required',
        key_id: '',
        recovery_reason: 'missing',
        can_initialize: false,
      })
    }, 'model-connections')
    await flushPromises()

    expect(
      wrapper!.find('#resource-api-key-mode option[value="managed"]').attributes('disabled'),
    ).toBeDefined()
    expect(
      wrapper!.find('#resource-api-key-mode option[value="env"]').attributes('disabled'),
    ).toBeUndefined()
    const text = wrapper!.text()
    expect(text).toContain('实例加密服务需要恢复（密钥文件不存在）')
    expect(text).toContain('docs/Operations.md')
    // 恢复态不提供「初始化」按钮：那是重建密钥，属恢复流程，不由表单发起。
    expect(wrapper!.findAll('button').some((item) => item.text() === '初始化实例加密')).toBe(false)
  })

  it('keeps the managed source selectable when the status read fails', async () => {
    mountForm((call) => {
      if (call.path === SECRETS_PATH) {
        throw new Error('secrets offline')
      }
      if (call.method === 'POST' && call.path === MODEL_PATH) {
        return envelope(200, 'OK', 'success', modelView('https://api.example.com/v1', ETAG_V1), ETAG_V1)
      }
      return undefined
    }, 'model-connections')
    await flushPromises()

    // 读不到状态不擅自禁用：服务端仍是最终闸门，页面只把不确定性说清楚。
    expect(
      wrapper!.find('#resource-api-key-mode option[value="managed"]').attributes('disabled'),
    ).toBeUndefined()
    expect(wrapper!.text()).toContain('读取实例加密状态失败')
    expect(wrapper!.text()).toContain('服务端仍是最终闸门')
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
