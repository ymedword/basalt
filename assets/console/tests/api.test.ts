// API 客户端用例：信封解包、凭据头、条件写、错误码映射与 401 / 429 处理（契约）。
// API client tests: envelope unwrapping, credential headers, conditional writes,
// error mapping and the 401 / 429 paths from the contracts.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  ApiClient,
  ApiError,
  RESOURCE_ERROR_COPY,
  errorMessage,
  resourceErrorCopy,
} from '../src/api'
import { ConsoleAuth } from '../src/auth'

interface FakeResponse {
  readonly status?: number
  readonly body?: unknown
  readonly headers?: Record<string, string>
  readonly nonJSON?: boolean
}

function fakeResponse(init: FakeResponse = {}): Response {
  const status = init.status ?? 200
  const headerMap = new Map(
    Object.entries(init.headers ?? {}).map(([key, value]) => [key.toLowerCase(), value]),
  )
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: {
      get: (name: string): string | null => headerMap.get(name.toLowerCase()) ?? null,
    },
    json: async (): Promise<unknown> => {
      if (init.nonJSON === true) {
        throw new SyntaxError('not json')
      }
      return init.body
    },
  } as unknown as Response
}

let calls: Array<{ url: string; init: RequestInit }> = []

function stubFetch(response: Response): void {
  calls = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      calls.push({ url, init })
      return response
    }),
  )
}

async function expectApiError(
  promise: Promise<unknown>,
  status: number,
  code: string,
): Promise<ApiError> {
  let caught: unknown = null
  try {
    await promise
  } catch (error) {
    caught = error
  }
  expect(caught).toBeInstanceOf(ApiError)
  const apiError = caught as ApiError
  expect(apiError.status).toBe(status)
  expect(apiError.code).toBe(code)
  return apiError
}

function headersOf(index = 0): Record<string, string> {
  return (calls[index]?.init.headers ?? {}) as unknown as Record<string, string>
}

