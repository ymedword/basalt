// 认证状态用例：内存凭据、请求头形态、清除语义与"不落任何存储"（约束 40）。
// Auth unit tests: in-memory credentials, header shapes, clearing semantics and the
// guarantee that nothing is persisted anywhere.
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ConsoleAuth } from '../src/auth'

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  document.cookie = ''
})

describe('ConsoleAuth', () => {
  it('starts unauthenticated with no headers', () => {
    const auth = new ConsoleAuth()
    expect(auth.authenticated).toBe(false)
    expect(auth.mode).toBe('token')
    expect(auth.headers()).toEqual({})
    expect(auth.snapshot()).toEqual({ mode: 'token', authenticated: false })
  })

  it('stores a trimmed bearer token in memory only', () => {
    const auth = new ConsoleAuth()
    auth.setToken('  secret-token  ')
    expect(auth.authenticated).toBe(true)
    expect(auth.mode).toBe('token')
    expect(auth.headers()).toEqual({ Authorization: 'Bearer secret-token' })
  })

  it('rejects blank tokens instead of silently authenticating', () => {
    const auth = new ConsoleAuth()
    expect(() => auth.setToken('   ')).toThrow('token 不能为空')
    expect(auth.authenticated).toBe(false)
  })

  it('switches to the local-trust header and back', () => {
    const auth = new ConsoleAuth()
    auth.setToken('secret-token')
    auth.setLocalTrust()
    expect(auth.mode).toBe('local_trust')
    expect(auth.headers()).toEqual({ 'X-Basalt-Local-Trust': '1' })
    auth.setToken('next-token')
    expect(auth.mode).toBe('token')
    expect(auth.headers()).toEqual({ Authorization: 'Bearer next-token' })
  })

  it('clears both credentials on clear()', () => {
    const auth = new ConsoleAuth()
    auth.setLocalTrust()
    auth.clear()
    expect(auth.authenticated).toBe(false)
    expect(auth.headers()).toEqual({})
  })

  it('notifies subscribers and honours unsubscribe', () => {
    const auth = new ConsoleAuth()
    const listener = vi.fn()
    const unsubscribe = auth.subscribe(listener)
    auth.setToken('secret-token')
    auth.clear()
    expect(listener).toHaveBeenCalledTimes(2)
    expect(listener).toHaveBeenLastCalledWith({ mode: 'token', authenticated: false })
    unsubscribe()
    auth.setToken('again')
    expect(listener).toHaveBeenCalledTimes(2)
  })

  it('never persists credentials into browser storage, cookies or the URL', () => {
    // 凭据只存在于内存：浏览器存储 / cookie / 地址栏都保持原样。
    const auth = new ConsoleAuth()
    auth.setToken('secret-token')
    auth.setLocalTrust()
    auth.clear()
    expect(localStorage.length).toBe(0)
    expect(sessionStorage.length).toBe(0)
    expect(document.cookie).toBe('')
    expect(window.location.search).toBe('')
  })

  it('exposes no token material in the snapshot', () => {
    const auth = new ConsoleAuth()
    auth.setToken('secret-token')
    expect(Object.keys(auth.snapshot()).sort()).toEqual(['authenticated', 'mode'])
    expect(JSON.stringify(auth.snapshot())).not.toContain('secret-token')
  })
})