beforeEach(() => {
  window.history.replaceState(null, '', '/console/')
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('ApiClient request shaping', () => {
  it('unwraps the envelope and sends the bearer token', async () => {
    stubFetch(fakeResponse({ body: { code: 'OK', message: 'success', data: { version: '0.9.0' } } }))
    const auth = new ConsoleAuth()
    auth.setToken('secret-token')
    const api = new ApiClient(auth)

    await expect(api.getInfo()).resolves.toEqual({ version: '0.9.0' })
    expect(calls[0]?.url).toBe('/api/v1/info')
    expect(calls[0]?.init.method).toBe('GET')
    expect(headersOf().Authorization).toBe('Bearer secret-token')
    expect(headersOf().Accept).toBe('application/json')
  })

  it('sends the local-trust marker instead of a bearer token', async () => {
    stubFetch(fakeResponse({ body: { code: 'OK', message: 'success', data: { version: '0.9.0' } } }))
    const auth = new ConsoleAuth()
    auth.setLocalTrust()
    const api = new ApiClient(auth)

    await api.getInfo()
    expect(headersOf()['X-Basalt-Local-Trust']).toBe('1')
    expect(headersOf().Authorization).toBeUndefined()
  })

  it('sends If-Match and a JSON body for the conditional content write', async () => {
    stubFetch(
      fakeResponse({
        body: {
          code: 'OK',
          message: 'success',
          data: { agent: 'reporter', lifecycle: {}, etag: '"sha256:new"', files: [] },
        },
      }),
    )
    const api = new ApiClient(new ConsoleAuth())

    await api.updateAgentContent('a b', { content: '# AGENT' }, '"sha256:old"')
    expect(calls[0]?.url).toBe('/api/v1/agents/a%20b/content')
    expect(calls[0]?.init.method).toBe('PUT')
    expect(headersOf()['If-Match']).toBe('"sha256:old"')
    expect(headersOf()['Content-Type']).toBe('application/json')
    expect(calls[0]?.init.body).toBe(JSON.stringify({ content: '# AGENT' }))
  })

  it('serializes only the contract query parameters and skips empty values', async () => {
    stubFetch(fakeResponse({ body: { code: 'OK', message: 'success', data: [] } }))
    const api = new ApiClient(new ConsoleAuth())

    await api.listRuns({ session_id: '', agent: 'reporter', status: 'running', limit: 10 })
    expect(calls[0]?.url).toBe('/api/v1/runs?agent=reporter&status=running&limit=10')
  })

  it('keeps the cancel message from the envelope', async () => {
    stubFetch(
      fakeResponse({
        body: {
          code: 'OK',
          message: '已是终态，无需取消',
          data: { run_id: 'run-1', status: 'completed' },
        },
      }),
    )
    const api = new ApiClient(new ConsoleAuth())
    const result = await api.cancelRun('run-1')
    expect(result.message).toBe('已是终态，无需取消')
    expect(result.run.run_id).toBe('run-1')
    expect(calls[0]?.init.method).toBe('POST')
  })
})

describe('ApiClient auth failures', () => {
  it('clears credentials and reports the current location on 401', async () => {
    window.history.replaceState(null, '', '/console/runs/run-9?x=1')
    stubFetch(
      fakeResponse({ status: 401, body: { code: 'UNAUTHORIZED', message: '未认证或凭据无效' } }),
    )
    const auth = new ConsoleAuth()
    auth.setToken('bad-token')
    const onAuthFailure = vi.fn()
    const api = new ApiClient(auth, { onAuthFailure })

    await expectApiError(api.getInfo(), 401, 'UNAUTHORIZED')
    expect(auth.authenticated).toBe(false)
    expect(onAuthFailure).toHaveBeenCalledTimes(1)
    expect(onAuthFailure).toHaveBeenCalledWith('/console/runs/run-9?x=1')
  })

  it('treats a 403 in local-trust mode as an auth failure', async () => {
    stubFetch(fakeResponse({ status: 403, body: { code: 'FORBIDDEN', message: '禁止访问' } }))
    const auth = new ConsoleAuth()
    auth.setLocalTrust()
    const onAuthFailure = vi.fn()
    const api = new ApiClient(auth, { onAuthFailure })

    await expectApiError(api.getInfo(), 403, 'FORBIDDEN')
    expect(auth.authenticated).toBe(false)
    expect(onAuthFailure).toHaveBeenCalledTimes(1)
  })

  it('keeps a token-mode 403 as an in-page error', async () => {
    stubFetch(fakeResponse({ status: 403, body: { code: 'FORBIDDEN', message: '禁止访问' } }))
    const auth = new ConsoleAuth()
    auth.setToken('secret-token')
    const onAuthFailure = vi.fn()
    const api = new ApiClient(auth, { onAuthFailure })

    await expectApiError(api.getInfo(), 403, 'FORBIDDEN')
    expect(auth.authenticated).toBe(true)
    expect(onAuthFailure).not.toHaveBeenCalled()
  })

  it('parses Retry-After on 429 and surfaces it to the user', async () => {
    stubFetch(
      fakeResponse({
        status: 429,
        headers: { 'Retry-After': '42' },
        body: { code: 'AUTH_RATE_LIMITED', message: '认证失败次数过多' },
      }),
    )
    const api = new ApiClient(new ConsoleAuth())

    const error = await expectApiError(api.getInfo(), 429, 'AUTH_RATE_LIMITED')
    expect(error.retryAfterSeconds).toBe(42)
    expect(errorMessage(error)).toContain('42 秒后重试')
  })

  it('ignores non-numeric Retry-After values', async () => {
    stubFetch(
      fakeResponse({
        status: 429,
        headers: { 'Retry-After': 'later' },
        body: { code: 'AUTH_RATE_LIMITED', message: '限速' },
      }),
    )
    const api = new ApiClient(new ConsoleAuth())
    const error = await expectApiError(api.getInfo(), 429, 'AUTH_RATE_LIMITED')
    expect(error.retryAfterSeconds).toBeNull()
    expect(errorMessage(error)).toBe('限速')
  })
})

describe('ApiClient failure mapping', () => {
  it('reports a missing data payload as a protocol error', async () => {
    stubFetch(fakeResponse({ body: { code: 'OK', message: 'success' } }))
    const api = new ApiClient(new ConsoleAuth())
    await expectApiError(api.getInfo(), 0, 'PROTOCOL')
  })

  it('reports a non-JSON success body as a protocol error', async () => {
    stubFetch(fakeResponse({ nonJSON: true }))
    const api = new ApiClient(new ConsoleAuth())
    await expectApiError(api.getInfo(), 0, 'PROTOCOL')
  })

  it('maps a network failure to NETWORK without touching auth state', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new TypeError('connection refused')
      }),
    )
    const auth = new ConsoleAuth()
    auth.setToken('secret-token')
    const onAuthFailure = vi.fn()
    const api = new ApiClient(auth, { onAuthFailure })

    const error = await expectApiError(api.getInfo(), 0, 'NETWORK')
    expect(auth.authenticated).toBe(true)
    expect(onAuthFailure).not.toHaveBeenCalled()
    expect(errorMessage(error)).toContain('无法连接到服务器')
  })

  it('keeps the server message for plain errors', () => {
    expect(errorMessage(new Error('boom'))).toBe('boom')
    expect(errorMessage('weird')).toBe('请求失败')
  })
})

describe('failure data projection', () => {
  it('keeps the four arrays and the three whitelisted scalars', async () => {
    stubFetch(
      fakeResponse({
        status: 409,
        body: {
          code: 'RESOURCE_UID_MISMATCH',
          message: '资源身份不一致',
          data: {
            agents: ['writer'],
            resource_uid: 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A',
            operation_id: 'op_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B',
            current_etag: `"sha256:${'b'.repeat(64)}"`,
            ignored: '不该出站',
          },
        },
      }),
    )
    const api = new ApiClient(new ConsoleAuth())
    const error = await expectApiError(api.getInfo(), 409, 'RESOURCE_UID_MISMATCH')
    expect(error.data).not.toBeNull()
    expect(error.data?.agents).toEqual(['writer'])
    expect(error.data?.resource_uid).toBe('rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A')
    expect(error.data?.operation_id).toBe('op_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B')
    expect(error.data?.current_etag).toBe(`"sha256:${'b'.repeat(64)}"`)
    // 白名单之外的同名键没有载荷：记录里只有契约字段。
    expect(Object.keys(error.data ?? {})).not.toContain('ignored')
  })

  it('drops empty scalars and unknown shapes entirely', async () => {
    stubFetch(
      fakeResponse({
        status: 412,
        body: {
          code: 'PRECONDITION_FAILED',
          message: '版本已变化',
          data: { current_etag: '', resource_uid: 5, other: ['x'] },
        },
      }),
    )
    const api = new ApiClient(new ConsoleAuth())
    const error = await expectApiError(api.getInfo(), 412, 'PRECONDITION_FAILED')
    // 空串按“未携带”处理：没有任何白名单字段时 data 为 null。
    expect(error.data).toBeNull()
  })
})

describe('resource error copy', () => {
  it('covers the twelve contract codes and nothing else', () => {
    expect(Object.keys(RESOURCE_ERROR_COPY)).toHaveLength(12)
    for (const [code, copy] of Object.entries(RESOURCE_ERROR_COPY)) {
      expect(copy, code).not.toBe('')
    }
  })

  it('returns the page copy for a known code and empty for others', () => {
    expect(resourceErrorCopy(new ApiError(400, 'CREDENTIAL_REVOKED', '已撤销'))).toContain(
      '重新录入',
    )
    expect(resourceErrorCopy(new ApiError(400, 'SOMETHING_ELSE', 'x'))).toBe('')
    expect(resourceErrorCopy(new Error('boom'))).toBe('')
  })
})
